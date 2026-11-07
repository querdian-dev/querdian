package policy

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

const hubName = "mla"

// mcConfig is the example config with the multicluster block. The cluster label differs for each backend.
func mcConfig(t testing.TB) *config.Config {
	t.Helper()
	cfg := testConfig(t)
	d := config.Duration(48 * time.Hour)
	cfg.Multicluster = &config.Multicluster{HubCluster: hubName, MaxStaleness: &d}
	labels := map[string]string{"metrics": "cluster", "logs": "k8s_cluster_name", "traces": "resource.k8s.cluster.name"}
	for i := range cfg.Backends {
		cfg.Backends[i].ClusterLabel = labels[cfg.Backends[i].Name]
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func spokeCluster(name, identity string, tenants ...string) v1.SpokeCluster {
	return v1.SpokeCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec:       v1.SpokeClusterSpec{Identity: identity, Tenants: tenants},
	}
}

func agentID(name string) string { return "spiffe://mla.example.com/agent/" + name }

func spokePolicy(cluster string, src v1.SpokePolicySource) v1.SpokePolicy {
	src.Generation = 2
	return v1.SpokePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: spokesync.ObjectName(cluster, &src), Generation: 7},
		Spec:       v1.SpokePolicySpec{Cluster: cluster, SpokePolicySource: src},
	}
}

func spokeCAP(cluster, name string, subj v1.Subjects, rules ...v1.ClusterRule) v1.SpokePolicy {
	return spokePolicy(cluster, v1.SpokePolicySource{
		Kind: v1.SpokeClusterAccessPolicy, Name: name,
		ClusterAccessPolicy: &v1.ClusterAccessPolicySpec{Subjects: subj, Rules: rules},
	})
}

func spokeAP(cluster, ns, name, tenantLabel string, subj v1.Subjects, rules ...v1.Rule) v1.SpokePolicy {
	return spokePolicy(cluster, v1.SpokePolicySource{
		Kind: v1.SpokeAccessPolicy, Namespace: ns, Name: name, TenantLabel: tenantLabel,
		AccessPolicy: &v1.AccessPolicySpec{Subjects: subj, Rules: rules},
	})
}

func spokeResult(t *testing.T, c *Compiled, p v1.SpokePolicy) Result {
	t.Helper()
	return result(t, c, KindSpokePolicy, p.Name)
}

func names(ms []Matcher) string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.k
	}
	return strings.Join(out, " & ")
}

var (
	gSubj   = v1.Subjects{Groups: []string{"g"}}
	spokeA  = spokeCluster("cluster-a", agentID("cluster-a"), "tenant-a", "shared")
	spokeB  = spokeCluster("cluster-b", agentID("cluster-b"), "tenant-b")
	metrics = []string{"metrics"}
)

func TestSpokeAccessPolicyCarriesClusterAndNamespace(t *testing.T) {
	cfg := mcConfig(t)
	p := spokeAP("cluster-a", "order", "p", "tenant-a", gSubj, v1.Rule{Backends: []string{"metrics", "logs", "traces"}})
	c := Compile(Input{Config: cfg, SpokeClusters: []v1.SpokeCluster{spokeA}, SpokePolicies: []v1.SpokePolicy{p}})
	r := spokeResult(t, c, p)
	if !r.Ready || r.Reason != ReasonCompiled || r.Generation != 7 || r.Namespace != "" {
		t.Fatalf("an AccessPolicy rule with no matchers stays valid; got %+v", r)
	}
	want := map[string]string{
		"metrics": `"namespace" Equal "order" & "cluster" Equal "cluster-a"`,
		"logs":    `"k8s_namespace_name" Equal "order" & "k8s_cluster_name" Equal "cluster-a"`,
		"traces":  `"resource.k8s.namespace.name" Equal "order" & "resource.k8s.cluster.name" Equal "cluster-a"`,
	}
	rules := c.ByGroup["g"]
	if len(rules) != 3 {
		t.Fatalf("want 3 rules, got %d", len(rules))
	}
	for _, cr := range rules {
		if got := names(cr.Matchers); got != want[cr.Backend] {
			t.Errorf("%s: matchers = %s, want %s", cr.Backend, got, want[cr.Backend])
		}
		if cr.Origin != "cluster-a" || cr.Unrestricted || cr.Policy != "SpokePolicy/"+p.Name {
			t.Errorf("%s: origin %q, unrestricted %v, policy %q", cr.Backend, cr.Origin, cr.Unrestricted, cr.Policy)
		}
		if cr.Backend != "metrics" && strings.Join(cr.Tenants, ",") != "tenant-a" {
			t.Errorf("%s: tenants = %v", cr.Backend, cr.Tenants)
		}
	}
}

