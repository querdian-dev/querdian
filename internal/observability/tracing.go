// Package observability provides the tracing, metrics and logging foundation
// described in PLAN §11: an OpenTelemetry tracer provider configured through
// standard environment variables, asymmetric context propagation, the
// Prometheus metric registry, a trace-aware slog handler and privacy-gated
// span attribute helpers.
package observability

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// ServiceName is the default service.name used when OTEL_SERVICE_NAME is unset.
const ServiceName = "querdian"

// noopExporter discards all spans.
type noopExporter struct{}

func (noopExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error { return nil }
func (noopExporter) Shutdown(context.Context) error                             { return nil }

// hasOTLPEndpoint reports whether an OTLP endpoint is configured via env.
func hasOTLPEndpoint() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// NewSpanExporter builds the span exporter through autoexport. When neither
// OTEL_TRACES_EXPORTER nor an OTLP endpoint is set, the exporter is "none".
func NewSpanExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	var opts []autoexport.SpanOption
	if !hasOTLPEndpoint() {
		opts = append(opts, autoexport.WithFallbackSpanExporter(
			func(context.Context) (sdktrace.SpanExporter, error) { return noopExporter{}, nil }))
	}
	exp, err := autoexport.NewSpanExporter(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create span exporter: %w", err)
	}
	return exp, nil
}

// NewTracerProvider creates a tracer provider around exp. The sampler follows
// OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG and defaults to
// parentbased_traceidratio with ratio 1.0 (PLAN §11.2). Resource attributes
// come from OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES. Extra options are
// applied last.
func NewTracerProvider(exp sdktrace.SpanExporter, extra ...sdktrace.TracerProviderOption) (*sdktrace.TracerProvider, error) {
	res, err := resource.New(context.Background(),
		resource.WithAttributes(semconv.ServiceName(ServiceName)),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	}
	if os.Getenv("OTEL_TRACES_SAMPLER") == "" {
		opts = append(opts, sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(1.0))))
	}
	opts = append(opts, extra...)
	return sdktrace.NewTracerProvider(opts...), nil
}

// Setup configures the global tracer provider and propagator and returns a
// shutdown function that flushes pending spans.
func Setup(ctx context.Context) (shutdown func(context.Context) error, err error) {
	exp, err := NewSpanExporter(ctx)
	if err != nil {
		return nil, err
	}
	tp, err := NewTracerProvider(exp)
	if err != nil {
		return nil, errors.Join(err, exp.Shutdown(ctx))
	}
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(NewPropagator())
	return tp.Shutdown, nil
}

// propagator extracts W3C trace context and baggage but injects trace context
// only, so client-supplied baggage never leaks to upstreams (PLAN §11.2).
type propagator struct {
	in  propagation.TextMapPropagator
	out propagation.TextMapPropagator
}

// NewPropagator returns the asymmetric propagator: inbound tracecontext and
// baggage, outbound tracecontext only.
func NewPropagator() propagation.TextMapPropagator {
	tc := propagation.TraceContext{}
	return propagator{
		in:  propagation.NewCompositeTextMapPropagator(tc, propagation.Baggage{}),
		out: tc,
	}
}

func (p propagator) Extract(ctx context.Context, c propagation.TextMapCarrier) context.Context {
	return p.in.Extract(ctx, c)
}

func (p propagator) Inject(ctx context.Context, c propagation.TextMapCarrier) {
	p.out.Inject(ctx, c)
}

func (p propagator) Fields() []string { return p.out.Fields() }
