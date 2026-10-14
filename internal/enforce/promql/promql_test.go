package promql

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
)

var queryEP = routes.Endpoint{Template: "query", Param: "query"}

func nsTerm() policy.Effective {
	return effWith(policy.Matcher{Name: "namespace", Op: v1.MatchEqual, Value: "team-a"})
}

func effWith(ms ...policy.Matcher) policy.Effective {
	return policy.Effective{Terms: []policy.Term{{Matchers: ms}}}
}

func plan(q string, eff policy.Effective) (enforce.Plan, error) {
	return New().Plan(context.Background(), queryEP, url.Values{"query": {q}}, eff)
}

// checkOutput asserts that out re-parses and that every vector selector carries all matchers ms.
func checkOutput(t *testing.T, out string, ms []policy.Matcher) {
	t.Helper()
	expr, err := promParser.ParseExpr(out)
	if err != nil {
		t.Fatalf("output %q does not re-parse: %v", out, err)
	}
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		vs, ok := node.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		for _, m := range ms {
			found := false
			for _, lm := range vs.LabelMatchers {
				if lm.Name == m.Name && lm.Value == m.Value && lm.Type == matchType(m.Op) {
					found = true
				}
			}
			if !found {
				t.Errorf("output %q: selector %s lacks matcher %s", out, vs, m.Name)
			}
		}
		return nil
	})
}

func matchType(op v1.MatchOp) labels.MatchType {
	switch op {
	case v1.MatchEqual:
		return labels.MatchEqual
	case v1.MatchNotEqual:
		return labels.MatchNotEqual
	case v1.MatchRegexMatch:
		return labels.MatchRegexp
	}
	return labels.MatchNotRegexp
}

func countSelectors(t *testing.T, q string) int {
	t.Helper()
	expr, err := promParser.ParseExpr(q)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		if _, ok := node.(*parser.VectorSelector); ok {
			n++
		}
		return nil
	})
	return n
}

