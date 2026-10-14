package fanout

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func setMerge(t *testing.T, path string, denied []string, bodies ...string) (map[string]any, error) {
	t.Helper()
	rs := make([]Response, len(bodies))
	for i, b := range bodies {
		rs[i] = Response{Status: 200, Body: []byte(b)}
	}
	r := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	m := NewStringSetMerger(func(*http.Request) []string { return denied })
	if err := m(w, r, rs); err != nil {
		if w.Body.Len() != 0 {
			t.Errorf("wrote before error")
		}
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid output %q", w.Body.String())
	}
	return out, nil
}

func TestStringSetLabels(t *testing.T) {
	out, err := setMerge(t, "/loki/api/v1/labels", []string{"team", "secret"},
		`{"status":"success","data":["service_name","app","team"]}`,
		`{"status":"success","data":["env","app","secret"]}`,
		`{"status":"success"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out["data"], []any{"app", "env", "service_name"}) || out["status"] != "success" {
		t.Fatalf("out = %v", out)
	}
}

func TestStringSetLabelValuesNotFiltered(t *testing.T) {
	// A value that happens to spell a denied label name is data, not a label.
	out, err := setMerge(t, "/loki/api/v1/label/app/values", []string{"team"},
		`{"status":"success","data":["b","team"]}`, `{"status":"success","data":["a","b"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out["data"], []any{"a", "b", "team"}) {
		t.Fatalf("out = %v", out)
	}
}

func TestStringSetAllEmpty(t *testing.T) {
	out, err := setMerge(t, "/loki/api/v1/labels", nil, `{"status":"success"}`, `{"status":"success"}`)
	if err != nil || !reflect.DeepEqual(out["data"], []any{}) {
		t.Fatalf("out = %v err %v", out, err)
	}
	out, err = setMerge(t, "/loki/api/v1/detected_labels", nil, `{}`, `{}`)
	if err != nil || len(out) != 0 {
		t.Fatalf("out = %v err %v", out, err)
	}
}

func TestStringSetDetectedLabels(t *testing.T) {
	out, err := setMerge(t, "/loki/api/v1/detected_labels", []string{"team"},
		`{"detectedLabels":[{"label":"service_name","cardinality":1},{"label":"app","cardinality":1},{"label":"team","cardinality":1}]}`,
		`{}`,
		`{"detectedLabels":[{"label":"env","cardinality":3},{"label":"app","cardinality":2}]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"label": "app", "cardinality": float64(3)},
		map[string]any{"label": "env", "cardinality": float64(3)},
		map[string]any{"label": "service_name", "cardinality": float64(1)},
	}
	if !reflect.DeepEqual(out["detectedLabels"], want) {
		t.Fatalf("out = %v", out)
	}
}

func TestStringSetDetectedFields(t *testing.T) {
	out, err := setMerge(t, "/loki/api/v1/detected_fields", []string{"secret"},
		`{"fields":[{"label":"n","type":"int","cardinality":2,"parsers":["logfmt"]},{"label":"secret","type":"string","cardinality":1,"parsers":null},{"label":"d","type":"string","cardinality":2,"parsers":null}],"limit":1000}`,
		`{"fields":[{"label":"n","type":"float","cardinality":1,"parsers":["json"]},{"label":"msg","type":"string","cardinality":4,"parsers":["json","logfmt"]}],"limit":1000}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"label": "d", "type": "string", "cardinality": float64(2), "parsers": nil},
		map[string]any{"label": "msg", "type": "string", "cardinality": float64(4), "parsers": []any{"json", "logfmt"}},
		map[string]any{"label": "n", "type": "string", "cardinality": float64(3), "parsers": []any{"json", "logfmt"}},
	}
	if !reflect.DeepEqual(out["fields"], want) || out["limit"] != float64(1000) {
		t.Fatalf("out = %v", out)
	}
	out, err = setMerge(t, "/loki/api/v1/detected_fields", nil, `{"fields":[{"label":"a","type":"string","cardinality":1,"parsers":null},{"label":"b","type":"string","cardinality":1,"parsers":null}],"limit":1}`)
	if err != nil || len(out["fields"].([]any)) != 1 {
		t.Fatalf("limit not applied: %v %v", out, err)
	}
}

func TestStringSetDetectedFieldValues(t *testing.T) {
	out, err := setMerge(t, "/loki/api/v1/detected_field/msg/values", []string{"hello"},
		`{"limit":1000,"values":["hello","b"]}`, `{}`, `{"limit":1000,"values":["a","b"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out["values"], []any{"a", "b", "hello"}) {
		t.Fatalf("out = %v", out)
	}
}

func TestStringSetErrors(t *testing.T) {
	list := `{"status":"success","data":["a"]}`
	for name, bad := range map[string]string{
		"not json":     `{`,
		"trailing":     list + ` x`,
		"status":       `{"status":"error","data":[]}`,
		"data type":    `{"status":"success","data":{"a":1}}`,
		"data element": `{"status":"success","data":[1]}`,
		"other shape":  `{"detectedLabels":[]}`,
		"unknown":      `{"foo":1}`,
		"no label":     `{"detectedLabels":[{"cardinality":1}]}`,
		"bad card":     `{"detectedLabels":[{"label":"a","cardinality":"x"}]}`,
		"empty":        ``,
		"array":        `[]`,
	} {
		if out, err := setMerge(t, "/loki/api/v1/labels", nil, list, bad); err == nil {
			t.Errorf("%s: expected error, got %v", name, out)
		}
	}
	if _, err := setMerge(t, "/loki/api/v1/labels", nil); err == nil {
		t.Error("no responses must fail")
	}
}

func TestStringSetNilDenied(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/loki/api/v1/labels", nil)
	w := httptest.NewRecorder()
	if err := NewStringSetMerger(nil)(w, r, []Response{{Body: []byte(`{"status":"success","data":["b","a"]}`)}}); err != nil {
		t.Fatal(err)
	}
}
