# Policy guide

This page explains how to write the policies that give users access to data through Querdian.

In short:

* Policies only grant access. An `AccessPolicy` grants the data of its own namespace. A
  `ClusterAccessPolicy` grants the data that its matchers select or, with `unrestricted: true`, all
  data of its tenants.
* The access of a request is the exact union of all rules that match the user name or a group of the
  user on the requested backend. If no rule matches, the answer is `403 no_grant`.
* If one rule of a policy is not valid, the full policy grants nothing. The `Ready` condition of the
  policy shows the reason.
* `labels.deny` stops queries that use a label, but query results still show the label. To hide the
  values, use `labels.redact`.
* The isolation depends on the namespace label in the telemetry. Your collection pipeline must set it.

For the path of a query through Querdian, see [How Querdian works](architecture.md). For the static
configuration, see [operations](operations.md). For denials, see [troubleshooting](troubleshooting.md). The sections from
[Status conditions](#status-conditions) to the end are reference tables. The
[policy reference](policy-reference.md) gives more details.

> **Read this first: namespace-label integrity**
>
> An `AccessPolicy` grants access to the data whose *namespace label* is equal to the namespace of the
> policy. Querdian cannot verify that label. It trusts the value that is stored in the telemetry.
>
> * Your **collection pipeline must set** the label that `backends[].namespaceLabel` names, for
>   example `namespace` in Prometheus, `k8s_namespace_name` in Loki and `resource.k8s.namespace.name`
>   in Tempo. The value must come from the namespace where the pod really runs. Examples of the
>   pipeline: Prometheus scrape/relabel configuration, OpenTelemetry Collector `k8sattributes`,
>   Alloy/Promtail.
> * A **workload must not be able to choose or override** that label. Some pods can send metrics, logs
>   or spans with `namespace="payments"`, for example through a push endpoint, through an OTLP
>   exporter that the collector does not overwrite, or with `honor_labels: true`. The data of such a
>   pod shows in the view of another team. The pod cannot read more data in this way, but it can put
>   misleading data into the dashboards of other teams.
> * Data without the label is not visible to any `AccessPolicy`. The proxy cannot detect this case or
>   the case above.
> * A person who can create or edit an `AccessPolicy` in namespace `X` decides who can read the data
>   with the label `X`. A person who can edit the labels of a `Namespace` decides which backend tenant
>   its policies use (`policy.querdian.dev/tenant`). Restrict the Kubernetes RBAC for both.
> * Give only platform administrators the permission to create `ClusterAccessPolicy` objects, because
>   these can grant `unrestricted` access.
>
> If you cannot make sure of the label integrity, use a `ClusterAccessPolicy` with matchers on labels
> that you control, or separate tenants on the backend. Also see the
> [threat model](security/threat-model.md).

## Examples that work

The three examples below are valid with the [sample configuration](operations.md#sample-configuration).
It has three backends: `metrics` is Prometheus without a tenant header, `logs` is Loki with
`X-Scope-OrgID`, and `traces` is Tempo with `X-Scope-OrgID` and `traceByID: filter`.

```yaml
apiVersion: policy.querdian.dev/v1alpha1
kind: ClusterAccessPolicy
metadata:
  name: platform-admins
spec:
  subjects:
    groups: [platform-admins]
  rules:
    - backends: [metrics, logs, traces]
      tenants: [tenant1, tenant2]   # ignored for "metrics" (no tenant header)
      unrestricted: true
---
apiVersion: policy.querdian.dev/v1alpha1
kind: ClusterAccessPolicy
metadata:
  name: sre-shared-infra
spec:
  subjects:
    groups: [sre]
  rules:
    - backends: [metrics]
      tenants: [tenant1]
      matchers:
        - {name: namespace, op: RegexMatch, value: "kube-system|ingress-nginx|cert-manager"}
      labels:
        deny: [customer_id]
---
apiVersion: policy.querdian.dev/v1alpha1
kind: AccessPolicy
metadata:
  name: order-team
  namespace: order-service        # this namespace needs the label policy.querdian.dev/tenant
spec:
  subjects:
    groups: [order-service-team]
    users: [alice@example.com]
  rules:
    - backends: [metrics, logs]
      matchers:
        - {name: service, op: NotEqual, value: ""}
    - backends: [traces]
```

After `kubectl label namespace order-service policy.querdian.dev/tenant=tenant1`, a member of
`order-service-team` gets this access:

| Backend | Effective policy |
|---|---|
| `metrics` | `namespace="order-service", service!=""` |
| `logs` | tenant header `tenant1`, `k8s_namespace_name="order-service", service!=""` |
| `traces` | tenant header `tenant1`, `resource.k8s.namespace.name = "order-service"` |

* A member of `platform-admins` is unrestricted on the three backends. On `logs` and `traces`, the
  tenant header is `tenant1|tenant2`.
* A member of `sre` has access to `metrics` only. On `logs` and `traces`, the answer is `403 no_grant`.
* If the namespace does not have the tenant label, `order-team` gets the reason `TenantNotAssigned`.
  Querdian accepts or rejects a policy as a whole, thus `order-team` **also loses its `metrics`
  rule**.

## Policy resources

### Subjects

```yaml
subjects:
  groups: [order-service-team]   # matched against the groups claim of the token
  users: [alice@example.com]     # matched against the username claim of the token
```

* A policy must have at least one group or user. The CRD enforces this.
* Users and groups are separate keyspaces. A user whose name is equal to a group name gets nothing from
  the policies of that group.
* The match is exact and case sensitive.
* The [static configuration](operations.md#auth) sets the token claims for the user name and the
  groups.

### ClusterAccessPolicy (cluster scoped, short name `cap`)

For platform administrators. A rule can use matchers on any label, can name tenants and can be `unrestricted`.

| Field | Meaning |
|---|---|
| `backends` | Names of backends from the static configuration. At least one. |
| `tenants` | The backend tenants of the rule. **Necessary for each backend that has a `tenantHeader`**. Backends without a `tenantHeader` ignore it. |
| `unrestricted` | `true` grants all data of the listed tenants. A rule must have `unrestricted: true` or a non-empty `matchers` list, not both. The CRD and the compiler check this. A rule with `unrestricted: false` and no matchers gets `InvalidMatcher`, also if it was stored without the CRD validation. |
| `matchers` | Label constraints. All must be true (AND). |
| `allClusters` | Multi-cluster only. The rule applies to all clusters, also to spokes that you add later. See [Multi-cluster](#multi-cluster). Without the `multicluster` configuration block, it has no effect. Do not use it together with `unrestricted`. |
| `labels.deny` | Labels that the user cannot use in queries. Query results still show them. See [Denied labels](#denied-labels). |

### AccessPolicy (namespaced, short name `ap`)

For teams. The differences from a `ClusterAccessPolicy` rule:

* Querdian adds the matcher `<backend.namespaceLabel> = "<metadata.namespace>"` to each rule (AND).
  This matcher is first. The policy author cannot remove or widen it. In a
  [multi-cluster](#multi-cluster) setup, a cluster matcher follows it.
* There is no `unrestricted` field and no `tenants` field.
* For backends that have a `tenantHeader`, the tenant comes from the label `policy.querdian.dev/tenant`
  of the namespace. If this label is absent or blank, the full policy is not valid
  (`TenantNotAssigned`).
* `matchers` are optional. A rule without matchers grants all data in the namespace.

### Matchers

```yaml
matchers:
  - {name: service, op: NotEqual, value: ""}
```

| `op` | Meaning | PromQL / LogQL operator |
|---|---|---|
| `Equal` | label equals value | `=` |
| `NotEqual` | label differs from value | `!=` |
| `RegexMatch` | label matches the regex | `=~` |
| `RegexNoMatch` | label does not match the regex | `!~` |

* Regexes use RE2 syntax and are **fully anchored** (`^(?:value)$`), as in Prometheus and Loki. If a
  regex does not compile, the policy is not valid (`InvalidMatcher`).
* Querdian validates the names of a rule against each backend that the rule lists. A name that is not
  valid for one backend makes the full policy not valid.
* In PromQL and LogQL, an absent label has the same effect as an empty value: `service != ""` excludes
  series without a `service` label. **Tempo is different**: see
  [Tempo: missing attributes](#tempo-missing-attributes).

| Backend type | Valid names |
|---|---|
| `prometheus`, `loki` | `[a-zA-Z_][a-zA-Z0-9_]*`. Querdian rejects the reserved name `__tenant_id__`. For Loki labels with dots (such as `k8s.namespace.name`), use the normalised name (`k8s_namespace_name`). |
| `tempo` | Attributes with an explicit scope: `resource.<name>` or `span.<name>`. The name can contain letters, digits, `_`, `.`, `-` and `/` (for example `resource.k8s.namespace.name`). Policies do not accept the unscoped form `.name`. |

### Tempo: missing attributes

Policy matchers on Tempo behave as TraceQL does, not as Prometheus and Loki do. In TraceQL, a span
that does not have an attribute satisfies **none** of `=`, `!=`, `=~`, `!~` (verified with Tempo
2.9.0). An attribute that is not a string also never satisfies a string comparison.

* For example, `resource.team != "x"` admits only spans that *have* a `team` attribute with a
  different value. The same `NotEqual` matcher in Prometheus also admits series without the label.
* Thus the Tempo form of a policy can only grant less, never more. Trace by ID uses the same rule in
  the proxy.
* If you need the semantics "label is absent" on Tempo, use an attribute that is always set.
* Policies do not accept unscoped matchers, so that this behaviour stays predictable.

## Union semantics in plain language

A user can match many rules at the same time: through several groups, through a personal rule and
group rules, or on several backends. Querdian combines the rules as follows.

1. Each rule becomes one *term*: a list of matchers that must all be true (AND), and the tenant of the
   rule.
2. The access of the user is *any* term (OR). A series, log line or span is visible if at least one
   term admits it. This is the **exact union**. Querdian never widens it, for example with a dropped
   matcher. It never narrows it, except with the denied labels.
3. Querdian simplifies the set of terms to an equivalent, smaller set (disjunctive normal form, DNF).
   The numbers below do not give a sequence: Querdian repeats steps 2 to 4 until nothing changes, then
   it does step 1, step 6 and step 5.
   1. **Unrestricted absorbs, for each tenant.** An unrestricted rule admits all data of its tenant,
      so the other terms of that tenant are redundant. The user is `Unrestricted` only if *each*
      tenant of the request has an unrestricted rule. Unrestricted access on `tenant1` never widens a
      restricted grant on `tenant2`.
   2. **Duplicates.** Querdian removes them.
   3. **Subsumption.** Term A and term B have the same tenant. If A has a subset of the matchers of B,
      A is broader, and Querdian removes B.
   4. **Single-label merge.** Some terms of the same tenant are identical except for one label. If
      that label uses `Equal` or `RegexMatch` in all of them, the terms merge into one regex
      alternation. `namespace="a"` and `namespace="b"` become `namespace=~"(?:a)|(?:b)"`.
   5. **Disjointness.** Two terms are disjoint if they have different tenants, or if both have an
      `Equal` matcher on the same label with different values. If all pairs of terms are disjoint,
      summed Loki statistics are exact (see [limitations](limitations.md#approximate-results)).
   6. **Limits.** If the simplified policy has more than `limits.maxPolicyTerms` terms (default 16),
      the answer is `403 policy_too_complex`. Fixed limits bound the work of each request (also
      `403 policy_too_complex`). `limits.maxQueryRegexWeight` limits the regexes of the client query,
      and `limits.maxRequestRegexWeight` limits the regexes of the rewritten query
      (`403 query_too_complex`). See the table in the
      [operations reference](operations-reference.md#limits-and-what-happens-when-they-trip).
4. Each backend applies the terms in the way that it can express them. It injects one term into each
   selector, or-lifts a query over several terms, expands `match[]` selectors, injects a DNF filter
   (Tempo), or sends one request for each term and merges the results (Loki). See
   [Which strategy does what](policy-reference.md#which-strategy-does-what).

Example: `alice@example.com` is in the groups `sre` and `order-service-team`. On `metrics`, she has the
terms `namespace="order-service", service!=""` and
`namespace=~"kube-system|ingress-nginx|cert-manager"`. The terms have a different shape, so they do not
merge. A query `up` becomes
`(up{namespace="order-service",service!=""} or up{namespace=~"kube-system|ingress-nginx|cert-manager"})`.

Properties to remember:

* More rules never reduce access. Only `labels.deny` and a policy that is not valid *narrow* access.
* The denied labels of a matched rule apply to the full request.
* A user without a rule for the backend gets no access, also if the user has rules on other
  backends.
* The result is deterministic: the same inputs in any order give the same terms.

## Tenants

A backend with a `tenantHeader` (Loki, Tempo, Mimir) is multi-tenant. Querdian sets that header on
each upstream request from the effective policy. It removes each value of this header that the client sends.

* A `ClusterAccessPolicy` rule names its tenants. An `AccessPolicy` gets its tenant from the namespace
  label `policy.querdian.dev/tenant`.
* One tenant: the header contains that tenant, and the terms need no tenant binding.
* Several tenants: the header is `t1|t2|...` (the federation syntax of Loki, Tempo and Mimir). The
  backend must have federation enabled: Loki `multi_tenant_queries_enabled`, Mimir
  `tenant_federation.enabled`, Tempo `multitenancy_enabled` and
  `query_frontend.multi_tenant_queries_enabled`.
* Backends without a `tenantHeader` ignore `tenants` completely.

### Cross-tenant behaviour per backend

If the rules of a user include several tenants, there are three cases:

1. The user is unrestricted on each tenant. Querdian sets the federated header and changes nothing
   else.
2. The terms are **identical in each tenant**, for example the same `AccessPolicy` shape in two
   tenants. Querdian sets the federated header and uses the terms without tenant binding.
3. The terms **are different for each tenant**. This is possible only if the backend can filter by
   tenant. With `tenantFederation: tenantIdLabel`, each term gets the extra matcher
   `__tenant_id__="<tenant>"`. With `tenantFederation: none`, Querdian rejects the request with
   `403 cross_tenant_union_unsupported`.

The support of each backend, and the recommended `tenantFederation` value (from
`docs/research/tenant-federation.md`):

| Backend | Federated queries | `__tenant_id__` filter | Use |
|---|---|---|---|
| Loki 3.5.0 | yes | yes, as label matcher (`=`, `=~`, `!=`) | `tenantIdLabel` |
| Mimir 2.16.0 | yes | yes | `tenantIdLabel` |
| Tempo 2.8.1 | yes (search, trace by ID, tags) | **no**, a filter on it silently matches nothing | `none` |
| Thanos / Prometheus | not applicable, no tenant header | n/a | `none` (leave `tenantHeader` empty) |

Consequences:

* With `tenantIdLabel`, Querdian injects `__tenant_id__` only when the header contains several
  tenants.
* Loki rejects a selector whose only matcher is `__tenant_id__`. Thus Querdian requires that each
  selector keeps at least one other matcher, and otherwise answers `400 unsupported`. A user query
  always has one, because Loki requires it. The case can occur for label or statistics endpoints
  without a `query`, if a restricted, tenant-bound term has no other matcher.
* On Tempo, give users with grants in several tenants the *same* rule shape in each tenant. If not,
  they get `403 cross_tenant_union_unsupported`. Tempo does not fan out for each tenant.
* The status `CrossTenantUnionUnsupported` shows at policy level which backends have this problem.
* The behaviour with duplicate tenants (`t1|t1`) and unusual tenant IDs is not tested.

## Multi-cluster

With the `multicluster` block in the static configuration, one Querdian on a central hub enforces the
policies of the hub and of several spoke clusters. For the setup, the trust model and the operations,
see [multicluster.md](multicluster.md). For policy authors, three rules change.

### The implicit cluster matcher

Querdian takes the cluster of a policy from the origin of the policy, never from its text. This is the
same as for the namespace of an `AccessPolicy`.

| Policy | Implicit matchers |
|---|---|
| Hub `AccessPolicy` in namespace `X` | `<namespaceLabel>="X"`, then `<clusterLabel>="<hubCluster>"` |
| Spoke `AccessPolicy` in namespace `X` | `<namespaceLabel>="X"`, then `<clusterLabel>="<spoke>"` |
| Spoke `ClusterAccessPolicy` | `<clusterLabel>="<spoke>"` (first) |
| Hub `ClusterAccessPolicy` | none; the rule states its own scope (below) |

* `<clusterLabel>` is the `clusterLabel` of the backend. `<hubCluster>` is `multicluster.hubCluster`.
  `<spoke>` is the name of the `SpokeCluster`.
* A policy author cannot remove or widen these matchers. A spoke matcher on the cluster label is ANDed
  with the implicit matcher, so it can only narrow.
* A spoke rule with `unrestricted: true` grants the full spoke (`<clusterLabel>="<spoke>"`). It is never
  unrestricted, so Querdian always rewrites the queries of its users.
* A spoke rule with `allClusters: true` gets `InvalidClusterScope`.
* Hub data without the cluster label is not visible to the users of hub `AccessPolicy` objects. Thus
  hub telemetry must have `<clusterLabel>="<hubCluster>"`.

### The cluster scope of a hub ClusterAccessPolicy

A new spoke must not silently widen a hub grant. Thus each rule must state its cluster scope **for
each of its backends**, with one of these:

* A matcher on the `clusterLabel` of that backend that names a **fixed set of non-empty cluster
  names**: `Equal`, or `RegexMatch` with a finite alternation such as `mla|cluster-a` or
  `cluster-(a|b)`.
* `allClusters: true`, without a matcher on the `clusterLabel` of that backend.
* `unrestricted: true` (all clusters, also clusters that you add later).

These matchers do not state a scope:

* `NotEqual`, `RegexNoMatch` and open regexes such as `.+` or `cluster-.*`, because they also match
  each new spoke.
* An empty value, because it matches data without a cluster label.
* Case-insensitive regexes, and regexes that name more than 1024 clusters.

If a rule does not state its scope, the policy gets `InvalidClusterScope` and grants nothing.
`allClusters` exists because `cluster=~".+"` is an open regex, and Tempo tag lookups reject open
regexes with `400`.

```yaml
rules:
  - backends: [metrics]
    matchers:
      - {name: cluster, op: RegexMatch, value: "mla|cluster-a"}
      - {name: namespace, op: Equal, value: kube-system}
  - backends: [logs]
    tenants: [tenant1]
    allClusters: true          # every cluster, also spokes added later
    matchers:
      - {name: k8s_namespace_name, op: Equal, value: kube-system}
```

A rule that lists several backends needs the scope on each of them. A cluster matcher names one
label, thus it states the scope only for the backends whose `clusterLabel` has that name.

### Spoke tenants

A spoke policy can use only the tenants in `SpokeCluster.spec.tenants`. Other tenants give
`TenantNotAllowed`. The tenant of a spoke `AccessPolicy` comes from the namespace label in the spoke,
or else from `spec.defaultTenant`. See [multicluster.md](multicluster.md#tenant-setups).

## Denied labels

`labels.deny` lists label names (Prometheus, Loki) or scoped attributes (Tempo, for example
`span.http.url`).

* The union rule is conservative: **if one matched rule denies a label, the label is denied for the
  full request, for all terms**. This is also true if another matched rule does not deny it.
* Querdian validates the names as matcher names. A name that is not valid makes the policy not valid
  (`InvalidMatcher`).

### What it does

* Querdian rejects a query that uses a denied label with `403 denied_label`. This also applies to
  unrestricted users, in PromQL, LogQL and TraceQL.
* Endpoints that list labels do not list denied labels. Endpoints that ask for the values of a denied
  label answer `403`.
* With `traceByID: filter`, trace by ID removes denied attributes from each returned span.

For the rules of each query language and each endpoint, see
[Denied labels in queries and endpoints](policy-reference.md#denied-labels-in-queries-and-endpoints).

### What it does NOT do

* **Query results keep their labels.** `up`, `{app="x"}` or a Tempo search return the full label set
  or all attributes of each series, stream or span. A denied label hides only the ability to filter,
  group or join on it, not the data. If `customer_id` is denied, `up` still shows
  `customer_id="42"` on the series that it returns. Querdian does not filter query results and Tempo
  search results. To hide the values also, use [`labels.redact`](#redacted-values). If you do not use
  it, do not put the value in a label that the user can read, or limit what the user can read with
  matchers.
* It does not hide a label from the policy itself. The matchers of a rule can use a label that the same
  rule denies.
* Querdian returns a LogQL log line that *contains* the value without a change. Parsers such as
  `| json` can make labels from line content, but Querdian rejects a query that uses the denied name.
* Querdian denies Prometheus `metadata`, `rules`, `targets` and the other endpoints that it cannot
  restrict by label. This is independent of `labels.deny`.
* The LogQL scan is intentionally broad and has false positives. See
  [limitations](limitations.md#denied-label-scan-false-positives).

### Redacted values

`labels.redact` is the opt-in that closes the gap above: it changes the values of a label in query
results. Each redacted name is **also denied**, so all of [What it does](#what-it-does) applies to it.
Without this, a user could filter on `customer_id="42"` and learn the value from the result: data or no
data.

```yaml
labels:
  deny: [team_secret]
  redact:
    - {name: customer_id, strategy: Placeholder}
    - {name: client_ip, strategy: IPv4Prefix, octets: 2}
```

| Strategy | `customer_id="42"`, `client_ip="10.1.2.3"` becomes | What the user can still learn |
|---|---|---|
| `Remove` | the label is dropped | how many series/streams were returned (duplicates are kept) |
| `Placeholder` | `redacted-1`, `redacted-2`, ... for each distinct value, numbered for each response in random order | which entries of one response have the same value |
| `Hash` | `redacted-` and 16 hex digits, an HMAC with a key that is random for each request | the same as `Placeholder`; never comparable across requests |
| `IPv4Prefix` (`octets`: 1–3) | `10.1.x.x`; a value that is not an IPv4 address becomes `redacted` | the kept octets |

* **Where it applies**: Prometheus `query`, `query_range`, `query_exemplars`. Loki `query`,
  `query_range` (stream labels and the `structuredMetadata` / `parsed` entry metadata of the
  categorize-labels encoding) and `tail`. Tempo `search`, `metrics/query`, `metrics/query_range`
  (Querdian drops their `promLabels` string) and trace by ID (`traceByID: filter`). The `series`,
  label and index endpoints remove redacted names in the same way as denied names.
* **Several rules**: as with `deny`, a redaction from any matched rule applies to the full request. If
  rules have different strategies for a label, the strictest strategy applies: `Remove`, then
  `Placeholder` or `Hash`, then `IPv4Prefix` with fewer octets. A rule that only denies a label does
  not weaken the redaction of that label by another rule.
* **Fail closed**: if Querdian cannot decode a response, or if the response is larger than
  `limits.maxMergeBytes`, Querdian answers `502`. It never sends that response to the client.
  Querdian asks the upstream for JSON.
* **Cost**: Querdian buffers a redacted response fully before it sends it, and does not stream it. Very
  large range queries can reach `limits.maxMergeBytes` (`502 merge_too_large`).
* **What it does not redact**: Loki log *lines* (`| json` can make labels from line content, and
  nested keys get other names, such as `ctx_customer_id`), values that the ingestion copied into other
  labels, the number and the order of the returned series, and the Tempo trace-level fields that the
  [threat model](security/threat-model.md) describes.

## Status conditions

Querdian compiles the policies in the background into a snapshot that does not change, about 250 ms
after a change. Then it replaces the old snapshot in one step. Thus a request never sees a change that
is only partly applied.

Each policy gets one condition `type: Ready`, which the leader replica writes.

* `kubectl get cap` and `kubectl get ap` show `Ready` and the reason. The metric
  `querdian_policies{kind,ready}` gives the same counts.
* `status.observedGeneration` is the generation that Querdian compiled. Querdian never writes a status
  for an older generation over a newer spec.
* **All reasons except `Compiled` and `CrossTenantUnionUnsupported` mean that the policy grants
  nothing, on all its backends.** One rule that is not valid disables the full policy.

| Reason | `Ready` | Meaning and fix |
|---|---|---|
| `Compiled` | `True` | The policy is active. |
| `InvalidMatcher` | `False` | A regex does not compile, a matcher or denied-label name is not valid for the backend type (for example `__tenant_id__`, a name with dots on Prometheus/Loki, an unscoped name on Tempo), an operator is unknown, an `unrestricted` rule also lists matchers, or a `ClusterAccessPolicy` rule has `unrestricted: false` and no matchers. The CRD also rejects the last case, but the compiler does not rely on it. This reason also occurs if the `namespaceLabel` of the backend is not a valid name for its type. The message names the rule and the field. Fix the matcher. |
| `UnknownBackend` | `False` | A rule lists a backend that is not in the static configuration. Fix the name or the configuration. A change of the configuration needs a restart. |
| `TenantRequired` | `False` | A `ClusterAccessPolicy` rule uses a backend with a `tenantHeader`, but lists no `tenants`. Add `tenants`. |
| `TenantNotAssigned` | `False` | An `AccessPolicy` uses a backend with a `tenantHeader`, but its namespace does not have a non-blank label `policy.querdian.dev/tenant`. Label the namespace. Querdian makes the snapshot again when labels change. For a spoke `AccessPolicy`: the spoke namespace has no tenant label, and the `SpokeCluster` has no `defaultTenant`. |
| `CrossTenantUnionUnsupported` | `True` | The policy is active. But a user who has this policy has grants in several tenants with different rules, on a backend with `tenantFederation: none`. The requests of such users get `403 cross_tenant_union_unsupported`. The message lists the backends. For information only. |
| `InvalidClusterScope` | `False` | Multi-cluster only. A hub `ClusterAccessPolicy` rule does not state its cluster scope for one of its backends (no `Equal` or finite `RegexMatch` on the `clusterLabel`, no `allClusters`, not `unrestricted`). Or the rule uses `allClusters` together with a cluster matcher or with `unrestricted`. Or a spoke rule sets `allClusters`. See [Multi-cluster](#multi-cluster). |
| `TenantNotAllowed` | `False` | Multi-cluster only. A spoke policy uses a tenant that is not in `SpokeCluster.spec.tenants`. Add the tenant to the allow-list on the hub, or change the spoke policy or the namespace label. |
| `MulticlusterDisabled` | `False` | A `SpokePolicy` exists, but the static configuration has no `multicluster` block. Spoke policies grant nothing without it. |
| `SpokeClusterNotFound` | `False` | No `SpokeCluster` has the name in `SpokePolicy.spec.cluster`. This is usually temporary after the deletion of a `SpokeCluster`, until garbage collection removes its objects. |
| `SpokeClusterInvalid` | `False` | Querdian cannot trust the `SpokeCluster`: its name is equal to `multicluster.hubCluster`, the last path segment of `spec.identity` is not its name, or the name occurs more than one time. Fix the `SpokeCluster` on the hub. |
| `PolicyTooLarge` | `False` | A list or a string of the spec is over a [size limit](#size-limits), for example more than 64 rules, a matcher value of more than 16384 characters, or a regex whose compiled program is too large. The message names the field. The CRD rejects such objects, but an object that was stored before the CRD had the limits can still exist. Make the policy smaller, or split it into several policies. |
| `InvalidSpokePolicy` | `False` | The `SpokePolicy` object itself is not correct: the kind, the namespace and the spec do not agree, a name is not valid, or the object name is not the name that the receiver gives (`sp-` and a hash). The receiver did not write such an object. Delete it. |

With the `multicluster` block, the leader also writes the `Ready` condition of each `SpokePolicy` on
the hub (`kubectl get sp`).

* At the next heartbeat, the `querdian agent` copies the conditions of the policies of its spoke back
  to the spoke objects. Thus spoke teams use `kubectl get cap,ap` as usual.
* The copy does not include the message of `CrossTenantUnionUnsupported`, because the message can show
  the grants of a shared subject in other tenants. Such a policy shows `Compiled` in the spoke.
* A policy of an expired spoke stays `Compiled`. Querdian checks the expiry for each request, not at
  compile time.

If the leader replica is not available, Querdian does not update the statuses. All replicas continue to
enforce their own snapshot.

## Size limits

The CRDs limit the size of each policy. The compiler and the receiver apply the same limits. The
compiler applies them also to objects that were stored before the CRD had them. A policy over a limit grants nothing and gets the
status reason `PolicyTooLarge`. Lengths count characters. The same limits apply to the policies of a
spoke (`SpokePolicy`). For the limits of each request, see
[operations](operations-reference.md#limits-and-what-happens-when-they-trip).

| Field | Limit |
|---|---|
| `subjects.users`, `subjects.groups` | 256 items each, 1024 characters each item |
| `rules` | 64 |
| `rules[].backends` | 16 items, 256 characters each item |
| `rules[].tenants` (`ClusterAccessPolicy`) | 64 items, 256 characters each item |
| `rules[].matchers` | 64 |
| `matchers[].name` | 256 characters |
| `matchers[].value` | 16384 characters |
| `labels.deny`, `labels.redact` | 64 items each, 256 characters each name |
| a `RegexMatch` or `RegexNoMatch` value | 8 KiB and weight 16384 (see below); not checked by the CRD |
| all regex values of one policy | weight 32768 together; not checked by the CRD |

The **weight** of a regex measures the cost of its compile. It counts the instructions of the compiled
program, the ranges of its character classes and other parts of the cost. The compiler and the
receiver measure it without a compile. Normal regexes such as `[a-z]+`, `prod-.*` or an alternation of
100 namespace names have a weight of some tens to some thousands. A short regex with many counted
repetitions or many Unicode classes can be over the limit. The weight of an `Equal` or `NotEqual` value
is its length plus one. For the full calculation, see [Regex weight](policy-reference.md#regex-weight).
