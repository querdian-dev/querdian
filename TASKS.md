# TASKS

Work breakdown for building the project described in `PLAN.md`. Read `PLAN.md` completely before
picking up any task — section references (§) point there.

## How to work on these tasks

- **Pick** a task whose `Depends on` tasks are all `[x]` done. Tasks marked `∥` can run in
  parallel with their siblings.
- **One task = one branch = one PR.** Branch name: `t<id>-<short-slug>` (e.g. `t1.4-dnf-normalisation`).
- **Commits:** signed off (`git commit -s`), author `geilername <maximilian.marschall@3xm.at>`, conventional commit style
  (`feat(policy): …`, `test(logql): …`).
- **Definition of done** for every task:
  - [ ] All acceptance criteria met.
  - [ ] `make lint test` green; `make generate manifests` produces no diff.
  - [ ] New code has unit tests; public functions have doc comments.
  - [ ] No new dependency outside the license allowlist (`make licenses` green).
  - [ ] Ground rules in PLAN §2 respected (clean room!).
  - [ ] Task checkbox below ticked in the same PR.
- **When blocked or unsure:** do not guess on security-relevant behaviour. Open an issue titled
  `question(t<id>): …`, describe the options, and stop.
- **Status markers:** `[ ]` open · `[~]` in progress (add `@agent-name`) · `[x]` done · `[!]` blocked.

---

## M0 — Foundations

### [ ] T0.0 — Org, repository and name check `HUMAN`
- **Depends on:** —
- **Description:** Name, org and git identity are decided (PLAN §0). Create the org and repo,
  harden the org, run the name conflict check.
- **Acceptance:**
  - [ ] Org `querdian-dev` created: 2FA required for members, base repository permission `read`.
  - [ ] Org Actions policy: only GitHub-owned and verified-creator actions allowed.
  - [ ] Repository `querdian-dev/querdian` exists with an empty `main` branch and a ruleset:
        PR required, required status checks (added once T0.2 lands), DCO, no force pushes,
        no deletions.
  - [ ] Name conflict check done: pkg.go.dev, Docker Hub, CNCF landscape, EUIPO TMview and
        USPTO (classes 9/42); `querdian.dev` registered or CRD group changed.

### [x] T0.1 — Repository scaffold and governance files
- **Depends on:** T0.0
- **Files:** `go.mod`, `cmd/querdian/main.go`, `Makefile`, `.golangci.yml`, `.gitignore`,
  `.editorconfig`, `LICENSE`, `README.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`,
  `SECURITY.md`, `GOVERNANCE.md`, `MAINTAINERS.md`, `ADOPTERS.md`, `PLAN.md`, `TASKS.md`
- **Description:**
  - `main.go` prints version and exits (version injected via `-ldflags`).
  - Makefile targets: `build`, `test`, `test-race`, `lint`, `fuzz` (short), `generate`,
    `manifests`, `licenses`, `envtest`, `e2e`, `fmt`. Tool versions pinned via `go tool`
    directives / `tools.go`.
  - `LICENSE`: Apache-2.0 full text. `README.md`: one-paragraph description, status "pre-alpha",
    credit line "Inspired by [evoila/janus](https://github.com/evoila/janus).", link to PLAN.
  - `CONTRIBUTING.md`: DCO requirement, clean-room rule (PLAN §2.2 verbatim), how to run tests.
  - `CODE_OF_CONDUCT.md`: CNCF Code of Conduct reference.
  - `SECURITY.md`: private vulnerability reporting via GitHub Security Advisories.
- **Acceptance:**
  - [x] `make build test lint` succeeds on a clean checkout.
  - [x] All listed files present and non-placeholder.

### [~] T0.2 — CI pipelines ∥
- **Depends on:** T0.1
- **Files:** `.github/workflows/*.yml`, `renovate.json`, `.github/dependabot.yml` (only if
  Renovate is not used — pick one: Renovate)
- **Description:** GitHub Actions:
  - `ci.yml` (PR + main): `lint`, `test-race`, `fuzz` (30s per target), `govulncheck`,
    `licenses` (`go-licenses check ./... --disallowed_types=forbidden,restricted,reciprocal,unknown`),
    `generate`/`manifests` diff check.
  - `dco.yml`: DCO sign-off check on PRs.
  - `scorecard.yml`: OpenSSF Scorecard (weekly + main).
  - `codeql.yml`: CodeQL for Go.
  - Actions pinned by commit SHA; minimal `permissions:`.
  - `renovate.json`: Go modules, GitHub Actions, container images in tests; group minor/patch.
