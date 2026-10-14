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
- A UI.
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
  → debounce (250ms) → compile Snapshot → atomic.Pointer swap
  → status conditions (leader-elected writer)
```

### Request lifecycle

1. `otelhttp` server span starts; trace context extracted.
2. If snapshot not ready → `503`.
3. Route lookup by backend prefix + path. No match → `403 endpoint not allowed`.
4. Authenticate (Bearer JWT). Failure → `401`. All routes require authentication.
5. Resolve effective policy for (user, groups, backend). No grant → `403`.
6. `safe` routes → proxied unchanged, tenant header set from the effective policy.
7. Normalise request (§9). Violations → `400`.
8. Enforcer builds a `Plan` from the query + effective policy. Parse/rewrite failure → `400`.
9. Execute plan (single proxied request, or fan-out + merge). Upstream error → `502`.
10. Response streamed back (single) or written after merge (fan-out). `X-Trace-Id` always set.

### Repository layout

```
api/v1alpha1/            CRD Go types, deepcopy (controller-gen)
cmd/querdian/         main
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
pkg/logql/               public: LogQL lexer, parser, injector (Apache-2.0, reusable)
pkg/traceql/             public: TraceQL lexer, parser, injector (Apache-2.0, reusable)
charts/querdian/      Helm chart
docs/                    user + operator docs, research notes
test/e2e/                kind + testcontainers suites
hack/                    scripts (quickstart, codegen)
```

---

## 4. API (CRDs) — `policy.querdian.dev/v1alpha1`

Two kinds. Both only **grant** access; there are no deny policies (except `labels.deny`, which
narrows a grant). An invalid policy therefore grants nothing → fail closed by construction.

### 4.1 ClusterAccessPolicy (cluster-scoped, platform admins)

May set arbitrary matchers, tenants and `unrestricted`.

### 4.2 AccessPolicy (namespaced, team-owned)

- Every rule is implicitly ANDed with `<backend.namespaceLabel>="<metadata.namespace>"`.
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
type ClusterRule struct {
	// Backend names from the static config.
	// +kubebuilder:validation:MinItems=1
	Backends []string `json:"backends"`
	// +optional
	Tenants []string `json:"tenants,omitempty"`
	// +optional
	// +kubebuilder:default=false
	Unrestricted bool `json:"unrestricted"`
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

Single condition type `Ready`. Reasons:

| Reason | Status | Meaning |
|---|---|---|
| `Compiled` | True | Policy is active |
| `InvalidMatcher` | False | Regex does not compile / invalid label name |
| `UnknownBackend` | False | Rule references a backend not in static config |
| `TenantRequired` | False | ClusterRule without tenants for a backend with `tenantHeader` |
| `TenantNotAssigned` | False | AccessPolicy namespace lacks `policy.querdian.dev/tenant` label |
| `CrossTenantUnionUnsupported` | True | Active, but some users' unions across tenants on a backend without federation support will be rejected (informational message lists backends) |

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
    tenantFederation: none          # none | tenantIdLabel (see §6.4)
  - name: logs
    type: loki
    url: http://loki-gateway.monitoring
    pathPrefix: /loki
    namespaceLabel: k8s_namespace_name
    tenantHeader: X-Scope-OrgID
    tenantFederation: tenantIdLabel
  - name: traces
    type: tempo
    url: http://tempo-query-frontend.monitoring:3200
    pathPrefix: /tempo
    namespaceLabel: resource.k8s.namespace.name
    tenantHeader: X-Scope-OrgID
    tenantFederation: none
    traceByID: filter               # deny | filter
limits:
  maxPolicyTerms: 16                # DNF size cap per request
  maxFanout: 16                     # parallel upstream requests per client request
  maxMergeBytes: 67108864           # 64 MiB cap for buffered merge responses
  upstreamTimeout: 5m
```

Validation at startup (fail to start on error): unique backend names and prefixes, valid URLs,
known types, non-empty audiences, `traceByID` only on tempo.

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
| `--tracing.record-queries` | `false` | add original + rewritten query to spans |

Tracing exporter/sampler config uses standard `OTEL_*` environment variables (§11.2).

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

1. **Unrestricted absorbs.** Any matched unrestricted rule → `Unrestricted=true`, terms dropped.
   (Tenants still collected from all matched rules.)
