//go:build e2e

// Package tempo holds the Tempo end-to-end suite (T4.6): a real Tempo container (tempoImage, monolithic,
// multitenancy, local storage) seeded over OTLP/HTTP with traces of three namespaces and three tenants, a
// second "reference" Tempo seeded with only the spans each policy grants, the real proxy handler chain
// in-process and a test OIDC issuer.
//
// # How results are compared
//
// Isolation checks use the test's own model of the data (seed_test.go): every span returned by any
// endpoint must be granted by the policy of the user, evaluated by the test (not by the proxy).
//
// Equivalence uses an oracle: for every policy the reference Tempo holds, under its own tenant, exactly
// the spans the policy grants (the trace structure is kept: parent IDs are not rewritten, so a trace whose
// root is not granted is rootless there). The unmodified user query on the reference must return what the
// proxy returns for the same query: the same traces, in each the same span sets (span IDs, matched count and
// grouping labels). Compared per endpoint:
//
//   - search: traceID -> sorted span sets. Not compared: rootServiceName, rootTraceName, serviceStats,
//     durationMs and startTimeUnixNano (the proxy redacts or recomputes them by design, checked separately),
//     and the span attributes (Tempo adds the attributes of the injected policy to the spans);
//   - metrics: label set -> timestamp/value samples, exemplars ignored, values to 9 digits;
//   - tags and tag values: the name / value sets;
//   - trace by ID: the set of span IDs per response (and the attribute names of each span, minus denied).
//
// The oracle is well defined for queries whose result depends on each span on its own or on the set of
// granted spans of the trace: filters, boolean combinations, regexes, spanset && and ||, pipelines with
// aggregates, by, select, coalesce and metrics functions. It is NOT defined for structural operators
// (>>, >, ~, <<, < and their union and negated forms): the proxy evaluates the relation on the complete
// trace (a granted parent and a granted grandchild stay related through a span the user may not see),
// whereas the reference trace lacks the ungranted span. For those queries the test checks the invariants
// instead: every returned span is granted, and for positive operators the result is a subset of what the
// unrestricted Tempo returns, restricted to granted spans. Queries with trace-level intrinsics (rootName,
// rootServiceName, traceDuration, ...) are rejected for restricted users (see rootoracle_test.go).
package tempo

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/test/e2e/framework"
)

const backendName = "traces"

// scenario is one user with a policy.
type scenario struct {
	name   string
	tenant string
	// rules are the DNF terms (one rule each); empty with unrestricted set means "all of the tenant".
	rules        [][]v1.Matcher
	deny         []string
	unrestricted bool
	// namespaced builds an AccessPolicy in this namespace instead of a ClusterAccessPolicy.
	namespaced string
	// noTagLookups: the policy cannot be expressed in a q shape Tempo evaluates on the tag endpoints
	// (a negative matcher or an open regex), so the proxy refuses tag lookups (GHSA-jcqw-3c3f-xrmj).
	noTagLookups bool
}

func (s scenario) user() string  { return "u-" + s.name }
func (s scenario) group() string { return "g-" + s.name }
func (s scenario) refTenant() string {
	if s.unrestricted {
		return s.tenant
	}
	return "ref-" + s.name
}

func (s scenario) terms() []term {
	var out []term
	for _, r := range s.rules {
		out = append(out, term{tenant: s.tenant, matchers: r})
	}
	return out
}

// grants reports whether the scenario grants the span.
func (s scenario) grants(sp spanDef) bool {
	if sp.tenant != s.tenant {
		return false
	}
	if s.unrestricted {
		return true
	}
	return granted(sp, s.tenant, s.terms())
}

const nsKey = "resource.k8s.namespace.name"

var scenarios = []scenario{
	{name: "ns-a", tenant: "t1", rules: [][]v1.Matcher{{framework.Eq(nsKey, "ns-a")}}},
	{name: "ns-ab", tenant: "t1", rules: [][]v1.Matcher{{framework.Eq(nsKey, "ns-a")}, {framework.Eq(nsKey, "ns-b")}}},
	{name: "ns-ac-notbeta", tenant: "t1", rules: [][]v1.Matcher{{framework.Re(nsKey, "ns-(a|c)"), framework.Ne("span.team", "beta")}}, noTagLookups: true},
	{name: "team-alpha", tenant: "t1", rules: [][]v1.Matcher{{framework.Eq("span.team", "alpha")}}},
	{name: "svc-cart-db", tenant: "t1", rules: [][]v1.Matcher{{framework.Re("resource.service.name", "cart|db")}}},
	{name: "t2-ns-a", tenant: "t2", rules: [][]v1.Matcher{{framework.Eq(nsKey, "ns-a")}}},
	{name: "t3-ns-a-or-svc", tenant: "t3", rules: [][]v1.Matcher{{framework.Eq(nsKey, "ns-a")}, {framework.Eq("resource.service.name", "svc-ns-b")}}},
	{name: "t3-ns-ac", tenant: "t3", rules: [][]v1.Matcher{{framework.Re(nsKey, "ns-(a|c)")}}},
	{name: "ns-a-deny", tenant: "t1", rules: [][]v1.Matcher{{framework.Eq(nsKey, "ns-a")}}, deny: []string{"span.customer_id"}},
	{name: "ns-b-ap", tenant: "t1", rules: [][]v1.Matcher{{framework.Eq(nsKey, "ns-b")}}, namespaced: "ns-b"},
	{name: "admin", tenant: "t1", unrestricted: true},
}

