//go:build envtest

package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

var k8s client.Client

func TestMain(m *testing.M) {
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	if err := v1.AddToScheme(scheme.Scheme); err != nil {
		panic(err)
	}
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme.Scheme}); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func caps(name string, subj v1.Subjects, rules ...v1.ClusterRule) *v1.ClusterAccessPolicy {
	return &v1.ClusterAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{GenerateName: name + "-"},
		Spec:       v1.ClusterAccessPolicySpec{Subjects: subj, Rules: rules},
	}
}

func eq(n, v string) v1.Matcher { return v1.Matcher{Name: n, Op: v1.MatchEqual, Value: v} }

func TestClusterAccessPolicyValidation(t *testing.T) {
	grp := v1.Subjects{Groups: []string{"g"}, Users: []string{}}
	tests := []struct {
		name    string
		obj     *v1.ClusterAccessPolicy
		wantErr string // empty = accepted
	}{
		{"unrestricted", caps("a", grp, v1.ClusterRule{Backends: []string{"metrics"}, Tenants: []string{"t1"}, Unrestricted: true}), ""},
		{"matchers", caps("b", grp, v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{{Name: "namespace", Op: v1.MatchRegexMatch, Value: "a|b"}}, Labels: &v1.LabelFilter{Deny: []string{"customer_id"}}}), ""},
		{"empty subjects", caps("c", v1.Subjects{Groups: []string{}, Users: []string{}}, v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true}), "at least one group or user required"},
		{"unrestricted with matchers", caps("d", grp, v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true, Matchers: []v1.Matcher{eq("a", "b")}}), "set either unrestricted or matchers"},
		{"neither", caps("e", grp, v1.ClusterRule{Backends: []string{"metrics"}}), "set either unrestricted or matchers"},
		{"invalid op", caps("f", grp, v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{{Name: "a", Op: "Like", Value: "b"}}}), "Unsupported value"},
		{"redact", caps("g", grp, redactRule(v1.RedactRule{Name: "c", Strategy: v1.RedactPlaceholder}, v1.RedactRule{Name: "ip", Strategy: v1.RedactIPv4Prefix, Octets: octets(2)})), ""},
		{"redact unknown strategy", caps("h", grp, redactRule(v1.RedactRule{Name: "c", Strategy: "Mask"})), "Unsupported value"},
		{"redact ipv4 without octets", caps("i", grp, redactRule(v1.RedactRule{Name: "c", Strategy: v1.RedactIPv4Prefix})), "octets is required for IPv4Prefix"},
		{"redact octets on hash", caps("j", grp, redactRule(v1.RedactRule{Name: "c", Strategy: v1.RedactHash, Octets: octets(1)})), "octets is required for IPv4Prefix"},
		{"redact octets out of range", caps("k", grp, redactRule(v1.RedactRule{Name: "c", Strategy: v1.RedactIPv4Prefix, Octets: octets(4)})), "less than or equal to 3"},
		{"redact duplicate name", caps("l", grp, redactRule(v1.RedactRule{Name: "c", Strategy: v1.RedactHash}, v1.RedactRule{Name: "c", Strategy: v1.RedactRemove})), "Duplicate value"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { check(t, k8s.Create(context.Background(), tc.obj), tc.wantErr) })
	}
}

func TestAccessPolicyValidation(t *testing.T) {
	mk := func(subj v1.Subjects, rules ...v1.Rule) *v1.AccessPolicy {
		return &v1.AccessPolicy{ObjectMeta: metav1.ObjectMeta{GenerateName: "ap-", Namespace: "default"}, Spec: v1.AccessPolicySpec{Subjects: subj, Rules: rules}}
	}
	// PLAN §4.5 example.
	ok := mk(v1.Subjects{Groups: []string{"order-service-team"}, Users: []string{"alice@example.com"}},
		v1.Rule{Backends: []string{"metrics", "logs"}, Matchers: []v1.Matcher{{Name: "service", Op: v1.MatchNotEqual, Value: ""}}},
		v1.Rule{Backends: []string{"traces"}})
	check(t, k8s.Create(context.Background(), ok), "")
	check(t, k8s.Create(context.Background(), mk(v1.Subjects{Groups: []string{}, Users: []string{}}, v1.Rule{Backends: []string{"metrics"}})), "at least one group or user required")
	check(t, k8s.Create(context.Background(), mk(v1.Subjects{Users: []string{"u"}})), "spec.rules")
}

