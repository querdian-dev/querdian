package logql

import (
	"errors"
	"math/rand"
	"strings"
	"testing"
)

func mustParse(t *testing.T, s string) *Query {
	t.Helper()
	q, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return q
}

func eq(name, val string) InjectMatcher { return InjectMatcher{Name: name, Op: "=", Value: val} }

func TestInjectSingle(t *testing.T) {
	ns := []InjectMatcher{eq("ns", "a"), {Name: "svc", Op: "=~", Value: `x|y\.z`}}
	tests := []struct{ in, want string }{
		{`{app="x"}`, `{app="x", ns="a", svc=~"x|y\\.z"}`},
		{`{app="x"} |= "e" | json | level="err"`, `{app="x", ns="a", svc=~"x|y\\.z"} |= "e" | json | level = "err"`},
		{`rate({app="x"}[5m])`, `rate({app="x", ns="a", svc=~"x|y\\.z"}[5m])`},
		{`sum by (a) (rate({app="x"}[5m] offset 1h))`, `sum by (a) (rate({app="x", ns="a", svc=~"x|y\\.z"}[5m] offset 1h))`},
		{`rate({app="x"}[5m]) / rate({app="y"}[5m])`, `rate({app="x", ns="a", svc=~"x|y\\.z"}[5m]) / rate({app="y", ns="a", svc=~"x|y\\.z"}[5m])`},
		{`vector(1)`, `vector(1)`},
		{`label_replace(rate({app="x"}[5m]), "a", "$1", "b", "(.*)")`, `label_replace(rate({app="x", ns="a", svc=~"x|y\\.z"}[5m]), "a", "$1", "b", "(.*)")`},
		{`sum_over_time({app="x"} | unwrap n [1m]) by (a)`, `sum_over_time({app="x", ns="a", svc=~"x|y\\.z"} | unwrap n[1m]) by (a)`},
	}
	for _, tc := range tests {
		got, err := Inject(mustParse(t, tc.in), ns)
		if err != nil {
			t.Errorf("Inject(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Inject(%q)\n got %s\nwant %s", tc.in, got, tc.want)
		}
	}
}

func TestInjectDoesNotMutate(t *testing.T) {
	q := mustParse(t, `rate({app="x"}[5m])`)
	before := q.String()
	if _, err := Inject(q, []InjectMatcher{eq("ns", "a")}); err != nil {
		t.Fatal(err)
	}
	if q.String() != before || len(q.Selectors[0].Matchers) != 1 {
		t.Errorf("query was mutated: %s", q.String())
	}
}

func TestOrLift(t *testing.T) {
	terms := [][]InjectMatcher{{eq("ns", "a")}, {eq("ns", "b")}, {eq("ns", "c")}}
	got, err := OrLift(mustParse(t, `sum(rate({app="x"}[5m])) / count_over_time({app="y"}[1m])`), terms)
	if err != nil {
		t.Fatal(err)
	}
	want := `sum((rate({app="x", ns="a"}[5m]) or rate({app="x", ns="b"}[5m])) or rate({app="x", ns="c"}[5m])) / ` +
		`((count_over_time({app="y", ns="a"}[1m]) or count_over_time({app="y", ns="b"}[1m])) or count_over_time({app="y", ns="c"}[1m]))`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got, err := OrLift(mustParse(t, `vector(1)`), terms); err != nil || got != "vector(1)" {
		t.Errorf("vector: %q %v", got, err)
	}
	// Single term equals Inject.
	a, _ := OrLift(mustParse(t, `rate({app="x"}[5m])`), terms[:1])
	b, _ := Inject(mustParse(t, `rate({app="x"}[5m])`), terms[0])
	if a != b {
		t.Errorf("single term: %s != %s", a, b)
	}
}

