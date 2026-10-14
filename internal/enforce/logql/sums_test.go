package logql

import (
	"errors"
	"net/url"
	"reflect"
	"testing"

	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
)

var sumTemplates = []string{tmplIndexStats, tmplIndexVolume, tmplIndexVolumeRange, tmplPatterns}

func TestSumEndpoints(t *testing.T) {
	for _, tmpl := range sumTemplates {
		e := epFor(tmpl, "query", "")
		p, err := planFor(t, e, url.Values{"query": {`{app="x"}`}, "step": {"60"}}, twoTerms)
		if err != nil {
			t.Fatalf("%s: %v", tmpl, err)
		}
		want := [][]string{{`{app="x", ns="a"}`}, {`{app="x", ns="b"}`}}
		if p.Merge != enforce.MergeSumCounts || p.Strategy != "fanout" || !reflect.DeepEqual(queries(p, "query"), want) {
			t.Errorf("%s: %+v", tmpl, p)
		}
		if p.Requests[1].Term != 1 || p.Requests[0].Query.Get("step") != "60" {
			t.Errorf("%s: %+v", tmpl, p.Requests)
		}
		// Omitted query: policy selector per term, never a pass-through.
		p, err = planFor(t, e, url.Values{}, twoTerms)
		if err != nil || !reflect.DeepEqual(queries(p, "query"), [][]string{{`{ns="a"}`}, {`{ns="b"}`}}) {
			t.Errorf("%s omitted: %v %v", tmpl, queries(p, "query"), err)
		}
		// Single term streams unmerged.
		p, _ = planFor(t, e, url.Values{"query": {`{app="x"}`}}, oneTerm)
		if p.Merge != enforce.MergeNone || len(p.Requests) != 1 {
			t.Errorf("%s single: %+v", tmpl, p)
		}
	}
}

func TestSumDeniedLabels(t *testing.T) {
	eff := twoTerms
	eff.DeniedLabels = []string{"secret"}
	e := epFor(tmplIndexVolume, "query", "")
	for name, q := range map[string]url.Values{
		"targetLabels":       {"targetLabels": {"app,secret"}},
		"targetLabels space": {"targetLabels": {"app, secret"}},
		"query":              {"query": {`{secret="x"}`}},
	} {
		if _, err := planFor(t, e, q, eff); !errors.Is(err, enforce.ErrDeniedLabel) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := planFor(t, e, url.Values{"targetLabels": {"app,env"}}, eff); err != nil {
		t.Errorf("allowed targetLabels: %v", err)
	}
	// Single term and unrestricted grants with denied labels still use the merge (it strips denied labels).
	one := oneTerm
	one.DeniedLabels = []string{"secret"}
	p, _ := planFor(t, e, url.Values{}, one)
	if p.Merge != enforce.MergeSumCounts {
		t.Errorf("single: %+v", p)
	}
	p, _ = planFor(t, e, url.Values{}, policy.Effective{Unrestricted: true, DeniedLabels: []string{"secret"}})
	if p.Merge != enforce.MergeSumCounts || p.Requests[0].Query.Has("query") {
		t.Errorf("unrestricted: %+v", p)
	}
	p, _ = planFor(t, e, url.Values{"query": {`{a="b"}`}}, policy.Effective{Unrestricted: true})
	if p.Merge != enforce.MergeNone || p.Requests[0].Query.Get("query") != `{a="b"}` {
		t.Errorf("unrestricted plain: %+v", p)
	}
}

func TestSumTemplatesMatchRouteTable(t *testing.T) {
	for path, tmpl := range map[string]string{
		"/loki/api/v1/index/stats":        tmplIndexStats,
		"/loki/api/v1/index/volume":       tmplIndexVolume,
		"/loki/api/v1/index/volume_range": tmplIndexVolumeRange,
		"/loki/api/v1/patterns":           tmplPatterns,
	} {
		ep := routes.Lookup(config.BackendLoki, path, "GET")
		if ep.Kind != routes.KindEnforce || ep.Template != tmpl {
			t.Errorf("%s: %+v, want template %s", path, ep, tmpl)
		}
	}
}
