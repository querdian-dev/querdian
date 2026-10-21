<p align="center"><img src="assets/logo.svg" alt="Querdian logo" width="96"></p>

<h1 align="center">Querdian</h1>

Querdian (query + guardian) is an Apache-2.0, Kubernetes-native reverse proxy that enforces
label-based access control on read queries against Prometheus/Thanos (PromQL), Loki (LogQL) and
Tempo (TraceQL). Policies are Kubernetes CRDs, queries are rewritten with real parsers, and
everything fails closed: unknown endpoints, unknown users and unparseable queries are rejected.

<!--
  Badges: ACTIVATE AFTER REGISTRATION. Do not uncomment before the project exists on
  bestpractices.dev (replace <ID> with the numeric project ID) and the Scorecard workflow has
  published a result for the public repository, otherwise the images 404. See
  docs/openssf-best-practices.md (items H5 and H8).

[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/<ID>/badge)](https://www.bestpractices.dev/projects/<ID>)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/querdian-dev/querdian/badge)](https://scorecard.dev/viewer/?uri=github.com/querdian-dev/querdian)
-->

**Status: pre-alpha, not released.** Nothing here is ready for production use. There is no tagged
release yet. The proxy, the policy CRDs and the PromQL, LogQL and TraceQL enforcers are implemented
and covered by unit, fuzz, envtest and container-based differential/equivalence suites (`make e2e`)
against real Prometheus, Loki and Tempo. A Helm chart, a Dockerfile, a release pipeline and a kind
quickstart are in the repository; the release pipeline has not run yet, so no image or chart is
published. See [CHANGELOG.md](CHANGELOG.md) and [ROADMAP.md](ROADMAP.md).

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
