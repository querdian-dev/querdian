# Multi-cluster quickstart reference

This page gives the details of the [multi-cluster quickstart](quickstart-multicluster.md) demo. Read the
quickstart first.

In short:

* The spokes reach the hub through NodePorts on the Docker network `kind`. The hub does not connect to
  the spokes.
* The agent certs and the ingest certs have different CAs and different identities.
* The demo ingest gateway overwrites the cluster label. The spoke collectors send false cluster labels
  on purpose, and the checks prove that the gateway corrects them.
* `up.sh` follows the [migration steps](multicluster.md#migration-ordered-steps) in their order.

## Options

Environment variables:

| Variable | Default | Effect |
|---|---|---|
| `CLUSTER_PREFIX` | `querdian` | Prefix of the cluster names: `querdian-hub`, `querdian-spoke-a`, `querdian-spoke-b`. |
| `MC_WORK_DIR` | `$XDG_CACHE_HOME/querdian-quickstart-multicluster`, or `~/.cache/querdian-quickstart-multicluster` if `XDG_CACHE_HOME` is not set | Work directory for the certs. Use a new or empty directory, because `down.sh` deletes it. `up.sh` refuses a directory with other files. `down.sh` deletes only a directory that `up.sh` marked (file `.querdian-quickstart-multicluster`). It never deletes `/` or `$HOME`. |
| `KEYCLOAK_PORT` | `8080` | Host port of Keycloak. |
| `DEMO_HISTORY_HOURS` | `24` | Hours of demo history for each cluster, 1 to 720. `verify.sh` gets the value from the retention of the hub Prometheus. |
| `IMAGE_TAG` | `quickstart` | Tag of the Querdian image. |
| `SKIP_BUILD=1` | off | `up.sh` does not build the Querdian image. It uses the image with `IMAGE_TAG`. |
| `CLEANUP_ON_FAILURE=1` | off | `up.sh` deletes the clusters if it fails. |
| `KEEP_IMAGE=1` | off | `down.sh` keeps the Querdian image. |

## Resources and run time

These values are **estimates**. They are not measured. They come from the measured values of the
single-cluster quickstart and from the size of a kind node.

| Item | Estimate |
|---|---|
| Memory in use | about 4 to 4.5 GiB in total |
| Memory of the hub | about 2.5 to 3 GiB: the single-cluster stack with the history of the three clusters, the gateway and the receiver. This value is not measured with the history. |
| Time for the history | Hub: no added time, because it runs while Keycloak starts. Spokes: about 1 minute for each spoke, in the background during steps 5 and 6. |
| Memory of each spoke | about 0.7 to 0.9 GiB: a kind control plane, one collector, the agent and two busybox workloads |
| Container VM | **8 GiB** of memory and 4 CPUs or more |
| Disk | about 6 GiB for the three node containers with their images, and more for the demo images that `up.sh` keeps on the host. |
| `up.sh` | about 8 to 12 minutes with a warm image cache; 15 to 20 minutes on a first run (Go build, node image and image pulls). The first pull of the demo images and Keycloak are the slowest parts. `up.sh` pulls the images on the host while the clusters start, and shows the progress of each pull. Later runs use the local images. |
| `verify.sh` | about 1 to 2 minutes. Most of the time is the wait for the access of the suspended spoke to return. |

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

The kind node containers share the Docker network `kind`. The spokes reach the hub through NodePorts,
at the address of the hub node on this network (`up.sh` prints it). The spokes need only this outbound
path. The hub does not connect to the spokes.

| Port | Where | What |
|---|---|---|
| 30443 | hub node | receiver (`querdian receiver`, mTLS, `POST /v1alpha1/sync`). The chart value `receiver.service.nodePort` keeps the port fixed. |
| 31001 | hub node | ingest gateway, port of spoke-a (mTLS OTLP/HTTP) |
| 31002 | hub node | ingest gateway, port of spoke-b (mTLS OTLP/HTTP) |
| 8080, 3000, 8081 | laptop, `127.0.0.1` only | Keycloak, Grafana, policy dashboard (kind port mappings to the NodePorts 30080, 30030, 30081) |

Each cluster has its own pod and Service ranges (hub `10.244.0.0/16`, spoke-a `10.245.0.0/16`, spoke-b
`10.246.0.0/16`). Thus the addresses in the data of different clusters do not overlap.

### Certificates

`hack/quickstart-multicluster/certs.sh` makes the certs with openssl in `$MC_WORK_DIR/certs` (EC
P-256, valid for 30 days). At the end, it verifies the chains and the separation with `openssl verify`.

| Files | Issued by | Use |
|---|---|---|
| `hub-server-ca.crt` | itself | CA of the two server certs; the spokes trust it |
| `receiver.crt` | hub-server-ca | server cert of the receiver, IP SAN of the hub node |
| `gateway.crt` | hub-server-ca | server cert of the ingest gateway, IP SAN of the hub node |
| `agent-ca.crt` | itself | the **hub-controlled agent CA**, `receiver.clientCA` |
| `agent-spoke-a.crt`, `agent-spoke-b.crt` | agent-ca | agent certs, one URI SAN `spiffe://quickstart.local/agent/1/<spoke>` |
| `ingest-ca-spoke-a.crt`, `ingest-ca-spoke-b.crt` | itself | a **separate ingest CA for each spoke** |
| `ingest-spoke-a.crt`, `ingest-spoke-b.crt` | the ingest CA of the spoke | ingest client certs, URI SAN `spiffe://quickstart.local/ingest/<spoke>` |

The agent cert is not the ingest cert. The two have different CAs and different identities.

* The receiver trusts only the agent CA, so it refuses an ingest cert.
* On the port of a spoke, the gateway trusts only the ingest CA of that spoke. Thus it refuses an agent
  cert and the ingest cert of the other spoke.
* The last path segment of the agent identity is the `SpokeCluster` name. The middle segment `1` is the
  cert generation (see [the agent certificate](multicluster.md#the-agent-certificate-and-its-ca)).

In a real deployment, only the hub holds the agent CA key. In this demo, all keys are on the laptop.

## The ingest gateway (a demo pattern)

Querdian cannot verify the cluster label, and it does not include an ingest gateway. Isolation between
the spokes depends on a gateway that sets the label from the credential of the spoke. The gateway must
**overwrite** each value that the spoke sends
([cluster label integrity](multicluster.md#cluster-label-integrity-the-ingest-gateway)). The demo uses
an OpenTelemetry Collector for this job (`hack/quickstart-multicluster/manifests/ingest-gateway.yaml`):

* One OTLP/HTTP receiver for each spoke, on its own port. Each receiver requires a client cert and
  trusts only the ingest CA of its spoke. The hub controls the ingest CAs, so the port and its CA
  identify the spoke.
* Each pipeline has one receiver and the processors of its spoke, for metrics, logs and traces. The
  processors use `upsert`, never `insert`:
  * The resource attributes `k8s.cluster.name` and `cluster` get the name of the spoke. Loki indexes
    `k8s.cluster.name` as the label `k8s_cluster_name`. Tempo keeps it as `resource.k8s.cluster.name`.
  * The attribute `cluster` of each data point, log record and span gets the name of the spoke (the
    Prometheus label `cluster`).
  * The processors delete the attribute `k8s.cluster.name` of data points, log records and spans.
  * The metrics pipelines also delete the resource attribute `k8s.cluster.name` after the upsert.
    Prometheus copies the resource attributes to the series `target_info`, which then has only the label
    `cluster`. Thus no second label with a value of the spoke exists.
* The gateway writes the metrics to the OTLP receiver of Prometheus. It writes the logs and traces to
  the OTLP endpoints of Loki and Tempo, with the tenant `demo`.

The collectors of the spokes are **hostile on purpose**. The collector of spoke-a writes the cluster
label `hub` on all of its data, and the collector of spoke-b writes `spoke-a`. `verify.sh` checks that
each team sees only pods of its own cluster. Thus a gateway that keeps the claim fails the checks.

### Limits of this design

| Limit | Explanation |
|---|---|
| The gateway checks the CA, not the URI SAN of the client cert. | This is correct only because each spoke has its own CA, and only the hub can issue certs from it. |
| There is no certificate revocation. | To remove a spoke, remove its port and its CA from the gateway ([remove a spoke](multicluster.md#remove-a-spoke)). |
| One port and one CA for each spoke does not scale to many spokes. | A production gateway needs a map from the cert identity to the cluster name. |
| The processors know which attributes the backends map to the cluster labels of this demo. | This applies to Prometheus (OTLP receiver), Loki (default `otlp_config`) and Tempo. A different map of attributes, for example more Loki index labels, needs a new review of the processors. |
| The backends have no write authentication. | A pod on the hub can write data with the cluster label of a spoke and bypass the gateway. A real hub restricts the write paths, for example with NetworkPolicies. |
| A hostile spoke can get access to Keycloak, Grafana and the dashboard of the hub. | The kind port mappings bind them to `127.0.0.1` on the laptop. But their NodePorts on the hub node (Keycloak 30080, Grafana 30030 and the dashboard 30081) are also open to each container on the Docker network `kind`, thus also to the pods of the spokes. Keycloak and Grafana use the known admin passwords of the demo. The dashboard has no authentication (as in the single-cluster quickstart). Do not use this setup with spokes that you do not trust. |

## The ordered setup

`up.sh` follows the [migration steps](multicluster.md#migration-ordered-steps) in their order:

1. **Steps 0, 1 and 3.** The hub is new: it has no old policies and no old data. The chart installs the
   CRDs and the `multicluster` block in one release (`hack/quickstart/querdian-values.yaml`, then
   `hack/quickstart-multicluster/hub-values.yaml`). The hub `ClusterAccessPolicy` objects state their
   cluster scope. `up.sh` waits until all hub policies are `Ready`, so none is `InvalidClusterScope`.
2. **Step 2.** Before Querdian and before a spoke exists, `up.sh` makes sure that hub telemetry carries
   `cluster="hub"`. Prometheus writes the target label `cluster="hub"` at scrape time. The hub
   collector writes `k8s.cluster.name="hub"` with `upsert`. The direct queries of step 2 must find no
   data without the label: the PromQL and LogQL queries return nothing. For traces, the guide compares
   two TraceQL metrics counts, but the demo Tempo has no metrics generator. Thus `up.sh` searches for
   spans with `resource.k8s.cluster.name = nil` and with a different value. Both searches must find
   nothing.
3. **Step 4.** The chart installs the receiver with the hub. `up.sh` registers each spoke as a
   `SpokeCluster` with `suspended: true`, then installs the agent in the spoke. The receiver rejects each
   sync of a suspended spoke, so `LastSync` stays empty. `up.sh` checks this.
4. **Step 5.** The gateway is ready before a spoke connects. The spoke collectors start only now.
   `up.sh` then runs `forged-label-test.sh` for each spoke while it is still suspended (see below). If a
   test fails, `up.sh` stops and the spoke stays suspended.
5. **Step 6.** Only after its test passes, `up.sh` sets `suspended: false`. It waits for the first valid
   sync (`lastSyncTime` and `observedGeneration`). It also waits for the `Ready` condition that the
   agent writes on the spoke `AccessPolicy`.

### The forged-label test

`hack/quickstart-multicluster/forged-label-test.sh <spoke>` sends one sample, one log line and one span
for each other cluster name: the other spoke and `hub`. It sends them from the node container of the
spoke, with the ingest cert of that spoke. Each item claims that name in its resource attributes and in
its own attributes. The script then queries Prometheus, Loki and Tempo **directly** (port-forwards on
the hub, not through Querdian):

* Each stored sample, log line and span must carry the cluster of the cert (Prometheus `cluster`, Loki
  `k8s_cluster_name` and the structured metadata `cluster`, Tempo `resource.k8s.cluster.name` and
  `resource.cluster`).
* No item may carry the attribute `k8s.cluster.name` on its data point or span.

The script also checks that the TLS handshake fails in these cases:

* the ingest cert on the port of the other spoke;
* the agent cert on the gateway;
* a client without a cert;
* the ingest cert on the receiver.

You can run the script again at any time.

## The demo history

Each cluster gets `DEMO_HISTORY_HOURS` (default 24) of demo data, so that the dashboards are not empty.
The generator is the generator of the single-cluster quickstart (`hack/quickstart/demogen`, see
[Backfill](quickstart-details.md#backfill)). With `-cluster <name>`, it adds:

* the label `cluster="<name>"` on each metric series;
* the resource attributes `k8s.cluster.name` and `cluster` on each log line and span, as the hub
  collector and the gateway set them;
* pod names of that cluster: `<service>-history-<name>`, for example `order-service-history-spoke-a`.

Each cluster has its own seed, so the clusters do not show the same values. The incidents of the
generator are at the same times in all clusters.

| Cluster | Metrics | Logs and traces | When |
|---|---|---|---|
| Hub | The init container `backfill` of Prometheus writes the blocks before Prometheus starts. | The Job `history-hub`. | Before the hub collector sends data, while Keycloak starts. |
| Each spoke | The sidecar `history` of Prometheus writes the blocks into the running Prometheus. Prometheus loads new blocks each minute, and it does not change its head for them. | The Job `history-<spoke>`. | After the check of step 2, in the background during steps 5 and 6, one spoke after the other. `up.sh` waits for it before the end. |

The design follows these rules:

* **Step 2.** The check of migration step 2 accepts only data with `cluster="hub"`. Thus the history of
  the spokes goes in only after that check.
* **No double count.** The history of each cluster ends (`-end`) before the live data of that cluster
  starts. The dashboards add the series of a namespace, so an overlap would count the same time two
  times.
* **Separate streams.** No real pod has a history pod name. Thus the history and the live logs are in
  different Loki streams, and Loki accepts the old entries. Loki rejects an entry that is more than about
  one hour older than the newest entry of its stream. `verify.sh` adds the history pods of each cluster to
  the pods of that cluster (`history_pod` in `lib.sh`). A history pod of one cluster in the result of
  another team still fails the check.
* **Not through the gateway.** The Jobs and the sidecar write directly to the backends of the hub. The
  history of a spoke stands for data that the spoke sent before. It does not test the gateway.
* **Live checks.** The history has the correct cluster labels, so it can satisfy a check that waits for
  live data. Thus the positive checks of step 2, `verify.sh --smoke` and the live sections of
  `verify.sh` exclude the history pods (`HISTORY_POD_RE` in `lib.sh`). The forged-label test is not
  affected, because it uses test IDs.

Limits:

* If the Prometheus container restarts, Prometheus truncates its head to the end of the newest block.
  The live data between the start of the hub and the end of the last spoke history (a few minutes) is
  then lost.
* A new Prometheus pod (for example after `kubectl delete pod`) has only the history of the hub, because
  the data volume is an `emptyDir`.

## Policies

| User | Group | Policy | Sees |
|---|---|---|---|
| alice | `team-a` | `AccessPolicy team-a` in `order-service` of **spoke-a** | `order-service` of spoke-a |
| bob | `team-b` | `AccessPolicy team-b` in `order-service` of **spoke-b** | `order-service` of spoke-b |
| hana | `team-hub` | hub `AccessPolicy team-hub` in `order-service` | `order-service` of the hub |
| sam | `sre` | hub `ClusterAccessPolicy sre`, a fixed cluster set `spoke-a\|spoke-b` | all namespaces of both spokes, not the hub |
| carol | `platform-admins` | hub `ClusterAccessPolicy platform-admins`, `unrestricted` | everything in tenant `demo` |

The realm is the realm of the single-cluster quickstart with these users and groups
(`hack/quickstart-multicluster/realm-users.json`, merged by `up.sh`).

* The spoke teams write their `AccessPolicy` in their own cluster
  (`hack/quickstart-multicluster/manifests/spoke-policies.yaml`). The agent sends it to the hub. The
  hub compiles it to `namespace="order-service"` AND `cluster="<spoke>"`. The cluster comes from the
  agent cert, never from the policy text.
* The hub `AccessPolicy` gets `cluster="hub"`. Thus team-hub does not see the `order-service`
  namespaces of the spokes, which have the same name.
* The SRE policy names a fixed set of clusters for each backend, because the cluster label has a
  different name on each backend. A third spoke would not be in this grant. `allClusters: true` would
  include it ([step 1](multicluster.md#1-review-each-hub-clusteraccesspolicy)).
* Hub policies never expire. While a spoke is suspended, sam and carol still see its data.

## Automatic verification

`hack/quickstart-multicluster/verify.sh` gets tokens for the five users from Keycloak with the password
grant (demo only). It queries the Querdian proxy of the hub directly (`kubectl port-forward`). It exits
with a non-zero status on any failure.

| Area | Checks |
|---|---|
| Isolation on all three backends | alice, bob and hana each see only `order-service` of their own cluster in PromQL, LogQL and TraceQL search results, and in the label values of the cluster label. Explicit queries for another cluster return nothing. The pods in their results are pods of their own cluster, from the API server of that cluster. Trace by ID of a trace of spoke-a gives `404` to bob and hana, and `200` to sam. |
| SRE and admin | sam sees both spokes and not the hub. carol sees all three clusters. For each cluster, the data with its cluster label comes from pods of that cluster. |
| State | Each `SpokeCluster` has a recent `lastSyncTime` and an `observedGeneration` equal to its generation. Each spoke has one `SpokePolicy` that is `Ready`. The agent wrote `Ready` on the spoke `AccessPolicy`. No proxy runs in a spoke. The dashboard API shows the origin of each policy. |
| Suspend and unsuspend | With spoke-a suspended, alice gets `403` on all three backends. The dashboard API reports `policy_source_expired` and the expired state. bob and sam keep their access. The metric `querdian_cluster_policy_expired` is 1 for spoke-a. After the unsuspend, the access of alice returns. If the script stops early, it unsuspends spoke-a. |
| Live data | The checks that prove that data arrives count only the data of real pods. They exclude the history pods, so the history alone cannot pass them. The isolation checks also cover the history. |
| History | At a time 10 minutes after the start of the history, carol finds metrics, logs and traces of all three clusters, and the metrics of each cluster come from pods of that cluster. alice, bob and hana see only `order-service` of their own cluster in the history too. |
| Grafana | Health and the three data sources. |

`verify.sh --smoke` only checks that the data of all three clusters arrives and that the spoke grants are
active. `up.sh` uses it at the end.

The hub Prometheus scrapes Querdian with the target label `cluster="hub"`. Thus the label `cluster` of
the metrics `querdian_cluster_policy_*` shows as `exported_cluster` in this demo.

### What the repository checks without a cluster

`go test ./hack/quickstart-multicluster/` renders the files with the same shell function as `up.sh`. It
checks them without Docker:

* The manifests decode strictly into their Kubernetes and Querdian types.
* The gateway has one receiver and one ingest CA for each spoke, and each pipeline overwrites the cluster
  label with the name of its spoke.
* The hub collector and the hub Prometheus write `hub`, and the spoke collectors write a false claim.
* The hub release gives a static config that `config.Parse` accepts, with the cluster labels that the
  gateway writes. The spoke release installs only the agent.
* The policies compile under the hub config to the expected terms. A suspended spoke gives
  `policy_source_expired` for its team only.
* The certs of `certs.sh` work with the TLS config of the receiver, and the receiver finds the identity
  of each `SpokeCluster`. Each gateway port accepts only the ingest cert of its spoke.

## Differences from the single-cluster quickstart

* The history of each cluster has the cluster label and pod names of that cluster (see
  [The demo history](#the-demo-history)). The history of the spokes goes in after migration step 2.
* The hub Prometheus accepts OTLP writes (`--web.enable-otlp-receiver`) for the metrics of the spokes.
  The spoke collectors scrape the workloads with the `prometheus` receiver. The metric names are the
  same.
* `verify.sh` does not do the Grafana login flow, and it does not run the dashboard queries.
* The NetworkPolicy option of the single-cluster quickstart is not available.
