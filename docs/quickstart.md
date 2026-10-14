# Quickstart on kind

A complete isolation demo on a laptop: one script creates a single-node [kind](https://kind.sigs.k8s.io)
cluster with Keycloak, Prometheus, Loki, Tempo, an OpenTelemetry Collector, Grafana, two sample
workloads (`order-service`, `payment-service`) and Querdian with example policies. Two demo users
log in to Grafana with Keycloak and see different data.

> **Demo only.** Keycloak runs in dev mode over plain HTTP with well-known passwords, the demo
> OAuth client allows the password grant (used by `verify.sh`), and nothing is persistent. Do not
> reuse any of it in production. Querdian is pre-alpha.

```sh
hack/quickstart/up.sh       # cluster + everything, ends with the URLs
hack/quickstart/verify.sh   # automatic assertions (71 checks)
hack/quickstart/down.sh     # removes the cluster and the built image
```

## Requirements

* `kind` (tested with v0.33), `kubectl`, `helm` 3/4, `make`, `curl`, `jq`, and Docker or Podman
  (with the `docker` CLI shim; the scripts then set `KIND_EXPERIMENTAL_PROVIDER=podman`).
* About **3 GiB of free memory** for the container VM and ~5 GiB of disk for images. Measured on a
  6 GiB Podman VM (5 CPUs): the kind node holds about 2.0 GiB with everything running, the VM
  reports about 2.3 GiB used and 3.6 GiB available. Keycloak is the largest part (JVM heap capped at
  300 MiB, container limit 750 MiB).
* Free host ports `8080` (Keycloak) and `3000` (Grafana). Change Keycloak's with `KEYCLOAK_PORT=8081`.
* Internet access: the images are pulled from Docker Hub and quay.io by the kind node.
* Querdian has no published image yet, so `up.sh` builds it from this checkout (`make docker-build`,
  in the background while the cluster starts) and loads it into kind.

Options (environment variables): `CLUSTER` (default `querdian-demo`), `KEYCLOAK_PORT`,
`IMAGE_TAG` (default `quickstart`), `SKIP_BUILD=1`, `QUICKSTART_NETWORKPOLICY=1`,
`CLEANUP_ON_FAILURE=1`; `down.sh` honours `KEEP_IMAGE=1`.

## What gets deployed

| Namespace | Component | Notes |
|---|---|---|
| `keycloak` | Keycloak 26.4.0 (dev mode) | realm `observability` imported from `hack/quickstart/realm-observability.json` |
| `observability` | Prometheus v3.5.0, Loki 3.5.0, Tempo 2.9.0 | single instances; Loki and Tempo multi-tenant, tenant `demo` |
| `observability` | OpenTelemetry Collector contrib 0.137.0 (DaemonSet) | filelog for container logs, OTLP receiver for traces |
| `observability` | Grafana 12.2.0 | Keycloak login, data sources point at Querdian |
| `querdian` | Querdian (chart `charts/querdian`, 2 replicas) | `http://querdian.querdian:8080/{prom,loki,tempo}` |
| `order-service`, `payment-service` | busybox 1.37 loop | emits metrics, logs and traces; both namespaces carry `policy.querdian.dev/tenant: demo` |

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
  metrics rule also carries `labels.deny: [customer_id]` (the **denied-label example**).
* `AccessPolicy payment-team` in `payment-service` (group `payment-team`): metrics, logs, traces.
* `ClusterAccessPolicy platform-admins` (group `platform-admins`): `unrestricted` on all three
  backends for tenant `demo`.

Backends in the Querdian configuration (`hack/quickstart/querdian-values.yaml`): `metrics`
(`/prom`, `namespaceLabel: namespace`), `logs` (`/loki`, `k8s_namespace_name`, tenant header
`X-Scope-OrgID`), `traces` (`/tempo`, `resource.k8s.namespace.name`, tenant header, `traceByID: filter`).

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

## Try it in Grafana (manual)

1. Open <http://localhost:3000> and choose **Sign in with Keycloak**. Log in as `alice` / `alice-demo`.
2. **Explore**, data source *Prometheus (via Querdian)*: run `demo_requests_total` or
   `count by (namespace) (up)`. Only `order-service` appears. Run
   `sum by (customer_id) (demo_requests_total)`: Querdian answers `403`, `customer_id` is denied for
   the order team.
3. Data source *Loki (via Querdian)*: `{k8s_namespace_name=~".+"}` returns only `order-service` logs;
   `{k8s_namespace_name="payment-service"}` returns nothing.
4. Data source *Tempo (via Querdian)*, query type TraceQL: `{}` returns only `order-service` traces.
5. Open a private window, log in as `bob` / `bob-demo`: the same queries return `payment-service`
   data. As `carol` / `carol-demo` you see both, plus `querdian` and `observability`.

Tempo search results for the order and payment teams omit `rootServiceName` and similar trace-level
fields: Querdian redacts them whenever the policy cannot prove the root span matches
(see the [limitations](limitations.md)). Carol's unrestricted grant sees them.

## Automatic verification

`hack/quickstart/verify.sh` (about 10 seconds) does the following and exits non-zero on any failure:

1. Gets tokens for alice, bob and carol from Keycloak with the **password grant** (direct access
   grants are enabled on the `grafana` client for this demo only) and checks `aud`, `groups`, `iss`.
2. Talks to the Querdian proxy directly (`kubectl port-forward`) and asserts:
   * 401 without or with an invalid token; 403 for an endpoint that is not allowed;
   * PromQL (`up`, `demo_requests_total`): alice sees only `order-service`, bob only
     `payment-service`, carol all namespaces; explicit cross-namespace selectors return nothing;
   * LogQL (log and metric queries, label values) and TraceQL (search, trace by ID: another team's
     trace answers 404) with the same expectations;
   * the denied-label example (`sum by (customer_id)` is 403 for alice, `customer_id` is missing from
     her label list, fine for bob);
   * the spoofing attempts of `order-service` have no effect.
3. Checks Grafana: health, the three provisioned data sources (URL `http://querdian.querdian:8080/...`,
   `oauthPassThru: true`), and **drives the real login flow headlessly**: Grafana's OAuth entry point,
   the Keycloak login form (cookie jar), the callback, then queries through Grafana's data source
   proxy for alice and bob. That exercises the OAuth pass-through end to end. The local Grafana
   admin, which has no token, is refused.

What it does *not* automate: the interactive Explore UI itself (rendering, the browser-side redirect
with a real browser) - that is the manual walkthrough above.

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
| `up.sh` in total: all images pulled by the node, Keycloak realm imported and every pod Ready, Querdian installed, policies `Ready`, telemetry flowing | **3 min 37 s** |
| `verify.sh` (71 assertions incl. the headless Grafana login) | 3 s |

A cold start also has to compile the Go binary in the image build (not measured separately; it runs
in parallel with the cluster creation and the first pulls) and download the node image, so plan for
roughly 5-10 minutes on a first run, well inside the 15-minute budget. Slow registries dominate.

Footprint with everything running: the kind node container about 2.3 GiB (container stats), the
Podman VM 2.2 GiB used and about 3.6 GiB available of 5.9 GiB. Requests of the demo components are
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
