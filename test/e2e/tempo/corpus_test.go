//go:build e2e

package tempo

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/querdian-dev/querdian/test/e2e/framework"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// tq is one query of the corpus with the facts the checks depend on.
type tq struct {
	q string
	// metrics: the query ends in a metrics function and is sent to the metrics endpoint.
	metrics bool
	// structural: uses a structural operator; the reference oracle is not defined (see the package doc).
	structural bool
	// negated: uses a negated structural operator (the docs call them experimental): only isolation is checked.
	negated bool
	// scalar: has a scalar filter stage, which is not monotone in the set of spans.
	scalar bool
	// traceLevel: references trace-level intrinsics (root*, traceDuration); rejected for restricted users.
	traceLevel bool
	// customer: references span.customer_id (denied for the "ns-a-deny" scenario).
	customer bool
	// noCompare: the result is not deterministic (sampling), accepted/rejected is the only check.
	noCompare bool
}

func (c tq) String() string { return c.q }

// fixedCorpus is hand-written: examples of the public TraceQL documentation plus the cases that matter
// for enforcement.
func fixedCorpus() []tq {
	plain := func(qs ...string) []tq {
		var out []tq
		for _, q := range qs {
			out = append(out, tq{q: q})
		}
		return out
	}
	var out []tq
	out = append(out, plain(
		`{}`,
		`{ true }`,
		`{ false }`,
		`{ status = error }`,
		`{ status != error }`,
		`{ kind = client }`,
		`{ name = "load" }`,
		`{ name =~ "op-[0-1]" }`,
		`{ name !~ "root.*" }`,
		`{ span:name = "query" }`,
		`{ span:status = error }`,
		`{ span:kind = server }`,
		`{ duration > 100ms }`,
		`{ span:duration <= 50ms }`,
		`{ resource.service.name = "cart" }`,
		`{ resource.service.name =~ "cart|db|worker" }`,
		`{ resource.service.name != "frontend" }`,
		`{ .service.name = "payment" }`,
		`{ resource.k8s.namespace.name = "ns-b" }`,
		`{ resource.k8s.namespace.name =~ "ns-(a|c)" }`,
		`{ resource.k8s.namespace.name != "ns-a" }`,
		`{ span.http.status_code = 500 }`,
		`{ span.http.status_code >= 400 && span.http.status_code < 600 }`,
		`{ span.http.status_code != 200 }`,
		`{ span.http.status_code - 1 = 499 }`,
		`{ span.http.method = "POST" }`,
		`{ span.http.method =~ "P.*" && status = error }`,
		`{ span.team = "alpha" || span.team = "beta" }`,
		`{ span.team = "alpha" && span.http.method = "GET" || status = error }`,
		`{ (span.team = "alpha" || status = error) && resource.k8s.namespace.name = "ns-c" }`,
		`{ span.nonexistent = "x" }`,
		`{ span.nonexistent != "x" }`,
		`{ span.http.method != nil }`,
		`{ resource.k8s.namespace.name = "ns-a" && resource.k8s.namespace.name = "ns-b" }`,
		`{ resource.k8s.namespace.name = "ns-a" } && { resource.k8s.namespace.name = "ns-b" }`,
		`{ resource.k8s.namespace.name = "ns-a" } || { resource.k8s.namespace.name = "ns-b" }`,
		`{ status = error } && { resource.service.name = "cart" }`,
		`({ status = error }) || ({ span.team = "beta" })`,
		`{ status = error } | count() > 1`,
		`{ } | count() >= 3`,
		`{ } | count() < 2`,
		`{ } | avg(duration) > 100ms`,
		`{ } | max(span.http.status_code) >= 500`,
		`{ } | min(duration) < 20ms`,
		`{ } | sum(duration) > 300ms`,
		`count() > 2`,
		`{ } | by(resource.service.name)`,
		`{ } | by(span.team) | count() > 1`,
		`{ } | by(resource.k8s.namespace.name) | avg(duration) > 50ms`,
		`{ } | select(span.http.method, resource.service.name)`,
		`{ status = error } | select(span.http.status_code)`,
		`{ } | by(resource.service.name) | coalesce() | count() > 1`,
		`{ } | coalesce()`,
		`{ span.team = "alpha" } | { status = error }`,
		`{ } | count() > 1 | { status = error }`,
		`{ resource.service.name = "frontend" } | select(name)`,
	)...)
	for _, op := range []string{">>", ">", "<<", "<", "~"} {
		out = append(out, tq{q: `{ resource.service.name = "frontend" } ` + op + ` { status = error }`, structural: true})
		out = append(out, tq{q: `{ } ` + op + ` { span.team = "alpha" }`, structural: true})
	}
	for _, op := range []string{"&>>", "&>", "&<<", "&<", "&~"} {
		out = append(out, tq{q: `{ resource.k8s.namespace.name = "ns-b" } ` + op + ` { status = error }`, structural: true})
	}
	for _, op := range []string{"!>>", "!>", "!<<", "!<", "!~"} {
		out = append(out, tq{q: `{ resource.service.name = "frontend" } ` + op + ` { status = error }`, structural: true, negated: true})
	}
	out = append(out, tq{q: `{ } >> { } | count() > 1`, structural: true, scalar: true})

	for _, q := range []string{
		`{ } | rate()`,
		`{ status = error } | rate()`,
		`{ } | count_over_time()`,
		`{ } | rate() by (resource.service.name)`,
		`{ } | count_over_time() by (resource.k8s.namespace.name)`,
		`{ status = error } | count_over_time() by (span.http.method)`,
		`{ span.team = "alpha" } | avg_over_time(duration)`,
		`{ } | min_over_time(duration) by (resource.k8s.namespace.name)`,
		`{ } | max_over_time(duration) by (resource.service.name)`,
		`{ } | sum_over_time(span.http.status_code)`,
		`{ } | quantile_over_time(duration, .5, .9)`,
		`{ } | quantile_over_time(duration, .99) by (span.http.method)`,
		`{ } | histogram_over_time(duration)`,
		`{ resource.k8s.namespace.name = "ns-b" } | rate()`,
		`{ span.http.status_code >= 500 } | count_over_time() with (exemplars=false)`,
		`{ status = error } | rate() by (resource.service.name, span.http.method)`,
	} {
		out = append(out, tq{q: q, metrics: true})
	}
	out = append(out,
		tq{q: `{ } | compare({ status = error })`, metrics: true, noCompare: true, traceLevel: true},
		// topk/bottomk break ties arbitrarily: accepted/enforced only.
		tq{q: `{ } | rate() by (resource.service.name) | topk(2)`, metrics: true, noCompare: true},
		tq{q: `{ } | count_over_time() by (resource.service.name) | bottomk(2)`, metrics: true, noCompare: true},
		tq{q: `{ resource.service.name = "frontend" } >> { status = error } | rate()`, metrics: true, structural: true},
		tq{q: `{ } | rate() by (span.customer_id)`, metrics: true, customer: true},
		tq{q: `{ span.customer_id = "cust-ns-a-0" }`, customer: true},
		tq{q: `{ } | select(span.customer_id)`, customer: true},
		tq{q: `{ } | by(.customer_id)`, customer: true},
		tq{q: `{ rootServiceName = "frontend" }`, traceLevel: true},
		tq{q: `{ trace:rootService = "billing-gateway" }`, traceLevel: true},
		tq{q: `{ rootName =~ "POST.*" }`, traceLevel: true},
		tq{q: `{ trace:rootName != "x" } | count() > 0`, traceLevel: true},
		tq{q: `{ traceDuration > 100ms }`, traceLevel: true},
		tq{q: `{ trace:duration < 10s && status = error }`, traceLevel: true},
		tq{q: `{ } | select(rootServiceName, rootName)`, traceLevel: true},
		tq{q: `{ } | by(trace:rootService) | count() > 0`, traceLevel: true},
		tq{q: `{ } | rate() by (rootServiceName)`, metrics: true, traceLevel: true},
		tq{q: `{ rootName = "cron" } | count_over_time()`, metrics: true, traceLevel: true},
	)
	return out
}

