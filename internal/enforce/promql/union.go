package promql

import (
	"slices"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
)

// maxSelectorCopies caps (selectors in the query) x (policy terms) of an or-lifted query so that a
// single request cannot be amplified into an arbitrarily large upstream query.
const maxSelectorCopies = 2048

// lifter rewrites an expression for a policy with several terms (PLAN §7.2, "or-lifting").
//
// Exactness. For an instant selector v the lift (v{t1} or v{t2} or ...) is exactly v restricted to
// the union of the terms: a series matching several terms appears on several sides with identical
// labels and an identical sample, and "or" keeps one of them. For a function over a range vector the
// whole call is copied per term, f(m{t1}) or f(m{t2}); this is exact because such functions work per
// series. It is NOT exact, and the query is rejected with enforce.ErrUnsupported, when
//   - a range vector is not the direct argument of a function call (for example a top-level
//     "up[5m]"), because "or" is not defined on range vectors;
//   - a function over a range vector has another argument that contains a selector (for example
//     quantile_over_time(scalar(x), m)): that argument would have to see the union of all terms but
//     the copy of term i can only carry term i.
//
// Two documented deviations remain, neither of which can grant more than the policy allows:
//   - absent(v) and absent_over_time(m) lose the label inference from the equality matchers of the
//     user selector, because the argument is no longer a plain selector. Their value stays exact
//     ("1 iff no series of the union exists"); absent_over_time(m) is rewritten to
//     absent(last_over_time(m{t1}) or last_over_time(m{t2}) ...) for that reason.
//   - A function that drops the metric name (rate, ...) fails upstream when two distinct series differ
//     only in their name. With "or" such a collision across terms is resolved silently instead of
//     failing, which can happen only if a term itself matches on __name__.
type lifter struct {
	terms  [][]*labels.Matcher
	copies int // selector copies emitted so far
}

// liftTerms rewrites expr for the given terms (at least two, none empty) and returns the new tree.
// The input tree is not modified.
func liftTerms(expr parser.Expr, terms []policy.Term) (parser.Expr, error) {
	l := &lifter{}
	for _, t := range terms {
		ms, err := toLabelMatchers(t.Matchers)
		if err != nil {
			return nil, err
		}
		l.terms = append(l.terms, ms)
	}
	return l.lift(expr)
}

func (l *lifter) lift(e parser.Expr) (parser.Expr, error) {
	switch n := e.(type) {
	case *parser.VectorSelector:
		return l.liftSelector(n)
	case *parser.MatrixSelector:
		return nil, enforce.Errorf(enforce.ErrUnsupported,
			"range vector selector %s outside of a function call cannot be combined across %d policy terms", n, len(l.terms))
	case *parser.Call:
		return l.liftCall(n)
	case *parser.SubqueryExpr:
		inner, err := l.lift(n.Expr)
		if err != nil {
			return nil, err
		}
		c := *n
		c.Expr = inner
		return &c, nil
	case *parser.AggregateExpr:
		c := *n
		var err error
		if c.Expr, err = l.lift(n.Expr); err != nil {
			return nil, err
		}
		if n.Param != nil {
			if c.Param, err = l.lift(n.Param); err != nil {
				return nil, err
			}
		}
		return &c, nil
	case *parser.BinaryExpr:
		c := *n
		var err error
		if c.LHS, err = l.lift(n.LHS); err != nil {
			return nil, err
		}
		if c.RHS, err = l.lift(n.RHS); err != nil {
			return nil, err
		}
		return &c, nil
	case *parser.ParenExpr:
		inner, err := l.lift(n.Expr)
		if err != nil {
			return nil, err
		}
		return &parser.ParenExpr{Expr: inner}, nil
	case *parser.UnaryExpr:
		inner, err := l.lift(n.Expr)
		if err != nil {
			return nil, err
		}
		return &parser.UnaryExpr{Op: n.Op, Expr: inner, StartPos: n.StartPos}, nil
	case *parser.NumberLiteral, *parser.StringLiteral:
		return e, nil
	}
	// StepInvariantExpr and any future node type: fail closed.
	return nil, enforce.Errorf(enforce.ErrUnsupported, "expression node %T is not supported with %d policy terms", e, len(l.terms))
}

