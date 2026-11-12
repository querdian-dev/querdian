# Operations reference

This page lists what the operator sees when Querdian runs: the result of each limit, the metrics and the
spans. For the settings, read [operations.md](operations.md) first.

In short:

* Most limits make the request fail. The table gives the status and the `reason` for each limit.
* Each request increments `querdian_requests_total` with a `decision` and a `reason`.
* Each response has an `X-Trace-Id` header. Use it to find the spans of the request.

## Limits and what happens when they trip

The configurable limits are in [operations.md](operations.md#limits).

| Limit | Value | Trips when | Client sees | `decision` / `reason` |
|---|---|---|---|---|
| `limits.maxPolicyTerms` | 16 | the simplified policy of the user has more terms | `403 forbidden` | deny / `policy_too_complex` |
| policy input (fixed) | 4096 terms, 32768 matchers, 1 MiB of matcher text, a fixed budget of simplification steps, a weight of 32768 after the simplification | the matched rules of the request are larger before the simplification, the simplification needs more steps, or the simplified policy is larger | `403 forbidden` | deny / `policy_too_complex` |
| `limits.maxRequestRegexWeight` | weight 131072 | the request regex weight (see [operations.md](operations.md#limits); policy weight: [policy-guide.md](policy-guide.md#size-limits)) is larger | `403 forbidden` | deny / `query_too_complex` |
| client regex bytes (fixed) | 8 KiB for each regex, 32 KiB for all regexes of a query | a client regex is longer | `403 forbidden` | deny / `query_too_complex` |
| `limits.maxQueryRegexWeight` | weight 32768 | the regexes of a PromQL or LogQL query (all `match[]` values together) are larger | `403 forbidden` | deny / `query_too_complex` |
| policy size (fixed) | see [policy-guide.md](policy-guide.md#size-limits) | a policy has too many rules, matchers or subjects, or too long strings | the CRD rejects it; a stored object grants nothing | status reason `PolicyTooLarge` |
| `limits.maxFanout` | 16 | the plan needs more upstream requests (terms for a fan-out; it also guards a plan without requests) | `403 forbidden` | deny / `query_too_complex` |
| `limits.maxMergeBytes` | 64 MiB | the buffered bodies of the fan-out are larger; also the trace-by-ID filter body and the Tempo search body | `502`, errorType `merge_too_large` (fan-out) or `upstream` | error / `upstream_error` |
| `limits.maxInflightMergeBytes` | 64 MiB | the buffered fan-out bodies of all concurrent requests of the replica are larger (the oldest fan-out that still reads waits instead) | `503 unavailable` with `Retry-After` | error / `merge_budget_exhausted` |
| `limits.upstreamTimeout` | 5m | the proxied request runs longer | `502 upstream` (or a truncated stream if the headers were sent) | error / `upstream_error` |
| selector copies (fixed) | 2048 | PromQL or-lift: selectors x terms | `403 forbidden` | deny / `query_too_complex` |
| `match[]` expansion (fixed) | 256 | Prometheus metadata: user selectors x terms | `403 forbidden` | deny / `query_too_complex` |
| form body (fixed) | 1 MiB | the POST body is larger | `400 bad_data` | deny / `bad_request` |
| labels/series response filter (fixed) | 16 MiB | the Prometheus `labels`/`series` body when Querdian filters denied labels | `502` | error / `upstream_error` |
| tail frame (fixed) | 4 MiB | a single upstream tail frame | the tail closes with an error | n/a (after upgrade) |
| tail dedupe memory (fixed) | 10 000 entries | Querdian forgets the oldest entries; then duplicates are possible | n/a | n/a |
| parser nesting (fixed) | 100 levels | LogQL or TraceQL nesting is deeper | `400 bad_data` | deny / `parse_error` |

If a branch of a fan-out fails upstream, the whole request fails with `502`, also when only one branch
fails. The message has the
upstream status and the first 256 bytes of its error body. If the policy of the caller redacts labels,
Querdian withholds the body (see the [threat model](security/threat-model.md#3-header-and-request-handling)).

## Metrics

Served on the management port. The label `endpoint` is the route template (for example
`/api/v1/label/:name/values`), never the raw path. Unknown paths use `unknown`.

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `querdian_requests_total` | counter | `backend`, `endpoint`, `decision`, `reason` | `decision` is `allow`, `deny` or `error`. The reasons are in [troubleshooting.md](troubleshooting.md#decision-reason-codes). |
| `querdian_request_duration_seconds` | histogram (native and classic, exemplars) | `backend`, `endpoint`, `decision` | Exemplar `trace_id` for sampled requests. Scrape with exemplar support (OpenMetrics). |
| `querdian_upstream_requests_total` | counter | `backend`, `code` | `code` is the HTTP status, or `error` for transport failures. One increment for each upstream request, so a fan-out counts more than one. |
| `querdian_upstream_duration_seconds` | histogram (native and classic, exemplars) | `backend` | Time until the response headers. |
| `querdian_rewrite_duration_seconds` | histogram | `language` (`promql`, `logql`, `traceql`), `strategy` | Only when a query was rewritten. |
| `querdian_policy_terms` | histogram | `backend` | DNF terms for each resolved request (`0` for unrestricted). |
| `querdian_auth_failures_total` | counter | `reason` | `missing_token`, `invalid_token`, `expired`, `audience`, `issuer`, `missing_username`. |
| `querdian_snapshot_generation` | gauge | | Counts the rebuilds **of this replica**. Do not compare it across replicas. |
| `querdian_snapshot_rebuilds_total` | counter | `result` | `ok` or `error`. |
| `querdian_policies` | gauge | `kind`, `ready` | Policies by kind and compile result. `kind` is `ClusterAccessPolicy`, `AccessPolicy`, and with the `multicluster` block also `SpokePolicy`. Each replica exports it. |
| `querdian_cluster_policy_age_seconds` | gauge | `cluster` | Multi-cluster: time since the last valid policy sync of a spoke (`+Inf` before the first). |
| `querdian_cluster_policy_expired` | gauge | `cluster` | Multi-cluster: `1` if the policies of a spoke are expired or suspended, else `0`. |

Querdian also exports the Go and process collectors.

## Spans

Span tree of one proxied request. For the tracing configuration and the privacy flags, see
[operations.md](operations.md#tracing).

| Span | Attributes |
|---|---|
| server span `HTTP <method> <route template>` | `http.route`, `querdian.decision` (`allow`, `deny`, `error`), `querdian.decision.reason`, `querdian.policies` (policy names), merge attributes `merge.kind` (`loki_streams`, `string_set`, `sum_counts`), `merge.items_in`, `merge.items_out`, `merge.deduped`, `merge.truncated` (log streams), `merge.approximate` (sums); status error when `decision` is `error` |
| `authn.verify` | status error when the authentication failed |
| `policy.resolve` | `snapshot.generation`, `policies.matched`, `dnf.terms`, `dnf.unrestricted`, `dnf.disjoint`, `tenants.count` |
| `enforce.rewrite` | `query.language`, `strategy` (`inject`, `or_lift`, `fanout`, `match_expand`, `passthrough`), `query.bytes_in`, `query.bytes_out` |
| `upstream` (client span, one for each upstream request) | standard HTTP client attributes; `url.full` has the scheme, host and path only, without the query string (see the privacy flags) |
| `snapshot.rebuild` (root span, controller) | `querdian.trigger` (`startup`, `event`), `snapshot.generation`, `policies.total`, `policies.invalid`, `compile.duration_ms` |