func TestSpokeUserMatchersAndedWithClusterMatcher(t *testing.T) {
	cfg := mcConfig(t)
	p := spokeCAP("cluster-a", "p", gSubj, v1.ClusterRule{Backends: metrics, Matchers: []v1.Matcher{eq("cluster", "cluster-b"), eq("job", "x")}})
	c := Compile(Input{Config: cfg, SpokeClusters: []v1.SpokeCluster{spokeA}, SpokePolicies: []v1.SpokePolicy{p}})
	if r := spokeResult(t, c, p); !r.Ready {
		t.Fatalf("got %+v", r)
	}
	cr := c.ByGroup["g"][0]
	if got := names(cr.Matchers); got != `"cluster" Equal "cluster-a" & "cluster" Equal "cluster-b" & "job" Equal "x"` {
		t.Fatalf("own cluster matchers must stay ANDed with the implicit matcher: %s", got)
	}
	// The snapshot gives no data of cluster-b: the term needs cluster="cluster-a" and cluster="cluster-b".
	eff, err := NewSnapshot(c, cfg, 1).Resolve("", []string{"g"}, "metrics")
	if err != nil {
		t.Fatal(err)
	}
	if matchTerms(eff.Terms, "", map[string]string{"cluster": "cluster-b", "job": "x"}) {
		t.Fatalf("label set of cluster-b matches: %s", render(eff.Terms))
	}
}

func TestSpokeUnrestrictedIsNeverUnrestricted(t *testing.T) {
	cfg := mcConfig(t)
	p := spokeCAP("cluster-a", "p", gSubj, v1.ClusterRule{Backends: []string{"metrics", "logs"}, Tenants: []string{"tenant-a"}, Unrestricted: true})
	c := Compile(Input{Config: cfg, SpokeClusters: []v1.SpokeCluster{spokeA}, SpokePolicies: []v1.SpokePolicy{p}})
	if r := spokeResult(t, c, p); !r.Ready {
		t.Fatalf("got %+v", r)
	}
	for _, cr := range c.ByGroup["g"] {
		if cr.Unrestricted || len(cr.Matchers) != 1 || cr.Matchers[0].Op != v1.MatchEqual || cr.Matchers[0].Value != "cluster-a" {
			t.Fatalf("%s: spoke unrestricted must compile to the single cluster matcher: %+v", cr.Backend, cr)
		}
	}
	snap := NewSnapshot(c, cfg, 1)
	for b, label := range map[string]string{"metrics": "cluster", "logs": "k8s_cluster_name"} {
		eff, err := snap.Resolve("", []string{"g"}, b)
		if err != nil {
			t.Fatal(err)
		}
		want := `"` + label + `" Equal "cluster-a"`
		if eff.Unrestricted || len(eff.Terms) != 1 || names(eff.Terms[0].Matchers) != want {
			t.Fatalf("%s: effective = %+v, want the single term %s", b, eff, want)
		}
	}
}

func TestClusterRuleWithoutMatchersIsInvalid(t *testing.T) {
	empty := v1.ClusterRule{Backends: metrics}
	for name, cfg := range map[string]*config.Config{"single cluster": testConfig(t), "multicluster": mcConfig(t)} {
		c := Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{capol("p", gSubj, empty)}})
		if r := result(t, c, "ClusterAccessPolicy", "p"); r.Ready || r.Reason != ReasonInvalidMatcher || len(c.ByGroup["g"]) != 0 {
			t.Errorf("%s: hub rule with unrestricted: false and no matchers: %+v", name, r)
		}
	}
	p := spokeCAP("cluster-a", "p", gSubj, empty)
	c := Compile(Input{Config: mcConfig(t), SpokeClusters: []v1.SpokeCluster{spokeA}, SpokePolicies: []v1.SpokePolicy{p}})
	if r := spokeResult(t, c, p); r.Ready || r.Reason != ReasonInvalidMatcher || len(c.ByGroup["g"]) != 0 {
		t.Errorf("spoke rule with unrestricted: false and no matchers: %+v", r)
	}
}

