# Querdian v0.1.1 release notes

A security release. **Upgrade if you proxy Tempo**: in 0.1.0, Tempo tag lookups could list tag names
and values of spans outside a user's policy
([GHSA-jcqw-3c3f-xrmj](https://github.com/querdian-dev/querdian/security/advisories/GHSA-jcqw-3c3f-xrmj)).
Prometheus and Loki backends are not affected. The API (`policy.querdian.dev/v1alpha1`), the
configuration and the Helm values are unchanged.

## Security

Querdian enforced policies on Tempo's `api/v2/search/tags` and `api/v2/search/tag/<name>/values` by
injecting the policy into `q`. Tempo does not evaluate every `q` on these endpoints: for a filter shape it
cannot use there it answers unfiltered, and the injected policy is dropped with the rest of `q`
(black-box: Tempo 2.9.0 and 2.10.10 for terms joined by `||`, Tempo 3.1.1 for `=~`). Affected were
restricted users whose policy has rules on different attributes (Tempo 2.x) or a regex, including
several `=` rules on one attribute (Tempo 3.x). Search, trace by ID, TraceQL metrics and tenant
isolation were not affected.

0.1.1 sends restricted tag lookups per policy term, each as one filter of `attribute = "string"`
conditions, and unites the answers. This changes behaviour for some users:

* Policies with `!=`, `!~` or an open regex (`ns-.*`) get `400 unsupported` on the two tag endpoints;
  Grafana's TraceQL autocomplete then offers no values for them. Search and metrics work as before.
* A user `q` on these endpoints is honoured only when it is a conjunction of `span.x = "v"`,
  `resource.x = "v"` and `status = ...` conditions; otherwise it is ignored and the answer lists the
  tags or values of all granted spans.
* A policy with several terms causes one upstream request per term, capped by `limits.maxFanout`
  (default 16).

Details are in [limitations.md](limitations.md#traceql-and-tempo). The fix was verified with the Tempo
end-to-end suite against Tempo 2.9.0, 2.10.10 and 3.1.1.

If you cannot upgrade, deny `api/v2/search/tags` and `api/v2/search/tag/*` to restricted users in
front of Querdian, or use only single-term policies whose matchers are all `=` (on Tempo 3.x also with
a single value per attribute).

## Other changes

* The end-to-end suites and the quickstart run Prometheus v3.15.1, Loki 3.7.9 and **Tempo 3.1.1**
  (0.1.0 was tested with v3.5.0, 3.5.0 and 2.9.0). Apart from the tag-lookup fix above, Tempo 3 needed
  no change in Querdian; the quickstart runs it in monolithic mode (live-store, no Kafka), with
  Grafana 13.2.4.
* Dependency updates, see the [CHANGELOG](../CHANGELOG.md): the distroless base image, Go modules
  (`prometheus/common` v0.72.1, `prometheus/prometheus` v0.315.1), release tooling and GitHub Actions.

## Artefacts

| | |
|---|---|
| Container image | `ghcr.io/querdian-dev/querdian:0.1.1` (`linux/amd64`, `linux/arm64`; distroless, non-root) |
| Helm chart | `oci://ghcr.io/querdian-dev/charts/querdian`, version `0.1.1`, also on [Artifact Hub](https://artifacthub.io/packages/helm/querdian/querdian) |
| Binaries | `querdian_0.1.1_linux_{amd64,arm64}.tar.gz` with SPDX SBOMs, on the [GitHub release](https://github.com/querdian-dev/querdian/releases/tag/v0.1.1) |

## Upgrading from 0.1.0

```sh
helm upgrade querdian oci://ghcr.io/querdian-dev/charts/querdian --version 0.1.1 \
  --namespace querdian -f my-values.yaml
```

The CRDs are unchanged. No configuration change is needed.

## Verify

```sh
IDENTITY='^https://github.com/querdian-dev/querdian/\.github/workflows/release\.yml@refs/tags/'
ISSUER=https://token.actions.githubusercontent.com

cosign verify ghcr.io/querdian-dev/querdian:0.1.1 \
  --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER"
cosign verify ghcr.io/querdian-dev/charts/querdian:0.1.1 \
  --certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER"
gh attestation verify oci://ghcr.io/querdian-dev/querdian:0.1.1 --repo querdian-dev/querdian
```

All commands, including the archives, are in [releasing.md](releasing.md#5-post-release-verification).
