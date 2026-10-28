# Quickstart on kind

A complete isolation demo on a laptop: one script creates a single-node [kind](https://kind.sigs.k8s.io)
cluster with Keycloak, Prometheus, Loki, Tempo, an OpenTelemetry Collector, Grafana, two sample
workloads (`order-service`, `payment-service`) and Querdian with example policies. Two demo users
log in to Grafana with Keycloak and see different data. The backends are pre-filled with **24 hours of
realistic, deterministic history** (metrics, logs and traces, with incidents) and Grafana comes with
**four provisioned dashboards**, so the demo has something to look at the moment it is up.

> **Demo only.** Keycloak runs in dev mode over plain HTTP with well-known passwords, the demo
> OAuth client allows the password grant (used by `verify.sh`), and nothing is persistent. Do not
> reuse any of it in production. Querdian is alpha.

```sh
hack/quickstart/up.sh       # cluster + everything, ends with the URLs
hack/quickstart/verify.sh   # automatic assertions (153 checks)
hack/quickstart/down.sh     # removes the cluster and the built image
```

## Requirements

* `kind` (tested with v0.33), `kubectl`, `helm` 3/4, `make`, `curl`, `jq`, and Docker or Podman
  (with the `docker` CLI shim; the scripts then set `KIND_EXPERIMENTAL_PROVIDER=podman`).
* About **3 GiB of free memory** for the container VM and ~5 GiB of disk for images. Measured on a
  6 GiB Podman VM (5 CPUs): with everything running and the 24 h history loaded the VM reports about
  2.6 GiB used and 3.3 GiB available (it was 2.3 / 3.6 GiB before the history existed; the extra
  0.3 GiB is mostly Tempo, about 360 MiB resident after the dashboards ran, and Loki). Keycloak is the
  largest part (JVM heap capped at 300 MiB, container limit 750 MiB).
* Free host ports `8080` (Keycloak), `3000` (Grafana) and `8081` (Querdian policy dashboard). Change
  Keycloak's with `KEYCLOAK_PORT=8082`.
* Internet access: the images are pulled from Docker Hub and quay.io by the kind node.
* By default `up.sh` builds Querdian from this checkout (`make docker-build`, in the background while
  the cluster starts) and loads it into kind. To run the published v0.1.2 image instead, check out
  the tag, pull the image and skip the build; the chart is still installed from the checkout:

  ```sh
  git switch --detach v0.1.2
  docker pull ghcr.io/querdian-dev/querdian:0.1.2
  SKIP_BUILD=1 IMAGE_TAG=0.1.2 hack/quickstart/up.sh
  ```

  `down.sh` removes that image again unless you set `KEEP_IMAGE=1`. To use the published chart as
  well, see [releasing.md](releasing.md#5-post-release-verification) (`helm upgrade` from
  `oci://ghcr.io/querdian-dev/charts/querdian`).

Options (environment variables): `CLUSTER` (default `querdian-demo`), `KEYCLOAK_PORT`,
`IMAGE_TAG` (default `quickstart`), `SKIP_BUILD=1`, `QUICKSTART_NETWORKPOLICY=1`,
`CLEANUP_ON_FAILURE=1`, `DEMO_HISTORY_HOURS` (default 24); `down.sh` honours `KEEP_IMAGE=1`.

## What gets deployed

| Namespace | Component | Notes |
|---|---|---|
| `keycloak` | Keycloak 26.8.1 (dev mode) | realm `observability` imported from `hack/quickstart/realm-observability.json` |
| `observability` | Prometheus v3.15.1, Loki 3.7.9, Tempo 3.1.1 (monolithic, no Kafka) | single instances; Loki and Tempo multi-tenant, tenant `demo`; pre-filled with 24 h of history (see below) |
| `observability` | Job `demo-backfill`, image `localhost/querdian-demogen` | pushes the log and trace history, then completes |
| `observability` | OpenTelemetry Collector contrib 0.163.2 (DaemonSet) | filelog for container logs, OTLP receiver for traces |
| `observability` | Grafana 13.2.4 | Keycloak login, data sources point at Querdian, four provisioned dashboards |
| `querdian` | Querdian (chart `charts/querdian`, 2 replicas) | `http://querdian.querdian:8080/{prom,loki,tempo}` |
| `order-service`, `payment-service` | busybox 1.37 (awk simulation) | emits metrics, JSON/logfmt logs and multi-span traces; both namespaces carry `policy.querdian.dev/tenant: demo` |

Users (realm `observability`, client `grafana`, secret `grafana-demo-secret`):

| User | Password | Group | Sees |
|---|---|---|---|
| alice | `alice-demo` | `order-team` | only `order-service` |
| bob | `bob-demo` | `payment-team` | only `payment-service` |
| carol | `carol-demo` | `platform-admins` | everything (tenant `demo`), also Querdian's and the stack's own telemetry |

Keycloak admin console: `admin` / `admin`. Grafana local admin: `admin` / `admin`; it has no OAuth
token, so its queries are refused by Querdian (this is the point: use the Keycloak login).

Policies (`hack/quickstart/manifests/policies.yaml`):

* `AccessPolicy order-team` in `order-service` (group `order-team`): metrics, logs, traces; the
  metrics rule also carries `labels.redact: [{name: customer_id, strategy: Placeholder}]` (the
  **redacted-label example**: the label is denied, and its values in results are replaced).
* `AccessPolicy payment-team` in `payment-service` (group `payment-team`): metrics, logs, traces.
* `ClusterAccessPolicy platform-admins` (group `platform-admins`): `unrestricted` on all three
  backends for tenant `demo`.

Backends in the Querdian configuration (`hack/quickstart/querdian-values.yaml`): `metrics`
(`/prom`, `namespaceLabel: namespace`), `logs` (`/loki`, `k8s_namespace_name`, tenant header
`X-Scope-OrgID`), `traces` (`/tempo`, `resource.k8s.namespace.name`, tenant header, `traceByID: filter`).

## Demo data: 24 hours of history

Right after `up.sh` the backends hold about a day of data for the two sample services, and the live
workloads keep adding fresh data at the "now" end (they follow the same model, so the curves join
without a step). Everything is **synthetic and deterministic**: `hack/quickstart/demogen` is a small
Go program (standard library only) driven by a seeded generator; the shape depends on the *age* of a
point (time before now), never on the wall clock, so the same seed always gives the same day.

| Signal | What exists | Volume (24 h) |
|---|---|---|
| Metrics (Prometheus) | `http_requests_total{route,method,status}`, the histogram `http_request_duration_seconds` (`route`, `method`), saturation gauges `demo_worker_pool_utilization`, `demo_db_connections_in_use`/`_max`, `demo_queue_depth`, `process_resident_memory_bytes`, `process_cpu_seconds_total`, and `demo_requests_total{customer_id}` (the redacted-label example). 5 routes per service, a diurnal load wave, resolution 30 s. | 2 x 99 series, 570 000 samples |
| Logs (Loki) | order-service writes **JSON**, payment-service **logfmt**, both with `level`, `method`, `route`, `status`, `duration_ms` and, for errors, slow requests and about 12% of the rest, a `trace_id`. Background lines (pool statistics at debug level, cache refreshes, circuit-breaker and pool-saturation warnings). | about 56 000 lines |
| Traces (Tempo) | multi-span traces: a server span per request with child spans for Redis, PostgreSQL and the downstream call (order-service calls `payment-service /payments`, payment-service calls `acme-pay`). Failing requests have error spans with an `exception` event. The `trace_id` in the logs is the id of these traces. | about 6 900 traces, 21 000 spans |

Incidents (offsets before the start of `up.sh`; the same ones show up in all three signals):

| When | Service | What happens |
|---|---|---|
| 7h30 to 6h30 ago | payment-service | acme-pay slowdown: latency of the provider calls x9, up to 14% 503s on `POST /payments` and `/refund`; high saturation. order-service `POST /orders` and `/cancel` suffer through their payment-service call (x6 latency, up to 10% 503s). |
| 3h40 to 3h10 ago | order-service | bad deploy: database connections exhausted, 18% 500s on every route, database calls x3 slower. |
| 1h to 50 min ago | payment-service | short acme-pay blip (latency x3, 3% 503s). |

Traces and logs are linked both ways: the `TraceID` derived field of the Loki data source opens a
trace from a log line, and *trace to logs* on the Tempo data source runs
`{k8s_namespace_name="<ns>"} |= "<trace id>"`.

### How the backfill works

Each backend takes old data a different way, so `up.sh` uses three:

* **Prometheus** does not accept old samples (no out-of-order ingestion over remote write for a day of
  data). The Prometheus pod has an **init container** that runs `demogen metrics` (an OpenMetrics file,
  about 115 MB) and `promtool tsdb create-blocks-from openmetrics` into the data directory (13 two-hour
  blocks, 4 s, 1.5 MB on disk); Prometheus then starts on top of them and begins scraping. The init
  container uses the image `localhost/querdian-demogen:<tag>`, which `up.sh` builds from the pinned
  Prometheus image plus the generator (in parallel with the Querdian image) and loads into kind.
* **Loki** accepts old timestamps because the demo config already has `reject_old_samples: false`;
  entries must only be in order *per stream*. The Job `demo-backfill` pushes OTLP/JSON to
  `/otlp/v1/logs` with the same resource attributes the collector sets (so the history and the live
  logs share labels: `k8s_namespace_name`, `k8s_pod_name`, `k8s_container_name`, `service_name`),
  chronologically, in batches of 1500 lines, paced below Loki's default rate limit. Each service's
  history is a different pod (a stream of its own), so it never interferes with the live stream.
* **Tempo** is the delicate one. Spans can be pushed with old timestamps over OTLP, and are found by
  `trace ID` at once, but time-ranged searches select blocks by the block's time range, and Tempo 3's
  live-store has its own setting `live_store.wal.ingestion_time_range_slack` (default 2 minutes; the
  `storage.trace.wal` one does not apply to it) for how far a span may be from the ingestion time. The
  demo raises it to history + 24 h. Black-box against Tempo 3.1.1: with the default, a span 6 hours old
  is not found by a search over its own time range; with the raised slack it is, within 30 seconds.
  Tempo 3 has no `/flush` endpoint; the live-store cuts a block every 2 minutes
  (`max_block_duration`), so the history is searchable shortly after the Job. Search by real time is
  verified by `verify.sh`.

The Job pushes straight to Loki and Tempo, as tenant `demo`, **before** Querdian is installed and
before any NetworkPolicy exists; Querdian never sees ingest traffic and its allow-lists are untouched.

**Backend settings changed for the demo (demo manifests only, `hack/quickstart/manifests/`):**

| Backend | Setting | Why |
|---|---|---|
| Prometheus | `--storage.tsdb.retention.time` 2h to history + 24 h | the 2 h retention would delete the blocks at once |
| Tempo | `storage.trace.wal.ingestion_time_range_slack` = history + 24 h | keep real span times in the block metadata |
| Tempo | `compactor.compaction.block_retention` 2h to history + 24 h | same as Prometheus |
| Tempo | `storage.trace.blocklist_poll: 30s` (default 5m) | new blocks become queryable within 30 s |
| Tempo | memory limit 500 to 700 MiB, `GOMEMLIMIT=500MiB` | the 24 h history made 500 MiB too tight (one OOM kill seen while testing) |
| Loki | none (`reject_old_samples: false` was already set) | |
| Collector | logs get `service.name` = namespace | Loki's `service_name` label was the container name `app` |

Querdian's own configuration and code are untouched.

### Changing or regenerating the data

* **Time span:** `DEMO_HISTORY_HOURS=6 hack/quickstart/up.sh` (1 to 720; also pass it to `verify.sh` if
  you changed it). Retention and slack follow automatically. Volume scales linearly; 24 h is the
  tested size.
* **Regenerate:** the history is created at start-up and anchored at "now", so `down.sh` + `up.sh` gives
  the same day again, ending at the new "now". The generator can also be run by hand:
  `go run ./hack/quickstart/demogen stats -hours 24` (counts), `... metrics -end <unix> > h.om`,
  `... -seed 2` for another day. `go test ./hack/quickstart/demogen/` checks determinism and that log
  lines refer to existing traces.
* **Different incidents or routes:** edit `services()` in `hack/quickstart/demogen/model.go`; the live
  workload table in `hack/quickstart/workload.sh` mirrors it (rates, medians, error ratios).

## Dashboards

Grafana loads four dashboards from the folder *Querdian demo* (file provisioning from the ConfigMap
`grafana-dashboards`, JSON in `hack/quickstart/dashboards/`; the home page is the first one). Every
query goes through Querdian with the logged-in user's token, so every user sees their own slice.
The variables `Namespace` (and `Route`) are filled by label-values queries, so they list only what
the user may see.

| Dashboard | What it shows |
|---|---|
| **Service overview (RED)** | request rate (by route, by status), error ratio (5xx, by route), latency p50/p95/p99 and p95 per route, saturation (worker pool, database connections against the limit, queue depth, memory, CPU). The incidents above are plainly visible. |
| **Logs** | log volume by level (stacked), error and warning counts, error rate, log panels for errors/warnings, a level filter and free text search, and the lines that carry a trace id. The `TraceID` link opens the trace in Tempo. |
| **Traces and latency** | a latency heatmap and p95 by namespace from metrics, and TraceQL searches through Querdian: recent traces, error traces, slow spans (threshold variable), failing downstream calls, server spans with a 5xx status, and a trace-by-ID panel. The trace view's *Logs* button runs the trace to logs query. |
| **What can I see?** | makes the isolation visible: the namespaces and labels each backend reports for the signed-in user (label and label-value queries), series per namespace, log lines per namespace, spans per namespace, explicit queries against the other team's namespace (empty), and *requests by customer_id*: grouped, it is **refused for alice** (redacted labels are denied); as raw series alice sees `redacted-1..3` with Querdian's notice on the panel, bob and carol see `c-1..c-3`. |

What differs per user: alice sees only `order-service` everywhere and the label list of the second
dashboard has no `customer_id`; bob only `payment-service`; carol both plus `observability` and
`querdian`.

Constructs avoided because Querdian rejects them (see [limitations](limitations.md)): `storeMatch`,
root-span intrinsics (`rootName`, `rootServiceName`, `traceDuration`, `compare()`) and tag values of
them for restricted users, `parent.*`, TraceQL metrics (the demo Tempo has no metrics generator),
`rate(... | unwrap)` and `by` clauses on range aggregations in LogQL, Prometheus `metadata`.
Trace search tables for alice and bob have no root name column and use recomputed start and duration:
this is Querdian's redaction, not a dashboard bug.

## Namespace-label integrity in the demo

`AccessPolicy` isolation is only as good as the namespace label in the backends (see the
[policy guide](policy-guide.md)). In this stack the collection pipeline, never the workload, sets it:

* **Metrics.** Prometheus relabels `namespace` from the pod's discovered namespace
  (`manifests/prometheus.yaml`) and runs with `honor_labels: false`.
* **Logs.** The Collector's filelog `container` parser derives `k8s.namespace.name` from the log file
  path, `k8sattributes` confirms it, Loki indexes it as `k8s_namespace_name`.
* **Traces.** The Collector deletes any `k8s.*` resource attribute sent by the workload, then
  `k8sattributes` sets `k8s.namespace.name` from the sender's pod IP (`manifests/otel-collector.yaml`).

The `order-service` sample is deliberately hostile: its metric carries `namespace="payment-service"`
and its spans claim `k8s.namespace.name=payment-service`. `verify.sh` asserts that bob never sees
either, and that the metric label survives only as `exported_namespace`.

## Hostnames: one issuer URL for the laptop and the cluster

A token's `iss` claim must equal `auth.issuer` byte for byte, and the browser, Grafana's server side
and Querdian all have to reach that URL. The quickstart uses

```
http://keycloak.localtest.me:8080/realms/observability
```

* `localtest.me` is a public wildcard domain that resolves to `127.0.0.1`. kind maps host port 8080
  to the Keycloak NodePort, so the browser and `curl` on the laptop reach Keycloak.
* Inside the cluster `up.sh` adds a CoreDNS rule
  `rewrite name keycloak.localtest.me keycloak.keycloak.svc.cluster.local`, and the Keycloak Service
  listens on the same port number (8080). Grafana (token exchange) and Querdian (OIDC discovery and
  JWKS) therefore use the identical URL.
* Keycloak is pinned to that URL with `KC_HOSTNAME`, so tokens always carry this issuer.

If your DNS filters answers pointing to 127.0.0.1 (some routers do), add
`127.0.0.1 keycloak.localtest.me` to `/etc/hosts`. `verify.sh` uses `curl --resolve` and does not
depend on it.

## Try the policy dashboard

The quickstart enables the read-only [policy dashboard](dashboard.md) on <http://localhost:8081> (bound to
`127.0.0.1`, no authentication, laptop demo only). **Policies** lists the three example policies and their
status. In **Access** and **Test bench** enter the group `order-team`, `payment-team` or `platform-admins`
(policies match groups, not user names). Try `demo_requests_total` for `order-team` and for `platform-admins`,
then `sum by (customer_id) (demo_requests_total)` for `order-team` to see the denial.