func TestSpokePolicyGrantsNothing(t *testing.T) {
	good := spokeCAP("cluster-a", "p", gSubj, v1.ClusterRule{Backends: metrics, Unrestricted: true})
	renamed := good
	renamed.Name = "sp-other"
	wrongKind := spokeCAP("cluster-a", "p", gSubj, v1.ClusterRule{Backends: metrics, Unrestricted: true})
	wrongKind.Spec.Namespace = "ns"
	wrongKind.Name = spokesync.ObjectName("cluster-a", &wrongKind.Spec.SpokePolicySource)
	noSpec := spokePolicy("cluster-a", v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Namespace: "ns", Name: "p"})
	unknownKind := spokePolicy("cluster-a", v1.SpokePolicySource{Kind: "Other", Name: "p",
		ClusterAccessPolicy: &v1.ClusterAccessPolicySpec{Subjects: gSubj, Rules: []v1.ClusterRule{{Backends: metrics, Unrestricted: true}}}})
	badNS := spokeAP("cluster-a", "Bad/NS", "p", "", gSubj, v1.Rule{Backends: metrics})
	hub := spokeCAP(hubName, "p", gSubj, v1.ClusterRule{Backends: metrics, Unrestricted: true})
	noCluster := spokeCAP("", "p", gSubj, v1.ClusterRule{Backends: metrics, Unrestricted: true})
	badCluster := spokeCAP("Cluster_A", "p", gSubj, v1.ClusterRule{Backends: metrics, Unrestricted: true})
	longCluster := spokeCAP(strings.Repeat("a", 64), "p", gSubj, v1.ClusterRule{Backends: metrics, Unrestricted: true})
	badName := spokeCAP("cluster-a", "P/1", gSubj, v1.ClusterRule{Backends: metrics, Unrestricted: true})
	capTenant := spokePolicy("cluster-a", v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: "p", TenantLabel: "tenant-a",
		ClusterAccessPolicy: &v1.ClusterAccessPolicySpec{Subjects: gSubj, Rules: []v1.ClusterRule{{Backends: metrics, Unrestricted: true}}}})

	tests := []struct {
		name     string
		cfg      *config.Config
		clusters []v1.SpokeCluster
		pol      v1.SpokePolicy
		reason   string
	}{
		{"no multicluster block", testConfig(t), []v1.SpokeCluster{spokeA}, good, ReasonMulticlusterDisabled},
		{"no SpokeCluster", mcConfig(t), nil, good, ReasonSpokeClusterNotFound},
		{"SpokeCluster of a different name", mcConfig(t), []v1.SpokeCluster{spokeB}, good, ReasonSpokeClusterNotFound},
		{"SpokeCluster with the hub name", mcConfig(t), []v1.SpokeCluster{spokeCluster(hubName, agentID(hubName))}, hub, ReasonSpokeClusterInvalid},
		{"identity suffix is not a full segment", mcConfig(t), []v1.SpokeCluster{spokeCluster("cluster-a", agentID("x-cluster-a"))}, good, ReasonSpokeClusterInvalid},
		{"identity of a different spoke", mcConfig(t), []v1.SpokeCluster{spokeCluster("cluster-a", agentID("cluster-b"))}, good, ReasonSpokeClusterInvalid},
		{"identity with a query", mcConfig(t), []v1.SpokeCluster{spokeCluster("cluster-a", agentID("cluster-a")+"?x=1")}, good, ReasonSpokeClusterInvalid},
		{"identity without a path", mcConfig(t), []v1.SpokeCluster{spokeCluster("cluster-a", "cluster-a")}, good, ReasonSpokeClusterInvalid},
		{"identity with an escape", mcConfig(t), []v1.SpokeCluster{spokeCluster("cluster-a", "spiffe://mla.example.com/agent%2Fx/cluster-a")}, good, ReasonSpokeClusterInvalid},
		{"identity without a host", mcConfig(t), []v1.SpokeCluster{spokeCluster("cluster-a", "spiffe:///agent/cluster-a")}, good, ReasonSpokeClusterInvalid},
		{"invalid cluster name", mcConfig(t), []v1.SpokeCluster{spokeCluster("Cluster_A", agentID("Cluster_A"))}, badCluster, ReasonInvalidSpokePolicy},
		{"cluster name too long", mcConfig(t), []v1.SpokeCluster{spokeCluster(strings.Repeat("a", 64), agentID(strings.Repeat("a", 64)))}, longCluster, ReasonInvalidSpokePolicy},
		{"invalid source name", mcConfig(t), []v1.SpokeCluster{spokeA}, badName, ReasonInvalidSpokePolicy},
		{"tenantLabel on a ClusterAccessPolicy", mcConfig(t), []v1.SpokeCluster{spokeA}, capTenant, ReasonInvalidSpokePolicy},
		{"duplicate SpokeCluster", mcConfig(t), []v1.SpokeCluster{spokeA, spokeA}, good, ReasonSpokeClusterInvalid},
		{"object name not from the receiver", mcConfig(t), []v1.SpokeCluster{spokeA}, renamed, ReasonInvalidSpokePolicy},
		{"kind and namespace disagree", mcConfig(t), []v1.SpokeCluster{spokeA}, wrongKind, ReasonInvalidSpokePolicy},
		{"no spec", mcConfig(t), []v1.SpokeCluster{spokeA}, noSpec, ReasonInvalidSpokePolicy},
		{"unknown kind", mcConfig(t), []v1.SpokeCluster{spokeA}, unknownKind, ReasonInvalidSpokePolicy},
		{"invalid namespace", mcConfig(t), []v1.SpokeCluster{spokeA}, badNS, ReasonInvalidSpokePolicy},
		{"empty cluster", mcConfig(t), []v1.SpokeCluster{spokeA}, noCluster, ReasonInvalidSpokePolicy},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Compile(Input{Config: tc.cfg, SpokeClusters: tc.clusters, SpokePolicies: []v1.SpokePolicy{tc.pol}})
			r := spokeResult(t, c, tc.pol)
			if r.Ready || r.Reason != tc.reason || len(c.ByGroup["g"]) != 0 {
				t.Fatalf("got %+v, want reason %s and no grant", r, tc.reason)
			}
		})
	}
	// The same policy is active with a valid SpokeCluster.
	c := Compile(Input{Config: mcConfig(t), SpokeClusters: []v1.SpokeCluster{spokeA, spokeB}, SpokePolicies: []v1.SpokePolicy{good}})
	if r := spokeResult(t, c, good); !r.Ready || len(c.ByGroup["g"]) != 1 {
		t.Fatalf("got %+v", r)
	}
}

