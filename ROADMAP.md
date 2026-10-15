# Roadmap

This is a statement of **intent, not a commitment or a schedule**. Querdian has one maintainer and
no funding commitments; items move when someone works on them. Every item below is derived from a
documented limitation or an unfinished task in this repository ([TASKS.md](TASKS.md),
[docs/limitations.md](docs/limitations.md), [docs/research/](docs/research/)); nothing here is a
promise to a user. Priorities will follow real feedback.

## Now: 0.1.0

* Finish the release: kind quickstart verified against the published artefacts, signed image,
  chart and provenance verified, release notes (T5.6, T5.7;
  [docs/releasing.md](docs/releasing.md)).
* OpenSSF Best Practices "passing" badge application and a TAG Security self-assessment review
  ([docs/openssf-best-practices.md](docs/openssf-best-practices.md),
  [docs/security/self-assessment.md](docs/security/self-assessment.md)).

## After 0.1: correctness gaps that are documented today

* **Loki under tenant-bound policies:** send one request per tenant instead of relying on a
  `__tenant_id__` matcher, so that `labels`, `label/<name>/values`, `series`, `index/stats` and
  `index/volume` work, and extend the same approach to the multi-tenant `tail`, `series` and
  `detected_labels` cases that fail today (limitations: "Findings from the end-to-end suite").
* **Thanos and Mimir end-to-end tests.** The end-to-end suites run against Prometheus, Loki and Tempo.
  Thanos Query support is covered by route and parameter tests only; Mimir federation was researched
  (`docs/research/tenant-federation.md`) but is not tested end to end.
* **Loki `patterns` and other unverified response shapes**, and the follow-ups listed in
  `docs/research/logql.md` and `docs/research/traceql.md` (for example Tempo metrics functions).
* **Backend version matrix.** Re-run the differential suites on more Prometheus, Loki and Tempo
  versions and record which are supported.
* **Duplicate tenants and unusual tenant IDs** (`t1|t1`), untested today.
* **Native histograms.** Evaluate PromQL native-histogram queries and functions against the
  enforcer and the dashboard; the Prometheus parser is used with default options today, so
  experimental syntax is rejected (limitations: PromQL).

## Project and supply chain

* CI additions: run `make e2e`, `make chart-lint` and `make release-check` (only the lighter jobs
  run on every pull request today; PLAN section 13 envisages nightly long fuzzing and full E2E).
* Measure test coverage.
* A second maintainer and contributors from other organizations (GOVERNANCE.md assumes more than
  one reviewer).
* CNCF Sandbox application once there are adopters ([GOVERNANCE.md](GOVERNANCE.md), PLAN section 15).
* Promote the CRD API beyond `v1alpha1` after real-world feedback (PLAN section 14).

## Ideas, not planned

These are open questions rather than tasks; the current non-goals in PLAN section 1 still stand
(no write paths, no UI, Kubernetes only).

* Further read-only backends beyond Prometheus/Thanos, Loki and Tempo. Nothing is designed.
* Tempo gRPC and streaming search (unsupported today, see limitations).
* Built-in rate limiting or per-user quotas (today these belong to the ingress and the backends).
