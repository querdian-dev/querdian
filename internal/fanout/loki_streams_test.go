package fanout

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type tEntry struct {
	ts   int64
	line string
}

type tStream struct {
	labels  map[string]string
	entries []tEntry
}

func streamsBody(t *testing.T, flags []string, stats string, streams ...tStream) []byte {
	t.Helper()
	res := []map[string]any{}
	for _, s := range streams {
		vals := [][]any{}
		for _, e := range s.entries {
			v := []any{strconv.FormatInt(e.ts, 10), e.line}
			if flags != nil {
				v = append(v, map[string]any{"structuredMetadata": map[string]string{"k": "v"}})
			}
			vals = append(vals, v)
		}
		res = append(res, map[string]any{"stream": s.labels, "values": vals})
	}
	data := map[string]any{"resultType": "streams", "result": res}
	if flags != nil {
		data["encodingFlags"] = flags
	}
	if stats != "" {
		data["stats"] = json.RawMessage(stats)
	}
	b, err := json.Marshal(map[string]any{"status": "success", "data": data})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func doMerge(t *testing.T, target string, resps ...[]byte) (map[string]any, error) {
	t.Helper()
	rs := make([]Response, len(resps))
	for i, b := range resps {
		rs[i] = Response{Status: 200, Body: b}
	}
	r := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	if err := MergeLokiStreams(w, r, rs); err != nil {
		if w.Body.Len() != 0 {
			t.Errorf("wrote %d bytes before returning error", w.Body.Len())
		}
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid output %q: %v", w.Body.String(), err)
	}
	return out, nil
}

// flatten returns "labels|ts|line" for every entry in output order.
func flatten(t *testing.T, out map[string]any) []string {
	t.Helper()
	var got []string
	for _, s := range out["data"].(map[string]any)["result"].([]any) {
		sm := s.(map[string]any)
		lab := sm["stream"].(map[string]any)
		keys := make([]string, 0, len(lab))
		for k, v := range lab {
			keys = append(keys, k+"="+v.(string))
		}
		sort.Strings(keys)
		for _, v := range sm["values"].([]any) {
			e := v.([]any)
			got = append(got, strings.Join(keys, ",")+"|"+e[0].(string)+"|"+e[1].(string))
		}
	}
	return got
}

var (
	lA = map[string]string{"app": "a"}
	lB = map[string]string{"app": "b"}
)

func TestMergeOverlapDedupAndOrder(t *testing.T) {
	b1 := streamsBody(t, nil, "", tStream{lA, []tEntry{{3, "x"}, {2, "y"}}}, tStream{lB, []tEntry{{2, "y"}}})
	b2 := streamsBody(t, nil, "", tStream{lA, []tEntry{{3, "x"}, {1, "z"}}})
	out, err := doMerge(t, "/q?limit=10", b1, b2)
	if err != nil {
		t.Fatal(err)
	}
	// Same (ts,line) in different streams is kept; within stream a it is deduplicated.
	want := []string{"app=a|3|x", "app=a|2|y", "app=a|1|z", "app=b|2|y"}
	if got := flatten(t, out); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if n := len(out["data"].(map[string]any)["result"].([]any)); n != 2 {
		t.Fatalf("streams = %d, want 2 (identical label sets merged)", n)
	}
}

func TestMergeDirections(t *testing.T) {
	b1 := streamsBody(t, nil, "", tStream{lA, []tEntry{{5, "e"}, {1, "a"}}})
	b2 := streamsBody(t, nil, "", tStream{lA, []tEntry{{4, "d"}, {2, "b"}}}, tStream{lB, []tEntry{{3, "c"}}})
	cases := []struct {
		query string
		want  []string
	}{
		{"limit=3", []string{"app=a|5|e", "app=a|4|d", "app=b|3|c"}},
		{"limit=3&direction=backward", []string{"app=a|5|e", "app=a|4|d", "app=b|3|c"}},
		{"limit=3&direction=forward", []string{"app=a|1|a", "app=a|2|b", "app=b|3|c"}},
		{"limit=0", nil},
	}
	for _, c := range cases {
		out, err := doMerge(t, "/q?"+c.query, b1, b2)
		if err != nil {
			t.Fatalf("%s: %v", c.query, err)
		}
		if got := flatten(t, out); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v want %v", c.query, got, c.want)
		}
	}
}

