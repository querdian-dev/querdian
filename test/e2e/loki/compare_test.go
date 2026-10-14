//go:build e2e

package loki

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/querdian-dev/querdian/test/e2e/framework"
)

// api is a Loki HTTP API client; the same code talks to the proxy (prefix "/loki", tenant from the
// policy) and to Loki directly (prefix "", explicit X-Scope-OrgID).
type api struct {
	c      *framework.Client
	prefix string
	tenant string // X-Scope-OrgID sent to the target; empty for the proxy
	end    time.Time
	span   time.Duration
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func (a api) window(q url.Values) url.Values {
	span := a.span
	if span == 0 {
		span = 30 * time.Minute
	}
	if !q.Has("start") {
		q.Set("start", nanos(a.end.Add(-span)))
	}
	if !q.Has("end") {
		q.Set("end", nanos(a.end.Add(30*time.Second)))
	}
	return q
}

func (a api) do(method, path string, q url.Values) *framework.Response {
	var req *http.Request
	var err error
	switch method {
	case http.MethodPost:
		req, err = http.NewRequest(method, a.c.Base+a.prefix+path, strings.NewReader(q.Encode())) //nolint:noctx // test-only
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	default:
		u := a.c.Base + a.prefix + path
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		req, err = http.NewRequest(method, u, nil) //nolint:noctx // test-only
	}
	if err != nil {
		panic(err)
	}
	if a.tenant != "" {
		req.Header.Set("X-Scope-OrgID", a.tenant)
	}
	t0 := time.Now()
	r := a.c.Do(req)
	if d := time.Since(t0); d > 1500*time.Millisecond {
		slowMu.Lock()
		slow = append(slow, fmt.Sprintf("%v %d %s", d.Round(time.Millisecond), r.Status, req.URL.String()))
		slowMu.Unlock()
	}
	return r
}

var (
	slowMu sync.Mutex
	slow   []string
)

func (a api) get(path string, q url.Values) *framework.Response {
	return a.do(http.MethodGet, "/loki/api/v1/"+path, q)
}

// queryRange runs a range query; extra overrides/adds parameters (limit, direction, step, ...).
func (a api) queryRange(q string, extra url.Values) *framework.Response {
	v := url.Values{"query": {q}, "limit": {"5000"}, "step": {"60"}}
	for k, e := range extra {
		v[k] = e
	}
	return a.get("query_range", a.window(v))
}

func (a api) instant(q string) *framework.Response {
	return a.get("query", url.Values{"query": {q}, "time": {nanos(a.end)}})
}

// sel adds the optional selector query parameter when non-empty.
func sel(q string, extra url.Values) url.Values {
	v := url.Values{}
	if q != "" {
		v.Set("query", q)
	}
	for k, e := range extra {
		v[k] = e
	}
	return v
}

func (a api) labels(q string) *framework.Response { return a.get("labels", a.window(sel(q, nil))) }
func (a api) labelValues(name, q string) *framework.Response {
	return a.get("label/"+name+"/values", a.window(sel(q, nil)))
}
func (a api) series(matches ...string) *framework.Response {
	v := url.Values{}
	if len(matches) > 0 {
		v["match[]"] = matches
	}
	return a.get("series", a.window(v))
}
func (a api) detectedLabels(q string) *framework.Response {
	return a.get("detected_labels", a.window(sel(q, nil)))
}
func (a api) detectedFields(q string) *framework.Response {
	return a.get("detected_fields", a.window(sel(q, nil)))
}
func (a api) detectedValues(name, q string) *framework.Response {
	return a.get("detected_field/"+name+"/values", a.window(sel(q, nil)))
}
func (a api) stats(q string) *framework.Response { return a.get("index/stats", a.window(sel(q, nil))) }
func (a api) volume(q string, extra url.Values) *framework.Response {
	return a.get("index/volume", a.window(sel(q, extra)))
}
func (a api) volumeRange(q string, extra url.Values) *framework.Response {
	if extra == nil {
		extra = url.Values{}
	}
	if !extra.Has("step") {
		extra.Set("step", "300")
	}
	return a.get("index/volume_range", a.window(sel(q, extra)))
}
func (a api) patterns(q string) *framework.Response {
	return a.get("patterns", a.window(sel(q, url.Values{"step": {"60"}})))
}

// --- envelopes ------------------------------------------------------------------------------------------

type envelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	TraceID   *string         `json:"traceId"`
}

// fromProxy reports whether an error body was written by the proxy: always JSON with a traceId member
// (empty here: the test proxy runs without a tracer provider). Loki's errors are plain text.
func fromProxy(r *framework.Response) bool {
	var e envelope
	return json.Unmarshal(r.Body, &e) == nil && e.TraceID != nil
}

