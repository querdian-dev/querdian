// Package promql implements the PromQL enforcer (PLAN §7.2). It parses queries with the Prometheus
// parser, rejects references to denied labels and injects the matchers of the effective policy into
// every vector selector. The query is never rewritten with string operations.
//
// A policy with one term is injected into every selector (T2.1), several terms are or-lifted
// (T2.2, union.go), and the metadata endpoints series, labels and label values get their match[]
// parameters expanded (T2.3, metadata.go).
package promql

import (
	"context"
	"net/url"
	"slices"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
)

// queryParam is the only enforcement parameter handled by this enforcer.
const queryParam = "query"

// metricNameLabel is the label that carries the metric name.
const metricNameLabel = "__name__"

// promParser uses the default options: experimental functions, duration expressions, extended range
// selectors and fill modifiers are not enabled, so queries using them fail to parse (fail closed).
var promParser = parser.NewParser(parser.Options{})

type enforcer struct{}

// New returns the PromQL enforcer for query, query_range and query_exemplars (enforcement parameter
// "query") and for series, labels and label/:name/values (enforcement parameter "match[]").
func New() enforce.Enforcer { return enforcer{} }

// Plan implements enforce.Enforcer for the query endpoints (enforcement parameter "query") and the
// metadata endpoints (enforcement parameter "match[]", see metadata.go).
//
// Query endpoints return a single upstream request whose "query" parameter carries the policy: with
// one term its matchers are appended to every vector selector (strategy "inject"); with several terms
// the query is or-lifted (strategy "or_lift", see lifter). All other parameters are copied unchanged.
// A policy without any restriction (Unrestricted, or a term without matchers) leaves the query as is.
func (enforcer) Plan(_ context.Context, ep routes.Endpoint, params url.Values, eff policy.Effective) (enforce.Plan, error) {
	switch ep.Param {
	case queryParam:
		return planQuery(params, eff)
	}
	return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "promql enforcer does not handle enforcement parameter %q", ep.Param)
}

func planQuery(params url.Values, eff policy.Effective) (enforce.Plan, error) {
	vals := params[queryParam]
	if len(vals) != 1 {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrParse, "exactly one %q parameter required, got %d", queryParam, len(vals))
	}
	expr, err := promParser.ParseExpr(vals[0])
	if err != nil {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrParse, "%v", err)
	}
	if err := checkDenied(expr, eff.DeniedLabels); err != nil {
		return enforce.Plan{}, err
	}
	if len(eff.Terms) == 0 && !eff.Unrestricted {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "effective policy grants no terms")
	}

	out := vals[0]
	strategy := "inject"
	switch {
	case eff.Unrestricted, unrestrictedTerm(eff.Terms):
		// Nothing to enforce: the query already sees everything the tenant header allows.
		if len(eff.Terms) > 1 {
			strategy = "passthrough"
		}
	case len(eff.Terms) == 1:
		ms, err := toLabelMatchers(eff.Terms[0].Matchers)
		if err != nil {
			return enforce.Plan{}, err
		}
		inject(expr, ms)
		out = expr.String()
	default:
		strategy = "or_lift"
		lifted, err := liftTerms(expr, eff.Terms)
		if err != nil {
			return enforce.Plan{}, err
		}
		out = lifted.String()
	}
	if out != vals[0] {
		// Defence in depth: the output must be valid PromQL, otherwise fail closed.
		if _, err := promParser.ParseExpr(out); err != nil {
			return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "rewritten query does not re-parse: %v", err)
		}
	}

	q := make(url.Values, len(params))
	for k, v := range params {
		q[k] = slices.Clone(v)
	}
	q.Set(queryParam, out)
	return enforce.Plan{
		Requests: []enforce.Upstream{{Query: q, Term: -1}},
		Merge:    enforce.MergeNone,
		Strategy: strategy,
	}, nil
}

// unrestrictedTerm reports whether some term has no matchers. Such a term (an unrestricted rule of a
// tenant, without a __tenant_id__ binding) admits everything the tenant header allows, so the union
// of all terms is unrestricted and the other terms are redundant.
func unrestrictedTerm(terms []policy.Term) bool {
	return slices.ContainsFunc(terms, func(t policy.Term) bool { return len(t.Matchers) == 0 })
}

