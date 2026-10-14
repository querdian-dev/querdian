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
}

// Enforcer rewrites the parameters of a request according to the effective policy.
type Enforcer interface {
	// Plan returns the upstream requests for params under eff. Errors are typed (see ErrParse,
	// ErrDeniedLabel, ErrUnsupported, ErrTooComplex); use HTTPStatus and ErrorType to map them.
	Plan(ctx context.Context, ep routes.Endpoint, params url.Values, eff policy.Effective) (Plan, error)
}