## Try it in Grafana (manual)

Start with the dashboards: after logging in, the home page is **Service overview (RED)**; the menu
*Querdian demo* switches between the four dashboards (see [Dashboards](#dashboards)). Log in as
alice, then as bob and carol in a private window, and open **What can I see?** to compare. For the
query-by-query walkthrough:

1. Open <http://localhost:3000> and choose **Sign in with Keycloak**. Log in as `alice` / `alice-demo`.
2. **Explore**, data source *Prometheus (via Querdian)*: run `demo_requests_total` or
   `count by (namespace) (up)`. Only `order-service` appears. Run
   `sum by (customer_id) (demo_requests_total)`: Querdian answers `403`, `customer_id` is denied for
   the order team. Run `demo_requests_total` again and look at the `customer_id` column: the values
   are `redacted-1`, `redacted-2`, ... and the query shows Querdian's warning *label values redacted
   by policy*. The same query as bob shows `c-1`, `c-2`, ...
3. Data source *Loki (via Querdian)*: `{k8s_namespace_name=~".+"}` returns only `order-service` logs;
   `{k8s_namespace_name="payment-service"}` returns nothing.
4. Data source *Tempo (via Querdian)*, query type TraceQL: `{}` returns only `order-service` traces.
   Follow an incident: on *Service overview* look at the 5xx ratio about 3h25m ago, on *Logs* open an
   error line and click **TraceID**, then use *Logs* on the trace to come back.
5. Open a private window, log in as `bob` / `bob-demo`: the same queries return `payment-service`
   data. As `carol` / `carol-demo` you see both, plus `querdian` and `observability`.

Tempo search results for the order and payment teams omit `rootServiceName` and similar trace-level
fields: Querdian redacts them whenever the policy cannot prove the root span matches
(see the [limitations](limitations.md)). Carol's unrestricted grant sees them.

## Automatic verification

`hack/quickstart/verify.sh` (about 25 seconds) does the following and exits non-zero on any failure:

1. Gets tokens for alice, bob and carol from Keycloak with the **password grant** (direct access
   grants are enabled on the `grafana` client for this demo only) and checks `aud`, `groups`, `iss`.
2. Talks to the Querdian proxy directly (`kubectl port-forward`) and asserts:
   * 401 without or with an invalid token; 403 for an endpoint that is not allowed;
   * PromQL (`up`, `demo_requests_total`): alice sees only `order-service`, bob only
     `payment-service`, carol all namespaces; explicit cross-namespace selectors return nothing;
   * LogQL (log and metric queries, label values) and TraceQL (search, trace by ID: another team's
     trace answers 404) with the same expectations;
   * the redacted-label example (`sum by (customer_id)` is 403 for alice, `customer_id` is missing
     from her label list, her raw `demo_requests_total` carries `redacted-<n>` values and the
     redaction notice; fine for bob);
   * the spoofing attempts of `order-service` have no effect;
   * the **history** (`DEMO_HISTORY_HOURS`, default 24): the oldest metric sample, log line and
     trace are at least 23 hours old, none are older than the history, alice and bob only see their
     own namespace's history and carol both, the old label list hides `customer_id` for alice and
     `route`, `method` and `status` exist, the incidents are in the data (5xx ratio during the bad
     deploy, p95 during the provider incident), and an old error log line of each team carries a
     `trace_id` whose trace Querdian returns to its own team and answers 404 to the other.
3. Checks Grafana: health, the three provisioned data sources (URL `http://querdian.querdian:8080/...`,
   `oauthPassThru: true`), and **drives the real login flow headlessly**: Grafana's OAuth entry point,
   the Keycloak login form (cookie jar), the callback, then queries through Grafana's data source
   proxy for alice and bob. That exercises the OAuth pass-through end to end. The local Grafana
   admin, which has no token, is refused.
4. Checks the **dashboards**: Grafana lists exactly the four files of `hack/quickstart/dashboards`,
   provisioned from file in the folder *Querdian demo*, with only provisioned data source uids and the
   service overview as home dashboard; the dashboard variable queries (namespace label values, label
   names) return the per-user answers; then **every query of every panel** (43 per user) is executed
   as alice, bob and carol with the user's own Grafana session: Prometheus and Loki through
   `POST /api/ds/query`, TraceQL searches through the data source proxy (Grafana runs those in the
   browser, not in its backend). All must return data, except the panel documented as denied for the
   order team (alice must get Querdian's denied-label error) and the two panels that query the other
   team's namespace (must be empty for that user).

What it does *not* automate: the interactive Explore UI itself (rendering, the browser-side redirect
with a real browser) and the visual rendering of the dashboards (panel layout, the text panels'
variable interpolation, the derived-field links) - that is the manual walkthrough above. The queries
behind every panel are verified, the pixels are not.

## Optional: NetworkPolicy

Off by default (it needs a CNI that enforces policies; recent kind's kindnet does):

```sh
QUICKSTART_NETWORKPOLICY=1 hack/quickstart/up.sh
# or on a running cluster:
helm upgrade querdian charts/querdian -n querdian --reuse-values \
  -f hack/quickstart/querdian-networkpolicy-values.yaml
```

This enables `networkPolicy` (proxy reachable from Grafana only, egress limited to DNS, API server,
Keycloak, backends, collector) and `backendProtection` (Prometheus, Loki and Tempo accept connections
only from Querdian and the collector). Grafana, or any other pod, can no longer bypass the proxy:
a pod in `order-service` that connects to `prometheus.observability:9090` or `loki.observability:3100`
is blocked. Notes:

* `networkPolicy.egress.oidc.ports` must be the **pod** port of the issuer (8080 here), not the
  Service port.
* New pods' policies take a few seconds to be programmed; connectivity tests right after a pod start
  can be admitted wrongly.
* The chart names each backend-protection policy `<release>-allow-querdian-only-<index>`, so several
  targets may share a namespace. The quickstart uses a single target selecting all three backends with
  `matchExpressions` for brevity.

## Timings and footprint

Measured on an Apple-silicon laptop, Podman VM with 5 CPUs and 6 GiB, kind v0.33 (node image
v1.37.0), warm image cache for the Querdian build and cold node image pulls:

| Step | Wall clock |
|---|---|
| `kind create cluster` (node image already local) | 30 s |
| Image build (cached Docker layers, in parallel with the cluster start) and load into kind | included in the next line |
| `up.sh` in total: all images pulled by the node, Keycloak realm imported and every pod Ready, Querdian installed, policies `Ready`, history and telemetry flowing | **2 min 40 s** (3 min 37 s before the history was added; the difference is registry speed, not a speed-up) |
| of which the history: generator image build (12 s, in parallel with the cluster start), Prometheus init container (about 10 s), Loki and Tempo Job (37 s, while Keycloak is still starting) | no measurable addition to the critical path |
| `verify.sh` (153 assertions incl. the headless Grafana login and 132 dashboard queries) | 24 s |

A cold start also has to compile the Go binary in the image build (not measured separately; it runs
in parallel with the cluster creation and the first pulls) and download the node image, so plan for
roughly 5-10 minutes on a first run, well inside the 15-minute budget. Slow registries dominate.

Footprint with everything running and the history loaded: the kind node container about 3.0 GiB
(container stats, includes page cache), the Podman VM 2.6 GiB used and about 3.3 GiB available of
5.9 GiB. Resident sizes: Keycloak 490 MiB, Tempo 360 MiB, Grafana 270 MiB, Loki 150 MiB, collector
135 MiB, Prometheus 90 MiB. The history on disk is small (Prometheus blocks 1.5 MiB). Requests of the demo components are
small (Keycloak 450 MiB, Loki/Tempo/Grafana/Prometheus 100-150 MiB each, Querdian 48 MiB per
replica, sample workloads 8 MiB).

If `up.sh` hangs on image pulls (seen once on a Podman VM whose kubelet stopped starting pods), run
`down.sh` and start again.

## Troubleshooting

* `up.sh` keeps the cluster if it fails (use `CLEANUP_ON_FAILURE=1` to delete it) and prints the pods
  that are not running. `kubectl --context kind-querdian-demo -n <ns> logs deploy/<name>`.
* Login fails with "invalid redirect" or the issuer does not match: make sure you browse to
  `http://localhost:3000` (not `127.0.0.1`) and that `keycloak.localtest.me` resolves (see above).
* Grafana shows `401`/"Authentication to data source failed" for the local admin: expected, log in via
  Keycloak.
* Tokens last 30 minutes (realm setting); log out and in again for new data after that.
* See [troubleshooting.md](troubleshooting.md) for Querdian's decision reasons.
