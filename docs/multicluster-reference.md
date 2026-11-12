# Multi-cluster reference

This page gives the configuration of a hub and spoke deployment: the hub configuration, the
`SpokeCluster` and `SpokePolicy` kinds, the flags of `querdian receiver` and `querdian agent`, and the
metrics. Read [multi-cluster enforcement](multicluster.md) first. It gives the requirements and the
ordered steps.

In short:

* The hub needs the `multicluster` block and a `clusterLabel` on each backend. Querdian refuses to start
  if the configuration of the block or of a `clusterLabel` is not valid (see the table below).
* A hub admin writes one `SpokeCluster` for each spoke. Only the receiver writes the `spec` of `SpokePolicy` objects.
* The receiver runs on the hub and is the only component with a write path. The agent runs in each spoke.
* Alert on `querdian_cluster_policy_age_seconds` long before the time limit.

## Hub static configuration

```yaml
multicluster:
  hubCluster: mla              # cluster label value of hub telemetry
  maxStaleness: 48h            # default; time limit of spoke policies
backends:
  - name: metrics
    type: prometheus
    namespaceLabel: namespace
    clusterLabel: cluster      # required on every backend with multicluster
    # ...
  - name: logs
    type: loki
    namespaceLabel: k8s_namespace_name
    clusterLabel: cluster
    # ...
  - name: traces
    type: tempo
    namespaceLabel: resource.k8s.namespace.name
    clusterLabel: resource.k8s.cluster.name
    # ...
```

Querdian refuses to start in these conditions:

