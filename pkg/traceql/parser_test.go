package traceql

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCorpusSize(t *testing.T) {
	if len(corpus) < 120 {
		t.Fatalf("corpus has %d queries, want >= 120", len(corpus))
	}
	for i, c := range corpus {
		if !strings.HasPrefix(c.url, "https://grafana.com/docs/") {
			t.Errorf("case %d: missing doc URL", i)
		}
	}
}

func TestCorpusParses(t *testing.T) {
	for _, c := range corpus {
		t.Run(c.query, func(t *testing.T) {
			q, err := Parse(c.query)
			if err != nil {
				t.Fatalf("%s\nParse(%q): %v", c.url, c.query, err)
			}
			checkFilters(t, q)
			checkRoundTrip(t, q)
		})
	}
}

func TestRejects(t *testing.T) {
	for _, r := range rejects {
		t.Run(r.why, func(t *testing.T) {
			q, err := Parse(r.query)
			if err == nil {
				t.Fatalf("Parse(%q) accepted (%s) as %q", r.query, r.why, q.String())
			}
			var pe *ParseError
			var le *Error
			if !errors.As(err, &pe) && !errors.As(err, &le) {
				t.Fatalf("unexpected error type %T", err)
			}
		})
	}
}

// checkFilters verifies that every '{' token of the source corresponds to exactly one located
// filter, in order, with a range that starts at '{' and ends just past the matching '}'.
func checkFilters(t *testing.T, q *Query) {
	t.Helper()
	var opens []Token
	for _, tk := range q.Tokens {
		if tk.Kind == KindLBrace {
			opens = append(opens, tk)
		}
	}
	if len(opens) != len(q.Filters) {
		t.Fatalf("%q: %d '{' tokens but %d filters", q.Source, len(opens), len(q.Filters))
	}
	for i, f := range q.Filters {
		if f.Start != opens[i].Start {
			t.Errorf("%q: filter %d starts at %d, want %d", q.Source, i, f.Start, opens[i].Start)
		}
		if q.Source[f.Start] != '{' || q.Source[f.End-1] != '}' {
			t.Errorf("%q: filter %d range [%d,%d) = %q not braced", q.Source, i, f.Start, f.End, q.Source[f.Start:f.End])
			continue
		}
		toks, err := Lex(q.Source[f.Start:f.End])
		if err != nil || toks[0].Kind != KindLBrace || toks[len(toks)-1].Kind != KindRBrace {
			t.Errorf("%q: filter %d slice does not lex as a braced block", q.Source, i)
			continue
		}
		for _, tk := range toks[1 : len(toks)-1] {
			if tk.Kind == KindLBrace || tk.Kind == KindRBrace {
				t.Errorf("%q: filter %d contains nested brace", q.Source, i)
			}
		}
		if i > 0 && f.Start < q.Filters[i-1].End {
			t.Errorf("%q: filters overlap or are unordered", q.Source)
		}
		in, out := f.InnerRange()
		if in < f.Start+1 || out > f.End-1 || in > out {
			t.Errorf("%q: bad inner range [%d,%d)", q.Source, in, out)
		}
	}
}

func checkRoundTrip(t *testing.T, q *Query) {
	t.Helper()
	s := q.String()
	q2, err := Parse(s)
	if err != nil {
		t.Fatalf("reparse of %q (from %q): %v", s, q.Source, err)
	}
	if !equalQuery(q, q2) {
		t.Fatalf("round trip differs: %q -> %q", q.Source, s)
	}
	if s2 := q2.String(); s2 != s {
		t.Fatalf("String not idempotent: %q vs %q", s, s2)
	}
}

// equalQuery compares two queries structurally, ignoring all offsets and the source text.
func equalQuery(a, b *Query) bool {
	return reflect.DeepEqual(strip(reflect.ValueOf(a.Pipeline)), strip(reflect.ValueOf(b.Pipeline))) &&
		reflect.DeepEqual(strip(reflect.ValueOf(a.With)), strip(reflect.ValueOf(b.With)))
}

// strip converts a value to a position-free form. Offsets are dropped; tokens compare by kind,
// text, scope and value.
func strip(v reflect.Value) any {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return strip(v.Elem())
	case reflect.Struct:
		m := map[string]any{"_type": v.Type().Name()}
		for i := 0; i < v.NumField(); i++ {
			switch name := v.Type().Field(i).Name; name {
			case "Start", "End", "Pos", "OpPos", "Neg":
			default:
				m[name] = strip(v.Field(i))
			}
		}
		return m
	case reflect.Slice:
		out := make([]any, v.Len())
		for i := range out {
			out[i] = strip(v.Index(i))
		}
		return out
	case reflect.String:
		return v.String()
	case reflect.Int:
		return v.Int()
	}
	return nil
}

