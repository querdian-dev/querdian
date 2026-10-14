# LogQL language research (T0.5)

Status: **docs-only, nothing verified against a running container.** The container runtime
(Podman machine) was not running, so every example is `unverified`. Verified-against Loki
version: **none yet** (pin when experiments are run). Pages read on 2026-10-14, `latest`.

Clean room: only grafana.com/docs pages (list at the end). No Loki source, grammar or tests
were read; nothing from evoila/janus.

Legend: `[doc]` stated by docs, `[unverified]` not run, `[undocumented]` docs silent.

## 1. Binary operators in metric queries

- Arithmetic `+ - * / % ^`: between two literals, a literal and a vector, or two vectors.
  `sum(rate({app="foo"}[1m])) * 2` [doc]
- Comparison `== != > >= < <=`: filter by default; `bool` after the operator yields 0/1:
  `count_over_time({foo="bar"}[1m]) > bool 10` [doc]
- **Set operators `and`, `or`, `unless`** between vectors. Docs show `and`/`or`; `unless` was not
  seen in the pages read (**unverified**):
  - `vector1 or vector2`: all elements of vector1 plus elements of vector2 whose label sets are
    not in vector1.
  - `rate({app=~"foo|bar"}[1m]) and rate({app="bar"}[1m])`
  - `sum(count_over_time({namespace="traefik"}[5m])) or vector(0)`
- Vector matching: `on(...)`, `ignoring(...)`, `group_left(...)`, `group_right(...)`:
  `<vector expr> <bin-op> ignoring(<labels>) group_left(<labels>) <vector expr>` [doc]. Many-to-one
  needs an explicit group modifier.
- Precedence: no table in docs; "regular mathematical convention", same level left-associative;
  `1 + 2 / 3 == 1 + (2 / 3)`, `2 * 3 % 2 == (2 * 3) % 2`. PromQL ordering
  (`^` > `* / %` > `+ -` > comparisons > `and unless` > `or`) is **unverified** here.
- Parser note: `or`/`and` also occur in the label-filter stage (`| a="x" or b="y"`), a different
  construct. Disambiguate by position (inside a pipeline vs between metric expressions).

## 2. Pipeline stage keywords [doc]

Stream selector operators: `=  !=  =~  !~` (regex fully anchored; `.` does not match newline
unless `(?s)`).

| Kind | Forms |
|---|---|
| line filter | `\|=`  `!=`  `\|~`  `!~`, pattern filters `\|>`  `!>` (with `<_>` wildcard), `ip("...")` filter function; line-filter regex is **unanchored** |
| parsers | `json` (optionally `json a="x.y[0]"`), `logfmt` (flags `--strict`, `--keep-empty`), `pattern "<a> <_>"`, `regexp "(?P<n>re)"`, `unpack` |
| label filter | `label op value`, ops `== = != > >= < <= =~ !~`; chained with `and`, `or`, `,`, whitespace; parentheses allowed; evaluated left to right (`a or b and c` == `(a or b) and c`) |
| formatters | `line_format "tmpl"`, `label_format dst=src, dst="{{.x}}"` |
| label ops | `drop l1, l2, m="v"`, `keep l1, l2` |
| other | `decolorize`; `distinct l1, l2` (named by newer docs, description not seen: **unverified**) |
| metric-only | `unwrap label`, `unwrap duration(label)`, `duration_seconds(label)`, `bytes(label)`; only label filters may follow `unwrap` |

Keyword set for the opaque-pipeline validator (T3.2):
`json logfmt pattern regexp unpack line_format label_format drop keep decolorize distinct unwrap
by without on ignoring group_left group_right bool and or unless offset`.
Template content of `line_format` lives inside a string and is not tokenised.

Range aggregations (log range): `rate count_over_time bytes_rate bytes_over_time absent_over_time`.
Unwrapped: `rate rate_counter sum_over_time avg_over_time max_over_time min_over_time
first_over_time last_over_time stdvar_over_time stddev_over_time quantile_over_time(scalar, ...)
absent_over_time`.
Vector aggregations: `sum avg min max stddev stdvar count topk bottomk sort sort_desc`, with
`by (...)` / `without (...)` after the parenthesised argument (before it is expected too,
**unverified**): `<aggr-op>([parameter,] <expr>) [without|by (<labels>)]`; `topk`/`bottomk`
require the parameter. Other functions: `vector(s)`, `label_replace(...)`.

## 3. Strings, raw strings, comments [doc]

- `"..."`: double quoted, backslash escapes (Go-style; exact set **unverified**).
- `` `...` ``: raw string, no escapes; `` `\w+` `` equals `"\\w+"`.
- `#` starts a comment to end of line: `{app="foo"} # anything after is ignored`.
  Experiment: `#` inside strings must not start a comment; multi-line queries.
- Single-quoted strings: not mentioned (**unverified**; assume invalid).

## 4. Duration and bytes literals [doc]

- Duration: decimals with units `ns us (µs) ms s m h`, combinable: `300ms`, `1.5h`, `2h45m`,
  `24s30ms`; range selectors `[5m]`. Units `d`, `w`, `y` are **undocumented/unverified**.
- Bytes: `B kB KB MB GB TB KiB MiB GiB TiB`, e.g. `42MB`, `1.5KiB`, `20B`; another page shows
  `5 MiB`, `3k`, `1G` (space and short suffixes: **unverified**). Used in label filters
  (`| size <= 20KB`).
- Numbers: 64-bit floats (`250`, `89.923`).
- A failed conversion adds an `__error__` label rather than dropping the line.

## 5. `offset` [doc]

Must follow the range selector immediately, inside the function parentheses:
`count_over_time({job="mysql"}[5m] offset 5m)` is valid;
`count_over_time({job="mysql"}[5m]) offset 5m` is invalid.
Experiments: with unwrap (`... | unwrap x [5m] offset 1h`), and `(sel | json)[5m] offset 5m`.

## 6. `label_replace` [doc]

`label_replace(v instant-vector, dst_label string, replacement string, src_label string, regex string)`.
RE2 regex matched against `src_label`; no match leaves the series unchanged; `$1`, `$2` are
capture groups.
`label_replace(rate({job="api-server",service="a:c"} |= "err" [1m]), "foo", "$1", "service", "(.*):.*")`
A search result mentioned an issue about instant queries failing with `label_replace`
(not read; version behaviour **unverified**). Regex anchoring **unverified**.
Enforcement note for T3.3: `label_replace` can create or overwrite labels, and its label names
are in strings, which PLAN §7.3's identifier scan skips. A rule for its dst/src arguments is needed.

## Open items

1. Every example (all unverified).
2. `unless` example, precedence table, `bool` with set operators.
3. String escape set, single quotes, comment edge cases.
4. Duration units beyond `h`, bytes spacing/short suffixes.
5. Full stage list for the pinned Loki version (`distinct`, `|>`).
6. `label_replace` on instant queries and regex anchoring.

## Doc URLs used (provenance)

- https://grafana.com/docs/loki/latest/query/
- https://grafana.com/docs/loki/latest/query/log_queries/
- https://grafana.com/docs/loki/latest/query/metric_queries/
- https://grafana.com/docs/loki/latest/query/query_examples/
- https://grafana.com/docs/loki/latest/query/query_reference/
- (search result listing only, not read) https://grafana.com/docs/loki/latest/query/template_functions/

Pages were retrieved through a summarising fetch tool, so wording is paraphrased; re-read the
pages directly when writing parser tests.
