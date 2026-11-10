# Threat model

This document describes what Querdian protects, what it trusts, how it can be attacked and which
mitigations exist in the code today. It describes the implementation in this repository; where a
mitigation is planned but not present, that is said explicitly. Report vulnerabilities as described
in [SECURITY.md](../../SECURITY.md). Related: [policy guide](../policy-guide.md),
[limitations](../limitations.md), [operations](../operations.md).

## 1. Namespace-label integrity (the assumption everything rests on)

> **`AccessPolicy` isolation is only as strong as the integrity of the namespace label in your
> telemetry.**
>
> An `AccessPolicy` in namespace `X` is implemented as the matcher
> `<backends[].namespaceLabel> = "X"` on every query. Querdian has no way to know whether a series,
> log stream or span labelled `X` really came from namespace `X`. That label must be written by the
> **collection pipeline**, from trusted metadata, and must be **impossible for workloads to set or
> override**:
>
> * Prometheus: set `namespace` through service discovery relabelling; do not use
>   `honor_labels: true` for scrapes of untrusted workloads; do not allow workloads to push metrics
>   (Pushgateway, remote write) with their own `namespace`.
> * Logs: set the namespace label in the agent (Alloy, Promtail, OpenTelemetry Collector) from the
>   pod metadata; do not derive it from log content a workload controls. Do not expose the Loki
>   push endpoint to workloads.
> * Traces: set `resource.k8s.namespace.name` in the Collector with the `k8sattributes` processor
>   and **overwrite** whatever the SDK sent; reject spans for which the association fails. Use a
>   `resource.` attribute as the `namespaceLabel` of a Tempo backend. Querdian also accepts a `span.`
>   attribute there, but an application sets its span attributes itself. The `clusterLabel` of a
>   Tempo backend must be a `resource.` attribute.
>
> If a workload can choose the label, it can place its data in another team's view and mislead
> that team. If the label is derived from content that someone other than the platform controls
> (for example a log field), the isolation boundary is simply whatever that party writes. Data
> that lacks the label is invisible to all `AccessPolicy` users. A workload does not gain read
> access to other namespaces through this alone, but the integrity of what each team sees is lost,
> and any policy matcher on a label that workloads can set has the same weakness.
>
> Two more integrity requirements follow:
>
> * **Kubernetes RBAC is part of the policy.** Whoever may create or edit an `AccessPolicy` in
>   namespace `X` decides who can read data labelled `X`; whoever may edit labels of the
>   `Namespace` object decides which backend tenant its policies map to
>   (`policy.querdian.dev/tenant`). Only `ClusterAccessPolicy` objects can grant `unrestricted`
>   access or arbitrary matchers; restrict them to platform administrators.
> * **Backends must not be reachable around Querdian.** Isolate them with NetworkPolicy or
>   equivalent and give Querdian the only credentials / network path. Anyone who can talk to a
>   backend directly bypasses all policies, including setting the tenant header.

**Multi-cluster: the cluster label has the same weight.** With the `multicluster` block
([multicluster.md](../multicluster.md)), every hub `AccessPolicy` and every spoke policy gets an
implicit matcher `<clusterLabel>="<cluster>"`, so the cluster label separates the data of the spokes
as the namespace label separates the teams. Querdian cannot verify it:

* The ingest gateway must set the label from the credential of the sending spoke and **overwrite**
  any value that the spoke sent, for metrics, logs and traces. A gateway that only adds a missing
  label lets a spoke write data under the name of another spoke (or of the hub), and the admin of
  that spoke can then mislead the readers of the other cluster's data.
* Writers on the hub that bypass the gateway (ruler output, hub agents) must not be able to set the
  label of a spoke, and hub telemetry must carry `<clusterLabel>="<hubCluster>"`. Hub data without
  the label is invisible to hub `AccessPolicy` users.
* The `SpokeCluster` name must equal the label value that the gateway writes, and cluster names must
  not change or be reused while old data with the name exists.
* Spoke data that arrives before the `multicluster` block is set is matched by hub `AccessPolicy`
  objects for the same namespace name. The order of the migration steps is part of the security
  model.

## 2. Assets, actors and trust boundary

**Protected:** the confidentiality of metrics, logs and traces in Prometheus/Thanos, Loki and
Tempo against *authenticated but unauthorised* readers; the integrity of the policy objects.
Availability of the backends against abusive queries is protected on a best-effort basis (section 8).
Write paths are out of scope: Querdian denies all of them.

**Trusted:** the OIDC issuer (its signing keys and the claims it issues), the Kubernetes API server
(policies and namespace labels), the operator-supplied static configuration, and the network path
between Querdian and the backends.

**Changes to the trusted set in a multi-cluster setup** (section 12):

