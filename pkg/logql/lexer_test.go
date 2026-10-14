package logql

import (
	"errors"
	"strings"
	"testing"
)

type tk struct {
	k Kind
	t string
}

func kinds(toks []Token) []tk {
	out := make([]tk, len(toks))
	for i, t := range toks {
		out[i] = tk{t.Kind, t.Text}
	}
	return out
}

func TestLexValid(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []tk
	}{
		{"empty", "", nil},
		{"whitespace only", " \t\r\n", nil},
		{"selector", `{app="foo",env=~"a|b"}`, []tk{
			{LBrace, "{"}, {Ident, "app"}, {Op, "="}, {String, `"foo"`}, {Comma, ","},
			{Ident, "env"}, {Op, "=~"}, {String, `"a|b"`}, {RBrace, "}"}}},
		{"line filters", `{a="b"}|="x"!="y"|~"z"!~"w"|>"p"!>"q"`, []tk{
			{LBrace, "{"}, {Ident, "a"}, {Op, "="}, {String, `"b"`}, {RBrace, "}"},
			{Op, "|="}, {String, `"x"`}, {Op, "!="}, {String, `"y"`}, {Op, "|~"}, {String, `"z"`},
			{Op, "!~"}, {String, `"w"`}, {Op, "|>"}, {String, `"p"`}, {Op, "!>"}, {String, `"q"`}}},
		{"comparison ops", `== != >= <= > < =`, []tk{
			{Op, "=="}, {Op, "!="}, {Op, ">="}, {Op, "<="}, {Op, ">"}, {Op, "<"}, {Op, "="}}},
		{"arithmetic", `1+2-3*4/5%6^7`, []tk{
			{Number, "1"}, {Op, "+"}, {Number, "2"}, {Op, "-"}, {Number, "3"}, {Op, "*"},
			{Number, "4"}, {Op, "/"}, {Number, "5"}, {Op, "%"}, {Number, "6"}, {Op, "^"}, {Number, "7"}}},
		{"raw string", "`raw\\w+ \"q\"`", []tk{{RawString, "`raw\\w+ \"q\"`"}}},
		{"raw string multiline", "`a\nb`", []tk{{RawString, "`a\nb`"}}},
		{"hash in string", `|= "x#y"`, []tk{{Op, "|="}, {String, `"x#y"`}}},
		{"hash in raw string", "|= `x#y`", []tk{{Op, "|="}, {RawString, "`x#y`"}}},
		{"comment", "{a=\"b\"} # c |= \"zzz\"\n|= \"x\"", []tk{
			{LBrace, "{"}, {Ident, "a"}, {Op, "="}, {String, `"b"`}, {RBrace, "}"},
			{Op, "|="}, {String, `"x"`}}},
		{"comment before query", "# hi\n{a=\"b\"}", []tk{
			{LBrace, "{"}, {Ident, "a"}, {Op, "="}, {String, `"b"`}, {RBrace, "}"}}},
		{"comment at eof", "{} # x", []tk{{LBrace, "{"}, {RBrace, "}"}}},
		{"numbers", `1 1.5 .5 1e3 1E-2 2.5e+10 0`, []tk{
			{Number, "1"}, {Number, "1.5"}, {Number, ".5"}, {Number, "1e3"},
			{Number, "1E-2"}, {Number, "2.5e+10"}, {Number, "0"}}},
		{"durations", `250ms 0.25s 1.5s 1h1m1s 1.5h 2m30s 1s1ms 1ns 1us 1µs 1μs 1d 1w 1y 5m 0s`, []tk{
			{Duration, "250ms"}, {Duration, "0.25s"}, {Duration, "1.5s"}, {Duration, "1h1m1s"},
			{Duration, "1.5h"}, {Duration, "2m30s"}, {Duration, "1s1ms"}, {Duration, "1ns"},
			{Duration, "1us"}, {Duration, "1µs"}, {Duration, "1μs"}, {Duration, "1d"},
			{Duration, "1w"}, {Duration, "1y"}, {Duration, "5m"}, {Duration, "0s"}}},
		{"bytes", `20KB 20kB 20k 1G 1B 1.5KB 1KiB 1.5MiB 2TiB 2Ki 3M 4Mi 5MB 6GiB`, []tk{
			{Bytes, "20KB"}, {Bytes, "20kB"}, {Bytes, "20k"}, {Bytes, "1G"}, {Bytes, "1B"},
			{Bytes, "1.5KB"}, {Bytes, "1KiB"}, {Bytes, "1.5MiB"}, {Bytes, "2TiB"}, {Bytes, "2Ki"},
			{Bytes, "3M"}, {Bytes, "4Mi"}, {Bytes, "5MB"}, {Bytes, "6GiB"}}},
		{"range selector", `rate({a="b"}[5m] offset 1h)`, []tk{
			{Ident, "rate"}, {LParen, "("}, {LBrace, "{"}, {Ident, "a"}, {Op, "="}, {String, `"b"`},
			{RBrace, "}"}, {LBracket, "["}, {Duration, "5m"}, {RBracket, "]"}, {Keyword, "offset"},
			{Duration, "1h"}, {RParen, ")"}}},
		{"keywords case-insensitive", `SUM BY Without JSON logfmt Offset AND or Unless`, []tk{
			{Ident, "SUM"}, {Keyword, "BY"}, {Keyword, "Without"}, {Keyword, "JSON"},
			{Keyword, "logfmt"}, {Keyword, "Offset"}, {Keyword, "AND"}, {Keyword, "or"},
			{Keyword, "Unless"}}},
		{"flags", `logfmt --strict --keep-empty`, []tk{
			{Keyword, "logfmt"}, {Op, "-"}, {Op, "-"}, {Ident, "strict"}, {Op, "-"}, {Op, "-"},
			{Keyword, "keep"}, {Op, "-"}, {Ident, "empty"}}},
		{"unwrap", `| unwrap bytes(size)`, []tk{
			{Op, "|"}, {Keyword, "unwrap"}, {Ident, "bytes"}, {LParen, "("}, {Ident, "size"}, {RParen, ")"}}},
		{"identifiers", `_a a1 __tenant_id__ Inf`, []tk{
			{Ident, "_a"}, {Ident, "a1"}, {Ident, "__tenant_id__"}, {Ident, "Inf"}}},
		{"number then ident-like operator context", `5 > 2`, []tk{{Number, "5"}, {Op, ">"}, {Number, "2"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toks, err := Lex(tt.src)
			if err != nil {
				t.Fatalf("Lex(%q): %v", tt.src, err)
			}
			got := kinds(toks)
			if len(got) != len(tt.want) {
				t.Fatalf("Lex(%q) = %v, want %v", tt.src, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("Lex(%q)[%d] = %v, want %v", tt.src, i, got[i], tt.want[i])
				}
			}
			checkInvariants(t, tt.src, toks)
		})
	}
}

