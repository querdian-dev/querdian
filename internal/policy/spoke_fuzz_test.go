package policy

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
)

var (
	// fuzzClusterValues are values and regexes for matchers that a spoke rule sets on the cluster label.
	fuzzClusterValues = []string{"cluster-a", "cluster-b", hubName, "", "cluster-a|cluster-b", ".*", ".+", "cluster-.*", "cluster-b|mla", "cluster-a.*"}
	fuzzOtherValues   = []string{"", "a", "b", "a|b", ".*"}
	// fuzzForeignClusters are cluster label values of data that is not in cluster-a. "" is data with no label.
	fuzzForeignClusters = []string{"cluster-b", hubName, "", "cluster-ab", "cluster-a-x", "Cluster-a", "xcluster-a"}
	fuzzOps             = []v1.MatchOp{v1.MatchEqual, v1.MatchNotEqual, v1.MatchRegexMatch, v1.MatchRegexNoMatch}
)

var (
	// fuzzCfg is loaded once; Compile does not change the config.
	fuzzCfg     *config.Config
	fuzzCfgOnce sync.Once
)

type fuzzBackend struct {
	name, clusterLabel, nsLabel string
	tenanted                    bool
}

var fuzzBackends = []fuzzBackend{
	{"metrics", "cluster", "namespace", false},
	{"logs", "k8s_cluster_name", "k8s_namespace_name", true},
}

// randSpokeMatchers returns up to three matchers. Most of them are on the cluster label, because these must
// only narrow the implicit cluster matcher.
func randSpokeMatchers(r *rand.Rand, b fuzzBackend) []v1.Matcher {
	n := r.Intn(4)
	out := make([]v1.Matcher, 0, n)
	for i := 0; i < n; i++ {
		m := v1.Matcher{Op: fuzzOps[r.Intn(len(fuzzOps))]}
		switch r.Intn(4) {
		case 0, 1:
			m.Name, m.Value = b.clusterLabel, fuzzClusterValues[r.Intn(len(fuzzClusterValues))]
		case 2:
			m.Name, m.Value = b.nsLabel, fuzzOtherValues[r.Intn(len(fuzzOtherValues))]
		default:
			m.Name, m.Value = "job", fuzzOtherValues[r.Intn(len(fuzzOtherValues))]
		}
		out = append(out, m)
	}
	return out
}

// randSpokePolicies returns valid policies of one spoke for one backend.
func randSpokePolicies(r *rand.Rand, cluster string, b fuzzBackend) []v1.SpokePolicy {
	n := 1 + r.Intn(5)
	out := make([]v1.SpokePolicy, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("p%d", i)
		ms := randSpokeMatchers(r, b)
		if r.Intn(2) == 0 {
			rule := v1.ClusterRule{Backends: []string{b.name}, Matchers: ms, Unrestricted: len(ms) == 0}
			if b.tenanted {
				rule.Tenants = [][]string{{"tenant-a"}, {"shared"}, {"tenant-a", "shared"}}[r.Intn(3)]
			}
			out = append(out, spokeCAP(cluster, name, gSubj, rule))
			continue
		}
		ns := []string{"a", "b"}[r.Intn(2)]
		tenant := []string{"tenant-a", ""}[r.Intn(2)] // "" uses defaultTenant
		out = append(out, spokeAP(cluster, ns, name, tenant, gSubj, v1.Rule{Backends: []string{b.name}, Matchers: ms}))
	}
	return out
}

// foreignLabels returns a label set of a cluster other than cluster-a.
func foreignLabels(r *rand.Rand, b fuzzBackend) map[string]string {
	labels := map[string]string{}
	// An empty value means that the label is absent.
	if v := fuzzForeignClusters[r.Intn(len(fuzzForeignClusters))]; v != "" {
		labels[b.clusterLabel] = v
	}
	for _, l := range []string{b.nsLabel, "job"} {
		if r.Intn(4) > 0 {
			labels[l] = []string{"", "a", "b", "x"}[r.Intn(4)]
		}
	}
	return labels
}

// checkSpokeTermsStayInCluster checks the property of T6.2: after Normalise, the terms of the rules of one
// spoke never match a label set of a different cluster, also if the rules have their own cluster matchers.
func checkSpokeTermsStayInCluster(t *testing.T, seed int64) {
	t.Helper()
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test data
	fuzzCfgOnce.Do(func() { fuzzCfg = mcConfig(t) })
	a := spokeCluster("cluster-a", agentID("cluster-a"), "tenant-a", "shared")
	a.Spec.DefaultTenant = "shared"
	b2 := spokeCluster("cluster-b", agentID("cluster-b"), "tenant-a", "shared")
	b2.Spec.DefaultTenant = "shared"
	for _, b := range fuzzBackends {
		// Policies of cluster-b are in the same compile, so their rules exist next to those of cluster-a.
		own := randSpokePolicies(r, "cluster-a", b)
		other := randSpokePolicies(r, "cluster-b", b)
		c := Compile(Input{Config: fuzzCfg, SpokeClusters: []v1.SpokeCluster{a, b2}, SpokePolicies: append(own, other...)})
		for _, p := range own {
			if res := spokeResult(t, c, p); !res.Ready {
				t.Fatalf("seed %d: generator made an invalid policy: %+v", seed, res)
			}
		}

		// The terms of cluster-a alone, built as Resolve builds them.
		var terms []Term
		for _, cr := range c.ByGroup["g"] {
			if cr.Origin != "cluster-a" || cr.Backend != b.name {
				continue
			}
			if cr.Unrestricted {
				t.Fatalf("seed %d: spoke rule compiled to Unrestricted: %+v", seed, cr)
			}
			if len(cr.Tenants) == 0 {
				terms = append(terms, Term{Matchers: cr.Matchers})
			}
			for _, tn := range cr.Tenants {
				terms = append(terms, Term{Matchers: cr.Matchers, Tenant: tn})
			}
		}
		if len(terms) == 0 {
			t.Fatalf("seed %d: no terms for %s", seed, b.name)
		}
		eff, err := Normalise(terms, config.Limits{MaxPolicyTerms: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		if eff.Unrestricted {
			t.Fatalf("seed %d: spoke terms normalised to Unrestricted: %s", seed, render(terms))
		}
		tenants := append([]string{""}, eff.Tenants...)
		for k := 0; k < 100; k++ {
			labels := foreignLabels(r, b)
			for _, tn := range tenants {
				if matchTerms(eff.Terms, tn, labels) {
					t.Fatalf("seed %d: %s: label set %v of a different cluster matches the terms of cluster-a\nin:  %s\nout: %s",
						seed, b.name, labels, render(terms), render(eff.Terms))
				}
			}
		}
	}
}

func TestSpokeTermsStayInCluster(t *testing.T) {
	for seed := int64(0); seed < 500; seed++ {
		checkSpokeTermsStayInCluster(t, seed)
	}
}

func FuzzSpokeTermsStayInCluster(f *testing.F) {
	for _, s := range []int64{0, 1, 42, 1 << 40} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, seed int64) { checkSpokeTermsStayInCluster(t, seed) })
}
