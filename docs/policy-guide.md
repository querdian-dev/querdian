# Policy guide

This guide describes how access is granted with the `policy.querdian.dev/v1alpha1` custom
resources and how Querdian turns those grants into rewritten queries. It documents the behaviour
of the code in this repository, not the plan. Companion documents:
[operations](operations.md) (static configuration), [limitations](limitations.md),
[troubleshooting](troubleshooting.md), [threat model](security/threat-model.md).

> **Read this first: namespace-label integrity**
>
> An `AccessPolicy` grants access to the data whose *namespace label* equals the policy's
> namespace. Querdian cannot verify that label. It trusts whatever is stored in the telemetry.
>
> * The label named by `backends[].namespaceLabel` (for example `namespace` in Prometheus,
>   `k8s_namespace_name` in Loki, `resource.k8s.namespace.name` in Tempo) **must be set by your
>   collection pipeline** (Prometheus scrape/relabel configuration, OpenTelemetry Collector
>   `k8sattributes`, Alloy/Promtail), derived from where a pod really runs.
> * It **must not be possible for a workload to choose or override that label**. A pod that can
>   emit metrics, logs or spans with `namespace="payments"` (for example through a push endpoint,
>   an OTLP exporter that is not overwritten by the collector, or `honor_labels: true`) makes its
>   data appear in another team's view. It cannot read more through this, but it can inject
>   misleading data into other teams' dashboards. Conversely, data without the label is invisible
>   to every `AccessPolicy`. The proxy cannot detect either case.
> * Whoever can create or edit an `AccessPolicy` in namespace `X` decides who may read the data
>   labelled `X`. Whoever can edit the labels of a `Namespace` decides which backend tenant its
>   policies map to (`policy.querdian.dev/tenant`). Restrict Kubernetes RBAC for both
>   accordingly. Only platform administrators should be able to create `ClusterAccessPolicy`
>   objects, which can grant `unrestricted` access.
>
> If you cannot guarantee the label integrity, use `ClusterAccessPolicy` with matchers on labels
> you do control, or separate tenants on the backend.

## Overview

