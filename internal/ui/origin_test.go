package ui

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

// mcFixture is a hub with one spoke: a hub policy and a spoke policy for the group "team", a spoke policy
// for the group "spoke-only", and a spoke policy of a cluster that has no SpokeCluster.
type mcFixture struct {
	h      http.Handler
	dl     *policy.Deadlines
	sc     *v1.SpokeCluster
	hubID  string
	readID string // SpokePolicy of cluster-a, AccessPolicy shop/read, groups team and spoke-only
	lostID string // SpokePolicy of the unknown cluster-x
}

func spokeAP(cluster, ns, name string, groups ...string) v1.SpokePolicy {
	return spokeAPFor(cluster, ns, name, "", "metrics", groups...)
}

// spokeAPFor is a SpokePolicy for an AccessPolicy with one rule on one backend. The rule denies "secret"
// ("span.secret" on traces).
func spokeAPFor(cluster, ns, name, tenant, backend string, groups ...string) v1.SpokePolicy {
	deny := "secret"
	if backend == "traces" {
		deny = "span.secret"
	}
	src := v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Namespace: ns, Name: name, Generation: 1, TenantLabel: tenant,
		AccessPolicy: &v1.AccessPolicySpec{Subjects: v1.Subjects{Groups: groups},
			Rules: []v1.Rule{{Backends: []string{backend}, Labels: &v1.LabelFilter{Deny: []string{deny}}}}}}
	return v1.SpokePolicy{ObjectMeta: metav1.ObjectMeta{Name: spokesync.ObjectName(cluster, &src), Generation: 1},
		Spec: v1.SpokePolicySpec{Cluster: cluster, SpokePolicySource: src}}
}

func newMCFixture(t *testing.T) *mcFixture {
	t.Helper()
	return newMCFixtureWith(t, nil, nil)
}

// newMCFixtureWith is newMCFixture with more hub and spoke policies. cluster-a allows the tenant "t1".
func newMCFixtureWith(t *testing.T, hubs []v1.ClusterAccessPolicy, spokes []v1.SpokePolicy) *mcFixture {
	t.Helper()
	cfg, err := config.Load("../config/testdata/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d := config.Duration(48 * time.Hour)
	cfg.Multicluster = &config.Multicluster{HubCluster: "mla", MaxStaleness: &d}
	for i := range cfg.Backends {
		cfg.Backends[i].ClusterLabel = map[config.BackendType]string{config.BackendPrometheus: "cluster",
			config.BackendLoki: "cluster", config.BackendTempo: "resource.k8s.cluster.name"}[cfg.Backends[i].Type]
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	hub := v1.ClusterAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: "hub-read", Generation: 1},
		Spec: v1.ClusterAccessPolicySpec{Subjects: v1.Subjects{Groups: []string{"team"}},
			Rules: []v1.ClusterRule{{Backends: []string{"metrics"}, AllClusters: true,
				Matchers: []v1.Matcher{{Name: "namespace", Op: v1.MatchEqual, Value: "a"}}}}}}
	read := spokeAP("cluster-a", "shop", "read", "team", "spoke-only")
	lost := spokeAP("cluster-x", "shop", "lost", "team")
	sc := v1.SpokeCluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Generation: 1},
		Spec:   v1.SpokeClusterSpec{Identity: "spiffe://mla.example.com/agent/cluster-a", Tenants: []string{"t1"}},
		Status: v1.SpokeClusterStatus{LastSyncTime: &metav1.Time{Time: time.Now()}, ObservedGeneration: 1}}
	c := policy.Compile(policy.Input{Config: cfg, ClusterPolicies: append([]v1.ClusterAccessPolicy{hub}, hubs...),
		SpokeClusters: []v1.SpokeCluster{sc}, SpokePolicies: append([]v1.SpokePolicy{read, lost}, spokes...)})
	for _, r := range c.Results {
		if r.Kind == policy.KindSpokePolicy && r.Spoke.Cluster == "cluster-a" && !r.Ready {
			t.Fatalf("fixture: %+v", r)
		}
	}
	dl := policy.NewDeadlines()
	dl.Update(&sc, time.Hour)
	f := &mcFixture{dl: dl, sc: &sc, hubID: "ClusterAccessPolicy/hub-read",
		readID: policy.KindSpokePolicy + "/" + read.Name, lostID: policy.KindSpokePolicy + "/" + lost.Name}
	f.h = Handler(Deps{Config: cfg, Snapshots: provider{policy.NewSnapshot(c, cfg, 1)}, Deadlines: dl})
	return f
}

