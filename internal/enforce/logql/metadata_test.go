package logql

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
)

func epFor(tmpl, param, pathParam string) routes.Endpoint {
	return routes.Endpoint{Kind: routes.KindEnforce, Param: param, Template: tmpl, PathParam: pathParam}
}

func planFor(t *testing.T, e routes.Endpoint, params url.Values, eff policy.Effective) (enforce.Plan, error) {
	t.Helper()
	return New().Plan(context.Background(), e, params, eff)
}

var (
	twoTerms = effOf(term(m("ns", v1.MatchEqual, "a")), term(m("ns", v1.MatchEqual, "b")))
	oneTerm  = effOf(term(m("ns", v1.MatchEqual, "a")))
)

func queries(p enforce.Plan, param string) [][]string {
	var out [][]string
	for _, r := range p.Requests {
		out = append(out, r.Query[param])
	}
	return out
}

func TestStringSetEndpoints(t *testing.T) {
	for _, tmpl := range []string{tmplLabels, tmplLabelValues, tmplDetectedLabels, tmplDetectedFields, tmplDetectedValues} {
		e := epFor(tmpl, "query", "")
		// Two terms with a client selector: fan-out, one injected selector per term.
		p, err := planFor(t, e, url.Values{"query": {`{app="x"}`}, "start": {"1"}}, twoTerms)
		if err != nil {
			t.Fatalf("%s: %v", tmpl, err)
		}
		want := [][]string{{`{app="x", ns="a"}`}, {`{app="x", ns="b"}`}}
		if p.Merge != enforce.MergeStringSet || p.Strategy != "fanout" || !reflect.DeepEqual(queries(p, "query"), want) {
			t.Errorf("%s: %+v", tmpl, p)
		}
		if p.Requests[0].Term != 0 || p.Requests[1].Term != 1 || p.Requests[1].Query.Get("start") != "1" {
			t.Errorf("%s: terms/params %+v", tmpl, p.Requests)
		}
		// Omitted query: the policy selector is sent (never a pass-through).
		p, err = planFor(t, e, url.Values{}, twoTerms)
		if err != nil {
			t.Fatalf("%s: %v", tmpl, err)
		}
		want = [][]string{{`{ns="a"}`}, {`{ns="b"}`}}
		if !reflect.DeepEqual(queries(p, "query"), want) {
			t.Errorf("%s omitted: %v", tmpl, queries(p, "query"))
		}
		// Empty query counts as omitted.
		p, _ = planFor(t, e, url.Values{"query": {""}}, oneTerm)
		if !reflect.DeepEqual(queries(p, "query"), [][]string{{`{ns="a"}`}}) {
			t.Errorf("%s empty: %v", tmpl, queries(p, "query"))
		}
	}
}

func TestStringSetSingleTermAndDenied(t *testing.T) {
	e := epFor(tmplLabels, "query", "")
	p, err := planFor(t, e, url.Values{}, oneTerm)
	if err != nil {
		t.Fatal(err)
	}
	if p.Merge != enforce.MergeNone || len(p.Requests) != 1 || p.Requests[0].Term != -1 || p.Strategy != "inject" {
		t.Errorf("single term without denied labels streams: %+v", p)
	}
	eff := oneTerm
	eff.DeniedLabels = []string{"secret"}
	p, _ = planFor(t, e, url.Values{}, eff)
	if p.Merge != enforce.MergeStringSet {
		t.Errorf("denied labels need the merge for response filtering: %+v", p)
	}
}

func TestStringSetUnrestricted(t *testing.T) {
	e := epFor(tmplLabels, "query", "")
	un := policy.Effective{Unrestricted: true}
	p, err := planFor(t, e, url.Values{"start": {"1"}}, un)
	if err != nil {
		t.Fatal(err)
	}
	if p.Requests[0].Query.Has("query") || p.Merge != enforce.MergeNone || p.Strategy != "passthrough" {
		t.Errorf("unrestricted, no query: %+v", p)
	}
	p, _ = planFor(t, e, url.Values{"query": {`{a="b"}`}}, un)
	if p.Requests[0].Query.Get("query") != `{a="b"}` {
		t.Errorf("unrestricted keeps query: %+v", p)
	}
	un.DeniedLabels = []string{"secret"}
	p, _ = planFor(t, e, url.Values{}, un)
	if p.Merge != enforce.MergeStringSet || len(p.Requests) != 1 {
		t.Errorf("unrestricted with denied labels merges: %+v", p)
	}
}

