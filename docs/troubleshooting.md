# Troubleshooting

Start from the trace ID. Every response carries `X-Trace-Id`, and every error body has the same
value in `traceId`:

```json
{"status":"error","errorType":"forbidden","error":"label customer_id denied by policy (matcher)","traceId":"4bf92f3577b34da6a3ce929d0e0e4736"}
```

The trace (if sampled and exported, see [operations.md](operations.md#tracing)) shows the decision
on the server span, and the request log line carries the same `trace_id`.

## Decision reason codes

Every proxied request ends in exactly one `decision` (`allow`, `deny`, `error`) and one `reason`.
They are recorded as the span attributes `querdian.decision` and `querdian.decision.reason`, as
the labels `decision` and `reason` of `querdian_requests_total`, and in the log line. The table
lists every reason the server can set.

| Reason | Decision | HTTP | errorType | Meaning | What to do |
|---|---|---|---|---|---|
| `allowed` | allow | any | | The request passed all checks and was proxied. The HTTP status is the backend's (a backend `4xx` on a single streamed request is still `allowed`). | Nothing; look at `querdian_upstream_requests_total{code}` for backend errors. |
| `endpoint_not_allowed` | deny | 403 | `forbidden` | The path is outside every backend prefix, not in the endpoint table, or the method is not allowed on that route. | Check the data source URL and `pathPrefix`; see [endpoints.md](endpoints.md) for the allowed paths and methods. Denied paths stay denied by design. |
| `bad_path` | deny | 400 | `bad_data` | The path has an escaped slash, `%00`, backslash, `.`/`..`, empty segment, trailing slash, control character, or lies outside the prefix. | Fix the client URL; do not percent-encode slashes. |
| `unauthenticated` | deny | 401 | `unauthorized` | Missing, malformed, expired or wrongly issued token; wrong issuer or audience; no username claim; unusable groups claim. The message says which. | Check `querdian_auth_failures_total{reason}`, the `auth.issuer`/`audiences`/`claims` configuration and the token (`iss`, `aud`, `exp`, claims). |
| `auth_unavailable` | error | 503 | `unavailable` | OIDC issuer discovery has not completed yet. | Check connectivity to the issuer and `caFile`; the pod is not ready until discovery succeeds. |
| `snapshot_not_ready` | error | 503 | `unavailable` | No policy snapshot exists yet (startup, or the Kubernetes caches have not synced). | Wait; if persistent, check the pod's access to the Kubernetes API (RBAC) and its logs. |
| `internal` | error | 500 | `internal` | The authentication middleware passed a request without an identity. Should not happen. | Report a bug with the trace ID. |
| `no_grant` | deny | 403 | `forbidden` | No active rule grants this user (by username or any group) access to this backend. | Check the user's groups in the token, the policy's `subjects`, the backend name in the rules, and that the policy is `Ready` (`kubectl get cap,ap`). A policy in `TenantNotAssigned` or any other failing status grants nothing. |
| `policy_too_complex` | deny | 403 | `forbidden` | The user's merged policy has more than `limits.maxPolicyTerms` terms. | Simplify the policies (fewer, broader rules or rules that differ in one label so they merge), or raise the limit knowingly. |
| `cross_tenant_union_unsupported` | deny | 403 | `forbidden` | The user has rules in several tenants with differing terms on a backend with `tenantFederation: none`. | Give the user identical rules in each tenant, use one tenant, or use a backend that supports `__tenant_id__` filtering. See the `CrossTenantUnionUnsupported` status. |
| `resolve_failed` | error | 500 | `internal` | Policy resolution failed unexpectedly (for example the backend is unknown to the snapshot). | Report a bug with the trace ID; check that the config of all replicas matches. |
| `trace_by_id_denied` | deny | 403 | `forbidden` | A trace-by-ID request on a Tempo backend with `traceByID: deny` (the default). | Set `traceByID: filter` on the backend if trace lookup by ID is wanted. |
| `bad_request` | deny | 400 | `bad_data` | The request does not satisfy the normalisation rules: method not allowed, POST without a form content type, body over 1 MiB, an enforcement parameter given more than once, or a parameter that is not on the route's allow-list. The message names it. | Fix the client; remove unsupported parameters (for example `storeMatch[]`, Tempo `tags`). |
| `query_too_complex` | deny | 403 | `forbidden` | The plan needs more upstream requests than `limits.maxFanout`, or the query would expand to too many selector copies (more than 2048 for PromQL or-lifting, more than 256 for Prometheus `match[]`). | Narrow the query, simplify the policy, or raise `maxFanout` (this multiplies backend load). |
| `enforcer_unavailable` | error | 503 | `unavailable` | No enforcer is registered for the backend type, or tailing is not available. Restricted requests fail closed. | Report a bug; this indicates a build or wiring problem. |
| `denied_label` | deny | 403 | `forbidden` | The query, `match[]`, path (`label/<name>/values`, tag values) or `targetLabels` references a label or attribute denied by the user's policy, or (LogQL) a template that could read it. The message names the label. | Remove the reference from the query or dashboard variable, or change `labels.deny`. Remember that one matched rule's denied labels apply to the whole request. |
| `parse_error` | deny | 400 | `bad_data` | The query (or `match[]`) did not parse. Querdian's parsers are stricter than the backends', see [limitations.md](limitations.md). The message carries the position. | Fix the query; if valid for the backend, check the strictness list. |
| `unsupported` | deny | 400 | `bad_data` | The query is valid but cannot be rewritten exactly for the user's policy (for example a PromQL range vector that cannot be or-lifted, a LogQL aggregation with `by` over several terms, a metric query where a selector is required, Tempo legacy parameters, tenant-bound terms on Tempo), or the rewritten query did not re-parse. | See the message and [limitations.md](limitations.md). Rewrite the query or simplify the user's policy to one term. |
| `enforcement_error` | error | 500 | `internal` | An untyped, unexpected error in an enforcer. Fails closed. | Report a bug with the trace ID. |
| `trace_filter_failed` | error | 500 | `internal` | The trace-by-ID filter could not be built from the user's policy (for example a tenant-bound term on Tempo or an unscoped attribute name). | Check the policy attribute names (`resource.x`, `span.x`) and tenant setup. |
| `upstream_error` | error | 5xx (normally 502) | `upstream` or `merge_too_large` | The request was allowed but the response status was 5xx: the backend failed, a fan-out branch failed, the timeout elapsed, the buffered merge exceeded `limits.maxMergeBytes`, a filter could not process the body, or a WebSocket tail could not connect. | See "Upstream errors" below. |

Authentication failure sub-reasons (`querdian_auth_failures_total{reason}`; the HTTP response is
`401` for all, `decision=deny reason=unauthenticated`): `missing_token`, `invalid_token`
(malformed, bad signature, bad `Authorization` scheme, invalid groups claim), `expired`,
`audience`, `issuer`, `missing_username`.

Policy compile status reasons (`Compiled`, `InvalidMatcher`, `UnknownBackend`, `TenantRequired`,
`TenantNotAssigned`, `CrossTenantUnionUnsupported`) are on the policy objects, not on requests; see
[policy-guide.md](policy-guide.md#status-conditions).

## Typical problems

* **"403 no policy grants access" for a user who should have access.** Look at `policy.resolve` in
  the trace (`policies.matched`), then at the token's groups (a group claim in a nested path needs
  `auth.claims.groups: realm_access.roles`). Then `kubectl get cap,ap -A`: a policy that is not
  `Ready` contributes nothing. Remember that users and groups are matched exactly and separately.
* **A team sees no data although its `AccessPolicy` is Ready.** The namespace label is injected as
  `<namespaceLabel>="<namespace>"`. If the backend's data does not carry exactly that label (wrong
  `namespaceLabel`, label missing at ingestion), nothing matches. Run the same selector directly
  against the backend.
* **Logs or traces of one tenant are missing.** Check the tenant label on the namespace and the
  tenant header that Querdian sets (visible in the backend's logs).
* **A dashboard variable fails with `denied_label`.** The variable query references a denied label
  (a `label_values(customer_id)` call or a `by (customer_id)` aggregation).
* **A dashboard works for admins but not for teams** (`unsupported`). Teams with several terms hit
  the or-lifting limits ([limitations.md](limitations.md)); check `dnf.terms` in `policy.resolve`.
* **The trace table in Grafana shows no root name/service for restricted users.** This is the
  Tempo search redaction, not a bug.
* **Hot policy change not visible yet.** Allow about a second, then check
  `querdian_snapshot_rebuilds_total{result="error"}` and the `snapshot.rebuild` spans.

## Metrics queries (PromQL)

Denials per reason (`deny` and `error`):

```promql
sum by (decision, reason) (rate(querdian_requests_total{decision!="allow"}[5m]))
```

Share of requests that were denied or failed, per backend:

```promql
sum by (backend) (rate(querdian_requests_total{decision!="allow"}[5m]))
  / sum by (backend) (rate(querdian_requests_total[5m]))
```

Where the denials happen (by endpoint; users are deliberately not a metric label):

```promql
topk(10, sum by (backend, endpoint, reason) (increase(querdian_requests_total{decision="deny"}[1h])))
```

Any request that failed closed unexpectedly (should be zero):

```promql
sum by (reason) (increase(querdian_requests_total{reason=~"internal|resolve_failed|enforcement_error|trace_filter_failed|enforcer_unavailable"}[15m])) > 0
```

Denied-label rejections and unsupported queries (the two reasons users complain about most):

```promql
sum by (backend, endpoint) (rate(querdian_requests_total{reason=~"denied_label|unsupported|parse_error"}[5m]))
```

Authentication failures:

```promql
sum by (reason) (rate(querdian_auth_failures_total[5m]))
```

Latency (classic buckets; with native histograms use
`histogram_quantile(0.99, sum by (backend) (rate(querdian_request_duration_seconds[5m])))`):

```promql
histogram_quantile(0.99, sum by (le, backend, endpoint) (rate(querdian_request_duration_seconds_bucket{decision="allow"}[5m])))
```

Slow fan-outs and heavy policies. Fan-out cost shows as terms per request and as the ratio of
upstream to client requests:

```promql
histogram_quantile(0.95, sum by (le, backend) (rate(querdian_policy_terms_bucket[15m])))

sum by (backend) (rate(querdian_upstream_requests_total[5m]))
  / sum by (backend) (rate(querdian_requests_total{decision="allow"}[5m]))

histogram_quantile(0.99, sum by (le, backend) (rate(querdian_upstream_duration_seconds_bucket[5m])))
```

If the ratio of upstream to client requests is far above 1 on Loki, a few users have many terms;
look at `dnf.terms` in traces. Time spent in Querdian itself:

```promql
histogram_quantile(0.99, sum by (le, language, strategy) (rate(querdian_rewrite_duration_seconds_bucket[5m])))
```

Upstream errors (`code` is `error` for transport failures such as refused connections or timeouts):

```promql
sum by (backend, code) (rate(querdian_upstream_requests_total{code!~"2.."}[5m]))
```

Policy and snapshot health:

```promql
querdian_policies{ready="false"} > 0
increase(querdian_snapshot_rebuilds_total{result="error"}[15m]) > 0
```

`querdian_snapshot_generation` is a per-replica counter and is not comparable between replicas.

## Trace queries (TraceQL)

Querdian's own spans carry the attributes below (use the data source that stores Querdian's
traces; `resource.service.name` defaults to `querdian`). All decision attributes are on the
**server span**.

All denials:

```traceql
{ resource.service.name = "querdian" && span.querdian.decision = "deny" }
```

One reason:

```traceql
{ resource.service.name = "querdian" && span.querdian.decision.reason = "denied_label" }
{ resource.service.name = "querdian" && span.querdian.decision.reason = "no_grant" }
{ resource.service.name = "querdian" && span.querdian.decision.reason = "unsupported" }
{ resource.service.name = "querdian" && span.querdian.decision.reason =~ "policy_too_complex|query_too_complex|cross_tenant_union_unsupported" }
```

Errors (`decision = error`, server span status is also `error`):

```traceql
{ resource.service.name = "querdian" && span.querdian.decision = "error" }
{ resource.service.name = "querdian" && status = error && span.querdian.decision.reason = "upstream_error" }
```

Denials by reason as a rate (requires TraceQL metrics on your Tempo):

```traceql
{ resource.service.name = "querdian" && span.querdian.decision = "deny" } | rate() by (span.querdian.decision.reason)
```

A user's trace is found from the `X-Trace-Id` they report: open that trace in Grafana. With
`--tracing.record-user` you can also filter `{ span.enduser.id = "alice@example.com" }`; the
policies matched are in `span.querdian.policies`.

Slow fan-outs and merges (merge attributes are on the server span):

```traceql
{ resource.service.name = "querdian" && span.merge.kind != nil && duration > 5s }
{ resource.service.name = "querdian" && span.strategy = "fanout" } && { span.querdian.decision = "allow" && duration > 5s }
{ resource.service.name = "querdian" && span.merge.approximate = true }
```

Slow or failing upstream calls (one `upstream` client span per backend request; the status
attribute name depends on the instrumentation version, so check one span first):

```traceql
{ resource.service.name = "querdian" && name = "upstream" && duration > 5s }
{ resource.service.name = "querdian" && name = "upstream" && status = error }
{ resource.service.name = "querdian" && name = "upstream" && span.http.response.status_code >= 500 }
```

How much time went into rewriting versus waiting for the backend: compare the duration of
`enforce.rewrite` and `upstream` children in the trace of a slow request.

Snapshot rebuilds (controller root spans):

```traceql
{ resource.service.name = "querdian" && name = "snapshot.rebuild" && span.policies.invalid > 0 }
```

## Upstream errors

`upstream_error` means a 5xx response. Check, in order:

1. **Which kind?** In the error body: `errorType: "merge_too_large"` means the buffered responses
   of a fan-out (or the filter buffer) exceeded `limits.maxMergeBytes` or the fixed 16 MiB of the
   Prometheus labels/series filter; narrow the query time range or selector.
   `errorType: "upstream"` with a message `upstream request N failed: status 500: ...` names the
   failing branch and shows the first 256 bytes of the backend's error. `upstream request failed`
   without detail is a transport error or timeout.
2. **Timeout?** Requests are cut after `limits.upstreamTimeout` (default 5 minutes). Compare with
   the `upstream` span duration and `querdian_upstream_duration_seconds`.
3. **Which backend and code?** `querdian_upstream_requests_total{code}` (the PromQL above).
   A `502` from a fan-out is raised even if only one of many branches fails.
4. **Backend-side limits.** Loki `max_query_series`, Tempo `max_search_duration`, query
   concurrency and per-tenant limits apply per upstream request; fan-out multiplies them. Reduce
   the number of policy terms for heavy users.
5. **Tail connections.** A tail that cannot connect to one of its upstream sockets fails before
   the upgrade with `502 upstream websocket failed`; an upstream that closes afterwards closes the
   client with a reason string.
6. **Trace by ID returns 502.** The upstream answered with an unsupported content type (only JSON
   and protobuf are filtered) or a body that cannot be decoded; or the trace is larger than
   `maxMergeBytes`.
7. **Cross-tenant requests fail upstream with 4xx/5xx.** The backend must have federation enabled
   for `t1|t2` tenant headers; see [policy-guide.md](policy-guide.md#tenants).

## Checks for operators

```sh
kubectl get cap,ap -A                      # Ready and Reason of every policy
kubectl get ns --show-labels | grep policy.querdian.dev/tenant
curl -s localhost:9090/readyz              # snapshot and issuer discovery
curl -s localhost:9090/metrics | grep '^querdian_policies'
```
