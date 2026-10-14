package routes

import (
	"slices"
	"strings"
	"testing"

	"github.com/querdian-dev/querdian/internal/config"
)

const (
	prom  = config.BackendPrometheus
	loki  = config.BackendLoki
	tempo = config.BackendTempo
)

type row struct {
	bt        config.BackendType
	path      string
	method    string
	kind      Kind
	param     string
	template  string
	pathParam string
	ws        bool
}

func enf(bt config.BackendType, path, method, param, tmpl string) row {
	return row{bt: bt, path: path, method: method, kind: KindEnforce, param: param, template: tmpl}
}

func rowKind(bt config.BackendType, path, method string, k Kind, tmpl string) row {
	return row{bt: bt, path: path, method: method, kind: k, template: tmpl}
}

func denied(bt config.BackendType, path, method string) row {
	return row{bt: bt, path: path, method: method, kind: KindDeny}
}

// TestEveryEndpointRow covers every row of docs/endpoints.md (all methods listed there, plus
// methods that must be refused).
func TestEveryEndpointRow(t *testing.T) {
	var rows []row

	// 1. Prometheus / Thanos.
	for _, p := range []string{"query", "query_range"} {
		for _, m := range []string{"GET", "POST"} {
			rows = append(rows, enf(prom, "/api/v1/"+p, m, "query", "/api/v1/"+p))
		}
		rows = append(rows, denied(prom, "/api/v1/"+p, "PUT"), denied(prom, "/api/v1/"+p, "DELETE"), denied(prom, "/api/v1/"+p, "get"))
	}
	for _, m := range []string{"GET", "POST"} {
		rows = append(rows,
			enf(prom, "/api/v1/query_exemplars", m, "query", "/api/v1/query_exemplars"),
			enf(prom, "/api/v1/series", m, "match[]", "/api/v1/series"),
			enf(prom, "/api/v1/labels", m, "match[]", "/api/v1/labels"),
			rowKind(prom, "/api/v1/format_query", m, KindSafe, "/api/v1/format_query"),
			rowKind(prom, "/api/v1/parse_query", m, KindSafe, "/api/v1/parse_query"),
		)
	}
	rows = append(rows,
		row{bt: prom, path: "/api/v1/label/job/values", method: "GET", kind: KindEnforce, param: "match[]", template: "/api/v1/label/:name/values", pathParam: "job"},
		row{bt: prom, path: "/api/v1/label/__name__/values", method: "GET", kind: KindEnforce, param: "match[]", template: "/api/v1/label/:name/values", pathParam: "__name__"},
		denied(prom, "/api/v1/label/job/values", "POST"), // GET only
		rowKind(prom, "/api/v1/status/buildinfo", "GET", KindSafe, "/api/v1/status/buildinfo"),
		denied(prom, "/api/v1/status/buildinfo", "POST"),
		denied(prom, "/api/v1/label//values", "GET"),
		denied(prom, "/api/v1/label/values", "GET"),
	)
	for _, p := range []string{
		"metadata", "targets", "targets/metadata", "targets/relabel_steps", "scrape_pools", "scrape_pools/config",
		"rules", "alerts", "alertmanagers", "search/metric_names", "search/label_names", "search/label_values",
		"status/config", "status/flags", "status/runtimeinfo", "status/tsdb", "status/tsdb/blocks", "status/walreplay",
		"status/self_metrics", "admin/tsdb/snapshot", "admin/tsdb/delete_series", "admin/tsdb/clean_tombstones",
		"write", "read", "otlp/v1/metrics", "notifications", "notifications/live", "features", "stores", "unknown",
	} {
		for _, m := range []string{"GET", "POST", "PUT"} {
			rows = append(rows, denied(prom, "/api/v1/"+p, m))
		}
	}
	rows = append(rows, denied(prom, "/openapi.yaml", "GET"), denied(prom, "/", "GET"), denied(prom, "/api/v1", "GET"))

	// 2. Loki.
	for _, p := range []string{"query", "query_range"} {
		// TODO(question t0.3 #8): POST denied until verified black-box.
		rows = append(rows, enf(loki, "/loki/api/v1/"+p, "GET", "query", "/loki/api/v1/"+p), denied(loki, "/loki/api/v1/"+p, "POST"))
	}
	rows = append(rows,
		enf(loki, "/loki/api/v1/labels", "GET", "query", "/loki/api/v1/labels"),
		denied(loki, "/loki/api/v1/labels", "POST"),
		row{bt: loki, path: "/loki/api/v1/label/app/values", method: "GET", kind: KindEnforce, param: "query", template: "/loki/api/v1/label/:name/values", pathParam: "app"},
		denied(loki, "/loki/api/v1/label/app/values", "POST"),
		enf(loki, "/loki/api/v1/patterns", "GET", "query", "/loki/api/v1/patterns"),
		row{bt: loki, path: "/loki/api/v1/tail", method: "GET", kind: KindEnforce, param: "query", template: "/loki/api/v1/tail", ws: true},
		denied(loki, "/loki/api/v1/tail", "POST"),
		rowKind(loki, "/loki/api/v1/status/buildinfo", "GET", KindSafe, "/loki/api/v1/status/buildinfo"),
		row{bt: loki, path: "/loki/api/v1/detected_field/level/values", method: "GET", kind: KindEnforce, param: "query", template: "/loki/api/v1/detected_field/:name/values", pathParam: "level"},
		row{bt: loki, path: "/loki/api/v1/detected_field/level/values", method: "POST", kind: KindEnforce, param: "query", template: "/loki/api/v1/detected_field/:name/values", pathParam: "level"},
	)
	for _, m := range []string{"GET", "POST"} {
		rows = append(rows,
			enf(loki, "/loki/api/v1/series", m, "match[]", "/loki/api/v1/series"),
			enf(loki, "/loki/api/v1/index/stats", m, "query", "/loki/api/v1/index/stats"),
			enf(loki, "/loki/api/v1/index/volume", m, "query", "/loki/api/v1/index/volume"),
			enf(loki, "/loki/api/v1/index/volume_range", m, "query", "/loki/api/v1/index/volume_range"),
			enf(loki, "/loki/api/v1/detected_fields", m, "query", "/loki/api/v1/detected_fields"),
			rowKind(loki, "/loki/api/v1/format_query", m, KindSafe, "/loki/api/v1/format_query"),
		)
	}
	for _, p := range []string{
		"/loki/api/v1/detected_labels", // TODO(question t0.3 #1)
		"/loki/api/v1/push", "/otlp/v1/logs", "/loki/api/v1/rules", "/loki/api/v1/rules/ns/group",
		"/prometheus/api/v1/rules", "/prometheus/api/v1/alerts", "/loki/api/v1/delete",
		"/api/prom/push", "/api/prom/tail", "/api/prom/query", "/api/prom/label", "/api/prom/series", "/api/prom/rules",
		"/ready", "/metrics", "/config", "/services", "/log_level", "/ring", "/flush", "/ingester/flush_shutdown",
		"/ingester/shutdown", "/loki/api/v1/nonexistent", "/api/v1/query",
	} {
		for _, m := range []string{"GET", "POST", "PUT", "DELETE"} {
			rows = append(rows, denied(loki, p, m))
		}
	}

	// 3. Tempo.
	for _, p := range []string{"search", "metrics/query_range", "metrics/query", "v2/search/tags"} {
		rows = append(rows, enf(tempo, "/api/"+p, "GET", "q", "/api/"+p), denied(tempo, "/api/"+p, "POST"))
	}
	rows = append(rows,
		row{bt: tempo, path: "/api/v2/search/tag/service.name/values", method: "GET", kind: KindEnforce, param: "q", template: "/api/v2/search/tag/:name/values", pathParam: "service.name"},
		row{bt: tempo, path: "/api/traces/0123456789abcdef0123456789abcdef", method: "GET", kind: KindTraceByID, template: "/api/traces/:id", pathParam: "0123456789abcdef0123456789abcdef"},
		row{bt: tempo, path: "/api/v2/traces/ABCDEF", method: "GET", kind: KindTraceByID, template: "/api/v2/traces/:id", pathParam: "ABCDEF"},
		denied(tempo, "/api/traces/abc", "POST"),
		denied(tempo, "/api/traces/not-hex", "GET"),
		denied(tempo, "/api/traces/00000000000000000000000000000000f", "GET"), // > 32 hex digits
		rowKind(tempo, "/api/echo", "GET", KindSafe, "/api/echo"),
		rowKind(tempo, "/api/status/buildinfo", "GET", KindSafe, "/api/status/buildinfo"),
		denied(tempo, "/api/echo", "POST"),
	)
	for _, p := range []string{
		"/api/search/tags", "/api/search/tag/service.name/values", "/api/v2/traces/diff", "/api/overrides", "/api/mcp",
		"/status", "/status/config", "/ready", "/metrics", "/memberlist", "/usage_metrics", "/querier/api/traces/abc",
		"/live-store/x", "/debug/pprof/heap", "/api/unknown", "/api/v2/traces/diff",
	} {
		for _, m := range []string{"GET", "POST"} {
			rows = append(rows, denied(tempo, p, m))
		}
	}

	for _, r := range rows {
		ep := Lookup(r.bt, r.path, r.method)
		if ep.Kind != r.kind {
			t.Errorf("%s %s %s: kind %v, want %v", r.bt, r.method, r.path, ep.Kind, r.kind)
			continue
		}
		if r.kind == KindDeny {
			if len(ep.Methods) != 0 || len(ep.AllowedParams) != 0 || ep.Param != "" || ep.Websocket {
				t.Errorf("%s %s: deny endpoint carries data: %+v", r.method, r.path, ep)
			}
			continue
		}
		if ep.Param != r.param || ep.Template != r.template || ep.PathParam != r.pathParam || ep.Websocket != r.ws {
			t.Errorf("%s %s %s: got %+v", r.bt, r.method, r.path, ep)
		}
		if !ep.AllowsMethod(r.method) {
			t.Errorf("%s %s: method not in Methods", r.method, r.path)
		}
		if r.kind == KindEnforce && !ep.AllowsParam(r.param) {
			t.Errorf("%s: enforcement param %q missing from allow-list", r.path, r.param)
		}
	}
}