func TestInjectTable(t *testing.T) {
	// want is optional (exact expected output); selectors is the expected number of selectors.
	tests := []struct {
		name, query, want string
		selectors         int
	}{
		{"vector", `up`, `up{namespace="team-a"}`, 1},
		{"vector with matcher", `up{job="x"}`, `up{job="x",namespace="team-a"}`, 1},
		{"vector no name", `{job="x"}`, `{job="x",namespace="team-a"}`, 1},
		{"vector regex", `up{job=~"a.*"}`, `up{job=~"a.*",namespace="team-a"}`, 1},
		{"vector neg regex", `up{job!~"a.*"}`, "", 1},
		{"vector not equal", `up{job!="a"}`, "", 1},
		{"empty braces", `up{}`, `up{namespace="team-a"}`, 1},
		{"user namespace override", `up{namespace="team-b"}`, `up{namespace="team-a",namespace="team-b"}`, 1},
		{"user namespace regex", `up{namespace=~".+"}`, "", 1},
		{"matrix", `up[5m]`, `up{namespace="team-a"}[5m]`, 1},
		{"rate", `rate(http_requests_total[5m])`, `rate(http_requests_total{namespace="team-a"}[5m])`, 1},
		{"irate", `irate(http_requests_total[1m])`, "", 1},
		{"increase", `increase(x[1h])`, "", 1},
		{"avg_over_time", `avg_over_time(x[1h])`, "", 1},
		{"quantile_over_time", `quantile_over_time(0.9, x[1h])`, "", 1},
		{"absent", `absent(up{job="x"})`, "", 1},
		{"absent_over_time", `absent_over_time(up[5m])`, "", 1},
		{"subquery", `max_over_time(rate(x[1m])[10m:1m])`, "", 1},
		{"subquery no step", `max_over_time(x[10m:])`, "", 1},
		{"subquery of aggregation", `max_over_time(sum(rate(x[1m]))[10m:30s])`, "", 1},
		{"subquery offset", `max_over_time(x[10m:1m] offset 5m)`, "", 1},
		{"nested subquery", `max_over_time(min_over_time(x[5m:1m])[1h:5m])`, "", 1},
		{"offset", `up offset 5m`, `up{namespace="team-a"} offset 5m`, 1},
		{"negative offset", `up offset -5m`, "", 1},
		{"matrix offset", `rate(x[5m] offset 1h)`, "", 1},
		{"at timestamp", `up @ 1609746000`, "", 1},
		{"at timestamp offset", `up @ 1609746000 offset 1m`, "", 1},
		{"at matrix", `rate(x[5m] @ 1609746000)`, "", 1},
		{"at start", `up @ start()`, `up{namespace="team-a"} @ start()`, 1},
		{"at end", `rate(x[5m] @ end())`, "", 1},
		{"at start offset", `x @ start() offset 1m`, "", 1},
		{"unary minus", `-up`, "", 1},
		{"unary plus", `+up`, "", 1},
		{"parens", `(up)`, `(up{namespace="team-a"})`, 1},
		{"nested parens", `((up))`, "", 1},
		{"number literal", `1`, `1`, 0},
		{"number arithmetic", `1 + 2 * 3`, "", 0},
		{"string args call", `label_replace(up, "a", "b", "c", "d")`, "", 1},
		{"scalar function", `scalar(up)`, "", 1},
		{"vector function", `vector(1)`, "", 0},
		{"time", `time()`, "", 0},
		{"vector scalar binop", `up * 2`, "", 1},
		{"scalar vector binop", `2 * up`, "", 1},
		{"vector vector binop", `a + b`, `a{namespace="team-a"} + b{namespace="team-a"}`, 2},
		{"comparison bool", `a > bool b`, "", 2},
		{"comparison filter", `a > 5`, "", 1},
		{"and", `a and b`, "", 2},
		{"or", `a or b`, "", 2},
		{"unless", `a unless b`, "", 2},
		{"on", `a / on(job) b`, "", 2},
		{"ignoring", `a / ignoring(instance) b`, "", 2},
		{"group_left", `a * on(job) group_left(team) b`, "", 2},
		{"group_right", `a * on(job) group_right b`, "", 2},
		{"group_left ignoring", `a * ignoring(x) group_left b`, "", 2},
		{"on empty", `a / on() b`, "", 2},
		{"sum", `sum(up)`, `sum(up{namespace="team-a"})`, 1},
		{"sum by", `sum by (job) (up)`, "", 1},
		{"sum without", `sum without (instance) (up)`, "", 1},
		{"sum by trailing", `sum(up) by (job)`, "", 1},
		{"count", `count(up)`, "", 1},
		{"topk", `topk(3, rate(x[5m]))`, "", 1},
		{"bottomk by", `bottomk by (job) (2, x)`, "", 1},
		{"quantile agg", `quantile(0.5, x)`, "", 1},
		{"count_values", `count_values("v", x)`, "", 1},
		{"group", `group(x)`, "", 1},
		{"stddev", `stddev by (job) (x)`, "", 1},
		{"nested aggregation", `sum by (a) (max by (a, b) (rate(x[5m])))`, "", 1},
		{"histogram_quantile", `histogram_quantile(0.99, sum by (le) (rate(h_bucket[5m])))`, "", 1},
		{"label_replace", `label_replace(up, "dst", "$1", "src", "(.*)")`, "", 1},
		{"label_join", `label_join(up, "dst", ",", "a", "b")`, "", 1},
		{"deep nesting", `sum(rate(a[5m])) / sum(rate(b[5m])) + on() max_over_time(c[1h:1m] offset 1m) - (d @ end())`, "", 4},
		{"nested selectors in args", `clamp_max(a, scalar(b))`, "", 2},
		{"selector in scalar and subquery", `scalar(sum(a)) * max_over_time(b[1h:])`, "", 2},
		{"metric name keywords", `sum{job="x"}`, "", 1},
		{"name via matcher", `{__name__="up"}`, "", 1},
		{"name regex", `{__name__=~"up|down"}`, "", 1},
		{"time funcs", `day_of_week(timestamp(up))`, "", 1},
		{"sort", `sort_desc(up)`, "", 1},
		{"predict_linear", `predict_linear(x[1h], 3600)`, "", 1},
		{"deriv", `deriv(x[5m])`, "", 1},
		{"query with comment", "up # comment", "", 1},
		{"query with whitespace", "  up  ", "", 1},
	}
	if len(tests) < 60 {
		t.Fatalf("only %d cases", len(tests))
	}
	term := nsTerm().Terms[0].Matchers
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := plan(tt.query, nsTerm())
			if err != nil {
				t.Fatal(err)
			}
			if p.Strategy != "inject" || p.Merge != enforce.MergeNone || len(p.Requests) != 1 || p.Requests[0].Term != -1 {
				t.Fatalf("bad plan %+v", p)
			}
			out := p.Requests[0].Query.Get("query")
			if tt.want != "" && out != tt.want {
				t.Errorf("got  %s\nwant %s", out, tt.want)
			}
			checkOutput(t, out, term)
			if got := countSelectors(t, out); got != tt.selectors {
				t.Errorf("selectors: got %d want %d (%s)", got, tt.selectors, out)
			}
		})
	}
}

