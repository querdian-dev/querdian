package logql

import (
	"errors"
	"strings"
	"testing"
)

// shape summarises a Query without positions, for round-trip comparison.
func shape(q *Query) string {
	var sb strings.Builder
	sb.WriteString(q.Kind.String() + "|" + q.String())
	for _, s := range q.Selectors {
		sb.WriteString("|" + s.String())
	}
	for _, r := range q.RangeAggs {
		sb.WriteString("|" + r.Func + "[" + r.Interval + "]")
	}
	return sb.String()
}

func TestCorpusSize(t *testing.T) {
	if len(corpus) < 150 {
		t.Fatalf("corpus has %d entries, want >= 150", len(corpus))
	}
	for _, c := range corpus {
		if !strings.HasPrefix(c.url, "https://grafana.com/docs/") {
			t.Errorf("bad doc url %q", c.url)
		}
	}
}

func TestCorpus(t *testing.T) {
	for _, c := range corpus {
		q, err := Parse(c.q)
		if err != nil {
			t.Errorf("%s\n  %s\n  parse error: %v", c.url, c.q, err)
			continue
		}
		if q.Kind != c.kind {
			t.Errorf("%s: kind = %v, want %v", c.q, q.Kind, c.kind)
		}
		if q.Source != c.q {
			t.Errorf("%s: Source mismatch", c.q)
		}
		s := q.String()
		q2, err := Parse(s)
		if err != nil {
			t.Errorf("%s\n  canonical %q does not re-parse: %v", c.q, s, err)
			continue
		}
		if shape(q) != shape(q2) {
			t.Errorf("%s: round trip differs:\n  %s\n  %s", c.q, shape(q), shape(q2))
		}
		if s2 := q2.String(); s2 != s {
			t.Errorf("%s: canonical form not stable: %q vs %q", c.q, s, s2)
		}
		if len(q.Selectors) == 0 && !strings.HasPrefix(c.q, "vector") && !isNumeric(c.q) {
			// Pure scalar queries have no selector; everything else has one.
			if !strings.Contains(c.q, "vector(") {
				t.Errorf("%s: no selectors found", c.q)
			}
		}
		if c.kind == KindLog && len(q.RangeAggs) != 0 {
			t.Errorf("%s: log query with range aggregations", c.q)
		}
		for _, s := range q.Selectors {
			if c.q[s.Pos] != '{' || c.q[s.End-1] != '}' {
				t.Errorf("%s: selector span %d-%d wrong", c.q, s.Pos, s.End)
			}
		}
		for _, r := range q.RangeAggs {
			if !strings.EqualFold(c.q[r.Pos:r.Pos+len(r.Func)], r.Func) {
				t.Errorf("%s: range agg pos %d wrong", c.q, r.Pos)
			}
			if c.q[r.End-1] != ')' && r.Grouping == nil {
				t.Errorf("%s: range agg end %d wrong", c.q, r.End)
			}
		}
	}
}

func isNumeric(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789.e+-*/%^<>=! ()bool", c) {
			return false
		}
	}
	return true
}

