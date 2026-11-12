# Querdian security self-assessment (DRAFT)

This page is the security self-assessment of Querdian. It follows the structure of the CNCF TAG Security
[self-assessment template](https://github.com/cncf/tag-security/blob/main/community/assessments/guide/self-assessment.md).

In short:

* Status: a draft by the maintainer. Nobody outside the project reviewed it. There was no external audit,
  no penetration test and no TAG Security review.
* The page describes **only what exists in this repository today**. It collects each missing item in
  [Gaps collected](#gaps-collected).
* The page does not repeat the attacker analysis. That is the [threat model](threat-model.md), and this
  page links to it where it applies.
* The project has one maintainer and no known adopters.

## Metadata

| | |
|---|---|
| Assessment stage | Draft; first release 0.1.0 (alpha) |
| Software | <https://github.com/querdian-dev/querdian> (Go module `github.com/querdian-dev/querdian`) |
| Security provider | Yes. Querdian is an access-control component. Its purpose: authenticated users must not read telemetry outside their policy. |
| Languages | Go (no cgo); Helm templates; shell scripts for the quickstart; YAML |
| Software bill of materials | The release pipeline ([.goreleaser.yaml](../../.goreleaser.yaml)) makes SPDX SBOMs for the release archives (assets of each [GitHub release](https://github.com/querdian-dev/querdian/releases)) and a BuildKit SBOM attestation on the image. The dependencies are in `go.mod` and `go.sum`. |
| Security links | [SECURITY.md](../../SECURITY.md), [threat model](threat-model.md), [limitations](../limitations.md), [OpenSSF checklist](../openssf-best-practices.md), [operations](../operations.md) |
| License | Apache-2.0 |
| Maintainers | One ([MAINTAINERS.md](../../MAINTAINERS.md)) |

## Overview

Querdian is a Kubernetes-native reverse proxy between users (usually Grafana with OAuth pass-through) and
Prometheus/Thanos, Loki and Tempo. Querdian:

1. authenticates a bearer token with OIDC;
2. resolves the groups of the user to policies, which are Kubernetes CRDs;
3. rewrites each query, so that the query can only match data that the policies allow;
4. forwards the query.

Querdian is default deny and fails closed.

### Background

Prometheus, Loki and Tempo have no authorisation on labels for each user. Multi-tenancy in Loki and
Tempo (and Mimir) uses only the tenant header, with no finer granularity. Thus teams that share a backend
run one stack for each team, or put a filtering layer in front of the backend. Querdian is such a layer.
[How Querdian works](../architecture.md) describes the architecture and the path of a request.

### Actors

| Actor | Role and trust |
|---|---|
| **Querdian proxy** (this project) | One Deployment, usually with two replicas. A single binary that contains the proxy, a management server and the policy controller. |
| **Identity provider** (OIDC issuer) | Issues the tokens. Trusted absolutely for the `username` and `groups` claims. |
| **Kubernetes API server** | The source of the policies (`ClusterAccessPolicy`, `AccessPolicy`) and of the namespace labels. Trusted. |
| **Backends** (Prometheus/Thanos, Loki, Tempo) | Trusted. Clients must not reach them except through Querdian (the chart supports NetworkPolicy). |
| **Users** | Authenticated, and possibly hostile within their authorisation. The main adversary. |
| **Platform admins and namespace admins** | Create policies. A namespace admin can write `AccessPolicy` objects only in her namespace. The implicit namespace matcher cannot be removed. |
| **Collection pipeline** (Prometheus scrape config, OTel Collector, Alloy/Promtail) | Must write the namespace label from trusted metadata. This is outside the control of Querdian, and it is the most important assumption ([threat model, section 1](threat-model.md#1-namespace-label-integrity-the-assumption-everything-rests-on)). |

Diagram and trust boundary: [threat model, section 2](threat-model.md#2-assets-actors-and-trust-boundary).

### Actions

1. A user (Grafana) sends `GET` or `POST` with `Authorization: Bearer <JWT>`.
2. Querdian does the route lookup (default deny) and the authentication (OIDC). It resolves the policies
   (the exact union of the policies that match the user or a group). It normalises the request
   (parameter allow-list, canonical paths).
3. The enforcer makes a plan. The plan injects matchers (single term), or-lifts the query (PromQL and
   LogQL metric queries), or fans out and merges (LogQL logs, metadata endpoints, tail).
4. The proxy executes the plan. It sets the tenant header only from the policy. It streams or merges the
   responses. For some endpoints, it filters or redacts the responses.
5. Each response carries `X-Trace-Id`. Querdian counts the decisions in metrics and records them on the
   trace.

### Goals

* **Isolation.** A user can read exactly the union of what the matching policies grant. This applies to
  the PromQL, LogQL and TraceQL read paths, and also to metadata endpoints.
* **Fail closed.** Errors in authentication, resolution, parsing, rewriting or merging give a rejection,
  never an unenforced pass-through. If Querdian cannot express the exact union, it rejects the request.
* Querdian rewrites queries with parsers, never with regular expressions or string surgery.
* Clients cannot choose a tenant. Querdian forwards only allow-listed client headers, and it sets the
  tenant header itself.
* Decisions are traceable. By default, Querdian does not log personal data.
* Supply-chain hygiene: a license allowlist, DCO, pinned CI actions and base image, and signed release
  artefacts.

### Non-goals

* Write and ingest paths, ruler, alerting and admin APIs. Querdian denies all of them.
* A UI, with one exception: the optional, read-only [policy dashboard](../dashboard.md).
* Operation outside Kubernetes.
* gRPC and streaming APIs.
* Protection of data against:
  * an authenticated user who has permission to see the data;
  * a compromised identity provider or Kubernetes API;
  * a workload that can forge the namespace label in the telemetry.
* Authentication of backends, TLS termination, rate limiting, token revocation and DDoS protection.
  These belong to the ingress, the mesh, the identity provider and the backends.
* Redaction of denied labels from query results by default. `labels.deny` hides and blocks; redaction is
  the opt-in `labels.redact` (see [limitations](../limitations.md#denied-labels)).
* Compatibility with evoila/janus. The project credits it as inspiration only. Nobody copied code, tests
  or docs from it.

## Self-assessment use

The maintainer wrote this self-assessment to help possible adopters and reviewers (TAG Security, CNCF
TOC, OpenSSF) to judge the project. It is not a security audit, not a certification and not a guarantee.
Read it together with the threat model and the limitations. In practice, the limitations are the most
important part.

## Security functions and features

### Critical

A failure of one of these components breaks the isolation. Unit tests cover each of them. Fuzzing and
differential tests against real backends cover most of them.

| Function | Where | What it guarantees / how it is tested |
|---|---|---|
| Authentication | `internal/auth` | OIDC verification (signature, issuer, audience, expiry) of each request, also on `safe` routes. Tests use a test issuer with generated keys, and a custom CA. |
| Default-deny routing, canonical paths, parameter allow-lists | `internal/routes`, `internal/proxy` | Querdian rejects an unknown path, method or parameter. Encodings cannot make an alias of a different route. Querdian rejects duplicate enforcement parameters. Table tests include traversal and encoded-slash cases. |
| Policy compilation and exact-union resolution | `internal/policy` | An invalid policy grants nothing. Users and groups are separate keyspaces. The DNF normalisation keeps the semantics (fuzzed property: the normalised DNF matches if and only if the input DNF matches). There is a limit on terms. |
| Tenant handling | `internal/policy`, `internal/proxy` | The tenant header comes only from the policy. `__tenant_id__` is bound only for federation headers. Querdian rejects cross-tenant unions that the backend does not support. |
| Query rewriting | `internal/enforce/*`, `pkg/logql`, `pkg/traceql` | Each selector or spanset filter carries the policy. PromQL uses the Prometheus parser. LogQL and TraceQL use clean-room parsers that are stricter than the backends. Property fuzzing (the repository has 22 fuzz targets), differential suites (see below). |
| Denied labels and attributes | `internal/enforce/*` | Queries that refer to denied labels get `403`. Querdian filters label listings. The token scan for LogQL is conservative. |
| Fan-out and merge | `internal/fanout` | All or nothing. Byte limits. Property tests prove the merges (for example top-L of the union). |
| Response filtering and redaction | `internal/fanout`, `internal/enforce/traceql`, `internal/enforce/redact` | Redaction of Tempo search metadata, trace-by-ID filtering, Prometheus/Loki series and label filtering, and `labels.redact` value rewriting. A body that Querdian cannot decode gets `502`, never an unfiltered answer. |

**Evidence of the end-to-end behaviour.** `test/e2e/{prometheus,loki,tempo}` run real Prometheus, Loki
and Tempo containers (`make e2e`). They test:

* the isolation across namespaces and tenants;
* the semantic equivalence of the rewritten query with the union of the queries for each term;
* differential behaviour: if the backend accepts a query, Querdian enforces or rejects it, and the
  backend accepts each rewrite.

Before the first release, these suites found real defects, and the defects were fixed. See
[Known issues over time](#known-issues-over-time). The `e2e` job runs on each pull request, but it is not
a required check (see [Development pipeline](#development-pipeline)).

### Security relevant

| Function | Notes |
|---|---|
| Resource limits | `maxPolicyTerms`, `maxFanout`, `maxMergeBytes`, `upstreamTimeout`, limits on selector copies, 1 MiB form bodies, parser nesting limit, `ReadHeaderTimeout`. No rate limiting ([threat model, section 8](threat-model.md#8-resource-exhaustion)). |
| Privacy defaults | Querdian does not log or trace user names and query text, unless `--tracing.record-user` or `--tracing.record-queries` is set. Querdian never logs or forwards tokens. |
| Header hygiene | Querdian allow-lists client headers for each backend (`forwardHeaders`; default `Accept`, `Accept-Encoding`, `User-Agent`, and `X-Loki-Response-Encoding-Flags` for Loki). It drops all other headers. Credentials, tenant headers, `baggage`, `Upgrade` and similar headers cannot be allow-listed. Querdian propagates only W3C `traceparent`, fixed in code. A backend `101` on a route that is not a WebSocket route fails closed. |
| Observability for security | Metrics by decision and reason, `X-Trace-Id` on each response, a Grafana dashboard ([dashboards](../dashboards.md)). The management port has no authentication (keep it in the cluster). |
| Least-privilege deployment | The chart limits RBAC to `get/list/watch` on the CRDs and namespaces, and `update/patch` on policy status, leases and events. The image is distroless, static and non-root (uid 65532), and it works with a read-only root filesystem. Optional NetworkPolicies protect the proxy and the backends (verified on kind). |
| Supply-chain controls | License gate (`go-licenses`), `govulncheck` in CI, actions pinned by commit SHA, base image pinned by digest, Renovate configuration (the Renovate app must be installed), DCO, CodeQL, OpenSSF Scorecard workflow. |
| Release integrity | cosign keyless signatures (image, chart, and the checksums file as a Sigstore bundle). GitHub build-provenance attestations (SLSA v1) for the archives and the image. SBOMs. The release candidates `v0.1.0-rc.1` and `v0.1.0-rc.2` used this pipeline. Their verification followed [releasing.md](../releasing.md#5-post-release-verification). The chart is signed but has no provenance attestation. |

### Not security relevant

Policy status conditions (information only; the enforcement uses the snapshot), dashboard JSON, the
quickstart (demo-only credentials), documentation tooling, Grafana panels.

## Project compliance

* **Certification.** The project claims no compliance certification (SOC 2, ISO 27001, FIPS, PCI) and no
  regulatory assessment.
* **Cryptography.** Querdian has no cryptography of its own. JWT verification uses `go-oidc`. TLS to the
  issuer uses Go's `crypto/tls` with a TLS 1.2 minimum. Querdian does not terminate TLS.
* **Licensing.** Apache-2.0. Each dependency must be Apache-2.0 or on the CNCF allowlist. CI enforces
  this (`licenses` job). Source files have no license header for each file; the root `LICENSE` applies.
  Only generated code uses the boilerplate in `hack/boilerplate.go.txt`.
* **Provenance.** A DCO sign-off is necessary, and a check on pull requests enforces it. The clean-room
  rules for the LogQL and TraceQL parsers are in [CONTRIBUTING.md](../../CONTRIBUTING.md).
  `pkg/logql/doc.go`, `pkg/traceql/doc.go` and `docs/research/` record the documentation that was used.
  The rules are self-enforced: only the care of the maintainer verifies them.
* **OpenSSF Best Practices.** Not registered. See [the checklist](../openssf-best-practices.md).

## Secure development practices

### Development pipeline

* **Contributions.** Pull requests from branches (`t<id>-<slug>`), conventional commits,
  `git commit -s`.
  * GOVERNANCE.md requires the approval of a maintainer other than the author. With one maintainer, this
    is not enforceable (gap).
  * The maintainer sets branch protection and rulesets in the GitHub settings. The repository cannot
    show them.
* **CI** ([.github/workflows](../../.github/workflows)):

  | Job | What it does |
  |---|---|
  | `lint` | golangci-lint with gosec, errorlint, revive, misspell, unconvert |
  | `test-race` | unit tests with the race detector |
  | `envtest` | controller tests against a real API server |
  | `fuzz` | 30 s for each fuzz target |
  | `govulncheck` | check for known vulnerabilities |
  | `licenses` | license allowlist |
  | `generate` | drift check of generated code |
  | `chart` | CRD sync check, `make chart-lint`, chart tests |
  | `e2e` | `make e2e`: the differential and equivalence suites |
  | `dco`, `codeql (go)`, Scorecard | sign-off, static analysis, OpenSSF Scorecard |

  `chart` and `e2e` run on each pull request, but they are not required checks yet. Workflows run with
  minimal `permissions:`, and all actions are pinned by SHA.
* **Not in CI:** `make release-check`, coverage measurement, long or nightly fuzzing. The maintainer runs
  them by hand.
* **Dependencies.** A Renovate configuration (Go modules, actions, test images, GoReleaser). It is not
  verified until the Renovate app is installed.
* **Build and release.** Flags that help reproducible builds (`-trimpath`, `CGO_ENABLED=0`). The image contains
  the same binaries as the archives. The distroless base is pinned by digest. The release workflow uses
  cosign keyless, SBOM and attestations ([releasing.md](../releasing.md)). The first release that used it
  was 0.1.0.
* **Design practice.** A written plan with ground rules (fail closed, no silent over-grant, clean room)
  came before the code. The threat model describes the implementation and lists the residual risks.
  Security-relevant findings from the E2E suites were fixed and recorded in the limitations.
* **Review.** A single maintainer wrote and reviewed the code. There was no independent security review
  (gap).

### Communication channels

* **Internal / project.** GitHub issues and pull requests are the only channel. There is no chat, no
  mailing list, no forum and no community meeting. There is no public roadmap other than
  [ROADMAP.md](../../ROADMAP.md).
* **Inbound.** Issues for bugs. GitHub private vulnerability reporting for security problems
  ([SECURITY.md](../../SECURITY.md)). See [Responsible disclosure process](#responsible-disclosure-process).
* **Outbound.** GitHub releases and the changelog. There is no security mailing list and no
  announcement channel. Code of Conduct reports go to the single maintainer.

### Ecosystem

* Querdian runs in Kubernetes in front of Prometheus and Thanos (CNCF projects), Loki, Tempo and Mimir
  (Grafana Labs projects).
* It integrates with each OIDC provider (tested with Keycloak in the quickstart), the OAuth pass-through
  of Grafana, OpenTelemetry for tracing, Prometheus for metrics, and Helm for packaging.
* It depends on `prometheus/prometheus` (PromQL parser), `coreos/go-oidc`, controller-runtime and
  OpenTelemetry.
* It imports no Tempo module and does not use the code of Loki (clean room). `internal/tempopb` is a copy
  of the generated protobuf types of Tempo's `pkg/tempopb` (Apache-2.0, see `internal/tempopb/SOURCE`).
* There are no known adopters ([ADOPTERS.md](../../ADOPTERS.md) is empty). Querdian is not on the CNCF
  landscape.

## Security issue resolution

### Responsible disclosure process

[SECURITY.md](../../SECURITY.md): report privately through GitHub security advisories. The maintainers
aim to acknowledge a report in 3 working days, and to agree a disclosure timeline with the reporter.
Security fixes are released for the latest minor release line only (currently 0.1.x).

| Exists | Does not exist |
|---|---|
| A stated contact route | A defined embargo and coordinated-disclosure policy with durations |
| A target time for the acknowledgement | A severity classification |
| A policy for supported versions | A security team or a backup contact |
| | A PGP key |
| | A practice for CVE assignment (GitHub advisories can request CVEs) |

Private vulnerability reporting must be enabled in the repository settings, or the link does not work.
No report has ever been received.

### Incident response

There is no written incident response procedure. The intended procedure comes from the tooling that
exists. It is a description of intent, and nobody has tested it:

1. Reproduce the problem in the E2E framework (`test/e2e/framework`).
2. Fix it in a private advisory fork.
3. Release a patch version as in [releasing.md](../releasing.md).
4. Publish a GitHub security advisory and change the changelog.

Querdian fails closed and has no persistent state. Thus, to decrease the risk, operators can usually
delete the affected policies or scale the proxy down.

## Appendix

### Known issues over time

The releases 0.1.0, 0.1.1 and 0.1.2 exist. The "Security" sections of
[CHANGELOG.md](../../CHANGELOG.md) list the security fixes after the first release, for example
GHSA-jcqw-3c3f-xrmj (Tempo tag lookups, fixed in 0.1.1).

The project found the defects below with its own differential and end-to-end tests, before the first
release. They show the class of risk of the proxy. The commit hashes refer to this repository.

| Commit | Finding (fixed before release) |
|---|---|
| `4c9e482`, `2d5342c` | TraceQL: trace-level intrinsics (`rootName`, `traceDuration`, ...) and `compare()` let a user probe redacted root-span data (oracle). Querdian now rejects them unless the policy covers the root span. |
| `fd33e89` | LogQL: the tenant binding of metric queries is expressed as a negative `__tenant_id__` matcher (commit message: "express tenant binding of metric queries as a negative matcher"). The Loki differential suite found it. |
| `f8db76f` | Loki series responses did not filter denied labels. |
| `a0a9bdd` | Querdian rejects LogQL templates that can read all labels when labels are denied. Legacy Tempo search parameters are dropped (now `400`). |
| `1bfe673` | The fan-out merge was not correct for Loki branches that answer with an empty vector (a correctness defect, not a disclosure). |

The residual and known risks are in [threat model, section 13](threat-model.md#13-summary-of-residual-risks).
The Loki findings are in [limitations](../limitations.md#logql) ("Findings from the end-to-end suite").

### OpenSSF Best Practices

Not registered yet. [openssf-best-practices.md](../openssf-best-practices.md) gives the state of each
criterion and the exact steps that only the maintainer can do:

* 41 of 67 criteria are Met on the evidence in the repository.
* 15 are Unmet. Most of them depend on a public repository, private vulnerability reporting and a
  release.
* 11 are N/A.

Scorecard runs weekly when the repository is public. The project claims no score.

### Case studies

None. There are no adopters. The only evidence of a deployment:

* the kind quickstart ([quickstart.md](../quickstart.md)): a demo with two teams, and `verify.sh` with
  153 checks;
* the chart verification script `hack/kind/verify-chart.sh`.

Placeholder: add case studies of adopters here when they exist, with their permission.

### Related projects

This is an honest comparison. The descriptions of other projects come from their public documentation,
as the maintainer understands it. They change, so examine them again.

| Project | What it does | Compared to Querdian |
|---|---|---|
| [prom-label-proxy](https://github.com/prometheus-community/prom-label-proxy) | A mature and widely used PromQL proxy that enforces a label matcher on Prometheus-compatible queries (used by Thanos, Observatorium and Prometheus-operator setups). Prometheus API only. The enforced label value comes from a request parameter or header that a trusted gateway in front chooses. | Smaller, simpler and battle-tested for exactly one label on PromQL. Querdian adds LogQL and TraceQL, OIDC authentication, policy CRDs, unions of more than one policy with or-lifting, denied labels, tenant handling and response redaction. The cost is much more code and much less field time. If you only need one label on Prometheus and you already authenticate in a different place, prom-label-proxy is the choice with lower risk. |
| PromQL AST-rewriting proxies in general (the "prom-lens-style" approach: parse, inject matchers, print) | The same technique as the PromQL path of Querdian. | Nothing unique there. The contribution is the same discipline for LogQL and TraceQL (clean-room parsers), the merge of fan-out results, and the management with CRDs. |
| Grafana Enterprise / Grafana Cloud label-based access control (LBAC) for data sources | Commercial and closed source. Enforces label selectors for each team for supported data sources, in the data source proxy of Grafana. | Not comparable on code maturity, support or integration with Grafana teams. Querdian is open source and does not depend on the edition of Grafana. It enforces in front of the backends, thus it also protects clients other than Grafana (if the backends are not reachable in a different way). Grafana states the scope and the supported data sources of LBAC. |
| [kube-rbac-proxy](https://github.com/brancz/kube-rbac-proxy) | An AuthN/AuthZ proxy in front of a metrics endpoint. It uses Kubernetes TokenReview/SubjectAccessReview. It decides for each request path and verb. | Coarse: it decides if a caller can reach an endpoint. It does not examine or rewrite the query. It is a complement to Querdian (for example, to protect the management port of Querdian), not a substitute. |
| Loki/Tempo/Mimir native multi-tenancy (`X-Scope-OrgID`) | Isolation by tenant. | Querdian uses it where tenants exist, and adds isolation by label in a tenant. |
| Simple tenant-header proxies (for example basic-auth users mapped to a tenant) | Map credentials to a tenant header. | No control on labels, no examination of queries. |
| [evoila/janus](https://github.com/evoila/janus) | Prior art for a multi-tenant proxy for Prometheus, Loki and Tempo. The project credits it as inspiration. | **Nobody took or read code, tests, docs or configuration from it for the implementation. Compatibility is an explicit non-goal.** |

## Gaps collected

This section collects each missing item of this page, so that a reviewer does not have to look for it.

**Project and governance**

* One maintainer. The rule of GOVERNANCE.md "approved by a maintainer other than the author" cannot be
  met or technically enforced. A majority vote of one is not governance. Code of Conduct reports and
  security reports go to the same person.
* No adopters, no case studies, no contributors from other organizations, no public community channel
  or meeting.
* The name conflict check (for example a trademark search) is not recorded as done. The settings of the
  GitHub organization and repository (2FA, ruleset, Actions policy) cannot be verified from the
  repository.
* No license headers for each file in the source (Apache-2.0 does not require them).

**Security process**

* SECURITY.md: no embargo or disclosure durations, no PGP key.
* No written incident response plan. No external audit, no penetration test, no review of the threat
  model.
* No VEX documents.

**Engineering**

* `make release-check` is not part of CI. The `e2e` and `chart` jobs run on each pull request, but they
  are not required checks.
* There is no nightly long fuzz run and no nightly full E2E run. Fuzzing in CI is 30 seconds for each
  target. No OSS-Fuzz. No committed fuzz corpora.
* No coverage measurement.
* The release pipeline (signing, SBOM, attestations, multi-arch image, OCI chart) has made three
  releases: 0.1.0 and 0.1.1, each after two release candidates, and 0.1.2. The chart is signed but not
  attested.
* Product limitations with security relevance ([threat model, section 13](threat-model.md#13-summary-of-residual-risks)):
  * no rate limiting;
  * the management port has no authentication;
  * no TLS termination;
  * tokens are valid until they expire;
  * the integrity of the namespace label is the responsibility of the operator;
  * the clean-room parsers can diverge from the backends in corners that nobody examined;
  * the revocation is eventually consistent.
* Querdian is tested only against the backend versions in the E2E suites. Thanos and Mimir federation
  are not tested end to end.