- **Acceptance:**
  - [ ] A PR adding an AGPL test dependency fails the `licenses` job (verify once, then revert).
  - [ ] A commit without sign-off fails DCO.
  - [ ] All workflows green on `main`.

### [x] T0.3 — Research: endpoint inventory ∥
- **Depends on:** T0.0
- **Files:** `docs/endpoints.md`
- **Description:** Verify PLAN §8 against the **current public API documentation** of Prometheus,
  Thanos Query, Loki and Tempo. For every endpoint record: path, methods, enforcement parameter,
  single/multi-valued, response shape, and the PLAN classification (enforce/safe/deny). Flag any
  endpoint not in PLAN §8 that Grafana's Prometheus/Loki/Tempo data sources call.
- **Acceptance:**
  - [ ] `docs/endpoints.md` with tables per backend and doc URLs (with access date).
  - [ ] Differences to PLAN §8 listed in a "Deltas" section; PLAN updated in the same PR if the
        delta is a pure fact (path rename); otherwise open a `question(t0.3)` issue.

### [x] T0.4 — Research: tenant federation and `__tenant_id__` ∥
- **Depends on:** T0.0
- **Files:** `docs/research/tenant-federation.md`, `hack/research/tenant-federation/` (scripts)
- **Description:** Black-box experiments with containers (Loki, Tempo, Mimir; Thanos for
  reference). For each: does a `t1|t2` tenant header work for queries, and can queries filter
  with `__tenant_id__` (label matcher / TraceQL attribute)? Record versions tested.
- **Acceptance:**
  - [x] Reproducible script + results table (backend × version × supported?).
  - [x] Recommended default `tenantFederation` per backend type added to PLAN §5.1 comments.

### [x] T0.5 — Research: TraceQL and LogQL language details ∥
- **Depends on:** T0.0
- **Files:** `docs/research/traceql.md`, `docs/research/logql.md`
- **Description:** From **public docs** plus black-box experiments only (clean room):
  - TraceQL: `=~` anchoring, `&&`/`||` precedence inside spanset filters, full operator list
    (structural, spanset, comparison), intrinsics list, scoped attribute syntax, quoting of
    attribute names with special characters, search response fields carrying root span data.
  - LogQL: binary set operators in metric queries, complete pipeline stage keyword list,
    string/raw-string/comment rules, duration/bytes literal syntax, `offset`, `label_replace`.
  - Include the list of doc URLs used (these become the clean-room provenance record).
- **Acceptance:**
  - [x] Both documents complete with examples verified against a running container
        (Loki 3.5.0, Tempo 2.9.0; docs-only leftovers such as Tempo metrics functions are
        explicitly marked unverified / listed as follow-ups).
  - [x] Doc URL list included.

---

## M1 — Core

### [x] T1.1 — CRD types and manifests
- **Depends on:** T0.1
- **Files:** `api/v1alpha1/*.go`, `config/crd/*.yaml`, `hack/boilerplate.go.txt`,
  `api/v1alpha1/*_test.go`
- **Description:** Implement PLAN §4.3 exactly. `controller-gen` for deepcopy + CRDs.
  `groupversion_info.go` with scheme registration.
- **Acceptance:**
  - [x] `make generate manifests` produces committed output with no diff.
  - [x] envtest suite: valid examples from PLAN §4.5 are accepted; CEL rejects (a) empty
        subjects, (b) `unrestricted: true` with matchers, (c) `unrestricted: false` without
        matchers, (d) invalid `op`.

### [x] T1.2 — Static config and flags ∥
- **Depends on:** T0.1
- **Files:** `internal/config/*`
- **Description:** Load and validate the config file (PLAN §5.1) with strict YAML decoding
  (unknown fields = error). Flags per PLAN §5.2. Types: `Config`, `AuthConfig`, `Backend`,
  `Limits`, `BackendType` enum.
- **Acceptance:**
  - [x] Table tests for every validation rule in PLAN §5.1.
  - [x] Example config from PLAN §5.1 loads successfully (`testdata/`).

