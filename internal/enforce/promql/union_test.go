package promql

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
)

func eq(name, value string) policy.Matcher {
	return policy.Matcher{Name: name, Op: v1.MatchEqual, Value: value}
}

func terms(ts ...[]policy.Matcher) policy.Effective {
	var eff policy.Effective
	for _, ms := range ts {
		eff.Terms = append(eff.Terms, policy.Term{Matchers: ms})
	}
	return eff
}

// two is a two-term policy: namespace=a OR (namespace=b AND env!=dev).
func two() policy.Effective {
	return terms(
		[]policy.Matcher{eq("namespace", "a")},
		[]policy.Matcher{eq("namespace", "b"), {Name: "env", Op: v1.MatchNotEqual, Value: "dev"}},
	)
}

func TestOrLiftTable(t *testing.T) {
	tests := []struct {
		name, query, want string
	}{
		{"instant selector", `up`,
			`(up{namespace="a"} or up{env!="dev",namespace="b"})`},
		{"selector with matcher and offset", `up{job="x"} offset 5m`,
			`(up{job="x",namespace="a"} offset 5m or up{env!="dev",job="x",namespace="b"} offset 5m)`},
		{"at modifier", `up @ 100`,
			`(up{namespace="a"} @ 100.000 or up{env!="dev",namespace="b"} @ 100.000)`},
		{"at start offset", `up @ start() offset 1m`,
			`(up{namespace="a"} @ start() offset 1m or up{env!="dev",namespace="b"} @ start() offset 1m)`},
		{"rate", `rate(x[5m])`,
			`(rate(x{namespace="a"}[5m]) or rate(x{env!="dev",namespace="b"}[5m]))`},
		{"rate offset", `rate(x[5m] offset 1h)`,
			`(rate(x{namespace="a"}[5m] offset 1h) or rate(x{env!="dev",namespace="b"}[5m] offset 1h))`},
		{"quantile_over_time scalar arg", `quantile_over_time(0.9, x[1h])`,
			`(quantile_over_time(0.9, x{namespace="a"}[1h]) or quantile_over_time(0.9, x{env!="dev",namespace="b"}[1h]))`},
		{"predict_linear", `predict_linear(x[1h], 3600)`,
			`(predict_linear(x{namespace="a"}[1h], 3600) or predict_linear(x{env!="dev",namespace="b"}[1h], 3600))`},
		{"nested aggregation", `sum by (job) (rate(x[5m]))`,
			`sum by (job) ((rate(x{namespace="a"}[5m]) or rate(x{env!="dev",namespace="b"}[5m])))`},
		{"aggregation of aggregation", `max(sum by (job) (up))`,
			`max(sum by (job) ((up{namespace="a"} or up{env!="dev",namespace="b"})))`},
		{"topk with param", `topk(3, up)`,
			`topk(3, (up{namespace="a"} or up{env!="dev",namespace="b"}))`},
		{"binary on", `a / on(job) b`,
			`(a{namespace="a"} or a{env!="dev",namespace="b"}) / on (job) (b{namespace="a"} or b{env!="dev",namespace="b"})`},
		{"binary ignoring group_left", `a * ignoring(x) group_left(team) b`,
			`(a{namespace="a"} or a{env!="dev",namespace="b"}) * ignoring (x) group_left (team) (b{namespace="a"} or b{env!="dev",namespace="b"})`},
		{"binary bool", `a > bool b`,
			`(a{namespace="a"} or a{env!="dev",namespace="b"}) > bool (b{namespace="a"} or b{env!="dev",namespace="b"})`},
		{"set operator", `a and b`,
			`(a{namespace="a"} or a{env!="dev",namespace="b"}) and (b{namespace="a"} or b{env!="dev",namespace="b"})`},
		{"scalar rhs", `a * 2`,
			`(a{namespace="a"} or a{env!="dev",namespace="b"}) * 2`},
		{"unary", `-up`, `-(up{namespace="a"} or up{env!="dev",namespace="b"})`},
		{"paren", `(up)`, `((up{namespace="a"} or up{env!="dev",namespace="b"}))`},
		{"subquery", `max_over_time(rate(x[1m])[10m:1m])`,
			`max_over_time((rate(x{namespace="a"}[1m]) or rate(x{env!="dev",namespace="b"}[1m]))[10m:1m])`},
		{"subquery of selector", `max_over_time(x[10m:])`,
			`max_over_time((x{namespace="a"} or x{env!="dev",namespace="b"})[10m:])`},
		{"top-level subquery offset", `x[10m:1m] offset 5m`,
			`(x{namespace="a"} or x{env!="dev",namespace="b"})[10m:1m] offset 5m`},
		{"absent", `absent(up{job="x"})`,
			`absent((up{job="x",namespace="a"} or up{env!="dev",job="x",namespace="b"}))`},
		{"absent_over_time", `absent_over_time(up{job="x"}[5m])`,
			`absent((last_over_time(up{job="x",namespace="a"}[5m]) or last_over_time(up{env!="dev",job="x",namespace="b"}[5m])))`},
		{"absent_over_time subquery", `absent_over_time(up[5m:1m])`,
			`absent_over_time((up{namespace="a"} or up{env!="dev",namespace="b"})[5m:1m])`},
		{"label_replace", `label_replace(up, "a", "$1", "b", "(.*)")`,
			`label_replace((up{namespace="a"} or up{env!="dev",namespace="b"}), "a", "$1", "b", "(.*)")`},
		{"histogram_quantile", `histogram_quantile(0.9, sum by (le) (rate(h_bucket[5m])))`,
			`histogram_quantile(0.9, sum by (le) ((rate(h_bucket{namespace="a"}[5m]) or rate(h_bucket{env!="dev",namespace="b"}[5m]))))`},
		{"scalar function", `scalar(up) + 1`,
			`scalar((up{namespace="a"} or up{env!="dev",namespace="b"})) + 1`},
		{"literal only", `1 + 1`, `1 + 1`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := plan(tt.query, two())
			if err != nil {
				t.Fatal(err)
			}
			if p.Strategy != "or_lift" {
				t.Errorf("strategy %q", p.Strategy)
			}
			out := p.Requests[0].Query.Get("query")
			if out != tt.want {
				t.Errorf("got  %s\nwant %s", out, tt.want)
			}
			if _, err := promParser.ParseExpr(out); err != nil {
				t.Errorf("does not re-parse: %v", err)
			}
		})
	}
}

