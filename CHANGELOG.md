# Changelog

All notable changes to Querdian are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). The API group
`policy.querdian.dev/v1alpha1` may change incompatibly before it is promoted.

0.1.0 is the first release; its entries are derived from the git history and [TASKS.md](TASKS.md).
Details and the known limitations are in [docs/limitations.md](docs/limitations.md).

## [Unreleased]

### Added

* New settings of the security fixes: `--ui-allowed-host` (`ui.allowedHosts`),
  `--tracing.trust-inbound-context` (`tracing.trustInboundContext`), `limits.maxInflightMergeBytes`,
  `limits.maxQueryRegexWeight`, `limits.maxRequestRegexWeight` and the environment variable
  `QUERDIAN_MEMORY_LIMIT` (set by the Helm chart). New reasons: `PolicyTooLarge`,
  `merge_budget_exhausted` and `write_not_bounded`. See "Security".
* **Multi-cluster enforcement** (opt-in, PLAN section 18). One Querdian on a central hub enforces the
  policies of the hub and of several spoke clusters. Querdian takes the cluster of a policy from its
  origin, never from its text: each spoke policy and each hub `AccessPolicy` gets an implicit matcher
  on the cluster label, and a spoke rule never compiles to unrestricted. Without the new
  `multicluster` configuration block nothing changes. See [docs/multicluster.md](docs/multicluster.md)
  for the requirements on the ingest gateway and the agent CA, the ordered migration steps, the tenant
  setups and the procedures for a stolen agent cert and for the removal of a spoke.
* Static configuration: the `multicluster` block (`hubCluster`, `maxStaleness`, default 48h) and
  `clusterLabel` on each backend. Querdian refuses to start with an empty `hubCluster`, a zero or
  negative `maxStaleness`, a missing or invalid `clusterLabel`, a Tempo `clusterLabel` that is not a
  `resource.` attribute, or a `clusterLabel` without the block.
* New subcommands of the same binary: `querdian receiver` runs on the hub, terminates mTLS with its
  own client CA, takes the cluster from the URI SAN of the agent cert only, and stores the spoke
  policies; `querdian agent` runs in each spoke, reads its policies directly from the spoke API
  server, sends a digest at each heartbeat (default one minute) and the full state when it differs,
  and writes the `Ready` conditions back to the spoke objects. Both use leader election. The receiver
  limits each agent identity to a burst of six requests, then one request each 5 seconds, limits the
  number of JSON values in a body, and handles at most 8 requests at a time (`--max-in-flight`). The
  agent exports `querdian_agent_syncs_total{result}` and `querdian_agent_last_sync_timestamp_seconds`
  on its management port.
* New cluster-scoped kinds: `SpokeCluster` (short name `spoke`; a registered spoke with its agent
  identity, tenant allow-list, optional `defaultTenant`, `maxStaleness` and `suspended`) and
  `SpokePolicy` (short name `sp`; the hub copy of one spoke policy, written by the receiver only).
* `ClusterAccessPolicy` rules have the field `allClusters`. With the `multicluster` block, each hub
  `ClusterAccessPolicy` rule must state its cluster scope for each backend: a cluster matcher that
  names a fixed set of clusters, `allClusters: true`, or `unrestricted: true`. Without the block the
  field has no effect.
* Time limit: the policies of a spoke stop when it has not synced for `maxStaleness`, or when it is
  suspended, and return at the next valid sync. Expired rules keep their denied and redacted labels.
  New decision reason `policy_source_expired` (403). An open Loki tail closes within 10 seconds when a
  spoke in its policy expires, is suspended, or gets a new `SpokeCluster` spec or UID.
* New status reasons: `InvalidClusterScope`, `TenantNotAllowed`, `MulticlusterDisabled`,
  `SpokeClusterNotFound`, `SpokeClusterInvalid` and `InvalidSpokePolicy`.
* New metrics: `querdian_cluster_policy_age_seconds{cluster}` and
  `querdian_cluster_policy_expired{cluster}`; `querdian_policies` has the kind `SpokePolicy` with the
  `multicluster` block.
