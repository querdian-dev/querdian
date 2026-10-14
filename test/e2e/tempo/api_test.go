//go:build e2e

package tempo

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	tracev1 "github.com/grafana/tempo/pkg/tempopb/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/querdian-dev/querdian/test/e2e/framework"
)

// api is a Tempo HTTP API client; the same code talks to the proxy (prefix "/tempo") and to Tempo
// directly (prefix "", with the X-Scope-OrgID header set on the client).
type api struct {
	c      *framework.Client
	prefix string
	from   time.Time // search/metrics window
	to     time.Time
}

func (a api) window() url.Values {
	return url.Values{"start": {strconv.FormatInt(a.from.Unix(), 10)}, "end": {strconv.FormatInt(a.to.Unix(), 10)}}
}

func (a api) search(q string) *framework.Response {
	v := a.window()
	v.Set("limit", "1000")
	v.Set("spss", "100")
	if q != "" {
		v.Set("q", q)
	}
	return a.c.Get(a.prefix+"/api/search", v)
}

func (a api) searchRaw(v url.Values) *framework.Response { return a.c.Get(a.prefix+"/api/search", v) }

func (a api) metricsRange(q string) *framework.Response {
	v := a.window()
	v.Set("q", q)
	v.Set("step", "60")
	return a.c.Get(a.prefix+"/api/metrics/query_range", v)
}

func (a api) metricsInstant(q string) *framework.Response {
	v := a.window()
	v.Set("q", q)
	return a.c.Get(a.prefix+"/api/metrics/query", v)
}

func (a api) tags(q string) *framework.Response {
	v := a.window()
	if q != "" {
		v.Set("q", q)
	}
	return a.c.Get(a.prefix+"/api/v2/search/tags", v)
}

func (a api) tagValues(name, q string) *framework.Response {
	v := a.window()
	if q != "" {
		v.Set("q", q)
	}
	return a.c.Get(a.prefix+"/api/v2/search/tag/"+url.PathEscape(name)+"/values", v)
}

// trace fetches a trace; version 1 or 2, proto selects the protobuf representation.
func (a api) trace(id string, version int, proto bool) *framework.Response {
	path := a.prefix + "/api/traces/" + id
	if version == 2 {
		path = a.prefix + "/api/v2/traces/" + id
	}
	if !proto {
		return a.c.Get(path, nil)
	}
	return a.c.GetWith(path, nil, map[string]string{"Accept": "application/protobuf"})
}

// --- response decoding --------------------------------------------------------------------------

// spanOut is one span of a search result.
type spanOut struct {
	SpanID            string          `json:"spanID"`
	StartTimeUnixNano string          `json:"startTimeUnixNano"`
	DurationNanos     string          `json:"durationNanos"`
	Attributes        json.RawMessage `json:"attributes"`
}

type spansetOut struct {
	Spans      []spanOut       `json:"spans"`
	Matched    int             `json:"matched"`
	Attributes json.RawMessage `json:"attributes"`
}

type traceOut struct {
	TraceID           string                     `json:"traceID"`
	RootServiceName   *string                    `json:"rootServiceName"`
	RootTraceName     *string                    `json:"rootTraceName"`
	StartTimeUnixNano string                     `json:"startTimeUnixNano"`
	DurationMs        *int64                     `json:"durationMs"`
	SpanSet           *spansetOut                `json:"spanSet"`
	SpanSets          []spansetOut               `json:"spanSets"`
	ServiceStats      map[string]json.RawMessage `json:"serviceStats"`
}

type searchOut struct {
	Traces []traceOut `json:"traces"`
}

func decodeSearch(t testing.TB, r *framework.Response) searchOut {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("search status %d: %s", r.Status, r.Body)
	}
	var s searchOut
	r.JSON(t, &s)
	return s
}

// sets returns the span sets of a trace (spanSets, falling back to the singular spanSet).
func (tr traceOut) sets() []spansetOut {
	if len(tr.SpanSets) > 0 {
		return tr.SpanSets
	}
	if tr.SpanSet != nil {
		return []spansetOut{*tr.SpanSet}
	}
	return nil
}

