package policy

import (
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

// rule builds a compiled rule by hand. specs use the syntax of tm, for example "namespace=a".
func rule(policyID, backend, origin string, specs ...string) *CompiledRule {
	return &CompiledRule{Policy: policyID, Backend: backend, Origin: origin, Matchers: tm("", specs...).Matchers}
}

func withLabels(r *CompiledRule, deny []string, red map[string]Redaction) *CompiledRule {
	r.DeniedLabels = sortedUnique(append(append([]string(nil), deny...), slices.Collect(maps.Keys(red))...))
	r.Redactions = red
	return r
}

func withTenants(r *CompiledRule, tenants ...string) *CompiledRule {
	r.Tenants = tenants
	return r
}

// handSnap indexes the rules under the group g.
func handSnap(t testing.TB, rules ...*CompiledRule) *Snapshot {
	t.Helper()
	return NewSnapshot(&Compiled{ByGroup: map[string][]*CompiledRule{"g": rules}}, testConfig(t), 1)
}

func tableWith(spokes ...string) *Deadlines {
	d := NewDeadlines()
	for _, s := range spokes {
		d.Update(spoke(s), defaultStale)
	}
	return d
}

// hubAndSpoke has a hub rule, a rule of spoke cluster-a with deny and redact, and a rule of cluster-b.
func hubAndSpoke(t testing.TB) *Snapshot {
	return handSnap(t,
		rule("ClusterAccessPolicy/hub", "metrics", "", "cluster=mla", "namespace=a"),
		withLabels(rule("SpokePolicy/cluster-a/x", "metrics", "cluster-a", "cluster=cluster-a", "namespace=b"),
			[]string{"secret"}, map[string]Redaction{"ip": {Strategy: v1.RedactIPv4Prefix, Octets: 1}}),
		rule("SpokePolicy/cluster-b/y", "metrics", "cluster-b", "cluster=cluster-b", "namespace=c"),
	)
}

func TestResolveAtMixOfHubAndSpokeRules(t *testing.T) {
	s := hubAndSpoke(t)
	tests := []struct {
		name    string
		table   *Deadlines
		terms   string
		origins string
	}{
		{"all valid", tableWith("cluster-a", "cluster-b"),
			"cluster=mla,namespace=a | cluster=cluster-a,namespace=b | cluster=cluster-b,namespace=c", "cluster-a,cluster-b"},
		{"cluster-a expired", tableWith("cluster-b"), "cluster=mla,namespace=a | cluster=cluster-b,namespace=c", "cluster-b"},
		{"both expired", NewDeadlines(), "cluster=mla,namespace=a", ""},
		{"no table", nil, "cluster=mla,namespace=a", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eff, err := s.ResolveAt("u", []string{"g"}, "metrics", tc.table, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if got := render(eff.Terms); !sameTerms(got, tc.terms) {
				t.Fatalf("terms = %q, want %q", got, tc.terms)
			}
			if got := strings.Join(eff.Origins, ","); got != tc.origins {
				t.Fatalf("origins = %q, want %q", got, tc.origins)
			}
			// Rule 5: the deny and redact labels of cluster-a stay also when it is expired.
			if strings.Join(eff.DeniedLabels, ",") != "ip,secret" ||
				eff.Redactions["ip"] != (Redaction{Strategy: v1.RedactIPv4Prefix, Octets: 1}) {
				t.Fatalf("labels lost: %v %v", eff.DeniedLabels, eff.Redactions)
			}
			if eff.Unrestricted {
				t.Fatal("must stay restricted")
			}
		})
	}
	// Resolve without a table is ResolveAt with no table.
	eff, err := s.Resolve("u", []string{"g"}, "metrics")
	if err != nil || render(eff.Terms) != "cluster=mla,namespace=a" || len(eff.Origins) != 0 {
		t.Fatalf("Resolve: %s %v", render(eff.Terms), err)
	}
}

// sameTerms compares rendered term lists without order.
func sameTerms(a, b string) bool {
	sa, sb := strings.Split(a, " | "), strings.Split(b, " | ")
	slices.Sort(sa)
	slices.Sort(sb)
	return slices.Equal(sa, sb)
}

func TestResolveAtAllExpired(t *testing.T) {
	s := handSnap(t,
		withLabels(rule("SpokePolicy/cluster-a/x", "metrics", "cluster-a", "cluster=cluster-a", "namespace=b"), []string{"secret"}, nil),
		rule("SpokePolicy/cluster-b/y", "metrics", "cluster-b", "cluster=cluster-b", "namespace=c"),
	)
	for name, table := range map[string]*Deadlines{"empty table": NewDeadlines(), "nil table": nil, "other spoke valid": tableWith("cluster-z")} {
		eff, err := s.ResolveAt("u", []string{"g"}, "metrics", table, testNow)
		if !errors.Is(err, ErrPolicySourceExpired) {
			t.Fatalf("%s: want ErrPolicySourceExpired, got %v", name, err)
		}
		if eff.Unrestricted || len(eff.Terms) != 0 || len(eff.Policies) != 0 || len(eff.DeniedLabels) != 0 {
			t.Fatalf("%s: want an empty Effective with the error, got %+v", name, eff)
		}
	}
	// One valid spoke is sufficient.
	eff, err := s.ResolveAt("u", []string{"g"}, "metrics", tableWith("cluster-b"), testNow)
	if err != nil || render(eff.Terms) != "cluster=cluster-b,namespace=c" || strings.Join(eff.DeniedLabels, ",") != "secret" {
		t.Fatalf("unexpected: %s %v %v", render(eff.Terms), eff.DeniedLabels, err)
	}
	// Users with no rule still get ErrNoGrant, not the expiry reason.
	if _, err := s.ResolveAt("u", []string{"other"}, "metrics", NewDeadlines(), testNow); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("want ErrNoGrant, got %v", err)
	}
}

