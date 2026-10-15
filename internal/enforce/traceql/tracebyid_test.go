package traceql

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	commonv1 "github.com/querdian-dev/querdian/internal/tempopb/common/v1"
	resourcev1 "github.com/querdian-dev/querdian/internal/tempopb/resource/v1"
	tracev1 "github.com/querdian-dev/querdian/internal/tempopb/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
)

// Fixtures recorded black box from Tempo 2.9.0 (testdata/record.sh): trace 1 has the spans
// "GET /" (frontend, a=x, http.status_code=200 int), "db query" (backend, resource team=t1, a=y,
// "weird name"=w) and "cache" (backend, team=t1, a=xy).
func fx(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // fixed fixtures
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func filterFor(t *testing.T, v2 bool, ts ...policy.Term) *TraceFilter {
	t.Helper()
	f, err := NewTraceFilter(eff(nil, ts...), TraceFilterOptions{V2: v2})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// jsonSpanNames returns the sorted span names of a v1 ("batches") or v2 ("trace") JSON body.
func jsonSpanNames(t *testing.T, body []byte) []string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if tr, ok := doc["trace"].(map[string]any); ok {
		doc = tr
	}
	key := "batches"
	if _, ok := doc[key]; !ok {
		key = "resourceSpans"
	}
	var names []string
	for _, rs := range doc[key].([]any) {
		scopes := rs.(map[string]any)["scopeSpans"].([]any)
		if len(scopes) == 0 {
			t.Error("empty scopeSpans kept")
		}
		for _, ss := range scopes {
			spans := ss.(map[string]any)["spans"].([]any)
			if len(spans) == 0 {
				t.Error("empty spans kept")
			}
			for _, sp := range spans {
				names = append(names, sp.(map[string]any)["name"].(string))
			}
		}
	}
	slices.Sort(names)
	return names
}

func protoSpanNames(t *testing.T, body []byte, v2 bool) []string {
	t.Helper()
	if v2 {
		for b := body; len(b) > 0; {
			num, typ, n := protowire.ConsumeTag(b)
			m := protowire.ConsumeFieldValue(num, typ, b[n:])
			if num == 1 {
				inner, _ := protowire.ConsumeBytes(b[n:])
				return protoSpanNames(t, inner, false)
			}
			b = b[n+m:]
		}
		return nil
	}
	var names []string
	for b := body; len(b) > 0; {
		num, typ, n := protowire.ConsumeTag(b)
		m := protowire.ConsumeFieldValue(num, typ, b[n:])
		if num == 1 {
			raw, _ := protowire.ConsumeBytes(b[n:])
			var rs tracev1.ResourceSpans
			if err := rs.Unmarshal(raw); err != nil {
				t.Fatal(err)
			}
			if len(rs.ScopeSpans) == 0 {
				t.Error("empty ScopeSpans kept")
			}
			for _, ss := range rs.ScopeSpans {
				if len(ss.Spans) == 0 {
					t.Error("empty spans kept")
				}
				for _, sp := range ss.Spans {
					names = append(names, sp.Name)
				}
			}
		}
		b = b[n+m:]
	}
	slices.Sort(names)
	return names
}

func TestFilterSemantics(t *testing.T) {
	tests := []struct {
		name string
		term policy.Term
		want []string // sorted span names; nil = no span remains
	}{
		{"resource equal", term(m("resource.service.name", v1.MatchEqual, "backend")), []string{"cache", "db query"}},
		{"span equal", term(m("span.a", v1.MatchEqual, "x")), []string{"GET /"}},
		{"unscoped finds resource attribute", term(m(".team", v1.MatchEqual, "t1")), []string{"cache", "db query"}},
		{"unscoped finds span attribute", term(m(".a", v1.MatchEqual, "y")), []string{"db query"}},
		{"wrong scope", term(m("resource.a", v1.MatchEqual, "x")), nil},
		{"and", term(m("resource.service.name", v1.MatchEqual, "backend"), m("span.a", v1.MatchEqual, "y")), []string{"db query"}},
		{"regex anchored", term(m("span.a", v1.MatchRegexMatch, "x")), []string{"GET /"}},
		{"regex alternation anchored as a whole", term(m("span.a", v1.MatchRegexMatch, "x|y")), []string{"GET /", "db query"}},
		{"regex wildcard", term(m("span.a", v1.MatchRegexMatch, "x.*")), []string{"GET /", "cache"}},
		{"not equal", term(m("span.a", v1.MatchNotEqual, "x")), []string{"cache", "db query"}},
		{"not regex", term(m("span.a", v1.MatchRegexNoMatch, "x")), []string{"cache", "db query"}},
		{"not regex alternation", term(m("span.a", v1.MatchRegexNoMatch, "x|y")), []string{"cache"}},
		// Missing attribute: no operator is satisfied (TraceQL behaviour verified on Tempo 2.9.0).
		{"missing not equal", term(m(`span.weird name`, v1.MatchNotEqual, "zzz")), []string{"db query"}},
		{"missing not regex", term(m(`span.weird name`, v1.MatchRegexNoMatch, "zzz")), []string{"db query"}},
		{"missing equal empty", term(m(`span.weird name`, v1.MatchEqual, "")), nil},
		{"missing regex empty", term(m(`span.weird name`, v1.MatchRegexMatch, "")), nil},
		{"missing regex wildcard", term(m(`span.weird name`, v1.MatchRegexMatch, ".*")), []string{"db query"}},
		{"present not empty", term(m(`span.weird name`, v1.MatchNotEqual, "")), []string{"db query"}},
		// An integer attribute never satisfies a string matcher, also not != (verified on Tempo 2.9.0).
		{"int attribute not equal", term(m("span.http.status_code", v1.MatchNotEqual, "x")), nil},
		{"int attribute equal", term(m("span.http.status_code", v1.MatchEqual, "200")), nil},
		{"int attribute regex", term(m("span.http.status_code", v1.MatchRegexMatch, "200")), nil},
	}
	for _, v2 := range []bool{false, true} {
		fixture := map[bool]string{false: "trace1_v1", true: "trace1_v2"}[v2]
		for _, tt := range tests {
			t.Run(fixture+"/"+tt.name, func(t *testing.T) {
				f := filterFor(t, v2, tt.term)
				out, found, err := f.FilterJSON(fx(t, fixture+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if found != (tt.want != nil) {
					t.Fatalf("found = %v, want %v", found, tt.want != nil)
				}
				if found {
					if got := jsonSpanNames(t, out); !slices.Equal(got, tt.want) {
						t.Errorf("json spans = %v, want %v", got, tt.want)
					}
				}
				pout, pfound, err := f.FilterProto(fx(t, fixture+".pb"))
				if err != nil {
					t.Fatal(err)
				}
				if pfound != found {
					t.Fatalf("protobuf found = %v, json found = %v", pfound, found)
				}
				if pfound {
					if got := protoSpanNames(t, pout, v2); !slices.Equal(got, tt.want) {
						t.Errorf("protobuf spans = %v, want %v", got, tt.want)
					}
				}
			})
		}
	}
}

func TestFilterDNFOrAndUnrestricted(t *testing.T) {
	f := filterFor(t, false,
		term(m("span.a", v1.MatchEqual, "x")),
		term(m("resource.service.name", v1.MatchEqual, "backend"), m("span.a", v1.MatchEqual, "xy")))
	out, found, err := f.FilterJSON(fx(t, "trace1_v1.json"))
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if got := jsonSpanNames(t, out); !slices.Equal(got, []string{"GET /", "cache"}) {
		t.Errorf("spans = %v", got)
	}

	u, err := NewTraceFilter(policy.Effective{Unrestricted: true}, TraceFilterOptions{})
	if err != nil || !u.Passthrough() {
		t.Fatalf("unrestricted: %v %v", u, err)
	}
	all, err := NewTraceFilter(eff(nil, term()), TraceFilterOptions{})
	if err != nil || !all.Passthrough() {
		t.Fatalf("empty term: %v", err)
	}
	if got := jsonSpanNames(t, mustFilterJSON(t, all, fx(t, "trace1_v1.json"))); len(got) != 3 {
		t.Errorf("empty term dropped spans: %v", got)
	}
}

func mustFilterJSON(t *testing.T, f *TraceFilter, b []byte) []byte {
	t.Helper()
	out, found, err := f.FilterJSON(b)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	return out
}

func TestNewTraceFilterErrors(t *testing.T) {
	bad := []policy.Effective{
		{},
		eff(nil, policy.Term{Tenant: "t1"}),
		eff(nil, term(m(policy.TenantIDLabel, v1.MatchEqual, "t1"))),
		eff(nil, term(m("noscope", v1.MatchEqual, "x"))),
		eff(nil, term(m("event.x", v1.MatchEqual, "x"))),
		eff(nil, term(m("span.x", v1.MatchRegexMatch, "("))),
		eff(nil, term(m("span.x", v1.MatchOp("Bogus"), "x"))),
	}
	for i, e := range bad {
		if _, err := NewTraceFilter(e, TraceFilterOptions{}); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestDeniedAttributesStripped(t *testing.T) {
	f, err := NewTraceFilter(eff([]string{"resource.team", "span.weird name"}, term(m("resource.service.name", v1.MatchEqual, "backend"))), TraceFilterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := mustFilterJSON(t, f, fx(t, "trace1_v1.json"))
	for _, leak := range []string{`"team"`, "weird name", "t1"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("denied attribute %q still present", leak)
		}
	}
	if !strings.Contains(string(out), "service.name") || !strings.Contains(string(out), "dash-name") {
		t.Errorf("permitted attributes lost: %s", out)
	}
	pout, found, err := f.FilterProto(fx(t, "trace1_v1.pb"))
	if err != nil || !found {
		t.Fatal(found, err)
	}
	for _, leak := range []string{"team", "weird name"} {
		if bytes.Contains(pout, []byte(leak)) {
			t.Errorf("denied attribute %q still in protobuf", leak)
		}
	}
	// Unrestricted with denied attributes still filters.
	u, _ := NewTraceFilter(policy.Effective{Unrestricted: true, DeniedLabels: []string{"span.a"}}, TraceFilterOptions{})
	if u.Passthrough() {
		t.Error("denied attributes must disable passthrough")
	}
}

func kv(k, v string) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}}
}

func TestDuplicateAndBothScopes(t *testing.T) {
	rs := &tracev1.ResourceSpans{
		Resource: &resourcev1.Resource{Attributes: []*commonv1.KeyValue{kv("x", "r"), kv("dup", "v"), kv("dup", "v")}},
		ScopeSpans: []*tracev1.ScopeSpans{{Spans: []*tracev1.Span{
			{Name: "both", Attributes: []*commonv1.KeyValue{kv("x", "s")}},
			{Name: "res-only"},
		}}},
	}
	enc, err := rs.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	body := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), enc)

	run := func(mt policy.Matcher) []string {
		f := filterFor(t, false, term(mt))
		out, found, err := f.FilterProto(body)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			return nil
		}
		return protoSpanNames(t, out, false)
	}
	// Unscoped, present in both scopes: positive operators need one value to match ...
	if got := run(m(".x", v1.MatchEqual, "s")); !slices.Equal(got, []string{"both"}) {
		t.Errorf("unscoped equal = %v", got)
	}
	// ... negative operators need every present value to pass.
	if got := run(m(".x", v1.MatchNotEqual, "s")); !slices.Equal(got, []string{"res-only"}) {
		t.Errorf("unscoped not equal = %v", got)
	}
	// A duplicated key never matches, not even a negative operator.
	if got := run(m("resource.dup", v1.MatchNotEqual, "zzz")); got != nil {
		t.Errorf("duplicate key matched: %v", got)
	}
	if got := run(m("resource.dup", v1.MatchEqual, "v")); got != nil {
		t.Errorf("duplicate key matched: %v", got)
	}
}

