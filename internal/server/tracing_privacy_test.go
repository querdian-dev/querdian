package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/querdian-dev/querdian/internal/config"
	enforcelogql "github.com/querdian-dev/querdian/internal/enforce/logql"
	"github.com/querdian-dev/querdian/internal/observability"
)

// secretValue is the matcher value of the policies in these tests. No span attribute may contain it unless
// --tracing.record-queries is set.
const secretValue = "s3cr3t-ns"

// probeTransport is the base upstream transport of the tests. It records the url.full attribute of the
// upstream span at the time when the request leaves the process, and the query string that it sends.
type probeTransport struct {
	next    http.RoundTripper
	mu      sync.Mutex
	urlFull []string
	queries []string
}

func (p *probeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	full := "<no span>"
	if s, ok := trace.SpanFromContext(r.Context()).(sdktrace.ReadOnlySpan); ok {
		full = "<no url.full>"
		for _, a := range s.Attributes() {
			if a.Key == "url.full" {
				full = a.Value.AsString()
			}
		}
	}
	p.mu.Lock()
	p.urlFull = append(p.urlFull, full)
	p.queries = append(p.queries, r.URL.RawQuery)
	p.mu.Unlock()
	return p.next.RoundTrip(r)
}

// tracedServer starts the proxy listener with a span recorder and the probe transport.
func tracedServer(t *testing.T, h *harness, priv observability.Privacy, extra ...func(*Deps)) (*httptest.Server, *tracetest.SpanRecorder, *probeTransport) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	probe := &probeTransport{next: http.DefaultTransport.(*http.Transport).Clone()}
	opts := append([]func(*Deps){allEnforcers, func(d *Deps) {
		d.TracerProvider = tp
		d.Transport = probe
		d.Privacy = priv
		d.TailPlanners = map[config.BackendType]TailPlanner{config.BackendLoki: enforcelogql.PlanTail}
	}}, extra...)
	srv := httptest.NewServer(h.server(opts...).ProxyHandler())
	t.Cleanup(srv.Close)
	return srv, rec, probe
}

// waitServerSpan waits until the server span ends. otelhttp ends it after the handler returns, so the client
// can see the response first.
func waitServerSpan(t *testing.T, rec *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		for _, s := range rec.Ended() {
			if s.SpanKind() == trace.SpanKindServer {
				return s
			}
		}
	}
	t.Fatal("server span never ended")
	return nil
}

// upstreamSpans returns all started client spans, also the spans that did not end yet.
func upstreamSpans(rec *tracetest.SpanRecorder) []sdktrace.ReadWriteSpan {
	var out []sdktrace.ReadWriteSpan
	for _, s := range rec.Started() {
		if s.SpanKind() == trace.SpanKindClient {
			out = append(out, s)
		}
	}
	return out
}