func TestUnknownBackendType(t *testing.T) {
	if ep := Lookup("bogus", "/api/v1/query", "GET"); ep.Kind != KindDeny {
		t.Fatal("unknown backend type must deny")
	}
	if ForType("bogus") != nil {
		t.Fatal("ForType(bogus) != nil")
	}
}

func TestAllowedParams(t *testing.T) {
	q := Lookup(prom, "/api/v1/query", "GET")
	for _, p := range []string{"query", "time", "dedup", "partial_response", "replicaLabels", "max_source_resolution"} {
		if !q.AllowsParam(p) {
			t.Errorf("prometheus query should allow %q", p)
		}
	}
	for _, p := range []string{"storeMatch[]", "storeMatch", "match[]", "foo", ""} {
		if q.AllowsParam(p) {
			t.Errorf("prometheus query must not allow %q", p)
		}
	}
	for _, p := range []string{"/api/v1/series", "/api/v1/labels", "/api/v1/label/x/values", "/api/v1/query_range"} {
		ep := Lookup(prom, p, "GET")
		if ep.Kind != KindEnforce || ep.AllowsParam("storeMatch[]") || !ep.AllowsParam("dedup") {
			t.Errorf("%s: bad allow-list %v", p, ep.AllowedParams)
		}
	}
	if Lookup(prom, "/api/v1/query_exemplars", "GET").AllowsParam("dedup") {
		t.Error("exemplars should not allow thanos params")
	}
	for tmpl, p := range map[string]string{"/api/search": "tags", "/api/traces/abc": "q", "/api/v2/traces/abc": "q"} {
		if Lookup(tempo, tmpl, "GET").AllowsParam(p) {
			t.Errorf("tempo %s must not allow %q", tmpl, p)
		}
	}
	if !Lookup(tempo, "/api/traces/abc", "GET").AllowsParam("start") {
		t.Error("traceByID should allow start")
	}
	if !Lookup(loki, "/loki/api/v1/index/volume", "GET").AllowsParam("targetLabels") {
		t.Error("volume should allow targetLabels")
	}
	// Allow-lists are sorted and deduplicated, and nothing carries storeMatch.
	for _, bt := range []config.BackendType{prom, loki, tempo} {
		for _, r := range ForType(bt).routes {
			if !slices.IsSorted(r.ep.AllowedParams) || slices.Contains(r.ep.AllowedParams, "storeMatch[]") {
				t.Errorf("%s %s: bad allow-list %v", bt, r.ep.Template, r.ep.AllowedParams)
			}
			if r.ep.Kind == KindDeny && (r.ep.Methods != nil || r.ep.AllowedParams != nil) {
				t.Errorf("%s: deny route with data", r.ep.Template)
			}
		}
	}
}