* Policies only **grant**. There are no deny policies. The only narrowing mechanism is
  `labels.deny` (see [Denied labels](#denied-labels)). A policy that is invalid grants nothing.
* The access of a request is the **exact union** of all rules of all policies that match the
  caller's username or any of the caller's groups for the requested backend. Nothing matched means
  `403 no_grant`.
* Policies are compiled in the background into an immutable snapshot (about 250 ms after a change)
  and swapped atomically. Requests never see a half-applied change.

## Resources

### Subjects

```yaml
subjects:
  groups: [order-service-team]   # matched against the groups claim of the token
  users: [alice@example.com]     # matched against the username claim of the token
```

At least one group or user is required (enforced by the CRD). Users and groups are separate
keyspaces: a user whose name equals a group name receives nothing from the group's policies.
Matching is exact and case sensitive. Which token claims carry the username and groups is set in
the [static configuration](operations.md#auth).

### ClusterAccessPolicy (cluster scoped, short name `cap`)

For platform administrators. A rule may use arbitrary matchers, name tenants and be `unrestricted`.

| Field | Meaning |
|---|---|
| `backends` | Names of backends from the static configuration (at least one). |
| `tenants` | Backend tenants the rule applies to. **Required for every backend that has a `tenantHeader`**, ignored for backends without one. |
| `unrestricted` | `true` grants all data of the listed tenants. Must not be combined with `matchers` (and the CRD requires exactly one of `unrestricted: true` or a non-empty `matchers`). |
| `matchers` | Label constraints, ANDed. |
| `labels.deny` | Labels hidden from this grant, see below. |

### AccessPolicy (namespaced, short name `ap`)

For teams. Compared to a cluster rule:

* Every rule is implicitly ANDed with `<backend.namespaceLabel> = "<metadata.namespace>"`. This
  matcher is placed first and cannot be removed or widened by the policy author.
* There is no `unrestricted` and no `tenants`.
* The backend tenant is read from the label `policy.querdian.dev/tenant` of the policy's
  namespace, but only for backends that have a `tenantHeader`. A missing or blank label makes the
  whole policy invalid with `TenantNotAssigned`.
* `matchers` are optional; without them the rule grants everything in the namespace.

### Matchers

```yaml
matchers:
  - {name: service, op: NotEqual, value: ""}
```

| `op` | Meaning | PromQL / LogQL rendering |
|---|---|---|
| `Equal` | label equals value | `=` |
| `NotEqual` | label differs from value | `!=` |
| `RegexMatch` | label matches the regex | `=~` |
| `RegexNoMatch` | label does not match the regex | `!~` |

* Regexes use RE2 syntax and are **fully anchored** (`^(?:value)$`), as in Prometheus and Loki.
  A regex that does not compile makes the policy invalid (`InvalidMatcher`).
* Valid names depend on the backend type of each backend the rule lists. A rule that lists several
  backends is validated against each of them, so a name that is invalid for one makes the whole
  policy invalid.
  * `prometheus` and `loki`: `[a-zA-Z_][a-zA-Z0-9_]*`. The reserved name `__tenant_id__` is rejected.
    Loki labels with dots (such as `k8s.namespace.name`) must be referred to by their normalised
    name (`k8s_namespace_name`).
  * `tempo`: attributes with an explicit scope, `resource.<name>` or `span.<name>`, where the
    name may contain letters, digits, `_`, `.`, `-` and `/` (for example
    `resource.k8s.namespace.name`). Unscoped `.name` is not accepted in policies.
* Absent labels behave like empty values in PromQL and LogQL (`service != ""` excludes series
  without a `service` label). **Tempo differs**: see [Tempo semantics](#tempo-missing-attributes).

### Examples that work

The three examples below are valid against the sample static configuration of
[operations.md](operations.md#sample-configuration) (`metrics` = Prometheus without tenant header,
`logs` = Loki with `X-Scope-OrgID`, `traces` = Tempo with `X-Scope-OrgID` and `traceByID: filter`).
They were compiled and resolved against the real policy compiler while writing this guide.

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

With `kubectl label namespace order-service policy.querdian.dev/tenant=tenant1`, a member of
`order-service-team` receives on:

| Backend | Effective policy |
|---|---|
| `metrics` | `namespace="order-service", service!=""` |
| `logs` | tenant header `tenant1`, `k8s_namespace_name="order-service", service!=""` |
| `traces` | tenant header `tenant1`, `resource.k8s.namespace.name = "order-service"` |

A member of `platform-admins` is unrestricted on all three (tenant header `tenant1|tenant2` on
`logs` and `traces`). A member of `sre` has access to `metrics` only; `logs` and `traces` answer
`403 no_grant`. Without the tenant label on the namespace, `order-team` is marked
`TenantNotAssigned` and, because a policy is accepted or rejected as a whole, **also loses its
`metrics` rule**.

## Union semantics in plain language

A user can match many rules at once (through several groups, a personal rule and group rules,
several backends). Querdian combines them as follows.

1. Every rule becomes one *term*: a list of matchers that must all hold (AND), plus its tenant.
2. The user's access is *any* term (OR). A series, log line or span is visible if at least one term
   admits it. This is the **exact union**; Querdian never widens it (for example by dropping a
   matcher) and never narrows it, except by the denied labels described below.
3. The set of terms is simplified to an equivalent smaller one (disjunctive normal form, DNF), in
   this order:
   1. **Unrestricted absorbs, per tenant.** An unrestricted rule admits everything of its tenant, so
      it makes that tenant's other terms redundant. The user is `Unrestricted` only if *every*
      tenant involved has an unrestricted rule. Unrestricted on `tenant1` never widens a restricted
      grant on `tenant2`.
   2. **Duplicates** are removed.
   3. **Subsumption.** If term A has a subset of the matchers of term B (same tenant), A is broader
      and B is dropped.
   4. **Single-label merge.** Terms of the same tenant that are identical except for one label, which
      uses `Equal` or `RegexMatch` in all of them, merge into one regex alternation.
      `namespace="a"` and `namespace="b"` become `namespace=~"(?:a)|(?:b)"`. This repeats until nothing
      changes.
   5. **Disjointness** is computed: all term pairs are disjoint if they have different tenants, or
      both have an `Equal` matcher on the same label with different values. It decides whether summed
      Loki statistics are exact (see [limitations](limitations.md#approximate-results)).
   6. **Cap.** More than `limits.maxPolicyTerms` terms (default 16) after simplification is rejected
      with `403 policy_too_complex`.
4. Each backend then applies the terms in the way it can express them: injecting one term into every
   selector, or-lifting a query over several terms, expanding `match[]` selectors, injecting a
   DNF filter (Tempo), or sending one request per term and merging (Loki).

Example: `alice@example.com` in groups `sre` and `order-service-team` has, on `metrics`, the terms
`namespace="order-service", service!=""` and `namespace=~"kube-system|ingress-nginx|cert-manager"`.
They are not mergeable (different shape), so two terms remain. A query `up` becomes
`(up{namespace="order-service",service!=""} or up{namespace=~"kube-system|ingress-nginx|cert-manager"})`.

Properties worth remembering:

* More rules never reduce access. The only things that *narrow* are `labels.deny` and an invalid
  policy.
* Denied labels of a matched rule apply to the whole request, see below.
* A user without any matching rule for the backend is denied, even if they have rules on other
  backends.
* The result is deterministic: the same inputs in any order give the same terms.

## Tenants

A backend with a `tenantHeader` (Loki, Tempo, Mimir) is multi-tenant. Querdian sets that header on
every upstream request from the effective policy and strips any client-supplied value.

* Rules name tenants (`ClusterAccessPolicy`) or inherit one from the namespace label
  `policy.querdian.dev/tenant` (`AccessPolicy`).
* One tenant: the header carries that tenant and terms need no tenant binding.
* Several tenants: the header is `t1|t2|...` (the federation syntax of Loki, Tempo and Mimir) and
  the backend itself must have federation enabled (Loki `multi_tenant_queries_enabled`, Mimir
  `tenant_federation.enabled`, Tempo `multitenancy_enabled` plus
  `query_frontend.multi_tenant_queries_enabled`).
* Backends without a `tenantHeader` ignore `tenants` completely.

### Cross-tenant behaviour per backend

When a user's rules span several tenants, three cases exist:

1. The user is unrestricted on every tenant: the federated header is set, nothing else changes.
2. The terms are **identical in every tenant** (for example the same `AccessPolicy` shape in two
   tenants): the federated header is set and the terms are used without tenant binding.
3. The terms **differ per tenant**. This is only possible if the backend can filter by tenant.
   With `tenantFederation: tenantIdLabel` each term gets an extra matcher
   `__tenant_id__="<tenant>"`. With `tenantFederation: none` the request is rejected with
   `403 cross_tenant_union_unsupported`.

What the research (`docs/research/tenant-federation.md`) found, which is also the recommended
`tenantFederation` value:

| Backend | Federated queries | `__tenant_id__` filter | Use |
|---|---|---|---|
| Loki 3.5.0 | yes | yes, as label matcher (`=`, `=~`, `!=`) | `tenantIdLabel` |
| Mimir 2.16.0 | yes | yes | `tenantIdLabel` |
| Tempo 2.8.1 | yes (search, trace by ID, tags) | **no**, a filter on it silently matches nothing | `none` |
| Thanos / Prometheus | not applicable, no tenant header | n/a | `none` (leave `tenantHeader` empty) |

Practical consequences:

* Under `tenantIdLabel`, `__tenant_id__` is only injected when the header carries several tenants.
  Loki rejects a selector whose only matcher is `__tenant_id__`; Querdian therefore requires every
  selector to keep at least one other matcher and otherwise answers `400 unsupported`. A user query
  always has one (Loki requires it); the case can arise for label or statistics endpoints without
  a `query` when a restricted tenant-bound term has no other matcher.
* On Tempo, give users with grants in several tenants the *same* rule shape in each tenant, or
  accept `403 cross_tenant_union_unsupported`. Tempo does not fan out per tenant.
* The `CrossTenantUnionUnsupported` status (below) warns you at policy level which backends are
  affected.
* Behaviour with duplicate tenants (`t1|t1`) and unusual tenant IDs has not been tested.

## Denied labels

`labels.deny` lists label names (Prometheus, Loki) or scoped attributes (Tempo, for example
`span.http.url`). The union rule for them is conservative: **a label denied by any rule that
matches the request is denied for the whole request, for all terms**, even if another matching rule
does not deny it. Names are validated like matcher names, so an invalid name makes the policy
invalid (`InvalidMatcher`).

### What it does

* **Queries are rejected with `403 denied_label`** if they reference a denied label:
  * PromQL (AST based): in any selector matcher, `by`/`without`, `on`/`ignoring`,
    `group_left`/`group_right`, and the label-name arguments of `label_replace`, `label_join` and
    `count_values`. This applies to unrestricted users too.
  * LogQL (token based, conservative): any identifier or keyword token with that spelling outside
    string literals, anywhere in the query (selector, pipeline, grouping, `unwrap`); the `dst` and
    `src` string arguments of `label_replace`; and any `line_format` or `label_format` template that
    contains the denied name as a substring, or that can read all labels without naming them
    (`{{ . }}`, `range`, `with`, `template`, `block`, `define`, `$` variables).
  * TraceQL: any attribute or intrinsic token naming it. A scoped denied name (`span.x`) denies
    `span.x` and the unscoped `.x`; an unscoped query token `.x` is denied by `span.x` and by
    `resource.x`.
* **Endpoints that list labels no longer list them**, and endpoints that ask for the values of a
  denied label answer 403:
  * Prometheus `labels`: names removed from the response. `series`: the labels are removed from
    every returned label set (sets that become identical are collapsed). `label/<denied>/values`:
    403. A denied label in `match[]`: 403.
  * Loki `labels`, `detected_labels`, `detected_fields`: names removed; `label/<denied>/values`
    and `detected_field/<denied>/values`: 403; `index/volume*`: denied labels removed from the
    reported label sets, `targetLabels` naming a denied label: 403.
  * Tempo `api/v2/search/tags`: denied tags removed; `api/v2/search/tag/<denied>/values`: 403.
* **Trace by ID strips them**: with `traceByID: filter`, denied attributes are removed from the
  span, event, link, scope and resource attributes of every span that is returned.

### What it does NOT do

* **Query results keep their labels.** `up`, `{app="x"}` or a Tempo search return the complete
  label set / attributes of every series, stream or span. A denied label hides the *ability to
  filter, group or join on it*, not the data. If `customer_id` is denied, `up` still shows
  `customer_id="42"` on the series it returns. Query results and Tempo search results are not
  filtered. To hide the values as well, use [`labels.redact`](#redacting-values); otherwise do not
  put the value in a label the user can read, or constrain what they can read with matchers.
* It does not hide a label from the policy itself: a rule's own matchers may use a label that the
  same rule denies.
* A LogQL log line that *contains* the value is returned as is. Parsers such as `| json` can turn
  line content into labels, but referring to the denied name is rejected.
* Prometheus `metadata`, `rules`, `targets` and the other endpoints that cannot be restricted by
  label are denied altogether, independent of `labels.deny`.
* The LogQL scan is intentionally broad and has false positives, see
  [limitations](limitations.md#denied-label-scan-false-positives).

### Redacting values

`labels.redact` is the opt-in that closes the gap above: it rewrites the values of a label in
query results. Every redacted name is **also denied**, so everything in [What it does](#what-it-does)
applies to it as well: without that, a user could filter on `customer_id="42"` and learn the value
from whether data comes back.

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
| `Placeholder` | `redacted-1`, `redacted-2`, ... per distinct value, numbered per response in random order | which entries of one response share a value |
| `Hash` | `redacted-` + 16 hex digits, HMAC under a key that is random per request | as `Placeholder`; never comparable across requests |
| `IPv4Prefix` (`octets`: 1–3) | `10.1.x.x`; a value that is not an IPv4 address becomes `redacted` | the kept octets |

* **Where it applies**: Prometheus `query`, `query_range`, `query_exemplars`; Loki `query`,
  `query_range` (stream labels and the `structuredMetadata` / `parsed` entry metadata of the
  categorize-labels encoding) and `tail`; Tempo `search`, `metrics/query`, `metrics/query_range`
  (their `promLabels` string is dropped) and trace by ID (`traceByID: filter`). The `series`,
  label and index endpoints remove redacted names exactly like denied ones.
* **Several rules**: like `deny`, a redaction from any matched rule applies to the whole request.
  If rules disagree on the strategy for a label, the strictest one wins: `Remove`, then
  `Placeholder`/`Hash`, then `IPv4Prefix` with fewer octets. A rule that only denies a label does
  not weaken another rule's redaction of it.
* **Fail closed**: a response that cannot be decoded, or that is larger than
  `limits.maxMergeBytes`, is answered with `502` and never passed on. The upstream is asked for
  JSON.
* **Cost**: redacted responses are buffered in full before they are sent, instead of streamed.
  Very large range queries can hit `limits.maxMergeBytes` (`502 merge_too_large`).
* **What it does not redact**: Loki log *lines* (`| json` can make line content into labels, and
  nested keys get other names, such as `ctx_customer_id`), values copied into other labels at
  ingestion, the number and order of returned series, and the Tempo trace-level fields covered in
  the [threat model](security/threat-model.md).

### Tempo: missing attributes

Policy matchers on Tempo behave like TraceQL does, which differs from Prometheus and Loki. In
TraceQL a span that lacks an attribute satisfies **none** of `=`, `!=`, `=~`, `!~` (verified against
Tempo 2.9.0; an attribute of non-string type never satisfies a string comparison either). So
`resource.team != "x"` admits only spans that *have* a `team` attribute with another value, whereas
the same `NotEqual` matcher in Prometheus also admits series without the label. The Tempo
rendering can therefore only under-grant. Trace by ID uses exactly the same rule in-process. If
you want "label absent" semantics on Tempo, express them on an attribute that is always set.
Unscoped matchers are not accepted in policies precisely so that this stays predictable.

## Status conditions

Every policy gets one condition `type: Ready` (shown by `kubectl get cap,ap`) written by the
leader replica. `status.observedGeneration` is the generation that was compiled; a status for an
older generation is never written over a newer spec.

| Reason | `Ready` | Meaning and fix |
|---|---|---|
| `Compiled` | `True` | The policy is active. |
| `InvalidMatcher` | `False` | A regex does not compile, a matcher or denied-label name is invalid for the backend type (for example `__tenant_id__`, a name with dots on Prometheus/Loki, an unscoped name on Tempo), an operator is unknown, or an `unrestricted` rule also lists matchers. Also raised if the backend's `namespaceLabel` is not a valid name for its type. The message names the rule and field. Fix the matcher. |
| `UnknownBackend` | `False` | A rule lists a backend that is not in the static configuration. Fix the name or the configuration (changing the configuration needs a restart). |
| `TenantRequired` | `False` | A `ClusterAccessPolicy` rule targets a backend with a `tenantHeader` but lists no `tenants`. Add `tenants`. |
| `TenantNotAssigned` | `False` | An `AccessPolicy` targets a backend with a `tenantHeader`, but its namespace lacks a non-blank label `policy.querdian.dev/tenant`. Label the namespace; the snapshot is rebuilt on label changes. |
| `CrossTenantUnionUnsupported` | `True` | The policy is active, but it contributes to a user whose grants span several tenants with differing rules on a backend with `tenantFederation: none`. Those users' requests are answered `403 cross_tenant_union_unsupported`. The message lists the affected backends. Informational. |

All reasons except `Compiled` and `CrossTenantUnionUnsupported` mean the policy grants nothing, on
all its backends: one invalid rule disables the whole policy. `kubectl get cap` and `kubectl get ap`
show `Ready` and the reason. The same counts are exported as `querdian_policies{kind,ready}`.

If the leader replica is not available, statuses are not updated, but all replicas keep enforcing
their own snapshot.

## Which strategy does what

For an overview of how the effective terms are applied (details in
[promql-enforcement.md](promql-enforcement.md), [limitations](limitations.md) and
[endpoints.md](endpoints.md)):

| Backend | Single term | Several terms |
|---|---|---|
| Prometheus: `query`, `query_range`, `query_exemplars` | matchers appended to every selector (`inject`) | or-lifting: `(v{t1} or v{t2})` per selector or per function over a range vector (`or_lift`); one upstream request |
| Prometheus: `series`, `labels`, `label/<n>/values` | one `match[]` per user selector (`match_expand`) | one `match[]` per (user selector x term); one request, the API returns the union |
| Loki: log queries | matchers injected into the selector (`inject`) | **one upstream request per term**, results merged, deduplicated and cut to `limit` (`fanout`) |
| Loki: metric queries | `inject` | or-lifting per range aggregation (`or_lift`); one request |
| Loki: `labels`, label values, `detected_*` | inject into the optional `query` | one request per term, union of names (`fanout`) |
| Loki: `series` | `match_expand` | `match_expand` (one request) |
| Loki: `index/stats`, `index/volume*`, `patterns` | inject | one request per term, counters summed; an estimate unless the terms are disjoint |
| Loki: `tail` | one upstream WebSocket | one WebSocket per term, frames merged and deduplicated |
| Tempo: `search`, tags, tag values, `metrics/*` | the policy is injected into every spanset filter | same: a parenthesised DNF `(a && b) \|\| (c && d)` is injected; no fan-out |
| Tempo: trace by ID | response filter | response filter |

An unrestricted user (or a user with a term without matchers) with no denied labels is forwarded
without query rewriting or parsing; only the tenant header and the parameter allow-list apply.
Omitting the enforcement parameter (`query`, `q`, `match[]`) never means pass-through for a
restricted user: a policy-only selector is sent instead.
