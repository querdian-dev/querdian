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
| `query` | GET (docs); POST per PLAN §9 to be verified | `query` | no | `streams`\|`vector` | enforce | yes |
| `query_range` | GET (docs) | `query` | no | `streams`\|`matrix` | enforce | yes |
| `labels` | GET | `query` (optional selector) | no | `[name]` | enforce (fan-out string set) | yes |
| `label/<name>/values` | GET | `query` (optional selector) | no | `[value]` | enforce (fan-out string set) | yes |
| `series` | GET, POST | `match[]` (required) | yes | `[labelset]` | enforce (match expand) | yes |
| `index/stats` | GET, POST | `query` | no | `{streams,chunks,entries,bytes}` | enforce (fan-out sum) | yes (unconfirmed) |
| `index/volume` | GET, POST | `query`; also `targetLabels`, `aggregateBy` | no | vector | enforce (fan-out sum) | yes (unconfirmed) |
| `index/volume_range` | GET, POST | `query`; also `step` | no | matrix | enforce (fan-out sum) | yes (unconfirmed) |
| `patterns` | GET | `query` | no | `[{pattern, samples}]` | enforce (fan-out sum) | yes (unconfirmed) |
| `detected_fields` | GET, POST | `query` | no | `{fields:[{label,type,cardinality,parsers,...}], limit}` | enforce (fan-out string set) | yes (unconfirmed) |
| `detected_field/<name>/values` | GET, POST | `query` | no | `{values, limit}` | enforce (fan-out string set) | yes (unconfirmed) |
| `detected_labels` | ? | `query` | ? | ? | enforce (PLAN) | yes (unconfirmed) - NOT in docs, see Deltas |
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

1. **`question(t0.3)`: Loki `detected_labels`.** PLAN §8.2 lists it, but the Loki HTTP API page
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
8. **Loki `query`/`query_range` methods**: docs show GET only; PLAN §9 accepts POST for query
   endpoints. Verify black-box that POST form is accepted upstream, else restrict to GET.
9. **Tempo streaming over gRPC** (`Search*`, `MetricsQueryRange` StreamingQuerier): Grafana can use
   it when enabled; not HTTP and out of scope for the proxy (PLAN). Document that streaming must
   be disabled on the Grafana Tempo data source.
10. **Grafana-called endpoints are largely unconfirmed from public docs** (see Limitation). A
    black-box capture of Grafana requests in the E2E harness should settle the "(unconfirmed)"
    entries before routes are frozen (T1.8).
