// Package traceql implements enforcement for Tempo (PLAN §7.4): denied-attribute checks and
// injection of the effective policy into every spanset filter of a TraceQL query.
//
// Handled endpoints (by route template): /api/search, /api/v2/search/tags,
// /api/v2/search/tag/:name/values, /api/metrics/query and /api/metrics/query_range. All use the
// parameter q. Trace-by-ID (routes.KindTraceByID) is planned by NewWithOptions enforcers only (see
// tracebyid.go); New denies it.
//
// # Decisions
//
//   - Denied attributes. Policy deny entries are scoped names (span.x, resource.x). A query token
//     is denied when its scope equals the denied scope or is absent and the name is equal:
//     the unscoped form .x is denied by both span.x and resource.x, because Tempo resolves it to
//     either scope. Quoted names are compared decoded. Intrinsic tokens are compared the same way
//     (span:name is "span", "name"), which only matters for a deny entry that names an intrinsic.
//     The tag-values path parameter is checked like a token. The check runs for restricted and
//     unrestricted users alike.
//   - Legacy search parameters tags, minDuration and maxDuration are rejected (ErrUnsupported)
//     whether or not q is present: they are evaluated outside TraceQL and cannot carry the
//     policy.
//   - q is optional on the tag endpoints and on search. When it is absent (or empty) a
//     restricted user gets q={ dnf }, never a pass-through; an unrestricted user gets no q. The
//     metrics endpoints require q (ErrUnsupported otherwise) because a policy-only filter is not
//     a metrics query.
//   - A query without any spanset filter (`count() > 10`, which Tempo runs as `{ } | count() > 10`)
//     is accepted and gets the prefix `{ dnf } | ` (pkg/traceql.Inject).
//   - Several terms are fine: the DNF covers them and TraceQL needs no fan-out. A term bound to
//     a tenant or containing a __tenant_id__ matcher is rejected (ErrUnsupported): Tempo has no
//     tenant-id attribute (federation none).
//   - A term without matchers matches everything, so it makes the policy a no-op for injection
//     (denied attributes are still checked).
//   - Policy NotEqual/RegexNoMatch render as != and !~. In TraceQL a span without the attribute
//     does not satisfy these, whereas the policy semantics (absent equals empty) would admit it.
//     The rendering can therefore only under-grant, never over-grant.
package traceql

import (
	"context"
	"net/url"
	"slices"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
	"github.com/querdian-dev/querdian/pkg/traceql"
)

// Route templates handled by this enforcer.
const (
	tmplSearch      = "/api/search"
	tmplTags        = "/api/v2/search/tags"
	tmplTagValues   = "/api/v2/search/tag/:name/values"
	tmplMetrics     = "/api/metrics/query"
	tmplMetricsFull = "/api/metrics/query_range"
)

// legacyParams are the pre-TraceQL search filters that are always rejected.
var legacyParams = []string{"tags", "minDuration", "maxDuration"}

type enforcer struct{ opt Options }

// New returns the TraceQL enforcer for Tempo backends with trace-by-ID denied (see NewWithOptions).
func New() enforce.Enforcer { return enforcer{} }

// Plan implements enforce.Enforcer. It returns a single upstream request: search uses
// enforce.MergeTempoSearch (response redaction, T4.4), every other endpoint enforce.MergeNone.
// Strategy is "inject", or "passthrough" for unrestricted users (denied attributes are still
// checked).
func (e enforcer) Plan(_ context.Context, ep routes.Endpoint, params url.Values, eff policy.Effective) (enforce.Plan, error) {
	if ep.Kind == routes.KindTraceByID {
		return e.planTraceByID(ep, params, eff)
	}
	if ep.Kind != routes.KindEnforce {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "endpoint %q is not a TraceQL enforcement endpoint", ep.Template)
	}
	merge := enforce.MergeNone
	required := false // q must be present
	switch ep.Template {
	case tmplSearch:
		merge = enforce.MergeTempoSearch
	case tmplTags:
	case tmplTagValues:
		if traceLevelNames[ep.PathParam] && !RootCovered(eff) {
			return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "trace-level intrinsic %s needs a policy that covers the root span", ep.PathParam)
		}
		scope, name := traceql.SplitAttr(ep.PathParam)
		if denied(eff.DeniedLabels, scope, name) {
			return enforce.Plan{}, enforce.Errorf(enforce.ErrDeniedLabel, "attribute %s denied by policy", ep.PathParam)
		}
	case tmplMetrics, tmplMetricsFull:
		required = true
	default:
		return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "endpoint %q is not handled by the TraceQL enforcer", ep.Template)
	}

	for _, k := range legacyParams {
		if _, ok := params[k]; ok {
			return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "legacy parameter %q is not supported", k)
		}
	}
	for k := range params {
		if len(ep.AllowedParams) > 0 && !ep.AllowsParam(k) {
			return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "parameter %q is not supported", k)
		}
	}
	qs := params[ep.Param]
	if len(qs) > 1 {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "parameter %q given more than once", ep.Param)
	}
	var query string
	if len(qs) == 1 {
		query = qs[0]
	}

	var parsed *traceql.Query
	if query != "" {
		var err error
		parsed, err = traceql.Parse(query)
		if err != nil {
			return enforce.Plan{}, enforce.Errorf(enforce.ErrParse, "%v", err)
		}
		for _, tok := range parsed.Attributes() {
			if tok.Kind == traceql.KindIntrinsic && isTraceLevel(tok.Scope, tok.Value) && !RootCovered(eff) {
				return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "trace-level intrinsic %s needs a policy that covers the root span", tok.Text)
			}
			if denied(eff.DeniedLabels, tok.Scope, tok.Value) {
				return enforce.Plan{}, enforce.Errorf(enforce.ErrDeniedLabel, "attribute %s denied by policy", tok.Text)
			}
		}
	} else if required {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "parameter %q is required", ep.Param)
	}

	out := cloneValues(params)
	strategy := "passthrough"
	if !eff.Unrestricted {
		dnf, all, err := policyDNF(eff)
		if err != nil {
			return enforce.Plan{}, err
		}
		if !all {
			strategy = "inject"
			if parsed == nil {
				out.Set(ep.Param, "{ "+dnf+" }")
			} else {
				rewritten, err := traceql.Inject(parsed, dnf)
				if err != nil {
					return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "cannot inject policy: %v", err)
				}
				out.Set(ep.Param, rewritten)
			}
		}
	}
	return enforce.Plan{
		Requests: []enforce.Upstream{{Query: out, Term: -1}},
		Merge:    merge,
		Strategy: strategy,
	}, nil
}

