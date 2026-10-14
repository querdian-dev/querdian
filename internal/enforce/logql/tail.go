package logql

import (
	"context"
	"net/url"
	"strconv"

	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
	lq "github.com/querdian-dev/querdian/pkg/logql"
)

// maxTailDelayFor is the largest delay_for Loki accepts, in seconds (public Loki HTTP API docs).
const maxTailDelayFor = 5

// PlanTail builds the plan for the Loki tail WebSocket endpoint. It accepts the parameters query, limit,
// start and delay_for, requires a log query (metric queries cannot be tailed), and reuses the query and
// query_range enforcement for the query parameter. The result always has merge kind enforce.MergeTailStreams:
// one upstream socket for an unrestricted grant or a single term (strategy "passthrough" or "inject"), one per
// term otherwise (strategy "fanout"). The server must call it instead of Plan for endpoints with
// routes.Endpoint.Websocket set.
func PlanTail(ctx context.Context, params url.Values, eff policy.Effective) (enforce.Plan, error) {
	for k := range params {
		switch k {
		case "query", "limit", "start", "delay_for":
		default:
			return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "parameter %s not supported on tail", k)
		}
	}
	if err := checkInt(params, "limit", 1, 1<<31-1); err != nil {
		return enforce.Plan{}, err
	}
	if err := checkInt(params, "delay_for", 0, maxTailDelayFor); err != nil {
		return enforce.Plan{}, err
	}
	if !params.Has("query") {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrParse, "missing query parameter")
	}
	q, err := lq.Parse(params.Get("query"))
	if err != nil {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrParse, "%v", err)
	}
	if q.Kind == lq.KindMetric {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "tail requires a log query, not a metric query")
	}
	plan, err := New().Plan(ctx, routes.Endpoint{Param: "query"}, params, eff)
	if err != nil {
		return enforce.Plan{}, err
	}
	plan.Merge = enforce.MergeTailStreams
	return plan, nil
}

// checkInt validates an optional integer parameter within [lo, hi].
func checkInt(params url.Values, name string, lo, hi int) error {
	if !params.Has(name) {
		return nil
	}
	n, err := strconv.Atoi(params.Get(name))
	if err != nil || n < lo || n > hi {
		return enforce.Errorf(enforce.ErrParse, "invalid %s", name)
	}
	return nil
}