func TestResolveAtEachExpiryCondition(t *testing.T) {
	s := handSnap(t, rule("SpokePolicy/cluster-a/x", "metrics", "cluster-a", "cluster=cluster-a", "namespace=b"))
	deleted := spoke("cluster-a")
	for name, sc := range map[string]*v1.SpokeCluster{
		"suspended":           spoke("cluster-a", func(sc *v1.SpokeCluster) { sc.Spec.Suspended = true }),
		"no lastSyncTime":     spoke("cluster-a", func(sc *v1.SpokeCluster) { sc.Status.LastSyncTime = nil }),
		"future lastSyncTime": spoke("cluster-a", syncedAt(testNow.Add(10*time.Minute))),
		"generation differs":  spoke("cluster-a", func(sc *v1.SpokeCluster) { sc.Generation = 9 }),
		"maxStaleness zero":   spoke("cluster-a", staleness(0)),
		"stale":               spoke("cluster-a", syncedAt(testNow.Add(-72*time.Hour))),
		"deleted":             deleted,
	} {
		d := NewDeadlines()
		d.Update(sc, defaultStale)
		if sc == deleted {
			d.Delete("cluster-a")
		}
		if _, err := s.ResolveAt("u", []string{"g"}, "metrics", d, testNow); !errors.Is(err, ErrPolicySourceExpired) {
			t.Errorf("%s: want ErrPolicySourceExpired, got %v", name, err)
		}
	}
	// Deleted and re-created: expired until the next valid sync, then the grant returns (rule 7).
	d := tableWith("cluster-a")
	d.Delete("cluster-a")
	d.Update(spoke("cluster-a", func(sc *v1.SpokeCluster) { sc.Generation, sc.Status = 1, v1.SpokeClusterStatus{} }), defaultStale)
	if _, err := s.ResolveAt("u", []string{"g"}, "metrics", d, testNow); !errors.Is(err, ErrPolicySourceExpired) {
		t.Fatalf("re-created: want ErrPolicySourceExpired, got %v", err)
	}
	d.Update(spoke("cluster-a", func(sc *v1.SpokeCluster) { sc.Generation, sc.Status.ObservedGeneration = 1, 1 }), defaultStale)
	if _, err := s.ResolveAt("u", []string{"g"}, "metrics", d, testNow); err != nil {
		t.Fatalf("re-synced: %v", err)
	}
}

func TestResolveAtExpiredTenantsLeaveHeader(t *testing.T) {
	s := handSnap(t,
		withTenants(rule("ClusterAccessPolicy/hub", "logs", "", "k8s_namespace_name=a"), "t1"),
		withTenants(rule("SpokePolicy/cluster-a/x", "logs", "cluster-a", "cluster=cluster-a", "k8s_namespace_name=b"), "t2"),
	)
	eff, err := s.ResolveAt("u", []string{"g"}, "logs", tableWith("cluster-a"), testNow)
	if err != nil || strings.Join(eff.Tenants, ",") != "t1,t2" {
		t.Fatalf("valid: %v %v", eff.Tenants, err)
	}
	eff, err = s.ResolveAt("u", []string{"g"}, "logs", NewDeadlines(), testNow)
	if err != nil || strings.Join(eff.Tenants, ",") != "t1" || render(eff.Terms) != "k8s_namespace_name=a" {
		t.Fatalf("expired: tenants=%v terms=%s %v", eff.Tenants, render(eff.Terms), err)
	}
}

