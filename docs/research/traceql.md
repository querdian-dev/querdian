# TraceQL language research (T0.5)

Status: **docs-only, nothing verified against a running container.** The container runtime
(Podman machine) was not running when this was written, so every example below is
`unverified`. Items the public docs do not state are listed under "Open items" and must be
settled by black-box experiments against a pinned Tempo container before `pkg/traceql` relies on
them. Pages were read on 2026-10-14 in their `latest` version. Verified-against Tempo version:
**none yet** (pin it when experiments are run).

Clean room: only grafana.com/docs pages (list at the end) were used. No Tempo source, grammar or
tests were read, nothing from evoila/janus.

Legend: `[doc]` stated by a public doc page, `[unverified]` not run against a container,
`[undocumented]` docs are silent.

## 1. Regex matching (`=~`, `!~`)  [doc] [unverified]

- Regexes use Go (RE2) syntax and are **fully anchored at both ends**: `{ span.foo =~ "bar" }`
  behaves like `^bar$`. For substring matching write `.*bar.*`.
- `!~` is the negation of the same anchored match.
- Implication for Querdian: do not assume `=~` is a substring match; emit explicit alternation
  (`"a|b"`), which must be confirmed to be anchored as a whole.
- Experiments needed: `"a|b"` is `^(?:a|b)$` and not `^a|b$`; multi-line values; empty regex.

## 2. `&&` / `||` inside a spanset filter  [undocumented]

The docs define `&&` ("both conditions match") and `||` ("either condition matches") inside
`{ ... }`, e.g. `{ span.http.status_code >= 200 && span.http.status_code < 300 }`.
They **do not state a precedence** and the page read does not show parenthesised grouping.
Do not assume `&&` binds tighter than `||`. Safe rule for the injector (PLAN §7.4): always wrap
operands in parentheses: `{ (X) && (dnf) }`, with the DNF written `(a && b) || (c && d)`.
Experiments to run (all `unverified`): compare result sets of `{ A || B && C }`,
`{ (A || B) && C }`, `{ A || (B && C) }` on a fixture trace where they differ; check that
parentheses are accepted inside a filter.

## 3. Operator list

### Comparison  [doc] [unverified]
`=`  `!=`  `>`  `>=`  `<`  `<=`  `=~`  `!~`

### Logical  [doc]
`&&`  `||` (inside a filter and between spansets).

### Arithmetic  [doc, partial]
Docs say arbitrary arithmetic is supported, example
`{ span.http.request_content_length > 10 * 1024 * 1024 }`. The full set is not listed on the
page read (expect `+ - * / % ^`: **unverified**). On metrics results: `+ - * /` and comparisons.

### Literals  [doc]
integers, floats, strings (double-quoted; only `\"` and `\\` escapes), durations
(`ns us ms s m h`), `nil`, constants `minInt`, `maxInt`; bare-word values such as `error`,
`ok`, `unset` (status) and kinds like `server` appear in examples (full list unverified).

### Spanset operators  [doc]
`{A} && {B}` (spans from both, trace must match both) and `{A} || {B}` (union).
Example: `{ resource.cloud.region = "us-east-1" } && { resource.cloud.region = "us-west-1" }`

### Structural operators  [doc]
| Group | Operators |
|---|---|
| standard (return right-hand matches) | `>>` descendant, `<<` ancestor, `>` child, `<` parent, `~` sibling |
| union (return matches of both sides) | `&>>` `&<<` `&>` `&<` `&~` |
| negated, documented as experimental (may give false positives) | `!>>` `!<<` `!>` `!<` `!~` |

Examples: `{ resource.service.name="frontend" } >> { status = error }`,
`{ } !< { resource.service.name = "foo" }` (`unverified`).
Lexer note: maximal munch for these multi-character tokens (`!>>` before `!>` before `!`).

### Pipeline, aggregates, grouping, selection  [doc]
- `|` chains stages: `{ ... } | count() > 2`.
- Aggregates: `count()`, `avg(x)`, `max(x)`, `min(x)`, `sum(x)` followed by a comparison.
  Examples: `{ } | sum(span.bytesProcessed) > 1000000000`.
- `by(attr)`: `{ status = error } | by(resource.service.name) | count() > 1`.
- `select(a, b)`: `{ status = error } | select(span.http.status_code, span.http.url)`.
- `coalesce()` exists in the language but was not described on the pages read (**unverified**).
- Metrics functions after a pipe: `rate`, `count_over_time`, `sum_over_time`, `max_over_time`,
  `min_over_time`, `avg_over_time`, `quantile_over_time`, `histogram_over_time`, `compare`;
  also `topk`, `bottomk`; optional `by (...)`; optional hints `with (exemplars=true)`,
  `with (spanonly_fetch=false)`. Examples (`unverified`):
  - `100 * ({status=error} | rate()) / ({} | rate())`
  - `{ span:name = "GET /:endpoint" } | quantile_over_time(duration, .99) by (span.http.target) with (exemplars=true)`
  - `{ resource.service.name = "frontend" } | rate() by (status) with (spanonly_fetch=false)`