### [x] T1.3 — Policy compilation
- **Depends on:** T1.1, T1.2
- **Files:** `internal/policy/compile.go`, `internal/policy/types.go`, tests
- **Description:** Pure functions (no Kubernetes client) that turn CRD objects + namespace
  labels + static config into `CompiledRule`s and per-object status results (PLAN §6.1, §4.4).
  Label name validation per backend type (Prometheus/Loki label names; Tempo attribute names).
  Regexes compiled anchored.
- **Acceptance:**
  - [x] Every status reason in PLAN §4.4 covered by a test.
  - [x] AccessPolicy implicit namespace matcher uses the backend's `namespaceLabel`.
  - [x] Users and groups indexed in separate maps; a user named like a group gets no grant from it.

### [x] T1.4 — DNF normalisation
- **Depends on:** T1.3
- **Files:** `internal/policy/dnf.go`, tests
- **Description:** Implement PLAN §6.3 steps 1–6 as a pure function
  `Normalise([]Term, Limits) (Effective, error)`.
- **Acceptance:**
  - [x] Table tests for each step individually and combined (≥ 30 cases).
  - [x] Property test (fuzz): for random terms and random label sets, a label set is matched
        by the normalised DNF **iff** it is matched by the input DNF (semantic equivalence).
  - [x] Cap enforced with `ErrTooComplex`.

### [x] T1.5 — Resolver and tenant handling
- **Depends on:** T1.4
- **Files:** `internal/policy/resolve.go`, `internal/policy/snapshot.go`, tests
- **Description:** `Snapshot` (immutable) with `Resolve(user string, groups []string, backend string) (Effective, error)`
  per PLAN §6.2 and §6.4, including `__tenant_id__` binding and
  `cross_tenant_union_unsupported` rejection; `DeniedLabels` union.
- **Acceptance:**
  - [x] Tests: no grant, single rule, multiple rules same tenant, multiple tenants identical
        terms, multiple tenants with federation, multiple tenants without federation (reject),
        unrestricted + restricted mix, denied labels union.
  - [x] Benchmark: resolve with 1,000 policies and 20 groups < 50µs/op.
  - [x] `__tenant_id__` is bound only for federation (multi-tenant) headers; test with single-tenant header. Selectors always keep at least one non-`__tenant_id__` matcher (see PLAN §6.4).

### [x] T1.6 — Controller and snapshot lifecycle
- **Depends on:** T1.5
- **Files:** `internal/controller/*`, tests (envtest)
- **Description:** controller-runtime manager; informers for both CRD kinds and Namespaces;
  any event → debounce 250ms → list from cache → compile → `atomic.Pointer[Snapshot]` swap;
  `Ready()` true after cache sync + first snapshot. Status writer as a leader-elected runnable
  that patches `status.conditions` + `observedGeneration` only on change.
- **Acceptance:**
  - [x] envtest: create/update/delete policy → snapshot generation increments, resolve reflects it
        within 1s.
  - [x] envtest: namespace tenant label added → `TenantNotAssigned` flips to `Compiled`.
  - [x] Two managers against one envtest: only the leader writes status.
  - [x] Tracing span `snapshot.rebuild` emitted (use tracetest).

### [x] T1.7 — OIDC authentication ∥
- **Depends on:** T1.2
- **Files:** `internal/auth/*`, tests
- **Description:** PLAN §10. Middleware producing `Identity{Username string; Groups []string}`
  in context. Dotted claim paths. Typed errors → 401 with reason (`missing_token`,
  `invalid_token`, `expired`, `audience`, `issuer`, `missing_username`).
- **Acceptance:**
  - [x] Tests with a local test issuer (generated keys, httptest discovery + JWKS).
  - [x] Custom CA path works (httptest TLS server).
  - [x] `querdian_auth_failures_total{reason}` incremented.

### [ ] T1.8 — Enforce interface and route tables
- **Depends on:** T1.2, T0.3
- **Files:** `internal/enforce/enforce.go`, `internal/routes/*`, tests
- **Description:** PLAN §7.1 types and errors. Route tables per backend type from
  `docs/endpoints.md` (PLAN §8) with route templates (`label/:name/values`); lookup function
  returning `Endpoint{Kind, Param, Template, Methods, Websocket}`. Unknown → deny.
- **Acceptance:**
  - [x] Table test for every endpoint row in `docs/endpoints.md`, plus path traversal /
        double-slash / encoded-slash / trailing-slash cases (all must not escape to a different
        route).
  - [ ] Route templates carry a parameter allow-list (PLAN §9); `storeMatch[]` and unlisted params are dropped/rejected; Loki `detected_labels` and Loki POST on `query`/`query_range` are decided by a black-box test (record result in `docs/endpoints.md`).

