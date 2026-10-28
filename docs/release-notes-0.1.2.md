# Querdian v0.1.2 release notes

A maintenance release with a dependency security fix. **Upgrade recommended**: go-jose v4.1.6 fixes a
denial of service when parsing JWKs with RSA keys
([GHSA-mgcc-cww5-fp2q](https://github.com/advisories/GHSA-mgcc-cww5-fp2q)), and Querdian parses the OIDC
issuer's JWKS with it. OIDC issuer discovery failures are now logged. The API
(`policy.querdian.dev/v1alpha1`), the configuration and the Helm values are unchanged.

## Security

* **go-jose v4.1.6** ([GHSA-mgcc-cww5-fp2q](https://github.com/advisories/GHSA-mgcc-cww5-fp2q),
  CVE-2026-116587, high). Querdian verifies tokens with `go-oidc`, which fetches the JWKS from the
  configured issuer and parses it with `go-jose`. A JWKS with an oversized RSA key could exhaust
  Querdian's CPU. Exploiting it requires control of the JWKS response: a compromised identity provider,
  or the network path to an issuer configured with `http`. go-jose now rejects RSA keys over 4096 bits.
  If you cannot upgrade, configure the issuer with `https`.
* **golang.org/x/net v0.62.0** fixes GO-2026-6860 and GO-2026-6866 in `x/net/html`. Querdian does not
  call the affected code (govulncheck); the update keeps image scanners clean.

## Fixed

* **OIDC issuer discovery failures are logged.** A wrong issuer URL, a bad CA or a DNS failure used to
  leave `/readyz` at "issuer discovery not complete" with no cause anywhere. Each failed attempt now
  logs `issuer discovery failed` at warn level with the issuer, the error and the retry delay; success
  logs `issuer discovery complete`. See [troubleshooting.md](troubleshooting.md).

## Other changes

* Dependency updates, see the [CHANGELOG](../CHANGELOG.md): Go modules (`prometheus/common` v0.73.0,
  `k8s.io/*` v0.37.2), the `golang:1.27.3` builder image, and in the quickstart Grafana 13.3.0 and
  OpenTelemetry Collector 0.164.0.
* `prometheus/common` v0.73.0 adds `github.com/hashicorp/go-uuid` (MPL-2.0, file-level copyleft, used
  unmodified) to the binary. The license check allows it explicitly; it is listed in the SBOMs.

## Artefacts

| | |
|---|---|
| Container image | `ghcr.io/querdian-dev/querdian:0.1.2` (`linux/amd64`, `linux/arm64`; distroless, non-root) |
| Helm chart | `oci://ghcr.io/querdian-dev/charts/querdian`, version `0.1.2`, also on [Artifact Hub](https://artifacthub.io/packages/helm/querdian/querdian) |
| Binaries | `querdian_0.1.2_linux_{amd64,arm64}.tar.gz` with SPDX SBOMs, on the [GitHub release](https://github.com/querdian-dev/querdian/releases/tag/v0.1.2) |

## Upgrading from 0.1.1

```sh
helm upgrade querdian oci://ghcr.io/querdian-dev/charts/querdian --version 0.1.2 \
  --namespace querdian -f my-values.yaml
```

The CRDs are unchanged. No configuration change is needed. From 0.1.0, also read the
[0.1.1 release notes](release-notes-0.1.1.md).

## Verify

```sh
IDENTITY='^https://github.com/querdian-dev/querdian/\.github/workflows/release\.yml@refs/tags/'
ISSUER=https://token.actions.githubusercontent.com

cosign verify ghcr.io/querdian-dev/querdian:0.1.2 \
  --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER"
cosign verify ghcr.io/querdian-dev/charts/querdian:0.1.2 \
  --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER"
gh attestation verify oci://ghcr.io/querdian-dev/querdian:0.1.2 --repo querdian-dev/querdian
```

All commands, including the archives, are in [releasing.md](releasing.md#5-post-release-verification).
