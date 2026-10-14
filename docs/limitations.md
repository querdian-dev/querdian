# Known limitations

Querdian is deliberately strict. Wherever it cannot prove that a request is handled exactly as the
backend would handle it, or cannot express the exact union of a user's policies, it **rejects the
request** instead of passing it through or guessing. This page collects those choices so that you
can tell an intended rejection from a bug. Most of them return `400 bad_data` (`parse_error`,
`unsupported` or `bad_request`) or `403 forbidden`; the reason codes are explained in
[troubleshooting.md](troubleshooting.md#decision-reason-codes). Each item is tied to the code that
enforces it. If something here blocks a legitimate use case, open an issue; relaxing a rule always
needs a parser or rewriter change plus tests.

Contents: [general](#general-request-handling) / [PromQL](#promql) / [LogQL](#logql) /
[TraceQL and Tempo](#traceql-and-tempo) / [denied labels](#denied-labels) /
[approximate results](#approximate-results) / [policy model](#policy-model) /
[authentication](#authentication-and-sessions)

## General request handling

* **Default deny.** Any path not in the endpoint tables is `403 endpoint_not_allowed`, as is a known
  path used with a wrong method. Denied on purpose: write and ingest paths (`push`, `write`,
  `otlp`, `read`), rules and alerts, admin and status pages, `targets`, `scrape_pools`, Prometheus
  `metadata` (it cannot be restricted by label), `search/*`, `notifications`, `features`,
  `/openapi.yaml`, Loki `delete`, deprecated `/api/prom/*`, Tempo v1 `search/tags` and
  `search/tag/<name>/values` (no `q` support), `v2/traces/diff`, `overrides`, `mcp`, and the internal
  endpoints of all three. The inventory is in [endpoints.md](endpoints.md).
* **Paths must be canonical.** Escaped slashes (`%2f`), backslashes, `%00`, any `%` left after
  decoding, `.`/`..` segments, empty segments (`//`), trailing slashes, control characters and
  invalid UTF-8 are `400 bad_path`.
* **Parameters are allow-listed per route and unknown ones are rejected (`400 bad_request`)**, not
  silently dropped. Notably `storeMatch[]` (Thanos) is never forwarded. Allowed Thanos extras:
  `dedup`, `partial_response`, `replicaLabels`, `replicaLabels[]`, `max_source_resolution`.
* A single-valued enforcement parameter (`query`, `q`) given more than once, in URL and form body
  together or repeated, is `400 bad_request`. Backends disagree about which value wins; Querdian
  refuses to guess.
* `POST` bodies must be `application/x-www-form-urlencoded` and at most 1 MiB; JSON bodies are
  rejected. The rewritten request is always re-encoded by the proxy (`GET`: URL query, `POST`: form
  body).
* **Merged responses are JSON only.** Plans that fan out or buffer (all Loki fan-out endpoints,
  Tempo search) send `Accept: application/json` upstream. Loki Parquet and Tempo
  `application/vnd.grafana.llm` are never merged. Single-request streamed plans pass the client's
  `Accept` through. Prometheus `labels`/`series` with denied labels, and trace by ID, buffer and
  rewrite the body; a body they cannot decode (unexpected media type, compression other than gzip
  for traces, malformed JSON) is a `502`, never an unfiltered pass-through.
* **Unrestricted users are not parsed.** A user whose effective policy is unrestricted (or has a term
  without matchers) and who has no denied labels gets their query forwarded unchanged, apart from
  the tenant header and the parameter allow-list. Syntax errors then come from the backend.
* Errors from a **single streamed** upstream request (for example a Loki `400` for a log query on
  the instant `query` endpoint) are passed through with the upstream status and are counted as
  `allow` in `querdian_requests_total`. Any failure of a **fan-out** branch becomes `502` for the
  whole request.
* `GET` is required for `label/<name>/values` and Loki `labels`, `label/<name>/values`, `tail`,
  `patterns`; most other query endpoints accept `GET` and form `POST`.
* Grafana features that need denied endpoints degrade: for example the Prometheus metrics browser
  help text (`metadata`) is unavailable. Tempo gRPC streaming search is unsupported; disable
  streaming in the Grafana Tempo data source.
* Nothing is rate limited or queued inside Querdian (see [operations.md](operations.md#scaling)).
* Querdian does not terminate TLS and does not authenticate to backends other than by setting the
  tenant header. Backends must be isolated at network level.

## PromQL

Queries are parsed with the Prometheus parser using **default options**: experimental functions,
duration expressions, extended range selectors and fill modifiers are not enabled, so queries using
them are `400 parse_error`. A rewritten query is parsed again; if it does not parse, the request
is `400 unsupported`.

With **one** policy term every query that parses is supported. With **several** terms the query is
or-lifted ([promql-enforcement.md](promql-enforcement.md)), which is only exact for some shapes.
These are rejected with `400 unsupported`:

* a range vector that is not the direct argument of a function, for example the whole query
  `up[5m]`;
* a function over a range vector with another argument that contains a selector, for example
  `quantile_over_time(scalar(x), y[1h])`;
* a function with several range-vector arguments;
* any expression node type the lifter does not know (future syntax fails closed).

Further limits:

* **`absent()` / `absent_over_time()` edge cases.** Prometheus derives the labels of the result of
  `absent(m{job="x"})` from the equality matchers of the selector. With several terms the argument
  becomes an `or` expression, so the result has **no labels**; with one term the labels include the
  policy's equality matchers. The value (1 if no series exists in the union) stays exact, and no
  data is leaked. `absent_over_time(m[5m])` is rewritten to `absent(last_over_time(m{t1}[5m]) or ...)`.
* A function that drops the metric name makes Prometheus fail when two series differ only in their
  name. Across terms `or` resolves such a collision silently. This needs a term that matches on
  `__name__`.
* An or-lifted query contains (selectors x terms) selector copies; more than 2048 is `403
  query_too_complex`. Prometheus metadata endpoints expand (user `match[]` x terms) selectors; more
  than 256 is `403 query_too_complex`.
* `match[]` selectors need at least one matcher that does not match the empty string; if neither
  the user nor the policy supplies one, `__name__=~".+"` is added (this can only narrow).
* `series` and `labels` responses are buffered and filtered when labels are denied, up to 16 MiB.
* `query_exemplars` returns the union of the exemplars of the series matched by the lifted
  selectors.

## LogQL

`pkg/logql` is a clean-room parser written from the public documentation and black-box tests
(`docs/research/logql.md`). The parts it does not need to understand (log pipelines) are
tokenised and structurally validated, not interpreted. Anything it does not recognise is a
`400 parse_error`; there is no lenient mode. The strictness choices:

**Lexer**

* Strings: only the escapes `\" \\ \a \b \f \n \r \t \v \xHH \NNN` (octal at most 255). `\u`, `\U`,
  `\'` and all other escapes are errors. Single-quoted strings, raw control characters (except tab)
  and invalid UTF-8 are errors. Backtick strings are accepted.
* Identifiers are `[A-Za-z_][A-Za-z0-9_]*`; `:` and `.` are not identifier characters. A stray one
  is an error, so a label such as `k8s.namespace.name` cannot be written (use the normalised
  `k8s_namespace_name`).
* Numbers are decimals with optional fraction and exponent; no hex, `Inf` or `NaN` literals.
* Durations: number+unit groups with `ns us µs μs ms s m h d w y`, lower case, no spaces. Bytes:
  one number and one unit out of `B k kB KB Ki KiB M MB Mi MiB G GB Gi GiB T TB Ti TiB`; `1kb`, `2PB`,
  compound bytes are rejected. `1m` is a duration and `1M` bytes.
* Flags such as `--strict` are lexed as separate tokens and reassembled by the parser.

**Parser**

* Range selectors and `offset` take integer durations without the `us` unit: `[1.5m]`, `[1us]` and
  subqueries (`[5m:1m]`) are errors. `offset` must directly follow the range inside the function.
* Literals as operands of `and`, `or`, `unless` are errors; `bool` is only valid on comparisons;
  `group_left`/`group_right` require `on` or `ignoring` and are not allowed on set operators.
  Precedence of `and` versus `unless` is assumed to be the same level.
* A stream selector needs at least one `=` or `=~` matcher that cannot match the empty string (or
  `!= ""`), and regexes must compile (as in Loki).
* `logfmt` accepts exactly `--strict` and `--keep-empty` (each at most once); `json` accepts no
  flags and every parameter needs `="expr"`.
* `unwrap`: only label filters may follow it; an unwrapped range function needs `unwrap` and
  `unwrap` needs an unwrapped range function. **`rate(... | unwrap x [r])` is rejected** because it is
  not documented. Grouping after the parentheses is only accepted for unwrapped functions.
* Supported functions only: range aggregations `rate`, `count_over_time`, `bytes_rate`,
  `bytes_over_time`, `absent_over_time`, `rate_counter`, `sum_over_time`, `avg_over_time`,
  `max_over_time`, `min_over_time`, `stdvar_over_time`, `stddev_over_time`, `quantile_over_time`,
  `first_over_time`, `last_over_time`; vector aggregations `sum avg min max count stddev stdvar topk
  bottomk sort sort_desc`; `vector` and `label_replace`. **`distinct`, `approx_topk`, `abs`,
  `count_values` and every other function are rejected.**
* `topk`/`bottomk`/`vector` take plain numbers; `label_replace` takes exactly five string literals.
* A log query must start with `{`; a bare or parenthesised selector as operand of a metric
  expression is an error. Nesting deeper than 100 levels is rejected.
* Keywords are case insensitive and may be label names where unambiguous.

**Enforcement**

* With several policy terms, **metric queries are or-lifted per range aggregation, and range
  aggregations with a `by` / `without` clause cannot be lifted** (`400 unsupported`). Rewrite such
  queries as an outer `sum by (...)` over a plain range aggregation. When a pipeline collapses
  distinct streams to the same label set (`drop`, `keep`), the counts of terms after the first can
  be under-reported because `or` keeps only the first of identical label sets. This never grants
  more than the policy allows.
* **Log queries with several terms are fanned out**, one request per term, up to `maxFanout`
  (otherwise `403 query_too_complex`). Entries are deduplicated within identical streams by
  `(timestamp, line)`, ordered by direction and cut to `limit`. All branches share the same
  parameters; invalid or inconsistent upstream responses fail the request with `502`.
* The optional `query` of `labels`, `label/<name>/values`, `detected_*`, `index/*`, `patterns` and
  `match[]` of `series` must be a **log selector** (with optional pipeline), not a metric query
  (`400 unsupported`). If it is omitted, restricted users get the policy selector instead;
  unrestricted users get no selector (and `detected_fields` then fails upstream, as in Loki).
* Under `tenantIdLabel`, every selector must keep a matcher other than `__tenant_id__`
  (Loki rejects a selector with only that matcher). A policy-only selector built for a term that
  consists only of the tenant binding is therefore `400 unsupported`.
* `tail`: only log queries (not metric queries); only the parameters `query`, `limit`, `start`
  and `delay_for` (`limit >= 1`, `delay_for` 0 to 5 seconds) are accepted; browser clients must come
  from the same host (cross-origin WebSocket handshakes are refused); each upstream frame is limited
  to 4 MiB and duplicates are removed with a memory of the last 10 000 entries; an invalid frame
  closes the tail. The tail runs under the policy and token it started with.
* `patterns` returns 404 upstream unless the pattern ingester is enabled; the non-empty response
  shape is unverified against a live Loki.
* The `X-Loki-Response-Encoding-Flags: categorize-labels` encoding is preserved by the stream merge;
  branches that disagree on it fail the request.

**Findings from the end-to-end suite (Loki 3.5.0)**

* Under a **tenant-bound policy** (rules that differ per tenant, so terms carry a `__tenant_id__`
  matcher) the metadata endpoints `labels`, `label/<name>/values`, `series`, `index/stats` and
  `index/volume` do not work reliably: Loki 3.5.0 ignores a negative `__tenant_id__` matcher there and
  treats a positive one as matching nothing. Results are empty or wrong, never wider than the policy.
  Metric queries are unaffected (they bind with a negative matcher). A proper fix needs one request per
  tenant.
* **Multi-tenant grants** (header `t1|t2`) do not work for `tail` (Loki: "multiple org IDs present"),
  for `series` (empty result) or for `detected_labels`.
* **Duplicate collapse.** When a pipeline gives different streams identical labels and lines (`keep`,
  `drop`, `line_format`), the fan-out merge for several-term log queries and the `or` of an or-lifted
  metric query collapse them: fewer entries or lower counts than the union of the per-term results. The
  result never contains data outside the policy.
* **Rejected although Loki accepts them:** a range aggregation with `by`/`without` when the policy has
  several terms (for example `quantile_over_time(... | unwrap x [5m]) by (app)`, also `max_over_time`,
  `min_over_time`, `stddev_over_time`, `last_over_time`); parenthesised log queries
  (`( {app="web"} |= "x" )`); `bool` on arithmetic operators; selectors with no positive matcher anywhere.
* `absent_over_time` under several terms returns one series per term.

## TraceQL and Tempo

`pkg/traceql` is a clean-room front end. It only has to locate spanset filters and attributes, so it
validates more than it interprets. Where Tempo accepts something undocumented or divergent, Querdian
rejects it (`400 parse_error`), because a token stream that differs from Tempo's reading could hide
an attribute from the policy checks.

**Lexer**

* Comments (`#`, `//`, `/* */`) are errors.
* Strings and quoted names accept only the escapes `\"` and `\\`; backtick strings, single-quoted
  strings, control characters and invalid UTF-8 are errors.
* Unquoted attribute names are letters, digits, `_` and `.`; a name followed by `-` or `/` is an
  error ("quote the name") because it could be arithmetic.
* A scope before `.` must be `span`, `resource`, `event`, `link` or `instrumentation`;
  **`parent.*` is rejected** and so are upper-case or unknown scopes. A scope before `:` must be a
  known intrinsic scope with a known intrinsic name.
* Durations are one number plus one unit (`ns us ms s m h d`); `1h30m`, glued numbers and exponents
  are errors.
* A lone `&` is an error.

**Parser**

* A query is a pipeline of stages: spanset expressions, scalar filters, `by(attr)`, `select(...)`,
  `coalesce()`, one metrics function (optionally followed by `topk`/`bottomk`) and a trailing
  `with (...)`.
* Filters must be boolean, comparisons need an attribute or intrinsic, regex operators need an
  attribute on the left and a string literal on the right. `nil` only as `attr != nil`. Enum values
  (`error`, `ok`, `client`, ...) only against `status`/`kind` with `=` or `!=`. `minInt`/`maxInt`
  and unary `!` are rejected.
* Mixing different spanset operators on one level without parentheses is rejected, parentheses may
  only wrap spanset expressions, and arithmetic between metrics queries
  (`100 * ({..} | rate()) / ({} | rate())`) is rejected.
* `by()` takes exactly one attribute; `select()` and metrics arguments are single attributes; `with`
  values are literals. Nesting beyond 100 levels is rejected.

**Enforcement**

* The policy is injected into **every** spanset filter, including those inside structural operators,
  pipelines, `select`, metrics functions and `compare(...)`: `{ X }` becomes `{ (X) && (dnf) }`, `{}`
  becomes `{ dnf }`, and a query with no filter is prefixed with `{ dnf } | `. `&&` and `||` have
  equal precedence in Tempo, so the injected text is fully parenthesised.
* The legacy `tags`, `minDuration` and `maxDuration` search parameters are `400 unsupported`,
  with or without `q`.
* `q` is required on the `metrics/*` endpoints (`400 unsupported`); it is optional on search and tag
  endpoints, where its absence yields a policy-only filter for restricted users.
* **Tenant-bound terms are unsupported on Tempo** (`400 unsupported`): there is no `__tenant_id__`
  attribute. Cross-tenant unions with differing terms are `403 cross_tenant_union_unsupported`.
* **Missing-attribute semantics.** A span without an attribute satisfies none of `=`, `!=`, `=~`,
  `!~`; a non-string attribute never satisfies a string comparison. Policy `NotEqual`/`RegexNoMatch`
  therefore admits less on Tempo than on Prometheus/Loki, and a policy cannot select "attribute
  absent". Policy attribute names must be scoped (`resource.x`, `span.x`).
* **Search response redaction.** Tempo reports trace-level data (root span name and service, per
  service span and error counts, trace start and duration) even if only a child span matched. For
  every user who is not unrestricted (and has no term without matchers), `rootServiceName`,
  `rootTraceName` and `serviceStats` are **removed**, and `startTimeUnixNano` and `durationMs` are
  **recomputed from the returned matching spans only** (earliest start; duration from earliest
  start to latest end, rounded up to whole milliseconds). They are therefore not the real trace
  start or duration, and the Grafana result table shows no root name for such users. Only
  `traceID`, `spanSet`, `spanSets` and the two recomputed fields survive per trace, and only
  `traces` and `metrics` at the top level; fields added by future Tempo versions are dropped. The
  root span is deliberately not treated as covered by a matching policy, because spans of one trace
  can carry different resource attributes. The body is buffered (up to `maxMergeBytes`).
* **Trace-level intrinsics are rejected unless the root is covered.** `rootName`, `rootServiceName`,
  `traceDuration` (and their `trace:` forms), `compare()` and the tag-value lookup of the root names
  return `400` for users whose policy does not cover the root span (not unrestricted, no term without
  matchers), because they would let the user probe redacted root data
  ([security/threat-model.md](security/threat-model.md)).
* **Trace by ID** (`traceByID: filter`): `q` and every parameter except `start`/`end` is `400`; only
  JSON and protobuf (`application/protobuf`) responses are filtered and gzip is decoded; anything
  else is `502`. Spans outside the policy are dropped, then empty scope and resource groups; if nothing
  remains the answer is `404` with an empty body, **also on `v2`** where Tempo itself answers 200
  with an empty trace. Denied attributes are removed from the spans that remain. The filter
  evaluates string attributes of the span and its resource; unknown protobuf fields of the wrapper
  `Trace` message are dropped; the buffered body is capped at `maxMergeBytes`. The default mode is
  `deny`.
* Tag endpoints: `api/v2/search/tags` lists tags of spans that satisfy the injected policy; denied
  tags are filtered out. Tag *values* are computed over policy-matching spans and returned as is.
* Only the HTTP API is proxied; gRPC and streaming search are out of scope.

## Denied labels

* Denied labels hide the ability to filter, group or join on a label and remove it from
  label/series listings. **They do not remove the label from query results** (except trace by ID).
  See [policy-guide.md](policy-guide.md#denied-labels).
* The union rule is conservative: a label denied by any matched rule is denied for all terms.
* Loki `series` responses are not filtered (Prometheus `series` is).

### Denied-label scan false positives

The PromQL check works on the syntax tree and has no false positives. The LogQL check is a token
and string scan and over-rejects deliberately:

* A denied name is matched against every identifier **and keyword** token, wherever it appears in
  the query. A label that is named like a keyword or function (`json`, `count`, `rate`, `level` in
  an unrelated clause) will block legitimate uses of that word; avoid such label names.
* `line_format` and `label_format` template strings are rejected when they contain the denied name
  as a **substring**: with `app` denied, `| line_format "{{.application}}"` or a literal text such
  as `"appointment"` is rejected.
* Templates that can read all labels without naming them (`{{ . }}`, `range`, `with`, `template`,
  `block`, `define`, `$` variables) are rejected as soon as any label is denied, even if the denied
  label is irrelevant to the query. Plain field access (`{{ .app }}`) stays allowed.
* The `dst` and `src` string arguments of `label_replace` are compared with denied names; the regex
  and replacement are not.
* TraceQL: a denied scoped name denies the unscoped `.x` form and vice versa, and denied tag lists
  treat the intrinsic scope as "any scope", which can hide more tags than strictly necessary.

## Approximate results

When the effective terms are **not provably disjoint**, a record that matches several terms is
counted once per term in summed results. Disjointness is only recognised for different tenants or
for `Equal` matchers on the same label with different values; regex terms that happen to be
disjoint are not detected, so such results are marked approximate although they may be exact.

* Loki `index/stats`, `index/volume`, `index/volume_range` and `patterns`: counters are summed per
  term. If the terms are not provably disjoint the response gets the header
  `X-Querdian-Approximate: true` and the server span `merge.approximate=true`; the body keeps
  Loki's format. Truncation to `limit` after summation is exact only if no branch cut off series.
  Denied labels are stripped from the reported label sets, which can merge series.
* Loki `stats` blocks in query responses are summed over fan-out branches; shared work is counted
  once per branch.
* Loki `detected_labels` / `detected_fields` cardinalities are summed per label: exact for
  disjoint terms, otherwise an upper bound. Conflicting field types become `string`.
* Loki log stream merges are exact (the first `limit` entries of the union are the union of each
  branch's first `limit` entries).

## Policy model

* One invalid rule disables its whole policy on all its backends.
* An invalid or unknown-backend policy grants nothing; there is no partial application.
* Policy compilation uses namespace labels only for `policy.querdian.dev/tenant`; changing the
  `namespaceLabel` of a backend needs a restart.
* A user's effective policy with more than `maxPolicyTerms` terms is `403 policy_too_complex`;
  the DNF simplification only merges terms that differ in one `Equal`/`RegexMatch` label and drops
  terms whose matcher set is a superset of another's, it does not reason about overlapping regexes.
* No deny policies exist; to exclude data from a broad rule, write the rule narrower.
* Cross-tenant unions with differing terms are only possible on backends with
  `tenantFederation: tenantIdLabel` (see [policy-guide.md](policy-guide.md#tenants)).

## Authentication and sessions

* Bearer tokens only. Authorization is evaluated per request against the token's claims; there is no
  session, no token introspection and no revocation list, so a stolen token works until it expires.
* An open Loki tail keeps working after the token expires or the policy changes, until either side
  closes it.
* Group membership comes only from the token. A change in the identity provider takes effect when
  the user obtains a new token.