### [x] T1.9 — Proxy core and request normalisation
- **Depends on:** T1.8
- **Files:** `internal/proxy/*`, `internal/fanout/fanout.go` (executor only, no merges), tests
- **Description:** PLAN §9: method checks, param merging, duplicate detection, header
  stripping/setting, `ReverseProxy` for single plans (streaming), fan-out executor (bounded
  parallelism, cancellation, all-or-nothing errors, byte cap), error writer with JSON format and
  `traceId`, `X-Trace-Id` header.
- **Acceptance:**
  - [x] Tests with httptest upstreams: client `X-Scope-OrgID`, `Authorization`, `baggage` never
        reach upstream; tenant header set from Effective.
  - [x] POST with URL `query` + form `query` → 400; POST forwarded with empty URL query.
  - [x] Fan-out: one branch fails → 502, other branches cancelled (verify via context).
  - [x] Merge byte cap → 502 `merge_too_large`.
  - [x] Merge plans force `Accept: application/json`; unlisted parameters (e.g. `storeMatch[]`) → 400; escaped slashes in the path → 400.

### [x] T1.10 — Observability foundation ∥
- **Depends on:** T0.1
- **Files:** `internal/observability/*`, tests
- **Description:** PLAN §11. Tracer provider via `autoexport` (default exporter `none` when
  `OTEL_EXPORTER_OTLP_ENDPOINT` unset), propagators (outbound tracecontext only), metric registry
  with all metrics from §11.1 (native histograms + exemplars), slog handler adding
  `trace_id`/`span_id`, privacy flag plumbing (helpers that only set attributes when enabled).
- **Acceptance:**
  - [x] tracetest: inbound `traceparent` continued; outbound request has `traceparent`, no
        `baggage`.
  - [x] With default flags, no span carries `enduser.id` or query attributes.
  - [x] Exemplar with trace ID present on request histogram (testutil).

### [ ] T1.11 — Server wiring and health
- **Depends on:** T1.6, T1.7, T1.9, T1.10
- **Files:** `internal/server/*`, `cmd/querdian/main.go`
- **Description:** Two HTTP servers (proxy, management). Handler chain: otelhttp → readiness
  gate (503) → route → auth (required for all routes, including `safe`) → resolve →
  enforce (skipped for `safe`) → proxy. Follow the lifecycle in PLAN §3 exactly. Graceful shutdown. `/healthz` (process), `/readyz` (snapshot ready + issuer
  discovery done).
- **Acceptance:**
  - [ ] Integration test (envtest + httptest upstream + test issuer): unrestricted policy →
        request proxied with tenant header; no policy → 403; before snapshot ready → 503.
  - [ ] Server span carries `querdian.decision`, `.decision.reason`, `.policies`.

---

## M2 — PromQL (parallel with M3, M4 after M1)

### [ ] T2.1 — PromQL single-term injection and denied labels
- **Depends on:** T1.11
- **Files:** `internal/enforce/promql/*`
- **Description:** PLAN §7.2 parse, denied-label checks, single-term injection into every
  vector selector (incl. matrix selectors, subqueries, `@`, `offset`).
- **Acceptance:**
  - [ ] ≥ 60 table cases covering all PromQL expression node types.
  - [ ] Output re-parses; every `VectorSelector` contains all term matchers.
  - [ ] Denied label in matcher / `by` / `on` / `label_replace` → `ErrDeniedLabel`.

### [ ] T2.2 — PromQL or-lifting for multiple terms
- **Depends on:** T2.1
- **Files:** `internal/enforce/promql/union.go`, tests
- **Description:** PLAN §7.2 multi-term rules. Strategy `or_lift`.
- **Acceptance:**
  - [ ] Table cases: instant selector, every range-vector function, nested aggregations,
        binary ops with `on`/`ignoring`, subqueries, `absent`/`absent_over_time` (documented edge).
  - [ ] Property fuzz: random query × random 1–4 terms → output re-parses, every selector carries
        exactly one term's matchers, number of selector copies = original × terms.