func TestAllClustersValidation(t *testing.T) {
	grp := v1.Subjects{Groups: []string{"g"}, Users: []string{}}
	ctx := context.Background()
	check(t, k8s.Create(ctx, caps("ac1", grp, v1.ClusterRule{Backends: []string{"metrics"}, AllClusters: true, Matchers: []v1.Matcher{eq("namespace", "x")}})), "")
	check(t, k8s.Create(ctx, caps("ac2", grp, v1.ClusterRule{Backends: []string{"metrics"}, AllClusters: true, Unrestricted: true})), "allClusters and unrestricted are mutually exclusive")

	// Objects created before the field existed get the default and stay valid.
	old := caps("ac3", grp, v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{eq("namespace", "x")}})
	check(t, k8s.Create(ctx, old), "")
	var got v1.ClusterAccessPolicy
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(old), &got); err != nil || got.Spec.Rules[0].AllClusters {
		t.Fatalf("allClusters default: %v %+v", err, got.Spec.Rules)
	}
}

func TestSpokeClusterValidation(t *testing.T) {
	ctx := context.Background()
	mk := func(name string, spec v1.SpokeClusterSpec) *v1.SpokeCluster {
		return &v1.SpokeCluster{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	}
	ok := mk("cluster-a", v1.SpokeClusterSpec{Identity: "spiffe://mla.example.com/agent/cluster-a", Tenants: []string{"t1"}, DefaultTenant: "t1",
		MaxStaleness: &metav1.Duration{Duration: 168 * 3600e9}})
	check(t, k8s.Create(ctx, ok), "")
	check(t, k8s.Create(ctx, mk("cluster-b", v1.SpokeClusterSpec{})), "spec.identity")
	check(t, k8s.Create(ctx, mk("cluster-c", v1.SpokeClusterSpec{Identity: "spiffe://x/cluster-c", Tenants: []string{"t1"}, DefaultTenant: "t2"})), "defaultTenant must be in tenants")
	check(t, k8s.Create(ctx, mk(strings.Repeat("a", 64), v1.SpokeClusterSpec{Identity: "spiffe://x/a"})), "at most 63 characters")

	check(t, k8s.Create(ctx, mk("cluster-d", v1.SpokeClusterSpec{Identity: "spiffe://x/cluster-d", MaxStaleness: &metav1.Duration{}})), "maxStaleness must be positive")
	check(t, k8s.Create(ctx, mk("cluster-e", v1.SpokeClusterSpec{Identity: "spiffe://x/cluster-e", MaxStaleness: &metav1.Duration{Duration: -3600e9}})), "maxStaleness must be positive")
	check(t, k8s.Create(ctx, mk("cluster-f", v1.SpokeClusterSpec{Identity: "spiffe://x/cluster-f", Tenants: []string{"t1", "t1"}})), "Duplicate value")
	// Each tenant is a Kubernetes label value.
	for _, bad := range []string{"", "a b", "-t1", "t1/x", strings.Repeat("t", 64)} {
		check(t, k8s.Create(ctx, mk("cluster-g", v1.SpokeClusterSpec{Identity: "spiffe://x/cluster-g", Tenants: []string{bad}})), "spec.tenants")
	}

	// The receiver writes status through the subresource; a status write cannot change spec.
	ok.Status.LastSyncTime = &metav1.Time{Time: metav1.Now().Time}
	ok.Status.ObservedGeneration = ok.Generation
	ok.Spec.Suspended = true
	if err := k8s.Status().Update(ctx, ok); err != nil {
		t.Fatal(err)
	}
	var got v1.SpokeCluster
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(ok), &got); err != nil || got.Status.LastSyncTime == nil || got.Spec.Suspended {
		t.Fatalf("status update changed spec or lost status: %v %+v", err, got)
	}
}