// --- generator ------------------------------------------------------------------------------------------

type gen struct {
	r         *rand.Rand
	traceBias bool
}

func (g *gen) pick(s ...string) string { return s[g.r.Intn(len(s))] }

// atom returns one comparison; flags are set on the query being built.
func (g *gen) atom(c *tq) string {
	switch n := g.r.Intn(100); {
	case n < 12:
		return fmt.Sprintf("%s %s %s", g.pick("span.http.status_code", ".http.status_code"), g.pick("=", "!=", ">", ">=", "<", "<="), g.pick("200", "404", "500", "201", "0"))
	case n < 22:
		return fmt.Sprintf("%s %s %q", g.pick("span.http.method", ".http.method"), g.pick("=", "!=", "=~", "!~"), g.pick("GET", "POST", "P.*", "GET|PUT", "DELETE", ""))
	case n < 32:
		return fmt.Sprintf("%s %s %q", g.pick("span.team", ".team"), g.pick("=", "!=", "=~", "!~"), g.pick("alpha", "beta", "al.*", "gamma"))
	case n < 44:
		return fmt.Sprintf("%s %s %q", g.pick("resource.service.name", ".service.name"), g.pick("=", "!=", "=~", "!~"),
			g.pick("frontend", "cart", "payment", "db", "worker", "search", "billing-gateway", "t2-svc", "cart|db", ".*e.*"))
	case n < 56:
		return fmt.Sprintf("%s %s %q", g.pick("resource.k8s.namespace.name", ".k8s.namespace.name"), g.pick("=", "!=", "=~", "!~"),
			g.pick("ns-a", "ns-b", "ns-c", "ns-d", "ns-(a|b)", "ns-.*"))
	case n < 62:
		return fmt.Sprintf("%s %s %q", g.pick("name", "span:name"), g.pick("=", "!=", "=~", "!~"), g.pick("load", "query", "ping", "audit", "op-1", "op-.*", "root-.*"))
	case n < 68:
		return "status " + g.pick("=", "!=") + " " + g.pick("error", "ok", "unset")
	case n < 72:
		return "kind " + g.pick("=", "!=") + " " + g.pick("server", "client")
	case n < 80:
		return fmt.Sprintf("duration %s %s", g.pick(">", ">=", "<", "<="), g.pick("10ms", "50ms", "100ms", "200ms", "1s"))
	case n < 83:
		return g.pick("true", "false")
	case n < 86:
		return "span.nonexistent " + g.pick("=", "!=", "=~") + ` "x"`
	case n < 88:
		return g.pick("span.http.method", "resource.service.name", "span.team") + " != nil"
	case n < 90:
		return fmt.Sprintf("span.http.status_code %s %s", g.pick("+", "*"), g.pick("1 > 400", "2 = 1000", "1 = 201"))
	case n < 92 && g.traceBias:
		c.traceLevel = true
		return g.pick(`rootServiceName = "frontend"`, `trace:rootService =~ "b.*"`, `rootName != "x"`, `trace:rootName = "cron"`,
			`traceDuration > 100ms`, `trace:duration < 1s`)
	case n < 94:
		c.customer = true
		return fmt.Sprintf("span.customer_id %s %q", g.pick("=", "!=", "=~"), g.pick("cust-ns-a-0", "cust-.*", "x"))
	default:
		return fmt.Sprintf("span.http.status_code %s %s", g.pick("=", ">", "<"), g.pick("200", "500"))
	}
}

