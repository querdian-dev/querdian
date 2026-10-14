package dashboards

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"
)

type panel struct {
	ID      int      `json:"id"`
	Type    string   `json:"type"`
	Title   string   `json:"title"`
	Targets []target `json:"targets"`
	Panels  []panel  `json:"panels"`
}

type target struct {
	Expr string `json:"expr"`
}

type dash struct {
	UID           string   `json:"uid"`
	Tags          []string `json:"tags"`
	SchemaVersion int      `json:"schemaVersion"`
	Panels        []panel  `json:"panels"`
	Templating    struct {
		List []struct {
			Name  string `json:"name"`
			Query any    `json:"query"`
		} `json:"list"`
	} `json:"templating"`
}

// grafanaVars are replaced by sample values so the expressions can be parsed.
var grafanaVars = strings.NewReplacer("$__rate_interval", "5m", "$__range", "1h", "$backend", ".*", "$endpoint", ".*", "$decision", ".*")

func walk(ps []panel, f func(panel)) {
	for _, p := range ps {
		f(p)
		walk(p.Panels, f)
	}
}

func TestDashboard(t *testing.T) {
	raw, err := os.ReadFile("querdian.json")
	if err != nil {
		t.Fatal(err)
	}
	var d dash
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if d.UID != "querdian-overview" || len(d.Tags) == 0 || d.SchemaVersion < 39 {
		t.Fatalf("bad header: uid=%q tags=%v schema=%d", d.UID, d.Tags, d.SchemaVersion)
	}
	vars := map[string]bool{}
	for _, v := range d.Templating.List {
		vars[v.Name] = true
	}
	for _, n := range []string{"ds_prom", "ds_tempo", "backend", "endpoint", "decision"} {
		if !vars[n] {
			t.Errorf("missing template variable %s", n)
		}
	}
	ids := map[int]bool{}
	titles := map[string]bool{}
	exprs := 0
	walk(d.Panels, func(p panel) {
		if ids[p.ID] {
			t.Errorf("duplicate panel id %d", p.ID)
		}
		ids[p.ID] = true
		titles[p.Title] = true
		for _, tg := range p.Targets {
			exprs++
			if _, err := parser.NewParser(parser.Options{}).ParseExpr(grafanaVars.Replace(tg.Expr)); err != nil {
				t.Errorf("panel %d %q: %v\n%s", p.ID, p.Title, err, tg.Expr)
			}
		}
	})
	for _, want := range []string{
		"Request rate by decision", "Request rate by reason (non-allowed)", "Request latency p50 / p95 / p99",
		"Upstream errors per backend (5xx and transport errors)", "Fan-out terms distribution",
		"Rewrite duration p95 by language / strategy", "Auth failures by reason", "Snapshot generation",
		"Snapshot rebuilds by result", "Time since last rebuild", "Policies by kind and readiness",
	} {
		if !titles[want] {
			t.Errorf("missing panel %q", want)
		}
	}
	t.Logf("%d panels, %d expressions parsed", len(ids), exprs)
}