func TestFilterMalformed(t *testing.T) {
	f := filterFor(t, false, term(m("span.a", v1.MatchEqual, "x")))
	f2 := filterFor(t, true, term(m("span.a", v1.MatchEqual, "x")))
	for _, bad := range [][]byte{[]byte("{"), []byte(`[]`), []byte(`{"batches":{}}`), []byte(`{"batches":[{"scopeSpans":[{"spans":"x"}]}]}`),
		[]byte(`{"batches":[{"instrumentationLibrarySpans":[]}]}`), []byte(`{"batches":[],"resourceSpans":[]}`), []byte(`{"trace":1}`)} {
		if _, found, err := f.FilterJSON(bad); err == nil || found {
			t.Errorf("%s accepted", bad)
		}
	}
	for _, bad := range [][]byte{{0xff}, {0x0a, 0x05, 0x01}, {0x0a, 0x02, 0xff, 0xff}} {
		if _, found, err := f.FilterProto(bad); err == nil || found {
			t.Errorf("proto % x accepted", bad)
		}
		if _, found, err := f2.FilterProto(bad); err == nil || found {
			t.Errorf("proto v2 % x accepted", bad)
		}
	}
	// Tempo's empty answers.
	for _, empty := range []string{`{}`, `{"trace":{}}`, `{"trace":{},"metrics":{}}`} {
		if _, found, err := f.FilterJSON([]byte(empty)); err != nil || found {
			t.Errorf("%s: found %v err %v", empty, found, err)
		}
	}
	if _, found, err := f.FilterProto(nil); err != nil || found {
		t.Errorf("empty protobuf: %v %v", found, err)
	}
}

