# Threat model

This page describes what Querdian protects, what it trusts, how an attacker can attack it, and which
mitigations are in the code.

In short:

* Querdian protects metrics, logs and traces against authenticated users who do not have permission to
  read them. It denies all write paths.
* All isolation depends on the namespace label in the telemetry. In a multi-cluster setup, it also
  depends on the cluster label. Querdian cannot verify these labels.
* Querdian fails closed. An error in authentication, resolution, parsing, rewriting or merging gives a
  rejection, never an unenforced pass-through.
* Querdian has no built-in rate limiting, no token revocation and no TLS termination.

The page describes the implementation in this repository. If a mitigation is planned but not in the code,
the page says so. To report a vulnerability, follow [SECURITY.md](../../SECURITY.md). Related pages:
[how Querdian works](../architecture.md) (drawings of the query path and of hub and spoke),
[policy guide](../policy-guide.md), [limitations](../limitations.md), [operations](../operations.md).

## Trust boundaries and the main assumption

[Section 2](#2-assets-actors-and-trust-boundary) gives the details of these boundaries:

* **Client to Querdian.** Querdian trusts only the claims of the verified token. All other parts of the
  request are not trusted.
* **Querdian to the services that it trusts.** Querdian trusts the OIDC issuer, the Kubernetes API
  server, the static configuration of the operator, and the network path to the backends.
* **Collection pipeline to the backends.** Querdian does not see this path. The pipeline writes the
  labels that the policies use.
* **Spoke to hub (multi-cluster).** The receiver, the agent CA, the agent certs and the ingest gateway
  are added to the trusted set. Each spoke is trusted only for its own data.

**The one assumption.** Querdian adds the matcher `<namespaceLabel>="X"` to each query of an
`AccessPolicy` in namespace `X`. Thus the isolation is only as strong as the namespace label in the
telemetry. The collection pipeline must write this label from trusted metadata, and workloads must not
be able to set it. In a multi-cluster setup, the cluster label has the same weight: the ingest gateway
must overwrite it. See [section 1](#1-namespace-label-integrity-the-assumption-everything-rests-on).

## Threats and mitigations

| Threat | Mitigation | Residual risk |
|---|---|---|
| A workload sets a false namespace label ([1](#1-namespace-label-integrity-the-assumption-everything-rests-on)) | None in Querdian. The collection pipeline must set the label from trusted metadata. | Outside the control of Querdian. |
| A spoke writes data with the cluster label of a different cluster ([1](#1-namespace-label-integrity-the-assumption-everything-rests-on)) | None in Querdian. The ingest gateway must overwrite the label. | Outside the control of Querdian. |
| A client sends queries directly to a backend ([1](#1-namespace-label-integrity-the-assumption-everything-rests-on)) | None in Querdian. Isolate the backends with NetworkPolicy. | Direct access bypasses all policies. |
| A person who can edit policies or namespace labels grants access ([1](#1-namespace-label-integrity-the-assumption-everything-rests-on), [11](#11-other-considerations)) | Kubernetes RBAC. An `AccessPolicy` cannot grant `unrestricted` access, tenants or matchers outside its namespace. | RBAC is part of the policy. |
| A client chooses its tenant or sends backend controls in headers ([3](#3-header-and-request-handling)) | Header allow-list. Querdian sets the tenant header from the policy. | An operator who allow-lists a header gives its effect to each client. |
| A path, parameter or protocol switch that bypasses the checks ([3](#3-header-and-request-handling)) | Canonical paths. Duplicate and unknown parameters are rejected. A `101` outside Loki tail gives `502`. | — |
| Error text shows data ([3](#3-header-and-request-handling)) | With redactions, no upstream error text gets to the client. | Without redactions, fan-out failures show the first 256 bytes of the backend error body. |
| Query injection and parser divergence ([4](#4-query-injection-and-parser-divergence)) | Real parsers that are stricter than the backends, re-parse checks, fuzz targets, differential suites. | The parsers can diverge from the backends in rare constructs and in versions that were not tested. |
| A user learns the values of a denied label ([5](#5-label-oracle-attacks-denied-labels)) | `403 denied_label` for each query that uses the label. Opt-in redaction with `labels.redact`. | `labels.deny` alone does not remove values from results. Redaction does not hide cardinality, order, timing or log lines. |
| Tempo search returns data of spans outside the policy ([6](#6-tempo-metadata-leaks-and-the-root-span-redaction-rules)) | Allow-list rewrite of the response. Root fields are removed, start and duration are calculated again. | — |
| Trace-level intrinsics as an oracle ([7](#7-trace-intrinsics-oracle-fixed)) | `400` for these intrinsics, `compare()` and root-name tag values, unless the policy covers the root span. | Policies that cover only part of a trace depend on this restriction. |
| Resource exhaustion ([8](#8-resource-exhaustion)) | Limits on policy terms, fan-out, merge bytes, regex weight, body size and time. | No rate limiting. The number of policy objects and of open tails is not limited. |
| A stolen or replayed token ([9](#9-token-replay-and-authentication)) | Verification on each request. The token is never forwarded or logged. | A stolen token is valid until it expires. Open tails continue after the token expires. |
| Revoked access stays active ([10](#10-snapshot-staleness)) | Snapshot rebuild, normally in less than one second. Time limit for spoke policies. | Revocation is eventually consistent. Revoked access stays during an API outage. |
| Disclosure through the management port, the dashboard or the telemetry of Querdian ([11](#11-other-considerations)) | Dashboard off by default, host check, OIDC option. No query text in spans by default. | The management port has no authentication. Spans always record policy names and counts. |
| A compromised receiver ([12](#12-multi-cluster)) | Separate Deployment, mTLS, strict decoding, rate limits, narrow RBAC. | It can grant the data of all spokes. |
| A stolen agent cert ([12](#12-multi-cluster)) | The cert grants only data of its own spoke. Procedure to suspend the spoke. | It grants the data of its spoke until a hub admin suspends the spoke. |
| A spoke admin tries to get the data of the hub or of other spokes ([12](#12-multi-cluster)) | Implicit cluster matcher, tenant allow-list, a spoke rule is never unrestricted. | She can decrease the access of a shared group in other places. No data leaks. |

A dash means that this page states no residual risk for the threat.
[Section 13](#13-summary-of-residual-risks) lists all residual risks.

## 1. Namespace-label integrity (the assumption everything rests on)

> **The isolation of an `AccessPolicy` is only as strong as the integrity of the namespace label in your
> telemetry.**

Querdian implements an `AccessPolicy` in namespace `X` as the matcher
`<backends[].namespaceLabel> = "X"` on each query. Querdian cannot know if a series, log stream or span
with the label `X` really came from namespace `X`. Thus the **collection pipeline** must write that label
from trusted metadata. It must be **impossible for workloads to set or override** the label:

| Signal | Requirement |
|---|---|
| Metrics (Prometheus) | Set `namespace` through service discovery relabelling. Do not use `honor_labels: true` for scrapes of workloads that are not trusted. Do not let workloads push metrics with their own `namespace` (Pushgateway, remote write). |
| Logs | Set the namespace label in the agent (Alloy, Promtail, OpenTelemetry Collector) from the pod metadata. Do not derive it from log content that a workload controls. Do not expose the Loki push endpoint to workloads. |
| Traces | Set `resource.k8s.namespace.name` in the Collector with the `k8sattributes` processor, and **overwrite** the value that the SDK sent. Reject spans for which the association fails. Use a `resource.` attribute as the `namespaceLabel` of a Tempo backend. Querdian also accepts a `span.` attribute there, but an application sets its span attributes itself. The `clusterLabel` of a Tempo backend must be a `resource.` attribute. |

If the label is not trustworthy:

* If a workload can choose the label, it can put its data in the view of a different team and mislead
  that team.
* If the label comes from content that a party other than the platform controls (for example a log
  field), the isolation boundary is what that party writes.
* Data without the label is invisible to all `AccessPolicy` users.
* A workload does not get read access to other namespaces through this alone. But the integrity of what
  each team sees is lost.
* Each policy matcher on a label that workloads can set has the same weakness.

Two more integrity requirements follow:

* **Kubernetes RBAC is part of the policy.**
  * A person who can create or edit an `AccessPolicy` in namespace `X` decides who can read data with
    the label `X`.
  * A person who can edit the labels of the `Namespace` object decides which backend tenant its
    policies map to (`policy.querdian.dev/tenant`).
  * Only `ClusterAccessPolicy` objects can grant `unrestricted` access or arbitrary matchers. Give
    permission to write them only to platform administrators.
* **Clients must not reach the backends around Querdian.** Isolate the backends with NetworkPolicy or an
  equivalent. Give the credentials and the network path to the backends only to Querdian. A person who
  can send requests directly to a backend bypasses all policies, and can also set the tenant header.

**Multi-cluster: the cluster label has the same weight.** With the `multicluster` block
([multi-cluster](../multicluster.md)), each hub `AccessPolicy` and each spoke policy gets an implicit
matcher `<clusterLabel>="<cluster>"`. Thus the cluster label separates the data of the spokes, as the
namespace label separates the teams. Querdian cannot verify the cluster label:

* The ingest gateway must set the label from the credential of the spoke that sends the data. It must
  **overwrite** each value that the spoke sent, for metrics, logs and traces.
* If a gateway only adds a missing label, a spoke can write data under the name of a different spoke or
  of the hub. The admin of that spoke can then mislead the readers of the data of the other cluster.
* Writers on the hub that bypass the gateway (ruler output, hub agents) must not be able to set the
  label of a spoke.
* Hub telemetry must carry `<clusterLabel>="<hubCluster>"`. Hub data without the label is invisible to
  hub `AccessPolicy` users.
* The `SpokeCluster` name must be equal to the label value that the gateway writes. While old data with
  a cluster name exists, do not change that name and do not use it again.
* Spoke data that arrives before you set the `multicluster` block matches hub `AccessPolicy` objects for
  the same namespace name. Thus the order of the migration steps is part of the security model.

## 2. Assets, actors and trust boundary

| | |
|---|---|
| **Protected** | The confidentiality of metrics, logs and traces in Prometheus/Thanos, Loki and Tempo against users who are *authenticated but not authorised*. The integrity of the policy objects. The availability of the backends against abusive queries, on a best-effort basis ([section 8](#8-resource-exhaustion)). |
| **Out of scope** | Write paths. Querdian denies all of them. |
| **Trusted** | The OIDC issuer (its signing keys and the claims that it issues). The Kubernetes API server (policies and namespace labels). The static configuration of the operator. The network path between Querdian and the backends. |
| **Not trusted** | All of the client request except the claims of the verified token. This includes the path, the query string, the form body, all headers (also tenant headers, `Forwarded`, `traceparent`, `baggage`), the query text and WebSocket frames. |

**Changes to the trusted set in a multi-cluster setup** ([section 12](#12-multi-cluster)):

| Change | Components |
|---|---|
| *Added* | `querdian receiver` and its service account, which can write `SpokePolicy` objects and `SpokeCluster.status`. The agent CA, which the hub must control. Each agent cert, which can grant the data of its own spoke. The ingest gateway, which writes the cluster label. The hub admins who write `SpokeCluster` objects (identity, tenant allow-list, suspension). |
| *Trusted for one spoke only* | The admins and the API server of each spoke. What the agent of spoke A sends can grant only data with the cluster label A, in the tenants that the hub allows for A. The proxy never reads the API server of a spoke. It reads the copies on the hub. |
| *Unchanged* | The hub policies, the static configuration and the OIDC issuer. All clusters share the issuer, thus a group name has the same meaning in each cluster. |

**Actors:**

* An authenticated user with a narrow policy. This is the main adversary.
* A user with no policy.
* An anonymous client.
* A workload owner, who controls what her pods emit.
* A namespace admin, who can create `AccessPolicy` objects in her namespace.
* A client that replays or steals tokens.

In a multi-cluster setup, also:

* A **spoke admin**. She controls the policies, namespaces and tenant labels of one spoke. Thus she can
  grant the data of that spoke to any subject (by design). She can try to get the data of the hub or of
  other spokes.
* A spoke team with `AccessPolicy` objects in the spoke.
* A person who has an agent cert or an ingest cert of a spoke.

```
client --Bearer JWT--> [ Querdian ] --tenant header set by Querdian--> backends
                         auth -> resolve -> routes -> enforce -> proxy/fan-out/merge
                              ^ snapshot (policies, namespace labels) from the Kubernetes API

multi-cluster:
spoke API --> [ agent ] --mTLS (agent cert)--> [ receiver ] --> SpokePolicy, SpokeCluster.status (hub API)
spoke telemetry --ingest cert--> [ ingest gateway: overwrites cluster label ] --> backends
```

[How Querdian works](../architecture.md) has drawings of the same paths.

The design keeps these properties:

* **Default deny** for unknown paths, methods, parameters and users.
* **Fail closed.** Each error in authentication, resolution, parsing, rewriting or merging gives a
  rejection, never an unenforced pass-through.
* **No silent over-grant.** If Querdian cannot express the exact union of the policies of a user for a
  request, it rejects the request with a reason.

## 3. Header and request handling

* **Tenant headers.**
  * The allow-list below drops the tenant headers of the client. The configured `tenantHeader`,
    `X-Scope-OrgID` and Thanos `THANOS-TENANT` cannot be added to the allow-list.
  * The proxy sets the tenant header only from the effective policy (`t1|t2` for more than one tenant).
  * Thus a client can never choose its tenant. The one exception: an operator allow-lists a header that
    selects tenants under a different name.
* **Client headers are forwarded by allow-list** (`forwardHeaders` for each backend).
  * By default, Querdian copies only `Accept`, `Accept-Encoding` and `User-Agent` from the client, and
    `X-Loki-Response-Encoding-Flags` to Loki. The merges and the redaction handle the
    `categorize-labels` encoding.
  * Querdian drops all other headers, for example backend controls such as `X-Loki-Query-Limits`.
  * These headers cannot be allow-listed: credentials, tenant headers, trace context, forwarding
    headers, hop-by-hop headers (`Upgrade`, `Connection`, ...), `Host`, `Content-Type` and
    `Content-Length`. If the configuration contains one of them, Querdian rejects it at startup.
  * An operator who adds a header gives its effect in the backend to each client.
  * Querdian sets `Content-Type`, the tenant header, and `traceparent` from its own span. Only W3C trace
    context goes out. The propagator is fixed in code.
  * On streamed (single-request) plans, the proxy sets `X-Forwarded-For`, `X-Forwarded-Host` and
    `X-Forwarded-Proto` again and discards the client values. It does not send `Forwarded`.
  * Fan-out requests and WebSocket upstream handshakes carry no headers of the client.
* **Protocol switches.**
  * Querdian proxies only Loki `tail` as a WebSocket, on its own code path.
  * On all other routes, Querdian never forwards the `Upgrade` header of the client.
  * If a backend (or a gateway in front of it) sends `101 Switching Protocols`, the request fails with
    `502` before a byte gets to the client. Thus no raw tunnel can bypass the response filters.
* **Path confusion.**
  * Querdian matches routes on a canonical path only.
  * It rejects escaped slashes, backslash, `%00`, residual `%`, `.` and `..`, empty segments, trailing
    slashes, control characters and invalid UTF-8. Thus no encoding can make a request an alias of a
    different route.
  * The lookup uses whole segments. A literal segment has priority over a parameter, and a parameter
    has priority over a wildcard. The method is part of the match.
* **Parameter pollution.**
  * Querdian merges the URL query and the form body as Prometheus does.
  * It rejects each single-valued parameter that occurs more than once. This applies to each name that
    does not end in `[]`, not only to `query` and `q`. The reason: backends do not agree about which
    value wins (Loki prefers the form value), and the enforcers read the first value (`targetLabels`,
    `delay_for`, ...).
  * Querdian builds the upstream request again from the parsed parameters. `GET` carries only a URL
    query, `POST` only a form body.
  * Querdian rejects unknown parameters. It does not drop them. Examples: Thanos `storeMatch[]`, which
    can select stores, and Tempo `q` on trace by ID.
* **Content negotiation.** Merged plans force `Accept: application/json`. Filters that rewrite a
  response fail with `502` for each representation that they cannot parse.
* **Errors.**
  * Error bodies contain a generic message, and parser or rewriter details for the query of the caller.
  * Fan-out failures include the first 256 bytes of the error body of the backend.
  * If the request has redactions ([section 5](#5-label-oracle-attacks-denied-labels)), no upstream
    error text gets to the client. Querdian replaces backend error bodies (it keeps the status).
    Fan-out failures do not include the body. Tails close without the reason of the upstream. The
    reason: backends print series label sets in some query errors.
  * Authentication errors never contain token content.

## 4. Query injection and parser divergence

**Threat.** The proxy rewrites a query so that each selector carries the policy. Querdian can understand
a query differently from the backend. Examples: a different tokenisation, a construct that the proxy does
not see as a selector, or a comment or escape that hides text. Then an attacker can make a query with
selectors that the proxy never rewrites, or hide a second query in it. Thus the attacker can read data
outside the policy. This is the central risk of each rewriting proxy.

**Mitigations in the design and the code:**

1. **No string surgery.**
   * Querdian parses PromQL with the Prometheus parser, rewrites the AST and prints it with `String()`.
   * LogQL and TraceQL use the parsers of the project (`pkg/logql`, `pkg/traceql`). These parsers were
     written from the public language documentation and black-box tests against Loki 3.5.0 and
     Tempo 2.9.0.
   * These parsers rewrite by the byte positions of the selectors and filters that they recognise.
2. **The parsers of the project fail closed and are stricter than the backends.**
   * If the documentation and the behaviour are ambiguous or different, the parser rejects the query.
   * Examples: unusual escapes, single quotes, comments (TraceQL), backtick strings (TraceQL), compound
     durations, undocumented operators and functions, `parent.*`, unquoted attribute names followed by
     `-` or `/`, and mixed spanset operators without parentheses. See [limitations](../limitations.md).
   * A rejected valid query is an inconvenience. A query that Querdian parses differently would be a
     vulnerability.
   * An unknown token is an error. The parser never skips it.
3. **Re-parse checks on the output.** If a check fails, Querdian rejects the request.
   * PromQL: the rewritten string must parse again.
   * LogQL: Querdian parses the result again. The number of selectors must be equal to the expected
     number. Each selector must carry all matchers of a policy term, and keep an anchor matcher that is
     not empty.
   * TraceQL: Querdian makes sure that the policy filter is exactly one filter without hints. It parses
     the result again, and the result must contain the expected number of filters.
4. **Querdian never assumes precedence.**
   * Tempo evaluates `&&` and `||` with equal precedence, from left to right (verified). Thus the
     injected filter is `{ (X) && (dnf) }`, with parentheses around each disjunct.
   * The canonical LogQL rendering puts parentheses around each nested binary operation.
   * Querdian does not interpret the log pipeline again. It keeps the pipeline as the original token
     sequence.
5. **Semantic cover of each selector.**
   * Single term: Querdian injects the matchers into each selector. In PromQL, these are all vector
     selectors, also in range vectors and subqueries. In LogQL, these are all stream selectors and range
     aggregations. In TraceQL, these are all spanset filters, also in structural operators, pipelines,
     `select`, metrics functions and `compare`.
   * More than one term: Querdian or-lifts PromQL and LogQL metric queries, so that each copy of a
     selector carries exactly one term. It sends log queries one time for each term. If Querdian cannot
     lift a query exactly, it rejects the query.
   * Querdian joins user matchers to policy matchers with AND. User matchers can only narrow the result.
6. **Omission is not pass-through.** If a restricted user does not send `query`, `q` or `match[]`,
   Querdian sends the policy selector.
7. **Request-level consistency.** What Querdian parses is what it sends. The rewritten parameter
   replaces the original in a request that Querdian builds again from allow-listed parameters. There is
   no second channel, for example a difference between the body and the URL.
8. **Tests.**
   * Fuzz targets for the lexers and parsers: `FuzzLex` and `FuzzParseRoundTrip` in `pkg/logql` and
     `pkg/traceql`. They check that there are no panics and that `Parse(String(x))` is equivalent.
   * Fuzz targets for the injectors: `FuzzInject` in `pkg/logql`, `pkg/traceql` and
     `internal/enforce/promql`, and `FuzzOrLift`. Generated queries must parse again, and each selector
     or filter must carry a policy term.
   * Fuzz targets for the DNF normalisation (`FuzzNormaliseEquivalence`) and for the Prometheus labels
     response filter.
   * Run the fuzz targets with `make fuzz`.
   * The end-to-end suites pin the token boundaries that the injectors depend on, against the real
     backends (`parser-boundaries` in `test/e2e/loki` and `test/e2e/tempo`). Loki ends a `#` comment at
     LF only, and a `#` in a string does not start a comment. Tempo uses `\"` and `\\` as escapes in
     strings and in quoted attribute names. If a backend version changes these boundaries, these tests
     fail first.
   * Querdian renders the LogQL rewrite from its own tokens. Thus comment text never gets to Loki.

**Residual risk.**

* PromQL: the parser of Querdian is the Prometheus library version in `go.mod`. A backend of a different
  version (Thanos, Mimir) can parse a rare construct differently. The default parser options (no
  experimental syntax) make the difference smaller. Keep the library version near the version of your
  backends.
* LogQL and TraceQL: the parsers are clean-room. Nobody can prove that they are identical to the parsers
  of the backends.
  * Mitigation: `make e2e` runs differential and equivalence suites against real Prometheus, Loki and
    Tempo containers (`test/e2e/`). The suites pin the image versions, for example in
    `test/e2e/loki/seed_test.go`.
  * The suites send generated queries, also mutated ones, directly to the backend and through the proxy,
    under many policies. If the backend accepts a query, the proxy must enforce it or reject it.
    "Enforce" means that the result is equal to the result of a reference instance. That instance holds
    only the granted data. The backend must not reject a rewrite.
  * The suites were mutation-checked: if the injection or the redaction is broken, the suites fail.
  * The suites are evidence, not proof. They cover only the pinned versions. Until the suites pass
    against a new major version of Loki or Tempo, treat that version as untested.
  * There is no end-to-end test of the Grafana UI. The kind quickstart sends queries through the Grafana
    API, but it does not examine how Grafana shows the results.
* Querdian does not parse the queries of unrestricted users: they go to the backend unchanged. By
  definition, these users can read all data in their tenants.

## 5. Label oracle attacks (denied labels)

**Threat.** A user who must not see the label `customer_id` can try to learn its values in other ways:

* filter on it (`{customer_id="42"}`) and see if data returns;
* group by it, or join on it;
* make the backend echo it in a listing.

**Mitigations.** Querdian rejects each query that refers to a denied label with `403 denied_label`:

| Language | What Querdian examines |
|---|---|
| PromQL | `by`, `without`, `on`, `ignoring`, `group_*`, matchers, `label_replace`, `label_join`, `count_values` |
| LogQL | each identifier or keyword token, the string arguments of `label_replace`, and templates |
| TraceQL | each attribute token |

* Querdian parses a LogQL template with Go's `text/template/parse`. The template must contain only
  allow-listed nodes and functions. If a template does not parse, Querdian rejects it.
* Label listing endpoints do not show denied names. Value endpoints answer `403` for denied labels. This
  also applies to the `U__`-escaped form of the name that Prometheus 3 decodes (`U__customer__id`).
* If one matched rule denies a label, the label is denied for the whole request.

**Redaction (opt-in).** `labels.redact` also rewrites the values of a label in query results. The
strategies are `Remove`, `Placeholder`, `Hash` (with a key for each request) and `IPv4Prefix`.

* A redacted name is always also denied. Thus nobody can use it as a filter oracle.
* The strictest strategy of all matched rules applies.
* Querdian rewrites these results:
  * Prometheus query, range and exemplar results;
  * Loki query results (stream labels and categorize-labels metadata) and tail frames;
  * Tempo search and TraceQL metrics results, and trace by ID.
* If Querdian cannot decode a body, or the body is larger than `limits.maxMergeBytes`, Querdian answers
  with `502` and never forwards the body. Querdian holds back upstream error bodies
  ([section 3](#3-header-and-request-handling)).
* Querdian assigns placeholder numbers in random order, and the hash keys are random for each request.
  Thus they do not show the sort order of values, and they do not link values across requests.

**What this does not protect.**

* With `labels.deny` alone, the values of a denied label *are still in the label sets of results*. For
  example, a query for `up` returns `customer_id`. Deny limits what users can do with labels. It is not
  a redaction mechanism. The one exception is trace by ID, which removes denied attributes.
* With `labels.redact`, these channels remain:
  * the result cardinality, the series order and the timing of each query that the user can run;
  * with `Placeholder` and `Hash`: which entries of one response have the same value;
  * with `IPv4Prefix`: the octets that it keeps;
  * the content of Loki log lines, and labels that parsers derive from it under a different name
    (`| json` flattens nested keys);
  * values that the ingestion copied into other labels;
  * the trace-level intrinsics of Tempo in [section 6](#6-tempo-metadata-leaks-and-the-root-span-redaction-rules).

## 6. Tempo metadata leaks and the root-span redaction rules

**Threat.** The Tempo search API returns trace-level data from *all* spans of a trace, not only from the
spans that matched the injected policy. Thus a user whose policy admits a child span of a trace also gets
data about spans outside the policy. Tempo 2.9.0 returned these fields:

| Field | Content |
|---|---|
| `rootServiceName`, `rootTraceName` | data of the root span, also if only a child span matched |
| `serviceStats` | each service of the trace, with span and error counts |
| `durationMs` | the extent of the whole trace |
| `startTimeUnixNano` | the earliest start of a span in the trace |

**Rules in the code** (`internal/fanout`, `RootCovered` in `internal/enforce/traceql`):

1. Querdian buffers the search response (up to `maxMergeBytes`) and rewrites it as an **allow-list**.
   * For each trace, only `traceID`, `spanSet`, `spanSets` and two recalculated fields stay.
   * At the top level, only `traces` and `metrics` stay.
   * Querdian drops unknown fields, also fields that future Tempo versions add.
   * Querdian copies matched spans and their attributes byte for byte.
2. If Querdian does not know that the root span satisfies the policy, it removes `rootServiceName`,
   `rootTraceName` and `serviceStats`. It also removes the placeholder `<root span not yet received>`.
3. Querdian **calculates `startTimeUnixNano` and `durationMs` again, only from the returned spans that
   match**: the earliest matched start, and the extent to the latest matched end, rounded up to whole
   milliseconds.
4. The root span is covered only if the effective policy admits each span: unrestricted, or a term
   without matchers and without tenant binding. A matching policy is not sufficient. The reason: the
   injected filter limits only the *matched* spans, and the spans of one trace can have different
   resource attributes. To prove the attributes of the root, Querdian would need more queries.
5. Tag lookups:
   * Querdian removes denied tags from tag listings (`api/v2/search/tags`). Tag values endpoints answer
     `403` for a denied tag.
   * For some `q` shapes, Tempo answers both endpoints without the filter, and thus drops an injected
     policy (GHSA-jcqw-3c3f-xrmj).
   * Thus Querdian sends restricted lookups one time for each policy term, as a conjunction of `=`
     conditions only, and unites the results. Querdian refuses policies that it cannot write in this
     form.
6. Trace by ID (`traceByID: filter`):
   * Querdian gets the trace. It drops each span unless a policy term matches the attributes of the span
     and the attributes of its resource. A missing attribute never satisfies an operator, as in TraceQL.
   * Querdian removes empty groups, and removes denied attributes from the spans that stay.
   * An empty result is a `404` (also for v2).
   * The default is `deny`.
   * If Querdian cannot filter the response (unknown content type, a body that it cannot decode, a body
     that is too large), it answers `502`.

## 7. Trace intrinsics oracle (fixed)

**Threat.** Users can query the trace-level intrinsics `rootName`, `rootServiceName`, `traceDuration` and
their `trace:` forms (`trace:rootName`, `trace:rootService`, `trace:duration`) in spanset filters
(`docs/research/traceql.md` section 8). Without a check, these intrinsics are a boolean oracle:

* Take a trace in which at least one span satisfies the policy.
* A user can test if the name or the service of the redacted root span is equal to a value.
* A user can test if the trace duration is more than a threshold.

An end-to-end test against Tempo 2.9.0 confirmed this. The same information was also available through:

* the tag values of `rootServiceName`;
* `select`, `by` and metrics grouping;
* `compare()`, because its result has `rootServiceName` as a label dimension.

**Mitigation.** The enforcer rejects these intrinsics, `compare()` and the tag-value lookup of the root
names with `400`. This applies to each user whose policy does not cover the root span
(`traceql.RootCovered`: the policy is unrestricted or has a term without matchers). Thus, to give a team
trace-level queries, give it a policy that admits whole traces. Regression tests:
`internal/enforce/traceql/rootintrinsics_test.go` and `test/e2e/tempo`.

## 8. Resource exhaustion

| Vector | Mitigation | Remaining exposure |
|---|---|---|
| A policy with many terms, which causes fan-out | `limits.maxPolicyTerms` (16): `403 policy_too_complex`. The DNF simplification merges and drops redundant terms. | Operators who increase the limit. |
| Large policies that make each request or each snapshot rebuild expensive (a namespace admin, or a spoke team through its agent) | Size limits on each list and string of a policy, limits on the simplification, and a weight limit for regexes. See [policy size and regex cost](#policy-size-and-regex-cost). | Querdian does not limit the number of policy objects. See [policy size and regex cost](#policy-size-and-regex-cost). |
| Many upstream requests for one client request | `limits.maxFanout` (16) limits the requests and the parallelism: `403 query_too_complex`. All or nothing: one failure cancels the other requests. | A load multiplier of up to `maxFanout` for each request. |
| Large merged responses | `limits.maxMergeBytes` (64 MiB) for each request, and `limits.maxInflightMergeBytes` for each replica. See [merge memory](#merge-memory). | Memory can be more than the budget. A full budget rejects fan-outs. See [merge memory](#merge-memory). |
| Selector explosion in rewrites | 2048 selector copies for PromQL or-lifting, 256 for `match[]` expansion, a parser nesting limit of 100 (LogQL, TraceQL). | |
| Large request bodies | 1 MiB limit on form bodies. A `ReadHeaderTimeout` of 10 s on the proxy, management and dashboard listeners. | No limits on header size other than the Go defaults. |
| Slow backends, or backends that do not answer | `limits.upstreamTimeout` for each request. Dial and write timeouts on tails. | Idle client connections. |
| WebSocket tails | Upstream sockets for each term, limited by `maxFanout`. A 4 MiB frame limit. Limited dedupe memory (10 000 entries). If one side closes, all sides close. | Querdian does not limit the number of concurrent tails. |
| Metric cardinality | `endpoint` is a route template. User names are not labels. | |

Querdian has **no built-in rate limiting and no quotas for each user**. Apply them at the ingress, and use
the per-tenant limits of the backends.

### Policy size and regex cost

Querdian limits the size of each list and each string of a policy. It checks these limits at three
places: in the CRDs, in the compiler and in the receiver. The compiler reports `PolicyTooLarge`, also for
objects that were stored before the limits existed. These limits also apply:

| Limit | Value | Result or time of the check |
|---|---|---|
| Input of the DNF simplification (ResolveAt) | 4096 terms, 32768 matchers and 1 MiB of matcher text | before the simplification |
| Steps of the simplification | a fixed budget | |
| Weight of the simplified policy | 32768 | `403 policy_too_complex` |
| Weight of one regex matcher of a policy | 16384 | `PolicyTooLarge`, also in the receiver |
| Weight of all regexes of a policy | 32768 | `PolicyTooLarge`, also in the receiver |
| Weight of the regexes that one request compiles | 131072; `limits.maxRequestRegexWeight`, at most 262144 | `403 query_too_complex`, before any compile |
| Weight of the regexes of a PromQL or LogQL client query | 32768; `limits.maxQueryRegexWeight`, at most 262144 | `403 query_too_complex`, before any compile |
| Length of one client regex, or of one policy regex value | 8 KiB | before any parse |
| Length of the client regexes of a query | 32 KiB | before any parse |

Querdian counts the weight of the regexes of one request as
(2 x terms + 1) x client regex weight + (2 x selectors + 1) x policy weight.

**Regex weight.** The weight of a regex is a bound of its compile cost. Querdian calculates it before any
compile, from a parse with a work budget:

* The parser is a copy of the Go regexp parser with a work counter (`internal/regexparse`). It stops
  when its work is more than the limit.
* Each loop of the parser is charged or has a stated bound. This includes the height and size checks,
  which read wide subexpressions again at each level. It also includes the comparison of alternation
  branches, which can run again at each level of nested groups.
* The weight counts these items:
  * the instructions of the compiled program;
  * the rune ranges of each character class, counted again for each repetition;
  * the counted work of the parser, for example case folding, rune by rune;
  * for regexes that the Prometheus matcher can get: the runes of each class in the case-folding range
    of Unicode, because its printer examines each of them.
* A compile in Go and in the Prometheus matcher together takes at most about 256 KiB and 2 ms, plus
  2 KiB and 2 microseconds for each unit of weight. The measured maximum was about 900 bytes and
  0.9 microseconds for each unit.
* Querdian rejects a regex that does not parse, before any compile.
* The snapshot keeps only the sources of the regexes.

The length limits are a second layer that does not depend on the work count of the parser. At these
lengths, the slowest known parse path takes some tens of milliseconds.

The simplification and the snapshot rebuild do no work that grows with the square of the matchers of a
term. At these limits, one request used at most about 25 MiB and 25 milliseconds (`BenchmarkFullPath`).

**Remaining exposure.**

* Querdian does not limit the number of policy objects. Set a `ResourceQuota` in each team namespace, for
  example `count/accesspolicies.policy.querdian.dev: "20"`.
* The receiver flag `--max-policies` (default 2000) limits the `SpokePolicy` objects of each spoke.
* Many policies for one shared group can make the requests of that group fail with
  `403 policy_too_complex`.

### Merge memory

* `limits.maxMergeBytes` (64 MiB) is the total for each request.
  * Each branch counts each chunk of at most 32 KiB in a shared counter before it keeps the chunk.
  * The first chunk above the limit stops the read and cancels the other branches
    (`502 merge_too_large`).
  * Buffers grow only to what the branch can keep.
* `limits.maxInflightMergeBytes` (default `maxMergeBytes`) limits the fan-out bodies of all concurrent
  requests of a replica. The oldest fan-out that reads waits for space. The other fan-outs fail
  (`503 merge_budget_exhausted`).
* After the merge, the budget holds only the merged response. The write of the response has a deadline
  (10 s + 1 s for each MiB, at most 2 min). Thus a client that does not read cannot hold the budget.
* The Go memory limit is 90 percent of the container limit.
* Prometheus labels and series filtering use a fixed 16 MiB. Trace-by-ID and Tempo search buffers use
  `maxMergeBytes`.

**Remaining exposure.**

* All branches together can read up to 32 KiB each past `maxMergeBytes`.
* The budget counts only body bytes. The merge needs about 5 to 6 x the body bytes (measured). The
  single-request filter buffers are not in the budget. Thus memory is about
  6 x `maxInflightMergeBytes` plus `concurrency x maxMergeBytes` for single-request filters.
* A full budget rejects fan-outs. This is a denial of service, but it fails closed.

## 9. Token replay and authentication

* Querdian verifies the token on each request: the signature against the keys of the issuer, `exp`, the
  issuer (exact match) and the audience (any configured value). There is no session state.
* A stolen bearer token is usable until it expires, from each place that can reach the proxy. There is
  no token binding, no introspection and no revocation list. To decrease this risk, use:
  * short token lifetimes;
  * TLS on the path to Querdian (Querdian does not terminate TLS);
  * narrow audiences;
  * network restrictions.
* Querdian never forwards the token upstream and never logs it. Error messages contain fixed text only.
* If an identity provider issues tokens with `groups` or `preferred_username` claims that an attacker
  controls, the model breaks. The trust in the issuer is absolute. Use a dedicated audience for
  Querdian.
* A WebSocket tail authenticates only at the handshake. It continues after the token expires or the
  policy changes, until it closes. The one exception is the multi-cluster time limit
  ([section 10](#10-snapshot-staleness)). A tail closes in 10 seconds if a spoke whose rules gave its
  terms expires, is suspended, or gets a new `SpokeCluster` spec or UID.
* The issuer discovery runs in the background. Until it succeeds, all requests fail with `503`.

## 10. Snapshot staleness

Policies are in an immutable snapshot. After changes, Querdian builds the snapshot again from informer
caches (250 ms debounce, at most 2 s under a steady stream of changes). It swaps the snapshot
atomically. Each request sees the old or the new snapshot. The consequences:

| Topic | Behaviour |
|---|---|
| **Revocation delay** | Kubernetes watch latency + debounce + rebuild time, normally less than one second. Requests in progress and tails keep the policy that they started with (except the spoke expiry below). |
| **API outage or failed rebuild** | The previous snapshot stays in use. A failed rebuild runs again after one second. Thus revoked access stays while the API server is unreachable. This is the choice of availability over freshness. `querdian_snapshot_rebuilds_total{result="error"}` shows failures. |
| **Replicas converge independently** | For a short time after a change, two replicas can give different answers to the same user. |
| **Namespace label changes** (tenant assignment) | They are part of the snapshot and have the same delay. |
| **Invalid policies** | Querdian excludes them completely: they grant nothing. Querdian never applies a snapshot partially. |
| **Status conditions** | Only the leader writes them. They can be late or missing when the leader is down. They are information only: the enforcement does not use them. |

**Multi-cluster: spoke policies and the time limit.** A spoke policy gets to the hub through the agent
and the receiver. Thus its revocation takes longer.

* **Revocation delay of a spoke policy** = up to one agent heartbeat (default one minute) + the delay
  above. The limit of one changed sync in 10 seconds can add one retry.
* During an outage of the agent, the receiver, the network path or the hub API server, a revoked spoke
  grant stays active until the time limit.
* **Time limit.** The policies of a spoke stop when `SpokeCluster.status.lastSyncTime` is older than
  `maxStaleness` (default 48 hours). The check runs on each request against a deadline table outside
  the snapshot, because expiry makes no Kubernetes event.
* A spoke is also expired in these cases:
  * The spoke is suspended.
  * The deletion of the spoke has started.
  * The spoke has no valid sync of its current `metadata.generation`.
  * The spoke is not yet in the snapshot with its current version (UID and generation). Thus a change of
    `SpokeCluster.spec`, such as a smaller tenant allow-list, never applies late. This is also true for
    an object that was deleted and created again.
  * The `lastSyncTime` of the spoke is more than five minutes in the future.
* An expired rule keeps its `labels.deny` and `labels.redact`. Thus expiry only narrows access. A user
  with no terms left gets `403 policy_source_expired`.
* An open Loki tail closes in 10 seconds after a spoke in its terms expires, is suspended, or gets a new
  `SpokeCluster` version. Thus a smaller tenant allow-list also ends an open tail.
* A replica with the `multicluster` block is ready only after it has read all `SpokeCluster` objects.
* The time limit does **not** protect against a stolen agent cert, because the cert can renew
  `lastSyncTime` ([section 12](#12-multi-cluster)). Hub policies never expire.

## 11. Other considerations

* **Confused deputy through policies.** A user cannot make access for himself: only the objects in
  [section 1](#1-namespace-label-integrity-the-assumption-everything-rests-on) can grant access.
  Validation fails closed, thus an invalid policy grants nothing. An `AccessPolicy` cannot express
  `unrestricted`, tenants, or matchers outside its own namespace. The namespace matcher comes first and
  cannot be removed.
* **Cross-tenant union.** Querdian adds tenant bindings with `__tenant_id__` only where the backend
  supports it, and only if the header is a federation header. Where the backend does not support it,
  Querdian rejects the request. It does not widen the request.
* **Management port** (`/metrics`, `/healthz`, `/readyz`). It has no authentication. It shows policy
  counts, and request labels by endpoint and reason. Keep it in the cluster.
* **Management port, multi-cluster.** `querdian_cluster_policy_age_seconds` and
  `querdian_cluster_policy_expired` show the names of all spokes and their sync state.
* **Policy dashboard** (`--ui-listen`, off by default).
  * It shows group names, tenants and matchers.
  * It is read-only. Its query test bench is a dry run in memory that never sends a request to a backend.
  * Authenticate it (`--ui-auth=oidc`) or keep it on a trusted network. See [dashboard](../dashboard.md).
  * On a multi-cluster hub, it shows the policies of **all** spokes. Thus a dashboard user learns the
    subjects, matchers and tenants of each cluster. Give dashboard access only to persons who can see
    the policies of all clusters, for example with `--ui-admin-group`.
* **DNS rebinding and cross-origin requests against the dashboard.** Other web pages in the browser of a
  dashboard user must not read the dashboard or use the test bench. This is most important with
  `--ui-auth=none`, which has no authentication.
  * The dashboard accepts only an allowed host in the `Host` header: `localhost`, `127.0.0.1`, `[::1]`,
    the host of `--ui-listen` and each `--ui-allowed-host`. This applies to the API and to the static
    files, in all authentication modes.
  * A request with a method other than `GET` must also come from the origin of the dashboard.
  * Use `none` only on a loopback address or through a port-forward.
* **Privacy.** By default, Querdian does not write user names and query text to logs or traces. The two
  tracing flags that record them (`--tracing.record-user`, `--tracing.record-queries`) are opt-in.
  `querdian.policies` records policy names. See [section 11.1](#111-privacy-of-the-telemetry-of-querdian).
* **Supply chain.**
  * Dependencies are limited to Apache-2.0 and permissive licences. `make licenses` checks this.
  * Commits are signed off (DCO). CI includes CodeQL and OpenSSF Scorecard.
  * Releases have cosign signatures, SBOMs and build-provenance attestations. See the
    [self-assessment](self-assessment.md) and [releasing](../releasing.md).

### 11.1 Privacy of the telemetry of Querdian

The traces of Querdian go to a trace backend. That backend can have other readers than the protected
data. Thus the spans must not show the data that the policies protect, or the policies themselves.

* **Query text.** The rewritten query holds the matchers of the policy of the user. The original query
  can hold sensitive values. Without `--tracing.record-queries`, no span records query text: not the
  `querdian.*` attributes, and not the standard HTTP attributes.
  * The server span records only the path.
  * Each `upstream` client span records `url.full` with scheme, host and path only. This also applies
    to each fan-out request and to the Loki tail handshake.
  * Querdian removes the query string before the client span starts. Thus the attribute never holds it.
* **Paths.** Querdian records paths. They can hold values from the client, for example a trace ID or a
  tag name, but not the matchers of a policy.
* **Inbound trace context.** By default, the server span does not continue the trace of the client. It
  starts a new trace and links the trace context of the client. Thus a client cannot select the trace
  ID, cannot force sampling, and cannot put its spans in the same trace as the spans of Querdian.
  `--tracing.trust-inbound-context` removes this protection. Use it only if all clients that can reach
  the proxy are trusted. Querdian never sends client baggage to a backend.
* **Residual risk.** Querdian always records span names, `http.route`, decisions, policy names and
  counts. Give access to the trace backend only to persons who are permitted to know which policies
  exist.

## 12. Multi-cluster

With the `multicluster` block, the hub enforces the policies of spoke clusters
([multi-cluster](../multicluster.md)). This adds a write path for policy state, and new credentials.

**The receiver is a new attack surface.** `querdian receiver` is the only Querdian component with a
write path, and the spokes can reach it.

*Impact of a compromise:* a compromised receiver (its process, its service account or its Deployment)
can write any `SpokePolicy` for any `SpokeCluster` and renew `lastSyncTime`. Thus it can grant the data
of **all spokes** to any subject. It cannot grant hub data: a `SpokeCluster` with the name of the hub
cluster grants nothing, and spoke rules always carry the cluster matcher of a registered spoke. Protect
the namespace, the service account and the RBAC of the receiver as you protect the hub
`ClusterAccessPolicy` objects.

*Mitigations in the code:*

| Area | Mitigation |
|---|---|
| Separation | The receiver runs as a separate Deployment. Thus the read proxy has no write path. |
| Identity | The receiver terminates mTLS itself, with its own required client CA. It takes the cluster only from the URI SAN of the client cert (the protocol has no cluster field). It rejects a cert with no URI SAN or with more than one. The identity must be an exact match of `spec.identity` of exactly one `SpokeCluster`. The name of that `SpokeCluster` must be the last path segment of the identity. |
| Decoding | One path and one method. Strict decoding: the receiver rejects unknown fields, fields with a different case, duplicate keys and trailing data. It decodes in one pass over the tokens, with a limit on the number of JSON values (256 for each policy of `--max-policies`). Thus the memory of one request has a fixed upper limit. |
| Validation | The receiver checks each policy against the CRD rules in Go. It rejects the whole sync on any doubt. It rejects a body larger than `--max-body-bytes` and more than `--max-policies` policies. It also rejects a digest that does not match, more than one changed sync of a cluster in 10 seconds, and a suspended or deleted `SpokeCluster`. |
| Consistency | The receiver reads the `SpokeCluster` directly from the API server at the start of each sync and before each write. It writes `status` with a `resourceVersion` precondition. Thus a sync that overlaps a spec change validates nothing. |
| Other clusters | The receiver never changes or deletes an object of a different cluster. Object names are a hash of cluster, kind, namespace and name. |
| Request limits | Each identity can send a burst of six requests, then one request each 5 seconds (`429`). This limit applies before the receiver reads the body and before any read of the API server. It also applies to heartbeats and to identities that are not registered. `--max-in-flight` limits the requests in progress. One identity and one cluster can each have only one sync at a time. |
| Errors | Error messages are generic and never contain data of other spokes. |

*RBAC:* the receiver needs write access only to `spokepolicies` and `spokeclusters/status`. The proxy
needs read access and `spokepolicies/status` only. Only hub admins write `SpokeCluster.spec`.

*Remaining exposure:*

* There is no rate limit for each source address before the TLS handshake.
* The receiver does not load its certs again: a certificate rotation needs a restart.
* The compiler is a second line of defence, not an independent one. It checks the object name, the
  kind, the `SpokeCluster` and the tenant allow-list again.

**The agent cert.** An agent cert can write any grant for its own spoke, but not for a different spoke
and not for the hub. Its holder can also renew `lastSyncTime`. Thus the time limit gives no protection
against a stolen agent cert.

* The hub must control the client CA of the receiver. If a spoke can issue certs from this CA, it can
  impersonate other spokes.
* The agent cert must be dedicated. It must not be the ingest cert, and its identity must not be on an
  ingest cert. If it is, the theft of a write-only telemetry credential gives the power to grant read
  access.
* The `SpokeCluster` object links the agent identity to the cluster name. Only a hub admin writes it.
* The receiver rejects an identity on two `SpokeCluster` objects. It also rejects an identity whose last
  path segment is not the name of its `SpokeCluster`. A suffix of a segment does not match.
* There is no certificate revocation list. A hub admin handles a stolen agent cert with the procedure in
  [multi-cluster](../multicluster.md#stolen-agent-cert):
  1. Suspend the spoke.
  2. Find the cause of the theft.
  3. Issue a cert with a new identity.
  4. Delete the `SpokePolicy` objects of the spoke.
  5. Only then, unsuspend the spoke.

  Grants come back only after a new valid sync. The time limit gives no protection here, because the
  holder of the cert can renew `lastSyncTime`.

**The spoke admin.** A spoke admin can grant the data of that spoke to any subject. This is by design.
These rules prevent more:

* Spoke rules always get `<clusterLabel>="<spoke>"`.
* A spoke rule never compiles to unrestricted. Thus Querdian always parses and rewrites the queries of
  its users.
* `allClusters` is not valid on spoke rules.
* Querdian joins a spoke matcher on the cluster label with AND, thus it can only narrow the access. A
  fuzz target pins this after the DNF normalisation.
* A spoke policy can only use tenants in `SpokeCluster.spec.tenants`. Hub namespace labels never apply
  to spoke policies.
* A `SpokeCluster` with the name of the hub cluster grants nothing.
* Each hub `ClusterAccessPolicy` must state its cluster scope as a fixed set of names, `allClusters` or
  `unrestricted`. Thus a new spoke never widens a hub grant that exists.
* *Remaining exposure:* a spoke admin who names a shared group can decrease the access of that group in
  other places (denied labels apply to the whole request, more terms, cross-tenant unions). See
  [limitations](../limitations.md#multi-cluster). No data leaks through this.

**Status back to the spoke.** The receiver returns only the `Ready` conditions of the policies of the
spoke itself. It replaces `CrossTenantUnionUnsupported` with `Compiled`, because that message can show
the grants of a shared subject in other tenants.

**Rollback.** Without the `multicluster` block, a hub `AccessPolicy` has no cluster matcher and matches
namespace `X` of each spoke. Querdian refuses to start if a backend keeps `clusterLabel` without the
block. But an operator who removes both opens this path again while spoke data is in the backends
([multi-cluster](../multicluster.md#rollback)).

## 13. Summary of residual risks

1. Namespace-label integrity, and RBAC on policies and namespaces, are outside the control of Querdian
   ([section 1](#1-namespace-label-integrity-the-assumption-everything-rests-on)).
2. The clean-room LogQL and TraceQL parsers can still diverge from the backends in corners that nobody
   examined. The differential suites cover only the tested backend versions
   ([section 4](#4-query-injection-and-parser-divergence)).
3. Denied labels do not redact results unless you use `labels.redact`. Redaction does not cover log
   lines or cardinality ([section 5](#5-label-oracle-attacks-denied-labels)).
4. Policies that cover only part of a trace depend on the root-intrinsic restriction
   ([section 7](#7-trace-intrinsics-oracle-fixed)).
5. Bearer tokens are valid until they expire. Open tails continue after the token expires and after
   policy changes. But they close when a spoke in their terms expires, is suspended or gets a new spec
   ([section 9](#9-token-replay-and-authentication)).
6. Policy revocation is eventually consistent. Revoked access stays during API outages
   ([section 10](#10-snapshot-staleness)).
7. The proxy has no built-in rate limiting ([section 8](#8-resource-exhaustion)).
   * The fan-out bodies of all requests share one budget. Thus many concurrent fan-outs fail with `503`.
   * The single-request filter buffers are not in that budget, and they grow with concurrency.
   * Only the multi-cluster receiver limits the requests of each agent identity
     ([section 12](#12-multi-cluster)).
8. Multi-cluster:
   * The isolation between spokes depends on the ingest gateway, which must overwrite the cluster label
     ([section 1](#1-namespace-label-integrity-the-assumption-everything-rests-on)).
   * A compromised receiver can grant the data of all spokes ([section 12](#12-multi-cluster)).
   * A stolen agent cert can grant the data of its spoke until a hub admin suspends the spoke
     ([section 12](#12-multi-cluster)).
   * A revoked spoke policy stays active for up to one heartbeat, or up to the time limit during an
     outage ([section 10](#10-snapshot-staleness)).
