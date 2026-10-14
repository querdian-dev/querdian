// Package enforce defines the enforcement contract between the proxy core and the per-language
// enforcers (PLAN §7.1): a request is turned into a Plan of upstream requests plus a merge kind.
package enforce

import (
	"context"
	"net/url"

	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
)

// MergeKind says how the responses of a Plan's upstream requests are combined.
type MergeKind int

// Merge kinds (PLAN §7.1).
const (
	MergeNone        MergeKind = iota // single upstream request, streamed
	MergeLokiStreams                  // log query results
	MergeStringSet                    // labels / label values / detected_*
	MergeSumCounts                    // index/stats, volume, patterns
	MergeTailStreams                  // websocket tail
	MergeTempoSearch                  // search results with metadata redaction
)

// Upstream is one request to send to the backend.
type Upstream struct {
	Query url.Values // full param set to send (enforcement param rewritten)
	Term  int        // term index, -1 when not fanned out (for tracing)
}

// Plan is the result of enforcement for one client request.
type Plan struct {
	Requests []Upstream // len 1 unless fanned out
	Merge    MergeKind
	// Strategy is one of "inject", "or_lift", "fanout", "match_expand", "passthrough" (tracing).
	Strategy string

	// ResponseFilter is optional (nil for almost all plans). When set, the proxy must buffer the
	// complete body of every successful (2xx) upstream response of this plan, pass it through
	// ResponseFilter and send the returned body to the client instead (with a corrected
	// Content-Length; the proxy should request an uncompressed upstream response). An error from the filter, or a body larger than
	// ResponseFilterMaxBytes, must fail the request closed (502); the unfiltered body must never be
	// sent. Non-2xx responses carry no data and are streamed unchanged. It is used for the Prometheus
	// "labels" endpoint to hide denied label names (promql.FilterLabelsResponse).
	ResponseFilter func(body []byte) ([]byte, error)
	// ResponseFilterMaxBytes bounds the buffering for ResponseFilter; it is meaningful only when
	// ResponseFilter is set.
	ResponseFilterMaxBytes int64
}

// Enforcer rewrites the parameters of a request according to the effective policy.
type Enforcer interface {
	// Plan returns the upstream requests for params under eff. Errors are typed (see ErrParse,
	// ErrDeniedLabel, ErrUnsupported, ErrTooComplex); use HTTPStatus and ErrorType to map them.
	Plan(ctx context.Context, ep routes.Endpoint, params url.Values, eff policy.Effective) (Plan, error)
}
