package proxy

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/querdian-dev/querdian/internal/routes"
)

// DefaultMaxFormBytes bounds POST bodies.
const DefaultMaxFormBytes = 1 << 20

// RequestError is a client-side violation; it is always reported as 400 bad_data.
type RequestError struct{ Msg string }

// Error implements error.
func (e *RequestError) Error() string { return e.Msg }

func badRequest(format string, a ...any) error { return &RequestError{Msg: fmt.Sprintf(format, a...)} }

// UpstreamPath validates the request path against the backend's pathPrefix and returns the remainder (for
// example "/api/v1/query") for route lookup and for the upstream request.
//
// url.URL.Path is already percent-decoded, so an escaped slash would be indistinguishable from a real one and
// could alias another route; escaped slashes, backslashes and NUL in the raw path are therefore rejected here.
func UpstreamPath(r *http.Request, pathPrefix string) (string, error) {
	raw := strings.ToLower(r.URL.EscapedPath())
	for _, bad := range []string{"%2f", "%5c", "%00"} {
		if strings.Contains(raw, bad) {
			return "", badRequest("invalid escape in path")
		}
	}
	p := r.URL.Path
	if !strings.HasPrefix(p, pathPrefix+"/") {
		return "", badRequest("path outside backend prefix")
	}
	rest := p[len(pathPrefix):]
	if !routes.Canonical(rest) {
		return "", badRequest("non-canonical path")
	}
	return rest, nil
}

// Normalise merges the URL query and the form body the way Prometheus does, then validates the result against
// the endpoint: the method must be allowed, POST bodies must be form-encoded, single-valued parameters (every
// name not ending in "[]") must occur at most once, and only allow-listed parameters may be present. Unknown and
// repeated parameters are rejected rather than dropped so that a client never silently gets different semantics
// than it asked for: the enforcers and mergers read the first value of a parameter (query, q, targetLabels,
// limit, delay_for, direction), and backends do not agree on which value wins.
func Normalise(r *http.Request, ep routes.Endpoint, maxFormBytes int64) (url.Values, error) {
	if !ep.AllowsMethod(r.Method) {
		return nil, badRequest("method %s not allowed", r.Method)
	}
	params, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, badRequest("invalid query string")
	}
	if r.Method == http.MethodPost {
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "application/x-www-form-urlencoded" {
			return nil, badRequest("POST requires Content-Type application/x-www-form-urlencoded")
		}
		if maxFormBytes <= 0 {
			maxFormBytes = DefaultMaxFormBytes
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxFormBytes+1))
		if err != nil {
			return nil, badRequest("cannot read request body")
		}
		if int64(len(body)) > maxFormBytes {
			return nil, badRequest("request body too large")
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, badRequest("invalid form body")
		}
		for k, v := range form {
			params[k] = append(params[k], v...)
		}
	}

	var repeated []string
	for k, v := range params {
		if len(v) > 1 && !strings.HasSuffix(k, "[]") {
			repeated = append(repeated, strconv.Quote(k))
		}
	}
	if len(repeated) > 0 {
		sort.Strings(repeated)
		return nil, badRequest("parameter %s must be given at most once", strings.Join(repeated, ", "))
	}
	var unknown []string
	for k := range params {
		if !ep.AllowsParam(k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, badRequest("parameter not allowed: %s", strings.Join(unknown, ", "))
	}
	return params, nil
}

// IsRequestError reports whether err is a client-side violation.
func IsRequestError(err error) bool {
	var re *RequestError
	return errors.As(err, &re)
}
