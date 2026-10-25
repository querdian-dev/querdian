package traceql

// # Tag lookups (GHSA-jcqw-3c3f-xrmj)
//
// Tempo does not evaluate every q on /api/v2/search/tags and /api/v2/search/tag/:name/values. For a filter
// shape it cannot use there it answers unfiltered, and the injected policy is dropped with the rest of q
// (black-box: Tempo 2.9.0 and 2.10.10 for terms joined by ||, Tempo 3.1.1 for =~). Injecting the policy
// DNF as for search therefore leaked tag names and values of spans outside the policy.
//
// For restricted users the tag endpoints only send the shape that every tested version evaluates: one
// spanset filter that is a conjunction of `attribute = "string"` conditions, without parentheses.
//
//   - The policy is planned per term: one upstream request per term, united by the Tempo tags merger
//     (enforce.MergeTempoTags). A single request keeps enforce.MergeNone.
//   - A regex matcher is expanded into equalities when it matches a finite set of strings (the merged
//     equalities "(?:a)|(?:b)" of internal/policy, or "ns-(a|c)"); the expansion uses Go's regexp parser,
//     so it has exactly the full-match semantics of the policy. Other regexes and the negative matchers
//     (!=, !~) cannot be expressed and make the lookup ErrUnsupported. An empty string in an expansion
//     renders as `= ""`, which a span without the attribute does not satisfy: it can only under-grant.
//   - More than maxTagRequests requests are ErrTooComplex.
//   - A user q is kept when it is one spanset filter whose conditions are `span.x = "v"`, `resource.x =
//     "v"` or `status = error|ok|unset` joined by &&; it is added to every request. Any other q is dropped:
//     the answer then lists every tag of the granted spans, a superset of the requested one, never more.

import (
	"net/url"
	"regexp/syntax"
	"slices"
	"strings"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/pkg/traceql"
)

// maxTagRequests caps the upstream requests of one tag lookup (policy terms after regex expansion).
const maxTagRequests = 32

// planTagLookup plans a restricted tag lookup: one request per q of tagQueries, the other parameters of
// params unchanged.
func planTagLookup(param string, params url.Values, parsed *traceql.Query, eff policy.Effective) (enforce.Plan, error) {
	qs, err := tagQueries(parsed, eff)
	if err != nil {
		return enforce.Plan{}, err
	}
	p := enforce.Plan{Merge: enforce.MergeNone, Strategy: "inject"}
	if len(qs) > 1 {
		p.Merge, p.Strategy = enforce.MergeTempoTags, "fanout"
	}
	for i, q := range qs {
		v := cloneValues(params)
		v.Set(param, q)
		term := i
		if len(qs) == 1 {
			term = -1
		}
		p.Requests = append(p.Requests, enforce.Upstream{Query: v, Term: term})
	}
	return p, nil
}

// tagQueries returns the q of every upstream request of a restricted tag lookup. parsed is the user q
// (nil when absent); eff must be restricted and have at least one term with matchers.
func tagQueries(parsed *traceql.Query, eff policy.Effective) ([]string, error) {
	var terms []traceql.Term
	for _, t := range eff.Terms {
		expanded, err := expandTerm(t)
		if err != nil {
			return nil, err
		}
		terms = append(terms, expanded...)
		if len(terms) > maxTagRequests {
			return nil, enforce.Errorf(enforce.ErrTooComplex, "tag lookup needs more than %d upstream requests for this policy", maxTagRequests)
		}
	}
	user := userConditions(parsed)
	qs := make([]string, 0, len(terms))
	for _, t := range terms {
		conj, err := traceql.RenderConjunction(t)
		if err != nil {
			return nil, enforce.Errorf(enforce.ErrUnsupported, "cannot render policy: %v", err)
		}
		q := "{ " + strings.Join(append(slices.Clone(user), conj), " && ") + " }"
		if !slices.Contains(qs, q) {
			qs = append(qs, q)
		}
	}
	return qs, nil
}

// expandTerm turns one policy term into equality-only terms (the cross product of the expanded matchers).
func expandTerm(t policy.Term) ([]traceql.Term, error) {
	out := []traceql.Term{{}}
	for _, m := range t.Matchers {
		var values []string
		switch m.Op {
		case v1.MatchEqual:
			values = []string{m.Value}
		case v1.MatchRegexMatch:
			vs, ok := literalSet(m.Value, maxTagRequests)
			if !ok {
				return nil, enforce.Errorf(enforce.ErrUnsupported, "tag lookups support only policies whose regexes match a fixed set of values (%s =~ %q)", m.Name, m.Value)
			}
			values = vs
		default:
			return nil, enforce.Errorf(enforce.ErrUnsupported, "tag lookups do not support the policy matcher %s %s", m.Name, m.Op)
		}
		next := make([]traceql.Term, 0, len(out)*len(values))
		for _, prefix := range out {
			for _, v := range values {
				next = append(next, append(slices.Clone(prefix), traceql.Condition{Attr: m.Name, Op: traceql.KindEq, Value: v}))
			}
		}
		if len(next) > maxTagRequests {
			return nil, enforce.Errorf(enforce.ErrTooComplex, "tag lookup needs more than %d upstream requests for this policy", maxTagRequests)
		}
		out = next
	}
	return out, nil
}

