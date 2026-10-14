# Contributing

Thanks for helping! Read [PLAN.md](PLAN.md) first; [TASKS.md](TASKS.md) lists the work items.

## Developer Certificate of Origin

Every commit must be signed off (`git commit -s`), certifying the
[DCO](https://developercertificate.org/). CI rejects unsigned commits.

## Clean-room rule

- Do **not** import, copy, or read the source of `grafana/loki` (except Apache-licensed dirs
  `pkg/logproto`, `pkg/loghttp`, `pkg/logqlmodel`, `pkg/push`) or `grafana/tempo` (except
  `pkg/tempopb`). In particular never open Loki's or Tempo's grammar/parser sources.
- Do **not** copy anything (code, tests, fixtures, docs, images) from `evoila/janus`.
- The LogQL and TraceQL parsers are written **only from the public language documentation**
  (grafana.com/docs). Record the doc URLs used in package docs.
- Running Loki/Tempo containers as black boxes in tests is allowed.

## Workflow

- One task = one branch (`t<id>-<short-slug>`) = one pull request.
- Conventional commits (`feat(policy): …`, `test(logql): …`).
- Dependencies must be Apache-2.0 or on the CNCF allowlist; `make licenses` enforces this.

## Running tests

```sh
make lint test        # unit tests and linters
make test-race        # with the race detector
make fuzz             # short fuzz run
make envtest          # controller tests against a local API server
make generate manifests   # must produce no diff
```

## Reporting security issues

See [SECURITY.md](SECURITY.md).
