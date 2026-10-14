package traceql

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func kinds(toks []Token) []Kind {
	out := make([]Kind, len(toks))
	for i, t := range toks {
		out[i] = t.Kind
	}
	return out
}

func TestLexKinds(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Kind
	}{
		{"empty", "", nil},
		{"whitespace only", " \t\r\n ", nil},
		{"empty filter", "{ }", []Kind{KindLBrace, KindRBrace}},
		{"basic filter", `{ span.http.status_code >= 500 && .team = "t1" }`,
			[]Kind{KindLBrace, KindAttribute, KindGte, KindNumber, KindAnd, KindAttribute, KindEq, KindString, KindRBrace}},
		{"no spaces", `{span.a="x"}>>{span.a="y"}`,
			[]Kind{KindLBrace, KindAttribute, KindEq, KindString, KindRBrace, KindDescendant, KindLBrace, KindAttribute, KindEq, KindString, KindRBrace}},
		{"regex", `{ span.a =~ "x|y" || span.a !~ "z" }`,
			[]Kind{KindLBrace, KindAttribute, KindRegex, KindString, KindOr, KindAttribute, KindNotRegex, KindString, KindRBrace}},
		{"comparison", `a != b > c < d <= e`, nil}, // placeholder replaced below
		{"durations", `{ duration > 200ms && duration < 1.5s } | avg(duration) > 1h`,
			[]Kind{KindLBrace, KindIntrinsic, KindGt, KindDuration, KindAnd, KindIntrinsic, KindLt, KindDuration, KindRBrace,
				KindPipe, KindIdent, KindLParen, KindIntrinsic, KindRParen, KindGt, KindDuration}},
		{"colon intrinsics", `{ span:name = "a" && trace:duration > 1s && trace:rootService = "x" }`,
			[]Kind{KindLBrace, KindIntrinsic, KindEq, KindString, KindAnd, KindIntrinsic, KindGt, KindDuration, KindAnd, KindIntrinsic, KindEq, KindString, KindRBrace}},
		{"quoted attr", `{ span."a b" = 1 && resource."service.name" = "x" && ."q" = 2 }`,
			[]Kind{KindLBrace, KindAttribute, KindEq, KindNumber, KindAnd, KindAttribute, KindEq, KindString, KindAnd, KindAttribute, KindEq, KindNumber, KindRBrace}},
		{"pipeline", `{ } | count() > 1 | by(resource.service.name) | select(a, b)`,
			nil},
		{"metrics", `{ status = error } | quantile_over_time(duration, .99) by (span.http.target) with (exemplars=true)`,
			[]Kind{KindLBrace, KindIntrinsic, KindEq, KindIdent, KindRBrace, KindPipe, KindIdent, KindLParen, KindIntrinsic, KindComma, KindNumber, KindRParen,
				KindIdent, KindLParen, KindAttribute, KindRParen, KindIdent, KindLParen, KindIdent, KindEq, KindIdent, KindRParen}},
		{"arithmetic", `{ 1 + 2 - 3 * 4 / 5 % 6 ^ 7 }`,
			[]Kind{KindLBrace, KindNumber, KindAdd, KindNumber, KindSub, KindNumber, KindMul, KindNumber, KindDiv, KindNumber, KindMod, KindNumber, KindPow, KindNumber, KindRBrace}},
		{"not and parens", `{ !(a = 1) }`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			toks, err := Lex(tc.in)
			if err != nil {
				t.Fatalf("Lex(%q): %v", tc.in, err)
			}
			checkTokens(t, tc.in, toks)
			if tc.want == nil && len(toks) > 0 {
				return // only checking that it lexes and round-trips
			}
			got := kinds(toks)
			if len(got) != len(tc.want) {
				t.Fatalf("kinds = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("token %d kind = %v, want %v (all: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

func TestLexStructuralOperators(t *testing.T) {
	ops := map[string]Kind{
		">>": KindDescendant, "<<": KindAncestor, ">": KindGt, "<": KindLt, "~": KindSibling,
		"&>>": KindUnionDescendant, "&<<": KindUnionAncestor, "&>": KindUnionChild, "&<": KindUnionParent, "&~": KindUnionSibling,
		"!>>": KindNotDescendant, "!<<": KindNotAncestor, "!>": KindNotChild, "!<": KindNotParent, "!~": KindNotRegex,
		"&&": KindAnd, "||": KindOr, "=~": KindRegex, "!=": KindNeq, ">=": KindGte, "<=": KindLte, "!": KindNot,
	}
	for op, want := range ops {
		for _, in := range []string{"{a}" + op + "{b}", "{ a } " + op + " { b }"} {
			toks, err := Lex(in)
			if err != nil {
				t.Fatalf("Lex(%q): %v", in, err)
			}
			checkTokens(t, in, toks)
			var found bool
			for _, tk := range toks {
				if tk.Text == op {
					found = true
					if tk.Kind != want {
						t.Errorf("%q: kind = %v, want %v", in, tk.Kind, want)
					}
				}
			}
			if !found {
				t.Errorf("%q: operator %q not found in %v", in, op, toks)
			}
		}
	}
}

func TestLexValues(t *testing.T) {
	tests := []struct {
		in           string
		kind         Kind
		scope, value string
	}{
		{`"plain"`, KindString, "", "plain"},
		{`"a\"b\\c"`, KindString, "", `a"b\c`},
		{`""`, KindString, "", ""},
		{`span.http.status_code`, KindAttribute, "span", "http.status_code"},
		{`resource.service.name`, KindAttribute, "resource", "service.name"},
		{`.team`, KindAttribute, "", "team"},
		{`.http.status_code`, KindAttribute, "", "http.status_code"},
		{`span."a b"`, KindAttribute, "span", "a b"},
		{`span."q\"x\\y"`, KindAttribute, "span", `q"x\y`},
		{`span.""`, KindAttribute, "span", ""},
		{`."http.status_code"`, KindAttribute, "", "http.status_code"},
		{`event.foo`, KindAttribute, "event", "foo"},
		{`link.foo`, KindAttribute, "link", "foo"},
		{`instrumentation.foo`, KindAttribute, "instrumentation", "foo"},
		{`span.é_1`, KindAttribute, "span", "é_1"},
		{`.99abc`, KindAttribute, "", "99abc"},
		{`span:name`, KindIntrinsic, "span", "name"},
		{`span:statusMessage`, KindIntrinsic, "span", "statusMessage"},
		{`trace:rootName`, KindIntrinsic, "trace", "rootName"},
		{`event:timeSinceStart`, KindIntrinsic, "event", "timeSinceStart"},
		{`link:spanID`, KindIntrinsic, "link", "spanID"},
		{`instrumentation:version`, KindIntrinsic, "instrumentation", "version"},
		{`duration`, KindIntrinsic, "", "duration"},
		{`traceDuration`, KindIntrinsic, "", "traceDuration"},
		{`rootServiceName`, KindIntrinsic, "", "rootServiceName"},
		{`error`, KindIdent, "", ""},
		{`nil`, KindIdent, "", ""},
		{`200`, KindNumber, "", ""},
		{`200.0`, KindNumber, "", ""},
		{`.99`, KindNumber, "", ""},
		{`0.2s`, KindDuration, "", ""},
		{`1d`, KindDuration, "", ""},
		{`5ns`, KindDuration, "", ""},
		{`5us`, KindDuration, "", ""},
		{`5m`, KindDuration, "", ""},
		{`5ms`, KindDuration, "", ""},
	}
	for _, tc := range tests {
		toks, err := Lex(tc.in)
		if err != nil {
			t.Errorf("Lex(%q): %v", tc.in, err)
			continue
		}
		if len(toks) != 1 {
			t.Errorf("Lex(%q) = %d tokens, want 1: %v", tc.in, len(toks), toks)
			continue
		}
		tk := toks[0]
		if tk.Kind != tc.kind || tk.Scope != tc.scope || tk.Value != tc.value || tk.Text != tc.in || tk.Start != 0 || tk.End != len(tc.in) {
			t.Errorf("Lex(%q) = %+v, want kind=%v scope=%q value=%q", tc.in, tk, tc.kind, tc.scope, tc.value)
		}
	}
}

func TestLexPositions(t *testing.T) {
	in := "{ span.a = \"x\" }\n| count() > 1"
	toks, err := Lex(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range toks {
		if in[tk.Start:tk.End] != tk.Text {
			t.Errorf("token %v: Text %q != input slice %q", tk.Kind, tk.Text, in[tk.Start:tk.End])
		}
	}
	if toks[1].Start != 2 || toks[1].End != 8 {
		t.Errorf("attribute offsets = %d..%d, want 2..8", toks[1].Start, toks[1].End)
	}
}

func TestLexErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		pos  int
	}{
		{"newline escape", `{ span.a = "x\ny" }`, 13},
		{"other escape", `"a\tb"`, 2},
		{"raw newline in string", "\"a\nb\"", 2},
		{"unterminated string", `{ span.a = "x`, 11},
		{"backtick string", "{ span.a = `x` }", 11},
		{"unterminated backtick", "`abc", 0},
		{"unterminated escape", `"abc\`, 4},
		{"single quoted", `{ span.a = 'x' }`, 11},
		{"hash comment", `{ } # c`, 4},
		{"slash comment", `{ } // c`, 4},
		{"lone ampersand", `{ a & b }`, 4},
		{"and keyword", `{ a and b }`, -1}, // lexes fine as idents; parser rejects
		{"unknown char", `{ a @ b }`, 4},
		{"semicolon", `{ };`, 3},
		{"uppercase scope", `{ SPAN.a = 1 }`, 2},
		{"unknown scope", `{ foo.a = 1 }`, 2},
		{"parent scope", `{ parent.span.a = 1 }`, 2},
		{"unknown colon scope", `{ foo:name = 1 }`, 2},
		{"unknown intrinsic", `{ span:nope = 1 }`, 7},
		{"empty intrinsic", `{ span: = 1 }`, 7},
		{"dot only", `{ . = 1 }`, 2},
		{"scope dot only", `{ span. = 1 }`, 2},
		{"trailing dot", `{ span.a. = 1 }`, 8},
		{"double dot", `{ span.a..b = 1 }`, 9},
		{"unquoted space name", `{ span.weird name = 1 }`, -1}, // lexes as attr + ident; parser rejects
		{"dash in name", `{ span.dash-name = 1 }`, 11},
		{"slash in name", `{ span.slash/name = 1 }`, 12},
		{"compound duration", `{ duration > 1h30m }`, 15},
		{"bad unit", `{ duration > 5x }`, 14},
		{"number glued to ident", `{ 1abc }`, 3},
		{"exponent", `{ 1e3 }`, 3},
		{"trailing decimal point", `{ 1. }`, 3},
		{"double decimal point", `{ 1.2.3 }`, 5},
		{"invalid utf8", "{ \xff }", 2},
		{"nul byte", "{ \x00 }", 2},
		{"control char in name", "span.\"a\x01\"", 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			toks, err := Lex(tc.in)
			if tc.pos == -1 {
				if err != nil {
					t.Fatalf("Lex(%q): unexpected error %v", tc.in, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Lex(%q) = %v, want error", tc.in, toks)
			}
			var le *Error
			if !errors.As(err, &le) {
				t.Fatalf("error %T is not *Error", err)
			}
			if le.Pos != tc.pos {
				t.Errorf("Lex(%q) error pos = %d (%v), want %d", tc.in, le.Pos, err, tc.pos)
			}
			if !strings.Contains(err.Error(), "byte") {
				t.Errorf("error text lacks position: %v", err)
			}
		})
	}
}

// checkTokens verifies the structural invariants: ordered, non-overlapping tokens whose Text
// matches the input slice, with only whitespace between them.
func checkTokens(t testing.TB, in string, toks []Token) {
	t.Helper()
	prev := 0
	var sb strings.Builder
	for _, tk := range toks {
		if tk.Start < prev || tk.End <= tk.Start || tk.End > len(in) {
			t.Fatalf("bad token bounds %+v (prev end %d, len %d) for %q", tk, prev, len(in), in)
		}
		if in[tk.Start:tk.End] != tk.Text {
			t.Fatalf("token text %q != input %q", tk.Text, in[tk.Start:tk.End])
		}
		if strings.Trim(in[prev:tk.Start], " \t\r\n") != "" {
			t.Fatalf("non-whitespace gap %q before %+v", in[prev:tk.Start], tk)
		}
		if tk.Kind == KindIllegal {
			t.Fatalf("illegal token %+v", tk)
		}
		sb.WriteString(tk.Text)
		prev = tk.End
	}
	if strings.Trim(in[prev:], " \t\r\n") != "" {
		t.Fatalf("non-whitespace tail %q", in[prev:])
	}
	if strings.Join(strings.Fields(in), "") != strings.Join(strings.Fields(sb.String()), "") {
		// whitespace inside strings is preserved in Text, so compare with all whitespace removed
		t.Fatalf("concatenation mismatch: %q vs %q", sb.String(), in)
	}
}

func FuzzLex(f *testing.F) {
	seeds := []string{
		``, `{ }`, `{ span.a = "x" && .b =~ "y|z" }`, `{a}>>{b} &>> {c} !~ {d}`,
		`{ span."a b" = "q\"" } | count() > 1`, `{ duration > 1.5s }`, `{ a = "raw" }`,
		`quantile_over_time(duration, .99)`, `{ span:name = "x" } | by(resource.service.name)`,
		`{ span.a-b = 1 }`, `{ "\n" }`, `.99.`, `1h30m`, "\xff",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		toks, err := Lex(in)
		if err != nil {
			var le *Error
			if !errors.As(err, &le) || le.Pos < 0 || le.Pos > len(in) {
				t.Fatalf("bad error %v for %q", err, in)
			}
			return
		}
		if !utf8.ValidString(in) {
			t.Fatalf("accepted invalid UTF-8 %q", in)
		}
		checkTokens(t, in, toks)
	})
}
