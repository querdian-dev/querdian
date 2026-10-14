//go:build e2e

package loki

import (
	"encoding/json"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/querdian-dev/querdian/test/e2e/framework"
)

// tenantBound reports whether the set resolves to terms bound to tenants through __tenant_id__ matchers
// (different terms per tenant). Loki 3.5.0 does not evaluate a positive __tenant_id__ matcher on the
// index-based endpoints (labels, label values, series, index/*): they answer as if no stream matched.
func (f *fixture) tenantBound(set termSet) bool {
	for _, term := range f.eff[set.name].Terms {
		for _, m := range term.Matchers {
			if m.Name == "__tenant_id__" {
				return true
			}
		}
	}
	return false
}

// without returns names minus the denied ones.
func without(names []string, denied []string) []string {
	var out []string
	for _, n := range names {
		if !slices.Contains(denied, n) {
			out = append(out, n)
		}
	}
	return out
}

var selectorsForMetadata = []string{"", `{app="web"}`, `{app=~".+"}`, `{k8s_namespace_name="ns-b"}`, `{app="api"} | json`, `{k8s_namespace_name=~"ns-d|ns-a"} |= "request"`}

// detected decodes {"detectedLabels":[{label,cardinality}]} into label -> cardinality.
func detectedLabels(t testing.TB, r *framework.Response) map[string]int {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("status %d: %.300s", r.Status, r.Body)
	}
	var d struct {
		DetectedLabels []struct {
			Label       string `json:"label"`
			Cardinality int    `json:"cardinality"`
		} `json:"detectedLabels"`
	}
	if err := json.Unmarshal(r.Body, &d); err != nil {
		t.Fatalf("decode %.300q: %v", r.Body, err)
	}
	out := map[string]int{}
	for _, l := range d.DetectedLabels {
		out[l.Label] = l.Cardinality
	}
	return out
}

