package traceql

import (
	"errors"
	"slices"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
)

const nsAttr = "resource.k8s.namespace.name"

func tagQs(t *testing.T, p enforce.Plan) []string {
	t.Helper()
	var out []string
	for _, r := range p.Requests {
		out = append(out, r.Query.Get("q"))
	}
	return out
}

func TestLiteralSet(t *testing.T) {
	cases := []struct {
		re   string
		want []string // nil: not a finite literal set
	}{
		{`(?:ns-a)|(?:ns-b)`, []string{"ns-a", "ns-b"}}, // merged equalities (internal/policy)
		{`ns-(a|c)`, []string{"ns-a", "ns-c"}},
		{`ns-[ab]`, []string{"ns-a", "ns-b"}},
		{`ns\.a`, []string{"ns.a"}},
		{`a?`, []string{"", "a"}},
		{`(?:x)|(?:x)`, []string{"x"}},
		{`ns-.*`, nil},
		{`ns-a+`, nil},
		{`(?i)ns-a`, nil},
		{`^ns-a$`, nil},
		{`[a-z]{3}`, nil}, // 26^3 values
		{`[^a]`, nil},
		{`(`, nil},
	}
	for _, c := range cases {
		got, ok := literalSet(c.re, maxTagRequests)
		if (c.want == nil) != !ok || (ok && !slices.Equal(got, c.want)) {
			t.Errorf("literalSet(%q) = %q, %v; want %q", c.re, got, ok, c.want)
		}
	}
}

func TestTagLookupPlans(t *testing.T) {
	const path = "/api/v2/search/tag/" + nsAttr + "/values"
	cases := []struct {
		name  string
		eff   policy.Effective
		userQ string
		merge enforce.MergeKind
		want  []string
	}{
		{"single equality", eff(nil, term(m(nsAttr, v1.MatchEqual, "ns-a"))), "",
			enforce.MergeNone, []string{`{ resource.k8s.namespace.name = "ns-a" }`}},
		{"merged equalities", eff(nil, term(m(nsAttr, v1.MatchRegexMatch, `(?:ns-a)|(?:ns-b)`))), "",
			enforce.MergeTempoTags, []string{`{ resource.k8s.namespace.name = "ns-a" }`, `{ resource.k8s.namespace.name = "ns-b" }`}},
		{"terms on different attributes", eff(nil, term(m(nsAttr, v1.MatchEqual, "ns-a")), term(m("resource.service.name", v1.MatchEqual, "svc-b"))), "",
			enforce.MergeTempoTags, []string{`{ resource.k8s.namespace.name = "ns-a" }`, `{ resource.service.name = "svc-b" }`}},
		{"cross product", eff(nil, term(m(nsAttr, v1.MatchRegexMatch, "ns-(a|b)"), m("span.team", v1.MatchEqual, "alpha"))), "",
			enforce.MergeTempoTags, []string{
				`{ resource.k8s.namespace.name = "ns-a" && span.team = "alpha" }`,
				`{ resource.k8s.namespace.name = "ns-b" && span.team = "alpha" }`,
			}},
		{"user q kept", eff(nil, term(m(nsAttr, v1.MatchEqual, "ns-a"))), `{ span.http.method = "POST" && status = error }`,
			enforce.MergeNone, []string{`{ span.http.method = "POST" && status = error && resource.k8s.namespace.name = "ns-a" }`}},
		{"user q with regex dropped", eff(nil, term(m(nsAttr, v1.MatchEqual, "ns-a"))), `{ span.http.method =~ "P.*" }`,
			enforce.MergeNone, []string{`{ resource.k8s.namespace.name = "ns-a" }`}},
		{"user q with || dropped", eff(nil, term(m(nsAttr, v1.MatchEqual, "ns-a"))), `{ span.a = "x" || span.b = "y" }`,
			enforce.MergeNone, []string{`{ resource.k8s.namespace.name = "ns-a" }`}},
		{"user q with pipeline dropped", eff(nil, term(m(nsAttr, v1.MatchEqual, "ns-a"))), `{ span.a = "x" } | count() > 1`,
			enforce.MergeNone, []string{`{ resource.k8s.namespace.name = "ns-a" }`}},
		{"unscoped user attribute dropped", eff(nil, term(m(nsAttr, v1.MatchEqual, "ns-a"))), `{ .a = "x" }`,
			enforce.MergeNone, []string{`{ resource.k8s.namespace.name = "ns-a" }`}},
		{"duplicate terms collapse", eff(nil, term(m(nsAttr, v1.MatchEqual, "ns-a")), term(m(nsAttr, v1.MatchRegexMatch, "ns-a"))), "",
			enforce.MergeNone, []string{`{ resource.k8s.namespace.name = "ns-a" }`}},
	}
	for _, c := range cases {
		params := q()
		if c.userQ != "" {
			params = q("q", c.userQ)
		}
		for _, p := range []string{path, "/api/v2/search/tags"} {
			pl, err := plan(t, p, params, c.eff)
			if err != nil {
				t.Fatalf("%s %s: %v", c.name, p, err)
			}
			if got := tagQs(t, pl); pl.Merge != c.merge || !slices.Equal(got, c.want) {
				t.Errorf("%s %s: merge %v q %q; want merge %v q %q", c.name, p, pl.Merge, got, c.merge, c.want)
			}
		}
	}
}

func TestTagLookupRejects(t *testing.T) {
	const path = "/api/v2/search/tag/" + nsAttr + "/values"
	cases := []struct {
		name string
		eff  policy.Effective
		want error
	}{
		{"not equal", eff(nil, term(m(nsAttr, v1.MatchNotEqual, "ns-a"))), enforce.ErrUnsupported},
		{"regex no match", eff(nil, term(m(nsAttr, v1.MatchRegexNoMatch, "ns-a"))), enforce.ErrUnsupported},
		{"open regex", eff(nil, term(m(nsAttr, v1.MatchRegexMatch, "ns-.*"))), enforce.ErrUnsupported},
		{"regex with a negative matcher", eff(nil, term(m(nsAttr, v1.MatchRegexMatch, "ns-(a|c)"), m("span.team", v1.MatchNotEqual, "beta"))), enforce.ErrUnsupported},
		{"too many requests", eff(nil, term(m(nsAttr, v1.MatchRegexMatch, "[a-f]"), m("span.team", v1.MatchRegexMatch, "[a-f]"))), enforce.ErrTooComplex},
	}
	for _, c := range cases {
		if _, err := plan(t, path, q(), c.eff); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	// A matcher-less term makes the policy a no-op: no q, as before.
	pl, err := plan(t, path, q(), eff(nil, term(), term(m(nsAttr, v1.MatchNotEqual, "x"))))
	if err != nil || pl.Requests[0].Query.Has("q") {
		t.Errorf("matcher-less term: %v %v", pl, err)
	}
}
