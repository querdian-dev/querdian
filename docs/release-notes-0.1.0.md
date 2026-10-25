# Querdian v0.1.0 release notes

Querdian is an Apache-2.0, Kubernetes-native reverse proxy that enforces label-based access control
on read queries against Prometheus/Thanos (PromQL), Loki (LogQL) and Tempo (TraceQL). Policies are
Kubernetes CRDs, queries are rewritten with parsers, and every error fails closed.

This is the first release. It is an alpha release with a `v1alpha1` API: the CRDs may change
incompatibly, and the project has no production adopters yet. Treat it as something to evaluate and
give feedback on, not as a drop-in security boundary you have not tested in your own environment.

## Highlights

* `ClusterAccessPolicy` and `AccessPolicy` CRDs (`policy.querdian.dev/v1alpha1`) with status
  conditions; a user's access is the exact union of every policy that matches their username or
  groups, or the request is rejected with a reason.
* OIDC bearer-token authentication; default deny for unknown endpoints, methods, parameters and users.
* PromQL, LogQL and TraceQL enforcement, including multi-tenant (`X-Scope-OrgID`) backends and
  Loki `tail`. The LogQL and TraceQL parsers are clean-room implementations from the public
  documentation (`pkg/logql`, `pkg/traceql`).
* `labels.deny` hides labels; the opt-in `labels.redact` also rewrites their values in query results.
* Tempo search responses are redacted and trace-by-ID can be filtered by policy.
* An optional, read-only policy dashboard with a query test bench (off by default).
* Helm chart (with optional NetworkPolicies protecting the backends), distroless multi-arch image,
  Grafana dashboard, metrics, tracing and a kind quickstart.
* Release artefacts are signed with cosign (keyless) and carry SBOMs and GitHub build-provenance
  attestations; see "Verify" below.

Full list: [CHANGELOG.md](../CHANGELOG.md).

## Artefacts

| | |
|---|---|
| Container image | `ghcr.io/querdian-dev/querdian:0.1.0` (`linux/amd64`, `linux/arm64`; distroless, non-root) |
| Helm chart | `oci://ghcr.io/querdian-dev/charts/querdian`, version `0.1.0`, also on [Artifact Hub](https://artifacthub.io/packages/helm/querdian/querdian) |
| Binaries | `querdian_0.1.0_linux_{amd64,arm64}.tar.gz` with SPDX SBOMs, on the [GitHub release](https://github.com/querdian-dev/querdian/releases/tag/v0.1.0) |

## Install

```sh
helm install querdian oci://ghcr.io/querdian-dev/charts/querdian --version 0.1.0 \
  --namespace querdian --create-namespace -f my-values.yaml
```

You must set `config.auth` and `config.backends`. Start with the [quickstart](quickstart.md) and the
[policy guide](policy-guide.md). Helm installs the CRDs on first install only; apply `crds/` yourself
on upgrades. To pin the image by digest, set `image.digest` (it takes precedence over `image.tag`).

## Verify

The image, the chart and the checksums file are signed by the release workflow of this repository:

```sh
IDENTITY='^https://github.com/querdian-dev/querdian/\.github/workflows/release\.yml@refs/tags/'
ISSUER=https://token.actions.githubusercontent.com

cosign verify ghcr.io/querdian-dev/querdian:0.1.0 \
  --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER"
cosign verify ghcr.io/querdian-dev/charts/querdian:0.1.0 \
  --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER"
gh attestation verify oci://ghcr.io/querdian-dev/querdian:0.1.0 --repo querdian-dev/querdian
```

The checksums file is signed as a Sigstore bundle (`cosign verify-blob --bundle`, cosign v3 or later).
All commands, including the archives, are in [releasing.md](releasing.md#5-post-release-verification).

## Read this before you depend on it

The complete list is [limitations.md](limitations.md); the attacker model is in the
[threat model](security/threat-model.md). The ones that matter most:

* **Namespace-label integrity.** `AccessPolicy` isolation is only as strong as the integrity of the
  namespace label written by your collection pipeline. If workloads can set it, they can place data
  in another team's view. Querdian cannot detect that.
* **Strict by design.** Queries Querdian cannot prove it handles exactly as the backend would
  are rejected (`400`/`403`). Examples: PromQL range vectors that are not the direct argument of a
  function when a policy has several terms; LogQL range aggregations with `by`/`without` under
  several terms; unsupported LogQL and TraceQL syntax; TraceQL comments and `parent.*` scopes;
  trace-level TraceQL intrinsics (`rootName`, `traceDuration`, `compare()`) for users whose policy
  does not cover the root span.
* **Loki under tenant-bound policies (tested with Loki 3.5.0).** When the rules differ per tenant,
  the metadata endpoints `labels`, `label/<name>/values`, `series`, `index/stats` and `index/volume`
  do not work reliably (empty or incomplete results, never more than the policy allows). Multi-tenant
  grants do not work for `tail`, `series` and `detected_labels`. A proper fix needs one request per
  tenant (see [ROADMAP.md](../ROADMAP.md)).
* **Tempo has no tenant-id attribute.** Cross-tenant unions with differing terms are rejected on Tempo.
  Search responses lose `rootServiceName`, `rootTraceName` and `serviceStats`, and the trace start
  and duration are recomputed from the matching spans, for every user who is not unrestricted.
* **Denied labels hide, they do not redact.** `labels.deny` blocks filtering, grouping and listing of
  a label; the label can still appear in query results (except trace by ID). Use the opt-in
  `labels.redact` to rewrite its values as well; redacted responses are buffered rather than
  streamed.
* **Approximate results.** Summed Loki statistics, volume, patterns and detected-label cardinalities
  can count a record once per matching term when terms are not provably disjoint; such responses are
  marked with `X-Querdian-Approximate: true`.
* **Authentication.** Bearer tokens are valid until they expire; there is no revocation list, and an
  open Loki tail outlives the token and policy changes. Policy revocation is eventually consistent
  and survives a Kubernetes API outage (the previous snapshot is used).
* **Operations.** No rate limiting or quotas, no TLS termination (use an ingress or mesh), the
  management port is unauthenticated. Memory scales with concurrent merged responses
  (`limits.maxMergeBytes` each).
* **Tested versions.** The differential and equivalence suites ran against Prometheus v3.5.0,
  Loki 3.5.0 and Tempo 2.9.0. Other versions may diverge in corners the parsers do not model. Mimir
  and Thanos federation behaviour is based on research notes, not on end-to-end tests.
* **Scope.** Read paths only: no write, ingest, ruler or alert paths. Kubernetes only, HTTP API only
  (no gRPC or streaming search). The policy dashboard is read-only and meant for administrators
  ([dashboard.md](dashboard.md)).
* **Supply chain.** The signing, SBOM and attestation pipeline was exercised end to end by two release
  candidates (`v0.1.0-rc.1`, `v0.1.0-rc.2`) and verified as described in [releasing.md](releasing.md).
  No third-party security audit has been performed.
* **Project.** Single maintainer; not a CNCF project.

## Security

No released version was ever affected by a known vulnerability: this is the first release, and the
security-relevant fixes in the [CHANGELOG](../CHANGELOG.md) were found before it. Report
vulnerabilities as described in [SECURITY.md](../SECURITY.md).

## Upgrading

First release; nothing to upgrade from.