// literalSet returns the strings a fully anchored regex matches when that set is finite and has at most
// limit members. Case folding, repetition, anchors and every other construct report false.
func literalSet(expr string, limit int) ([]string, bool) {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return nil, false
	}
	set, ok := expand(re.Simplify(), limit)
	if !ok {
		return nil, false
	}
	slices.Sort(set)
	return slices.Compact(set), true
}

func expand(re *syntax.Regexp, limit int) ([]string, bool) {
	switch re.Op {
	case syntax.OpEmptyMatch:
		return []string{""}, true
	case syntax.OpLiteral:
		if re.Flags&syntax.FoldCase != 0 {
			return nil, false
		}
		return []string{string(re.Rune)}, true
	case syntax.OpCharClass:
		var out []string
		for i := 0; i+1 < len(re.Rune); i += 2 {
			for r := re.Rune[i]; r <= re.Rune[i+1]; r++ {
				if len(out) == limit {
					return nil, false
				}
				out = append(out, string(r))
			}
		}
		return out, true
	case syntax.OpCapture:
		return expand(re.Sub[0], limit)
	case syntax.OpQuest:
		sub, ok := expand(re.Sub[0], limit)
		if !ok || len(sub) == limit {
			return nil, false
		}
		return append(sub, ""), true
	case syntax.OpAlternate:
		var out []string
		for _, s := range re.Sub {
			sub, ok := expand(s, limit)
			if !ok || len(out)+len(sub) > limit {
				return nil, false
			}
			out = append(out, sub...)
		}
		return out, true
	case syntax.OpConcat:
		out := []string{""}
		for _, s := range re.Sub {
			sub, ok := expand(s, limit)
			if !ok || len(out)*len(sub) > limit {
				return nil, false
			}
			next := make([]string, 0, len(out)*len(sub))
			for _, p := range out {
				for _, x := range sub {
					next = append(next, p+x)
				}
			}
			out = next
		}
		return out, true
	default:
		return nil, false
	}
}

// userConditions returns the rendered conditions of a user q that has the shape Tempo evaluates on the tag
// endpoints, and nil otherwise (see the package notes in this file).
func userConditions(q *traceql.Query) []string {
	if q == nil || len(q.Pipeline.Stages) != 1 || len(q.Filters) != 1 || len(q.With) != 0 {
		return nil
	}
	f, ok := q.Pipeline.Stages[0].(*traceql.SpansetFilter)
	if !ok || f.Expr == nil {
		return nil
	}
	var conds []string
	if !collect(f.Expr, &conds) {
		return nil
	}
	return conds
}

func collect(e traceql.Expr, out *[]string) bool {
	switch x := e.(type) {
	case *traceql.BinaryExpr:
		if x.Op == traceql.KindAnd {
			return collect(x.Left, out) && collect(x.Right, out)
		}
		if x.Op != traceql.KindEq {
			return false
		}
		l, lok := x.Left.(*traceql.OperandExpr)
		r, rok := x.Right.(*traceql.OperandExpr)
		if !lok || !rok || l.Neg >= 0 || r.Neg >= 0 {
			return false
		}
		switch {
		case l.Tok.Kind == traceql.KindAttribute && (l.Tok.Scope == "span" || l.Tok.Scope == "resource") && r.Tok.Kind == traceql.KindString:
			s, err := traceql.RenderConjunction(traceql.Term{{Attr: l.Tok.Scope + "." + l.Tok.Value, Op: traceql.KindEq, Value: r.Tok.Value}})
			if err != nil {
				return false
			}
			*out = append(*out, s)
			return true
		case l.Tok.Kind == traceql.KindIntrinsic && l.Tok.Value == "status" && (l.Tok.Scope == "" || l.Tok.Scope == "span") &&
			r.Tok.Kind == traceql.KindIdent && (r.Tok.Text == "error" || r.Tok.Text == "ok" || r.Tok.Text == "unset"):
			*out = append(*out, "status = "+r.Tok.Text)
			return true
		}
		return false
	default:
		return false
	}
}