func decodeEnv(t testing.TB, r *framework.Response) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(r.Body, &e); err != nil {
		t.Fatalf("status %d, body is not JSON: %.300q", r.Status, r.Body)
	}
	return e
}

// --- result normalisation ---------------------------------------------------------------------------------

func labelKey(m map[string]string) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		fmt.Fprintf(&b, "%s=%q,", k, m[k])
	}
	return b.String()
}

// renameTenant rewrites __tenant_id__ values (reference tenants back to the real ones).
func renameTenant(m map[string]string, ren map[string]string) map[string]string {
	if v, ok := m["__tenant_id__"]; ok && ren != nil {
		if r, ok := ren[v]; ok {
			c := make(map[string]string, len(m))
			for k, x := range m {
				c[k] = x
			}
			c["__tenant_id__"] = r
			return c
		}
	}
	return m
}

// result is a normalised query result: label set -> entries ("ts line" for streams, "ts=value" for samples).
type result map[string][]string

// normalise turns a successful query/query_range response into a result. For stream results the entries
// of one stream keep their order except that entries with equal timestamps are sorted by line (their order
// is unspecified); order is checked separately by checkOrder.
func normalise(t testing.TB, r *framework.Response, ren map[string]string) result {
	t.Helper()
	e := decodeEnv(t, r)
	if r.Status != 200 || e.Status != "success" {
		t.Fatalf("not a success: %d %.500s", r.Status, r.Body)
	}
	var d struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatal(err)
	}
	out := result{}
	add := func(k string, v []string) {
		if _, dup := out[k]; dup {
			t.Fatalf("duplicate label set %s in result", k)
		}
		out[k] = v
	}
	switch d.ResultType {
	case "scalar", "string":
		var p []any
		if err := json.Unmarshal(d.Result, &p); err != nil {
			t.Fatal(err)
		}
		out[""] = []string{fmt.Sprintf("%v=%v", p[0], p[1])}
	case "vector":
		var items []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		}
		if err := json.Unmarshal(d.Result, &items); err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			add(labelKey(renameTenant(it.Metric, ren)), []string{fmt.Sprintf("%v=%v", it.Value[0], it.Value[1])})
		}
	case "matrix":
		var items []struct {
			Metric map[string]string `json:"metric"`
			Values [][]any           `json:"values"`
		}
		if err := json.Unmarshal(d.Result, &items); err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			var pts []string
			for _, v := range it.Values {
				pts = append(pts, fmt.Sprintf("%v=%v", v[0], v[1]))
			}
			add(labelKey(renameTenant(it.Metric, ren)), pts)
		}
	case "streams":
		var items []struct {
			Stream map[string]string `json:"stream"`
			Values [][]any           `json:"values"`
		}
		if err := json.Unmarshal(d.Result, &items); err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			var ents []string
			for _, v := range it.Values {
				ents = append(ents, fmt.Sprintf("%v %v", v[0], v[1]))
			}
			add(labelKey(renameTenant(it.Stream, ren)), canonTies(ents))
		}
	default:
		t.Fatalf("unexpected result type %q", d.ResultType)
	}
	return out
}

// canonTies sorts runs of entries with equal timestamps by line (stable otherwise).
func canonTies(ents []string) []string {
	out := append([]string(nil), ents...)
	for i := 0; i < len(out); {
		j := i + 1
		for j < len(out) && tsOf(out[j]) == tsOf(out[i]) {
			j++
		}
		sort.Strings(out[i:j])
		i = j
	}
	return out
}

func tsOf(e string) string { ts, _, _ := strings.Cut(e, " "); return ts }

// flat is one log entry with its stream.
type flat struct {
	stream string
	ts     int64
	line   string
}

// entries flattens a streams result (for limit and ordering checks).
func entries(t testing.TB, r *framework.Response, ren map[string]string) []flat {
	t.Helper()
	res := normalise(t, r, ren)
	var out []flat
	for k, v := range res {
		for _, e := range v {
			ts, line, _ := strings.Cut(e, " ")
			n, err := strconv.ParseInt(ts, 10, 64)
			if err != nil {
				t.Fatalf("bad ts in %q", e)
			}
			out = append(out, flat{k, n, line})
		}
	}
	return out
}