func TestOrLiftRangeFunctions(t *testing.T) {
	// Every function that takes a range vector must lift to one copy per term.
	n := 0
	for name, f := range parser.Functions {
		if f.Experimental {
			continue
		}
		hasMatrix := slices.Contains(f.ArgTypes, parser.ValueTypeMatrix)
		if !hasMatrix {
			continue
		}
		args := make([]string, len(f.ArgTypes))
		for i, at := range f.ArgTypes {
			switch at {
			case parser.ValueTypeMatrix:
				args[i] = `x[5m]`
			case parser.ValueTypeScalar:
				args[i] = `0.5`
			default:
				args[i] = `x`
			}
		}
		q := name + "(" + strings.Join(args, ", ") + ")"
		n++
		t.Run(name, func(t *testing.T) {
			p, err := plan(q, two())
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			checkLifted(t, q, p.Requests[0].Query.Get("query"), two())
		})
	}
	if n < 20 {
		t.Errorf("only %d range-vector functions found", n)
	}
}

func TestOrLiftZeroMatcherTerm(t *testing.T) {
	// A term without matchers is unrestricted: the other terms are redundant, query unchanged.
	eff := terms([]policy.Matcher{eq("namespace", "a")}, nil)
	q := `sum by(job)  (rate(x[5m]))`
	p, err := plan(q, eff)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Requests[0].Query.Get("query"); got != q {
		t.Errorf("query modified: %q", got)
	}
	if p.Strategy != "passthrough" {
		t.Errorf("strategy %q", p.Strategy)
	}
	// A term that only carries the tenant binding is a normal restriction.
	eff = terms([]policy.Matcher{eq(policy.TenantIDLabel, "t1")},
		[]policy.Matcher{eq("namespace", "a"), eq(policy.TenantIDLabel, "t2")})
	p, err = plan(`up`, eff)
	if err != nil {
		t.Fatal(err)
	}
	want := `(up{__tenant_id__="t1"} or up{__tenant_id__="t2",namespace="a"})`
	if got := p.Requests[0].Query.Get("query"); got != want {
		t.Errorf("got %s want %s", got, want)
	}
}

