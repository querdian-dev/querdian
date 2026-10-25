package fanout

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures in testdata/tempo were recorded black box against Tempo 2.9.0 (record.sh).
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "tempo", name)) //nolint:gosec // fixed fixtures
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type anyMap = map[string]any

func decode(t *testing.T, b []byte) anyMap {
	t.Helper()
	var m anyMap
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid JSON %s: %v", b, err)
	}
	return m
}

func traces(t *testing.T, b []byte) []anyMap {
	t.Helper()
	var out []anyMap
	for _, x := range decode(t, b)["traces"].([]any) {
		out = append(out, x.(anyMap))
	}
	return out
}

func TestRedactChildMatch(t *testing.T) {
	in := fixture(t, "search_child_match.json")
	out, err := RedactTempoSearch(in, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"frontend", "GET /", "serviceStats", "rootServiceName", "rootTraceName", "backend"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("redacted response still contains %q: %s", leak, out)
		}
	}
	tr := traces(t, out)
	if len(tr) != 1 {
		t.Fatalf("traces = %d", len(tr))
	}
	got := tr[0]
	if got["traceID"] != "1" {
		t.Errorf("traceID = %v", got["traceID"])
	}
	// Recomputed from the single matched span: start 1791981251365724032 (recorded), 300 ms.
	orig := traces(t, in)[0]
	span := orig["spanSet"].(anyMap)["spans"].([]any)[0].(anyMap)
	if got["startTimeUnixNano"] != span["startTimeUnixNano"] {
		t.Errorf("startTimeUnixNano = %v, want matched span start %v", got["startTimeUnixNano"], span["startTimeUnixNano"])
	}
	if got["durationMs"] != float64(300) {
		t.Errorf("durationMs = %v, want 300 (matched span only, trace had 1000)", got["durationMs"])
	}
	// Both spanSet and spanSets survive byte-identical in content.
	if !jsonEqual(got["spanSet"], orig["spanSet"]) || !jsonEqual(got["spanSets"], orig["spanSets"]) {
		t.Errorf("spanSet/spanSets changed: %v", got)
	}
	if decode(t, out)["metrics"] == nil {
		t.Error("metrics dropped")
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestRedactRootlessPlaceholder(t *testing.T) {
	out, err := RedactTempoSearch(fixture(t, "search_rootless.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "root span not yet received") || strings.Contains(string(out), "rootServiceName") {
		t.Errorf("placeholder not removed: %s", out)
	}
	tr := traces(t, out)
	if len(tr) != 1 || tr[0]["durationMs"] != float64(40) {
		t.Errorf("unexpected trace: %v", tr)
	}
}

func TestRedactGroupedMultipleSpanSets(t *testing.T) {
	in := fixture(t, "search_grouped.json")
	out, err := RedactTempoSearch(in, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"serviceStats", "rootServiceName", "rootTraceName", "spanCount", "errorCount"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("leak %q in %s", leak, out)
		}
	}
	byID := map[string]anyMap{}
	for _, tr := range traces(t, out) {
		byID[tr["traceID"].(string)] = tr
	}
	// Trace 3: root3 (0..100 ms) and child3 (+50..+550 ms): matched spans cover the whole trace here.
	if d := byID["3"]["durationMs"]; d != float64(550) {
		t.Errorf("trace 3 durationMs = %v, want 550", d)
	}
	// Trace 1 has two span sets (frontend, backend); both must be intact.
	if n := len(byID["1"]["spanSets"].([]any)); n != 2 {
		t.Errorf("trace 1 spanSets = %d, want 2", n)
	}
	if byID["1"]["spanSet"] == nil {
		t.Error("singular spanSet lost")
	}
}

func TestRedactSelectKeepsSpanAttributes(t *testing.T) {
	in := fixture(t, "search_select.json")
	out, err := RedactTempoSearch(in, false)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(traces(t, in)[0]["spanSets"], traces(t, out)[0]["spanSets"]) {
		t.Error("selected span attributes were altered")
	}
}

func TestRootCoveredPassesThrough(t *testing.T) {
	for _, f := range []string{"search_child_match.json", "search_rootless.json", "search_root_match.json"} {
		in := fixture(t, f)
		out, err := RedactTempoSearch(in, true)
		if err != nil || string(out) != string(in) {
			t.Errorf("%s: rootCovered body changed (err %v)", f, err)
		}
	}
}

func TestRedactAllowList(t *testing.T) {
	in := `{"traces":[{"traceID":"a","rootServiceName":"r","rootTraceName":"n","startTimeUnixNano":"5","durationMs":9,
	  "futureField":"secret","spanSets":[{"spans":[{"spanID":"1","startTimeUnixNano":"1000000","durationNanos":"1500001"}],"matched":1}],
	  "serviceStats":{"x":{"spanCount":1}}}],"metrics":{"completedJobs":1},"unknownTop":1}`
	out, err := RedactTempoSearch([]byte(in), false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "secret") || strings.Contains(string(out), "unknownTop") {
		t.Errorf("unknown fields not dropped: %s", out)
	}
	tr := traces(t, out)[0]
	if tr["startTimeUnixNano"] != "1000000" || tr["durationMs"] != float64(2) { // 1.500001 ms rounds up
		t.Errorf("recomputed = %v / %v", tr["startTimeUnixNano"], tr["durationMs"])
	}
	if _, ok := tr["spanSet"]; ok {
		t.Error("absent spanSet must stay absent")
	}
}

func TestRedactNoSpansDropsTimes(t *testing.T) {
	out, err := RedactTempoSearch([]byte(`{"traces":[{"traceID":"a","startTimeUnixNano":"5","durationMs":9,"spanSets":[{"spans":[],"matched":0}]}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	tr := traces(t, out)[0]
	if _, ok := tr["startTimeUnixNano"]; ok {
		t.Errorf("time fields kept without spans: %v", tr)
	}
	if _, ok := tr["durationMs"]; ok {
		t.Errorf("time fields kept without spans: %v", tr)
	}
}

func TestRedactEmptyAndMalformed(t *testing.T) {
	out, err := RedactTempoSearch([]byte(`{"traces":[],"metrics":{}}`), false)
	if err != nil || string(out) != `{"traces":[],"metrics":{}}` {
		t.Errorf("empty = %s, %v", out, err)
	}
	for _, bad := range []string{``, `nope`, `{"traces":{}}`, `{"traces":[{"traceID":"1","spanSets":[{"spans":[{"startTimeUnixNano":"x"}]}]}]}`,
		`{"traces":[{"traceID":"1","spanSets":"x"}]}`} {
		if _, err := RedactTempoSearch([]byte(bad), false); !errors.Is(err, ErrTempoResponse) {
			t.Errorf("%q: err = %v, want ErrTempoResponse", bad, err)
		}
	}
}

func TestSearchMerger(t *testing.T) {
	m := NewTempoSearchMerger(false)
	rec := httptest.NewRecorder()
	err := m(rec, httptest.NewRequest(http.MethodGet, "/api/search", nil), []Response{{Status: 200, Body: fixture(t, "search_child_match.json")}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || strings.Contains(rec.Body.String(), "serviceStats") {
		t.Errorf("response: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	rec = httptest.NewRecorder()
	if err := m(rec, httptest.NewRequest(http.MethodGet, "/", nil), nil); err == nil || rec.Body.Len() != 0 {
		t.Errorf("zero responses: err %v, body %q", err, rec.Body)
	}
	if err := m(rec, httptest.NewRequest(http.MethodGet, "/", nil), []Response{{Body: []byte("x")}}); err == nil || rec.Body.Len() != 0 {
		t.Errorf("malformed: err %v, wrote %q", err, rec.Body)
	}
}

func tagNames(t *testing.T, b []byte) map[string][]string {
	t.Helper()
	var r struct {
		Scopes []struct {
			Name string
			Tags []string
		}
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, s := range r.Scopes {
		out[s.Name] = s.Tags
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestFilterTags(t *testing.T) {
	in := fixture(t, "tags_v2.json")
	tests := []struct {
		name    string
		denied  []string
		absent  map[string][]string // scope -> tags that must be gone
		present map[string][]string // scope -> tags that must stay
	}{
		{"none denied", nil, nil, map[string][]string{"span": {"a", "http.status_code"}, "resource": {"team"}}},
		{"scoped span", []string{"span.a"}, map[string][]string{"span": {"a"}}, map[string][]string{"resource": {"service.name", "team"}, "span": {"dash-name"}}},
		{"scoped resource", []string{"resource.team"}, map[string][]string{"resource": {"team"}}, map[string][]string{"span": {"a"}, "resource": {"service.name"}}},
		{"scope mismatch keeps", []string{"resource.a"}, nil, map[string][]string{"span": {"a"}}},
		{"unscoped dot form", []string{".team"}, map[string][]string{"resource": {"team"}}, nil},
		{"unscoped bare", []string{"service.name"}, map[string][]string{"resource": {"service.name"}}, map[string][]string{"span": {"a"}}},
		{"intrinsic by bare name", []string{"name"}, map[string][]string{"intrinsic": {"name", "span:name"}}, map[string][]string{"intrinsic": {"kind"}}},
		{"intrinsic verbatim", []string{"trace:rootService"}, map[string][]string{"intrinsic": {"trace:rootService"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := FilterTempoTags(in, tt.denied)
			if err != nil {
				t.Fatal(err)
			}
			got := tagNames(t, out)
			for sc, ts := range tt.absent {
				for _, tag := range ts {
					if contains(got[sc], tag) {
						t.Errorf("%s/%s still listed", sc, tag)
					}
				}
			}
			for sc, ts := range tt.present {
				for _, tag := range ts {
					if !contains(got[sc], tag) {
						t.Errorf("%s/%s missing", sc, tag)
					}
				}
			}
			if decode(t, out)["metrics"] == nil {
				t.Error("metrics dropped")
			}
		})
	}
}

func TestFilterTagsDropsEmptiedScope(t *testing.T) {
	out, err := FilterTempoTags(fixture(t, "tags_v2.json"), []string{"resource.service.name", "resource.team"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tagNames(t, out)["resource"]; ok {
		t.Errorf("emptied scope still listed: %s", out)
	}
}

func TestFilterTagsLegacyShapeAndErrors(t *testing.T) {
	out, err := FilterTempoTags([]byte(`{"tagNames":["a","secret"],"metrics":{}}`), []string{"span.secret"})
	if err != nil || strings.Contains(string(out), "secret") || !strings.Contains(string(out), `"a"`) {
		t.Errorf("tagNames: %s, %v", out, err)
	}
	for _, bad := range []string{``, `[]`, `{"scopes":{}}`, `{"scopes":[{"tags":["a"]}]}`, `{"scopes":[{"name":"span","tags":"a"}]}`} {
		if _, err := FilterTempoTags([]byte(bad), nil); !errors.Is(err, ErrTempoResponse) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestFilterTagValues(t *testing.T) {
	in := fixture(t, "tag_values_v2.json")
	out, err := FilterTempoTagValues(in, "span.a", []string{"span.secret"})
	if err != nil || string(out) != string(in) {
		t.Errorf("permitted values changed: %v", err)
	}
	for _, name := range []string{"span.a", ".a", "a"} {
		if _, err := FilterTempoTagValues(in, name, []string{"span.a"}); err == nil {
			t.Errorf("denied name %q accepted", name)
		}
	}
	if _, err := FilterTempoTagValues(in, "resource.a", []string{"span.a"}); err != nil {
		t.Errorf("other scope rejected: %v", err)
	}
	if _, err := FilterTempoTagValues([]byte("x"), "span.a", nil); !errors.Is(err, ErrTempoResponse) {
		t.Errorf("malformed: %v", err)
	}
}

func TestTagsMerger(t *testing.T) {
	rec := httptest.NewRecorder()
	err := NewTempoTagsMerger([]string{"span.a"}, "")(rec, httptest.NewRequest(http.MethodGet, "/", nil),
		[]Response{{Status: 200, Body: fixture(t, "tags_v2.json")}})
	if err != nil || contains(tagNames(t, rec.Body.Bytes())["span"], "a") {
		t.Errorf("tags merger: %v %s", err, rec.Body)
	}
	rec = httptest.NewRecorder()
	err = NewTempoTagsMerger([]string{"span.a"}, "span.a")(rec, httptest.NewRequest(http.MethodGet, "/", nil),
		[]Response{{Status: 200, Body: fixture(t, "tag_values_v2.json")}})
	if err == nil || rec.Body.Len() != 0 {
		t.Errorf("denied values: err %v body %q", err, rec.Body)
	}
}

func TestUnionTempoTagValues(t *testing.T) {
	a := []byte(`{"tagValues":[{"type":"string","value":"ns-a"},{"type":"string","value":"x"}],"metrics":{"inspectedBytes":"1"}}`)
	b := []byte(`{"tagValues":[{"type":"string","value":"x"},{"type":"int","value":"x"},{"type":"string","value":"ns-b"}]}`)
	got, err := UnionTempoTagValues([][]byte{a, b})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"tagValues":[{"type":"string","value":"ns-a"},{"type":"string","value":"x"},{"type":"int","value":"x"},{"type":"string","value":"ns-b"}]}`
	if string(got) != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	if got, _ := UnionTempoTagValues([][]byte{[]byte(`{}`), []byte(`{"tagValues":null}`)}); string(got) != `{"tagValues":[]}` {
		t.Errorf("empty: %s", got)
	}
	if _, err := UnionTempoTagValues([][]byte{a, []byte(`[`)}); !errors.Is(err, ErrTempoResponse) {
		t.Errorf("malformed: %v", err)
	}
}

func TestUnionTempoTags(t *testing.T) {
	a := []byte(`{"scopes":[{"name":"span","tags":["a","b"]},{"name":"resource","tags":["service.name"]}],"metrics":{}}`)
	b := []byte(`{"scopes":[{"name":"span","tags":["b","c"]},{"name":"intrinsic","tags":["name"]}]}`)
	got, err := UnionTempoTags([][]byte{a, b})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"scopes":[{"name":"span","tags":["a","b","c"]},{"name":"resource","tags":["service.name"]},{"name":"intrinsic","tags":["name"]}]}`
	if string(got) != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	if got, _ := UnionTempoTags([][]byte{[]byte(`{"tagNames":["x"]}`), []byte(`{"tagNames":["x","y"]}`)}); string(got) != `{"scopes":[],"tagNames":["x","y"]}` {
		t.Errorf("legacy: %s", got)
	}
	if _, err := UnionTempoTags([][]byte{a, []byte(`{"scopes":7}`)}); !errors.Is(err, ErrTempoResponse) {
		t.Errorf("malformed: %v", err)
	}
}

func TestTempoTagsMergerUnitesAndFilters(t *testing.T) {
	resps := []Response{
		{Status: 200, Body: []byte(`{"scopes":[{"name":"span","tags":["team","secret"]}]}`)},
		{Status: 200, Body: []byte(`{"scopes":[{"name":"span","tags":["team","method"]}]}`)},
	}
	rec := httptest.NewRecorder()
	if err := NewTempoTagsMerger([]string{"span.secret"}, "")(rec, nil, resps); err != nil {
		t.Fatal(err)
	}
	if got, want := rec.Body.String(), `{"scopes":[{"name":"span","tags":["team","method"]}]}`; got != want {
		t.Errorf("tags: got %s want %s", got, want)
	}
	vals := []Response{
		{Status: 200, Body: []byte(`{"tagValues":[{"type":"string","value":"ns-a"}]}`)},
		{Status: 200, Body: []byte(`{"tagValues":[{"type":"string","value":"ns-b"}]}`)},
	}
	rec = httptest.NewRecorder()
	if err := NewTempoTagsMerger(nil, "resource.k8s.namespace.name")(rec, nil, vals); err != nil {
		t.Fatal(err)
	}
	if got, want := rec.Body.String(), `{"tagValues":[{"type":"string","value":"ns-a"},{"type":"string","value":"ns-b"}]}`; got != want {
		t.Errorf("values: got %s want %s", got, want)
	}
	if err := NewTempoTagsMerger(nil, "x")(httptest.NewRecorder(), nil, nil); !errors.Is(err, ErrTempoResponse) {
		t.Errorf("no responses: %v", err)
	}
}
