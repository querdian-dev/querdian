package traceql

import (
	"math/rand"
	"strings"
	"testing"
)

const testDNF = `(resource.ns = "a" && span.x =~ "b|c") || (resource.ns = "d")`

func TestRenderDNF(t *testing.T) {
	cases := []struct {
		name  string
		terms []Term
		want  string
	}{
		{"single", []Term{{{"resource.ns", KindEq, "a"}}}, `(resource.ns = "a")`},
		{"two terms", []Term{
			{{"resource.ns", KindEq, "a"}, {"span.x", KindRegex, `b|c`}},
			{{"resource.ns", KindNeq, "d"}, {".y", KindNotRegex, "e.*"}},
		}, `(resource.ns = "a" && span.x =~ "b|c") || (resource.ns != "d" && .y !~ "e.*")`},
		{"dash name quoted", []Term{{{"span.dash-name", KindEq, "v"}}}, `(span."dash-name" = "v")`},
		{"slash name quoted", []Term{{{"resource.a/b", KindEq, "v"}}}, `(resource."a/b" = "v")`},
		{"dotted name bare", []Term{{{"resource.k8s.namespace.name", KindEq, "v"}}}, `(resource.k8s.namespace.name = "v")`},
		{"escapes", []Term{{{"span.x", KindEq, `a"b\c`}}}, `(span.x = "a\"b\\c")`},
		{"regex backslash", []Term{{{"span.x", KindRegex, `\d+`}}}, `(span.x =~ "\\d+")`},
		{"unicode", []Term{{{"span.x", KindEq, "äö"}}}, `(span.x = "äö")`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := RenderDNF(c.terms)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
			if _, err := Parse("{ " + got + " }"); err != nil {
				t.Errorf("does not parse: %v", err)
			}
		})
	}
}

func TestRenderDNFRejects(t *testing.T) {
	bad := [][]Term{
		nil,
		{{}},
		{{{"span.x", KindEq, "a\nb"}}},
		{{{"span.x", KindEq, "a\x00"}}},
		{{{"span.x", KindEq, "a\x7f"}}},
		{{{"span.x", KindEq, "a\xffb"}}},
		{{{"span.a\nb", KindEq, "v"}}},
		{{{"span.", KindEq, "v"}}},
		{{{"noscope", KindEq, "v"}}},
		{{{"bogus.x", KindEq, "v"}}},
		{{{"span.x", KindGt, "v"}}},
	}
	for i, terms := range bad {
		if got, err := RenderDNF(terms); err == nil {
			t.Errorf("case %d: expected error, got %q", i, got)
		}
	}
}

func TestInject(t *testing.T) {
	const d = testDNF
	cases := []struct{ in, want string }{
		{`{ span.a = "x" }`, `{ (span.a = "x") && (` + d + `) }`},
		{`{span.a="x"}`, `{ (span.a="x") && (` + d + `) }`},
		{`{}`, `{ ` + d + ` }`},
		{`{ }`, `{ ` + d + ` }`},
		{`{ span.a = "x" || span.b = "y" && span.c = "z" }`, `{ (span.a = "x" || span.b = "y" && span.c = "z") && (` + d + `) }`},
		{`{ span.a = "x" } >> { }`, `{ (span.a = "x") && (` + d + `) } >> { ` + d + ` }`},
		{`{ span.a = "x" } && { span.b = "y" }`, `{ (span.a = "x") && (` + d + `) } && { (span.b = "y") && (` + d + `) }`},
		{`({ span.a = "x" } ~ { span.b = "y" }) | count() > 1`, `({ (span.a = "x") && (` + d + `) } ~ { (span.b = "y") && (` + d + `) }) | count() > 1`},
		{`{ } | select(span.a)`, `{ ` + d + ` } | select(span.a)`},
		{`{ span.a = "x" } | rate() by (span.b)`, `{ (span.a = "x") && (` + d + `) } | rate() by (span.b)`},
		{`{ } | compare({ span.a = "x" })`, `{ ` + d + ` } | compare({ (span.a = "x") && (` + d + `) })`},
		{`{ } | {  }`, `{ ` + d + ` } | { ` + d + ` }`},
		{`{ span.a = "x" } with (sample=true)`, `{ (span.a = "x") && (` + d + `) } with (sample=true)`},
		{`count() > 10`, `{ ` + d + ` } | count() > 10`},
		{`by(span.a) | count() > 1`, `{ ` + d + ` } | by(span.a) | count() > 1`},
		{`{ span.s = "} | {" }`, `{ (span.s = "} | {") && (` + d + `) }`},
	}
	for _, c := range cases {
		q, err := Parse(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		got, err := Inject(q, d)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.in, got, c.want)
		}
	}
}

