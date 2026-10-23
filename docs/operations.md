# Operations

How to configure, run and observe Querdian. This documents the binary as it exists in this
repository. There is **no published release, container image or Helm chart yet** (they belong to
the packaging tasks of the plan); the sections on deployment therefore describe what the binary
needs rather than a packaged install. See [policy-guide.md](policy-guide.md) for the policy
resources and [troubleshooting.md](troubleshooting.md) for diagnosing denials.

## Process model

One binary, `querdian`, runs three things:

* the **proxy listener** (`--listen`, default `:8080`): plain HTTP and WebSocket. It does not
  terminate TLS; put an ingress, gateway or service mesh in front of it and make it the only way
  clients reach the backends.
* the **management listener** (`--management-listen`, default `:9090`): `GET /metrics`,
  `GET /healthz`, `GET /readyz`. It is unauthenticated; do not expose it outside the cluster.
* the optional **dashboard listener** (`--ui-listen`, off by default): a read-only view of the active
  policies, see [dashboard.md](dashboard.md).
* the **controller**: informers on `ClusterAccessPolicy`, `AccessPolicy` and `Namespace`, a 250 ms
  debounce, snapshot compilation and, on the leader only, status writing.

Every replica builds its own snapshot and serves traffic independently. Replicas share no state.

## Static configuration

Process settings are flags; structured settings come from one YAML file, read **once at startup**
(`--config`, default `/etc/querdian/config.yaml`; mount it from a ConfigMap and restart the pods
to change it). Unknown fields are an error, and any validation failure stops the process before
it listens. Policies reference backends by `name`, so changing backend names invalidates policies
(`UnknownBackend`).

### Sample configuration

```yaml
auth:
  issuer: https://keycloak.example.com/realms/observability
  audiences: [querdian]
  claims:
    username: preferred_username
    groups: groups
backends:
  - name: metrics
    type: prometheus
    url: http://thanos-query.monitoring:10902
    pathPrefix: /prometheus
    namespaceLabel: namespace
  - name: logs
    type: loki
    url: http://loki-gateway.monitoring
    pathPrefix: /loki
    namespaceLabel: k8s_namespace_name
    tenantHeader: X-Scope-OrgID
    tenantFederation: tenantIdLabel
  - name: traces
    type: tempo
    url: http://tempo-query-frontend.monitoring:3200
    pathPrefix: /tempo
    namespaceLabel: resource.k8s.namespace.name
    tenantHeader: X-Scope-OrgID
    tenantFederation: none
    traceByID: filter
limits:
  maxPolicyTerms: 16
  maxFanout: 16
  maxMergeBytes: 67108864
  upstreamTimeout: 5m
```

Grafana data sources use the proxy URL plus the `pathPrefix` (for example
`https://querdian.example.com/loki`) with the user's OAuth token forwarded.

### `auth`

| Field | Default | Meaning |
|---|---|---|
| `issuer` | required | OIDC issuer URL (`http` or `https`). Discovery runs in the background with exponential backoff (1 s up to 30 s). The token's `iss` must equal this string exactly. |
| `audiences` | required, non-empty, no empty entries | The token's `aud` must contain **any** of these. |
| `caFile` | empty | PEM bundle trusted for the issuer's discovery and JWKS requests (TLS 1.2 minimum). It is not used for backends. An unreadable file or one without certificates stops startup. |
| `claims.username` | `preferred_username` | Claim holding the username; a dotted path is allowed (a literal key containing dots is tried first). Missing or empty means `401 unauthorized`. |
| `claims.groups` | `groups` | Claim holding the groups: a string or an array of strings, dotted path allowed (for example `realm_access.roles`). A missing claim means no groups; any other shape (or non-string elements) means `401`. |

Tokens are verified for signature, expiry, issuer and audience. There is no token cache, no
introspection and no revocation check beyond token expiry.

### `backends[]`

