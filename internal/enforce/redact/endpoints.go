package redact

import "github.com/querdian-dev/querdian/internal/config"

// ResultRewriter returns the rewriter for a query result endpoint (route template tmpl of a backend of
// type t), or nil for every other endpoint. Label, series and index endpoints remove redacted names like
// denied ones instead; tail and trace by ID are redacted by their own paths (TailFrame, and the trace
// filter's Redactor option).
func (r *Redactor) ResultRewriter(t config.BackendType, tmpl string) func([]byte) ([]byte, error) {
	if r == nil {
		return nil
	}
	switch rewrites(t, tmpl) {
	case "query":
		return r.Query
	case "exemplars":
		return r.Exemplars
	case "tempo":
		return r.Tempo
	}
	return nil
}

// RewritesValues reports whether responses of the endpoint keep redacted labels with rewritten values
// (query results, tail, trace by ID). Elsewhere redacted names are removed like denied ones.
func RewritesValues(t config.BackendType, tmpl string) bool {
	if rewrites(t, tmpl) != "" {
		return true
	}
	switch t {
	case config.BackendLoki:
		return tmpl == "/loki/api/v1/tail"
	case config.BackendTempo:
		return tmpl == "/api/traces/:id" || tmpl == "/api/v2/traces/:id"
	}
	return false
}

func rewrites(t config.BackendType, tmpl string) string {
	switch t {
	case config.BackendPrometheus:
		switch tmpl {
		case "/api/v1/query", "/api/v1/query_range":
			return "query"
		case "/api/v1/query_exemplars":
			return "exemplars"
		}
	case config.BackendLoki:
		switch tmpl {
		case "/loki/api/v1/query", "/loki/api/v1/query_range":
			return "query"
		}
	case config.BackendTempo:
		switch tmpl {
		case "/api/search", "/api/metrics/query_range", "/api/metrics/query":
			return "tempo"
		}
	}
	return ""
}
