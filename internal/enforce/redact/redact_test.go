package redact

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/policy"
)

var (
	remove      = policy.Redaction{Strategy: v1.RedactRemove}
	placeholder = policy.Redaction{Strategy: v1.RedactPlaceholder}
	hash        = policy.Redaction{Strategy: v1.RedactHash}
	ip2         = policy.Redaction{Strategy: v1.RedactIPv4Prefix, Octets: 2}
)

func TestNewNil(t *testing.T) {
	if New(nil) != nil {
		t.Fatal("New(nil) must be nil")
	}
}

func TestIPv4Prefix(t *testing.T) {
	for _, tc := range []struct {
		in   string
		k    int
		want string
	}{
		{"10.1.2.3", 1, "10.x.x.x"},
		{"10.1.2.3", 2, "10.1.x.x"},
		{"10.1.2.3", 3, "10.1.2.x"},
		{"10.1.2.3", 4, Fallback},
		{"::ffff:10.1.2.3", 2, Fallback},
		{"2001:db8::1", 2, Fallback},
		{"010.1.2.3", 2, Fallback},
		{"10.1.2.3:8080", 2, Fallback},
		{"", 2, Fallback},
		{"host-1", 2, Fallback},
	} {
		if got := ipv4Prefix(tc.in, tc.k); got != tc.want {
			t.Errorf("ipv4Prefix(%q, %d) = %q, want %q", tc.in, tc.k, got, tc.want)
		}
	}
}

func TestLabelsStrategies(t *testing.T) {
	r := New(map[string]policy.Redaction{"rm": remove, "ph": placeholder, "h": hash, "ip": ip2})
	sets := []map[string]string{
		{"job": "a", "rm": "secret", "ph": "x", "h": "x", "ip": "192.168.7.9"},
		{"job": "a", "rm": "other", "ph": "y", "h": "x", "ip": "nope"},
		{"job": "b", "ph": "x"},
	}
	r.Labels(sets...)
	for _, s := range sets {
		if _, ok := s["rm"]; ok {
			t.Errorf("rm not removed: %v", s)
		}
		if s["job"] == "" {
			t.Errorf("other labels lost: %v", s)
		}
	}
	if sets[0]["ph"] != sets[2]["ph"] || sets[0]["ph"] == sets[1]["ph"] {
		t.Errorf("placeholders not consistent per value: %v", sets)
	}
	if !regexp.MustCompile(`^redacted-[12]$`).MatchString(sets[0]["ph"]) {
		t.Errorf("placeholder %q", sets[0]["ph"])
	}
	if sets[0]["h"] != sets[1]["h"] || !regexp.MustCompile(`^redacted-[0-9a-f]{16}$`).MatchString(sets[0]["h"]) {
		t.Errorf("hash %q / %q", sets[0]["h"], sets[1]["h"])
	}
	if sets[0]["ip"] != "192.168.x.x" || sets[1]["ip"] != Fallback {
		t.Errorf("ip %q / %q", sets[0]["ip"], sets[1]["ip"])
	}
	for _, s := range sets {
		for _, v := range s {
			if v == "secret" || v == "other" || v == "192.168.7.9" {
				t.Errorf("original value leaked: %v", s)
			}
		}
	}
}

func TestHashSaltedPerRedactor(t *testing.T) {
	rules := map[string]policy.Redaction{"h": hash}
	a, b := New(rules), New(rules)
	va, _ := a.Value(hash, "h", "42")
	vb, _ := b.Value(hash, "h", "42")
	if va == vb {
		t.Fatal("hash must differ between requests")
	}
	if va2, _ := a.Value(hash, "h", "42"); va2 != va {
		t.Fatal("hash must be stable within a request")
	}
	if vo, _ := a.Value(hash, "other", "42"); vo == va {
		t.Fatal("hash must depend on the label name")
	}
}

