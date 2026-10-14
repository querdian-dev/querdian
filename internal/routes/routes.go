// Package routes holds the endpoint tables per backend type (PLAN §8, docs/endpoints.md) and the
// default-deny lookup that maps (backend type, path, method) to an Endpoint.
package routes

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/querdian-dev/querdian/internal/config"
)

// Kind classifies how the proxy treats an endpoint.
type Kind int

// Endpoint kinds. The zero value is KindDeny so that a zero Endpoint is never permissive.
const (
	// KindDeny: reject with 403 (default for anything not listed).
	KindDeny Kind = iota
	// KindEnforce: the enforcement parameter (Endpoint.Param) is rewritten per policy.
	KindEnforce
	// KindSafe: proxied unchanged (tenant header still set).
	KindSafe
	// KindTraceByID: Tempo trace-by-ID, handled by the configured traceById mode (PLAN §7.4).
	KindTraceByID
)

// String returns the lowercase kind name.
func (k Kind) String() string {
	switch k {
	case KindEnforce:
		return "enforce"
	case KindSafe:
		return "safe"
	case KindTraceByID:
		return "traceByID"
	}
	return "deny"
}

// Endpoint is the result of a route lookup.
type Endpoint struct {
	// Kind is the treatment of the endpoint. KindDeny for unknown paths and for known paths hit
	// with a method that is not in Methods.
	Kind Kind
	// Param is the enforcement parameter ("query", "q", "match[]"), empty for safe/deny/traceByID.
	Param string
	// Template is the matched route template, e.g. "/api/v1/label/:name/values". It is empty for
	// unknown paths and is safe to use as a low-cardinality metrics label.
	Template string
	// Methods are the accepted HTTP methods (upper case). Empty for KindDeny.
	Methods []string
	// Websocket is true when the endpoint is served via WebSocket upgrade (Loki tail).
	Websocket bool
	// AllowedParams is the per-route allow-list of request parameters that may be forwarded
	// (PLAN §9). Every other parameter must be dropped or rejected; Thanos storeMatch[] is never
	// listed. It includes the enforcement parameter. The slice is shared: do not modify it.
	AllowedParams []string
	// PathParam is the value captured for a ":name" / ":id" template segment, if any.
	PathParam string
}

// AllowsParam reports whether name is on the endpoint's parameter allow-list.
func (e Endpoint) AllowsParam(name string) bool { return slices.Contains(e.AllowedParams, name) }

// AllowsMethod reports whether method (exact, upper case) is accepted by the endpoint.
func (e Endpoint) AllowsMethod(method string) bool { return slices.Contains(e.Methods, method) }

type route struct {
	ep       Endpoint // Template, Kind, ... ; PathParam unset
	segs     []string
	literals int
	wildcard bool
	validate func(string) bool // optional validator for the captured parameter
}

// Table is the immutable route table of one backend type.
type Table struct {
	routes []route
}

func compile(rs []route) *Table {
	for i := range rs {
		r := &rs[i]
		r.segs = strings.Split(strings.TrimPrefix(r.ep.Template, "/"), "/")
		for j, s := range r.segs {
			switch {
			case s == "*":
				if j != len(r.segs)-1 {
					panic("routes: wildcard must be last: " + r.ep.Template)
				}
				r.wildcard = true
			case strings.HasPrefix(s, ":"):
			default:
				r.literals++
			}
		}
		if r.ep.Kind == KindDeny {
			r.ep.Methods = nil
			r.ep.AllowedParams = nil
			r.ep.Param = ""
		}
	}
	return &Table{routes: rs}
}

// ForType returns the route table for a backend type, or nil for unknown types.
func ForType(t config.BackendType) *Table {
	switch t {
	case config.BackendPrometheus:
		return prometheusTable
	case config.BackendLoki:
		return lokiTable
	case config.BackendTempo:
		return tempoTable
	}
	return nil
}

// Lookup resolves path and method against the table of backend type t. path is the request path
// after the backend's pathPrefix has been stripped, already percent-decoded (r.URL.Path), starting
// with "/". It never returns an error: anything unknown, non-canonical, or hit with a method that
// the route does not accept yields an Endpoint with Kind == KindDeny (default deny).
func Lookup(t config.BackendType, path, method string) Endpoint {
	tab := ForType(t)
	if tab == nil || !Canonical(path) {
		return Endpoint{}
	}
	return tab.lookup(path, method)
}

func (tab *Table) lookup(path, method string) Endpoint {
	segs := strings.Split(path[1:], "/")
	var best *route
	var bestParam string
	bestScore := -1
	for i := range tab.routes {
		r := &tab.routes[i]
		param, ok := r.match(segs)
		if !ok {
			continue
		}
		// Literal segments beat parameters beat wildcards.
		score := r.literals * 4
		if !r.wildcard {
			score += 2
		}
		if score > bestScore {
			best, bestParam, bestScore = r, param, score
		}
	}
	if best == nil {
		return Endpoint{}
	}
	ep := best.ep
	if ep.Kind == KindDeny || !ep.AllowsMethod(method) {
		return Endpoint{Template: ep.Template}
	}
	ep.PathParam = bestParam
	return ep
}

func (r *route) match(segs []string) (param string, ok bool) {
	if r.wildcard {
		if len(segs) < len(r.segs) { // "*" needs at least one segment
			return "", false
		}
	} else if len(segs) != len(r.segs) {
		return "", false
	}
	for i, rs := range r.segs {
		if rs == "*" {
			return param, true
		}
		if strings.HasPrefix(rs, ":") {
			if r.validate != nil && !r.validate(segs[i]) {
				return "", false
			}
			param = segs[i]
			continue
		}
		if rs != segs[i] {
			return "", false
		}
	}
	return param, true
}

// Canonical reports whether path is in the only form the router accepts: absolute, no empty
// segments (double slashes, trailing slash), no "." or ".." segments, no backslash, no percent
// sign (so no encoded slash or dot can survive a second decoding), no control characters
// including NUL, and valid UTF-8. Non-canonical paths are denied rather than normalised, so they
// can never alias another route. Callers should additionally refuse requests whose raw
// (escaped) path contains %2f, %5c or %00, which url.URL.Path has already decoded.
func Canonical(path string) bool {
	if len(path) < 2 || path[0] != '/' || !utf8.ValidString(path) {
		return false
	}
	if strings.HasSuffix(path, "/") {
		return false
	}
	for i := 0; i < len(path); i++ {
		if c := path[i]; c < 0x20 || c == 0x7f || c == '\\' || c == '%' {
			return false
		}
	}
	for seg := range strings.SplitSeq(path[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
