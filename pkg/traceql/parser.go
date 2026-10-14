package traceql

import (
	"fmt"
	"strconv"
)

// maxDepth bounds parenthesis nesting so hostile input cannot exhaust the stack.
const maxDepth = 100

// ParseError is a syntax or validation error with the byte offset of the offending input.
type ParseError struct {
	// Pos is the byte offset in the input where the problem was detected.
	Pos int
	// Msg describes the problem.
	Msg string
}

// Error implements the error interface.
func (e *ParseError) Error() string {
	return fmt.Sprintf("traceql: parse error at byte %d: %s", e.Pos, e.Msg)
}

func perr(pos int, format string, args ...any) error {
	return &ParseError{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

// Parse parses and validates a TraceQL query. Anything that is not understood is an error
// (fail closed); lexical problems are returned as *Error and syntax problems as *ParseError.
func Parse(input string) (*Query, error) {
	toks, err := Lex(input)
	if err != nil {
		return nil, err
	}
	p := &parser{src: input, toks: toks}
	q := &Query{Source: input, Tokens: toks}
	if len(toks) == 0 {
		return nil, perr(0, "empty query")
	}
	if err := p.parseQuery(q); err != nil {
		return nil, err
	}
	q.Filters = p.filters
	return q, nil
}

type parser struct {
	src     string
	toks    []Token
	i       int
	depth   int
	filters []*SpansetFilter
}

func (p *parser) eof() bool { return p.i >= len(p.toks) }

func (p *parser) peek() (Token, bool) {
	if p.eof() {
		return Token{}, false
	}
	return p.toks[p.i], true
}

func (p *parser) peekAt(n int) (Token, bool) {
	if p.i+n >= len(p.toks) {
		return Token{}, false
	}
	return p.toks[p.i+n], true
}

func (p *parser) endPos() int {
	if len(p.toks) == 0 {
		return 0
	}
	return p.toks[len(p.toks)-1].End
}

func (p *parser) posHere() int {
	if t, ok := p.peek(); ok {
		return t.Start
	}
	return p.endPos()
}

func (p *parser) describe() string {
	if t, ok := p.peek(); ok {
		return fmt.Sprintf("%q", t.Text)
	}
	return "end of query"
}

func (p *parser) expect(k Kind) (Token, error) {
	t, ok := p.peek()
	if !ok || t.Kind != k {
		return Token{}, perr(p.posHere(), "expected %q, found %s", k.String(), p.describe())
	}
	p.i++
	return t, nil
}

func (p *parser) isIdent(name string) bool {
	t, ok := p.peek()
	return ok && t.Kind == KindIdent && t.Text == name
}

func (p *parser) nextIs(n int, k Kind) bool {
	t, ok := p.peekAt(n)
	return ok && t.Kind == k
}

var aggregates = map[string]bool{"count": true, "avg": true, "max": true, "min": true, "sum": true}

var metricsAttrFuncs = map[string]bool{
	"sum_over_time": true, "min_over_time": true, "max_over_time": true, "avg_over_time": true,
	"histogram_over_time": true, "quantile_over_time": true,
}

var spansetOps = map[Kind]bool{
	KindAnd: true, KindOr: true, KindGt: true, KindLt: true, KindDescendant: true, KindAncestor: true,
	KindSibling: true, KindUnionChild: true, KindUnionParent: true, KindUnionDescendant: true,
	KindUnionAncestor: true, KindUnionSibling: true, KindNotChild: true, KindNotParent: true,
	KindNotDescendant: true, KindNotAncestor: true, KindNotRegex: true,
}

func (p *parser) parseQuery(q *Query) error {
	pl := &Pipeline{}
	q.Pipeline = pl
	seenMetrics := false
	for {
		st, err := p.parseStage(len(pl.Stages), seenMetrics)
		if err != nil {
			return err
		}
		if _, ok := st.(*MetricsFunc); ok {
			seenMetrics = true
		}
		pl.Stages = append(pl.Stages, st)
		t, ok := p.peek()
		if !ok {
			return nil
		}
		if t.Kind == KindPipe {
			p.i++
			continue
		}
		if t.Kind == KindIdent && t.Text == "with" && !seenMetrics {
			w, err := p.parseWith()
			if err != nil {
				return err
			}
			q.With = w
			if !p.eof() {
				return perr(p.posHere(), "unexpected %s after with clause", p.describe())
			}
			return nil
		}
		return perr(t.Start, "unexpected %s", p.describe())
	}
}

func (p *parser) parseStage(idx int, seenMetrics bool) (Stage, error) {
	t, ok := p.peek()
	if !ok {
		return nil, perr(p.endPos(), "expected a pipeline stage after '|'")
	}
	switch t.Kind {
	case KindLBrace, KindLParen:
		if seenMetrics {
			return nil, perr(t.Start, "only topk or bottomk may follow a metrics function")
		}
		return p.parseSpansetExpr()
	case KindNumber, KindDuration:
		if seenMetrics {
			return nil, perr(t.Start, "only topk or bottomk may follow a metrics function")
		}
		return p.parseScalarFilter()
	case KindIdent:
		if !p.nextIs(1, KindLParen) {
			return nil, perr(t.Start, "unexpected identifier %q", t.Text)
		}
		name := t.Text
		switch {
		case name == "topk" || name == "bottomk":
			if !seenMetrics {
				return nil, perr(t.Start, "%s may only follow a metrics function", name)
			}
			return p.parseTopBottom()
		case seenMetrics:
			return nil, perr(t.Start, "only topk or bottomk may follow a metrics function")
		case aggregates[name]:
			return p.parseScalarFilter()
		case name == "by":
			return p.parseBy()
		case name == "select":
			return p.parseSelect()
		case name == "coalesce":
			return p.parseCoalesce()
		case name == "rate" || name == "count_over_time" || name == "compare" || metricsAttrFuncs[name]:
			if idx == 0 {
				return nil, perr(t.Start, "metrics function %s needs a preceding spanset stage", name)
			}
			return p.parseMetrics()
		}
		return nil, perr(t.Start, "unknown function %q", name)
	}
	return nil, perr(t.Start, "unexpected %s, expected a spanset filter or function", p.describe())
}

// parseSpansetExpr parses operands joined by one kind of spanset operator (left to right).
// Mixing different operators on one level requires parentheses.
func (p *parser) parseSpansetExpr() (SpansetExpr, error) {
	left, err := p.parseSpansetPrimary()
	if err != nil {
		return nil, err
	}
	var level Kind
	for {
		t, ok := p.peek()
		if !ok || !spansetOps[t.Kind] {
			return left, nil
		}
		if level != KindIllegal && level != t.Kind {
			return nil, perr(t.Start, "mixing %q and %q without parentheses is ambiguous", level.String(), t.Text)
		}
		level = t.Kind
		p.i++
		right, err := p.parseSpansetPrimary()
		if err != nil {
			return nil, err
		}
		left = &SpansetBinary{Op: t.Kind, OpPos: t.Start, Left: left, Right: right}
	}
}

func (p *parser) parseSpansetPrimary() (SpansetExpr, error) {
	t, ok := p.peek()
	if !ok {
		return nil, perr(p.endPos(), "expected a spanset filter")
	}
	switch t.Kind {
	case KindLBrace:
		return p.parseFilter()
	case KindLParen:
		if p.depth >= maxDepth {
			return nil, perr(t.Start, "nesting too deep")
		}
		p.depth++
		p.i++
		x, err := p.parseSpansetExpr()
		if err != nil {
			return nil, err
		}
		r, err := p.expect(KindRParen)
		if err != nil {
			return nil, err
		}
		p.depth--
		return &SpansetParen{Start: t.Start, End: r.End, X: x}, nil
	}
	return nil, perr(t.Start, "expected '{' or '(', found %s", p.describe())
}

func (p *parser) parseFilter() (*SpansetFilter, error) {
	open, err := p.expect(KindLBrace)
	if err != nil {
		return nil, err
	}
	f := &SpansetFilter{Start: open.Start}
	p.filters = append(p.filters, f)
	for {
		t, ok := p.peek()
		if !ok {
			return nil, perr(open.Start, "unbalanced '{': missing '}'")
		}
		if t.Kind == KindLBrace {
			return nil, perr(t.Start, "'{' inside a spanset filter")
		}
		p.i++
		if t.Kind == KindRBrace {
			f.End = t.End
			break
		}
		f.Tokens = append(f.Tokens, t)
	}
	if len(f.Tokens) == 0 {
		return f, nil
	}
	ep := &exprParser{toks: f.Tokens}
	e, err := ep.parse()
	if err != nil {
		return nil, err
	}
	if k := kindOf(e); k != kBool && k != kBoolLit {
		s, _ := e.Span()
		return nil, perr(s, "filter expression must be a boolean expression")
	}
	f.Expr = e
	return f, nil
}

// parenArgs consumes `( a b , c )` after the function name and returns the top-level comma
// separated token groups. Empty groups are errors except for an entirely empty list.
func (p *parser) parenArgs() ([][]Token, int, error) {
	l, err := p.expect(KindLParen)
	if err != nil {
		return nil, 0, err
	}
	var args [][]Token
	var cur []Token
	depth := 0
	for {
		t, ok := p.peek()
		if !ok {
			return nil, 0, perr(l.Start, "unbalanced '(': missing ')'")
		}
		p.i++
		switch t.Kind {
		case KindLParen:
			depth++
		case KindRParen:
			if depth == 0 {
				if len(cur) == 0 && len(args) > 0 {
					return nil, 0, perr(t.Start, "empty argument")
				}
				if len(cur) > 0 {
					args = append(args, cur)
				}
				return args, t.End, nil
			}
			depth--
		case KindComma:
			if depth == 0 {
				if len(cur) == 0 {
					return nil, 0, perr(t.Start, "empty argument")
				}
				args = append(args, cur)
				cur = nil
				continue
			}
		case KindLBrace, KindRBrace, KindPipe:
			return nil, 0, perr(t.Start, "unexpected %q in argument list", t.Text)
		}
		cur = append(cur, t)
	}
}

func isAttrTok(t Token) bool { return t.Kind == KindAttribute || t.Kind == KindIntrinsic }

func singleAttr(arg []Token, what string) (Token, error) {
	if len(arg) != 1 || !isAttrTok(arg[0]) {
		return Token{}, perr(arg[0].Start, "%s must be a single attribute or intrinsic", what)
	}
	return arg[0], nil
}

func (p *parser) parseCoalesce() (Stage, error) {
	name := p.toks[p.i]
	p.i++
	args, end, err := p.parenArgs()
	if err != nil {
		return nil, err
	}
	if len(args) != 0 {
		return nil, perr(args[0][0].Start, "coalesce() takes no arguments")
	}
	return &Coalesce{Start: name.Start, End: end}, nil
}

func (p *parser) parseBy() (Stage, error) {
	name := p.toks[p.i]
	p.i++
	args, end, err := p.parenArgs()
	if err != nil {
		return nil, err
	}
	if len(args) != 1 {
		return nil, perr(name.Start, "by() takes exactly one attribute")
	}
	a, err := singleAttr(args[0], "by() argument")
	if err != nil {
		return nil, err
	}
	return &GroupBy{Start: name.Start, End: end, Attr: a}, nil
}

func (p *parser) parseSelect() (Stage, error) {
	name := p.toks[p.i]
	p.i++
	args, end, err := p.parenArgs()
	if err != nil {
		return nil, err
	}
	if len(args) == 0 {
		return nil, perr(name.Start, "select() needs at least one attribute")
	}
	s := &Select{Start: name.Start, End: end}
	for _, a := range args {
		t, err := singleAttr(a, "select() argument")
		if err != nil {
			return nil, err
		}
		s.Attrs = append(s.Attrs, t)
	}
	return s, nil
}

func isUint(t Token) bool {
	if t.Kind != KindNumber || t.Text == "" {
		return false
	}
	for i := 0; i < len(t.Text); i++ {
		if t.Text[i] < '0' || t.Text[i] > '9' {
			return false
		}
	}
	return len(t.Text) <= 18
}

func (p *parser) parseTopBottom() (Stage, error) {
	name := p.toks[p.i]
	p.i++
	args, end, err := p.parenArgs()
	if err != nil {
		return nil, err
	}
	if len(args) != 1 || len(args[0]) != 1 || !isUint(args[0][0]) || args[0][0].Text == "0" {
		return nil, perr(name.Start, "%s() takes one positive integer", name.Text)
	}
	return &TopBottom{Start: name.Start, End: end, Name: name.Text, N: args[0][0]}, nil
}

func (p *parser) parseScalarFilter() (Stage, error) {
	start := p.posHere()
	l, err := p.parseScalarOperand()
	if err != nil {
		return nil, err
	}
	op, ok := p.peek()
	switch {
	case !ok:
		return nil, perr(p.endPos(), "expected a comparison operator")
	case op.Kind == KindEq, op.Kind == KindNeq, op.Kind == KindGt, op.Kind == KindGte, op.Kind == KindLt, op.Kind == KindLte:
	default:
		return nil, perr(op.Start, "expected a comparison operator, found %s", p.describe())
	}
	p.i++
	r, err := p.parseScalarOperand()
	if err != nil {
		return nil, err
	}
	if l.Func == "" && r.Func == "" {
		return nil, perr(start, "scalar filter needs an aggregate")
	}
	if l.Func != "" && r.Func != "" {
		return nil, perr(start, "comparing two aggregates is not supported")
	}
	return &ScalarFilter{Start: start, End: p.toks[p.i-1].End, Left: l, Right: r, Op: op.Kind}, nil
}

func (p *parser) parseScalarOperand() (ScalarOperand, error) {
	t, ok := p.peek()
	if !ok {
		return ScalarOperand{}, perr(p.endPos(), "expected an aggregate or number")
	}
	switch {
	case t.Kind == KindNumber || t.Kind == KindDuration:
		p.i++
		return ScalarOperand{Lit: t}, nil
	case t.Kind == KindIdent && aggregates[t.Text] && p.nextIs(1, KindLParen):
		p.i++
		args, _, err := p.parenArgs()
		if err != nil {
			return ScalarOperand{}, err
		}
		o := ScalarOperand{Func: t.Text}
		if t.Text == "count" {
			if len(args) != 0 {
				return ScalarOperand{}, perr(args[0][0].Start, "count() takes no arguments")
			}
			return o, nil
		}
		if len(args) != 1 {
			return ScalarOperand{}, perr(t.Start, "%s() takes exactly one argument", t.Text)
		}
		ep := &exprParser{toks: args[0]}
		e, err := ep.parse()
		if err != nil {
			return ScalarOperand{}, err
		}
		if kindOf(e) != kValue || !hasAttr(e) {
			return ScalarOperand{}, perr(args[0][0].Start, "%s() argument must be a numeric expression over an attribute", t.Text)
		}
		o.Arg, o.ArgExpr = args[0], e
		return o, nil
	}
	return ScalarOperand{}, perr(t.Start, "expected an aggregate or number, found %s", p.describe())
}

func (p *parser) parseMetrics() (Stage, error) {
	name := p.toks[p.i]
	m := &MetricsFunc{Start: name.Start, Name: name.Text}
	var end int
	if name.Text == "compare" {
		p.i += 2 // name and '('
		f, err := p.parseFilter()
		if err != nil {
			return nil, err
		}
		m.Filter = f
		for !p.eof() && p.toks[p.i].Kind == KindComma {
			p.i++
			t, ok := p.peek()
			if !ok || !isUint(t) || len(m.Params) >= 3 {
				return nil, perr(p.posHere(), "compare() takes a filter and up to three integers")
			}
			m.Params = append(m.Params, t)
			p.i++
		}
		r, err := p.expect(KindRParen)
		if err != nil {
			return nil, err
		}
		end = r.End
	} else {
		p.i++
		args, e, err := p.parenArgs()
		if err != nil {
			return nil, err
		}
		end = e
		switch name.Text {
		case "rate", "count_over_time":
			if len(args) != 0 {
				return nil, perr(args[0][0].Start, "%s() takes no arguments", name.Text)
			}
		default:
			if len(args) == 0 {
				return nil, perr(name.Start, "%s() needs an attribute argument", name.Text)
			}
			a, err := singleAttr(args[0], name.Text+"() first argument")
			if err != nil {
				return nil, err
			}
			m.Attr = &a
			rest := args[1:]
			if name.Text == "quantile_over_time" {
				if len(rest) == 0 {
					return nil, perr(name.Start, "quantile_over_time() needs at least one quantile")
				}
				for _, q := range rest {
					v, perr2 := quantile(q)
					if perr2 != nil {
						return nil, perr2
					}
					m.Params = append(m.Params, v)
				}
			} else if len(rest) != 0 {
				return nil, perr(rest[0][0].Start, "%s() takes one argument", name.Text)
			}
		}
	}
	if p.isIdent("by") {
		p.i++
		args, e, err := p.parenArgs()
		if err != nil {
			return nil, err
		}
		if len(args) == 0 {
			return nil, perr(p.toks[p.i-1].Start, "by () needs at least one attribute")
		}
		for _, a := range args {
			t, err := singleAttr(a, "by () argument")
			if err != nil {
				return nil, err
			}
			m.By = append(m.By, t)
		}
		end = e
	}
	if p.isIdent("with") {
		w, err := p.parseWith()
		if err != nil {
			return nil, err
		}
		m.With = w
		end = p.toks[p.i-1].End
	}
	m.End = end
	return m, nil
}

func quantile(arg []Token) (Token, error) {
	if len(arg) != 1 || arg[0].Kind != KindNumber {
		return Token{}, perr(arg[0].Start, "quantile must be a number between 0 and 1")
	}
	v, err := strconv.ParseFloat(arg[0].Text, 64)
	if err != nil || v < 0 || v > 1 {
		return Token{}, perr(arg[0].Start, "quantile must be a number between 0 and 1")
	}
	return arg[0], nil
}

func isWithValue(v Token) bool {
	switch v.Kind {
	case KindNumber, KindDuration, KindString:
		return true
	case KindIdent:
		return v.Text == "true" || v.Text == "false"
	}
	return false
}

// parseWith parses `with (key=value, ...)`; values are literals only, never attributes.
func (p *parser) parseWith() ([]WithOption, error) {
	if !p.nextIs(1, KindLParen) {
		return nil, perr(p.posHere(), "expected '(' after with")
	}
	p.i += 2
	var out []WithOption
	for {
		k, ok := p.peek()
		if !ok || k.Kind != KindIdent {
			return nil, perr(p.posHere(), "expected an option name in with (...), found %s", p.describe())
		}
		p.i++
		if _, err := p.expect(KindEq); err != nil {
			return nil, err
		}
		v, ok := p.peek()
		if !ok || !isWithValue(v) {
			return nil, perr(p.posHere(), "expected a literal option value, found %s", p.describe())
		}
		p.i++
		out = append(out, WithOption{Key: k, Value: v})
		t, ok := p.peek()
		if !ok {
			return nil, perr(p.endPos(), "unbalanced '(' in with clause")
		}
		p.i++
		if t.Kind == KindRParen {
			return out, nil
		}
		if t.Kind != KindComma {
			return nil, perr(t.Start, "expected ',' or ')' in with clause, found %q", t.Text)
		}
	}
}

// ---- filter expressions ----

type exprKind int

const (
	kValue exprKind = iota
	kBool
	kBoolLit
)

type exprParser struct {
	toks  []Token
	i     int
	depth int
}

func (e *exprParser) parse() (Expr, error) {
	x, err := e.binary(1)
	if err != nil {
		return nil, err
	}
	if e.i < len(e.toks) {
		return nil, perr(e.toks[e.i].Start, "unexpected %q", e.toks[e.i].Text)
	}
	return x, nil
}

func prec(k Kind) int {
	switch k {
	case KindAnd, KindOr:
		return 1
	case KindEq, KindNeq, KindGt, KindGte, KindLt, KindLte, KindRegex, KindNotRegex:
		return 2
	case KindAdd, KindSub:
		return 3
	case KindMul, KindDiv, KindMod:
		return 4
	case KindPow:
		return 5
	}
	return 0
}

func (e *exprParser) binary(minPrec int) (Expr, error) {
	left, err := e.primary()
	if err != nil {
		return nil, err
	}
	for e.i < len(e.toks) {
		t := e.toks[e.i]
		pr := prec(t.Kind)
		if pr == 0 || pr < minPrec {
			break
		}
		e.i++
		right, err := e.binary(pr + 1)
		if err != nil {
			return nil, err
		}
		left, err = mkBinary(t, left, right)
		if err != nil {
			return nil, err
		}
	}
	return left, nil
}

var statusEnums = map[string]bool{"error": true, "ok": true, "unset": true}
var kindEnums = map[string]bool{
	"unspecified": true, "internal": true, "server": true, "client": true, "producer": true, "consumer": true,
}

func (e *exprParser) primary() (Expr, error) {
	if e.i >= len(e.toks) {
		end := 0
		if len(e.toks) > 0 {
			end = e.toks[len(e.toks)-1].End
		}
		return nil, perr(end, "expression ends unexpectedly")
	}
	t := e.toks[e.i]
	switch t.Kind {
	case KindAttribute, KindIntrinsic, KindString, KindNumber, KindDuration:
		e.i++
		return &OperandExpr{Tok: t, Neg: -1}, nil
	case KindIdent:
		if t.Text == "true" || t.Text == "false" || t.Text == "nil" || statusEnums[t.Text] || kindEnums[t.Text] {
			e.i++
			return &OperandExpr{Tok: t, Neg: -1}, nil
		}
		return nil, perr(t.Start, "unexpected identifier %q (quote attribute names containing spaces; unknown keywords are rejected)", t.Text)
	case KindSub:
		if e.i+1 < len(e.toks) && (e.toks[e.i+1].Kind == KindNumber || e.toks[e.i+1].Kind == KindDuration) {
			e.i += 2
			return &OperandExpr{Tok: e.toks[e.i-1], Neg: t.Start}, nil
		}
		return nil, perr(t.Start, "unary '-' is only supported before a number or duration literal")
	case KindNot:
		return nil, perr(t.Start, "unary '!' is not supported")
	case KindLParen:
		if e.depth >= maxDepth {
			return nil, perr(t.Start, "nesting too deep")
		}
		e.depth++
		e.i++
		x, err := e.binary(1)
		if err != nil {
			return nil, err
		}
		if e.i >= len(e.toks) || e.toks[e.i].Kind != KindRParen {
			return nil, perr(t.Start, "unbalanced '(': missing ')'")
		}
		r := e.toks[e.i]
		e.i++
		e.depth--
		return &ParenExpr{Start: t.Start, End: r.End, X: x}, nil
	}
	return nil, perr(t.Start, "unexpected %q, expected an attribute or value", t.Text)
}

func unparen(x Expr) Expr {
	for {
		p, ok := x.(*ParenExpr)
		if !ok {
			return x
		}
		x = p.X
	}
}

func kindOf(x Expr) exprKind {
	switch v := x.(type) {
	case *ParenExpr:
		return kindOf(v.X)
	case *OperandExpr:
		if v.Tok.Kind == KindIdent && (v.Tok.Text == "true" || v.Tok.Text == "false") {
			return kBoolLit
		}
		return kValue
	case *BinaryExpr:
		if v.Op == KindAnd || v.Op == KindOr || prec(v.Op) == 2 {
			return kBool
		}
	}
	return kValue
}

func hasAttr(x Expr) bool {
	switch v := x.(type) {
	case *ParenExpr:
		return hasAttr(v.X)
	case *OperandExpr:
		return isAttrTok(v.Tok)
	case *BinaryExpr:
		return hasAttr(v.Left) || hasAttr(v.Right)
	}
	return false
}

// numeric reports whether x is a valid arithmetic operand.
func numeric(x Expr) bool {
	switch v := x.(type) {
	case *ParenExpr:
		return numeric(v.X)
	case *OperandExpr:
		return isAttrTok(v.Tok) || v.Tok.Kind == KindNumber || v.Tok.Kind == KindDuration
	case *BinaryExpr:
		return prec(v.Op) >= 3
	}
	return false
}

func operandOf(x Expr) *OperandExpr {
	o, _ := unparen(x).(*OperandExpr)
	return o
}

func isIdentLit(x Expr, pred func(string) bool) bool {
	o := operandOf(x)
	return o != nil && o.Tok.Kind == KindIdent && pred(o.Tok.Text)
}

func isEnum(x Expr) bool {
	return isIdentLit(x, func(s string) bool { return statusEnums[s] || kindEnums[s] })
}

func mkBinary(op Token, l, r Expr) (Expr, error) {
	b := &BinaryExpr{Op: op.Kind, Left: l, Right: r}
	lk, rk := kindOf(l), kindOf(r)
	switch prec(op.Kind) {
	case 1:
		if lk == kValue || rk == kValue {
			return nil, perr(op.Start, "operands of %q must be boolean expressions", op.Text)
		}
	case 2:
		if lk == kBool || rk == kBool {
			return nil, perr(op.Start, "cannot chain comparisons or compare boolean expressions")
		}
		if !hasAttr(l) && !hasAttr(r) {
			return nil, perr(op.Start, "comparison needs an attribute or intrinsic")
		}
		if err := checkComparison(op, l, r); err != nil {
			return nil, err
		}
	default:
		if !numeric(l) || !numeric(r) {
			return nil, perr(op.Start, "arithmetic %q needs numeric operands", op.Text)
		}
	}
	return b, nil
}

func checkComparison(op Token, l, r Expr) error {
	if op.Kind == KindRegex || op.Kind == KindNotRegex {
		if o, ok := r.(*OperandExpr); !ok || o.Tok.Kind != KindString || !isAttrTok(operandTok(l)) {
			return perr(op.Start, "%q needs an attribute on the left and a string literal on the right", op.Text)
		}
	}
	if isIdentLit(l, func(s string) bool { return s == "nil" }) {
		return perr(op.Start, "nil is only supported as `attribute != nil`")
	}
	if isIdentLit(r, func(s string) bool { return s == "nil" }) {
		o := operandOf(l)
		if op.Kind != KindNeq || o == nil || !isAttrTok(o.Tok) {
			return perr(op.Start, "nil is only supported as `attribute != nil`")
		}
	}
	for _, pair := range [][2]Expr{{l, r}, {r, l}} {
		if !isEnum(pair[0]) {
			continue
		}
		name := operandOf(pair[0]).Tok.Text
		o := operandOf(pair[1])
		if (op.Kind != KindEq && op.Kind != KindNeq) || o == nil || o.Tok.Kind != KindIntrinsic ||
			(o.Tok.Scope != "" && o.Tok.Scope != "span") {
			return perr(op.Start, "enum value %q can only be compared with = or != against status or kind", name)
		}
		want := "kind"
		if statusEnums[name] {
			want = "status"
		}
		if o.Tok.Value != want {
			return perr(op.Start, "enum value %q does not match span %s", name, o.Tok.Value)
		}
	}
	return nil
}

func operandTok(x Expr) Token {
	if o := operandOf(x); o != nil {
		return o.Tok
	}
	return Token{}
}
