# Multi-cluster quickstart on kind

This demo shows [multi-cluster enforcement](multicluster.md) on one laptop. One script creates three
[kind](https://kind.sigs.k8s.io) clusters. The **hub** has the backends, Keycloak, Grafana and Querdian.
The two **spokes** run only the Querdian agent, and they send their telemetry to the hub. All three
clusters have a namespace with the same name, `order-service`. A different team owns it in each cluster,
and each team sees only the data of its own cluster.

> **Demo only.** Keycloak runs in dev mode over plain HTTP with known passwords, all CA keys are in one
> directory on the laptop, and the ingest gateway is a demo pattern, not a part of Querdian. Do not use
> any of it in production. The multi-cluster feature is alpha.

```sh
hack/quickstart-multicluster/up.sh       # three clusters and the ordered setup, ends with the URLs
hack/quickstart-multicluster/verify.sh   # automatic checks through the Querdian proxy
hack/quickstart-multicluster/down.sh     # removes the clusters, the certs and the built image
```

The design of the feature is [PLAN.md section 18](../PLAN.md#18-multi-cluster). The operator guide,
with the ordered migration steps that `up.sh` follows, is [multi-cluster enforcement](multicluster.md).
The [single-cluster quickstart](quickstart.md) is the base of this demo: read it first.

## Requirements

* The tools of the single-cluster quickstart (`kind`, `kubectl`, `helm`, `make`, `curl`, `jq`, Docker or
  Podman with the `docker` shim), and `openssl` (OpenSSL 1.1 or later; LibreSSL was not tested).
* Bash 3.2 or later. The scripts do not use features of later versions.
* Free host ports `8080` (Keycloak), `3000` (Grafana) and `8081` (policy dashboard). The single-cluster
  quickstart uses the same three ports, so remove it first (`hack/quickstart/down.sh`). `KEYCLOAK_PORT`
  changes only the port of Keycloak.
* Free local ports `18080`, `19090`, `13100` and `13200` for the port-forwards of the scripts.
* Internet access: each kind node pulls its images from Docker Hub and quay.io.

### Resources and run time

These values are **estimates**. Nobody has measured a run of this demo yet. They come from the
measured values of the single-cluster quickstart and from the size of a kind node.

| Item | Estimate |
|---|---|
| Memory in use | about 4 to 4.5 GiB in total |
| Memory of the hub | about 2.5 GiB: the single-cluster stack without the history, the gateway and the receiver |
| Memory of each spoke | about 0.7 to 0.9 GiB: a kind control plane, one collector, the agent and two busybox workloads |
| Container VM | **8 GiB** of memory and 4 CPUs or more |
| Disk | about 6 GiB: three node containers, and the collector and busybox images are pulled in each cluster. |
| `up.sh` | about 8 to 12 minutes with a warm image cache; 15 to 20 minutes on a first run (Go build, node image and image pulls). Keycloak and the image pulls are the slowest parts. |
| `verify.sh` | about 1 to 2 minutes. Most of the time is the wait for the access of the suspended spoke to return. |

Options (environment variables): `CLUSTER_PREFIX` (default `querdian`, gives the cluster names
`querdian-hub`, `querdian-spoke-a`, `querdian-spoke-b`), `MC_WORK_DIR` (default
`~/.cache/querdian-quickstart-multicluster`), `KEYCLOAK_PORT`, `IMAGE_TAG`, `SKIP_BUILD=1`,
`CLEANUP_ON_FAILURE=1`; `down.sh` uses `KEEP_IMAGE=1`. `up.sh` sets the current kubectl context to
the hub.

## Topology

```
 laptop: 127.0.0.1:3000 Grafana, :8080 Keycloak, :8081 policy dashboard
   |
 Docker network "kind"
   |
   +-- querdian-hub (cluster label "hub") ------------------------------------------------------------+
   |    Keycloak, Prometheus, Loki, Tempo, Grafana, hub collector, order-service, payment-service      |
   |    querdian (proxy, multicluster block, dashboard)   querdian-receiver   NodePort 30443 (mTLS)   |
   |    ingest-gateway (demo): NodePort 31001 for spoke-a, 31002 for spoke-b (mTLS OTLP/HTTP)          |
   +------------------------------------------------------------------------------------------------+
          ^ agent sync (agent cert)          ^ metrics, logs, traces (ingest cert)
          |                                  |
   +-- querdian-spoke-a / querdian-spoke-b ---------------------------------------------------------+
   |    querdian-agent (chart mode spoke), otel-collector, order-service, payment-service             |
   +------------------------------------------------------------------------------------------------+
```

| Cluster | Components | Namespaces with a team |
|---|---|---|
| `querdian-hub` | the stack of the single-cluster quickstart (Keycloak, Prometheus, Loki, Tempo, Grafana, an OpenTelemetry Collector, the two busybox workloads), the demo ingest gateway, Querdian from the chart with the `multicluster` block (`hubCluster: hub`) and the receiver | `order-service`: team-hub (hub `AccessPolicy`) |
| `querdian-spoke-a` | the chart in `mode: spoke` (the agent only), the two busybox workloads, a collector that sends to the gateway | `order-service`: team-a (spoke `AccessPolicy`) |
| `querdian-spoke-b` | the same as spoke-a | `order-service`: team-b (spoke `AccessPolicy`) |

### Network and ports

The kind node containers share the Docker network `kind`. The spokes reach the hub at the address of the
hub node on this network (`up.sh` prints it), through NodePorts. The spokes need only this outbound
path. The hub does not connect to the spokes.

| Port | Where | What |
|---|---|---|
| 30443 | hub node | receiver (`querdian receiver`, mTLS, `POST /v1alpha1/sync`). The chart value `receiver.service.nodePort` keeps the port fixed. |
| 31001 | hub node | ingest gateway, port of spoke-a (mTLS OTLP/HTTP) |
| 31002 | hub node | ingest gateway, port of spoke-b (mTLS OTLP/HTTP) |
| 8080, 3000, 8081 | laptop, `127.0.0.1` only | Keycloak, Grafana, policy dashboard (kind port mappings to the NodePorts 30080, 30030, 30081) |

Each cluster has its own pod and Service ranges (hub `10.244.0.0/16`, spoke-a `10.245.0.0/16`, spoke-b
`10.246.0.0/16`), so the addresses in the data of different clusters do not overlap.

### Certificates

`hack/quickstart-multicluster/certs.sh` generates the certs with openssl in `$MC_WORK_DIR/certs` (EC
P-256, valid for 30 days). At the end it verifies the chains and the separation with `openssl verify`.

| Files | Issued by | Use |
|---|---|---|
| `hub-server-ca.crt` | itself | CA of the two server certs; the spokes trust it |
| `receiver.crt` | hub-server-ca | server cert of the receiver, IP SAN of the hub node |
| `gateway.crt` | hub-server-ca | server cert of the ingest gateway, IP SAN of the hub node |
| `agent-ca.crt` | itself | the **hub-controlled agent CA**, `receiver.clientCA` |
| `agent-spoke-a.crt`, `agent-spoke-b.crt` | agent-ca | agent certs, one URI SAN `spiffe://quickstart.local/agent/1/<spoke>` |
| `ingest-ca-spoke-a.crt`, `ingest-ca-spoke-b.crt` | itself | a **separate ingest CA for each spoke** |
| `ingest-spoke-a.crt`, `ingest-spoke-b.crt` | the ingest CA of the spoke | ingest client certs, URI SAN `spiffe://quickstart.local/ingest/<spoke>` |

The agent cert is not the ingest cert, and the two have different CAs and different identities
([PLAN 18.5](../PLAN.md#185-policy-transport)). The receiver trusts only the agent CA, so it refuses an
ingest cert. The gateway trusts only the ingest CA of a spoke on the port of that spoke, so it refuses
an agent cert and the ingest cert of the other spoke. The last path segment of the agent identity is the
`SpokeCluster` name, and the middle segment `1` is the cert generation (see
[the agent certificate](multicluster.md#the-agent-certificate-and-its-ca)).

In a real deployment, only the hub holds the agent CA key. In this demo all keys are on the laptop.

## The ingest gateway (a demo pattern)

Querdian cannot verify the cluster label, and it does not include an ingest gateway. Isolation between
the spokes depends on a gateway that sets the label from the credential of the spoke and **overwrites**
each value that the spoke sends ([cluster label integrity](multicluster.md#cluster-label-integrity-the-ingest-gateway)).
The demo uses an OpenTelemetry Collector for this job
(`hack/quickstart-multicluster/manifests/ingest-gateway.yaml`):

* One OTLP/HTTP receiver for each spoke, on its own port. Each receiver requires a client cert and
  trusts only the ingest CA of its spoke. The hub controls the ingest CAs, so the port and its CA identify
  the spoke.
* Each pipeline has one receiver and the processors of its spoke, for metrics, logs and traces. The
  processors use `upsert`, never `insert`:
  * the resource attributes `k8s.cluster.name` and `cluster` get the name of the spoke (Loki indexes
    `k8s.cluster.name` as the label `k8s_cluster_name`, Tempo keeps it as `resource.k8s.cluster.name`);
  * the attribute `cluster` of each data point, log record and span gets the name of the spoke (the
    Prometheus label `cluster`);
  * the attribute `k8s.cluster.name` of data points, log records and spans is deleted;
  * the metrics pipelines also delete the resource attribute `k8s.cluster.name` after the upsert.
    Prometheus copies the resource attributes to the series `target_info`, which then has only the label
    `cluster`. Thus no second label with a value of the spoke exists.
* The gateway writes the metrics to the OTLP receiver of Prometheus, and the logs and traces to the OTLP
  endpoints of Loki and Tempo, with the tenant `demo`.

The collectors of the spokes are **hostile on purpose**: the collector of spoke-a writes the cluster label
`hub` on all of its data, and the collector of spoke-b writes `spoke-a`. `verify.sh` checks that each team
sees only pods of its own cluster, so a gateway that keeps the claim fails the checks.

Limits of this design:

* The gateway checks the CA, not the URI SAN of the client cert. This is correct only because each spoke
  has its own CA and only the hub can issue certs from it.
* There is no certificate revocation. To remove a spoke, remove its port and its CA from the gateway
  ([remove a spoke](multicluster.md#remove-a-spoke)).
* One port and one CA for each spoke does not scale to many spokes. A production gateway needs a
  mapping from the cert identity to the cluster name.
* The processors know which attributes Prometheus (OTLP receiver), Loki (default `otlp_config`) and Tempo
  map to the cluster labels of this demo. A different mapping, for example more Loki index labels, needs
  a new review of the processors.
* The backends have no write authentication. A pod on the hub can write data with the cluster label of
  a spoke and bypass the gateway ([PLAN 18.8](../PLAN.md#188-label-integrity)). A real hub restricts the
  write paths, for example with NetworkPolicies.
* The policy dashboard has no authentication (as in the single-cluster quickstart). The kind port
  mapping binds it to `127.0.0.1` on the laptop, but its NodePort 30081 on the hub node is also open to
  each container on the Docker network `kind`, thus also to the pods of the spokes. The dashboard shows
  policies, not telemetry.

## The ordered setup

`up.sh` follows the [migration steps](multicluster.md#migration-ordered-steps) in their order:

1. **Steps 0, 1 and 3.** The hub is new: it has no old policies and no old data. The chart installs the
   CRDs and the `multicluster` block in one release (`hack/quickstart/querdian-values.yaml`, then
   `hack/quickstart-multicluster/hub-values.yaml`). The hub `ClusterAccessPolicy` objects state their
   cluster scope, and `up.sh` waits until all hub policies are `Ready`, so none is `InvalidClusterScope`.
2. **Step 2.** Before Querdian and before a spoke exists, `up.sh` makes sure that hub telemetry carries
   `cluster="hub"`. Prometheus writes the target label `cluster="hub"` at scrape time, and the hub
   collector writes `k8s.cluster.name="hub"` with `upsert`. The direct queries of step 2 must find no
   data without the label: the PromQL and LogQL queries return nothing. For traces, the guide compares
   two TraceQL metrics counts, but the demo Tempo has no metrics generator. Thus `up.sh` searches for
   spans with `resource.k8s.cluster.name = nil` and with a different value, and both searches must find
   nothing.
3. **Step 4.** The chart installs the receiver with the hub. `up.sh` registers each spoke as a
   `SpokeCluster` with `suspended: true`, then installs the agent in the spoke. The receiver rejects each
   sync of a suspended spoke, so `LastSync` stays empty, and `up.sh` checks this.
4. **Step 5.** The gateway is ready before a spoke connects. The spoke collectors start only now.
   `up.sh` then runs `forged-label-test.sh` for each spoke while it is still suspended (see below). If a
   test fails, `up.sh` stops and the spoke stays suspended.
5. **Step 6.** Only after its test passes, `up.sh` sets `suspended: false`. It waits for the first valid
   sync (`lastSyncTime` and `observedGeneration`) and for the `Ready` condition that the agent writes on
   the spoke `AccessPolicy`.

### The forged-label test

`hack/quickstart-multicluster/forged-label-test.sh <spoke>` sends, from the node container of the spoke
and with the ingest cert of that spoke, one sample, one log line and one span for each other cluster
name: the other spoke and `hub`. Each item claims that name in its resource attributes and in its own
attributes. The script then queries Prometheus, Loki and Tempo **directly** (port-forwards on the hub, not
through Querdian):

* each stored sample, log line and span must carry the cluster of the cert (Prometheus `cluster`, Loki
  `k8s_cluster_name` and the structured metadata `cluster`, Tempo `resource.k8s.cluster.name` and
  `resource.cluster`);
* no item may carry the attribute `k8s.cluster.name` on its data point or span.

It also checks that the TLS handshake fails for the ingest cert on the port of the other spoke, for the
agent cert on the gateway, for a client without a cert, and for the ingest cert on the receiver. You can
run the script again at any time.

## Users and policies

| User | Password | Group | Policy | Sees |
|---|---|---|---|---|
| alice | `alice-demo` | `team-a` | `AccessPolicy team-a` in `order-service` of **spoke-a** | `order-service` of spoke-a |
| bob | `bob-demo` | `team-b` | `AccessPolicy team-b` in `order-service` of **spoke-b** | `order-service` of spoke-b |
| hana | `hana-demo` | `team-hub` | hub `AccessPolicy team-hub` in `order-service` | `order-service` of the hub |
| sam | `sam-demo` | `sre` | hub `ClusterAccessPolicy sre`, a fixed cluster set `spoke-a\|spoke-b` | all namespaces of both spokes, not the hub |
| carol | `carol-demo` | `platform-admins` | hub `ClusterAccessPolicy platform-admins`, `unrestricted` | everything in tenant `demo` |

The realm is the realm of the single-cluster quickstart with these users and groups
(`hack/quickstart-multicluster/realm-users.json`, merged by `up.sh`).

* The spoke teams write their `AccessPolicy` in their own cluster
  (`hack/quickstart-multicluster/manifests/spoke-policies.yaml`). The agent sends it to the hub, and the
  hub compiles it to `namespace="order-service"` AND `cluster="<spoke>"`. The cluster comes from the agent
  cert, never from the policy text.
* The hub `AccessPolicy` gets `cluster="hub"`, so team-hub does not see the `order-service` namespaces of
  the spokes, which have the same name.
* The SRE policy names a fixed set of clusters for each backend, because the cluster label has a
  different name on each backend. A third spoke would not be in this grant. `allClusters: true` would
  include it ([step 1](multicluster.md#1-review-each-hub-clusteraccesspolicy)).
* Hub policies never expire. While a spoke is suspended, sam and carol still see its data.

## Try it

* **Grafana.** Open <http://localhost:3000>, choose **Sign in with Keycloak** and log in as alice. In
  **Explore** run `count by (cluster, namespace) (demo_requests_total)` on *Prometheus (via Querdian)*:
  only `spoke-a`, `order-service`. Log in as bob, hana and sam in private windows and compare. The four
  dashboards of the single-cluster quickstart are provisioned too. They have no cluster variable, so a
  user with grants in several clusters sees the sum of those clusters.
* **Policy dashboard.** Open <http://localhost:8081>. **Policies** shows the origin of each policy: `hub`,
  or the spoke cluster with the kind, namespace and name of the source object. In **Access**, enter the
  group `team-a`, `team-b`, `team-hub` or `sre`.
* **Suspend a spoke.** On the hub, run

  ```sh
  kubectl --context kind-querdian-hub patch spokecluster spoke-a --type merge -p '{"spec":{"suspended":true}}'
  ```

  Queries of alice now get `403` with the message `the policy source of this grant has expired`, and the
  dashboard shows the spoke policy as expired. bob, sam and carol are not affected. Set
  `"suspended":false` again: the grants return after the next sync of the agent (the heartbeat of the demo
  is 15 seconds).
* **State on the hub and in the spokes.**

  ```sh
  kubectl --context kind-querdian-hub get spokeclusters,spokepolicies
  kubectl --context kind-querdian-spoke-a -n order-service get accesspolicy   # Ready, written by the agent
  ```

## Automatic verification

`hack/quickstart-multicluster/verify.sh` gets tokens for the five users from Keycloak with the password
grant (demo only) and queries the Querdian proxy of the hub directly (`kubectl port-forward`). It exits
with a non-zero status on any failure.

1. **Isolation on all three backends.** alice, bob and hana each see only `order-service` of their own
   cluster in PromQL, LogQL and TraceQL search results, and in the label values of the cluster label.
   Explicit queries for another cluster return nothing. The pods in their results are pods of their own
   cluster, from the API server of that cluster. Trace by ID of a trace of spoke-a gives `404` to bob and
   hana and `200` to sam.
2. **SRE and admin.** sam sees both spokes and not the hub; carol sees all three clusters. For each
   cluster, the data with its cluster label comes from pods of that cluster.
3. **State.** Each `SpokeCluster` has a recent `lastSyncTime` and an `observedGeneration` equal to its
   generation, each spoke has one `SpokePolicy` that is `Ready`, the agent wrote `Ready` on the spoke
   `AccessPolicy`, and no proxy runs in a spoke. The dashboard API shows the origin of each policy.
4. **Suspend and unsuspend.** With spoke-a suspended, alice gets `403` on all three backends, the
   dashboard API reports `policy_source_expired` and the expired state, and bob and sam keep their
   access. The metric `querdian_cluster_policy_expired` is 1 for spoke-a. After the unsuspend, the access of
   alice returns. If the script stops early, it unsuspends spoke-a.
5. **Grafana.** Health and the three data sources.

`verify.sh --smoke` only checks that the data of all three clusters arrives and that the spoke grants are
active. `up.sh` uses it at the end.

The hub Prometheus scrapes Querdian with the target label `cluster="hub"`. The label `cluster` of the
metrics `querdian_cluster_policy_*` therefore appears as `exported_cluster` in this demo.

### What the repository checks without a cluster

`go test ./hack/quickstart-multicluster/` renders the files with the same shell function as `up.sh` and
checks them without Docker:

* the manifests decode strictly into their Kubernetes and Querdian types;
* the gateway has one receiver and one ingest CA for each spoke, and each pipeline overwrites the cluster
  label with the name of its spoke;
* the hub collector and the hub Prometheus write `hub`, and the spoke collectors write a false claim;
* the hub release gives a static config that `config.Parse` accepts, with the cluster labels that the
  gateway writes; the spoke release installs only the agent;
* the policies compile under the hub config to the expected terms, and a suspended spoke gives
  `policy_source_expired` for its team only;
* the certs of `certs.sh` work with the TLS config of the receiver, the receiver finds the identity of
  each `SpokeCluster`, and each gateway port accepts only the ingest cert of its spoke.

## Differences from the single-cluster quickstart

* No backfilled history: the data starts when the clusters start.
* The hub Prometheus accepts OTLP writes (`--web.enable-otlp-receiver`) for the metrics of the spokes, and
  the spoke collectors scrape the workloads with the `prometheus` receiver. The metric names are the same.
* `verify.sh` does not drive the Grafana login flow and does not run the dashboard queries.
* The NetworkPolicy option of the single-cluster quickstart is not available.

## Troubleshooting

* On Linux, three kind clusters can reach the inotify limits of the host, and then a collector or a
  kubelet fails with "too many open files". Increase `fs.inotify.max_user_instances` and
  `fs.inotify.max_user_watches` as the kind
  [known issues](https://kind.sigs.k8s.io/docs/user/known-issues/#pod-errors-due-to-too-many-open-files)
  describe.

* `up.sh` keeps the clusters if it fails (`CLEANUP_ON_FAILURE=1` deletes them) and prints the pods that
  are not running in each cluster.
* A spoke does not sync: examine the agent log
  (`kubectl --context kind-querdian-spoke-a -n querdian logs deploy/querdian-agent`) and the receiver log
  on the hub. `403` while the spoke is suspended is correct. See
  [receiver HTTP statuses](troubleshooting.md#receiver-http-statuses).
* No spoke data: examine the collector log in the spoke and the gateway log on the hub
  (`kubectl --context kind-querdian-hub -n observability logs deploy/ingest-gateway`). A TLS handshake
  error there shows a wrong cert or CA.
* The address of the hub node is in the server certs. If Docker gives the hub node a new address (for
  example after a restart of Docker), run `down.sh` and `up.sh` again.