func TestSpokePolicyValidation(t *testing.T) {
	ctx := context.Background()
	grp := v1.Subjects{Groups: []string{"g"}, Users: []string{}}
	capSpec := &v1.ClusterAccessPolicySpec{Subjects: grp, Rules: []v1.ClusterRule{{Backends: []string{"metrics"}, Unrestricted: true}}}
	apSpec := &v1.AccessPolicySpec{Subjects: grp, Rules: []v1.Rule{{Backends: []string{"metrics"}}}}
	mk := func(src v1.SpokePolicySource) *v1.SpokePolicy {
		return &v1.SpokePolicy{ObjectMeta: metav1.ObjectMeta{GenerateName: "sp-"}, Spec: v1.SpokePolicySpec{Cluster: "cluster-a", SpokePolicySource: src}}
	}
	okCAP := mk(v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: "x", Generation: 1, ClusterAccessPolicy: capSpec})
	check(t, k8s.Create(ctx, okCAP), "")
	check(t, k8s.Create(ctx, mk(v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Namespace: "ns", Name: "x", Generation: 2, TenantLabel: "t1", AccessPolicy: apSpec})), "")

	tests := []struct {
		name string
		src  v1.SpokePolicySource
		want string
	}{
		{"cap with namespace", v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Namespace: "ns", Name: "x", Generation: 1, ClusterAccessPolicy: capSpec}, "kind, namespace and spec do not agree"},
		{"ap without namespace", v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Name: "x", Generation: 1, AccessPolicy: apSpec}, "kind, namespace and spec do not agree"},
		{"kind and spec differ", v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: "x", Generation: 1, AccessPolicy: apSpec}, "kind, namespace and spec do not agree"},
		{"both specs", v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Namespace: "ns", Name: "x", Generation: 1, AccessPolicy: apSpec, ClusterAccessPolicy: capSpec}, "kind, namespace and spec do not agree"},
		{"no spec", v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: "x", Generation: 1}, "kind, namespace and spec do not agree"},
		{"unknown kind", v1.SpokePolicySource{Kind: "Role", Name: "x", Generation: 1, ClusterAccessPolicy: capSpec}, "Unsupported value"},
		{"no generation", v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: "x", ClusterAccessPolicy: capSpec}, "spec.generation"},
		// The CEL rules of the spoke kinds apply to the nested specs.
		{"nested rule check", v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: "x", Generation: 1,
			ClusterAccessPolicy: &v1.ClusterAccessPolicySpec{Subjects: grp, Rules: []v1.ClusterRule{{Backends: []string{"metrics"}}}}}, "set either unrestricted or matchers"},
		{"nested subjects check", v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Namespace: "ns", Name: "x", Generation: 1,
			AccessPolicy: &v1.AccessPolicySpec{Subjects: v1.Subjects{Groups: []string{}, Users: []string{}}, Rules: apSpec.Rules}}, "at least one group or user required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { check(t, k8s.Create(ctx, mk(tc.src)), tc.want) })
	}
	check(t, k8s.Create(ctx, &v1.SpokePolicy{ObjectMeta: metav1.ObjectMeta{GenerateName: "sp-"},
		Spec: v1.SpokePolicySpec{SpokePolicySource: okCAP.Spec.SpokePolicySource}}), "spec.cluster")

	for name, mut := range map[string]func(*v1.SpokePolicy){
		"cluster":   func(p *v1.SpokePolicy) { p.Spec.Cluster = "cluster-b" },
		"name":      func(p *v1.SpokePolicy) { p.Spec.Name = "y" },
		"kind":      func(p *v1.SpokePolicy) { p.Spec.Kind = v1.SpokeAccessPolicy },
		"namespace": func(p *v1.SpokePolicy) { p.Spec.Namespace = "ns" },
	} {
		upd := okCAP.DeepCopy()
		mut(upd)
		// A namespace on a ClusterAccessPolicy is caught by the kind rule.
		if err := k8s.Update(ctx, upd); err == nil || !(strings.Contains(err.Error(), "immutable") || strings.Contains(err.Error(), "do not agree")) {
			t.Errorf("change of %s: want a rejection, got %v", name, err)
		}
	}
	upd := okCAP.DeepCopy()
	upd.Spec.Generation = 2
	check(t, k8s.Update(ctx, upd), "")

	// The Querdian leader writes status through the subresource.
	upd.Status = v1.SpokePolicyStatus{ObservedGeneration: upd.Generation, SourceGeneration: 2,
		Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Compiled", LastTransitionTime: metav1.Now()}}}
	upd.Spec.Name = "changed-by-status"
	if err := k8s.Status().Update(ctx, upd); err != nil {
		t.Fatal(err)
	}
	var got v1.SpokePolicy
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(upd), &got); err != nil || got.Status.SourceGeneration != 2 || got.Spec.Name != "x" {
		t.Fatalf("status update changed spec or lost status: %v %+v", err, got)
	}
}

