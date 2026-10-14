package logql

import (
	"fmt"
	"regexp"
	"strings"
)

// ParseError is a syntax error at a byte position of the query.
type ParseError struct {
	// Pos is the byte offset of the offending input.
	Pos int
	// Msg describes the problem.
	Msg string
}

// Error implements the error interface.
func (e *ParseError) Error() string {
	return fmt.Sprintf("logql: parse error at byte %d: %s", e.Pos, e.Msg)
}

// maxDepth bounds recursion (nested parentheses and function calls).
const maxDepth = 100

// Function tables (docs/research/logql.md section 2, plus the public metric
// query docs). Anything not listed is rejected.
var (
	// rangeFuncs maps range aggregations to whether they require unwrap.
	rangeFuncs = map[string]bool{
		"rate": false, "count_over_time": false, "bytes_rate": false,
		"bytes_over_time": false, "absent_over_time": false,
		"rate_counter": true, "sum_over_time": true, "avg_over_time": true,
		"max_over_time": true, "min_over_time": true, "stdvar_over_time": true,
		"stddev_over_time": true, "quantile_over_time": true,
		"first_over_time": true, "last_over_time": true,
	}
	// vectorFuncs maps vector aggregations to whether they take a leading
	// number parameter.
	vectorFuncs = map[string]bool{
		"sum": false, "avg": false, "min": false, "max": false, "count": false,
		"stddev": false, "stdvar": false, "sort": false, "sort_desc": false,
		"topk": true, "bottomk": true,
	}
	unwrapConvs = map[string]bool{"bytes": true, "duration": true, "duration_seconds": true}
)

// Parse parses a LogQL query. It returns a *[ParseError] (or the lexer's
// *[Error]) for anything it does not recognise; there is no lenient mode.
func Parse(src string) (*Query, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, &ParseError{Pos: 0, Msg: "empty query"}
	}
	p := &parser{src: src, toks: toks}
	q := &Query{Source: src}
	if toks[0].Kind == LBrace {
		q.Kind = KindLog
		sel, err := p.parseSelector()
		if err != nil {
			return nil, err
		}
		pipe, uw, err := p.parsePipeline()
		if err != nil {
			return nil, err
		}
		if uw != nil {
			return nil, p.errAt(uw.Pos, "unwrap is only allowed inside a range aggregation")
		}
		q.Expr = &LogExpr{Selector: sel, Pipeline: pipe}
	} else {
		q.Kind = KindMetric
		e, err := p.parseBinary(1)
		if err != nil {
			return nil, err
		}
		q.Expr = e
	}
	if t := p.peek(); t.Kind != EOF {
		return nil, p.unexpected(t)
	}
	q.Selectors = p.sels
	q.RangeAggs = p.aggs
	return q, nil
}

type parser struct {
	src   string
	toks  []Token
	i     int
	depth int
	sels  []*Selector
	aggs  []*RangeAgg
}

func (p *parser) peekAt(n int) Token {
	if p.i+n < len(p.toks) {
		return p.toks[p.i+n]
	}
	return Token{Kind: EOF, Pos: len(p.src), End: len(p.src)}
}

func (p *parser) peek() Token { return p.peekAt(0) }

func (p *parser) next() Token {
	t := p.peek()
	if p.i < len(p.toks) {
		p.i++
	}
	return t
}

