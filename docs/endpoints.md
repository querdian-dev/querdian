# Endpoint inventory (T0.3)

Verification of PLAN §8 against the current public API documentation. Clean room: only
prometheus.io, thanos.io and grafana.com/docs were used. No Loki/Tempo/Janus source was read.

Legend: **enforce** = query parameter rewritten per policy; **safe** = proxied unchanged (tenant
header still set); **deny** = 403. "Multi" = parameter may legally be repeated by the client
(PLAN §9 rejects duplicates of single-valued enforcement params with 400). All paths are relative
to the backend's `pathPrefix`.

## Sources (accessed 2026-10-14)

| Backend | URL |
|---|---|
| Prometheus | https://prometheus.io/docs/prometheus/latest/querying/api/ |
| Thanos Query | https://thanos.io/tip/components/query.md/ |
| Loki | https://grafana.com/docs/loki/latest/reference/loki-http-api/ |
| Tempo | https://grafana.com/docs/tempo/latest/api_docs/ |
| Grafana Prometheus DS | https://grafana.com/docs/grafana/latest/datasources/prometheus/configure/ |
| Grafana Loki DS | https://grafana.com/docs/grafana/latest/datasources/loki/ |
| Grafana Tempo DS | https://grafana.com/docs/grafana/latest/datasources/tempo/configure-tempo-data-source/ |

Limitation: the Grafana data source pages do not enumerate the HTTP calls the data sources make.
The "Grafana calls" column is therefore limited to what the docs state, plus items marked
"(unconfirmed)" which come from general knowledge and MUST be confirmed black-box in the E2E suite
(Grafana against a logging stub). Do not treat unconfirmed items as verified.

## 1. Prometheus / Thanos Query (`type: prometheus`), prefix `/api/v1/`

Thanos Query implements the Prometheus v1 API; the Thanos page documents only extra parameters
(see Deltas). Response envelope for all JSON endpoints: `{"status","data","errorType","error","warnings","infos"}`.

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

Notes: errors are 400/422/503. `limit` is accepted on query endpoints (result truncation only).
`stats` param on query adds per-step stats; harmless.

## 2. Loki (`type: loki`), prefix `/loki/api/v1/`

Multi-tenancy via `X-Scope-OrgID`. Log query responses: `{status, data:{resultType: streams, result, stats}}`;
metric: `vector`/`matrix`.

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

Notes: `query`/`query_range` on a **log** query via instant `query` returns 400 upstream. Parquet
output via `Accept: application/vnd.apache.parquet` is documented; the proxy cannot merge it
(see Open questions). `volume*` `targetLabels`/`aggregateBy` name labels and must pass the
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

`search` response `traces[]` carries root-span metadata (PLAN §7.4 redaction); exact field list
is T0.5 scope. Tempo also offers gRPC streaming search (`stream_over_http_enabled`); see Open questions.

## Deltas to PLAN §8

Applied to PLAN.md §8 (pure facts, all still deny by default):
1. Prometheus: `label/<name>/values` is GET only; explicit deny rows added for `targets/*`,
   `scrape_pools*`, `alertmanagers`, `search/*`, `notifications*`, `features`, `read`, `/openapi.yaml`.
2. Loki: explicit deny for `/otlp/*`, ruler paths incl. `/prometheus/api/v1/*`, deprecated `/api/prom/*`.
3. Tempo: explicit deny for v1 `search/tags`, `search/tag/<name>/values`, `v2/traces/diff`, `overrides`, `mcp`.

Confirmed as correct: all enforce paths and parameters in §8.1-8.3 (including `status/buildinfo`
under Tempo being `/api/status/buildinfo`, `echo`, `metrics/query[_range]` with `q`, Loki
`labels`/`label/*/values` taking `query`, Loki `series` taking `match[]`).

Not changed in PLAN, raised below: everything marked as a question.

## Open questions (to raise; no issues opened, repo not reachable)

1. **`question(t0.3)`: Loki `detected_labels`. ANSWERED (T1.8 black-box, Loki 3.5.0, see "Black-box results").** PLAN §8.2 lists it, but the Loki HTTP API page
   documents only `detected_fields` and `detected_field/<name>/values`. Grafana Logs Drilldown
   is known to call it (unconfirmed). Options: keep as enforce with `query`, determine method and
   response shape black-box in T3.x/e2e, or deny until verified. Recommendation: black-box test,
   deny until then.