* *Added:* `querdian receiver` and its service account, which can write `SpokePolicy` objects and
  `SpokeCluster.status`; the agent CA, which must be controlled by the hub; each agent cert, which
  can grant the data of its own spoke; the ingest gateway, which writes the cluster label; and the
  hub admins who write `SpokeCluster` objects (identity, tenant allow-list, suspension).
* *Trusted for one spoke only:* the admins and the API server of each spoke. What the agent of
  spoke A sends can grant only data labelled with cluster A, in the tenants that the hub allows for
  A. The proxy never reads a spoke API server; it reads the hub copies.
* *Unchanged:* hub policies, the static configuration and the OIDC issuer. All clusters share the
  issuer, so a group name means the same in every cluster.

**Not trusted:** everything in the client request except the verified token's claims: path, query
string, form body, all headers (including tenant headers, `Forwarded`, `traceparent`, `baggage`),
the query text, and WebSocket frames.

**Actors:** an authenticated user with a narrow policy (the main adversary); a user with no
policy; an anonymous client; a workload owner who controls what her pods emit; a namespace admin who
can create `AccessPolicy` objects in her namespace; a client that replays or steals tokens. In a
multi-cluster setup also: a **spoke admin**, who controls the policies, namespaces and tenant labels
of one spoke and can therefore grant that spoke's data to any subject (by design), and who may try to
reach the data of the hub or of other spokes; a spoke team with `AccessPolicy` objects in the spoke;
and whoever holds an agent cert or an ingest cert of a spoke.

```
client --Bearer JWT--> [ Querdian ] --tenant header set by Querdian--> backends
                         auth -> resolve -> routes -> enforce -> proxy/fan-out/merge
                              ^ snapshot (policies, namespace labels) from the Kubernetes API

multi-cluster:
spoke API --> [ agent ] --mTLS (agent cert)--> [ receiver ] --> SpokePolicy, SpokeCluster.status (hub API)
spoke telemetry --ingest cert--> [ ingest gateway: overwrites cluster label ] --> backends
```

Properties the design maintains: default deny for unknown paths, methods, parameters and users;
**fail closed**: any error in authentication, resolution, parsing, rewriting or merging produces a
rejection, never an unenforced pass-through; and **no silent over-grant**: if the exact union of a
user's policies cannot be expressed for a request, it is rejected with a reason.

## 3. Header and request handling

* **Tenant headers.** Client tenant headers are dropped by the allow-list below, and the
  configured `tenantHeader`, `X-Scope-OrgID` and Thanos `THANOS-TENANT` cannot be added to it. The
  proxy sets the tenant header only from the effective policy (`t1|t2` for several tenants). A
  client can never choose its tenant, unless an operator allow-lists a header that selects tenants
  under another name.
* **Client headers are forwarded by allow-list** (`forwardHeaders` per backend). By default only
  `Accept`, `Accept-Encoding` and `User-Agent` are copied from the client, plus
  `X-Loki-Response-Encoding-Flags` towards Loki (the `categorize-labels` encoding is handled by the
  merges and by redaction). Everything else is dropped, such as backend controls like
  `X-Loki-Query-Limits`. Credentials, tenant headers, trace context, forwarding headers, hop-by-hop
  headers (`Upgrade`, `Connection`, ...), `Host`, `Content-Type` and `Content-Length` cannot be
  allow-listed; the configuration is rejected at startup. An operator who adds a header hands its
  effect in the backend to every client. Querdian sets `Content-Type`, the tenant header and
  `traceparent` from its own span (only W3C trace context goes out; the propagator is fixed in
  code). On streamed
  (single-request) plans `X-Forwarded-For`, `X-Forwarded-Host` and `X-Forwarded-Proto` are re-set
  by the proxy (client values discarded) and `Forwarded` is not sent. Fan-out requests and
  WebSocket upstream handshakes carry none of the client's headers.
* **Protocol switches.** Only Loki `tail` is proxied as a WebSocket, on its own code path. On every
  other route the client's `Upgrade` is never forwarded, and a `101 Switching Protocols` from a
  backend (or a gateway in front of it) fails with `502` before any byte reaches the client, so no
  raw tunnel can bypass the response filters.
* **Path confusion.** Routes are matched on a canonical path only. Escaped slashes, backslash,
  `%00`, residual `%`, `.`/`..`, empty segments, trailing slashes, control characters and invalid
  UTF-8 are rejected, so no encoding can make a request alias a different route. Lookup is
  literal-over-parameter-over-wildcard on whole segments; the method is part of the match.
* **Parameter pollution.** URL query and form body are merged the way Prometheus does; any
  single-valued parameter (every name not ending in `[]`, not only `query` and `q`) appearing more
  than once is rejected, because backends disagree about which value wins (Loki prefers the form
  value) and the enforcers read the first one (`targetLabels`, `delay_for`, ...). The upstream
  request is rebuilt from the parsed parameters: `GET` carries only a URL query, `POST` only a
  form body. Unknown parameters are rejected rather than dropped (for example Thanos
  `storeMatch[]`, which could select stores, and Tempo `q` on trace by ID).
