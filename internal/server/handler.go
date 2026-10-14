package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/querdian-dev/querdian/internal/auth"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/observability"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/proxy"
	"github.com/querdian-dev/querdian/internal/routes"
)

// Decisions recorded on the server span and in metrics.
const (
	decisionAllow = "allow"
	decisionDeny  = "deny"
	decisionError = "error"
)

// reqState is the per-request outcome used for span attributes, metrics and the log line.
type reqState struct {
	backend, endpoint string
	decision, reason  string
	policies          []string
	user              string
}

// statusRecorder captures the response status while keeping Flush/Hijack reachable via Unwrap.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// serveProxy implements the request lifecycle of PLAN §3: readiness gate, route lookup, authentication,
// policy resolution, enforcement, execution. Every failure is a rejection; nothing passes unenforced.
func (s *Server) serveProxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()
	span := trace.SpanFromContext(ctx)
	proxy.SetTraceHeader(w.Header(), ctx)

	st := &reqState{backend: "unknown", endpoint: "unknown", decision: decisionAllow, reason: "allowed"}
	rec := &statusRecorder{ResponseWriter: w}
	defer func() { s.finish(r, rec, st, span, start) }()

	snap := s.d.Snapshots.Snapshot()
	if snap == nil {
		st.set(decisionError, "snapshot_not_ready")
		proxy.Unavailable(rec, r)
		return
	}

	b := s.matchBackend(r.URL.Path)
	if b == nil {
		st.set(decisionDeny, "endpoint_not_allowed")
		proxy.WriteError(rec, r, http.StatusForbidden, "forbidden", "endpoint not allowed")
		return
	}
	st.backend = b.Name
	upath, err := proxy.UpstreamPath(r, b.PathPrefix)
	if err != nil {
		st.set(decisionDeny, "bad_path")
		proxy.WriteError(rec, r, http.StatusBadRequest, "bad_data", err.Error())
		return
	}
	ep := routes.Lookup(b.Type, upath, r.Method)
	if ep.Template != "" {
		st.endpoint = ep.Template
		span.SetName("HTTP " + r.Method + " " + ep.Template)
		span.SetAttributes(attribute.String("http.route", ep.Template))
	}
	if ep.Kind == routes.KindDeny {
		st.set(decisionDeny, "endpoint_not_allowed")
		proxy.WriteError(rec, r, http.StatusForbidden, "forbidden", "endpoint not allowed")
		return
	}

	// Authentication is required for all routes, including safe ones.
	authCtx, authSpan := s.tracer.Start(ctx, "authn.verify")
	reached := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, ar *http.Request) {
		reached = true
		authSpan.End()
		id, ok := auth.IdentityFrom(ar.Context())
		if !ok {
			st.set(decisionError, "internal")
			proxy.WriteError(w, ar, http.StatusInternalServerError, "internal", "identity missing")
			return
		}
		// Continue on the original context so later spans are not children of the finished auth span.
		s.authorized(w, ar.WithContext(auth.WithIdentity(ctx, id)), st, snap, b, ep, upath, id)
	})
	s.d.Auth.Middleware(inner).ServeHTTP(rec, r.WithContext(authCtx))
	if !reached {
		authSpan.SetStatus(codes.Error, "authentication failed")
		authSpan.End()
		if rec.status == http.StatusServiceUnavailable {
			st.set(decisionError, "auth_unavailable")
		} else {
			st.set(decisionDeny, "unauthenticated")
		}
	}
}

