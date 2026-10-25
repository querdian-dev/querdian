//go:build e2e

// Package prometheus holds the Prometheus end-to-end suite (T2.4): a real Prometheus container seeded
// with data of three namespaces, the real proxy handler chain in-process, and a test OIDC issuer.
package prometheus

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/test/e2e/framework"
)

// api is a Prometheus HTTP API client; the same code talks to the proxy (prefix "/prom") and to
// Prometheus directly (prefix "").
type api struct {
	c      *framework.Client
	prefix string
	end    time.Time
}

func (a api) instant(q string) *framework.Response {
	return a.c.Get(a.prefix+"/api/v1/query", url.Values{"query": {q}, "time": {tsStr(a.end)}})
}

func (a api) rng(q string) *framework.Response {
	return a.c.Get(a.prefix+"/api/v1/query_range", url.Values{
		"query": {q}, "start": {tsStr(a.end.Add(-10 * time.Minute))}, "end": {tsStr(a.end)}, "step": {"60"}})
}

func (a api) post(path string, form url.Values) *framework.Response {
	return a.c.PostForm(a.prefix+"/api/v1/"+path, form)
}

func (a api) series(matches ...string) *framework.Response {
	return a.c.Get(a.prefix+"/api/v1/series", url.Values{"match[]": matches, "start": {tsStr(a.end.Add(-time.Hour))}, "end": {tsStr(a.end)}})
}

func (a api) labels(matches ...string) *framework.Response {
	q := url.Values{"start": {tsStr(a.end.Add(-time.Hour))}, "end": {tsStr(a.end)}}
	if len(matches) > 0 {
		q["match[]"] = matches
	}
	return a.c.Get(a.prefix+"/api/v1/labels", q)
}

func (a api) labelValues(name string, matches ...string) *framework.Response {
	q := url.Values{"start": {tsStr(a.end.Add(-time.Hour))}, "end": {tsStr(a.end)}}
	if len(matches) > 0 {
		q["match[]"] = matches
	}
	return a.c.Get(a.prefix+"/api/v1/label/"+name+"/values", q)
}

func (a api) exemplars(q string) *framework.Response {
	return a.c.Get(a.prefix+"/api/v1/query_exemplars", url.Values{"query": {q}, "start": {tsStr(a.end.Add(-time.Hour))}, "end": {tsStr(a.end)}})
}