func (p *parser) errAt(pos int, format string, args ...any) error {
	return &ParseError{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) unexpected(t Token) error {
	switch {
	case t.Kind == EOF:
		return p.errAt(t.Pos, "unexpected end of query")
	case t.Kind == Keyword && t.Value == "offset":
		return p.errAt(t.Pos, "unexpected offset: offset must directly follow the range selector inside the function")
	}
	return p.errAt(t.Pos, "unexpected %s %q", t.Kind, t.Text)
}

func (p *parser) enter() error {
	p.depth++
	if p.depth > maxDepth {
		return p.errAt(p.peek().Pos, "query nested too deeply")
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

func (p *parser) expect(k Kind, what string) (Token, error) {
	t := p.peek()
	if t.Kind != k {
		if t.Kind == EOF {
			return t, p.errAt(t.Pos, "unexpected end of query, expected %s", what)
		}
		return t, p.errAt(t.Pos, "unexpected %s %q, expected %s", t.Kind, t.Text, what)
	}
	return p.next(), nil
}

func isOp(t Token, text string) bool { return t.Kind == Op && t.Text == text }

func isKw(t Token, v string) bool { return t.Kind == Keyword && t.Value == v }

func isName(t Token) bool { return t.Kind == Ident || t.Kind == Keyword }

// asIdent returns t re-kinded to Ident when it is a keyword used as a name.
func asIdent(t Token) Token {
	if t.Kind == Keyword {
		t.Kind = Ident
		t.Value = t.Text
	}
	return t
}

// ---------------------------------------------------------------- selectors

func (p *parser) parseSelector() (*Selector, error) {
	lb, err := p.expect(LBrace, "'{'")
	if err != nil {
		return nil, err
	}
	sel := &Selector{Pos: lb.Pos}
	if p.peek().Kind == RBrace {
		return nil, p.errAt(lb.Pos, "empty stream selector")
	}
	for {
		name := p.peek()
		if !isName(name) {
			if name.Kind == EOF {
				return nil, p.errAt(name.Pos, "unterminated stream selector")
			}
			return nil, p.errAt(name.Pos, "expected label name, got %s %q", name.Kind, name.Text)
		}
		p.next()
		op := p.peek()
		if op.Kind != Op || (op.Text != "=" && op.Text != "!=" && op.Text != "=~" && op.Text != "!~") {
			return nil, p.errAt(op.Pos, "expected one of = != =~ !~ after label %q", name.Text)
		}
		p.next()
		val := p.peek()
		if val.Kind != String && val.Kind != RawString {
			return nil, p.errAt(val.Pos, "expected string value for label %q", name.Text)
		}
		p.next()
		sel.Matchers = append(sel.Matchers, Matcher{
			Name: name.Text, Op: op.Text, Value: val.Value, Raw: val.Text,
			Pos: name.Pos, End: val.End, NamePos: name.Pos,
		})
		t := p.peek()
		if t.Kind == Comma {
			p.next()
			continue
		}
		if t.Kind == RBrace {
			p.next()
			sel.End = t.End
			break
		}
		if t.Kind == EOF {
			return nil, p.errAt(t.Pos, "unterminated stream selector")
		}
		return nil, p.errAt(t.Pos, "expected ',' or '}' in stream selector, got %s %q", t.Kind, t.Text)
	}
	if err := p.checkSelector(sel); err != nil {
		return nil, err
	}
	p.sels = append(p.sels, sel)
	return sel, nil
}

// checkSelector compiles the regexes and requires one matcher that cannot
// match the empty string (docs/research/logql.md section 2, stream selector).
func (p *parser) checkSelector(sel *Selector) error {
	ok := false
	for _, m := range sel.Matchers {
		switch m.Op {
		case "=":
			ok = ok || m.Value != ""
		case "!=":
			ok = ok || m.Value == ""
		case "=~", "!~":
			re, err := regexp.Compile("^(?:" + m.Value + ")$")
			if err != nil {
				return p.errAt(m.Pos, "invalid regular expression for label %q: %v", m.Name, err)
			}
			if m.Op == "=~" {
				ok = ok || !re.MatchString("")
			}
		}
	}
	if !ok {
		return p.errAt(sel.Pos, "stream selector needs at least one equality or regex matcher that does not match the empty string")
	}
	return nil
}

// ----------------------------------------------------------------- pipeline

var lineFilterOps = map[string]bool{"|=": true, "!=": true, "|~": true, "!~": true, "|>": true, "!>": true}

var comparisonOps = map[string]bool{
	"=": true, "==": true, "!=": true, ">": true, ">=": true, "<": true, "<=": true, "=~": true, "!~": true,
}

// parsePipeline consumes stages while they continue and returns the opaque
// token sequence and the unwrap stage, if any. It stops at the first token
// that cannot start a stage; the caller decides whether that is acceptable.
func (p *parser) parsePipeline() ([]Token, *Unwrap, error) {
	var out []Token
	var uw *Unwrap
	for {
		t := p.peek()
		switch {
		case t.Kind == Op && lineFilterOps[t.Text]:
			if uw != nil {
				return nil, nil, p.errAt(t.Pos, "only label filters are allowed after unwrap")
			}
			out = append(out, p.next())
			var err error
			if out, err = p.lineFilterValue(out, t.Text); err != nil {
				return nil, nil, err
			}
			for isKw(p.peek(), "or") {
				out = append(out, p.next())
				if out, err = p.lineFilterValue(out, t.Text); err != nil {
					return nil, nil, err
				}
			}
		case isOp(t, "|"):
			out = append(out, p.next())
			var err error
			if out, err = p.stage(out, &uw); err != nil {
				return nil, nil, err
			}
		default:
			return out, uw, nil
		}
	}
}

// lineFilterValue parses a string, a raw string or (for |= and !=) ip("...").
func (p *parser) lineFilterValue(out []Token, op string) ([]Token, error) {
	t := p.peek()
	switch {
	case t.Kind == String || t.Kind == RawString:
		return append(out, p.next()), nil
	case isKw(t, "ip"):
		if op != "|=" && op != "!=" {
			return nil, p.errAt(t.Pos, "ip() is only allowed with |= and !=")
		}
		return p.ipCall(out)
	}
	if t.Kind == EOF {
		return nil, p.errAt(t.Pos, "unexpected end of query, expected line filter string")
	}
	return nil, p.errAt(t.Pos, "expected string after line filter operator %q, got %s %q", op, t.Kind, t.Text)
}

func (p *parser) ipCall(out []Token) ([]Token, error) {
	out = append(out, p.next())
	for _, k := range []Kind{LParen, String, RParen} {
		if k == String && (p.peek().Kind == RawString) {
			out = append(out, p.next())
			continue
		}
		t, err := p.expect(k, "ip(\"...\")")
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// stage parses one stage after '|'.
func (p *parser) stage(out []Token, uw **Unwrap) ([]Token, error) {
	t := p.peek()
	isLabelFilter := t.Kind == LParen || (isName(t) && p.peekAt(1).Kind == Op && comparisonOps[p.peekAt(1).Text])
	if *uw != nil && !isLabelFilter {
		return nil, p.errAt(t.Pos, "only label filters are allowed after unwrap")
	}
	if isLabelFilter {
		return p.labelFilter(out)
	}
	if t.Kind != Keyword {
		if t.Kind == EOF {
			return nil, p.errAt(t.Pos, "unexpected end of query, expected pipeline stage")
		}
		return nil, p.errAt(t.Pos, "unknown pipeline stage %s %q", t.Kind, t.Text)
	}
	switch t.Value {
	case "json":
		out = append(out, p.next())
		return p.parseParams(out, true)
	case "logfmt":
		out = append(out, p.next())
		var err error
		if out, err = p.logfmtFlags(out); err != nil {
			return nil, err
		}
		return p.parseParams(out, false)
	case "pattern", "regexp", "line_format":
		out = append(out, p.next())
		s := p.peek()
		if s.Kind != String && s.Kind != RawString {
			return nil, p.errAt(s.Pos, "%s needs a string argument", t.Value)
		}
		return append(out, p.next()), nil
	case "unpack", "decolorize":
		return append(out, p.next()), nil
	case "label_format":
		return p.labelFormat(out)
	case "drop", "keep":
		return p.dropKeep(out)
	case "unwrap":
		return p.unwrap(out, uw)
	}
	return nil, p.errAt(t.Pos, "unexpected keyword %q, expected pipeline stage", t.Text)
}

// parseParams parses the optional extraction parameters of json and logfmt:
// name="expr" (json: required "="; logfmt: bare names allowed), comma
// separated, no trailing comma.
func (p *parser) parseParams(out []Token, requireExpr bool) ([]Token, error) {
	if !isName(p.peek()) {
		return out, nil
	}
	for {
		name := p.peek()
		if !isName(name) {
			return nil, p.errAt(name.Pos, "expected label name, got %s %q", name.Kind, name.Text)
		}
		out = append(out, asIdent(p.next()))
		if isOp(p.peek(), "=") {
			out = append(out, p.next())
			s := p.peek()
			if s.Kind != String && s.Kind != RawString {
				return nil, p.errAt(s.Pos, "expected string expression for label %q", name.Text)
			}
			out = append(out, p.next())
		} else if requireExpr {
			return nil, p.errAt(p.peek().Pos, "json parameter %q needs =\"expression\"", name.Text)
		}
		if p.peek().Kind != Comma {
			return out, nil
		}
		out = append(out, p.next())
	}
}

// logfmtFlags reassembles --strict and --keep-empty, which the lexer
// delivers as '-', '-', name ['-' name].
func (p *parser) logfmtFlags(out []Token) ([]Token, error) {
	seen := map[string]bool{}
	for isOp(p.peek(), "-") {
		a, b, c := p.peekAt(0), p.peekAt(1), p.peekAt(2)
		if !isOp(b, "-") || b.Pos != a.End || !isName(c) || c.Pos != b.End {
			return nil, p.errAt(a.Pos, "unexpected '-' in logfmt parameters")
		}
		p.i += 3
		flag := "--" + c.Text
		end := c.End
		if d, e := p.peekAt(0), p.peekAt(1); isOp(d, "-") && d.Pos == end && isName(e) && e.Pos == d.End {
			p.i += 2
			flag += "-" + e.Text
			end = e.End
		}
		if flag != "--strict" && flag != "--keep-empty" {
			return nil, p.errAt(a.Pos, "unknown logfmt flag %q", flag)
		}
		if seen[flag] {
			return nil, p.errAt(a.Pos, "duplicate logfmt flag %q", flag)
		}
		seen[flag] = true
		out = append(out, Token{Kind: Ident, Text: flag, Value: flag, Pos: a.Pos, End: end})
	}
	return out, nil
}

func (p *parser) labelFormat(out []Token) ([]Token, error) {
	out = append(out, p.next())
	dst := map[string]bool{}
	for {
		name := p.peek()
		if !isName(name) {
			return nil, p.errAt(name.Pos, "label_format needs name=value, got %s %q", name.Kind, name.Text)
		}
		if dst[name.Text] {
			return nil, p.errAt(name.Pos, "duplicate label_format target %q", name.Text)
		}
		dst[name.Text] = true
		out = append(out, asIdent(p.next()))
		eq, err := p.expect(Op, "'='")
		if err != nil || eq.Text != "=" {
			return nil, p.errAt(eq.Pos, "label_format needs '=' after %q", name.Text)
		}
		out = append(out, eq)
		v := p.peek()
		switch {
		case isName(v):
			out = append(out, asIdent(p.next()))
		case v.Kind == String || v.Kind == RawString:
			out = append(out, p.next())
		default:
			return nil, p.errAt(v.Pos, "label_format needs a label or a template string for %q", name.Text)
		}
		if p.peek().Kind != Comma {
			return out, nil
		}
		out = append(out, p.next())
	}
}

func (p *parser) dropKeep(out []Token) ([]Token, error) {
	stage := p.next()
	out = append(out, stage)
	for {
		name := p.peek()
		if !isName(name) {
			return nil, p.errAt(name.Pos, "%s needs label names, got %s %q", stage.Value, name.Kind, name.Text)
		}
		out = append(out, asIdent(p.next()))
		if op := p.peek(); op.Kind == Op && (op.Text == "=" || op.Text == "!=" || op.Text == "=~" || op.Text == "!~") {
			out = append(out, p.next())
			v := p.peek()
			if v.Kind != String && v.Kind != RawString {
				return nil, p.errAt(v.Pos, "%s matcher needs a string value", stage.Value)
			}
			out = append(out, p.next())
		}
		if p.peek().Kind != Comma {
			return out, nil
		}
		out = append(out, p.next())
	}
}

func (p *parser) unwrap(out []Token, uw **Unwrap) ([]Token, error) {
	kw := p.next()
	out = append(out, kw)
	u := &Unwrap{Pos: kw.Pos}
	t := p.peek()
	if !isName(t) {
		return nil, p.errAt(t.Pos, "unwrap needs a label, got %s %q", t.Kind, t.Text)
	}
	if p.peekAt(1).Kind == LParen {
		conv := strings.ToLower(t.Text)
		if !unwrapConvs[conv] {
			return nil, p.errAt(t.Pos, "unknown unwrap conversion %q", t.Text)
		}
		u.Conv = conv
		out = append(out, asIdent(p.next()), p.next())
		l := p.peek()
		if !isName(l) {
			return nil, p.errAt(l.Pos, "unwrap conversion needs a label, got %s %q", l.Kind, l.Text)
		}
		u.Label = l.Text
		out = append(out, asIdent(p.next()))
		rp, err := p.expect(RParen, "')'")
		if err != nil {
			return nil, err
		}
		out = append(out, rp)
		u.End = rp.End
	} else {
		u.Label = t.Text
		out = append(out, asIdent(p.next()))
		u.End = t.End
	}
	*uw = u
	return out, nil
}

// labelFilter parses a label filter expression: atoms joined by and, or, ','
// or plain juxtaposition. Precedence is not interpreted (the pipeline stays
// opaque); the canonical form keeps the source order.
func (p *parser) labelFilter(out []Token) ([]Token, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	out, err := p.lfAtom(out)
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case isKw(t, "and") || isKw(t, "or") || t.Kind == Comma:
			out = append(out, p.next())
			if out, err = p.lfAtom(out); err != nil {
				return nil, err
			}
		case t.Kind == LParen || (isName(t) && p.peekAt(1).Kind == Op && comparisonOps[p.peekAt(1).Text]):
			if out, err = p.lfAtom(out); err != nil {
				return nil, err
			}
		default:
			return out, nil
		}
	}
}

func (p *parser) lfAtom(out []Token) ([]Token, error) {
	t := p.peek()
	if t.Kind == LParen {
		out = append(out, p.next())
		out, err := p.labelFilter(out)
		if err != nil {
			return nil, err
		}
		rp, err := p.expect(RParen, "')'")
		if err != nil {
			return nil, err
		}
		return append(out, rp), nil
	}
	if !isName(t) {
		if t.Kind == EOF {
			return nil, p.errAt(t.Pos, "unexpected end of query, expected label filter")
		}
		return nil, p.errAt(t.Pos, "expected label filter, got %s %q", t.Kind, t.Text)
	}
	out = append(out, asIdent(p.next()))
	op := p.peek()
	if op.Kind != Op || !comparisonOps[op.Text] {
		return nil, p.errAt(op.Pos, "expected comparison operator after label %q", t.Text)
	}
	out = append(out, p.next())
	v := p.peek()
	switch v.Kind {
	case String, RawString:
		return append(out, p.next()), nil
	case Number, Duration, Bytes:
		if op.Text == "=~" || op.Text == "!~" {
			return nil, p.errAt(v.Pos, "regex operator %q needs a string", op.Text)
		}
		return append(out, p.next()), nil
	case Keyword:
		if v.Value == "ip" && (op.Text == "=" || op.Text == "!=") {
			return p.ipCall(out)
		}
	}
	if v.Kind == EOF {
		return nil, p.errAt(v.Pos, "unexpected end of query, expected label filter value")
	}
	return nil, p.errAt(v.Pos, "invalid label filter value %s %q", v.Kind, v.Text)
}

// ------------------------------------------------------------ metric layer

type binInfo struct {
	prec  int
	op    string
	right bool
}

func binaryOp(t Token) (binInfo, bool) {
	switch t.Kind {
	case Keyword:
		switch t.Value {
		case "or":
			return binInfo{1, "or", false}, true
		case "and":
			return binInfo{2, "and", false}, true
		case "unless":
			return binInfo{2, "unless", false}, true
		}
	case Op:
		switch t.Text {
		case "==", "!=", ">", ">=", "<", "<=":
			return binInfo{3, t.Text, false}, true
		case "+", "-":
			return binInfo{4, t.Text, false}, true
		case "*", "/", "%":
			return binInfo{5, t.Text, false}, true
		case "^":
			return binInfo{6, "^", true}, true
		}
	}
	return binInfo{}, false
}

func isSetOp(op string) bool { return op == "and" || op == "or" || op == "unless" }

func isComparison(op string) bool {
	switch op {
	case "==", "!=", ">", ">=", "<", "<=":
		return true
	}
	return false
}

// isScalar reports whether e consists only of numeric literals.
func isScalar(e Expr) bool {
	switch e := e.(type) {
	case *Literal:
		return true
	case *BinaryExpr:
		return isScalar(e.LHS) && isScalar(e.RHS)
	}
	return false
}

func (p *parser) parseBinary(minPrec int) (Expr, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	lhs, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		opTok := p.peek()
		info, ok := binaryOp(opTok)
		if !ok || info.prec < minPrec {
			return lhs, nil
		}
		p.next()
		b := &BinaryExpr{Op: info.op, LHS: lhs, OpPos: opTok.Pos}
		if err := p.parseMatching(b); err != nil {
			return nil, err
		}
		nextPrec := info.prec + 1
		if info.right {
			nextPrec = info.prec
		}
		rhs, err := p.parseBinary(nextPrec)
		if err != nil {
			return nil, err
		}
		b.RHS = rhs
		if isSetOp(b.Op) {
			if isScalar(b.LHS) {
				return nil, p.errAt(opTok.Pos, "unexpected literal for left leg of logical/set binary operation")
			}
			if isScalar(b.RHS) {
				return nil, p.errAt(opTok.Pos, "unexpected literal for right leg of logical/set binary operation")
			}
		}
		lhs = b
	}
}

// parseMatching parses [bool] [on|ignoring (...) [group_left|group_right [(...)]]].
func (p *parser) parseMatching(b *BinaryExpr) error {
	if t := p.peek(); isKw(t, "bool") {
		if !isComparison(b.Op) {
			return p.errAt(t.Pos, "bool modifier is only allowed on comparison operators")
		}
		p.next()
		b.Bool = true
	}
	t := p.peek()
	if isKw(t, "on") || isKw(t, "ignoring") {
		p.next()
		labels, err := p.parseLabelList()
		if err != nil {
			return err
		}
		b.Matching = &VectorMatching{On: t.Value == "on", Labels: labels}
	}
	if g := p.peek(); isKw(g, "group_left") || isKw(g, "group_right") {
		if b.Matching == nil {
			return p.errAt(g.Pos, "%s needs on(...) or ignoring(...)", g.Value)
		}
		if isSetOp(b.Op) {
			return p.errAt(g.Pos, "%s is not allowed on set operators", g.Value)
		}
		p.next()
		b.Matching.Group = g.Value
		if p.peek().Kind == LParen {
			inc, err := p.parseLabelList()
			if err != nil {
				return err
			}
			b.Matching.Include = inc
		}
	}
	return nil
}

// parseLabelList parses '(' [name {',' name}] ')'.
func (p *parser) parseLabelList() ([]string, error) {
	if _, err := p.expect(LParen, "'('"); err != nil {
		return nil, err
	}
	var labels []string
	if p.peek().Kind == RParen {
		p.next()
		return labels, nil
	}
	for {
		t := p.peek()
		if !isName(t) {
			if t.Kind == EOF {
				return nil, p.errAt(t.Pos, "unexpected end of query, expected label name")
			}
			return nil, p.errAt(t.Pos, "expected label name, got %s %q", t.Kind, t.Text)
		}
		p.next()
		labels = append(labels, t.Text)
		if p.peek().Kind == Comma {
			p.next()
			continue
		}
		if _, err := p.expect(RParen, "',' or ')'"); err != nil {
			return nil, err
		}
		return labels, nil
	}
}

// parseNumber parses [-] Number.
func (p *parser) parseNumber() (*Literal, error) {
	t := p.peek()
	neg := ""
	if isOp(t, "-") {
		p.next()
		neg = "-"
	}
	n := p.peek()
	if n.Kind != Number {
		if n.Kind == EOF {
			return nil, p.errAt(n.Pos, "unexpected end of query, expected number")
		}
		if neg != "" {
			return nil, p.errAt(t.Pos, "unary minus is only supported on numeric literals")
		}
		return nil, p.errAt(n.Pos, "expected number, got %s %q", n.Kind, n.Text)
	}
	p.next()
	return &Literal{Text: neg + n.Text, Pos: t.Pos, End: n.End}, nil
}

func (p *parser) parseUnary() (Expr, error) {
	t := p.peek()
	if isOp(t, "-") {
		return p.parseNumber()
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.Kind {
	case Number:
		return p.parseNumber()
	case LParen:
		p.next()
		e, err := p.parseBinary(1)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(RParen, "')'"); err != nil {
			return nil, err
		}
		return e, nil
	case LBrace:
		return nil, p.errAt(t.Pos, "a log query is not allowed here; wrap it in a range aggregation")
	case Ident:
		name := strings.ToLower(t.Text)
		nxt := p.peekAt(1)
		if _, ok := rangeFuncs[name]; ok && nxt.Kind == LParen {
			return p.parseRangeAgg(name)
		}
		if _, ok := vectorFuncs[name]; ok && (nxt.Kind == LParen || isKw(nxt, "by") || isKw(nxt, "without")) {
			return p.parseVectorAgg(name)
		}
		if nxt.Kind == LParen {
			switch name {
			case "vector":
				return p.parseVector()
			case "label_replace":
				return p.parseLabelReplace()
			}
			return nil, p.errAt(t.Pos, "unsupported function %q", t.Text)
		}
	}
	return nil, p.unexpected(t)
}

func (p *parser) parseGrouping() (*Grouping, error) {
	kw := p.next()
	labels, err := p.parseLabelList()
	if err != nil {
		return nil, err
	}
	return &Grouping{Without: kw.Value == "without", Labels: labels}, nil
}

func (p *parser) parseVectorAgg(name string) (Expr, error) {
	start := p.next()
	v := &VectorAgg{Op: name, Pos: start.Pos}
	var err error
	if kw := p.peek(); isKw(kw, "by") || isKw(kw, "without") {
		if v.Grouping, err = p.parseGrouping(); err != nil {
			return nil, err
		}
	}
	if _, err := p.expect(LParen, "'('"); err != nil {
		return nil, err
	}
	if vectorFuncs[name] {
		if v.Param, err = p.parseNumber(); err != nil {
			return nil, err
		}
		if _, err := p.expect(Comma, "','"); err != nil {
			return nil, err
		}
	}
	if v.Arg, err = p.parseBinary(1); err != nil {
		return nil, err
	}
	rp, err := p.expect(RParen, "')'")
	if err != nil {
		return nil, err
	}
	v.End = rp.End
	if kw := p.peek(); isKw(kw, "by") || isKw(kw, "without") {
		if v.Grouping != nil {
			return nil, p.errAt(kw.Pos, "grouping given both before and after the arguments of %s", name)
		}
		if v.Grouping, err = p.parseGrouping(); err != nil {
			return nil, err
		}
		v.End = p.toks[p.i-1].End
	}
	if v.Grouping != nil && (name == "sort" || name == "sort_desc") {
		return nil, p.errAt(start.Pos, "grouping is not allowed for %s", name)
	}
	return v, nil
}

func (p *parser) parseVector() (Expr, error) {
	start := p.next()
	p.next() // '('
	n, err := p.parseNumber()
	if err != nil {
		return nil, err
	}
	rp, err := p.expect(RParen, "')'")
	if err != nil {
		return nil, err
	}
	return &VectorFunc{Arg: *n, Pos: start.Pos, End: rp.End}, nil
}

func (p *parser) parseString() (StringLit, error) {
	t := p.peek()
	if t.Kind != String && t.Kind != RawString {
		if t.Kind == EOF {
			return StringLit{}, p.errAt(t.Pos, "unexpected end of query, expected string")
		}
		return StringLit{}, p.errAt(t.Pos, "expected string argument, got %s %q", t.Kind, t.Text)
	}
	p.next()
	return StringLit{Value: t.Value, Raw: t.Text, Pos: t.Pos, End: t.End}, nil
}

func (p *parser) parseLabelReplace() (Expr, error) {
	start := p.next()
	p.next() // '('
	l := &LabelReplace{Pos: start.Pos}
	var err error
	if l.Arg, err = p.parseBinary(1); err != nil {
		return nil, err
	}
	for _, dst := range []*StringLit{&l.Dst, &l.Repl, &l.Src, &l.Regex} {
		if _, err := p.expect(Comma, "',' (label_replace takes exactly 5 arguments)"); err != nil {
			return nil, err
		}
		if *dst, err = p.parseString(); err != nil {
			return nil, err
		}
	}
	if _, err := regexp.Compile("^(?:" + l.Regex.Value + ")$"); err != nil {
		return nil, p.errAt(l.Regex.Pos, "invalid regular expression: %v", err)
	}
	rp, err := p.expect(RParen, "')' (label_replace takes exactly 5 arguments)")
	if err != nil {
		return nil, err
	}
	l.End = rp.End
	return l, nil
}

// validRangeDuration accepts integer number+unit groups with units ns ms s m
// h d w y. Decimals and the microsecond unit are rejected in range selectors
// and offsets (docs/research/logql.md section 4).
func validRangeDuration(t Token) bool {
	if t.Kind != Duration {
		return false
	}
	s := t.Text
	for len(s) > 0 {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 {
			return false
		}
		s = s[i:]
		unit := ""
		for _, u := range []string{"ns", "ms", "s", "m", "h", "d", "w", "y"} {
			if strings.HasPrefix(s, u) && len(u) > len(unit) {
				unit = u
			}
		}
		if unit == "" {
			return false
		}
		s = s[len(unit):]
	}
	return true
}

func (p *parser) parseRangeAgg(name string) (Expr, error) {
	start := p.next()
	p.next() // '('
	r := &RangeAgg{Func: name, Pos: start.Pos}
	p.aggs = append(p.aggs, r)
	needUnwrap := rangeFuncs[name]
	var err error
	if name == "quantile_over_time" {
		if r.Param, err = p.parseNumber(); err != nil {
			return nil, err
		}
		if _, err := p.expect(Comma, "','"); err != nil {
			return nil, err
		}
	}
	if p.peek().Kind == LParen {
		p.next()
		r.Range.Paren = true
	}
	if r.Range.Selector, err = p.parseSelector(); err != nil {
		return nil, err
	}
	if r.Range.Pipeline, r.Range.Unwrap, err = p.parsePipeline(); err != nil {
		return nil, err
	}
	if r.Range.Paren {
		if _, err := p.expect(RParen, "')'"); err != nil {
			return nil, err
		}
	}
	if needUnwrap && r.Range.Unwrap == nil {
		return nil, p.errAt(start.Pos, "%s requires an unwrap stage", name)
	}
	if !needUnwrap && r.Range.Unwrap != nil {
		return nil, p.errAt(r.Range.Unwrap.Pos, "unwrap is not allowed in %s", name)
	}
	if _, err := p.expect(LBracket, "'[' range selector"); err != nil {
		return nil, err
	}
	d := p.peek()
	if !validRangeDuration(d) {
		return nil, p.errAt(d.Pos, "invalid range %q: expected integer duration such as 5m (no decimals, no us unit)", d.Text)
	}
	p.next()
	r.Interval = d.Text
	if _, err := p.expect(RBracket, "']'"); err != nil {
		return nil, err
	}
	if off := p.peek(); isKw(off, "offset") {
		p.next()
		od := p.peek()
		if !validRangeDuration(od) {
			return nil, p.errAt(od.Pos, "invalid offset %q: expected integer duration such as 5m (no decimals, no us unit)", od.Text)
		}
		p.next()
		r.Offset = od.Text
	}
	rp, err := p.expect(RParen, "')'")
	if err != nil {
		return nil, err
	}
	r.End = rp.End
	if kw := p.peek(); isKw(kw, "by") || isKw(kw, "without") {
		if !needUnwrap {
			return nil, p.errAt(kw.Pos, "grouping not allowed for %s aggregation", name)
		}
		if r.Grouping, err = p.parseGrouping(); err != nil {
			return nil, err
		}
		r.End = p.toks[p.i-1].End
	}
	return r, nil
}