func TestFilterRanges(t *testing.T) {
	cases := []struct {
		q    string
		want []string
	}{
		{`{ }`, []string{`{ }`}},
		{`{}`, []string{`{}`}},
		{`{span.a="x"}>>{span.a="y"}`, []string{`{span.a="x"}`, `{span.a="y"}`}},
		{`  { span.a = "}" }  &&  ({ } ~ { name = "{" })`, []string{`{ span.a = "}" }`, `{ }`, `{ name = "{" }`}},
		{`{ span.b = 1 } | count() > 1 | { span.c = 2 }`, []string{`{ span.b = 1 }`, `{ span.c = 2 }`}},
		{`{ status = error } | select(span.a) | by(span.b)`, []string{`{ status = error }`}},
		{`{ } | compare({ status = error }, 10)`, []string{`{ }`, `{ status = error }`}},
		{`{ span.a = 1 } | rate() by (span.b) with (exemplars=true)`, []string{`{ span.a = 1 }`}},
		{`({ span.a = 1 } &>> { span.b = 2 }) !~ { span.c = 3 } with (most_recent=true)`, []string{`{ span.a = 1 }`, `{ span.b = 2 }`, `{ span.c = 3 }`}},
	}
	for _, c := range cases {
		q, err := Parse(c.q)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.q, err)
			continue
		}
		var got []string
		for _, f := range q.Filters {
			got = append(got, c.q[f.Start:f.End])
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: filters %q, want %q", c.q, got, c.want)
		}
	}
}

func TestInnerRange(t *testing.T) {
	q, err := Parse(`{   span.a = "x"  }`)
	if err != nil {
		t.Fatal(err)
	}
	s, e := q.Filters[0].InnerRange()
	if got := q.Source[s:e]; got != `span.a = "x"` {
		t.Errorf("inner = %q", got)
	}
	q, err = Parse(`{ }`)
	if err != nil {
		t.Fatal(err)
	}
	s, e = q.Filters[0].InnerRange()
	if s != 1 || e != 2 {
		t.Errorf("empty inner = [%d,%d)", s, e)
	}
}

func TestAttributes(t *testing.T) {
	q, err := Parse(`{ span.a = "x" && duration > 1s } >> { resource."b c" = 1 } | by(span:name) | select(.d, trace:rootName) | count() > 1`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range q.Attributes() {
		got = append(got, a.Text)
	}
	want := []string{`span.a`, `duration`, `resource."b c"`, `span:name`, `.d`, `trace:rootName`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("attributes %q, want %q", got, want)
	}
	q, err = Parse(`{ } | quantile_over_time(span.q, .5) by (resource.r) with (a=1)`)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(q.Attributes()); n != 2 {
		t.Errorf("metrics attributes = %d, want 2", n)
	}
}

func TestLeftToRightAST(t *testing.T) {
	q, err := Parse(`{ span.a = "xy" || span.a = "x" && span.sc = 500 }`)
	if err != nil {
		t.Fatal(err)
	}
	top, ok := q.Filters[0].Expr.(*BinaryExpr)
	if !ok || top.Op != KindAnd {
		t.Fatalf("top node = %#v, want && (equal precedence, left to right)", q.Filters[0].Expr)
	}
	if l, ok := top.Left.(*BinaryExpr); !ok || l.Op != KindOr {
		t.Fatalf("left node = %#v, want ||", top.Left)
	}
}

func TestStructuralByPosition(t *testing.T) {
	q, err := Parse(`{ span.a > 1 } > { span.b < 2 }`)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := q.Pipeline.Stages[0].(*SpansetBinary)
	if !ok || b.Op != KindGt {
		t.Errorf("stage = %#v, want structural >", q.Pipeline.Stages[0])
	}
	if _, err := Parse(`{ span.a > 1 } > { span.b < 2 } !~ { span.c !~ "x" }`); err == nil {
		t.Error("mixed structural operators must be rejected")
	}
}

func FuzzParseRoundTrip(f *testing.F) {
	for _, c := range corpus {
		f.Add(c.query)
	}
	for _, r := range rejects {
		f.Add(r.query)
	}
	f.Fuzz(func(t *testing.T, in string) {
		q, err := Parse(in)
		if err != nil {
			return
		}
		checkFilters(t, q)
		checkRoundTrip(t, q)
	})
}
