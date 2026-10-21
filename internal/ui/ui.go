// Package ui serves the optional, strictly read-only policy dashboard: a JSON API over the current policy
// snapshot plus embedded static assets. It never contacts a backend and never mutates anything.
package ui

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
)

//go:embed web
var webFS embed.FS

// SnapshotProvider supplies the current policy snapshot (nil before the first one exists).
type SnapshotProvider interface {
	Snapshot() *policy.Snapshot
}

// Deps are the collaborators of the dashboard.
type Deps struct {
	Config    *config.Config
	Snapshots SnapshotProvider
	// Enforcers rewrite queries for the test bench, per backend type.
	Enforcers map[config.BackendType]enforce.Enforcer
	Version   string
	Logger    *slog.Logger
}

type handler struct{ d Deps }

// Handler returns the dashboard handler: GET /api/*, POST /api/test and the static assets.
func Handler(d Deps) http.Handler {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	h := &handler{d: d}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // the embedded tree always has a web directory
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/overview", h.overview)
	mux.HandleFunc("GET /api/policies", h.policies)
	mux.HandleFunc("GET /api/subjects", h.subjects)
	mux.HandleFunc("GET /api/access", h.access)
	mux.HandleFunc("GET /api/backends", h.backends)
	mux.HandleFunc("POST /api/test", h.test)
	mux.Handle("GET /", http.FileServerFS(sub))
	return secure(mux)
}

// secure sets the response headers on every answer. The CSP forbids inline script and any third-party origin.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Cache-Control", "no-store")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; "+
			"img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"status": "error", "error": msg})
}

// snap returns the current snapshot or answers 503 and reports false.
func (h *handler) snap(w http.ResponseWriter) (*policy.Snapshot, bool) {
	s := h.d.Snapshots.Snapshot()
	if s == nil {
		writeErr(w, http.StatusServiceUnavailable, "policy snapshot not ready")
		return nil, false
	}
	return s, true
}