func TestStringSetDeniedAndInvalid(t *testing.T) {
	eff := twoTerms
	eff.DeniedLabels = []string{"secret"}
	cases := []struct {
		name string
		e    routes.Endpoint
		q    url.Values
		want *enforce.Error
	}{
		{"denied in path", epFor(tmplLabelValues, "query", "secret"), url.Values{}, enforce.ErrDeniedLabel},
		{"denied field in path", epFor(tmplDetectedValues, "query", "secret"), url.Values{}, enforce.ErrDeniedLabel},
		{"denied in query", epFor(tmplLabels, "query", ""), url.Values{"query": {`{secret="x"}`}}, enforce.ErrDeniedLabel},
		{"denied in pipeline", epFor(tmplDetectedFields, "query", ""), url.Values{"query": {`{a="b"} | secret="x"`}}, enforce.ErrDeniedLabel},
		{"parse", epFor(tmplLabels, "query", ""), url.Values{"query": {`{a=`}}, enforce.ErrParse},
		{"metric query", epFor(tmplLabels, "query", ""), url.Values{"query": {`rate({a="b"}[1m])`}}, enforce.ErrUnsupported},
	}
	for _, c := range cases {
		if _, err := planFor(t, c.e, c.q, eff); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
	// A non-denied path name is fine.
	if _, err := planFor(t, epFor(tmplLabelValues, "query", "app"), url.Values{}, eff); err != nil {
		t.Errorf("app: %v", err)
	}
}

func TestStringSetTenantOnlyTerm(t *testing.T) {
	// A term whose only matcher is __tenant_id__ cannot form a selector: reject, never send {}.
	eff := effOf(term(m("__tenant_id__", v1.MatchEqual, "t1")))
	if _, err := planFor(t, epFor(tmplLabels, "query", ""), url.Values{}, eff); !errors.Is(err, enforce.ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
	if _, err := planFor(t, epFor(tmplLabels, "query", ""), url.Values{}, policy.Effective{}); !errors.Is(err, enforce.ErrUnsupported) {
		t.Errorf("no terms: err = %v", err)
	}
}

func TestSeriesExpansion(t *testing.T) {
	e := epFor(tmplSeries, "match[]", "")
	// user match[] x terms
	p, err := planFor(t, e, url.Values{"match[]": {`{app="x"}`, `{app="y"}`}, "start": {"1"}}, twoTerms)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`{app="x", ns="a"}`, `{app="y", ns="a"}`, `{app="x", ns="b"}`, `{app="y", ns="b"}`}
	got := p.Requests[0].Query["match[]"]
	if !reflect.DeepEqual(got, want) || len(p.Requests) != 1 || p.Merge != enforce.MergeNone || p.Strategy != "match_expand" {
		t.Errorf("plan = %+v", p)
	}
	if p.Requests[0].Query.Get("start") != "1" {
		t.Error("other params lost")
	}
	// no user match[]: one per term
	p, _ = planFor(t, e, url.Values{}, twoTerms)
	if !reflect.DeepEqual(p.Requests[0].Query["match[]"], []string{`{ns="a"}`, `{ns="b"}`}) {
		t.Errorf("no user match: %v", p.Requests[0].Query["match[]"])
	}
	// single term
	p, _ = planFor(t, e, url.Values{"match[]": {`{app="x"}`}}, oneTerm)
	if !reflect.DeepEqual(p.Requests[0].Query["match[]"], []string{`{app="x", ns="a"}`}) {
		t.Errorf("single: %v", p.Requests[0].Query["match[]"])
	}
}

func TestSeriesUnrestrictedAndErrors(t *testing.T) {
	e := epFor(tmplSeries, "match[]", "")
	p, _ := planFor(t, e, url.Values{}, policy.Effective{Unrestricted: true})
	if p.Requests[0].Query.Has("match[]") || p.Strategy != "passthrough" {
		t.Errorf("unrestricted: %+v", p)
	}
	eff := oneTerm
	eff.DeniedLabels = []string{"secret"}
	if _, err := planFor(t, e, url.Values{"match[]": {`{secret="x"}`}}, eff); !errors.Is(err, enforce.ErrDeniedLabel) {
		t.Errorf("denied: %v", err)
	}
	if _, err := planFor(t, e, url.Values{"match[]": {`{a=`}}, eff); !errors.Is(err, enforce.ErrParse) {
		t.Errorf("parse: %v", err)
	}
	// every match[] is validated, also the second one
	if _, err := planFor(t, e, url.Values{"match[]": {`{a="b"}`, `{secret="x"}`}}, eff); !errors.Is(err, enforce.ErrDeniedLabel) {
		t.Errorf("second: %v", err)
	}
}

func TestMetadataDoesNotMutateParams(t *testing.T) {
	params := url.Values{"match[]": {`{app="x"}`}, "start": {"1"}}
	p, _ := planFor(t, epFor(tmplSeries, "match[]", ""), params, twoTerms)
	p.Requests[0].Query["start"][0] = "9"
	if params.Get("start") != "1" || len(params["match[]"]) != 1 {
		t.Error("params aliased")
	}
}

// The template constants must match the real Loki route table, otherwise an endpoint would silently fall
// through to the query handler.
func TestTemplatesMatchRouteTable(t *testing.T) {
	for path, tmpl := range map[string]string{
		"/loki/api/v1/labels":                    tmplLabels,
		"/loki/api/v1/label/app/values":          tmplLabelValues,
		"/loki/api/v1/detected_labels":           tmplDetectedLabels,
		"/loki/api/v1/detected_fields":           tmplDetectedFields,
		"/loki/api/v1/detected_field/msg/values": tmplDetectedValues,
		"/loki/api/v1/series":                    tmplSeries,
	} {
		ep := routes.Lookup(config.BackendLoki, path, "GET")
		if ep.Kind != routes.KindEnforce || ep.Template != tmpl {
			t.Errorf("%s: %+v, want template %s", path, ep, tmpl)
		}
	}
	if ep := routes.Lookup(config.BackendLoki, "/loki/api/v1/label/app/values", "GET"); ep.PathParam != "app" {
		t.Errorf("PathParam = %q", ep.PathParam)
	}
}