// checkOrder verifies that the entries of every stream are sorted in the requested direction.
func checkOrder(t testing.TB, r *framework.Response, forward bool) {
	t.Helper()
	e := decodeEnv(t, r)
	var d struct {
		Result []struct {
			Stream map[string]string `json:"stream"`
			Values [][]string        `json:"values"`
		} `json:"result"`
	}
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatal(err)
	}
	for _, s := range d.Result {
		for i := 1; i < len(s.Values); i++ {
			a, _ := strconv.ParseInt(s.Values[i-1][0], 10, 64)
			b, _ := strconv.ParseInt(s.Values[i][0], 10, 64)
			if (forward && a > b) || (!forward && a < b) {
				t.Errorf("stream %v not ordered (forward=%v): %d before %d", s.Stream, forward, a, b)
				return
			}
		}
	}
}

// closePoint compares "ts=value" points: timestamps exactly, values with a relative tolerance of 1e-9.
func closePoint(a, b string) bool {
	if a == b {
		return true
	}
	as, bs := strings.SplitN(a, "=", 2), strings.SplitN(b, "=", 2)
	if len(as) != 2 || len(bs) != 2 || as[0] != bs[0] {
		return false
	}
	x, err1 := strconv.ParseFloat(as[1], 64)
	y, err2 := strconv.ParseFloat(bs[1], 64)
	if err1 != nil || err2 != nil {
		return false
	}
	if math.IsNaN(x) && math.IsNaN(y) {
		return true
	}
	return math.Abs(x-y) <= 1e-9*math.Max(1, math.Max(math.Abs(x), math.Abs(y)))
}

// diff returns "" when got equals want, otherwise a description of the first differences.
func diff(want, got result) string {
	var msgs []string
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		w, wok := want[k]
		g, gok := got[k]
		switch {
		case !wok:
			msgs = append(msgs, fmt.Sprintf("unexpected series {%s}: %.200v", k, head(g)))
		case !gok:
			msgs = append(msgs, fmt.Sprintf("missing series {%s}: %.200v", k, head(w)))
		case len(w) != len(g):
			msgs = append(msgs, fmt.Sprintf("series {%s}: %d points, want %d", k, len(g), len(w)))
		default:
			for i := range w {
				if !closePoint(w[i], g[i]) {
					msgs = append(msgs, fmt.Sprintf("series {%s} point %d: got %.200s want %.200s", k, i, g[i], w[i]))
					break
				}
			}
		}
		if len(msgs) >= 4 {
			break
		}
	}
	return strings.Join(msgs, "\n  ")
}

func head(v []string) []string {
	if len(v) > 3 {
		return v[:3]
	}
	return v
}

// strings of a labels/label values/detected_fields-like response ("data": [...]), sorted.
func stringData(t testing.TB, r *framework.Response) []string {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("status %d: %.300s", r.Status, r.Body)
	}
	var d struct {
		Data []string `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &d); err != nil {
		t.Fatalf("decode %.300q: %v", r.Body, err)
	}
	sort.Strings(d.Data)
	return d.Data
}

// seriesData decodes the label sets of a series response as canonical keys (reference tenants renamed).
func seriesData(t testing.TB, r *framework.Response, ren map[string]string) []string {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("status %d: %.300s", r.Status, r.Body)
	}
	var d struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(r.Body, &d); err != nil {
		t.Fatalf("decode %.300q: %v", r.Body, err)
	}
	var out []string
	for _, m := range d.Data {
		out = append(out, labelKey(renameTenant(m, ren)))
	}
	sort.Strings(out)
	return out
}

// generic canonical JSON of a response body for endpoints with small, order-insensitive shapes: the
// named array members are sorted by their JSON text; stats members are dropped.
func canonJSON(t testing.TB, r *framework.Response) string {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("status %d: %.300s", r.Status, r.Body)
	}
	var v any
	if err := json.Unmarshal(r.Body, &v); err != nil {
		t.Fatalf("decode %.300q: %v", r.Body, err)
	}
	return canonValue(v)
}

func canonValue(v any) string {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			if k == "stats" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, strconv.Quote(k)+":"+canonValue(x[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = canonValue(e)
		}
		sort.Strings(parts)
		return "[" + strings.Join(parts, ",") + "]"
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// valuesOnly replaces the label sets by the rank of the series' value list (sorted), which compares results
// whose labels legitimately differ.
func valuesOnly(in result) result {
	lists := make([]string, 0, len(in))
	for _, v := range in {
		lists = append(lists, strings.Join(v, ";"))
	}
	sort.Strings(lists)
	out := result{}
	for i, l := range lists {
		out[strconv.Itoa(i)] = strings.Split(l, ";")
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

// distinct keeps one series per distinct value list (keyed by the list).
func distinct(in result) result {
	out := result{}
	for _, v := range in {
		out[strings.Join(v, ";")] = v
	}
	return out
}