* **Content negotiation.** Merged plans force `Accept: application/json`. Filters that rewrite a
  response fail with `502` for any representation they cannot parse.
* **Errors.** Error bodies contain a generic message plus parser or rewriter detail for the
  caller's own query. Fan-out failures include the first 256 bytes of the backend's error body.
  When the request has redactions (section 5) no upstream error text reaches the client: backend
  error bodies are replaced (the status is kept), fan-out failures omit the body, and tails close
  without the upstream's reason, because backends print series label sets in some query errors.
  Authentication errors never contain token content.

## 4. Query injection and parser divergence

**Threat.** The proxy rewrites a query so that every selector carries the policy. If Querdian
understands the query differently from the backend (a different tokenisation, a construct the proxy
does not see as a selector, a comment or escape that hides text), an attacker can craft a query
whose selectors the proxy never rewrites, or smuggle in a second query, and read data outside the
policy. This is the central risk of any rewriting proxy.

**Mitigations in the design and code:**

1. **No string surgery.** PromQL is parsed with the Prometheus parser, rewritten on the AST and
   printed with `String()`. LogQL and TraceQL use the project's own parsers (`pkg/logql`,
   `pkg/traceql`), written from the public language documentation and black-box tests against
   Loki 3.5.0 and Tempo 2.9.0, and rewrite by byte positions of recognised selectors and filters.
2. **Own parsers fail closed and are stricter than the backends.** Where documentation and
   behaviour are ambiguous or divergent, the parser rejects: unusual escapes, single quotes, comments
   (TraceQL), backtick strings (TraceQL), compound durations, undocumented operators and functions,
   `parent.*`, unquoted attribute names followed by `-` or `/`, mixed spanset operators without
   parentheses, and so on ([limitations.md](../limitations.md)). A rejected valid query is an
   inconvenience; a differently parsed one would be a vulnerability. An unknown token is an error,
   never skipped.
3. **Re-parse checks on the output.** PromQL: the rewritten string must parse again. LogQL: the
   result is parsed again and the number of selectors must equal the expected number, every
   selector must carry all matchers of a policy term and keep a non-empty anchor matcher. TraceQL: the
   policy filter is probed to be exactly one filter without hints, and the result is parsed again and
   must contain the expected number of filters. Any failure rejects the request.
4. **Precedence is never assumed.** Tempo evaluates `&&` and `||` with equal precedence, left to
   right (verified); the injected filter is `{ (X) && (dnf) }` with every disjunct parenthesised.
   The canonical LogQL rendering parenthesises every nested binary operation, and the log pipeline is
   not reinterpreted: it is kept as the original token sequence.
5. **Semantic cover of every selector.** Single term: matchers are injected into every vector
   selector, including those in range vectors and subqueries (PromQL) and every stream selector and
   range aggregation (LogQL) and every spanset filter, including inside structural operators,
   pipelines, `select`, metrics functions and `compare` (TraceQL). Several terms: PromQL/LogQL
   metric queries are or-lifted so that each selector copy carries exactly one term; log queries
   are sent once per term. If a query cannot be lifted exactly it is rejected. User matchers are
   ANDed with policy matchers; they can only narrow.
6. **Omission is not pass-through.** If a restricted user omits `query`, `q` or `match[]`, the policy
   selector is sent instead.
7. **Request-level consistency.** What is parsed is what is sent: the rewritten parameter replaces
   the original in a request rebuilt from allow-listed parameters; there is no second channel such as
   a body vs URL difference.
8. **Tests.** Fuzz targets exist for the lexers and parsers (`FuzzLex`, `FuzzParseRoundTrip` in
   `pkg/logql` and `pkg/traceql`: no panics, `Parse(String(x))` is equivalent), for the injectors
   (`FuzzInject` in `pkg/logql`, `pkg/traceql`, `internal/enforce/promql`; `FuzzOrLift`; generated
   queries must re-parse and every selector or filter must carry a policy term), for DNF
   normalisation (`FuzzNormaliseEquivalence`) and for the Prometheus labels response filter. Run
   them with `make fuzz`. The end-to-end suites pin the token boundaries the injectors depend on
   against the real backends (`parser-boundaries` in `test/e2e/loki` and `test/e2e/tempo`): Loki ends
   a `#` comment at LF only and `#` inside a string starts none; Tempo treats `\"` and `\\` as escapes
   in strings and quoted attribute names. A backend version that changes them fails there first. The
   LogQL rewrite is rendered from the proxy's tokens, so comment text never reaches Loki.

**Residual risk.**