func TestSpokePolicyNameValidation(t *testing.T) {
	ctx := context.Background()
	grp := v1.Subjects{Groups: []string{"g"}, Users: []string{}}
	capSpec := &v1.ClusterAccessPolicySpec{Subjects: grp, Rules: []v1.ClusterRule{{Backends: []string{"metrics"}, Unrestricted: true}}}
	apSpec := &v1.AccessPolicySpec{Subjects: grp, Rules: []v1.Rule{{Backends: []string{"metrics"}}}}
	mk := func(src v1.SpokePolicySource) *v1.SpokePolicy {
		return &v1.SpokePolicy{ObjectMeta: metav1.ObjectMeta{GenerateName: "sp-"}, Spec: v1.SpokePolicySpec{Cluster: "cluster-a", SpokePolicySource: src}}
	}
	for _, tc := range []struct {
		name string
		src  v1.SpokePolicySource
		want string
	}{
		{"nul in name", v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: "a\x00b", Generation: 1, ClusterAccessPolicy: capSpec}, "spec.name"},
		{"upper case name", v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: "Admins", Generation: 1, ClusterAccessPolicy: capSpec}, "spec.name"},
		{"dotted name", v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: "team.admins", Generation: 1, ClusterAccessPolicy: capSpec}, ""},
		{"long name", v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: strings.Repeat("a", 254), Generation: 1, ClusterAccessPolicy: capSpec}, "spec.name"},
		{"dotted namespace", v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Namespace: "a.b", Name: "x", Generation: 1, AccessPolicy: apSpec}, "spec.namespace"},
		{"long namespace", v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Namespace: strings.Repeat("a", 64), Name: "x", Generation: 1, AccessPolicy: apSpec}, "spec.namespace"},
		{"long tenant label", v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Namespace: "ns", Name: "x", Generation: 1, TenantLabel: strings.Repeat("t", 64), AccessPolicy: apSpec}, "spec.tenantLabel"},
		{"tenant label not a label value", v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Namespace: "ns", Name: "x", Generation: 1, TenantLabel: "t1|t2", AccessPolicy: apSpec}, "spec.tenantLabel"},
		{"tenant label on cap", v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: "x", Generation: 1, TenantLabel: "t1", ClusterAccessPolicy: capSpec}, "kind, namespace and spec do not agree"},
	} {
		t.Run(tc.name, func(t *testing.T) { check(t, k8s.Create(ctx, mk(tc.src)), tc.want) })
	}
}

// An object read back from the API server, with defaults applied, has the digest of the state the agent sent.
func TestSpokePolicyDigestRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := []v1.SpokePolicySource{
		{Kind: v1.SpokeClusterAccessPolicy, Name: "rt-cap", Generation: 4, ClusterAccessPolicy: &v1.ClusterAccessPolicySpec{
			Subjects: v1.Subjects{Groups: []string{"g"}},
			Rules: []v1.ClusterRule{{Backends: []string{"metrics", "logs"}, Tenants: []string{"t1"},
				Matchers: []v1.Matcher{{Name: "app", Op: v1.MatchNotEqual, Value: ""}, {Name: "env", Op: v1.MatchRegexMatch, Value: "a|b"}},
				Labels:   &v1.LabelFilter{Deny: []string{"customer"}, Redact: []v1.RedactRule{{Name: "ip", Strategy: v1.RedactIPv4Prefix, Octets: octets(2)}}}}},
		}},
		{Kind: v1.SpokeAccessPolicy, Namespace: "ns", Name: "rt-ap", Generation: 1, TenantLabel: "t1", AccessPolicy: &v1.AccessPolicySpec{
			Subjects: v1.Subjects{Users: []string{"u"}},
			Rules:    []v1.Rule{{Backends: []string{"traces"}, Labels: &v1.LabelFilter{}}},
		}},
	}
	want, err := spokesync.Digest(src)
	if err != nil {
		t.Fatal(err)
	}
	var back []v1.SpokePolicySource
	for _, s := range src {
		obj := &v1.SpokePolicy{ObjectMeta: metav1.ObjectMeta{Name: spokesync.ObjectName("rt", &s)}, Spec: v1.SpokePolicySpec{Cluster: "rt", SpokePolicySource: s}}
		if err := k8s.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
		var got v1.SpokePolicy
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(obj), &got); err != nil {
			t.Fatal(err)
		}
		back = append(back, got.Spec.SpokePolicySource)
	}
	if got, err := spokesync.Digest(back); err != nil || got != want {
		t.Fatalf("digest after round trip = %s, %v; want %s", got, err, want)
	}
}

func check(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Fatalf("expected accepted, got %v", err)
	case want != "" && err == nil:
		t.Fatalf("expected rejection containing %q, got none", want)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Fatalf("expected %q in error, got %v", want, err)
	}
}

func octets(n int32) *int32 { return &n }

func redactRule(rs ...v1.RedactRule) v1.ClusterRule {
	return v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true, Labels: &v1.LabelFilter{Redact: rs}}
}