// toLabelMatchers maps policy matchers to Prometheus matchers. Prometheus anchors regexes itself.
func toLabelMatchers(in []policy.Matcher) ([]*labels.Matcher, error) {
	out := make([]*labels.Matcher, 0, len(in))
	for _, m := range in {
		var t labels.MatchType
		switch m.Op {
		case v1.MatchEqual:
			t = labels.MatchEqual
		case v1.MatchNotEqual:
			t = labels.MatchNotEqual
		case v1.MatchRegexMatch:
			t = labels.MatchRegexp
		case v1.MatchRegexNoMatch:
			t = labels.MatchNotRegexp
		default:
			return nil, enforce.Errorf(enforce.ErrUnsupported, "unknown matcher operator %q", m.Op)
		}
		lm, err := labels.NewMatcher(t, m.Name, m.Value)
		if err != nil {
			return nil, enforce.Errorf(enforce.ErrUnsupported, "invalid policy matcher %s: %v", m.Name, err)
		}
		out = append(out, lm)
	}
	return out, nil
}

// inject appends ms to every vector selector in the tree, including those inside matrix selectors
// and subqueries. Offset and @ modifiers live on the selector and are preserved.
func inject(expr parser.Expr, ms []*labels.Matcher) {
	nameMatcher := slices.ContainsFunc(ms, func(m *labels.Matcher) bool { return m.Name == metricNameLabel })
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		vs, ok := n.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		vs.LabelMatchers = append(vs.LabelMatchers, ms...)
		if nameMatcher {
			// "up{__name__=~..}" is not valid PromQL: the metric name must not be set twice. The
			// equality matcher for the name stays in LabelMatchers, so dropping Name is lossless.
			vs.Name = ""
		}
		return nil
	})
}

// checkDenied rejects expressions that reference a denied label in a matcher, a grouping, a
// vector-matching list or as a label-name argument of label_replace, label_join or count_values.
func checkDenied(expr parser.Expr, denied []string) error {
	if len(denied) == 0 {
		return nil
	}
	var firstErr error
	deny := func(name, where string) {
		if firstErr == nil && slices.Contains(denied, name) {
			firstErr = enforce.Errorf(enforce.ErrDeniedLabel, "label %s denied by policy (%s)", name, where)
		}
	}
	denyAll := func(names []string, where string) {
		for _, n := range names {
			deny(n, where)
		}
	}
	// denyStr applies deny to a string-literal argument. A non-literal string expression cannot be
	// produced by the parser today; it is rejected fail-closed should it ever appear.
	denyStr := func(e parser.Expr, fn string) {
		for {
			p, ok := e.(*parser.ParenExpr)
			if !ok {
				break
			}
			e = p.Expr
		}
		sl, ok := e.(*parser.StringLiteral)
		if !ok {
			if firstErr == nil {
				firstErr = enforce.Errorf(enforce.ErrDeniedLabel, "label name argument of %s is not a string literal", fn)
			}
			return
		}
		deny(sl.Val, fn)
	}
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		switch n := n.(type) {
		case *parser.VectorSelector:
			for _, m := range n.LabelMatchers {
				deny(m.Name, "matcher")
			}
		case *parser.AggregateExpr:
			denyAll(n.Grouping, "by/without")
			if n.Op == parser.COUNT_VALUES {
				denyStr(n.Param, "count_values")
			}
		case *parser.BinaryExpr:
			if n.VectorMatching != nil {
				denyAll(n.VectorMatching.MatchingLabels, "on/ignoring")
				denyAll(n.VectorMatching.Include, "group_left/group_right")
			}
		case *parser.Call:
			var idx []int
			switch n.Func.Name {
			case "label_replace":
				idx = []int{1, 3}
			case "label_join":
				idx = []int{1}
				for i := 2; i < len(n.Args); i++ {
					idx = append(idx, i)
				}
			}
			for _, i := range idx {
				if i < len(n.Args) {
					denyStr(n.Args[i], n.Func.Name)
				}
			}
		}
		return nil
	})
	return firstErr
}
