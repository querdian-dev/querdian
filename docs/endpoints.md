# Endpoint inventory

This page lists the HTTP endpoints of each backend type, and tells which endpoints Querdian enforces,
forwards unchanged or denies.

In short:

* Querdian denies each path that is not in these tables, and each listed path with a method that is
  not listed (`403`).
* **enforce**: Querdian rewrites the enforcement parameter for the policy. **safe**: Querdian forwards the
  request unchanged, but still sets the tenant header. **deny**: `403`.
* All paths are relative to the `pathPrefix` of the backend.
* The rows come from the public API documentation and from black-box tests with Loki 3.5.0. Items in
  the column "Grafana calls" that have the mark "(unconfirmed)" are not verified.

The limits of each endpoint are in [limitations.md](limitations.md).

## Columns

| Column | Meaning |
|---|---|
| Enforcement param | The parameter that holds the query or the selector. Querdian rewrites it only on `enforce` rows. |
| Multi | The client can legally repeat the parameter. Querdian rejects a repeated single-valued parameter with `400`. |
| PLAN | The mode of the endpoint: `enforce`, `safe`, `deny` or `traceByID`. "not listed -> deny" means that the first design did not list the endpoint. Querdian denies it. "new" marks a deny row from the [deltas](#deltas-to-the-design). |
| Grafana calls | Whether the Grafana data source calls the endpoint. The Grafana data source pages do not list the HTTP calls of the data sources. Thus this column has only what the pages state, plus items with the mark "(unconfirmed)". These items come from general knowledge and the end-to-end suite must confirm them black-box (Grafana against a logging stub). Do not treat them as verified. |

## 1. Prometheus / Thanos Query (`type: prometheus`), prefix `/api/v1/`

Thanos Query implements the Prometheus v1 API. The Thanos page documents only extra parameters (see
[question 3](#questions-and-decisions)). The response envelope of all JSON endpoints is
`{"status","data","errorType","error","warnings","infos"}`.

| Path | Methods | Enforcement param | Multi | Response `data` | PLAN | Grafana calls |
|---|---|---|---|---|---|---|
| `query` | GET, POST | `query` | no | `{resultType: vector\|scalar\|string, result}` | enforce | yes |
| `query_range` | GET, POST | `query` | no | `{resultType: matrix, result}` | enforce | yes |
| `query_exemplars` (experimental) | GET, POST | `query` (PromQL expr) | no | `[{seriesLabels, exemplars[]}]` | enforce | yes (exemplars) |
| `series` | GET, POST | `match[]` (required) | yes | `[labelset]` | enforce (match expand) | yes (opt-in "use series endpoint") |
| `labels` | GET, POST | `match[]` (optional) | yes | `[name]` | enforce (match expand) | yes |
| `label/<name>/values` | **GET only** | `match[]` (optional) | yes | `[value]` | enforce (match expand) | yes (default for label values) |
| `format_query` | GET, POST | `query` (not evaluated) | no | formatted string | safe | (unconfirmed) |
| `parse_query` (experimental) | GET, POST | `query` (not evaluated) | no | AST JSON | safe | (unconfirmed) |
| `status/buildinfo` | GET | - | - | `{version,revision,branch,buildUser,buildDate,goVersion}` | safe | (unconfirmed; flavour detection) |
| `metadata` | GET | `metric`, `limit`, `limit_per_metric` | no | `{metric: [{type,help,unit}]}` | deny | (unconfirmed; metrics browser help) |
| `targets`, `targets/metadata`, `targets/relabel_steps` | GET | - | - | targets | deny | no |
| `scrape_pools`, `scrape_pools/config` | GET | - | - | pool names / YAML | deny | no |
| `rules`, `alerts`, `alertmanagers` | GET | `match[]` on rules | yes | rule groups / alerts | deny | (unconfirmed; alerting UI) |
| `search/metric_names`, `search/label_names`, `search/label_values` (experimental, feature flag) | GET, POST | `match[]`, `search[]` | yes | NDJSON stream | deny (new, see Deltas) | no |
| `status/config`, `flags`, `runtimeinfo`, `tsdb`, `tsdb/blocks`, `walreplay`, `self_metrics` | GET | - | - | internals | deny | no |
| `admin/tsdb/snapshot`, `delete_series`, `clean_tombstones` | POST, PUT | `match[]` on delete | yes | - | deny | no |
| `write` | POST | - | - | - | deny | no |
| `read` | POST | - | - | protobuf | deny (new) | no |
| `otlp/v1/metrics` | POST | - | - | - | deny | no |
| `notifications`, `notifications/live` (SSE), `features` | GET | - | - | - | deny (new) | no |
| `/openapi.yaml` (outside `/api/v1/`) | GET | - | - | OpenAPI | deny (new) | no |

Notes: errors are 400, 422 or 503. Query endpoints accept `limit` (it only cuts the result). The
`stats` parameter of a query adds statistics for each step; it is harmless.

## 2. Loki (`type: loki`), prefix `/loki/api/v1/`

Multi-tenancy uses `X-Scope-OrgID`. Log query responses are
`{status, data:{resultType: streams, result, stats}}`. Metric query responses are `vector` or `matrix`.

| Path | Methods | Enforcement param | Multi | Response | PLAN | Grafana calls |
|---|---|---|---|---|---|---|
| `query` | GET, POST (form; verified, Q8) | `query` | no | `streams`\|`vector` | enforce | yes |
| `query_range` | GET, POST (form; verified, Q8) | `query` | no | `streams`\|`matrix` | enforce | yes |
| `labels` | GET | `query` (optional selector) | no | `[name]` | enforce (fan-out string set) | yes |
| `label/<name>/values` | GET | `query` (optional selector) | no | `[value]` | enforce (fan-out string set) | yes |
| `series` | GET, POST | `match[]` (required) | yes | `[labelset]` | enforce (match expand) | yes |
| `index/stats` | GET, POST | `query` | no | `{streams,chunks,entries,bytes}` | enforce (fan-out sum) | yes (unconfirmed) |
| `index/volume` | GET, POST | `query`; also `targetLabels`, `aggregateBy` | no | vector | enforce (fan-out sum) | yes (unconfirmed) |
| `index/volume_range` | GET, POST | `query`; also `step` | no | matrix | enforce (fan-out sum) | yes (unconfirmed) |
| `patterns` | GET | `query` | no | `[{pattern, samples}]` | enforce (fan-out sum) | yes (unconfirmed) |
| `detected_fields` | GET, POST | `query` | no | `{fields:[{label,type,cardinality,parsers,...}], limit}` | enforce (fan-out string set) | yes (unconfirmed) |
| `detected_field/<name>/values` | GET, POST | `query` | no | `{values, limit}` | enforce (fan-out string set) | yes (unconfirmed) |
| `detected_labels` | GET, POST (verified, Q1) | `query` (optional) | no | `{detectedLabels:[{label,cardinality}]}` | enforce (fan-out; merge by label) | yes (unconfirmed) - NOT in docs, see Q1 |
| `tail` | GET (WebSocket) | `query` | no | frames `{streams, dropped_entries}` | enforce (fan-out tail) | yes (Explore live) |
| `format_query` | GET, POST | `query` | no | `data: <string>` | safe | (unconfirmed) |
| `status/buildinfo` | GET | - | - | version info | safe | (unconfirmed) |
| `push`, `/otlp/v1/logs` | POST | - | - | - | deny | no |
| `rules*` (`/loki/api/v1/rules`, `/prometheus/api/v1/rules`, `/prometheus/api/v1/alerts`) | GET/POST/DELETE | - | - | YAML/JSON | deny | no |
| `delete` | GET, POST, PUT, DELETE | `query` | no | - | deny | no |
| `/api/prom/{push,tail,query,label,series,rules}` (deprecated) | various | - | - | - | deny | no |
| `/ready`, `/metrics`, `/config`, `/services`, `/log_level`, ring pages, `/flush`, `/ingester/*` | various | - | - | - | deny | no |

Notes:

* A **log** query on the instant `query` endpoint returns 400 upstream.
* Loki documents Parquet output with `Accept: application/vnd.apache.parquet`. The proxy cannot merge
  it (see [question 7](#questions-and-decisions)).
* The `targetLabels` and `aggregateBy` parameters of `volume*` name labels, so they must pass the
  denied-label check.

## 3. Tempo (`type: tempo`), prefix `/api/`

| Path | Methods | Enforcement param | Multi | Response | PLAN | Grafana calls |
|---|---|---|---|---|---|---|
| `search` | GET | `q` (TraceQL); legacy `tags` (logfmt), `minDuration`, `maxDuration` | no | `{traces:[...], metrics}` | enforce + redaction | yes |
| `v2/search/tags` | GET | `q` (optional) | no | `{scopes:[{name,tags}], metrics}` | enforce | yes |
| `v2/search/tag/<name>/values` | GET | `q` (optional) | no | `{tagValues:[{type,value}], metrics}` | enforce | yes |
| `search/tags` (v1) | GET | none (no `q`) | - | `{tagNames, metrics}` | not listed -> deny | possibly older Tempo (unconfirmed) |
| `search/tag/<name>/values` (v1) | GET | none (no `q`) | - | `{tagValues:[string], metrics}` | not listed -> deny | possibly older Tempo (unconfirmed) |
| `metrics/query_range` | GET | `q` (TraceQL metrics); `start`,`end`\|`since`,`step`,`exemplars` | no | Prometheus-like series | enforce | yes |
| `metrics/query` | GET | `q`; no `step` | no | one value per series | enforce | yes (unconfirmed) |
| `traces/<id>` | GET | none | - | OTel JSON; protobuf via `Accept: application/protobuf` | traceByID | yes |
| `v2/traces/<id>` | GET | none, but accepts client `q` (single spanset filter) | no | same, status `PARTIAL` when filtered | traceByID | yes |
| `v2/traces/diff` (experimental) | POST | JSON body | - | diff | not listed -> deny | no |
| `echo` | GET | - | - | body `echo` (used to test data sources in Grafana) | safe | yes (documented) |
| `status/buildinfo` | GET | - | - | version info | safe | (unconfirmed) |
| `overrides`, `mcp` | various | - | - | - | not listed -> deny | no |
| `/status*`, `/ready`, `/metrics`, `/memberlist`, `/usage_metrics`, `/querier/api/*`, rings, `/live-store/*`, `/debug/pprof/*` | various | - | - | - | deny | no |

The `traces[]` of a `search` response contain metadata of the root span. Querdian redacts it (see
[limitations.md](limitations.md#search-response-redaction)). Tempo also has gRPC streaming search
(`stream_over_http_enabled`); see [question 9](#questions-and-decisions).

## Questions and decisions

The inventory raised these questions. The code references them by number (for example "Q8").

| # | Question | Decision |
|---|---|---|
| 1 | Loki `detected_labels` is not in the Loki HTTP API page, which documents only `detected_fields` and `detected_field/<name>/values`. Grafana Logs Drilldown calls it (unconfirmed). | Answered by a [black-box test](#q1-detected_labels-loki-350): the route is `enforce` on `query`, with `GET` and `POST`. |
| 2 | Grafana's metrics browser uses Prometheus `metadata` for help text (unconfirmed). `metadata` lists metric names and help across all series, so it cannot be restricted by label (`match_target` exists only on `targets/metadata`). | Denied. The help text is not available in Grafana ([limitations.md](limitations.md#general-request-handling)). |
| 3 | Thanos-specific query parameters (`dedup`, `partial_response`, `replicaLabels`, `max_source_resolution`, `storeMatch[]`). `storeMatch[]` selects which stores serve a query and could bypass store-level separation. `partial_response` changes the failure semantics. | Each route has an allow-list of parameters. On `query`, `query_range`, `series`, `labels` and `label/<name>/values`, Querdian forwards `dedup`, `partial_response`, `replicaLabels`, `replicaLabels[]` and `max_source_resolution`. It never forwards `storeMatch[]`. Thanos-only paths that the documentation does not list, for example `/api/v1/stores`, are denied. |
| 4 | Tempo `v2/traces/<id>` accepts a client `q` (a TraceQL filter, plus `keep_hierarchy`, `match_depth`, `ancestor_depth`, `span_pruning*`). | Trace by ID accepts only `start` and `end`. Each other parameter, also `q`, is rejected (400). |
| 5 | Tempo `search` documents the legacy parameters `tags`, `minDuration` and `maxDuration` next to `q`. The documentation does not state if they combine with `q`. | `tags`, `minDuration` and `maxDuration` are always rejected (400), with or without `q`. They are evaluated outside TraceQL and cannot carry the policy. |
| 6 | `q` is optional on Tempo v2 tags and tag values. The same is true for `query` on Loki `labels` and label values. | Without the parameter, a restricted user gets a policy-only filter on Tempo and the policy selector on Loki, never a pass-through ([limitations.md](limitations.md#traceql-and-tempo)). |
| 7 | Fan-out cannot merge Loki Parquet (`Accept: application/vnd.apache.parquet`), Tempo `application/protobuf` or `application/vnd.grafana.llm`. | Merge plans send `Accept: application/json` upstream. With `traceByID: filter`, trace by ID filters JSON and protobuf, and anything else is `502` ([limitations.md](limitations.md#merged-and-buffered-responses)). |
| 8 | Loki `query` and `query_range`: the documentation shows only `GET`. | Answered by a [black-box test](#q8-post-on-query--query_range-loki-350): `POST` with a form body is accepted. Both routes allow `GET` and `POST`. |
| 9 | Grafana can use Tempo streaming over gRPC (`Search*`, `MetricsQueryRange` StreamingQuerier) when it is enabled. This is not HTTP. | Out of scope for the proxy. Disable streaming in the Grafana Tempo data source. |
| 10 | The public documentation does not confirm most of the endpoints that Grafana calls. | Open. A black-box capture of the Grafana requests in the end-to-end suite must settle the "(unconfirmed)" items. |

## Deltas to the design

The inventory added explicit deny rows to the first endpoint design. All of them are also denied by
default:

1. Prometheus: `label/<name>/values` is `GET` only. Deny rows for `targets/*`, `scrape_pools*`,
   `alertmanagers`, `search/*`, `notifications*`, `features`, `read`, `/openapi.yaml`.
2. Loki: deny rows for `/otlp/*`, the ruler paths (also `/prometheus/api/v1/*`) and the deprecated
   `/api/prom/*`.
3. Tempo: deny rows for v1 `search/tags`, `search/tag/<name>/values`, `v2/traces/diff`, `overrides`,
   `mcp`.

The inventory confirmed all enforce paths and parameters of the design. This includes Tempo
`status/buildinfo` at `/api/status/buildinfo`, `echo`, `metrics/query[_range]` with `q`, Loki `labels`
and `label/*/values` with `query`, and Loki `series` with `match[]`.

## Sources (accessed 2026-10-14)

Clean room: the inventory used only prometheus.io, thanos.io and grafana.com/docs. Nobody read Loki,
Tempo or Janus source code for it.

| Backend | URL |
|---|---|
| Prometheus | https://prometheus.io/docs/prometheus/latest/querying/api/ |
| Thanos Query | https://thanos.io/tip/components/query.md/ |
| Loki | https://grafana.com/docs/loki/latest/reference/loki-http-api/ |
| Tempo | https://grafana.com/docs/tempo/latest/api_docs/ |
| Grafana Prometheus DS | https://grafana.com/docs/grafana/latest/datasources/prometheus/configure/ |
| Grafana Loki DS | https://grafana.com/docs/grafana/latest/datasources/loki/ |
| Grafana Tempo DS | https://grafana.com/docs/grafana/latest/datasources/tempo/configure-tempo-data-source/ |

## Black-box results

The script `hack/research/loki-endpoints/run.sh` has the exact curl commands. The captured output is in
`results.txt`. Set-up: Loki `grafana/loki:3.5.0` (`/loki/api/v1/status/buildinfo`: version 3.5.0,
revision 4b16bc4f), single binary, `auth_enabled: true`, two streams pushed (`{app="a",env="prod"}`,
`{app="b",team="x"}`). The test used HTTP only.

### Q1 `detected_labels` (Loki 3.5.0)

* The endpoint exists. `GET` and `POST` both return 200 (`POST` with a form body or a URL query).
* Shape: `{"detectedLabels":[{"label":"app","cardinality":1}, ...]}`. The order of the array changes
  between calls, so merge and compare it as a set. It includes the derived `service_name` label. It has
  only structured metadata and stream labels. No match: `{}` (not an empty array).
* Loki uses `query` (a stream selector): `{app="a"}` gives app, env, service_name (cardinality 1).
  `{app="b"}` gives app, service_name, team. Without `query`, the result is the union (app and
  service_name with cardinality 2). An invalid selector gives 400 with a plain-text parse error.
  `start`/`end` (ns) and `since` work.
* Loki ignores unknown parameters (`foo=bar`) silently. With a duplicate `query`, Loki uses the first
  value.
* Decision: the route is `enforce` on `query`, with the methods `GET` and `POST`, and the allowed
  parameters `start,end,since`. The response has only label names and cardinalities. The merge across
  tenants is by label name. A cardinality across tenants cannot be summed exactly, so it is an upper
  bound ([limitations.md](limitations.md#approximate-results)).

### Q8 POST on `query` / `query_range` (Loki 3.5.0)

* Both endpoints accept `POST` with `application/x-www-form-urlencoded`, and return the same results as
  `GET` (`streams` for `query_range`, `vector` for a metric `query`). Log queries on `/query` are 400
  for `GET` and `POST` ("log queries are not supported as an instant query").
* `POST` with all parameters in the URL query and an empty body also works.
* **If the URL and the form both have `query`, the form value wins** (URL `app=a` and form `app=b`
  returned app b; the same on `detected_labels`). With repeated `query` values (URL or form), Loki uses
  the first one. Thus the proxy must not let a request have `query` in both places, or a repeated
  `query`. Otherwise the enforced query and the executed query could be different. The proxy answers
  400 for both.
* Loki does not parse a JSON body (`application/json`): 400 (parse error, the query is seen as empty).
  The proxy writes a form body and sets the content type itself.
* Decision: `query` and `query_range` allow `GET` and `POST`.

## Response shapes for the merges

These shapes come from `grafana/loki:3.5.0` with the configuration of
`hack/research/loki-endpoints/loki.yaml` (`auth_enabled: true`, one tenant, the streams
`{app="a",env="prod"}` and `{app="b",team="x"}`), over HTTP only. `internal/fanout` merges them.

* `query_range`, log query:
  `{"status":"success","data":{"resultType":"streams","result":[{"stream":{...},"values":[["<ns>","<line>"]]}],"stats":{summary,querier,ingester,cache,index}}}`.
  * `direction=backward` (the default) lists streams and entries newest first, `forward` oldest first.
  * Each label set is a separate stream (Loki adds `detected_level` and `service_name`).
  * No match: `"result":[]` (`stats` is still there).
  * With the header `X-Loki-Response-Encoding-Flags: categorize-labels`, the data object gets
    `"encodingFlags":["categorize-labels"]`, and each entry gets a third element
    `{"structuredMetadata":{...}}` (and `detected_level` leaves the stream labels).
  * `stats.summary` has counters, rates (`bytesProcessedPerSecond`, ...) and `execTime`.
* `labels`: `{"status":"success","data":["app","env"]}`. **No match: `{"status":"success"}` without
  `data`**. `label/<n>/values` has the same shape.
* `series`: `{"status":"success","data":[{"app":"a",...}]}`. No match: `"data":[]`. Without `match[]`,
  Loki returns all series.
* `detected_labels`: `{"detectedLabels":[{"label","cardinality"}]}`. No match: `{}` (see
  [Q1](#q1-detected_labels-loki-350)).
* `detected_fields`: `{"fields":[{"label","type","cardinality","parsers":["logfmt"]|null}],"limit":1000}`.
  No match: `{}`. **`query` is required: without it, Loki answers 400**
  (`parse error : syntax error: unexpected $end`).
* `detected_field/<n>/values`: `{"limit":1000,"values":["hello"]}`. No match: `{}`.
* `index/stats`: flat `{"streams":N,"chunks":N,"bytes":N,"entries":N}` (zeros when nothing matches).
* `index/volume`:
  `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"app":"a"},"value":[<sec>,"<bytes>"]}],"stats":{...}}}`.
  Without `targetLabels`, the metric is the full label set of the series (so denied labels would be
  visible). With `aggregateBy=labels&targetLabels=app`, it is `{"app":"a"}` for each value.
* `index/volume_range`: Loki answers the documented `matrix` (`values:[[ts,"n"],...]`) only when the
  data of the selector spans several steps. When all data is in one step (always for a step larger than
  the data span), the answer is a `vector` with one sample for each series at that step. When nothing
  matches, the answer is an empty `vector` (Loki 3.5.0, seen in the end-to-end suite). Thus the fan-out
  branches of one request can have different shapes. The merge treats a vector branch as a matrix
  series with a single point.
* `patterns`: Loki answers 404 unless the pattern ingester is enabled (`pattern_ingester.enabled: true`).
  With the ingester enabled and little data, the answer was `{"status":"success","data":[]}`. The
  non-empty shape `data:[{"pattern":"...","samples":[[<sec>,<count>],...]}]` comes from the public
  documentation and is **not verified**.
