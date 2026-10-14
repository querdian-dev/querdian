package logql

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// TenantIDLabel is the reserved label Loki exposes under tenant federation.
// Loki rejects a stream selector whose only matcher is this label (400), so
// the injector refuses to produce such a selector (see [ErrNoAnchor]).
const TenantIDLabel = "__tenant_id__"

// Errors returned by the injector.
var (
	// ErrNoAnchor means an injected selector would lack a matcher other than
	// __tenant_id__ that cannot match the empty string.
	ErrNoAnchor = errors.New("logql: injected selector has no non-__tenant_id__ anchor matcher")
	// ErrLiftUnsupported means the query cannot be or-lifted over several
	// terms without changing its result (a log query, or a range aggregation
	// with a by/without clause that aggregates across streams).
	ErrLiftUnsupported = errors.New("logql: query cannot be or-lifted over several policy terms")
)

// InjectMatcher is one label matcher to be added to a stream selector.
type InjectMatcher struct {
	// Name is the label name, [A-Za-z_][A-Za-z0-9_]*.
	Name string
	// Op is one of "=", "!=", "=~", "!~".
	Op string
	// Value is the undecoded label value or regular expression; it is quoted
	// by the injector.
	Value string
}

var labelNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (m InjectMatcher) validate() error {
	if !labelNameRE.MatchString(m.Name) {
		return fmt.Errorf("logql: invalid label name %q", m.Name)
	}
	switch m.Op {
	case "=", "!=", "=~", "!~":
	default:
		return fmt.Errorf("logql: invalid matcher operator %q", m.Op)
	}
	if !utf8.ValidString(m.Value) {
		return fmt.Errorf("logql: matcher value for %q is not valid UTF-8", m.Name)
	}
	return nil
}

// quote renders s as a double-quoted LogQL string using only escapes the
// lexer accepts.
func quote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			sb.WriteString(`\"`)
		case r == '\\':
			sb.WriteString(`\\`)
		case r == '\n':
			sb.WriteString(`\n`)
		case r == '\t':
			sb.WriteString(`\t`)
		case r == '\r':
			sb.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&sb, `\x%02x`, r)
		default:
			sb.WriteRune(r)
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// Inject adds ms to every stream selector of q (including selectors inside
// label_replace and both operands of binary operations) and returns the
// canonical rendering of the rewritten query. q is not modified.
//
// The result is re-parsed before it is returned and checked: it has the same
// number of selectors, every selector carries all of ms, and every selector
// keeps a non-__tenant_id__ matcher that cannot match the empty string
// ([ErrNoAnchor] otherwise). Any failure is returned as an error; callers
// must never fall back to the original query.
func Inject(q *Query, ms []InjectMatcher) (string, error) {
	return rewrite(q, [][]InjectMatcher{ms})
}

// PolicySelector renders the stream selector consisting only of ms, for
// endpoints whose query parameter is optional (labels, series, ...): when the
// client sends no selector, the policy term itself is the selector. The
// result is re-parsed and must keep a non-__tenant_id__ anchor matcher;
// otherwise [ErrNoAnchor] is returned (this includes an empty ms).
func PolicySelector(ms []InjectMatcher) (string, error) {
	sel := &Selector{Matchers: make([]Matcher, 0, len(ms))}
	for _, m := range ms {
		if err := m.validate(); err != nil {
			return "", err
		}
		sel.Matchers = append(sel.Matchers, Matcher{Name: m.Name, Op: m.Op, Value: m.Value, Raw: quote(m.Value)})
	}
	if !hasAnchor(sel) {
		return "", ErrNoAnchor
	}
	out := sel.String()
	q, err := Parse(out)
	if err != nil {
		return "", fmt.Errorf("logql: policy selector does not parse: %w", err)
	}
	if len(q.Selectors) != 1 || !carries(q.Selectors[0], ms) {
		return "", errors.New("logql: policy selector does not carry the policy term")
	}
	return out, nil
}

