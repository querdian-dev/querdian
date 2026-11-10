package spokesync

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

func cap1(name string) v1.SpokePolicySource {
	return v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: name, Generation: 1,
		ClusterAccessPolicy: &v1.ClusterAccessPolicySpec{
			Subjects: v1.Subjects{Groups: []string{"g"}},
			Rules:    []v1.ClusterRule{{Backends: []string{"metrics"}, Matchers: []v1.Matcher{{Name: "app", Op: v1.MatchEqual, Value: "x"}}}},
		}}
}

func ap1(ns, name string) v1.SpokePolicySource {
	return v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Namespace: ns, Name: name, Generation: 3, TenantLabel: "t1",
		AccessPolicy: &v1.AccessPolicySpec{Subjects: v1.Subjects{Users: []string{"u"}}, Rules: []v1.Rule{{Backends: []string{"logs"}}}}}
}

func mustDigest(t *testing.T, ps []v1.SpokePolicySource) string {
	t.Helper()
	d, err := Digest(ps)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDigestCanonical(t *testing.T) {
	a, b := cap1("a"), ap1("ns", "b")
	base := mustDigest(t, []v1.SpokePolicySource{a, b})
	if !strings.HasPrefix(base, "sha256:") {
		t.Fatalf("digest format: %s", base)
	}
	if got := mustDigest(t, []v1.SpokePolicySource{b, a}); got != base {
		t.Error("digest depends on order")
	}

	// The API server applies defaults on read: empty lists and false booleans appear.
	defaulted := cap1("a")
	defaulted.ClusterAccessPolicy.Subjects.Users = []string{}
	defaulted.ClusterAccessPolicy.Rules[0].Unrestricted = false
	defaulted.ClusterAccessPolicy.Rules[0].AllClusters = false
	if got := mustDigest(t, []v1.SpokePolicySource{defaulted, b}); got != base {
		t.Error("defaulted empty values change the digest")
	}

	// A JSON round trip (as through the API server) keeps the digest.
	raw, _ := json.Marshal([]v1.SpokePolicySource{a, b})
	var back []v1.SpokePolicySource
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if got := mustDigest(t, back); got != base {
		t.Error("round trip changes the digest")
	}

	empty := mustDigest(t, nil)
	if empty == base || mustDigest(t, []v1.SpokePolicySource{}) != empty {
		t.Error("empty state digest")
	}
}

func TestDigestChanges(t *testing.T) {
	base := mustDigest(t, []v1.SpokePolicySource{cap1("a"), ap1("ns", "b")})
	for name, mut := range map[string]func(c *v1.SpokePolicySource, a *v1.SpokePolicySource){
		"matcher value": func(c, _ *v1.SpokePolicySource) { c.ClusterAccessPolicy.Rules[0].Matchers[0].Value = "y" },
		"matcher order": func(c, _ *v1.SpokePolicySource) {
			c.ClusterAccessPolicy.Rules[0].Matchers = append(c.ClusterAccessPolicy.Rules[0].Matchers, v1.Matcher{Name: "b", Op: v1.MatchEqual})
		},
		"unrestricted": func(c, _ *v1.SpokePolicySource) { c.ClusterAccessPolicy.Rules[0].Unrestricted = true },
		"allClusters":  func(c, _ *v1.SpokePolicySource) { c.ClusterAccessPolicy.Rules[0].AllClusters = true },
		"tenant label": func(_, a *v1.SpokePolicySource) { a.TenantLabel = "t2" },
		"generation":   func(_, a *v1.SpokePolicySource) { a.Generation = 4 },
		"namespace":    func(_, a *v1.SpokePolicySource) { a.Namespace = "other" },
		"empty group":  func(c, _ *v1.SpokePolicySource) { c.ClusterAccessPolicy.Subjects.Groups = []string{"g", ""} },
		"deny label": func(_, a *v1.SpokePolicySource) {
			a.AccessPolicy.Rules[0].Labels = &v1.LabelFilter{Deny: []string{"x"}}
		},
		"second backend": func(_, a *v1.SpokePolicySource) {
			a.AccessPolicy.Rules[0].Backends = append(a.AccessPolicy.Rules[0].Backends, "m")
		},
	} {
		c, a := cap1("a"), ap1("ns", "b")
		mut(&c, &a)
		if mustDigest(t, []v1.SpokePolicySource{c, a}) == base {
			t.Errorf("%s: digest did not change", name)
		}
	}
	if mustDigest(t, []v1.SpokePolicySource{cap1("a")}) == base {
		t.Error("removed policy: digest did not change")
	}
}

func TestDigestDuplicate(t *testing.T) {
	if _, err := Digest([]v1.SpokePolicySource{ap1("ns", "b"), ap1("ns", "b")}); err == nil {
		t.Fatal("duplicate keys must be an error")
	}
	// Same name in different namespaces or kinds is not a duplicate.
	if _, err := Digest([]v1.SpokePolicySource{ap1("ns", "b"), ap1("ns2", "b"), cap1("b")}); err != nil {
		t.Fatal(err)
	}
}

func TestObjectName(t *testing.T) {
	// Joined naively, these collide: "a-b"+"c" and "a"+"b-c".
	p1, p2 := cap1("c"), cap1("b-c")
	n1, n2 := ObjectName("a-b", &p1), ObjectName("a", &p2)
	if n1 == n2 {
		t.Fatal("object names collide")
	}
	if len(n1) > 253 || !strings.HasPrefix(n1, "sp-") || strings.ToLower(n1) != n1 {
		t.Fatalf("invalid object name %q", n1)
	}
	p3 := cap1("c")
	if ObjectName("a-b", &p3) != n1 {
		t.Fatal("object name is not stable")
	}
	ap, cp := ap1("", "c"), cap1("c")
	ap.Namespace = ""
	if ObjectName("x", &ap) == ObjectName("x", &cp) {
		t.Fatal("kind is not part of the name")
	}
}

func TestCertIdentity(t *testing.T) {
	const id = "spiffe://mla.example.com/agent/cluster-a"
	u, err := url.Parse(id)
	if err != nil || CertIdentity(u) != id || !IdentityMatchesName(CertIdentity(u), "cluster-a") {
		t.Fatalf("CertIdentity(%q) = %q, %v", id, CertIdentity(u), err)
	}
}

func TestIdentityMatchesName(t *testing.T) {
	for _, tc := range []struct {
		identity, name string
		want           bool
	}{
		{"spiffe://mla.example.com/agent/cluster-a", "cluster-a", true},
		{"spiffe://mla.example.com/cluster-a", "cluster-a", true},
		{"spiffe://mla.example.com/agent/x-cluster-a", "cluster-a", false},
		{"spiffe://mla.example.com/agent/cluster-a/", "cluster-a", false},
		{"spiffe://mla.example.com/agent/cluster-a-b", "cluster-a", false},
		{"spiffe://cluster-a", "cluster-a", false},
		{"spiffe://mla.example.com/agent/cluster-a?x=1", "cluster-a", false},
		{"spiffe://mla.example.com/agent/cluster-a#f", "cluster-a", false},
		{"/agent/cluster-a", "cluster-a", false},
		{"spiffe://mla.example.com/agent/", "", false},
		{"%zz", "cluster-a", false},
		{"spiffe://x/agent/other%2Fcluster-a", "cluster-a", false},
		{"spiffe://x/agent/cluster%2Da", "cluster-a", false},
		{"spiffe://mla.example.com/agent/cluster-a?", "cluster-a", false},
		{"spiffe://mla.example.com/agent/cluster-a#", "cluster-a", false},
		{"spiffe:///cluster-a", "cluster-a", false},
		{"spiffe://user:pw@x/cluster-a", "cluster-a", false},
		{"spiffe://x/agent\\cluster-a", "cluster-a", false},
		{"SPIFFE://x/agent/cluster-a", "cluster-a", false},
		{"spiffe://x/agent//cluster-a", "cluster-a", true},
	} {
		if got := IdentityMatchesName(tc.identity, tc.name); got != tc.want {
			t.Errorf("IdentityMatchesName(%q, %q) = %v, want %v", tc.identity, tc.name, got, tc.want)
		}
	}
}
