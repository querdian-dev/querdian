//go:build e2e

package loki

import (
	"fmt"
	"math/rand"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/querdian-dev/querdian/test/e2e/framework"
)

// gen generates LogQL over the seeded data. It deliberately produces some syntactically invalid queries
// (mutations) and adversarial selectors (other namespaces/tenants, label rewriting) next to valid ones.
type gen struct {
	r *rand.Rand
	// noTenant: do not write __tenant_id__ matchers. The oracle's tenants are named after the term set, so a
	// user-written tenant matcher would not mean the same there; user tenant matchers are tested separately.
	noTenant bool
}

func (g *gen) pick(ss ...string) string { return ss[g.r.Intn(len(ss))] }
func (g *gen) chance(p float64) bool    { return g.r.Float64() < p }

var (
	nsValues = []string{"ns-a", "ns-b", "ns-c", "ns-d", "ns-a", "ns-b", "ns-c", "ns-.*", "ns-(a|b)", ".+", "ns-[bc]", "nonexistent"}
	// "status" is left out: grouping by the label an unwrap consumes gives shape-dependent results in Loki itself
	// (see the report: sum by (status) (sum_over_time(... | unwrap status [r]))).
	lblPool   = []string{"k8s_namespace_name", "app", "pod", "env", "service_name", "level", "__tenant_id__", "detected_level"}
	rangePool = []string{"1m", "2m", "5m", "10m", "30s"}
)

func (g *gen) matcher() string {
	switch g.r.Intn(6) {
	case 0, 1:
		return fmt.Sprintf(`k8s_namespace_name%s"%s"`, g.pick("=", "=~", "!=", "!~", "=", "=~", "=~", "="), g.pick(nsValues...))
	case 2:
		return fmt.Sprintf(`app%s"%s"`, g.pick("=", "=~", "!=", "!~"), g.pick("web", "api", "w.*", ".+", "x"))
	case 3:
		return fmt.Sprintf(`pod%s"%s"`, g.pick("=~", "!~", "=", "!="), g.pick("web-.*", "api-ns-a-0", "(web|api)-ns-b-0", ".+"))
	case 4:
		return fmt.Sprintf(`env%s"%s"`, g.pick("=", "!=", "=~"), g.pick("prod", "dev", "p.*"))
	default:
		if g.noTenant {
			return fmt.Sprintf(`service_name%s"%s"`, g.pick("=", "=~", "!="), g.pick("web", "api", ".+"))
		}
		return fmt.Sprintf(`__tenant_id__%s"%s"`, g.pick("=", "!=", "=~", "!~"), g.pick("t1", "t2", "t.*"))
	}
}

func (g *gen) selector() string {
	n := 1
	if g.chance(0.4) {
		n = 2
	}
	if g.chance(0.12) {
		n = 3
	}
	ms := make([]string, 0, n+1)
	for range n {
		ms = append(ms, g.matcher())
	}
	if len(ms) == 1 || g.chance(0.85) { // usually keep a positive anchor so that Loki accepts the selector
		ms = append(ms, g.pick(`app=~".+"`, `k8s_namespace_name=~".+"`, `service_name=~".+"`, `pod=~".+"`))
	}
	g.r.Shuffle(len(ms), func(i, j int) { ms[i], ms[j] = ms[j], ms[i] })
	return "{" + strings.Join(ms, g.pick(", ", ",", " , ")) + "}"
}

