package traceql

import "strings"

// Query is a parsed TraceQL query. All offsets are byte offsets into Source.
type Query struct {
	// Source is the exact input text that was parsed.
	Source string
	// Tokens is the complete token stream of Source.
	Tokens []Token
	// Pipeline is the top-level pipeline; it always has at least one stage.
	Pipeline *Pipeline
	// Filters lists every spanset filter `{ ... }` in source order, including filters nested in
	// structural operators, spanset &&/||, parentheses, pipelines and compare(...).
	Filters []*SpansetFilter
	// With holds the query-level hints of a trailing `with (k=v, ...)` on a non-metrics query.
	With []WithOption
}

// Pipeline is a sequence of stages separated by '|'.
type Pipeline struct {
	// Stages are the pipeline elements in order.
	Stages []Stage
}

// Stage is one pipeline element: a spanset expression, scalar filter, by(), select(),
// coalesce(), a metrics function or topk/bottomk.
type Stage interface {
	// Span returns the byte range [start, end) of the stage in the source.
	Span() (start, end int)
	// String renders the stage canonically.
	String() string
	stage()
}

// SpansetExpr is a spanset expression: a filter, a parenthesised expression or a binary
// combination (spanset && / ||, structural operators).
type SpansetExpr interface {
	Stage
	spansetExpr()
}

// SpansetFilter is a `{ ... }` spanset filter.
type SpansetFilter struct {
	// Start is the offset of '{' and End the offset just past '}'.
	Start, End int
	// Tokens are the tokens strictly between the braces (empty for `{}`).
	Tokens []Token
	// Expr is the validated expression, nil for an empty filter.
	Expr Expr
}

// InnerRange returns the byte range between the braces, trimmed to the first and last inner
// token (for an empty filter it is the range just after '{' up to '}').
func (f *SpansetFilter) InnerRange() (start, end int) {
	if len(f.Tokens) == 0 {
		return f.Start + 1, f.End - 1
	}
	return f.Tokens[0].Start, f.Tokens[len(f.Tokens)-1].End
}

// SpansetParen is a parenthesised spanset expression.
type SpansetParen struct {
	// Start and End delimit the parentheses inclusive.
	Start, End int
	// X is the inner expression.
	X SpansetExpr
}

// SpansetBinary combines two spanset expressions with Op, which is one of KindAnd, KindOr or a
// structural operator kind (KindGt, KindLt, KindDescendant, KindAncestor, KindSibling, the
// KindUnion* kinds, KindNotChild, KindNotParent, KindNotDescendant, KindNotAncestor and
// KindNotRegex meaning the negated sibling operator !~).
type SpansetBinary struct {
	// Op is the operator kind.
	Op Kind
	// OpPos is the offset of the operator token.
	OpPos int
	// Left and Right are the operands; chains associate to the left.
	Left, Right SpansetExpr
}

// ScalarOperand is one side of a scalar filter: an aggregate call or a numeric/duration literal.
type ScalarOperand struct {
	// Func is the aggregate name (count, avg, max, min, sum), empty for a literal.
	Func string
	// Arg holds the argument tokens of the aggregate (empty for count()).
	Arg []Token
	// ArgExpr is the validated argument expression, nil for count().
	ArgExpr Expr
	// Lit is the literal token when Func is empty.
	Lit Token
}

// ScalarFilter is a pipeline stage such as `count() > 3` or `avg(duration) >= 20ms`.
type ScalarFilter struct {
	// Start and End delimit the stage.
	Start, End int
	// Left and Right are the operands.
	Left, Right ScalarOperand
	// Op is the comparison operator kind (=, !=, >, >=, <, <=).
	Op Kind
}

// GroupBy is `by(attr)`.
type GroupBy struct {
	// Start and End delimit the stage.
	Start, End int
	// Attr is the attribute or intrinsic token.
	Attr Token
}

