package fanout

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func sumMerge(t *testing.T, target string, denied []string, approx bool, bodies ...string) (*httptest.ResponseRecorder, map[string]any, error) {
	t.Helper()
	rs := make([]Response, len(bodies))
	for i, b := range bodies {
		rs[i] = Response{Status: 200, Body: []byte(b)}
	}
	r := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	m := NewSumCountsMerger(func(*http.Request) []string { return denied }, func(*http.Request) bool { return approx })
	if err := m(w, r, rs); err != nil {
		if w.Body.Len() != 0 {
			t.Error("wrote before error")
		}
		return w, nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid output %q", w.Body.String())
	}
	return w, out, nil
}

func TestSumIndexStats(t *testing.T) {
	w, out, err := sumMerge(t, "/loki/api/v1/index/stats", nil, false,
		`{"streams":1,"chunks":2,"bytes":100,"entries":10}`, `{"streams":2,"chunks":0,"bytes":50,"entries":5}`, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"streams": 3.0, "chunks": 2.0, "bytes": 150.0, "entries": 15.0}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("out = %v", out)
	}
	if w.Header().Get(ApproximateHeader) != "" {
		t.Error("disjoint merge must not be marked approximate")
	}
	_, out, err = sumMerge(t, "/x", nil, true, `{}`, `{}`)
	if err != nil || len(out) != 0 {
		t.Fatalf("all empty: %v %v", out, err)
	}
}

func volBody(rt string, results string, stats string) string {
	s := ""
	if stats != "" {
		s = `,"stats":` + stats
	}
	return `{"status":"success","data":{"resultType":"` + rt + `","result":` + results + s + `}}`
}

func TestSumVolumeVector(t *testing.T) {
	w, out, err := sumMerge(t, "/loki/api/v1/index/volume", []string{"secret"}, true,
		volBody("vector", `[{"metric":{"app":"a","secret":"1"},"value":[100,"10"]},{"metric":{"app":"b"},"value":[100,"7"]}]`, `{"summary":{"totalBytesProcessed":1}}`),
		volBody("vector", `[{"metric":{"app":"a","secret":"2"},"value":[101,"5"]},{"metric":{"app":"c"},"value":[101,"20"]}]`, `{"summary":{"totalBytesProcessed":2}}`),
		volBody("vector", `[]`, ""))
	if err != nil {
		t.Fatal(err)
	}
	data := out["data"].(map[string]any)
	want := []any{
		map[string]any{"metric": map[string]any{"app": "c"}, "value": []any{101.0, "20"}},
		map[string]any{"metric": map[string]any{"app": "a"}, "value": []any{101.0, "15"}},
		map[string]any{"metric": map[string]any{"app": "b"}, "value": []any{100.0, "7"}},
	}
	if !reflect.DeepEqual(data["result"], want) || data["resultType"] != "vector" || out["status"] != "success" {
		t.Fatalf("out = %v", out)
	}
	if data["stats"].(map[string]any)["summary"].(map[string]any)["totalBytesProcessed"] != 3.0 {
		t.Errorf("stats = %v", data["stats"])
	}
	if w.Header().Get(ApproximateHeader) != "true" {
		t.Error("approximate marker missing")
	}
}

func TestSumVolumeLimit(t *testing.T) {
	b := volBody("vector", `[{"metric":{"a":"1"},"value":[1,"1"]},{"metric":{"a":"2"},"value":[1,"3"]},{"metric":{"a":"3"},"value":[1,"2"]}]`, "")
	_, out, err := sumMerge(t, "/v?limit=2", nil, false, b)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(out["data"].(map[string]any)["result"].([]any)); n != 2 {
		t.Fatalf("limit not applied: %d", n)
	}
	if _, _, err := sumMerge(t, "/v?limit=x", nil, false, b); err == nil {
		t.Error("bad limit must fail")
	}
}

func TestSumVolumeRange(t *testing.T) {
	_, out, err := sumMerge(t, "/loki/api/v1/index/volume_range", nil, false,
		volBody("matrix", `[{"metric":{"app":"a"},"values":[[60,"1"],[120,"2"]]}]`, ""),
		volBody("matrix", `[{"metric":{"app":"a"},"values":[[120,"3"],[180,"4"]]},{"metric":{"app":"b"},"values":[[60,"1"]]}]`, ""))
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"metric": map[string]any{"app": "a"}, "values": []any{[]any{60.0, "1"}, []any{120.0, "5"}, []any{180.0, "4"}}},
		map[string]any{"metric": map[string]any{"app": "b"}, "values": []any{[]any{60.0, "1"}}},
	}
	if got := out["data"].(map[string]any)["result"]; !reflect.DeepEqual(got, want) || out["data"].(map[string]any)["resultType"] != "matrix" {
		t.Fatalf("out = %v", out)
	}
}

