package logql

import (
	"strings"
)

// QueryKind distinguishes log queries from metric queries.
type QueryKind int

// Query kinds.
const (
	// KindLog is a log query: a stream selector followed by a pipeline.
	KindLog QueryKind = iota + 1
	// KindMetric is a metric query: any expression built from range
	// aggregations, vector aggregations, literals and binary operators.
	KindMetric
)

// String returns "log" or "metric".
func (k QueryKind) String() string {
	switch k {
	case KindLog:
		return "log"
	case KindMetric:
		return "metric"
	}
	return "unknown"
}

// Query is a parsed LogQL query.
type Query struct {
	// Source is the exact input that was parsed.
	Source string
	// Kind is KindLog or KindMetric.
	Kind QueryKind
	// Expr is the root expression. Redundant parentheses are not kept in the
	// tree; String re-adds parentheses wherever precedence needs them.
	Expr Expr
	// Selectors lists every stream selector in source order.
	Selectors []*Selector
	// RangeAggs lists every range aggregation in source order (outer before
	// inner).
	RangeAggs []*RangeAgg
}

// String renders the canonical form of the query. Comments and redundant
// whitespace and parentheses are dropped; keywords are lower-cased. The
// result parses back to an equivalent Query.
func (q *Query) String() string { return q.Expr.String() }

// Expr is a node of the query tree: *LogExpr, *Literal, *VectorFunc,
// *VectorAgg, *RangeAgg, *LabelReplace or *BinaryExpr.
type Expr interface {
	// String renders the canonical form of the node.
	String() string
	isExpr()
}

// Matcher is one label matcher of a stream selector.
type Matcher struct {
	// Name is the label name (verbatim; a keyword spelling is allowed).
	Name string
	// Op is one of "=", "!=", "=~", "!~".
	Op string
	// Value is the decoded string value.
	Value string
	// Raw is the verbatim source text of the value token (quotes included).
	Raw string
	// Pos and End are the byte offsets of the whole matcher.
	Pos, End int
	// NamePos is the byte offset of the label name.
	NamePos int
}

// Selector is a stream selector such as {app="foo", env=~"a|b"}.
type Selector struct {
	// Matchers are the label matchers in source order; never empty.
	Matchers []Matcher
	// Pos is the byte offset of '{' and End one past '}'.
	Pos, End int
}

// String renders the selector canonically.
func (s *Selector) String() string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, m := range s.Matchers {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(m.Name + m.Op + m.Raw)
	}
	sb.WriteByte('}')
	return sb.String()
}

// LogExpr is a log query: a stream selector with an opaque pipeline.
type LogExpr struct {
	// Selector is the stream selector.
	Selector *Selector
	// Pipeline is the structurally validated stage sequence as raw tokens
	// with source positions. A Keyword token is a reserved word in keyword
	// role; keywords used as label names are re-kinded to Ident. The
	// --strict and --keep-empty flags are single Ident tokens whose Text is
	// the flag (their Pos/End span the original '-' tokens).
	Pipeline []Token
}

func (*LogExpr) isExpr() {}

// String renders the selector followed by the pipeline.
func (e *LogExpr) String() string { return logString(e.Selector, e.Pipeline) }

// Unwrap describes the unwrap stage of a range aggregation.
type Unwrap struct {
	// Conv is "", "bytes", "duration" or "duration_seconds".
	Conv string
	// Label is the unwrapped label.
	Label string
	// Pos and End span the unwrap stage (from the "unwrap" keyword).
	Pos, End int
}

// LogRange is the log range selected by a range aggregation, without the
// interval.
type LogRange struct {
	// Selector is the stream selector.
	Selector *Selector
	// Pipeline is as in [LogExpr.Pipeline], including the unwrap stage.
	Pipeline []Token
	// Unwrap is non-nil when the pipeline contains an unwrap stage.
	Unwrap *Unwrap
	// Paren records that the source used the form (selector pipeline)[range].
	Paren bool
}

// RangeAgg is a range aggregation such as rate({a="b"}[5m]).
type RangeAgg struct {
	// Func is the lower-case function name.
	Func string
	// Param is the leading number of quantile_over_time, else nil.
	Param *Literal
	// Range is the selected log range.
	Range LogRange
	// Interval is the verbatim range, e.g. "5m".
	Interval string
	// Offset is the verbatim offset duration, or "".
	Offset string
	// Grouping is the by/without clause after the parentheses, or nil.
	Grouping *Grouping
	// Pos and End span the whole call.
	Pos, End int
}

func (*RangeAgg) isExpr() {}

// String renders the call canonically.
func (r *RangeAgg) String() string {
	var sb strings.Builder
	sb.WriteString(r.Func + "(")
	if r.Param != nil {
		sb.WriteString(r.Param.Text + ", ")
	}
	if r.Range.Paren {
		sb.WriteString("(" + logString(r.Range.Selector, r.Range.Pipeline) + ")")
	} else {
		sb.WriteString(logString(r.Range.Selector, r.Range.Pipeline))
	}
	sb.WriteString("[" + r.Interval + "]")
	if r.Offset != "" {
		sb.WriteString(" offset " + r.Offset)
	}
	sb.WriteString(")")
	if r.Grouping != nil {
		sb.WriteString(" " + r.Grouping.String())
	}
	return sb.String()
}

// Grouping is a by/without clause.
type Grouping struct {
	// Without is true for without(...) and false for by(...).
	Without bool
	// Labels are the listed label names.
	Labels []string
}