// copySelector returns a copy of vs that additionally carries the matchers ms.
func copySelector(vs *parser.VectorSelector, ms []*labels.Matcher) *parser.VectorSelector {
	c := *vs
	c.LabelMatchers = append(slices.Clone(vs.LabelMatchers), ms...)
	if slices.ContainsFunc(ms, func(m *labels.Matcher) bool { return m.Name == metricNameLabel }) {
		c.Name = "" // see inject: the metric name must not be given twice
	}
	return &c
}

func (l *lifter) account() error {
	l.copies += len(l.terms)
	if l.copies > maxSelectorCopies {
		return enforce.Errorf(enforce.ErrTooComplex, "query needs more than %d selector copies for %d policy terms", maxSelectorCopies, len(l.terms))
	}
	return nil
}

// orAll joins exprs with "or" (left associative) and wraps the result in parentheses.
func orAll(exprs []parser.Expr) parser.Expr {
	acc := exprs[0]
	for _, e := range exprs[1:] {
		acc = &parser.BinaryExpr{
			Op:             parser.LOR,
			LHS:            acc,
			RHS:            e,
			VectorMatching: &parser.VectorMatching{Card: parser.CardManyToMany},
		}
	}
	return &parser.ParenExpr{Expr: acc}
}

func (l *lifter) liftSelector(vs *parser.VectorSelector) (parser.Expr, error) {
	if err := l.account(); err != nil {
		return nil, err
	}
	copies := make([]parser.Expr, len(l.terms))
	for i, ms := range l.terms {
		copies[i] = copySelector(vs, ms)
	}
	return orAll(copies), nil
}

func hasSelector(e parser.Expr) bool {
	found := false
	parser.Inspect(e, func(n parser.Node, _ []parser.Node) error {
		switch n.(type) {
		case *parser.VectorSelector, *parser.MatrixSelector:
			found = true
		}
		return nil
	})
	return found
}

func (l *lifter) liftCall(c *parser.Call) (parser.Expr, error) {
	mi := -1
	for i, a := range c.Args {
		if _, ok := a.(*parser.MatrixSelector); ok {
			if mi >= 0 {
				return nil, enforce.Errorf(enforce.ErrUnsupported, "%s with several range vector arguments is not supported with %d policy terms", c.Func.Name, len(l.terms))
			}
			mi = i
		}
	}
	if mi < 0 {
		out := *c
		out.Args = make([]parser.Expr, len(c.Args))
		for i, a := range c.Args {
			var err error
			if out.Args[i], err = l.lift(a); err != nil {
				return nil, err
			}
		}
		return &out, nil
	}

	for i, a := range c.Args {
		if i != mi && hasSelector(a) {
			return nil, enforce.Errorf(enforce.ErrUnsupported,
				"argument %d of %s contains a selector next to a range vector; this cannot be combined exactly across %d policy terms", i+1, c.Func.Name, len(l.terms))
		}
	}
	ms := c.Args[mi].(*parser.MatrixSelector) //nolint:forcetypeassert // checked above
	vs, ok := ms.VectorSelector.(*parser.VectorSelector)
	if !ok {
		return nil, enforce.Errorf(enforce.ErrUnsupported, "range vector of %s is not a plain selector", c.Func.Name)
	}
	if err := l.account(); err != nil {
		return nil, err
	}

	// absent_over_time(m) is only exact as a single absent() over the union of all terms; copying it
	// per term would report "absent" for every term that has no data while another term has some.
	fn := c.Func
	wrap := false
	if c.Func.Name == "absent_over_time" {
		fn = parser.Functions["last_over_time"]
		wrap = true
	}
	copies := make([]parser.Expr, len(l.terms))
	for i, tm := range l.terms {
		mc := *ms
		mc.VectorSelector = copySelector(vs, tm)
		cc := *c
		cc.Func = fn
		cc.Args = slices.Clone(c.Args)
		cc.Args[mi] = &mc
		copies[i] = &cc
	}
	res := orAll(copies)
	if wrap {
		return &parser.Call{Func: parser.Functions["absent"], Args: parser.Expressions{res}}, nil
	}
	return res, nil
}
