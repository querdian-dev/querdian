package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	otelsemconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/querdian-dev/querdian/internal/observability"
)

// meteredTransport records upstream request count and duration per backend.
type meteredTransport struct {
	base    http.RoundTripper
	backend string
	m       *observability.Metrics
}

// RoundTrip implements http.RoundTripper.
func (t *meteredTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(r)
	if t.m != nil {
		code := "error"
		if err == nil {
			code = strconv.Itoa(resp.StatusCode)
		}
		t.m.UpstreamRequests.WithLabelValues(t.backend, code).Inc()
		observability.ObserveWithExemplar(r.Context(), t.m.UpstreamDuration.WithLabelValues(t.backend), time.Since(start).Seconds())
	}
	return resp, err
}

// hiddenQueryKey is the context key under which hideQueryTransport keeps the query string.
type hiddenQueryKey struct{}

// hiddenQuery is the query string that hideQueryTransport removed from the request URL. A request that went
// through hideQueryTransport always has a hiddenQuery in its context, also with an empty query string.
type hiddenQuery struct {
	raw   string
	force bool
}

// errQueryNotHidden is the error when restoreQueryTransport gets a request without a hiddenQuery in its
// context. Then the query string is unknown, and a request without it can have a larger result (for
// example a label list without match[]). Thus the request is not sent.
var errQueryNotHidden = errors.New("upstream request without its query string: not sent")

// hideQueryTransport removes the query string from the request URL before the otelhttp transport sees the
// request. The otelhttp transport records url.full from the request URL, and for GET requests the query
// string holds the rewritten query. Thus the upstream span records only scheme, host and path. The query
// goes in the request context, and restoreQueryTransport puts it back below the otelhttp transport. Use
// this pair only when the privacy flags do not permit query text in spans.
type hideQueryTransport struct{ next http.RoundTripper }

// RoundTrip implements http.RoundTripper. It does not change r.
func (t hideQueryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var q hiddenQuery
	if r.URL != nil {
		q = hiddenQuery{raw: r.URL.RawQuery, force: r.URL.ForceQuery}
	}
	out := r.WithContext(context.WithValue(r.Context(), hiddenQueryKey{}, q))
	if r.URL != nil {
		u := *r.URL
		u.RawQuery, u.ForceQuery = "", false
		out.URL = &u
	}
	resp, err := t.next.RoundTrip(out)
	var se *spanSafeError
	if errors.As(err, &se) {
		err = se.err // the caller gets the original error
	}
	return resp, err
}

// restoreQueryTransport puts back the query string that hideQueryTransport removed. Without a hiddenQuery in
// the context, it fails closed: it sends nothing and returns errQueryNotHidden. It wraps each error of the
// next transport in a spanSafeError, because the otelhttp transport records the error text in the span
// status, and the error text can contain the URL with the query string. hideQueryTransport unwraps it.
type restoreQueryTransport struct{ next http.RoundTripper }

// RoundTrip implements http.RoundTripper. It does not change r.
func (t restoreQueryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	q, ok := r.Context().Value(hiddenQueryKey{}).(hiddenQuery)
	if !ok {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, errQueryNotHidden
	}
	if r.URL == nil {
		return t.next.RoundTrip(r)
	}
	out := r.WithContext(r.Context())
	u := *r.URL
	u.RawQuery, u.ForceQuery = q.raw, q.force
	out.URL = &u
	resp, err := t.next.RoundTrip(out)
	if err != nil {
		err = &spanSafeError{err: err}
	}
	return resp, err
}

// spanSafeError carries a transport error through the otelhttp transport. Its text is fixed and has no URL
// and no query string, so the span status gets only this text. hideQueryTransport returns the original
// error to the caller.
type spanSafeError struct{ err error }

func (e *spanSafeError) Error() string { return "upstream request failed" }
func (e *spanSafeError) Unwrap() error { return e.err }

// ErrorType gives the error.type span attribute of the original error.
func (e *spanSafeError) ErrorType() string { return otelsemconv.ErrorType(e.err).Value.AsString() }
