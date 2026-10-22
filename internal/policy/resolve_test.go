package policy

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

func snap(t testing.TB, cps []v1.ClusterAccessPolicy, aps []v1.AccessPolicy, ns map[string]map[string]string) *Snapshot {
	t.Helper()
	cfg := testConfig(t)
	return NewSnapshot(Compile(Input{Config: cfg, ClusterPolicies: cps, Policies: aps, NamespaceLabels: ns}), cfg, 7)
}

func ns(n string) v1.Matcher { return eq("namespace", n) }

func TestResolveNoGrant(t *testing.T) {
	s := snap(t, []v1.ClusterAccessPolicy{
		capol("p", v1.Subjects{Groups: []string{"g"}}, v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{ns("a")}}),
	}, nil, nil)
	for name, call := range map[string]func() error{
		"other user":       func() error { _, e := s.Resolve("u", nil, "metrics"); return e },
		"other group":      func() error { _, e := s.Resolve("u", []string{"x"}, "metrics"); return e },
		"other backend":    func() error { _, e := s.Resolve("u", []string{"g"}, "logs"); return e },
		"user named group": func() error { _, e := s.Resolve("g", nil, "metrics"); return e },
		"empty identity":   func() error { _, e := s.Resolve("", nil, "metrics"); return e },
	} {
		if err := call(); !errors.Is(err, ErrNoGrant) {
			t.Errorf("%s: want ErrNoGrant, got %v", name, err)
		}
	}
	if _, err := s.Resolve("u", []string{"g"}, "nope"); !errors.Is(err, ErrUnknownBackend) {
		t.Errorf("want ErrUnknownBackend, got %v", err)
	}
	if _, err := s.Resolve("u", []string{"g"}, "metrics"); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSingleRuleAndGroups(t *testing.T) {
	s := snap(t, []v1.ClusterAccessPolicy{
		capol("p", v1.Subjects{Groups: []string{"g"}}, v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{ns("a")},
			Labels: &v1.LabelFilter{Deny: []string{"secret"}}}),
	}, nil, nil)
	eff, err := s.Resolve("u", []string{"g", "g"}, "metrics")
	if err != nil {
		t.Fatal(err)
	}
	if eff.Unrestricted || render(eff.Terms) != "namespace=a" || len(eff.Tenants) != 0 || !eff.Disjoint {
		t.Fatalf("unexpected: %+v", eff)
	}
	if strings.Join(eff.DeniedLabels, ",") != "secret" || strings.Join(eff.Policies, ",") != "ClusterAccessPolicy/p" {
		t.Fatalf("unexpected: %+v", eff)
	}
}

func TestResolveMultipleRulesSameTenant(t *testing.T) {
	s := snap(t, []v1.ClusterAccessPolicy{
		capol("a", v1.Subjects{Groups: []string{"g1"}}, v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eq("k8s_namespace_name", "a")}}),
		capol("b", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eq("k8s_namespace_name", "b")}}),
	}, nil, nil)
	eff, err := s.Resolve("u", []string{"g1"}, "logs")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(eff.Tenants, ",") != "t1" || render(eff.Terms) != "k8s_namespace_name=~(?:a)|(?:b)" {
		t.Fatalf("unexpected: tenants=%v terms=%s", eff.Tenants, render(eff.Terms))
	}
	for _, tm := range eff.Terms {
		if tm.Tenant != "" {
			t.Error("single tenant must not bind terms")
		}
	}
}

func TestResolveMultiTenantIdenticalTerms(t *testing.T) {
	// traces has no federation support; identical terms still work.
	s := snap(t, []v1.ClusterAccessPolicy{
		capol("a", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"t1", "t2"}, Matchers: []v1.Matcher{eq("resource.k8s.namespace.name", "x")}}),
	}, nil, nil)
	eff, err := s.Resolve("u", nil, "traces")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(eff.Tenants, ",") != "t1,t2" || len(eff.Terms) != 1 || eff.Terms[0].Tenant != "" ||
		render(eff.Terms) != "resource.k8s.namespace.name=x" {
		t.Fatalf("unexpected: %+v", eff)
	}
	for _, m := range eff.Terms[0].Matchers {
		if m.Name == TenantIDLabel {
			t.Fatal("no tenant binding expected for identical terms")
		}
	}
}

func differingTenants(backend, label string) []v1.ClusterAccessPolicy {
	return []v1.ClusterAccessPolicy{
		capol("a", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{backend}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eq(label, "x")}}),
		capol("b", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{backend}, Tenants: []string{"t2"}, Matchers: []v1.Matcher{eq(label, "y")}}),
	}
}

func TestResolveMultiTenantWithFederation(t *testing.T) {
	s := snap(t, differingTenants("logs", "k8s_namespace_name"), nil, nil)
	eff, err := s.Resolve("u", nil, "logs")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(eff.Tenants, ",") != "t1,t2" || len(eff.Terms) != 2 || !eff.Disjoint {
		t.Fatalf("unexpected: %+v", eff)
	}
	got := render(eff.Terms)
	want := "t1:__tenant_id__=t1,k8s_namespace_name=x | t2:__tenant_id__=t2,k8s_namespace_name=y"
	if got != want {
		t.Fatalf("terms = %q, want %q", got, want)
	}
}