// authorized runs resolve, enforce and execute for an authenticated caller.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request, st *reqState, snap *policy.Snapshot,
	b *config.Backend, ep routes.Endpoint, upath string, id auth.Identity) {
	ctx := r.Context()
	span := trace.SpanFromContext(ctx)
	st.user = id.Username
	s.d.Privacy.SetUser(span, id.Username)

	// --- resolve
	_, rspan := s.tracer.Start(ctx, "policy.resolve")
	eff, err := snap.Resolve(id.Username, id.Groups, b.Name)
	rspan.SetAttributes(
		attribute.Int64("snapshot.generation", snap.Generation),
		attribute.Int("policies.matched", len(eff.Policies)),
		attribute.Int("dnf.terms", len(eff.Terms)),
		attribute.Bool("dnf.unrestricted", eff.Unrestricted),
		attribute.Bool("dnf.disjoint", eff.Disjoint),
		attribute.Int("tenants.count", len(eff.Tenants)),
	)
	rspan.End()
	if err != nil {
		switch {
		case errors.Is(err, policy.ErrNoGrant):
			st.set(decisionDeny, "no_grant")
			proxy.WriteError(w, r, http.StatusForbidden, "forbidden", "no policy grants access to this backend")
		case errors.Is(err, policy.ErrTooComplex):
			st.set(decisionDeny, "policy_too_complex")
			proxy.WriteError(w, r, http.StatusForbidden, "forbidden", "effective policy is too complex")
		case errors.Is(err, policy.ErrCrossTenantUnion):
			st.set(decisionDeny, "cross_tenant_union_unsupported")
			proxy.WriteError(w, r, http.StatusForbidden, "forbidden", "access across tenants with differing rules is not supported on this backend")
		default:
			st.set(decisionError, "resolve_failed")
			proxy.WriteError(w, r, http.StatusInternalServerError, "internal", "policy resolution failed")
		}
		return
	}
	st.policies = eff.Policies
	span.SetAttributes(attribute.StringSlice("querdian.policies", eff.Policies))
	if s.d.Metrics != nil {
		s.d.Metrics.PolicyTerms.WithLabelValues(b.Name).Observe(float64(len(eff.Terms)))
	}

	if ep.Kind == routes.KindTraceByID && b.TraceByID != config.TraceByIDFilter {
		st.set(decisionDeny, "trace_by_id_denied")
		proxy.WriteError(w, r, http.StatusForbidden, "forbidden", "trace lookup by ID is disabled")
		return
	}

	params, err := proxy.Normalise(r, ep, 0)
	if err != nil {
		st.set(decisionDeny, "bad_request")
		proxy.WriteError(w, r, http.StatusBadRequest, "bad_data", err.Error())
		return
	}

	plan, ok := s.plan(w, r, st, b, ep, params, eff)
	if !ok {
		return
	}
	if len(plan.Requests) == 0 || len(plan.Requests) > s.d.Config.Limits.MaxFanout {
		st.set(decisionDeny, "query_too_complex")
		proxy.WriteError(w, r, http.StatusForbidden, "forbidden", "query requires too many upstream requests")
		return
	}
	s.d.Privacy.SetQueries(span, strings.Join(params[ep.Param], " "), strings.Join(plan.Requests[0].Query[ep.Param], " "))

	s.proxies[b.Name].Execute(w, r, proxy.Target{Backend: b, Endpoint: ep, UpstreamPath: upath, Effective: eff}, plan, s.d.Mergers[plan.Merge])
}