func TestSumPatterns(t *testing.T) {
	_, out, err := sumMerge(t, "/loki/api/v1/patterns", nil, false,
		`{"status":"success","data":[{"pattern":"user <_> in","samples":[[60,2],[120,1]]},{"pattern":"x","samples":[[60,1]]}]}`,
		`{"status":"success","data":[{"pattern":"user <_> in","samples":[[120,4]]}]}`,
		`{"status":"success","data":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"pattern": "user <_> in", "samples": []any{[]any{60.0, 2.0}, []any{120.0, 5.0}}},
		map[string]any{"pattern": "x", "samples": []any{[]any{60.0, 1.0}}},
	}
	if !reflect.DeepEqual(out["data"], want) {
		t.Fatalf("out = %v", out)
	}
	_, out, err = sumMerge(t, "/p", nil, false, `{"status":"success","data":[]}`)
	if err != nil || !reflect.DeepEqual(out["data"], []any{}) {
		t.Fatalf("empty: %v %v", out, err)
	}
}

func TestSumWarningsKept(t *testing.T) {
	_, out, err := sumMerge(t, "/p", nil, false,
		`{"status":"success","data":[],"warnings":["w1"]}`, `{"status":"success","data":[],"warnings":["w1","w2"]}`)
	if err != nil || !reflect.DeepEqual(out["warnings"], []any{"w1", "w2"}) {
		t.Fatalf("out = %v %v", out, err)
	}
}

func TestSumErrors(t *testing.T) {
	stats := `{"streams":1}`
	vec := volBody("vector", `[]`, "")
	for name, bad := range map[string]string{
		"not json":      `{`,
		"trailing":      stats + `x`,
		"string count":  `{"streams":"1"}`,
		"unknown shape": `{"data":{}}`,
		"status error":  `{"status":"error","data":[]}`,
		"no data":       `{"status":"success"}`,
		"streams type":  volBody("streams", `[]`, ""),
		"bad result":    volBody("vector", `{}`, ""),
		"bad value":     volBody("vector", `[{"metric":{"a":"b"},"value":[1,"x"]}]`, ""),
		"short value":   volBody("vector", `[{"metric":{"a":"b"},"value":[1]}]`, ""),
		"num value":     volBody("vector", `[{"metric":{"a":"b"},"value":[1,2]}]`, ""),
		"bad matrix":    volBody("matrix", `[{"metric":{"a":"b"},"values":[[1]]}]`, ""),
		"bad pattern":   `{"status":"success","data":[{"pattern":"p","samples":[[1]]}]}`,
		"bad stats":     volBody("vector", `[]`, `[]`),
	} {
		for _, good := range []string{stats, vec, `{"status":"success","data":[]}`} {
			if _, out, err := sumMerge(t, "/x", nil, false, good, bad); err == nil {
				t.Errorf("%s with %s: expected error, got %v", name, good, out)
			}
		}
	}
	// Mismatching but individually valid shapes also fail.
	if _, _, err := sumMerge(t, "/x", nil, false, stats, vec); err == nil {
		t.Error("shape mismatch must fail")
	}
	if _, _, err := sumMerge(t, "/x", nil, false); err == nil {
		t.Error("no responses must fail")
	}
}

func TestSumNilApproximateIsApproximate(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	if err := NewSumCountsMerger(nil, nil)(w, r, []Response{{Body: []byte(`{"streams":1}`)}}); err != nil {
		t.Fatal(err)
	}
	if w.Header().Get(ApproximateHeader) != "true" {
		t.Error("nil approximate func must mark the result approximate")
	}
}

func TestSumSpanAttribute(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	ctx, span := tp.Tracer("t").Start(context.Background(), "merge")
	r := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)
	m := NewSumCountsMerger(nil, func(*http.Request) bool { return true })
	if err := m(httptest.NewRecorder(), r, []Response{{Body: []byte(`{"streams":1}`)}}); err != nil {
		t.Fatal(err)
	}
	span.End()
	var got, kind bool
	for _, a := range rec.Ended()[0].Attributes() {
		if a.Key == "merge.approximate" && a.Value.AsBool() {
			got = true
		}
		if a.Key == "merge.kind" && a.Value.AsString() == "sum_counts" {
			kind = true
		}
	}
	if !got || !kind {
		t.Errorf("attributes = %v", rec.Ended()[0].Attributes())
	}
}