func (g *gen) stage(parsed *bool) string {
	switch g.r.Intn(16) {
	case 0:
		return g.pick(`|= "request"`, `|= "heartbeat"`, `|= "dup-ts"`, `|= "tenant=t2"`, `|= "ns-b"`, "|= `error`")
	case 1:
		return g.pick(`!= "debug"`, `!= "heartbeat"`, `!~ "info|warn"`)
	case 2:
		return g.pick(`|~ "level=(warn|error)"`, `|~ "(?i)ERROR"`, `|~ "ns-[ab]"`, `|= "request" or "heartbeat"`)
	case 3:
		*parsed = true
		return "| logfmt"
	case 4:
		*parsed = true
		return "| json"
	case 5:
		return g.pick(`| level="error"`, `| level=~"warn|error"`, `| status >= 500`, `| status == 200`, `| duration > 50ms`, `| latency < 0.5`, `| level!="info" and status=500`, `| ns="ns-b"`, `| tenant="t2"`)
	case 6:
		return g.pick(`| line_format "{{.level}} {{.msg}}"`, `| line_format "{{.app}}"`, `| line_format "{{ .k8s_namespace_name }}:{{ .app }}"`)
	case 7:
		return g.pick(`| label_format lvl=level`, `| label_format app=env`, `| label_format k8s_namespace_name="ns-b"`, `| label_format a=app, b=env`)
	case 8:
		return g.pick(`| drop level`, `| drop env, pod`, `| drop k8s_namespace_name`, `| keep app`, `| keep k8s_namespace_name, app`)
	case 9:
		return g.pick(`| regexp "status=(?P<st>\\d+)"`, `| pattern "<_> msg=<m> <_>"`, `| unpack`, `| decolorize`)
	case 10:
		return g.pick(`| __error__=""`, `| __error__!=""`, `| detected_level="error"`)
	case 11:
		return `| line_format "{{.tenant}}"`
	default:
		return g.pick(`|= "request"`, `!= "info"`, `|~ "call"`)
	}
}