func TestPlaceholderOrderIsRandom(t *testing.T) {
	// Values arrive sorted (as Prometheus sorts series); the numbering must not follow that order.
	vals := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	identity := 0
	for range 50 {
		r := New(map[string]policy.Redaction{"ph": placeholder})
		sets := make([]map[string]string, len(vals))
		for i, v := range vals {
			sets[i] = map[string]string{"ph": v}
		}
		r.Labels(sets...)
		inOrder := true
		for i, s := range sets {
			if s["ph"] != "redacted-"+string(rune('1'+i)) {
				inOrder = false
			}
		}
		if inOrder {
			identity++
		}
	}
	if identity > 2 { // 8! permutations: even one identity permutation is unlikely
		t.Fatalf("placeholder numbering followed input order %d/50 times", identity)
	}
}

func TestPlaceholderLazyNumbering(t *testing.T) {
	r := New(map[string]policy.Redaction{"ph": placeholder})
	v1, _ := r.Value(placeholder, "ph", "a")
	v2, _ := r.Value(placeholder, "ph", "b")
	v3, _ := r.Value(placeholder, "ph", "a")
	if v1 != "redacted-1" || v2 != "redacted-2" || v3 != v1 {
		t.Fatalf("got %s %s %s", v1, v2, v3)
	}
}

func TestUnknownStrategyRemoves(t *testing.T) {
	r := New(map[string]policy.Redaction{"x": {Strategy: "Bogus"}})
	if _, keep := r.Value(policy.Redaction{Strategy: "Bogus"}, "x", "v"); keep {
		t.Fatal("unknown strategy must remove the value")
	}
}

func TestAttrRule(t *testing.T) {
	r := New(map[string]policy.Redaction{
		"span.customer":     placeholder,
		"resource.customer": remove,
		"span.client.ip":    ip2,
	})
	for _, tc := range []struct {
		scope, key string
		want       policy.Redaction
		ok         bool
	}{
		{"span", "customer", placeholder, true},
		{"resource", "customer", remove, true},
		{"", "customer", remove, true}, // unknown scope: strictest of both
		{"", "span.customer", placeholder, true},
		{"", "resource.customer", remove, true},
		{"", "client.ip", ip2, true},
		{"resource", "client.ip", policy.Redaction{}, false},
		{"span", "other", policy.Redaction{}, false},
	} {
		got, _, ok := r.AttrRule(tc.scope, tc.key)
		if ok != tc.ok || got != tc.want {
			t.Errorf("AttrRule(%q, %q) = %v, %v; want %v, %v", tc.scope, tc.key, got, ok, tc.want, tc.ok)
		}
	}
}

