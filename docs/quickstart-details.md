# Quickstart reference

This page gives the details of the [quickstart](quickstart.md) demo. Read the quickstart first.

## Options

Environment variables for `up.sh`:

| Variable | Default | Effect |
|---|---|---|
| `CLUSTER` | `querdian-demo` | Name of the kind cluster. |
| `KEYCLOAK_PORT` | `8080` | Host port of Keycloak. |
| `IMAGE_TAG` | `quickstart` | Tag of the Querdian image. |
| `SKIP_BUILD=1` | off | Does not build the Querdian image. |
| `DEMO_HISTORY_HOURS` | `24` | Hours of demo data, 1 to 720. Give the same value to `verify.sh`. |
| `QUICKSTART_NETWORKPOLICY=1` | off | Enables the [NetworkPolicy](#networkpolicy) example. |
| `CLEANUP_ON_FAILURE=1` | off | Deletes the cluster if `up.sh` fails. |

`KEEP_IMAGE=1` for `down.sh` keeps the Querdian image. With Podman, the scripts set
`KIND_EXPERIMENTAL_PROVIDER=podman`.

### Published image

By default, `up.sh` builds Querdian from the checkout (`make docker-build`) while the cluster starts,
and loads the image into kind. To run the published v0.1.2 image:

```sh
git switch --detach v0.1.2
docker pull ghcr.io/querdian-dev/querdian:0.1.2
SKIP_BUILD=1 IMAGE_TAG=0.1.2 hack/quickstart/up.sh
```

`up.sh` still installs the chart from the checkout. To use the published chart also, see
[releasing.md](releasing.md#5-post-release-verification).

## Components

| Namespace | Component | Notes |
|---|---|---|
| `keycloak` | Keycloak 26.8.1 (dev mode) | Realm `observability`, from `hack/quickstart/realm-observability.json`. |
| `observability` | Prometheus v3.15.1, Loki 3.7.9, Tempo 3.1.1 (monolithic, no Kafka) | Single instances. Loki and Tempo are multi-tenant, with the tenant `demo`. |
| `observability` | Job `demo-backfill`, image `localhost/querdian-demogen` | Pushes the log and trace history, then completes. |
| `observability` | OpenTelemetry Collector contrib 0.163.2 (DaemonSet) | `filelog` for container logs, OTLP receiver for traces. |
| `observability` | Grafana 13.2.4 | Keycloak login. The data sources point at Querdian. |
| `querdian` | Querdian (chart `charts/querdian`, 2 replicas) | `http://querdian.querdian:8080/{prom,loki,tempo}` |
| `order-service`, `payment-service` | busybox 1.37 (awk simulation) | Sends metrics, JSON or logfmt logs, and traces. The namespaces have the label `policy.querdian.dev/tenant: demo`. |

Accounts: the demo users are in the realm `observability` (client `grafana`, secret
`grafana-demo-secret`). The Keycloak admin console and the local Grafana admin use `admin` / `admin`.

## Policies

From `hack/quickstart/manifests/policies.yaml`:

| Policy | Group | Access |
|---|---|---|
| `AccessPolicy order-team` in `order-service` | `order-team` | Metrics, logs and traces of its namespace. |
| `AccessPolicy payment-team` in `payment-service` | `payment-team` | Metrics, logs and traces of its namespace. |
| `ClusterAccessPolicy platform-admins` | `platform-admins` | `unrestricted` on the three backends for the tenant `demo`. |

The metrics rule of `order-team` has `labels.redact: [{name: customer_id, strategy: Placeholder}]`. This
is the redacted-label example: Querdian denies the label in queries and replaces its values in results.

Backends, from `hack/quickstart/querdian-values.yaml`:

| Backend | Path | Namespace label | Other settings |
|---|---|---|---|
| `metrics` | `/prom` | `namespace` | |
| `logs` | `/loki` | `k8s_namespace_name` | Tenant header `X-Scope-OrgID`. |
| `traces` | `/tempo` | `resource.k8s.namespace.name` | Tenant header, `traceByID: filter`. |

## Demo data

After `up.sh`, the backends contain about one day of data for the two sample services. The live
workloads continue with the same model, and the curves have no step.

The data is synthetic and deterministic. The generator `hack/quickstart/demogen` is a Go program with a
seed. The data depends on the age of a point, not on the clock time: one seed always gives the same
day.

| Signal | Content | Volume (24 h) |
|---|---|---|
| Metrics | `http_requests_total{route,method,status}`, the histogram `http_request_duration_seconds`, `demo_worker_pool_utilization`, `demo_db_connections_in_use`, `demo_db_connections_max`, `demo_queue_depth`, `process_resident_memory_bytes`, `process_cpu_seconds_total`, and `demo_requests_total{customer_id}`. 5 routes for each service, a daily load wave, a resolution of 30 s. | 2 x 99 series, 570 000 samples |
| Logs | `order-service` writes JSON, `payment-service` writes logfmt, with `level`, `method`, `route`, `status` and `duration_ms`. Errors, slow requests and about 12% of the other lines have a `trace_id`. Background lines: pool statistics, cache refreshes, warnings. | about 56 000 lines |
| Traces | One server span for each request, with child spans for Redis, PostgreSQL and the downstream call. `order-service` calls `payment-service /payments`, and `payment-service` calls `acme-pay`. Requests that fail have error spans with an `exception` event. | about 6 900 traces, 21 000 spans |

Each incident shows in the three signals. The times are before the start of `up.sh`:

| Time | Service | Event |
|---|---|---|
| 7 h 30 min to 6 h 30 min | `payment-service` | `acme-pay` is slow: provider calls have 9 times the latency, with up to 14% 503 responses on `POST /payments` and `/refund`. `POST /orders` and `/cancel` of `order-service` depend on these calls: 6 times the latency, up to 10% 503 responses. |
| 3 h 40 min to 3 h 10 min | `order-service` | Bad deploy: no free database connections, 18% 500 responses on each route, database calls 3 times slower. |
| 1 h to 50 min | `payment-service` | Short `acme-pay` problem: 3 times the latency, 3% 503 responses. |

Logs and traces have links in the two directions. The `TraceID` field of the Loki data source opens the
trace of a log line. *Trace to logs* on the Tempo data source runs
`{k8s_namespace_name="<ns>"} |= "<trace id>"`.

### Backfill

Each backend accepts old data in a different way:

| Backend | Method |
|---|---|
| Prometheus | Prometheus does not accept one day of old samples through remote write. An init container runs `demogen metrics` (an OpenMetrics file of about 115 MB), then `promtool tsdb create-blocks-from openmetrics`. This makes 13 two-hour blocks in 4 s (1.5 MB on disk). Prometheus starts with these blocks. The init container image is `localhost/querdian-demogen:<tag>`: the pinned Prometheus image and the generator. |
| Loki | The Job `demo-backfill` pushes OTLP/JSON to `/otlp/v1/logs`, in batches of 1500 lines, below the default rate limit. The demo configuration has `reject_old_samples: false`. The entries of each stream are in time sequence. The Job sets the same resource attributes as the collector, and gives each service a pod name of its own: the history has the same labels as the live logs, but different streams. |
| Tempo | The Job pushes spans with old timestamps over OTLP. A search by trace ID finds them immediately. A search by time needs a larger `live_store.wal.ingestion_time_range_slack` (see below). |

Tempo 3 selects blocks by their time range. The slack setting of its live-store limits the distance
between the span time and the ingestion time (default 2 minutes; `storage.trace.wal` does not apply
here). With the default, a search over the correct time range did not find a span of 6 hours ago in a
test with Tempo 3.1.1. With the larger slack, the search found it in 30 seconds. Tempo 3 has no
`/flush` endpoint. The live-store cuts a block every 2 minutes (`max_block_duration`).

The Job pushes directly to Loki and Tempo as the tenant `demo`, before the installation of Querdian and
of a NetworkPolicy. Querdian does not see ingest traffic.

Backend settings that the demo changes, in `hack/quickstart/manifests/` only:

| Backend | Setting | Reason |
|---|---|---|
| Prometheus | `--storage.tsdb.retention.time` from 2h to history + 24 h | A retention of 2 h deletes the blocks immediately. |
| Tempo | `storage.trace.wal.ingestion_time_range_slack` = history + 24 h | Keeps the real span times in the block metadata. |
| Tempo | `compactor.compaction.block_retention` from 2h to history + 24 h | Same reason as for Prometheus. |
| Tempo | `storage.trace.blocklist_poll: 30s` (default 5m) | Queries can use new blocks after 30 s. |
| Tempo | Memory limit from 500 to 700 MiB, `GOMEMLIMIT=500MiB` | 500 MiB was too small for the 24 h history (one OOM kill in a test). |
| Collector | Logs get `service.name` = namespace | Without this, the `service_name` label in Loki was the container name `app`. |

Loki needs no change. The demo does not change the configuration or the code of Querdian.

### Change the data

* **Time span.** `DEMO_HISTORY_HOURS=6 hack/quickstart/up.sh` (1 to 720). Retention and slack follow
  automatically. The volume is linear to the hours. Only 24 h has tests.
* **New data.** The history ends at the start time of `up.sh`. `down.sh`, then `up.sh`, gives the same
  day again with a new end.
* **Generator.** `go run ./hack/quickstart/demogen stats -hours 24` prints the counts.
  `... metrics -end <unix> > h.om` writes the metrics. `... -seed 2` gives a different day.
  `go test ./hack/quickstart/demogen/` tests that the data is deterministic and that the log lines
  refer to traces that exist.
* **Incidents and routes.** Edit `services()` in `hack/quickstart/demogen/model.go`. Keep the table of
  the live workload in `hack/quickstart/workload.sh` the same.

## Dashboards

Grafana loads four dashboards into the folder *Querdian demo*, from the ConfigMap `grafana-dashboards`
(JSON in `hack/quickstart/dashboards/`). Each query goes through Querdian with the token of the user.
Label-values queries fill the variables `Namespace` and `Route`. Thus each user sees only permitted
data and permitted values.

| Dashboard | Content |
|---|---|
| **Service overview (RED)**, the home page | Request rate, error ratio (5xx) and latency (p50, p95, p99) by route. Saturation: worker pool, database connections, queue depth, memory, CPU. |
| **Logs** | Log volume by level, error and warning counts, log panels with a level filter and a text search. The `TraceID` link opens the trace. |
| **Traces and latency** | Latency heatmap and p95 from metrics. TraceQL searches: recent traces, error traces, slow spans, downstream calls that fail, server spans with a 5xx status, trace by ID. |
| **What can I see?** | The namespaces and labels that each backend reports for the user. Series, log lines and spans for each namespace. Queries against the namespace of the other team (no data). *Requests by customer_id*. |

*Requests by customer_id* is the redacted-label example. Querdian refuses the grouped query for alice.
As raw series, alice sees `redacted-1` to `redacted-3` with the notice of Querdian. Bob and carol see
`c-1` to `c-3`. The label list of alice has no `customer_id`.

The dashboards do not use constructs that Querdian rejects (see [limitations](limitations.md)):

* `storeMatch`, `parent.*` and Prometheus `metadata`;
* the root-span intrinsics `rootName`, `rootServiceName`, `traceDuration` and `compare()`, and their
  tag values for restricted users;
* TraceQL metrics (the demo Tempo has no metrics generator);
* `rate(... | unwrap)` and `by` clauses on range aggregations in LogQL.

The trace search tables of alice and bob have no root name column, and Querdian calculates start and
duration again. This is redaction, not a defect of the dashboard.

## Namespace-label integrity

The isolation of an `AccessPolicy` depends on a correct namespace label in the backends (see the
[policy guide](policy-guide.md)). In this stack, the collection pipeline sets the label, not the
workload:

| Signal | Source of the namespace |
|---|---|
| Metrics | Prometheus relabels `namespace` from the namespace of the discovered pod, with `honor_labels: false` (`manifests/prometheus.yaml`). |
| Logs | The collector gets `k8s.namespace.name` from the path of the log file (`filelog`, `container` parser), and `k8sattributes` confirms it. Loki indexes it as `k8s_namespace_name`. |
| Traces | The collector deletes each `k8s.*` resource attribute from the workload. Then `k8sattributes` sets `k8s.namespace.name` from the pod IP of the sender (`manifests/otel-collector.yaml`). |

The `order-service` sample is hostile on purpose: its metric has the label
`namespace="payment-service"`, and its spans claim `k8s.namespace.name=payment-service`. `verify.sh`
tests that bob sees neither, and that the metric label stays only as `exported_namespace`.

## Hostnames

The `iss` claim of a token must be equal to `auth.issuer`, byte for byte. The browser, Grafana and
Querdian must all get access to that URL:

```
http://keycloak.localtest.me:8080/realms/observability
```

* **Laptop.** `localtest.me` is a public wildcard domain that resolves to `127.0.0.1`. kind maps the
  host port 8080 to the Keycloak NodePort.
* **Cluster.** `up.sh` adds the CoreDNS rule
  `rewrite name keycloak.localtest.me keycloak.keycloak.svc.cluster.local`. The Keycloak Service uses
  the same port, 8080. Thus Grafana (token exchange) and Querdian (OIDC discovery, JWKS) use the same
  URL.
* **Keycloak.** `KC_HOSTNAME` sets this URL as the issuer of each token.

Some routers remove DNS answers that point to `127.0.0.1`. In that case, add
`127.0.0.1 keycloak.localtest.me` to `/etc/hosts`. `verify.sh` uses `curl --resolve` and does not
depend on DNS.

## Verification

`hack/quickstart/verify.sh` needs about 25 seconds and stops with a non-zero exit code if a check
fails.

| Area | Checks |
|---|---|
| Tokens | Password grant for alice, bob and carol (the client `grafana` permits it for this demo only). Claims `aud`, `groups` and `iss`. |
| Authentication | 401 without a token or with an invalid token. 403 for an endpoint that is not allowed. |
| PromQL, LogQL, TraceQL | Directly against Querdian (`kubectl port-forward`). Alice sees only `order-service`, bob only `payment-service`, carol all namespaces. A selector for a different namespace gives no data. A trace of a different team gives 404. |
| Redacted label | `sum by (customer_id)` gives 403 for alice. Her label list has no `customer_id`. Her raw series have `redacted-<n>` values and the redaction notice. Bob has no restriction. |
| Spoofing | The false namespace of `order-service` has no effect. |
| History | The oldest sample, log line and trace are a minimum of 23 hours old, and none is older than the history. Each user sees only the permitted history. The incidents are in the data. An old error log line of each team has a `trace_id`: Querdian gives that trace to its team and 404 to the other team. |
| Grafana | Health and the three data sources (`oauthPassThru: true`). The real login flow without a browser: OAuth entry point, Keycloak login form, callback. Queries through the data source proxy for alice and bob. Querdian refuses the local admin. |
| Dashboards | Exactly the four dashboards, with the service overview as home page. The variable queries for each user. Each query of each panel (43 for each user) as alice, bob and carol. |

All panel queries must give data, with three exceptions. The panel that is denied for the order team
must give the denied-label error to alice. The two panels that query the namespace of the other team
must be empty for that user.

The script does not examine the Explore UI, the redirect in a real browser, or the look of the
dashboards. For these, do the manual steps of the
[quickstart](quickstart.md#see-the-isolation-in-grafana).

## NetworkPolicy

The NetworkPolicy example is off by default. It needs a CNI that enforces policies; `kindnet` of a
recent kind does.

```sh
QUICKSTART_NETWORKPOLICY=1 hack/quickstart/up.sh
# or on a cluster that runs:
helm upgrade querdian charts/querdian -n querdian --reuse-values \
  -f hack/quickstart/querdian-networkpolicy-values.yaml
```

The example enables two chart features:

* `networkPolicy`: only Grafana gets access to the proxy. The egress of Querdian is limited to DNS, the
  API server, Keycloak, the backends and the collector.
* `backendProtection`: Prometheus, Loki and Tempo accept connections only from Querdian and the
  collector.

No pod can then bypass the proxy. For example, a pod in `order-service` cannot connect to
`prometheus.observability:9090` or `loki.observability:3100`.

* `networkPolicy.egress.oidc.ports` must be the pod port of the issuer (8080 here), not the Service
  port.
* The cluster needs some seconds to apply the policies to a new pod. A connectivity test immediately
  after a pod start can pass incorrectly.
* The chart names each backend-protection policy `<release>-allow-querdian-only-<index>`: more than
  one target can be in one namespace. The quickstart uses one target that selects the three backends
  with `matchExpressions`.

## Timings and footprint

Measured on a laptop with Apple silicon: Podman VM with 5 CPUs and 6 GiB, kind v0.33, node image
v1.37.0, warm cache for the Querdian build, all other images pulled.

| Step | Time |
|---|---|
| `kind create cluster`, with a local node image | 30 s |
| `up.sh` in total, until the policies are `Ready` and the telemetry flows | 2 min 40 s |
| `verify.sh` (153 checks) | 24 s |

The history adds no measurable time: its steps run while other steps wait (generator image 12 s,
Prometheus init container about 10 s, Job for Loki and Tempo 37 s). A first run also compiles the Go
binary and downloads the node image, and needs 5 to 10 minutes. Slow registries have the largest
effect.

| Memory, with all components and the history | Value |
|---|---|
| kind node container (with page cache) | about 3.0 GiB |
| Podman VM | 2.6 GiB used, about 3.3 GiB available of 5.9 GiB |
| Resident memory | Keycloak 490 MiB, Tempo 360 MiB, Grafana 270 MiB, Loki 150 MiB, collector 135 MiB, Prometheus 90 MiB |
| Memory requests | Keycloak 450 MiB; Loki, Tempo, Grafana and Prometheus 100 to 150 MiB each; Querdian 48 MiB for each replica; sample workloads 8 MiB |

Keycloak uses the most memory: its JVM heap has a limit of 300 MiB, and its container a limit of
750 MiB.