func TestResolveAtUnrestrictedSpokeRuleGrantsNothing(t *testing.T) {
	r := withLabels(&CompiledRule{Policy: "SpokePolicy/cluster-a/x", Backend: "metrics", Origin: "cluster-a", Unrestricted: true},
		[]string{"secret"}, nil)
	s := handSnap(t, r)
	if _, err := s.ResolveAt("u", []string{"g"}, "metrics", tableWith("cluster-a"), testNow); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("want ErrNoGrant, got %v", err)
	}
	s = handSnap(t, r, rule("ClusterAccessPolicy/hub", "metrics", "", "namespace=a"))
	eff, err := s.ResolveAt("u", []string{"g"}, "metrics", tableWith("cluster-a"), testNow)
	if err != nil || eff.Unrestricted || render(eff.Terms) != "namespace=a" || strings.Join(eff.DeniedLabels, ",") != "secret" {
		t.Fatalf("unexpected: %+v %v", eff, err)
	}
}

// TestResolveAtExpiryNeverWidens checks for random rule sets that each label set the expired result
// matches is also matched by the valid result, and that the denied labels and redactions do not shrink.
func TestResolveAtExpiryNeverWidens(t *testing.T) {
	values := []string{"a", "b", "c"}
	clusters := []string{"mla", "cluster-a", "cluster-b"}
	reds := []Redaction{{Strategy: v1.RedactRemove}, {Strategy: v1.RedactHash}, {Strategy: v1.RedactIPv4Prefix, Octets: 2}}
	for seed := int64(0); seed < 300; seed++ {
		r := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test data
		var rules []*CompiledRule
		for i := range 1 + r.Intn(5) {
			origin := clusters[r.Intn(len(clusters))]
			if origin == "mla" {
				origin = ""
			}
			cl := origin
			if cl == "" {
				cl = "mla"
			}
			cr := rule(fmt.Sprintf("p%d", i), "metrics", origin, "cluster="+cl, "namespace="+values[r.Intn(len(values))])
			var deny []string
			if r.Intn(2) == 0 {
				deny = []string{"d" + values[r.Intn(len(values))]}
			}
			var red map[string]Redaction
			if r.Intn(2) == 0 {
				red = map[string]Redaction{"r" + values[r.Intn(len(values))]: reds[r.Intn(len(reds))]}
			}
			rules = append(rules, withLabels(cr, deny, red))
		}
		s := handSnap(t, rules...)
		valid, verr := s.ResolveAt("u", []string{"g"}, "metrics", tableWith("cluster-a", "cluster-b"), testNow)
		if verr != nil {
			t.Fatalf("seed %d: valid: %v", seed, verr)
		}
		for _, table := range []*Deadlines{tableWith("cluster-a"), tableWith("cluster-b"), NewDeadlines()} {
			exp, err := s.ResolveAt("u", []string{"g"}, "metrics", table, testNow)
			if errors.Is(err, ErrPolicySourceExpired) {
				continue // no access at all
			}
			if err != nil {
				t.Fatalf("seed %d: %v", seed, err)
			}
			if exp.Unrestricted || len(exp.Terms) == 0 {
				t.Fatalf("seed %d: expiry gave %+v", seed, exp)
			}
			for _, cl := range clusters {
				for _, ns := range values {
					lbl := map[string]string{"cluster": cl, "namespace": ns}
					if matchTerms(exp.Terms, "", lbl) && !matchTerms(valid.Terms, "", lbl) {
						t.Fatalf("seed %d: expiry widened access to %v", seed, lbl)
					}
				}
			}
			if !slices.Equal(exp.DeniedLabels, valid.DeniedLabels) || !maps.Equal(exp.Redactions, valid.Redactions) {
				t.Fatalf("seed %d: labels changed: %v %v / %v %v", seed, exp.DeniedLabels, exp.Redactions, valid.DeniedLabels, valid.Redactions)
			}
		}
	}
}
