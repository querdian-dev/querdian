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
>   and **overwrite** whatever the SDK sent; reject spans for which the association fails.
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

## 2. Assets, actors and trust boundary

**Protected:** the confidentiality of metrics, logs and traces in Prometheus/Thanos, Loki and
Tempo against *authenticated but unauthorised* readers; the integrity of the policy objects.
Availability of the backends against abusive queries is protected on a best-effort basis (section 8).
Write paths are out of scope: Querdian denies all of them.

**Trusted:** the OIDC issuer (its signing keys and the claims it issues), the Kubernetes API server
(policies and namespace labels), the operator-supplied static configuration, and the network path
between Querdian and the backends.

**Not trusted:** everything in the client request except the verified token's claims: path, query
string, form body, all headers (including tenant headers, `Forwarded`, `traceparent`, `baggage`),
the query text, and WebSocket frames.

**Actors:** an authenticated user with a narrow policy (the main adversary); a user with no
policy; an anonymous client; a workload owner who controls what her pods emit; a namespace admin who
can create `AccessPolicy` objects in her namespace; a client that replays or steals tokens.

```
client --Bearer JWT--> [ Querdian ] --tenant header set by Querdian--> backends
                         auth -> resolve -> routes -> enforce -> proxy/fan-out/merge
                              ^ snapshot (policies, namespace labels) from the Kubernetes API
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
LogQL; any attribute token in TraceQL). Label listing endpoints omit denied names and value
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
| Large merged responses | `limits.maxMergeBytes` (64 MiB) total per request, enforced while reading (`502 merge_too_large`); fixed 16 MiB for Prometheus labels/series filtering; trace-by-ID and Tempo search buffers use `maxMergeBytes` | Memory is `concurrency x maxMergeBytes`; no global limit |
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
  changes until it closes.
* The issuer discovery runs in the background; until it succeeds all requests fail with `503`.

## 10. Snapshot staleness

Policies live in an immutable snapshot rebuilt after changes (250 ms debounce) from informer caches,
and swapped atomically; requests see either the old or the new snapshot. Consequences:

* **Revocation delay** = Kubernetes watch latency + debounce + rebuild time, normally under a
  second. In-flight requests and tails keep the policy they started with.
* **API outage or failed rebuild**: the previous snapshot stays in use. Revoked access therefore
  persists while the API server is unreachable; this is the availability-over-freshness choice.
  `querdian_snapshot_rebuilds_total{result="error"}` signals failures.
* **Replicas converge independently.** For a short time two replicas can answer the same user
  differently after a change.
* **Namespace label changes** (tenant assignment) are part of the snapshot and have the same delay.
* Invalid policies are excluded entirely (grant nothing); a snapshot is never partially applied.
* Status conditions are written by the leader only and may lag or be absent when the leader is
  down; they are informational and not used for enforcement.

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
  it (`--ui-auth=oidc`) or keep it on a trusted network; see [dashboard.md](../dashboard.md).
* **Privacy.** Usernames and query text are not written to logs or traces by default; the two
  tracing flags are opt-in. `querdian.policies` records policy names.
* **Supply chain.** Dependencies are limited to Apache-2.0 and permissive licences (checked by
  `make licenses`), commits are signed off (DCO), and CI includes CodeQL and OpenSSF Scorecard.
  There are no signed releases, SBOM or provenance yet because there is no release.

## 12. Summary of residual risks

1. Namespace-label integrity and RBAC on policies and namespaces are outside Querdian's control
   (section 1).
2. Clean-room LogQL and TraceQL parsers may still diverge from the backends in unexplored corners;
   the differential suites cover only the tested backend versions (section 4).
3. Denied labels do not redact results unless `labels.redact` is used, and redaction does not cover
   log lines or cardinality (section 5).
4. Policies that cover only part of a trace rely on the root-intrinsic restriction (section 7).
5. Bearer tokens are valid until expiry; open tails outlive tokens and policy changes (section 9).
6. Policy revocation is eventually consistent and survives API outages (section 10).
7. No built-in rate limiting; memory use of merged responses scales with concurrency (section 8).