| Field | Default | Meaning |
|---|---|---|
| `name` | required, unique | Referenced by policy rules, used as the `backend` metric label. |
| `type` | required | `prometheus` (Prometheus and Thanos Query; Mimir's Prometheus-compatible API was only researched for tenant federation, not tested end to end), `loki` or `tempo`. |
| `url` | required | `http` or `https` URL with a host. A path in the URL is kept as a prefix of the upstream path. Upstream TLS uses the system trust store. |
| `pathPrefix` | required | Route prefix on the proxy. Must start with `/`, not be `/`, no trailing `/`. Prefixes must not be equal or nest (`/a` and `/a/b` conflict). |
| `namespaceLabel` | required | Label/attribute carrying the namespace in this backend; used for the implicit matcher of `AccessPolicy`. It must be a valid name for the type (Tempo: `resource.<name>` or `span.<name>`) or every `AccessPolicy` targeting the backend is `InvalidMatcher`. **Its integrity is a security requirement**, see [policy-guide.md](policy-guide.md). |
| `tenantHeader` | empty | Header that selects the tenant upstream (for example `X-Scope-OrgID`). Empty means single-tenant: tenants in policies are ignored. The proxy sets this header itself; client-sent tenant headers are dropped (see `forwardHeaders`). For Thanos Query with `--query.enforce-tenancy`, set it to Thanos's tenant header (`THANOS-TENANT` unless `--query.tenant-header` changes it); without it Thanos applies its default tenant to every request. |
| `tenantFederation` | `none` | `none` or `tenantIdLabel`. `tenantIdLabel` requires a `tenantHeader` and says the backend can filter `__tenant_id__` under a `t1\|t2` header (Loki, Mimir). Use `none` for Tempo. See [policy-guide.md](policy-guide.md#cross-tenant-behaviour-per-backend). |
| `forwardHeaders` | `[Accept, Accept-Encoding, User-Agent]`, on Loki plus `X-Loki-Response-Encoding-Flags` | Client headers passed to the backend; every other client header is dropped. A list replaces the default, `[]` forwards none. Rejected at startup: credentials (`Authorization`, `Cookie`, `Proxy-Authorization`), tenant headers (`X-Scope-OrgID`, `THANOS-TENANT`, this backend's `tenantHeader`), trace context (`traceparent`, `tracestate`, `baggage`), forwarding headers (`Forwarded`, `X-Forwarded-*`, `X-Real-IP`), hop-by-hop headers (`Connection`, `Upgrade`, `TE`, ...), `Host`, `Content-Type` and `Content-Length`. Add a header only once you know what it does in the backend: one that selects a tenant under another name or changes limits (for example Loki's `X-Loki-Query-Limits`) hands that control to every client. Responses in a format a filter cannot parse fail with `502`. Applies to single-request plans; fan-out requests and `tail` handshakes carry no client headers. |
| `traceByID` | `deny` on Tempo | Tempo only (setting it elsewhere is an error): `deny` answers trace-by-ID requests with `403 trace_by_id_denied`; `filter` fetches the trace and removes spans the policy does not admit. |

### `limits`

All must be positive. What happens when one trips is listed under [Limits](#limits-and-what-happens-when-they-trip).

| Field | Default | Meaning |
|---|---|---|
| `maxPolicyTerms` | 16 | Maximum number of terms of a user's effective policy after simplification. |
| `maxFanout` | 16 | Maximum number of upstream requests one client request may create, and the number run in parallel. |
| `maxMergeBytes` | 67108864 (64 MiB) | Total size of buffered upstream bodies of one merged request; also the buffer cap of the trace-by-ID filter. |
| `upstreamTimeout` | `5m` | Deadline of one proxied request (all of its upstream requests together). Go duration syntax. |

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--config` | `/etc/querdian/config.yaml` | Static configuration file. |
| `--listen` | `:8080` | Proxy listener. |
| `--management-listen` | `:9090` | `/metrics`, `/healthz`, `/readyz`. Must differ from `--listen`. |
| `--ui-listen` | empty (off) | Read-only policy dashboard, see [dashboard.md](dashboard.md). Must differ from the other listeners. |
| `--ui-auth` | `oidc` | `oidc` (bearer token required) or `none`. |
| `--ui-admin-group` | empty | With `oidc`: group a token must carry. Empty: any valid token. |
| `--leader-elect` | `true` | Leader election for the status writer. |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error`. |
| `--log-format` | `json` | `json` or `text`. |
| `--tracing.record-user` | `false` | Add `enduser.id` to spans and `user` to request log lines. |
| `--tracing.record-queries` | `false` | Add the original and rewritten query to the server span. |
| `--version` | | Print the version and exit. |

Kubernetes access uses the usual controller-runtime discovery (in-cluster service account, or
`KUBECONFIG` / `~/.kube/config` for local runs). Shutdown on `SIGINT`/`SIGTERM` drains in-flight
requests for up to 30 s.

## Deployment notes

Until a chart exists, a deployment needs:

* **CRDs**: apply `config/crd/policy.querdian.dev_clusteraccesspolicies.yaml` and
  `config/crd/policy.querdian.dev_accesspolicies.yaml` (generated by `make manifests`). The CRDs
  carry the CEL validations (at least one subject; exactly one of `unrestricted` or `matchers`).
* **RBAC** for the service account:
  * `get`, `list`, `watch` on `clusteraccesspolicies`, `accesspolicies` (group
    `policy.querdian.dev`) and on `namespaces`;
  * `update`, `patch` on `clusteraccesspolicies/status` and `accesspolicies/status`;
  * `get`, `list`, `watch`, `create`, `update`, `patch` on `coordination.k8s.io` `leases` in the
    namespace of the pod (the lease is named `querdian-leader-election`).
  The service account needs no access to workloads or secrets.
* **Network isolation**: backends must not be reachable by clients except through Querdian
  (NetworkPolicy or equivalent). The security model depends on it.
* **Several replicas** behind one Service. The pods are stateless.
* **Backend settings** for cross-tenant requests (multi-tenant query federation) if you use
  several tenants per user, see [policy-guide.md](policy-guide.md#tenants). Tempo streaming over
  gRPC is out of scope and must be disabled in the Grafana Tempo data source.
* A **`traceparent`** header from clients is honoured as the parent of the request span; towards
  upstreams only W3C trace context is sent.

### Readiness and health

| Endpoint | Returns 200 when |
|---|---|
| `/healthz` | the process is serving (always `ok`). |
| `/readyz` | a first policy snapshot exists **and** OIDC issuer discovery has completed. Otherwise `503` with the missing part in the body. |

Until then the proxy answers `503 unavailable` (`snapshot_not_ready` or `auth_unavailable`).
After readiness, readiness stays true while the Kubernetes API is unreachable: the last good
snapshot keeps being used (see [Upgrade and failure behaviour](#upgrade-and-failure-behaviour)).

### Scaling

* Each request costs one parse and rewrite (see `querdian_rewrite_duration_seconds`; typically
  microseconds to milliseconds) plus streaming the response. Scale horizontally on CPU.
* **Fan-out multiplies upstream load**: a log query for a user with N terms is N upstream requests,
  up to `maxFanout`. Watch `querdian_policy_terms` and `querdian_upstream_requests_total`. Prefer
  policy shapes that simplify to one term (the single-label merge helps).
* **Memory**: streaming requests use constant memory. Merged requests buffer up to
  `maxMergeBytes` each, so plan for `peak concurrent merged requests x maxMergeBytes`. Reduce the
  value if you expect many concurrent fan-outs. There is no built-in rate limit or concurrency cap;
  apply one at the ingress.
* The snapshot is held per replica in memory and rebuilt completely on every policy or namespace
  label change (about 250 ms after the last change). Snapshot size grows with the number of rules.
* WebSocket tails keep one connection per client plus one per term open.

### Leader election

`--leader-elect=true` (default) uses a Kubernetes `Lease` (`querdian-leader-election`, released on
shutdown, controller-runtime default timings). Only the **status writer** is leader-gated:
exactly one replica patches `status.conditions`. All replicas build snapshots and serve requests,
so leader loss never interrupts enforcement; it only pauses status updates until a new leader
acquires the lease. With a single replica or for local runs you may use `--leader-elect=false`.
`querdian_policies` and `querdian_snapshot_*` are exported by every replica.

## Limits and what happens when they trip

| Limit | Value | When exceeded | Client sees | `decision` / `reason` |
|---|---|---|---|---|
| `limits.maxPolicyTerms` | 16 | the user's simplified policy has more terms | `403 forbidden` | deny / `policy_too_complex` |
| `limits.maxFanout` | 16 | the plan needs more upstream requests (terms for a fan-out; also guards a plan without requests) | `403 forbidden` | deny / `query_too_complex` |
| `limits.maxMergeBytes` | 64 MiB | buffered bodies of the fan-out exceed it; also the trace-by-ID filter body, and the Tempo search body | `502`, errorType `merge_too_large` (fan-out) or `upstream` | error / `upstream_error` |
| `limits.upstreamTimeout` | 5m | the proxied request runs longer | `502 upstream` (or a truncated stream if headers were already sent) | error / `upstream_error` |
| selector copies (fixed) | 2048 | PromQL or-lift: selectors x terms | `403 forbidden` | deny / `query_too_complex` |
| `match[]` expansion (fixed) | 256 | Prometheus metadata: user selectors x terms | `403 forbidden` | deny / `query_too_complex` |
| form body (fixed) | 1 MiB | POST body larger | `400 bad_data` | deny / `bad_request` |
| labels/series response filter (fixed) | 16 MiB | Prometheus `labels`/`series` body when denied labels are filtered | `502` | error / `upstream_error` |
| tail frame (fixed) | 4 MiB | a single upstream tail frame | tail closed with an error | n/a (after upgrade) |
| tail dedupe memory (fixed) | 10 000 entries | oldest entries are forgotten, duplicates after that are possible | n/a | n/a |
| parser nesting (fixed) | 100 levels | LogQL or TraceQL nesting deeper | `400 bad_data` | deny / `parse_error` |

An upstream failure of **any** branch of a fan-out fails the whole request with `502` (the
upstream status and the first 256 bytes of its error body are included in the message, unless the
caller's policy redacts labels: then the body is withheld, see the
[threat model](security/threat-model.md#3-header-and-request-handling)).

## Observability

### Metrics

Served on the management port. Labels: `endpoint` is the route template (for example
`/api/v1/label/:name/values`), never the raw path; unknown paths use `unknown`.

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `querdian_requests_total` | counter | `backend`, `endpoint`, `decision`, `reason` | `decision` is `allow`, `deny` or `error`; reasons are listed in [troubleshooting.md](troubleshooting.md#decision-reason-codes). |
| `querdian_request_duration_seconds` | histogram (native and classic, exemplars) | `backend`, `endpoint`, `decision` | Exemplar `trace_id` for sampled requests. Scrape with exemplar support (OpenMetrics). |
| `querdian_upstream_requests_total` | counter | `backend`, `code` | `code` is the HTTP status or `error` for transport failures. One increment per upstream request, so fan-outs count several. |
| `querdian_upstream_duration_seconds` | histogram (native and classic, exemplars) | `backend` | Time until response headers. |
| `querdian_rewrite_duration_seconds` | histogram | `language` (`promql`, `logql`, `traceql`), `strategy` | Only observed when a query was rewritten. |
| `querdian_policy_terms` | histogram | `backend` | DNF terms per resolved request (`0` for unrestricted). |
| `querdian_auth_failures_total` | counter | `reason` | `missing_token`, `invalid_token`, `expired`, `audience`, `issuer`, `missing_username`. |
| `querdian_snapshot_generation` | gauge | | Counts rebuilds **of this replica**; not comparable across replicas. |
| `querdian_snapshot_rebuilds_total` | counter | `result` | `ok` or `error`. |
| `querdian_policies` | gauge | `kind`, `ready` | Policies by kind and compile result. |

Go and process collectors are also exported.

### Tracing

Configured exclusively through the standard `OTEL_*` environment variables
(`OTEL_SERVICE_NAME` default `querdian`, `OTEL_EXPORTER_OTLP_ENDPOINT`,
`OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_TRACES_EXPORTER`, `OTEL_TRACES_SAMPLER`,
`OTEL_TRACES_SAMPLER_ARG`, `OTEL_RESOURCE_ATTRIBUTES`).

* If neither an OTLP endpoint nor `OTEL_TRACES_EXPORTER` is set, spans are discarded.
* If `OTEL_TRACES_SAMPLER` is unset the sampler is `parentbased_traceidratio` with ratio 1.0.
* Propagation is fixed in code: inbound W3C `tracecontext` and `baggage`, outbound `tracecontext`
  only. `OTEL_PROPAGATORS` is ignored on purpose, so client baggage never reaches a backend.
* Every response carries `X-Trace-Id` and every error body a `traceId`; use it to find the trace.

Span tree of one proxied request:

| Span | Attributes |
|---|---|
| server span `HTTP <method> <route template>` | `http.route`, `querdian.decision` (`allow`, `deny`, `error`), `querdian.decision.reason`, `querdian.policies` (policy names), merge attributes `merge.kind` (`loki_streams`, `string_set`, `sum_counts`), `merge.items_in`, `merge.items_out`, `merge.deduped`, `merge.truncated` (log streams), `merge.approximate` (sums); status error when `decision` is `error` |
| `authn.verify` | status error when authentication failed |
| `policy.resolve` | `snapshot.generation`, `policies.matched`, `dnf.terms`, `dnf.unrestricted`, `dnf.disjoint`, `tenants.count` |
| `enforce.rewrite` | `query.language`, `strategy` (`inject`, `or_lift`, `fanout`, `match_expand`, `passthrough`), `query.bytes_in`, `query.bytes_out` |
| `upstream` (client span, one per upstream request) | standard HTTP client attributes |
| `snapshot.rebuild` (root span, controller) | `querdian.trigger` (`startup`, `event`), `snapshot.generation`, `policies.total`, `policies.invalid`, `compile.duration_ms` |

Privacy flags, both **off by default**:

* `--tracing.record-user`: `enduser.id` on the server span and `user` in the request log line.
* `--tracing.record-queries`: `querdian.query.original` and `querdian.query.rewritten` on the
  server span. Queries can contain sensitive values; enable only where the trace backend has
  equivalent access controls. Queries are never written to the log.

Without these flags no username and no query text is recorded; `querdian.policies` records the
names of the matched policies.

### Logging

`log/slog` to stderr, JSON by default (`--log-format=text` for humans). Every proxied request
produces one `request` line with `backend`, `route`, `decision`, `reason`, `status`, `duration`
and, when the request is traced, `trace_id` and `span_id`; plus `user` if `--tracing.record-user`
is set. A `querdian started` line is written at startup.

## Upgrade and failure behaviour

* **Pre-release.** The API group is `policy.querdian.dev/v1alpha1` and may change incompatibly
  before a stable version; read the changelog of each release once releases exist.
* **Order**: apply updated CRDs first (`kubectl apply --server-side -f config/crd`), then roll the
  deployment. The static configuration is read at startup, so a configuration change needs a
  rollout.
* **Rolling updates** are safe for availability: a new pod is ready only after its first snapshot
  and issuer discovery. During a rollout old and new replicas may enforce slightly different
  behaviour if the new version changed rewriting rules; there is no cross-replica coordination.
* **Policy changes** reach each replica within the watch latency plus ~250 ms. In-flight requests
  and **open WebSocket tails continue under the policy they started with**, and an open tail is not
  closed when the token expires.
* **Kubernetes API outage**: replicas keep serving with the last snapshot (they do not fail open
  or closed). A failed rebuild keeps the previous snapshot and increments
  `querdian_snapshot_rebuilds_total{result="error"}`; a new pod cannot become ready.
* **Issuer outage**: tokens can only be verified with keys the verifier has cached; unreachable
  issuer discovery at startup keeps the pod unready (`auth_unavailable`).
* **Removed backend names or labels**: policies referencing an unknown backend become
  `UnknownBackend` and grant nothing.