// plan builds the upstream plan. It reports false after having written the error response.
func (s *Server) plan(w http.ResponseWriter, r *http.Request, st *reqState, b *config.Backend, ep routes.Endpoint,
	params map[string][]string, eff policy.Effective) (enforce.Plan, bool) {
	passthrough := enforce.Plan{Requests: []enforce.Upstream{{Query: params, Term: -1}}, Merge: enforce.MergeNone, Strategy: "passthrough"}
	// Safe endpoints and unrestricted grants without denied labels need no query rewriting.
	if ep.Kind == routes.KindSafe || (eff.Unrestricted && len(eff.DeniedLabels) == 0) {
		return passthrough, true
	}
	if ep.Kind == routes.KindTraceByID {
		st.set(decisionError, "enforcer_unavailable")
		proxy.WriteError(w, r, http.StatusServiceUnavailable, "unavailable", "enforcement is not available for this endpoint")
		return enforce.Plan{}, false
	}
	e := s.d.Enforcers[b.Type]
	if e == nil {
		st.set(decisionError, "enforcer_unavailable")
		proxy.WriteError(w, r, http.StatusServiceUnavailable, "unavailable", "enforcement is not available for this backend type")
		return enforce.Plan{}, false
	}

	ctx, span := s.tracer.Start(r.Context(), "enforce.rewrite")
	defer span.End()
	t0 := time.Now()
	plan, err := e.Plan(ctx, ep, params, eff)
	lang := language(b.Type)
	if err != nil {
		span.SetStatus(codes.Error, "enforcement failed")
		reason, decision := enforceReason(err)
		st.set(decision, reason)
		proxy.WriteError(w, r, enforce.HTTPStatus(err), enforce.ErrorType(err), enforce.Detail(err))
		return enforce.Plan{}, false
	}
	if s.d.Metrics != nil {
		s.d.Metrics.RewriteDuration.WithLabelValues(lang, plan.Strategy).Observe(time.Since(t0).Seconds())
	}
	span.SetAttributes(
		attribute.String("query.language", lang),
		attribute.String("strategy", plan.Strategy),
		attribute.Int("query.bytes_in", joinedLen(params[ep.Param])),
	)
	if len(plan.Requests) > 0 {
		span.SetAttributes(attribute.Int("query.bytes_out", joinedLen(plan.Requests[0].Query[ep.Param])))
	}
	return plan, true
}

func joinedLen(v []string) int {
	n := 0
	for _, s := range v {
		n += len(s)
	}
	return n
}

func language(t config.BackendType) string {
	switch t {
	case config.BackendLoki:
		return "logql"
	case config.BackendTempo:
		return "traceql"
	default:
		return "promql"
	}
}

func enforceReason(err error) (reason, decision string) {
	switch {
	case errors.Is(err, enforce.ErrDeniedLabel):
		return "denied_label", decisionDeny
	case errors.Is(err, enforce.ErrTooComplex):
		return "query_too_complex", decisionDeny
	case errors.Is(err, enforce.ErrParse):
		return "parse_error", decisionDeny
	case errors.Is(err, enforce.ErrUnsupported):
		return "unsupported", decisionDeny
	default:
		return "enforcement_error", decisionError
	}
}

// matchBackend finds the backend whose pathPrefix is a path prefix of p (prefixes never nest, see config).
func (s *Server) matchBackend(p string) *config.Backend {
	for i := range s.d.Config.Backends {
		b := &s.d.Config.Backends[i]
		if strings.HasPrefix(p, b.PathPrefix+"/") {
			return b
		}
	}
	return nil
}

func (st *reqState) set(decision, reason string) { st.decision, st.reason = decision, reason }

// finish records span attributes, metrics and the log line.
func (s *Server) finish(r *http.Request, rec *statusRecorder, st *reqState, span trace.Span, start time.Time) {
	if st.decision == decisionAllow && rec.status >= http.StatusInternalServerError {
		st.set(decisionError, "upstream_error")
	}
	dur := time.Since(start)
	span.SetAttributes(
		attribute.String("querdian.decision", st.decision),
		attribute.String("querdian.decision.reason", st.reason),
		attribute.StringSlice("querdian.policies", st.policies),
	)
	if st.decision == decisionError {
		span.SetStatus(codes.Error, st.reason)
	}
	if m := s.d.Metrics; m != nil {
		m.Requests.WithLabelValues(st.backend, st.endpoint, st.decision, st.reason).Inc()
		observability.ObserveWithExemplar(r.Context(), m.RequestDuration.WithLabelValues(st.backend, st.endpoint, st.decision), dur.Seconds())
	}
	attrs := []slog.Attr{
		slog.String("backend", st.backend),
		slog.String("route", st.endpoint),
		slog.String("decision", st.decision),
		slog.String("reason", st.reason),
		slog.Int("status", rec.status),
		slog.Duration("duration", dur),
	}
	if s.d.Privacy.LogUser() && st.user != "" {
		attrs = append(attrs, slog.String("user", st.user))
	}
	s.logger.LogAttrs(context.WithoutCancel(r.Context()), slog.LevelInfo, "request", attrs...)
}