func tsStr(t time.Time) string { return strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', 3, 64) }

// fixture is everything the subtests share.
type fixture struct {
	end    time.Time
	prom   *framework.Container        // all data
	refs   map[string]api              // per term set: Prometheus seeded with only the data the term set grants
	proxy  *framework.Proxy            // policies for every scenario below
	direct api                         // direct access to prom
	eff    map[string]policy.Effective // per term set, as resolved by the real policy engine
}

// users (group -> allowed namespaces) for the isolation scenarios.
var isolationUsers = []struct {
	user, group string
	allowed     []string
}{
	{"alice", "team-a", []string{"ns-a"}},
	{"bob", "team-b", []string{"ns-b"}},
	{"carol", "team-ab", []string{"ns-a", "ns-b"}}, // two AccessPolicies = two terms
}

func newFixture(t *testing.T) *fixture {
	framework.RequireRuntime(t)
	f := &fixture{end: time.Now().Truncate(seedStep), refs: map[string]api{}, eff: map[string]policy.Effective{}}

	// Start all Prometheus containers concurrently: the main one and one reference per term set.
	f.prom = startPrometheus(t, f.end, nil)
	f.direct = api{c: framework.NewClient(t, f.prom.URL("9090")), end: f.end}
	for _, ts := range termSets {
		var terms [][]*labels.Matcher
		for _, term := range ts.terms {
			terms = append(terms, toLabelMatchers(t, term))
		}
		c := startPrometheus(t, f.end, func(l labels.Labels) bool { return visible(l, terms) })
		f.refs[ts.name] = api{c: framework.NewClient(t, c.URL("9090")), end: f.end}
	}

	var cluster []v1.ClusterAccessPolicy
	for _, ts := range termSets {
		var rules []v1.ClusterRule
		for _, term := range ts.terms {
			rules = append(rules, framework.Rule("metrics", term...))
		}
		cluster = append(cluster, framework.ClusterPolicy(ts.name, framework.Group(ts.group), rules...))
	}
	// Denied labels: one term and several terms.
	cluster = append(cluster,
		framework.ClusterPolicy("deny-single", framework.Group("deny-1"),
			framework.Deny(framework.Rule("metrics", eq("namespace", "ns-a")), "customer_id")),
		framework.ClusterPolicy("deny-multi", framework.Group("deny-2"),
			framework.Deny(framework.Rule("metrics", eq("namespace", "ns-a")), "customer_id"),
			framework.Rule("metrics", eq("job", "api"))),
		framework.ClusterPolicy("admin", framework.Group("admin"), framework.Unrestricted("metrics")),
		framework.ClusterPolicy("redact-single", framework.Group("redact-1"),
			framework.Redact(framework.Rule("metrics", eq("namespace", "ns-a")), v1.RedactPlaceholder, "customer_id")),
	)
	var ap []v1.AccessPolicy
	nsLabels := map[string]map[string]string{}
	for _, ns := range namespaces {
		nsLabels[ns] = map[string]string{}
	}
	ap = append(ap,
		framework.NamespacedPolicy("ns-a", "a", framework.Group("team-a", "team-ab"), v1.Rule{Backends: []string{"metrics"}}),
		framework.NamespacedPolicy("ns-b", "b", framework.Group("team-b", "team-ab"), v1.Rule{Backends: []string{"metrics"}}),
	)

	f.proxy = framework.StartProxy(t, framework.ProxyOptions{
		Backends: []config.Backend{{Name: "metrics", Type: config.BackendPrometheus, URL: f.prom.URL("9090"),
			PathPrefix: "/prom", NamespaceLabel: "namespace"}},
		Policies: framework.Policies{Cluster: cluster, Namespaced: ap, NamespaceLabels: nsLabels},
	})
	// Resolve each term set with the real engine to know how many DNF terms remain after normalisation.
	snap := policy.NewSnapshot(policy.Compile(policy.Input{Config: f.proxy.Config, ClusterPolicies: cluster}), f.proxy.Config, 1)
	for _, ts := range termSets {
		eff, err := snap.Resolve("u", []string{ts.group}, "metrics")
		if err != nil {
			t.Fatal(err)
		}
		f.eff[ts.name] = eff
	}
	return f
}

func (f *fixture) as(group string) api {
	return api{c: f.proxy.As("u-"+group, group), prefix: "/prom", end: f.end}
}

func TestPrometheusE2E(t *testing.T) {
	f := newFixture(t)

	// Sanity: the seeded data is queryable and distinct per namespace.
	t.Run("seed", func(t *testing.T) {
		r := f.direct.instant(`count({__name__=~".+"})`)
		got := normalise(t, r, false)
		want := len(allSeries())
		if v := got[""]; len(v) != 1 && len(got) != 1 {
			t.Fatalf("unexpected count result %v", got)
		}
		for _, v := range got {
			if !strings.HasSuffix(v[0], "="+strconv.Itoa(want)) {
				t.Fatalf("seeded %v, want %d series", v, want)
			}
		}
	})

	t.Run("isolation", func(t *testing.T) { testIsolation(t, f) })
	t.Run("equivalence", func(t *testing.T) { testEquivalence(t, f) })
	t.Run("denied-labels", func(t *testing.T) { testDeniedLabels(t, f) })
	t.Run("redaction", func(t *testing.T) { testRedaction(t, f) })
	t.Run("endpoints-and-auth", func(t *testing.T) { testEndpoints(t, f) })
}

// --- isolation ---------------------------------------------------------------------------------

func seriesFor(allowed []string) []seriesDef {
	var out []seriesDef
	for _, s := range allSeries() {
		if slices.Contains(allowed, s.labels.Get("namespace")) {
			out = append(out, s)
		}
	}
	return out
}

func testIsolation(t *testing.T, f *fixture) {
	for _, u := range isolationUsers {
		t.Run(u.user, func(t *testing.T) {
			a := api{c: f.proxy.As(u.user, u.group), prefix: "/prom", end: f.end}
			want := seriesFor(u.allowed)
			wantSets := map[string]bool{}
			wantNames := map[string]bool{}
			wantNS := map[string]bool{}
			for _, s := range want {
				wantSets[labelKey(s.labels.Map())] = true
				s.labels.Range(func(l labels.Label) { wantNames[l.Name] = true })
				wantNS[s.labels.Get("namespace")] = true
			}
			allNS := func(m map[string]string) {
				t.Helper()
				if ns := m["namespace"]; !slices.Contains(u.allowed, ns) {
					t.Fatalf("leak: series %v outside namespaces %v", m, u.allowed)
				}
			}

			// query / query_range: all series with a name.
			for _, call := range []func(string) *framework.Response{a.instant, a.rng} {
				r := call(`{__name__=~".+"}`)
				if r.Status != 200 {
					t.Fatalf("status %d %s", r.Status, r.Body)
				}
				res := normalise(t, r, false)
				if len(res) != len(want) {
					t.Fatalf("got %d series, want %d", len(res), len(want))
				}
				for k := range res {
					if !wantSets[k] {
						t.Fatalf("unexpected series %s", k)
					}
				}
			}

			// series: with name selector, with a narrower selector, and several match[].
			for _, ms := range [][]string{{`{__name__=~".+"}`}, {`up`}, {`up`, `temperature`}} {
				r := a.series(ms...)
				var d struct{ Data []map[string]string }
				r.JSON(t, &d)
				if r.Status != 200 || len(d.Data) == 0 {
					t.Fatalf("series %v: %d %s", ms, r.Status, r.Body)
				}
				for _, m := range d.Data {
					allNS(m)
				}
				if ms[0] == `{__name__=~".+"}` {
					if len(d.Data) != len(want) {
						t.Fatalf("series: %d, want %d", len(d.Data), len(want))
					}
				}
			}

			// labels: exactly the label names of the visible series (no match[] from the client).
			var ln struct{ Data []string }
			r := a.labels()
			r.JSON(t, &ln)
			if r.Status != 200 {
				t.Fatalf("labels: %d %s", r.Status, r.Body)
			}
			for _, n := range ln.Data {
				if !wantNames[n] {
					t.Fatalf("labels lists %q which no visible series has", n)
				}
			}
			if len(ln.Data) != len(wantNames) {
				t.Fatalf("labels = %v, want %d names", ln.Data, len(wantNames))
			}

			// label values: namespace exactly the allowed ones, with and without match[].
			for _, ms := range [][]string{nil, {`up`}, {`{__name__=~".+"}`}} {
				var lv struct{ Data []string }
				r := a.labelValues("namespace", ms...)
				r.JSON(t, &lv)
				if r.Status != 200 {
					t.Fatalf("label values: %d %s", r.Status, r.Body)
				}
				got := slices.Clone(lv.Data)
				slices.Sort(got)
				if !slices.Equal(got, u.allowed) {
					t.Fatalf("namespace values = %v, want %v", got, u.allowed)
				}
			}
			// pod values never contain other namespaces' pods.
			var pv struct{ Data []string }
			a.labelValues("pod").JSON(t, &pv)
			for _, p := range pv.Data {
				ok := false
				for _, ns := range u.allowed {
					ok = ok || strings.HasSuffix(p, "-"+ns+"-0")
				}
				if !ok {
					t.Fatalf("pod %q leaks from another namespace", p)
				}
			}

			// exemplars: only exemplars of allowed namespaces, and not vacuous.
			r = a.exemplars(`http_requests_total`)
			var ex struct {
				Data []struct {
					SeriesLabels map[string]string `json:"seriesLabels"`
					Exemplars    []struct {
						Labels map[string]string `json:"labels"`
					} `json:"exemplars"`
				}
			}
			r.JSON(t, &ex)
			if r.Status != 200 || len(ex.Data) == 0 {
				t.Fatalf("exemplars: %d %s", r.Status, r.Body)
			}
			for _, e := range ex.Data {
				allNS(e.SeriesLabels)
				for _, x := range e.Exemplars {
					if ns := strings.Split(x.Labels["trace_id"], "-"); len(ns) < 3 || !slices.Contains(u.allowed, ns[1]+"-"+ns[2]) {
						t.Fatalf("exemplar %v leaks", x.Labels)
					}
				}
			}

			// POST form works the same and is enforced.
			r = a.post("query", url.Values{"query": {`count by (namespace) ({__name__=~".+"})`}, "time": {tsStr(f.end)}})
			for k := range normalise(t, r, false) {
				if !strings.Contains(k, `namespace="`+u.allowed[0]+`"`) && (len(u.allowed) == 1 || !strings.Contains(k, `namespace="`+u.allowed[1]+`"`)) {
					t.Fatalf("POST leak: %s", k)
				}
			}

			// Adversarial queries that try to reach foreign data must return nothing foreign.
			foreign := "ns-c"
			if u.user == "alice" {
				foreign = "ns-b"
			}
			for _, q := range []string{
				`{namespace="` + foreign + `"}`,
				`up{namespace="` + foreign + `"}`,
				`{namespace=~"ns-.*", namespace!~"` + strings.Join(u.allowed, "|") + `"}`,
				`up{namespace!="` + u.allowed[0] + `"} and up{namespace="` + foreign + `"}`,
				`count({namespace="` + foreign + `"})`,
				`max_over_time({namespace="` + foreign + `"}[5m:1m])`,
				`label_replace(up{namespace="` + foreign + `"}, "x", "$1", "job", "(.*)")`,
				`sum by (namespace) (up{namespace="` + foreign + `"})`,
				`absent(up{namespace="` + foreign + `"})`, // answers about the visible data only: nothing there, so 1
			} {
				res := normalise(t, a.instant(q), false)
				if strings.HasPrefix(q, "absent") {
					if len(res) != 1 {
						t.Fatalf("%s: %v", q, res)
					}
					continue
				}
				if len(res) != 0 {
					t.Fatalf("query %s should return nothing, got %v", q, res)
				}
			}
			// A metric that exists in ns-b only is visible exactly to users who may see ns-b.
			wantB := 0
			if slices.Contains(u.allowed, "ns-b") {
				wantB = 1
			}
			if n := len(normalise(t, a.instant(`ns_b_only_total`), false)); n != wantB {
				t.Fatalf("ns_b_only_total: %d series, want %d", n, wantB)
			}
			for _, ms := range []string{`{namespace="` + foreign + `"}`, `ns_b_only_total`} {
				var d struct{ Data []map[string]string }
				a.series(ms).JSON(t, &d)
				for _, m := range d.Data {
					allNS(m)
				}
				var lv struct{ Data []string }
				a.labelValues("namespace", ms).JSON(t, &lv)
				for _, v := range lv.Data {
					if !slices.Contains(u.allowed, v) {
						t.Fatalf("label values for match %s leak %q", ms, v)
					}
				}
			}
		})
	}
}

// --- equivalence -------------------------------------------------------------------------------

func hasNameMatcher(ts termSet) bool {
	for _, term := range ts.terms {
		for _, m := range term {
			if m.Name == "__name__" {
				return true
			}
		}
	}
	return false
}

func testEquivalence(t *testing.T, f *fixture) {
	if len(corpus)*len(termSets) < 40 {
		t.Fatal("corpus too small")
	}
	checks, empty := 0, 0
	for _, ts := range termSets {
		t.Run(ts.name, func(t *testing.T) {
			multi := len(f.eff[ts.name].Terms) > 1
			t.Logf("policy terms after normalisation: %d", len(f.eff[ts.name].Terms))
			user := f.as(ts.group)
			ref := f.refs[ts.name]
			for _, c := range corpus {
				for _, mode := range []string{"instant", "range"} {
					if c.instantOnly && mode == "range" {
						continue
					}
					q := c.userForm()
					call := func(a api) *framework.Response {
						if mode == "range" {
							return a.rng(q)
						}
						return a.instant(q)
					}
					r := call(user)
					a := decode(t, r)
					if c.rejectMulti && multi {
						if r.Status != 400 || !a.fromProxy() {
							t.Errorf("[%s] %s: want proxy 400 (unsupported), got %d %s", mode, q, r.Status, r.Body)
						}
						continue
					}
					if r.Status != 200 {
						// A 400 without traceId would be Prometheus rejecting the rewritten query.
						t.Errorf("[%s] %s: status %d (proxy-originated=%v): %s", mode, q, r.Status, a.fromProxy(), r.Body)
						continue
					}
					checks++
					got := normalise(t, r, c.valuesOnly)
					want := normalise(t, call(ref), c.valuesOnly)
					if d := diff(want, got); d != "" {
						t.Errorf("[%s] %s: result differs from the query over the granted data\n  %s", mode, q, d)
						continue
					}
					if len(want) == 0 && !strings.HasPrefix(q, "absent") && !strings.Contains(q, "nonexistent") && !strings.Contains(q, "> bool") {
						empty++
					}

					// Pointwise queries additionally equal the union of the per-term queries run directly.
					if c.pointwise && !c.valuesOnly && !hasNameMatcher(ts) {
						var parts []result
						for _, term := range ts.terms {
							tq := c.termForm(term)
							var rr *framework.Response
							if mode == "range" {
								rr = f.direct.rng(tq)
							} else {
								rr = f.direct.instant(tq)
							}
							parts = append(parts, normalise(t, rr, false))
						}
						u, conflict := union(parts)
						if conflict != "" {
							t.Errorf("[%s] %s: %s", mode, q, conflict)
							continue
						}
						if d := diff(u, got); d != "" {
							t.Errorf("[%s] %s: result differs from the union of per-term results\n  %s", mode, q, d)
						}
					}
				}
			}
		})
	}
	t.Logf("%d comparisons (%d queries x %d term sets x instant/range), %d with an empty expected result", checks, len(corpus), len(termSets), empty)
}

// --- denied labels -----------------------------------------------------------------------------

func testDeniedLabels(t *testing.T, f *fixture) {
	for _, group := range []string{"deny-1", "deny-2"} {
		t.Run(group, func(t *testing.T) {
			a := f.as(group)
			// Queries referencing the denied label are forbidden: matcher, by, without, on, label_replace, ...
			for _, q := range []string{
				`temperature{customer_id="cust-ns-a-api"}`,
				`temperature{customer_id=~".+"}`,
				`sum by (customer_id) (temperature)`,
				`sum without (customer_id) (temperature)`,
				`temperature / on(customer_id) temperature`,
				`label_replace(temperature, "x", "$1", "customer_id", "(.*)")`,
				`label_join(temperature, "x", "-", "customer_id")`,
				`rate(http_requests_total[5m]) / ignoring(customer_id) up`,
				`count({customer_id!=""})`,
			} {
				for _, call := range []func(string) *framework.Response{a.instant, a.rng} {
					r := call(q)
					if r.Status != 403 || !decode(t, r).fromProxy() {
						t.Errorf("%s: want 403 forbidden, got %d %s", q, r.Status, r.Body)
					}
				}
			}
			if r := a.labelValues("customer_id"); r.Status != 403 {
				t.Errorf("label/customer_id/values: %d %s", r.Status, r.Body)
			}
			if r := a.series(`temperature{customer_id="x"}`); r.Status != 403 {
				t.Errorf("series with denied matcher: %d %s", r.Status, r.Body)
			}
			if r := a.exemplars(`http_requests_total{customer_id="x"}`); r.Status != 403 {
				t.Errorf("exemplars with denied matcher: %d %s", r.Status, r.Body)
			}

			// labels: the denied name is gone, others remain.
			var ln struct{ Data []string }
			a.labels().JSON(t, &ln)
			if slices.Contains(ln.Data, "customer_id") || !slices.Contains(ln.Data, "namespace") {
				t.Errorf("labels not filtered: %v", ln.Data)
			}
			// series: label sets lose the denied label, identical sets are not repeated.
			r := a.series(`temperature`)
			var sr struct{ Data []map[string]string }
			r.JSON(t, &sr)
			if r.Status != 200 || len(sr.Data) == 0 {
				t.Fatalf("series: %d %s", r.Status, r.Body)
			}
			seen := map[string]bool{}
			for _, m := range sr.Data {
				if _, ok := m["customer_id"]; ok {
					t.Errorf("series response contains the denied label: %v", m)
				}
				k := labelKey(m)
				if seen[k] {
					t.Errorf("duplicate label set after filtering: %v", m)
				}
				seen[k] = true
			}
			// Query results are NOT filtered (documented): the denied label is still returned.
			res := normalise(t, a.instant(`temperature`), false)
			found := false
			for k := range res {
				found = found || strings.Contains(k, `customer_id="cust-ns-a-`)
			}
			if !found {
				t.Errorf("query results must keep denied labels untouched (PLAN §7.2): %v", res)
			}
			// Unaffected queries still work.
			if r := a.instant(`up`); r.Status != 200 {
				t.Errorf("up: %d %s", r.Status, r.Body)
			}
		})
	}
}

// --- other endpoints, authentication ------------------------------------------------------------

func testEndpoints(t *testing.T, f *fixture) {
	a := f.as("ts-0")
	for _, p := range []string{"/api/v1/targets", "/api/v1/metadata", "/api/v1/rules", "/api/v1/alerts", "/api/v1/status/config", "/api/v1/admin/tsdb/snapshot", "/-/reload"} {
		r := a.c.Get(a.prefix+p, nil)
		if r.Status != 403 {
			t.Errorf("%s: want 403, got %d", p, r.Status)
		}
	}
	if r := f.proxy.Anonymous().Get("/prom/api/v1/query", url.Values{"query": {"up"}}); r.Status != 401 {
		t.Errorf("anonymous: %d", r.Status)
	}
	nobody := api{c: f.proxy.As("nobody", "no-such-group"), prefix: "/prom", end: f.end}
	if r := nobody.instant("up"); r.Status != 403 {
		t.Errorf("no grant: %d %s", r.Status, r.Body)
	}
	// Unrestricted admin sees everything, identical to Prometheus itself.
	admin := f.as("admin")
	want := normalise(t, f.direct.instant(`count by (namespace) ({__name__=~".+"})`), false)
	got := normalise(t, admin.instant(`count by (namespace) ({__name__=~".+"})`), false)
	if d := diff(want, got); d != "" {
		t.Errorf("admin differs from direct access: %s", d)
	}
	// Unparseable queries are rejected by the proxy, never passed through.
	if r := a.instant(`sum(`); r.Status != 400 || !decode(t, r).fromProxy() {
		t.Errorf("parse error: %d %s", r.Status, r.Body)
	}
}