// assertNoQueryText fails if an attribute of a recorded span contains the matcher value or a query string.
// It also checks the url.full value that the upstream span had when the request left the process.
func assertNoQueryText(t *testing.T, rec *tracetest.SpanRecorder, probe *probeTransport, wantUpstream int) {
	t.Helper()
	ups := upstreamSpans(rec)
	if len(ups) != wantUpstream {
		t.Fatalf("got %d upstream spans, want %d", len(ups), wantUpstream)
	}
	for _, s := range rec.Started() {
		for _, a := range s.Attributes() {
			v := a.Value.String()
			if strings.Contains(v, secretValue) || strings.Contains(v, "?") {
				t.Errorf("span %q: attribute %s=%q contains query text", s.Name(), a.Key, v)
			}
		}
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if len(probe.urlFull) != wantUpstream {
		t.Fatalf("probe saw %d upstream requests, want %d", len(probe.urlFull), wantUpstream)
	}
	for i, full := range probe.urlFull {
		if !strings.HasPrefix(full, "http://") || strings.Contains(full, "?") || strings.Contains(full, secretValue) {
			t.Errorf("url.full during the request = %q", full)
		}
		// The upstream must still get the full rewritten query.
		if !strings.Contains(probe.queries[i], secretValue) {
			t.Errorf("upstream query %q does not contain the policy matcher", probe.queries[i])
		}
	}
}

// urlFullValues returns the url.full and http.url values of all upstream spans.
func urlFullValues(rec *tracetest.SpanRecorder) []string {
	var out []string
	for _, s := range upstreamSpans(rec) {
		for _, a := range s.Attributes() {
			if a.Key == "url.full" || a.Key == "http.url" {
				out = append(out, a.Value.AsString())
			}
		}
	}
	return out
}

func promGet(t *testing.T, h *harness, srv *httptest.Server) {
	t.Helper()
	resp, body := get(t, srv, "/prom/api/v1/query?query=up", h.issuer.token(t, "u", "g"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

func lokiFanoutGet(t *testing.T, h *harness, srv *httptest.Server) {
	t.Helper()
	qs := url.Values{"query": {`{job="j"}`}, "limit": {"10"}}
	resp, body := get(t, srv, "/loki/loki/api/v1/query_range?"+qs.Encode(), h.issuer.token(t, "u", "g"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

func tailHandshake(t *testing.T, h *harness, srv *httptest.Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/loki/loki/api/v1/tail?query=%7Bapp%3D%22a%22%7D"
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + h.issuer.token(t, "u", "g")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	if _, msg, err := c.Read(ctx); err != nil || !strings.Contains(string(msg), "hello") {
		t.Fatalf("read: %v %q", err, msg)
	}
}

// privacyHarness returns a harness with restricted policies: one term on metrics, two terms on logs (fan-out)
// and a fake Loki that accepts a tail.
func privacyHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.policies(
		clusterPolicy("p-metrics", v1Subjects("g"), restricted("metrics", nil, nil, match("namespace", secretValue))),
		clusterPolicy("p-logs-ns", v1Subjects("g"), restricted("logs", nil, []string{"t1"}, match("k8s_namespace_name", secretValue))),
		clusterPolicy("p-logs-app", v1Subjects("g"), restricted("logs", nil, []string{"t1"}, match("app", secretValue))),
	)
	h.logsUp.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tail") {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = c.CloseNow() }()
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"streams":[{"stream":{"app":"a"},"values":[["1","hello"]]}]}`))
			_ = c.Close(websocket.StatusNormalClosure, "done")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[],"stats":{}}}`))
	})
	return h
}

func testUpstreamSpansHideQueries(t *testing.T) {
	for name, tc := range map[string]struct {
		do       func(*testing.T, *harness, *httptest.Server)
		upstream int
	}{
		"prometheus GET": {promGet, 1},
		"loki fan-out":   {lokiFanoutGet, 2},
		"loki tail":      {tailHandshake, 2},
	} {
		t.Run(name, func(t *testing.T) {
			h := privacyHarness(t)
			srv, rec, probe := tracedServer(t, h, observability.Privacy{})
			tc.do(t, h, srv)
			waitServerSpan(t, rec)
			assertNoQueryText(t, rec, probe, tc.upstream)
			for _, v := range urlFullValues(rec) {
				if !strings.HasPrefix(v, h.metricsUp.URL+"/") && !strings.HasPrefix(v, h.logsUp.URL+"/") {
					t.Errorf("url.full = %q, want scheme, host and path", v)
				}
			}
		})
	}
}

func testUpstreamSpansRecordQueriesWhenEnabled(t *testing.T) {
	h := privacyHarness(t)
	srv, rec, _ := tracedServer(t, h, observability.Privacy{RecordQueries: true})
	promGet(t, h, srv)
	waitServerSpan(t, rec)
	vals := urlFullValues(rec)
	if len(vals) == 0 {
		t.Fatal("no url.full attribute")
	}
	for _, v := range vals {
		if !strings.Contains(v, "?") || !strings.Contains(v, secretValue) {
			t.Errorf("url.full = %q, want the query string with --tracing.record-queries", v)
		}
	}
}

// By default, no upstream span records the query string: not for a plain GET, a fan-out or a tail handshake.
func TestUpstreamSpansHideQueries(t *testing.T) { testUpstreamSpansHideQueries(t) }

// The same with OTEL_SEMCONV_STABILITY_OPT_IN=http/dup. otelhttp v0.72.0 ignores this variable, but an older or
// newer version can then also set the old http.url attribute. urlFullValues also checks http.url.
func TestUpstreamSpansHideQueriesDupSemconv(t *testing.T) {
	t.Setenv("OTEL_SEMCONV_STABILITY_OPT_IN", "http/dup")
	testUpstreamSpansHideQueries(t)
}

// With --tracing.record-queries, url.full keeps the query string.
func TestUpstreamSpansRecordQueriesWhenEnabled(t *testing.T) {
	testUpstreamSpansRecordQueriesWhenEnabled(t)
}

func TestUpstreamSpansRecordQueriesWhenEnabledDupSemconv(t *testing.T) {
	t.Setenv("OTEL_SEMCONV_STABILITY_OPT_IN", "http/dup")
	testUpstreamSpansRecordQueriesWhenEnabled(t)
}

// A sampled traceparent of a client does not become the parent of the server span unless the operator trusts
// the trace context of clients.
func TestInboundTraceContext(t *testing.T) {
	const (
		remoteTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
		remoteSpan  = "00f067aa0ba902b7"
	)
	for name, trust := range map[string]bool{"default": false, "trusted": true} {
		t.Run(name, func(t *testing.T) {
			h := privacyHarness(t)
			// The default sampler of Querdian: ParentBased, so a sampled parent forces sampling.
			rec := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec),
				sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.NeverSample())))
			srv := httptest.NewServer(h.server(allEnforcers, func(d *Deps) {
				d.TracerProvider = tp
				d.TrustInboundTraceContext = trust
			}).ProxyHandler())
			defer srv.Close()
			resp, _ := get(t, srv, "/prom/api/v1/query?query=up", h.issuer.token(t, "u", "g"),
				"traceparent", "00-"+remoteTrace+"-"+remoteSpan+"-01")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if !trust {
				// The client cannot force sampling: with NeverSample as root sampler, no span is recorded.
				time.Sleep(50 * time.Millisecond)
				if n := len(rec.Started()); n != 0 {
					t.Fatalf("an untrusted traceparent forced sampling of %d spans", n)
				}
				return
			}
			server := waitServerSpan(t, rec)
			if server.Parent().TraceID().String() != remoteTrace || server.Parent().SpanID().String() != remoteSpan {
				t.Fatalf("trusted: parent = %v", server.Parent())
			}
		})
	}
}