func TestResolveMultiTenantWithoutFederationRejects(t *testing.T) {
	s := snap(t, differingTenants("traces", "resource.k8s.namespace.name"), nil, nil)
	if _, err := s.Resolve("u", nil, "traces"); !errors.Is(err, ErrCrossTenantUnion) {
		t.Fatalf("want ErrCrossTenantUnion, got %v", err)
	}
	// A user who only holds one of the two tenants is fine.
	s2 := snap(t, differingTenants("traces", "resource.k8s.namespace.name")[:1], nil, nil)
	if _, err := s2.Resolve("u", nil, "traces"); err != nil {
		t.Fatal(err)
	}
}

func TestResolveUnrestrictedRestrictedMix(t *testing.T) {
	// Same tenant: unrestricted absorbs the restricted rule.
	s := snap(t, []v1.ClusterAccessPolicy{
		capol("all", v1.Subjects{Groups: []string{"admins"}}, v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Unrestricted: true}),
		capol("one", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eq("k8s_namespace_name", "a")}}),
	}, nil, nil)
	eff, err := s.Resolve("u", []string{"admins"}, "logs")
	if err != nil || !eff.Unrestricted || len(eff.Terms) != 0 || strings.Join(eff.Tenants, ",") != "t1" {
		t.Fatalf("unexpected: %+v %v", eff, err)
	}
	// Without the admin group only the restricted rule applies.
	eff, err = s.Resolve("u", nil, "logs")
	if err != nil || eff.Unrestricted || render(eff.Terms) != "k8s_namespace_name=a" {
		t.Fatalf("unexpected: %+v %v", eff, err)
	}

	// Different tenants: unrestricted on t1 must NOT widen the grant on t2.
	s = snap(t, []v1.ClusterAccessPolicy{
		capol("all", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Unrestricted: true}),
		capol("one", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t2"}, Matchers: []v1.Matcher{eq("k8s_namespace_name", "a")}}),
	}, nil, nil)
	eff, err = s.Resolve("u", nil, "logs")
	if err != nil || eff.Unrestricted {
		t.Fatalf("must stay restricted: %+v %v", eff, err)
	}
	if got := render(eff.Terms); got != "t1:__tenant_id__=t1 | t2:__tenant_id__=t2,k8s_namespace_name=a" {
		t.Fatalf("terms = %q", got)
	}
	// ... and on a backend without federation the mix is rejected.
	s = snap(t, []v1.ClusterAccessPolicy{
		capol("all", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"t1"}, Unrestricted: true}),
		capol("one", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"t2"}, Matchers: []v1.Matcher{eq("resource.a", "b")}}),
	}, nil, nil)
	if _, err = s.Resolve("u", nil, "traces"); !errors.Is(err, ErrCrossTenantUnion) {
		t.Fatalf("want ErrCrossTenantUnion, got %v", err)
	}
}

func TestResolveDeniedLabelsUnion(t *testing.T) {
	s := snap(t, []v1.ClusterAccessPolicy{
		capol("a", v1.Subjects{Groups: []string{"g1"}}, v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{ns("a")}, Labels: &v1.LabelFilter{Deny: []string{"x", "y"}}}),
		capol("b", v1.Subjects{Groups: []string{"g2"}}, v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true, Labels: &v1.LabelFilter{Deny: []string{"y", "z"}}}),
		capol("c", v1.Subjects{Groups: []string{"g3"}}, v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{ns("c")}}),
	}, nil, nil)
	eff, err := s.Resolve("u", []string{"g1", "g2", "g3"}, "metrics")
	if err != nil {
		t.Fatal(err)
	}
	// Conservative: denied by any matched rule means denied for the request, even if another rule has none.
	if strings.Join(eff.DeniedLabels, ",") != "x,y,z" || !eff.Unrestricted {
		t.Fatalf("unexpected: %+v", eff)
	}
	if len(eff.Policies) != 3 {
		t.Fatalf("policies = %v", eff.Policies)
	}
}

func TestResolveAccessPolicyTenantFromNamespace(t *testing.T) {
	s := snap(t, nil, []v1.AccessPolicy{
		ap("order", "p", v1.Subjects{Groups: []string{"team"}}, v1.Rule{Backends: []string{"logs"}}),
		ap("billing", "p", v1.Subjects{Groups: []string{"team"}}, v1.Rule{Backends: []string{"logs"}}),
	}, map[string]map[string]string{"order": {TenantLabel: "t1"}, "billing": {TenantLabel: "t2"}})
	eff, err := s.Resolve("u", []string{"team"}, "logs")
	if err != nil {
		t.Fatal(err)
	}
	want := "t1:__tenant_id__=t1,k8s_namespace_name=order | t2:__tenant_id__=t2,k8s_namespace_name=billing"
	if strings.Join(eff.Tenants, ",") != "t1,t2" || render(eff.Terms) != want {
		t.Fatalf("terms = %q tenants=%v", render(eff.Terms), eff.Tenants)
	}
}

