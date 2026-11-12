# PLAN — Multi-tenant access proxy for Prometheus, Loki and Tempo

This document is the single source of truth for building the project. Agents working from
`TASKS.md` must read this file first. If a task and this plan disagree, this plan wins; if
something is genuinely unspecified, stop and raise it instead of guessing.

---

## 0. Naming

The project is called **Querdian** (query + guardian — the gatekeeper between users
and their data).

| Item | Value |
|---|---|
| Project / binary / Helm chart | `querdian` |
| Prometheus metric prefix | `querdian_` |
| Span attribute namespace | `querdian.*` |
| CRD API group | `policy.querdian.dev` |
| GitHub org | `querdian-dev` |
| Repository | `github.com/querdian-dev/querdian` |
| Go module path | `github.com/querdian-dev/querdian` |
| Container image | `ghcr.io/querdian-dev/querdian` |
| Helm chart (OCI) | `oci://ghcr.io/querdian-dev/charts/querdian` |
| Git identity | `geilername <maximilian.marschall@3xm.at>` |

The project lives in its own org from day one, so the module path stays stable through a
future CNCF submission (CNCF is added as org owner on acceptance).

The CRD group is a Kubernetes naming convention and does not require owning the domain, but
registering `querdian.dev` (or switching the group to a domain you own) avoids future collisions.

---

## 1. Goal

An Apache-2.0, Kubernetes-native reverse proxy that enforces label-based access control on
read queries against Prometheus/Thanos (PromQL), Loki (LogQL) and Tempo (TraceQL).

- Policies are Kubernetes CRDs.
- Queries are rewritten with parsers, never with regex/string surgery.
- Default deny: unknown endpoints, unknown users, unparseable queries → rejected.
- Access is the **exact union** of all policies a user matches — never more, never less.
- Built to be submittable to the CNCF (Sandbox) from day one.

### Non-goals

- Compatibility with evoila/janus (config, API, code). It is credited as inspiration only.
- Write paths (push / ingest / remote_write / OTLP ingest).
- Ruler / alerting / admin APIs.
- A UI, with one exception: the optional, read-only policy dashboard (see docs/dashboard.md). It has no
  write path, is off by default and never contacts a backend.
- Running outside Kubernetes.

---

## 2. Ground rules (non-negotiable)