func (g *gen) cond(c *tq, depth int) string {
	if depth == 0 || g.r.Intn(3) == 0 {
		return g.atom(c)
	}
	switch g.r.Intn(4) {
	case 0:
		return g.cond(c, depth-1) + " && " + g.cond(c, depth-1)
	case 1:
		return g.cond(c, depth-1) + " || " + g.cond(c, depth-1)
	case 2:
		return "(" + g.cond(c, depth-1) + " || " + g.cond(c, depth-1) + ") && " + g.atom(c)
	default:
		return g.atom(c) + " && (" + g.cond(c, depth-1) + " || " + g.cond(c, depth-1) + ")"
	}
}

func (g *gen) filter(c *tq) string {
	if g.r.Intn(12) == 0 {
		return "{ }"
	}
	return "{ " + g.cond(c, 2) + " }"
}

func (g *gen) spanset(c *tq) string {
	switch g.r.Intn(10) {
	case 0:
		return g.filter(c) + " && " + g.filter(c)
	case 1:
		return g.filter(c) + " || " + g.filter(c)
	case 2:
		return "(" + g.filter(c) + ") && (" + g.filter(c) + ")"
	case 3, 4:
		ops := []string{">>", ">", "<<", "<", "~", "&>>", "&>", "&<<", "&<", "&~", "!>>", "!>", "!<<", "!<", "!~"}
		op := ops[g.r.Intn(len(ops))]
		c.structural = true
		c.negated = strings.HasPrefix(op, "!")
		return g.filter(c) + " " + op + " " + g.filter(c)
	default:
		return g.filter(c)
	}
}