### [ ] T2.3 — Prometheus metadata endpoints
- **Depends on:** T2.1
- **Files:** `internal/enforce/promql/metadata.go`, tests
- **Description:** `match[]` expansion for `series`, `labels`, `label/<n>/values`; denied label
  filtering of `labels` responses and 403 on `label/<denied>/values`. Strategy `match_expand`.
- **Acceptance:**
  - [ ] Tests for: no user `match[]`, one, several; × 1 and 3 terms.
  - [ ] `labels` response filtering is streaming-safe (bounded buffering, cap applies).
  - [ ] When the client omits `match[]`, one policy `match[]` per term is still sent (omission is never a pass-through).

### [ ] T2.4 — Prometheus E2E and semantic equivalence
- **Depends on:** T2.2, T2.3
- **Files:** `test/e2e/prometheus/*`, `test/e2e/framework/*`
- **Description:** testcontainers Prometheus with seeded data (backfilled via promtool TSDB
  blocks or remote-write from the test). Framework reused by M3/M4: start upstream, start proxy
  in-process with a static Snapshot (no k8s), issue requests as a user.
- **Acceptance:**
  - [ ] Isolation: user A never sees series of namespace B across all enforced endpoints.
  - [ ] Equivalence: for a corpus of ≥ 40 queries × term sets, rewritten result equals the union
        of per-term results computed by the test.
  - [ ] Runs in CI under 5 minutes.

---

## M3 — LogQL (parallel with M2, M4 after M1)

> Clean room applies with full force here. Only public LogQL docs and `docs/research/logql.md`.

### [x] T3.1 — `pkg/logql` lexer
- **Depends on:** T0.5
- **Files:** `pkg/logql/lexer.go`, tests, `pkg/logql/doc.go` (doc URLs)
- **Acceptance:**
  - [x] Token types per PLAN §7.3; unknown input → error with position.
  - [x] Fuzz: no panics; `concat(tokens) == input` modulo whitespace/comments.

### [x] T3.2 — `pkg/logql` parser
- **Depends on:** T3.1
- **Files:** `pkg/logql/parser.go`, `pkg/logql/ast.go`, tests
- **Description:** Recursive descent per PLAN §7.3 (metric layer + selectors + opaque, validated
  pipeline). `String()` renders a canonical form.
- **Acceptance:**
  - [x] Own corpus ≥ 150 queries derived from public doc examples (doc URL per case).
  - [x] Fuzz round-trip: `Parse(String(Parse(x)))` equals `Parse(x)`.
  - [x] Query kind (log/metric) correct for all corpus entries.

### [ ] T3.3 — LogQL injection, denied labels, or-lifting
- **Depends on:** T3.2, T1.11
- **Files:** `pkg/logql/inject.go`, `internal/enforce/logql/*`
- **Description:** Single-term injection into every stream selector; conservative denied-label
  token scan; or-lifting for metric queries; fan-out plan for log queries.
- **Acceptance:**
  - [ ] Property fuzz analogous to T2.2.
  - [ ] Log query + 2 terms → `Plan` with 2 requests, `MergeLokiStreams`.
  - [ ] `label_replace` string arguments are scanned for denied labels; tests cover overwriting/removing a policy label.

### [ ] T3.4 — Loki stream merge
- **Depends on:** T3.3, T1.9
- **Files:** `internal/fanout/loki_streams.go`, tests
- **Description:** PLAN §7.3 merge semantics for `query` / `query_range` log results (both
  directions), stats summing, response format preserved (incl. `encodingFlags` if present).
- **Acceptance:**
  - [ ] Unit tests: overlapping streams deduped, limit truncation correct both directions,
        empty branches, identical label sets across branches.
  - [ ] Property test: merge(top-L(A), top-L(B)) == top-L(A ∪ B) on random data.

### [ ] T3.5 — Loki label, detected_* and series endpoints
- **Depends on:** T3.3, T1.9
- **Files:** `internal/fanout/stringset.go`, `internal/enforce/logql/metadata.go`, tests
- **Acceptance:**
  - [ ] Union, sort, dedupe, denied labels removed; `series` uses `match[]` expansion.
  - [ ] When the client omits `query` on labels/label-values/detected_*, the policy filter is still applied (omission is never a pass-through).

### [ ] T3.6 — Loki stats, volume, patterns
- **Depends on:** T3.3, T1.9
- **Files:** `internal/fanout/sumcounts.go`, tests
- **Acceptance:**
  - [ ] Summation per response type; `Effective.Disjoint=false` sets span attribute
        `merge.approximate=true` and a response warning field if the API supports warnings.

