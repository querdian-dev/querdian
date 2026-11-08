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

### [x] T1.8 — Enforce interface and route tables
- **Depends on:** T1.2, T0.3
- **Files:** `internal/enforce/enforce.go`, `internal/routes/*`, tests
- **Description:** PLAN §7.1 types and errors. Route tables per backend type from
  `docs/endpoints.md` (PLAN §8) with route templates (`label/:name/values`); lookup function
  returning `Endpoint{Kind, Param, Template, Methods, Websocket}`. Unknown → deny.
- **Acceptance:**
  - [x] Table test for every endpoint row in `docs/endpoints.md`, plus path traversal /
        double-slash / encoded-slash / trailing-slash cases (all must not escape to a different
        route).
  - [x] Route templates carry a parameter allow-list (PLAN §9); `storeMatch[]` and unlisted params are dropped/rejected; Loki `detected_labels` and Loki POST on `query`/`query_range` are decided by a black-box test (record result in `docs/endpoints.md`).

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

### [x] T1.11 — Server wiring and health
- **Depends on:** T1.6, T1.7, T1.9, T1.10
- **Files:** `internal/server/*`, `cmd/querdian/main.go`
- **Description:** Two HTTP servers (proxy, management). Handler chain: otelhttp → readiness
  gate (503) → route → auth (required for all routes, including `safe`) → resolve →
  enforce (skipped for `safe`) → proxy. Follow the lifecycle in PLAN §3 exactly. Graceful shutdown. `/healthz` (process), `/readyz` (snapshot ready + issuer
  discovery done).
- **Acceptance:**
  - [x] Integration test (envtest + httptest upstream + test issuer): unrestricted policy →
        request proxied with tenant header; no policy → 403; before snapshot ready → 503.
  - [x] Server span carries `querdian.decision`, `.decision.reason`, `.policies`.

---

## M2 — PromQL (parallel with M3, M4 after M1)

### [x] T2.1 — PromQL single-term injection and denied labels
- **Depends on:** T1.11
- **Files:** `internal/enforce/promql/*`
- **Description:** PLAN §7.2 parse, denied-label checks, single-term injection into every
  vector selector (incl. matrix selectors, subqueries, `@`, `offset`).
- **Acceptance:**
  - [x] ≥ 60 table cases covering all PromQL expression node types.
  - [x] Output re-parses; every `VectorSelector` contains all term matchers.
  - [x] Denied label in matcher / `by` / `on` / `label_replace` → `ErrDeniedLabel`.

### [x] T2.2 — PromQL or-lifting for multiple terms
- **Depends on:** T2.1
- **Files:** `internal/enforce/promql/union.go`, tests, `docs/promql-enforcement.md`
- **Description:** PLAN §7.2 multi-term rules. Strategy `or_lift`.
- **Acceptance:**
  - [x] Table cases: instant selector, every range-vector function, nested aggregations,
        binary ops with `on`/`ignoring`, subqueries, `absent`/`absent_over_time` (documented edge).
  - [x] Property fuzz: random query × random 1–4 terms → output re-parses, every selector carries
        exactly one term's matchers, number of selector copies = original × terms.

### [x] T2.3 — Prometheus metadata endpoints
- **Depends on:** T2.1
- **Files:** `internal/enforce/promql/metadata.go`, tests, `internal/enforce/enforce.go` (additive `Plan.ResponseFilter`)
- **Description:** `match[]` expansion for `series`, `labels`, `label/<n>/values`; denied label
  filtering of `labels` responses and 403 on `label/<denied>/values`. Strategy `match_expand`.
- **Acceptance:**
  - [x] Tests for: no user `match[]`, one, several; × 1 and 3 terms.
  - [x] `labels` response filtering is streaming-safe (bounded buffering, cap applies).
  - [x] When the client omits `match[]`, one policy `match[]` per term is still sent (omission is never a pass-through).

### [x] T2.4 — Prometheus E2E and semantic equivalence
- **Depends on:** T2.2, T2.3
- **Files:** `test/e2e/prometheus/*`, `test/e2e/framework/*`
- **Description:** testcontainers Prometheus with seeded data (backfilled via promtool TSDB
  blocks or remote-write from the test). Framework reused by M3/M4: start upstream, start proxy
  in-process with a static Snapshot (no k8s), issue requests as a user.