func TestHubAccessPolicyClusterMatcher(t *testing.T) {
	pol := ap("order", "p", gSubj, v1.Rule{Backends: metrics, Matchers: []v1.Matcher{eq("job", "x")}})
	c := Compile(Input{Config: mcConfig(t), Policies: []v1.AccessPolicy{pol}})
	cr := c.ByGroup["g"][0]
	if got := names(cr.Matchers); got != `"namespace" Equal "order" & "cluster" Equal "mla" & "job" Equal "x"` || cr.Origin != "" {
		t.Fatalf("hub AccessPolicy under multicluster: %s (origin %q)", got, cr.Origin)
	}
	c = Compile(Input{Config: testConfig(t), Policies: []v1.AccessPolicy{pol}})
	if got := names(c.ByGroup["g"][0].Matchers); got != `"namespace" Equal "order" & "job" Equal "x"` {
		t.Fatalf("without multicluster nothing changes: %s", got)
	}
	// The compiler does not accept an empty hub cluster, also if the config was not validated.
	cfg := mcConfig(t)
	cfg.Multicluster.HubCluster = ""
	c = Compile(Input{Config: cfg, Policies: []v1.AccessPolicy{pol}})
	if r := result(t, c, "AccessPolicy", "p"); r.Ready || r.Reason != ReasonInvalidClusterScope {
		t.Fatalf("got %+v", r)
	}
}

