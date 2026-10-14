package logql

import (
	"context"
	"errors"
	"math/rand"
	"net/url"
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
	lq "github.com/querdian-dev/querdian/pkg/logql"
)

var ep = routes.Endpoint{Kind: routes.KindEnforce, Param: "query", Template: "/loki/api/v1/query_range"}

func m(name string, op v1.MatchOp, val string) policy.Matcher {
	return policy.Matcher{Name: name, Op: op, Value: val}
}

func term(ms ...policy.Matcher) policy.Term { return policy.Term{Matchers: ms} }

func effOf(terms ...policy.Term) policy.Effective { return policy.Effective{Terms: terms} }

func plan(t *testing.T, query string, eff policy.Effective) (enforce.Plan, error) {
	t.Helper()
	return New().Plan(context.Background(), ep, url.Values{"query": {query}, "limit": {"10"}, "direction": {"backward"}}, eff)
}

func TestSingleTerm(t *testing.T) {
	eff := effOf(term(m("ns", v1.MatchEqual, "a"), m("env", v1.MatchRegexNoMatch, "dev|test")))
	p, err := plan(t, `sum(rate({app="x"} |= "e" [5m]))`, eff)
	if err != nil {
		t.Fatal(err)
	}
	want := `sum(rate({app="x", ns="a", env!~"dev|test"} |= "e"[5m]))`
	if len(p.Requests) != 1 || p.Requests[0].Query.Get("query") != want || p.Requests[0].Term != -1 {
		t.Errorf("got %+v", p.Requests)
	}
	if p.Strategy != "inject" || p.Merge != enforce.MergeNone {
		t.Errorf("strategy %q merge %v", p.Strategy, p.Merge)
	}
	if p.Requests[0].Query.Get("limit") != "10" {
		t.Error("other params lost")
	}
}

func TestParamsNotAliased(t *testing.T) {
	params := url.Values{"query": {`{app="x"}`}, "limit": {"10"}}
	p, err := New().Plan(context.Background(), ep, params, effOf(term(m("ns", v1.MatchEqual, "a")), term(m("ns", v1.MatchEqual, "b"))))
	if err != nil {
		t.Fatal(err)
	}
	p.Requests[0].Query["limit"][0] = "99"
	if params.Get("limit") != "10" || p.Requests[1].Query.Get("limit") != "10" || params.Get("query") != `{app="x"}` {
		t.Error("params aliased")
	}
}

func TestLogQueryFanout(t *testing.T) {
	eff := effOf(term(m("ns", v1.MatchEqual, "a")), term(m("ns", v1.MatchEqual, "b")))
	p, err := plan(t, `{app="x"} |= "e"`, eff)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Requests) != 2 || p.Merge != enforce.MergeLokiStreams || p.Strategy != "fanout" {
		t.Fatalf("plan %+v", p)
	}
	for i, r := range p.Requests {
		if r.Term != i {
			t.Errorf("term index %d = %d", i, r.Term)
		}
		if r.Query.Get("limit") != "10" || r.Query.Get("direction") != "backward" {
			t.Errorf("branch %d lost params: %v", i, r.Query)
		}
	}
	if got := p.Requests[0].Query.Get("query"); got != `{app="x", ns="a"} |= "e"` {
		t.Errorf("branch 0: %s", got)
	}
	if got := p.Requests[1].Query.Get("query"); got != `{app="x", ns="b"} |= "e"` {
		t.Errorf("branch 1: %s", got)
	}
}