- **Acceptance:**
  - [x] Isolation: user A never sees series of namespace B across all enforced endpoints.
  - [x] Equivalence: for a corpus of ≥ 40 queries × term sets, rewritten result equals the union
        of per-term results computed by the test.
  - [x] Runs in CI under 5 minutes.

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

### [x] T3.3 — LogQL injection, denied labels, or-lifting
- **Depends on:** T3.2, T1.11
- **Files:** `pkg/logql/inject.go`, `internal/enforce/logql/*`
- **Description:** Single-term injection into every stream selector; conservative denied-label
  token scan; or-lifting for metric queries; fan-out plan for log queries.
- **Acceptance:**
  - [ ] Property fuzz analogous to T2.2.
  - [ ] Log query + 2 terms → `Plan` with 2 requests, `MergeLokiStreams`.
  - [ ] `label_replace` string arguments are scanned for denied labels; tests cover overwriting/removing a policy label.

### [x] T3.4 — Loki stream merge
- **Depends on:** T3.3, T1.9
- **Files:** `internal/fanout/loki_streams.go`, tests
- **Description:** PLAN §7.3 merge semantics for `query` / `query_range` log results (both
  directions), stats summing, response format preserved (incl. `encodingFlags` if present).
- **Acceptance:**
  - [x] Unit tests: overlapping streams deduped, limit truncation correct both directions,
        empty branches, identical label sets across branches.
  - [x] Property test: merge(top-L(A), top-L(B)) == top-L(A ∪ B) on random data.

### [x] T3.5 — Loki label, detected_* and series endpoints
- **Depends on:** T3.3, T1.9
- **Files:** `internal/fanout/stringset.go`, `internal/enforce/logql/metadata.go`, tests
- **Acceptance:**
  - [x] Union, sort, dedupe, denied labels removed; `series` uses `match[]` expansion.
  - [x] When the client omits `query` on labels/label-values/detected_*, the policy filter is still applied (omission is never a pass-through).

### [x] T3.6 — Loki stats, volume, patterns
- **Depends on:** T3.3, T1.9
- **Files:** `internal/fanout/sumcounts.go`, tests
- **Acceptance:**
  - [x] Summation per response type; `Effective.Disjoint=false` sets span attribute
        `merge.approximate=true` and a response warning field if the API supports warnings.
        (None of the four APIs has a warnings field; the marker is the response header
        `X-Querdian-Approximate: true`, see `fanout.ApproximateHeader`.)

### [x] T3.7 — Loki tail WebSocket fan-out
- **Depends on:** T3.3, T1.9
- **Files:** `internal/fanout/tail.go`, `internal/proxy/websocket.go`, tests
- **Description:** Accept client WebSocket, open one upstream WebSocket per term, merge frames,
  dedupe (bounded LRU), close all on any error/close, propagate `dropped_entries`.
- **Acceptance:**
  - [x] Tests with fake upstream WebSocket servers: merging, dedupe, upstream close → client
        close with reason, client close → upstreams closed.
  - [x] No goroutine leaks (`goleak`).

### [x] T3.8 — Loki differential, E2E and equivalence
- **Depends on:** T3.4, T3.5, T3.6, T3.7, T2.4 (framework)
- **Files:** `test/e2e/loki/*`
- **Acceptance:**
  - [x] Differential fuzz: generated LogQL sent to Loki directly and via proxy; any query Loki
        accepts is either enforced or rejected by the proxy — never passed through unenforced.
  - [x] Every proxy rewrite is accepted by Loki.
  - [x] Isolation and equivalence suites analogous to T2.4 (log + metric queries, tail).

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

### [x] T4.3 — TraceQL injection and denied attributes
- **Depends on:** T4.2, T1.11
- **Files:** `pkg/traceql/inject.go`, `internal/enforce/traceql/*`
- **Acceptance:**
  - [x] `{ X }` → `{ (X) && (dnf) }`, empty filter → `{ dnf }`, for every filter in the query.
  - [x] Property fuzz: every spanset filter in output contains the DNF; output re-parses.
  - [x] Legacy `tags=` without `q` → 400.
  - [x] Injection parenthesises original filter, DNF and each disjunct (equal-precedence `&&`/`||`); absent `q` on tag endpoints yields a policy-only filter; legacy `tags`/`minDuration`/`maxDuration` → 400.