2. **Prometheus `metadata` is denied** but Grafana's metrics browser uses it (unconfirmed) for
   help text. It lists metric names/help across all series, so it cannot be restricted by label
   (`match_target` only exists on `targets/metadata`). Deny is safe; Grafana degrades gracefully
   if "Disable metrics lookup" is not needed. Confirm in e2e; decide whether to document.
3. **Thanos-specific query parameters** (`dedup`, `partial_response`, `replicaLabels`,
   `max_source_resolution`, `storeMatch[]`). They are not in PLAN §8/§9. `storeMatch[]` selects
   which stores serve a query and could bypass store-level separation; `partial_response` changes
   failure semantics (fail-closed rule §2.4). Proposal: allow-list forwarded params per endpoint and
   drop/deny the rest. Also unverified in docs: Thanos `/api/v1/stores` and other Thanos-only
   paths (page does not list them) - default deny.
4. **Allow-list of query parameters in general**: Tempo `v2/traces/<id>` accepts a client `q`
   (a TraceQL filter, plus `keep_hierarchy`, `match_depth`, `ancestor_depth`, `span_pruning*`).
   With `traceByID: filter` the proxy must strip or reject client `q` and must verify the filtered
   output is post-processed after upstream filtering. Proposal: reject `q` on trace-by-id
   (400), forward only `start`/`end`.
5. **Tempo `search` legacy params**: PLAN rejects `tags` only when `q` is absent. Docs list `tags`,
   `minDuration`, `maxDuration` next to `q`; whether they combine with `q` is not stated.
   Proposal: reject `tags` always (400); `minDuration`/`maxDuration` are safe (filters only)
   but verify in T0.5.
6. **Tempo v2 tags/values without `q`**: `q` is optional; when absent the enforcer must inject a
   policy-only filter (not pass through). Confirm in T4.x tests; applies equally to Loki
   `labels`/`label values` without `query`.
7. **Content negotiation**: Loki Parquet (`Accept: application/vnd.apache.parquet`), Tempo
   `application/protobuf` and `application/vnd.grafana.llm` cannot be merged by fan-out. Proposal:
   for merge plans, require JSON (strip/reject other `Accept`); for traceByID filter, support JSON
   and protobuf as in PLAN §7.4, reject `vnd.grafana.llm`.
8. **ANSWERED (T1.8 black-box, Loki 3.5.0, see "Black-box results"): POST form is accepted.** Original text: **Loki `query`/`query_range` methods**: docs show GET only; PLAN §9 accepts POST for query
   endpoints. Verify black-box that POST form is accepted upstream, else restrict to GET.
9. **Tempo streaming over gRPC** (`Search*`, `MetricsQueryRange` StreamingQuerier): Grafana can use
   it when enabled; not HTTP and out of scope for the proxy (PLAN). Document that streaming must
   be disabled on the Grafana Tempo data source.
10. **Grafana-called endpoints are largely unconfirmed from public docs** (see Limitation). A
    black-box capture of Grafana requests in the E2E harness should settle the "(unconfirmed)"
    entries before routes are frozen (T1.8).

## Black-box results (T1.8)

Script: `hack/research/loki-endpoints/run.sh` (exact curl commands; captured output in
`results.txt`). Loki `grafana/loki:3.5.0` (`/loki/api/v1/status/buildinfo`: version 3.5.0,
revision 4b16bc4f), single binary, `auth_enabled: true`, two streams pushed
(`{app="a",env="prod"}`, `{app="b",team="x"}`). Observed over HTTP only.

**Q1 `detected_labels` (Loki 3.5.0):**
- Exists. `GET` and `POST` both return 200 (POST via form body or URL query).
- Shape: `{"detectedLabels":[{"label":"app","cardinality":1}, ...]}`; array order is not stable
  between calls (merge/compare as a set). Includes the derived `service_name` label; structured
  metadata / stream labels only. No matches: `{}` (not an empty array).
- Honours `query` (a stream selector): `{app="a"}` -> app, env, service_name (cardinality 1);
  `{app="b"}` -> app, service_name, team; without `query` the union (app/service_name cardinality 2).
  Invalid selector: 400 with a plain-text parse error. `start`/`end` (ns) and `since` work.