func scenarioByName(name string) scenario {
	for _, s := range scenarios {
		if s.name == name {
			return s
		}
	}
	panic("unknown scenario " + name)
}

type fixture struct {
	base       time.Time
	main, ref  *framework.Container
	proxy      *framework.Proxy
	spans      []spanDef
	byID       map[string]spanDef
	byTrace    map[string][]spanDef
	from, to   time.Time
	directT    map[string]api // direct, per tenant (main Tempo, and reference tenants)
	proxyUsers map[string]api
	eff        map[string]policy.Effective
}

func newFixture(t *testing.T) *fixture {
	framework.RequireRuntime(t)
	f := &fixture{
		spans:   allSpans(),
		byID:    map[string]spanDef{},
		byTrace: map[string][]spanDef{},
		directT: map[string]api{},
		eff:     map[string]policy.Effective{},
	}
	for _, s := range f.spans {
		f.byID[s.id] = s
		f.byTrace[s.trace] = append(f.byTrace[s.trace], s)
	}
	f.main = startTempo(t)
	f.ref = startTempo(t)
	waitReady(t, f.main)
	waitReady(t, f.ref)

	// The span timestamps are taken now: the metrics generator discards spans older than its ingestion
	// slack (30s), so the base time must not predate the push.
	f.base = time.Now().Truncate(time.Second)
	f.from = f.base.Truncate(time.Minute).Add(-time.Minute)
	f.to = f.from.Add(5 * time.Minute)

	// Seed: main holds everything per tenant; the reference holds the granted spans per scenario.
	for _, ten := range []string{"t1", "t2", "t3"} {
		var sp []spanDef
		for _, s := range f.spans {
			if s.tenant == ten {
				sp = append(sp, s)
			}
		}
		push(t, f.main.URL("4318"), ten, sp, f.base)
	}
	for _, sc := range scenarios {
		if sc.unrestricted {
			continue
		}
		var sp []spanDef
		for _, s := range f.spans {
			if sc.grants(s) {
				sp = append(sp, s)
			}
		}
		if len(sp) == 0 {
			t.Fatalf("scenario %s grants no span", sc.name)
		}
		push(t, f.ref.URL("4318"), sc.refTenant(), sp, f.base)
	}

	mainClient := func(c *framework.Container, tenant string) api {
		cl := framework.NewClient(t, c.URL("3200"))
		cl.Headers = map[string]string{"X-Scope-OrgID": tenant}
		return api{c: cl, from: f.from, to: f.to}
	}
	for _, ten := range []string{"t1", "t2", "t3"} {
		f.directT[ten] = mainClient(f.main, ten)
	}
	for _, sc := range scenarios {
		if !sc.unrestricted {
			f.directT[sc.refTenant()] = mainClient(f.ref, sc.refTenant())
		}
	}

	// Wait until every tenant is fully searchable and queryable with metrics.
	want := map[string][]spanDef{}
	for _, s := range f.spans {
		want["main/"+s.tenant] = append(want["main/"+s.tenant], s)
	}
	for _, sc := range scenarios {
		if sc.unrestricted {
			continue
		}
		for _, s := range f.spans {
			if sc.grants(s) {
				want["ref/"+sc.refTenant()] = append(want["ref/"+sc.refTenant()], s)
			}
		}
	}
	for key, sp := range want {
		_, tenant, _ := strings.Cut(key, "/")
		a := f.directT[tenant]
		traces := map[string]bool{}
		for _, s := range sp {
			traces[s.trace] = true
		}
		waitFor(t, key+" searchable", 90*time.Second, func() bool {
			r := a.search("{}")
			if r.Status != 200 {
				t.Logf("%s: search status %d: %.200s", key, r.Status, r.Body)
				return false
			}
			return len(decodeSearch(t, r).Traces) == len(traces)
		})
		waitFor(t, key+" metrics", 120*time.Second, func() bool {
			r := a.metricsRange("{} | count_over_time()")
			if r.Status != 200 {
				return false
			}
			total := 0.0
			for _, pts := range metricsSig(t, r) {
				for _, p := range pts {
					var v float64
					_, _ = fmt.Sscanf(p[strings.Index(p, "=")+1:], "%g", &v)
					total += v
				}
			}
			return int(total+0.5) == len(sp)
		})
	}

	// Proxy: one cluster policy per scenario (the namespaced one as AccessPolicy).
	var cluster []v1.ClusterAccessPolicy
	var ap []v1.AccessPolicy
	nsLabels := map[string]map[string]string{}
	for _, sc := range scenarios {
		var rules []v1.ClusterRule
		if sc.unrestricted {
			r := framework.Unrestricted(backendName)
			r.Tenants = []string{sc.tenant}
			rules = append(rules, r)
		}
		for _, m := range sc.rules {
			if sc.namespaced != "" {
				continue
			}
			r := framework.Rule(backendName, m...)
			r.Tenants = []string{sc.tenant}
			if len(sc.deny) > 0 {
				r = framework.Deny(r, sc.deny...)
			}
			rules = append(rules, r)
		}
		if sc.namespaced != "" {
			ap = append(ap, framework.NamespacedPolicy(sc.namespaced, sc.name, framework.Group(sc.group()), v1.Rule{Backends: []string{backendName}}))
			nsLabels[sc.namespaced] = map[string]string{policy.TenantLabel: sc.tenant}
			continue
		}
		cluster = append(cluster, framework.ClusterPolicy(sc.name, framework.Group(sc.group()), rules...))
	}
	f.proxy = framework.StartProxy(t, framework.ProxyOptions{
		Backends: []config.Backend{{Name: backendName, Type: config.BackendTempo, URL: f.main.URL("3200"),
			PathPrefix: "/tempo", NamespaceLabel: nsKey, TenantHeader: "X-Scope-OrgID", TraceByID: config.TraceByIDFilter}},
		Policies: framework.Policies{Cluster: cluster, Namespaced: ap, NamespaceLabels: nsLabels},
	})
	f.proxyUsers = map[string]api{}
	for _, sc := range scenarios {
		f.proxyUsers[sc.name] = api{c: f.proxy.As(sc.user(), sc.group()), prefix: "/tempo", from: f.from, to: f.to}
	}
	snap := policy.NewSnapshot(policy.Compile(policy.Input{Config: f.proxy.Config, ClusterPolicies: cluster, Policies: ap, NamespaceLabels: nsLabels}), f.proxy.Config, 1)
	for _, sc := range scenarios {
		eff, err := snap.Resolve(sc.user(), []string{sc.group()}, backendName)
		if err != nil {
			t.Fatalf("resolve %s: %v", sc.name, err)
		}
		f.eff[sc.name] = eff
	}
	return f
}