### [x] T4.4 — Tempo search metadata redaction
- **Depends on:** T4.3, T1.9
- **Files:** `internal/fanout/tempo_search.go`, tests
- **Description:** PLAN §7.4 redaction of root span fields when the root span is not covered.
  Field list from `docs/research/traceql.md`.
- **Acceptance:**
  - [x] Unit tests with recorded response shapes (self-generated against a container, not copied
        from Tempo source).
  - [x] `rootServiceName`, `rootTraceName` and `serviceStats` redacted; both `spanSet` and `spanSets` rewritten; decision recorded on trace-level `durationMs`/`startTimeUnixNano`.
  - [x] `v2/search/tags` and tag-values responses are filtered against `DeniedLabels` (denied attribute names and scopes must not be listed); the T4.3 enforcer only restricts which spans are scanned.

### [x] T4.5 — Trace by ID filter
- **Depends on:** T4.3, T1.9
- **Files:** `internal/enforce/traceql/tracebyid.go`, tests
- **Description:** `traceByID: deny|filter` for v1 and v2 endpoints, protobuf (`pkg/tempopb`)
  and JSON. Attribute matching against DNF terms (resource + span attributes).
- **Acceptance:**
  - [x] Spans outside policy removed; empty result → 404; formats preserved.
  - [x] `pkg/tempopb` is the only Tempo code used. It is copied into `internal/tempopb` by
    `hack/sync-tempopb.sh` (Tempo's tags are not importable as Go module versions) and a monthly
    workflow reports drift against the latest release.
  - [x] Client `q` on trace by ID → 400; only `start`/`end` forwarded.

### [x] T4.6 — Tempo differential, E2E and equivalence
- **Depends on:** T4.4, T4.5, T2.4 (framework)
- **Files:** `test/e2e/tempo/*`
- **Acceptance:** as T3.8 for Tempo (search, tag values, metrics, trace by ID).
  - [x] Isolation (search incl. redaction, tags, tag values, metrics, trace by ID v1/v2 JSON/protobuf), two tenants, three namespaces, cross-namespace traces with root in another namespace.
  - [x] Differential: ~1000 generated TraceQL checks (filters, boolean combos, regex, structural, pipelines, select/by, metrics) direct vs proxy; no unenforced pass-through, every rewrite accepted by Tempo.
  - [x] Equivalence against a reference Tempo seeded with only the granted spans (method in `test/e2e/tempo/tempo_test.go`); structural operators checked by invariants (oracle undefined).
  - [x] Root-metadata oracle confirmed (rootServiceName/rootName/traceDuration filters, tag values, and `compare()`); fixed by rejecting them unless the root is covered.
  - Tag values with `q` are approximate in Tempo 2.9.0 (omit values, may return the attribute key); checked by containment only.

---

## M5 — Release 0.1.0

### [~] T5.1 — Helm chart
- **Depends on:** T1.11
- **Files:** `charts/querdian/**`
- **Description:** PLAN §14 chart contents, tracing values → `OTEL_*` env + Downward API
  resource attributes, `values.schema.json`, `helm lint` + `ct` (chart-testing) in CI.
- **Acceptance:**
  - [x] `helm install` on kind succeeds; pods ready; leader election lease present.
        (Verified with `hack/kind/verify-chart.sh` on kind v0.33 / Kubernetes 1.37, podman provider,
        one control-plane node: 2/2 replicas Ready against a stub in-cluster OIDC issuer, Lease
        `querdian-leader-election` held, sample AccessPolicy status written by the leader
        (`Compiled` / `TenantNotAssigned`, so RBAC works), no error/warn logs; `helm upgrade` with a
        changed config rolls the Deployment via `checksum/config`; `helm uninstall` leaves nothing
        but the CRDs. `ct` CI wiring is part of T0.2/CI.)
  - [x] NetworkPolicy example restricts backend access to proxy pods.
        (Verified on kindnet, which enforces policies: with `backendProtection` a client pod can no
        longer reach the dummy backend, a pod with the proxy's labels in the release namespace can;
        with `networkPolicy.enabled` and empty `ingress.from` nobody reaches the proxy or management
        port, `ingress.from` admits only the listed namespace, and querdian stays Ready under its own
        egress policy. Not covered: real proxy traffic to real backends, as the backends are dummies.)

### [x] T5.2 — Image and release pipeline ∥
- **Depends on:** T1.11
- **Files:** `Dockerfile`, `.goreleaser.yaml`, `.github/workflows/release.yml`
- **Acceptance:**
  - [x] Multi-arch distroless image, non-root, read-only root fs compatible.
        (`v0.1.0-rc.2`: the index lists `linux/amd64` and `linux/arm64`; the published image runs
        as `65532:65532` with `--read-only --cap-drop ALL --security-opt no-new-privileges`, has no
        `/bin/sh`, 52 MB.)
  - [x] Tag push → GitHub release with binaries, signed image (cosign keyless), SBOM, SLSA
        provenance; chart published to OCI registry.
        (`v0.1.0-rc.2`, run 40188933704, verified anonymously per docs/releasing.md §5: release with
        archives, SPDX SBOMs and the checksums bundle (`cosign verify-blob --bundle` OK); image and
        chart `cosign verify` OK; SLSA v1 provenance for the archives and the image. `rc.1` failed
        in the chart job and led to #8; #7 fixed two bugs found before tagging.)

### [~] T5.3 — Quickstart on kind ∥
- **Depends on:** T5.1, T2.4, T3.8, T4.6
- **Files:** `hack/quickstart/*`, `docs/quickstart.md`
- **Description:** One script: kind cluster, Keycloak (realm, users, groups), Prometheus, Loki,
  Tempo, OTel Collector, Grafana with data sources pointing at the proxy (OAuth pass-through),
  sample workloads in two namespaces, example policies.
- **Acceptance:**
  - [x] From zero to working isolation demo in ≤ 15 minutes on a laptop (measured 3 min 37 s warm
        build cache, see `docs/quickstart.md`; cold first run not timed separately).
  - [x] Two demo users demonstrably see different data in Grafana: `hack/quickstart/verify.sh`
        (71 assertions) drives the Keycloak login headlessly and queries through the Grafana data
        sources with OAuth pass-through. The interactive Explore UI in a real browser was NOT
        automated (manual walkthrough in the doc).
  - [x] About 24 h of deterministic demo history for metrics, logs and traces in both sample
        namespaces plus live data at the "now" end (`hack/quickstart/demogen`, `docs/quickstart.md`);
        verified by `verify.sh`: oldest sample, log line and trace are >= 23 h old, per-user isolation
        of the history, denied label hidden, error logs link to traces. Prometheus via promtool blocks,
        Loki via OTLP push, Tempo via OTLP push with raised `ingestion_time_range_slack` and a flush.
  - [x] Four provisioned Grafana dashboards (service overview, logs, traces and latency, what can I
        see) in `hack/quickstart/dashboards`; JSON, data source uids and all PromQL/LogQL/TraceQL
        parse-checked by `go test ./hack/quickstart/dashboards/`; every panel query executed as
        alice, bob and carol through Grafana by `verify.sh` (150 assertions in total). Visual
        rendering in a real browser was NOT checked.
  - [x] Footprint measured with the history: VM 2.6 GiB used, 3.3 GiB available of 5.9 GiB;
        `up.sh` 2 min 40 s, `verify.sh` 23 s (see `docs/quickstart.md`).
  - Note: the chart's `backendProtection` names its policy per namespace, so several targets in one
    namespace collide; the quickstart works around it with one `matchExpressions` target.

### [~] T5.4 — Documentation ∥
- **Depends on:** T2.4, T3.8, T4.6
- **Status:** written against the code at `integrate-m3-m4` (T2.4, T3.8, T4.6 still open). Revisit after
  they land (differential/E2E findings) and when the chart, image and dashboards exist; the
  quickstart and dashboards docs belong to T5.3 / T5.5.
- **Files:** `docs/**`
- **Description:** Policy guide (CRDs, union semantics, tenants, denied labels), operations
  (config, scaling, upgrades), security model + `docs/security/threat-model.md` (PLAN §12),
  troubleshooting with ready-made TraceQL/PromQL queries (denies by reason, slow fan-outs,
  upstream errors), known limitations (absent edge cases, approximate stats under overlap,
  unsupported syntax → 400).
- **Acceptance:**
  - [x] Every reason code and status reason documented (cross-checked against `handler.go`,
        `internal/policy/types.go` and `internal/auth/errors.go`).
  - [x] Namespace-label integrity requirement prominent in policy guide and security model.

### [x] T5.5 — Grafana dashboard ∥
- **Depends on:** T1.10
- **Files:** `charts/querdian/dashboards/*.json`, `docs/dashboards.md`
- **Acceptance:**
  - [x] Panels: request rate by decision/reason, latency (with exemplar links), upstream errors,
        fan-out terms distribution, snapshot health, policy readiness.

### [~] T5.6 — CNCF/OpenSSF readiness ∥
- **Depends on:** T0.2, T5.4
- **Files:** `docs/security/self-assessment.md`, badge in README
- **Acceptance:**
  - [ ] OpenSSF Best Practices "passing" criteria satisfied (badge application submitted `HUMAN`).
        (Checklist: `docs/openssf-best-practices.md`. Registered on bestpractices.dev as project
        15762, status "in progress"; the questionnaire is not complete. Repository public, private
        vulnerability reporting and the first release (0.1.0) are done; the checklist rows for the
        GitHub settings have not been re-checked since. README badges are active.)
  - [x] TAG Security self-assessment draft complete (`docs/security/self-assessment.md`; gaps listed
        in it; not reviewed by anyone outside the project).

### [ ] T5.7 — Release 0.1.0 `HUMAN` + agent
- **Depends on:** all M5 tasks
- **Acceptance:**
  - [ ] CHANGELOG, release notes, tag `v0.1.0`, release pipeline green, quickstart verified
        against the released artefacts.
        (CHANGELOG `[0.1.0]` and final release notes in the release pull request. Pipeline and
        quickstart verified on `v0.1.0-rc.2`: `verify.sh` 153 passed against the published image and
        chart. Open: tag `v0.1.0` and repeat docs/releasing.md §5 and §7 against it.)

---

## M6 — Multi-cluster

The design is PLAN §18. Read it completely before you start a task in this milestone.

### [x] T6.1 — Configuration and CRDs
- **Depends on:** —
- **Files:** `api/v1alpha1/`, `internal/config/`, `config/crd/`, `charts/querdian/crds/`
- **Acceptance:**
  - [x] The `multicluster` block and `clusterLabel` exist. A config without the block behaves as today.
  - [x] Querdian refuses to start for each condition in §18.4. The check finds an explicit
        `maxStaleness: 0s`, which the current default pattern would change to the default.
  - [x] `ClusterRule` has the field `allClusters`. A CEL rule rejects it together with `unrestricted: true`.
  - [x] The `SpokeCluster` and `SpokePolicy` kinds exist, with envtest coverage. Both have a status
        subresource. `SpokePolicy` has typed fields and the CEL rules of the spoke kinds.
  - [x] Shared contracts for the parallel tasks: `internal/spokesync` (wire types, `Digest`,
        `ObjectName`, `IdentityMatchesName`), `CompiledRule.Origin`, and `config.ValidLabelName`.

### [x] T6.2 — Policy origin in the compiler
- **Depends on:** T6.1
- **Files:** `internal/policy/`
- **Acceptance:**
  - [x] The compiler input keeps the origin of each policy. Namespace labels and policy names do not
        collide across clusters.
  - [x] Each spoke rule and each hub `AccessPolicy` rule carries the implicit cluster matcher.
  - [x] A spoke rule never compiles to `Unrestricted: true`. A test covers spoke `unrestricted: true`,
        and a `ClusterAccessPolicy` rule with `unrestricted: false` and no matchers. An `AccessPolicy` rule
        with no matchers stays valid.
  - [x] A `SpokePolicy` grants nothing in these conditions: no `multicluster` block, no `SpokeCluster`,
        a `SpokeCluster` with the name of `hubCluster`, or a `spec.identity` whose last path segment is
        not equal to the name of the `SpokeCluster`. A test covers a suffix that is not a full path
        segment, such as `.../agent/x-cluster-a` for `cluster-a`.
  - [x] The cluster scope rule in §18.3 applies for each backend of a hub rule, with the new reason
        `InvalidClusterScope`. Tests cover `NotEqual`, `RegexNoMatch`, open regexes and empty values on
        the cluster label, `allClusters` with a cluster matcher, a rule with several backends, and a
        spoke rule with `allClusters`.
  - [x] Without the `multicluster` block, `allClusters` has no effect and the policy stays valid.
  - [x] The tenant allow-list (§18.9) applies, with the new reason `TenantNotAllowed`.
  - [x] A fuzz target shows this property after DNF normalisation: a label set of a different cluster
        never matches the terms of a spoke alone, also if the rule has its own cluster matchers. Use
        `FuzzNormaliseEquivalence` for a mix of hub and spoke terms.

### [x] T6.3 — Time limit in `Resolve`
- **Depends on:** T6.2
- **Files:** `internal/policy/`, `internal/server/`, `internal/ui/`, `internal/observability/`, `internal/proxy/`
- **Acceptance:**
  - [x] Rules 3 to 8 of §18.6 hold. Tests cover each condition in rule 4, a deleted `SpokeCluster` and a
        re-created `SpokeCluster`.
  - [x] If all rules of a user expire, `Resolve` returns an error and never an empty `Effective`. The
        reason is `policy_source_expired` with status 403, in the handler and in the dashboard.
  - [x] Expired rules keep `labels.deny` and `labels.redact`. A test shows that expiry never widens access.
  - [x] An open Loki tail closes within 10 seconds after expiry or suspension of a spoke in its policy.
  - [x] The dashboard dry run uses the same check.
  - [x] The metric `querdian_cluster_policy_age_seconds` exists.

### [x] T6.4 — Controller for the hub objects
- **Depends on:** T6.3
- **Files:** `internal/controller/`
- **Acceptance:**
  - [x] Informers on `SpokeCluster` and `SpokePolicy` feed the snapshot and the deadline table. They
        start only if the `multicluster` block is set.
  - [x] A change of `lastSyncTime`, `observedGeneration`, `metadata.generation`, `suspended` or
        `spec.maxStaleness` reaches the deadline table. An envtest covers each field. A heartbeat starts
        no recompile. The current filter ignores updates that do not change the generation.
  - [x] A replica reports ready only when the deadline table has its first complete state.
  - [x] A change of `spec.tenants` or `defaultTenant` starts a recompile. A test shows that a smaller
        allow-list removes the grant.
  - [x] An unreachable spoke does not affect startup or readiness.
  - [x] The leader writes status conditions on `SpokePolicy` objects.

### [x] T6.5 — Receiver
- **Depends on:** T6.1
- **Files:** `cmd/querdian/`, new package `internal/receiver/`
- **Acceptance:**
  - [x] `querdian receiver` terminates mTLS with its own required client CA and takes the cluster from
        the cert only.
  - [x] It uses leader election, and only the leader reports ready. A test shows that a second sync for a
        cluster gets `409` during a running sync, and that a sync for a different cluster is not affected.
  - [x] Tests reject a cert with no URI SAN, a cert with several URI SANs, a cert from a different CA, an
        unknown identity, and an identity on two `SpokeCluster` objects.
  - [x] A test shows that the cert of one spoke cannot change the objects of a different spoke, also for
        crafted names such as cluster `a-b` with policy `c`, and cluster `a` with policy `b-c`.
  - [x] It reads the `SpokeCluster` directly at the start of each sync and writes `status` with a
        `resourceVersion` precondition. A test shows that a sync that overlaps a spec change validates
        nothing.
  - [x] Tests reject the whole sync for each condition under "Unknown input" in §18.5, and
        `lastSyncTime` does not change.
  - [x] It writes changed objects, then deletes, then sets `lastSyncTime` and `observedGeneration`. It
        sets an owner reference on each `SpokePolicy`.
  - [x] A test stops a sync between the writes and the deletes. The next digest does not renew
        `lastSyncTime`.
  - [x] The sync reply carries the status conditions and omits `CrossTenantUnionUnsupported`. Each
        condition carries the generation of the spoke object.

### [x] T6.6 — Agent
- **Depends on:** T6.5
- **Files:** `cmd/querdian/`, new package `internal/agent/`
- **Acceptance:**
  - [x] `querdian agent` sends a digest at each heartbeat and the full state if the digest differs.
  - [x] It sends nothing until it has a complete read, and it reads the API server directly before each
        heartbeat. A test shows that an agent without an API server does not renew `lastSyncTime`.
  - [x] It retries at least each minute after a failure, and it uses leader election.
  - [x] It writes status conditions to the spoke objects. Its RBAC is the minimum in §18.5, with no
        wildcard on `status`. It handles `409` with a retry.

### [x] T6.7 — Chart and documentation
- **Depends on:** T6.4, T6.6
- **Files:** `charts/querdian/`, `docs/`, `PLAN.md`, `CHANGELOG.md`
- **Acceptance:**
  - [x] The chart can install the hub with the receiver, or the agent only for a spoke. It includes the
        optional alert at 24 hours.
  - [x] The chart RBAC gives the receiver write access to `SpokePolicy` and `spokeclusters/status` only.
        The proxy gets read access and `spokepolicies/status` only. A test covers this.
  - [x] The threat model covers these items:
        - the spoke admin as an actor, and the cluster label in section 1;
        - the receiver as a new attack surface, because a compromised receiver can grant the data of all
          spokes;
        - the agent cert, the procedure in §18.7, and the time limit in section 10;
        - the policies of all spokes in the dashboard, and the changes to the trusted set.
  - [x] `docs/limitations.md` lists the limits in §18.10, and the statement about TLS covers the receiver.
  - [x] The statements about open tails in `docs/limitations.md` and in the threat model describe the
        close on expiry and on suspension.
  - [x] An operations guide describes all setups in §18.9, the requirements for the ingest gateway, the
        requirements for the agent CA, the removal of a spoke, and the risk of a rollback.
  - [x] The operations guide gives the ordered steps in §18.8, including the review of each hub
        `ClusterAccessPolicy`, and a verification step for hub telemetry on each signal.
  - [x] `docs/limitations.md` and the policy guide describe the cluster scope rule and the old hub data.
  - [x] `docs/troubleshooting.md` lists `policy_source_expired`, and PLAN §4.4 lists the new reasons.
  - [x] PLAN §3, §4, §5.1 and §12 describe the new kinds, and §18 is no longer marked as a design only.
  - [x] The chart README and `CHANGELOG.md` describe the change.
  - [x] The policy dashboard shows the origin and the expired state of a policy.

### [x] T6.8 — End-to-end tests
- **Depends on:** T6.7
- **Files:** `test/e2e/multicluster/`
- **Acceptance:**
  - [x] A hub and two spokes, each with its own envtest API server. The hub runs the controller, the
        proxy handler chain and the receiver in the test process, and each spoke runs the agent. The
        backends are real Prometheus, Loki and Tempo containers. The package comment gives the reason
        for envtest instead of kind. A policy in spoke A never returns data of spoke B on all three
        backends, also for equal namespace names.
  - [x] The suite covers all setups in §18.9 that the backends support, with the limits of federated
        tenants.
  - [x] A new spoke does not widen a hub rule with a cluster matcher. A hub `AccessPolicy` sees hub
        telemetry with `cluster="<hubCluster>"`, and it does not see hub data with no cluster label.
  - [x] Grants stop after the time limit and return after the next sync.
  - [x] The procedure in §18.7 works. `suspended: true` stops new requests and closes an open Loki tail,
        and grants return only after a new valid sync.

### [ ] T6.9 — Kind smoke test of the chart
- **Depends on:** T6.7, T6.8
- **Files:** `test/`, `charts/querdian/`, `.github/workflows/`
- **Acceptance:**
  - [ ] A hub and one spoke on kind, installed with the chart and the real `querdian` image: the proxy,
        the receiver and the agent as Deployments with the RBAC of the chart.
  - [ ] The agent authenticates to its API server with a ServiceAccount token, and it reaches the
        receiver over the network path between the clusters, with the cert files from Secrets.
  - [ ] A smoke test shows a grant of the spoke through the proxy, its stop after `suspended: true`, and
        its return after the next sync. The detailed cases stay in the envtest suite of T6.8.

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

M6: T6.1 ─┬─ T6.2 ─ T6.3 ─ T6.4 ─┬─ T6.7 ─ T6.8 ─ T6.9
          └─ T6.5 ─ T6.6 ───────┘

Lexer/parser work (T3.1–T3.2, T4.1–T4.2) can start as soon as T0.5 is done, in parallel with M1.