// Select is `select(attr, ...)`.
type Select struct {
	// Start and End delimit the stage.
	Start, End int
	// Attrs are the attribute or intrinsic tokens.
	Attrs []Token
}

// Coalesce is `coalesce()`.
type Coalesce struct {
	// Start and End delimit the stage.
	Start, End int
}

// WithOption is one key=value entry of a with (...) clause.
type WithOption struct {
	// Key is the option name and Value its literal.
	Key, Value Token
}

// MetricsFunc is a TraceQL metrics function such as rate() or quantile_over_time(...).
type MetricsFunc struct {
	// Start and End delimit the stage including by and with clauses.
	Start, End int
	// Name is the function name.
	Name string
	// Attr is the attribute argument (nil for rate, count_over_time and compare).
	Attr *Token
	// Params are the numeric parameters (quantiles, or the optional compare arguments).
	Params []Token
	// Filter is the spanset filter argument of compare(...), otherwise nil.
	Filter *SpansetFilter
	// By lists the by (...) grouping attributes.
	By []Token
	// With lists the with (...) options.
	With []WithOption
}

// TopBottom is the second-stage `topk(n)` / `bottomk(n)` after a metrics function.
type TopBottom struct {
	// Start and End delimit the stage.
	Start, End int
	// Name is topk or bottomk.
	Name string
	// N is the integer argument.
	N Token
}

// Expr is a node of a validated filter expression.
type Expr interface {
	// Span returns the byte range of the expression.
	Span() (start, end int)
	expr()
}

// BinaryExpr is a binary operation: && / || (equal precedence, left to right), a comparison or
// arithmetic (precedence ^ over * / % over + -, left to right).
type BinaryExpr struct {
	// Op is the operator kind.
	Op Kind
	// Left and Right are the operands.
	Left, Right Expr
}

// ParenExpr is a parenthesised expression.
type ParenExpr struct {
	// Start and End delimit the parentheses inclusive.
	Start, End int
	// X is the inner expression.
	X Expr
}

// OperandExpr is an attribute, intrinsic or literal operand, with optional unary minus.
type OperandExpr struct {
	// Tok is the operand token (KindAttribute, KindIntrinsic, KindString, KindNumber,
	// KindDuration or KindIdent for true, false, nil and enum values).
	Tok Token
	// Neg is the offset of a leading unary minus, or -1.
	Neg int
}

// Span implements Expr.
func (e *BinaryExpr) Span() (int, int) {
	s, _ := e.Left.Span()
	_, t := e.Right.Span()
	return s, t
}

// Span implements Expr.
func (e *ParenExpr) Span() (int, int) { return e.Start, e.End }

// Span implements Expr.
func (e *OperandExpr) Span() (int, int) {
	if e.Neg >= 0 {
		return e.Neg, e.Tok.End
	}
	return e.Tok.Start, e.Tok.End
}

func (*BinaryExpr) expr()  {}
func (*ParenExpr) expr()   {}
func (*OperandExpr) expr() {}

// Span implements Stage.
func (f *SpansetFilter) Span() (int, int) { return f.Start, f.End }

// Span implements Stage.
func (p *SpansetParen) Span() (int, int) { return p.Start, p.End }

// Span implements Stage.
func (b *SpansetBinary) Span() (int, int) {
	s, _ := b.Left.Span()
	_, e := b.Right.Span()
	return s, e
}

// Span implements Stage.
func (s *ScalarFilter) Span() (int, int) { return s.Start, s.End }

// Span implements Stage.
func (s *GroupBy) Span() (int, int) { return s.Start, s.End }

// Span implements Stage.
func (s *Select) Span() (int, int) { return s.Start, s.End }

// Span implements Stage.
func (s *Coalesce) Span() (int, int) { return s.Start, s.End }

// Span implements Stage.
func (s *MetricsFunc) Span() (int, int) { return s.Start, s.End }

// Span implements Stage.
func (s *TopBottom) Span() (int, int) { return s.Start, s.End }