func TestAllOperators(t *testing.T) {
	ms := []policy.Matcher{
		{Name: "ns", Op: v1.MatchEqual, Value: "a"},
		{Name: "env", Op: v1.MatchNotEqual, Value: "dev"},
		{Name: "app", Op: v1.MatchRegexMatch, Value: "web|api"},
		{Name: "pod", Op: v1.MatchRegexNoMatch, Value: "canary-.*"},
		{Name: policy.TenantIDLabel, Op: v1.MatchEqual, Value: "t1"},
	}
	p, err := plan(`sum by (job) (rate(up{job="x"}[5m] offset 1m))`, effWith(ms...))
	if err != nil {
		t.Fatal(err)
	}
	out := p.Requests[0].Query.Get("query")
	checkOutput(t, out, ms)
	want := `sum by (job) (rate(up{__tenant_id__="t1",app=~"web|api",env!="dev",job="x",ns="a",pod!~"canary-.*"}[5m] offset 1m))`
	if out != want {
		t.Errorf("got %s want %s", out, want)
	}
}

func TestNameMatcherInPolicy(t *testing.T) {
	for _, q := range []string{`up`, `up{job="x"}`, `{job="x"}`, `rate(up[5m])`} {
		ms := []policy.Matcher{{Name: "__name__", Op: v1.MatchRegexMatch, Value: "up|down"}}
		p, err := plan(q, effWith(ms...))
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		checkOutput(t, p.Requests[0].Query.Get("query"), ms)
	}
}

func TestUnrestrictedAndEmptyTerm(t *testing.T) {
	for name, eff := range map[string]policy.Effective{
		"unrestricted": {Unrestricted: true},
		"empty term":   {Terms: []policy.Term{{Tenant: "t"}}},
	} {
		q := `sum  by(job)(up)`
		p, err := plan(q, eff)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := p.Requests[0].Query.Get("query"); got != q {
			t.Errorf("%s: query modified: %q", name, got)
		}
	}
}

func TestDeniedWithoutInjection(t *testing.T) {
	eff := policy.Effective{Unrestricted: true, DeniedLabels: []string{"secret"}}
	if _, err := plan(`up{secret="x"}`, eff); !errors.Is(err, enforce.ErrDeniedLabel) {
		t.Errorf("got %v", err)
	}
	if _, err := plan(`up{ok="x"}`, eff); err != nil {
		t.Error(err)
	}
}