// Without trust, the server span starts a new trace and links the trace context of the client.
func TestInboundTraceContextIsLinked(t *testing.T) {
	const remoteTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	h := privacyHarness(t)
	srv, rec, _ := tracedServer(t, h, observability.Privacy{})
	resp, _ := get(t, srv, "/prom/api/v1/query?query=up", h.issuer.token(t, "u", "g"),
		"traceparent", "00-"+remoteTrace+"-00f067aa0ba902b7-01")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	server := waitServerSpan(t, rec)
	if server.Parent().IsValid() || server.SpanContext().TraceID().String() == remoteTrace {
		t.Fatalf("server span continues the client trace: parent=%v trace=%v", server.Parent(), server.SpanContext().TraceID())
	}
	links := server.Links()
	if len(links) != 1 || links[0].SpanContext.TraceID().String() != remoteTrace {
		t.Fatalf("links = %v", links)
	}
	// The upstream gets the new trace, not the trace of the client.
	if tp := h.metricsUp.last(t).Header.Get("traceparent"); strings.Contains(tp, remoteTrace) {
		t.Fatalf("upstream traceparent %q continues the client trace", tp)
	}
}

// The transport pair hides the query only from the layers between them and does not change the request of
// the caller.
func TestHideQueryTransportPair(t *testing.T) {
	var between, below string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		below = r.URL.String()
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
	})
	mid := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		between = r.URL.String()
		return restoreQueryTransport{next: base}.RoundTrip(r)
	})
	rt := hideQueryTransport{next: mid}
	for _, raw := range []string{"http://up:9090/api/v1/query?query=up%7Bns%3D%22a%22%7D", "http://up/x?", "http://up/x"} {
		req, _ := http.NewRequest(http.MethodGet, raw, nil)
		if _, err := rt.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(between, "?") || !strings.HasPrefix(raw, between) {
			t.Errorf("%s: the middle layer saw %q", raw, between)
		}
		if below != raw || req.URL.String() != raw {
			t.Errorf("%s: base saw %q, caller request is now %q", raw, below, req.URL.String())
		}
	}
	// Without the hidden query in the context, restoreQueryTransport sends the request unchanged.
	req, _ := http.NewRequest(http.MethodGet, "http://up/x?a=b", nil)
	if _, err := (restoreQueryTransport{next: base}).RoundTrip(req); err != nil || below != "http://up/x?a=b" {
		t.Fatalf("restore without context: %q %v", below, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