* PromQL: Querdian's parser is the Prometheus library version in `go.mod`; a backend of a
  different version (Thanos, Mimir) could parse a rare construct differently. The default parser
  options (no experimental syntax) reduce the difference. Keep the library close to the version
  of your backends.
* LogQL and TraceQL: the parsers are clean-room and cannot be proven identical to the backends'.
  Mitigation: `make e2e` runs differential and equivalence suites against real Prometheus v3.5.0,
  Loki 3.5.0 and Tempo 2.9.0 containers (`test/e2e/`). Generated queries, including mutated ones, are
  sent to the backend directly and through the proxy under many policies; a query the backend accepts
  must be enforced (result equal to a reference instance holding only the granted data) or rejected by
  the proxy, and no rewrite may be rejected by the backend. The suites were mutation-checked (breaking
  injection or redaction makes them fail). They are evidence, not proof, and they cover exactly the
  versions above: treat a new Loki or Tempo major version as untested until the suites pass against it.
  There is no Grafana UI end-to-end test; the kind quickstart is a manual demonstration.
* Unrestricted users are not parsed (their queries go through unmodified); by definition they may
  read everything in their tenants.

## 5. Label oracle attacks (denied labels)

**Threat.** A user who may not see label `customer_id` could still learn its values by filtering on
it (`{customer_id="42"}` and observing whether data returns), grouping by it, joining on it, or by
making the backend echo it in a listing.

**Mitigations.** Queries that reference a denied label are rejected with `403 denied_label`
(`by`/`without`/`on`/`ignoring`/`group_*`, matchers, `label_replace`, `label_join`, `count_values`
in PromQL; any identifier or keyword token, `label_replace` string arguments, and templates in
LogQL; any attribute token in TraceQL). A LogQL template is parsed with Go's `text/template/parse`
and must contain only allow-listed nodes and functions; a template that does not parse is
rejected. Label listing endpoints omit denied names and value
endpoints for denied labels answer 403, also for the `U__`-escaped form of the name that Prometheus 3
decodes (`U__customer__id`). A label denied by any matched rule is denied for the whole
request.

**Redaction (opt-in).** `labels.redact` additionally rewrites the values of a label in query
results (`Remove`, `Placeholder`, `Hash` with a per-request key, `IPv4Prefix`). A redacted name is
always denied as well, so it cannot be used as a filter oracle, and the strictest strategy of all
matched rules applies. Rewriting covers Prometheus query, range and exemplar results, Loki query
results (stream labels and categorize-labels metadata) and tail frames, Tempo search and TraceQL
metrics results, and trace by ID. Bodies that cannot be decoded, or that exceed
`limits.maxMergeBytes`, are answered with 502 and never forwarded; upstream error bodies are
withheld (section 3). Placeholder numbers are
assigned in random order and hash keys are random per request, so neither reveals the sort order
of values or links values across requests.

**What this does not protect.** With `labels.deny` alone, the values of a denied label are *still
present in the label sets of results* (a query for `up` returns `customer_id`): deny restricts what
users can do with labels and is not a redaction mechanism. The one exception is trace by ID, which
strips denied attributes. With `labels.redact`, the following channels remain: result cardinality,
series order and timing of any query the user may run; under `Placeholder` and `Hash`, which entries
of one response share a value; under `IPv4Prefix`, the kept octets; Loki log line content, and
labels that parsers derive from it under a different name (`| json` flattening nested keys);
values that ingestion copied into other labels; and the Tempo trace-level intrinsics in section 6.

## 6. Tempo metadata leaks and the root-span redaction rules

**Threat.** The Tempo search API reports trace-level data derived from *all* spans of a trace, not
only those that matched the injected policy. A user whose policy admits a child span of a trace
would also receive data about spans outside the policy. Tempo 2.9.0 was observed to return:
`rootServiceName` and `rootTraceName` (the root span, even if only a child matched),
`serviceStats` (every service of the trace with span and error counts), `durationMs` (extent of the
whole trace) and `startTimeUnixNano` (earliest start of any span in the trace).

**Rules implemented** (`internal/fanout`, `RootCovered` in `internal/enforce/traceql`):

1. The search response is buffered (up to `maxMergeBytes`) and rewritten as an **allow-list**: per
   trace only `traceID`, `spanSet`, `spanSets` and two recomputed fields survive; at top level only
   `traces` and `metrics`. Unknown fields, including those added by future Tempo versions, are
   dropped. Matched spans and their attributes are copied byte for byte.
2. Unless the root span is known to satisfy the policy, `rootServiceName`, `rootTraceName` and
   `serviceStats` are removed (the `<root span not yet received>` placeholder as well).
3. `startTimeUnixNano` and `durationMs` are **recomputed from the returned matching spans only**:
   earliest matched start, and extent to the latest matched end rounded up to whole milliseconds.