func TestHubClusterPolicyScope(t *testing.T) {
	logsT := []string{"t1"}
	m := func(n string, op v1.MatchOp, v string) v1.Matcher { return v1.Matcher{Name: n, Op: op, Value: v} }
	tests := []struct {
		name string
		rule v1.ClusterRule
		ok   bool
	}{
		{"Equal on cluster", v1.ClusterRule{Backends: metrics, Matchers: []v1.Matcher{eq("cluster", "a")}}, true},
		{"RegexMatch on cluster", v1.ClusterRule{Backends: metrics, Matchers: []v1.Matcher{m("cluster", v1.MatchRegexMatch, "a|b")}}, true},
		{"Equal and NotEqual on cluster", v1.ClusterRule{Backends: metrics, Matchers: []v1.Matcher{eq("cluster", "a"), m("cluster", v1.MatchNotEqual, "b")}}, true},
		{"NotEqual on cluster", v1.ClusterRule{Backends: metrics, Matchers: []v1.Matcher{m("cluster", v1.MatchNotEqual, "a")}}, false},
		{"RegexNoMatch on cluster", v1.ClusterRule{Backends: metrics, Matchers: []v1.Matcher{m("cluster", v1.MatchRegexNoMatch, "a")}}, false},
		{"no cluster matcher", v1.ClusterRule{Backends: metrics, Matchers: []v1.Matcher{eq("namespace", "x")}}, false},
		{"matcher on the cluster label of a different backend", v1.ClusterRule{Backends: metrics, Matchers: []v1.Matcher{eq("k8s_cluster_name", "a")}}, false},
		{"allClusters", v1.ClusterRule{Backends: metrics, AllClusters: true, Matchers: []v1.Matcher{eq("namespace", "x")}}, true},
		{"allClusters with Equal on cluster", v1.ClusterRule{Backends: metrics, AllClusters: true, Matchers: []v1.Matcher{eq("cluster", "a")}}, false},
		{"allClusters with NotEqual on cluster", v1.ClusterRule{Backends: metrics, AllClusters: true, Matchers: []v1.Matcher{m("cluster", v1.MatchNotEqual, "a")}}, false},
		{"unrestricted", v1.ClusterRule{Backends: metrics, Unrestricted: true}, true},
		{"unrestricted with allClusters", v1.ClusterRule{Backends: metrics, Unrestricted: true, AllClusters: true}, false},
		{"several backends, one without scope", v1.ClusterRule{Backends: []string{"metrics", "logs"}, Tenants: logsT, Matchers: []v1.Matcher{eq("cluster", "a")}}, false},
		{"several backends, each with scope", v1.ClusterRule{Backends: []string{"metrics", "logs"}, Tenants: logsT, Matchers: []v1.Matcher{eq("cluster", "a"), eq("k8s_cluster_name", "a")}}, true},
		{"several backends, allClusters", v1.ClusterRule{Backends: []string{"metrics", "logs"}, Tenants: logsT, AllClusters: true, Matchers: []v1.Matcher{eq("job", "a")}}, true},
		{"several backends, allClusters with a cluster matcher on one", v1.ClusterRule{Backends: []string{"metrics", "logs"}, Tenants: logsT, AllClusters: true, Matchers: []v1.Matcher{eq("k8s_cluster_name", "a")}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := capol("p", gSubj, tc.rule)
			c := Compile(Input{Config: mcConfig(t), ClusterPolicies: []v1.ClusterAccessPolicy{p}})
			r := result(t, c, "ClusterAccessPolicy", "p")
			if tc.ok {
				if !r.Ready || r.Reason != ReasonCompiled {
					t.Fatalf("got %+v", r)
				}
				for _, cr := range c.ByGroup["g"] {
					if cr.Origin != "" || cr.Unrestricted != tc.rule.Unrestricted || len(cr.Matchers) != len(tc.rule.Matchers) {
						t.Fatalf("hub rule must keep its own matchers only: %+v", cr)
					}
				}
				return
			}
			if r.Ready || r.Reason != ReasonInvalidClusterScope || len(c.ByGroup["g"]) != 0 {
				t.Fatalf("got %+v, want InvalidClusterScope and no grant", r)
			}
			// Without the multicluster block, the scope rule and allClusters have no effect.
			c = Compile(Input{Config: testConfig(t), ClusterPolicies: []v1.ClusterAccessPolicy{capol("p", gSubj, tc.rule)}})
			if r := result(t, c, "ClusterAccessPolicy", "p"); !r.Ready || r.Reason != ReasonCompiled {
				t.Fatalf("without multicluster: got %+v", r)
			}
		})
	}
}

func TestSpokeAllClustersIsInvalid(t *testing.T) {
	for name, rule := range map[string]v1.ClusterRule{
		"with matchers":     {Backends: metrics, AllClusters: true, Matchers: []v1.Matcher{eq("job", "x")}},
		"with unrestricted": {Backends: metrics, AllClusters: true, Unrestricted: true},
	} {
		p := spokeCAP("cluster-a", "p", gSubj, rule)
		c := Compile(Input{Config: mcConfig(t), SpokeClusters: []v1.SpokeCluster{spokeA}, SpokePolicies: []v1.SpokePolicy{p}})
		if r := spokeResult(t, c, p); r.Ready || r.Reason != ReasonInvalidClusterScope || len(c.ByGroup["g"]) != 0 {
			t.Errorf("%s: got %+v", name, r)
		}
	}
}