### [ ] T3.7 — Loki tail WebSocket fan-out
- **Depends on:** T3.3, T1.9
- **Files:** `internal/fanout/tail.go`, `internal/proxy/websocket.go`, tests
- **Description:** Accept client WebSocket, open one upstream WebSocket per term, merge frames,
  dedupe (bounded LRU), close all on any error/close, propagate `dropped_entries`.
- **Acceptance:**
  - [ ] Tests with fake upstream WebSocket servers: merging, dedupe, upstream close → client
        close with reason, client close → upstreams closed.
  - [ ] No goroutine leaks (`goleak`).

### [ ] T3.8 — Loki differential, E2E and equivalence
- **Depends on:** T3.4, T3.5, T3.6, T3.7, T2.4 (framework)
- **Files:** `test/e2e/loki/*`
- **Acceptance:**
  - [ ] Differential fuzz: generated LogQL sent to Loki directly and via proxy; any query Loki
        accepts is either enforced or rejected by the proxy — never passed through unenforced.
  - [ ] Every proxy rewrite is accepted by Loki.
  - [ ] Isolation and equivalence suites analogous to T2.4 (log + metric queries, tail).

---

## M4 — TraceQL (parallel with M2, M3 after M1)

> Clean room applies. Only public TraceQL docs and `docs/research/traceql.md`.

### [x] T4.1 — `pkg/traceql` lexer
- **Depends on:** T0.5
- **Files:** `pkg/traceql/lexer.go`, `pkg/traceql/doc.go`, tests
- **Acceptance:** as T3.1 for TraceQL tokens (PLAN §7.4).

### [x] T4.2 — `pkg/traceql` parser
- **Depends on:** T4.1
- **Files:** `pkg/traceql/parser.go`, `pkg/traceql/ast.go`, tests
- **Acceptance:**
  - [x] Own corpus ≥ 120 queries from public doc examples (doc URL per case), incl. structural
        operators, pipelines, metrics functions, `select()`.
  - [x] Every spanset filter located with correct byte range; fuzz round-trip.

### [ ] T4.3 — TraceQL injection and denied attributes
- **Depends on:** T4.2, T1.11
- **Files:** `pkg/traceql/inject.go`, `internal/enforce/traceql/*`
- **Acceptance:**
  - [ ] `{ X }` → `{ (X) && (dnf) }`, empty filter → `{ dnf }`, for every filter in the query.
  - [ ] Property fuzz: every spanset filter in output contains the DNF; output re-parses.
  - [ ] Legacy `tags=` without `q` → 400.
  - [ ] Injection parenthesises original filter, DNF and each disjunct (equal-precedence `&&`/`||`); absent `q` on tag endpoints yields a policy-only filter; legacy `tags`/`minDuration`/`maxDuration` → 400.

### [ ] T4.4 — Tempo search metadata redaction
- **Depends on:** T4.3, T1.9
- **Files:** `internal/fanout/tempo_search.go`, tests
- **Description:** PLAN §7.4 redaction of root span fields when the root span is not covered.
  Field list from `docs/research/traceql.md`.
- **Acceptance:**
  - [ ] Unit tests with recorded response shapes (self-generated against a container, not copied
        from Tempo source).
  - [ ] `rootServiceName`, `rootTraceName` and `serviceStats` redacted; both `spanSet` and `spanSets` rewritten; decision recorded on trace-level `durationMs`/`startTimeUnixNano`.

### [ ] T4.5 — Trace by ID filter
- **Depends on:** T4.3, T1.9
- **Files:** `internal/enforce/traceql/tracebyid.go`, tests
- **Description:** `traceByID: deny|filter` for v1 and v2 endpoints, protobuf (`pkg/tempopb`)
  and JSON. Attribute matching against DNF terms (resource + span attributes).
- **Acceptance:**
  - [ ] Spans outside policy removed; empty result → 404; formats preserved.
  - [ ] `pkg/tempopb` is the only Tempo import (license job green).
  - [ ] Client `q` on trace by ID → 400; only `start`/`end` forwarded.

### [ ] T4.6 — Tempo differential, E2E and equivalence
- **Depends on:** T4.4, T4.5, T2.4 (framework)
- **Files:** `test/e2e/tempo/*`
- **Acceptance:** as T3.8 for Tempo (search, tag values, metrics, trace by ID).