func TestQueryMatrix(t *testing.T) {
	r := New(map[string]policy.Redaction{"customer_id": placeholder, "ip": remove})
	in := `{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{"__name__":"up","instance":"a","customer_id":"42","ip":"1.2.3.4"},"values":[[1,"1"]]},
		{"metric":{"__name__":"up","instance":"a","customer_id":"43"},"values":[[1,"0"]]}]},
		"warnings":["w"]}`
	out, err := r.Query([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, out, `"42"`, `"43"`, "1.2.3.4", `"ip"`)
	var resp struct {
		Status string
		Data   struct {
			Result []struct {
				Metric map[string]string
				Values [][2]any
			}
		}
		Warnings []string
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "success" || len(resp.Warnings) != 2 || resp.Warnings[0] != "w" || len(resp.Data.Result) != 2 {
		t.Fatalf("envelope not preserved: %s", out)
	}
	if want := "querdian: label values redacted by policy: customer_id (numbered placeholders), ip (removed)"; resp.Warnings[1] != want {
		t.Fatalf("notice = %q, want %q", resp.Warnings[1], want)
	}
	a, b := resp.Data.Result[0].Metric["customer_id"], resp.Data.Result[1].Metric["customer_id"]
	if a == b || !strings.HasPrefix(a, "redacted-") || !strings.HasPrefix(b, "redacted-") {
		t.Fatalf("placeholders %q %q", a, b)
	}
	if resp.Data.Result[0].Values[0][1] != "1" {
		t.Fatalf("samples changed: %s", out)
	}
}

func TestQueryVectorRemoveKeepsDuplicates(t *testing.T) {
	r := New(map[string]policy.Redaction{"customer_id": remove})
	in := `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"instance":"a","customer_id":"1"},"value":[1,"1"]},
		{"metric":{"instance":"a","customer_id":"2"},"value":[1,"2"]}]}}`
	out, err := r.Query([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(out), `"metric":{"instance":"a"}`); got != 2 {
		t.Fatalf("want 2 series with identical labels, got %d: %s", got, out)
	}
}

func TestQueryPassthrough(t *testing.T) {
	r := New(map[string]policy.Redaction{"x": remove})
	for _, in := range []string{
		`{"status":"success","data":{"resultType":"scalar","result":[1,"2"]}}`,
		`{"status":"error","errorType":"bad_data","error":"x=\"secret\""}`,
	} {
		out, err := r.Query([]byte(in))
		if err != nil || string(out) != in {
			t.Errorf("Query(%s) = %s, %v; want unchanged", in, out, err)
		}
	}
}

func TestQueryMalformed(t *testing.T) {
	r := New(map[string]policy.Redaction{"x": remove})
	for _, in := range []string{
		``, `null`, `[]`, `"x"`, `{"status":"success"}`,
		`{"status":"success","data":[]}`,
		`{"status":"success","data":{"resultType":"histogram","result":[]}}`,
		`{"status":"success","data":{"resultType":"vector","result":{}}}`,
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"x":1}}]}}`,
		`{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[["1"]]}]}}`,
		`{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[["1","l",[]]]}]}}`,
		"\x0a\x03abc", // protobuf
	} {
		if _, err := r.Query([]byte(in)); !errors.Is(err, ErrResponse) {
			t.Errorf("Query(%q): want ErrResponse, got %v", in, err)
		}
	}
}

func TestQueryLokiStreams(t *testing.T) {
	r := New(map[string]policy.Redaction{"customer_id": hash, "ip": ip2})
	in := `{"status":"success","data":{"resultType":"streams","result":[
		{"stream":{"app":"x","customer_id":"42"},"values":[["1","line 42"],["2","l2",{"structuredMetadata":{"ip":"10.9.8.7"},"parsed":{"customer_id":"42"}}]]}],
		"stats":{"summary":{}}}}`
	out, err := r.Query([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, out, `"42"`, "10.9.8.7")
	if !strings.Contains(string(out), `"line 42"`) {
		t.Errorf("log lines must be unchanged: %s", out)
	}
	if !strings.Contains(string(out), `"ip":"10.9.x.x"`) || !strings.Contains(string(out), `"stats"`) {
		t.Errorf("unexpected output: %s", out)
	}
	var resp struct {
		Data struct {
			Result []struct {
				Stream map[string]string
				Values [][]json.RawMessage
			}
		}
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	var meta struct{ Parsed map[string]string }
	_ = json.Unmarshal(resp.Data.Result[0].Values[1][2], &meta)
	if meta.Parsed["customer_id"] != resp.Data.Result[0].Stream["customer_id"] {
		t.Errorf("same value must get the same hash in stream labels and metadata: %s", out)
	}
}

func TestExemplars(t *testing.T) {
	r := New(map[string]policy.Redaction{"customer_id": placeholder})
	in := `{"status":"success","data":[{"seriesLabels":{"__name__":"m","customer_id":"42"},
		"exemplars":[{"labels":{"trace_id":"t","customer_id":"42"},"value":"1","timestamp":1}]}]}`
	out, err := r.Exemplars([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, out, `"42"`)
	if strings.Count(string(out), `"customer_id":"redacted-1"`) != 2 || !strings.Contains(string(out), `"trace_id":"t"`) {
		t.Errorf("unexpected output: %s", out)
	}
}

func TestTailFrame(t *testing.T) {
	r := New(map[string]policy.Redaction{"customer_id": placeholder})
	f1, err := r.TailFrame([]byte(`{"streams":[{"stream":{"customer_id":"a"},"values":[["1","x"]]}],"dropped_entries":[{"labels":{"customer_id":"b"},"timestamp":"1"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, f1, `"a"`, `"b"`)
	f2, err := r.TailFrame([]byte(`{"streams":[{"stream":{"customer_id":"a"},"values":[["2","y"]]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Streams []struct{ Stream map[string]string }
	}
	_ = json.Unmarshal(f1, &got)
	first := got.Streams[0].Stream["customer_id"]
	_ = json.Unmarshal(f2, &got)
	if got.Streams[0].Stream["customer_id"] != first {
		t.Errorf("placeholders must be stable across frames of one connection: %s / %s", f1, f2)
	}
	if _, err := r.TailFrame([]byte(`[]`)); !errors.Is(err, ErrResponse) {
		t.Errorf("want ErrResponse for a non-object frame, got %v", err)
	}
}

func TestTempo(t *testing.T) {
	r := New(map[string]policy.Redaction{"span.customer": placeholder, "span.client.ip": ip2, "resource.secret": remove})
	in := `{"traces":[{"traceID":"1","spanSet":{"spans":[{"spanID":"2","attributes":[
		{"key":"customer","value":{"intValue":"42"}},
		{"key":"client.ip","value":{"stringValue":"10.1.2.3"}},
		{"key":"secret","value":{"stringValue":"s3cr3t"}},
		{"key":"http.status_code","value":{"intValue":"500"}}]}],"matched":1,
		"attributes":[{"key":"customer","value":{"intValue":"42"}}]}}],
		"series":[{"labels":[{"key":"span.customer","value":{"stringValue":"7"}}],"promLabels":"{customer=\"7\"}","samples":[{"timestampMs":"1","value":1.5}]}],
		"metrics":{"inspectedBytes":"123456789012345678"}}`
	out, err := r.Tempo([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeak(t, out, `"42"`, "10.1.2.3", "s3cr3t", `"secret"`, "promLabels", `"7"`)
	for _, want := range []string{
		`{"key":"client.ip","value":{"stringValue":"10.1.x.x"}}`,
		`{"key":"http.status_code","value":{"intValue":"500"}}`,
		`"inspectedBytes":"123456789012345678"`,
		`"value":1.5`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	if strings.Count(string(out), `{"key":"customer","value":{"stringValue":"redacted-`) != 2 {
		t.Errorf("customer not rewritten twice: %s", out)
	}
	for _, bad := range []string{``, `{`, `{} {}`, `{"x":[{"key":"customer"}`, `{"labels":{"customer":"1"}}`, `{"a":{"attributes":[{"k":"customer"}]}}`} {
		if _, err := r.Tempo([]byte(bad)); !errors.Is(err, ErrResponse) {
			t.Errorf("Tempo(%q): want ErrResponse, got %v", bad, err)
		}
	}
}

func assertNoLeak(t *testing.T, out []byte, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if strings.Contains(string(out), n) {
			t.Errorf("output contains %s: %s", n, out)
		}
	}
}

// Every recorded Tempo response shape must be accepted by the walker.
func TestTempoRecordedFixtures(t *testing.T) {
	files, err := filepath.Glob("../../fanout/testdata/tempo/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	r := New(map[string]policy.Redaction{"span.a": placeholder})
	for _, f := range files {
		body, err := os.ReadFile(f) //nolint:gosec // fixed fixtures
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Tempo(body); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
	}
}

func TestNoticeOnlyWhenRedacted(t *testing.T) {
	r := New(map[string]policy.Redaction{"customer_id": ip2})
	out, err := r.Query([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"job":"a"},"value":[1,"1"]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "warnings") {
		t.Fatalf("no redacted label, no notice: %s", out)
	}
	out, err = r.Query([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"customer_id":"1.2.3.4"},"value":[1,"1"]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"warnings":["querdian: label values redacted by policy: customer_id (first 2 octets kept)"]`) {
		t.Fatalf("missing notice: %s", out)
	}
	if _, err := r.Query([]byte(`{"status":"success","warnings":{},"data":{"resultType":"vector","result":[{"metric":{"customer_id":"x"}}]}}`)); !errors.Is(err, ErrResponse) {
		t.Fatalf("malformed warnings must fail, got %v", err)
	}
}