4. The root span counts as covered only when the effective policy admits every span (unrestricted,
   or a term without matchers and without tenant binding). A matching policy is not sufficient,
   because the injected filter constrains the *matched* spans only and the spans of one trace may
   carry different resource attributes; proving the root's attributes would need extra queries.
5. Tag listings (`api/v2/search/tags`) are filtered for denied tags; tag values endpoints answer
   403 for a denied tag. Tempo answers both unfiltered for `q` shapes it does not evaluate there, which
   would drop an injected policy (GHSA-jcqw-3c3f-xrmj), so restricted lookups are sent per policy term as
   a conjunction of `=` conditions only and united; policies that cannot be written that way are refused.
6. Trace by ID (`traceByID: filter`): the trace is fetched and spans are dropped unless a policy
   term matches the span's attributes and its resource's attributes (a missing attribute never
   satisfies any operator, matching TraceQL), empty groups are pruned, denied attributes are removed
   from remaining spans, and an empty result is a `404` (also for v2). The default is `deny`.
   Anything that cannot be filtered (unknown content type, undecodable body, oversize) is `502`.

## 7. Trace intrinsics oracle (fixed)

The trace-level intrinsics `rootName`, `rootServiceName`, `traceDuration` and their `trace:` forms
(`trace:rootName`, `trace:rootService`, `trace:duration`) are queryable in spanset filters
(`docs/research/traceql.md` section 8). Left unchecked they are a boolean oracle: for a trace in which
at least one span satisfies the policy, a user could test whether the (redacted) root span's name or
service equals a value or whether the trace duration exceeds a threshold. The T4.6 end-to-end suite
confirmed this against Tempo 2.9.0, including the same information through tag values for
`rootServiceName`, through `select`/`by`/metrics grouping, and through `compare()` (whose result carries
`rootServiceName` as a label dimension).

**Mitigation.** The enforcer rejects these intrinsics, `compare()` and the tag-value lookup of the root
names with `400` for every user whose policy does not cover the root span (`traceql.RootCovered`: the
policy is unrestricted or has a term without matchers). A policy that admits whole traces is therefore
the way to give a team trace-level queries. Regression tests: `internal/enforce/traceql/rootintrinsics_test.go`
and `test/e2e/tempo`.

## 8. Resource exhaustion

| Vector | Mitigation | Remaining exposure |
|---|---|---|
| Policy with many terms causing fan-out | `limits.maxPolicyTerms` (16): `403 policy_too_complex`; DNF simplification merges and drops redundant terms | Operators who raise it |
| Many upstream requests per client request | `limits.maxFanout` (16): bound on requests and on parallelism, `403 query_too_complex`; all-or-nothing: one failure cancels the rest | Load multiplier up to `maxFanout` per request |
| Large merged responses | `limits.maxMergeBytes` (64 MiB) total per request: each branch counts each chunk of at most 32 KiB in a shared counter before it keeps it, and the first chunk above the limit stops the read and cancels the other branches (`502 merge_too_large`); buffers grow only to what the branch can keep. `limits.maxInflightMergeBytes` (default `maxMergeBytes`) bounds the fan-out bodies of all concurrent requests of a replica; the oldest reading fan-out waits for space, the others fail (`503 merge_budget_exhausted`). After the merge the budget holds only the merged response, and its write has a deadline (10 s + 1 s per MiB, at most 2 min), so a client that does not read cannot hold the budget. The Go memory limit is 90 percent of the container limit. Fixed 16 MiB for Prometheus labels/series filtering; trace-by-ID and Tempo search buffers use `maxMergeBytes` | All branches together can read up to 32 KiB each past `maxMergeBytes`. The budget counts body bytes only; the merge needs about 5 to 6 x the body bytes (measured), and the single-request filter buffers are outside the budget, so memory is about 6 x `maxInflightMergeBytes` plus `concurrency x maxMergeBytes` for single-request filters. A full budget rejects fan-outs (denial of service, fails closed) |
| Selector explosion in rewrites | 2048 selector copies for PromQL or-lifting, 256 for `match[]` expansion, parser nesting limit of 100 (LogQL, TraceQL) | |
| Large request bodies | 1 MiB limit on form bodies; `ReadHeaderTimeout` of 10 s on both listeners | No limits on header size beyond Go defaults |
| Slow or hanging backends | `limits.upstreamTimeout` per request; dial and write timeouts on tails | Idle client connections |
| WebSocket tails | per-term upstream sockets bounded by `maxFanout`; 4 MiB frame cap; bounded dedupe memory (10 000 entries); closing one side closes all | Number of concurrent tails is not limited |
| Metric cardinality | `endpoint` is a route template; user names are not labels | |

Querdian has **no built-in rate limiting or per-user quotas**; apply them at the ingress, and use
the backends' own per-tenant limits.

## 9. Token replay and authentication