// trimID makes trace IDs comparable: Tempo strips leading zeros in search results.
func trimID(s string) string {
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0"
	}
	return s
}

func spanIDsOf(sets []spansetOut) []string {
	var out []string
	for _, s := range sets {
		for _, sp := range s.Spans {
			out = append(out, sp.SpanID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// searchSig is the comparable content of a search result: per trace the sorted span sets, each rendered as
// its sorted span IDs, matched count and set attributes. Metadata (root fields, trace extent, service
// statistics, span attributes) is deliberately NOT part of it: the proxy redacts the metadata by design and
// Tempo adds the attributes of the injected policy to the spans.
func searchSig(s searchOut) map[string][]string {
	out := map[string][]string{}
	for _, tr := range s.Traces {
		var sets []string
		for _, ss := range tr.sets() {
			ids := make([]string, 0, len(ss.Spans))
			for _, sp := range ss.Spans {
				ids = append(ids, sp.SpanID)
			}
			slices.Sort(ids)
			sets = append(sets, fmt.Sprintf("%s#%d#%s", strings.Join(ids, ","), ss.Matched, setLabels(ss)))
		}
		slices.Sort(sets)
		out[trimID(tr.TraceID)] = sets
	}
	return out
}

// setLabels renders the grouping attributes of a span set (by()) canonically.
func setLabels(ss spansetOut) string {
	if len(ss.Attributes) == 0 {
		return ""
	}
	var attrs []struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if json.Unmarshal(ss.Attributes, &attrs) != nil {
		return string(ss.Attributes)
	}
	parts := make([]string, len(attrs))
	for i, a := range attrs {
		parts[i] = a.Key + "=" + string(a.Value)
	}
	slices.Sort(parts)
	return strings.Join(parts, ";")
}

// diffSig describes the difference of two signatures ("" when equal).
func diffSig(want, got map[string][]string) string {
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
			msgs = append(msgs, fmt.Sprintf("unexpected trace %s: %v", k, g))
		case !gok:
			msgs = append(msgs, fmt.Sprintf("missing trace %s: %v", k, w))
		case !slices.Equal(w, g):
			msgs = append(msgs, fmt.Sprintf("trace %s: got %v want %v", k, g, w))
		}
		if len(msgs) >= 4 {
			break
		}
	}
	return strings.Join(msgs, "\n  ")
}

// --- metrics ------------------------------------------------------------------------------------

type metricsOut struct {
	Series []struct {
		Labels  []labelKV `json:"labels"`
		Samples []struct {
			TimestampMs string   `json:"timestampMs"`
			Value       *float64 `json:"value"`
		} `json:"samples"`
	} `json:"series"`
}

type labelKV struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// metricsSig maps the canonical label set to "timestamp=value" samples. A sample without value (0) is
// dropped. Exemplars are ignored (random sampling). Values are rounded to 9 significant digits.
func metricsSig(t testing.TB, r *framework.Response) map[string][]string {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("metrics status %d: %s", r.Status, r.Body)
	}
	var m metricsOut
	r.JSON(t, &m)
	out := map[string][]string{}
	for _, s := range m.Series {
		ls := make([]string, len(s.Labels))
		for i, l := range s.Labels {
			ls[i] = l.Key + "=" + string(l.Value)
		}
		slices.Sort(ls)
		var pts []string
		for _, p := range s.Samples {
			if p.Value == nil || *p.Value == 0 {
				continue
			}
			pts = append(pts, p.TimestampMs+"="+strconv.FormatFloat(*p.Value, 'g', 9, 64))
		}
		if len(pts) == 0 {
			continue
		}
		out[strings.Join(ls, ",")] = pts
	}
	return out
}

// --- tags -----------------------------------------------------------------------------------------

// tagNames flattens the v2 tag list to "scope/name" entries.
func tagNames(t testing.TB, r *framework.Response) map[string]bool {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("tags status %d: %s", r.Status, r.Body)
	}
	var d struct {
		Scopes []struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		} `json:"scopes"`
	}
	r.JSON(t, &d)
	out := map[string]bool{}
	for _, s := range d.Scopes {
		for _, tg := range s.Tags {
			out[s.Name+"/"+tg] = true
		}
	}
	return out
}