// String renders e.g. "by (a, b)".
func (g *Grouping) String() string {
	kw := "by"
	if g.Without {
		kw = "without"
	}
	return kw + " (" + strings.Join(g.Labels, ", ") + ")"
}

// Literal is a numeric literal, possibly with a leading minus.
type Literal struct {
	// Text is the source text, e.g. "-1.5" (a leading minus is joined).
	Text string
	// Pos and End are the byte offsets of the literal.
	Pos, End int
}

func (*Literal) isExpr() {}

// String returns the literal text.
func (l *Literal) String() string { return l.Text }

// VectorFunc is vector(<number>).
type VectorFunc struct {
	// Arg is the number.
	Arg Literal
	// Pos and End span the call.
	Pos, End int
}

func (*VectorFunc) isExpr() {}

// String renders "vector(n)".
func (v *VectorFunc) String() string { return "vector(" + v.Arg.Text + ")" }

// VectorAgg is a vector aggregation: sum, avg, min, max, count, stddev,
// stdvar, topk, bottomk, sort or sort_desc.
type VectorAgg struct {
	// Op is the lower-case function name.
	Op string
	// Param is the k of topk/bottomk, else nil.
	Param *Literal
	// Grouping is the by/without clause (before or after the argument in the
	// source; rendered before), or nil.
	Grouping *Grouping
	// Arg is the aggregated expression.
	Arg Expr
	// Pos and End span the whole call.
	Pos, End int
}

func (*VectorAgg) isExpr() {}

// String renders the aggregation canonically.
func (v *VectorAgg) String() string {
	var sb strings.Builder
	sb.WriteString(v.Op)
	if v.Grouping != nil {
		sb.WriteString(" " + v.Grouping.String() + " ")
	}
	sb.WriteString("(")
	if v.Param != nil {
		sb.WriteString(v.Param.Text + ", ")
	}
	sb.WriteString(v.Arg.String() + ")")
	return sb.String()
}

// StringLit is a string argument with its source form.
type StringLit struct {
	// Value is the decoded string, Raw the verbatim source text.
	Value, Raw string
	// Pos and End are the byte offsets of the string token.
	Pos, End int
}

// LabelReplace is label_replace(v, dst, replacement, src, regex).
type LabelReplace struct {
	// Arg is the first argument.
	Arg Expr
	// Dst, Repl, Src and Regex are the four string arguments.
	Dst, Repl, Src, Regex StringLit
	// Pos and End span the call.
	Pos, End int
}

func (*LabelReplace) isExpr() {}

// String renders the call canonically.
func (l *LabelReplace) String() string {
	return "label_replace(" + l.Arg.String() + ", " + l.Dst.Raw + ", " + l.Repl.Raw + ", " +
		l.Src.Raw + ", " + l.Regex.Raw + ")"
}

// VectorMatching holds the on/ignoring and group_left/group_right modifiers
// of a binary operation.
type VectorMatching struct {
	// On is true for on(...) and false for ignoring(...).
	On bool
	// Labels are the labels of on(...)/ignoring(...).
	Labels []string
	// Group is "", "group_left" or "group_right".
	Group string
	// Include are the labels of group_left(...)/group_right(...).
	Include []string
}

// BinaryExpr is a binary operation between two metric expressions.
type BinaryExpr struct {
	// Op is one of + - * / % ^ == != > >= < <= and or unless.
	Op string
	// Bool is set for comparisons with the bool modifier.
	Bool bool
	// Matching is the vector matching clause, or nil.
	Matching *VectorMatching
	// LHS and RHS are the operands.
	LHS, RHS Expr
	// OpPos is the byte offset of the operator.
	OpPos int
}

func (*BinaryExpr) isExpr() {}

// String renders the operation; nested binary operations are parenthesised.
func (b *BinaryExpr) String() string {
	var sb strings.Builder
	sb.WriteString(operand(b.LHS) + " " + b.Op)
	if b.Bool {
		sb.WriteString(" bool")
	}
	if m := b.Matching; m != nil {
		kw := "ignoring"
		if m.On {
			kw = "on"
		}
		sb.WriteString(" " + kw + " (" + strings.Join(m.Labels, ", ") + ")")
		if m.Group != "" {
			sb.WriteString(" " + m.Group)
			if len(m.Include) > 0 {
				sb.WriteString(" (" + strings.Join(m.Include, ", ") + ")")
			}
		}
	}
	sb.WriteString(" " + operand(b.RHS))
	return sb.String()
}

func operand(e Expr) string {
	if _, ok := e.(*BinaryExpr); ok {
		return "(" + e.String() + ")"
	}
	return e.String()
}

func logString(sel *Selector, pipe []Token) string {
	var sb strings.Builder
	sb.WriteString(sel.String())
	for i, t := range pipe {
		text := t.Text
		if t.Kind == Keyword {
			text = t.Value
		}
		if i == 0 || needSpace(pipe[i-1], t) {
			sb.WriteByte(' ')
		}
		sb.WriteString(text)
	}
	return sb.String()
}

func needSpace(prev, cur Token) bool {
	switch cur.Kind {
	case Comma, RParen, RBracket:
		return false
	}
	switch prev.Kind {
	case LParen, LBracket:
		return false
	}
	if cur.Kind == LParen && (prev.Kind == Ident || prev.Kind == Keyword) {
		return false
	}
	return true
}
