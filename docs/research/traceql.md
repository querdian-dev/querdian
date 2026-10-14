# TraceQL language research (T0.5)

Method: public docs (grafana.com/docs, list at the end) plus black-box experiments against a
Tempo container. No Tempo source, grammar or tests were read; nothing from evoila/janus.

**Verified-against:** `docker.io/grafana/tempo:2.9.0` (image id
`84944a38730995ce06b71da44c68727fee034bd8b93fa9c2bdaad7459974ee5f`), single binary, local
storage, OTLP/HTTP ingest, searched through `GET /api/search?q=...`. Config and fixtures:
`hack/research/languages/tempo.yaml`, `tempo_push.py`, `tempo_run.py`, `tempo_cases*.txt`.
The containers were removed afterwards.

Legend: **[V]** verified by running it against Tempo 2.9.0; **[doc]** stated in docs only (not
run); **[?]** inconclusive / not exercised. Anything without a tag is docs-only.
Where behaviour differs from the docs this is called out as **DIFFERS**.

Fixture (searched with `/api/search`), one attribute `a` on spans, `http.status_code` where noted:
- Trace 1: root `GET /` (frontend, a="x", http.status_code=200, 1000 ms); child `db query`
  (backend, a="y", http.status_code=500, status error, kind client, extra attributes
  `weird name`, `dash-name`, `slash/name`, resource attr `team`); child `cache` (backend, a="xy").
- Trace 2: root `POST /x` (other, a="z", 20 ms).
- Trace 3: root `root3` (rootsvc, 100 ms) with child `child3` (childsvc, +50 ms, 500 ms).
- Trace 4: a single child span whose parent is never sent (no root span).

## 1. Regex matching (`=~`, `!~`)  [V]

- Fully anchored at both ends, Go (RE2) syntax: `{ span.a =~ "x" }` matches span `a="x"` but
  not `a="xy"`; `{ span.a =~ "x.*" }` matches both.
- Alternation is anchored as a whole group: `{ span.a =~ "x|y" }` matches `x` and `y` and
  not `xy` (equals `^(?:x|y)$`).
- `!~` is the exact complement: `{ span.a !~ "x" }` returns spans `y`, `xy` and `z`.
- `{ span.a =~ "" }` returns nothing.
- Implication: emitting `=~ "a|b"` for policy values is safe; no extra anchors are needed.

## 2. `&&` / `||` precedence inside a spanset filter  [V] **DIFFERS / undocumented**

The docs do not state precedence. Experiment result for Tempo 2.9.0: **`&&` and `||` have
equal precedence and associate left to right.** Evidence (spans: 1 = x/200, 2 = y/500, 3 = xy/-):
| Query | Result | Reading |
|---|---|---|
| `{ a="xy" \|\| a="x" && sc=500 }` | nothing | `((xy \|\| x) && 500)`; "&& binds tighter" would give span 3 |
| `{ a="x" && sc=200 \|\| a="y" && sc=500 }` | span 2 only | `(((x && 200) \|\| y) && 500)`; "&& tighter" would give 1 and 2 |
| `{ a="y" \|\| a="x" && sc=200 }` | span 1 only | `((y \|\| x) && 200)`; "&& tighter" would give 1 and 2 |
| `{ a="x" && sc=200 \|\| a="y" }` | spans 1, 2 | `((x && 200) \|\| y)` |
| `{ a="z" && a="x" \|\| a="xy" }` | span 3 | left to right |
Parentheses are accepted, nest arbitrarily (`{ ((a = "x")) }`) and override this:
`{ a="y" || (a="x" && sc=200) }` returns spans 1 and 2; `{ (a="y" || a="x") && sc=200 }` span 1.

**Consequence for the injector (critical):** never emit an un-parenthesised mix. The planned
`{ (X) && (dnf) }` with each side parenthesised is required, and a DNF such as
`a && b || c && d` must always be written `(a && b) || (c && d)`. Do not rely on any precedence
and re-verify on version bumps (this behaviour is surprising and may be fixed upstream).

## 3. Operators

