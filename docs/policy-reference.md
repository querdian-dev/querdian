# Policy reference

This page gives the details of the [policy guide](policy-guide.md): how Querdian finds denied labels,
how it applies the terms of a policy for each backend, and how it calculates the weight of a regex.
Read the policy guide first.

## Denied labels in queries and endpoints

Querdian rejects a query with `403 denied_label` if the query uses a denied label:

| Language | Where Querdian finds the label |
|---|---|
| PromQL (AST based) | In each selector matcher, `by`/`without`, `on`/`ignoring`, `group_left`/`group_right`, and the label-name arguments of `label_replace`, `label_join` and `count_values`. This applies to unrestricted users too. |
| LogQL (token based, conservative) | Each identifier or keyword token that is equal to the denied name, outside string literals, at each position in the query (selector, pipeline, grouping, `unwrap`). The `dst` and `src` string arguments of `label_replace`. Each `line_format` or `label_format` template that does not parse, that reads the denied label as a field (`{{ .customer_id }}`), or that uses a template node or function outside an allow-list, for example `{{ . }}`, `$` variables, `{{ .a.b }}`, `range`, `with`, `template`, `block`, `define` or `index` ([limitations](limitations.md#denied-label-scan-false-positives)). |
| TraceQL | Each attribute or intrinsic token with that name. A scoped denied name (`span.x`) denies `span.x` and the unscoped `.x`. An unscoped query token `.x` is denied by `span.x` and by `resource.x`. |

Endpoints that list labels do not list denied labels. Endpoints that ask for the values of a denied
label answer `403`:

| Backend | Endpoint | Effect |
|---|---|---|
| Prometheus | `labels` | The names are removed from the response. |
| Prometheus | `series` | The labels are removed from each returned label set. Sets that become identical are collapsed. |
| Prometheus | `label/<denied>/values`, a denied label in `match[]` | `403` |
| Loki | `labels`, `detected_labels`, `detected_fields` | The names are removed. |
| Loki | `label/<denied>/values`, `detected_field/<denied>/values` | `403` |
| Loki | `index/volume*` | Denied labels are removed from the reported label sets. `targetLabels` that names a denied label: `403`. |
| Tempo | `api/v2/search/tags` | Denied tags are removed. |
| Tempo | `api/v2/search/tag/<denied>/values` | `403` |
| Tempo | trace by ID (`traceByID: filter`) | Denied attributes are removed from the span, event, link, scope and resource attributes of each returned span. |

Query results keep the denied labels. See
[What it does NOT do](policy-guide.md#what-it-does-not-do).

## Which strategy does what

The table shows how each backend applies the effective terms. For details, see
[promql-enforcement.md](promql-enforcement.md), [limitations](limitations.md) and
[endpoints.md](endpoints.md).

| Backend | Single term | Several terms |
|---|---|---|
| Prometheus: `query`, `query_range`, `query_exemplars` | matchers appended to every selector (`inject`) | or-lifting: `(v{t1} or v{t2})` per selector or per function over a range vector (`or_lift`); one upstream request |
| Prometheus: `series`, `labels`, `label/<n>/values` | one `match[]` per user selector (`match_expand`) | one `match[]` per (user selector x term); one request, the API returns the union |
| Loki: log queries | matchers injected into the selector (`inject`) | **one upstream request per term**, results merged, deduplicated and cut to `limit` (`fanout`) |
| Loki: metric queries | `inject` | or-lifting per range aggregation (`or_lift`); one request |
| Loki: `labels`, label values, `detected_*` | inject into the optional `query` | one request per term, union of names (`fanout`) |
| Loki: `series` | `match_expand` | `match_expand` (one request) |
| Loki: `index/stats`, `index/volume*`, `patterns` | inject | one request per term, counters summed; an estimate unless the terms are disjoint |
| Loki: `tail` | one upstream WebSocket | one WebSocket per term, frames merged and deduplicated |
| Tempo: `search`, `metrics/*` | the policy is injected into every spanset filter | same: a parenthesised DNF `(a && b) \|\| (c && d)` is injected; no fan-out |
| Tempo: tags, tag values | one filter `{ a = "x" && b = "y" }`; `!=`, `!~` and open regexes are `400` | one request per term (regexes with a fixed value set expanded), union of names or values (`fanout`); see [limitations](limitations.md#traceql-and-tempo) |
| Tempo: trace by ID | response filter | response filter |

* If a user is unrestricted, or has a term without matchers, and has no denied labels, Querdian
  forwards the request without a query rewrite or parse. Only the tenant header and the parameter
  allow-list apply.
* If a restricted user omits the enforcement parameter (`query`, `q`, `match[]`), Querdian does not
  pass the request through. It sends a selector with only the policy matchers.

## Regex weight

The [size limits](policy-guide.md#size-limits) and the
[request limits](operations.md#limits-and-what-happens-when-they-trip) use the weight of a regex. The
weight measures the cost of a compile. The compiler and the receiver get it from a parse with a work
budget, without a compile. The weight is the sum of:

* the instructions of the compiled program (`a{1000}` has 1000);
* the rune ranges of each character class, counted again for each repetition (`\pL` has about 700);
* the work of the parser divided by 32, for example for the case folding of a wide range in a `(?i)`
  class, rune by rune;
* for regexes that the Prometheus matcher can get: the runes of each character class in the
  case-folding range of Unicode, divided by 64.

| Value | Weight |
|---|---|
| `[a-z]+`, `prod-.*`, `(?i)(warn\|error)\s+\d+`, an alternation of 100 namespace names | some tens to some thousands |
| `[^/]+` | about 1970 |
| `\pL+` | about 1820 |
| A short regex with many counted repetitions or many Unicode classes | can be over the limit |
| An `Equal` or `NotEqual` value | its length plus one |

A compile in Go and in the Prometheus matcher together takes at most about 256 KiB and 2 ms, plus
2 KiB and 2 microseconds for each unit of weight.