// TestNonCanonicalDenied: none of these may resolve to any route, for any method.
func TestNonCanonicalDenied(t *testing.T) {
	cases := map[config.BackendType][]string{
		prom: {
			"/api/v1/query/", "/api/v1/query//", "//api/v1/query", "/api//v1/query", "/api/v1//query",
			"/api/v1/../v1/query", "/api/v1/./query", "/api/v1/foo/../query", "/api/v1/query/..", "/api/v1/query/.",
			"/api/v1/%2e%2e/v1/query", "/api/v1/query%2f", "/api/v1%2fquery", "/api%2fv1/query", "/api/v1/query%00",
			"/api/v1/query\x00", "/api/v1/query\x00/x", "/api/v1\\query", "/api\\v1/query", "/api/v1/query\\",
			"api/v1/query", "", "/", "/api/v1/query\n", "/api/v1/query\t", "/api/v1/%71uery", "/api/v1/query%252f",
			"/api/v1/label/job/values/", "/api/v1/label/../values", "/api/v1/label/job%2fx/values",
			"/api/v1/label/./values", "/api/v1/label/..%2fvalues", "/api/v1/label/job/values/..",
			"/api/v1/status/buildinfo/", "/api/v1/query\xff", "/openapi.yaml/", "/api/v1/targets/../query",
		},
		loki: {
			"/loki/api/v1/query/", "/loki//api/v1/query", "/loki/api/v1/../v1/query", "/loki/api/v1/tail/",
			"/loki/api/v1/label/x%2fvalues", "/loki/api/v1/label//values", "/loki/api/v1/label/../values",
			"/loki/api/v1/query\x00", "/loki\\api/v1/query", "/loki/api/v1/rules/../query", "/loki/api/v1/push/../query",
			"/loki/api/v1/index/stats/", "/loki/api/v1/labels/",
		},
		tempo: {
			"/api/search/", "/api//search", "/api/search/..", "/api/traces/", "/api/traces//abc", "/api/traces/abc/",
			"/api/traces/abc%2f..", "/api/traces/../search", "/api/v2/traces/diff/", "/api/traces/abc\x00",
			"/api/v2/search/tag//values", "/api/v2/search/tag/../values", "/api\\search", "/api/echo/",
			"/api/traces/./abc", "/api/traces/abc/.",
		},
	}
	methods := []string{"GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS", ""}
	for bt, paths := range cases {
		for _, p := range paths {
			if Canonical(p) {
				t.Errorf("%q reported canonical", p)
			}
			for _, m := range methods {
				if ep := Lookup(bt, p, m); ep.Kind != KindDeny || ep.Template != "" {
					t.Errorf("%s %s %q resolved: %+v", bt, m, p, ep)
				}
			}
		}
	}
}

