package routes

import "slices"

// Method sets.
var (
	getOnly = []string{"GET"}
	getPost = []string{"GET", "POST"}
)

func enforce(tmpl, param string, methods []string, params ...string) route {
	return route{ep: Endpoint{
		Kind: KindEnforce, Param: param, Template: tmpl, Methods: methods,
		AllowedParams: sorted(append([]string{param}, params...)),
	}}
}

func safe(tmpl string, methods []string, params ...string) route {
	return route{ep: Endpoint{Kind: KindSafe, Template: tmpl, Methods: methods, AllowedParams: sorted(params)}}
}

func denyAll(tmpls ...string) []route {
	rs := make([]route, len(tmpls))
	for i, t := range tmpls {
		rs[i] = route{ep: Endpoint{Template: t}}
	}
	return rs
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return slices.Compact(s)
}

func join(groups ...[]route) []route {
	var out []route
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// isTraceID accepts 1..32 hex digits (Tempo trace IDs); anything else never reaches traceByID.
func isTraceID(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		hex := c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
		if !hex {
			return false
		}
	}
	return true
}

// thanos are the Thanos Query extras that are forwarded (docs/endpoints.md, open question 3).
// storeMatch[] is deliberately absent: it selects which stores serve a query and could bypass
// store-level separation. partial_response is allowed per PLAN §9; the proxy fails closed on
// partial results independently of it.
var thanos = []string{"dedup", "partial_response", "replicaLabels", "replicaLabels[]", "max_source_resolution"}

func with(base []string, more ...string) []string { return slices.Concat(base, more) }

var prometheusTable = compile(join(
	[]route{
		enforce("/api/v1/query", "query", getPost, with(thanos, "time", "timeout", "limit", "lookback_delta", "stats")...),
		enforce("/api/v1/query_range", "query", getPost, with(thanos, "start", "end", "step", "timeout", "limit", "lookback_delta", "stats")...),
		enforce("/api/v1/query_exemplars", "query", getPost, "start", "end"),
		enforce("/api/v1/series", "match[]", getPost, with(thanos, "start", "end", "limit")...),
		enforce("/api/v1/labels", "match[]", getPost, with(thanos, "start", "end", "limit")...),
		enforce("/api/v1/label/:name/values", "match[]", getOnly, with(thanos, "start", "end", "limit")...),
		safe("/api/v1/format_query", getPost, "query"),
		safe("/api/v1/parse_query", getPost, "query"),
		safe("/api/v1/status/buildinfo", getOnly),
	},
	// TODO(question t0.3 #2): metadata is denied (cannot be restricted by label); revisit after e2e capture.
	denyAll(
		"/api/v1/metadata",
		"/api/v1/targets", "/api/v1/targets/*",
		"/api/v1/scrape_pools", "/api/v1/scrape_pools/*",
		"/api/v1/rules", "/api/v1/alerts", "/api/v1/alertmanagers",
		"/api/v1/search/*",
		"/api/v1/status/*", // every status/* except buildinfo (literal wins)
		"/api/v1/admin/*",
		"/api/v1/write", "/api/v1/read", "/api/v1/otlp/*",
		"/api/v1/notifications", "/api/v1/notifications/*", "/api/v1/features",
		"/openapi.yaml",
		// Thanos-only paths are not documented; default deny (question t0.3 #3).
		"/api/v1/stores",
	),
))

var lokiTable = compile(join(
	[]route{
		// POST (form body) on query/query_range is accepted by Loki 3.5.0 (black-box, docs/endpoints.md Q8).
		enforce("/loki/api/v1/query", "query", getPost, "limit", "time", "direction"),
		enforce("/loki/api/v1/query_range", "query", getPost, "limit", "start", "end", "since", "step", "interval", "direction"),
		enforce("/loki/api/v1/labels", "query", getOnly, "start", "end", "since"),
		enforce("/loki/api/v1/label/:name/values", "query", getOnly, "start", "end", "since"),
		enforce("/loki/api/v1/series", "match[]", getPost, "start", "end", "since"),
		// detected_labels: GET and POST, honours query/start/end/since (black-box, docs/endpoints.md Q1).
		enforce("/loki/api/v1/detected_labels", "query", getPost, "start", "end", "since"),
		enforce("/loki/api/v1/index/stats", "query", getPost, "start", "end", "since"),
		enforce("/loki/api/v1/index/volume", "query", getPost, "start", "end", "since", "limit", "step", "targetLabels", "aggregateBy"),
		enforce("/loki/api/v1/index/volume_range", "query", getPost, "start", "end", "since", "limit", "step", "targetLabels", "aggregateBy"),
		enforce("/loki/api/v1/patterns", "query", getOnly, "start", "end", "since", "step"),
		enforce("/loki/api/v1/detected_fields", "query", getPost, "start", "end", "since", "limit", "step"),
		enforce("/loki/api/v1/detected_field/:name/values", "query", getPost, "start", "end", "since", "limit", "step"),
		func() route {
			r := enforce("/loki/api/v1/tail", "query", getOnly, "delay_for", "limit", "start")
			r.ep.Websocket = true
			return r
		}(),
		safe("/loki/api/v1/format_query", getPost, "query"),
		safe("/loki/api/v1/status/buildinfo", getOnly),
	},
	denyAll(
		"/loki/api/v1/push", "/otlp/*",
		"/loki/api/v1/rules", "/loki/api/v1/rules/*", "/prometheus/*",
		"/loki/api/v1/delete",
		"/api/prom/*",
		"/ready", "/metrics", "/config", "/services", "/log_level", "/flush", "/ingester/*",
		"/ring", "/distributor/*", "/ruler/*", "/compactor/*", "/memberlist",
	),
))

var tempoTable = compile(join(
	[]route{
		// Legacy tags, minDuration and maxDuration are never allowed (PLAN §7.4, question t0.3 #5).
		enforce("/api/search", "q", getOnly, "start", "end", "limit", "spss"),
		// TODO(question t0.3 #6): q is optional here; the enforcer must inject a policy-only filter when omitted.
		enforce("/api/v2/search/tags", "q", getOnly, "start", "end", "scope", "limit"),
		enforce("/api/v2/search/tag/:name/values", "q", getOnly, "start", "end", "limit"),
		enforce("/api/metrics/query_range", "q", getOnly, "start", "end", "since", "step", "exemplars"),
		enforce("/api/metrics/query", "q", getOnly, "start", "end", "since"),
		// TODO(question t0.3 #4): client q (and keep_hierarchy etc.) is not allowed on trace-by-id;
		// only start/end are forwarded.
		traceByID("/api/traces/:id"),
		traceByID("/api/v2/traces/:id"),
		safe("/api/echo", getOnly),
		safe("/api/status/buildinfo", getOnly),
	},
	denyAll(
		"/api/search/tags", "/api/search/tag/:name/values", // v1: no q support
		"/api/v2/traces/diff", "/api/overrides", "/api/mcp", "/api/overrides/*",
		"/status", "/status/*", "/ready", "/metrics", "/memberlist", "/usage_metrics",
		"/querier/api/*", "/live-store/*", "/debug/pprof/*",
	),
))

func traceByID(tmpl string) route {
	r := route{ep: Endpoint{Kind: KindTraceByID, Template: tmpl, Methods: getOnly, AllowedParams: []string{"end", "start"}}}
	r.validate = isTraceID
	return r
}