func TestLexValues(t *testing.T) {
	tests := []struct{ src, want string }{
		{`"abc"`, "abc"},
		{`"a\"b"`, `a"b`},
		{`"a\\b"`, `a\b`},
		{`"\n\a\b\f\r\t\v"`, "\n\a\b\f\r\t\v"},
		{`"\x41"`, "A"},
		{`"\x4a\x4A"`, "JJ"},
		{`"\101"`, "A"},
		{`"\377"`, "\xff"},
		{`"\000"`, "\x00"},
		{"\"a\tb\"", "a\tb"},
		{"`raw\\w+`", `raw\w+`},
		{"``", ""},
		{`""`, ""},
		{`"é世"`, "é世"},
		{`"msg=\"hello\""`, `msg="hello"`},
	}
	for _, tt := range tests {
		toks, err := Lex(tt.src)
		if err != nil {
			t.Fatalf("Lex(%q): %v", tt.src, err)
		}
		if len(toks) != 1 || toks[0].Value != tt.want {
			t.Errorf("Lex(%q) value = %q, want %q (%v)", tt.src, toks[0].Value, tt.want, toks)
		}
	}
	toks, err := Lex("OFFSET")
	if err != nil || toks[0].Value != "offset" || toks[0].Text != "OFFSET" || !toks[0].Is("offset") {
		t.Errorf("keyword value: %v %v", toks, err)
	}
}

func TestLexPositions(t *testing.T) {
	src := "{a=\"b\"} # c\n  |= `x`"
	toks, err := Lex(src)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 6}, {6, 7}, {14, 16}, {17, 20}}
	if len(toks) != len(want) {
		t.Fatalf("got %d tokens: %v", len(toks), toks)
	}
	for i, w := range want {
		if toks[i].Pos != w[0] || toks[i].End != w[1] {
			t.Errorf("token %d (%q) = [%d,%d), want %v", i, toks[i].Text, toks[i].Pos, toks[i].End, w)
		}
	}
}

func TestLexErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		pos  int
	}{
		{"single quote", `{a='b'}`, 3},
		{"unterminated string", `{a="b`, 3},
		{"unterminated string after escape", `"a\`, 2},
		{"unterminated raw", "{a=`b", 3},
		{"newline in string", "\"a\nb\"", 2},
		{"nul in string", "\"a\x00b\"", 2},
		{"bad escape q", `"a\q"`, 2},
		{"escape u", "\"\\u0041\"", 1},
		{"escape U", `"\U00000041"`, 1},
		{"escape single quote", `"\'"`, 1},
		{"short hex", `"\x4"`, 1},
		{"bad hex", `"\xzz"`, 1},
		{"short octal", `"\10"`, 1},
		{"octal range", `"\400"`, 1},
		{"octal 8", `"\18a"`, 1},
		{"bang alone", `a ! b`, 2},
		{"colon", `[5m:1m]`, 3},
		{"dot alone", `a.b`, 1},
		{"at", `a @ b`, 2},
		{"semicolon", `a;`, 1},
		{"dollar", `$x`, 0},
		{"vtab", "a\vb", 1},
		{"formfeed", "a\fb", 1},
		{"nul", "a\x00", 1},
		{"invalid utf8", "a\xff", 1},
		{"invalid utf8 in string", "\"a\xff\"", 2},
		{"non-ascii ident", "é", 0},
		{"hex number", `0x10`, 0},
		{"number glued to ident", `1abc`, 0},
		{"number then 1kb", `1kb`, 0},
		{"2PB", `x 2PB`, 2},
		{"bare K", `1K`, 0},
		{"space unit is separate", `a 5 !`, 4},
		{"trailing dot", `1.`, 0},
		{"double dot", `1.5.5`, 0},
		{"dangling exponent", `1e`, 0},
		{"exponent then ident", `1e3x`, 0},
		{"duration missing unit", `1h30`, 0},
		{"duration bad tail", `1h30x`, 0},
		{"duration glued bytes", `1mB`, 0},
		{"compound bytes", `1KB5B`, 0},
		{"uppercase duration", `5S`, 0},
		{"ms glued digit", `5ms5`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toks, err := Lex(tt.src)
			if err == nil {
				t.Fatalf("Lex(%q) = %v, want error", tt.src, kinds(toks))
			}
			var le *Error
			if !errors.As(err, &le) {
				t.Fatalf("error type %T", err)
			}
			if le.Pos != tt.pos {
				t.Errorf("Lex(%q) error pos = %d (%v), want %d", tt.src, le.Pos, err, tt.pos)
			}
			if !strings.Contains(err.Error(), "byte") {
				t.Errorf("error text lacks position: %v", err)
			}
		})
	}
}

func TestKindString(t *testing.T) {
	if Comma.String() != "Comma" || Kind(99).String() != "Kind(99)" {
		t.Error("Kind.String")
	}
}

// checkInvariants verifies the token-stream contract used by FuzzLex.
func checkInvariants(t *testing.T, src string, toks []Token) {
	t.Helper()
	prev := 0
	for i, tok := range toks {
		if tok.Pos < prev || tok.End <= tok.Pos || tok.End > len(src) {
			t.Fatalf("token %d out of order or range: %+v (prev end %d, len %d)", i, tok, prev, len(src))
		}
		if tok.Text != src[tok.Pos:tok.End] {
			t.Fatalf("token %d text %q != src[%d:%d] %q", i, tok.Text, tok.Pos, tok.End, src[tok.Pos:tok.End])
		}
		if !onlyGap(src[prev:tok.Pos]) {
			t.Fatalf("gap before token %d is not whitespace/comment: %q", i, src[prev:tok.Pos])
		}
		prev = tok.End
	}
	if !onlyGap(src[prev:]) {
		t.Fatalf("trailing gap is not whitespace/comment: %q", src[prev:])
	}
}

// onlyGap reports whether s consists solely of whitespace and # comments.
func onlyGap(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
		case '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		default:
			return false
		}
	}
	return true
}

// FuzzLex checks that Lex never panics, that errors carry an in-range
// position, and that concatenating the tokens (with the skipped whitespace and
// comments) reproduces the input exactly.
func FuzzLex(f *testing.F) {
	for _, s := range []string{
		``, `{app="foo"}`, `{a=~"x|y"} |= "a\tb" != ` + "`raw`", `rate({a="b"}[5m] offset 1h)`,
		`sum by (a) (count_over_time({a="b"} | logfmt | unwrap size [1h30m]))`,
		`| duration > 1.5s and size >= 20KiB`, "# c\n{a=\"#\"}", `"\x41\101\n"`, `1e3 .5 1µs`,
		`'x'`, "\"\\u0041\"", "\"a\nb\"", `1kb`, "\xff", `--strict`, `{a="b"} |> "<_> x"`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		toks, err := Lex(src)
		if err != nil {
			var le *Error
			if !errors.As(err, &le) || le.Pos < 0 || le.Pos > len(src) {
				t.Fatalf("bad error %v for %q", err, src)
			}
			return
		}
		checkInvariants(t, src, toks)
		var sb strings.Builder
		prev := 0
		for _, tok := range toks {
			sb.WriteString(src[prev:tok.Pos])
			sb.WriteString(tok.Text)
			prev = tok.End
		}
		sb.WriteString(src[prev:])
		if sb.String() != src {
			t.Fatalf("reassembly mismatch for %q", src)
		}
	})
}
