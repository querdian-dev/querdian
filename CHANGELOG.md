# Changelog

All notable changes to Querdian are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). The API group
`policy.querdian.dev/v1alpha1` may change incompatibly before it is promoted.

0.1.0 is the first release; its entries are derived from the git history and [TASKS.md](TASKS.md).
Details and the known limitations are in [docs/limitations.md](docs/limitations.md).

## [Unreleased]

### Fixed

* **OIDC issuer discovery failures are logged.** A wrong issuer URL, bad CA or DNS failure used to leave
  `/readyz` at "issuer discovery not complete" with no cause anywhere. Each failed attempt now logs
  `issuer discovery failed` (issuer, error, retry delay) and success logs `issuer discovery complete`.

## [0.1.1] - 2026-10-26

A security release. See the [release notes](docs/release-notes-0.1.1.md).

### Security

* **Tempo tag lookups could list names and values of spans outside the policy**
  ([GHSA-jcqw-3c3f-xrmj](https://github.com/querdian-dev/querdian/security/advisories/GHSA-jcqw-3c3f-xrmj)).
  Tempo answers `api/v2/search/tags` and `api/v2/search/tag/<name>/values` unfiltered for some `q`
  shapes, which dropped the injected policy. Restricted tag lookups are now planned per policy term
  in a shape Tempo evaluates; policies with `!=`, `!~` or open regexes get `400` on these two
  endpoints, and a user `q` outside that shape is ignored. See
  [docs/limitations.md](docs/limitations.md#traceql-and-tempo).

### Changed

* The end-to-end suites run against Prometheus v3.15.1, Loki 3.7.9 and Tempo 3.1.1 (0.1.0: v3.5.0,
  3.5.0 and 2.9.0); the Tempo tag-lookup fix was also verified against Tempo 2.9.0 and 2.10.10.
* Quickstart: Tempo 3.1.1 in monolithic mode (live-store, no Kafka), Loki 3.7.9, Prometheus v3.15.1,
  Grafana 13.2.4, Keycloak 26.8.1 and OpenTelemetry Collector 0.163.2. The demo data generator no
  longer calls Tempo's `/flush`, which Tempo 3 does not have.
* Dependency updates: the distroless base image, Go modules (`prometheus/common` v0.72.1,
  `prometheus/prometheus` v0.315.1), GoReleaser v2.19.0, oras v1.3.6 and GitHub Actions. Renovate now
  also tracks the end-to-end and quickstart images.

## [0.1.0] - 2026-10-25

The first release: a signed multi-arch image `ghcr.io/querdian-dev/querdian:0.1.0` and the Helm chart
`oci://ghcr.io/querdian-dev/charts/querdian` version 0.1.0. See the
[release notes](docs/release-notes-0.1.0.md).

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
  configuration, and a release workflow with cosign keyless signing (image, chart, and the checksums
  file as a Sigstore bundle), SBOMs, GitHub build-provenance attestations and an OCI Helm chart,
  exercised end to end by the release candidates 0.1.0-rc.1 and 0.1.0-rc.2.
* Artifact Hub metadata for the chart: static annotations (category, license, links, CRDs and
  examples, icon), per-release annotations written by `hack/chartmeta` (changelog from this file,
  pre-release and security-update flags, image list) and the repository metadata
  `charts/artifacthub-repo.yml`, pushed with oras by the release workflow.
* Kind quickstart with Keycloak, Grafana, Prometheus, Loki, Tempo and two demo teams, with a
  153-assertion verification script.
* Quickstart demo data: 24 hours (`DEMO_HISTORY_HOURS`) of deterministic, backfilled metrics, JSON and
  logfmt logs and multi-span traces with incidents, produced by `hack/quickstart/demogen` (Prometheus
  via `promtool` blocks in an init container, Loki and Tempo via a Job pushing OTLP), live workloads
  that continue it, and four provisioned Grafana dashboards (service overview, logs, traces and
  latency, what can I see) whose queries are verified per user.

#### Quality and project

* Unit, fuzz, envtest and container-based end-to-end suites (differential, isolation, semantic
  equivalence) against Prometheus, Loki and Tempo.
* End-to-end coverage of `labels.redact` against the real backends (results, upstream errors, `U__`
  names) and of the token boundaries the injectors rely on: where Loki ends a `#` comment, that `#`
  inside strings is no comment, and how Tempo ends strings and quoted attribute names (`\"`, `\\`).
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
* `labels.redact` no longer leaks through upstream errors: Prometheus and Loki print whole series
  label sets in some query errors, which reached the client unredacted. For requests with redactions
  the error body of a backend is withheld (the status is kept), a failed fan-out branch is reported
  without its body, and a tail closes without the upstream's close reason.
* Prometheus `label/:name/values` checks denied and redacted labels against the name Prometheus 3
  looks up: a `U__`-escaped name (`U__customer__id`) is decoded first, so it can no longer read
  the values of a denied label.
* Every single-valued request parameter (every name not ending in `[]`) given more than once is
  `400 bad_request`, not only `query` and `q`: the enforcers and mergers read the first value of
  parameters such as `targetLabels`, `delay_for` and `limit`, and backends disagree on which wins.
* Tempo `search/tag/<name>/values` rejects quoted attribute names (`span."x"`) with `400`. The deny
  check compares the name as plain text; Tempo 2.9.0 does not decode the quoting there, and the
  proxy no longer relies on that.

### Known limitations

See [docs/limitations.md](docs/limitations.md). Notably: Loki metadata endpoints under
tenant-bound policies are unreliable (empty or incomplete, never wider than the policy); denied
labels do not redact query results; bearer tokens are valid until expiry; there is no built-in rate
limiting.

[Unreleased]: https://github.com/querdian-dev/querdian/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/querdian-dev/querdian/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/querdian-dev/querdian/releases/tag/v0.1.0