func TestCanonical(t *testing.T) {
	for _, p := range []string{"/a", "/api/v1/query", "/api/v2/search/tag/service.name/values", "/api/v1/label/ünï/values", "/a..b/c", "/.hidden"} {
		if !Canonical(p) {
			t.Errorf("%q should be canonical", p)
		}
	}
}

// A path that differs from a real route only in a non-canonical way must not match the real route's
// neighbours either: every enforce/safe template must resolve to itself and nothing else.
func TestTemplatesResolveToThemselves(t *testing.T) {
	for _, bt := range []config.BackendType{prom, loki, tempo} {
		for _, r := range ForType(bt).routes {
			if r.ep.Kind == KindDeny || r.wildcard {
				continue
			}
			p := strings.NewReplacer(":name", "x", ":id", "abc").Replace(r.ep.Template)
			ep := Lookup(bt, p, r.ep.Methods[0])
			if ep.Template != r.ep.Template || ep.Kind != r.ep.Kind {
				t.Errorf("%s: %s resolved to %+v", bt, p, ep)
			}
		}
	}
}

func TestKindString(t *testing.T) {
	for k, want := range map[Kind]string{KindDeny: "deny", KindEnforce: "enforce", KindSafe: "safe", KindTraceByID: "traceByID"} {
		if k.String() != want {
			t.Errorf("%d: %s", k, k.String())
		}
	}
	if (Endpoint{}).Kind != KindDeny {
		t.Error("zero Endpoint must deny")
	}
}