func TestCanonical(t *testing.T) {
	tests := []struct{ in, want string }{
		{`{app="foo",env=~"a|b"}|="x"!="y"`, `{app="foo", env=~"a|b"} |= "x" != "y"`},
		{`{a="b"} | JSON | LOGFMT --strict`, `{a="b"} | json | logfmt --strict`},
		{`{a="b"} | logfmt - -strict` + "", ``},
		{`SUM(Rate({a="b"}[5m])) BY (x)`, `sum by (x) (rate({a="b"}[5m]))`},
		{`sum(rate({a="b"}[5m])) by (x)`, `sum by (x) (rate({a="b"}[5m]))`},
		{`{a="b"} # c` + "\n" + `|= "x"`, `{a="b"} |= "x"`},
		{`((1))`, `1`},
		{`1 + 2 * 3`, `1 + (2 * 3)`},
		{`2 ^ 3 ^ 2`, `2 ^ (3 ^ 2)`},
		{`-2 ^ 2`, `-2 ^ 2`},
		{`10 - 2 - 3`, `(10 - 2) - 3`},
		{`10 - (2 - 3)`, `10 - (2 - 3)`},
		{`vector(1) unless vector(1) or vector(7)`, `(vector(1) unless vector(1)) or vector(7)`},
		{`vector(1) or vector(1) unless vector(7)`, `vector(1) or (vector(1) unless vector(7))`},
		{`vector(1) and vector(2) unless vector(3)`, `(vector(1) and vector(2)) unless vector(3)`},
		{`vector(1) + vector(2) > bool vector(3)`, `(vector(1) + vector(2)) > bool vector(3)`},
		{`rate({a="b"}[5m]) / ON(x) group_LEFT (y) rate({a="c"}[5m])`, `rate({a="b"}[5m]) / on (x) group_left (y) rate({a="c"}[5m])`},
		{`count_over_time({a="b"}[5m] OFFSET 1h)`, `count_over_time({a="b"}[5m] offset 1h)`},
		{`{a="b"} |= "x" OR "y"`, `{a="b"} |= "x" or "y"`},
		{`{a="b"} | x="1" AND y="2"`, `{a="b"} | x = "1" and y = "2"`},
		{`{a="b"} | drop JSON`, `{a="b"} | drop JSON`},
		{`{a="b"} | JSON = "x"`, `{a="b"} | JSON = "x"`},
		{`{a="b"} | x = ip ( "1.2.3.4" )`, `{a="b"} | x = ip("1.2.3.4")`},
		{`{a="b"} | label_format x=y,z="{{.w}}"`, `{a="b"} | label_format x = y, z = "{{.w}}"`},
	}
	for _, tt := range tests {
		if tt.want == "" {
			if _, err := Parse(tt.in); err == nil {
				t.Errorf("Parse(%q) should fail", tt.in)
			}
			continue
		}
		q, err := Parse(tt.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.in, err)
			continue
		}
		if got := q.String(); got != tt.want {
			t.Errorf("String(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPrecedenceTree(t *testing.T) {
	q, err := Parse(`1 + 2 * 3 ^ 2 ^ 2 - 4`)
	if err != nil {
		t.Fatal(err)
	}
	want := `(1 + (2 * (3 ^ (2 ^ 2)))) - 4`
	if got := q.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Comparison binds tighter than set operators, looser than arithmetic.
	q, err = Parse(`vector(1) > vector(2) + vector(3) or vector(4) < vector(5)`)
	if err != nil {
		t.Fatal(err)
	}
	want = `(vector(1) > (vector(2) + vector(3))) or (vector(4) < vector(5))`
	if got := q.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPositions(t *testing.T) {
	src := `sum by (x) (rate({a="b"} |= "q" [5m]) / count_over_time(({c=~"d.+"} | json)[1m] offset 2m) by (y))`
	_ = src
	src = `rate({a="b"} |= "q" [5m]) / sum_over_time({c=~"d.+"} | json | unwrap n [1m] offset 2m) by (y)`
	q, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Selectors) != 2 || len(q.RangeAggs) != 2 {
		t.Fatalf("selectors=%d aggs=%d", len(q.Selectors), len(q.RangeAggs))
	}
	if got := src[q.Selectors[0].Pos:q.Selectors[0].End]; got != `{a="b"}` {
		t.Errorf("selector 0 = %q", got)
	}
	if got := src[q.Selectors[1].Pos:q.Selectors[1].End]; got != `{c=~"d.+"}` {
		t.Errorf("selector 1 = %q", got)
	}
	m := q.Selectors[1].Matchers[0]
	if m.Name != "c" || m.Op != "=~" || m.Value != "d.+" || src[m.NamePos:m.NamePos+1] != "c" {
		t.Errorf("matcher = %+v", m)
	}
	r0, r1 := q.RangeAggs[0], q.RangeAggs[1]
	if got := src[r0.Pos:r0.End]; got != `rate({a="b"} |= "q" [5m])` {
		t.Errorf("agg 0 = %q", got)
	}
	if got := src[r1.Pos:r1.End]; !strings.HasSuffix(got, "by (y)") || !strings.HasPrefix(got, "sum_over_time(") {
		t.Errorf("agg 1 = %q", got)
	}
	if r1.Offset != "2m" || r1.Interval != "1m" || r1.Range.Unwrap == nil || r1.Range.Unwrap.Label != "n" {
		t.Errorf("agg 1 = %+v", r1)
	}
	if got := src[r1.Range.Unwrap.Pos:r1.Range.Unwrap.End]; got != "unwrap n" {
		t.Errorf("unwrap span = %q", got)
	}
	for _, tok := range r0.Range.Pipeline {
		if src[tok.Pos:tok.End] != tok.Text {
			t.Errorf("token %+v position mismatch", tok)
		}
	}
	// Nested range aggregations are listed outer first, in source order.
	q, err = Parse(`sum(rate({a="b"}[1m])) / sum(rate({a="c"}[2m]))`)
	if err != nil {
		t.Fatal(err)
	}
	if q.RangeAggs[0].Interval != "1m" || q.RangeAggs[1].Interval != "2m" {
		t.Error("range aggregation order")
	}
}

func TestPipelineTokens(t *testing.T) {
	src := `{a="b"} | logfmt --strict --keep-empty x | JSON = "y" or z=~"q" |= "s" or "t"`
	q, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	pipe := q.Expr.(*LogExpr).Pipeline
	var texts []string
	for _, tok := range pipe {
		texts = append(texts, tok.Text)
	}
	got := strings.Join(texts, " ")
	want := `| logfmt --strict --keep-empty x | JSON = "y" or z =~ "q" |= "s" or "t"`
	if got != want {
		t.Errorf("tokens = %s\nwant     %s", got, want)
	}
	// The keyword JSON is a label name here: re-kinded to Ident, 'or' stays a keyword.
	for _, tok := range pipe {
		if tok.Text == "JSON" && tok.Kind != Ident {
			t.Errorf("label JSON kind = %v", tok.Kind)
		}
		if tok.Text == "or" && tok.Kind != Keyword {
			t.Errorf("or kind = %v", tok.Kind)
		}
		if tok.Text == "--strict" && src[tok.Pos:tok.End] != "--strict" {
			t.Errorf("flag span = %q", src[tok.Pos:tok.End])
		}
	}
}

func TestReject(t *testing.T) {
	tests := []struct{ name, q string }{
		{"empty", ``},
		{"comment only", `# nothing`},
		{"empty selector", `{}`},
		{"empty compatible regex", `{app=~".*"}`},
		{"empty regex", `{app=~""}`},
		{"empty equality", `{app=""}`},
		{"only negative matchers", `{app!="x"}`},
		{"only negated regex", `{app!~"x"}`},
		{"trailing comma", `{app="foo",}`},
		{"double comma", `{app="foo",,env="a"}`},
		{"unterminated selector", `{app="foo"`},
		{"missing value", `{app=}`},
		{"unquoted value", `{app=foo}`},
		{"== in selector", `{app=="foo"}`},
		{"bad regex", `{app=~"("}`},
		{"number label", `{1="a"}`},
		{"selector in selector", `{{app="a"}}`},
		{"unknown stage", `{app="a"} | foo`},
		{"distinct is not a stage", `{app="a"} | distinct foo`},
		{"stage without bar", `{app="a"} json`},
		{"bar at end", `{app="a"} |`},
		{"line filter without string", `{app="a"} |= 5`},
		{"line filter chained or", `{app="a"} |= "a" or |= "b"`},
		{"line filter trailing or", `{app="a"} |= "a" or`},
		{"or with label filter", `{app="a"} |= "a" or x="b"`},
		{"ip in regex filter", `{app="a"} |~ ip("1.2.3.4")`},
		{"ip in pattern filter", `{app="a"} |> ip("1.2.3.4")`},
		{"ip with number", `{app="a"} != ip(1)`},
		{"ip unclosed", `{app="a"} != ip("1.2.3.4"`},
		{"json bare label", `{app="a"} | json foo`},
		{"json trailing comma", `{app="a"} | json a="b",`},
		{"json flag", `{app="a"} | json --strict`},
		{"logfmt unknown flag", `{app="a"} | logfmt --foo`},
		{"logfmt duplicate flag", `{app="a"} | logfmt --strict --strict`},
		{"logfmt flag with space", `{app="a"} | logfmt - -strict`},
		{"logfmt single dash", `{app="a"} | logfmt -strict`},
		{"logfmt upper flag", `{app="a"} | logfmt --STRICT`},
		{"logfmt trailing comma", `{app="a"} | logfmt a,`},
		{"pattern without string", `{app="a"} | pattern`},
		{"regexp number", `{app="a"} | regexp 5`},
		{"line_format ident", `{app="a"} | line_format foo`},
		{"unpack with arg", `{app="a"} | unpack "x"`},
		{"label_format duplicate", `{app="a"} | label_format a=b,a=c`},
		{"label_format trailing comma", `{app="a"} | label_format a=b,`},
		{"label_format no value", `{app="a"} | label_format a`},
		{"label_format number", `{app="a"} | label_format a=5`},
		{"drop trailing comma", `{app="a"} | drop level,`},
		{"drop nothing", `{app="a"} | drop`},
		{"drop number", `{app="a"} | drop 5`},
		{"keep matcher without value", `{app="a"} | keep x=`},
		{"label filter no value", `{app="a"} | x =`},
		{"label filter no op", `{app="a"} | x "y"`},
		{"label filter regex number", `{app="a"} | x =~ 5`},
		{"label filter ip with <", `{app="a"} | x < ip("1.2.3.4")`},
		{"label filter space in bytes", `{app="a"} | x > 20 KB`},
		{"label filter bad bytes", `{app="a"} | x > 1kb`},
		{"label filter negative number", `{app="a"} | x > -1`},
		{"label filter dangling and", `{app="a"} | x = "1" and`},
		{"label filter dangling comma", `{app="a"} | x = "1",`},
		{"label filter unbalanced", `{app="a"} | (x = "1"`},
		{"label filter unbalanced close", `{app="a"} | x = "1")`},
		{"label filter empty paren", `{app="a"} | ()`},
		{"unwrap in log query", `{app="a"} | logfmt | unwrap size`},
		{"unwrap no label", `rate({a="b"} | unwrap [5m])`},
		{"unwrap unknown conv", `sum_over_time({a="b"} | unwrap foo(x) [5m])`},
		{"unwrap conv unclosed", `sum_over_time({a="b"} | unwrap bytes(x [5m])`},
		{"line_format after unwrap", `sum_over_time({a="b"} | unwrap x | line_format "y" [5m])`},
		{"line filter after unwrap", `sum_over_time({a="b"} | unwrap x |= "y" [5m])`},
		{"parser after unwrap", `sum_over_time({a="b"} | unwrap x | json [5m])`},
		{"double unwrap", `sum_over_time({a="b"} | unwrap x | unwrap y [5m])`},
		{"unwrapped func without unwrap", `sum_over_time({a="b"}[5m])`},
		{"quantile without unwrap", `quantile_over_time(0.9, {a="b"}[5m])`},
		{"quantile without param", `quantile_over_time({a="b"} | unwrap x [5m])`},
		{"unwrap in rate", `rate({a="b"} | unwrap x [5m])`},
		{"unwrap in count_over_time", `count_over_time({a="b"} | unwrap x [5m])`},
		{"grouping on rate", `rate({a="b"}[5m]) by (x)`},
		{"grouping on count_over_time", `count_over_time({a="b"}[5m]) without (x)`},
		{"grouping before range agg", `sum_over_time by (x) ({a="b"} | unwrap x [5m])`},
		{"decimal range", `rate({a="b"}[1.5m])`},
		{"us range", `rate({a="b"}[1us])`},
		{"micro sign range", `rate({a="b"}[1µs])`},
		{"space in range", `rate({a="b"}[5 m])`},
		{"upper M range", `rate({a="b"}[5M])`},
		{"no unit range", `rate({a="b"}[5])`},
		{"bytes range", `rate({a="b"}[5KB])`},
		{"subquery", `rate({a="b"}[5m:1m])`},
		{"empty range", `rate({a="b"}[])`},
		{"missing range", `rate({a="b"})`},
		{"unclosed range", `rate({a="b"}[5m)`},
		{"decimal offset", `rate({a="b"}[5m] offset 1.5m)`},
		{"us offset", `rate({a="b"}[5m] offset 1us)`},
		{"offset without duration", `rate({a="b"}[5m] offset)`},
		{"offset after call", `count_over_time({a="b"}[5m]) offset 5m`},
		{"offset after aggregation", `sum(count_over_time({a="b"}[5m]) offset 1m)`},
		{"offset after paren", `(count_over_time({a="b"}[5m])) offset 1m`},
		{"offset on literal", `1 offset 5m`},
		{"offset before range", `rate({a="b"} offset 5m [5m])`},
		{"range before pipeline", `rate({a="b"}[5m] | json)`},
		{"log query in call", `rate(5)`},
		{"bare log query as operand", `{a="b"} + 1`},
		{"bare log query in sum", `sum({a="b"})`},
		{"parenthesised log query", `({a="b"})`},
		{"log query then metric op", `{a="b"} or {a="c"}`},
		{"unknown function", `foo({a="b"}[5m])`},
		{"unsupported function", `abs(rate({a="b"}[5m]))`},
		{"count_values", `count_values("x", rate({a="b"}[5m]))`},
		{"approx_topk", `approx_topk(1, rate({a="b"}[5m]))`},
		{"bare identifier", `foo`},
		{"bare keyword", `by`},
		{"sum no parens", `sum rate({a="b"}[5m])`},
		{"sum two args", `sum(rate({a="b"}[5m]), 1)`},
		{"sum empty", `sum()`},
		{"sum grouping twice", `sum by (a) (rate({a="b"}[5m])) by (b)`},
		{"sum grouping before and after", `sum by (a) (rate({a="b"}[5m])) without (b)`},
		{"grouping trailing comma", `sum by (a,) (rate({a="b"}[5m]))`},
		{"grouping string label", `sum by ("a") (rate({a="b"}[5m]))`},
		{"grouping unclosed", `sum by (a (rate({a="b"}[5m]))`},
		{"sort grouped", `sort by (a) (rate({a="b"}[5m]))`},
		{"topk without k", `topk(rate({a="b"}[5m]))`},
		{"topk expression k", `topk(1+1, rate({a="b"}[5m]))`},
		{"topk string k", `topk("1", rate({a="b"}[5m]))`},
		{"sum with param", `sum(1, rate({a="b"}[5m]))`},
		{"vector expression", `vector(1+1)`},
		{"vector selector", `vector({a="b"})`},
		{"vector empty", `vector()`},
		{"vector two args", `vector(1, 2)`},
		{"label_replace 4 args", `label_replace(rate({a="b"}[5m]), "x", "$1", "y")`},
		{"label_replace 6 args", `label_replace(rate({a="b"}[5m]), "x", "$1", "y", "(.*)", "z")`},
		{"label_replace non-string", `label_replace(rate({a="b"}[5m]), x, "$1", "y", "(.*)")`},
		{"label_replace bad regex", `label_replace(rate({a="b"}[5m]), "x", "$1", "y", "(")`},
		{"label_replace log query", `label_replace({a="b"}, "x", "$1", "y", "(.*)")`},
		{"literal left of or", `1 or 2`},
		{"literal left of and", `1 and rate({a="b"}[5m])`},
		{"literal left of unless", `1 unless rate({a="b"}[5m])`},
		{"literal right of or", `rate({a="b"}[5m]) or 1`},
		{"scalar expression left of or", `1 + 2 or rate({a="b"}[5m])`},
		{"parenthesised literal of or", `(1) or rate({a="b"}[5m])`},
		{"bool on set op", `rate({a="b"}[5m]) and bool rate({a="c"}[5m])`},
		{"bool on arithmetic", `rate({a="b"}[5m]) + bool 1`},
		{"group_left on set op", `rate({a="b"}[5m]) and on(a) group_left rate({a="c"}[5m])`},
		{"group_left without on", `rate({a="b"}[5m]) / group_left rate({a="c"}[5m])`},
		{"on without parens", `rate({a="b"}[5m]) / on a rate({a="c"}[5m])`},
		{"on trailing comma", `rate({a="b"}[5m]) / on(a,) rate({a="c"}[5m])`},
		{"missing rhs", `rate({a="b"}[5m]) +`},
		{"missing lhs", `+ rate({a="b"}[5m])`},
		{"unary plus", `+1`},
		{"unary minus on call", `-rate({a="b"}[5m])`},
		{"unary minus on paren", `-(1)`},
		{"double unary minus", `--1`},
		{"unclosed paren", `(1 + 2`},
		{"extra paren", `1 + 2)`},
		{"empty paren", `()`},
		{"trailing tokens", `1 2`},
		{"trailing tokens after log", `{a="b"} )`},
		{"two selectors", `{a="b"} {c="d"}`},
		{"single quotes", `{a='b'}`},
		{"bad escape", `{a="\q"}`},
		{"invalid utf8", "{a=\"\xff\"}"},
		{"unterminated string", `{a="b}`},
		{"bad character", `{a="b"} @ 5`},
		{"colon", `rate({a="b"}[5m:])`},
		{"comparison chain no bool literal garbage", `1 > > 2`},
		{"deep nesting", strings.Repeat("(", 500) + "1" + strings.Repeat(")", 500)},
	}
	for _, tt := range tests {
		q, err := Parse(tt.q)
		if err == nil {
			t.Errorf("%s: Parse(%q) accepted, canonical %q", tt.name, tt.q, q.String())
			continue
		}
		var pe *ParseError
		var le *Error
		if !errors.As(err, &pe) && !errors.As(err, &le) {
			t.Errorf("%s: error %T is neither *ParseError nor *Error", tt.name, err)
		}
	}
}

func TestErrorMessages(t *testing.T) {
	tests := []struct{ q, want string }{
		{`count_over_time({a="b"}[5m]) offset 5m`, "offset must directly follow"},
		{`1 or 2`, "left leg"},
		{`rate({a="b"}[1.5m])`, "no decimals"},
		{`rate({a="b"}[1us])`, "no us unit"},
		{`rate({a="b"}[5m]) by (x)`, "grouping not allowed"},
		{`{a="b"} | distinct x`, "unknown pipeline stage"},
		{`abs(1)`, "unsupported function"},
		{`{a="b"} | logfmt | unwrap x`, "inside a range aggregation"},
		{`{app=~".*"}`, "does not match the empty string"},
	}
	for _, tt := range tests {
		_, err := Parse(tt.q)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Parse(%q) error = %v, want substring %q", tt.q, err, tt.want)
		}
	}
	_, err := Parse(`{a="b"} | foo`)
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Pos != 10 {
		t.Errorf("error position = %v", err)
	}
}

func TestQueryKindString(t *testing.T) {
	if KindLog.String() != "log" || KindMetric.String() != "metric" || QueryKind(0).String() != "unknown" {
		t.Error("QueryKind.String")
	}
}

func FuzzParseRoundTrip(f *testing.F) {
	for _, c := range corpus {
		f.Add(c.q)
	}
	f.Add(`{a="b"} | logfmt - - strict`)
	f.Add(`-1 ^ -2`)
	f.Fuzz(func(t *testing.T, src string) {
		q, err := Parse(src)
		if err != nil {
			return
		}
		s := q.String()
		q2, err := Parse(s)
		if err != nil {
			t.Fatalf("canonical form %q of %q does not parse: %v", s, src, err)
		}
		if shape(q) != shape(q2) {
			t.Fatalf("round trip differs for %q:\n  %s\n  %s", src, shape(q), shape(q2))
		}
		if s2 := q2.String(); s2 != s {
			t.Fatalf("canonical form unstable for %q: %q vs %q", src, s, s2)
		}
	})
}