func TestV2KeepsOtherFields(t *testing.T) {
	f := filterFor(t, true, term(m("span.a", v1.MatchEqual, "x")))
	body := fx(t, "trace1_v2.pb")
	out, found, err := f.FilterProto(body)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	// The recorded response carries metrics (field 2); it must survive.
	if !bytes.Contains(rawFields(t, out, 2), rawFields(t, body, 2)) || len(rawFields(t, out, 2)) == 0 {
		t.Error("metrics field not preserved")
	}
	jout := mustFilterJSON(t, f, fx(t, "trace1_v2.json"))
	if !strings.Contains(string(jout), `"metrics"`) {
		t.Error("json metrics dropped")
	}
}

func rawFields(t *testing.T, b []byte, field protowire.Number) []byte {
	t.Helper()
	var out []byte
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		m := protowire.ConsumeFieldValue(num, typ, b[n:])
		if num == field {
			out = append(out, b[n:n+m]...)
		}
		b = b[n+m:]
	}
	return out
}

func resp(status int, ct, enc string, body []byte) *http.Response {
	h := http.Header{}
	if ct != "" {
		h.Set("Content-Type", ct)
	}
	if enc != "" {
		h.Set("Content-Encoding", enc)
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: h, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}

func readAll(t *testing.T, r *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestModifyResponse(t *testing.T) {
	backend := term(m("resource.service.name", v1.MatchEqual, "backend"))
	nothing := term(m("resource.service.name", v1.MatchEqual, "nothing"))

	t.Run("json keeps format", func(t *testing.T) {
		r := resp(200, "application/json", "", fx(t, "trace1_v1.json"))
		if err := filterFor(t, false, backend).ModifyResponse(r); err != nil {
			t.Fatal(err)
		}
		b := readAll(t, r)
		if r.StatusCode != 200 || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Content-Length") != itoa(len(b)) || r.ContentLength != int64(len(b)) {
			t.Errorf("response: %d %v", r.StatusCode, r.Header)
		}
		if got := jsonSpanNames(t, b); !slices.Equal(got, []string{"cache", "db query"}) {
			t.Errorf("spans = %v", got)
		}
	})
	t.Run("protobuf v1 and v2 keep format", func(t *testing.T) {
		for _, v2 := range []bool{false, true} {
			name := map[bool]string{false: "trace1_v1.pb", true: "trace1_v2.pb"}[v2]
			r := resp(200, "application/protobuf", "", fx(t, name))
			if err := filterFor(t, v2, backend).ModifyResponse(r); err != nil {
				t.Fatal(err)
			}
			if r.Header.Get("Content-Type") != "application/protobuf" {
				t.Errorf("content type %q", r.Header.Get("Content-Type"))
			}
			if got := protoSpanNames(t, readAll(t, r), v2); !slices.Equal(got, []string{"cache", "db query"}) {
				t.Errorf("v2=%v spans = %v", v2, got)
			}
		}
	})
	t.Run("gzip is decoded", func(t *testing.T) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(fx(t, "trace1_v1.json"))
		_ = zw.Close()
		r := resp(200, "application/json; charset=utf-8", "gzip", buf.Bytes())
		if err := filterFor(t, false, backend).ModifyResponse(r); err != nil {
			t.Fatal(err)
		}
		if r.Header.Get("Content-Encoding") != "" || len(jsonSpanNames(t, readAll(t, r))) != 2 {
			t.Error("gzip response not rewritten")
		}
	})
	t.Run("nothing left is 404", func(t *testing.T) {
		for _, c := range []struct {
			ct   string
			body []byte
			v2   bool
		}{{"application/json", fx(t, "trace1_v1.json"), false}, {"application/json", fx(t, "trace1_v2.json"), true},
			{"application/protobuf", fx(t, "trace1_v1.pb"), false}, {"application/protobuf", fx(t, "trace1_v2.pb"), true}} {
			r := resp(200, c.ct, "", c.body)
			if err := filterFor(t, c.v2, nothing).ModifyResponse(r); err != nil {
				t.Fatal(err)
			}
			if b := readAll(t, r); r.StatusCode != 404 || len(b) != 0 || r.ContentLength != 0 || r.Header.Get("Content-Type") != "" || !strings.HasPrefix(r.Status, "404") {
				t.Errorf("%s: %d %q %v", c.ct, r.StatusCode, b, r.Header)
			}
		}
	})
	t.Run("other statuses pass through", func(t *testing.T) {
		r := resp(404, "text/plain", "", []byte("trace not found"))
		if err := filterFor(t, false, backend).ModifyResponse(r); err != nil || string(readAll(t, r)) != "trace not found" {
			t.Error("404 changed")
		}
	})
	t.Run("errors fail closed", func(t *testing.T) {
		f := filterFor(t, false, backend)
		for _, r := range []*http.Response{
			resp(200, "text/html", "", []byte("<html>")),
			resp(200, "application/vnd.grafana.llm", "", []byte("{}")),
			resp(200, "", "", fx(t, "trace1_v1.json")),
			resp(200, "application/json", "br", fx(t, "trace1_v1.json")),
			resp(200, "application/json", "gzip", []byte("not gzip")),
			resp(200, "application/json", "", []byte("{")),
			resp(200, "application/protobuf", "", []byte{0xff}),
		} {
			if err := f.ModifyResponse(r); err == nil {
				t.Errorf("response %v accepted", r.Header)
			}
		}
	})
	t.Run("size cap", func(t *testing.T) {
		f, _ := NewTraceFilter(eff(nil, backend), TraceFilterOptions{MaxBytes: 100})
		if err := f.ModifyResponse(resp(200, "application/json", "", fx(t, "trace1_v1.json"))); err == nil {
			t.Error("over-cap body accepted")
		}
	})
	t.Run("passthrough untouched", func(t *testing.T) {
		u, _ := NewTraceFilter(policy.Effective{Unrestricted: true}, TraceFilterOptions{})
		r := resp(200, "text/html", "", []byte("x"))
		if err := u.ModifyResponse(r); err != nil || string(readAll(t, r)) != "x" {
			t.Error("passthrough modified response")
		}
	})
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestPlanTraceByID(t *testing.T) {
	restricted := eff(nil, term(m("resource.ns", v1.MatchEqual, "a")))
	filter := NewWithOptions(Options{TraceByID: config.TraceByIDFilter})
	ep1 := ep("/api/traces/0123456789abcdef0123456789abcdef")
	ep2 := ep("/api/v2/traces/abcdef")
	if ep1.Kind == 0 || ep2.Template != "/api/v2/traces/:id" {
		t.Fatalf("routes: %+v %+v", ep1, ep2)
	}

	// deny is the default and the explicit mode
	for _, e := range []enforce.Enforcer{New(), NewWithOptions(Options{TraceByID: config.TraceByIDDeny})} {
		_, err := e.Plan(t.Context(), ep1, url.Values{}, restricted)
		if !errors.Is(err, enforce.ErrDeniedLabel) || enforce.HTTPStatus(err) != 403 {
			t.Errorf("deny: %v", err)
		}
	}

	// filter: single request, start/end forwarded, no merge
	p, err := filter.Plan(t.Context(), ep2, url.Values{"start": {"1"}, "end": {"2"}}, restricted)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Requests) != 1 || p.Merge != enforce.MergeNone || p.Strategy != "filter" || p.Requests[0].Term != -1 ||
		p.Requests[0].Query.Get("start") != "1" || p.Requests[0].Query.Get("end") != "2" || len(p.Requests[0].Query) != 2 {
		t.Errorf("plan = %+v", p)
	}
	// unrestricted without denied labels needs no filtering
	p, err = filter.Plan(t.Context(), ep1, nil, policy.Effective{Unrestricted: true})
	if err != nil || p.Strategy != "passthrough" {
		t.Errorf("unrestricted: %+v %v", p, err)
	}
	p, err = filter.Plan(t.Context(), ep1, nil, policy.Effective{Unrestricted: true, DeniedLabels: []string{"span.x"}})
	if err != nil || p.Strategy != "filter" {
		t.Errorf("unrestricted with denied: %+v %v", p, err)
	}
	// client q and other parameters are rejected
	for _, k := range []string{"q", "keep_hierarchy", "match_depth"} {
		_, err := filter.Plan(t.Context(), ep2, url.Values{k: {"{}"}}, restricted)
		if !errors.Is(err, enforce.ErrUnsupported) {
			t.Errorf("param %s: %v", k, err)
		}
	}
	// policies Tempo cannot express
	if _, err := filter.Plan(t.Context(), ep1, nil, eff(nil, policy.Term{Tenant: "t"})); !errors.Is(err, enforce.ErrUnsupported) {
		t.Errorf("tenant term: %v", err)
	}
}