2. **Dedup** identical terms.
3. **Subsumption.** Drop term `b` if another term `a` (same tenant) has a matcher set that is a
   subset of `b`'s (`a` is broader).
4. **Single-label merge.** Two terms (same tenant) whose matchers are identical except for one
   label where both use `Equal`/`RegexMatch` → merge into one `RegexMatch` with alternation
   (`(?:a)|(?:b)`). Repeat to fixpoint.
5. **Disjointness.** Two terms are provably disjoint if they have different tenants, or both have
   an `Equal` matcher on the same label with different values. `Disjoint` = all pairs disjoint.
6. **Cap.** `len(Terms) > maxPolicyTerms` → reject (403, reason `policy_too_complex`).

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
| `metadata`, `targets`, `rules`, `alerts`, `status/*` (other), `admin/*`, `write`, `otlp/*` | deny | — |

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
| `push`, rules, delete, everything else | deny | — |

### 8.3 Tempo (`type: tempo`), under `pathPrefix` + `/api/`

| Path | Kind | Param |
|---|---|---|
| `search` | enforce + metadata redaction | `q` |
| `v2/search/tags`, `v2/search/tag/<name>/values` | enforce | `q` |
| `metrics/query`, `metrics/query_range` | enforce | `q` |
| `traces/<id>`, `v2/traces/<id>` | traceByID | — |
| `status/buildinfo`, `echo` | safe | — |
| everything else | deny | — |

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
- Strip from client request before forwarding: `Authorization`, `Cookie`, any configured
  `tenantHeader`, `X-Scope-OrgID`, `Forwarded`/`X-Forwarded-*` (re-set by proxy), `baggage`.
- Set: tenant header from `Effective.Tenants`; `traceparent` (propagation).
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

`endpoint` is the route template (e.g. `label/:name/values`), never the raw path.

### 11.2 Tracing (OpenTelemetry)

- Libraries: `go.opentelemetry.io/otel`, SDK, contrib `otelhttp`, `autoexport`, `autoprop`.
- Configuration exclusively via standard env vars (`OTEL_SERVICE_NAME`,
  `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_TRACES_SAMPLER`,
  `OTEL_TRACES_SAMPLER_ARG`, `OTEL_TRACES_EXPORTER`, `OTEL_RESOURCE_ATTRIBUTES`).
  Default sampler `parentbased_traceidratio`.
- Propagation: inbound `tracecontext` + `baggage`; outbound **only** `tracecontext`.

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
`querdian.query.rewritten` (`--tracing.record-queries`).

Every response has header `X-Trace-Id`; every error body has `traceId`.

### 11.3 Logging

`log/slog`, JSON by default. Every request log line carries `trace_id`, `span_id`, backend,
route, decision, reason, duration. User identity and queries only logged when the matching
tracing privacy flag is enabled.

---

## 12. Security model

- Trust boundary: the proxy trusts the OIDC issuer and the cluster API server; it trusts nothing
  from the client except the verified token.
- **Namespace label integrity:** `AccessPolicy` isolation relies on `namespaceLabel` being set by
  the collection pipeline (Prometheus scrape config, OTel k8sattributes, Promtail/Alloy), not by
  workloads. Must be prominent in docs.
- Tenant headers from clients are always stripped.
- Backends must not be reachable by clients directly (NetworkPolicy in the chart).
- Threat model document (`docs/security/threat-model.md`) covers: query injection via
  parser divergence, label oracle attacks (filters on denied labels), metadata leaks (Tempo root
  fields), resource exhaustion (fan-out, merge size), token replay, snapshot staleness.

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

M2, M3 and M4 can run in parallel once M1 is done (separate agents).

---

## 17. To verify (research tasks, results in `docs/research/`)

1. Exact current API paths and parameters for Prometheus, Loki, Tempo (`T0.3`).
2. `__tenant_id__` matcher support under tenant federation: Loki, Tempo, Thanos/Mimir (`T0.4`).
3. Tempo: regex anchoring of `=~`, operator precedence of `&&`/`||` in spanset filters, search
   response fields that expose root span data (`T0.5`).
4. LogQL: `or`/`and`/`unless` support in metric queries, complete pipeline stage keyword list
   from docs (`T0.5`).
