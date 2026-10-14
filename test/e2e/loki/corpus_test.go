//go:build e2e

package loki

// lcase is one query of the equivalence corpus; the user sends q verbatim and the expected result is what
// the reference instance (holding only the granted streams) returns for the same text.
type lcase struct {
	q string
	// valuesOnly: label sets legitimately differ (absent_over_time infers labels from the equality
	// matchers of the selector, which the injected matchers extend); compare the values only.
	valuesOnly bool
	// instantOnly: ties between series make the range variant ambiguous (topk).
	instantOnly bool
	// absent: under several terms every term yields its own "absent" series (labels inferred from the
	// term's equality matchers); compare the set of distinct value lists.
	absent bool
	// rejectMulti: documented to be rejected (HTTP 400, unsupported) under a multi-term policy
	// (range aggregations with a by/without clause cannot be or-lifted).
	rejectMulti bool
}

// logCorpus holds log queries (query_range only; Loki rejects log queries on the instant endpoint).
var logCorpus = []lcase{
	{q: `{k8s_namespace_name=~".+"}`},
	{q: `{app="web"}`},
	{q: `{app=~".+"} |= "request"`},
	{q: `{app="web"} != "debug"`},
	{q: `{app=~".+"} |~ "level=(warn|error)"`},
	{q: `{app="web"} | logfmt | level="error"`},
	{q: `{app="api"} | json | status >= 500`},
	{q: `{app=~".+"} | logfmt | duration > 100ms`},
	{q: `{app="web"} | logfmt | line_format "{{.level}} {{.msg}}"`},
	{q: `{app="web"} | logfmt | label_format lvl=level | lvl="info"`},
	{q: `{k8s_namespace_name="ns-b"}`},
	{q: `{k8s_namespace_name="ns-d"}`},
	{q: `{k8s_namespace_name=~"ns-.*", app!="web"}`},
	{q: `{k8s_namespace_name!="ns-a", app=~".+"}`},
	{q: `{pod=~"web-.*"} |= "heartbeat"`},
	{q: `{app=~".+"} |= "heartbeat"`},
	{q: `{app=~".+"} |= "dup-ts"`},
	{q: `{app="web"} | drop level`},
	{q: `{app="web"} | regexp "status=(?P<st>\\d+)" | st="500"`},
	{q: `{app=~".+"} |= "request" or "heartbeat"`},
	{q: `{app=~".+"} != "heartbeat" | json | __error__=""`},
	{q: `{app=~".+"} | json | latency > 0.5`},
	{q: `{app="web"} |= "tenant=t2"`},
	{q: `{service_name=~".+"} | line_format "{{ .app }}-{{ .k8s_namespace_name }}"`},
	{q: `{app=~".+"} | pattern "<_> msg=<m> <_>"`},
	{q: "{app=`web`} |= `request 1`"},
	{q: `{app=~"web|api"} # trailing comment`},
}

// metricCorpus holds metric queries, run as instant and range queries.
var metricCorpus = []lcase{
	{q: `count_over_time({app=~".+"}[1m])`},
	{q: `rate({app="web"}[5m])`},
	{q: `bytes_over_time({app=~".+"}[2m])`},
	{q: `bytes_rate({app="api"}[2m])`},
	{q: `sum(count_over_time({app=~".+"}[5m]))`},
	{q: `sum by (k8s_namespace_name) (count_over_time({app=~".+"}[5m]))`},
	{q: `sum by (app) (rate({k8s_namespace_name=~".+"}[1m]))`},
	{q: `topk(1, sum by (k8s_namespace_name) (count_over_time({app=~".+"}[5m])))`, instantOnly: true},
	{q: `sum by (level) (count_over_time({app="web"} | logfmt [5m]))`},
	{q: `count_over_time({app="web"} | logfmt | level="error" [5m])`},
	{q: `sum(count_over_time({app=~".+"} |= "heartbeat" [5m]))`},
	{q: `count_over_time({app="web"}[5m] offset 1m)`},
	{q: `sum(count_over_time({app=~".+"}[5m] offset 2m))`},
	{q: `avg by (app) (avg_over_time({app="web"} | logfmt | unwrap duration(duration) [5m]))`},
	{q: `sum by (k8s_namespace_name) (sum_over_time({app="web"} | logfmt | unwrap duration(duration) [5m]))`},
	{q: `max by (k8s_namespace_name) (max_over_time({app="api"} | json | unwrap latency [5m]))`},
	{q: `quantile_over_time(0.9, {app="api"} | json | unwrap latency [5m]) by (k8s_namespace_name)`, rejectMulti: true},
	{q: `max_over_time({app="web"} | logfmt | unwrap duration(duration) [5m]) by (k8s_namespace_name)`, rejectMulti: true},
	{q: `count_over_time({app="web"}[1m]) / count_over_time({app="api"}[1m])`},
	{q: `sum(count_over_time({app="web"}[5m])) / sum(count_over_time({app=~".+"}[5m]))`},
	{q: `count_over_time({app=~".+"}[1m]) > 3`},
	{q: `count_over_time({app=~".+"}[1m]) > bool 3`},
	{q: `label_replace(sum by (k8s_namespace_name) (count_over_time({app=~".+"}[5m])), "ns", "$1", "k8s_namespace_name", "ns-(.*)")`},
	{q: `label_replace(count_over_time({app="web"}[5m]), "x", "y", "app", "web")`},
	{q: `vector(1)`},
	{q: `sum(count_over_time({app="web"}[5m])) or vector(0)`},
	{q: `absent_over_time({app="nonexistent"}[5m])`, valuesOnly: true, absent: true},
	{q: `count_over_time({app="web"}[1m]) unless on(k8s_namespace_name) count_over_time({app="api"}[1m])`},
	{q: `sum by (app) (count_over_time({app=~".+"}[5m])) and sum by (app) (rate({app="web"}[5m]))`},
	{q: `sum by (k8s_namespace_name, app) (count_over_time({service_name=~".+"}[3m]))`},
	{q: `sum without (pod, env) (count_over_time({app=~".+"}[3m]))`},
}
