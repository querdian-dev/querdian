package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/proxy"
	"github.com/querdian-dev/querdian/internal/routes"
)

func get(t testing.TB, srv *httptest.Server, path, token string, hdr ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func errBody(t testing.TB, body string) proxy.ErrorBody {
	t.Helper()
	var e proxy.ErrorBody
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("not an error document: %q", body)
	}
	return e
}

func TestNotReadyIs503(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(h.server().ProxyHandler())
	defer srv.Close()
	resp, body := get(t, srv, "/prom/api/v1/query?query=up", h.issuer.token(t, "u"))
	if resp.StatusCode != 503 || errBody(t, body).ErrorType != "unavailable" || h.metricsUp.count() != 0 {
		t.Fatalf("got %d %s (upstream hits %d)", resp.StatusCode, body, h.metricsUp.count())
	}
}

func TestUnrestrictedPolicyIsProxiedWithTenantHeader(t *testing.T) {
	h := newHarness(t)
	h.policies(clusterPolicy("admins", v1.Subjects{Groups: []string{"admins"}}, unrestricted("metrics"), unrestricted("logs", "t1", "t2")))
	srv := httptest.NewServer(h.server().ProxyHandler())
	defer srv.Close()
	tok := h.issuer.token(t, "alice", "admins")

	resp, body := get(t, srv, "/prom/api/v1/query?query=up", tok, "X-Scope-OrgID", "evil")
	if resp.StatusCode != 200 || !strings.Contains(body, "success") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	up := h.metricsUp.last(t)
	if up.URL.Query().Get("query") != "up" || up.Header.Get("Authorization") != "" || up.Header.Get("X-Scope-OrgID") != "" {
		t.Fatalf("unexpected upstream request: %v %v", up.URL, up.Header)
	}

	resp, _ = get(t, srv, "/loki/loki/api/v1/labels", tok)
	if resp.StatusCode != 200 {
		t.Fatalf("logs: %d", resp.StatusCode)
	}
	if got := h.logsUp.last(t).Header.Get("X-Scope-OrgID"); got != "t1|t2" {
		t.Fatalf("tenant header = %q, want t1|t2", got)
	}
}

func TestDenials(t *testing.T) {
	h := newHarness(t)
	h.policies(clusterPolicy("metrics-only", v1.Subjects{Groups: []string{"team"}}, unrestricted("metrics")))
	srv := httptest.NewServer(h.server().ProxyHandler())
	defer srv.Close()
	tok := h.issuer.token(t, "bob", "team")
	other := h.issuer.token(t, "carol", "nobody")

	tests := []struct {
		name, path, token string
		status            int
		errType, reason   string
	}{
		{"no token", "/prom/api/v1/query?query=up", "", 401, "unauthorized", ""},
		{"garbage token", "/prom/api/v1/query?query=up", "garbage", 401, "unauthorized", ""},
		{"no grant for user", "/prom/api/v1/query?query=up", other, 403, "forbidden", "no grant"},
		{"no grant on other backend", "/loki/loki/api/v1/labels", tok, 403, "forbidden", "no policy"},
		{"unknown backend prefix", "/nope/api/v1/query", tok, 403, "forbidden", "endpoint not allowed"},
		{"unknown backend prefix without auth", "/nope/api/v1/query", "", 403, "forbidden", "endpoint not allowed"},
		{"denied endpoint", "/prom/api/v1/admin/tsdb/delete_series", tok, 403, "forbidden", "endpoint not allowed"},
		{"denied endpoint without auth", "/prom/api/v1/rules", "", 403, "forbidden", "endpoint not allowed"},
		{"non-canonical path", "/prom//api/v1/query?query=up", tok, 400, "bad_data", "non-canonical"},
		{"escaped slash", "/prom/api%2Fv1/query?query=up", tok, 400, "bad_data", "escape"},
		{"unknown parameter", "/prom/api/v1/query?query=up&storeMatch[]=x", tok, 400, "bad_data", "storeMatch"},
		{"duplicate query", "/prom/api/v1/query?query=a&query=b", tok, 400, "bad_data", "at most once"},
		{"safe endpoint needs auth", "/prom/api/v1/status/buildinfo", "", 401, "unauthorized", ""},
		{"safe endpoint needs grant", "/prom/api/v1/status/buildinfo", other, 403, "forbidden", "no grant"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := h.metricsUp.count() + h.logsUp.count()
			resp, body := get(t, srv, tc.path, tc.token)
			e := errBody(t, body)
			if resp.StatusCode != tc.status || e.ErrorType != tc.errType || !strings.Contains(e.Error+tc.reason, tc.reason) || e.Status != "error" {
				t.Fatalf("got %d %s, want %d %s %q", resp.StatusCode, body, tc.status, tc.errType, tc.reason)
			}
			if h.metricsUp.count()+h.logsUp.count() != before {
				t.Fatal("a rejected request reached an upstream")
			}
		})
	}
	resp, _ := get(t, srv, "/prom/api/v1/status/buildinfo", tok)
	if resp.StatusCode != 200 {
		t.Fatalf("safe endpoint with grant: %d", resp.StatusCode)
	}
	if got := testutil.ToFloat64(h.metrics.AuthFailures.WithLabelValues("missing_token")); got < 2 {
		t.Fatalf("missing_token failures = %v", got)
	}
}