func (*SpansetFilter) stage() {}
func (*SpansetParen) stage()  {}
func (*SpansetBinary) stage() {}
func (*ScalarFilter) stage()  {}
func (*GroupBy) stage()       {}
func (*Select) stage()        {}
func (*Coalesce) stage()      {}
func (*MetricsFunc) stage()   {}
func (*TopBottom) stage()     {}

func (*SpansetFilter) spansetExpr() {}
func (*SpansetParen) spansetExpr()  {}
func (*SpansetBinary) spansetExpr() {}

// Attributes returns every attribute and intrinsic token of the query (including legacy bare
// intrinsics) in source order. The parser only accepts such tokens in validated operand
// positions, so this is the complete set for denied-attribute checks.
func (q *Query) Attributes() []Token {
	var out []Token
	for _, t := range q.Tokens {
		if t.Kind == KindAttribute || t.Kind == KindIntrinsic {
			out = append(out, t)
		}
	}
	return out
}

// String renders the query in canonical form: single spaces, filters as `{ body }` (`{ }` when
// empty), stages joined with ` | `. Parsing the result yields an equivalent query.
func (q *Query) String() string {
	parts := make([]string, len(q.Pipeline.Stages))
	for i, s := range q.Pipeline.Stages {
		parts[i] = s.String()
	}
	out := strings.Join(parts, " | ")
	if len(q.With) > 0 {
		out += " " + withString(q.With)
	}
	return out
}

func joinTokens(toks []Token) string {
	var sb strings.Builder
	for i, t := range toks {
		if i > 0 && toks[i-1].Kind != KindLParen && t.Kind != KindRParen {
			sb.WriteByte(' ')
		}
		sb.WriteString(t.Text)
	}
	return sb.String()
}

func joinList(toks []Token) string {
	parts := make([]string, len(toks))
	for i, t := range toks {
		parts[i] = t.Text
	}
	return strings.Join(parts, ", ")
}

func withString(w []WithOption) string {
	parts := make([]string, len(w))
	for i, o := range w {
		parts[i] = o.Key.Text + "=" + o.Value.Text
	}
	return "with (" + strings.Join(parts, ", ") + ")"
}

// String implements Stage.
func (f *SpansetFilter) String() string {
	if len(f.Tokens) == 0 {
		return "{ }"
	}
	return "{ " + joinTokens(f.Tokens) + " }"
}

// String implements Stage.
func (p *SpansetParen) String() string { return "(" + p.X.String() + ")" }

// String implements Stage.
func (b *SpansetBinary) String() string {
	return b.Left.String() + " " + b.Op.String() + " " + b.Right.String()
}

// String renders the operand.
func (o ScalarOperand) String() string {
	if o.Func == "" {
		return o.Lit.Text
	}
	return o.Func + "(" + joinTokens(o.Arg) + ")"
}

// String implements Stage.
func (s *ScalarFilter) String() string {
	return s.Left.String() + " " + s.Op.String() + " " + s.Right.String()
}

// String implements Stage.
func (s *GroupBy) String() string { return "by(" + s.Attr.Text + ")" }

// String implements Stage.
func (s *Select) String() string { return "select(" + joinList(s.Attrs) + ")" }

// String implements Stage.
func (s *Coalesce) String() string { return "coalesce()" }

// String implements Stage.
func (s *TopBottom) String() string { return s.Name + "(" + s.N.Text + ")" }

// String implements Stage.
func (s *MetricsFunc) String() string {
	var args []string
	if s.Filter != nil {
		args = append(args, s.Filter.String())
	}
	if s.Attr != nil {
		args = append(args, s.Attr.Text)
	}
	for _, p := range s.Params {
		args = append(args, p.Text)
	}
	out := s.Name + "(" + strings.Join(args, ", ") + ")"
	if len(s.By) > 0 {
		out += " by (" + joinList(s.By) + ")"
	}
	if len(s.With) > 0 {
		out += " " + withString(s.With)
	}
	return out
}