### Comparison  [V]
`=  !=  >  >=  <  <=  =~  !~` all parse and behave as expected on strings, ints and durations
(`{ span.http.status_code != 200 }`, `<= 200`, `>= 200 && < 300`, `{ duration > 200ms }`).

### Arithmetic  [V partial]
`+`, `-`, `*`, `/` verified in filters: `{ span.http.status_code > 100 * 4 }`,
`= 100 + 400`, `= 1000 / 2`, `{ span.http.status_code - 1 = 499 }` all return the 500 span.
`%` and `^` parse (`= 1000 % 600`, `= 5 ^ 2`, results were as expected for non-matches) but a
positive match was not exercised [?].

### Logical, literals, misc
- `&&`, `||` [V]; `!` unary: parses but `{ !(span.a = "x") }` returned no traces, which looks
  wrong [?] (do not use).
- Literals: ints, floats (`200.0` parses), strings `"..."` and backtick strings `` `x` `` [V];
  `true`, `false` (`{ true }` matches all, `{ false }` none) [V]; `nil` only as
  `span.a != nil` [V], `{ span.a = nil }` is a syntax error in 2.9.0 [V] **DIFFERS** from docs'
  nil mention; `minInt`/`maxInt` are rejected as unexpected identifiers [V] **DIFFERS**.
- Rejected [V]: single-quoted strings, `and`/`AND`/`&` as logical operators, upper-case scope
  (`SPAN.a`), `#` comments (`{ ... # c }` is a syntax error). `{ span.a = "x" } // c` was
  accepted (reason unknown) [?]; the lexer must not rely on it.
- Duration literals `ns us ms s m h d` and decimals (`0.2s`) all parse [V]; `{ duration > 1d }` ok.

### Spanset operators  [V]
`{A} && {B}` (spans of both sets when the trace matches both), `{A} || {B}`, chains of three,
and parenthesised `({A}) && ({B})`; `{A} && {B}` with a non-matching side returns nothing.
No-space forms parse: `{span.a="x"}>>{span.a="y"}`.

### Structural operators  [V] (all 15 parse and execute on 2.9.0)
| Group | Operators | Observed |
|---|---|---|
| standard | `>>` `>` `<<` `<` `~` | result is the right-hand match: `{frontend} >> {a="y"}` returns the `y` span; `{a="y"} < {frontend}` returns the frontend span; `{a="y"} ~ {a="xy"}` returns the sibling `xy` span |
| union | `&>>` `&>` `&<` `&~` (and `&<<`) | returns spans of both sides, e.g. `{frontend} &>> {a="y"}` returns root and `y` |
| negated (docs: experimental) | `!>>` `!>` `!<` `!<<` `!~` | parse and run; may produce false positives per docs |
Lexer: maximal munch over `!>>`, `!<<`, `&>>`, `&<<`, `>>`, `<<`, `!>`, `!<`, `!~`, `&>`, `&<`, `&~`.

### Pipeline, aggregates, grouping  [V]
- `{ } | count() > 1`, `count() = 3`, `avg(duration) > 100ms`, `max(...)`, `min(...)`,
  `sum(duration) > 1s`, `avg(span.http.status_code) > 300` work.
- Top-level scalar filter without braces: `count() > 1` parses and runs.
- `by(resource.service.name) | count() > 1`, `select(a, b)`, `select(...) | count() > 1`,
  `coalesce()`, `by(...) | coalesce() | count() > 1` parse and run.