* Helm chart: `receiver.service.nodePort` sets a fixed node port for a receiver Service of type
  `NodePort`, so the receiver URL of the agents stays the same after a reinstall.
* Multi-cluster quickstart on kind (`hack/quickstart-multicluster/up.sh`): one hub and two spokes on a
  laptop, with a demo ingest gateway, the ordered migration steps, a forged-label test of the gateway
  and checks through the proxy. See [docs/quickstart-multicluster.md](docs/quickstart-multicluster.md).

### Changed

* **Upgrade notes for the security fixes** (see "Security"):
  * Fan-out: with the default `limits.maxInflightMergeBytes`, a second large fan-out at the same time
    waits or gets `503 merge_budget_exhausted`. To run more large fan-outs at the same time, raise
    `maxInflightMergeBytes` and the memory limit of the pod together (see the memory sizing in
    [docs/operations.md](docs/operations.md#scaling)). A merged response is written with a deadline of
    10 seconds plus 1 second for each MiB, at most 2 minutes, so a client that reads more slowly fails.
  * Dashboard: behind an Ingress or a proxy with its own host name, the dashboard gives
    `403 host not allowed` until you add the host with `ui.allowedHosts` (`--ui-allowed-host`). A
    port-forward and the quickstarts need no change.
  * LogQL: while labels are denied or redacted, `line_format` and `label_format` templates with `.`,
    variables, chained fields, `index`, `slice`, `call`, `range`, `with`, `define` or a control
    character get `403 denied_label`. Use a field with one name, for example `{{ .app }}`.
  * Tracing: client traces no longer continue into the server span; they are linked. Set
    `--tracing.trust-inbound-context` (`tracing.trustInboundContext`) only for trusted clients.
* **Breaking: size limits for policies and requests** (see "Security"). The CRDs reject policies over a
  limit. Before the upgrade, find the policies that are too large, because the compiler disables such a
  policy as a whole. After the upgrade, check the `Ready` condition of your policies
  (`kubectl get cap,ap`); split a policy that is now `PolicyTooLarge` into smaller policies. Users with
  very large merged policies, or queries with many selectors on such policies, can get
  `403 policy_too_complex` or `403 query_too_complex`. For `query_too_complex` from these limits, split
  the query into queries with fewer selectors, use shorter regexes in the query, or make the policy
  smaller; `maxFanout` has no effect on them. Character classes cost more when they are wide: a negated
  class (`[^/]`), a large Unicode class (`\pL`), or a case-insensitive class with wide ranges. Use
  narrow classes, for example `[a-z0-9-]` in place of `[^/]`, and keep `(?i)` for literals and small
  classes. A regex that does not parse now gives `400 bad_data` before the query is parsed.
* The 8 KiB byte limit of a regex also applies to literal lists. A Grafana "All" value of a variable
  with many values (about 400 or more namespaces, joined with `|`) gives `403 query_too_complex`: set a
  custom all value such as `.*` for the variable. A policy regex over 8 KiB gives `PolicyTooLarge`: split
  the list into more rules, each with a part of the list. Do not split it into more matchers of one
  rule, because the matchers of a rule are ANDed.
* With the `multicluster` block, the receiver rejects the whole sync of a spoke (`400`, message
  `invalid policy: ...` with the field over the limit) when one of its policies is over a limit. The
  spoke then keeps its last accepted state, and its grants expire after `maxStaleness`. The agent logs
  `agent: the receiver rejected the sync` with `status=400`, `querdian_agent_syncs_total{result="error"}`
  increases, and
  `querdian_agent_last_sync_timestamp_seconds` stops. Make the policy in the spoke smaller. See
  [docs/multicluster.md](docs/multicluster.md#time-limit-and-outages).
* A `ClusterAccessPolicy` rule with `unrestricted: false` and no matchers is now `InvalidMatcher` in
  the compiler too, not only in the CRD validation. This affects only objects stored without the
  CRD's CEL rule.
* With the `multicluster` block, a replica is ready only after it has read all `SpokeCluster` objects
  once, and a spoke stays expired until the current snapshot was compiled from the current generation
  of its `SpokeCluster`, so a change of `SpokeCluster.spec` never applies late.
* The snapshot rebuild debounce has a maximum wait of 8 intervals (2 seconds by default) under a
  steady stream of events, and a failed rebuild runs again after one second.

### Security

* **LogQL templates could print denied and redacted labels**
  ([GHSA-g593-w5rf-53qv](https://github.com/querdian-dev/querdian/security/advisories/GHSA-g593-w5rf-53qv)). Affects 0.1.2 and earlier.
  The check of `line_format` and `label_format` templates was a text scan, and some templates could
  pass it and print all labels, including labels in `labels.deny` and `labels.redact`. Querdian now
  parses each template with Go's `text/template/parse` and accepts only an allow-list of template nodes
  and functions while labels are denied. A template that does not parse, that has a `define` action, or
  that has a control character other than tab and newline is rejected. Literal text in a template is no
  longer compared with denied names. See
  [docs/limitations.md](docs/limitations.md#denied-label-scan-false-positives).
* **Large policies could exhaust the CPU and memory of the proxy**
  ([GHSA-72w7-fm25-mxhm](https://github.com/querdian-dev/querdian/security/advisories/GHSA-72w7-fm25-mxhm)).
  Policy lists and strings had no size limits, and the work for each request grew faster than the
  policy size. A user who can write an `AccessPolicy`, or a spoke team through its agent, could thus
  make the shared proxy slow or stop it. A client query with a large regex could do the same. Affects
  0.1.2 and earlier. Now:
  * The CRDs limit each list and string of `AccessPolicy`, `ClusterAccessPolicy` and the specs in
    `SpokePolicy` (for example 64 rules, 64 matchers for each rule, 16384 characters for each matcher
    value; see [docs/policy-guide.md](docs/policy-guide.md#size-limits)). The compiler applies the same
    limits, also to objects stored before this release: such a policy grants nothing and gets the new
    status reason `PolicyTooLarge`. The receiver rejects spoke policies over a limit.
  * The cost of a regex compile grows with the size of the compiled program, not with the length of the
    regex, and with the rune ranges and the width of its character classes and with case folding of
    wide ranges. Querdian now
    measures this cost without a compile (the weight). A regex matcher of a policy can have a weight of
    at most 16384, and the regexes of one policy together at most 32768
    (`PolicyTooLarge`). The CRD cannot express these two limits; the compiler and the receiver apply
    them.
  * Each request checks the size of its matched rules before the simplification (at most 4096 terms,
    32768 matchers and 1 MiB of matcher text), the simplification has a fixed budget of steps, and
    the simplified policy can have a weight of at most 32768. A request over one of these limits gets
    `403 policy_too_complex`.
  * The regexes that the proxy compiles for one request have a weight of at most 131072: the client
    regexes, compiled again for each term of the policy, and the policy, compiled again for each
    selector. The regexes of a client query can have a weight of at most 32768 (PromQL and LogQL). The
    proxy checks both before it compiles a regex. The new settings `limits.maxRequestRegexWeight` and
    `limits.maxQueryRegexWeight` change these limits (at most 262144; Helm: `config.limits`). A client
    regex can have at most 8 KiB and the client regexes of a query 32 KiB together, and a policy regex
    value at most 8 KiB.
  * The weight comes from a parse with a work budget (a copy of the Go regexp parser with a work
    counter, `internal/regexparse`), which stops as soon as the work is over the limit. A regex that
    does not parse gives `400 bad_data` before any compile. A request over one of these limits
    gets `403 query_too_complex`.
  * The simplification and the cross-tenant check of the snapshot rebuild no longer do work that grows
    with the square of the input in the normal case.

  See the upgrade notes under "Changed".
* **One fan-out request could use much more memory than its limit**
  ([GHSA-hh8g-744m-9r98](https://github.com/querdian-dev/querdian/security/advisories/GHSA-hh8g-744m-9r98)). Affects 0.1.2 and earlier.
  `limits.maxMergeBytes` did not hold for parallel branches. Now each branch counts each chunk before it
  keeps it, and the read stops at the first chunk above the limit for all branches together. The new
  limit `limits.maxInflightMergeBytes` (default `maxMergeBytes`, 64 MiB) bounds the buffered fan-out
  bodies of all concurrent requests of a replica; when it is full, the oldest fan-out that still reads
  waits for space, and each other fan-out fails with `503 unavailable`, `Retry-After` and the reason
  `merge_budget_exhausted`. After the merge, the budget holds only the merged response, and the proxy
  writes it with a deadline. The Helm chart gives the container memory limit to Querdian
  (`QUERDIAN_MEMORY_LIMIT`), and Querdian sets the Go memory limit to 90 percent of it, unless
  `GOMEMLIMIT` is set. See [docs/operations.md](docs/operations.md#scaling) for the memory sizing.
* **Upstream spans recorded the rewritten query by default**
  ([GHSA-c2rh-rmcg-357j](https://github.com/querdian-dev/querdian/security/advisories/GHSA-c2rh-rmcg-357j)). Affects 0.1.2 and earlier.
  The `upstream` client spans now record `url.full` with scheme, host and path only; the query string
  is recorded only with `--tracing.record-queries`. The trace context of a client is no longer the
  parent of the server span: the server span starts a new trace and has a span link to the trace
  context of the client, so a client cannot select the trace ID or force sampling. The new flag
  `--tracing.trust-inbound-context` (Helm: `tracing.trustInboundContext`, default `false`) restores
  the old behaviour for trusted clients.
* **The policy dashboard did not check the `Host` header**
  ([GHSA-gvjr-jcq3-jc25](https://github.com/querdian-dev/querdian/security/advisories/GHSA-gvjr-jcq3-jc25)). Affects 0.1.2 and earlier.
  The dashboard now answers only for `localhost`, `127.0.0.1`, `[::1]`, the host of `--ui-listen` and
  the names of the new repeatable flag `--ui-allowed-host` (Helm: `ui.allowedHosts`), with any port.
  Other hosts get 403, for the API and the static files, in all authentication modes. A request with a
  method other than `GET` must also not come from a different origin. See
  [docs/dashboard.md](docs/dashboard.md#allowed-hosts).

### Documentation

* `ROADMAP.md`, the OpenSSF Best Practices checklist and the security self-assessment no longer say
  that `make e2e` and `make chart-lint` do not run in CI: the `e2e` and `chart` jobs run on every pull
  request.

## [0.1.2] - 2026-10-28

A maintenance release: better diagnostics for OIDC issuer discovery and dependency updates. See the
[release notes](docs/release-notes-0.1.2.md).

### Fixed

* **OIDC issuer discovery failures are logged.** A wrong issuer URL, bad CA or DNS failure used to leave
  `/readyz` at "issuer discovery not complete" with no cause anywhere. Each failed attempt now logs
  `issuer discovery failed` (issuer, error, retry delay) and success logs `issuer discovery complete`.

### Security

* **`github.com/go-jose/go-jose/v4` v4.1.6** fixes a denial of service when parsing JWKs with RSA keys
  ([GHSA-mgcc-cww5-fp2q](https://github.com/advisories/GHSA-mgcc-cww5-fp2q), CVE-2026-116587). Querdian
  parses the OIDC issuer's JWKS through `go-oidc` and `go-jose`, so whoever controls the JWKS response
  (a compromised identity provider, or the network path to an `http` issuer) could exhaust Querdian's
  CPU. RSA keys are now capped at 4096 bits.
* `golang.org/x/net` v0.62.0 fixes GO-2026-6860 and GO-2026-6866 in `x/net/html`, which Querdian does
  not call; the update keeps image scanners clean.

### Changed

* Dependency updates: Go modules (`prometheus/common` v0.73.0, `k8s.io/*` v0.37.2), the `golang:1.27.3`
  builder image digest and Hugo v0.169.0 for the website. `prometheus/common` v0.73.0 adds
  `github.com/hashicorp/go-uuid` (MPL-2.0, used unmodified), which the license check now allows.
* Quickstart: Grafana 13.3.0 and OpenTelemetry Collector 0.164.0.

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

[Unreleased]: https://github.com/querdian-dev/querdian/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/querdian-dev/querdian/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/querdian-dev/querdian/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/querdian-dev/querdian/releases/tag/v0.1.0