func TestSpokeTenantAllowList(t *testing.T) {
	withDefault := spokeA
	withDefault.Spec.DefaultTenant = "shared"
	noTenants := spokeCluster("cluster-a", agentID("cluster-a"))
	logs := []string{"logs"}
	tests := []struct {
		name    string
		cluster v1.SpokeCluster
		pol     v1.SpokePolicy
		hubNS   map[string]map[string]string
		reason  string
		tenants string
	}{
		{"CAP tenants in the allow-list", spokeA, spokeCAP("cluster-a", "p", gSubj, v1.ClusterRule{Backends: logs, Tenants: []string{"shared", "tenant-a"}, Unrestricted: true}), nil, ReasonCompiled, "shared,tenant-a"},
		{"CAP tenant not in the allow-list", spokeA, spokeCAP("cluster-a", "p", gSubj, v1.ClusterRule{Backends: logs, Tenants: []string{"tenant-a", "tenant-b"}, Unrestricted: true}), nil, ReasonTenantNotAllowed, ""},
		{"CAP with an empty allow-list", noTenants, spokeCAP("cluster-a", "p", gSubj, v1.ClusterRule{Backends: logs, Tenants: []string{"tenant-a"}, Unrestricted: true}), nil, ReasonTenantNotAllowed, ""},
		{"CAP empty tenant", spokeA, spokeCAP("cluster-a", "p", gSubj, v1.ClusterRule{Backends: logs, Tenants: []string{""}, Unrestricted: true}), nil, ReasonTenantNotAllowed, ""},
		{"CAP backend without tenant header", noTenants, spokeCAP("cluster-a", "p", gSubj, v1.ClusterRule{Backends: metrics, Tenants: []string{"x"}, Unrestricted: true}), nil, ReasonCompiled, ""},
		{"AP tenant label in the allow-list", spokeA, spokeAP("cluster-a", "order", "p", "tenant-a", gSubj, v1.Rule{Backends: logs}), nil, ReasonCompiled, "tenant-a"},
		{"AP tenant label not in the allow-list", withDefault, spokeAP("cluster-a", "order", "p", "tenant-b", gSubj, v1.Rule{Backends: logs}), nil, ReasonTenantNotAllowed, ""},
		{"AP tenant label is not trimmed", spokeA, spokeAP("cluster-a", "order", "p", " tenant-a", gSubj, v1.Rule{Backends: logs}), nil, ReasonTenantNotAllowed, ""},
		{"AP default tenant", withDefault, spokeAP("cluster-a", "order", "p", "", gSubj, v1.Rule{Backends: logs}), nil, ReasonCompiled, "shared"},
		{"AP no tenant label and no default", spokeA, spokeAP("cluster-a", "order", "p", "", gSubj, v1.Rule{Backends: logs}), nil, ReasonTenantNotAssigned, ""},
		{"AP ignores hub namespace labels", spokeA, spokeAP("cluster-a", "order", "p", "", gSubj, v1.Rule{Backends: logs}),
			map[string]map[string]string{"order": {TenantLabel: "tenant-a"}}, ReasonTenantNotAssigned, ""},
		{"AP backend without tenant header", noTenants, spokeAP("cluster-a", "order", "p", "", gSubj, v1.Rule{Backends: metrics}), nil, ReasonCompiled, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Compile(Input{Config: mcConfig(t), SpokeClusters: []v1.SpokeCluster{tc.cluster}, SpokePolicies: []v1.SpokePolicy{tc.pol}, NamespaceLabels: tc.hubNS})
			r := spokeResult(t, c, tc.pol)
			if r.Reason != tc.reason || r.Ready != (tc.reason == ReasonCompiled) {
				t.Fatalf("got %+v, want %s", r, tc.reason)
			}
			if !r.Ready {
				if len(c.ByGroup["g"]) != 0 {
					t.Fatal("an invalid policy must grant nothing")
				}
				return
			}
			if got := strings.Join(c.ByGroup["g"][0].Tenants, ","); got != tc.tenants {
				t.Fatalf("tenants = %q, want %q", got, tc.tenants)
			}
		})
	}
}

