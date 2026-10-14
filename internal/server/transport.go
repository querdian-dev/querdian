package server

import (
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