* Tokens are verified per request against the issuer's keys (signature), `exp`, issuer (exact
  match) and audience (any configured value). There is no session state.
* A stolen bearer token is usable until it expires, from anywhere that can reach the proxy. There
  is no token binding, introspection or revocation list. Mitigate with short token lifetimes, TLS
  on the path to Querdian (Querdian itself does not terminate TLS), narrow audiences, and network
  restrictions.
* The token is never forwarded upstream and never logged. Error messages contain fixed text only.
* An identity provider that issues tokens with attacker-controlled `groups` or `preferred_username`
  claims breaks the model; the trust in the issuer is absolute. Use a dedicated audience for
  Querdian.
* A WebSocket tail authenticates only at the handshake; it continues after token expiry or policy
  changes until it closes. The one exception is the multi-cluster time limit (section 10): a tail
  closes within 10 seconds when a spoke whose rules gave its terms expires, is suspended, or gets a
  new `SpokeCluster` spec or UID.
* The issuer discovery runs in the background; until it succeeds all requests fail with `503`.

## 10. Snapshot staleness

Policies live in an immutable snapshot rebuilt after changes (250 ms debounce, at most 2 s under a
steady stream of changes) from informer caches,
and swapped atomically; requests see either the old or the new snapshot. Consequences:

* **Revocation delay** = Kubernetes watch latency + debounce + rebuild time, normally under a
  second. In-flight requests and tails keep the policy they started with (except the spoke expiry
  below).
* **API outage or failed rebuild**: the previous snapshot stays in use, and a failed rebuild runs
  again after one second. Revoked access therefore
  persists while the API server is unreachable; this is the availability-over-freshness choice.
  `querdian_snapshot_rebuilds_total{result="error"}` signals failures.
* **Replicas converge independently.** For a short time two replicas can answer the same user
  differently after a change.
* **Namespace label changes** (tenant assignment) are part of the snapshot and have the same delay.
* Invalid policies are excluded entirely (grant nothing); a snapshot is never partially applied.
* Status conditions are written by the leader only and may lag or be absent when the leader is
  down; they are informational and not used for enforcement.

**Multi-cluster: spoke policies and the time limit.** A spoke policy reaches the hub through the agent
and the receiver, so its revocation takes longer:

* **Revocation delay of a spoke policy** = up to one agent heartbeat (default one minute) + the
  delay above. The limit of one changed sync in 10 seconds can add one retry. During an outage of the
  agent, the receiver, the network path or the hub API server, a revoked spoke grant stays active until
  the time limit.
* **Time limit.** The policies of a spoke stop when `SpokeCluster.status.lastSyncTime` is older than
  `maxStaleness` (default 48 hours). The check runs on each request against a deadline table outside
  the snapshot, because expiry makes no Kubernetes event. A spoke is also expired when it is
  suspended, being deleted, has no valid sync of its current `metadata.generation`, is not yet in the
  snapshot with its current version (UID and generation, so a change of `SpokeCluster.spec`, such as
  a smaller tenant allow-list, never applies late, also not for a deleted and re-created object), or
  has a `lastSyncTime` more than five minutes in the future. An expired rule keeps its `labels.deny`
  and `labels.redact`, so expiry only narrows; a user left with no terms gets `403 policy_source_expired`.
  An open Loki tail closes within 10 seconds after a spoke in its terms expires, is suspended, or
  gets a new `SpokeCluster` version, so a smaller tenant allow-list also ends an open tail. A
  replica with the `multicluster` block is ready only after it has read all `SpokeCluster` objects.
* The time limit does **not** protect against a stolen agent cert, which can renew `lastSyncTime`
  (section 12). Hub policies never expire.

## 11. Other considerations

* **Confused deputy through policies.** A user cannot create their own access: only the objects
  listed in section 1 can. Fail-closed validation means an invalid policy grants nothing, and
  `AccessPolicy` cannot express `unrestricted`, tenants, or matchers outside its own namespace
  (the namespace matcher comes first and cannot be removed).
* **Cross-tenant union.** Tenant bindings are added with `__tenant_id__` only where the backend
  supports it and only when the header is a federation header; where it is unsupported the request
  is rejected rather than widened.
* **Management port** (`/metrics`, `/healthz`, `/readyz`) is unauthenticated and exposes policy
  counts and request labels by endpoint and reason; keep it cluster-internal.
* **Policy dashboard** (`--ui-listen`, off by default) discloses group names, tenants and matchers. It is
  read-only and its query test bench is an in-memory dry run that never contacts a backend. Authenticate
  it (`--ui-auth=oidc`) or keep it on a trusted network; see [dashboard.md](../dashboard.md). On a
  multi-cluster hub it shows the policies of **all** spokes, so a dashboard user learns the subjects,
  matchers and tenants of every cluster. Give dashboard access only to people who may see the policies
  of all clusters, for example with `--ui-admin-group`.
