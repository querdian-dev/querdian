package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/fanout"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
)

// recorder is an upstream that records what it receives.
type recorder struct {
	mu   sync.Mutex
	reqs []recorded
}

type recorded struct {
	Method, Path, RawQuery, Body string
	Header                       http.Header
}

func (rc *recorder) handler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.reqs = append(rc.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Clone()})
		rc.mu.Unlock()
		w.Header().Set("X-Trace-Id", "from-upstream")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func (rc *recorder) last(t *testing.T) recorded {
	t.Helper()
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.reqs) == 0 {
		t.Fatal("upstream received nothing")
	}
	return rc.reqs[len(rc.reqs)-1]
}

var traceCtx = func() context.Context {
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	return trace.ContextWithSpanContext(context.Background(), sc)
}()

func backend(t *testing.T, upstream, tenantHeader string) *config.Backend {
	t.Helper()
	return &config.Backend{Name: "b", Type: config.BackendPrometheus, URL: upstream, PathPrefix: "/prom", NamespaceLabel: "namespace", TenantHeader: tenantHeader}
}

func newProxy() *Proxy {
	return &Proxy{Limits: config.Limits{UpstreamTimeout: config.Duration(5 * time.Second), MaxFanout: 4, MaxMergeBytes: 1 << 20}}
}

func clientRequest(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(traceCtx)
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Header.Set("Authorization", "Bearer secret-token")
	r.Header.Set("Cookie", "session=1")
	r.Header.Set("X-Scope-OrgID", "evil-tenant")
	r.Header.Set("Baggage", "k=v")
	r.Header.Set("Forwarded", "for=6.6.6.6")
	r.Header.Set("X-Forwarded-For", "6.6.6.6")
	r.Header.Set("Traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01")
	r.Header.Set("Accept", "application/json")
	return r
}

func queryEndpoint() routes.Endpoint {
	return routes.Lookup(config.BackendPrometheus, "/api/v1/query", http.MethodGet)
}

