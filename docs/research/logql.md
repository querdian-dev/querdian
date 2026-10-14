# LogQL language research (T0.5)

Method: public docs (grafana.com/docs, list at the end) plus black-box experiments against a
Loki container. No Loki source, grammar or tests were read; nothing from evoila/janus.

**Verified-against:** `docker.io/grafana/loki:3.5.0` (image id
`c8f7864de9ad688bb9219730b06c8941b86997dd2122f7066a6289e63c6f87e7`), default
`local-config.yaml`, queried via `/loki/api/v1/query_range` (last hour, step 60). Scripts and
case lists: `hack/research/languages/loki.sh`, `loki_run.sh`, `loki_vals.sh`,
`loki_cases*.txt`. The container was removed afterwards.

Legend: **[V]** verified against Loki 3.5.0 ("accepted" means HTTP 200, "rejected" means HTTP
400 parse error); **[doc]** docs only, not run; **[?]** inconclusive. **DIFFERS** marks
behaviour that contradicts the docs.

Fixture streams: `{app="foo",env="a"}` with two logfmt lines
(`level=info msg="hello" duration=250ms size=2KB`, `level=error msg="bad" duration=2s size=30KB`)
and `{app="bar",env="a",service="a:c"}` with one line
(`level=info msg="x#y" duration=1m size=1MiB`).

## 1. Binary operators in metric queries

- Set operators `and`, `or`, `unless` [V]: `rate(A) and rate(B)`, `A or vector(0)`,
  `A unless B`, with `on(app)`/`ignoring(...)` modifiers all accepted and behave as in PromQL
  (e.g. `A unless on(app) B` removes series whose `app` is in B).
  `1 or 2` / `1 and 2` rejected: "unexpected literal for left leg of logical/set binary operation".
- Arithmetic `+ - * / % ^` [V] between scalars, scalar/vector, vector/vector.
- Comparison `== != > >= < <=` filter; with `bool` they yield 0/1 [V]:
  `count_over_time({app="foo"}[5m]) > bool 1`.
- Vector matching syntax `on(..)`, `ignoring(..)`, `group_left()` accepted [V]; semantic errors
  such as many-to-many come back as HTTP 500 ("many-to-many matching not allowed").
