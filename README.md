# Querdian

Querdian (query + guardian) is an Apache-2.0, Kubernetes-native reverse proxy that enforces
label-based access control on read queries against Prometheus/Thanos (PromQL), Loki (LogQL) and
Tempo (TraceQL). Policies are Kubernetes CRDs, queries are rewritten with real parsers, and
everything fails closed: unknown endpoints, unknown users and unparseable queries are rejected.

**Status: pre-alpha.** Nothing here is ready for production use. There is no release, container
image or Helm chart yet; the proxy, the policy CRDs and the PromQL, LogQL and TraceQL enforcers are
implemented, end-to-end and differential test suites are still open.

Inspired by [evoila/janus](https://github.com/evoila/janus).

See [PLAN.md](PLAN.md) for the design and [TASKS.md](TASKS.md) for the work breakdown.

## Documentation

* [Policy guide](docs/policy-guide.md): the CRDs, union semantics, tenants, denied labels and status
  conditions. Includes the requirement that the namespace label is set by your telemetry pipeline.
* [Operations](docs/operations.md): configuration reference, flags, limits, metrics, tracing, logging.
* [Troubleshooting](docs/troubleshooting.md): decision reason codes and ready-made PromQL/TraceQL queries.
* [Known limitations](docs/limitations.md): strictness choices and unsupported syntax.
* [Threat model](docs/security/threat-model.md) and [security policy](SECURITY.md).
* Design notes: [PromQL enforcement](docs/promql-enforcement.md), [endpoint inventory](docs/endpoints.md),
  [research notes](docs/research/).

## Building

```sh
make build test lint
```

## License

Apache-2.0, see [LICENSE](LICENSE).