* **DNS rebinding and cross-origin requests against the dashboard.** Other web pages in the browser of a
  dashboard user must not read the dashboard or use the test bench. This is most important with
  `--ui-auth=none`, which has no authentication. The dashboard accepts only an allowed host in the `Host`
  header (`localhost`, `127.0.0.1`, `[::1]`, the host of `--ui-listen` and each `--ui-allowed-host`), for
  the API and the static files, in all authentication modes. A request with a method other than `GET`
  must also come from the origin of the dashboard. Use `none` only on a loopback address or through a
  port-forward.
* **Management port, multi-cluster.** `querdian_cluster_policy_age_seconds` and
  `querdian_cluster_policy_expired` expose the names of all spokes and their sync state.
* **Privacy.** Usernames and query text are not written to logs or traces by default; the two
  tracing flags are opt-in. `querdian.policies` records policy names. See section 11.1.
* **Supply chain.** Dependencies are limited to Apache-2.0 and permissive licences (checked by
  `make licenses`), commits are signed off (DCO), and CI includes CodeQL and OpenSSF Scorecard.
  Releases have cosign signatures, SBOMs and build-provenance attestations (see
  [self-assessment.md](self-assessment.md) and [releasing.md](../releasing.md)).

### 11.1 Privacy of the telemetry of Querdian

The traces of Querdian go to a trace backend that can have other readers than the protected data.
Thus the spans must not disclose the data that the policies protect, and the policies themselves.

* **Query text.** The rewritten query holds the matchers of the policy of the user, and the original
  query can hold sensitive values. Without `--tracing.record-queries`, no span records query text:
  not the `querdian.*` attributes, and not the standard HTTP attributes. The server span records
  only the path. Each `upstream` client span records `url.full` with scheme, host and path only,
  also for each fan-out request and for the Loki tail handshake. The query string is removed
  before the client span starts, so the attribute never holds it.
* **Paths.** Paths are recorded. They can hold values from the client, for example a trace ID or a
  tag name, but not the matchers of a policy.
* **Inbound trace context.** By default, the server span does not continue the trace of the client.
  It starts a new trace and links the trace context of the client. Thus a client cannot select the
  trace ID, cannot force sampling and cannot put its spans in the same trace as the spans of
  Querdian. `--tracing.trust-inbound-context` removes this protection. Use it only when all
  clients that can reach the proxy are trusted. Client baggage is never sent to a backend.
* **Residual risk.** Span names, `http.route`, decisions, policy names and counts are always
  recorded. Give access to the trace backend only to people who may know which policies exist.

## 12. Multi-cluster

With the `multicluster` block, policies of spoke clusters are enforced on the hub
([multicluster.md](../multicluster.md), PLAN section 18). This adds a write path for policy state and
new credentials.

**The receiver is a new attack surface.** `querdian receiver` is the only Querdian component with a
write path, and it is reachable from the spokes.

* *Impact of a compromise:* a compromised receiver (its process, its service account or its
  Deployment) can write any `SpokePolicy` for any `SpokeCluster` and renew `lastSyncTime`. That
  grants the data of **all spokes** to any subject. It cannot grant hub data: a `SpokeCluster` with
  the hub cluster name grants nothing, and spoke rules always carry the cluster matcher of a
  registered spoke. Protect the receiver's namespace, service account and RBAC like the hub
  `ClusterAccessPolicy` objects.
* *Mitigations in the code:* the receiver runs as a separate Deployment, so the read proxy keeps no
  write path. It terminates mTLS itself with its own required client CA, takes the cluster only from
  the URI SAN of the client cert (the protocol has no cluster field), rejects a cert with no or
  several URI SANs, and requires an exact match with `spec.identity` of exactly one `SpokeCluster`
  whose name is the last path segment of the identity. It has one path and one method, decodes
  strictly (unknown or case-variant fields, duplicate keys and trailing data are rejected) in one
  pass over the tokens with a limit on the number of JSON values (256 for each policy of
  `--max-policies`), so the memory of one request has a fixed upper limit, checks
  every policy against the CRD rules in Go, and rejects the whole sync on any doubt: a body above
  `--max-body-bytes`, more than `--max-policies` policies, a digest that does not match, more than one
  changed sync of a cluster in 10 seconds, a suspended or deleted `SpokeCluster`. It reads the
  `SpokeCluster` directly from the API server at the start of each sync and before each write, and
  writes `status` with a `resourceVersion` precondition, so a sync that overlaps a spec change
  validates nothing. It never changes or deletes an object of a different cluster; object names are
  a hash of cluster, kind, namespace and name. Each identity can send a burst of six requests and then
  one request each 5 seconds (`429`); this limit applies before the body is read and before any read
  of the API server, also to heartbeats and to identities that are not registered. Requests in
  progress are limited (`--max-in-flight`), and one identity and one cluster can each have only one
  sync at a time. Error
  messages are generic and never contain data of other spokes.