// suspend puts cluster-a in the expired state, as a hub admin does with spec.suspended.
func (f *mcFixture) suspend() {
	f.sc.Spec.Suspended = true
	f.dl.Update(f.sc, time.Hour)
}

func getJSON(t *testing.T, h http.Handler, path string, v any) {
	t.Helper()
	w := do(h, "GET", path, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func TestPoliciesShowOriginAndExpiry(t *testing.T) {
	f := newMCFixture(t)
	policies := func() []policyJSON {
		var p struct {
			Policies []policyJSON `json:"policies"`
		}
		getJSON(t, f.h, "/api/policies", &p)
		return p.Policies
	}

	ps := policies()
	if len(ps) != 3 {
		t.Fatalf("want 3 policies, got %+v", ps)
	}
	// Hub policies come first. Spoke policies follow by cluster, kind, namespace and name, not by hash.
	hub, read, lost := ps[0], ps[1], ps[2]
	if hub.ID != f.hubID || read.ID != f.readID || lost.ID != f.lostID {
		t.Fatalf("order: %s, %s, %s", hub.ID, read.ID, lost.ID)
	}
	if *hub.Origin != (originJSON{Hub: true}) || hub.Expired || *hub.Rules[0].Origin != (originJSON{Hub: true}) || hub.Rules[0].Expired {
		t.Fatalf("hub policy: %+v", hub)
	}
	wantRead := originJSON{Cluster: "cluster-a", Kind: "AccessPolicy", Namespace: "shop", Name: "read"}
	if !read.Ready || *read.Origin != wantRead || read.Expired {
		t.Fatalf("valid spoke policy: %+v", read)
	}
	if r := read.Rules[0]; *r.Origin != (originJSON{Cluster: "cluster-a"}) || r.Expired {
		t.Fatalf("valid spoke rule: %+v", r)
	}
	// A policy that grants nothing has no expired state, but its origin shows where it comes from.
	if lost.Ready || lost.Origin.Cluster != "cluster-x" || lost.Origin.Name != "lost" || lost.Expired || len(lost.Rules) != 0 {
		t.Fatalf("spoke policy of an unknown cluster: %+v", lost)
	}
	// The compiler did not verify the source of a policy that is not ready, so the dashboard marks it.
	if !lost.Origin.Unverified || read.Origin.Unverified || hub.Origin.Unverified {
		t.Fatalf("unverified: hub %+v, read %+v, lost %+v", hub.Origin, read.Origin, lost.Origin)
	}

	// The dashboard uses the expiry check of the table as it is: a spoke is also expired while the snapshot
	// does not have the current generation of its SpokeCluster.
	f.dl.TrackCompiled()
	if ps = policies(); !ps[1].Expired || !ps[1].Rules[0].Expired {
		t.Fatalf("snapshot without the current spoke generation: %+v", ps[1])
	}
	f.dl.SetCompiled(map[string]int64{"cluster-a": f.sc.Generation})
	if ps = policies(); ps[1].Expired {
		t.Fatalf("snapshot with the current spoke generation: %+v", ps[1])
	}

	f.suspend()
	ps = policies()
	hub, read = ps[0], ps[1]
	if hub.Expired || hub.Rules[0].Expired {
		t.Fatalf("hub policies never expire: %+v", hub)
	}
	if !read.Expired || !read.Ready || !read.Rules[0].Expired {
		t.Fatalf("suspended spoke: %+v", read)
	}
	// The expired rule keeps its denied labels, because they still apply.
	if strings.Join(read.Rules[0].DeniedLabels, ",") != "secret" {
		t.Fatalf("expired rule must keep its denied labels: %+v", read.Rules[0])
	}

	var o struct {
		SpokePolicies map[string]int `json:"spokePolicies"`
	}
	getJSON(t, f.h, "/api/overview", &o)
	if o.SpokePolicies["total"] != 2 || o.SpokePolicies["expired"] != 1 {
		t.Fatalf("overview: %+v", o)
	}
}

func TestAccessShowsExpiredSources(t *testing.T) {
	f := newMCFixture(t)
	metrics := func(groups string) accessJSON {
		var a struct {
			Access []accessJSON `json:"access"`
		}
		getJSON(t, f.h, "/api/access?groups="+groups, &a)
		if a.Access[0].Backend != "metrics" {
			t.Fatalf("first backend: %+v", a.Access[0])
		}
		return a.Access[0]
	}
	hubSrc := sourceJSON{ID: f.hubID, Origin: originJSON{Hub: true}}
	readSrc := sourceJSON{ID: f.readID, Origin: originJSON{Cluster: "cluster-a", Kind: "AccessPolicy", Namespace: "shop", Name: "read"}}

	a := metrics("team")
	if !a.Granted || len(a.Sources) != 2 || a.Sources[0] != hubSrc || a.Sources[1] != readSrc {
		t.Fatalf("valid spoke: %+v", a)
	}

	f.suspend()
	readSrc.Expired = true
	a = metrics("team")
	// The hub rule still grants data. The expired spoke policy is listed, and its denied label applies.
	if !a.Granted || len(a.Sources) != 2 || a.Sources[0] != hubSrc || a.Sources[1] != readSrc {
		t.Fatalf("suspended spoke with a hub grant: %+v", a)
	}
	if strings.Join(a.DeniedLabels, ",") != "secret" || len(a.Terms) != 1 {
		t.Fatalf("an expired policy must keep its denied labels and give no term: %+v", a)
	}
	a = metrics("spoke-only")
	if a.Granted || a.Reason != "policy_source_expired" || len(a.Sources) != 1 || a.Sources[0] != readSrc {
		t.Fatalf("only an expired spoke policy: %+v", a)
	}
	if a = metrics("nobody"); a.Granted || a.Reason != "no_grant" || len(a.Sources) != 0 {
		t.Fatalf("no grant: %+v", a)
	}

	var b struct {
		Backends []struct {
			Name    string       `json:"name"`
			Sources []sourceJSON `json:"sources"`
		} `json:"backends"`
	}
	getJSON(t, f.h, "/api/backends", &b)
	if s := b.Backends[0].Sources; b.Backends[0].Name != "metrics" || len(s) != 2 || s[0] != hubSrc || s[1] != readSrc {
		t.Fatalf("backends: %+v", b.Backends)
	}
}

func accessFor(t *testing.T, h http.Handler, groups string) map[string]accessJSON {
	t.Helper()
	var a struct {
		Access []accessJSON `json:"access"`
	}
	getJSON(t, h, "/api/access?groups="+groups, &a)
	out := map[string]accessJSON{}
	for _, x := range a.Access {
		out[x.Backend] = x
	}
	return out
}

// TestRefusedBackendListsOnlyExpiredSources: a backend that ResolveAt refuses for a different reason lists no
// policies, as without the multicluster block. An expired policy stays listed.
func TestRefusedBackendListsOnlyExpiredSources(t *testing.T) {
	tenantRule := func(name, tenant, ns string) v1.ClusterAccessPolicy {
		return v1.ClusterAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
			Spec: v1.ClusterAccessPolicySpec{Subjects: v1.Subjects{Groups: []string{"split"}},
				Rules: []v1.ClusterRule{{Backends: []string{"traces"}, Tenants: []string{tenant}, AllClusters: true,
					Matchers: []v1.Matcher{{Name: "resource.k8s.namespace.name", Op: v1.MatchEqual, Value: ns}}}}}}
	}
	trace := spokeAPFor("cluster-a", "shop", "trace", "t1", "traces", "split")
	f := newMCFixtureWith(t, []v1.ClusterAccessPolicy{tenantRule("split-1", "t1", "a"), tenantRule("split-2", "t2", "b")},
		[]v1.SpokePolicy{trace})

	// Two tenants with different terms on a backend without __tenant_id__ support.
	a := accessFor(t, f.h, "split")["traces"]
	if a.Granted || a.Reason != "cross_tenant_union_unsupported" || len(a.Sources) != 0 || len(a.Policies) != 0 {
		t.Fatalf("refused backend: %+v", a)
	}

	f.suspend()
	a = accessFor(t, f.h, "split")["traces"]
	want := sourceJSON{ID: policy.KindSpokePolicy + "/" + trace.Name, Expired: true,
		Origin: originJSON{Cluster: "cluster-a", Kind: "AccessPolicy", Namespace: "shop", Name: "trace"}}
	if a.Granted || a.Reason != "cross_tenant_union_unsupported" || len(a.Sources) != 1 || a.Sources[0] != want {
		t.Fatalf("refused backend with an expired spoke: %+v", a)
	}
}

// TestSourcesUseThePolicyOrder: sources are in the order of the Policies view, not in the order of the hash IDs.
func TestSourcesUseThePolicyOrder(t *testing.T) {
	read := spokeAP("cluster-a", "shop", "read")
	// Find a namespace that sorts before "shop" but whose hash ID sorts after the ID of shop/read.
	var early v1.SpokePolicy
	for _, ns := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		if early = spokeAP("cluster-a", ns, "read", "team"); early.Name > read.Name {
			break
		}
	}
	if early.Name < read.Name {
		t.Fatal("no namespace found for the test")
	}
	f := newMCFixtureWith(t, nil, []v1.SpokePolicy{early})
	earlyID := policy.KindSpokePolicy + "/" + early.Name

	var p struct {
		Policies []policyJSON `json:"policies"`
	}
	getJSON(t, f.h, "/api/policies", &p)
	var want []string
	for _, x := range p.Policies {
		if len(x.Rules) > 0 {
			want = append(want, x.ID)
		}
	}
	if strings.Join(want, ",") != strings.Join([]string{f.hubID, earlyID, f.readID}, ",") {
		t.Fatalf("policies order: %v", want)
	}
	ids := func(ss []sourceJSON) string {
		var out []string
		for _, s := range ss {
			out = append(out, s.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(accessFor(t, f.h, "team")["metrics"].Sources); got != strings.Join(want, ",") {
		t.Fatalf("access sources: %s", got)
	}
	var b struct {
		Backends []struct {
			Sources []sourceJSON `json:"sources"`
		} `json:"backends"`
	}
	getJSON(t, f.h, "/api/backends", &b)
	if got := ids(b.Backends[0].Sources); got != strings.Join(want, ",") {
		t.Fatalf("backend sources: %s", got)
	}
}

// TestInvalidSpokePolicyIsNotHub: a spoke policy with no cluster in its spec is still a spoke policy, and its
// origin is unverified.
func TestInvalidSpokePolicyIsNotHub(t *testing.T) {
	bad := spokeAP("", "shop", "bad", "team")
	f := newMCFixtureWith(t, nil, []v1.SpokePolicy{bad})
	var p struct {
		Policies []policyJSON `json:"policies"`
	}
	getJSON(t, f.h, "/api/policies", &p)
	for _, x := range p.Policies {
		if x.ID == policy.KindSpokePolicy+"/"+bad.Name {
			if x.Ready || x.Origin.Hub || !x.Origin.Unverified || x.Origin.Name != "bad" {
				t.Fatalf("invalid spoke policy: %+v", x)
			}
			return
		}
	}
	t.Fatal("invalid spoke policy not listed")
}

// TestNoMulticlusterKeepsTheAPI: without the multicluster block, the answers have none of the new fields.
func TestNoMulticlusterKeepsTheAPI(t *testing.T) {
	h := newHandler(t, true)
	for _, p := range []string{"/api/overview", "/api/policies", "/api/access?groups=team-a", "/api/backends"} {
		body := do(h, "GET", p, "", "").Body.String()
		for _, key := range []string{`"origin"`, `"expired"`, `"sources"`, `"spokePolicies"`} {
			if strings.Contains(body, key) {
				t.Errorf("%s: unexpected %s in %s", p, key, body)
			}
		}
	}
}

// TestWebAssetsStayReadOnly: the script reads the dashboard API only, and it never writes HTML.
func TestWebAssetsStayReadOnly(t *testing.T) {
	js := do(newHandler(t, true), "GET", "/app.js", "", "").Body.String()
	if !strings.Contains(js, "function policies()") {
		t.Fatal("app.js not served")
	}
	for _, bad := range []string{".innerHTML", ".outerHTML", ".insertAdjacentHTML", "document.write(", "XMLHttpRequest", "WebSocket", "EventSource"} {
		if strings.Contains(js, bad) {
			t.Errorf("app.js uses %s", bad)
		}
	}
	if n, m := strings.Count(js, "fetch("), strings.Count(js, `fetch("api/" + path`); n != 1 || m != 1 {
		t.Errorf("app.js must fetch the dashboard API only; found %d calls", n)
	}
}