// traceLevelNames are the intrinsics evaluated against the whole trace (root span or trace extent). Used in a
// filter, grouping, select() or as a tag-value name they reveal data of spans the policy may not grant
// (the redacted rootServiceName, rootTraceName and trace extent) although only a matching child span is
// listed: an oracle (docs/research/traceql.md section 8, verified by the e2e suite). They are rejected
// unless RootCovered.
var traceLevelNames = map[string]bool{
	"rootName": true, "rootServiceName": true, "traceDuration": true,
	"trace:rootName": true, "trace:rootService": true, "trace:duration": true,
}

func isTraceLevel(scope, name string) bool {
	if scope == "" {
		return traceLevelNames[name]
	}
	return scope == "trace" && traceLevelNames["trace:"+name]
}

// denied reports whether the attribute (scope, name) is covered by one of the denied names.
// An unscoped attribute is denied by a denied name of any scope.
func denied(deniedNames []string, scope, name string) bool {
	for _, d := range deniedNames {
		ds, dn := traceql.SplitAttr(d)
		if dn == name && (scope == "" || ds == "" || scope == ds) {
			return true
		}
	}
	return false
}

// policyDNF renders the effective terms. all is true when some term has no matchers, i.e. the
// policy admits every span and nothing needs to be injected.
func policyDNF(eff policy.Effective) (dnf string, all bool, err error) {
	if len(eff.Terms) == 0 {
		return "", false, enforce.Errorf(enforce.ErrUnsupported, "effective policy has no terms")
	}
	terms := make([]traceql.Term, len(eff.Terms))
	for i, t := range eff.Terms {
		if t.Tenant != "" {
			return "", false, enforce.Errorf(enforce.ErrUnsupported, "tenant-bound terms are not supported on Tempo")
		}
		if len(t.Matchers) == 0 {
			all = true
			continue
		}
		term := make(traceql.Term, len(t.Matchers))
		for j, m := range t.Matchers {
			if m.Name == policy.TenantIDLabel {
				return "", false, enforce.Errorf(enforce.ErrUnsupported, "%s matchers are not supported on Tempo", policy.TenantIDLabel)
			}
			op, ok := ops[m.Op]
			if !ok {
				return "", false, enforce.Errorf(enforce.ErrUnsupported, "unsupported matcher operator %q", m.Op)
			}
			term[j] = traceql.Condition{Attr: m.Name, Op: op, Value: m.Value}
		}
		terms[i] = term
	}
	if all {
		return "", true, nil
	}
	dnf, err = traceql.RenderDNF(terms)
	if err != nil {
		return "", false, enforce.Errorf(enforce.ErrUnsupported, "cannot render policy: %v", err)
	}
	return dnf, false, nil
}

var ops = map[v1.MatchOp]traceql.Kind{
	v1.MatchEqual:        traceql.KindEq,
	v1.MatchNotEqual:     traceql.KindNeq,
	v1.MatchRegexMatch:   traceql.KindRegex,
	v1.MatchRegexNoMatch: traceql.KindNotRegex,
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v)+1)
	for k, vs := range v {
		out[k] = slices.Clone(vs)
	}
	return out
}