---

## M5 — Release 0.1.0

### [ ] T5.1 — Helm chart
- **Depends on:** T1.11
- **Files:** `charts/querdian/**`
- **Description:** PLAN §14 chart contents, tracing values → `OTEL_*` env + Downward API
  resource attributes, `values.schema.json`, `helm lint` + `ct` (chart-testing) in CI.
- **Acceptance:**
  - [ ] `helm install` on kind succeeds; pods ready; leader election lease present.
  - [ ] NetworkPolicy example restricts backend access to proxy pods.

### [ ] T5.2 — Image and release pipeline ∥
- **Depends on:** T1.11
- **Files:** `Dockerfile`, `.goreleaser.yaml`, `.github/workflows/release.yml`
- **Acceptance:**
  - [ ] Multi-arch distroless image, non-root, read-only root fs compatible.
  - [ ] Tag push → GitHub release with binaries, signed image (cosign keyless), SBOM, SLSA
        provenance; chart published to OCI registry.

### [ ] T5.3 — Quickstart on kind ∥
- **Depends on:** T5.1, T2.4, T3.8, T4.6
- **Files:** `hack/quickstart/*`, `docs/quickstart.md`
- **Description:** One script: kind cluster, Keycloak (realm, users, groups), Prometheus, Loki,
  Tempo, OTel Collector, Grafana with data sources pointing at the proxy (OAuth pass-through),
  sample workloads in two namespaces, example policies.
- **Acceptance:**
  - [ ] From zero to working isolation demo in ≤ 15 minutes on a laptop.
  - [ ] Two demo users demonstrably see different data in Grafana.

### [ ] T5.4 — Documentation ∥
- **Depends on:** T2.4, T3.8, T4.6
- **Files:** `docs/**`
- **Description:** Policy guide (CRDs, union semantics, tenants, denied labels), operations
  (config, scaling, upgrades), security model + `docs/security/threat-model.md` (PLAN §12),
  troubleshooting with ready-made TraceQL/PromQL queries (denies by reason, slow fan-outs,
  upstream errors), known limitations (absent edge cases, approximate stats under overlap,
  unsupported syntax → 400).
- **Acceptance:**
  - [ ] Every reason code and status reason documented.
  - [ ] Namespace-label integrity requirement prominent in policy guide and security model.

### [ ] T5.5 — Grafana dashboard ∥
- **Depends on:** T1.10
- **Files:** `charts/querdian/dashboards/*.json`, `docs/dashboards.md`
- **Acceptance:**
  - [ ] Panels: request rate by decision/reason, latency (with exemplar links), upstream errors,
        fan-out terms distribution, snapshot health, policy readiness.

### [ ] T5.6 — CNCF/OpenSSF readiness ∥
- **Depends on:** T0.2, T5.4
- **Files:** `docs/security/self-assessment.md`, badge in README
- **Acceptance:**
  - [ ] OpenSSF Best Practices "passing" criteria satisfied (badge application submitted `HUMAN`).
  - [ ] TAG Security self-assessment draft complete.

### [ ] T5.7 — Release 0.1.0 `HUMAN` + agent
- **Depends on:** all M5 tasks
- **Acceptance:**
  - [ ] CHANGELOG, release notes, tag `v0.1.0`, release pipeline green, quickstart verified
        against the released artefacts.

---

## Dependency overview

```
T0.0 ─┬─ T0.1 ─┬─ T0.2
      │        ├─ T1.1 ─┐
      │        ├─ T1.2 ─┼─ T1.3 ─ T1.4 ─ T1.5 ─ T1.6 ─┐
      │        │        ├─ T1.7 ────────────────────────┤
      │        │        └─ T1.8 ─ T1.9 ─────────────────┼─ T1.11 ─┬─ M2 (T2.*)
      │        └─ T1.10 ────────────────────────────────┘         ├─ M3 (T3.3+)
      ├─ T0.3 ─ (T1.8)                                            ├─ M4 (T4.3+)
      ├─ T0.4                                                     └─ M5
      └─ T0.5 ─┬─ T3.1 ─ T3.2 ─ (T3.3)
               └─ T4.1 ─ T4.2 ─ (T4.3)
```

Lexer/parser work (T3.1–T3.2, T4.1–T4.2) can start as soon as T0.5 is done, in parallel with M1.