func detectedFields(t testing.TB, r *framework.Response) map[string]string {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("status %d: %.300s", r.Status, r.Body)
	}
	var d struct {
		Fields []struct {
			Label string `json:"label"`
			Type  string `json:"type"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(r.Body, &d); err != nil {
		t.Fatalf("decode %.300q: %v", r.Body, err)
	}
	out := map[string]string{}
	for _, l := range d.Fields {
		out[l.Label] = l.Type
	}
	return out
}

func detectedValues(t testing.TB, r *framework.Response) []string {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("status %d: %.300s", r.Status, r.Body)
	}
	var d struct {
		Values []string `json:"values"`
	}
	if err := json.Unmarshal(r.Body, &d); err != nil {
		t.Fatalf("decode %.300q: %v", r.Body, err)
	}
	sort.Strings(d.Values)
	return d.Values
}

type statsT struct{ Streams, Chunks, Bytes, Entries int }

func indexStats(t testing.TB, r *framework.Response) statsT {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("status %d: %.300s", r.Status, r.Body)
	}
	var s statsT
	var raw map[string]int
	if err := json.Unmarshal(r.Body, &raw); err != nil {
		t.Fatalf("decode %.300q: %v", r.Body, err)
	}
	s.Streams, s.Chunks, s.Bytes, s.Entries = raw["streams"], raw["chunks"], raw["bytes"], raw["entries"]
	return s
}

// volumes decodes a volume response into label set (denied labels removed, tenant renamed) -> summed value.
// Instant volume has one value per series, volume_range a list of points per series (summed).
func volumes(t testing.TB, r *framework.Response, ren map[string]string, denied []string) map[string]float64 {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("status %d: %.300s", r.Status, r.Body)
	}
	e := decodeEnv(t, r)
	var d struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
			Values [][]any           `json:"values"`
		} `json:"result"`
	}
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatalf("decode %.300q: %v", r.Body, err)
	}
	out := map[string]float64{}
	for _, it := range d.Result {
		m := renameTenant(it.Metric, ren)
		c := map[string]string{}
		for k, v := range m {
			if !slices.Contains(denied, k) {
				c[k] = v
			}
		}
		sum := func(v []any) {
			if len(v) == 2 {
				x, _ := strconv.ParseFloat(v[1].(string), 64)
				out[labelKey(c)] += x
			}
		}
		sum(it.Value)
		for _, v := range it.Values {
			sum(v)
		}
	}
	return out
}

func equalFloats(a, b map[string]float64) string {
	var msgs []string
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			msgs = append(msgs, "{"+k+"}: want "+strconv.FormatFloat(v, 'f', -1, 64)+" got "+strconv.FormatFloat(b[k], 'f', -1, 64))
		}
	}
	for k, v := range b {
		if _, ok := a[k]; !ok {
			msgs = append(msgs, "unexpected {"+k+"}: "+strconv.FormatFloat(v, 'f', -1, 64))
		}
	}
	sort.Strings(msgs)
	if len(msgs) > 3 {
		msgs = msgs[:3]
	}
	return strings.Join(msgs, "; ")
}

func testMetadata(t *testing.T, f *fixture) {
	for _, set := range termSets {
		t.Run(set.name, func(t *testing.T) {
			user, oracle, ren := f.as(set), f.oracle(set), set.rename()
			bound := f.tenantBound(set)
			// expect compares the user's response with the oracle's for the same call; "oracle error" means
			// Loki itself fails for the call (e.g. detected_labels and series under tenant federation), in which
			// case the proxy must not answer with data.
			for _, q := range selectorsForMetadata {
				if bound {
					continue // see tenantBound; covered by testTenantBound
				}
				// labels
				if ro, rp := oracle.labels(q), user.labels(q); pair(t, "labels("+q+")", ro, rp) {
					if want, got := stringData(t, ro), stringData(t, rp); !slices.Equal(without(want, set.denied), got) {
						t.Errorf("labels(%q): got %v want %v", q, got, without(want, set.denied))
					}
				}
				// label values
				for _, name := range []string{"k8s_namespace_name", "app", "pod", "env", "service_name", "detected_level"} {
					r := user.labelValues(name, q)
					if slices.Contains(set.denied, name) {
						if r.Status != 403 || !fromProxy(r) {
							t.Errorf("label values %s: want proxy 403, got %d %.200s", name, r.Status, r.Body)
						}
						continue
					}
					ro := oracle.labelValues(name, q)
					if pair(t, "label values "+name+"("+q+")", ro, r) {
						if want, got := stringData(t, ro), stringData(t, r); !slices.Equal(want, got) {
							t.Errorf("label values %s(%q): got %v want %v", name, q, got, want)
						}
					}
				}
				// detected labels / fields / values
				ro, rp := oracle.detectedLabels(q), user.detectedLabels(q)
				switch {
				case ro.Status != 200:
					if rp.Status == 200 {
						t.Errorf("detected_labels(%q): oracle answers %d (%.100s) but the proxy 200 %.200s", q, ro.Status, ro.Body, rp.Body)
					}
				case rp.Status != 200:
					t.Errorf("detected_labels(%q): status %d %.200s", q, rp.Status, rp.Body)
				default:
					want, got := detectedLabels(t, ro), detectedLabels(t, rp)
					for _, d := range set.denied {
						delete(want, d)
					}
					if !sameKeys(want, got) {
						t.Errorf("detected_labels(%q): got %v want %v", q, got, want)
					}
					for k, w := range want {
						// Cardinalities of a merged response cannot be summed exactly; they never exceed the sum and
						// never fall below the largest part, so only a single branch is exact.
						if len(f.eff[set.name].Terms) <= 1 && !set.unrestricted && got[k] != w {
							t.Errorf("detected_labels(%q) %s cardinality %d, want %d", q, k, got[k], w)
						}
					}
				}
				if q != "" {
					wo, wp := oracle.detectedFields(q), user.detectedFields(q)
					if wo.Status != 200 || wp.Status != 200 {
						if wo.Status != wp.Status && (wo.Status == 200 || wp.Status == 200) {
							t.Errorf("detected_fields(%q): oracle %d, proxy %d %.200s", q, wo.Status, wp.Status, wp.Body)
						}
					} else {
						want, got := detectedFields(t, wo), detectedFields(t, wp)
						for _, d := range set.denied {
							delete(want, d)
						}
						if !sameKeys(want, got) {
							t.Errorf("detected_fields(%q): got %v want %v", q, got, want)
						}
					}
					for _, field := range []string{"level", "status"} {
						vo, vp := oracle.detectedValues(field, q), user.detectedValues(field, q)
						if vo.Status != 200 || vp.Status != 200 {
							if vo.Status == 200 || vp.Status == 200 {
								t.Errorf("detected_field/%s/values(%q): oracle %d, proxy %d %.200s", field, q, vo.Status, vp.Status, vp.Body)
							}
							continue
						}
						if want, got := detectedValues(t, vo), detectedValues(t, vp); !slices.Equal(want, got) {
							t.Errorf("detected_field/%s/values(%q): got %v want %v", field, q, got, want)
						}
					}
				}
			}
			if bound {
				testTenantBoundMetadata(t, f, set)
				return
			}

			if len(set.tenants) == 1 { // not vacuous: the oracle has series (Loki answers none under federation)
				if len(seriesData(t, oracle.series(`{app=~".+"}`), ren)) == 0 {
					t.Error("the oracle has no series: the metadata comparison would be vacuous")
				}
			}
			// series: none, one, several match[]
			for _, ms := range [][]string{nil, {`{app="web"}`}, {`{app="web"}`, `{app="api"}`}, {`{k8s_namespace_name=~"ns-a|ns-d"}`}} {
				ro, rp := oracle.series(ms...), user.series(ms...)
				if ro.Status != 200 || rp.Status != 200 {
					if ro.Status != rp.Status && (ro.Status == 200 || rp.Status == 200) {
						t.Errorf("series(%v): oracle %d, proxy %d %.200s", ms, ro.Status, rp.Status, rp.Body)
					}
					continue
				}
				want := seriesData(t, ro, ren)
				if len(set.denied) > 0 { // denied labels are removed from label sets; equal sets collapse
					want = stripDenied(t, ro, ren, set.denied)
				}
				if got := seriesData(t, rp, ren); !slices.Equal(want, got) {
					t.Errorf("series(%v): got %d sets, want %d\n  got  %.300v\n  want %.300v", ms, len(got), len(want), got, want)
				}
			}

			// index stats / volume / volume_range / patterns
			for _, q := range []string{"", `{app="web"}`, `{k8s_namespace_name=~"ns-a|ns-b"}`} {
				if q == "" {
					// Loki requires a selector here (400); the proxy supplies the policy selector instead, so an
					// omitted query is answered, and only about the granted data.
					for name, r := range map[string]*framework.Response{"index/stats": user.stats(""), "volume": user.volume("", nil), "volume_range": user.volumeRange("", nil)} {
						if r.Status != 200 && !set.unrestricted {
							t.Errorf("%s without query: %d %.200s", name, r.Status, r.Body)
						}
					}
					continue
				}
				ro, rp := oracle.stats(q), user.stats(q)
				if ro.Status != 200 || rp.Status != 200 {
					if ro.Status != rp.Status {
						t.Errorf("index/stats(%q): oracle %d, proxy %d %.200s", q, ro.Status, rp.Status, rp.Body)
					}
				} else {
					want, got := indexStats(t, ro), indexStats(t, rp)
					exact := f.eff[set.name].Disjoint || len(f.eff[set.name].Terms) <= 1
					switch {
					case exact && want != got:
						t.Errorf("index/stats(%q): got %+v want %+v", q, got, want)
					// Loki's stream/entry counts are index estimates (sketches, chunk proration): allow small noise.
					case !exact && got.Entries*10 < want.Entries*9:
						t.Errorf("index/stats(%q): overlapping terms must not under-count: got %+v, granted data %+v", q, got, want)
					case !exact && rp.Header.Get("X-Querdian-Approximate") != "true":
						t.Errorf("index/stats(%q): overlapping terms must set X-Querdian-Approximate", q)
					}
				}
				for _, vc := range []struct {
					name  string
					extra url.Values
					call  func(*api, string, url.Values) *framework.Response
				}{
					{"volume", nil, func(a *api, q string, e url.Values) *framework.Response { return a.volume(q, e) }},
					{"volume agg", url.Values{"aggregateBy": {"labels"}, "targetLabels": {"app"}}, func(a *api, q string, e url.Values) *framework.Response { return a.volume(q, e) }},
					{"volume_range", url.Values{"aggregateBy": {"labels"}, "targetLabels": {"k8s_namespace_name"}}, func(a *api, q string, e url.Values) *framework.Response { return a.volumeRange(q, e) }},
				} {
					ro, rp := vc.call(&oracle, q, cloneValues(vc.extra)), vc.call(&user, q, cloneValues(vc.extra))
					if ro.Status != 200 || rp.Status != 200 {
						if ro.Status != rp.Status {
							t.Errorf("%s(%q): oracle %d, proxy %d %.200s", vc.name, q, ro.Status, rp.Status, rp.Body)
						}
						continue
					}
					want, got := volumes(t, ro, ren, set.denied), volumes(t, rp, ren, set.denied)
					exact := f.eff[set.name].Disjoint || len(f.eff[set.name].Terms) <= 1
					if vc.extra == nil {
						// Without targetLabels Loki names the series after the labels of the selector's matchers, and
						// the injected policy matchers add labels: compare the totals only.
						want, got = map[string]float64{"": total(want)}, map[string]float64{"": total(got)}
					}
					if exact {
						if d := equalFloats(want, got); d != "" {
							t.Errorf("%s(%q): %s", vc.name, q, d)
						}
					} else {
						for k, v := range want {
							if got[k] < v {
								t.Errorf("%s(%q): {%s} = %v, below the granted data %v", vc.name, q, k, got[k], v)
							}
						}
					}
				}
			}
			// patterns: the same set of patterns as the oracle
			for _, q := range []string{`{app="web"}`, `{app=~".+"}`} {
				ro, rp := oracle.patterns(q), user.patterns(q)
				if ro.Status != 200 || rp.Status != 200 {
					if ro.Status != rp.Status {
						t.Errorf("patterns(%q): oracle %d, proxy %d %.200s", q, ro.Status, rp.Status, rp.Body)
					}
					continue
				}
				if want, got := patternSet(t, ro), patternSet(t, rp); !slices.Equal(want, got) {
					t.Errorf("patterns(%q): got %v want %v", q, got, want)
				}
			}
		})
	}
}

func sameKeys[V any](a, b map[string]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func cloneValues(v url.Values) url.Values {
	c := url.Values{}
	for k, x := range v {
		c[k] = slices.Clone(x)
	}
	return c
}

// stripDenied decodes a series response removing denied labels and collapsing equal label sets.
func stripDenied(t testing.TB, r *framework.Response, ren map[string]string, denied []string) []string {
	t.Helper()
	var d struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &d); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range d.Data {
		c := map[string]string{}
		for k, v := range renameTenant(m, ren) {
			if !slices.Contains(denied, k) {
				c[k] = v
			}
		}
		seen[labelKey(c)] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func patternSet(t testing.TB, r *framework.Response) []string {
	t.Helper()
	var d struct {
		Data []struct {
			Pattern string `json:"pattern"`
		} `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &d); err != nil {
		t.Fatalf("decode %.300q: %v", r.Body, err)
	}
	var out []string
	for _, p := range d.Data {
		out = append(out, p.Pattern)
	}
	sort.Strings(out)
	return out
}