## 4. Intrinsics  [doc] [unverified]

Current docs use `<scope>:<name>`:

| Scope | Intrinsics |
|---|---|
| span | `span:status`, `span:statusMessage`, `span:duration`, `span:name`, `span:kind`, `span:id`, `span:parentID`, `span:childCount` |
| trace | `trace:duration`, `trace:rootName`, `trace:rootService`, `trace:id` |
| event | `event:name`, `event:timeSinceStart` |
| link | `link:spanID`, `link:traceID` |
| instrumentation | `instrumentation:name`, `instrumentation:version` |

Unscoped forms (`status`, `duration`, `name`, `kind`, ...) still appear in doc examples
(`{ status = error }`); bare `duration` is an alias of `span:duration`, not `trace:duration`.
The exact unscoped list (e.g. `rootName`, `rootServiceName`, `traceDuration`) is **unverified**;
the lexer must accept both forms. Examples: `{ span:name = "foo" }`, `{ trace:id = "1234" }`.

## 5. Scoped attribute syntax  [doc] [unverified]

- `span.<name>`, `resource.<name>`, `link.<name>`, `event.<name>`, `instrumentation.<name>`
  (period after the scope). `trace` has no custom attributes.
- Unscoped `.<name>` (leading dot) is expected to search span and resource scope
  (**unverified**, from general knowledge of the docs, not seen on the pages read).
- The name itself may contain dots: `span.http.status_code` is scope `span`, name `http.status_code`.
- `parent.` scoping: not in the pages read (**unverified**).

## 6. Quoting attribute names with special characters  [doc] [unverified]

Names containing spaces or dots that must be a single segment are wrapped in double quotes after
the scope dot: `{ span."attribute name with space" = "value" }`. Only `\"` and `\\` escapes are
supported inside the quotes.
Experiments: which characters force quoting (`-`, `/`, `:`, `@`), `resource."service.name"` vs
`resource.service.name`, empty name, unicode.
Injector rule (PLAN §7.4 "names rendered as given"): quote with `"..."` escaping only `\` and
`"` whenever the name is not made of letters, digits, `_` and `.` (starting with a letter or `_`).

## 7. Search response fields carrying root span data  [doc] [unverified]

`GET /api/search` params: `q` (TraceQL), legacy `tags` (logfmt, case-insensitive substring),
`minDuration`, `maxDuration`, `limit` (default 20), `start`/`end` (unix seconds), `spss`
(default 3).

Each element of `traces[]` carries trace-level root data:
- `traceID`
- `rootServiceName` (root span's service)
- `rootTraceName` (root span's name)
- `startTimeUnixNano` (string)
- `durationMs`
- `spanSets[]` (older versions: singular `spanSet`) each with `matched` and `spans[]`; each span
  has `spanID`, `startTimeUnixNano`, `durationNanos`, `attributes[]` (`key`, `value.stringValue`...).
- top-level `metrics` (`totalBlocks`, `inspectedTraces`, `inspectedBytes`, ...): cost counters,
  not span data.

Redaction candidates for T4.4 (root span not known to match the policy): `rootServiceName`,
`rootTraceName`, `durationMs`, `startTimeUnixNano`. `spanSets[].spans[]` are matched spans only,
so they already satisfy the injected filter. Whether `durationMs`/`startTimeUnixNano` are
root-derived (vs. whole-trace) needs a black-box check. Also check the v2 and tag-value
endpoints and any `rootSpan*` fields in the pinned version.

## Open items (verify before relying)

1. Precedence of `&&`/`||`, parentheses acceptance (section 2).
2. `=~` anchoring with alternation.
3. Arithmetic operator set, unary `!`, unscoped intrinsic list, `parent.` scope, `coalesce()`.
4. Quoting rules for special characters.
5. Root-derived search response fields (section 7).
6. Every example in this file.

## Doc URLs used (provenance)

- https://grafana.com/docs/tempo/latest/traceql/
- https://grafana.com/docs/tempo/latest/traceql/construct-traceql-queries/
- https://grafana.com/docs/tempo/latest/api_docs/
- https://grafana.com/docs/tempo/latest/metrics-from-traces/metrics-queries/

Pages were retrieved through a summarising fetch tool, so wording is paraphrased; re-read the
pages directly when writing parser tests.
