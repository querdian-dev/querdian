package traceql

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
	pq "github.com/querdian-dev/querdian/pkg/traceql"
)

func m(name string, op v1.MatchOp, v string) policy.Matcher {
	return policy.Matcher{Name: name, Op: op, Value: v}
}

func term(ms ...policy.Matcher) policy.Term { return policy.Term{Matchers: ms} }

func eff(denied []string, ts ...policy.Term) policy.Effective {
	return policy.Effective{Terms: ts, DeniedLabels: denied}
}

var one = eff(nil, term(m("resource.ns", v1.MatchEqual, "a")))

func ep(path string) routes.Endpoint { return routes.Lookup(config.BackendTempo, path, "GET") }

func plan(t *testing.T, path string, params url.Values, e policy.Effective) (enforce.Plan, error) {
	t.Helper()
	return New().Plan(context.Background(), ep(path), params, e)
}

func q(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	return v
}

func TestSearchInject(t *testing.T) {
	p, err := plan(t, "/api/search", q("q", `{ span.a = "x" }`, "limit", "5", "start", "1"), one)
	if err != nil {
		t.Fatal(err)
	}
	if p.Merge != enforce.MergeTempoSearch || p.Strategy != "inject" || len(p.Requests) != 1 || p.Requests[0].Term != -1 {
		t.Errorf("plan = %+v", p)
	}
	got := p.Requests[0].Query
	want := `{ (span.a = "x") && ((resource.ns = "a")) }`
	if got.Get("q") != want || got.Get("limit") != "5" || got.Get("start") != "1" {
		t.Errorf("query = %v, want q=%s", got, want)
	}
}