// OrLift rewrites a metric query for the union of several policy terms. Every
// range aggregation f(sel[r]) is replaced by
// "f(sel+t1[r]) or f(sel+t2[r]) or ...", so every stream selector carries
// exactly the matchers of one term. A term without matchers leaves its copy
// unrestricted. With a single term OrLift equals [Inject].
//
// OrLift returns [ErrLiftUnsupported] for log queries (use one [Inject] per
// term and merge the responses) and, with several terms, for range
// aggregations with a by/without clause, whose aggregation across streams
// cannot be split per term. A query without any stream selector (only
// vector() and literals) is returned unchanged. The result is verified as in
// [Inject]; every selector must carry all matchers of at least one term.
//
// Limitation: "or" keeps only the first of several series with identical
// label sets, so when a pipeline collapses distinct streams to the same label
// set (for example drop or keep), the counts of terms after the first may be
// under-reported. This never grants more than the policy allows.
func OrLift(q *Query, terms [][]InjectMatcher) (string, error) {
	return rewrite(q, terms)
}

func rewrite(q *Query, terms [][]InjectMatcher) (string, error) {
	if q == nil || q.Expr == nil {
		return "", errors.New("logql: nil query")
	}
	if len(terms) == 0 {
		return "", errors.New("logql: no terms to inject")
	}
	for _, t := range terms {
		for _, m := range t {
			if err := m.validate(); err != nil {
				return "", err
			}
		}
	}
	if len(terms) > 1 && q.Kind != KindMetric {
		return "", fmt.Errorf("%w: log query", ErrLiftUnsupported)
	}
	e, err := (&injector{terms: terms}).expr(q.Expr)
	if err != nil {
		return "", err
	}
	out := e.String()
	if err := verify(q, out, terms); err != nil {
		return "", err
	}
	return out, nil
}

type injector struct {
	terms [][]InjectMatcher
}

func (in *injector) selector(s *Selector, ms []InjectMatcher) *Selector {
	c := &Selector{Pos: s.Pos, End: s.End, Matchers: make([]Matcher, 0, len(s.Matchers)+len(ms))}
	c.Matchers = append(c.Matchers, s.Matchers...)
	for _, m := range ms {
		c.Matchers = append(c.Matchers, Matcher{Name: m.Name, Op: m.Op, Value: m.Value, Raw: quote(m.Value)})
	}
	return c
}

func (in *injector) expr(e Expr) (Expr, error) {
	switch n := e.(type) {
	case *LogExpr:
		c := *n
		c.Selector = in.selector(n.Selector, in.terms[0])
		return &c, nil
	case *RangeAgg:
		if len(in.terms) == 1 {
			c := *n
			c.Range.Selector = in.selector(n.Range.Selector, in.terms[0])
			return &c, nil
		}
		if n.Grouping != nil {
			return nil, fmt.Errorf("%w: %s with %s clause", ErrLiftUnsupported, n.Func, n.Grouping.String())
		}
		var out Expr
		for _, t := range in.terms {
			c := *n
			c.Range.Selector = in.selector(n.Range.Selector, t)
			if out == nil {
				out = &c
			} else {
				out = &BinaryExpr{Op: "or", LHS: out, RHS: &c}
			}
		}
		return out, nil
	case *VectorAgg:
		arg, err := in.expr(n.Arg)
		if err != nil {
			return nil, err
		}
		c := *n
		c.Arg = arg
		return &c, nil
	case *LabelReplace:
		arg, err := in.expr(n.Arg)
		if err != nil {
			return nil, err
		}
		c := *n
		c.Arg = arg
		return &c, nil
	case *BinaryExpr:
		l, err := in.expr(n.LHS)
		if err != nil {
			return nil, err
		}
		r, err := in.expr(n.RHS)
		if err != nil {
			return nil, err
		}
		c := *n
		c.LHS, c.RHS = l, r
		return &c, nil
	case *Literal, *VectorFunc:
		return e, nil
	}
	return nil, fmt.Errorf("logql: unsupported expression %T", e)
}

// verify re-parses out and checks the injection invariants.
func verify(orig *Query, out string, terms [][]InjectMatcher) error {
	rq, err := Parse(out)
	if err != nil {
		return fmt.Errorf("logql: rewritten query does not parse: %w", err)
	}
	want := len(orig.Selectors)
	if len(terms) > 1 {
		want = len(orig.RangeAggs) * len(terms)
	}
	if len(rq.Selectors) != want {
		return fmt.Errorf("logql: rewritten query has %d selectors, want %d", len(rq.Selectors), want)
	}
	for _, s := range rq.Selectors {
		if !carriesAny(s, terms) {
			return errors.New("logql: rewritten selector does not carry a policy term")
		}
		if !hasAnchor(s) {
			return ErrNoAnchor
		}
	}
	return nil
}

