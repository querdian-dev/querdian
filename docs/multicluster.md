# Multi-cluster enforcement

This guide is for operators who run one central observability cluster (the **hub**) that stores the
metrics, logs and traces of several other clusters (the **spokes**). One Querdian on the hub enforces
the policies of all clusters. It documents the behaviour of the code in this repository; the design
is [PLAN.md section 18](../PLAN.md#18-multi-cluster). Read the [policy guide](policy-guide.md) and
[operations](operations.md) first. Related: [limitations](limitations.md#multi-cluster),
[threat model](security/threat-model.md#12-multi-cluster),
[troubleshooting](troubleshooting.md#receiver-http-statuses).

> **The feature is new and alpha.** Without the `multicluster` block in the static configuration,
> Querdian works as a single-cluster proxy and nothing on this page applies.

Contents: [how it works](#how-it-works) / [requirements](#requirements) /
[migration](#migration-ordered-steps) / [configuration](#configuration-reference) /
[tenant setups](#tenant-setups) / [day-2 operations](#day-2-operations)

## How it works

```
spoke: [ querdian agent ] --mTLS: digest, or full state--> hub: [ querdian receiver ] --> SpokePolicy, SpokeCluster.status
          ^ reads ClusterAccessPolicy, AccessPolicy, Namespace                                   | informers
          | from the spoke API server                                                            v
                                                                    Grafana --> [ querdian (proxy) ] --> backends
```

Three processes from the same image take part:

* **`querdian`** (the proxy) on the hub, with the `multicluster` block. It compiles the hub policies and
  the `SpokePolicy` objects into one snapshot and enforces them. It also keeps a table of spoke
  deadlines, outside the snapshot, that it checks on each request.
* **`querdian receiver`** on the hub, as a separate Deployment. It is the only component with a write
  path. It accepts syncs from agents over mTLS, stores each spoke policy as a cluster-scoped
  `SpokePolicy` object and writes `SpokeCluster.status.lastSyncTime`.
* **`querdian agent`** in each spoke. It reads the policies and namespaces of its spoke from the spoke
  API server and sends them to the receiver. It needs only an outbound path to the receiver.

Querdian takes the cluster of a policy from its **origin**, never from the policy text, in the same way
as the namespace of an `AccessPolicy`. The policies compile to these scopes:

| Policy source | Author | Compiled scope |
|---|---|---|
| Hub `ClusterAccessPolicy` | hub platform admin | its own matchers; it must state its cluster scope (see [step 1](#1-review-each-hub-clusteraccesspolicy)) |
| Hub `AccessPolicy` | hub team | `<namespaceLabel>="X"`, `<clusterLabel>="<hubCluster>"`, plus its matchers |
| Spoke `ClusterAccessPolicy` | spoke admin | `<clusterLabel>="<spoke>"`, plus its matchers |
| Spoke `AccessPolicy` | spoke team | `<namespaceLabel>="X"`, `<clusterLabel>="<spoke>"`, plus its matchers |

* A spoke rule with `unrestricted: true` compiles to the single matcher `<clusterLabel>="<spoke>"`. A
  spoke rule never becomes unrestricted, so the queries of a spoke admin are always rewritten.
* A spoke matcher on the cluster label stays ANDed with the implicit matcher. It can only narrow.
* A spoke admin can grant the data of that spoke to any user or group. All clusters use the same
  OIDC issuer, so group names mean the same thing everywhere.

**Time limit.** Hub policies never expire. The policies of a spoke stop when the spoke has not synced
for `maxStaleness` (default 48 hours), and the next valid sync restores them with no manual work. The
agent sends a heartbeat each minute by default, so a revoked spoke policy normally stops within one
heartbeat. See [time limit and outages](#time-limit-and-outages).

## Requirements

### Cluster label integrity: the ingest gateway

The cluster label is to the spokes what the namespace label is to the teams
([threat model, section 1](security/threat-model.md#1-namespace-label-integrity-the-assumption-everything-rests-on)).
Querdian cannot verify any of the requirements below. A startup check cannot help either: Querdian
does not query backends at startup, and an empty result does not prove a wrong configuration.

* Each spoke sends telemetry with its own credential (normally an mTLS client cert). The ingest
  gateway on the hub sets the cluster label **from that credential**.
* The gateway must **overwrite** the label, not add it, for all three signals: metrics (remote write),
  logs (Loki push) and traces (OTLP). A spoke that sends `cluster="cluster-b"` in its own data must be
  stored as `cluster="cluster-a"`. An "add if missing" rule (for example an OpenTelemetry
  `resource` processor with `action: insert`, or a relabel rule that keeps an existing value) is not
  sufficient; use `upsert` or replace the label unconditionally.
* Writers on the hub that bypass the gateway must not be able to set the label of a spoke. This
  includes ruler output (Prometheus, Mimir and Loki recording rules) and hub agents.
* The `SpokeCluster` name must be equal to the label value that the gateway writes. Cluster names must
  not change: a renamed cluster loses its policies, and its old data keeps the old label.

To test the overwrite, send one sample, one log line and one span from a spoke with a forged cluster
label, then query the backend directly. The stored value must be the cluster of the credential.

### Hub telemetry

Hub telemetry must carry `<clusterLabel>="<hubCluster>"`, also ruler output and the data of hub
agents. Each hub `AccessPolicy` gets the implicit matcher `<clusterLabel>="<hubCluster>"`, so hub data
without the label is invisible to hub teams, and they get no error. Step 2 of the migration gives a
verification query for each signal.

### The agent certificate and its CA

The receiver terminates mTLS itself and takes the cluster from the client cert only. The sync
protocol has no cluster field.

* **A hub-controlled CA.** `querdian receiver --client-ca-file` must contain only a CA that the hub
  controls. No spoke may be able to issue certs from it: a spoke admin with access to the CA key can
  issue a cert for a different spoke. Do not use a cert-manager issuer whose CA key is stored in a
  spoke. Issue the cert on the hub and copy it to the spoke, or use a signing process that the hub
  approves.
* **A dedicated agent cert.** The agent cert must not be the ingest cert, and its identity must not
  occur on an ingest cert. Otherwise the theft of a write-only telemetry credential also gives the
  power to grant read access. Use a separate CA for agent certs, so that the receiver does not accept
  ingest certs at all. The `SpokeCluster` object, which only a hub admin can write, is the link between
  the agent identity and the cluster name.
* **Exactly one URI SAN.** The identity is the URI SAN of the client cert. The receiver rejects a cert
  with no URI SAN or with several URI SANs. Other SANs (DNS names, the subject) are ignored.
* **The last path segment is the cluster name.** The URI SAN must be equal to `spec.identity` of the
  `SpokeCluster`, and the last path segment of `spec.identity` must be equal to the `SpokeCluster`
  name. A suffix does not count: `spiffe://mla.example.com/agent/x-cluster-a` does not match
  `cluster-a`. If the segment differs, the policies of that spoke grant nothing
  (`SpokeClusterInvalid`), and the receiver rejects its syncs.
* **Canonical form.** The identity must be an absolute URI with a scheme, a host and a path, in the
  form that Go's `url.URL.String()` gives back (for example a lower-case scheme). It must not contain
  `%`, `?`, `#`, `@` or `\`. A non-canonical identity never matches.

A recommended format is `spiffe://<trust-domain>/agent/<generation>/<cluster>`, for example
`spiffe://mla.example.com/agent/1/cluster-a`. The middle segment lets you issue a cert with a new
identity for the same cluster after a theft (see [stolen agent cert](#stolen-agent-cert)).

The receiver accepts TLS 1.2 or later; the agent connects with TLS 1.3 only. The agent verifies the
receiver cert with `--tls-ca-file`. A load balancer between agent and receiver must pass TLS through:
if it terminates TLS, the client cert does not reach the receiver.

## Migration: ordered steps

Do these steps in this order. A later step does not repair a mistake in an earlier step.

### 1. Review each hub ClusterAccessPolicy

Under the `multicluster` block, each rule of a hub `ClusterAccessPolicy` must state its cluster scope,
because a new spoke must not silently widen a grant. For **each backend** of the rule, one of these
must be true, otherwise the policy is `InvalidClusterScope` and grants nothing:

* The rule has a matcher on the `clusterLabel` of that backend that names a **fixed set of non-empty
  cluster names**: an `Equal`, or a `RegexMatch` such as `mla|cluster-a` or `cluster-(a|b)`. Not
  accepted: `NotEqual`, `RegexNoMatch`, an open regex such as `.+` or `cluster-.*` (each also matches
  each new spoke), an empty value (it matches data without a cluster label), case-insensitive regexes
  and regexes with more than 1024 names.
* The rule sets `allClusters: true` and has no matcher on the `clusterLabel` of that backend.
* The rule sets `unrestricted: true`. An unrestricted rule covers all clusters, including spokes that
  you add later. `allClusters` and `unrestricted` must not be set together.

```yaml
apiVersion: policy.querdian.dev/v1alpha1
kind: ClusterAccessPolicy
metadata:
  name: sre-shared-infra
spec:
  subjects:
    groups: [sre]
  rules:
    - backends: [metrics]
      matchers:
        - {name: cluster, op: RegexMatch, value: "mla|cluster-a"}   # a fixed set of clusters
        - {name: namespace, op: RegexMatch, value: "kube-system|ingress-nginx"}
    - backends: [logs]
      tenants: [tenant1]
      allClusters: true                                              # all clusters, also new ones
      matchers:
        - {name: k8s_namespace_name, op: Equal, value: kube-system}
```

Without the `multicluster` block, `allClusters` has no effect and the policy stays valid. You can
therefore add `allClusters: true` or cluster matchers to the hub policies before you set the block.
Find the policies to change with:

```sh
kubectl get cap -o name
```

A cluster matcher that is correct today can become too narrow later: `mla|cluster-a` does not grant
`cluster-b`. Decide for each rule whether a new spoke must be included (`allClusters`) or not (a fixed
set).

### 2. Make sure that hub telemetry carries the hub cluster label

Configure the hub's collection pipeline and the ruler so that all hub data carries
`<clusterLabel>="<hubCluster>"`. Then check each signal directly against the backend (or through
Querdian as an unrestricted user), for each tenant on a multi-tenant backend. The examples use
`hubCluster: mla`, `clusterLabel: cluster` on Prometheus and Loki, and
`clusterLabel: resource.k8s.cluster.name` on Tempo. Before the first spoke sends data, each query must
return no data (Prometheus, Loki) or two equal values (Tempo).

* **Metrics** (PromQL). `!=` also matches series without the label. This includes recording-rule
  output:

  ```promql
  count by (__name__) ({__name__=~".+", cluster!="mla"})
  ```

* **Logs** (LogQL). A stream selector needs a matcher that does not match the empty string; use a
  label that each stream has, such as the namespace label or `service_name`:

  ```logql
  sum by (k8s_namespace_name) (count_over_time({k8s_namespace_name=~".+", cluster!="mla"}[1h]))
  ```

  Repeat with each label that your log pipeline always sets, so that streams without the namespace
  label are also checked.

* **Traces** (TraceQL metrics). A span without the attribute matches neither `=` nor `!=`, so compare
  two counts. They must be equal:

  ```traceql
  {} | count_over_time()
  { resource.k8s.cluster.name = "mla" } | count_over_time()
  ```

Old hub data, from before this step, has no cluster label. Hub `AccessPolicy` users lose access to it
until it leaves retention (see [limitations](limitations.md#multi-cluster)).

### 3. Set the multicluster block and clusterLabel

Add the block and a `clusterLabel` on each backend to the hub configuration (see
[hub configuration](#hub-static-configuration)), apply the new CRDs, and restart the proxy. Then
check that no hub policy has become invalid:

```sh
kubectl get cap,ap -A    # look for InvalidClusterScope
```

From now on each hub `AccessPolicy` also carries `<clusterLabel>="<hubCluster>"`.

### 4. Install the receiver and register the spokes

Deploy `querdian receiver` on the hub (see [receiver](#querdian-receiver)) and make it reachable from
the spokes. For each spoke, issue an agent cert, create a `SpokeCluster` object and install
`querdian agent` in the spoke (see [agent](#querdian-agent)). Check that the spoke syncs:

```sh
kubectl get spokeclusters       # LastSync must be recent
kubectl get spokepolicies       # one object for each policy of each spoke
```

### 5. Let the spokes send telemetry

Connect a spoke to the ingest gateway only after step 3. If spoke data arrives before the block is
set, each hub `AccessPolicy` for namespace `X` also matches namespace `X` of that spoke. No check can
find this condition.

## Configuration reference

### Hub static configuration

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

Querdian refuses to start if:

* `multicluster.hubCluster` is empty (a matcher `cluster=""` would also accept data without the
  label);
* `multicluster.maxStaleness` is zero or negative (if it is absent, the default of 48 hours applies);
* a backend has no `clusterLabel`, the name is not valid for the backend type (Tempo: `resource.<name>`
  or `span.<name>`), it starts with `__`, or it is equal to `namespaceLabel`;
* a backend has `clusterLabel` but the `multicluster` block is absent. This stops an accidental
  rollback (see [rollback](#rollback)).

With the block, the proxy also watches `SpokeCluster` and `SpokePolicy` objects. A replica reports
ready only after it has read all `SpokeCluster` objects once. An unreachable spoke does not affect
startup or readiness. Without the block, the proxy starts no informers for these kinds, so their CRDs
can be absent, and each `SpokePolicy` grants nothing (`MulticlusterDisabled`).

The proxy needs this RBAC in addition to the single-cluster rules: `get`, `list`, `watch` on
`spokeclusters` and `spokepolicies`, and `update`, `patch` on `spokepolicies/status`.

### SpokeCluster

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
| `spec.identity` | Required. The URI SAN of the agent cert, see [the agent certificate](#the-agent-certificate-and-its-ca). |
| `spec.tenants` | The tenant allow-list, see [tenant setups](#tenant-setups). A smaller list removes grants at once. |
| `spec.defaultTenant` | Must be in `tenants` (CEL). |
| `spec.maxStaleness` | Must be positive (CEL). |
| `spec.suspended` | Stops all grants of the spoke and makes the receiver reject its syncs. |
| `status.lastSyncTime` | Hub time of the last valid sync, written by the receiver. |
| `status.observedGeneration` | `metadata.generation` at the last valid sync. |

The policies of a spoke grant nothing (`SpokeClusterInvalid`) if the `SpokeCluster` name is equal to
`hubCluster`, or if the last path segment of `spec.identity` is not the name. A change to the `spec`
stops the grants of the spoke until the next valid sync **and** the next snapshot rebuild of each
replica: `observedGeneration` differs from `metadata.generation` until the sync, and a replica keeps the
spoke expired until its snapshot was compiled from the new generation. So a smaller tenant allow-list
never applies late. If the spoke is offline, the grants stay stopped until it returns.

### SpokePolicy

The hub copy of one spoke policy (cluster-scoped, short name `sp`). Only the receiver writes its
`spec`, and the Querdian leader writes its `status`. Do not create or edit these objects by hand.

* The object name is `sp-` and a SHA-256 hash of the cluster, the kind, the namespace and the name,
  so names of different clusters cannot collide. An object with any other name grants nothing
  (`InvalidSpokePolicy`).
* `spec.cluster` is taken from the agent cert. The label `policy.querdian.dev/cluster` and an owner
  reference to the `SpokeCluster` are set by the receiver, so a deletion of the `SpokeCluster` also
  removes its `SpokePolicy` objects.
* `status.conditions` has the `Ready` condition with the reasons in the
  [policy guide](policy-guide.md#status-conditions). `status.sourceGeneration` is the spoke generation
  that was compiled.

```sh
kubectl get sp     # Cluster, Kind, Namespace, Source (spoke name) and Ready
```

The tenant of a spoke `AccessPolicy` comes from the label `policy.querdian.dev/tenant` of its
namespace **in the spoke**, then from `defaultTenant`. Hub namespace labels never apply to a spoke
policy.

### querdian receiver

`querdian receiver` runs as its own Deployment on the hub. It reads the `SpokeCluster` objects and
the stored `SpokePolicy` objects directly from the API server, never from a cache.

| Flag | Default | Meaning |
|---|---|---|
| `--listen` | `:8443` | mTLS listener for agents. The only path is `POST /v1alpha1/sync`. |
| `--management-listen` | `:9090` | `/healthz` and `/readyz`. No `/metrics`. |
| `--tls-cert-file`, `--tls-key-file` | required | Server cert and key. |
| `--client-ca-file` | required | CA bundle for agent certs. Only the hub may issue from it. |
| `--leader-elect` | `true` | Lease `querdian-receiver-leader-election`. Only the leader serves syncs. |
| `--leader-election-namespace` | pod namespace | Namespace of the lease. |
| `--max-body-bytes` | `8388608` (8 MiB) | Maximum size of a sync body (`413`). |
| `--max-policies` | `2000` | Maximum number of policies in one full sync (`413`). |
| `--max-in-flight` | `16` | Maximum number of requests in progress (`503`). |
| `--log-level`, `--log-format` | `info`, `json` | As for the proxy. |

* **Readiness.** `/readyz` returns 200 only on the leader. A Service in front of the receiver
  therefore sends traffic only to the leader. Run two replicas for a fast failover; the receiver is
  not in the read path, and the time limit is long, so a short failover does no harm.
* **One sync per cluster.** A second sync for a cluster while one runs gets `409`, and the agent
  retries. More than one changed sync from one cluster in 10 seconds gets `429`.
* **Write order.** A changed sync writes new and changed objects first, then deletes, and sets
  `lastSyncTime` last, with a `resourceVersion` precondition on the `SpokeCluster` read at the start
  of the sync. A sync that overlaps a change of the `SpokeCluster` validates nothing.
* **RBAC.** `get`, `list` on `spokeclusters`; `update` on `spokeclusters/status`; `list`, `create`,
  `update`, `delete` on `spokepolicies`; and the `coordination.k8s.io` lease for leader election. Give
  the receiver no other write access. A compromised receiver can grant the data of all spokes
  ([threat model](security/threat-model.md#12-multi-cluster)).
* The HTTP statuses of the receiver are listed in
  [troubleshooting](troubleshooting.md#receiver-http-statuses).

### querdian agent

`querdian agent` runs in each spoke. Before each heartbeat it lists all `ClusterAccessPolicy`,
`AccessPolicy` and `Namespace` objects directly from the spoke API server. If a list fails, it sends
nothing, so an agent that has lost its API server cannot renew old policies.

| Flag | Default | Meaning |
|---|---|---|
| `--receiver-url` | required | `https://host[:port][/prefix]` of the receiver. |
| `--tls-cert-file`, `--tls-key-file` | required | The agent cert and key. Read again before each request. |
| `--tls-ca-file` | required | CA certs that verify the receiver cert. Read again before each request. |
| `--heartbeat-interval` | `1m` | Time between two heartbeats, at least `10s`. |
| `--request-timeout` | `30s` | Time limit for one request to the receiver. |
| `--leader-elect` | `true` | One agent sends at a time. |
| `--leader-election-namespace` | pod namespace | Namespace of the lease. |
| `--leader-election-id` | `querdian-agent` | Name of the lease. |
| `--management-listen` | `:9090` | `/healthz` and `/readyz` (always 200). Empty disables it. |
| `--log-level`, `--log-format` | `info`, `json` | As for the proxy. |

* At each heartbeat the agent sends the digest of its state. If the receiver has a different state,
  it answers `fullStateRequired`, and the agent sends the full state at once. The full state replaces
  the earlier state of the spoke, so a policy that is deleted in the spoke is deleted on the hub.
* After a failure the agent retries at least each minute: with backoff after `409`, `429` and network
  errors, and at the next heartbeat (at most one minute) after other errors.
* The agent writes the `Ready` condition and `status.observedGeneration` of each spoke policy from the
  sync reply, so spoke teams can use `kubectl get cap,ap`. The conditions show the last compile on the
  hub, so a new policy shows its state at the next heartbeat. The reply omits the hub-only
  information of `CrossTenantUnionUnsupported`: such a policy shows `Compiled`.
* **RBAC.** `get`, `list`, `watch` on `clusteraccesspolicies`, `accesspolicies` and `namespaces`;
  `update`, `patch` on `clusteraccesspolicies/status` and `accesspolicies/status`; `get`, `create`,
  `update` on `coordination.k8s.io` `leases` in its own namespace. No other permission and no
  wildcard on `status`.

### Metrics and alerts

The proxy exports these metrics on its management port, for each `SpokeCluster` with a valid name
and identity. A `SpokeCluster` with the name of `hubCluster`, or whose identity does not end with its
name, has **no series** in the two `querdian_cluster_policy_*` metrics; the `Ready` condition of its
`SpokePolicy` objects (`SpokeClusterInvalid`) shows the cause.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `querdian_cluster_policy_age_seconds` | gauge | `cluster` | Time since the last valid sync. `+Inf` if the spoke has never synced, negative if `lastSyncTime` is in the future. |
| `querdian_cluster_policy_expired` | gauge | `cluster` | `1` if the policies of the spoke are expired or suspended, else `0`. The age alone does not show a suspended spoke or a spoke whose generation is not synced. |
| `querdian_policies{kind="SpokePolicy"}` | gauge | `kind`, `ready` | `SpokePolicy` objects by compile result (only with the block). |

Alert long before the time limit, for example at 24 hours with the default of 48 hours:

```promql
querdian_cluster_policy_age_seconds > 86400
querdian_cluster_policy_expired == 1
```

The Helm chart can install an alert at 24 hours; see the [chart README](../charts/querdian/README.md).

## Tenant setups

The design does not assume a tenant setup. For a backend with a `tenantHeader`:

* The `tenants` of a spoke `ClusterAccessPolicy` rule must be a subset of `SpokeCluster.spec.tenants`.
* The tenant of a spoke `AccessPolicy` is the namespace label in the spoke, then `defaultTenant`. It
  must be in `spec.tenants`. The value is compared exactly (a hub namespace label is trimmed, a spoke
  label is not).
* Any other tenant makes the policy invalid with `TenantNotAllowed`, so a spoke admin cannot select a
  tenant that the hub did not allow. A spoke `AccessPolicy` with no tenant label and no
  `defaultTenant` is `TenantNotAssigned`.
* A change to `spec.tenants` or `defaultTenant` starts a recompile, so a smaller allow-list removes
  the grant at once.
* Hub policies have no allow-list.

| Setup | Result |
|---|---|
| No tenant header | Terms have no tenant. Terms of several clusters merge where the [DNF rules](policy-guide.md#union-semantics-in-plain-language) permit it. |
| One tenant for all clusters | The same as above, with one tenant header. Set `tenants: [shared]` on each `SpokeCluster`. |
| One tenant for each cluster | A user with grants in two clusters has a cross-tenant union, and the cluster matcher makes the terms differ. This needs `tenantFederation: tenantIdLabel` (Loki, Mimir). Otherwise the request is `403 cross_tenant_union_unsupported`: today on Tempo and on Prometheus/Thanos with a tenant header. |
| Several tenants for each cluster | The rules of the row above apply when the user's terms span tenants. |

The setups with a cross-tenant union also get the current limits of federated tenants: on Loki,
tenant-bound metadata endpoints (`labels`, `label/<name>/values`, `series`, `index/*`) give empty or
wrong results and `tail` does not work with several tenants; on Tempo, tenant-bound terms give
`400 unsupported`. See [limitations](limitations.md#logql) and
[TraceQL and Tempo](limitations.md#traceql-and-tempo). If most users read one cluster at a time, one
tenant for all clusters, or no tenant header, avoids these limits.

## Day-2 operations

### Certificate rotation

* **Agent.** The agent reads its cert, key and CA file again before each request, so a rotated cert
  (for example a cert-manager renewal into a mounted Secret) takes effect at the next heartbeat with
  no restart. If a file cannot be read during the rotation, the agent keeps the last good values. A
  normal renewal keeps the same URI SAN, so the hub needs no change.
* **Receiver.** The receiver reads `--tls-cert-file`, `--tls-key-file` and `--client-ca-file` once at
  startup. **Restart the receiver after a rotation** of its server cert or of the client CA bundle.
  During a CA change, put the old and the new CA in the bundle, restart, rotate the agent certs, then
  remove the old CA and restart again.

### Time limit and outages

* Revocation of a spoke policy takes up to one heartbeat. During an outage of the agent, it takes up
  to the time limit. The limit on changed syncs (one in 10 seconds) can delay a revocation by one
  retry.
* An expired agent cert stops the syncs only. The grants continue until the time limit.
* A failure of the receiver, the network path or the hub API server expires **all** spokes after the
  time limit. Hub grants stay active; a hub admin can add a temporary hub `ClusterAccessPolicy`.
* A spoke expires if any of these is true: no `SpokeCluster` exists, `suspended` is true, the
  `SpokeCluster` is being deleted, `lastSyncTime` is absent or more than five minutes in the future,
  `status.observedGeneration` differs from `metadata.generation`, the current snapshot was not yet
  compiled from the current generation of the `SpokeCluster`, or `maxStaleness` has passed since
  `lastSyncTime`.
* An expired rule gives no terms, but its `labels.deny` and `labels.redact` stay active, so expiry
  only narrows access. A user whose grants all come from expired spokes gets
  `403 policy_source_expired`; the message does not name the cluster.
* An open Loki tail that uses terms of a spoke closes within 10 seconds after that spoke expires or is
  suspended (WebSocket close code 1008, reason `policy source expired`).

### Suspend a spoke

```sh
kubectl patch spokecluster cluster-a --type merge -p '{"spec":{"suspended":true}}'
```

New requests stop when each replica sees the change, normally in about one second, and open tails
close within 10 seconds. The receiver rejects each sync of a suspended spoke with `403`. After
`suspended: false`, the grants return only after a new valid sync, because the generation changed.

### Stolen agent cert

The time limit gives no protection against a stolen agent cert, because the holder can renew
`lastSyncTime`. The holder can write any grant for that spoke, but not for a different spoke. There is
no certificate revocation list. Do these steps on the hub:

1. Suspend the spoke (see above). The receiver rejects each sync, new requests stop in about one
   second and open tails close within 10 seconds.
2. Issue a new agent cert with a **new URI SAN** (the last segment stays the cluster name, for example
   `spiffe://mla.example.com/agent/2/cluster-a`), then change `spec.identity`. The old cert no longer
   matches. Install the new cert in the spoke.
3. Delete the `SpokePolicy` objects of the spoke. This also removes the `labels.deny` and
   `labels.redact` of forged rules, which stay active while the spoke is suspended:

   ```sh
   kubectl get spokepolicies -o json \
     | jq -r '.items[] | select(.spec.cluster == "cluster-a") | .metadata.name' \
     | xargs -r kubectl delete spokepolicy
   ```

4. Set `suspended: false`. The generation changed, so grants return only after a new valid sync from
   the agent with the new cert.

Find out how the cert was stolen before step 4. If the CA key itself is exposed, replace the CA (see
[certificate rotation](#certificate-rotation)) and issue new certs for all spokes.

### Remove a spoke

1. Uninstall the agent in the spoke.
2. On the hub, delete the `SpokeCluster`. The spoke expires at once, the receiver rejects its identity,
   and Kubernetes deletes its `SpokePolicy` objects through the owner reference.
3. Revoke the ingest credential of the spoke at the ingest gateway, so that no new data arrives with
   its cluster name.
4. Remove the cluster name from hub `ClusterAccessPolicy` matchers where it is no longer needed.

The data of the spoke stays in the backends until it leaves retention. Hub rules with `allClusters`,
with `unrestricted` or with a matcher that names the cluster still match it. **Do not reuse the
cluster name** for a different cluster while that data exists: the admin of the new cluster would
control access to the old data.

### Rollback

A rollback to a single-cluster configuration is a risk while spoke data is in the backends.

* Querdian refuses to start if a backend keeps `clusterLabel` without the `multicluster` block.
* If you remove both, each hub `AccessPolicy` loses its cluster matcher and matches namespace `X` of
  **every** spoke. Each `SpokePolicy` grants nothing (`MulticlusterDisabled`), and `allClusters` has no
  effect.

Before such a rollback, delete or narrow the hub `AccessPolicy` objects (for example by moving them to
hub `ClusterAccessPolicy` objects with an explicit cluster matcher), or keep the block until the
spoke data has left retention. To pause the spokes, suspend them instead of removing the block.

### Do not run a local Querdian and the agent on the same spoke

The agent writes the `Ready` condition and `status.observedGeneration` of the spoke policies. A
Querdian proxy that runs in the same spoke writes the same fields from its own compile. The two
writers would change the status again and again, and the status would not show what the hub enforces.
Run either a local Querdian or the agent in a cluster, not both.