- `{A} | {B}` is accepted (second filter applies to the first's result); `{ } | count() > 1 | { span.a = "y" }` returns the `y` span.
- Rejected [V]: `count() > 1 && avg(...) > 1ms` / `||` between aggregates, `count() + 1 > 2`,
  `max(...) - min(...) > ...`, `count() > 1 + 1` ("not yet supported"), `| (count() > 1)`.
- Metrics functions (`rate`, `count_over_time`, `quantile_over_time`, `histogram_over_time`,
  `compare`, `with (exemplars=true)`) were accepted by `/api/search` but that endpoint does not
  evaluate them [?]; `topk` and `(A | rate()) / (B | rate())` are rejected there. They need the
  metrics endpoint and the local-blocks processor, which were **not** exercised. Doc examples
  remain unverified:
  - `100 * ({status=error} | rate()) / ({} | rate())`
  - `{ span:name = "GET /:endpoint" } | quantile_over_time(duration, .99) by (span.http.target) with (exemplars=true)`

## 4. Intrinsics  [V]

Accepted and behaving correctly: `span:name`, `span:status` (`error`, `ok`; `unset` returns
nothing on the fixture), `span:kind` (`client`, `server`), `span:duration`, `span:statusMessage`,
`trace:duration`, `trace:rootName`, `trace:rootService`, `instrumentation:name`,
`instrumentation:version`, `event:name`, `event:timeSinceStart`, `link:spanID` (the last three
parse; no event/link data in the fixture).
Unscoped legacy forms also work: `name`, `status`, `kind`, `duration`, `statusMessage`,
`traceDuration`, `rootName`, `rootServiceName`.
Problems / inconclusive:
- `childCount` (unscoped) -> 400 "intrinsic (childCount) not yet supported"; `span:childCount` is a
  syntax error in 2.9.0 **DIFFERS** from docs.
- `trace:id = "1"` matched trace 1 but the full 32-digit form `"000...001"` did not (ids appear
  leading-zero-trimmed) [V]; `span:id` and `span:parentID` never matched with either spelling [?].
- `instrumentation.name` (dot) is parsed as an attribute and matches nothing (colon form is the
  intrinsic) [V]. Bare `duration` is span duration; `trace:duration` / `traceDuration` is trace-wide [V].
- Lexer: the scope separator after `span`, `trace`, `event`, `link`, `instrumentation` is `:`
  for intrinsics and `.` for attributes.

## 5. Scoped attribute syntax  [V]

- `span.x`, `resource.x`, and unscoped `.x`. `.x` matches either span or resource attribute
  (`{ .team = "t1" }` matched the resource attribute, `{ .http.status_code = 500 }` the span one).
- `span.team` does not match a resource attribute, `resource.a` does not match a span attribute.
- Names may contain dots: `span.http.status_code`.
- `parent.span.x` / `parent.resource.x` -> 400 "parent not yet supported" in 2.9.0.
- Attribute names and values are case sensitive (`span.A`, `"X"` no match); scope keywords are lower case only.

## 6. Quoting attribute names with special characters  [V]

- A space needs quotes: `{ span."weird name" = "w" }` works; `{ span.weird name = "w" }` -> 400.
- In 2.9.0, `-` and `/` are accepted unquoted (`span.dash-name`, `span.slash/name`) and also
  quoted (`span."dash-name"`, `span."slash/name"`).
- Quoted dotted names equal unquoted: `span."http.status_code"`, `resource."service.name"`,
  `."http.status_code"` all match.
- Escapes inside quoted names/values: `\"` and `\\` accepted; `\n` -> 400 "invalid escape sequence".
  Empty name `span.""` parses (no match).
- Injector rule (confirmed safe): always render a policy attribute name quoted
  (`span."name"`) with only `\` and `"` escaped, unless it matches `[A-Za-z_][A-Za-z0-9_.]*`.
  Reject names containing control characters or newlines.

## 7. Search response fields carrying root span data  [V]

`GET /api/search?q=...` response (2.9.0). Example (condensed), query `{ span.a = "y" }`, which
matches only a child span:

```json
{"traces":[{
  "traceID":"1",                       // leading zeros stripped
  "rootServiceName":"frontend",        // ROOT DATA, present although the root span did not match
  "rootTraceName":"GET /",             // ROOT DATA
  "startTimeUnixNano":"1791975876446223872",
  "durationMs":1000,
  "spanSet":{"spans":[...],"matched":1},     // singular, still emitted in 2.9.0
  "spanSets":[{"spans":[{"spanID":"0000000000000002","startTimeUnixNano":"...","durationNanos":"300000000",
               "attributes":[{"key":"a","value":{"stringValue":"y"}}]}],"matched":1}],
  "serviceStats":{"backend":{"spanCount":2,"errorCount":1},"frontend":{"spanCount":1}}
}],
"metrics":{"inspectedBytes":"18415","completedJobs":1,"totalJobs":1}}
```
Findings:
- `rootServiceName` and `rootTraceName` come from the root span regardless of what matched.
  Without a root span (trace 4): `rootServiceName` is the literal `"<root span not yet received>"`
  and `rootTraceName` is omitted.
- `durationMs` is the **whole-trace** extent, not the root span duration (trace 3: root 100 ms,
  `durationMs` 550). `startTimeUnixNano` is the earliest span start of the trace (it equals the
  root's start in the fixtures; for rootless trace 4 it is the earliest span). Both are
  independent of which spans matched. PLAN §7.4 wording "root span duration fields" is
  therefore inaccurate: it is trace-level duration.
- **New leak:** `serviceStats` lists every service in the trace with `spanCount` and
  `errorCount` (also services whose spans were not matched). Must be redacted or recomputed.
- Matched spans (`spanSet(s).spans[]`) only contain spans satisfying the query, with `spanID`,
  `startTimeUnixNano`, `durationNanos` and the selected attributes (`select()` adds more, e.g.
  `service.name`; no span `name` field in 2.9.0 `/api/search` output).
- Both `spanSet` (singular) and `spanSets` carry the same data: redact/rewrite both.
- Redaction field list for T4.4 when the root span is not known to match the policy:
  `rootServiceName`, `rootTraceName`, `durationMs`, `startTimeUnixNano`, `serviceStats`
  (keep `traceID`, `spanSet`/`spanSets`, `metrics`). Decide in T4.4 whether to drop or blank
  them; blanking `rootServiceName` to an empty string vs omitting is untested against Grafana.

## 8. Tag endpoints and decisions of T4.4  [V]

Recorded against Tempo 2.9.0 (`internal/fanout/testdata/tempo/record.sh`):
- `GET /api/v2/search/tags` returns `{"scopes":[{"name":"intrinsic|span|resource","tags":[...]}],"metrics":{...}}`.
  The `intrinsic` scope lists, among others, `rootName`, `rootServiceName`, `trace:rootName`,
  `trace:rootService`, `traceDuration`, `trace:duration`, `span:name` (scope-prefixed with a colon).
- `GET /api/v2/search/tag/<scoped name>/values` returns `{"tagValues":[{"type":"string","value":"..."}],"metrics":{...}}`.
- Because the trace-level intrinsics are queryable, a user can filter on root data of a trace even when only a
  child span matches the policy (an oracle for the redacted fields); the enforcer does not forbid them yet.
- Search redaction (`fanout.RedactTempoSearch`): `rootServiceName`, `rootTraceName`, `serviceStats` are removed
  (also the `<root span not yet received>` placeholder); `startTimeUnixNano` and `durationMs` are recomputed from
  the returned matched spans (earliest start, extent rounded up to ms); everything not on an allow-list
  (`traceID`, `spanSet`, `spanSets`; top level `traces`, `metrics`) is dropped. The root is treated as covered
  only for unrestricted policies (`traceql.RootCovered`), since the DNF constrains matched spans only and
  the spans of one trace may carry different resource attributes.

## Not exercised (follow-ups)

Tempo metrics endpoints and functions; tag/tag-value endpoints (`/api/v2/search/tags`,
`/api/v2/search/tag/<t>/values`) and their root-data exposure; `/api/v2/traces/<id>`; legacy
`tags=` param; the `//` acceptance; `!` operator semantics; id intrinsics; `%`/`^` positive matches.
Re-run `hack/research/languages/` scripts when the pinned Tempo version changes.

## Doc URLs used (provenance)

- https://grafana.com/docs/tempo/latest/traceql/
- https://grafana.com/docs/tempo/latest/traceql/construct-traceql-queries/
- https://grafana.com/docs/tempo/latest/api_docs/
- https://grafana.com/docs/tempo/latest/metrics-from-traces/metrics-queries/

Docs were retrieved through a summarising fetch tool, so doc wording is paraphrased.