func (g *gen) query() tq {
	var c tq
	c.q = g.spanset(&c)
	switch n := g.r.Intn(20); {
	case n < 7:
	case n < 9:
		c.q += " | count() " + g.pick(">", ">=", "<", "=") + " " + g.pick("0", "1", "2", "3")
		c.scalar = true
	case n < 10:
		c.q += " | avg(duration) " + g.pick(">", "<") + " " + g.pick("50ms", "100ms")
		c.scalar = true
	case n < 11:
		c.q += " | max(span.http.status_code) >= " + g.pick("200", "500")
		c.scalar = true
	case n < 12:
		c.q += " | sum(duration) > " + g.pick("100ms", "400ms")
		c.scalar = true
	case n < 13:
		c.q += " | by(" + g.pick("resource.service.name", "span.team", "resource.k8s.namespace.name", "span.http.method") + ")"
	case n < 14:
		c.q += " | by(" + g.pick("resource.service.name", "span.team") + ") | count() > 1"
		c.scalar = true
	case n < 15:
		c.q += " | select(" + g.pick("span.http.method", "span.http.method, resource.service.name", "name", "span.http.status_code, span.team") + ")"
	case n < 16:
		c.q += " | coalesce()"
	case n < 17:
		c.q += " | " + g.filter(&c)
	case n < 18:
		c.q = "count() > " + g.pick("0", "1", "2")
		c.scalar = true
		c.structural = false
	default:
		by := g.pick("", " by (resource.service.name)", " by (resource.k8s.namespace.name)", " by (span.http.method)", " by (status)")
		fn := g.pick("rate()", "count_over_time()", "avg_over_time(duration)", "max_over_time(duration)", "min_over_time(duration)",
			"sum_over_time(span.http.status_code)", "quantile_over_time(duration, .5, .9)", "histogram_over_time(duration)")
		if strings.HasPrefix(fn, "rate") || strings.HasPrefix(fn, "count_") {
			c.q += " | " + fn + by
		} else {
			c.q += " | " + fn + by
		}
		if g.r.Intn(6) == 0 {
			c.q += " | topk(2)"
			c.noCompare = true
		}
		c.metrics = true
	}
	return c
}

// --- checks -------------------------------------------------------------------------------------------------

// stats counts the outcomes so that the suite can prove it was not vacuous.
type stats struct {
	mu       sync.Mutex
	accepted int            // proxy 2xx and enforced/equivalent
	compared int            // equivalence against the oracle performed
	subset   int            // invariants checked for structural queries
	rejected map[string]int // proxy rejections by reason
	tempoRej int            // Tempo itself rejects the query (direct)
	failures int
}

func (s *stats) add(f func(*stats)) { s.mu.Lock(); f(s); s.mu.Unlock() }

// proxyError tells whether a non-2xx response was written by the proxy: its error body has a traceId
// member (empty without a tracer provider), which Tempo's plain-text errors never have.
func proxyError(r *framework.Response) (msg string, ok bool) {
	var e struct {
		Error   string  `json:"error"`
		TraceID *string `json:"traceId"`
	}
	if json.Unmarshal(r.Body, &e) != nil {
		return "", false
	}
	return e.Error, e.TraceID != nil
}

// positiveStructural reports whether results are monotone in the set of spans: structural operators
// other than the negated forms and no scalar stage.
func (c tq) monotone() bool { return !c.negated && !c.scalar }

func (f *fixture) call(a api, c tq, ep string) *framework.Response {
	if ep == "metrics" {
		return a.metricsRange(c.q)
	}
	return a.search(c.q)
}

