# Contributing

Thanks for helping! Read [PLAN.md](PLAN.md) first; [TASKS.md](TASKS.md) lists the work items.

## Developer Certificate of Origin

Every commit must be signed off (`git commit -s`), certifying the
[DCO](https://developercertificate.org/). CI rejects unsigned commits.

## Clean-room rule

- Do **not** import, copy, or read the source of `grafana/loki` (except Apache-licensed dirs
  `pkg/logproto`, `pkg/loghttp`, `pkg/logqlmodel`, `pkg/push`) or `grafana/tempo` (except
  `pkg/tempopb`). In particular never open Loki's or Tempo's grammar/parser sources.
  The generated protobuf types in `internal/tempopb` are copied from Tempo's `pkg/tempopb` by
  `hack/sync-tempopb.sh`; never edit them by hand.
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
make e2e              # differential/equivalence suites against real Prometheus, Loki and Tempo (needs docker)
make chart-lint       # Helm chart lint (needs helm)
make licenses         # dependency licence gate
```

## What a change needs

* Unit tests for new code, and tests that fail without the change for a bug fix. Parsers and rewriters
  also need corpus or fuzz coverage; behaviour that depends on a backend needs a case in `test/e2e`.
* `make lint test-race` green, `make generate manifests` without a diff, and no new dependency outside
  the licence allowlist (`make licenses`).
* Documentation for user-visible behaviour (`docs/`), including new decision reasons in
  `docs/troubleshooting.md`.
* A sign-off on every commit (`git commit -s`).

## Reporting bugs and requesting features

Open an issue using the templates. For anything that lets a user read data their policy does not grant,
do not open an issue: see [SECURITY.md](SECURITY.md).

## Reporting security issues

See [SECURITY.md](SECURITY.md).