// pair reports a discrepancy when only one of the oracle and the proxy answers 200 and returns whether both
// did (then the bodies can be compared). Loki rejects some parameter combinations itself; the proxy must then
// not answer with data (a proxy-originated rejection is accepted).
func pair(t testing.TB, what string, ro, rp *framework.Response) bool {
	t.Helper()
	switch {
	case ro.Status == 200 && rp.Status == 200:
		return true
	case ro.Status == 200:
		t.Errorf("%s: the granted data answers 200, the proxy %d (proxy-originated=%v): %.200s", what, rp.Status, fromProxy(rp), rp.Body)
	case rp.Status == 200:
		t.Errorf("%s: Loki rejects it (%d %.100s) but the proxy answers 200 %.200s", what, ro.Status, ro.Body, rp.Body)
	}
	return false
}

func total(m map[string]float64) float64 {
	var x float64
	for _, v := range m {
		x += v
	}
	return x
}

// testTenantBoundMetadata documents what tenant-bound policies (different terms per tenant) do on the
// index-based endpoints. Loki 3.5.0 does not evaluate a positive __tenant_id__ matcher there, so those
// calls answer as if nothing matched (labels list only __tenant_id__, label values and series are empty,
// stats are zero): wrong but fail-closed. The test pins the safety property (never data outside the grant)
// and logs the observed behaviour.
func testTenantBoundMetadata(t *testing.T, f *fixture, set termSet) {
	user := f.as(set)
	visible := f.visibleStreams(t, set)
	nsOK, appOK := map[string]bool{}, map[string]bool{}
	for _, s := range visible {
		nsOK[s.labels[nsLabel]] = true
		appOK[s.labels["app"]] = true
	}
	for _, v := range lenientStrings(t, user.labelValues(nsLabel, `{app=~".+"}`)) {
		if !nsOK[v] {
			t.Errorf("label values leak namespace %q", v)
		}
	}
	for _, v := range lenientStrings(t, user.labelValues("app", "")) {
		if !appOK[v] {
			t.Errorf("label values leak app %q", v)
		}
	}
	for _, m := range lenientSeries(t, user.series(`{app=~".+"}`)) {
		if !strings.Contains(m, `k8s_namespace_name="`) {
			continue
		}
		ok := false
		for ns := range nsOK {
			ok = ok || strings.Contains(m, `k8s_namespace_name="`+ns+`"`)
		}
		if !ok {
			t.Errorf("series leaks %s", m)
		}
	}
	entries := 0
	for _, s := range visible {
		entries += len(s.entries)
	}
	if r := user.stats(`{app=~".+"}`); r.Status == 200 {
		if got := indexStats(t, r); got.Entries > entries {
			t.Errorf("index/stats counts %d entries, the grant has %d", got.Entries, entries)
		}
	}
	t.Logf("labels=%v label values(ns)=%v series=%d stats=%s", lenientStrings(t, user.labels("")), lenientStrings(t, user.labelValues(nsLabel, "")),
		len(lenientSeries(t, user.series(`{app=~".+"}`))), user.stats(`{app=~".+"}`).Body)
}

// A tenant-bound term may have no positive anchor matcher (e.g. a "!=" term); then the proxy rejects requests
// whose selector is omitted or negative itself (400), which is accepted here.
func lenientStrings(t testing.TB, r *framework.Response) []string {
	if r.Status == 400 && fromProxy(r) {
		return nil
	}
	return stringData(t, r)
}

func lenientSeries(t testing.TB, r *framework.Response) []string {
	if r.Status == 400 && fromProxy(r) {
		return nil
	}
	return seriesData(t, r, nil)
}
