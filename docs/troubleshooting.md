# Troubleshooting

Use this page to find why Querdian rejected a request, and how to correct the cause.

In short:

* Each response has the header `X-Trace-Id`. Each error body has the same value in `traceId`.
* Each proxied request ends with one `decision` (`allow`, `deny` or `error`) and one `reason`.
* The request log line, the server span (`querdian.decision`, `querdian.decision.reason`) and
  `querdian_requests_total{decision,reason}` record them.
* Querdian fails closed: if it cannot enforce a request, it rejects the request.

## Find the reason of a failure

1. Get the trace ID from the header `X-Trace-Id`, or from `traceId` in the error body:

   ```json
   {"status":"error","errorType":"forbidden","error":"label customer_id denied by policy (matcher)","traceId":"4bf92f3577b34da6a3ce929d0e0e4736"}
   ```

2. Read `errorType` and `error` in the body.
3. Find the log line with the same `trace_id`. It has the `decision` and the `reason`.
4. If the trace is sampled and exported, open it (see [operations.md](operations.md#tracing)).
   The decision is on the server span.
5. Find the reason in the [table](#decision-reason-codes) and do the correction.
6. If the reason is `upstream_error`, do the [upstream checks](#upstream-errors).

## Decision reason codes

The table gives each reason that the server can set.

| Reason | Decision | HTTP | errorType | Meaning | Correction |
|---|---|---|---|---|---|
| `allowed` | allow | any | | The request passed all checks and was proxied. The status is the backend status. A backend `4xx` on a single streamed request is also `allowed`. | None. For backend errors, see `querdian_upstream_requests_total{code}`. |
| `endpoint_not_allowed` | deny | 403 | `forbidden` | The path is outside each backend prefix or not in the endpoint table, or the route does not permit the method. | Check the data source URL and `pathPrefix`. See [endpoints.md](endpoints.md) for the permitted paths and methods. Denied paths stay denied by design. |
| `bad_path` | deny | 400 | `bad_data` | The path has an escaped slash, `%00`, a backslash, `.` or `..`, an empty segment, a trailing slash or a control character, or is outside the prefix. | Correct the client URL. Do not percent-encode slashes. |
| `unauthenticated` | deny | 401 | `unauthorized` | Token missing, malformed, expired or wrongly issued; wrong issuer or audience; no username claim; groups claim not usable. The message gives the cause. | Check `querdian_auth_failures_total{reason}` ([reasons](#authentication-failure-reasons)), `auth.issuer`/`audiences`/`claims`, and the token (`iss`, `aud`, `exp`, claims). |
| `auth_unavailable` | error | 503 | `unavailable` | OIDC issuer discovery is not complete. | For the cause, find `issuer discovery failed` in the pod log. Check the connection to the issuer and `caFile`. The pod is not ready until discovery is successful. |
| `snapshot_not_ready` | error | 503 | `unavailable` | No policy snapshot exists (startup, or the Kubernetes caches did not sync). | Wait. If the error continues, check the pod log and the access of the pod to the Kubernetes API (RBAC). |
| `internal` | error | 500 | `internal` | The authentication middleware passed a request without an identity. This must not occur. | Report a bug with the trace ID. |
| `no_grant` | deny | 403 | `forbidden` | No active rule gives this user (by username or by a group) access to this backend. | Look at `policies.matched` on the `policy.resolve` span. Check the groups in the token (a nested group claim needs, for example, `auth.claims.groups: realm_access.roles`), the `subjects` of the policy, the backend name in the rules, and that the policy is `Ready` (`kubectl get cap,ap -A`). A policy in `TenantNotAssigned` or a different failed status gives nothing. Users and groups are matched exactly and separately. |
| `policy_source_expired` | deny | 403 | `forbidden` | Multi-cluster only. Rules give access to the backend, but each rule that gives a term comes from an expired or suspended spoke (no valid sync in `maxStaleness`, `suspended: true`, `SpokeCluster` changed or deleted). The message does not name the cluster. | Check `kubectl get spokeclusters` (`LastSync`, `Suspended`), `querdian_cluster_policy_expired`, the agent in the spoke and the receiver logs. The next valid sync gives the access again. See [multicluster.md](multicluster.md#time-limit-and-outages). |
| `policy_too_complex` | deny | 403 | `forbidden` | The merged policy of the user has more than `limits.maxPolicyTerms` terms. Or the matched rules are over a fixed limit for one request: before the simplification, more than 4096 terms, 32768 matchers or 1 MiB of matcher text; too many simplification steps; after the simplification, a [weight](policy-guide.md#size-limits) of more than 32768. | Make the policies simpler (fewer, broader rules, or rules that differ in one label so that they merge). Or increase the limit, if you accept the effect. |
| `cross_tenant_union_unsupported` | deny | 403 | `forbidden` | The user has rules with different terms in more than one tenant, on a backend with `tenantFederation: none`. | Give the user the same rules in each tenant, use one tenant, or use a backend that supports `__tenant_id__` filters. See the status `CrossTenantUnionUnsupported`. |
| `resolve_failed` | error | 500 | `internal` | The policy resolution failed unexpectedly (for example, the snapshot does not know the backend). | Report a bug with the trace ID. Make sure that all replicas have the same configuration. |
| `trace_by_id_denied` | deny | 403 | `forbidden` | A trace-by-ID request on a Tempo backend with `traceByID: deny` (the default). | To permit trace lookup by ID, set `traceByID: filter` on the backend. |
| `bad_request` | deny | 400 | `bad_data` | The request does not obey the normalisation rules: a method that is not permitted; a POST without a form content type; a body of more than 1 MiB; more than one value for a parameter whose name does not end in `[]`; a parameter that is not on the allow-list of the route. The message names the cause. | Correct the client. Remove parameters that are not supported (for example `storeMatch[]`, Tempo `tags`). |
| `query_too_complex` | deny | 403 | `forbidden` | (1) The plan needs more upstream requests than `limits.maxFanout`. (2) The query expands to too many selector copies: more than 2048 for PromQL or-lifting, more than 256 for Prometheus `match[]`. (3) The regexes that the request compiles have a [weight](policy-guide.md#size-limits) of more than `limits.maxRequestRegexWeight` (default 131072; (2 x terms + 1) x client regex weight + (2 x selectors + 1) x policy weight). (4) The regexes of the query have a weight of more than `limits.maxQueryRegexWeight` (default 32768). The message gives the cause. | (1) Make the query narrower, make the policy simpler, or increase `maxFanout` (this multiplies the backend load). (3) Divide the query into queries with fewer selectors, use fewer or shorter regexes, or make the policy smaller. (4) Use shorter regexes, fewer Unicode classes, no case folding on classes, or fewer counted repetitions. |
| `enforcer_unavailable` | error | 503 | `unavailable` | No enforcer is registered for the backend type, or tail is not available. Restricted requests fail closed. | Report a bug. This shows a build or wiring problem. |
| `denied_label` | deny | 403 | `forbidden` | The query, `match[]`, the path (`label/<name>/values`, tag values) or `targetLabels` refers to a label or attribute that the policy of the user denies, or (LogQL) a template could read it. The message names the label. For a LogQL template, it shows `(template reading all labels)` (a node or function outside the allow-list) or `(template that does not parse)` (not a valid Go template). | Remove the reference from the query or the dashboard variable (for example `label_values(customer_id)` or `by (customer_id)`), or change `labels.deny`. The denied labels of one matched rule apply to the full request. |
| `parse_error` | deny | 400 | `bad_data` | The query (or `match[]`) did not parse. The parsers of Querdian are stricter than the parsers of the backends (see [limitations.md](limitations.md)). The message gives the position. | Correct the query. If the backend accepts it, check the list of strict rules. |
| `unsupported` | deny | 400 | `bad_data` | The query is valid, but Querdian cannot rewrite it exactly for the policy of the user. Examples: a PromQL range vector that cannot be or-lifted, a LogQL aggregation with `by` over more than one term, a metric query where a selector is necessary, Tempo legacy parameters, tenant-bound terms on Tempo. Or the rewritten query did not parse again. | Read the message and [limitations.md](limitations.md). Write the query again, or make the policy of the user simpler (one term). If a dashboard works for admins but not for teams, teams with more than one term can be over the or-lifting limits: check `dnf.terms` on the `policy.resolve` span. |
| `enforcement_error` | error | 500 | `internal` | An unexpected enforcer error without a type. The request fails closed. | Report a bug with the trace ID. |
| `trace_filter_failed` | error | 500 | `internal` | Querdian could not make the trace-by-ID filter from the policy of the user (for example, a tenant-bound term on Tempo, or an attribute name without a scope). | Check the attribute names in the policy (`resource.x`, `span.x`) and the tenant configuration. |
| `merge_budget_exhausted` | error | 503 | `unavailable` | The request was allowed and fanned out. But the buffered fan-out bodies of all concurrent requests of the replica reached `limits.maxInflightMergeBytes`, and an older fan-out had the first claim on the space. The request fails closed. The response has `Retry-After` (1 to 3 seconds). | Try again. If the error is frequent, decrease concurrent large fan-outs (narrower queries, fewer policy terms), add replicas, or increase `maxInflightMergeBytes` together with the memory limit of the pod (see [operations.md](operations.md#scaling)). |
| `write_not_bounded` | error | 500 | `internal` | The merged response of a fan-out was ready, but the response writer cannot set a write deadline (message `cannot bound the response write`). The request fails closed: without a deadline, a client that does not read could hold the memory budget. The listeners of Querdian support write deadlines, thus this shows a build or wiring problem. | Report a bug with the trace ID. |
| `upstream_error` | error | 5xx (usually 502) | `upstream` or `merge_too_large` | The request was allowed, but the response status was 5xx: the backend failed, a fan-out branch failed, the timeout elapsed, the buffered merge was more than `limits.maxMergeBytes`, a filter could not process the body, or a WebSocket tail could not connect. | See [Upstream errors](#upstream-errors). |

### Authentication failure reasons

`querdian_auth_failures_total{reason}` counts these reasons. For each one, the response is `401`, with
`decision=deny reason=unauthenticated`.

| Reason | Cause |
|---|---|
| `missing_token` | No token. |
| `invalid_token` | Malformed token, bad signature, bad `Authorization` scheme, or a groups claim that is not valid. |
| `expired` | Expired token. |
| `audience` | Wrong audience. |
| `issuer` | Wrong issuer. |
| `missing_username` | No username claim. |

### Policy status reasons

These reasons are on the policy objects, not on requests: `Compiled`, `InvalidMatcher`, `UnknownBackend`,
`TenantRequired`, `TenantNotAssigned`, `CrossTenantUnionUnsupported`, `PolicyTooLarge`, and in a
multi-cluster setup `InvalidClusterScope`, `TenantNotAllowed`, `MulticlusterDisabled`,
`SpokeClusterNotFound`, `SpokeClusterInvalid`, `InvalidSpokePolicy`. See
[policy-guide.md](policy-guide.md#status-conditions).

An expired spoke rule gives no terms, but its denied and redacted labels continue to apply. Thus its
policy is still in `policies.matched` and `querdian.policies`, and its denied labels can still cause
`403 denied_label`.

## Upstream errors

`upstream_error` means a 5xx response. Do these checks in sequence:

1. **Find the kind of error in the body.**
   * `errorType: "merge_too_large"`: the buffered responses of a fan-out (or the filter buffer) were
     more than `limits.maxMergeBytes`, or more than the fixed 16 MiB of the Prometheus labels/series
     filter. Make the time range or the selector narrower.
   * `errorType: "upstream"` with `upstream request N failed: status 500: ...`: the message names the
     failed branch and shows the first 256 bytes of the backend error. If the policy of the caller
     redacts labels, the message has `details withheld because the policy redacts label values`. Then
     find the backend error in the warning log of Querdian or in the backend.
   * `upstream request failed` without a detail is a transport error or a timeout.
2. **Timeout.** Querdian stops requests after `limits.upstreamTimeout` (default 5 minutes). Compare
   with the `upstream` span duration and `querdian_upstream_duration_seconds`.
3. **Backend and code.** See `querdian_upstream_requests_total{code}` ([query](#metrics-queries-promql)).
   A fan-out gives `502` also if only one of many branches fails.
4. **Limits of the backend.** Loki `max_query_series`, Tempo `max_search_duration`, query concurrency
   and tenant limits apply to each upstream request. A fan-out multiplies them. Decrease the policy terms
   of heavy users.
5. **Tail connections.** If a tail cannot connect to one of its upstream sockets, it fails before the
   upgrade with `502 upstream websocket failed`. If an upstream closes later, Querdian closes the client
   with a reason string.
6. **Trace by ID gives 502.** The upstream sent a content type that is not filtered (only JSON and
   protobuf are filtered) or a body that cannot be decoded, or the trace is larger than `maxMergeBytes`.
7. **Cross-tenant requests fail upstream with 4xx or 5xx.** The backend must have federation enabled for
   `t1|t2` tenant headers. See [policy-guide.md](policy-guide.md#tenants).

## Typical problems

| Symptom | Cause and correction |
|---|---|
| A team sees no data, but its `AccessPolicy` is Ready | Querdian injects the namespace label as `<namespaceLabel>="<namespace>"`. If the backend data does not have exactly that label (wrong `namespaceLabel`, label missing at ingestion), nothing matches. Send the same selector directly to the backend. |
| Logs or traces of one tenant are missing | Check the tenant label on the namespace, and the tenant header that Querdian sets (see the backend logs). |
| No root name or root service in the Grafana trace table for restricted users | This is the Tempo search redaction, not a bug. |
| A policy change is not visible yet | Wait about one second. Then check `querdian_snapshot_rebuilds_total{result="error"}` and the `snapshot.rebuild` spans. |
| Multi-cluster: a hub team sees no data, but its `AccessPolicy` is Ready | A hub `AccessPolicy` also matches `<clusterLabel>="<hubCluster>"`. Hub data without that label (old data, ruler output, a hub agent that does not set it) is not visible. See [multicluster.md](multicluster.md#2-make-sure-that-hub-telemetry-carries-the-hub-cluster-label). |
| Multi-cluster: a spoke policy is not active | On the hub, `kubectl get sp` shows the reason. At the next heartbeat, the agent copies it to the policy in the spoke. A spoke policy that shows `Compiled` but gives `policy_source_expired` belongs to an expired or suspended spoke. |
| Multi-cluster: the proxy is not ready | With the `multicluster` block, `/readyz` answers `spoke deadline table not ready` until the replica has read all `SpokeCluster` objects. Check the RBAC for `spokeclusters`, and that the CRDs are installed. |

## Metrics queries (PromQL)

Denials and errors for each reason:

```promql
sum by (decision, reason) (rate(querdian_requests_total{decision!="allow"}[5m]))
```

Part of the requests that were denied or failed, for each backend:

```promql
sum by (backend) (rate(querdian_requests_total{decision!="allow"}[5m]))
  / sum by (backend) (rate(querdian_requests_total[5m]))
```

Denials by endpoint (users are not a metric label, by design):

```promql
topk(10, sum by (backend, endpoint, reason) (increase(querdian_requests_total{decision="deny"}[1h])))
```

Unexpected fail-closed errors (the result must be empty):

```promql
sum by (reason) (increase(querdian_requests_total{reason=~"internal|resolve_failed|enforcement_error|trace_filter_failed|enforcer_unavailable|write_not_bounded"}[15m])) > 0
```

Denied labels, unsupported queries and parse errors (the most frequent user complaints):

```promql
sum by (backend, endpoint) (rate(querdian_requests_total{reason=~"denied_label|unsupported|parse_error"}[5m]))
```

Authentication failures:

```promql
sum by (reason) (rate(querdian_auth_failures_total[5m]))
```

Latency with the classic buckets. With native histograms, use
`histogram_quantile(0.99, sum by (backend) (rate(querdian_request_duration_seconds[5m])))`.

```promql
histogram_quantile(0.99, sum by (le, backend, endpoint) (rate(querdian_request_duration_seconds_bucket{decision="allow"}[5m])))
```

Fan-out cost: terms for each request, the ratio of upstream requests to client requests, and the
upstream latency:

```promql
histogram_quantile(0.95, sum by (le, backend) (rate(querdian_policy_terms_bucket[15m])))

sum by (backend) (rate(querdian_upstream_requests_total[5m]))
  / sum by (backend) (rate(querdian_requests_total{decision="allow"}[5m]))

histogram_quantile(0.99, sum by (le, backend) (rate(querdian_upstream_duration_seconds_bucket[5m])))
```

If the ratio is much more than 1 on Loki, some users have many terms. Look at `dnf.terms` in the traces.

Rewrite time in Querdian:

```promql
histogram_quantile(0.99, sum by (le, language, strategy) (rate(querdian_rewrite_duration_seconds_bucket[5m])))
```

Upstream errors (`code` is `error` for transport failures, for example a refused connection or a timeout):

```promql
sum by (backend, code) (rate(querdian_upstream_requests_total{code!~"2.."}[5m]))
```

Policy and snapshot health:

```promql
querdian_policies{ready="false"} > 0
increase(querdian_snapshot_rebuilds_total{result="error"}[15m]) > 0
```

`querdian_snapshot_generation` is a counter of one replica. Do not compare it between replicas.

## Trace queries (TraceQL)

Use the data source that stores the traces of Querdian. The default `resource.service.name` is
`querdian`. All decision attributes are on the **server span**.

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

Errors (`decision = error`; the status of the server span is also `error`):

```traceql
{ resource.service.name = "querdian" && span.querdian.decision = "error" }
{ resource.service.name = "querdian" && status = error && span.querdian.decision.reason = "upstream_error" }
```

Denials for each reason as a rate (Tempo must support TraceQL metrics):

```traceql
{ resource.service.name = "querdian" && span.querdian.decision = "deny" } | rate() by (span.querdian.decision.reason)
```

To find the trace of a user, open the `X-Trace-Id` that the user reports in Grafana. With
`--tracing.record-user`, you can also filter `{ span.enduser.id = "alice@example.com" }`. The matched
policies are in `span.querdian.policies`.

Slow fan-outs and merges (the merge attributes are on the server span):

```traceql
{ resource.service.name = "querdian" && span.merge.kind != nil && duration > 5s }
{ resource.service.name = "querdian" && span.strategy = "fanout" } && { span.querdian.decision = "allow" && duration > 5s }
{ resource.service.name = "querdian" && span.merge.approximate = true }
```

Slow or failed upstream calls. Each backend request has one `upstream` client span. The name of the
status attribute changes with the instrumentation version, thus check one span first.

```traceql
{ resource.service.name = "querdian" && name = "upstream" && duration > 5s }
{ resource.service.name = "querdian" && name = "upstream" && status = error }
{ resource.service.name = "querdian" && name = "upstream" && span.http.response.status_code >= 500 }
```

To compare the rewrite time with the backend time, compare the durations of the `enforce.rewrite` and
`upstream` children in the trace of a slow request.

Snapshot rebuilds (root spans of the controller):

```traceql
{ resource.service.name = "querdian" && name = "snapshot.rebuild" && span.policies.invalid > 0 }
```

## Policy dashboard

The [policy dashboard](dashboard.md) answers some requests with these statuses, with the JSON body
`{"status":"error","error":"..."}`. With `--log-level=debug`, Querdian logs each `403` of this table as
`dashboard request refused`, with the reason and the first 64 bytes of the `Host` header.

| Status | Message | Cause | Correction |
|---|---|---|---|
| 403 | `host not allowed` | The `Host` header is empty or names a host that is not permitted. The dashboard accepts only `localhost`, `127.0.0.1`, `[::1]`, the host of `--ui-listen` and each `--ui-allowed-host`, with any port. Frequent causes: an Ingress or a proxy, or a URL with the IP address or the DNS name of the node. | Open the dashboard through `kubectl port-forward` and `http://localhost:<port>`. Or add the host name of the URL with `--ui-allowed-host` (Helm: `ui.allowedHosts`). Add only the names of your own Ingress or proxy. |
| 403 | `cross-origin request not allowed` | A request with a method other than `GET` (the test bench) has an `Origin` header with a different host or port than the `Host` header, or a `Sec-Fetch-Site` header other than `same-origin` or `none`. A proxy that changes the `Host` header causes this. | Make the Ingress or proxy keep the `Host` header of the browser. Send test bench requests only from the dashboard page. |
| 500 | `dashboard authentication not configured` | A wiring error: the dashboard handler has no authentication mode. It serves nothing. | Report a bug. |

## Receiver HTTP statuses

In a [multi-cluster](multicluster.md) setup, `querdian receiver` answers each agent request with one of
these statuses. Each rejection has the JSON body `{"error":"..."}` with a generic message. The receiver
logs it as `sync rejected`, with the identity and the cause. A rejected sync never renews
`lastSyncTime`. If `--client-ca-file` did not sign the cert, the TLS handshake fails before an HTTP
status, and the agent logs a connection error.

| Status | Message | Cause | Agent behaviour |
|---|---|---|---|
| 200 | `result: ok` | The stored state is equal to the state of the agent. The receiver renews `lastSyncTime`. The body has the `Ready` conditions. | waits for the next heartbeat |
| 200 | `result: fullStateRequired` | The digest of the heartbeat is different from the stored state. Nothing is renewed. | sends the full state immediately |
| 400 | `invalid request: ...`, `invalid policy: ...`, `invalid state: ...`, `the digest is not valid`, `the digest does not agree with the policies`, `a heartbeat must not carry policies`, `cannot read the body` | The body does not obey the protocol: an unknown field; too many JSON values (256 for each policy of `--max-policies`); a kind that does not agree with the spec; an `AccessPolicy` without a namespace; a `ClusterAccessPolicy` with a namespace; a duplicate policy. | retries at the next heartbeat (maximum one minute) |
| 400 | `the API server rejected a policy` | The API server rejected a `SpokePolicy` (CRD validation). | retries at the next heartbeat |
| 403 | `client certificate not accepted` | The cert has no URI SAN, or more than one. | retries at the next heartbeat |
| 403 | `unknown identity`, `the identity is on more than one SpokeCluster`, `the identity does not agree with the SpokeCluster name`, `the SpokeCluster name is not a valid label value` | No `SpokeCluster` has the URI SAN as `spec.identity`, or two have it, or the last path segment is not the name. | retries at the next heartbeat |
| 403 | `the SpokeCluster is suspended`, `the SpokeCluster has a deletion timestamp` | The spoke is suspended or in deletion. | retries at the next heartbeat |
| 404 | `unknown path` | A path other than `/v1alpha1/sync`, or a query string. Check `--receiver-url`. | retries at the next heartbeat |
| 405 | `unknown method` | A method other than `POST`. | retries at the next heartbeat |
| 409 | `a request with this identity is in progress`, `a sync for this cluster is in progress`, `the SpokeCluster changed during the sync`, `the stored state changed during the sync` | Two agents with the same cert (leader election off), an overlap of two syncs, or a `SpokeCluster` or `SpokePolicy` changed during the sync. | retries with backoff |
| 413 | `the body is too large`, `too many policies` | More than `--max-body-bytes` or `--max-policies`. Increase the limit, or decrease the policies of the spoke. | retries at the next heartbeat |
| 415 | `unknown content type` | A content type other than `application/json`, or a `Content-Encoding` other than `identity`. | retries at the next heartbeat |
| 429 | `too many changed syncs` | More than one changed sync of a cluster in 10 seconds (`Retry-After: 10`). | retries after `Retry-After` |
| 429 | `too many requests` | More than six requests of one identity in a burst, then more than one each 5 seconds (`Retry-After: 10`). An agent sends a maximum of one heartbeat and one full sync in each heartbeat interval (minimum 10 seconds). Look for two agents with the same cert, or a retry loop. | retries after `Retry-After` |
| 500 | `internal error` | An API server error, or a stored state that is not consistent. The receiver log has the cause. | retries at the next heartbeat |
| 503 | `too many requests in progress` | More than `--max-in-flight` requests at the same time (`Retry-After: 1`). | retries at the next heartbeat |

On each receiver replica that does not hold the lease, `/readyz` answers `503 not the leader`. This is
normal: only the leader serves syncs.

## Checks for operators

```sh
kubectl get cap,ap -A                      # Ready and Reason of every policy
kubectl get ns --show-labels | grep policy.querdian.dev/tenant
curl -s localhost:9090/readyz              # snapshot, issuer discovery and (multi-cluster) spoke table
curl -s localhost:9090/metrics | grep '^querdian_policies'
# multi-cluster, on the hub:
kubectl get spokeclusters                  # Suspended and LastSync of every spoke
kubectl get sp                             # spoke policies with cluster, kind and Ready
curl -s localhost:9090/metrics | grep '^querdian_cluster_policy_'
```