- Precedence [V] (PromQL-like, with one oddity), highest first:
  1. unary minus: `-2 ^ 2` evaluates to **4** (binds tighter than `^`; differs from PromQL's -4)
  2. `^`, **right**-associative (`2 ^ 3 ^ 2 = 512`)
  3. `* / %`, left-assoc (`2 * 3 % 4 = 2`)
  4. `+ -`, left-assoc (`10 - 2 - 3 = 5`, `1 + 2 / 3 = 1.667`, `2 + 3 * 4 = 14`)
  5. comparisons (`5 > 2 + 1` evaluates `5 > 3`)
  6. `unless` binds tighter than `or` (`vector(1) unless vector(1) or vector(7) = 7`) [V].
     `and` vs `or` and `and` vs `unless` were not distinguished by the experiments [?];
     assume PromQL (`and`/`unless` same level, above `or`) and parenthesise when rendering.
  7. `or`
- Keywords are case-insensitive [V]: `SUM(...)`, `OFFSET`, `| LOGFMT`, `AND` all accepted.
- Parser note: `or`/`and` appear in three contexts (metric set operator, label-filter, and
  line-filter `or`, see §2). Disambiguate by position: inside a pipeline after a stage keyword
  vs between two complete metric expressions.

## 2. Pipeline stage keywords

Verified accepted [V]: line filters `|=` `!=` `|~` `!~`, pattern filters `|>` and `!>`
(`{app="foo"} |> "<_> msg=<_>"`), `ip("...")` filter (`!= ip("1.2.3.4")`); parsers `json`,
`logfmt` (with `--strict`, `--keep-empty`), `pattern "..."`, `regexp "(?P<n>..)"`, `unpack`;
`line_format "tmpl"`, `label_format a=b, c=d` and `label_format lv="{{.level}}"`; `drop l1, m="v"`,
`keep l`, `decolorize`; label filters; `unwrap`.
- **Line filter chaining with `or`** [V]: `|= "hello" or "bad"` is accepted (matches either);
  `|= "a" or |= "b"` is rejected. New relative to the docs pages read.
- `distinct` is **rejected** in 3.5.0 [V] ("unexpected IDENTIFIER"): treated as an ordinary
  identifier, not a stage keyword. **DIFFERS** from newer docs mentioning it.
- Unknown stage `| foo` rejected [V]; `label_format a=b,a=c` rejected (duplicate name); trailing
  commas rejected (`drop level,`, `label_format a=b,`).
- Label-filter combination [V]:
  - operators `= == != > >= < <= =~ !~`; parentheses allowed; `and`, `or`, `,`, whitespace
    and a following `|` are all accepted separators.
  - **DIFFERS from docs ("left to right"):** `and` binds tighter than `or`:
    `level="info" or level="error" and msg="bad"` returned both lines (= `info or (error and bad)`);
    left-to-right would return only the `bad` line. `level="info" or msg="hello" and level="error"` returns the hello line.
  - the comma/space/pipe separators bind loosest: `level="error", msg="bad" or level="info"`
    returned only the `bad` line (= `error and (bad or info)`).
  - Always parenthesise when rendering.
- `unwrap` forms [V]: `unwrap label`, `unwrap bytes(label)`, `unwrap duration(label)`,
  `unwrap duration_seconds(label)`; label filter after unwrap accepted; `line_format` after
  `unwrap` rejected; `| unwrap x` without a range aggregation rejected. Plain `unwrap size` on a
  value like `2KB` is a runtime `SampleExtractionErr` (HTTP 400), not a parse error.
- Range aggregations [V accepted/run]: `rate count_over_time bytes_rate bytes_over_time
  absent_over_time`; unwrapped `sum_over_time quantile_over_time(0.9, ...) rate_counter
  first_over_time last_over_time stddev_over_time` (with `by (...)` after the parentheses).
  `rate(...) by (app)` rejected: "grouping not allowed for rate aggregation".
- Vector aggregations [V]: `sum by (l) (expr)` and `sum (expr) by (l)` both accepted, both
  together rejected; `topk(1, expr)`, `sort`, `sort_desc` accepted.
- Stream selector [V]: `{}`, `{app=~".*"}`, `{app=~""}`, `{app=""}` rejected ("at least one
  regexp or equality matcher that does not have an empty-compatible value"); `{app!=""}`
  accepted; trailing comma `{app="foo",}` rejected; whitespace around commas ok; `{app="foo"}|="x"`
  without spaces ok.
- Regex: selector matchers are fully anchored (`{app=~"fo"}` returns nothing, `fo.*` matches),
  line filter `|~ "ell"` is unanchored, `(?i)` works [V].
Keyword set for the T3.2 opaque-pipeline validator (3.5.0): `json logfmt pattern regexp unpack
line_format label_format drop keep decolorize unwrap by without on ignoring group_left
group_right bool and or unless offset ip` plus line-filter operators. Parser error text
format: `parse error at line L, col C: syntax error: unexpected X`.

## 3. Strings, raw strings, comments

- Double-quoted strings [V]: escapes accepted `\" \\ \n \a \b \f \r \t \v \x41 \101` (hex and
  octal); `\q` rejected ("invalid char escape"). `\uXXXX` not exercised [?].
- Backtick raw strings [V]: accepted, no escapes (`` `raw\w+` ``, `` `msg="hello"` ``).
- Single-quoted strings rejected ("invalid char literal") [V].
- `"msg=\"hello\""`, `` `msg="hello"` `` and regex `"msg=\\\"hel+o\\\""` all select the same line [V].
- Comments [V]: `#` to end of line after a complete query works, also after a pipeline or
  a metric expression; text after `#` is ignored (`|= "hello" # c |= "zzz"`); `#` inside a
  string or raw string is data (`|= "x#y"` matches the line). A comment on its own line
  *before* the query was not exercised (the runner is line based) [?].

## 4. Duration and bytes literals

- Durations in label filters [V] (`| duration > X`): `250ms`, `0.25s`, `1.5s`, `1h1m1s`, `1.5h`,
  `2m30s`, `1s1ms`, `1ns`, `1us`, `1µs`, `1d`, `1w`, `1y` accepted.
- Durations in range selectors [V]: accepted `[500ms] [1m30s] [1h30m] [1d] [1w] [0s]`;
  `[1y]` parses (blocked only by the 30d query-length limit). **Rejected:** `[1.5m]` (decimals),
  `[1us]` (microsecond unit!), `[5 m]`, `[5M]`, `[5]` (no unit), `[5m:1m]` (no subqueries).
  **DIFFERS** from label filters: the two contexts use different parsers.
- `offset` takes the same duration syntax (`offset 5m`, `0s`, `1d` accepted) [V].
- Bytes in label filters [V]: accepted `20KB 20kB 20k 1G 1B 1.5KB 1KiB 1.5MiB 2TiB 2Ki`;
  rejected `20 KB` and `5 MiB` (space), `1kb`, `2PB`/`2EB`.
  Semantics: `size > 2KiB` returned only the 30KB line (2KB < 2048 B) [V], i.e. KB = 1000 and KiB = 1024.
- Numbers are floats; failed conversions add an `__error__` label rather than dropping the line (doc).

## 5. `offset`  [V]

- `count_over_time({app="foo"}[5m] offset 5m)` accepted.
- `count_over_time({app="foo"}[5m]) offset 5m` rejected: "unexpected offset".
- `sum(count_over_time(...[5m]) offset 1m)` rejected too (must be inside the function, directly after `[range]`).
- Accepted with pipelines and unwrap: `count_over_time(({app="foo"} | logfmt)[5m] offset 5m)`,
  `sum_over_time({app="foo"} | logfmt | unwrap size [5m] offset 1h)`.
- Case-insensitive keyword (`OFFSET`).

## 6. `label_replace`  [V]

`label_replace(v, "dst", "replacement", "src", "regex")`, exactly 5 args (otherwise 400).
- **Anchored** match: regex `"a"` does not match `service="a:c"` (series unchanged); `"a:.*"`,
  `".*:.*"`, `"a:c|zzz"`, `"(?i)A:C"` match; `":c"` and `"(a)"` do not; trailing `\n` does not.
- Replacement uses `$1`, `${1}z`, `X$1` (an unset group gives empty).
- No match -> series unchanged; match with empty result removes the dst label.
- dst may overwrite an existing label (`"app" := "zz"`); empty replacement removes it.
- Missing src with regex `""` or `".*"` matches (empty source) and sets dst, as in PromQL.
- Invalid regex rejected at parse time (400); backtick strings accepted; can be nested and used
  inside `sum(... ) by (foo)`.
- Instant-query behaviour (a GitHub issue title reported failures) was not tested [?]; only
  `query_range` was used.
- Enforcement note for T3.3: dst/src label names are in strings, which the PLAN §7.3 identifier
  scan skips. Since `label_replace` can create or overwrite any label (e.g. a policy label) and
  can change what later aggregations group on, T3.3 needs an explicit rule: scan the dst/src
  string arguments against denied labels, and reject writes to injected-label names.

## Not exercised (follow-ups)

`\uXXXX` escapes; a leading comment line; `group_left` result semantics; instant queries;
`bool` with set operators; relative order of `and` vs `unless`; Loki endpoints other than
`query_range`; parse-time behaviour of other versions.

## Doc URLs used (provenance)

- https://grafana.com/docs/loki/latest/query/
- https://grafana.com/docs/loki/latest/query/log_queries/
- https://grafana.com/docs/loki/latest/query/metric_queries/
- https://grafana.com/docs/loki/latest/query/query_examples/
- https://grafana.com/docs/loki/latest/query/query_reference/
- (search result listing only, not read) https://grafana.com/docs/loki/latest/query/template_functions/

Docs were retrieved through a summarising fetch tool, so doc wording is paraphrased.
