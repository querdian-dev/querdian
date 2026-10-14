// Package proxy normalises client requests and forwards enforced plans to the backends (PLAN §9).
package proxy

import (
	"context"
	"encoding/json"
	"net/http"

	"go.opentelemetry.io/otel/trace"
)

// TraceHeader carries the trace ID of every response (PLAN §11.2).
const TraceHeader = "X-Trace-Id"

// ErrorBody is the Prometheus-style error document used by all backends (PLAN §9).
type ErrorBody struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	TraceID   string `json:"traceId"`
}

// TraceID returns the hex trace ID of the span in ctx, or "" if there is none.
func TraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}

// SetTraceHeader sets X-Trace-Id from ctx. It does nothing without a valid span.
func SetTraceHeader(w http.Header, ctx context.Context) { //nolint:revive // header first reads better at call sites
	if id := TraceID(ctx); id != "" {
		w.Set(TraceHeader, id)
	}
}

// WriteError writes the JSON error document with the given HTTP status and errorType.
func WriteError(w http.ResponseWriter, r *http.Request, status int, errorType, msg string) {
	SetTraceHeader(w.Header(), r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorBody{Status: "error", ErrorType: errorType, Error: msg, TraceID: TraceID(r.Context())})
}

// Request-level violations (PLAN §9): all map to 400 bad_data.
const (
	errorTypeBadData     = "bad_data"
	errorTypeUpstream    = "upstream"
	errorTypeUnavailable = "unavailable"
)