func TestStreamStripsClientHeadersAndSetsTenant(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tenantHeader string
		tenants      []string
		wantTenant   string
	}{
		{"single tenant", "X-Scope-OrgID", []string{"t1"}, "t1"},
		{"federation", "X-Scope-OrgID", []string{"t1", "t2"}, "t1|t2"},
		{"custom header", "X-Tenant", []string{"t1"}, "t1"},
		{"single-tenant backend", "", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := &recorder{}
			up := httptest.NewServer(rc.handler(200, `{"status":"success"}`))
			defer up.Close()

			r := clientRequest(http.MethodGet, "/prom/api/v1/query?query=up&time=1", "")
			r.Header.Set("X-Tenant", "evil-tenant")
			w := httptest.NewRecorder()
			plan := enforce.Plan{Requests: []enforce.Upstream{{Query: url.Values{"query": {`up{namespace="a"}`}, "time": {"1"}}, Term: -1}}, Merge: enforce.MergeNone}
			newProxy().Execute(w, r, Target{Backend: backend(t, up.URL, tc.tenantHeader), Endpoint: queryEndpoint(), UpstreamPath: "/api/v1/query", Effective: policy.Effective{Tenants: tc.tenants}}, plan, nil)

			got := rc.last(t)
			for _, h := range []string{"Authorization", "Cookie", "Baggage", "Forwarded"} {
				if v := got.Header.Get(h); v != "" {
					t.Errorf("%s reached upstream: %q", h, v)
				}
			}
			if v := got.Header.Get("X-Forwarded-For"); v == "" || strings.Contains(v, "6.6.6.6") {
				t.Errorf("X-Forwarded-For must be set by the proxy, got %q", v)
			}
			if tc.tenantHeader == "X-Tenant" && got.Header.Get("X-Tenant") != tc.wantTenant {
				t.Errorf("client-supplied configured tenant header must be replaced, got %q", got.Header.Get("X-Tenant"))
			}
			if tc.tenantHeader == "" {
				if v := got.Header.Get("X-Scope-OrgID"); v != "" {
					t.Errorf("client X-Scope-OrgID reached a single-tenant upstream: %q", v)
				}
			} else if v := got.Header.Get(tc.tenantHeader); v != tc.wantTenant {
				t.Errorf("tenant header = %q, want %q", v, tc.wantTenant)
			}
			if tc.tenantHeader != "X-Scope-OrgID" && got.Header.Get("X-Scope-OrgID") != "" {
				t.Errorf("X-Scope-OrgID must never be forwarded from the client: %q", got.Header.Get("X-Scope-OrgID"))
			}
			tp := got.Header.Get("Traceparent")
			if !strings.HasPrefix(tp, "00-4bf92f3577b34da6a3ce929d0e0e4736-") {
				t.Errorf("traceparent = %q, want ours (not the client's)", tp)
			}
			if got.Method != http.MethodGet || got.Path != "/api/v1/query" {
				t.Errorf("unexpected request %s %s", got.Method, got.Path)
			}
			q, _ := url.ParseQuery(got.RawQuery)
			if q.Get("query") != `up{namespace="a"}` || q.Get("time") != "1" {
				t.Errorf("rewritten params not forwarded: %q", got.RawQuery)
			}
			if w.Header().Get(TraceHeader) != "4bf92f3577b34da6a3ce929d0e0e4736" {
				t.Errorf("X-Trace-Id = %q", w.Header().Get(TraceHeader))
			}
			if w.Code != 200 || !strings.Contains(w.Body.String(), "success") {
				t.Errorf("response not streamed back: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestStreamPostForwardedWithEmptyURLQuery(t *testing.T) {
	rc := &recorder{}
	up := httptest.NewServer(rc.handler(200, "{}"))
	defer up.Close()

	r := clientRequest(http.MethodPost, "/prom/api/v1/query", "query=original")
	w := httptest.NewRecorder()
	ep := routes.Lookup(config.BackendPrometheus, "/api/v1/query", http.MethodPost)
	plan := enforce.Plan{Requests: []enforce.Upstream{{Query: url.Values{"query": {`up{namespace="a"}`}}, Term: -1}}}
	newProxy().Execute(w, r, Target{Backend: backend(t, up.URL, ""), Endpoint: ep, UpstreamPath: "/api/v1/query"}, plan, nil)

	got := rc.last(t)
	if got.Method != http.MethodPost || got.RawQuery != "" {
		t.Fatalf("POST must have an empty URL query, got %s %q", got.Method, got.RawQuery)
	}
	form, _ := url.ParseQuery(got.Body)
	if form.Get("query") != `up{namespace="a"}` || strings.Contains(got.Body, "original") {
		t.Fatalf("body = %q", got.Body)
	}
	if ct := got.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestStreamBackendPathPrefixAndUpstreamFailure(t *testing.T) {
	rc := &recorder{}
	up := httptest.NewServer(rc.handler(200, "{}"))
	defer up.Close()
	b := backend(t, up.URL+"/base/", "")
	w := httptest.NewRecorder()
	plan := enforce.Plan{Requests: []enforce.Upstream{{Query: url.Values{"query": {"up"}}, Term: -1}}}
	newProxy().Execute(w, clientRequest(http.MethodGet, "/prom/api/v1/query?query=up", ""), Target{Backend: b, Endpoint: queryEndpoint(), UpstreamPath: "/api/v1/query"}, plan, nil)
	if got := rc.last(t); got.Path != "/base/api/v1/query" {
		t.Fatalf("path = %q", got.Path)
	}

	up.Close() // connection refused -> JSON 502 without leaking the address
	w = httptest.NewRecorder()
	newProxy().Execute(w, clientRequest(http.MethodGet, "/prom/api/v1/query?query=up", ""), Target{Backend: b, Endpoint: queryEndpoint(), UpstreamPath: "/api/v1/query"}, plan, nil)
	var body ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 502 || body.ErrorType != "upstream" ||
		body.Status != "error" || strings.Contains(w.Body.String(), "127.0.0.1") || body.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("unexpected error response %d %s", w.Code, w.Body.String())
	}
}

func TestStreamResponseHeaderTraceIDNotFromUpstream(t *testing.T) {
	rc := &recorder{}
	up := httptest.NewServer(rc.handler(200, "{}"))
	defer up.Close()
	w := httptest.NewRecorder()
	plan := enforce.Plan{Requests: []enforce.Upstream{{Query: url.Values{"query": {"up"}}, Term: -1}}}
	newProxy().Execute(w, clientRequest(http.MethodGet, "/prom/api/v1/query?query=up", ""), Target{Backend: backend(t, up.URL, ""), Endpoint: queryEndpoint(), UpstreamPath: "/api/v1/query"}, plan, nil)
	if got := w.Header().Values(TraceHeader); len(got) != 1 || got[0] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("X-Trace-Id = %v", got)
	}
}

func fanPlan(n int) enforce.Plan {
	p := enforce.Plan{Merge: enforce.MergeLokiStreams}
	for i := 0; i < n; i++ {
		p.Requests = append(p.Requests, enforce.Upstream{Query: url.Values{"query": {"q" + string(rune('0'+i))}}, Term: i})
	}
	return p
}

func concat(w http.ResponseWriter, _ *http.Request, resps []fanout.Response) error {
	for _, r := range resps {
		_, _ = w.Write(r.Body)
	}
	return nil
}

func TestFanoutHeadersAndMerge(t *testing.T) {
	rc := &recorder{}
	up := httptest.NewServer(rc.handler(200, "R"))
	defer up.Close()
	w := httptest.NewRecorder()
	ep := routes.Lookup(config.BackendPrometheus, "/api/v1/query", http.MethodGet)
	newProxy().Execute(w, clientRequest(http.MethodGet, "/prom/api/v1/query?query=x", ""), Target{
		Backend: backend(t, up.URL, "X-Scope-OrgID"), Endpoint: ep, UpstreamPath: "/api/v1/query", Effective: policy.Effective{Tenants: []string{"t1", "t2"}},
	}, fanPlan(3), concat)
	if w.Code != 200 || w.Body.String() != "RRR" {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.reqs) != 3 {
		t.Fatalf("upstream got %d requests", len(rc.reqs))
	}
	for _, g := range rc.reqs {
		if g.Header.Get("X-Scope-OrgID") != "t1|t2" || g.Header.Get("Authorization") != "" || g.Header.Get("Baggage") != "" || g.Header.Get("Cookie") != "" {
			t.Errorf("bad headers on fan-out request: %v", g.Header)
		}
		if g.Header.Get("Accept") != "application/json" {
			t.Errorf("merge plans must force JSON, got %q", g.Header.Get("Accept"))
		}
		if !strings.HasPrefix(g.Header.Get("Traceparent"), "00-4bf92f3577b34da6a3ce929d0e0e4736-") {
			t.Errorf("traceparent = %q", g.Header.Get("Traceparent"))
		}
	}
}

func TestFanoutMergeTooLargeAndBranchFailure(t *testing.T) {
	big := httptest.NewServer((&recorder{}).handler(200, strings.Repeat("x", 600)))
	defer big.Close()
	p := newProxy()
	p.Limits.MaxMergeBytes = 1000
	w := httptest.NewRecorder()
	ep := routes.Lookup(config.BackendPrometheus, "/api/v1/query", http.MethodGet)
	p.Execute(w, clientRequest(http.MethodGet, "/prom/api/v1/query?query=x", ""), Target{Backend: backend(t, big.URL, ""), Endpoint: ep, UpstreamPath: "/api/v1/query"}, fanPlan(2), concat)
	var body ErrorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 502 || body.ErrorType != "merge_too_large" {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}

	bad := httptest.NewServer((&recorder{}).handler(500, "internal details"))
	defer bad.Close()
	w = httptest.NewRecorder()
	newProxy().Execute(w, clientRequest(http.MethodGet, "/prom/api/v1/query?query=x", ""), Target{Backend: backend(t, bad.URL, ""), Endpoint: ep, UpstreamPath: "/api/v1/query"}, fanPlan(2), concat)
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 502 || body.ErrorType != "upstream" {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	newProxy().Execute(w, clientRequest(http.MethodGet, "/prom/api/v1/query?query=x", ""), Target{Backend: backend(t, bad.URL, ""), Endpoint: ep, UpstreamPath: "/api/v1/query"}, fanPlan(2), nil)
	if w.Code != 502 {
		t.Fatalf("fan-out without merger must fail closed, got %d", w.Code)
	}
}

func TestUnavailableAndWriteError(t *testing.T) {
	w := httptest.NewRecorder()
	Unavailable(w, clientRequest(http.MethodGet, "/x", ""))
	var body ErrorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 503 || body.ErrorType != "unavailable" || body.Status != "error" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	// Without a span the trace ID is empty and no header is set.
	w = httptest.NewRecorder()
	WriteError(w, httptest.NewRequest(http.MethodGet, "/x", nil), 400, "bad_data", "m")
	if w.Header().Get(TraceHeader) != "" || !strings.Contains(w.Body.String(), `"traceId":""`) {
		t.Fatalf("got %v %s", w.Header(), w.Body.String())
	}
}
