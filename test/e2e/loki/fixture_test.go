//go:build e2e

package loki

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/test/e2e/framework"
)

const backend = "logs"

// isolation users: group -> allowed namespaces (all of tenant t1), granted by namespaced AccessPolicies.
var isolationUsers = []struct {
	user, group string
	allowed     []string
}{
	{"alice", "team-a", []string{"ns-a"}},
	{"bob", "team-b", []string{"ns-b"}},
	{"carol", "team-ab", []string{"ns-a", "ns-b"}}, // two AccessPolicies = two terms, merged to one regex term
}

type fixture struct {
	end     time.Time
	mainURL string
	refURL  string
	direct  func(tenant string) api // direct access to the main Loki as a tenant (or "t1|t2")
	oracle  func(set termSet) api   // the reference instance as seen by the oracle of a term set
	proxy   *framework.Proxy
	eff     map[string]policy.Effective
	all     []streamDef
	pol     framework.Policies
	skew    time.Duration // Loki's clock minus ours (a container VM may run behind the host)
}

func newFixture(t *testing.T) *fixture {
	framework.RequireRuntime(t)
	f := &fixture{}
	// Development shortcut: LOKI_E2E_DEV="<main url>,<ref url>[,<end unix nanos>]" uses Lokis started by hand;
	// without the end timestamp they are seeded (and the end is logged), with it seeding is skipped.
	if dev := strings.Split(os.Getenv("LOKI_E2E_DEV"), ","); len(dev) >= 2 {
		f.mainURL, f.refURL = dev[0], dev[1]
		f.skew = lokiClockSkew(t, f.mainURL)
		f.end = f.now().Truncate(lineStep)
		if len(dev) == 3 {
			n, _ := strconv.ParseInt(dev[2], 10, 64)
			f.end = time.Unix(0, n)
		}
		f.all = allStreams(f.end)
		if len(dev) < 3 {
			seed(t, f.mainURL, f.refURL, f.end)
			t.Logf("seeded, LOKI_E2E_DEV end=%d", f.end.UnixNano())
		}
	} else {
		f.mainURL, f.refURL = startLokis(t)
		f.skew = lokiClockSkew(t, f.mainURL)
		f.end = f.now().Truncate(lineStep)
		f.all = allStreams(f.end)
		seed(t, f.mainURL, f.refURL, f.end)
	}

	mc := framework.NewClient(t, f.mainURL)
	rc := framework.NewClient(t, f.refURL)
	f.direct = func(tenant string) api { return api{c: mc, tenant: tenant, end: f.end} }
	f.oracle = func(set termSet) api { return api{c: rc, tenant: set.refHeader(), end: f.end} }

	var cluster []v1.ClusterAccessPolicy
	for _, set := range termSets {
		cluster = append(cluster, framework.ClusterPolicy(set.name, framework.Group(set.group), set.rules()...))
	}
	nsLabels := map[string]map[string]string{}
	for _, ns := range []string{"ns-a", "ns-b", "ns-c", "ns-d"} {
		nsLabels[ns] = map[string]string{policy.TenantLabel: "t1"}
	}
	ap := []v1.AccessPolicy{
		framework.NamespacedPolicy("ns-a", "a", framework.Group("team-a", "team-ab"), v1.Rule{Backends: []string{backend}}),
		framework.NamespacedPolicy("ns-b", "b", framework.Group("team-b", "team-ab"), v1.Rule{Backends: []string{backend}}),
	}
	f.pol = framework.Policies{Cluster: cluster, Namespaced: ap, NamespaceLabels: nsLabels}
	f.proxy = framework.StartProxy(t, framework.ProxyOptions{
		Backends: []config.Backend{lokiBackend(f.mainURL)},
		Policies: f.pol,
	})
	snap := policy.NewSnapshot(policy.Compile(policy.Input{Config: f.proxy.Config, ClusterPolicies: cluster}), f.proxy.Config, 1)
	f.eff = map[string]policy.Effective{}
	for _, set := range termSets {
		eff, err := snap.Resolve("u", []string{set.group}, backend)
		if err != nil {
			t.Fatalf("%s: %v", set.name, err)
		}
		f.eff[set.name] = eff
		if multi := len(eff.Terms) > 1; multi != set.multi {
			t.Fatalf("%s: resolves to %d terms, scenario expects multi=%v", set.name, len(eff.Terms), set.multi)
		}
		if !set.unrestricted && eff.Disjoint != set.disjoint {
			t.Fatalf("%s: engine says disjoint=%v, scenario expects %v", set.name, eff.Disjoint, set.disjoint)
		}
	}
	return f
}

func lokiBackend(url string) config.Backend {
	return config.Backend{Name: backend, Type: config.BackendLoki, URL: url, PathPrefix: "/loki",
		NamespaceLabel: nsLabel, TenantHeader: "X-Scope-OrgID", TenantFederation: config.FederationTenantIDLabel}
}

// rules builds the CRD rules of a term set.
func (ts termSet) rules() []v1.ClusterRule {
	if ts.unrestricted {
		return []v1.ClusterRule{framework.WithTenants(framework.Unrestricted(backend), ts.tenants...)}
	}
	var out []v1.ClusterRule
	for _, term := range ts.terms {
		tenants := ts.tenants
		if term.tenant != "" {
			tenants = []string{term.tenant}
		}
		r := framework.WithTenants(framework.Rule(backend, term.ms...), tenants...)
		if len(ts.denied) > 0 {
			r = framework.Deny(r, ts.denied...)
		}
		out = append(out, r)
	}
	return out
}

// as returns the proxy API as a member of the set's group.
func (f *fixture) as(set termSet) api {
	return api{c: f.proxy.As("u-"+set.group, set.group), prefix: "/loki", end: f.end}
}

func (f *fixture) asUser(user, group string) api {
	return api{c: f.proxy.As(user, group), prefix: "/loki", end: f.end}
}

func setByName(name string) termSet {
	for _, s := range termSets {
		if s.name == name {
			return s
		}
	}
	panic("no term set " + name)
}

// visibleStreams returns the seeded streams a term set grants.
func (f *fixture) visibleStreams(t testing.TB, set termSet) []streamDef {
	var out []streamDef
	for _, s := range f.all {
		if set.visible(t, s) {
			out = append(out, s)
		}
	}
	return out
}

// now is the current time on Loki's clock. Loki rejects samples that are too far in the future (and the tail
// only delivers new lines), so everything that is pushed uses its clock, not the test host's.
func (f *fixture) now() time.Time { return time.Now().Add(f.skew) }

// freshNow re-reads Loki's clock (a container VM clock can step, e.g. after the host slept).
func (f *fixture) freshNow(t testing.TB) time.Time {
	t.Helper()
	f.skew = lokiClockSkew(t, f.mainURL)
	return f.now()
}

// lokiClockSkew returns Loki's clock minus the local clock, from the Date header of an HTTP response.
func lokiClockSkew(t testing.TB, base string) time.Duration {
	t.Helper()
	before := time.Now()
	resp, err := http.Get(base + "/ready") //nolint:gosec,noctx // test-only
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	d, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		t.Fatalf("no usable Date header from Loki: %v", err)
	}
	// The header has a resolution of one second.
	skew := d.Sub(before.Truncate(time.Second))
	if skew > -2*time.Second && skew < 2*time.Second {
		return 0
	}
	t.Logf("Loki's clock differs from the host's by %v; timestamps follow Loki's clock", skew)
	return skew
}
