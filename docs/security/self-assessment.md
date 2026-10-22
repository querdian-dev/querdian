# Querdian security self-assessment (DRAFT)

This follows the structure of the CNCF TAG Security
[self-assessment template](https://github.com/cncf/tag-security/blob/main/community/assessments/guide/self-assessment.md).
It describes **only what exists in this repository today**; where something is missing it says so and
the item is collected in [Gaps](#gaps-collected). It does not repeat the attacker analysis: that
is the [threat model](threat-model.md), which is linked wherever it applies.

Status: draft by the maintainer, not reviewed by anyone outside the project. No external audit,
penetration test or TAG Security review has taken place.

## Metadata

| | |
|---|---|
| Assessment stage | Draft, pre-release (no tagged release exists) |
| Software | <https://github.com/querdian-dev/querdian> (Go module `github.com/querdian-dev/querdian`) |
| Security provider | Yes. Querdian is an access-control component: its purpose is to prevent authenticated users from reading telemetry outside their policy. |
| Languages | Go (no cgo); Helm templates; shell scripts for the quickstart; YAML |
| Software bill of materials | Not published yet. The release pipeline generates SPDX SBOMs for the archives and the image ([.goreleaser.yaml](../../.goreleaser.yaml)); it has not run. Dependencies are in `go.mod` / `go.sum`. |
| Security links | [SECURITY.md](../../SECURITY.md), [threat model](threat-model.md), [limitations](../limitations.md), [OpenSSF checklist](../openssf-best-practices.md), [operations](../operations.md) |
| License | Apache-2.0 |
| Maintainers | One ([MAINTAINERS.md](../../MAINTAINERS.md)) |

## Overview

Querdian is a Kubernetes-native reverse proxy that sits between users (typically Grafana with OAuth
pass-through) and Prometheus/Thanos, Loki and Tempo. It authenticates a bearer token with OIDC,
resolves the user's groups to policies expressed as Kubernetes CRDs, rewrites every query so that it
can only match data the policies allow, and forwards it. It is default deny and fails closed.

### Background

Prometheus, Loki and Tempo have no per-user authorisation on labels. Multi-tenancy in Loki and
Tempo (and Mimir) is by tenant header only, with no finer granularity. Teams that share a backend
therefore either run one stack per team or put a filtering layer in front. Querdian is such a layer.
Architecture and request lifecycle are in [PLAN.md](../../PLAN.md) section 3 (the plan is a design
document; where code and plan differ, the docs under `docs/` describe the code).

### Actors

* **Querdian proxy** (this project): one Deployment, usually two replicas, a single binary that
  contains the proxy, a management server and the policy controller.
* **Identity provider** (OIDC issuer): issues the tokens; trusted absolutely for `username` and
  `groups` claims.
* **Kubernetes API server**: source of the policies (`ClusterAccessPolicy`, `AccessPolicy`) and
  namespace labels; trusted.
* **Backends** (Prometheus/Thanos, Loki, Tempo): trusted, but must not be reachable by clients
  except through Querdian (NetworkPolicy support exists in the chart).
* **Users**: authenticated, possibly hostile within their authorisation (the main adversary).
* **Platform admins / namespace admins**: create policies. A namespace admin can write
  `AccessPolicy` in their namespace only; the implicit namespace matcher cannot be removed.
* **Collection pipeline** (Prometheus scrape config, OTel Collector, Alloy/Promtail): must write the
  namespace label from trusted metadata. This is outside Querdian's control and the single most
  important assumption ([threat model, section 1](threat-model.md#1-namespace-label-integrity-the-assumption-everything-rests-on)).

Diagram and trust boundary: [threat model, section 2](threat-model.md#2-assets-actors-and-trust-boundary).

### Actions

1. A user (Grafana) sends `GET`/`POST` with `Authorization: Bearer <JWT>`.
2. Route lookup (default deny), authentication (OIDC), policy resolution (exact union of the policies
   matched by user or group), request normalisation (parameter allow-list, canonical paths).
3. The enforcer builds a plan: inject matchers (single term), or-lift (PromQL/LogQL metric queries) or
   fan out and merge (LogQL logs, metadata endpoints, tail).
4. The proxy executes the plan with the tenant header set from the policy only; responses are streamed
   or merged and, for some endpoints, filtered or redacted.
5. Every response carries `X-Trace-Id`; decisions are counted in metrics and recorded on the trace.

### Goals

* Isolation: a user can read exactly the union of what their matching policies grant, on PromQL,
  LogQL and TraceQL read paths, including metadata endpoints.
* Fail closed: errors in authentication, resolution, parsing, rewriting or merging produce a
  rejection, never an unenforced pass-through. If the exact union cannot be expressed, reject.
* Queries are rewritten with parsers, never with regular expressions or string surgery.
* Clients cannot choose a tenant: only allow-listed client headers are forwarded, and the tenant
  header is set by Querdian.
* Traceable decisions without logging personal data by default.
* Supply-chain hygiene: license allowlist, DCO, pinned CI actions and base image, signed release
  artefacts (once released).

### Non-goals

* Write and ingest paths, ruler, alerting and admin APIs: all denied.
* A UI; running outside Kubernetes; gRPC and streaming APIs.
* Protecting data from an authenticated user who is entitled to see it, from a compromised identity
  provider or Kubernetes API, or from a workload that can forge the namespace label in the telemetry.
* Authentication of backends, TLS termination, rate limiting, token revocation and DDoS protection
  (these belong to the ingress, mesh, identity provider and the backends).
* Redacting denied labels from query results by default (`labels.deny` hides and blocks;
  redaction is the opt-in `labels.redact`, see [limitations](../limitations.md#denied-labels)).
* Compatibility with evoila/janus: credited as inspiration only; no code, tests or docs were copied.

## Self-assessment use

This document is a self-assessment by the maintainer to help potential adopters and reviewers (TAG
Security, CNCF TOC, OpenSSF) judge the project. It is not a security audit, not a certification and
not a guarantee. A reviewer should read it together with the threat model and the limitations; the
limitations are the part most likely to matter in practice.

## Security functions and features

### Critical

These are the components whose failure breaks isolation. Each is covered by unit tests, most by fuzzing
and by differential tests against real backends.

| Function | Where | What it guarantees / how it is tested |
|---|---|---|
| Authentication | `internal/auth` | OIDC verification (signature, issuer, audience, expiry) of every request, including `safe` routes. Test issuer with generated keys; custom CA. |
| Default-deny routing, canonical paths, parameter allow-lists | `internal/routes`, `internal/proxy` | Unknown path/method/parameter is rejected; encodings cannot alias another route; duplicate enforcement parameters rejected. Table tests incl. traversal and encoded-slash cases. |
| Policy compilation and exact-union resolution | `internal/policy` | Invalid policy grants nothing; users and groups are separate keyspaces; DNF normalisation preserves semantics (fuzzed property: normalised DNF matches iff input DNF matches); term cap. |
| Tenant handling | `internal/policy`, `internal/proxy` | Tenant header only from policy; `__tenant_id__` bound only for federation headers; cross-tenant unions unsupported by the backend are rejected. |
| Query rewriting | `internal/enforce/*`, `pkg/logql`, `pkg/traceql` | Every selector or spanset filter carries the policy. PromQL uses the Prometheus parser; LogQL and TraceQL use clean-room parsers that are stricter than the backends. Property fuzzing, 10 fuzz targets, differential suites (see below). |
| Denied labels and attributes | `internal/enforce/*` | Queries referencing denied labels are 403; label listings filtered. Conservative token scan for LogQL. |
| Fan-out and merge | `internal/fanout` | All-or-nothing; byte caps; merges proven by property tests (e.g. top-L of the union). |
| Response filtering and redaction | `internal/fanout`, `internal/enforce/traceql`, `internal/enforce/redact` | Tempo search metadata redaction, trace-by-ID filtering, Prometheus/Loki series and label filtering, `labels.redact` value rewriting; undecodable bodies are `502`, never unfiltered. |

Evidence of end-to-end behaviour: `test/e2e/{prometheus,loki,tempo}` run real Prometheus, Loki and
Tempo containers (`make e2e`): isolation across namespaces and tenants, semantic equivalence of the
rewritten query with the union of per-term queries, and differential tests (a query the backend
accepts is either enforced or rejected, and every rewrite is accepted by the backend). These suites
found and led to fixes for real defects before release, listed under
[Known issues over time](#known-issues-over-time). They are **not** part of the CI workflows (see gaps).

### Security relevant

| Function | Notes |
|---|---|
| Resource limits | `maxPolicyTerms`, `maxFanout`, `maxMergeBytes`, `upstreamTimeout`, selector-copy caps, 1 MiB form bodies, parser nesting limit, `ReadHeaderTimeout`. No rate limiting ([threat model, section 8](threat-model.md#8-resource-exhaustion)). |
| Privacy defaults | Usernames and query text are not logged or traced unless `--tracing.record-user` / `--tracing.record-queries` are set. Tokens are never logged or forwarded. |
| Header hygiene | Client headers are allow-listed per backend (`forwardHeaders`, default `Accept`, `Accept-Encoding`, `User-Agent`, Loki's `X-Loki-Response-Encoding-Flags`); everything else is dropped, and credentials, tenant headers, `baggage`, `Upgrade` etc. cannot be allow-listed. Only W3C `traceparent` is propagated, fixed in code. A backend `101` on a non-WebSocket route fails closed. |
| Observability for security | Metrics by decision and reason, `X-Trace-Id` on every response, Grafana dashboard ([dashboards](../dashboards.md)). Management port is unauthenticated (keep it cluster-internal). |
| Least-privilege deployment | Chart: RBAC limited to `get/list/watch` on the CRDs and namespaces, `update/patch` on policy status, leases and events; distroless static non-root image (uid 65532), compatible with a read-only root filesystem; optional NetworkPolicies for the proxy and for backend protection (verified on kind, [TASKS.md](../../TASKS.md) T5.1). |
| Supply-chain controls | License gate (`go-licenses`), `govulncheck` in CI, actions pinned by commit SHA, base image pinned by digest, Renovate configuration (requires the app to be installed), DCO, CodeQL, OpenSSF Scorecard workflow. |
| Release integrity | cosign keyless signatures (image, checksums, chart), GitHub build-provenance attestations, SBOMs. **Configured, never executed.** |

### Not security relevant

Policy status conditions (informational; enforcement uses the snapshot), dashboard JSON, the
quickstart (demo-only credentials), documentation tooling, Grafana panels.

## Project compliance

* No compliance certification (SOC 2, ISO 27001, FIPS, PCI) or regulatory assessment is claimed.
  Querdian implements no cryptography of its own: JWT verification uses `go-oidc`, TLS to the issuer
  uses Go's `crypto/tls` with a TLS 1.2 minimum. It does not terminate TLS.
* Licensing: Apache-2.0; every dependency must be Apache-2.0 or on the CNCF allowlist, enforced in CI
  (`licenses` job). Source files carry no per-file license header (only generated code uses the
  boilerplate in `hack/boilerplate.go.txt`); the root `LICENSE` applies.
* Provenance: DCO sign-off is required and checked on pull requests. Clean-room rules for the
  LogQL and TraceQL parsers are in [CONTRIBUTING.md](../../CONTRIBUTING.md); the documentation used is
  recorded in `pkg/logql/doc.go`, `pkg/traceql/doc.go` and `docs/research/`. The rules are
  self-enforced; nothing but the maintainer's care verifies them.
* OpenSSF Best Practices: not registered; see [the checklist](../openssf-best-practices.md).

## Secure development practices

### Development pipeline

* **Contributions:** pull requests from branches (`t<id>-<slug>`), conventional commits, `git commit -s`.
  GOVERNANCE.md requires approval by a maintainer other than the author; with one maintainer this is
  not enforceable (gap). Branch protection / rulesets are a HUMAN task and are not verifiable from
  the repository.
* **CI** ([.github/workflows](../../.github/workflows)): `lint` (golangci-lint with gosec, errorlint,
  revive, misspell, unconvert), `test-race`, `envtest` (controller tests against a real API server),
  `fuzz` (30 s per target), `govulncheck`, `licenses`, `generate` (drift check), `dco`, `codeql (go)`,
  Scorecard. Workflows run with minimal `permissions:` and all actions are pinned by SHA.
* **Not in CI:** `make e2e` (differential and equivalence suites), `make chart-lint`,
  `make release-check`, coverage measurement, long-running or nightly fuzzing. They are run by hand.
* **Dependencies:** Renovate configuration (Go modules, actions, test images, GoReleaser); unverified
  until the Renovate app is installed.
* **Build and release:** reproducible-leaning flags (`-trimpath`, `CGO_ENABLED=0`); the image is built
  from the same binaries as the archives; distroless base pinned by digest; release workflow with
  cosign keyless, SBOM and attestations ([releasing.md](../releasing.md)). Never run.
* **Design practice:** a written plan with ground rules (fail closed, no silent over-grant, clean
  room) preceded the code; the threat model describes the implementation and lists residual risks;
  security-relevant findings from the E2E suites were fixed and recorded in the limitations.
* **Review:** a single maintainer wrote and reviewed the code. There was no independent security
  review (gap).

### Communication channels

* **Internal / project:** GitHub issues and pull requests are the only channel. There is no chat,
  mailing list or forum, no community meeting, no public roadmap other than [ROADMAP.md](../../ROADMAP.md).
* **Inbound:** issues for bugs; GitHub private vulnerability reporting for security problems
  ([SECURITY.md](../../SECURITY.md)), once enabled by the maintainer (HUMAN item H2).
* **Outbound:** GitHub releases and the changelog. No security mailing list or announcement
  channel exists. Code of Conduct reports go to the (single) maintainer.

### Ecosystem

* Runs in Kubernetes in front of Prometheus and Thanos (CNCF projects), Loki, Tempo and Mimir
  (Grafana Labs projects). Integrates with any OIDC provider (tested with Keycloak in the
  quickstart), Grafana's OAuth pass-through, OpenTelemetry for tracing, Prometheus for metrics,
  Helm for packaging.
* Depends on `prometheus/prometheus` (PromQL parser), `coreos/go-oidc`, controller-runtime and
  OpenTelemetry. `pkg/tempopb` is the only Tempo import and Loki's code is not used (clean room).
* No known adopters ([ADOPTERS.md](../../ADOPTERS.md) is empty). Not on the CNCF landscape.

## Security issue resolution

### Responsible disclosure process

[SECURITY.md](../../SECURITY.md): report privately through GitHub security advisories; the
maintainers aim to acknowledge within 3 working days and agree a disclosure timeline with the
reporter. What exists, honestly: a stated contact route and an acknowledgement target. What does not
exist: a defined embargo and coordinated-disclosure policy with durations, severity classification,
a security team or backup contact, a PGP key, a list of supported release lines (SECURITY.md says
only the latest `main` is supported; this must be updated at release), and CVE assignment practice
(GitHub advisories can request CVEs). Private vulnerability reporting must be switched on in the
repository settings for the link to work (H2). No report has ever been received.

### Incident response

There is no written incident response procedure. The intended course, derived from the tooling that
exists, is: reproduce in the E2E framework (`test/e2e/framework`), fix in a private advisory fork,
release a patch version via [releasing.md](../releasing.md), publish a GitHub security advisory and
amend the changelog. Because Querdian fails closed and has no persistent state, mitigation for
operators is usually to delete the affected policies or to scale the proxy down. This is a
description of intent, untested.

## Appendix

### Known issues over time

No vulnerability has been reported against a release (there is no release). The defects below were
found by the project's own differential and end-to-end testing during development, before the first
release; they are listed because they show the class of risk the proxy carries. Commit hashes refer
to this repository.

| Commit | Finding (fixed before release) |
|---|---|
| `4c9e482`, `2d5342c` | TraceQL: trace-level intrinsics (`rootName`, `traceDuration`, ...) and `compare()` let a user probe redacted root-span data (oracle); rejected unless the policy covers the root span. |
| `fd33e89` | LogQL: tenant binding of metric queries is expressed as a negative `__tenant_id__` matcher (commit message: "express tenant binding of metric queries as a negative matcher"); found by the Loki differential suite. |
| `f8db76f` | Loki series responses did not filter denied labels. |
| `a0a9bdd` | LogQL templates that can read all labels are rejected when labels are denied; legacy Tempo search parameters dropped (now `400`). |
| `1bfe673` | Fan-out merge incorrect for Loki branches answering with an empty vector (correctness, not disclosure). |

Residual and known risks that remain are enumerated in
[threat model, section 12](threat-model.md#12-summary-of-residual-risks) and the Loki findings in
[limitations](../limitations.md#logql) ("Findings from the end-to-end suite").

### OpenSSF Best Practices

Not registered yet. The criterion-by-criterion state and the exact HUMAN steps are in
[openssf-best-practices.md](../openssf-best-practices.md): 41 of 67 criteria Met on the repository
evidence, 15 Unmet (most depend on making the repository public, enabling private vulnerability
reporting and cutting a release), 11 N/A. Scorecard runs weekly once the repository is public; no
score is claimed.

### Case studies

None. There are no adopters. The only deployment evidence is the kind quickstart
([quickstart.md](../quickstart.md)): a demo with two teams and `verify.sh` (71 assertions), and the
chart verification script `hack/kind/verify-chart.sh`. Placeholder: add adopter case studies here when
they exist, with their permission.

### Related projects

An honest comparison. Descriptions of other projects are from public documentation as the maintainer
understands it and should be re-checked; they change.

| Project | What it does | Compared to Querdian |
|---|---|---|
| [prom-label-proxy](https://github.com/prometheus-community/prom-label-proxy) | Mature, widely used PromQL proxy that enforces a label matcher on Prometheus-compatible queries (used by Thanos, Observatorium and Prometheus-operator setups). Prometheus API only; the enforced label value comes from a request parameter or header chosen by a trusted gateway in front. | Smaller, simpler and battle-tested for exactly one label on PromQL. Querdian adds LogQL and TraceQL, OIDC authentication, policy CRDs, unions of several policies with or-lifting, denied labels, tenant handling and response redaction, at the cost of far more code and far less field time. If you only need one label on Prometheus and already authenticate elsewhere, prom-label-proxy is the lower-risk choice. |
| PromQL AST-rewriting proxies in general (the "prom-lens-style" approach: parse, inject matchers, print) | Same technique as Querdian's PromQL path. | Nothing unique there; the contribution is applying the same discipline to LogQL and TraceQL (clean-room parsers), merging fan-out results, and managing it with CRDs. |
| Grafana Enterprise / Grafana Cloud label-based access control (LBAC) for data sources | Commercial, closed source; enforces label selectors per team for supported data sources inside Grafana's data source proxy. | Not comparable on code maturity, support or integration with Grafana teams. Querdian is open source, independent of Grafana's edition, and enforces in front of the backends so it also protects non-Grafana clients (if the backends are not otherwise reachable). Scope and supported data sources of LBAC are Grafana's to state. |
| [kube-rbac-proxy](https://github.com/brancz/kube-rbac-proxy) | AuthN/AuthZ proxy in front of a metrics endpoint using Kubernetes TokenReview/SubjectAccessReview; decisions are per request path and verb. | Coarse: it decides whether a caller may reach an endpoint, it does not look at or rewrite the query. Complementary to Querdian (e.g. protecting Querdian's management port), not a substitute. |
| Loki/Tempo/Mimir native multi-tenancy (`X-Scope-OrgID`) | Isolation by tenant. | Querdian relies on it where tenants exist and adds label-level isolation inside a tenant. |
| Simple tenant-header proxies (for example basic-auth users mapped to a tenant) | Map credentials to a tenant header. | No label-level control, no query inspection. |
| [evoila/janus](https://github.com/evoila/janus) | Prior art for a multi-tenant proxy for Prometheus, Loki and Tempo, credited as inspiration. | **No code, tests, docs or configuration were taken or read for implementation; compatibility is an explicit non-goal.** |

## Gaps collected

Everything this document found missing, so a reviewer does not have to hunt for it.

Project and governance

* One maintainer. GOVERNANCE.md's "approved by a maintainer other than the author" cannot be met or
  technically enforced; a majority vote of one is not governance. Code of Conduct and security
  reports go to the same person.
* No adopters, no case studies, no contributors from other organizations, no public community
  channel or meeting.
* No issue or pull request templates, no `CODEOWNERS`; CONTRIBUTING.md has no bug-reporting guidance
  and does not state the test requirement (it lives in TASKS.md).
* Name and domain check (T0.0) is a HUMAN task and unticked: trademark search, `querdian.dev`
  registration. The GitHub org and repository settings (2FA, ruleset, Actions policy) cannot be
  verified from the repository.
* No per-file license headers in the source (Apache-2.0 does not require them).
* No project website; docs live in the repository.

Security process

* SECURITY.md: no embargo/disclosure durations, no supported-versions policy beyond `main`, no PGP key.
* No written incident response plan; no external audit, penetration test or threat-model review.
* No VEX or security advisories yet (none needed so far).

Engineering

* `make e2e`, `make chart-lint` and `make release-check` are not part of CI; PLAN section 13's nightly
  long fuzz and full E2E do not exist. Fuzz in CI is 30 seconds per target; no OSS-Fuzz; no committed
  fuzz corpora.
* No coverage measurement.
* The release pipeline (signing, SBOM, attestations, multi-arch image, OCI chart) has never run; T5.2's
  acceptance items are unticked. The chart is signed but not attested.
* Product limitations with security relevance: no rate limiting, unauthenticated management port, no
  TLS termination, tokens valid until expiry, namespace-label integrity is the operator's
  responsibility, clean-room parsers may diverge from the backends in unexplored corners,
  eventually consistent revocation ([threat model, section 12](threat-model.md#12-summary-of-residual-risks)).
* Tested only against the backend versions in the E2E suites; Thanos and Mimir federation are not
  tested end to end.
