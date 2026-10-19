// Package dashboards only holds the demo dashboards (JSON, provisioned into Grafana by
// hack/quickstart/up.sh) and the test that checks them.
package dashboards

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/querdian-dev/querdian/pkg/logql"
	"github.com/querdian-dev/querdian/pkg/traceql"
)

type ds struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

type target struct {
	RefID      string `json:"refId"`
	Datasource ds     `json:"datasource"`
	Expr       string `json:"expr"`
	Query      string `json:"query"`
	QueryType  string `json:"queryType"`
}

type panel struct {
	ID         int      `json:"id"`
	Type       string   `json:"type"`
	Title      string   `json:"title"`
	Datasource *ds      `json:"datasource"`
	Targets    []target `json:"targets"`
	Panels     []panel  `json:"panels"`
	Options    struct {
		Content string `json:"content"`
	} `json:"options"`
}

type variable struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Datasource *ds    `json:"datasource"`
}

type dashboard struct {
	UID           string   `json:"uid"`
	Title         string   `json:"title"`
	Tags          []string `json:"tags"`
	SchemaVersion int      `json:"schemaVersion"`
	Panels        []panel  `json:"panels"`
	Templating    struct {
		List []variable `json:"list"`
	} `json:"templating"`
}

// substitutions emulate an "All" selection and the built-in interval variables.
var substitutions = strings.NewReplacer(
	"$__rate_interval", "2m", "$__interval", "1m", "$__range", "24h",
	"$namespace", ".+", "$route", ".+", "$level", ".+", "$search", "", "$min_duration", "500ms",
)

var varRef = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)

func flatten(ps []panel) []panel {
	var out []panel
	for _, p := range ps {
		out = append(out, p)
		out = append(out, flatten(p.Panels)...)
	}
	return out
}

// provisionedUIDs reads the data source uids from the Grafana manifest of the quickstart.
func provisionedUIDs(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("../manifests/grafana.yaml")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s+uid: (\S+)\n\s+type: (\S+)`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		out[m[1]] = m[2]
	}
	if len(out) != 3 {
		t.Fatalf("expected 3 provisioned data sources, found %v", out)
	}
	return out
}

func TestDashboards(t *testing.T) {
	uids := provisionedUIDs(t)
	files, err := filepath.Glob("*.json")
	if err != nil || len(files) < 4 {
		t.Fatalf("expected at least 4 dashboards, got %v (%v)", files, err)
	}
	seen := map[string]string{}
	for _, f := range files {
		raw, err := os.ReadFile(f) // #nosec G304 -- file names come from the glob above
		if err != nil {
			t.Fatal(err)
		}
		var d dashboard
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if d.UID == "" || d.Title == "" || d.SchemaVersion < 39 {
			t.Fatalf("%s: bad header (uid=%q title=%q schema=%d)", f, d.UID, d.Title, d.SchemaVersion)
		}
		if other, dup := seen[d.UID]; dup {
			t.Fatalf("%s: uid %s also used by %s", f, d.UID, other)
		}
		seen[d.UID] = f
		if !strings.Contains(strings.Join(d.Tags, ","), "querdian-demo") {
			t.Fatalf("%s: missing tag querdian-demo (the dashboard links use it)", f)
		}
		defined := map[string]bool{}
		for _, v := range d.Templating.List {
			defined[v.Name] = true
			if v.Datasource != nil && uids[v.Datasource.UID] != v.Datasource.Type {
				t.Errorf("%s: variable %s uses unknown data source %+v", f, v.Name, *v.Datasource)
			}
		}
		ids := map[int]bool{}
		queries := 0
		for _, p := range flatten(d.Panels) {
			if ids[p.ID] || p.ID == 0 {
				t.Errorf("%s: panel id %d (%q) is zero or duplicated", f, p.ID, p.Title)
			}
			ids[p.ID] = true
			if p.Type == "text" {
				for _, m := range varRef.FindAllStringSubmatch(p.Options.Content, -1) {
					if !defined[m[1]] && !strings.HasPrefix(m[1], "__") {
						t.Errorf("%s: text panel %q uses undefined variable %s", f, p.Title, m[1])
					}
				}
			}
			for _, tg := range p.Targets {
				queries++
				if uids[tg.Datasource.UID] != tg.Datasource.Type || tg.Datasource.Type == "" {
					t.Errorf("%s: %q target %s uses unknown data source %+v", f, p.Title, tg.RefID, tg.Datasource)
				}
				text := tg.Expr + tg.Query
				for _, m := range varRef.FindAllStringSubmatch(text, -1) {
					if !defined[m[1]] && !strings.HasPrefix(m[1], "__") {
						t.Errorf("%s: %q uses undefined variable %s", f, p.Title, m[1])
					}
				}
				q := substitutions.Replace(text)
				switch tg.Datasource.Type {
				case "prometheus":
					if _, err := parser.NewParser(parser.Options{}).ParseExpr(q); err != nil {
						t.Errorf("%s: %q: PromQL %q: %v", f, p.Title, q, err)
					}
				case "loki":
					if _, err := logql.Parse(q); err != nil {
						t.Errorf("%s: %q: LogQL %q: %v", f, p.Title, q, err)
					}
				case "tempo":
					if q == "$traceid" {
						continue // trace by ID panel
					}
					if _, err := traceql.Parse(q); err != nil {
						t.Errorf("%s: %q: TraceQL %q: %v", f, p.Title, q, err)
					}
				}
			}
		}
		if queries == 0 {
			t.Errorf("%s: no queries", f)
		}
	}
	for _, want := range []string{"demo-service-overview", "demo-logs", "demo-traces", "demo-isolation"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("dashboard %s is missing", want)
		}
	}
}