- Unknown parameters (`foo=bar`) are silently ignored; a duplicated `query` uses the first value.
- Decision: route is `enforce` on `query`, methods GET+POST, allowed params `start,end,since`.
  The response carries only label names and cardinalities, merged across tenants by label name
  (cardinality across tenants is not exactly summable; the merge must treat it as an upper bound
  or recompute, decide in the merge task).

**Q8 POST on `query` / `query_range` (Loki 3.5.0):**
- `POST` with `application/x-www-form-urlencoded` is accepted on both and returns the same
  results as `GET` (`streams` for `query_range`, `vector` for a metric `query`). Log queries on
  `/query` are 400 for both GET and POST ("log queries are not supported as an instant query").
- POST with all parameters in the URL query and an empty body also works.
- **Precedence when both URL and form carry `query`: the form value wins** (URL `app=a` +
  form `app=b` returned app b; same on `detected_labels`). Repeated `query` values (URL or form):
  the first one is used. Consequently the proxy must not let a request carry `query` in both
  places (T1.9 already answers 400) nor repeated `query`, otherwise the enforced and the executed
  query could differ.
- JSON body (`application/json`) is not parsed: 400 (parse error, query seen as empty).
  The proxy should rewrite to a form body and set the content type itself.
- Decision: `query` and `query_range` allow GET and POST.

## Response shapes for the merges (T3.4-T3.6)

Captured from `grafana/loki:3.5.0` with the config of `hack/research/loki-endpoints/loki.yaml`
(`auth_enabled: true`, one tenant, streams `{app="a",env="prod"}` and `{app="b",team="x"}`),
HTTP only. These are what `internal/fanout` merges.

- `query_range`, log query: `{"status":"success","data":{"resultType":"streams","result":[{"stream":{...},"values":[["<ns>","<line>"]]}],"stats":{summary,querier,ingester,cache,index}}}`.
  `direction=backward` (default) lists streams and entries newest first, `forward` oldest first.
  Streams are separate per label set (Loki adds `detected_level` and `service_name`). No match:
  `"result":[]` (stats still present). With header `X-Loki-Response-Encoding-Flags: categorize-labels`
  the data object gains `"encodingFlags":["categorize-labels"]` and each entry gets a third element
  `{"structuredMetadata":{...}}` (and `detected_level` leaves the stream labels).
  `stats.summary` holds counters plus rates (`bytesProcessedPerSecond`, ...) and `execTime`.
- `labels`: `{"status":"success","data":["app","env"]}`; **no match: `{"status":"success"}` without `data`**.
  `label/<n>/values` has the same shape.
- `series`: `{"status":"success","data":[{"app":"a",...}]}`; no match: `"data":[]`. Without `match[]` all series are returned.
- `detected_labels`: `{"detectedLabels":[{"label","cardinality"}]}`; no match: `{}` (see Q1).
- `detected_fields`: `{"fields":[{"label","type","cardinality","parsers":["logfmt"]|null}],"limit":1000}`;
  no match: `{}`. **`query` is required: without it Loki answers 400** (`parse error : syntax error: unexpected $end`).
- `detected_field/<n>/values`: `{"limit":1000,"values":["hello"]}`; no match: `{}`.
- `index/stats`: flat `{"streams":N,"chunks":N,"bytes":N,"entries":N}` (zeros when nothing matches).
- `index/volume`: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"app":"a"},"value":[<sec>,"<bytes>"]}],"stats":{...}}}`.
  Without `targetLabels` the metric is the full series label set (so denied labels would be visible);
  with `aggregateBy=labels&targetLabels=app` it is `{"app":"a"}` per value.
  `index/volume_range` answers the documented `matrix` (`values:[[ts,"n"],...]`) only when the data of
  the selector spans several steps. When it all falls into one step (always for a step larger than the
  data span) the answer is a `vector` with one sample per series at that step, and when nothing matches
  an empty `vector` (Loki 3.5.0, observed in the e2e suite). The fan-out branches of one request can
  therefore differ in shape; the merge treats a vector branch as single-point matrix series.
- `patterns`: answers 404 unless the pattern ingester is enabled (`pattern_ingester.enabled: true`). With it
  enabled and little data, `{"status":"success","data":[]}` was observed; the non-empty shape
  `data:[{"pattern":"...","samples":[[<sec>,<count>],...]}]` is from the public docs and **unverified**.