// checkQuery sends one query directly, through the proxy and (for restricted users) to the oracle and
// verifies the differential and equivalence properties. It reports problems with t.Errorf.
func (f *fixture) checkQuery(t *testing.T, sc scenario, c tq, ep string, st *stats) {
	rd := f.call(f.direct(sc), c, ep)
	rp := f.call(f.as(sc), c, ep)
	label := fmt.Sprintf("[%s %s] %s", sc.name, ep, c.q)
	fail := func(format string, args ...any) {
		t.Errorf("%s: %s", label, fmt.Sprintf(format, args...))
		st.add(func(s *stats) { s.failures++ })
	}
	dOK := rd.Status/100 == 2
	if !dOK {
		st.add(func(s *stats) { s.tempoRej++ })
	}
	if rp.Status/100 == 5 {
		fail("proxy answered %d: %.300s", rp.Status, rp.Body)
		return
	}
	if rp.Status/100 != 2 {
		msg, fromProxy := proxyError(rp)
		if dOK && !fromProxy {
			fail("Tempo accepts the query but rejected the proxy's rewrite (%d): %.300s", rp.Status, rp.Body)
			return
		}
		if fromProxy {
			st.add(func(s *stats) { s.rejected[reasonOf(msg)]++ })
			// A rejection must be justified: denied attribute, trace-level intrinsic, or a documented limit.
			ok := (c.customer && len(sc.deny) > 0) || (c.traceLevel && !sc.unrestricted) || !dOK ||
				strings.Contains(msg, "parse") || strings.Contains(msg, "syntax") || strings.Contains(msg, "unsupported") ||
				strings.Contains(msg, "not supported") || strings.Contains(msg, "unexpected")
			if !ok {
				fail("rejected without justification (%d): %s", rp.Status, msg)
			}
		}
		return
	}

	// Proxy answered 2xx.
	if c.customer && len(sc.deny) > 0 {
		fail("query on denied attribute was not rejected: %.200s", rp.Body)
		return
	}
	if c.traceLevel && !sc.unrestricted {
		fail("query with trace-level intrinsics was not rejected for a user whose root is not covered: %.200s", rp.Body)
		return
	}
	if !dOK {
		// Tempo rejects the original but the proxy produced something it accepts (e.g. the metrics
		// endpoint with a non-metrics query was normalised). Only isolation can be checked.
		st.add(func(s *stats) { s.accepted++ })
	}
	if !sc.unrestricted {
		f.assertNoLeak(t, sc, label, rp.Body)
	}

	if ep == "search" {
		got := decodeSearch(t, rp)
		for _, tr := range got.Traces {
			for _, id := range spanIDsOf(tr.sets()) {
				sp, ok := f.byID[id]
				if !ok || !sc.grants(sp) {
					fail("returned span %s (ns %s, svc %s, tenant %s) is outside the policy", id, sp.ns, sp.service, sp.tenant)
				}
			}
		}
		if !sc.unrestricted {
			for _, tr := range got.Traces {
				if tr.RootServiceName != nil || tr.RootTraceName != nil || tr.ServiceStats != nil {
					fail("trace %s carries root metadata", tr.TraceID)
				}
			}
		}
		switch {
		case !dOK, c.noCompare:
		case sc.unrestricted:
			if d := diffSig(searchSig(decodeSearch(t, rd)), searchSig(got)); d != "" {
				fail("unrestricted result differs from Tempo's:\n  %s", d)
			}
			st.add(func(s *stats) { s.compared++ })
		case c.structural:
			if !c.monotone() {
				break
			}
			direct := searchSig(decodeSearch(t, rd))
			for tid, sets := range searchSig(got) {
				dsets, ok := direct[tid]
				if !ok {
					fail("trace %s not in Tempo's unrestricted result", tid)
					continue
				}
				have := map[string]bool{}
				for _, id := range idsInSets(dsets) {
					have[id] = true
				}
				for _, id := range idsInSets(sets) {
					if !have[id] {
						fail("span %s of trace %s not in Tempo's unrestricted result", id, tid)
					}
				}
			}
			st.add(func(s *stats) { s.subset++ })
		case c.noCompare:
		default:
			ro := f.call(f.oracle(sc), c, ep)
			if ro.Status/100 != 2 {
				fail("oracle rejects the query (%d): %.200s", ro.Status, ro.Body)
				break
			}
			if d := diffSig(searchSig(decodeSearch(t, ro)), searchSig(got)); d != "" {
				fail("proxy result differs from the oracle:\n  %s", d)
			}
			st.add(func(s *stats) { s.compared++ })
		}
	} else if dOK {
		switch {
		case c.noCompare:
		case sc.unrestricted:
			if d := diffMetrics(metricsSig(t, rd), metricsSig(t, rp)); d != "" {
				fail("unrestricted metrics differ from Tempo's:\n  %s", d)
			}
			st.add(func(s *stats) { s.compared++ })
		case c.structural:
		default:
			ro := f.call(f.oracle(sc), c, ep)
			if ro.Status/100 != 2 {
				fail("oracle rejects the query (%d): %.200s", ro.Status, ro.Body)
				break
			}
			if d := diffMetrics(metricsSig(t, ro), metricsSig(t, rp)); d != "" {
				fail("proxy metrics differ from the oracle:\n  %s", d)
			}
			st.add(func(s *stats) { s.compared++ })
		}
	}
	st.add(func(s *stats) { s.accepted++ })
}