func TestMergeDefaultLimit(t *testing.T) {
	var es []tEntry
	for i := int64(1); i <= 150; i++ {
		es = append(es, tEntry{i, "l" + strconv.FormatInt(i, 10)})
	}
	out, err := doMerge(t, "/q", streamsBody(t, nil, "", tStream{lA, es}))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(flatten(t, out)); n != DefaultLokiLimit {
		t.Fatalf("entries = %d, want %d", n, DefaultLokiLimit)
	}
}

func TestMergeEmptyBranches(t *testing.T) {
	empty := streamsBody(t, nil, "")
	full := streamsBody(t, nil, "", tStream{lA, []tEntry{{1, "a"}}})
	for name, in := range map[string][][]byte{"all": {empty, empty}, "one": {empty, full}, "first": {full, empty}} {
		out, err := doMerge(t, "/q?limit=5", in...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		n := len(flatten(t, out))
		if (name == "all") != (n == 0) {
			t.Errorf("%s: %d entries", name, n)
		}
		if out["data"].(map[string]any)["result"] == nil {
			t.Errorf("%s: result must be [] not null", name)
		}
	}
}

func TestMergeIdenticalLabelSetsAcrossBranches(t *testing.T) {
	b1 := streamsBody(t, nil, "", tStream{map[string]string{"a": "1", "b": "2"}, []tEntry{{1, "x"}}})
	b2 := streamsBody(t, nil, "", tStream{map[string]string{"b": "2", "a": "1"}, []tEntry{{2, "y"}}})
	out, err := doMerge(t, "/q", b1, b2)
	if err != nil {
		t.Fatal(err)
	}
	res := out["data"].(map[string]any)["result"].([]any)
	if len(res) != 1 || len(res[0].(map[string]any)["values"].([]any)) != 2 {
		t.Fatalf("result = %v", res)
	}
}

func TestMergeEncodingFlagsAndMetadata(t *testing.T) {
	fl := []string{"categorize-labels"}
	b1 := streamsBody(t, fl, "", tStream{lA, []tEntry{{2, "x"}}})
	b2 := streamsBody(t, fl, "", tStream{lA, []tEntry{{1, "y"}}})
	out, err := doMerge(t, "/q", b1, b2)
	if err != nil {
		t.Fatal(err)
	}
	data := out["data"].(map[string]any)
	if !reflect.DeepEqual(data["encodingFlags"], []any{"categorize-labels"}) {
		t.Fatalf("encodingFlags = %v", data["encodingFlags"])
	}
	e := data["result"].([]any)[0].(map[string]any)["values"].([]any)[0].([]any)
	if len(e) != 3 || e[2].(map[string]any)["structuredMetadata"] == nil {
		t.Fatalf("metadata lost: %v", e)
	}
	// Disagreeing flags cannot be merged.
	if _, err := doMerge(t, "/q", b1, streamsBody(t, nil, "", tStream{lA, nil})); err == nil {
		t.Fatal("expected error for differing encodingFlags")
	}
}

func TestMergeStatsAndWarnings(t *testing.T) {
	st := `{"summary":{"totalBytesProcessed":10,"totalLinesProcessed":4,"execTime":0.5,"bytesProcessedPerSecond":1,"linesProcessedPerSecond":1},"index":{"usedBloomFilters":false},"cache":{"chunk":{"requests":2}}}`
	st2 := `{"summary":{"totalBytesProcessed":30,"totalLinesProcessed":6,"execTime":0.5,"bytesProcessedPerSecond":1,"linesProcessedPerSecond":1},"index":{"usedBloomFilters":true},"cache":{"chunk":{"requests":3}}}`
	b1 := streamsBody(t, nil, st, tStream{lA, []tEntry{{1, "x"}}})
	b2 := streamsBody(t, nil, st2, tStream{lA, []tEntry{{2, "y"}}})
	// add warnings to b2
	var top map[string]any
	_ = json.Unmarshal(b2, &top)
	top["warnings"] = []string{"w1"}
	b2, _ = json.Marshal(top)
	top["warnings"] = []string{"w1", "w2"}
	b3, _ := json.Marshal(top)

	out, err := doMerge(t, "/q", b1, b2, b3)
	if err != nil {
		t.Fatal(err)
	}
	stats := out["data"].(map[string]any)["stats"].(map[string]any)
	sum := stats["summary"].(map[string]any)
	if sum["totalBytesProcessed"].(float64) != 70 || sum["totalLinesProcessed"].(float64) != 16 {
		t.Errorf("summary = %v", sum)
	}
	if sum["execTime"].(float64) != 1.5 {
		t.Errorf("execTime = %v", sum["execTime"])
	}
	if sum["bytesProcessedPerSecond"].(float64) != 46 { // 70 / 1.5
		t.Errorf("rate = %v", sum["bytesProcessedPerSecond"])
	}
	if stats["index"].(map[string]any)["usedBloomFilters"] != true {
		t.Errorf("bool not OR-ed: %v", stats["index"])
	}
	if stats["cache"].(map[string]any)["chunk"].(map[string]any)["requests"].(float64) != 8 {
		t.Errorf("nested sum: %v", stats["cache"])
	}
	if !reflect.DeepEqual(out["warnings"], []any{"w1", "w2"}) {
		t.Errorf("warnings = %v", out["warnings"])
	}
}

func TestMergeErrors(t *testing.T) {
	good := streamsBody(t, nil, "", tStream{lA, []tEntry{{1, "x"}}})
	bad := map[string]string{
		"not json":       `{"status":`,
		"trailing":       string(good) + `{}`,
		"array":          `[]`,
		"null":           `null`,
		"status error":   `{"status":"error","data":{"resultType":"streams","result":[]}}`,
		"no data":        `{"status":"success"}`,
		"matrix":         `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
		"bad result":     `{"status":"success","data":{"resultType":"streams","result":{}}}`,
		"bad ts":         `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"a":"b"},"values":[["x","l"]]}]}}`,
		"short entry":    `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"a":"b"},"values":[["1"]]}]}}`,
		"numeric line":   `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"a":"b"},"values":[["1",2]]}]}}`,
		"no stream":      `{"status":"success","data":{"resultType":"streams","result":[{"values":[["1","l"]]}]}}`,
		"bad stats":      `{"status":"success","data":{"resultType":"streams","result":[],"stats":[1]}}`,
		"empty body":     ``,
		"bad flags type": `{"status":"success","data":{"resultType":"streams","result":[],"encodingFlags":1}}`,
	}
	for name, b := range bad {
		for _, order := range [][][]byte{{good, []byte(b)}, {[]byte(b), good}} {
			if out, err := doMerge(t, "/q", order...); err == nil {
				t.Errorf("%s: expected error, got %v", name, out)
			}
		}
	}
	if _, err := mergeNone(nil); err == nil {
		t.Error("no responses must be an error")
	}
}

// mergeNone merges zero responses.
func mergeNone(resps []Response) (*httptest.ResponseRecorder, error) {
	w := httptest.NewRecorder()
	return w, MergeLokiStreams(w, httptest.NewRequest(http.MethodGet, "/q", nil), resps)
}

func TestMergeParamSources(t *testing.T) {
	good := streamsBody(t, nil, "", tStream{lA, []tEntry{{1, "x"}, {2, "y"}}})
	for _, target := range []string{"/q?limit=x", "/q?limit=-1", "/q?direction=sideways"} {
		if _, err := doMerge(t, target, good); err == nil {
			t.Errorf("%s: expected error", target)
		}
	}
	// POST without Form: parameters unknown -> error; with Form -> used.
	post := httptest.NewRequest(http.MethodPost, "/q", nil)
	if err := MergeLokiStreams(httptest.NewRecorder(), post, []Response{{Body: good}}); err == nil {
		t.Error("POST without r.Form must fail")
	}
	post.Form = url.Values{"limit": {"1"}}
	w := httptest.NewRecorder()
	if err := MergeLokiStreams(w, post, []Response{{Body: good}}); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if n := len(flatten(t, out)); n != 1 {
		t.Errorf("entries = %d, want 1", n)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("content type = %q", w.Header().Get("Content-Type"))
	}
}

// refTopL is the reference: dedupe per stream, order totally, take L.
func refTopL(in map[string][]tEntry, limit int, backward bool) []string {
	type ent struct {
		key string
		tEntry
	}
	seen := map[string]bool{}
	var all []ent
	for k, es := range in {
		for _, e := range es {
			id := fmt.Sprintf("%s|%d|%s", k, e.ts, e.line)
			if !seen[id] {
				seen[id] = true
				all = append(all, ent{k, e})
			}
		}
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.ts != b.ts {
			return (a.ts > b.ts) == backward
		}
		if a.key != b.key {
			return a.key < b.key
		}
		return a.line < b.line
	})
	if len(all) > limit {
		all = all[:limit]
	}
	out := make([]string, len(all))
	for i, e := range all {
		out[i] = fmt.Sprintf("%s|%d|%s", e.key, e.ts, e.line)
	}
	return out
}

// groupRef groups a globally ordered id list by stream in order of first appearance, which is the output layout.
func groupRef(ids []string) []string {
	var order []string
	by := map[string][]string{}
	for _, id := range ids {
		k, _, _ := splitID(id)
		if _, ok := by[k]; !ok {
			order = append(order, k)
		}
		by[k] = append(by[k], id)
	}
	var out []string
	for _, k := range order {
		out = append(out, by[k]...)
	}
	return out
}

func splitID(id string) (key string, ts int64, line string) {
	i := strings.LastIndex(id, "|")
	line = id[i+1:]
	rest := id[:i]
	j := strings.LastIndex(rest, "|")
	ts, _ = strconv.ParseInt(rest[j+1:], 10, 64)
	return rest[:j], ts, line
}

// TestMergePropertyTopL checks merge(topL(A), topL(B)) == topL(A union B) on random data, both directions,
// with overlapping branches and duplicated entries.
func TestMergePropertyTopL(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test data
	labelSets := []map[string]string{{"app": "a"}, {"app": "b"}, {"app": "c", "env": "x"}}
	byKey := map[string]map[string]string{}
	for _, ls := range labelSets {
		byKey[labelSetKey(ls)] = ls
	}
	for iter := 0; iter < 500; iter++ {
		backward := rng.Intn(2) == 0
		limit := rng.Intn(12)
		branches := make([]map[string][]tEntry, 1+rng.Intn(3))
		for b := range branches {
			branches[b] = map[string][]tEntry{}
		}
		union := map[string][]tEntry{}
		for i := 0; i < rng.Intn(40); i++ {
			k := labelSetKey(labelSets[rng.Intn(len(labelSets))])
			e := tEntry{ts: int64(rng.Intn(15)), line: []string{"a", "b", "c"}[rng.Intn(3)]}
			for b := range branches {
				if rng.Intn(2) == 0 {
					branches[b][k] = append(branches[b][k], e)
					union[k] = append(union[k], e)
				}
			}
		}
		var bodies [][]byte
		for _, br := range branches {
			// Each branch answers like Loki would: its own top-L under the same total order.
			var streams []tStream
			idx := map[string]int{}
			for _, id := range groupRef(refTopL(br, limit, backward)) {
				k, ts, line := splitID(id)
				if _, ok := idx[k]; !ok {
					idx[k] = len(streams)
					streams = append(streams, tStream{labels: byKey[k]})
				}
				streams[idx[k]].entries = append(streams[idx[k]].entries, tEntry{ts, line})
			}
			bodies = append(bodies, streamsBody(t, nil, "", streams...))
		}
		dir := "backward"
		if !backward {
			dir = "forward"
		}
		out, err := doMerge(t, fmt.Sprintf("/q?limit=%d&direction=%s", limit, dir), bodies...)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		var got []string
		for _, s := range out["data"].(map[string]any)["result"].([]any) {
			sm := s.(map[string]any)
			lab := map[string]string{}
			for k, v := range sm["stream"].(map[string]any) {
				lab[k] = v.(string)
			}
			for _, v := range sm["values"].([]any) {
				e := v.([]any)
				got = append(got, fmt.Sprintf("%s|%s|%s", labelSetKey(lab), e[0].(string), e[1].(string)))
			}
		}
		want := groupRef(refTopL(union, limit, backward))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("iter %d (limit %d backward %v):\n got  %v\n want %v", iter, limit, backward, got, want)
		}
	}
}
