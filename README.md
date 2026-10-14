# Querdian

Querdian (query + guardian) is an Apache-2.0, Kubernetes-native reverse proxy that enforces
label-based access control on read queries against Prometheus/Thanos (PromQL), Loki (LogQL) and
Tempo (TraceQL). Policies are Kubernetes CRDs, queries are rewritten with real parsers, and
everything fails closed: unknown endpoints, unknown users and unparseable queries are rejected.

**Status: pre-alpha.** Nothing here is ready for production use.

Inspired by [evoila/janus](https://github.com/evoila/janus).

See [PLAN.md](PLAN.md) for the design and [TASKS.md](TASKS.md) for the work breakdown.

## Building

```sh
make build test lint
```

## License

Apache-2.0, see [LICENSE](LICENSE).