func (g *gen) pipeline(most int) string {
	var parsed bool
	var parts []string
	for range g.r.Intn(most + 1) {
		parts = append(parts, g.stage(&parsed))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}

func (g *gen) logQuery() string { return g.selector() + g.pipeline(4) }

func (g *gen) rangeAgg() string {
	sel := g.selector()
	rng := g.pick(rangePool...)
	off := ""
	if g.chance(0.2) {
		off = " offset " + g.pick("30s", "1m", "5m")
	}
	if g.chance(0.5) {
		fn := g.pick("count_over_time", "rate", "bytes_over_time", "bytes_rate", "count_over_time")
		return fmt.Sprintf("%s(%s%s [%s]%s)", fn, sel, g.pipeline(3), rng, off)
	}
	un := g.pick(
		" | logfmt | unwrap duration(duration)", " | json | unwrap latency", " | json | unwrap status",
		" | logfmt | unwrap status", " | logfmt | duration > 20ms | unwrap duration(duration)")
	body := sel + un
	by := ""
	if g.chance(0.3) {
		by = " by (" + g.pick("k8s_namespace_name", "app", "env", "pod") + ")"
	}
	switch fn := g.pick("sum_over_time", "avg_over_time", "max_over_time", "min_over_time", "stddev_over_time", "first_over_time", "last_over_time", "quantile_over_time", "rate_counter", "sum_over_time"); fn {
	case "quantile_over_time":
		return fmt.Sprintf("quantile_over_time(%s, %s [%s]%s)%s", g.pick("0.5", "0.9", "0.99"), body, rng, off, by)
	default:
		return fmt.Sprintf("%s(%s [%s]%s)%s", fn, body, rng, off, by)
	}
}

func (g *gen) labelList() string {
	n := 1 + g.r.Intn(2)
	l := make([]string, n)
	for i := range l {
		l[i] = g.pick(lblPool...)
	}
	return strings.Join(l, ", ")
}

func (g *gen) vecAgg(inner string) string {
	fn := g.pick("sum", "avg", "min", "max", "count", "stddev", "sum", "sum")
	switch g.r.Intn(4) {
	case 0:
		return fmt.Sprintf("%s by (%s) (%s)", fn, g.labelList(), inner)
	case 1:
		return fmt.Sprintf("%s without (%s) (%s)", fn, g.labelList(), inner)
	case 2:
		return fmt.Sprintf("%s (%s) by (%s)", fn, inner, g.labelList())
	default:
		return fmt.Sprintf("%s(%s)", fn, inner)
	}
}

func (g *gen) metric(depth int) string {
	if depth <= 0 {
		return g.rangeAgg()
	}
	switch g.r.Intn(9) {
	case 0, 1, 2:
		return g.vecAgg(g.metric(depth - 1))
	case 3:
		op := g.pick("+", "-", "*", "/", ">", "<", ">=", "==", "!=")
		return fmt.Sprintf("%s %s %s", g.metric(depth-1), op, g.pick("2", "0.5", "bool 3", "10"))
	case 4:
		mod := g.pick("", "", " on(k8s_namespace_name)", " ignoring(pod)", " on(app)")
		// Arithmetic between two vectors regularly fails in Loki itself (many-to-many matching, HTTP 500 after
		// seconds of retries); keep to the set operators.
		op := g.pick("and", "or", "unless")
		return fmt.Sprintf("(%s) %s%s (%s)", g.metric(depth-1), op, mod, g.metric(depth-1))
	case 5:
		return fmt.Sprintf(`label_replace(%s, "%s", "%s", "%s", "%s")`, g.metric(depth-1),
			g.pick("dst", "app", "k8s_namespace_name", "ns"), g.pick("$1", "x", "ns-b", ""), g.pick("k8s_namespace_name", "app", "pod"), g.pick("(.*)", "ns-(.*)", ".+", "(a|b)"))
	case 6:
		return fmt.Sprintf("%s or %s", g.metric(depth-1), g.pick("vector(0)", "vector(1)"))
	default:
		return g.rangeAgg()
	}
}

// mutate sometimes turns a query into something the lexer or Loki might treat differently.
func (g *gen) mutate(q string) string {
	m := g.mutate1(q)
	if strings.Contains(q, "_format") {
		return q // a broken template makes Loki retry for many seconds
	}
	return m
}

func (g *gen) mutate1(q string) string {
	switch g.r.Intn(12) {
	case 0, 1:
		for _, kw := range []string{"sum", "count_over_time", "rate", "by", "offset", "logfmt", "json", "unwrap", "label_replace"} {
			q = strings.Replace(q, kw, strings.ToUpper(kw), 1)
		}
		return q
	case 2:
		return q + " # }{ app=\"x\""
	case 3:
		return "( " + q + " )"
	case 4:
		return strings.ReplaceAll(q, ", ", ",\n  ")
	case 5:
		i := g.r.Intn(len(q))
		return q[:i] + q[i+1:] // delete one byte: usually a parse error
	case 6:
		i := g.r.Intn(len(q))
		return q[:i] + g.pick(`}`, `{`, `"`, "`", `)`, ` or `, `|`) + q[i:]
	case 7:
		return strings.Replace(q, `"`, "`", 2)
	case 8:
		return strings.Replace(q, "{", "{ ", 1)
	default:
		return q
	}
}

// --- the differential run -------------------------------------------------------------------------------

type fuzzStats struct {
	total, accepted, empty, proxyRejected, bothRejected, anchored, collapsed, orLimit int
	reasons                                                                           map[string][]string
}

func (s *fuzzStats) reject(reason, q string) {
	s.proxyRejected++
	if s.reasons == nil {
		s.reasons = map[string][]string{}
	}
	if len(s.reasons[reason]) < 4 {
		s.reasons[reason] = append(s.reasons[reason], q)
	}
}

var fuzzSets = []string{"one", "two", "overlap", "three", "mt-same", "mt-diff", "mt-diff-overlap", "mt-not", "unrestricted", "deny-one", "deny-multi"}

// testDifferential sends generated LogQL to the reference (the granted data only, original query) and via
// the proxy. For every query: if the oracle answers 200, the proxy must answer an identical result or reject
// it itself (4xx with a traceId); an upstream 4xx/5xx for a query Loki accepts on the granted data means a
// rewrite Loki rejects. A proxy 200 that differs from the oracle is a leak or an unfaithful rewrite.
func testDifferential(t *testing.T, f *fixture, perSet int) {
	for _, name := range fuzzSets {
		set := setByName(name)
		t.Run(name, func(t *testing.T) {
			g := &gen{r: rand.New(rand.NewSource(seedOf(name))), noTenant: len(set.tenants) > 1} //nolint:gosec // deterministic test data
			user, oracle, ren := f.as(set), f.oracle(set), set.rename()
			direct := f.direct(strings.Join(set.tenants, "|"))
			multi := len(f.eff[set.name].Terms) > 1
			var st fuzzStats
			for range perSet {
				var q string
				metric := g.chance(0.6)
				if metric {
					q = g.metric(g.r.Intn(3))
				} else {
					q = g.logQuery()
				}
				q = g.mutate(q)
				instant := metric && g.chance(0.4)
				dir := g.pick("forward", "backward")
				call := func(a api) *framework.Response {
					if instant {
						return a.instant(q)
					}
					return a.queryRange(q, url.Values{"direction": {dir}})
				}
				st.total++
				t0 := time.Now()
				o := call(oracle)
				t1 := time.Now()
				p := call(user)
				if d := time.Since(t0); d > 2*time.Second {
					t.Logf("SLOW %v (oracle %v) %q", d, t1.Sub(t0), q)
				}
				switch {
				case p.Status == 200 && o.Status == 400 && strings.Contains(string(o.Body), "empty-compatible"):
					// Loki rejects a selector without a positive matcher; the policy's injected matchers supply one.
					st.anchored++
				case p.Status == 200 && o.Status != 200:
					t.Errorf("proxy answered %q (direct: %d) but the reference rejects it (%d %.200s)", q, call(direct).Status, o.Status, o.Body)
				case o.Status != 200 && p.Status != 200:
					st.bothRejected++
				case p.Status == 200:
					st.accepted++
					want, got := normalise(t, o, ren), normalise(t, p, ren)
					if d := diff(want, got); d != "" {
						// Known limitation (documented in the report): a pipeline stage can give different streams
						// the same label set and lines (keep/drop/line_format); the fan-out merge deduplicates equal
						// (timestamp, line) entries of one label set across branches and so returns fewer duplicates.
						// Nothing is missing or foreign: the sets of distinct entries are equal.
						if multi && metric && collapsingStage.MatchString(q) {
							// Known limitation (internal/... OrLift doc): "or" keeps one of several series with equal label
							// sets, and drop/keep make distinct streams equal. Values may be under-reported; nothing foreign
							// may appear: every proxy series must exist in the oracle result.
							for k := range got {
								if _, ok := want[k]; !ok {
									t.Errorf("%q: proxy returned the series {%s} which the granted data does not have", q, k)
								}
							}
							st.orLimit++
							t.Logf("OR-LIFT-COLLAPSE %q", q)
							continue
						}
						if !instant && diff(distinctEntries(want), distinctEntries(got)) == "" && countEntries(got) < countEntries(want) {
							st.collapsed++
							t.Logf("DUPLICATE-COLLAPSE %q: %d entries, the granted data has %d", q, countEntries(got), countEntries(want))
							continue
						}
						t.Errorf("%q (instant=%v): proxy result differs from the query over the granted data\n  %s", q, instant, d)
						continue
					}
					if len(want) == 0 {
						st.empty++
					}
				case fromProxy(p) && p.Status >= 400 && p.Status < 500:
					st.reject(fmt.Sprintf("%d %s", p.Status, shorten(decodeEnv(t, p).Error)), q)
				case fromProxy(p):
					t.Errorf("%q: proxy error %d %.300s", q, p.Status, p.Body)
				default:
					t.Errorf("%q: Loki accepts the original on the granted data but rejected the proxy's rewrite: %d %.300s", q, p.Status, p.Body)
				}
			}
			t.Logf("%d generated queries: %d answered (%d empty), %d rejected by the proxy, %d rejected by both, %d accepted only thanks to the injected anchor matcher, %d with collapsed duplicates", st.total, st.accepted, st.empty, st.proxyRejected, st.bothRejected, st.anchored, st.collapsed)
			if st.accepted < st.total/4 {
				t.Errorf("only %d of %d queries were answered; the generator does not exercise enforcement", st.accepted, st.total)
			}
			var keys []string
			for k := range st.reasons {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				t.Logf("STRICTNESS %s: %q", k, st.reasons[k])
			}
		})
	}
}

// shorten removes the variable parts of a proxy error so that equal reasons group.
func shorten(s string) string {
	s = byClause.ReplaceAllString(s, "<range aggregation> with a by/without clause")
	s = positions.ReplaceAllString(s, "N")
	if len(s) > 110 {
		s = s[:110]
	}
	return s
}

var byClause = regexp.MustCompile(`\w+ with (by|without) \(.*`)

var positions = regexp.MustCompile(`\d+`)

func seedOf(name string) int64 {
	var h int64 = 1469598103
	for _, c := range name {
		h = h*16777619 + int64(c)
	}
	return h
}

func countEntries(r result) int {
	n := 0
	for _, v := range r {
		n += len(v)
	}
	return n
}

// distinctEntries removes repeated entries inside every stream.
func distinctEntries(r result) result {
	out := result{}
	for k, v := range r {
		seen := map[string]bool{}
		for _, e := range v {
			if !seen[e] {
				seen[e] = true
				out[k] = append(out[k], e)
			}
		}
	}
	return out
}

// collapsingStage matches pipeline stages that can give distinct streams equal label sets.
var collapsingStage = regexp.MustCompile(`(?i)\|\s*(drop|keep)\b`)
