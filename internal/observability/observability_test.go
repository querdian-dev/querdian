package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/querdian-dev/querdian/internal/observability"
)

func newTP(t *testing.T) (*sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp, err := observability.NewTracerProvider(exp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp, exp
}

func TestPropagation(t *testing.T) {
	tp, exp := newTP(t)
	prop := observability.NewPropagator()

	var gotTraceparent, gotBaggage string
	var gotBaggageInHandler string
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		gotBaggage = r.Header.Get("baggage")
	}))
	defer upstream.Close()

	client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport,
		otelhttp.WithTracerProvider(tp), otelhttp.WithPropagators(prop))}

	proxy := otelhttp.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBaggageInHandler = baggage.FromContext(r.Context()).String()
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		_ = resp.Body.Close()
		w.WriteHeader(http.StatusOK)
	}), "proxy", otelhttp.WithTracerProvider(tp), otelhttp.WithPropagators(prop))

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	req.Header.Set("baggage", "secret=1")
	proxy.ServeHTTP(httptest.NewRecorder(), req)

	if gotBaggageInHandler != "secret=1" {
		t.Errorf("inbound baggage not extracted: %q", gotBaggageInHandler)
	}
	if gotTraceparent == "" {
		t.Fatal("outbound request has no traceparent")
	}
	if gotBaggage != "" {
		t.Errorf("outbound request leaked baggage: %q", gotBaggage)
	}
	spans := flushed(tp, exp)
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	for _, s := range spans {
		if s.SpanContext.TraceID().String() != traceID {
			t.Errorf("span %q not in inbound trace: %s", s.Name, s.SpanContext.TraceID())
		}
	}
}

func TestPrivacyDefaultsRecordNothing(t *testing.T) {
	tp, exp := newTP(t)
	_, span := tp.Tracer("t").Start(context.Background(), "s")
	var p observability.Privacy
	p.SetUser(span, "alice")
	p.SetQueries(span, "up", `up{a="b"}`)
	span.End()
	for _, s := range flushed(tp, exp) {
		for _, a := range s.Attributes {
			if a.Key == observability.AttrEndUserID || a.Key == observability.AttrQueryOriginal || a.Key == observability.AttrQueryRewritten {
				t.Errorf("unexpected attribute %s with default flags", a.Key)
			}
		}
	}
}

func TestPrivacyEnabled(t *testing.T) {
	tp, exp := newTP(t)
	_, span := tp.Tracer("t").Start(context.Background(), "s")
	p := observability.Privacy{RecordUser: true, RecordQueries: true}
	p.SetUser(span, "alice")
	p.SetQueries(span, "up", "up{a=\"b\"}")
	span.End()
	got := map[attribute.Key]string{}
	for _, a := range flushed(tp, exp)[0].Attributes {
		got[a.Key] = a.Value.AsString()
	}
	if got[observability.AttrEndUserID] != "alice" || got[observability.AttrQueryOriginal] != "up" ||
		got[observability.AttrQueryRewritten] != "up{a=\"b\"}" {
		t.Errorf("attributes = %v", got)
	}
}

func TestNewSpanExporterDefaultsToNone(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_TRACES_EXPORTER", "")
	exp, err := observability.NewSpanExporter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := exp.ExportSpans(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := exp.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestExemplarOnRequestHistogram(t *testing.T) {
	tp, _ := newTP(t)
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)

	ctx, span := tp.Tracer("t").Start(context.Background(), "s")
	defer span.End()
	h := m.RequestDuration.WithLabelValues("prom", "query", "allow")
	observability.ObserveWithExemplar(ctx, h, 0.2)

	if n := testutil.CollectAndCount(m.RequestDuration); n != 1 {
		t.Fatalf("collected %d series", n)
	}
	var out dto.Metric
	if err := h.(prometheus.Metric).Write(&out); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range out.GetHistogram().GetBucket() {
		if ex := b.GetExemplar(); ex != nil {
			for _, l := range ex.GetLabel() {
				if l.GetName() == "trace_id" && l.GetValue() == span.SpanContext().TraceID().String() {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("no exemplar with trace_id on request histogram")
	}
	if out.GetHistogram().GetSchema() == 0 && out.GetHistogram().GetZeroThreshold() == 0 && len(out.GetHistogram().GetPositiveSpan()) == 0 {
		t.Error("native histogram not enabled")
	}
}

func TestMetricsRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)
	m.Requests.WithLabelValues("b", "e", "allow", "ok").Inc()
	m.UpstreamRequests.WithLabelValues("b", "200").Inc()
	m.UpstreamDuration.WithLabelValues("b").Observe(1)
	m.RewriteDuration.WithLabelValues("promql", "or_lift").Observe(1)
	m.PolicyTerms.WithLabelValues("b").Observe(2)
	m.AuthFailures.WithLabelValues("expired").Inc()
	m.SnapshotGen.Set(3)
	m.SnapshotRebuilds.WithLabelValues("ok").Inc()
	m.Policies.WithLabelValues("AccessPolicy", "true").Set(1)
	m.RequestDuration.WithLabelValues("b", "e", "allow").Observe(1)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, mf := range mfs {
		have[mf.GetName()] = true
	}
	for _, n := range []string{
		"querdian_requests_total", "querdian_request_duration_seconds", "querdian_upstream_requests_total",
		"querdian_upstream_duration_seconds", "querdian_rewrite_duration_seconds", "querdian_policy_terms",
		"querdian_auth_failures_total", "querdian_snapshot_generation", "querdian_snapshot_rebuilds_total",
		"querdian_policies",
	} {
		if !have[n] {
			t.Errorf("metric %s missing", n)
		}
	}
	if got := testutil.ToFloat64(m.SnapshotGen); got != 3 {
		t.Errorf("generation = %v", got)
	}
}

func TestObserveWithoutSpan(*testing.T) {
	m := observability.NewMetrics(prometheus.NewRegistry())
	observability.ObserveWithExemplar(context.Background(), m.UpstreamDuration.WithLabelValues("b"), 1)
}

func TestTraceHandler(t *testing.T) {
	tp, _ := newTP(t)
	var buf bytes.Buffer
	log := slog.New(observability.NewTraceHandler(slog.NewJSONHandler(&buf, nil))).With("k", "v")

	ctx, span := tp.Tracer("t").Start(context.Background(), "s")
	log.InfoContext(ctx, "traced")
	span.End()
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["trace_id"] != span.SpanContext().TraceID().String() || rec["span_id"] != span.SpanContext().SpanID().String() {
		t.Errorf("record = %v", rec)
	}

	buf.Reset()
	log.WithGroup("g").Info("untraced")
	if bytes.Contains(buf.Bytes(), []byte("trace_id")) {
		t.Errorf("untraced record has trace_id: %s", buf.String())
	}
}

func flushed(tp *sdktrace.TracerProvider, exp *tracetest.InMemoryExporter) tracetest.SpanStubs {
	_ = tp.ForceFlush(context.Background())
	return exp.GetSpans()
}
