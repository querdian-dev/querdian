// Package logql is the LogQL enforcer (PLAN §7.3). It parses the query with pkg/logql, rejects
// references to denied labels, and rewrites the query so that every stream selector carries the
// policy: injection for a single term, or-lifting for metric queries with several terms, and
// one fanned-out request per term for log queries.
//
// This file handles the query and query_range endpoints; metadata.go handles labels, label values,
// detected_*, and series, and later files the index and pattern endpoints.
package logql

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
	lq "github.com/querdian-dev/querdian/pkg/logql"
)

// New returns the LogQL enforcer.
func New() enforce.Enforcer { return enforcer{} }

type enforcer struct{}

// Plan implements enforce.Enforcer for the Loki query and query_range endpoints (parameter "query").
//
//   - A missing or unparseable query is enforce.ErrParse.
//   - A denied label anywhere in the query is enforce.ErrDeniedLabel, also for unrestricted grants.
//   - Unrestricted: no injection; strategy "passthrough". If labels are denied the canonical
//     rendering of the parsed query is sent so that the scan and Loki see the same text.
//   - One term: matchers are injected into every stream selector; strategy "inject".
//   - Several terms, metric query: or-lift per range aggregation; strategy "or_lift". Range
//     aggregations with a by/without clause cannot be lifted and yield enforce.ErrUnsupported.
//   - Several terms, log query: one upstream request per term (Term index set, all other
//     parameters identical), merge kind MergeLokiStreams, strategy "fanout".
//
// A term without matchers injects nothing for that term. The policy's __tenant_id__ matchers are
// ordinary matchers here; Inject guarantees every selector keeps a non-__tenant_id__ anchor.
func (enforcer) Plan(_ context.Context, ep routes.Endpoint, params url.Values, eff policy.Effective) (enforce.Plan, error) {
	switch ep.Template {
	case tmplLabels, tmplLabelValues, tmplDetectedLabels, tmplDetectedFields, tmplDetectedValues:
		return planStringSet(ep, params, eff)
	case tmplIndexStats, tmplIndexVolume, tmplIndexVolumeRange, tmplPatterns:
		return planSum(params, eff)
	case tmplSeries:
		return planSeries(params, eff)
	}
	param := ep.Param
	if param == "" {
		param = "query"
	}
	if !params.Has(param) {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrParse, "missing %s parameter", param)
	}
	q, err := lq.Parse(params.Get(param))
	if err != nil {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrParse, "%v", err)
	}
	label, found, err := lq.DeniedLabel(q, eff.DeniedLabels)
	if err != nil {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrParse, "%v", err)
	}
	if found {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrDeniedLabel, "label %s denied by policy", label)
	}

	if eff.Unrestricted {
		query := params.Get(param)
		if len(eff.DeniedLabels) > 0 {
			query = q.String()
		}
		return single(params, param, query, "passthrough", -1), nil
	}
	if len(eff.Terms) == 0 {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "effective policy has no terms")
	}
	terms := make([][]lq.InjectMatcher, len(eff.Terms))
	for i, t := range eff.Terms {
		ms, err := convert(t.Matchers)
		if err != nil {
			return enforce.Plan{}, err
		}
		if q.Kind == lq.KindMetric {
			ms = negateTenantBinding(ms, eff.Tenants)
		}
		terms[i] = ms
	}

	if len(terms) == 1 {
		out, err := lq.Inject(q, terms[0])
		if err != nil {
			return enforce.Plan{}, rewriteErr(err)
		}
		return single(params, param, out, "inject", -1), nil
	}
	if q.Kind == lq.KindMetric {
		out, err := lq.OrLift(q, terms)
		if err != nil {
			return enforce.Plan{}, rewriteErr(err)
		}
		return single(params, param, out, "or_lift", -1), nil
	}
	plan := enforce.Plan{Merge: enforce.MergeLokiStreams, Strategy: "fanout"}
	for i, t := range terms {
		out, err := lq.Inject(q, t)
		if err != nil {
			return enforce.Plan{}, rewriteErr(err)
		}
		plan.Requests = append(plan.Requests, enforce.Upstream{Query: withQuery(params, param, out), Term: i})
	}
	return plan, nil
}

// negateTenantBinding rewrites a positive __tenant_id__ matcher (the tenant binding of a term under tenant
// federation) of a metric query into the equivalent negative matcher over the other tenants of the
// request: __tenant_id__="t1" becomes __tenant_id__!~"t2|t3".
//
// Black-box finding (Loki 3.5.0, tenant federation, T3.8): a positive __tenant_id__ matcher ("=" or "=~")
// inside a metric query makes Loki return an empty result, while log queries and the negative forms are
// evaluated correctly (the negative form selects exactly the remaining tenants). Without this rewrite a
// tenant-bound policy would silently yield empty metric results.
func negateTenantBinding(ms []lq.InjectMatcher, tenants []string) []lq.InjectMatcher {
	out := make([]lq.InjectMatcher, len(ms))
	copy(out, ms)
	for i, m := range out {
		if m.Name != lq.TenantIDLabel || m.Op != "=" {
			continue
		}
		var others []string
		for _, t := range tenants {
			if t != m.Value {
				others = append(others, regexp.QuoteMeta(t))
			}
		}
		if len(others) == 0 || !slices.Contains(tenants, m.Value) {
			continue
		}
		out[i] = lq.InjectMatcher{Name: m.Name, Op: "!~", Value: strings.Join(others, "|")}
	}
	return out
}

func single(params url.Values, param, query, strategy string, term int) enforce.Plan {
	return enforce.Plan{
		Requests: []enforce.Upstream{{Query: withQuery(params, param, query), Term: term}},
		Merge:    enforce.MergeNone,
		Strategy: strategy,
	}
}

// withQuery returns a deep copy of params with param set to query.
func withQuery(params url.Values, param, query string) url.Values {
	c := make(url.Values, len(params))
	for k, v := range params {
		c[k] = slices.Clone(v)
	}
	c.Set(param, query)
	return c
}

func rewriteErr(err error) error {
	if errors.Is(err, lq.ErrLiftUnsupported) || errors.Is(err, lq.ErrNoAnchor) {
		return enforce.Errorf(enforce.ErrUnsupported, "%v", err)
	}
	// Anything else is an internal failure of the rewriter; fail closed.
	return enforce.Errorf(enforce.ErrUnsupported, "rewrite failed: %v", err)
}

func convert(ms []policy.Matcher) ([]lq.InjectMatcher, error) {
	out := make([]lq.InjectMatcher, len(ms))
	for i, m := range ms {
		var op string
		switch m.Op {
		case v1.MatchEqual:
			op = "="
		case v1.MatchNotEqual:
			op = "!="
		case v1.MatchRegexMatch:
			op = "=~"
		case v1.MatchRegexNoMatch:
			op = "!~"
		default:
			return nil, enforce.Errorf(enforce.ErrUnsupported, "unknown matcher operator %q", m.Op)
		}
		out[i] = lq.InjectMatcher{Name: m.Name, Op: op, Value: m.Value}
	}
	return out, nil
}