func carriesAny(s *Selector, terms [][]InjectMatcher) bool {
	for _, t := range terms {
		if carries(s, t) {
			return true
		}
	}
	return false
}

func carries(s *Selector, ms []InjectMatcher) bool {
	for _, m := range ms {
		found := false
		for _, sm := range s.Matchers {
			if sm.Name == m.Name && sm.Op == m.Op && sm.Value == m.Value {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// hasAnchor reports whether s has a matcher other than __tenant_id__ that
// satisfies the parser's non-empty rule.
func hasAnchor(s *Selector) bool {
	for _, m := range s.Matchers {
		if m.Name == TenantIDLabel {
			continue
		}
		switch m.Op {
		case "=":
			if m.Value != "" {
				return true
			}
		case "!=":
			if m.Value == "" {
				return true
			}
		case "=~":
			if re, err := regexp.Compile("^(?:" + m.Value + ")$"); err == nil && !re.MatchString("") {
				return true
			}
		}
	}
	return false
}

// DeniedLabel scans q for denied label names and returns the first denied
// label found. The scan is deliberately conservative:
//
//   - every [Ident] or [Keyword] token outside strings, anywhere in the query
//     (selectors, pipeline stages, by/without, on/ignoring, group_left/right,
//     unwrap), whose spelling equals a denied label;
//   - the dst and src string arguments of label_replace, which can overwrite
//     or copy any label (the regex and replacement are not label names);
//   - every string of a line_format or label_format stage that contains a
//     denied label name as a substring, because templates read labels by name.
//
// The result is false when q references no denied label. An error is returned
// only if q.Source cannot be lexed, in which case the caller must reject.
func DeniedLabel(q *Query, denied []string) (string, bool, error) {
	if len(denied) == 0 {
		return "", false, nil
	}
	set := make(map[string]struct{}, len(denied))
	for _, d := range denied {
		set[d] = struct{}{}
	}
	toks, err := Lex(q.Source)
	if err != nil {
		return "", false, err
	}
	for _, t := range toks {
		if t.Kind != Ident && t.Kind != Keyword {
			continue
		}
		if _, ok := set[t.Text]; ok {
			return t.Text, true, nil
		}
		if t.Kind == Keyword {
			if _, ok := set[t.Value]; ok {
				return t.Value, true, nil
			}
		}
	}
	var found string
	walk(q.Expr, func(e Expr) {
		if found != "" {
			return
		}
		switch n := e.(type) {
		case *LabelReplace:
			for _, s := range []string{n.Dst.Value, n.Src.Value} {
				if _, ok := set[s]; ok {
					found = s
					return
				}
			}
		case *LogExpr:
			found = templateDenied(n.Pipeline, denied)
		case *RangeAgg:
			found = templateDenied(n.Range.Pipeline, denied)
		}
	})
	return found, found != "", nil
}

// templateDenied returns a denied label named inside a line_format or
// label_format stage string of pipe, or "".
func templateDenied(pipe []Token, denied []string) string {
	inTmpl := false
	for i, t := range pipe {
		switch {
		case t.Kind == Op && t.Text == "|":
			inTmpl = false
			if i+1 < len(pipe) && pipe[i+1].Kind == Keyword &&
				(pipe[i+1].Value == "line_format" || pipe[i+1].Value == "label_format") {
				inTmpl = true
			}
		case t.Kind == Op && lineFilterOps[t.Text]:
			inTmpl = false
		case inTmpl && (t.Kind == String || t.Kind == RawString):
			for _, d := range denied {
				if strings.Contains(t.Value, d) {
					return d
				}
			}
		}
	}
	return ""
}

func walk(e Expr, fn func(Expr)) {
	fn(e)
	switch n := e.(type) {
	case *VectorAgg:
		walk(n.Arg, fn)
	case *LabelReplace:
		walk(n.Arg, fn)
	case *BinaryExpr:
		walk(n.LHS, fn)
		walk(n.RHS, fn)
	}
}