// fakeEnforcer records its input and injects a namespace matcher.
type fakeEnforcer struct {
	calls int
	err   error
	eff   policy.Effective
}

func (f *fakeEnforcer) Plan(_ context.Context, ep routes.Endpoint, params url.Values, eff policy.Effective) (enforce.Plan, error) {
	f.calls++
	f.eff = eff
	if f.err != nil {
		return enforce.Plan{}, f.err
	}
	q := url.Values{}
	for k, v := range params {
		q[k] = v
	}
	q.Set(ep.Param, `up{namespace="a"}`)
	return enforce.Plan{Requests: []enforce.Upstream{{Query: q, Term: -1}}, Merge: enforce.MergeNone, Strategy: "inject"}, nil
}

func TestEnforcement(t *testing.T) {
	h := newHarness(t)
	h.policies(
		clusterPolicy("restricted", v1.Subjects{Groups: []string{"dev"}},
			v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{{Name: "namespace", Op: v1.MatchEqual, Value: "a"}}}),
		clusterPolicy("deny-labels", v1.Subjects{Groups: []string{"ops"}},
			v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true, Labels: &v1.LabelFilter{Deny: []string{"secret"}}}),
	)
	dev, ops := h.issuer.token(t, "d", "dev"), h.issuer.token(t, "o", "ops")

	t.Run("restricted grant without enforcer fails closed", func(t *testing.T) {
		srv := httptest.NewServer(h.server().ProxyHandler())
		defer srv.Close()
		before := h.metricsUp.count()
		resp, body := get(t, srv, "/prom/api/v1/query?query=up", dev)
		if resp.StatusCode != 503 || errBody(t, body).ErrorType != "unavailable" || h.metricsUp.count() != before {
			t.Fatalf("got %d %s", resp.StatusCode, body)
		}
	})

	fe := &fakeEnforcer{}
	srv := httptest.NewServer(h.server(func(d *Deps) {
		d.Enforcers = map[config.BackendType]enforce.Enforcer{config.BackendPrometheus: fe}
	}).ProxyHandler())
	defer srv.Close()

	t.Run("rewritten query reaches upstream", func(t *testing.T) {
		resp, _ := get(t, srv, "/prom/api/v1/query?query=original", dev)
		if resp.StatusCode != 200 || h.metricsUp.last(t).URL.Query().Get("query") != `up{namespace="a"}` || fe.calls != 1 {
			t.Fatalf("status %d, upstream query %q, calls %d", resp.StatusCode, h.metricsUp.last(t).URL.Query().Get("query"), fe.calls)
		}
		if fe.eff.Unrestricted || len(fe.eff.Terms) != 1 {
			t.Fatalf("enforcer got effective policy %+v", fe.eff)
		}
	})

	t.Run("unrestricted without denied labels bypasses the enforcer", func(t *testing.T) {
		h2 := newHarness(t)
		h2.policies(clusterPolicy("all", v1.Subjects{Groups: []string{"ops"}}, unrestricted("metrics")))
		fe2 := &fakeEnforcer{}
		srv2 := httptest.NewServer(h2.server(func(d *Deps) {
			d.Enforcers = map[config.BackendType]enforce.Enforcer{config.BackendPrometheus: fe2}
		}).ProxyHandler())
		defer srv2.Close()
		resp, _ := get(t, srv2, "/prom/api/v1/query?query=raw", h2.issuer.token(t, "o", "ops"))
		if resp.StatusCode != 200 || fe2.calls != 0 || h2.metricsUp.last(t).URL.Query().Get("query") != "raw" {
			t.Fatalf("status %d calls %d", resp.StatusCode, fe2.calls)
		}
	})

	t.Run("unrestricted with denied labels still enforced", func(t *testing.T) {
		before := fe.calls
		resp, _ := get(t, srv, "/prom/api/v1/query?query=up", ops)
		if resp.StatusCode != 200 || fe.calls != before+1 || len(fe.eff.DeniedLabels) != 1 {
			t.Fatalf("status %d calls %d eff %+v", resp.StatusCode, fe.calls, fe.eff)
		}
	})

	for _, tc := range []struct {
		name   string
		err    error
		status int
		typ    string
	}{
		{"parse error", enforce.Errorf(enforce.ErrParse, "bad query"), 400, "bad_data"},
		{"denied label", enforce.Errorf(enforce.ErrDeniedLabel, "label secret denied by policy"), 403, "forbidden"},
		{"unsupported", enforce.Errorf(enforce.ErrUnsupported, "nope"), 400, "bad_data"},
		{"too complex", enforce.ErrTooComplex, 403, "forbidden"},
		{"unexpected error", errors.New("boom"), 500, "internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe.err = tc.err
			defer func() { fe.err = nil }()
			before := h.metricsUp.count()
			resp, body := get(t, srv, "/prom/api/v1/query?query=up", dev)
			if resp.StatusCode != tc.status || errBody(t, body).ErrorType != tc.typ || h.metricsUp.count() != before {
				t.Fatalf("got %d %s", resp.StatusCode, body)
			}
			if tc.name == "boom" && strings.Contains(body, "boom") {
				t.Fatal("internal error text leaked")
			}
		})
	}
}