func idsInSets(sets []string) []string {
	var out []string
	for _, s := range sets {
		ids, _, _ := strings.Cut(s, "#")
		if ids != "" {
			out = append(out, strings.Split(ids, ",")...)
		}
	}
	return out
}

func reasonOf(msg string) string {
	switch {
	case strings.Contains(msg, "denied"):
		return "denied attribute"
	case strings.Contains(msg, "root") || strings.Contains(msg, "trace-level"):
		return "trace-level intrinsic"
	case strings.Contains(msg, "parse") || strings.Contains(msg, "syntax") || strings.Contains(msg, "unexpected"):
		return "parser"
	default:
		return msg
	}
}

func diffMetrics(want, got map[string][]string) string {
	return diffSig(want, got)
}

// testCorpus runs the fixed corpus against every scenario (generated=false: equivalence) or a large generated
// set against a subset of the scenarios (generated=true: differential).
func testCorpus(t *testing.T, f *fixture, generated bool) {
	var jobs []struct {
		sc scenario
		c  tq
		ep string
	}
	add := func(sc scenario, c tq) {
		ep := "search"
		if c.metrics {
			ep = "metrics"
		}
		jobs = append(jobs, struct {
			sc scenario
			c  tq
			ep string
		}{sc, c, ep})
	}
	if generated {
		seed := int64(20260101)
		if v := os.Getenv("QUERDIAN_E2E_TEMPO_SEED"); v != "" {
			seed, _ = strconv.ParseInt(v, 10, 64)
		}
		n := 160
		if v := os.Getenv("QUERDIAN_E2E_TEMPO_QUERIES"); v != "" {
			n, _ = strconv.Atoi(v)
		}
		g := &gen{r: rand.New(rand.NewSource(seed)), traceBias: true} //nolint:gosec // deterministic test data
		qs := make([]tq, n)
		for i := range qs {
			qs[i] = g.query()
		}
		for _, name := range []string{"ns-a", "ns-ab", "ns-ac-notbeta", "team-alpha", "ns-a-deny", "t2-ns-a"} {
			for _, c := range qs {
				add(scenarioByName(name), c)
			}
		}
		// Cross-endpoint: metrics queries on search and plain queries on the metrics endpoint. Tempo accepts
		// the former (and ignores the function) and rejects the latter; the proxy must never pass either
		// through unenforced.
		for i, c := range qs {
			sc := scenarioByName("ns-a")
			if i%4 == 0 {
				jobs = append(jobs, struct {
					sc scenario
					c  tq
					ep string
				}{sc, c, map[string]string{"search": "metrics", "metrics": "search"}[epOf(c)]})
			}
		}
		t.Logf("seed %d: %d generated queries, %d checks", seed, n, len(jobs))
	} else {
		for _, sc := range scenarios {
			for _, c := range fixedCorpus() {
				add(sc, c)
			}
		}
		t.Logf("%d fixed queries, %d checks", len(fixedCorpus()), len(jobs))
	}

	st := &stats{rejected: map[string]int{}}
	ch := make(chan int)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				j := jobs[i]
				f.checkQuery(t, j.sc, j.c, j.ep, st)
			}
		}()
	}
	for i := range jobs {
		ch <- i
	}
	close(ch)
	wg.Wait()

	reasons := make([]string, 0, len(st.rejected))
	for k, v := range st.rejected {
		reasons = append(reasons, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(reasons)
	t.Logf("checks=%d accepted=%d compared-to-oracle=%d subset-invariant=%d tempo-rejected=%d proxy-rejected: %s failures=%d",
		len(jobs), st.accepted, st.compared, st.subset, st.tempoRej, strings.Join(reasons, " "), st.failures)
	if st.compared < len(jobs)/5 {
		t.Errorf("only %d of %d checks compared against the oracle: the suite is vacuous", st.compared, len(jobs))
	}
	if len(st.rejected) == 0 && generated {
		t.Errorf("no query was rejected by the proxy: the generator does not exercise the rejection path")
	}
}

func epOf(c tq) string {
	if c.metrics {
		return "metrics"
	}
	return "search"
}