func TestMetricOrLift(t *testing.T) {
	eff := effOf(term(m("ns", v1.MatchEqual, "a")), term(m("ns", v1.MatchEqual, "b")))
	p, err := plan(t, `sum by (app) (rate({app="x"}[5m]))`, eff)
	if err != nil {
		t.Fatal(err)
	}
	want := `sum by (app) (rate({app="x", ns="a"}[5m]) or rate({app="x", ns="b"}[5m]))`
	if len(p.Requests) != 1 || p.Strategy != "or_lift" || p.Merge != enforce.MergeNone || p.Requests[0].Query.Get("query") != want {
		t.Errorf("plan %+v", p)
	}
	// Grouped range aggregation cannot be lifted exactly.
	_, err = plan(t, `sum_over_time({app="x"} | unwrap n [1m]) by (a)`, eff)
	if !errors.Is(err, enforce.ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
	// Nothing to lift.
	p, err = plan(t, `vector(1)`, eff)
	if err != nil || p.Requests[0].Query.Get("query") != "vector(1)" {
		t.Errorf("vector: %+v %v", p, err)
	}
}

func TestErrors(t *testing.T) {
	eff := effOf(term(m("ns", v1.MatchEqual, "a")))
	if _, err := New().Plan(context.Background(), ep, url.Values{}, eff); !errors.Is(err, enforce.ErrParse) {
		t.Errorf("missing: %v", err)
	}
	for _, q := range []string{"", "{", `{app="x"} |`, "rate(", `{}`, `{app=~".*"}`} {
		if _, err := plan(t, q, eff); !errors.Is(err, enforce.ErrParse) {
			t.Errorf("%q: %v", q, err)
		}
	}
	if _, err := plan(t, `{app="x"}`, policy.Effective{}); !errors.Is(err, enforce.ErrUnsupported) {
		t.Errorf("no terms: %v", err)
	}
	if _, err := plan(t, `{app="x"}`, effOf(term(m("bad name", v1.MatchEqual, "a")))); !errors.Is(err, enforce.ErrUnsupported) {
		t.Errorf("bad policy label: %v", err)
	}
}

func TestDeniedLabels(t *testing.T) {
	den := []string{"secret"}
	restricted := policy.Effective{Terms: []policy.Term{term(m("ns", v1.MatchEqual, "a"))}, DeniedLabels: den}
	unrestricted := policy.Effective{Unrestricted: true, DeniedLabels: den}
	bad := []string{
		`{secret="x"}`,
		`{app="x"} | secret="y"`,
		`sum by (secret) (rate({app="x"}[1m]))`,
		`sum_over_time({app="x"} | unwrap secret [1m])`,
		`rate({app="x"}[1m]) / on(secret) rate({app="y"}[1m])`,
		`{app="x"} | line_format "{{.secret}}"`,
		// label_replace overwriting or removing (dst) or copying (src) a denied label.
		`label_replace(rate({app="x"}[1m]), "secret", "", "", "")`,
		`label_replace(rate({app="x"}[1m]), "d", "$1", "secret", "(.*)")`,
	}
	for _, q := range bad {
		for _, eff := range []policy.Effective{restricted, unrestricted} {
			if _, err := plan(t, q, eff); !errors.Is(err, enforce.ErrDeniedLabel) {
				t.Errorf("%q: err = %v, want ErrDeniedLabel", q, err)
			}
		}
	}
	// Unrestricted with denied labels: scan only, no injection; canonical text is forwarded.
	p, err := plan(t, `{app="x"}   # c`, unrestricted)
	if err != nil || p.Strategy != "passthrough" || p.Requests[0].Query.Get("query") != `{app="x"}` {
		t.Errorf("unrestricted: %+v %v", p, err)
	}
	// Without denied labels the original text is kept.
	p, err = plan(t, `{app="x"}   # c`, policy.Effective{Unrestricted: true})
	if err != nil || p.Requests[0].Query.Get("query") != `{app="x"}   # c` {
		t.Errorf("unrestricted plain: %+v %v", p, err)
	}
}

// TestLabelReplacePolicyLabel: label_replace may overwrite or remove the policy label (here ns), so
// the injected selector must sit inside the call and the policy applies before the label is rewritten.
func TestLabelReplacePolicyLabel(t *testing.T) {
	eff := effOf(term(m("ns", v1.MatchEqual, "a")), term(m("ns", v1.MatchEqual, "b")))
	for _, q := range []string{
		`label_replace(rate({app="x"}[1m]), "ns", "other", "ns", ".*")`,
		`label_replace(rate({app="x"}[1m]), "ns", "", "", "")`,
		`label_replace(label_replace(rate({app="x"}[1m]), "ns", "z", "", ""), "a", "$1", "ns", "(.*)")`,
	} {
		p, err := plan(t, q, eff)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		out := p.Requests[0].Query.Get("query")
		rq, err := lq.Parse(out)
		if err != nil {
			t.Fatalf("%q does not parse: %v", out, err)
		}
		if len(rq.Selectors) != 2 {
			t.Errorf("%q: %d selectors", out, len(rq.Selectors))
		}
		for _, s := range rq.Selectors {
			if len(s.Matchers) != 2 || s.Matchers[1].Name != "ns" {
				t.Errorf("selector %s lacks the policy matcher", s)
			}
		}
		// Every selector lies inside a label_replace call, before the label is rewritten.
		if !strings.HasPrefix(out, "label_replace(") || !strings.HasSuffix(out, ")") {
			t.Errorf("selectors not inside label_replace: %s", out)
		}
	}
	// With ns denied the rewrite attempts are rejected outright.
	denied := eff
	denied.DeniedLabels = []string{"ns"}
	if _, err := plan(t, `label_replace(rate({app="x"}[1m]), "ns", "other", "app", ".*")`, denied); !errors.Is(err, enforce.ErrDeniedLabel) {
		t.Errorf("dst denied: %v", err)
	}
}

func TestTenantIDTerms(t *testing.T) {
	tid := func(v string) policy.Matcher { return m(policy.TenantIDLabel, v1.MatchEqual, v) }
	// A term consisting only of __tenant_id__ is combined with the user's anchor matcher.
	eff := effOf(term(tid("t1")), term(tid("t2"), m("ns", v1.MatchEqual, "b")))
	p, err := plan(t, `{app="x"}`, eff)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Requests[0].Query.Get("query"); got != `{app="x", __tenant_id__="t1"}` {
		t.Errorf("branch 0: %s", got)
	}
	if got := p.Requests[1].Query.Get("query"); got != `{app="x", __tenant_id__="t2", ns="b"}` {
		t.Errorf("branch 1: %s", got)
	}
	// A user selector that is itself only __tenant_id__ would be rejected by Loki; refuse it.
	_, err = plan(t, `{__tenant_id__="t2"}`, effOf(term(tid("t1"))))
	if !errors.Is(err, enforce.ErrUnsupported) {
		t.Errorf("tenant-only selector: %v", err)
	}
	// Every injected selector keeps a non-__tenant_id__ non-empty matcher.
	p, err = plan(t, `sum(rate({app=~"a.+"}[1m]) / rate({job="j"}[1m]))`, eff)
	if err != nil {
		t.Fatal(err)
	}
	rq, err := lq.Parse(p.Requests[0].Query.Get("query"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rq.Selectors) != 4 {
		t.Fatalf("%d selectors", len(rq.Selectors))
	}
	for _, s := range rq.Selectors {
		if s.Matchers[0].Name == policy.TenantIDLabel || s.Matchers[0].Value == "" {
			t.Errorf("selector %s lost its anchor", s)
		}
	}
}

func TestZeroMatcherTerm(t *testing.T) {
	p, err := plan(t, `{app="x"}`, effOf(term()))
	if err != nil || p.Requests[0].Query.Get("query") != `{app="x"}` || p.Strategy != "inject" {
		t.Errorf("%+v %v", p, err)
	}
}

// TestPropertyPlans: random queries x random 1-4 terms. The output re-parses; with several terms
// every metric selector carries exactly one term and every fan-out branch carries exactly one term.
func TestPropertyPlans(t *testing.T) {
	queries := []string{
		`{app="x"}`, `{app="x"} |= "e" | json | level="err"`,
		`rate({app="x"}[5m])`, `sum by (a) (rate({app="x"} |= "e" [5m]))`,
		`rate({app="x"}[5m]) / rate({app="y"}[5m] offset 1h)`,
		`label_replace(rate({app="x"}[5m]), "a", "$1", "b", "(.*)")`,
		`sum_over_time({app="x"} | logfmt | unwrap n [1m])`,
		`topk(3, sum by (app) (count_over_time({app=~"a.+"}[1h])))`,
		`rate({a="1"}[1m]) and on(a) rate({a="2"}[1m]) or vector(1)`,
		`quantile_over_time(0.9, {app="x"} | json | unwrap d [5m]) `,
	}
	names := []string{"ns", "svc", policy.TenantIDLabel}
	ops := []v1.MatchOp{v1.MatchEqual, v1.MatchNotEqual, v1.MatchRegexMatch, v1.MatchRegexNoMatch}
	r := rand.New(rand.NewSource(3)) //nolint:gosec // deterministic test randomness
	for iter := 0; iter < 400; iter++ {
		q := queries[r.Intn(len(queries))]
		terms := make([]policy.Term, 1+r.Intn(4))
		for i := range terms {
			var ms []policy.Matcher
			for j := r.Intn(3); j > 0; j-- {
				ms = append(ms, m(names[r.Intn(len(names))], ops[r.Intn(len(ops))], []string{"a", "b|c", "x\"y", `\.`}[r.Intn(4)]))
			}
			terms[i] = term(ms...)
		}
		orig, _ := lq.Parse(q)
		p, err := plan(t, q, effOf(terms...))
		if err != nil {
			if errors.Is(err, enforce.ErrUnsupported) && len(terms) > 1 && strings.Contains(q, "unwrap") {
				continue
			}
			t.Fatalf("%q x %d terms: %v", q, len(terms), err)
		}
		if len(terms) > 1 && orig.Kind == lq.KindLog {
			if len(p.Requests) != len(terms) || p.Merge != enforce.MergeLokiStreams {
				t.Fatalf("fan-out plan %+v", p)
			}
		} else if len(p.Requests) != 1 {
			t.Fatalf("expected one request: %+v", p)
		}
		total := 0
		for bi, req := range p.Requests {
			rq, err := lq.Parse(req.Query.Get("query"))
			if err != nil {
				t.Fatalf("%q: output %q does not parse: %v", q, req.Query.Get("query"), err)
			}
			if len(p.Requests) > 1 && req.Term != bi {
				t.Fatalf("term index %d != %d", req.Term, bi)
			}
			total += len(rq.Selectors)
			for _, s := range rq.Selectors {
				ok := false
				for _, tm := range terms {
					ok = ok || carriesTerm(s, tm)
				}
				if !ok {
					t.Fatalf("%q: selector %s carries no term", req.Query.Get("query"), s)
				}
			}
		}
		wantSel := len(orig.Selectors)
		if len(terms) > 1 && orig.Kind == lq.KindMetric {
			wantSel = len(orig.RangeAggs) * len(terms)
		} else if len(terms) > 1 {
			wantSel = len(orig.Selectors) * len(terms)
		}
		if total != wantSel {
			t.Fatalf("%q: %d selectors in total, want %d", q, total, wantSel)
		}
	}
}

func carriesTerm(s *lq.Selector, tm policy.Term) bool {
	ops := map[v1.MatchOp]string{v1.MatchEqual: "=", v1.MatchNotEqual: "!=", v1.MatchRegexMatch: "=~", v1.MatchRegexNoMatch: "!~"}
	for _, pm := range tm.Matchers {
		found := false
		for _, sm := range s.Matchers {
			if sm.Name == pm.Name && sm.Op == ops[pm.Op] && sm.Value == pm.Value {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
