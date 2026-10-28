<p align="center"><img src="assets/logo.svg" alt="Querdian logo" width="96"></p>

<h1 align="center">Querdian</h1>

Querdian (query + guardian) is an Apache-2.0, Kubernetes-native reverse proxy that enforces
label-based access control on read queries against Prometheus/Thanos (PromQL), Loki (LogQL) and
Tempo (TraceQL). Policies are Kubernetes CRDs, queries are rewritten with real parsers, and
everything fails closed: unknown endpoints, unknown users and unparseable queries are rejected.

[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/15762/badge)](https://www.bestpractices.dev/projects/15762)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/querdian-dev/querdian/badge)](https://scorecard.dev/viewer/?uri=github.com/querdian-dev/querdian)
[![Artifact Hub](https://artifacthub.io/badge/repository/querdian)](https://artifacthub.io/packages/helm/querdian/querdian)

**Status: alpha, [v0.1.2](https://github.com/querdian-dev/querdian/releases/tag/v0.1.2) is the latest
release.** It is not ready for production use: the policy API is `v1alpha1` and may change
incompatibly, and there are no production adopters yet. The proxy, the policy CRDs and the PromQL,
LogQL and TraceQL enforcers are covered by unit, fuzz, envtest and container-based
differential/equivalence suites (`make e2e`) against real Prometheus, Loki and Tempo. Read the
[release notes](docs/release-notes-0.1.2.md) and the [known limitations](docs/limitations.md) before
you evaluate it. See also [CHANGELOG.md](CHANGELOG.md) and [ROADMAP.md](ROADMAP.md).

## Install

```sh
helm install querdian oci://ghcr.io/querdian-dev/charts/querdian --version 0.1.2 \
  --namespace querdian --create-namespace -f my-values.yaml
```

* Helm chart: `oci://ghcr.io/querdian-dev/charts/querdian`, also on
  [Artifact Hub](https://artifacthub.io/packages/helm/querdian/querdian); values in the
  [chart README](charts/querdian/README.md).
* Container image: `ghcr.io/querdian-dev/querdian:0.1.2` (`linux/amd64`, `linux/arm64`).
* Binaries, checksums and SBOMs: the [GitHub release](https://github.com/querdian-dev/querdian/releases/tag/v0.1.2).
* Everything is signed with cosign (keyless) and has build-provenance attestations; see
  [Verify](docs/release-notes-0.1.2.md#verify).

To see it working first, run the [quickstart](docs/quickstart.md).

Inspired by [evoila/janus](https://github.com/evoila/janus).

See [PLAN.md](PLAN.md) for the design and [TASKS.md](TASKS.md) for the work breakdown.

The website (landing page and rendered documentation) is built from [site/](site/) and `docs/` and published at <https://querdian.dev>, see [site/README.md](site/README.md).

## Documentation

* [Quickstart on kind](docs/quickstart.md): a complete isolation demo (Keycloak, Grafana, Prometheus,
  Loki, Tempo, two teams, 24 h of demo history and four Grafana dashboards) on a laptop with one script.
* [Policy guide](docs/policy-guide.md): the CRDs, union semantics, tenants, denied labels and status
  conditions. Includes the requirement that the namespace label is set by your telemetry pipeline.
* [Policy dashboard](docs/dashboard.md): optional read-only view of active policies and a query test bench.
* [Operations](docs/operations.md): configuration reference, flags, limits, metrics, tracing, logging.
* [Troubleshooting](docs/troubleshooting.md): decision reason codes and ready-made PromQL/TraceQL queries.
* [Known limitations](docs/limitations.md): strictness choices and unsupported syntax.
* [Threat model](docs/security/threat-model.md) and [security policy](SECURITY.md).
* [Releasing](docs/releasing.md) (maintainers), [OpenSSF Best Practices checklist](docs/openssf-best-practices.md)
  and the [security self-assessment](docs/security/self-assessment.md).
* Design notes: [PromQL enforcement](docs/promql-enforcement.md), [endpoint inventory](docs/endpoints.md),
  [research notes](docs/research/).

## Contributing and reporting

* Bugs and feature requests: open an issue at <https://github.com/querdian-dev/querdian/issues>.
  Please include the Querdian version or commit, the backend and its version, the request (with
  secrets removed), the response and the `X-Trace-Id` of the response.
* Contributions: see [CONTRIBUTING.md](CONTRIBUTING.md) (DCO sign-off and the clean-room rule apply).
* Security problems: do **not** open a public issue, follow [SECURITY.md](SECURITY.md).
* Project governance: [GOVERNANCE.md](GOVERNANCE.md), [MAINTAINERS.md](MAINTAINERS.md),
  [ADOPTERS.md](ADOPTERS.md), [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## Building

```sh
make build test lint
```

## License

Apache-2.0, see [LICENSE](LICENSE).