func TestPolicyNamesDoNotCollideAcrossClusters(t *testing.T) {
	cfg := mcConfig(t)
	rule := v1.Rule{Backends: metrics}
	hubAP := ap("order", "p", gSubj, rule)
	a := spokeAP("cluster-a", "order", "p", "", gSubj, rule)
	b := spokeAP("cluster-b", "order", "p", "", gSubj, rule)
	// Crafted names: cluster "a-b" with policy "c", and cluster "a" with policy "b-c".
	ab := spokeCAP("a-b", "c", gSubj, v1.ClusterRule{Backends: metrics, Unrestricted: true})
	abc := spokeCAP("a", "b-c", gSubj, v1.ClusterRule{Backends: metrics, Unrestricted: true})
	clusters := []v1.SpokeCluster{spokeA, spokeB, spokeCluster("a-b", agentID("a-b")), spokeCluster("a", agentID("a"))}
	c := Compile(Input{Config: cfg, Policies: []v1.AccessPolicy{hubAP}, SpokeClusters: clusters, SpokePolicies: []v1.SpokePolicy{a, b, ab, abc}})

	seen := map[string]string{}
	for _, cr := range c.ByGroup["g"] {
		if prev, ok := seen[cr.Policy]; ok {
			t.Fatalf("policy name %q used by origins %q and %q", cr.Policy, prev, cr.Origin)
		}
		seen[cr.Policy] = cr.Origin
	}
	if len(seen) != 5 {
		t.Fatalf("want 5 distinct policies, got %v", seen)
	}
	origins := map[string]string{"AccessPolicy/order/p": "", "SpokePolicy/" + a.Name: "cluster-a", "SpokePolicy/" + b.Name: "cluster-b",
		"SpokePolicy/" + ab.Name: "a-b", "SpokePolicy/" + abc.Name: "a"}
	for p, o := range origins {
		if got, ok := seen[p]; !ok || got != o {
			t.Errorf("policy %s: origin %q, want %q", p, got, o)
		}
	}
	// Each spoke result names its SpokePolicy object, and the dashboard view joins it with its rules.
	views := map[string]View{}
	for _, v := range NewSnapshot(c, cfg, 1).Policies() {
		views[v.ID] = v
	}
	if len(views) != 5 {
		t.Fatalf("want 5 views, got %d", len(views))
	}
	for _, p := range []v1.SpokePolicy{a, b, ab, abc} {
		v := views["SpokePolicy/"+p.Name]
		if v.Result.Kind != KindSpokePolicy || v.Result.Name != p.Name || v.Result.Generation != p.Generation || len(v.Rules) != 1 {
			t.Errorf("view of %s: %+v", p.Name, v)
		}
	}
}

func TestSpokeRulesInvalidKeepOtherSpokes(t *testing.T) {
	// An invalid policy of one spoke does not change the policies of a different spoke.
	cfg := mcConfig(t)
	bad := spokeCAP("cluster-b", "bad", gSubj, v1.ClusterRule{Backends: metrics, AllClusters: true, Unrestricted: true})
	good := spokeCAP("cluster-a", "good", gSubj, v1.ClusterRule{Backends: metrics, Unrestricted: true})
	c := Compile(Input{Config: cfg, SpokeClusters: []v1.SpokeCluster{spokeA, spokeB}, SpokePolicies: []v1.SpokePolicy{bad, good}})
	if r := spokeResult(t, c, good); !r.Ready {
		t.Fatalf("got %+v", r)
	}
	if rs := c.ByGroup["g"]; len(rs) != 1 || rs[0].Origin != "cluster-a" {
		t.Fatalf("got %+v", rs)
	}
}

func TestSpokeCrossTenantMarking(t *testing.T) {
	// Spoke terms of one subject in two tenants on a backend without federation are marked as for hub rules.
	cfg := mcConfig(t)
	a := spokeCAP("cluster-a", "p", gSubj, v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"tenant-a"}, Unrestricted: true})
	b := spokeCAP("cluster-b", "p", gSubj, v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"tenant-b"}, Unrestricted: true})
	c := Compile(Input{Config: cfg, SpokeClusters: []v1.SpokeCluster{spokeA, spokeB}, SpokePolicies: []v1.SpokePolicy{a, b}})
	for _, p := range []v1.SpokePolicy{a, b} {
		if r := spokeResult(t, c, p); !r.Ready || r.Reason != ReasonCrossTenantUnionUnsupported {
			t.Errorf("got %+v", r)
		}
	}
}