* *RBAC:* the receiver needs write access only to `spokepolicies` and `spokeclusters/status`. The
  proxy needs read access and `spokepolicies/status` only. Only hub admins write `SpokeCluster.spec`.
* *Remaining exposure:* there is no rate limit per source address before the TLS handshake; the
  receiver does not reload its certs (a rotation needs a restart); and the compiler is a second line
  of defence (it checks the object name, the kind, the `SpokeCluster` and the tenant allow-list
  again), not an independent one.

**The agent cert.** An agent cert can write any grant for its own spoke, but not for a different
spoke and not for the hub. Its holder can also renew `lastSyncTime`, so the time limit gives no
protection against a stolen agent cert.

* The client CA of the receiver must be controlled by the hub; a spoke that can issue certs from it
  can impersonate other spokes.
* The agent cert must be dedicated: it must not be the ingest cert, and its identity must not occur
  on an ingest cert. Otherwise the theft of a write-only telemetry credential would give the power to
  grant read access. The `SpokeCluster` object, written by a hub admin only, links the agent identity
  to the cluster name.
* An identity on two `SpokeCluster` objects, or one whose last path segment is not the name of its
  `SpokeCluster`, is rejected; a suffix of a segment does not match.
* There is no certificate revocation list. A hub admin handles a stolen agent cert with the
  procedure in [multicluster.md](../multicluster.md#stolen-agent-cert): suspend the spoke, find the
  cause of the theft, issue a cert with a new identity, delete the `SpokePolicy` objects of the spoke,
  and only then unsuspend it. Grants return only after a new valid sync. The time limit gives no
  protection here, because the holder of the cert can renew `lastSyncTime`.

**The spoke admin.** A spoke admin can grant the data of that spoke to any subject; this is by
design. These rules prevent more:

* Spoke rules always get `<clusterLabel>="<spoke>"`. A spoke rule never compiles to unrestricted, so
  the queries of its users are always parsed and rewritten. `allClusters` is invalid on spoke rules.
  A spoke matcher on the cluster label is ANDed and can only narrow (a fuzz target pins this after
  DNF normalisation).
* A spoke policy may only use tenants in `SpokeCluster.spec.tenants`; hub namespace labels never
  apply to spoke policies.
* A `SpokeCluster` with the hub cluster name grants nothing, and each hub `ClusterAccessPolicy` must
  state its cluster scope as a fixed set of names, `allClusters` or `unrestricted`, so a new spoke
  never widens an existing hub grant.
* *Remaining:* a spoke admin who names a shared group can reduce that group's access elsewhere
  (denied labels apply to the whole request, more terms, cross-tenant unions), see
  [limitations](../limitations.md#multi-cluster). No data leaks through this.

**Status back to the spoke.** The receiver returns the `Ready` conditions of the spoke's own policies
only. It replaces `CrossTenantUnionUnsupported` with `Compiled`, because that message can reveal the
grants of a shared subject in other tenants.

**Rollback.** Without the `multicluster` block a hub `AccessPolicy` has no cluster matcher and
matches namespace `X` of every spoke. Querdian refuses to start when a backend keeps `clusterLabel`
without the block, but an operator who removes both re-opens this path while spoke data is in the
backends ([multicluster.md](../multicluster.md#rollback)).

## 13. Summary of residual risks

1. Namespace-label integrity and RBAC on policies and namespaces are outside Querdian's control
   (section 1).
2. Clean-room LogQL and TraceQL parsers may still diverge from the backends in unexplored corners;
   the differential suites cover only the tested backend versions (section 4).
3. Denied labels do not redact results unless `labels.redact` is used, and redaction does not cover
   log lines or cardinality (section 5).
4. Policies that cover only part of a trace rely on the root-intrinsic restriction (section 7).
5. Bearer tokens are valid until expiry; open tails outlive tokens and policy changes, but close
   when a spoke in their terms expires, is suspended or gets a new spec (section 9).
6. Policy revocation is eventually consistent and survives API outages (section 10).
7. The proxy has no built-in rate limiting. Fan-out bodies of all requests share one budget, so many
   concurrent fan-outs fail with 503; the single-request filter buffers are not in that budget and
   scale with concurrency (section 8). Only the multi-cluster receiver limits the requests of each agent identity (section 12).
8. Multi-cluster: isolation between spokes relies on the ingest gateway overwriting the cluster label
   (section 1); a compromised receiver can grant the data of all spokes, and a stolen agent cert the
   data of its spoke until a hub admin suspends it (section 12); a revoked spoke policy stays active
   for up to one heartbeat, or up to the time limit during an outage (section 10).
