# Grafana dashboard

This page describes the Grafana overview dashboard of Querdian, and the demo dashboards of the quickstart.

In short:

* `charts/querdian/dashboards/querdian.json` is the overview dashboard (uid `querdian-overview`, tags
  `querdian`, `observability`, ...). It uses only the metrics of Querdian from the management port.
* If `dashboards.enabled=true`, the chart installs the dashboard as a ConfigMap for the Grafana sidecar
  (mode `proxy` only; the default is `false`).
* The links from exemplars to Tempo need a setting on the Prometheus data source.
* The quickstart has four different [demo dashboards](#demo-dashboards-quickstart) for the sample services.

## Rows and panels

| Row | Panels |
|---|---|
| Traffic | request rate, deny ratio, error ratio, unready policies, p95, snapshot generation (stats); request rate by decision; request rate by reason (non-allowed); rate by backend/endpoint |
| Latency | p50/p95/p99 with exemplars; p95 by backend; p95 by decision |
| Upstream | 5xx and transport errors per backend (rate and ratio); responses by code; upstream p50/p95/p99 with exemplars |
| Enforcement | fan-out terms heatmap and quantiles (`querdian_policy_terms`); rewrite duration and rate by language/strategy |
| Auth | auth failures by reason |
| Snapshot and policies | generation per instance; rebuilds by result; time since last rebuild (derived); policies by kind/ready (red for `ready=false`) |

Variables: `ds_prom` and `ds_tempo` (data source pickers), `backend`, `endpoint` and `decision`
(multi-select, default All).

Colours: the decisions use the reserved status colours (allow green, deny orange, error red), and
`ready=false` is red. The other series use the categorical palette of Grafana, with fixed colours for
p50, p95 and p99.

## Import the dashboard

Do one of these procedures:

* In Grafana, select Dashboards, New, Import. Upload `querdian.json` and select the Prometheus data source.
* Use the API:
  `curl -XPOST -H 'Content-Type: application/json' -d "{\"dashboard\": $(cat querdian.json), \"overwrite\": true}" http://grafana/api/dashboards/db`.

The latency quantiles use the classic `_bucket` series, which Querdian always exports. The histograms are
also native histograms. If you scrape with native histograms enabled, you can add
`histogram_quantile(0.95, sum(rate(querdian_request_duration_seconds{...}[$__rate_interval])))` as an
extra query.

## Install with the Helm chart

1. Set `dashboards.enabled=true`. The chart then makes a ConfigMap with the label
   `grafana_dashboard: "1"`. The Grafana dashboards sidecar finds ConfigMaps with this label.
2. If your sidecar uses a different label selector, set `dashboards.labels`.
3. To put the ConfigMap in the namespace of Grafana, set `dashboards.namespace`. The default is the
   release namespace.
4. To set the Grafana folder, add the annotation `grafana_folder` with `dashboards.annotations` (sidecar
   setting `folderAnnotation`).

## Exemplars and Tempo

The latency panels send an extra `exemplar: true` query and show exemplar points. Exemplars have a
`trace_id` label (only for sampled requests). Configure the link to Tempo on the Prometheus data source,
not in the dashboard:

```yaml
datasources:
  - name: Prometheus
    type: prometheus
    jsonData:
      exemplarTraceIdDestinations:
        - name: trace_id
          datasourceUid: tempo   # uid of your Tempo data source
```

Prometheus must run with `--enable-feature=exemplar-storage`. Prometheus gets exemplars only if it
scrapes with the OpenMetrics (or protobuf) format.

## Validation

`go test ./charts/querdian/dashboards/` checks that the JSON is valid, that the panel ids are unique,
that the necessary variables and panels exist, and that each PromQL expression parses with the
Prometheus parser (with the Grafana variables replaced).

## Suggested recording and alert rules

The chart does not install these rules. Change the thresholds and `for` durations for your traffic.
The label `result` of `querdian_snapshot_rebuilds_total` has the values `ok` and `error`.

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
      - alert: QuerdianFanOutNearCap
        expr: |
          histogram_quantile(0.99, sum by (le, backend) (rate(querdian_policy_terms_bucket[15m]))) >= 32
        for: 30m
        labels: {severity: info}
```

## Demo dashboards (quickstart)

The kind quickstart installs four dashboards for the *sample services*, not for Querdian, in the Grafana
folder *Querdian demo*: `service-overview` (also the home dashboard), `logs`, `traces` and `isolation`.
The [quickstart reference](quickstart-details.md#dashboards) tells what each dashboard shows.

* The JSON is in `hack/quickstart/dashboards/`. `up.sh` puts it into the ConfigMap `grafana-dashboards`,
  and a file provider (`grafana-dashboard-provider`) loads it.
* The dashboards use only the three provisioned data sources (`prometheus`, `loki`, `tempo`), all
  through Querdian with OAuth pass-through, and only constructs that Querdian accepts.
* `go test ./hack/quickstart/dashboards/` parses each PromQL expression with the Prometheus parser, each
  LogQL expression with `pkg/logql` and each TraceQL query with `pkg/traceql`. The test replaces the
  Grafana variables before it parses. These are the parsers that Querdian uses for enforcement. The test also checks the panel ids, the
  variables, and that each data source uid is one of the provisioned uids.
* `hack/quickstart/verify.sh` then runs each query of each dashboard as alice, bob and carol through Grafana.