func tagValueSet(t testing.TB, r *framework.Response) map[string]bool {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("tag values status %d: %s", r.Status, r.Body)
	}
	var d struct {
		TagValues []struct {
			Value string `json:"value"`
		} `json:"tagValues"`
	}
	r.JSON(t, &d)
	out := map[string]bool{}
	for _, v := range d.TagValues {
		out[v.Value] = true
	}
	return out
}

// --- trace by ID ------------------------------------------------------------------------------------

// traceSpan is a span of a trace-by-ID response with its attribute names.
type traceSpan struct {
	ID    string
	Attrs map[string]bool // "span/<key>" and "resource/<key>"
}

// decodeTrace extracts the spans of a trace-by-ID body (v1 or v2, JSON or protobuf).
func decodeTrace(t testing.TB, r *framework.Response, version int, protobuf bool) map[string]traceSpan {
	t.Helper()
	out := map[string]traceSpan{}
	if protobuf {
		body := r.Body
		if version == 2 {
			// TraceByIDResponse: field 1 is the Trace.
			var trace []byte
			for len(body) > 0 {
				num, typ, n := protowire.ConsumeTag(body)
				if n < 0 {
					t.Fatalf("bad protobuf: %v", protowire.ParseError(n))
				}
				body = body[n:]
				if num == 1 && typ == protowire.BytesType {
					v, m := protowire.ConsumeBytes(body)
					trace = v
					body = body[m:]
					continue
				}
				m := protowire.ConsumeFieldValue(num, typ, body)
				body = body[m:]
			}
			body = trace
		}
		for len(body) > 0 { // Trace: repeated ResourceSpans = 1
			num, typ, n := protowire.ConsumeTag(body)
			if n < 0 {
				t.Fatalf("bad protobuf: %v", protowire.ParseError(n))
			}
			body = body[n:]
			if num == 1 && typ == protowire.BytesType {
				v, m := protowire.ConsumeBytes(body)
				body = body[m:]
				var rs tracev1.ResourceSpans
				if err := rs.Unmarshal(v); err != nil {
					t.Fatal(err)
				}
				for _, ss := range rs.ScopeSpans {
					for _, sp := range ss.Spans {
						ts := traceSpan{ID: hex.EncodeToString(sp.SpanId), Attrs: map[string]bool{}}
						for _, a := range sp.Attributes {
							ts.Attrs["span/"+a.Key] = true
						}
						for _, a := range rs.Resource.GetAttributes() {
							ts.Attrs["resource/"+a.Key] = true
						}
						out[ts.ID] = ts
					}
				}
				continue
			}
			m := protowire.ConsumeFieldValue(num, typ, body)
			body = body[m:]
		}
		return out
	}
	type jattr struct {
		Key string `json:"key"`
	}
	type jrs struct {
		Resource struct {
			Attributes []jattr `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Spans []struct {
				SpanID     string  `json:"spanId"`
				Attributes []jattr `json:"attributes"`
			} `json:"spans"`
		} `json:"scopeSpans"`
	}
	var rss []jrs
	if version == 2 {
		var d struct {
			Trace struct {
				ResourceSpans []jrs `json:"resourceSpans"`
			} `json:"trace"`
		}
		r.JSON(t, &d)
		rss = d.Trace.ResourceSpans
	} else {
		var d struct {
			Batches []jrs `json:"batches"`
		}
		r.JSON(t, &d)
		rss = d.Batches
	}
	for _, rs := range rss {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				raw, err := base64.StdEncoding.DecodeString(sp.SpanID)
				if err != nil {
					t.Fatalf("span id %q: %v", sp.SpanID, err)
				}
				ts := traceSpan{ID: hex.EncodeToString(raw), Attrs: map[string]bool{}}
				for _, a := range sp.Attributes {
					ts.Attrs["span/"+a.Key] = true
				}
				for _, a := range rs.Resource.Attributes {
					ts.Attrs["resource/"+a.Key] = true
				}
				out[ts.ID] = ts
			}
		}
	}
	return out
}
