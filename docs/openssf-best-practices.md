# OpenSSF Best Practices: "passing" level checklist

This is the maintainer's working checklist for the OpenSSF Best Practices badge at the **passing**
level (<https://www.bestpractices.dev>). It records, criterion by criterion, what the repository
proves today and what is missing. It is a preparation aid, not the application: the badge is only
earned by registering the project and answering the questionnaire (HUMAN item H5).

How to read it:

* **Met** means there is evidence in this repository (file or workflow named in the table).
* **Unmet** means there is no evidence yet, or the criterion depends on a GitHub setting or an
  event (a release) that cannot be shown from the files. The last column says what is missing and
  which HUMAN item (H1 ...) closes it.
* **N/A** means the criterion does not apply, with the reason.
* A criterion is marked Met only when the file is there. "Workflow exists" is not "workflow is
  green on `main`"; the maintainer must confirm the latter (H4) before answering Met in the form.
* The criterion identifiers and their MUST/SHOULD/SUGGESTED strength are quoted from memory of the
  current form. **Verify every row against the live form at bestpractices.dev** when filling it in;
  criteria are occasionally reworded or added.
* Level of MUST (M), SHOULD (S) and SUGGESTED (U) is given in the second column.

Last reviewed against the repository at the state of branch `t5.6-readiness` (no release, no tags).

## Summary

| Section | Met | Unmet | N/A |
|---|---|---|---|
| Basics (incl. repository and versioning) | 14 | 6 | 0 |
| Change control and reporting | 3 | 6 | 1 |
| Quality | 11 | 2 | 0 |
| Security | 6 | 1 | 9 |
| Analysis | 7 | 0 | 1 |
| **Total** | **41** | **15** | **11** |

Counted in the tables below. The Unmet MUST criteria that block the badge today are:

* GitHub-dependent (H1, H2): `repo_public`, `sites_https`, `discussion`, `report_archive`,
  `vulnerability_report_private`;
* release-dependent (T5.7): `version_unique`, `release_notes`, `delivery_mitm`;
* history-dependent: `report_responses` (no reports have been filed yet; answer honestly).

The remaining Unmet rows are SHOULD or SUGGESTED.

## Basics

| ID | Level | Status | Evidence / what is missing |
|---|---|---|---|
| `description_good` | M | Met | First paragraph of [README.md](../README.md) states what Querdian does and for whom. |
| `interact` | M | Met | README "Contributing and reporting" section links the issue tracker, [CONTRIBUTING.md](../CONTRIBUTING.md) and [SECURITY.md](../SECURITY.md). The issue tracker itself must be enabled (H1). |
| `contribution` | M | Met | [CONTRIBUTING.md](../CONTRIBUTING.md): DCO, clean-room rule, workflow, test commands. |
| `contribution_requirements` | S | Met | CONTRIBUTING.md (DCO, conventional commits, license allowlist, `make lint test`); style is enforced by [.golangci.yml](../.golangci.yml) and [.editorconfig](../.editorconfig). The requirement for tests lives in the definition of done in [TASKS.md](../TASKS.md); CONTRIBUTING.md does not repeat it (see gaps in the self-assessment). |
| `floss_license` | M | Met | [LICENSE](../LICENSE) is Apache-2.0. |
| `floss_license_osi` | U | Met | Apache-2.0 is OSI approved. |
| `license_location` | M | Met | `LICENSE` in the repository root. |
| `documentation_basics` | M | Met | README plus [docs/](.): [quickstart](quickstart.md), [policy guide](policy-guide.md), [operations](operations.md), [troubleshooting](troubleshooting.md), [limitations](limitations.md). |
| `documentation_interface` | M | Met | External interfaces are the HTTP API (route inventory in [endpoints.md](endpoints.md), error format in [troubleshooting.md](troubleshooting.md)), the CRDs ([policy-guide.md](policy-guide.md)), the config file and flags ([operations.md](operations.md)) and the metrics. |
| `sites_https` | M | Unmet | The project site is the GitHub repository (`https://github.com/querdian-dev/querdian`), which is HTTPS, but the repository's existence and visibility cannot be shown from the files. Closed by H1. |
| `discussion` | M | Unmet | Issues and pull requests on GitHub satisfy this (searchable, URL-addressable, no client-side proprietary software), once the repository is public with issues enabled (H1). There is no separate forum, chat or mailing list; that is fine for the criterion. |
| `english` | M | Met | All documentation and code comments are English; bug reports are accepted in English. |
| `maintained` | M | Met | 107 commits by one maintainer, tasks tracked in [TASKS.md](../TASKS.md). Honest caveat: there is **one** maintainer ([MAINTAINERS.md](../MAINTAINERS.md)). |
| `repo_public` | M | Unmet | `origin` is `git@github.com:querdian-dev/querdian.git` but public visibility must be set and confirmed (H1). |
| `repo_track` | M | Met | git history with author, date and message per change. |
| `repo_interim` | M | Met | git history contains the intermediate work (task branches and merges), not only releases. |
| `repo_distributed` | U | Met | git. |
| `version_unique` | M | Unmet | No release, no tag yet. [release.yml](../.github/workflows/release.yml) only accepts tags matching `vMAJOR.MINOR.PATCH[-pre]`. Closed by T5.7 (the first tag). |
| `version_semver` | U | Unmet | Same: SemVer is the stated scheme (PLAN section 14) and enforced by the release workflow, but nothing has been released yet. |
| `version_tags` | U | Unmet | `git tag` is empty. Closed by T5.7. |

## Change control and reporting

| ID | Level | Status | Evidence / what is missing |
|---|---|---|---|
| `release_notes` | M | Unmet | Nothing released. Prepared: [CHANGELOG.md](../CHANGELOG.md) and [release-notes-0.1.0.md](release-notes-0.1.0.md) (drafts); GoReleaser also generates a changelog from commit messages ([.goreleaser.yaml](../.goreleaser.yaml)). Closed by T5.7. |
| `release_notes_vulns` | M | N/A | The release notes must identify fixed publicly known vulnerabilities. There are none to report; state "no known vulnerabilities fixed" in the notes and keep this up to date. |
| `report_process` | M | Met | README "Contributing and reporting" and [CONTRIBUTING.md](../CONTRIBUTING.md) point to the GitHub issue tracker; [SECURITY.md](../SECURITY.md) covers vulnerabilities. There are no issue templates (`.github/ISSUE_TEMPLATE` does not exist); optional. |
| `report_tracker` | S | Unmet | GitHub issues, once the repository is public with issues enabled (H1). |
| `report_responses` | M | Unmet | No issue has ever been filed; there is no response history to show. The form asks for a majority acknowledged within 2-12 months; answer honestly once there is a history. |
| `enhancement_responses` | S | Unmet | Same; no history. |
| `report_archive` | M | Unmet | GitHub issues are archived and searchable once public (H1). |
| `vulnerability_report_process` | M | Met | [SECURITY.md](../SECURITY.md) publishes the process and the advisory URL. |
| `vulnerability_report_private` | M | Unmet | SECURITY.md points to `https://github.com/querdian-dev/querdian/security/advisories/new`, which only works when **private vulnerability reporting is enabled** in the repository settings (H2). Nothing in the repo can prove it. |
| `vulnerability_report_response` | M | Met | SECURITY.md commits to acknowledging reports within 3 working days (criterion: initial response within 14 days). No report has been received, so the track record is empty. |

## Quality

| ID | Level | Status | Evidence / what is missing |
|---|---|---|---|
| `build` | M | Met | `make build` ([Makefile](../Makefile)), `go build`; reproducible flags in [.goreleaser.yaml](../.goreleaser.yaml) (`-trimpath`, `CGO_ENABLED=0`). |
| `build_common_tools` | S | Met | Go toolchain and make. |
| `build_floss_tools` | S | Met | Go, make, GoReleaser, Docker/BuildKit, Helm: all FLOSS. |
| `test` | M | Met | `make test`, `make test-race`, `make envtest`, `make fuzz`, `make e2e` (143 Go files, 75 of them tests; 10 fuzz targets; black-box differential suites against Prometheus, Loki and Tempo under [test/e2e/](../test/e2e)). |
| `test_invocation` | S | Met | Standard `go test ./...` via `make test`; documented in CONTRIBUTING.md. |
| `test_most` | U | Unmet | Statement coverage is not measured anywhere (no `-cover` in Makefile or workflows). The code is heavily tested, but there is no number. |
| `test_continuous_integration` | U | Met | [ci.yml](../.github/workflows/ci.yml) runs lint, test-race, envtest, fuzz (30 s per target), govulncheck, licenses and generate on every PR. Caveat: **`make e2e`, `make chart-lint` and `make release-check` are not run in CI** (documented in the self-assessment gaps). |
| `test_policy` | M | Met | The definition of done in [TASKS.md](../TASKS.md) ("New code has unit tests") is the project's written policy; GOVERNANCE.md requires review by a second maintainer. Recommendation: repeat the sentence in CONTRIBUTING.md so the form can point at a contributor-facing document. |
| `tests_are_added` | M | Met | The history shows tests added with the functionality (every `internal/` package has tests; E2E suites accompany every enforcer). |
| `tests_documented_added` | U | Met | TASKS.md definition of done (see `test_policy`). |
| `warnings` | M | Met | `make lint` runs golangci-lint with `standard`, `errorlint`, `gosec`, `misspell`, `revive`, `unconvert` ([.golangci.yml](../.golangci.yml)). |
| `warnings_fixed` | M | Met | `lint` is a CI job and the definition of done requires `make lint` green. Confirm the job is green on `main` (H4). |
| `warnings_strict` | U | Unmet | The linter set is moderate, not maximally strict. |

## Security

| ID | Level | Status | Evidence / what is missing |
|---|---|---|---|
| `know_secure_design` | M | Met | Self-attestation by the maintainer (H9). Evidence of practice: fail-closed and default-deny principles in PLAN section 2, [threat model](security/threat-model.md). |
| `know_common_errors` | M | Met | Self-attestation (H9). Evidence: the threat model covers injection and parser divergence, parameter pollution, header trust, oracle attacks, resource exhaustion, token replay. |
| `crypto_published` | M | N/A | Querdian does not implement cryptography. JWT/JWKS verification uses `github.com/coreos/go-oidc/v3`; TLS to the issuer uses Go's `crypto/tls` (minimum TLS 1.2, `internal/auth/auth.go`). |
| `crypto_call` | S | N/A | Same: no own implementation; libraries are called, nothing is re-implemented. |
| `crypto_floss` | M | N/A | Same; all crypto dependencies are FLOSS. |
| `crypto_keylength` | M | N/A | Querdian generates no keys; key sizes are the issuer's choice. |
| `crypto_working` | M | N/A | No broken algorithms are implemented or selected by the project. |
| `crypto_weaknesses` | S | N/A | No project-chosen algorithms. |
| `crypto_pfs` | S | N/A | Querdian does not terminate TLS (documented in [operations.md](operations.md)). |
| `crypto_password_storage` | M | N/A | No passwords are stored or verified. |
| `crypto_random` | M | N/A | No cryptographic keys or nonces are generated outside tests. |
| `delivery_mitm` | M | Unmet | Nothing is delivered yet. The pipeline is built for it: HTTPS GitHub releases and ghcr.io, cosign keyless signatures, GitHub build-provenance attestations ([release.yml](../.github/workflows/release.yml)). Closed by T5.7 and verification per [releasing.md](releasing.md). |
| `delivery_unsigned` | S | Met | No hash or key is fetched over plain HTTP in the Makefile, workflows, Dockerfile (images pinned by digest) or quickstart. |
| `vulnerabilities_fixed_60_days` | M | Met | No known unpatched vulnerability; `govulncheck` is a CI job and Renovate is configured ([renovate.json](../renovate.json)). Renovate only runs if the app is installed (H11). Confirm no open alert (H4). |
| `vulnerabilities_critical_fixed` | M | Met | Same evidence. |
| `no_leaked_credentials` | M | Met | A search of the tree for private key blocks finds none. The quickstart contains **deliberately public demo passwords** (`alice-demo`, `admin/admin`, the Grafana client secret in [hack/quickstart](../hack/quickstart)); they protect a throw-away kind cluster and are labelled DEMO ONLY. Enable GitHub secret scanning and push protection (H4). |

## Analysis

| ID | Level | Status | Evidence / what is missing |
|---|---|---|---|
| `static_analysis` | M | Met | CodeQL for Go ([codeql.yml](../.github/workflows/codeql.yml)) on every PR, on `main` and weekly; golangci-lint with gosec in `lint`. |
| `static_analysis_common_vulnerabilities` | U | Met | CodeQL security queries and gosec. |
| `static_analysis_fixed` | M | Met | CI must pass; confirm the Code scanning tab has no open high/critical alerts before answering (H4). |
| `static_analysis_often` | U | Met | Every PR and weekly. |
| `dynamic_analysis` | U | Met | Fuzzing of lexers, parsers, DNF normalisation and rewriters (10 `Fuzz*` targets, 30 s each in CI), the race detector (`make test-race`), and differential/equivalence suites against real backends. |
| `dynamic_analysis_unsafe` | S | N/A | Go is memory safe; there is no cgo and no `unsafe` use in the product code. |
| `dynamic_analysis_enable_assertions` | U | Met | Go runtime checks are always on; tests assert invariants under fuzz and race builds. |
| `dynamic_analysis_fixed` | M | Met | No known unfixed medium+ finding from dynamic analysis. Note that crashing fuzz inputs are not kept as committed corpora (no `testdata/fuzz` directories exist), so a regression would rely on the fix's own unit test. |

Interpretation: after H1, H2, H5 and the first release, the project should be able to answer
"passing". The SHOULD/SUGGESTED rows that stay Unmet (`test_most`, `warnings_strict`, response
history) do not block the badge.

## HUMAN actions

These cannot be done by a commit. Do them in this order.

| # | Action | Closes |
|---|---|---|
| H1 | Make `querdian-dev/querdian` public; enable Issues; set description, website and topics. | `repo_public`, `sites_https`, `discussion`, `report_tracker`, `report_archive`, Scorecard publishing |
| H2 | Settings > Code security: enable **Private vulnerability reporting**. | `vulnerability_report_private` |
| H3 | Settings > Rules: create the `main` ruleset: require pull request, require the status checks listed in [releasing.md](releasing.md#1-one-time-github-settings), block force pushes and deletions, require linear history if desired. With one maintainer, do **not** require an approving review (it would block merging); the review rule in GOVERNANCE.md cannot be technically enforced until there is a second maintainer. | Scorecard `Branch-Protection`, GOVERNANCE.md |
| H4 | Settings > Code security: enable Dependabot alerts (security updates only, Renovate does version updates), secret scanning and push protection, code scanning. Then confirm: all workflows green on `main`, no open CodeQL or Dependabot alerts, no open `govulncheck` finding. | `static_analysis_fixed`, `warnings_fixed`, `vulnerabilities_*`, `no_leaked_credentials` |
| H5 | Register the project at <https://www.bestpractices.dev>, answer the questionnaire against this checklist, keep the answers truthful (Unmet stays Unmet), and note the numeric project ID. Then activate the commented badges in the README. | the badge |
| H6 | Org and repository Actions policy (T0.0): only GitHub-owned and verified-creator actions, plus the allowlist entries listed in [releasing.md](releasing.md#1-one-time-github-settings). Org: 2FA required. | supply-chain posture, T0.0 |
| H7 | Cut `v0.0.0-rc.1`, then `v0.1.0` ([releasing.md](releasing.md)); set the ghcr.io packages to public. | `version_*`, `release_notes`, `delivery_mitm` |
| H8 | Confirm the Scorecard workflow publishes results (needs the public repo) and read the first report at <https://scorecard.dev/viewer/?uri=github.com/querdian-dev/querdian>. | Scorecard badge |
| H9 | Maintainer self-attestation for `know_secure_design` and `know_common_errors`. | those two criteria |
| H10 | Recruit a second maintainer (GOVERNANCE.md assumes reviews by a maintainer other than the author). Not required for the badge. | bus factor, Scorecard `Code-Review` |
| H11 | Install the Renovate GitHub app (or run Renovate self-hosted) for the organization; `renovate.json` alone does nothing. | dependency updates |

## Badges

The README carries **commented-out** badge markup. Uncomment it only after H5 (the numeric ID) and
after H8 (the Scorecard publishes). Patterns:

```markdown
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/<ID>/badge)](https://www.bestpractices.dev/projects/<ID>)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/querdian-dev/querdian/badge)](https://scorecard.dev/viewer/?uri=github.com/querdian-dev/querdian)
```