func TestOrLiftNameMatcher(t *testing.T) {
	eff := terms([]policy.Matcher{{Name: "__name__", Op: v1.MatchRegexMatch, Value: "up|down"}}, []policy.Matcher{eq("ns", "a")})
	for _, q := range []string{`up`, `up{job="x"}`, `{job="x"}`, `rate(up[5m])`, `absent(up)`} {
		p, err := plan(q, eff)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		checkLifted(t, q, p.Requests[0].Query.Get("query"), eff)
	}
}

func TestOrLiftUnsupported(t *testing.T) {
	for _, q := range []string{
		`up[5m]`,                               // a range vector result cannot be or-ed
		`(up[5m])`,                             // same, parenthesised
		`quantile_over_time(scalar(y), x[1h])`, // selector next to the range vector
		`predict_linear(x[1h], scalar(y))`,
	} {
		_, err := plan(q, two())
		if !errors.Is(err, enforce.ErrUnsupported) {
			t.Errorf("%s: got %v, want ErrUnsupported", q, err)
		}
	}
	// ...but the same shapes are fine with a single term.
	if _, err := plan(`up[5m]`, nsTerm()); err != nil {
		t.Error(err)
	}
}

func TestOrLiftTooComplex(t *testing.T) {
	q := strings.TrimSuffix(strings.Repeat("up + ", 700), " + ") // 700 selectors x 4 terms
	eff := terms([]policy.Matcher{eq("a", "1")}, []policy.Matcher{eq("a", "2")}, []policy.Matcher{eq("a", "3")}, []policy.Matcher{eq("a", "4")})
	if _, err := plan(q, eff); !errors.Is(err, enforce.ErrTooComplex) {
		t.Errorf("got %v", err)
	}
}

func TestOrLiftDenied(t *testing.T) {
	eff := two()
	eff.DeniedLabels = []string{"secret"}
	if _, err := plan(`sum by (secret) (up)`, eff); !errors.Is(err, enforce.ErrDeniedLabel) {
		t.Errorf("got %v", err)
	}
}

func TestOrLiftExemplarsEndpoint(t *testing.T) {
	// query_exemplars extracts the selectors of the query: the lifted query yields the term union.
	ep := queryEP
	ep.Template = "/api/v1/query_exemplars"
	p, err := New().Plan(t.Context(), ep, map[string][]string{"query": {`rate(x[5m])`}, "start": {"1"}, "end": {"2"}}, two())
	if err != nil {
		t.Fatal(err)
	}
	checkLifted(t, `rate(x[5m])`, p.Requests[0].Query.Get("query"), two())
	if p.Requests[0].Query.Get("start") != "1" {
		t.Error("start not copied")
	}
}

// selectorsOf returns every vector selector of q.
func selectorsOf(t *testing.T, q string) []*parser.VectorSelector {
	t.Helper()
	expr, err := promParser.ParseExpr(q)
	if err != nil {
		t.Fatalf("%q does not parse: %v", q, err)
	}
	var out []*parser.VectorSelector
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		if vs, ok := n.(*parser.VectorSelector); ok {
			out = append(out, vs)
		}
		return nil
	})
	return out
}

// checkLifted asserts the or-lifting invariants: the output re-parses, it has original x terms
// selector copies, and every copy carries exactly the matchers of its original plus the matchers of
// exactly one term. The selectors of the output are in the order original 0 x terms, original 1 x ...
func checkLifted(t *testing.T, orig, out string, eff policy.Effective) {
	t.Helper()
	origSel := selectorsOf(t, orig)
	outSel := selectorsOf(t, out)
	nt := len(eff.Terms)
	if len(outSel) != len(origSel)*nt {
		t.Fatalf("%q -> %q: %d selectors, want %d x %d", orig, out, len(outSel), len(origSel), nt)
	}
	for i, o := range origSel {
		for j, term := range eff.Terms {
			want := matcherStrings(o)
			tm, err := toLabelMatchers(term.Matchers)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range tm {
				want = append(want, m.String())
			}
			slices.Sort(want)
			got := matcherStrings(outSel[i*nt+j])
			if !slices.Equal(got, want) {
				t.Fatalf("%q -> %q: selector %d term %d has matchers %v, want %v", orig, out, i, j, got, want)
			}
		}
	}
}

func matcherStrings(vs *parser.VectorSelector) []string {
	var s []string
	for _, m := range vs.LabelMatchers {
		s = append(s, m.String())
	}
	slices.Sort(s)
	return s
}
