// Package server wires authentication, policy resolution, enforcement and the proxy core into the two HTTP
// servers of Querdian: the proxy listener and the management listener (PLAN §3).
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/querdian-dev/querdian/internal/auth"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/fanout"
	"github.com/querdian-dev/querdian/internal/observability"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/proxy"
)

const tracerName = "github.com/querdian-dev/querdian/internal/server"

// SnapshotProvider supplies the current policy snapshot; *controller.Controller implements it.
type SnapshotProvider interface {
	// Snapshot returns the current snapshot, or nil before the first one exists.
	Snapshot() *policy.Snapshot
}

// Authenticator verifies bearer tokens; *auth.Authenticator implements it. Middleware must put the identity
// into the request context (auth.IdentityFrom), answer failures itself and count them.
type Authenticator interface {
	Middleware(next http.Handler) http.Handler
	Ready() bool
}

// TailPlanner plans a WebSocket tail request (Loki). It receives the already normalised parameters.
type TailPlanner func(ctx context.Context, params url.Values, eff policy.Effective) (enforce.Plan, error)

// Deps are the collaborators of a Server.
type Deps struct {
	Config    *config.Config
	Snapshots SnapshotProvider
	Auth      Authenticator
	// Enforcers are the per-backend-type query enforcers. A backend type without one can only serve
	// unrestricted grants and safe endpoints; everything else fails closed with 503.
	Enforcers map[config.BackendType]enforce.Enforcer
	// TailPlanners plan WebSocket (tail) endpoints per backend type; without one such requests fail closed.
	TailPlanners map[config.BackendType]TailPlanner
	// Mergers combine fan-out responses per merge kind.
	Mergers map[enforce.MergeKind]proxy.Merger

	// UIListen and UIHandler enable the optional read-only dashboard listener (both set, or neither).
	UIListen  string
	UIHandler http.Handler

	Metrics *observability.Metrics
	// Gatherer backs /metrics; nil serves the default registry.
	Gatherer prometheus.Gatherer
	Privacy  observability.Privacy
	Logger   *slog.Logger

	// Transport is the base upstream transport (nil: a clone of http.DefaultTransport).
	Transport http.RoundTripper
	// TracerProvider defaults to the global provider.
	TracerProvider trace.TracerProvider
}

// Server holds the two HTTP handlers and runs them.
type Server struct {
	d       Deps
	tracer  trace.Tracer
	tp      trace.TracerProvider
	proxies map[string]*proxy.Proxy // by backend name
	mergers map[enforce.MergeKind]proxy.Merger
	logger  *slog.Logger
}

// New builds a Server. Metrics may be nil, in which case none are recorded.
func New(d Deps) *Server {
	tp := d.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	base := d.Transport
	if base == nil {
		base = http.DefaultTransport.(*http.Transport).Clone()
	}
	s := &Server{d: d, tracer: tp.Tracer(tracerName), tp: tp, logger: logger, proxies: map[string]*proxy.Proxy{},
		mergers: map[enforce.MergeKind]proxy.Merger{
			enforce.MergeLokiStreams: fanout.MergeLokiStreams,
			enforce.MergeStringSet:   fanout.NewStringSetMerger(deniedFromRequest),
			enforce.MergeSumCounts:   fanout.NewSumCountsMerger(deniedFromRequest, approximateFromRequest),
		}}
	for i := range d.Config.Backends {
		b := &d.Config.Backends[i]
		rt := http.RoundTripper(&meteredTransport{base: base, backend: b.Name, m: d.Metrics})
		rt = otelhttp.NewTransport(rt,
			otelhttp.WithTracerProvider(tp),
			otelhttp.WithPropagators(observability.NewPropagator()),
			otelhttp.WithSpanNameFormatter(func(string, *http.Request) string { return "upstream" }),
		)
		s.proxies[b.Name] = &proxy.Proxy{Transport: rt, Limits: d.Config.Limits, Logger: logger}
	}
	return s
}

// ProxyHandler returns the handler for the proxy listener, wrapped in the server span.
func (s *Server) ProxyHandler() http.Handler {
	return otelhttp.NewHandler(http.HandlerFunc(s.serveProxy), "proxy",
		otelhttp.WithTracerProvider(s.tp),
		otelhttp.WithPropagators(observability.NewPropagator()),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return "HTTP " + r.Method }),
	)
}

// ManagementHandler returns the handler for /metrics, /healthz and /readyz.
func (s *Server) ManagementHandler() http.Handler {
	mux := http.NewServeMux()
	g := s.d.Gatherer
	if g == nil {
		g = prometheus.DefaultGatherer
	}
	mux.Handle("GET /metrics", promhttp.HandlerFor(g, promhttp.HandlerOpts{EnableOpenMetrics: true}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		switch {
		case s.d.Snapshots.Snapshot() == nil:
			http.Error(w, "policy snapshot not ready", http.StatusServiceUnavailable)
		case !s.d.Auth.Ready():
			http.Error(w, "issuer discovery not complete", http.StatusServiceUnavailable)
		default:
			_, _ = w.Write([]byte("ok\n"))
		}
	})
	return mux
}

// Run serves both listeners until ctx is cancelled, then shuts them down gracefully (draining in-flight
// requests for up to shutdownTimeout). It returns the first serve error, or nil after a clean shutdown.
func (s *Server) Run(ctx context.Context, proxyAddr, mgmtAddr string, shutdownTimeout time.Duration) error {
	servers := []*http.Server{
		{Addr: proxyAddr, Handler: s.ProxyHandler(), ReadHeaderTimeout: 10 * time.Second},
		{Addr: mgmtAddr, Handler: s.ManagementHandler(), ReadHeaderTimeout: 10 * time.Second},
	}
	if s.d.UIListen != "" && s.d.UIHandler != nil {
		servers = append(servers, &http.Server{Addr: s.d.UIListen, Handler: s.d.UIHandler, ReadHeaderTimeout: 10 * time.Second})
	}
	var listeners []net.Listener
	for _, srv := range servers {
		l, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, o := range listeners {
				_ = o.Close()
			}
			return err
		}
		listeners = append(listeners, l)
	}
	return s.serve(ctx, servers, listeners, shutdownTimeout)
}

func (s *Server) serve(ctx context.Context, servers []*http.Server, listeners []net.Listener, shutdownTimeout time.Duration) error {
	errc := make(chan error, len(servers))
	for i, srv := range servers {
		go func() { errc <- srv.Serve(listeners[i]) }()
	}
	var first error
	select {
	case <-ctx.Done():
	case first = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(sctx); err != nil && first == nil {
			first = err
		}
	}
	if errors.Is(first, http.ErrServerClosed) {
		first = nil
	}
	return first
}

// RequireGroup wraps next so that only authenticated callers carrying group (when non-empty) get through.
// It must run inside Authenticator.Middleware.
func RequireGroup(group string, next http.Handler) http.Handler {
	if group == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := auth.IdentityFrom(r.Context()); ok && slices.Contains(id.Groups, group) {
			next.ServeHTTP(w, r)
			return
		}
		proxy.WriteError(w, r, http.StatusForbidden, "forbidden", "dashboard access requires membership of the admin group")
	})
}
