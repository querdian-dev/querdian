package logql

import (
	"net/url"
	"slices"

	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
	lq "github.com/querdian-dev/querdian/pkg/logql"
)

// Route templates of the endpoints handled outside of the query/query_range path (see routes.Table).
const (
	tmplLabels         = "/loki/api/v1/labels"
	tmplLabelValues    = "/loki/api/v1/label/:name/values"
	tmplDetectedLabels = "/loki/api/v1/detected_labels"
	tmplDetectedFields = "/loki/api/v1/detected_fields"
	tmplDetectedValues = "/loki/api/v1/detected_field/:name/values"
	tmplSeries         = "/loki/api/v1/series"
)

// branch is one upstream request of a fanned-out metadata endpoint.
type branch struct {
	query string // value of the enforcement parameter
	omit  bool   // send no enforcement parameter at all (unrestricted grant, none from the client)
	term  int    // policy term, -1 when not fanned out
}

// planStringSet handles labels, label/:name/values, detected_labels, detected_fields and
// detected_field/:name/values. The optional "query" is a log selector; when the client omits it the policy
// selector of each term is sent instead (omission is never a pass-through for a restricted grant). Several
// terms fan out one request per term, merged with MergeStringSet (union, sorted, denied labels removed).
//
// A denied label (or field) name in the request path is enforce.ErrDeniedLabel. With denied labels the
// merge is also used for a single request so that the response can be filtered.
func planStringSet(ep routes.Endpoint, params url.Values, eff policy.Effective) (enforce.Plan, error) {
	if ep.PathParam != "" && slices.Contains(eff.DeniedLabels, ep.PathParam) {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrDeniedLabel, "label %s denied by policy", ep.PathParam)
	}
	branches, strategy, err := optionalQueryBranches(params, "query", eff)
	if err != nil {
		return enforce.Plan{}, err
	}
	return branchPlan(params, "query", branches, strategy, enforce.MergeStringSet, len(eff.DeniedLabels) > 0), nil
}

// optionalQueryBranches builds the per-term values of the optional log selector parameter param. An absent
// or empty parameter counts as omitted.
func optionalQueryBranches(params url.Values, param string, eff policy.Effective) ([]branch, string, error) {
	var q *lq.Query
	if raw := params.Get(param); raw != "" {
		var err error
		if q, err = parseSelectorQuery(raw, eff.DeniedLabels); err != nil {
			return nil, "", err
		}
	}
	if eff.Unrestricted {
		switch {
		case q == nil:
			return []branch{{omit: true, term: -1}}, "passthrough", nil
		case len(eff.DeniedLabels) > 0:
			return []branch{{query: q.String(), term: -1}}, "passthrough", nil
		}
		return []branch{{query: params.Get(param), term: -1}}, "passthrough", nil
	}
	terms, err := injectTerms(eff)
	if err != nil {
		return nil, "", err
	}
	out := make([]branch, len(terms))
	for i, ms := range terms {
		var s string
		if q == nil {
			s, err = lq.PolicySelector(ms)
		} else {
			s, err = lq.Inject(q, ms)
		}
		if err != nil {
			return nil, "", rewriteErr(err)
		}
		out[i] = branch{query: s, term: i}
		if len(terms) == 1 {
			out[i].term = -1
		}
	}
	if len(terms) == 1 {
		return out, "inject", nil
	}
	return out, "fanout", nil
}

// parseSelectorQuery parses a client-supplied selector parameter: it must be a log query (stream selector with
// optional pipeline), not a metric query, and must not reference denied labels.
func parseSelectorQuery(raw string, denied []string) (*lq.Query, error) {
	q, err := lq.Parse(raw)
	if err != nil {
		return nil, enforce.Errorf(enforce.ErrParse, "%v", err)
	}
	if q.Kind != lq.KindLog {
		return nil, enforce.Errorf(enforce.ErrUnsupported, "a log stream selector is required, not a metric query")
	}
	label, found, err := lq.DeniedLabel(q, denied)
	if err != nil {
		return nil, enforce.Errorf(enforce.ErrParse, "%v", err)
	}
	if found {
		return nil, enforce.Errorf(enforce.ErrDeniedLabel, "label %s denied by policy", label)
	}
	return q, nil
}

// injectTerms converts the policy terms of a restricted grant.
func injectTerms(eff policy.Effective) ([][]lq.InjectMatcher, error) {
	if len(eff.Terms) == 0 {
		return nil, enforce.Errorf(enforce.ErrUnsupported, "effective policy has no terms")
	}
	terms := make([][]lq.InjectMatcher, len(eff.Terms))
	for i, t := range eff.Terms {
		ms, err := convert(t.Matchers)
		if err != nil {
			return nil, err
		}
		terms[i] = ms
	}
	return terms, nil
}

// branchPlan turns branches into a plan. A single branch is streamed unmerged unless needMerge is set; several
// branches always carry merge.
func branchPlan(params url.Values, param string, branches []branch, strategy string, merge enforce.MergeKind, needMerge bool) enforce.Plan {
	p := enforce.Plan{Strategy: strategy, Merge: enforce.MergeNone}
	if len(branches) > 1 || needMerge {
		p.Merge = merge
	}
	for _, b := range branches {
		c := withQuery(params, param, b.query)
		if b.omit {
			c.Del(param)
		}
		p.Requests = append(p.Requests, enforce.Upstream{Query: c, Term: b.term})
	}
	return p
}

// planSeries handles series: one match[] per (user match[] x term), or one per term when the client sends
// none, in a single request (Loki returns the union of all selectors), strategy "match_expand". Unrestricted
// grants keep the user's match[] as sent (canonical text when labels are denied).
func planSeries(params url.Values, eff policy.Effective) (enforce.Plan, error) {
	const param = "match[]"
	var users []*lq.Query
	for _, raw := range params[param] {
		q, err := parseSelectorQuery(raw, eff.DeniedLabels)
		if err != nil {
			return enforce.Plan{}, err
		}
		users = append(users, q)
	}
	c := make(url.Values, len(params))
	for k, v := range params {
		c[k] = slices.Clone(v)
	}
	strategy := "passthrough"
	if eff.Unrestricted {
		if len(eff.DeniedLabels) > 0 {
			c.Del(param)
			for _, q := range users {
				c.Add(param, q.String())
			}
		}
	} else {
		terms, err := injectTerms(eff)
		if err != nil {
			return enforce.Plan{}, err
		}
		c.Del(param)
		for _, ms := range terms {
			if len(users) == 0 {
				s, err := lq.PolicySelector(ms)
				if err != nil {
					return enforce.Plan{}, rewriteErr(err)
				}
				c.Add(param, s)
			}
			for _, q := range users {
				s, err := lq.Inject(q, ms)
				if err != nil {
					return enforce.Plan{}, rewriteErr(err)
				}
				c.Add(param, s)
			}
		}
		strategy = "match_expand"
	}
	plan := enforce.Plan{
		Requests: []enforce.Upstream{{Query: c, Term: -1}},
		Merge:    enforce.MergeNone,
		Strategy: strategy,
	}
	if len(eff.DeniedLabels) > 0 {
		denied := slices.Clone(eff.DeniedLabels)
		plan.ResponseFilterMaxBytes = seriesFilterMaxBytes
		plan.ResponseFilter = func(body []byte) ([]byte, error) {
			return enforce.FilterSeriesResponse(body, denied, seriesFilterMaxBytes)
		}
	}
	return plan, nil
}

// seriesFilterMaxBytes bounds buffering of a series response that is filtered for denied labels.
const seriesFilterMaxBytes = 16 << 20
