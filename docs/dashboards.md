# Grafana dashboard

This page describes the Querdian overview dashboard shipped with the chart. The quickstart has its
own, separate set of demo dashboards for the services behind Querdian; see
[Demo dashboards](#demo-dashboards-quickstart) at the end.

`charts/querdian/dashboards/querdian.json` is the Querdian overview dashboard (uid `querdian-overview`,
tags `querdian`, `observability`, ...). It uses only the metrics of PLAN section 11.1, scraped from the
management port.

## Rows and panels

| Row | Panels |
|---|---|
| Traffic | request rate, deny ratio, error ratio, unready policies, p95, snapshot generation (stats); request rate by decision; request rate by reason (non-allowed); rate by backend/endpoint |
| Latency | p50/p95/p99 with exemplars; p95 by backend; p95 by decision |
| Upstream | 5xx and transport errors per backend (rate and ratio); responses by code; upstream p50/p95/p99 with exemplars |
| Enforcement | fan-out terms heatmap and quantiles (`querdian_policy_terms`); rewrite duration and rate by language/strategy |
| Auth | auth failures by reason |
| Snapshot and policies | generation per instance; rebuilds by result; time since last rebuild (derived); policies by kind/ready (red for `ready=false`) |

Variables: `ds_prom` and `ds_tempo` (data source pickers), `backend`, `endpoint`, `decision`
(multi-select, default All).

Colour rules: decisions use the reserved status colours (allow green, deny orange, error red) and
`ready=false` is red; other series use Grafana's categorical palette, with fixed colours for
p50/p95/p99.

## Importing

1. Grafana, Dashboards, New, Import, upload `querdian.json`, pick the Prometheus data source.
2. Or via the API: `curl -XPOST -H 'Content-Type: application/json' -d "{\"dashboard\": $(cat querdian.json), \"overwrite\": true}" http://grafana/api/dashboards/db`.

Latency quantiles use the classic `_bucket` series (always exported). The histograms are also native
histograms; if you scrape with native histograms enabled you can add
`histogram_quantile(0.95, sum(rate(querdian_request_duration_seconds{...}[$__rate_interval])))` as an
extra query.

## Exemplars and Tempo

The latency panels issue an extra `exemplar: true` query and show exemplar points. Exemplars carry a
`trace_id` label (only for sampled requests). The link to Tempo is configured on the Prometheus data
source, not in the dashboard:

```yaml
datasources:
  - name: Prometheus
    type: prometheus
    jsonData:
      exemplarTraceIdDestinations:
        - name: trace_id
          datasourceUid: tempo   # uid of your Tempo data source
```

Prometheus must run with `--enable-feature=exemplar-storage`, and exemplars are only exposed when
scraping with the OpenMetrics (or protobuf) format.

## Shipping with the Helm chart

The Grafana sidecar imports ConfigMaps carrying the `grafana_dashboard` label. A chart template could
look like this (optional, not shipped yet; the chart templates are owned by the chart task):

```yaml
{{- if .Values.dashboard.enabled }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "querdian.fullname" . }}-dashboard
  labels:
    {{- include "querdian.labels" . | nindent 4 }}
    grafana_dashboard: "1"
data:
  querdian.json: |-
{{ .Files.Get "dashboards/querdian.json" | indent 4 }}
{{- end }}
```

The sidecar's folder can be set with the annotation `grafana_folder` (sidecar `folderAnnotation`).

## Validation

`go test ./charts/querdian/dashboards/` checks that the JSON is valid, panel ids are unique, the
required variables and panels exist, and that every PromQL expression parses with the Prometheus
parser (Grafana variables substituted).

## Suggested recording and alert rules

Not shipped in the chart. Adjust thresholds and `for` durations to your traffic.

```yaml
groups:
  - name: querdian.rules
    rules:
      - record: querdian:requests:rate5m
        expr: sum by (backend, decision, reason) (rate(querdian_requests_total[5m]))
      - record: querdian:request_duration_seconds:p95_5m
        expr: histogram_quantile(0.95, sum by (le, backend) (rate(querdian_request_duration_seconds_bucket[5m])))
      - record: querdian:upstream_error_ratio:rate5m
        expr: |
          sum by (backend) (rate(querdian_upstream_requests_total{code=~"5..|error"}[5m]))
          / sum by (backend) (rate(querdian_upstream_requests_total[5m]))

      - alert: QuerdianPoliciesNotReady
        expr: sum(querdian_policies{ready="false"}) > 0
        for: 10m
        labels: {severity: warning}
      - alert: QuerdianUpstreamErrors
        expr: querdian:upstream_error_ratio:rate5m > 0.05
        for: 10m
        labels: {severity: critical}
      - alert: QuerdianHighLatencyP95
        expr: querdian:request_duration_seconds:p95_5m > 2
        for: 15m
        labels: {severity: warning}
      - alert: QuerdianInternalErrors
        expr: |
          sum(rate(querdian_requests_total{decision="error"}[5m]))
          / sum(rate(querdian_requests_total[5m])) > 0.01
        for: 10m
        labels: {severity: critical}
      - alert: QuerdianAuthFailureSpike
        expr: sum by (reason) (rate(querdian_auth_failures_total[5m])) > 1
        for: 10m
        labels: {severity: warning}
      - alert: QuerdianSnapshotRebuildFailing
        expr: sum(increase(querdian_snapshot_rebuilds_total{result!="ok"}[15m])) > 0
        for: 15m
        labels: {severity: warning}
      - alert: QuerdianSnapshotDiverged
        expr: max(querdian_snapshot_generation) - min(querdian_snapshot_generation) > 0
        for: 15m
        labels: {severity: warning}
      - alert: QuerdianFanOutNearCap
        expr: |
          histogram_quantile(0.99, sum by (le, backend) (rate(querdian_policy_terms_bucket[15m]))) >= 32
        for: 30m
        labels: {severity: info}
```

Check the actual `result` label values of `querdian_snapshot_rebuilds_total` in your build before
using the `result!="ok"` rule.

## Shipping the dashboard with the Helm chart

Set `dashboards.enabled=true` to render the dashboard as a ConfigMap labelled `grafana_dashboard: "1"`, which
the Grafana dashboards sidecar picks up. Use `dashboards.labels` to match your sidecar's label selector and
`dashboards.namespace` to place the ConfigMap in Grafana's namespace.

## Demo dashboards (quickstart)

The kind quickstart provisions four dashboards of the *sample services* (not of Querdian itself) into
the Grafana folder *Querdian demo*: `service-overview` (also the home dashboard), `logs`, `traces`
and `isolation`. The JSON lives in `hack/quickstart/dashboards/`, `up.sh` packs it into the ConfigMap
`grafana-dashboards` and a file provider (`grafana-dashboard-provider`) loads it. They only use the
three provisioned data sources (`prometheus`, `loki`, `tempo`, all via Querdian with OAuth
pass-through) and only constructs Querdian accepts; what each one shows is described in
[quickstart.md](quickstart.md#dashboards).

Checks: `go test ./hack/quickstart/dashboards/` parses every PromQL expression with the Prometheus
parser, every LogQL expression with `pkg/logql` and every TraceQL query with `pkg/traceql` (the same
parsers Querdian enforces with, with Grafana variables substituted), checks panel ids, variables
and that every data source uid is one of the provisioned ones. `hack/quickstart/verify.sh` then runs
every query of every dashboard as alice, bob and carol through Grafana.