func TestInjectRejectsBadDNF(t *testing.T) {
	q, _ := Parse(`{ }`)
	for _, d := range []string{``, `true } | { true`, `span.a = `, `(span.a = "x") } | rate() | { true`, `true } with (a=1) | {`} {
		if out, err := Inject(q, d); err == nil {
			t.Errorf("dnf %q accepted: %s", d, out)
		}
	}
	if _, err := Inject(nil, testDNF); err == nil {
		t.Error("nil query accepted")
	}
}

// checkInjected asserts the property of T4.3: the output parses, has one filter per input filter
// (one if the input had none) and every filter ends with the parenthesised DNF.
func checkInjected(t *testing.T, in *Query, dnf string) {
	t.Helper()
	out, err := Inject(in, dnf)
	if err != nil {
		t.Fatalf("Inject(%q): %v", in.Source, err)
	}
	q, err := Parse(out)
	if err != nil {
		t.Fatalf("output %q does not parse: %v", out, err)
	}
	if want := max(len(in.Filters), 1); len(q.Filters) != want {
		t.Fatalf("%q -> %q: %d filters, want %d", in.Source, out, len(q.Filters), want)
	}
	for i, f := range q.Filters {
		s, e := f.InnerRange()
		inner := out[s:e]
		if len(f.Tokens) == 0 {
			t.Fatalf("%q: filter %d is empty in output", out, i)
		}
		if inner != dnf && !strings.HasSuffix(inner, " && ("+dnf+")") {
			t.Fatalf("%q: filter %d %q lacks DNF", out, i, inner)
		}
		if inner != dnf {
			// Top level of the injected filter must be exactly (X) && (dnf): AST check.
			b, ok := f.Expr.(*BinaryExpr)
			if !ok || b.Op != KindAnd {
				t.Fatalf("%q: filter %d top is not &&", out, i)
			}
			if _, ok := b.Left.(*ParenExpr); !ok {
				t.Fatalf("%q: filter %d left side not parenthesised", out, i)
			}
			if _, ok := b.Right.(*ParenExpr); !ok {
				t.Fatalf("%q: filter %d right side not parenthesised", out, i)
			}
		}
	}
	// Idempotence of denied attributes: injection adds only policy attributes.
	if len(q.Attributes()) < len(in.Attributes()) {
		t.Fatalf("%q lost attributes", out)
	}
}

func randomTerms(r *rand.Rand) []Term {
	names := []string{"resource.ns", "span.x", "resource.k8s.namespace.name", "span.dash-name", "span.sp ace", `span.q"uote`, ".un", "resource.a/b"}
	vals := []string{"a", "b|c", `x"y`, `back\slash`, "é", "", ".*", `\d+`}
	ops := []Kind{KindEq, KindNeq, KindRegex, KindNotRegex}
	terms := make([]Term, 1+r.Intn(4))
	for i := range terms {
		term := make(Term, 1+r.Intn(3))
		for j := range term {
			term[j] = Condition{names[r.Intn(len(names))], ops[r.Intn(len(ops))], vals[r.Intn(len(vals))]}
		}
		terms[i] = term
	}
	return terms
}

func TestInjectCorpusProperty(t *testing.T) {
	r := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test data
	n := 0
	for _, c := range corpus {
		in, err := Parse(c.query)
		if err != nil {
			continue
		}
		for range 5 {
			dnf, err := RenderDNF(randomTerms(r))
			if err != nil {
				t.Fatal(err)
			}
			checkInjected(t, in, dnf)
			n++
		}
	}
	if n < 400 {
		t.Fatalf("only %d cases ran", n)
	}
}

func FuzzInject(f *testing.F) {
	for _, c := range corpus {
		f.Add(c.query, int64(len(c.query)))
	}
	f.Fuzz(func(t *testing.T, in string, seed int64) {
		q, err := Parse(in)
		if err != nil {
			return
		}
		dnf, err := RenderDNF(randomTerms(rand.New(rand.NewSource(seed)))) //nolint:gosec // deterministic test data
		if err != nil {
			t.Fatal(err)
		}
		checkInjected(t, q, dnf)
	})
}