1. **License: Apache-2.0.** Every dependency must be Apache-2.0 or on the
   [CNCF allowlist](https://github.com/cncf/foundation/blob/main/allowed-third-party-license-policy.md)
   (MIT, BSD-2/3, ISC, …). CI enforces this with `go-licenses`; AGPL/GPL/LGPL/MPL/SSPL/BUSL fail the build.
2. **Clean room.**
   - Do **not** import, copy, or read the source of `grafana/loki` (except Apache-licensed dirs
     `pkg/logproto`, `pkg/loghttp`, `pkg/logqlmodel`, `pkg/push`) or `grafana/tempo` (except
     `pkg/tempopb`). In particular never open Loki's or Tempo's grammar/parser sources.
   - Do **not** copy anything (code, tests, fixtures, docs, images) from `evoila/janus`.
   - The LogQL and TraceQL parsers are written **only from the public language documentation**
     (grafana.com/docs). Record the doc URLs used in package docs.
   - Running Loki/Tempo containers as black boxes in tests is allowed.
3. **DCO.** Every commit is signed off (`git commit -s`). CI checks it.
4. **Fail closed.** Any error in auth, policy resolution, parsing, rewriting or merging results
   in a rejection (4xx/5xx), never in an unenforced pass-through.
5. **No silent over-grant.** If an exact union cannot be expressed for a request, reject with a
   clear reason.
6. **Go.** Current stable Go release; `go.mod` pins it. No cgo. Standard library first.

---

## 3. Architecture

```
Grafana ──Bearer JWT, traceparent──▶ querdian
  │
  ├─ auth        OIDC verification (go-oidc), claim extraction (username, groups)
  ├─ resolve     user + groups + snapshot + backend → Effective policy (normalised DNF + tenants)
  ├─ routes      per-backend endpoint table: enforce(param) | safe | deny (default)
  ├─ enforce     language-specific rewrite → Plan (single request or fan-out set + merge kind)
  │                promql   prometheus/promql/parser (Apache-2.0)
  │                logql    pkg/logql   (own, clean-room)
  │                traceql  pkg/traceql (own, clean-room)
  ├─ proxy       execute Plan: ReverseProxy (streaming) or fan-out + merge
  └─ upstream    Prometheus/Thanos · Loki · Tempo   (tenant header set by proxy only)

controller (same binary): informers on ClusterAccessPolicy, AccessPolicy, Namespace
  (+ SpokeCluster, SpokePolicy with the multicluster block, §18)
  → debounce (250ms, max wait 2s) → compile Snapshot → atomic.Pointer swap
  → status conditions (leader-elected writer)
  SpokeCluster informer → deadline table (outside the snapshot; checked by resolve, §18.6)

multi-cluster (§18), same binary, other subcommands:
  querdian agent     (each spoke)  reads spoke policies → mTLS sync → hub
  querdian receiver  (hub)         writes SpokePolicy and SpokeCluster.status only
```

### Request lifecycle

1. `otelhttp` server span starts; trace context extracted.
2. If snapshot not ready → `503`. With the `multicluster` block, a replica is also not ready until the
   deadline table has its first complete state.
3. Route lookup by backend prefix + path. No match → `403 endpoint not allowed`.
4. Authenticate (Bearer JWT). Failure → `401`. All routes require authentication.
5. Resolve effective policy for (user, groups, backend). No grant → `403`. With the `multicluster`
   block, rules of an expired spoke give no terms; if no term remains → `403 policy_source_expired`.
6. `safe` routes → proxied unchanged, tenant header set from the effective policy.
7. Normalise request (§9). Violations → `400`.
8. Enforcer builds a `Plan` from the query + effective policy. Parse/rewrite failure → `400`.
9. Execute plan (single proxied request, or fan-out + merge). Upstream error → `502`.
10. Response streamed back (single) or written after merge (fan-out). `X-Trace-Id` always set.

### Repository layout

```
api/v1alpha1/            CRD Go types, deepcopy (controller-gen)
cmd/querdian/         main, subcommands `receiver` and `agent`
config/crd/              generated CRD manifests
internal/auth/           OIDC verifier, claims, middleware
internal/config/         static config file + flags
internal/controller/     informers, debounce, snapshot build, status writer
internal/policy/         matcher/rule compile, DNF normalisation, resolver, snapshot types
internal/routes/         endpoint tables per backend type
internal/enforce/        Enforcer interface, Plan types
internal/enforce/promql/ PromQL enforcer
internal/enforce/logql/  LogQL enforcer (uses pkg/logql)
internal/enforce/traceql/TraceQL enforcer (uses pkg/traceql)
internal/fanout/         parallel upstream execution + merge strategies
internal/proxy/          reverse proxy, request normalisation, error writer
internal/observability/  tracing, metrics, logging setup
internal/server/         HTTP servers wiring, health, readiness
internal/spokesync/      multi-cluster wire protocol: request types, digest, object name, identity check
internal/receiver/       `querdian receiver`: hub side of the policy transport (§18.5)
internal/agent/          `querdian agent`: spoke side of the policy transport (§18.5)
pkg/logql/               public: LogQL lexer, parser, injector (Apache-2.0, reusable)
pkg/traceql/             public: TraceQL lexer, parser, injector (Apache-2.0, reusable)
charts/querdian/      Helm chart
docs/                    user + operator docs, research notes
test/e2e/                kind + testcontainers suites
hack/                    scripts (quickstart, codegen)
```

---

## 4. API (CRDs) — `policy.querdian.dev/v1alpha1`

Two policy kinds, `ClusterAccessPolicy` and `AccessPolicy`, and two cluster-scoped hub kinds for the
multi-cluster mode (§4.6, §18): `SpokeCluster` and `SpokePolicy`. The policy kinds only **grant**
access; there are no deny policies (except `labels.deny`, which narrows a grant). An invalid policy
therefore grants nothing → fail closed by construction.

### 4.1 ClusterAccessPolicy (cluster-scoped, platform admins)

May set arbitrary matchers, tenants and `unrestricted`. With the `multicluster` block, each rule must
also state its cluster scope for each backend: a cluster matcher that names a fixed set of clusters,
`allClusters: true` or `unrestricted: true` (§18.3). Without the block, `allClusters` has no effect.

### 4.2 AccessPolicy (namespaced, team-owned)

- Every rule is implicitly ANDed with `<backend.namespaceLabel>="<metadata.namespace>"`.
- With the `multicluster` block, every rule also gets `<backend.clusterLabel>="<hubCluster>"` (§18.3).
- No `unrestricted`, no `tenants`.
- Tenant comes from the namespace label `policy.querdian.dev/tenant`. Missing while the backend
  requires a tenant → `Ready=False`, reason `TenantNotAssigned`.

### 4.3 Go types

```go
package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:validation:XValidation:rule="size(self.groups) + size(self.users) > 0",message="at least one group or user required"
type Subjects struct {
	// +optional
	// +kubebuilder:default={}
	Groups []string `json:"groups"`
	// +optional
	// +kubebuilder:default={}
	Users []string `json:"users"`
}

// +kubebuilder:validation:Enum=Equal;NotEqual;RegexMatch;RegexNoMatch
type MatchOp string

const (
	MatchEqual        MatchOp = "Equal"
	MatchNotEqual     MatchOp = "NotEqual"
	MatchRegexMatch   MatchOp = "RegexMatch"
	MatchRegexNoMatch MatchOp = "RegexNoMatch"
)

type Matcher struct {
	// Label (Prometheus/Loki) or attribute (Tempo, e.g. resource.service.name).
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	Op   MatchOp `json:"op"`
	// Regexes are fully anchored (RE2 syntax).
	Value string `json:"value"`
}

type LabelFilter struct {
	// Label/attribute names hidden from label endpoints; queries referencing them → 403.
	// +optional
	Deny []string `json:"deny,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.unrestricted == (size(self.matchers) == 0)",message="set either unrestricted or matchers"
// +kubebuilder:validation:XValidation:rule="!(self.allClusters && self.unrestricted)",message="allClusters and unrestricted are mutually exclusive"
type ClusterRule struct {
	// Backend names from the static config.
	// +kubebuilder:validation:MinItems=1
	Backends []string `json:"backends"`
	// +optional
	Tenants []string `json:"tenants,omitempty"`
	// +optional
	// +kubebuilder:default=false
	Unrestricted bool `json:"unrestricted"`
	// All clusters, also spokes that are added later (§18.3). No effect without multicluster.
	// +optional
	// +kubebuilder:default=false
	AllClusters bool `json:"allClusters"`
	// +optional
	// +kubebuilder:default={}
	Matchers []Matcher `json:"matchers"`
	// +optional
	Labels *LabelFilter `json:"labels,omitempty"`
}

type Rule struct {
	// +kubebuilder:validation:MinItems=1
	Backends []string `json:"backends"`
	// ANDed with the implicit namespace matcher.
	// +optional
	Matchers []Matcher `json:"matchers,omitempty"`
	// +optional
	Labels *LabelFilter `json:"labels,omitempty"`
}

type ClusterAccessPolicySpec struct {
	Subjects Subjects `json:"subjects"`
	// +kubebuilder:validation:MinItems=1
	Rules []ClusterRule `json:"rules"`
}

type AccessPolicySpec struct {
	Subjects Subjects `json:"subjects"`
	// +kubebuilder:validation:MinItems=1
	Rules []Rule `json:"rules"`
}

type PolicyStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cap
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
type ClusterAccessPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterAccessPolicySpec `json:"spec"`
	Status            PolicyStatus            `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=ap
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
type AccessPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AccessPolicySpec `json:"spec"`
	Status            PolicyStatus     `json:"status,omitempty"`
}
```

### 4.4 Status conditions

Single condition type `Ready`. The leader writes it on `ClusterAccessPolicy`, `AccessPolicy` and, with
the `multicluster` block, `SpokePolicy` objects. The agent copies the conditions of spoke policies to
the spoke objects (§18.5). Reasons:

| Reason | Status | Meaning |
|---|---|---|
| `Compiled` | True | Policy is active |
| `InvalidMatcher` | False | Regex does not compile / invalid label name / unknown operator / `unrestricted` with matchers / a `ClusterRule` with `unrestricted: false` and no matchers (the compiler does not rely on CEL) |
| `UnknownBackend` | False | Rule references a backend not in static config |
| `TenantRequired` | False | ClusterRule without tenants for a backend with `tenantHeader` |
| `TenantNotAssigned` | False | AccessPolicy namespace lacks `policy.querdian.dev/tenant` label (spoke: no namespace label and no `defaultTenant`) |
| `CrossTenantUnionUnsupported` | True | Active, but some users' unions across tenants on a backend without federation support will be rejected (informational message lists backends) |
| `InvalidClusterScope` | False | Multi-cluster: a hub `ClusterRule` does not state its cluster scope for a backend, `allClusters` with a cluster matcher or with `unrestricted`, or `allClusters` on a spoke rule (§18.3) |
| `TenantNotAllowed` | False | Multi-cluster: a spoke policy uses a tenant that is not in `SpokeCluster.spec.tenants` (§18.9) |
| `MulticlusterDisabled` | False | A `SpokePolicy` exists, but the static config has no `multicluster` block |
| `SpokeClusterNotFound` | False | No `SpokeCluster` has the name in `SpokePolicy.spec.cluster` |
| `SpokeClusterInvalid` | False | The `SpokeCluster` has the name of `hubCluster`, an identity whose last path segment is not its name, or a duplicate name |
| `InvalidSpokePolicy` | False | The `SpokePolicy` is malformed: kind, namespace and spec do not agree, an invalid name, or an object name that is not `spokesync.ObjectName` |

### 4.5 Examples

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
      tenants: [tenant1, tenant2]
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
  namespace: order-service
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

### 4.6 SpokeCluster and SpokePolicy (hub, cluster-scoped, multi-cluster only)

- `SpokeCluster` (short name `spoke`): one object for each spoke. Its name is the cluster label value
  of the spoke (at most 63 characters). Hub admins write `spec`: `identity` (URI SAN of the agent cert),
  `tenants` (allow-list), `defaultTenant` (must be in `tenants`), `maxStaleness` (positive),
  `suspended`. The receiver writes `status.lastSyncTime` and `status.observedGeneration`.
- `SpokePolicy` (short name `sp`): the hub copy of one spoke policy. The receiver writes `spec`:
  `cluster`, `kind`, `namespace`, `name`, `generation` (of the spoke object), `tenantLabel` and one of
  `clusterAccessPolicy` or `accessPolicy` (the typed specs of §4.3). The object name is
  `spokesync.ObjectName` (`sp-` and a SHA-256 hash). The leader writes `status` (conditions,
  `observedGeneration`, `sourceGeneration`).
- The Go types are in `api/v1alpha1/types.go`. The rules are in §18.4.

---

## 5. Static configuration

Process-level settings are flags; structured settings come from a config file
(`--config=/etc/querdian/config.yaml`, mounted from a ConfigMap, read at startup only).

### 5.1 Config file

```yaml
auth:
  issuer: https://keycloak.example.com/realms/observability
  audiences: [querdian]          # required, non-empty
  caFile: ""                        # optional PEM bundle for the issuer
  claims:
    username: preferred_username    # dotted path allowed
    groups: groups                  # dotted path allowed, e.g. realm_access.roles
backends:
  - name: metrics                   # referenced by policy rules
    type: prometheus                # prometheus | loki | tempo
    url: http://thanos-query.monitoring:10902
    pathPrefix: /prometheus         # proxy route prefix, unique
    namespaceLabel: namespace       # used by AccessPolicy implicit matcher
    tenantHeader: ""                # empty = single-tenant backend
    tenantFederation: none          # none | tenantIdLabel (see §6.4). T0.4: Thanos/Prometheus none; Mimir 2.16.0 tenantIdLabel
  - name: logs
    type: loki
    url: http://loki-gateway.monitoring
    pathPrefix: /loki
    namespaceLabel: k8s_namespace_name
    tenantHeader: X-Scope-OrgID
    tenantFederation: tenantIdLabel # T0.4: Loki 3.5.0 supports __tenant_id__ matcher -> default tenantIdLabel
    forwardHeaders: [Accept, Accept-Encoding, User-Agent, X-Loki-Response-Encoding-Flags] # client headers passed through; this is the Loki default
  - name: traces
    type: tempo
    url: http://tempo-query-frontend.monitoring:3200
    pathPrefix: /tempo
    namespaceLabel: resource.k8s.namespace.name
    tenantHeader: X-Scope-OrgID
    tenantFederation: none          # T0.4: Tempo 2.8.1 has no __tenant_id__ attribute -> default none
    traceByID: filter               # deny | filter
limits:
  maxPolicyTerms: 16                # DNF size cap per request
  maxFanout: 16                     # parallel upstream requests per client request
  maxMergeBytes: 67108864           # 64 MiB cap for buffered merge responses
  maxInflightMergeBytes: 67108864   # 64 MiB cap for buffered fan-out bodies of all requests (default maxMergeBytes)
  upstreamTimeout: 5m
multicluster:                       # optional (§18.4); absent = single-cluster behaviour
  hubCluster: mla                   # cluster label value of hub telemetry
  maxStaleness: 48h                 # default; time limit of spoke policies
# with multicluster, every backend also sets clusterLabel, for example:
#   clusterLabel: cluster                     (prometheus, loki)
#   clusterLabel: resource.k8s.cluster.name   (tempo)
```

Validation at startup (fail to start on error): unique backend names and prefixes, valid URLs,
known types, non-empty audiences, `traceByID` only on tempo, `forwardHeaders` valid header names
and none of the reserved ones (§9). With `multicluster`: `hubCluster` not empty, `maxStaleness`
positive, and on each backend a `clusterLabel` that is valid for the backend type, does not start
with `__` and is not equal to `namespaceLabel`. A `clusterLabel` without the `multicluster` block is
an error (§18.4).

### 5.2 Flags

| Flag | Default | Purpose |
|---|---|---|
| `--config` | `/etc/querdian/config.yaml` | static config file |
| `--listen` | `:8080` | proxy listener |
| `--management-listen` | `:9090` | `/metrics`, `/healthz`, `/readyz` |
| `--leader-elect` | `true` | leader election for status writer |
| `--log-level` | `info` | slog level |
| `--log-format` | `json` | `json` or `text` |
| `--tracing.record-user` | `false` | add `enduser.id` to spans |
| `--tracing.record-queries` | `false` | add original + rewritten query to spans, and the query string to `url.full` of upstream spans |
| `--tracing.trust-inbound-context` | `false` | use the client trace context as parent of the server span (default: new trace with a link) |

Tracing exporter/sampler config uses standard `OTEL_*` environment variables (§11.2).

The subcommands `querdian receiver` and `querdian agent` (§18.5) have their own flags. They do not read
the config file. The flags are in `docs/multicluster-reference.md`.

---

## 6. Policy model

### 6.1 Compilation (per snapshot)

For each policy object:
- Compile matchers (validate label/attribute names per backend type; RE2 compile anchored
  regexes `^(?:…)$`).
- For `AccessPolicy`: append implicit namespace matcher per backend using `namespaceLabel`;
  resolve tenant from namespace label.
- Produce `CompiledRule{policy, backend, tenants, unrestricted, matchers, deniedLabels}`.
- Index by group and by user. Users and groups are separate keyspaces.
- Write status condition per object (leader only).

### 6.2 Resolution (per request)

Input: username, groups, backend. Output:

```go
type Term struct {
	Matchers []Matcher // AND
	Tenant   string    // "" for single-tenant backends
}

type Effective struct {
	Unrestricted bool
	Tenants      []string // sorted, deduped; for the tenant header
	Terms        []Term   // DNF: OR of Terms (empty iff Unrestricted)
	Disjoint     bool     // all term pairs provably disjoint
	DeniedLabels []string // union of labels.deny over matched rules (applies to all terms)
	Policies     []string // names, for tracing/logging
}
```

No matched rule → `ErrNoGrant` → 403.

### 6.3 DNF normalisation (in order)

1. **Unrestricted absorbs, per tenant.** An unrestricted rule absorbs the terms of its own tenant only.
   `Unrestricted=true` (terms dropped) iff every tenant of the matched rules has an unrestricted
   rule; with a single tenant or a single-tenant backend this is "any unrestricted rule". Absorbing
   across tenants would over-grant (unrestricted on t1 must not widen a restricted grant on t2).
   Tenants are collected from all matched rules.
2. **Dedup** identical terms.
3. **Subsumption.** Drop term `b` if another term `a` (same tenant) has a matcher set that is a
   subset of `b`'s (`a` is broader).
4. **Single-label merge.** Two terms (same tenant) whose matchers are identical except for one
   label where both use `Equal`/`RegexMatch` → merge into one `RegexMatch` with alternation
   (`(?:a)|(?:b)`). Repeat to fixpoint.
5. **Disjointness.** Two terms are provably disjoint if they have different tenants, or both have
   an `Equal` matcher on the same label with different values. `Disjoint` = all pairs disjoint.
6. **Cap.** `len(Terms) > maxPolicyTerms` → reject (403, reason `policy_too_complex`).

Before step 1, the resolver counts the terms, matchers and bytes of matcher keys of the matched rules
and rejects the request (403, reason `policy_too_complex`) when a count is over its fixed limit
(`internal/policy/limits.go`). The simplification also has a fixed budget of steps, and the weight of
its result has a fixed limit, with the same result. The weight of a regex bounds its compile cost
(`internal/regexcost`, from a parse with a work budget in `internal/regexparse`, without a compile): the instructions of the compiled program, the rune
ranges and the width of its character classes and the cost of case folding of wide ranges; of an
`Equal`/`NotEqual` value, its
length plus one. The enforcers reject a query (403, reason `query_too_complex`) when the regexes that
the request compiles ((2 x terms + 1) x client regex weight + (2 x selectors + 1) x policy weight) or
the client regexes are over a fixed limit, before they compile a regex. Policy specs have fixed size
limits in the CRD, the compiler (reason `PolicyTooLarge`) and the receiver; the regex weight limits are
in the compiler and the receiver only.

`DeniedLabels` semantics: a label denied by **any** matched rule is denied for the request
(conservative; documented).

### 6.4 Tenants and cross-tenant unions

- Single tenant (or single-tenant backend): tenant header = that tenant; terms have no tenant
  binding.
- Multiple tenants:
  - Tenant header = `t1|t2|…` (federation syntax).
  - If all terms are identical across tenants → no tenant binding needed.
  - Else if backend `tenantFederation: tenantIdLabel` → each term gets an extra matcher
    `__tenant_id__="<tenant>"`.
  - Else → reject (403, reason `cross_tenant_union_unsupported`).
- Which backends support `__tenant_id__` filtering is verified in `T0.4` (Loki and Mimir: label
  matcher works; Tempo: silently matches nothing, so `none`); defaults in docs follow that research.
- The `__tenant_id__` matcher is injected **only** when the outgoing tenant header is a
  federation header (`t1|t2|…`). With a single-tenant header the label does not exist and a
  filter on it silently returns an empty result.
- Loki rejects a selector whose only matcher is `__tenant_id__` (400). The rewriter guarantees at
  least one other non-empty matcher in every selector, otherwise the request is rejected
  (`ErrUnsupported`).

---

## 7. Enforcement

### 7.1 Interface

```go
package enforce

type MergeKind int

const (
	MergeNone          MergeKind = iota // single upstream request, streamed
	MergeLokiStreams                    // log query results
	MergeStringSet                      // labels / label values / detected_*
	MergeSumCounts                      // index/stats, volume, patterns
	MergeTailStreams                    // websocket tail
	MergeTempoSearch                    // search results with metadata redaction
)

type Upstream struct {
	Query url.Values // full param set to send (enforcement param rewritten)
	Term  int        // term index, -1 when not fanned out (for tracing)
}

type Plan struct {
	Requests []Upstream // len 1 unless fanned out
	Merge    MergeKind
	Strategy string // "inject" | "or_lift" | "fanout" | "match_expand" | "passthrough" (tracing)
}

type Enforcer interface {
	Plan(ctx context.Context, ep routes.Endpoint, params url.Values, eff policy.Effective) (Plan, error)
}
```

Errors are typed: `ErrParse` (400 bad_data), `ErrDeniedLabel` (403), `ErrUnsupported` (400 with
reason), `ErrTooComplex` (403).

### 7.2 PromQL (`internal/enforce/promql`, uses `github.com/prometheus/prometheus/promql/parser`)

- Parse; error → `ErrParse`.
- Denied labels: any matcher in the user query or any `by`/`without`/`on`/`ignoring`/
  `group_left`/`group_right`/`label_replace`/`label_join` label referencing a denied label →
  `ErrDeniedLabel`.
- Single term: append term matchers to every `VectorSelector` (including those inside
  `MatrixSelector` and subqueries).
- Multiple terms (**or-lifting**):
  - Instant `VectorSelector` v (not inside a `MatrixSelector`) → `ParenExpr( v∧t1 or v∧t2 … )`.
  - `Call` with a `MatrixSelector` argument f(…, m, …) → `ParenExpr( f(…, m∧t1, …) or f(…, m∧t2, …) )`.
  - Subquery inner expressions are rewritten recursively (they are instant expressions).
  - `offset` and `@` modifiers are preserved on every copy.
  - Known semantic edge: `absent()` / `absent_over_time()` lose label inference from user
    equality matchers. Document; not a leak.
- Metadata endpoints (`series`, `labels`, `label/<n>/values`): emit one `match[]` per
  (user match[] × term) — the API returns the union natively. No user `match[]` → one `match[]`
  per term (`{t_i}`). Label endpoints additionally filter denied labels from responses
  (`labels`) or reject (`label/<denied>/values` → 403).
- Output = `expr.String()`; must re-parse (assert in tests).

### 7.3 LogQL (`pkg/logql` + `internal/enforce/logql`)

`pkg/logql` (clean-room, from public docs only):
- **Lexer:** strings `"…"` with escapes, raw strings `` `…` ``, `#` line comments, numbers,
  durations, bytes, identifiers, operators, braces/brackets/parens. Unknown input → error.
- **Parser:** recursive descent for the **metric layer** (vector aggregations, binary operators
  incl. `or`/`and`/`unless`, range aggregations, `unwrap`, `offset`, `by`/`without`, literals,
  `vector()`, `label_replace`) and for **stream selectors**; the **log pipeline** is tokenised and
  structurally validated (balanced, known stage keywords) but kept as an opaque token sequence.
- Exposes: `Parse(string) (*Query, error)`, query kind (log vs metric), positions of all stream
  selectors and range aggregations, `Inject(*Query, matchers)`, `String()`.

Enforcement:
- Denied labels: any identifier token outside strings that equals a denied label name, anywhere in
  the query (selector or pipeline) → `ErrDeniedLabel` (conservative).
- `label_replace` (and any function taking label names as string arguments) can overwrite or
  remove any label, including policy labels, and its names are strings that the identifier scan
  skips. Its string arguments are therefore scanned for denied labels as well, and the injected
  selectors always sit inside the function so the policy applies before the label is rewritten.
- Single term: inject matchers into every stream selector.
- Metric query, multiple terms: or-lift at range-aggregation level, analogous to PromQL.
- Log query, multiple terms: **fan-out**, one request per term, `MergeLokiStreams`:
  - Same `limit`, `direction`, `start`, `end`, `step` for every branch.
  - Merge streams with identical label sets; dedupe entries by (timestamp, line).
  - Sort by direction, truncate to `limit` (exact: top-L of the union ⊆ union of top-L).
  - `stats` in the response: summed; documented as approximate under overlap.
- `series`: multiple `match[]` as in PromQL.
- `labels`, `label/<n>/values`, `detected_labels`, `detected_fields`: fan-out, `MergeStringSet`
  (union, sorted, denied labels removed).
- `index/stats`, `index/volume`, `index/volume_range`, `patterns`: fan-out, `MergeSumCounts`;
  exact if `Effective.Disjoint`, else documented as estimate.
- `tail` (WebSocket): fan-out of upstream WebSockets, merge frames, dedupe by (stream labels,
  timestamp, line), `MergeTailStreams`.

### 7.4 TraceQL (`pkg/traceql` + `internal/enforce/traceql`)

`pkg/traceql` (clean-room, from public docs only):
- Lexer: strings, durations, numbers, identifiers incl. scoped attributes (`resource.x`,
  `span.x`, `.x`, intrinsics), operators, braces.
- Parser: locate every spanset filter `{ … }` (including inside structural operators
  `>>`, `>`, `~`, `<<`, `<`, `!>>`, …, spanset `&&`/`||`, pipelines, `select()`, metrics
  functions); validate balance; inner filter expression kept as token sequence with position.
- `Inject(*Query, dnf)` rewrites `{ X }` → `{ (X) && (<dnf>) }`, `{}` / `{ }` → `{ <dnf> }`,
  where `<dnf>` = `(a && b) || (c && d)`; attribute names rendered as given in the matcher.

Enforcement:
- Denied attributes: any attribute token equal to a denied name → `ErrDeniedLabel`.
- Search, metrics, tag values (`q` param): inject.
- `api/search` legacy `tags=` parameter without `q` → `ErrUnsupported` (400).
- `&&` and `||` have equal precedence and associate left to right (T0.5). Injection therefore
  parenthesises the original filter, the whole DNF and every disjunct explicitly.
- Search responses (`MergeTempoSearch`, single request still uses this merge for rewriting):
  redact trace-level metadata when the root span is not known to match the policy:
  `rootServiceName`, `rootTraceName` (returned even if only a child span matched) and
  `serviceStats` (lists every service of the trace with span and error counts). `durationMs`
  and `startTimeUnixNano` are trace-level, not root-span-level; handling is decided in T4.4.
  Both `spanSet` and `spanSets` are rewritten. Buffered, capped by `maxMergeBytes`.
- Optional `q` on tag/tag-value endpoints: when absent, a policy-only filter `{ <dnf> }` is
  injected; absence never means pass-through.
- Trace by ID: a client-supplied `q` is rejected (400); only `start` and `end` are forwarded.
- Legacy search parameters `tags`, `minDuration`, `maxDuration` are rejected (400) whether or
  not `q` is present.
- Trace by ID (`api/traces/<id>`, `api/v2/traces/<id>`), per backend `traceByID`:
  - `deny` → 403.
  - `filter` → fetch upstream (protobuf via `pkg/tempopb`, or JSON), drop spans whose
    resource/span attributes do not match the DNF, drop now-empty resource/scope spans; if no
    span remains → 404. Same response format as requested.

---

## 8. Endpoint inventory

Default for any path not listed: **deny (403)**. Exact paths to be verified against current
public API docs in `T0.3`; results go to `docs/endpoints.md` and `internal/routes`.

### 8.1 Prometheus / Thanos (`type: prometheus`), under `pathPrefix` + `/api/v1/`

| Path | Kind | Param |
|---|---|---|
| `query`, `query_range`, `query_exemplars` | enforce | `query` |
| `series`, `labels`, `label/<name>/values` | enforce (match expand) | `match[]` |
| `status/buildinfo`, `format_query`, `parse_query` | safe | — |
| `metadata`, `targets`, `targets/*`, `scrape_pools*`, `rules`, `alerts`, `alertmanagers`, `search/*`, `notifications*`, `features`, `status/*` (other), `admin/*`, `write`, `read`, `otlp/*`, `/openapi.yaml` | deny | — |

`label/<name>/values` is `GET` only (Prometheus docs); all other enforce rows accept `GET` and `POST`.
Details, Thanos-specific parameters and open questions: `docs/endpoints.md`.

### 8.2 Loki (`type: loki`), under `pathPrefix` + `/loki/api/v1/`

| Path | Kind | Param |
|---|---|---|
| `query`, `query_range` | enforce | `query` |
| `series` | enforce (match expand) | `match[]` |
| `labels`, `label/<name>/values` | enforce (fan-out string set) | `query` |
| `detected_labels`, `detected_fields`, `detected_field/<name>/values` | enforce (fan-out string set) | `query` |
| `index/stats`, `index/volume`, `index/volume_range`, `patterns` | enforce (fan-out sum) | `query` |
| `tail` (WebSocket) | enforce (fan-out tail) | `query` |
| `status/buildinfo`, `format_query` | safe | — |
| `push`, `/otlp/*`, rules (`/loki/api/v1/rules*`, `/prometheus/api/v1/*`), `delete`, deprecated `/api/prom/*`, everything else | deny | — |

### 8.3 Tempo (`type: tempo`), under `pathPrefix` + `/api/`

| Path | Kind | Param |
|---|---|---|
| `search` | enforce + metadata redaction | `q` |
| `v2/search/tags`, `v2/search/tag/<name>/values` | enforce | `q` |
| `metrics/query`, `metrics/query_range` | enforce | `q` |
| `traces/<id>`, `v2/traces/<id>` | traceByID | — |
| `status/buildinfo`, `echo` | safe | — |
| `search/tags`, `search/tag/<name>/values` (v1, no `q` support), `v2/traces/diff`, `overrides`, `mcp`, everything else | deny | — |

Grafana compatibility: the backend's `pathPrefix` + upstream API path must be what a Grafana
data source with URL `https://proxy/<pathPrefix>` calls.

---

## 9. Request normalisation and proxying

- Accept `GET` and `POST` (`application/x-www-form-urlencoded`) for query endpoints; `GET` only
  for others; WebSocket upgrade only for Loki `tail`.
- **Parameter allow-list.** Each route template lists the parameters it forwards (e.g. Thanos
  `dedup`, `partial_response`, `replicaLabels`, `max_source_resolution`; never `storeMatch[]`).
  Any other parameter is dropped or rejected (400), decided per endpoint in `docs/endpoints.md`.
- Optional enforcement parameters (`query`, `q`, `match[]` on metadata endpoints): when the client
  omits them the enforcer still injects the policy filter; omission is never a pass-through.
- Plans that merge responses require JSON (`Accept` is forced to `application/json`); Loki
  Parquet and Tempo `application/vnd.grafana.llm` are not merged.
- Merge URL query and form body the way Prometheus does, then:
  - more than one value for a single-valued enforcement param (`query`, `q`) → 400;
  - forward as: GET → rewritten URL query; POST → rewritten form body, empty URL query.
- Forward only allow-listed client headers (`forwardHeaders` per backend; default `Accept`,
  `Accept-Encoding`, `User-Agent`, and for Loki `X-Loki-Response-Encoding-Flags`). Everything else
  is dropped. Credentials, tenant headers (configured `tenantHeader`, `X-Scope-OrgID`,
  `THANOS-TENANT`), `traceparent`/`baggage`, `Forwarded`/`X-Forwarded-*`, hop-by-hop headers such
  as `Upgrade`/`Connection`, `Host` and `Content-*` cannot be allow-listed (startup error). On
  streamed requests `X-Forwarded-*` are re-set by the proxy (client values discarded).
- Set: tenant header from `Effective.Tenants`; `traceparent` (propagation).
- A `101 Switching Protocols` on a non-WebSocket route fails closed (502).
- Single-request plans use `httputil.ReverseProxy` with `Rewrite`, streaming responses.
- Fan-out plans: `internal/fanout` runs requests in parallel (bounded by `maxFanout`), shared
  context cancellation, any branch error → whole request fails (502, upstream error surfaced),
  buffered merge capped by `maxMergeBytes` (exceeded → 502 `merge_too_large`).
- Error format (Prometheus-style, all backends):

```json
{"status":"error","errorType":"forbidden","error":"label customer_id denied by policy","traceId":"4bf92f3577b34da6a3ce929d0e0e4736"}
```

| Status | errorType | When |
|---|---|---|
| 400 | `bad_data` | parse errors, invalid params, unsupported syntax |
| 401 | `unauthorized` | missing/invalid token |
| 403 | `forbidden` | no grant, denied label, endpoint not allowed, too complex, cross-tenant unsupported |
| 502 | `upstream` | upstream failure, merge too large |
| 503 | `unavailable` | snapshot not ready |

---

## 10. Authentication

- `github.com/coreos/go-oidc/v3`: discovery from `issuer`, JWKS caching, `audiences` check
  (any of), expiry, signature. Custom CA via `caFile`.
- Claims extracted by dotted path; groups claim may be string or array of strings.
- Missing username claim → 401.
- No token caching beyond go-oidc's key cache.

---

## 11. Observability

### 11.1 Metrics (`client_golang`, management port)

| Metric | Type | Labels |
|---|---|---|
| `querdian_requests_total` | counter | `backend`, `endpoint`, `decision`, `reason` |
| `querdian_request_duration_seconds` | histogram (native + exemplars) | `backend`, `endpoint`, `decision` |
| `querdian_upstream_requests_total` | counter | `backend`, `code` |
| `querdian_upstream_duration_seconds` | histogram (native + exemplars) | `backend` |
| `querdian_rewrite_duration_seconds` | histogram | `language`, `strategy` |
| `querdian_policy_terms` | histogram | `backend` |
| `querdian_auth_failures_total` | counter | `reason` |
| `querdian_snapshot_generation` | gauge | — |
| `querdian_snapshot_rebuilds_total` | counter | `result` |
| `querdian_policies` | gauge | `kind`, `ready` |
| `querdian_cluster_policy_age_seconds` | gauge | `cluster` |
| `querdian_cluster_policy_expired` | gauge | `cluster` |

`querdian_policies` has the `kind` value `SpokePolicy` only with the `multicluster` block. The two
`querdian_cluster_policy_*` metrics have one series for each `SpokeCluster` in the deadline table
(§18.6).

`endpoint` is the route template (e.g. `label/:name/values`), never the raw path.

### 11.2 Tracing (OpenTelemetry)

- Libraries: `go.opentelemetry.io/otel`, SDK, contrib `otelhttp`, `autoexport`, `autoprop`.
- Configuration exclusively via standard env vars (`OTEL_SERVICE_NAME`,
  `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_TRACES_SAMPLER`,
  `OTEL_TRACES_SAMPLER_ARG`, `OTEL_TRACES_EXPORTER`, `OTEL_RESOURCE_ATTRIBUTES`).
  Default sampler `parentbased_traceidratio`.
- The server span is a public endpoint: it starts a new trace and links the inbound trace context,
  so a client cannot select the trace ID or force sampling. `--tracing.trust-inbound-context`
  makes the inbound trace context the parent.
- Propagation: inbound `tracecontext` + `baggage`; outbound **only** `tracecontext`. The propagator
  is fixed in code (`autoprop` / `OTEL_PROPAGATORS` are deliberately not used) because
  outbound-tracecontext-only is a security rule.

Span tree per request:

```
HTTP <method> <route>                    server span (otelhttp)
├─ authn.verify                          auth.issuer, auth.jwks_cache_hit, error reason
├─ policy.resolve                        snapshot.generation, policies.matched, dnf.terms,
│                                        dnf.unrestricted, dnf.disjoint, tenants.count
├─ enforce.rewrite                       query.language, strategy, selectors.rewritten,
│                                        query.bytes_in, query.bytes_out
├─ upstream [term=N]                     client span (otelhttp transport), one per request
└─ merge                                 merge.kind, items_in, items_out, deduped, truncated
```

Controller root span: `snapshot.rebuild` (trigger, policies.total, policies.invalid,
compile.duration_ms).

Attributes namespace: `querdian.*`; semconv where applicable. Always on the server span:
`querdian.decision` (`allow|deny|error`), `querdian.decision.reason`,
`querdian.policies` (names).

Privacy (default off): `enduser.id` (`--tracing.record-user`), `querdian.query.original` /
`querdian.query.rewritten` (`--tracing.record-queries`). The same flag controls the query string in
`url.full` of the upstream client spans. Without it, a transport pair around the otelhttp transport
removes the query string from the URL that otelhttp sees and puts it back below otelhttp, so
`url.full` has scheme, host and path only (for `GET`, the query string holds the rewritten query).
If the inner transport does not find the query in the request context, it sends nothing (fail closed).
A transport error goes through otelhttp with a fixed text (the span status gets no URL); the outer
transport gives the original error to the caller.

Every response has header `X-Trace-Id`; every error body has `traceId`.

### 11.3 Logging

`log/slog`, JSON by default. Every request log line carries `trace_id`, `span_id`, backend,
route, decision, reason, duration. User identity and queries only logged when the matching
tracing privacy flag is enabled.

---

## 12. Security model

- Trust boundary: the proxy trusts the OIDC issuer and the cluster API server; it trusts nothing
  from the client except the verified token.
- **Multi-cluster trust boundary (§18.3, §18.5).** The proxy reads only the hub API server. It trusts a
  spoke only for the data of that spoke: a spoke policy always gets the cluster matcher of its spoke
  and only the tenants that the hub allows. The receiver is the only component with a write path. It
  trusts only agent certs from its own hub-controlled client CA, and it takes the cluster from the URI
  SAN of the cert. A compromised receiver can grant the data of all spokes, but not hub data. The
  ingest gateway must overwrite the cluster label (§18.8), in the same way as the namespace label
  below.
- **Namespace label integrity:** `AccessPolicy` isolation relies on `namespaceLabel` being set by
  the collection pipeline (Prometheus scrape config, OTel k8sattributes, Promtail/Alloy), not by
  workloads. Must be prominent in docs.
- Tenant headers from clients are never forwarded (client headers are allow-listed).
- Backends must not be reachable by clients directly (NetworkPolicy in the chart).
- Threat model document (`docs/security/threat-model.md`) covers: query injection via
  parser divergence, label oracle attacks (filters on denied labels), metadata leaks (Tempo root
  fields), resource exhaustion (fan-out, merge size), token replay, snapshot staleness, and the
  multi-cluster items (cluster label, receiver, agent cert, time limit).

---

## 13. Testing strategy

| Layer | Tooling | Required for |
|---|---|---|
| Unit | table tests | every package |
| Parser corpus | own test corpus written from public docs | `pkg/logql`, `pkg/traceql` |
| Fuzz | `go test -fuzz` | lexers/parsers: no panics, round-trip `Parse(String(x)) == x` |
| Property | fuzz harness per language | every selector/spanset in output carries policy (or each fan-out branch carries exactly one term) |
| Controller | envtest | snapshot, status, namespace tenant, leader election |
| Differential | testcontainers (Prometheus, Loki, Tempo black boxes) | upstream accepts rewritten query; our parser rejects ⇒ never passthrough |
| Semantic equivalence | testcontainers + seeded data | result(rewritten) == ∪ result(per-term queries) computed in test |
| Tracing | `sdk/trace/tracetest` | span tree present; privacy defaults hold |
| E2E | kind + Helm + Keycloak | isolation across namespaces and tenants via Grafana-like client |

CI runs unit, fuzz (short), envtest and differential on every PR; long fuzz and full E2E nightly.

---

## 14. Packaging and release

- Helm chart `charts/querdian`: CRDs (in `crds/`), Deployment (≥2 replicas), RBAC, Service,
  PodDisruptionBudget, ServiceMonitor (optional), NetworkPolicy (optional), ConfigMap for
  static config, `values.schema.json`, tracing values mapped to `OTEL_*` env + Downward API
  resource attributes.
- RBAC: `get/list/watch` on both CRD kinds and `namespaces`; `update/patch` on `*/status`;
  `coordination.k8s.io/leases` for leader election.
- Image: distroless static, multi-arch (amd64, arm64), non-root, read-only root fs.
- Release: GoReleaser, cosign keyless signing, SBOM (syft), SLSA provenance.
- Versioning: SemVer; API `v1alpha1` until real-world feedback.

---

## 15. CNCF readiness

From day one: Apache-2.0, DCO, CNCF Code of Conduct, `GOVERNANCE.md`, `MAINTAINERS.md`,
`SECURITY.md`, `ADOPTERS.md`, license gate in CI, OpenSSF Scorecard. Before 0.1.0: OpenSSF Best
Practices badge (passing), TAG Security self-assessment draft. Sandbox application after 0.x
with adopters.

`README.md` credits: "Inspired by [evoila/janus](https://github.com/evoila/janus)." Nothing else
from Janus.

---

## 16. Milestones

| Milestone | Scope | Estimate |
|---|---|---|
| M0 Foundations | repo, governance, CI with license gate, research (endpoints, tenant federation, TraceQL details) | 1 week |
| M1 Core | CRDs, policy compile/DNF/resolve, controller, auth, routes, proxy, observability | 2.5 weeks |
| M2 PromQL | enforcer incl. or-lifting, metadata endpoints, tests | 1.5 weeks |
| M3 LogQL | `pkg/logql`, enforcer, fan-out + merges, tail, tests | 3 weeks |
| M4 TraceQL | `pkg/traceql`, enforcer, redaction, traceByID filter, tests | 2 weeks |
| M5 Release 0.1.0 | chart, image, quickstart, docs, dashboard, OpenSSF | 1.5 weeks |
| M6 Multi-cluster | §18: origin in the compiler, time limit, hub controller, receiver, agent (T6.1 to T6.6 done); chart, docs (T6.7) and end-to-end tests (T6.8) | not estimated |

M2, M3 and M4 can run in parallel once M1 is done (separate agents).

---

## 17. To verify (research tasks, results in `docs/research/`)

1. Exact current API paths and parameters for Prometheus, Loki, Tempo (`T0.3`).
2. `__tenant_id__` matcher support under tenant federation: Loki, Tempo, Thanos/Mimir (`T0.4`).
3. Tempo: regex anchoring of `=~`, operator precedence of `&&`/`||` in spanset filters, search
   response fields that expose root span data (`T0.5`).
4. LogQL: `or`/`and`/`unless` support in metric queries, complete pipeline stage keyword list
   from docs (`T0.5`).

---

## 18. Multi-cluster

Status: the code exists (`TASKS.md`, milestone M6, T6.1 to T6.6). The end-to-end tests (T6.8) do not
exist yet. The operations guide is `docs/multicluster.md`. §18.12 lists where the code adds to this
design.

### 18.1 Goal and terms

The **hub** is the central MLA cluster. It runs one Querdian instance and the backends. A **spoke** is
a cluster that sends telemetry to the hub. Each spoke supplies an mTLS cert at ingest, and the ingest
gateway sets a `cluster` label from that cert. Querdian must enforce the policies of all clusters.

### 18.2 Main rule

Querdian gets the cluster matcher from the **origin** of a policy, never from the policy text. The
namespace matcher works the same way (§4.2). The current `AccessPolicy` has no cluster matcher, so a
central deployment needs this design before teams can use `AccessPolicy` safely.

### 18.3 Trust model

| Policy source | Author | Compiled scope |
|---|---|---|
| Hub `ClusterAccessPolicy` | MLA platform admin | Its matchers, as in §4.1. Its scope must be explicit (see below). |
| Hub `AccessPolicy` | Hub team | `cluster="<hubCluster>"`, `namespace="X"`, plus its matchers |
| Spoke `ClusterAccessPolicy` | Spoke admin | `cluster="<spoke>"`, plus its matchers |
| Spoke `AccessPolicy` | Spoke team | `cluster="<spoke>"`, `namespace="X"`, plus its matchers |

- A spoke rule with `unrestricted: true` compiles to the single matcher `cluster="<spoke>"`. A spoke
  rule must never compile to `Unrestricted: true`, because Querdian does not parse the queries of
  unrestricted users.
- Spoke matchers on the cluster label stay ANDed with the implicit matcher, so they can only narrow.
  `mergeSingleLabel` skips a label that occurs twice in a term. A fuzz target must pin this property.
- Under `multicluster`, each hub `ClusterAccessPolicy` rule must state its cluster scope, because a new
  spoke must not silently widen a grant. For each backend of the rule, one of these must be true:
  - The rule has a matcher on the `clusterLabel` of that backend that names a fixed, finite set of
    non-empty cluster names: an `Equal`, or a `RegexMatch` such as `a|b` or `(a|b)-prod`. `NotEqual`,
    `RegexNoMatch` and an open regex such as `.+` or `cluster-.*` do not count, because they also match
    each new spoke. An empty value does not count, because it matches data with no cluster label.
  - The rule sets `allClusters: true`, and it has no matcher on the `clusterLabel` of that backend.
  - The rule sets `unrestricted: true`.

  Otherwise, the policy is invalid (new reason `InvalidClusterScope`) and grants nothing. The field exists
  because `cluster=~".+"` is an open regex, and Tempo tag lookups reject an open regex with `400`.
- A spoke rule with `allClusters: true` is invalid. Without the `multicluster` block, the field has no
  effect, so admins can prepare policies before they set the block.
- A spoke admin can grant the data of that spoke to any subject. The design permits this. All clusters
  use one OIDC issuer, so group names have the same meaning everywhere.

### 18.4 Configuration

Without the `multicluster` block, Querdian works as it does today. It does not start informers for the
new kinds, and a `SpokePolicy` grants nothing.

```yaml
multicluster:
  hubCluster: mla            # cluster label value of hub telemetry
  maxStaleness: 48h          # default
backends:
  - name: metrics
    namespaceLabel: namespace
    clusterLabel: cluster    # required on every backend if multicluster is set
```

Querdian refuses to start in these conditions:

- `hubCluster` is empty. A matcher `cluster=""` also accepts data with no cluster label.
- `maxStaleness` is present with a zero or negative value. If it is absent, the default applies.
- A backend has no `clusterLabel`, or the name is not valid for the backend type.
- A Tempo backend has a `clusterLabel` that does not start with `resource.`. An application can set
  its own span attributes, so a `span.` attribute does not identify the cluster. An application can
  also set resource attributes, for example with `OTEL_RESOURCE_ATTRIBUTES`. Thus the `resource.`
  attribute is correct only if the ingest gateway writes it and overwrites the value of the SDK
  (§18.8), as §12 requires for `resource.k8s.namespace.name`.
- A backend has `clusterLabel`, but the `multicluster` block is absent. Without the block, a hub
  `AccessPolicy` loses its cluster matcher and also matches namespace X of each spoke. This check stops an
  accidental rollback.

Querdian reads the static configuration at startup only, so each spoke has a cluster-scoped hub object:

```yaml
apiVersion: policy.querdian.dev/v1alpha1
kind: SpokeCluster
metadata:
  name: cluster-a            # the cluster label value of this spoke
spec:
  identity: spiffe://mla.example.com/agent/cluster-a  # see 18.5
  tenants: [tenant-a]        # allow-list; see 18.9
  defaultTenant: tenant-a    # optional
  maxStaleness: 168h         # optional override
  suspended: false           # true stops the grants of this spoke; see 18.7
status:
  lastSyncTime: "2026-11-05T12:00:00Z"  # hub clock, written by the receiver
  observedGeneration: 3      # metadata.generation at the last valid sync
```

- Only hub admins can write `spec`. The kind has a status subresource, and the receiver can write only
  `status`.
- If the name of a `SpokeCluster` is equal to `hubCluster`, all policies of that spoke grant nothing.
  Otherwise, a spoke admin could grant hub data.

The hub stores a spoke policy as a cluster-scoped `SpokePolicy`, one object for each spoke policy.

- Its fields are the cluster, the kind, the namespace, the name, the generation of the spoke object, the
  policy spec, and the tenant label value of its namespace.
- The object name is a hash of the cluster, the kind, the namespace and the name, so names cannot collide.
  The receiver does not change or delete an object with a different `cluster` value.
- The spec has typed fields and the same CEL rules as the spoke kinds. For a `ClusterAccessPolicy` rule,
  on the hub and on spokes, the compiler also rejects `unrestricted: false` with no matchers.
  The compiler does not rely on CEL only. An `AccessPolicy` rule with no matchers stays valid.
- Only the receiver can write its `spec`. The Querdian leader writes its `status`.
- The receiver sets an owner reference to the `SpokeCluster`, so a deletion of the `SpokeCluster` also
  removes its objects. A `SpokePolicy` whose cluster has no `SpokeCluster` grants nothing.

### 18.5 Policy transport

```
spoke: [ agent ] --mTLS, full state or digest--> hub: [ receiver ] --> SpokePolicy, SpokeCluster.status
          ^ reads CAP, AP, Namespace                                       | informers
          | from the spoke API server                                      v
                                                                       [ querdian ] snapshot
```

- `querdian agent` runs in each spoke. It only needs an outbound path to the receiver. Its RBAC is
  `get/list/watch` on both policy kinds and `namespaces`, `update/patch` on
  `accesspolicies/status` and `clusteraccesspolicies/status`, and `coordination.k8s.io/leases` in its own
  namespace. It uses leader election, so one agent sends at a time.
- A spoke runs only the agent, never a standalone Querdian as well. Both would write the `Ready`
  condition and `status.observedGeneration` of the spoke policies, and the hub is the only enforcement
  point for spoke policies. The Helm chart refuses to render the agent together with the proxy in one release.
- `querdian receiver` runs on the hub as a separate Deployment, so the read proxy keeps no write path.
  The write path non-goal in §1 is about telemetry; the receiver writes only policy state.
- The receiver uses leader election, and only the leader reports ready. The write order and the limit on
  changed syncs hold only if one receiver handles a cluster at a time. The receiver is not in the read
  path, and the time limit is long, so a short failover does no harm.
- If a sync for a cluster is in progress, the receiver answers a second sync for that cluster with `409`,
  and the agent retries. A queue could let an old state overwrite a new state.
- After a failover, an old leader can still finish some writes. The digest then differs, so the next
  heartbeat repairs the state.
- The agent sends a digest at each heartbeat (default each minute) and the full state if the digest
  differs. The full state replaces the earlier state, which makes each sync idempotent.
- The receiver calculates its digest from the stored objects. It writes new and changed objects first,
  then deletes, and sets `lastSyncTime` last. A failed sync leaves a digest that differs, so the
  next heartbeat cannot renew `lastSyncTime` while a revoked grant stays.
- The agent sends nothing until it has a complete read of the spoke API server. An empty state from an
  agent at startup must not delete grants.
- The sync reply carries the status conditions of each policy. The agent writes them to the spoke
  objects, so spoke teams can use `kubectl get ap`. The reply keeps messages about the policy itself, for
  example a bad regex. It omits `CrossTenantUnionUnsupported`, because that condition can show the grants
  of a shared subject in other tenants.
- The conditions show the state of the last compile, so a new policy shows its state at the next
  heartbeat. Each condition carries the generation of the spoke object that it describes.

**Identity.** The receiver terminates mTLS itself and takes the cluster from the cert only. The protocol
has no cluster field.

1. The receiver has its own required client CA setting. The hub controls this CA, and no spoke can issue
   certs from it. Otherwise, a spoke admin can issue a cert for a different spoke.
2. The identity is the URI SAN of the client cert. The receiver rejects a cert with no URI SAN or with
   several URI SANs.
3. The receiver compares the identity with `spec.identity` for an exact match. It rejects an unknown
   identity. It also rejects an identity that occurs on two `SpokeCluster` objects.
4. The last path segment of `spec.identity` must be equal to the `SpokeCluster` name. Otherwise, the
   policies of that spoke grant nothing. This check finds an identity on the wrong `SpokeCluster`.
5. At the start of each sync, the receiver reads the `SpokeCluster` directly from the API server. It uses
   this value for the checks of identity, suspension and generation. It writes `status` with a
   `resourceVersion` precondition, so a sync that overlaps a spec change fails and validates nothing.

**Use a dedicated agent cert.** The agent cert must not be the ingest cert, and its identity must not
occur on an ingest cert. Otherwise, the theft of a write-only telemetry credential also gives the power
to grant read access. This means that the agent cert and the ingest cert have different identities. The
`SpokeCluster` object, which only a hub admin can write, is the link between the two.

**Unknown input.** The receiver has one path and one method. It rejects the whole sync for each of these
conditions, and it does not renew `lastSyncTime`:

- an unknown path, method, kind or field;
- a kind that does not agree with the spec;
- an `AccessPolicy` with an empty namespace, or a `ClusterAccessPolicy` with a namespace;
- a body or an object count above the limit;
- more than one changed sync from one cluster in 10 seconds;
- any sync while the `SpokeCluster` is suspended.

**Load limits.** One spoke must not be able to stop the receiver, because then all spokes expire. The
receiver applies these limits:

- Each identity has a burst of six requests, then one request each 5 seconds. This limit applies to
  heartbeats and full syncs, also from an identity that is not registered. The receiver applies it
  before it reads the body and before it reads the API server, and it answers `429` with `Retry-After`.
  An agent with the lowest heartbeat interval (10 seconds) sends at most a heartbeat and a full sync
  each 10 seconds, and it retries with backoff, so the limit does not stop it.
- The body has a size limit, and the number of JSON values in it has a limit that depends on the policy
  limit. The receiver checks the keys and counts the values in one pass over the tokens, before the typed
  decode. Thus the memory of one request has a fixed upper limit, also for a body of small values.
- A fixed number of requests can be in progress at the same time.

### 18.6 Time limit

If a spoke does not sync, its policies stop after `maxStaleness` (default 48 hours). Hub policies do not
expire.

1. The clock starts at `lastSyncTime`. The receiver sets this value with the hub clock.
2. An agent syncs only after a direct read of the spoke API server, not a read of its cache. Otherwise, an
   agent that has lost its API server can renew old policies forever. The receiver cannot verify this rule.
3. `Resolve` does the check for each request, because expiry makes no event for a snapshot rebuild.
   The deadlines are in a table outside the snapshot, so a heartbeat does not start a recompile. A
   replica reports ready only when the table has its first complete state.
4. Querdian treats a spoke as expired if any of these conditions is true:
   - The deadline table has no entry for the spoke.
   - `suspended` is true.
   - `lastSyncTime` is absent, or more than five minutes in the future.
   - `status.observedGeneration` differs from `metadata.generation`.
   - `spec.maxStaleness` is zero or negative.
   - The rule was compiled from a different version of the `SpokeCluster`. A version is the pair of
     `metadata.uid` and `metadata.generation`. A re-created object starts again at generation 1, so the
     generation alone is not sufficient.
5. An expired rule loses its terms but keeps its `labels.deny` and `labels.redact`, so expiry only
   narrows access.
6. If no term remains, `Resolve` must return an error. The `ErrNoGrant` check in `Resolve` runs before
   this filter, and `Normalise` returns no error for an empty term list. This case needs its own
   check and test. The new reason is `policy_source_expired` with status 403. It does not name the cluster.
7. The next valid sync restores the grants with no manual work.
8. Each 10 seconds, a Loki tail checks the deadlines of the spokes that had active terms when the tail
   started. If one has expired, or if its `SpokeCluster` has a different version than at the start, the
   tail closes. A suspended spoke can still send logs, so documentation alone is not sufficient here.

Results that operators must accept:

- Revocation of a spoke policy takes up to one heartbeat. During an outage, it takes up to the time limit.
- The limit on changed syncs applies to the whole spoke, so it can delay a revocation by up to one retry.
- A failure of an agent cert stops the sync only. The grants continue until the time limit.
- A failure of the receiver, the network path or the hub API server expires all spokes after the time
  limit. Hub grants stay active, and a hub admin can add a temporary hub `ClusterAccessPolicy`.
- A change to `SpokeCluster.spec` stops the grants of that spoke until the next sync. If the spoke is
  offline, the grants stay stopped until it returns.

Metric: `querdian_cluster_policy_age_seconds{cluster}`. The chart includes an optional alert at 24 hours.

### 18.7 Stolen agent cert

The time limit gives no protection against a stolen agent cert, because the holder can renew
`lastSyncTime`. The holder can write any grant for that spoke, but not for a different spoke. The first
version has no certificate revocation list. A hub admin does these steps:

1. Set `suspended: true`. The receiver rejects each sync. New requests stop when the replica sees the
   change, normally in about one second. Open tails close within 10 seconds.
2. Find the cause of the theft. If the spoke leaked the cert, fix the cause first, because a new cert on
   that spoke would be exposed in the same way. The spoke stays suspended.
3. Issue a new agent cert with a new URI SAN, then change `spec.identity`. The old cert no longer matches.
4. Delete the `SpokePolicy` objects of the spoke. This also removes `labels.deny` and `labels.redact` of
   forged rules, which a suspended spoke keeps (§18.6, rule 5).
5. Set `suspended: false`. The generation changed, so grants return only after a new valid sync.

`docs/multicluster.md` is the reference for the operator procedure.

### 18.8 Label integrity

§12 applies to the cluster label also. The ingest gateway must **overwrite** the label for all three
signals, not only add it. Writers on the hub that bypass the gateway, and ruler output, must not be
able to set the label of a spoke. The `SpokeCluster` name must also be equal to the label value that the
gateway writes, and cluster names must not change.

Hub telemetry must carry `cluster="<hubCluster>"`, also from ruler output and from hub agents. If it does
not, hub `AccessPolicy` users see no data, and they get no error.

Querdian cannot verify any of these requirements. A startup check cannot help, because Querdian does not
query backends at startup, and an empty result does not prove a wrong configuration. The operations guide
gives a verification step for each signal.

Do these steps in this order. A later step does not repair a mistake in an earlier step:

0. Upgrade Querdian and its CRDs without the `multicluster` block, so that the `allClusters` field exists.
1. Examine each hub `ClusterAccessPolicy`. Add a cluster matcher or `allClusters: true` where necessary.
2. Make sure that hub telemetry carries `cluster="<hubCluster>"`.
3. Set the `multicluster` block and `clusterLabel`, with the additional proxy RBAC.
4. Install the receiver. Register each spoke with `suspended: true` and install its agent.
5. Connect each spoke to the ingest gateway, which overwrites the cluster label for all three signals.
   Test the overwrite with a forged label while the spoke is suspended. If spoke data arrives before
   step 3, each hub `AccessPolicy` for namespace X also matches namespace X of that spoke. No check can
   find this condition.
6. Unsuspend each spoke only after its test passes.

These numbers are the same as in `docs/multicluster.md`, which gives the full procedure for these steps
and for a stolen agent cert (§18.7).

### 18.9 Tenants

The design does not assume a tenant setup. For a backend with a `tenantHeader`:

- The tenants of a spoke `ClusterAccessPolicy` rule must be a subset of `SpokeCluster.spec.tenants`.
- The tenant of a spoke `AccessPolicy` comes from its namespace label, then from `defaultTenant`. The
  value must be in `spec.tenants`.
- Any other value makes the policy invalid (new reason `TenantNotAllowed`). Because of this, a spoke
  admin cannot select a tenant that the hub did not allow.
- A smaller allow-list must remove the grant, so a change to `spec.tenants` starts a recompile. An open
  Loki tail closes, because the generation of the `SpokeCluster` changed (§18.6, rule 8).

| Setup | Result |
|---|---|
| No tenant header | Terms have no tenant. Terms of several clusters merge where §6.3 permits it. |
| One tenant for all clusters | The same as above, with one tenant header. |
| One tenant for each cluster | A user with grants in two clusters has a cross-tenant union, and the cluster matcher makes the terms differ. This needs `tenantFederation: tenantIdLabel`. Otherwise the result is `403 cross_tenant_union_unsupported`. Today this rejection applies to Tempo and to Prometheus/Thanos. |
| Several tenants for each cluster | The rules of the row above apply if the user's terms span tenants. |

The setups with a cross-tenant union also get the current limits of federated tenants. On Loki,
tenant-bound metadata endpoints and `tail` have limits, and on Tempo, tenant-bound terms give `400`
(see `docs/limitations.md`).

### 18.10 Known limits

- **Interference between clusters.** A spoke admin can name a shared group in a policy. Its
  `labels.deny` and `labels.redact` then apply to the whole request of that group (§6.3), also for other
  clusters. Too many terms give that group `403 policy_too_complex` everywhere. A rule for one tenant can
  also change a hub grant across tenants to `403 cross_tenant_union_unsupported`. No data leaks, but one
  spoke can reduce access to the data of other spokes.
- **Term growth.** The same namespace in several clusters merges to one term. Different namespaces in
  different clusters do not merge, and a merge would over-grant. Such users reach `maxPolicyTerms`, the
  or-lift limits and `maxFanout` more quickly.
- **Old hub data.** Hub data from before the migration has no cluster label. Hub `AccessPolicy` users lose
  access to it until it leaves retention. Querdian does not accept `cluster=""` for them, because that adds
  a grant on all data with no label. A hub rule with `allClusters: true` still matches this data.
- **Traces across clusters.** Redaction stays safe (§7.4), but partial traces become normal. A user with
  a grant for one cluster cannot use `rootName`.

### 18.11 Open items

1. Interference: a limit for each cluster on the rules for one subject, or deny rules for each term.
2. One tenant for each cluster: a term could omit the `__tenant_id__` binding if ingest proves that a
   tenant holds only one cluster. Querdian cannot verify this, so it stays out of the first version.

### 18.12 Implementation notes

The code adds these points to the design above:

- **Configuration (§18.4).** Querdian also refuses to start if a `clusterLabel` starts with `__` or is
  equal to `namespaceLabel`.
- **Status reasons.** In addition to `InvalidClusterScope` and `TenantNotAllowed`, the compiler uses
  `MulticlusterDisabled`, `SpokeClusterNotFound`, `SpokeClusterInvalid` and `InvalidSpokePolicy` (§4.4).
  A spoke `AccessPolicy` with no tenant label and no `defaultTenant` gets `TenantNotAssigned`, not
  `TenantNotAllowed`. The tenant label of a spoke namespace is compared exactly; the label of a hub
  namespace is trimmed.
- **Cluster scope (§18.3).** A `RegexMatch` counts only if it expands to at most 1024 non-empty names.
  A case-insensitive regex does not count.
- **Expiry (§18.6, rule 4).** A spoke is also expired while its `SpokeCluster` has a deletion timestamp,
  and while the current snapshot was not compiled from the current generation of the `SpokeCluster`.
  A `SpokeCluster` with the hub name or a wrong identity gets no entry in the deadline table, so it
  has no series in the two `querdian_cluster_policy_*` metrics.
- **Tail (§18.6, rule 8).** The tail checks the deadlines once at the start and then each 5 seconds,
  so it closes within 10 seconds. The close code is 1008 with the reason `policy source expired`.
- **Metrics (§18.6).** The code also has `querdian_cluster_policy_expired{cluster}` (1 or 0), because
  the age alone does not show a suspended spoke.
- **Debounce.** A rebuild waits at most 8 debounce intervals in a stream of events, and a failed
  rebuild runs again after one second, because heartbeats start no rebuild.
- **Receiver (§18.5).** It also rejects: a query string or a content type other than
  `application/json` (or with a `Content-Encoding` other than `identity`), a second request with the same identity while one
  runs (`409`), more requests in progress than `--max-in-flight` (`503`), a `SpokeCluster` name that is
  not a valid label value, and a `SpokeCluster` with a deletion timestamp. It reads its TLS files only
  at startup, so a cert rotation needs a restart. It has no `/metrics` endpoint.
- **Status reply (§18.5).** The reply does not omit a policy with `CrossTenantUnionUnsupported`. It
  sends `Compiled` with the message "policy is active" for it.
- **Agent (§18.5).** It reads its cert, key and CA again before each request. Its heartbeat is at least
  10 seconds. It uses a subset of the RBAC in §18.5: `get` and `list` on the policy kinds and
  namespaces, `patch` on the two status subresources, and `get`, `create` and `update` on its lease.
- **SpokePolicy.** The receiver also sets the label `policy.querdian.dev/cluster`. The status has
  `sourceGeneration`, the spoke generation of the last compile. The compiler rejects an object whose
  name is not `spokesync.ObjectName` (`InvalidSpokePolicy`).
