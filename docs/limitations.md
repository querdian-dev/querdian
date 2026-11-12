# Known limitations

This page lists the requests that Querdian rejects, the results that are approximate, and what the
policy model does not do. Use it to tell an intended rejection from a bug.

In short:

* Querdian rejects a request when it cannot prove that it handles the request exactly as the backend
  does, or cannot express the exact union of the policies of a user. It does not pass the request
  through and it does not guess.
* Most rejections are `400 bad_data` (`parse_error`, `unsupported` or `bad_request`) or `403 forbidden`.
  [troubleshooting.md](troubleshooting.md#decision-reason-codes) explains the reason codes.
* `labels.deny` alone does not remove a label from query results (except trace by ID). `labels.redact`
  does.
* Each limit comes from the code that enforces it. If a limit blocks a legitimate use case, open an
  issue. A relaxed rule always needs a parser or rewriter change, and tests.

Contents: [general](#general-request-handling) / [PromQL](#promql) / [LogQL](#logql) /
[TraceQL and Tempo](#traceql-and-tempo) / [denied labels](#denied-labels) /
[approximate results](#approximate-results) / [policy model](#policy-model) /
[multi-cluster](#multi-cluster) / [authentication](#authentication-and-sessions) /
[tracing](#tracing)

## General request handling

| Limit | Effect for the user | What to do |
|---|---|---|
| **Default deny**: a path that is not in the endpoint tables, or a known path with a wrong method | `403 endpoint_not_allowed`. The paths denied on purpose are below this table. | Use an endpoint of [endpoints.md](endpoints.md). |
| **Canonical paths only**: no escaped slashes (`%2f`), backslashes, `%00`, `%` left after the decode, `.` or `..` segments, empty segments (`//`), trailing slashes, control characters or invalid UTF-8 | `400 bad_path` | Send the canonical path. |
| **Parameters are allow-listed for each route** | An unknown parameter is `400 bad_request`; Querdian does not drop it silently. `storeMatch[]` (Thanos) is never forwarded. | Remove the parameter. Allowed Thanos extras: `dedup`, `partial_response`, `replicaLabels`, `replicaLabels[]`, `max_source_resolution`. |
| **A single-valued parameter more than one time**: a name that does not end in `[]` (such as `query`, `q`, `limit`, `targetLabels`), repeated, or in the URL and the form body together | `400 bad_request`. Backends do not agree about which value wins, so Querdian does not guess. | Send the parameter one time. |
| **`POST` bodies**: only `application/x-www-form-urlencoded`, at most 1 MiB | A JSON body is rejected. The proxy always encodes the rewritten request again (`GET`: URL query, `POST`: form body). | Send a form body. |
| **`GET` only**: Prometheus `label/<name>/values`; Loki `labels`, `label/<name>/values`, `tail`, `patterns` | Other methods are denied. Most other query endpoints accept `GET` and form `POST`. | Use `GET`. |
| **Merged responses are JSON only** | See [Merged and buffered responses](#merged-and-buffered-responses). | - |
| **Unrestricted users are not parsed**: an unrestricted effective policy (or a term without matchers), and no denied labels | The query goes to the backend unchanged, except the tenant header and the parameter allow-list. Syntax errors then come from the backend. | - |
| **Upstream errors** | An error of a **single streamed** upstream request goes to the client with the upstream status, and counts as `allow` in `querdian_requests_total` (for example a Loki `400` for a log query on the instant `query` endpoint). A failure of a **fan-out** branch is `502` for the full request. | - |
| **Grafana features that need denied endpoints degrade** | The help text of the Prometheus metrics browser (`metadata`) is not available. Tempo gRPC streaming search is not supported. | Disable streaming in the Grafana Tempo data source. |
| **No rate limit and no queue in the proxy** | Only `querdian receiver` in a multi-cluster setup limits the requests of each agent identity. | See [operations.md](operations.md#scaling). |
| **Fan-out body limits**: `limits.maxMergeBytes` for each request (counted while the branches read), `limits.maxInflightMergeBytes` for all requests (default: one `maxMergeBytes`) | A merged response larger than `maxMergeBytes` is `502 merge_too_large`, also when each body was under the limit. | See [Merge budget](#merge-budget). |
| **Slow clients**: Querdian writes a merged response with a deadline | A client that reads more slowly than about 1 MiB/s does not get the complete response. | - |
| **No TLS to backends**: the proxy does not terminate TLS, and authenticates to backends only with the tenant header | - | Isolate the backends at network level. |
| **TLS of `querdian receiver`**, the only component that terminates TLS ([multi-cluster](multicluster.md)) | It requires an mTLS client certificate. It reads its server certificate, key and client CA one time at startup. It has no certificate revocation list. | Restart the receiver after a certificate rotation. |

**Endpoints denied on purpose:** write and ingest paths (`push`, `write`, `otlp`, `read`), rules and
alerts, admin and status pages, `targets`, `scrape_pools`, Prometheus `metadata` (it cannot be
restricted by label), `search/*`, `notifications`, `features`, `/openapi.yaml`, Loki `delete`, the
deprecated `/api/prom/*`, Tempo v1 `search/tags` and `search/tag/<name>/values` (no `q` support),
`v2/traces/diff`, `overrides`, `mcp`, and the internal endpoints of all three backends. The inventory
is in [endpoints.md](endpoints.md).

### Merged and buffered responses

* Plans that fan out or buffer (all Loki fan-out endpoints, Tempo search) send
  `Accept: application/json` upstream. Querdian never merges Loki Parquet or Tempo
  `application/vnd.grafana.llm`.
* Plans with a single streamed request send the `Accept` header of the client through.
* Prometheus `labels` and `series` with denied labels, and trace by ID, buffer and rewrite the body. A
  body that they cannot decode (unexpected media type, compression other than gzip for traces,
  malformed JSON) is a `502`, never an unfiltered pass-through.

### Merge budget

When `limits.maxInflightMergeBytes` is full, only the oldest fan-out that still reads waits for space.
Each other fan-out fails with `503 merge_budget_exhausted`, and the client must try again. Thus, with
the default, two large fan-outs at the same time give one `503`. The budget counts body bytes only.
Buffer growth, the merge step, the response filters and the trace-by-ID buffer use more memory that it
does not count (see [operations.md](operations.md#scaling)).

## PromQL

Querdian parses queries with the Prometheus parser and its **default options**, and parses a rewritten
query again. With one policy term, each query that parses is supported. With **several** terms,
Querdian or-lifts the query, which is exact only for some query forms. The rewrite and its deviations
are in [promql-enforcement.md](promql-enforcement.md).

| Query form | One term | Several terms |
|---|---|---|
| Experimental functions, duration expressions, extended range selectors, fill modifiers (not enabled) | `400 parse_error` | `400 parse_error` |
| A range vector that is not the direct argument of a function, for example the full query `up[5m]` | supported | `400 unsupported` |
| A function over a range vector with a different argument that contains a selector, for example `quantile_over_time(scalar(x), y[1h])` | supported | `400 unsupported` |
| A function with several range-vector arguments | supported | `400 unsupported` |
| An expression node type that the lifter does not know (future syntax fails closed) | supported | `400 unsupported` |
| A rewritten query that does not parse again | `400 unsupported` | `400 unsupported` |

| Limit | Effect for the user |
|---|---|
| **`absent()` and `absent_over_time()`**: Prometheus gets the labels of the result of `absent(m{job="x"})` from the equality matchers of the selector | With several terms, the argument is an `or` expression, so the result has **no labels**. With one term, the labels include the equality matchers of the policy. The value (1 if no series exists in the union) stays exact, and no data leaks. `absent_over_time(m[5m])` becomes `absent(last_over_time(m{t1}[5m]) or ...)`. |
| **A function that drops the metric name**: Prometheus fails when two series differ only in their name | Across terms, `or` resolves such a collision silently. This needs a term that matches on `__name__`. |
| **Selector copies**: an or-lifted query has (selectors x terms) copies | More than 2048 is `403 query_too_complex`. |
| **Metadata endpoints**: Querdian expands (user `match[]` x terms) selectors | More than 256 is `403 query_too_complex`. |
| **`match[]` selectors** need a matcher that does not match the empty string | If the user and the policy supply none, Querdian adds `__name__=~".+"`. This can only narrow. |
| **`series` and `labels` responses with denied labels** | Querdian buffers and filters them, up to 16 MiB. |
| **`query_exemplars`** | The result is the union of the exemplars of the series that the lifted selectors match. |

## LogQL

`pkg/logql` is a clean-room parser, written from the public documentation and black-box tests
(`docs/research/logql.md`). It splits log pipelines into tokens and validates their structure, but does
not interpret them. Anything that it does not recognise is `400 parse_error`. There is no lenient mode.

### LogQL syntax

| Element | Rule (a violation is `400 parse_error`) |
|---|---|
| String escapes | Only `\" \\ \a \b \f \n \r \t \v \xHH \NNN` (octal at most 255). `\u`, `\U`, `\'` and all other escapes are errors. |
| Strings | Single-quoted strings, raw control characters (except tab) and invalid UTF-8 are errors. Backtick strings are accepted. |
| Identifiers | `[A-Za-z_][A-Za-z0-9_]*`. A stray `:` or `.` is an error, so a label such as `k8s.namespace.name` cannot be written. Use the normalised `k8s_namespace_name`. |
| Numbers | Decimals with an optional fraction and exponent. No hex, `Inf` or `NaN` literals. |
| Durations | Groups of number and unit with `ns us µs μs ms s m h d w y`, lower case, no spaces. |
| Bytes | One number and one unit of `B k kB KB Ki KiB M MB Mi MiB G GB Gi GiB T TB Ti TiB`. `1kb`, `2PB` and compound bytes are rejected. `1m` is a duration and `1M` is bytes. |
| Flags | The lexer makes separate tokens of flags such as `--strict`; the parser puts them together again. |
| Range selectors and `offset` | Integer durations without the unit `us`. `[1.5m]`, `[1us]` and subqueries (`[5m:1m]`) are errors. `offset` must directly follow the range inside the function. |
| Binary operators | Literals as operands of `and`, `or`, `unless` are errors. `bool` is only valid on comparisons. `group_left` and `group_right` require `on` or `ignoring`, and are not allowed on set operators. Querdian assumes that `and` and `unless` have the same precedence. |
| Stream selectors | At least one `=` or `=~` matcher that cannot match the empty string (or `!= ""`). Regexes must compile (as in Loki). |
| `logfmt`, `json` | `logfmt` accepts exactly `--strict` and `--keep-empty`, each at most one time. `json` accepts no flags, and each parameter needs `="expr"`. |
| `unwrap` | Only label filters can follow it. An unwrapped range function needs `unwrap`, and `unwrap` needs an unwrapped range function. **`rate(... \| unwrap x [r])` is rejected**, because it is not documented. A `by` or `without` clause after the parentheses is accepted only for unwrapped functions. |
| Function arguments | `topk`, `bottomk` and `vector` take plain numbers. `label_replace` takes exactly five string literals. |
| Query structure | A log query must start with `{`. A bare or parenthesised selector as operand of a metric expression is an error. Nesting deeper than 100 levels is rejected. |
| Keywords | Case insensitive. A keyword can be a label name where this is not ambiguous. |

| Kind | Supported functions |
|---|---|
| Range aggregations | `rate`, `count_over_time`, `bytes_rate`, `bytes_over_time`, `absent_over_time`, `rate_counter`, `sum_over_time`, `avg_over_time`, `max_over_time`, `min_over_time`, `stdvar_over_time`, `stddev_over_time`, `quantile_over_time`, `first_over_time`, `last_over_time` |
| Vector aggregations | `sum avg min max count stddev stdvar topk bottomk sort sort_desc` |
| Other | `vector`, `label_replace` |

**`distinct`, `approx_topk`, `abs`, `count_values` and all other functions are rejected.**

### LogQL enforcement

| Limit | Effect for the user | What to do |
|---|---|---|
| **Metric queries with several terms**: Querdian or-lifts each range aggregation | A range aggregation with `by` or `without` cannot be lifted: `400 unsupported`. | Write an outer `sum by (...)` over a plain range aggregation. |
| **Log queries with several terms**: one request for each term, up to `maxFanout` | Above `maxFanout`: `403 query_too_complex`. Entries in identical streams are deduplicated by `(timestamp, line)`, sorted by direction and cut to `limit`. All branches use the same parameters. An invalid or inconsistent upstream response is `502`. | - |
| **Metadata selectors**: the optional `query` of `labels`, `label/<name>/values`, `detected_*`, `index/*`, `patterns`, and `match[]` of `series`, must be a **log selector** (optional pipeline) | A metric query is `400 unsupported`. Without the parameter, restricted users get the policy selector, and unrestricted users get no selector (`detected_fields` then fails upstream, as in Loki). | - |
| **`tenantIdLabel`**: each selector must keep a matcher other than `__tenant_id__` (Loki rejects a selector with only that matcher) | A policy-only selector for a term that has only the tenant binding is `400 unsupported`. | - |
| **`tail`** | Only log queries. Only the parameters `query`, `limit`, `start`, `delay_for` (`limit >= 1`, `delay_for` 0 to 5 seconds). Querdian refuses cross-origin WebSocket handshakes, so browser clients must come from the same host. Each upstream frame has a limit of 4 MiB. Duplicates are removed with a memory of the last 10 000 entries. An invalid frame closes the tail. The tail keeps the policy and token of its start ([exceptions](#authentication-and-sessions)). | - |
| **`patterns`** | Loki returns 404 unless the pattern ingester is enabled. The non-empty response shape is not verified against a live Loki. | - |
| **`X-Loki-Response-Encoding-Flags: categorize-labels`** | The stream merge keeps this encoding. Branches that do not agree on it fail the request. | - |

When a pipeline makes distinct streams into the same label set (`drop`, `keep`), the counts of the
terms after the first can be too low, because `or` keeps only the first of identical label sets. This
never grants more than the policy permits.

### LogQL results with Loki 3.5.0

The end-to-end suite found these results:

| Case | Effect for the user |
|---|---|
| **Metadata endpoints under a tenant-bound policy** (rules that are different for each tenant, so terms have a `__tenant_id__` matcher) | `labels`, `label/<name>/values`, `series`, `index/stats` and `index/volume` do not work reliably. Loki 3.5.0 ignores a negative `__tenant_id__` matcher there, and a positive one matches nothing. Results are empty or wrong, never wider than the policy. Metric queries are not affected (they bind with a negative matcher). A correct fix needs one request for each tenant. |
| **Multi-tenant grants** (header `t1\|t2`) | They do not work for `tail` (Loki: "multiple org IDs present"), `series` (empty result) or `detected_labels`. |
| **Duplicate collapse**: a pipeline gives different streams identical labels and lines (`keep`, `drop`, `line_format`) | The fan-out merge of log queries with several terms, and the `or` of an or-lifted metric query, collapse them: fewer entries or lower counts than the union of the results of each term. The result never contains data outside the policy. |
| **`absent_over_time` with several terms** | One series for each term. |

**Rejected, although Loki accepts them:**

* a range aggregation with `by` or `without` when the policy has several terms, for example
  `quantile_over_time(... | unwrap x [5m]) by (app)`, also `max_over_time`, `min_over_time`,
  `stddev_over_time`, `last_over_time`;
* parenthesised log queries (`( {app="web"} |= "x" )`);
* `bool` on arithmetic operators;
* selectors with no positive matcher at all.

## TraceQL and Tempo

`pkg/traceql` is a clean-room front end. It must only find spanset filters and attributes, so it
validates more than it interprets. Where Tempo accepts something that is not documented or different,
Querdian rejects it (`400 parse_error`): a token stream that Tempo reads differently could hide an
attribute from the policy checks.

### TraceQL syntax

| Element | Rule (a violation is `400 parse_error`) |
|---|---|
| Comments | `#`, `//` and `/* */` are errors. |
| Strings and quoted names | Only the escapes `\"` and `\\`. Backtick strings, single-quoted strings, control characters and invalid UTF-8 are errors. |
| Unquoted attribute names | Letters, digits, `_` and `.`. A name followed by `-` or `/` is an error ("quote the name"), because it could be arithmetic. |
| Scopes | A scope before `.` must be `span`, `resource`, `event`, `link` or `instrumentation`. **`parent.*` is rejected**, and so are upper-case and unknown scopes. A scope before `:` must be a known intrinsic scope with a known intrinsic name. |
| Durations | One number and one unit (`ns us ms s m h d`). `1h30m`, glued numbers and exponents are errors. |
| `&` | A lone `&` is an error. |
| Query structure | A pipeline of stages: spanset expressions, scalar filters, `by(attr)`, `select(...)`, `coalesce()`, one metrics function (optionally followed by `topk` or `bottomk`) and a trailing `with (...)`. |
| Filters | Must be boolean. Comparisons need an attribute or intrinsic. Regex operators need an attribute on the left and a string literal on the right. `nil` only as `attr != nil`. Enum values (`error`, `ok`, `client`, ...) only against `status` or `kind` with `=` or `!=`. `minInt`, `maxInt` and unary `!` are rejected. |
| Spanset operators | Different spanset operators on one level without parentheses are rejected. Parentheses can only wrap spanset expressions. Arithmetic between metrics queries (`100 * ({..} \| rate()) / ({} \| rate())`) is rejected. |
| Arguments | `by()` takes exactly one attribute. `select()` and metrics arguments are single attributes. `with` values are literals. Nesting deeper than 100 levels is rejected. |

### TraceQL enforcement

| Limit | Effect for the user |
|---|---|
| **Policy injection** into **each** spanset filter, also in structural operators, pipelines, `select`, metrics functions and `compare(...)` | `{ X }` becomes `{ (X) && (dnf) }`, `{}` becomes `{ dnf }`, and a query with no filter gets the prefix `{ dnf } \| `. `&&` and `\|\|` have equal precedence in Tempo, so the injected text has full parentheses. |
| **Legacy search parameters** `tags`, `minDuration`, `maxDuration` | `400 unsupported`, with or without `q`. |
| **`q`** | Required on the `metrics/*` endpoints (`400 unsupported`). Optional on search and tag endpoints: without it, restricted users get a policy-only filter. |
| **Tenant-bound terms** | Not supported on Tempo (`400 unsupported`): there is no `__tenant_id__` attribute. Cross-tenant unions with different terms are `403 cross_tenant_union_unsupported`. |
| **Absent attributes**: a span without an attribute satisfies none of `=`, `!=`, `=~`, `!~`, and a non-string attribute never satisfies a string comparison | A policy `NotEqual` or `RegexNoMatch` admits less on Tempo than on Prometheus or Loki. A policy cannot select "attribute absent". Policy attribute names must have a scope (`resource.x`, `span.x`). |
| **Search responses** | Querdian removes and recomputes trace-level fields: see [Search response redaction](#search-response-redaction). |
| **Trace-level intrinsics** `rootName`, `rootServiceName`, `traceDuration` (and their `trace:` forms), `compare()`, and the tag-value lookup of the root names | `400` for users whose policy does not cover the root span (not unrestricted, no term without matchers), because they would let the user probe redacted root data ([security/threat-model.md](security/threat-model.md)). |
| **Trace by ID** | See [Trace by ID](#trace-by-id). The default mode is `deny`. |
| **Tag endpoints** (`api/v2/search/tags`, `api/v2/search/tag/<name>/values`) | Planned for each term: see [Tag endpoints](#tag-endpoints). |
| **Protocols** | Only the HTTP API is proxied. gRPC and streaming search are out of scope. |

#### Search response redaction

Tempo reports trace-level data even if only a child span matched: root span name and service, span and
error counts of each service, trace start and duration. For each user who is not unrestricted and has
no term without matchers:

* Querdian **removes** `rootServiceName`, `rootTraceName` and `serviceStats`.
* Querdian **recomputes** `startTimeUnixNano` and `durationMs` from the returned spans that match only:
  earliest start, and duration from the earliest start to the latest end, rounded up to whole
  milliseconds. They are thus not the real trace start or duration.
* The Grafana result table shows no root name for such users.
* Only `traceID`, `spanSet`, `spanSets` and the two recomputed fields stay for each trace, and only
  `traces` and `metrics` at the top level. Fields that future Tempo versions add are dropped.
* The root span is intentionally not treated as covered when a policy matches a span of the trace,
  because spans of one trace can have different resource attributes.
* The body is buffered (up to `maxMergeBytes`).

#### Trace by ID

With `traceByID: filter`:

* `q` and each parameter except `start` and `end` is `400`.
* Only JSON and protobuf (`application/protobuf`) responses are filtered, and gzip is decoded. Anything
  else is `502`.
* Spans outside the policy are dropped, then empty scope and resource groups. If nothing remains, the
  answer is `404` with an empty body, **also on `v2`**, where Tempo itself answers 200 with an empty
  trace.
* Denied attributes are removed from the spans that remain.
* The filter evaluates string attributes of the span and its resource.
* Unknown protobuf fields of the wrapper `Trace` message are dropped.
* The buffered body has a limit of `maxMergeBytes`.

#### Tag endpoints

Tempo does not evaluate each `q` on the tag endpoints. For a filter shape that it cannot use there, it
answers **unfiltered**, which would also drop an injected policy (GHSA-jcqw-3c3f-xrmj). Black-box tests
found this for terms joined by `||` (Tempo 2.9.0 and 2.10.10) and for `=~` (Tempo 3.1.1).

Thus, for restricted users, Querdian does not inject the policy as a DNF. It plans it **for each term**:
each request has one filter of `attribute = "string"` conditions joined by `&&`, and Querdian unites the
answers. There is one upstream request for each term, up to `maxFanout`.

| Policy or query | Result |
|---|---|
| A policy regex that matches a fixed set of values: several `=` matchers on one attribute (which the policy model merges into a regex), or `ns-(a\|c)` | Expanded into equalities. |
| A policy with `!=`, `!~` or an open regex (`ns-.*`) | Cannot be expressed. Tag lookups are `400 unsupported` for such users, and Grafana autocomplete then offers no values. Search, trace by ID and metrics are not affected. |
| A user `q` that is one filter of `span.x = "v"`, `resource.x = "v"` or `status = error\|ok\|unset` conditions joined by `&&` | Kept. |
| Any other user `q` | **Ignored**. The answer lists the tags (or values) of all granted spans, not of the spans that match `q`. |
| Denied tags | Removed. |
| The tag name of a values lookup | Must be unquoted (`span.x`, `.x`). `span."x"` is `400`. |

## Denied labels

| Limit | Effect for the user |
|---|---|
| **What a denied label does** | The user cannot filter, group or join on the label, and it is not in label and series lists. **`labels.deny` alone does not remove the label from query results** (except trace by ID); `labels.redact` does. See [policy-guide.md](policy-guide.md#denied-labels). |
| **Conservative union** | A label that any matched rule denies is denied for all terms. |
| **Redaction** (`labels.redact`) buffers the full response | Bounded by `limits.maxMergeBytes`. |
| **Redaction rewrites label sets only** | Loki log lines, values copied into other labels, and the number and order of returned series are not hidden. Grafana can show several series with the same legend under `Remove`. |

### Denied-label scan false positives

The PromQL check works on the syntax tree and has no false positives. The LogQL check is a token scan
plus a template check, and rejects too much on purpose:

| Check | Effect for the user |
|---|---|
| **LogQL token scan**: a denied name is compared with each identifier **and keyword** token, at any position in the query | A label named like a keyword or function (`json`, `count`, `rate`, `level` in an unrelated clause) blocks legitimate uses of that word. Do not use such label names. |
| **LogQL `line_format` and `label_format` templates**: each template is parsed with Go `text/template/parse`, and each node must be in an allow-list | See the list below. |
| **LogQL `label_replace`** | The `dst` and `src` string arguments are compared with denied names. The regex and the replacement are not. |
| **TraceQL**: a denied scoped name denies the unscoped `.x` form and the other way around; denied tag lists treat the intrinsic scope as "any scope" | This can hide more tags than necessary. |

The template check:

* A template that does not parse is rejected.
* A template with a control character other than tab and newline (for example CR), or with invalid
  UTF-8, is rejected, because Loki can decode these characters differently.
* The allow-list holds text, actions, pipelines without variables, `if`/`else`, constants, fields with
  one name (`{{ .app }}`) that is not denied, and the Go and Loki template functions that cannot take
  or build a label name. A field that names a denied label is rejected.
* As soon as any label is denied, all other nodes are rejected, also if the denied label has no
  relation to the query: `.`, `$` variables, chained fields (`{{ .a.b }}`), `range`, `with`,
  `template`, `block`, `define`, and the functions `index`, `slice`, `call` and unknown functions.
* Literal text and string constants are not compared with denied names.

## Approximate results

When the effective terms are **not provably disjoint**, a record that matches several terms counts one
time for each term in summed results. Querdian recognises disjoint terms only for different tenants,
or for `Equal` matchers on the same label with different values. It does not detect regex terms that
are disjoint, so it marks such results as approximate, although they can be exact.

| Result | Merge | Exact? |
|---|---|---|
| Loki `index/stats`, `index/volume`, `index/volume_range`, `patterns` | Counters are summed for each term. Denied labels are removed from the reported label sets, which can merge series. | If the terms are not provably disjoint, the response gets the header `X-Querdian-Approximate: true` and the server span `merge.approximate=true`. The body keeps the Loki format. The cut to `limit` after the sum is exact only if no branch cut off series. |
| Loki `index/volume_range` shape | A `matrix`, unless each branch answered a `vector` of one and the same step (the single-step shape of Loki), which is kept. | - |
| Loki `stats` blocks in query responses | Summed over the fan-out branches. | Shared work counts one time for each branch. |
| Loki `detected_labels`, `detected_fields` | Cardinalities are summed for each label. Field types that do not agree become `string`. | Exact for disjoint terms, otherwise an upper bound. |
| Loki log stream merges | The first `limit` entries of the union are the union of the first `limit` entries of each branch. | Exact. |

## Policy model

| Limit | Effect for the user | What to do |
|---|---|---|
| **One invalid rule** | It disables its full policy on all its backends. | - |
| **An invalid policy, or a policy for an unknown backend** | It grants nothing. There is no partial application. | - |
| **Namespace labels**: policy compilation uses them only for `policy.querdian.dev/tenant` | - | Restart after a change of the `namespaceLabel` of a backend. |
| **Fixed size limits of a policy**, for example 64 rules, 64 matchers for each rule, 16384 characters for each matcher value ([full list](policy-guide.md#size-limits)) | A policy over a limit grants nothing (`PolicyTooLarge`). The limits are not configurable. | - |
| **Regex weight of a policy**: at most 16384 for a regex matcher, 32768 for all regexes of one policy | `PolicyTooLarge` | See [Regex weight](#regex-weight). |
| **Work for one request**: before the simplification, the matched rules can give at most 4096 terms, 32768 matchers and 1 MiB of matcher text. The simplification has a fixed budget of steps. The simplified policy can have a weight of at most 32768. | `403 policy_too_complex`, also if the simplified policy would have few terms. Many policies for one shared group can reach these limits: for example, about 300 namespaces with names of 200 characters merge into a regex with a weight over 32768. | - |
| **Large merged policies limit the selectors of a query**: the regexes that the proxy compiles for one request can have a weight of at most 131072 ([Regex weight](#regex-weight)) | `403 query_too_complex` | Split the query into queries with fewer selectors, use fewer or shorter regexes, or make the policy smaller. Operators can change `limits.maxRequestRegexWeight` ([operations.md](operations.md#limits)). |
| **Regex bytes**: a client regex at most 8 KiB, all client regexes of one query at most 32 KiB, a policy regex value at most 8 KiB | Client: `403 query_too_complex`. Policy: `PolicyTooLarge`. This second layer does not depend on the work count of the regex parser. `Equal` values can still have 16384 characters. | - |
| **Client regexes of a PromQL or LogQL query**: weight at most 32768 together (`limits.maxQueryRegexWeight`; all selectors, all `match[]` values) | `403 query_too_complex`. Not for TraceQL client regexes: only Tempo compiles them, not the proxy. | - |
| **Terms of the effective policy**: more than `maxPolicyTerms` | `403 policy_too_complex`. The DNF simplification only merges terms that differ in one `Equal` or `RegexMatch` label, and drops terms whose matcher set is a superset of a different term. It does not reason about regexes that overlap. | - |
| **No deny policies** | - | To exclude data from a broad rule, write the rule narrower. |
| **Cross-tenant unions with different terms** | Possible only on backends with `tenantFederation: tenantIdLabel` ([policy-guide.md](policy-guide.md#tenants)). | - |

### Regex weight

Querdian gets the weight of a regex from a parse with a work budget, before any compile. The weight is
the sum of:

* the number of instructions of the compiled program (`a{1000}` has 1000);
* the rune ranges of each character class, counted again for each repetition (`\pL` has about 700);
* the work of the parser divided by 32 (for example the case folding of a wide range in a `(?i)`
  class, rune by rune);
* for regexes that the Prometheus matcher can get: the runes of each character class in the
  case-folding range of Unicode, divided by 64 (`[^/]+` has a weight of about 1970, `\pL+` about 1820).

The weight of an `Equal` or `NotEqual` value is its length plus one. A compile in Go and in the
Prometheus matcher together takes at most about 256 KiB plus 2 KiB, and 2 ms plus 2 microseconds, for
each unit of weight.

The regex weight of one request is (2 x terms + 1) x (weight of the client regexes) +
(2 x selectors + 1) x (weight of the policy). "Selectors" are the selectors (or TraceQL spanset
filters) of the query. Thus:

* A user whose policy has the largest weight can use 1 selector in one query. With a policy of a tenth
  of that weight, about 20.
* With N terms, a client regex counts (2 x N + 1) times. For example, a client regex of weight 2000
  under a policy with 16 terms counts 66000 of the 131072. A user with many terms can use fewer client
  regexes.

## Multi-cluster

These limits apply only with the `multicluster` block ([multicluster.md](multicluster.md)).

| Limit | Effect for the user | What to do |
|---|---|---|
| **A spoke runs only the agent** | A standalone Querdian on a spoke is not supported, also not for local queries: it would write the same status fields of the spoke policies as the agent. | Query spoke data through the hub ([multicluster.md](multicluster.md#a-spoke-runs-only-the-agent)). |
| **Cluster scope needs a fixed set of names**: each rule of a hub `ClusterAccessPolicy` names a fixed set of clusters for each backend | Other forms are `InvalidClusterScope`: see [Cluster scope](#cluster-scope). | Use `allClusters: true` for "all clusters". |
| **Interference between clusters**: a spoke admin can name a shared group in a spoke policy | Its `labels.deny` and `labels.redact` then apply to the full request of that group, also for other clusters (a denied label applies to all terms). Too many terms give that group `403 policy_too_complex` everywhere. A rule for one tenant can change a hub grant across tenants to `403 cross_tenant_union_unsupported`. No data leaks, but one spoke can reduce access to the data of other spokes. | - |
| **Term growth**: the same namespace in several clusters merges to one term; different namespaces in different clusters do not, because a merge would over-grant | Such users reach `maxPolicyTerms`, the or-lift limits and `maxFanout` more quickly. | - |
| **Old hub data**: hub data from before the migration has no cluster label | Hub `AccessPolicy` users lose access to it until it leaves retention. Querdian does not accept `cluster=""` for them, because that would grant all data without the label. A hub `ClusterAccessPolicy` rule with `allClusters: true` or `unrestricted: true` still matches this data. | - |
| **Traces across clusters** | Search redaction stays safe, but partial traces become normal: a trace that crosses clusters shows only the spans of the clusters that the user can read. A user whose grant covers only part of a trace cannot use `rootName`, `rootServiceName` or `traceDuration` (see [TraceQL and Tempo](#traceql-and-tempo)). | - |
| **Expired spoke policies still count as matched**: an expired or suspended spoke gives no terms, but the `labels.deny` and `labels.redact` of its rules stay active | Expiry only narrows access. Its policies still show as matched (span attributes `policies.matched` and `querdian.policies`, and in the dashboard), and their denied labels still apply. | - |
| **Revocation takes up to one heartbeat**, and up to the time limit (`maxStaleness`, default 48 hours) while the agent cannot reach the receiver | A stolen agent certificate can renew grants for its spoke until a hub admin suspends the spoke. There is no certificate revocation list. | - |
| **The cluster label is not verified**: Querdian relies on the ingest gateway to overwrite it from the spoke credential, and on hub telemetry to carry `cluster="<hubCluster>"` | Hub data without the label is invisible to hub `AccessPolicy` users, with no error. | - |
| **Tenants**: one tenant for each cluster gives cross-tenant unions as soon as a user reads two clusters | The limits of federated tenants in [LogQL](#logql) and [TraceQL](#traceql-and-tempo) apply. | - |

### Cluster scope

A rule of a hub `ClusterAccessPolicy` can name the clusters of a backend with an `Equal` matcher, or a
`RegexMatch` that expands to a finite set of at most 1024 non-empty names (`mla|cluster-a`,
`cluster-(a|b)`, `cluster-[ab]`).

These are `InvalidClusterScope`, also if they would match only clusters that exist today: an open regex
(`.+`, `cluster-.*`), `NotEqual`, `RegexNoMatch`, an empty value, a case-insensitive regex (`(?i)`),
and a regex that can match the empty string (`a|`, `(mla)?`). The rule is syntactic on purpose:
Querdian does not know which clusters exist in the data.

## Authentication and sessions

| Limit | Effect for the user |
|---|---|
| **Bearer tokens only**: authorization is evaluated for each request against the claims of the token | There is no session, no token introspection and no revocation list. A stolen token works until it expires. |
| **An open Loki tail** | It continues after the token expires or the policy changes, until one side closes it. The only exception is the multi-cluster time limit: a tail closes within 10 seconds after a spoke whose rules gave its terms at the start expires, is suspended, or gets a new `SpokeCluster` spec or UID (for example a smaller tenant allow-list). The close code is 1008, `policy source expired`. A change of a policy, also of a spoke policy, does not close it. |
| **Group membership comes only from the token** | A change in the identity provider has an effect when the user gets a new token. |

## Tracing

| Limit | Effect for the user |
|---|---|
| **Query text** | Without `--tracing.record-queries`, no span records query text, and the `upstream` client spans record `url.full` with scheme, host and path only. With the flag, `url.full` also has the query string. For a `GET` request, that is the rewritten query with the matchers of the policy. |
| **Paths** | Paths are always recorded. A path can hold a value from the client, for example a trace ID. |
| **Trace context** | The server span starts a new trace and links the trace context of the client, unless `--tracing.trust-inbound-context` is set. Thus the trace backend does not show the spans of the client and of Querdian as one tree. Follow the span link. |
