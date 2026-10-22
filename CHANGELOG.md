# Changelog

All notable changes to Querdian are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). The API group
`policy.querdian.dev/v1alpha1` may change incompatibly before it is promoted.

Nothing has been released yet. The `[Unreleased]` section summarises what is on `main` and goes
into 0.1.0; it is derived from the git history and [TASKS.md](TASKS.md). Details and the known
limitations are in [docs/limitations.md](docs/limitations.md).

## [Unreleased]

### Added

#### Policy dashboard

* Optional read-only web dashboard (`--ui-listen`, off by default): policies with compile status,
  effective access per user or group, backends, and a query test bench that shows the rewritten query
  without contacting a backend. Helm values `ui.*`. See [docs/dashboard.md](docs/dashboard.md).

#### Policy API and controller

* CRDs `ClusterAccessPolicy` and `AccessPolicy` in `policy.querdian.dev/v1alpha1`, with CEL
  validation, `Ready` status conditions (`Compiled`, `InvalidMatcher`, `UnknownBackend`,
  `TenantRequired`, `TenantNotAssigned`, `CrossTenantUnionUnsupported`) and printer columns.
* Policy compilation into immutable snapshots: anchored RE2 matchers, implicit namespace matcher for
  `AccessPolicy`, tenant taken from the namespace label `policy.querdian.dev/tenant`, users and
  groups indexed separately.
* Exact-union resolution: DNF normalisation (absorption per tenant, dedup, subsumption,
  single-label merge, disjointness, term cap) with semantic-equivalence fuzzing; cross-tenant unions
  with `__tenant_id__` binding where the backend supports it, rejected otherwise.
* Controller on controller-runtime: informers on both kinds and Namespaces, 250 ms debounce,
  atomic snapshot swap, leader-elected status writer.
* `labels.deny` to hide labels or attributes from label endpoints and to reject queries referencing
  them.
* `labels.redact` (opt-in, per rule) to rewrite the values of a label in query results with the
  strategy `Remove`, `Placeholder`, `Hash` (per-request key) or `IPv4Prefix` (keep 1–3 octets).
  A redacted label is also denied; the strictest strategy of all matched rules applies, and
  responses that cannot be redacted fail closed with 502.

#### Proxy

* Authentication with OIDC bearer tokens (go-oidc): issuer discovery, JWKS, audience checks,
  dotted claim paths for username and groups, custom CA, typed 401 reasons.
* Default-deny route tables for Prometheus/Thanos, Loki and Tempo, per-route parameter allow-lists,
  canonical-path checks, method checks, and request normalisation (duplicate enforcement
  parameters rejected, only allow-listed client headers forwarded, tenant header set from the
  policy).
* Streaming reverse proxy for single-request plans; bounded parallel fan-out with merge strategies
  and a byte cap; WebSocket fan-out for Loki `tail`.
* Prometheus-style JSON errors with `traceId` and an `X-Trace-Id` header on every response.
* Static configuration file with strict decoding and startup validation, flags, limits
  (`maxPolicyTerms`, `maxFanout`, `maxMergeBytes`, `upstreamTimeout`).

#### Query enforcement

* PromQL: injection into every vector selector, or-lifting for several policy terms, `match[]`
  expansion for `series`, `labels` and `label/<name>/values`, denied-label checks and series/label
  response filtering.
* LogQL: clean-room lexer and parser (`pkg/logql`) written from the public documentation, policy
  injection into every stream selector, or-lifting of metric queries, fan-out and merge for log
  queries, label/detected\_\*/series endpoints, `index/stats`, `volume`, `patterns` with summed
  results (marked with `X-Querdian-Approximate` when not provably exact), tail.
* TraceQL: clean-room lexer and parser (`pkg/traceql`), policy injection into every spanset filter,
  denied attributes, Tempo search metadata redaction, tag and tag-value endpoints, metrics endpoints,
  trace-by-ID `deny` or `filter` (JSON and protobuf).

#### Observability

* Prometheus metrics (`querdian_*`, native histograms and exemplars), OpenTelemetry tracing with a
  span per stage and privacy flags that are off by default, `slog` logging with trace correlation.
* Grafana overview dashboard (`charts/querdian/dashboards/querdian.json`), optionally installed as
  a ConfigMap by the chart.

#### Packaging and release engineering

* Helm chart `charts/querdian`: CRDs, Deployment with two replicas, RBAC, Service,
  PodDisruptionBudget, optional ServiceMonitor, NetworkPolicies (proxy ingress/egress and backend
  protection), `values.schema.json`, tracing values mapped to `OTEL_*`.
* Multi-arch distroless image (non-root, read-only root filesystem compatible), GoReleaser
  configuration, and a release workflow with cosign keyless signing, SBOMs, GitHub build-provenance
  attestations and an OCI Helm chart. The pipeline has not run yet.
* Kind quickstart with Keycloak, Grafana, Prometheus, Loki, Tempo and two demo teams, with a
  150-assertion verification script.
* Quickstart demo data: 24 hours (`DEMO_HISTORY_HOURS`) of deterministic, backfilled metrics, JSON and
  logfmt logs and multi-span traces with incidents, produced by `hack/quickstart/demogen` (Prometheus
  via `promtool` blocks in an init container, Loki and Tempo via a Job pushing OTLP), live workloads
  that continue it, and four provisioned Grafana dashboards (service overview, logs, traces and
  latency, what can I see) whose queries are verified per user.

#### Quality and project

* Unit, fuzz, envtest and container-based end-to-end suites (differential, isolation, semantic
  equivalence) against Prometheus, Loki and Tempo.
* CI: lint, race tests, envtest, fuzz, govulncheck, license gate, generate drift, DCO, CodeQL,
  OpenSSF Scorecard; Renovate configuration.
* Documentation: policy guide, operations, troubleshooting, limitations, threat model, endpoint
  inventory and research notes, security self-assessment draft, OpenSSF Best Practices checklist,
  release procedure.

### Security

* Fixes found by the differential and end-to-end suites, before any release: TraceQL
  trace-level intrinsics and `compare()` are rejected unless the root span is covered (root-span
  metadata oracle); Loki tenant binding of metric queries uses a negative matcher; denied labels are
  filtered out of Loki series responses; tag lists are filtered against denied labels.
* Client headers are forwarded by allow-list instead of a strip list, configurable per backend with
  `forwardHeaders` (default `Accept`, `Accept-Encoding`, `User-Agent`, and Loki's
  `X-Loki-Response-Encoding-Flags`). Tenant headers of any name (for example Thanos
  `THANOS-TENANT`), `Upgrade` and backend controls such as `X-Loki-Query-Limits` no longer reach
  backends; credentials, tenant, trace, forwarding and hop-by-hop headers cannot be allow-listed. A `101 Switching Protocols` from a backend on a non-WebSocket route fails
  with `502` instead of tunnelling unfiltered bytes past the response filters.

### Known limitations

See [docs/limitations.md](docs/limitations.md). Notably: Loki metadata endpoints under
tenant-bound policies are unreliable (empty or incomplete, never wider than the policy); denied
labels do not redact query results; bearer tokens are valid until expiry; there is no built-in rate
limiting.

## [0.1.0] - unreleased

_Stub. Fill in when releasing: move the relevant entries from `[Unreleased]` here, add the date and
the compare link. See [docs/releasing.md](docs/releasing.md)._

[Unreleased]: https://github.com/querdian-dev/querdian/commits/main
[0.1.0]: https://github.com/querdian-dev/querdian/releases/tag/v0.1.0
