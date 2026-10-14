package auth

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.opentelemetry.io/otel/trace"
)

type errorBody struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	TraceID   string `json:"traceId"`
}

func writeError(w http.ResponseWriter, r *http.Request, code int, errorType, msg string) {
	tid := ""
	if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
		tid = sc.TraceID().String()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(errorBody{Status: "error", ErrorType: errorType, Error: msg, TraceID: tid})
}

// Middleware authenticates each request and calls next with the Identity in
// the request context (see IdentityFrom). Failures yield a 401 in the PLAN §9
// error format and increment the failure counter. While issuer discovery is
// still pending it fails closed with 503 "unavailable" (not counted as an
// auth failure).
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := a.Authenticate(r)
		if err != nil {
			if errors.Is(err, ErrNotReady) {
				writeError(w, r, http.StatusServiceUnavailable, "unavailable", "authentication not ready")
				return
			}
			reason := ReasonOf(err)
			a.countFailure(reason)
			writeError(w, r, http.StatusUnauthorized, "unauthorized", clientMessage(reason))
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
	})
}
