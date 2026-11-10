package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

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

// hiddenQuery is the query string that hideQueryTransport removed from the request URL.
type hiddenQuery struct {
	raw   string
	force bool
}

// hideQueryTransport removes the query string from the request URL before the otelhttp transport sees the
// request. The otelhttp transport records url.full from the request URL, and for GET requests the query
// string holds the rewritten query with the policy matchers. Thus the upstream span records only scheme,
// host and path. The query goes in the request context, and restoreQueryTransport puts it back below the
// otelhttp transport. Use this pair only when the privacy flags do not permit query text in spans.
type hideQueryTransport struct{ next http.RoundTripper }

// RoundTrip implements http.RoundTripper. It does not change r.
func (t hideQueryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL == nil {
		return t.next.RoundTrip(r)
	}
	q := hiddenQuery{raw: r.URL.RawQuery, force: r.URL.ForceQuery}
	out := r.WithContext(context.WithValue(r.Context(), hiddenQueryKey{}, q))
	u := *r.URL
	u.RawQuery, u.ForceQuery = "", false
	out.URL = &u
	return t.next.RoundTrip(out)
}

// restoreQueryTransport puts back the query string that hideQueryTransport removed. Without a hidden query in
// the context, it sends the request unchanged.
type restoreQueryTransport struct{ next http.RoundTripper }

// RoundTrip implements http.RoundTripper. It does not change r.
func (t restoreQueryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	q, ok := r.Context().Value(hiddenQueryKey{}).(hiddenQuery)
	if !ok || r.URL == nil {
		return t.next.RoundTrip(r)
	}
	out := r.WithContext(r.Context())
	u := *r.URL
	u.RawQuery, u.ForceQuery = q.raw, q.force
	out.URL = &u
	return t.next.RoundTrip(out)
}