func TestMultipleTerms(t *testing.T) {
	e := eff(nil,
		term(m("resource.ns", v1.MatchEqual, "a"), m("span.t", v1.MatchRegexMatch, "x|y")),
		term(m("resource.ns", v1.MatchNotEqual, "b"), m(".z", v1.MatchRegexNoMatch, "w")))
	p, err := plan(t, "/api/metrics/query_range", q("q", `{ } | rate()`, "step", "1m"), e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{ (resource.ns = "a" && span.t =~ "x|y") || (resource.ns != "b" && .z !~ "w") } | rate()`
	if got := p.Requests[0].Query.Get("q"); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if p.Merge != enforce.MergeNone || p.Strategy != "inject" {
		t.Errorf("plan = %+v", p)
	}
}

func TestInputNotModified(t *testing.T) {
	in := q("q", `{ }`)
	if _, err := plan(t, "/api/search", in, one); err != nil {
		t.Fatal(err)
	}
	if in.Get("q") != `{ }` {
		t.Errorf("input mutated: %v", in)
	}
}

func TestTagEndpointsAbsentQ(t *testing.T) {
	for _, path := range []string{"/api/v2/search/tags", "/api/v2/search/tag/span.foo/values", "/api/search"} {
		for _, params := range []url.Values{q(), q("q", "")} {
			p, err := plan(t, path, params, one)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if got, want := p.Requests[0].Query.Get("q"), `{ (resource.ns = "a") }`; got != want {
				t.Errorf("%s: q = %q, want %q", path, got, want)
			}
		}
	}
	// Unrestricted: no q is added.
	p, err := plan(t, "/api/v2/search/tags", q("scope", "span"), policy.Effective{Unrestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Strategy != "passthrough" || p.Requests[0].Query.Has("q") || p.Requests[0].Query.Get("scope") != "span" {
		t.Errorf("unrestricted plan = %+v", p)
	}
}

func TestMetricsRequireQ(t *testing.T) {
	for _, path := range []string{"/api/metrics/query", "/api/metrics/query_range"} {
		for _, e := range []policy.Effective{one, {Unrestricted: true}} {
			if _, err := plan(t, path, q("step", "1m"), e); !errors.Is(err, enforce.ErrUnsupported) {
				t.Errorf("%s: err = %v, want ErrUnsupported", path, err)
			}
		}
	}
}

func TestLegacyParams(t *testing.T) {
	for _, params := range []url.Values{
		q("tags", "service.name=x"),
		q("tags", "service.name=x", "q", `{ }`),
		q("minDuration", "1s", "q", `{ }`),
		q("maxDuration", "1s"),
		q("tags", ""),
	} {
		for _, e := range []policy.Effective{one, {Unrestricted: true}} {
			_, err := plan(t, "/api/search", params, e)
			if !errors.Is(err, enforce.ErrUnsupported) || enforce.HTTPStatus(err) != 400 {
				t.Errorf("%v: err = %v, want 400 ErrUnsupported", params, err)
			}
		}
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		params url.Values
		eff    policy.Effective
		want   *enforce.Error
	}{
		{"parse", "/api/search", q("q", `{ span.a = `), one, enforce.ErrParse},
		{"two q", "/api/search", q("q", `{ }`, "q", `{ }`), one, enforce.ErrUnsupported},
		{"unlisted param", "/api/search", q("q", `{ }`, "storeMatch[]", "x"), one, enforce.ErrUnsupported},
		{"trace by id denied by default", "/api/traces/abc", q(), one, enforce.ErrDeniedLabel},
		{"denied route", "/api/overrides", q(), one, enforce.ErrUnsupported},
		{"tenant id matcher", "/api/search", q("q", `{ }`), eff(nil, term(m("resource.ns", v1.MatchEqual, "a"), m(policy.TenantIDLabel, v1.MatchEqual, "t"))), enforce.ErrUnsupported},
		{"tenant bound", "/api/search", q("q", `{ }`), eff(nil, policy.Term{Tenant: "t1", Matchers: one.Terms[0].Matchers}), enforce.ErrUnsupported},
		{"no terms", "/api/search", q("q", `{ }`), policy.Effective{}, enforce.ErrUnsupported},
		{"bad value", "/api/search", q("q", `{ }`), eff(nil, term(m("span.a", v1.MatchEqual, "x\ny"))), enforce.ErrUnsupported},
		{"bad op", "/api/search", q("q", `{ }`), eff(nil, term(m("span.a", v1.MatchOp("Foo"), "x"))), enforce.ErrUnsupported},
	}
	for _, c := range cases {
		_, err := plan(t, c.path, c.params, c.eff)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	// Zero Endpoint is deny.
	if _, err := New().Plan(context.Background(), routes.Endpoint{}, q(), one); !errors.Is(err, enforce.ErrUnsupported) {
		t.Errorf("zero endpoint: %v", err)
	}
}

func TestDeniedAttributes(t *testing.T) {
	denySpan := []string{"span.secret"}
	denyRes := []string{"resource.secret"}
	cases := []struct {
		name   string
		denied []string
		query  string
		deny   bool
	}{
		{"scoped same", denySpan, `{ span.secret = "x" }`, true},
		{"unscoped vs span", denySpan, `{ .secret = "x" }`, true},
		{"unscoped vs resource", denyRes, `{ .secret = "x" }`, true},
		{"other scope", denySpan, `{ resource.secret = "x" }`, false},
		{"other name", denySpan, `{ span.secret2 = "x" }`, false},
		{"prefix name", denySpan, `{ span.secret.sub = "x" }`, false},
		{"quoted", denySpan, `{ span."secret" = "x" }`, true},
		{"quoted dotted", []string{"span.a.b"}, `{ span."a.b" = 1 }`, true},
		{"on right of compare", denySpan, `{ span.a = 1 && span.secret > 2 }`, true},
		{"in second filter", denySpan, `{ } >> { span.secret = "x" }`, true},
		{"select", denySpan, `{ } | select(span.secret)`, true},
		{"by", denySpan, `{ } | by(.secret) | count() > 1`, true},
		{"metrics by", denySpan, `{ } | rate() by (span.secret)`, true},
		{"metrics attr", denySpan, `{ } | quantile_over_time(span.secret, .9)`, true},
		{"compare", denySpan, `{ } | compare({ span.secret = "x" })`, true},
		{"aggregate", denySpan, `{ } | avg(span.secret) > 1`, true},
		{"string literal is not an attribute", denySpan, `{ span.a = "span.secret" }`, false},
		{"event scope is different", denySpan, `{ event.secret = "x" }`, false},
	}
	for _, c := range cases {
		for _, e := range []policy.Effective{eff(c.denied, one.Terms...), {Unrestricted: true, DeniedLabels: c.denied}} {
			_, err := plan(t, "/api/search", q("q", c.query), e)
			if c.deny != errors.Is(err, enforce.ErrDeniedLabel) {
				t.Errorf("%s (unrestricted=%v): err = %v, want denied=%v", c.name, e.Unrestricted, err, c.deny)
			}
			if !c.deny && err != nil {
				t.Errorf("%s: unexpected error %v", c.name, err)
			}
		}
	}
}

func TestDeniedTagValuesName(t *testing.T) {
	e := eff([]string{"span.secret"}, one.Terms...)
	for path, deny := range map[string]bool{
		"/api/v2/search/tag/span.secret/values":     true,
		"/api/v2/search/tag/.secret/values":         true,
		"/api/v2/search/tag/secret/values":          true,
		"/api/v2/search/tag/resource.secret/values": false,
		"/api/v2/search/tag/span.other/values":      false,
	} {
		_, err := plan(t, path, q(), e)
		if deny != errors.Is(err, enforce.ErrDeniedLabel) {
			t.Errorf("%s: err = %v, want denied=%v", path, err, deny)
		}
	}
}

func TestUnrestricted(t *testing.T) {
	p, err := plan(t, "/api/search", q("q", `{ span.a = "x" }`), policy.Effective{Unrestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Strategy != "passthrough" || p.Requests[0].Query.Get("q") != `{ span.a = "x" }` {
		t.Errorf("plan = %+v", p)
	}
	// Parse errors are still reported (fail closed) for unrestricted users.
	if _, err := plan(t, "/api/search", q("q", `{ span.a = `), policy.Effective{Unrestricted: true}); !errors.Is(err, enforce.ErrParse) {
		t.Errorf("err = %v", err)
	}
}

func TestMatcherlessTermIsNoOp(t *testing.T) {
	e := eff(nil, term(m("span.a", v1.MatchEqual, "x")), term())
	p, err := plan(t, "/api/search", q("q", `{ span.b = 1 }`), e)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Requests[0].Query.Get("q"); got != `{ span.b = 1 }` {
		t.Errorf("q = %q", got)
	}
}

// TestScalarOnlyQuery documents the decision for queries without a spanset filter: Tempo accepts
// `count() > 10` as `{ } | count() > 10` (docs/research/traceql.md §4), so the policy filter is
// prepended instead of rejecting the query.
func TestScalarOnlyQuery(t *testing.T) {
	p, err := plan(t, "/api/search", q("q", `count() > 10`), one)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.Requests[0].Query.Get("q"), `{ (resource.ns = "a") } | count() > 10`; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestHintsUntouched(t *testing.T) {
	p, err := plan(t, "/api/search", q("q", `{ } with (most_recent=true)`), one)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.Requests[0].Query.Get("q"), `{ (resource.ns = "a") } with (most_recent=true)`; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// TestOutputsInjectedEverywhere re-parses outputs for several query shapes and checks that every
// filter carries the policy.
func TestOutputsInjectedEverywhere(t *testing.T) {
	e := eff(nil, term(m("resource.ns", v1.MatchEqual, `a"b`)), term(m("span.x-y", v1.MatchRegexMatch, `\d+`)))
	for _, in := range []string{
		`{ }`, `{ span.a = 1 } >> { } >> { span.b = 2 }`, `({ } && { span.z = 1 }) | by(span.a) | count() > 1`,
		`{ span.a = 1 } | select(span.b) | { span.c = 2 }`, `{ } | rate() by (span.a) with (sample=0.1)`,
	} {
		p, err := plan(t, "/api/search", q("q", in), e)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		out, err := pq.Parse(p.Requests[0].Query.Get("q"))
		if err != nil {
			t.Fatalf("%s: output does not parse: %v", in, err)
		}
		src := out.Source
		for _, f := range out.Filters {
			s, en := f.InnerRange()
			if !strings.Contains(src[s:en], `(resource.ns = "a\"b") || (span."x-y" =~ "\\d+")`) {
				t.Errorf("%s: filter %q lacks policy", in, src[s:en])
			}
		}
	}
}