| Condition | Reason |
|---|---|
| `multicluster.hubCluster` is empty. | A matcher `cluster=""` would also accept data without the label. |
| `multicluster.maxStaleness` is zero or negative. | The time limit must be positive. If the key is absent, the default of 48 hours applies. |
| A backend has no `clusterLabel`. | The label is required on each backend with the block. |
| The `clusterLabel` name is not valid for the backend type. | Tempo permits `resource.<name>` only, because an application can set its own `span.` attributes. |
| The `clusterLabel` starts with `__`. | Names that start with `__` are reserved. |
| The `clusterLabel` is equal to `namespaceLabel`. | The two labels must be different. |
| A backend has `clusterLabel`, but the `multicluster` block is absent. | This stops an accidental rollback (see [rollback](multicluster.md#rollback)). |

With the block, the proxy also watches `SpokeCluster` and `SpokePolicy` objects. A replica reports
ready only after it has read all `SpokeCluster` objects one time. An unreachable spoke does not affect
startup or readiness. Without the block, the proxy starts no informers for these kinds. Thus their CRDs
can be absent, and each `SpokePolicy` grants nothing (`MulticlusterDisabled`).

The proxy needs this RBAC in addition to the single-cluster rules: `get`, `list`, `watch` on
`spokeclusters` and `spokepolicies`, and `update`, `patch` on `spokepolicies/status`.

## SpokeCluster

One cluster-scoped object (short name `spoke`) for each spoke. Only hub admins write its `spec`.

```yaml
apiVersion: policy.querdian.dev/v1alpha1
kind: SpokeCluster
metadata:
  name: cluster-a              # the cluster label value of this spoke, at most 63 characters
spec:
  identity: spiffe://mla.example.com/agent/1/cluster-a   # URI SAN of the agent cert
  tenants: [tenant-a]          # tenants that the policies of this spoke may use
  defaultTenant: tenant-a      # optional; for spoke namespaces without a tenant label
  maxStaleness: 168h           # optional; overrides multicluster.maxStaleness
  suspended: false             # true stops the grants of this spoke
```

| Field | Meaning |
|---|---|
| `spec.identity` | Required. The URI SAN of the agent cert, see [the agent certificate](multicluster.md#the-agent-certificate-and-its-ca). |
| `spec.tenants` | The tenant allow-list, see [tenant setups](multicluster.md#tenant-setups). A smaller list removes grants at once. |
| `spec.defaultTenant` | Must be in `tenants` (CEL). |
| `spec.maxStaleness` | Must be positive (CEL). |
| `spec.suspended` | Stops all grants of the spoke and makes the receiver reject its syncs. |
| `status.lastSyncTime` | Hub time of the last valid sync, written by the receiver. |
| `status.observedGeneration` | `metadata.generation` at the last valid sync. |

The policies of a spoke grant nothing (`SpokeClusterInvalid`) in these conditions:

* The `SpokeCluster` name is equal to `hubCluster`.
* The last path segment of `spec.identity` is not the name.

A change to the `spec` stops the grants of the spoke until two events occur: the next valid sync,
**and** the next snapshot rebuild of each replica. Until the sync, `observedGeneration` differs from
`metadata.generation`. A replica keeps the spoke expired until its snapshot was compiled from the new
generation. Thus a smaller tenant allow-list never applies late. If the spoke is offline, the grants
stay stopped until it returns.

## SpokePolicy

The hub copy of one spoke policy (cluster-scoped, short name `sp`). Only the receiver writes its
`spec`, and the Querdian leader writes its `status`. Do not create or edit these objects by hand.

* The object name is `sp-` and a SHA-256 hash of the cluster, the kind, the namespace and the name.
  Thus names of different clusters cannot collide. An object with any other name grants nothing
  (`InvalidSpokePolicy`).
* `spec.cluster` comes from the agent cert. The receiver sets the label `policy.querdian.dev/cluster`
  and an owner reference to the `SpokeCluster`. Thus a deletion of the `SpokeCluster` also removes its
  `SpokePolicy` objects.
* `status.conditions` has the `Ready` condition with the reasons in the
  [policy guide](policy-guide.md#status-conditions). `status.sourceGeneration` is the spoke generation
  that was compiled.

```sh
kubectl get sp     # Cluster, Kind, Namespace, Source (spoke name) and Ready
```

The tenant of a spoke `AccessPolicy` comes from the label `policy.querdian.dev/tenant` of its
namespace **in the spoke**, then from `defaultTenant`. Hub namespace labels never apply to a spoke
policy.

## querdian receiver

`querdian receiver` runs as its own Deployment on the hub. It reads the `SpokeCluster` objects and
the stored `SpokePolicy` objects directly from the API server, never from a cache.

| Flag | Default | Meaning |
|---|---|---|
| `--listen` | `:8443` | mTLS listener for agents. The only path is `POST /v1alpha1/sync`. |
| `--management-listen` | `:9090` | `/healthz` and `/readyz`. No `/metrics`. |
| `--tls-cert-file`, `--tls-key-file` | required | Server cert and key. |
| `--client-ca-file` | required | CA bundle for agent certs. Only the hub may issue from it. |
| `--leader-elect` | `true` | Lease `querdian-receiver-leader-election`. Only the leader serves syncs. With `false`, run only one replica: the lock for one sync for each cluster and the `429` limit work only within one process. |
| `--leader-election-namespace` | pod namespace | Namespace of the lease. |
| `--max-body-bytes` | `8388608` (8 MiB) | Maximum size of a sync body (`413`). |
| `--max-policies` | `2000` | Maximum number of policies in one full sync (`413`). It also sets the limit of JSON values in a body: 256 for each policy (`400`). |
| `--max-in-flight` | `8` | Maximum number of requests in progress (`503`). |
| `--log-level`, `--log-format` | `info`, `json` | As for the proxy. |

Limits on the requests of the agents:

| Limit | Answer |
|---|---|
| Each agent identity can send a burst of six requests, then one request each 5 seconds. The receiver applies this limit before it reads the body, also to identities that are not registered. | `429` |
| One sync for each cluster at a time. A second sync for a cluster while one runs gets `409`, and the agent retries. | `409` |
| One changed sync from one cluster in 10 seconds. | `429` |

An agent sends at most a heartbeat and a full sync each heartbeat interval (at least 10 seconds). Thus
the limit on the requests of one identity has no effect on a normal agent. The HTTP statuses of the receiver are in
[troubleshooting](troubleshooting.md#receiver-http-statuses).

* **Memory.** With the default limits, one request uses at most about 50 MiB: the body and the decoded
  values at the value limit. Set the memory limit of the receiver to at least 64 MiB, plus 50 MiB for
  each request of `--max-in-flight`. The chart default is 512 MiB for `--max-in-flight=8`. If you
  increase `--max-body-bytes`, `--max-policies` or `--max-in-flight`, increase the memory limit too.
* **Readiness.** `/readyz` returns 200 only on the leader. Thus a Service in front of the receiver
  sends traffic only to the leader. One replica is the default. A second replica gives a faster
  failover, but it is never ready, so `helm --wait` and `kubectl rollout status` do not finish. The
  receiver is not in the read path, and the time limit is long, so a short failover does no harm.
* **Write order.** A changed sync writes new and changed objects first, then deletes objects, and sets
  `lastSyncTime` last. The write of `lastSyncTime` has a `resourceVersion` precondition on the
  `SpokeCluster` that the receiver read at the start of the sync. A sync that overlaps a change of the `SpokeCluster`
  validates nothing.
* **RBAC.** `get`, `list` on `spokeclusters`; `update` on `spokeclusters/status`; `list`, `create`,
  `update`, `delete` on `spokepolicies`; and the `coordination.k8s.io` lease for leader election. Give
  the receiver no other write access. A compromised receiver can grant the data of all spokes
  ([threat model](security/threat-model.md#12-multi-cluster)).

## querdian agent

`querdian agent` runs in each spoke. Before each heartbeat, it lists all `ClusterAccessPolicy`,
`AccessPolicy` and `Namespace` objects directly from the spoke API server. If a list fails, it sends
nothing. Thus an agent that has lost its API server cannot renew old policies.

| Flag | Default | Meaning |
|---|---|---|
| `--receiver-url` | required | `https://host[:port][/prefix]` of the receiver. |
| `--tls-cert-file`, `--tls-key-file` | required | The agent cert and key. Read again before each request. |
| `--tls-ca-file` | required | CA certs that verify the receiver cert. Read again before each request. |
| `--heartbeat-interval` | `1m` | Time between two heartbeats, at least `10s`. |
| `--request-timeout` | `30s` | Time limit for one request to the receiver or to the spoke API server. One attempt (reads, sync and status writes) also has a time limit of two heartbeat intervals, from 1 to 5 minutes. |
| `--leader-elect` | `true` | One agent sends at a time. |
| `--leader-election-namespace` | pod namespace | Namespace of the lease. |
| `--leader-election-id` | `querdian-agent` | Name of the lease. |
| `--management-listen` | `:9090` | `/healthz`, `/readyz` (always 200) and `/metrics`. Empty disables it. |
| `--log-level`, `--log-format` | `info`, `json` | As for the proxy. |

* **Sync.** At each heartbeat, the agent sends the digest of its state. If the receiver has a
  different state, it answers `fullStateRequired`, and the agent sends the full state at once. The full
  state replaces the earlier state of the spoke. Thus a policy that is deleted in the spoke is deleted
  on the hub.
* **Retry.** After a failure, the agent retries at least each minute. After `409`, `429` and network
  errors, it retries with backoff. After other errors, it retries at the next heartbeat (at most one
  minute).
* **Status.** The agent writes the `Ready` condition and `status.observedGeneration` of each spoke
  policy from the sync reply. Thus spoke teams can use `kubectl get cap,ap`. The conditions show the
  last compile on the hub, so a new policy shows its state at the next heartbeat. The reply omits the
  hub-only information of `CrossTenantUnionUnsupported`: such a policy shows `Compiled`.
* **Proxy.** The agent uses `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` for the connection to the
  receiver. A proxy must pass TLS through, because the receiver checks the agent cert.
* **Leader election.** With `--leader-elect=false`, run only one agent replica in a spoke. Two agents
  with the same cert get `409` on the syncs of the other agent and send twice the requests.
* **Readiness.** `/readyz` does not depend on the hub. Thus a rollout of the agent does not stop while
  the hub is down. Use the metrics to find a broken agent (see below).
* **RBAC.** `get`, `list`, `watch` on `clusteraccesspolicies`, `accesspolicies` and `namespaces`;
  `update`, `patch` on `clusteraccesspolicies/status` and `accesspolicies/status`; `get`, `create`,
  `update` on `coordination.k8s.io` `leases` in its own namespace. No other permission and no
  wildcard on `status`.

## Metrics and alerts

The proxy exports these metrics on its management port, for each `SpokeCluster` with a valid name and
identity:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `querdian_cluster_policy_age_seconds` | gauge | `cluster` | Time since the last valid sync. `+Inf` if the spoke has never synced, negative if `lastSyncTime` is in the future. |
| `querdian_cluster_policy_expired` | gauge | `cluster` | `1` if the policies of the spoke are expired or suspended, else `0`. The age alone does not show a suspended spoke, or a spoke whose generation is not synced. |
| `querdian_policies{kind="SpokePolicy"}` | gauge | `kind`, `ready` | `SpokePolicy` objects by compile result (only with the block). |

A `SpokeCluster` with the name of `hubCluster`, or whose identity does not end with its name, has **no
series** in the two `querdian_cluster_policy_*` metrics. The `Ready` condition of its `SpokePolicy`
objects (`SpokeClusterInvalid`) shows the cause.

The agent exports these metrics on its management port:

| Metric | Meaning |
|---|---|
| `querdian_agent_syncs_total{result}` | Counts the sync attempts: `ok`, `full_state_sent`, `retry`, `error`. |
| `querdian_agent_last_sync_timestamp_seconds` | The Unix time of the last sync that the receiver accepted. |

Alert long before the time limit, for example at 24 hours with the default of 48 hours:

```promql
querdian_cluster_policy_age_seconds > 86400
querdian_cluster_policy_expired == 1
```

The Helm chart can install an alert at 24 hours; see the [chart README](../charts/querdian/README.md).
In each spoke, alert if `time() - querdian_agent_last_sync_timestamp_seconds` is above a few heartbeat
intervals.