func TestSpansAndMetrics(t *testing.T) {
	h := newHarness(t)
	h.policies(clusterPolicy("all", v1.Subjects{Groups: []string{"g"}}, unrestricted("metrics")))
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	srv := httptest.NewServer(h.server(func(d *Deps) { d.TracerProvider = tp }).ProxyHandler())
	defer srv.Close()

	resp, _ := get(t, srv, "/prom/api/v1/query?query=up", h.issuer.token(t, "u", "g"))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if id := resp.Header.Get(proxy.TraceHeader); len(id) != 32 {
		t.Fatalf("X-Trace-Id = %q", id)
	}
	names := map[string]bool{}
	var server sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		names[s.Name()] = true
		if strings.HasPrefix(s.Name(), "HTTP ") {
			server = s
		}
	}
	for _, want := range []string{"authn.verify", "policy.resolve", "upstream", "HTTP GET /api/v1/query"} {
		if !names[want] {
			t.Errorf("missing span %q; have %v", want, names)
		}
	}
	attrs := map[string]string{}
	for _, a := range server.Attributes() {
		attrs[string(a.Key)] = a.Value.String()
	}
	if attrs["querdian.decision"] != "allow" || attrs["querdian.decision.reason"] != "allowed" || !strings.Contains(attrs["querdian.policies"], "ClusterAccessPolicy/all") {
		t.Fatalf("server span attributes: %v", attrs)
	}
	if _, ok := attrs["enduser.id"]; ok {
		t.Fatal("enduser.id must be absent by default")
	}
	if got := testutil.ToFloat64(h.metrics.Requests.WithLabelValues("metrics", "/api/v1/query", "allow", "allowed")); got != 1 {
		t.Fatalf("requests_total = %v", got)
	}
	if got := testutil.ToFloat64(h.metrics.UpstreamRequests.WithLabelValues("metrics", "200")); got != 1 {
		t.Fatalf("upstream_requests_total = %v", got)
	}

	// A denied request is counted with decision=deny and the error span status.
	get(t, srv, "/prom/api/v1/rules", "")
	if got := testutil.ToFloat64(h.metrics.Requests.WithLabelValues("metrics", "/api/v1/rules", "deny", "endpoint_not_allowed")); got != 1 {
		t.Fatalf("deny counter = %v", got)
	}
}

func TestManagementEndpoints(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(h.server().ManagementHandler())
	defer srv.Close()
	code := func(path string) int {
		resp, _ := get(t, srv, path, "")
		return resp.StatusCode
	}
	if code("/healthz") != 200 {
		t.Fatal("healthz must be 200 even when not ready")
	}
	if code("/readyz") != 503 {
		t.Fatal("readyz must be 503 before the first snapshot")
	}
	h.policies()
	if code("/readyz") != 200 {
		t.Fatal("readyz must be 200 with snapshot and issuer discovery")
	}
	resp, body := get(t, srv, "/metrics", "")
	if resp.StatusCode != 200 || !strings.Contains(body, "querdian_") {
		t.Fatalf("metrics: %d", resp.StatusCode)
	}
	// Not ready when auth is not.
	h2 := newHarness(t)
	h2.policies()
	s2 := New(Deps{Config: h2.cfg, Snapshots: h2.snaps, Auth: notReadyAuth{}})
	srv2 := httptest.NewServer(s2.ManagementHandler())
	defer srv2.Close()
	if resp, _ := get(t, srv2, "/readyz", ""); resp.StatusCode != 503 {
		t.Fatalf("readyz with pending discovery: %d", resp.StatusCode)
	}
}

type notReadyAuth struct{}

func (notReadyAuth) Middleware(next http.Handler) http.Handler { return next }
func (notReadyAuth) Ready() bool                               { return false }

func TestGracefulShutdown(t *testing.T) {
	h := newHarness(t)
	h.policies()
	s := h.server()
	l1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	servers := []*http.Server{{Handler: s.ProxyHandler()}, {Handler: s.ManagementHandler()}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, servers, []net.Listener{l1, l2}, 2*time.Second) }()

	resp, err := http.Get("http://" + l2.Addr().String() + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz before shutdown: %v", err)
	}
	_ = resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean shutdown returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if _, err := http.Get("http://" + l2.Addr().String() + "/healthz"); err == nil {
		t.Fatal("listener still accepting after shutdown")
	}
}