func TestDenied(t *testing.T) {
	denied := []struct{ name, query string }{
		{"matcher eq", `up{secret="x"}`},
		{"matcher neq", `up{secret!="x"}`},
		{"matcher regex", `up{secret=~".+"}`},
		{"matcher no name", `{secret="x", job="y"}`},
		{"matcher in matrix", `rate(up{secret="x"}[5m])`},
		{"matcher in subquery", `max_over_time(up{secret="x"}[5m:1m])`},
		{"matcher in rhs", `a + b{secret="x"}`},
		{"matcher offset", `up{secret="x"} offset 1m`},
		{"by", `sum by (secret) (up)`},
		{"by trailing", `sum(up) by (job, secret)`},
		{"without", `sum without (secret) (up)`},
		{"topk by", `topk by (secret) (1, up)`},
		{"on", `a / on(secret) b`},
		{"ignoring", `a / ignoring(secret) b`},
		{"group_left", `a * on(job) group_left(secret) b`},
		{"group_right", `a * on(job) group_right(secret) b`},
		{"label_replace dst", `label_replace(up, "secret", "x", "job", ".*")`},
		{"label_replace src", `label_replace(up, "dst", "$1", "secret", "(.*)")`},
		{"label_join dst", `label_join(up, "secret", ",", "a")`},
		{"label_join src", `label_join(up, "dst", ",", "a", "secret")`},
		{"label_replace parens", `label_replace(up, ("secret"), "x", "job", ".*")`},
		{"count_values", `count_values("secret", up)`},
		{"nested", `sum(rate(label_replace(up, "dst", "$1", "secret", "(.*)")[5m:]))`},
		{"deep", `sum(a) / on() group_left(secret) sum(b)`},
	}
	eff := nsTerm()
	eff.DeniedLabels = []string{"secret"}
	for _, tt := range denied {
		t.Run(tt.name, func(t *testing.T) {
			_, err := plan(tt.query, eff)
			if !errors.Is(err, enforce.ErrDeniedLabel) {
				t.Fatalf("got %v, want ErrDeniedLabel", err)
			}
		})
	}
	// Non-denied lookalikes pass.
	for _, q := range []string{`up{secrets="x"}`, `sum by (job) (up)`, `label_replace(up, "dst", "secret", "job", "(.*)")`, `up{job="secret"}`} {
		if _, err := plan(q, eff); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, q := range []string{``, `up{`, `sum by (`, `up +`, `{}`, `{a=~".*"}`, `rate(up)`, `1 +`, `up[5m]]`, `"unterminated`} {
		if _, err := plan(q, nsTerm()); !errors.Is(err, enforce.ErrParse) {
			t.Errorf("%q: got %v", q, err)
		}
	}
}

func TestMissingOrDuplicateQuery(t *testing.T) {
	for _, params := range []url.Values{{}, {"query": {"up", "down"}}, {"time": {"1"}}} {
		if _, err := New().Plan(context.Background(), queryEP, params, nsTerm()); !errors.Is(err, enforce.ErrParse) {
			t.Errorf("%v: got %v", params, err)
		}
	}
}

func TestMultiTermUnsupported(t *testing.T) {
	eff := policy.Effective{Terms: []policy.Term{
		{Matchers: []policy.Matcher{{Name: "a", Op: v1.MatchEqual, Value: "1"}}},
		{Matchers: []policy.Matcher{{Name: "a", Op: v1.MatchEqual, Value: "2"}}},
	}}
	if _, err := plan(`up`, eff); !errors.Is(err, enforce.ErrUnsupported) {
		t.Errorf("got %v", err)
	}
	if _, err := plan(`up`, policy.Effective{}); !errors.Is(err, enforce.ErrUnsupported) {
		t.Errorf("no terms: got %v", err)
	}
}

func TestWrongEndpointParam(t *testing.T) {
	_, err := New().Plan(context.Background(), routes.Endpoint{Param: "match[]"}, url.Values{"query": {"up"}}, nsTerm())
	if !errors.Is(err, enforce.ErrUnsupported) {
		t.Errorf("got %v", err)
	}
}

func TestParamsCopied(t *testing.T) {
	in := url.Values{"query": {"up"}, "start": {"1"}, "end": {"2"}, "step": {"15"}, "dedup": {"true"}}
	p, err := New().Plan(context.Background(), queryEP, in, nsTerm())
	if err != nil {
		t.Fatal(err)
	}
	q := p.Requests[0].Query
	for _, k := range []string{"start", "end", "step", "dedup"} {
		if q.Get(k) != in.Get(k) {
			t.Errorf("param %s changed", k)
		}
	}
	if in.Get("query") != "up" {
		t.Error("input mutated")
	}
	q.Set("start", "9")
	if in.Get("start") != "1" {
		t.Error("output aliases input")
	}
	if !strings.Contains(q.Get("query"), `namespace="team-a"`) {
		t.Error("not injected")
	}
}