func waitFor(t testing.TB, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if ok() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not ready after %s", what, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// as returns the proxy client of the scenario's user.
func (f *fixture) as(sc scenario) api { return f.proxyUsers[sc.name] }

// direct returns the direct client of the main Tempo for the scenario's tenant.
func (f *fixture) direct(sc scenario) api { return f.directT[sc.tenant] }

// oracle returns the client of the reference tenant holding only what the scenario grants.
func (f *fixture) oracle(sc scenario) api { return f.directT[sc.refTenant()] }

// grantedAttrNames returns the attribute names present on granted spans, as "span/x" and "resource/x".
func (f *fixture) grantedAttrNames(sc scenario) map[string]bool {
	out := map[string]bool{}
	for _, s := range f.spans {
		if !sc.grants(s) {
			continue
		}
		for k := range s.spanAttrs {
			out["span/"+k] = true
		}
		if s.code != 0 {
			out["span/http.status_code"] = true
		}
		for k := range s.resource() {
			out["resource/"+k] = true
		}
	}
	return out
}

func TestTempoE2E(t *testing.T) {
	if os.Getenv("QUERDIAN_E2E_SKIP_TEMPO") != "" {
		t.Skip("skipped by QUERDIAN_E2E_SKIP_TEMPO")
	}
	f := newFixture(t)
	t.Run("seed", func(t *testing.T) { testSeed(t, f) })
	t.Run("isolation", func(t *testing.T) { testIsolation(t, f) })
	t.Run("trace-by-id", func(t *testing.T) { testTraceByID(t, f) })
	t.Run("tags", func(t *testing.T) { testTags(t, f) })
	t.Run("denied-attributes", func(t *testing.T) { testDenied(t, f) })
	t.Run("parser-boundaries", func(t *testing.T) { testParserBoundaries(t, f) })
	t.Run("root-oracle", func(t *testing.T) { testRootOracle(t, f) })
	t.Run("equivalence", func(t *testing.T) { testCorpus(t, f, false) })
	t.Run("differential", func(t *testing.T) { testCorpus(t, f, true) })
	t.Run("endpoints", func(t *testing.T) { testEndpoints(t, f) })
}