func TestResolveTooComplex(t *testing.T) {
	var cps []v1.ClusterAccessPolicy
	for i := 0; i < 20; i++ {
		cps = append(cps, capol(fmt.Sprintf("p%d", i), v1.Subjects{Users: []string{"u"}},
			v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{{Name: "namespace", Op: v1.MatchNotEqual, Value: fmt.Sprint(i)}}}))
	}
	s := snap(t, cps, nil, nil)
	if _, err := s.Resolve("u", nil, "metrics"); !errors.Is(err, ErrTooComplex) {
		t.Fatalf("want ErrTooComplex, got %v", err)
	}
}

func TestSnapshotImmutableAndResults(t *testing.T) {
	c := Compile(Input{Config: testConfig(t), ClusterPolicies: []v1.ClusterAccessPolicy{
		capol("b", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true}),
		capol("a", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{"nope"}, Unrestricted: true}),
	}})
	s := NewSnapshot(c, testConfig(t), 9)
	c.ByUser["u"] = nil // later mutation of the input must not affect the snapshot
	if _, err := s.Resolve("u", nil, "metrics"); err != nil {
		t.Fatal(err)
	}
	r := s.Results()
	if s.Generation != 9 || len(r) != 2 || r[0].Name != "a" || r[1].Name != "b" {
		t.Fatalf("results not sorted: %+v", r)
	}
}

func BenchmarkResolve(b *testing.B) {
	var cps []v1.ClusterAccessPolicy
	for i := 0; i < 1000; i++ {
		cps = append(cps, capol(fmt.Sprintf("p%d", i), v1.Subjects{Groups: []string{fmt.Sprintf("g%d", i)}},
			v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{ns(fmt.Sprintf("ns%d", i)), eq("team", "t")}}))
	}
	s := snap(b, cps, nil, nil)
	groups := make([]string, 20)
	for i := range groups {
		groups[i] = fmt.Sprintf("g%d", i*37)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Resolve("user", groups, "metrics"); err != nil {
			b.Fatal(err)
		}
	}
}

func TestResolveIndependentOfOrder(t *testing.T) {
	var cps []v1.ClusterAccessPolicy
	for _, n := range []string{"a", "b", "c", "d"} {
		cps = append(cps, capol("p"+n, v1.Subjects{Groups: []string{"g" + n}},
			v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{ns(n), eq("team", "t")}}))
	}
	s := snap(t, cps, nil, nil)
	first, err := s.Resolve("u", []string{"ga", "gb", "gc", "gd"}, "metrics")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Resolve("u", []string{"gd", "gb", "ga", "gc"}, "metrics")
	if err != nil {
		t.Fatal(err)
	}
	if render(first.Terms) != render(second.Terms) || strings.Join(first.Policies, ",") != strings.Join(second.Policies, ",") {
		t.Fatalf("order dependent:\n%s\n%s", render(first.Terms), render(second.Terms))
	}
	if len(first.Terms) != 1 {
		t.Fatalf("want a single merged term, got %s", render(first.Terms))
	}
}

func TestResolveRedactionsStrictest(t *testing.T) {
	s := snap(t, []v1.ClusterAccessPolicy{
		capol("a", v1.Subjects{Groups: []string{"g1"}}, v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{ns("a")},
			Labels: &v1.LabelFilter{Redact: []v1.RedactRule{{Name: "c", Strategy: v1.RedactHash}, {Name: "ip", Strategy: v1.RedactIPv4Prefix, Octets: i32(3)}}}}),
		capol("b", v1.Subjects{Groups: []string{"g2"}}, v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{ns("b")},
			Labels: &v1.LabelFilter{Deny: []string{"c"}, Redact: []v1.RedactRule{{Name: "ip", Strategy: v1.RedactIPv4Prefix, Octets: i32(1)}}}}),
		capol("c", v1.Subjects{Groups: []string{"g3"}}, v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{ns("c")}}),
	}, nil, nil)
	eff, err := s.Resolve("u", []string{"g1", "g2", "g3"}, "metrics")
	if err != nil {
		t.Fatal(err)
	}
	// A rule that only denies c does not weaken another rule's redaction of c.
	if eff.Redactions["c"] != (Redaction{Strategy: v1.RedactHash}) || eff.Redactions["ip"] != (Redaction{Strategy: v1.RedactIPv4Prefix, Octets: 1}) {
		t.Fatalf("redactions = %+v", eff.Redactions)
	}
	if strings.Join(eff.DeniedLabels, ",") != "c,ip" {
		t.Fatalf("denied = %v", eff.DeniedLabels)
	}
	eff, err = s.Resolve("u", []string{"g3"}, "metrics")
	if err != nil || eff.Redactions != nil {
		t.Fatalf("no redactions expected: %+v, %v", eff.Redactions, err)
	}
}