func TestOrLiftUnsupported(t *testing.T) {
	terms := [][]InjectMatcher{{eq("ns", "a")}, {eq("ns", "b")}}
	for _, in := range []string{
		`{app="x"}`,
		`sum_over_time({app="x"} | unwrap n [1m]) by (a)`,
	} {
		if _, err := OrLift(mustParse(t, in), terms); !errors.Is(err, ErrLiftUnsupported) {
			t.Errorf("OrLift(%q): err = %v, want ErrLiftUnsupported", in, err)
		}
	}
	// Ungrouped unwrapped functions are fine.
	if _, err := OrLift(mustParse(t, `sum_over_time({app="x"} | unwrap n [1m])`), terms); err != nil {
		t.Errorf("ungrouped unwrap: %v", err)
	}
}

func TestInjectEmptyTerm(t *testing.T) {
	got, err := Inject(mustParse(t, `{app="x"}`), nil)
	if err != nil || got != `{app="x"}` {
		t.Errorf("got %q, %v", got, err)
	}
	got, err = OrLift(mustParse(t, `rate({app="x"}[5m])`), [][]InjectMatcher{nil, {eq("ns", "a")}})
	if err != nil || got != `rate({app="x"}[5m]) or rate({app="x", ns="a"}[5m])` {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := OrLift(mustParse(t, `{app="x"}`), nil); err == nil {
		t.Error("no terms accepted")
	}
}

func TestInjectInvalidMatchers(t *testing.T) {
	q := mustParse(t, `{app="x"}`)
	for _, m := range []InjectMatcher{
		{Name: "a b", Op: "=", Value: "x"},
		{Name: "", Op: "=", Value: "x"},
		{Name: "a", Op: "==", Value: "x"},
		{Name: "a", Op: "=", Value: "\xff"},
		{Name: "a", Op: "=~", Value: "("},
	} {
		if out, err := Inject(q, []InjectMatcher{m}); err == nil {
			t.Errorf("Inject with %+v accepted: %s", m, out)
		}
	}
}

func TestInjectQuotingRoundTrip(t *testing.T) {
	for _, v := range []string{`a"b`, `a\b`, "a\nb\tc\r", "a\x00b\x7f", `日本語`, "`tick`", `\.`, ""} {
		out, err := Inject(mustParse(t, `{app="x"}`), []InjectMatcher{{Name: "l", Op: "!=", Value: v}})
		if err != nil {
			t.Errorf("value %q: %v", v, err)
			continue
		}
		rq := mustParse(t, out)
		m := rq.Selectors[0].Matchers
		if len(m) != 2 || m[1].Value != v {
			t.Errorf("value %q round-tripped to %+v", v, m)
		}
	}
}

// TestInjectTenantIDAnchor covers the Loki quirk that a selector whose only
// matcher is __tenant_id__ is rejected: the user's selector always supplies an
// anchor, and a selector that does not is refused.
func TestInjectTenantIDAnchor(t *testing.T) {
	tenantOnly := []InjectMatcher{eq(TenantIDLabel, "t1")}
	out, err := Inject(mustParse(t, `{app="x"}`), tenantOnly)
	if err != nil {
		t.Fatal(err)
	}
	if out != `{app="x", __tenant_id__="t1"}` {
		t.Errorf("got %s", out)
	}
	rq := mustParse(t, out)
	if !hasAnchor(rq.Selectors[0]) {
		t.Error("no anchor")
	}
	// Combined with other terms.
	terms := [][]InjectMatcher{tenantOnly, {eq(TenantIDLabel, "t2"), eq("ns", "b")}}
	out, err = OrLift(mustParse(t, `rate({app=~".+"}[1m])`), terms)
	if err != nil {
		t.Fatal(err)
	}
	if want := `rate({app=~".+", __tenant_id__="t1"}[1m]) or rate({app=~".+", __tenant_id__="t2", ns="b"}[1m])`; out != want {
		t.Errorf("got %s", out)
	}
	// A user selector that is only __tenant_id__ has no anchor after injection.
	if _, err := Inject(mustParse(t, `{__tenant_id__="t9"}`), tenantOnly); !errors.Is(err, ErrNoAnchor) {
		t.Errorf("err = %v, want ErrNoAnchor", err)
	}
	// A non-empty-matching __tenant_id__ term never replaces the anchor of a
	// selector whose own anchor is a regex.
	if _, err := Inject(mustParse(t, `{app=~"a|b"}`), tenantOnly); err != nil {
		t.Error(err)
	}
}

// randomTerm builds a small term from a deterministic RNG.
func randomTerm(r *rand.Rand) []InjectMatcher {
	names := []string{"ns", "svc", "env", TenantIDLabel, "json"}
	ops := []string{"=", "!=", "=~", "!~"}
	vals := []string{"a", "b|c", ".+", `x"y`, `d\.e`, "", "日"}
	n := r.Intn(3)
	var out []InjectMatcher
	for i := 0; i < n; i++ {
		op := ops[r.Intn(len(ops))]
		v := vals[r.Intn(len(vals))]
		if strings.HasSuffix(op, "~") && v == `x"y` {
			v = "x.y"
		}
		out = append(out, InjectMatcher{Name: names[r.Intn(len(names))], Op: op, Value: v})
	}
	return out
}

func checkProperty(t *testing.T, src string, terms [][]InjectMatcher) {
	t.Helper()
	q, err := Parse(src)
	if err != nil {
		return
	}
	out, err := OrLift(q, terms)
	if len(terms) > 1 && q.Kind == KindLog {
		if !errors.Is(err, ErrLiftUnsupported) {
			t.Fatalf("%q: log query lifted: %v", src, err)
		}
		return
	}
	if err != nil {
		if errors.Is(err, ErrLiftUnsupported) || errors.Is(err, ErrNoAnchor) {
			return
		}
		t.Fatalf("OrLift(%q, %v): %v", src, terms, err)
	}
	rq, err := Parse(out)
	if err != nil {
		t.Fatalf("output %q does not parse: %v", out, err)
	}
	copies := len(terms)
	if len(rq.Selectors) != len(q.Selectors)*copies {
		t.Fatalf("%q: %d selectors, want %d", out, len(rq.Selectors), len(q.Selectors)*copies)
	}
	// Selector i*copies+j is copy j of original selector i; it carries the
	// original matchers plus exactly term j's matchers.
	for i, orig := range q.Selectors {
		for j := 0; j < copies; j++ {
			s := rq.Selectors[i*copies+j]
			if len(s.Matchers) != len(orig.Matchers)+len(terms[j]) {
				t.Fatalf("%q: selector %d copy %d has %d matchers, want %d", out, i, j, len(s.Matchers), len(orig.Matchers)+len(terms[j]))
			}
			for k, m := range orig.Matchers {
				if s.Matchers[k].Name != m.Name || s.Matchers[k].Op != m.Op || s.Matchers[k].Value != m.Value {
					t.Fatalf("%q: original matcher %d altered", out, k)
				}
			}
			for k, m := range terms[j] {
				got := s.Matchers[len(orig.Matchers)+k]
				if got.Name != m.Name || got.Op != m.Op || got.Value != m.Value {
					t.Fatalf("%q: term matcher %d = %+v, want %+v", out, k, got, m)
				}
			}
		}
	}
}

func TestInjectProperty(t *testing.T) {
	r := rand.New(rand.NewSource(7)) //nolint:gosec // deterministic test randomness
	for _, c := range corpus {
		for rep := 0; rep < 8; rep++ {
			n := 1 + r.Intn(4)
			terms := make([][]InjectMatcher, n)
			for i := range terms {
				terms[i] = randomTerm(r)
			}
			checkProperty(t, c.q, terms)
		}
	}
}

func FuzzInject(f *testing.F) {
	for _, c := range corpus {
		f.Add(c.q, "ns", "a", int64(1))
	}
	f.Fuzz(func(t *testing.T, src, name, val string, seed int64) {
		r := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test randomness
		n := 1 + r.Intn(4)
		terms := make([][]InjectMatcher, n)
		for i := range terms {
			terms[i] = randomTerm(r)
		}
		terms[0] = append(terms[0], InjectMatcher{Name: name, Op: "=", Value: val})
		// Invalid names and values must be refused, never panic or pass.
		q, err := Parse(src)
		if err != nil {
			return
		}
		if out, err := OrLift(q, terms); err == nil {
			if _, perr := Parse(out); perr != nil {
				t.Fatalf("output %q does not parse: %v", out, perr)
			}
			checkProperty(t, src, terms)
		}
	})
}

func TestDeniedLabel(t *testing.T) {
	den := []string{"secret"}
	tests := []struct {
		q    string
		want bool
	}{
		{`{app="x"}`, false},
		{`{secret="x"}`, true},
		{`{app="x", secret=~".+"}`, true},
		{`{app="x"} | secret="y"`, true},
		{`{app="x"} | json | secret != ""`, true},
		{`{app="x"} | json secret="a.b"`, true},
		{`{app="x"} |= "secret"`, false},
		{`{app="x"} != "secret" or "other"`, false},
		{`{app="x"} | label_format secret=app`, true},
		{`{app="x"} | line_format "{{.secret}}"`, true},
		{`{app="x"} | label_format a="{{ index . \"secret\" }}"`, true},
		{`{app="x"} | line_format "{{.app}}"`, false},
		{`{app="x"} | line_format "{{ . }}"`, true},
		{`{app="x"} | line_format "{{range $k, $v := .}}{{$k}}{{end}}"`, true},
		{`{app="x"} | line_format "{{ with . }}x{{ end }}"`, true},
		{`{app="x"} | line_format "{{ printf \"%v\" . }}"`, true},
		{`{app="x"} | label_format a="{{ index . .app }}"`, true},
		{`{app="x"} | line_format "{{ .app | upper }} {{ .env }}"`, false},
		{`{app="x"} | line_format "literal . text and { braces }"`, false},
		{`{app="x"} | line_format "{{ \".\" }}"`, false},
		{`{app="x"} | drop secret`, true},
		{`{app="x"} | keep app`, false},
		{`sum by (secret) (rate({app="x"}[1m]))`, true},
		{`sum without (secret) (rate({app="x"}[1m]))`, true},
		{`sum_over_time({app="x"} | unwrap secret [1m])`, true},
		{`rate({app="x"}[1m]) / on(secret) rate({app="y"}[1m])`, true},
		{`rate({app="x"}[1m]) / ignoring(secret) group_left(secret) rate({app="y"}[1m])`, true},
		{`label_replace(rate({app="x"}[1m]), "secret", "", "", "")`, true},
		{`label_replace(rate({app="x"}[1m]), "dst", "$1", "secret", "(.*)")`, true},
		{`label_replace(rate({app="x"}[1m]), "dst", "secret", "app", "(.*)")`, false},
		{`label_replace(rate({app="x"}[1m]), "dst", "$1", "app", "secret")`, false},
		{`rate({app="x"}[1m]) # secret`, false},
	}
	for _, tc := range tests {
		got, found, err := DeniedLabel(mustParse(t, tc.q), den)
		if err != nil {
			t.Errorf("%q: %v", tc.q, err)
		}
		if found != tc.want {
			t.Errorf("DeniedLabel(%q) = %q,%v want %v", tc.q, got, found, tc.want)
		}
	}
	if _, found, _ := DeniedLabel(mustParse(t, `{secret="x"}`), nil); found {
		t.Error("no denied labels must not match")
	}
	// A denied label spelled like a keyword is found as a Keyword token.
	if _, found, _ := DeniedLabel(mustParse(t, `{json="x"}`), []string{"json"}); !found {
		t.Error("keyword-spelled label not found")
	}
}
