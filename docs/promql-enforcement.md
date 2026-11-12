# PromQL enforcement

This page tells how Querdian rewrites PromQL queries and `match[]` selectors for a policy.

In short:

* Querdian rewrites the parsed syntax tree (`prometheus/promql/parser`) in `internal/enforce/promql`.
  It parses the result again before it sends it upstream. If the result does not parse, the request is
  rejected (`ErrUnsupported`).
* One policy term: Querdian adds the matchers of the term to each vector selector.
* Several terms: Querdian makes one copy of each selector for each term and joins the copies with `or`
  ("or-lifting"). A query that it cannot lift exactly is `400 unsupported`.
* Metadata endpoints: Querdian sends one `match[]` selector for each pair of user selector and term.

The rejected query forms are also in [limitations.md](limitations.md#promql).

## Query endpoints (`query`, `query_range`, `query_exemplars`)

| Policy | Strategy | Rewrite |
|---|---|---|
| unrestricted, or a term without matchers | `inject` / `passthrough` | query unchanged (the tenant header still applies) |
| one term | `inject` | the term's matchers are appended to every vector selector (also inside range vectors and subqueries); a user matcher on the same label stays ANDed and can only narrow |
| several terms | `or_lift` | see below |

`query_exemplars` evaluates nothing: Prometheus gets the selectors of the expression and returns the
exemplars of the series that they match. The lifted query has one selector for each term, so the
result is the union of the terms.

### Or-lifting (several terms)

| Query part | Rewrite |
|---|---|
| Instant selector `v` (not in a range vector) | `(v{t1} or v{t2} or ...)` |
| Function with a range-vector argument | `(f(.., m{t1}, ..) or f(.., m{t2}, ..))` |
| Subquery | The inner expression is rewritten recursively. |
| `offset`, `@`, `start()`, `end()` | Kept on each copy. |

* Selector copies = selectors of the original query x terms. Each copy has exactly its original
  matchers plus the matchers of one term (also a `__tenant_id__` binding, if there is one). Above 2048
  copies, the request is rejected (`ErrTooComplex`).
* A term without matchers is unrestricted for the tenant header: the union is unrestricted, and the
  query goes upstream unchanged. A term with only the `__tenant_id__` binding is a usual restriction.

### Exactness

`or` removes duplicates by label set. Thus the lifted query is exactly the query over the union of the
terms when each duplicate is the same series with the same sample. This is true for instant selectors
(a series that matches several terms has the same labels and value on each side), and for all
functions that work on each series separately (each function over a range vector).

The proxy never guesses. When it cannot lift a query exactly, it rejects it with `unsupported` (HTTP
400), and does not risk a wrong result or too much data:

* A range vector that is not the direct argument of a function, for example `up[5m]` as the full
  query: `or` is not defined on range vectors. A single-term policy accepts it.
* A function over a range vector with a different argument that contains a selector, for example
  `quantile_over_time(scalar(y), x[1h])`: that argument must see the union of all terms, but the copy
  for term *i* can only have term *i*.

### Documented deviations (never a leak)

* **`absent()` and `absent_over_time()` lose the label inference.** Prometheus makes the labels of
  the result from the equality matchers of a plain selector argument. The lifted argument is an `or`
  expression, so the result has no labels. With one term, the labels of the equality matchers of the
  user *and* of the policy are inferred. The value stays exact: 1 if and only if no series of the
  union exists. `absent_over_time(m)` becomes `absent(last_over_time(m{t1}) or ...)`, because a copy
  for each term would report "absent" for each term without data, also when a different term has data.
* **A function that drops the metric name.** Prometheus fails when two different series differ only in
  their name. Across terms, `or` resolves this collision silently. This needs a term that itself
  matches on `__name__`.

## Metadata endpoints (`series`, `labels`, `label/:name/values`)

The strategy is `match_expand`. The API returns the union of all `match[]` selectors, so the enforcer
sends one selector for each pair (user `match[]` x term). This is exactly
`(user selectors) AND (term 1 OR term 2 ...)`:

| Client sends | Upstream receives |
|---|---|
| no `match[]` | one `{t_i}` per term (omission is never a pass-through) |
| `match[]=u1&match[]=u2` | `u_j` + `t_i` for every pair (identical results are sent once) |
| unrestricted policy, or a term without matchers | the user's `match[]` unchanged (none if there is none) |

* A user matcher on a policy label stays ANDed and can only narrow.
* A selector must have a matcher that does not match the empty string. If the user and term matchers
  have none (for example only `env!="dev"`), `__name__=~".+"` is added. This can only narrow.
* More than 256 expanded selectors are rejected (`ErrTooComplex`).

### Denied labels

* A denied label in a user `match[]` selector is rejected (403), and so is `label/<denied>/values`.
* `labels` responses: `Plan.ResponseFilter` (`promql.FilterLabelsResponse`) removes the denied names
  from `data`.
* `series` responses: `promql.FilterSeriesResponse` removes the denied names from each label set, and
  collapses sets that became identical.
* For these filters, the proxy buffers the (2xx) body up to `Plan.ResponseFilterMaxBytes`
  (`promql.MaxLabelsResponseBytes`, 16 MiB). If the filter returns an error or the body is larger, the
  request fails closed (502).
* Query results are not filtered on purpose: a series in a query result keeps all its labels, also
  the denied ones. Only `labels.redact` rewrites query results
  ([policy-guide.md](policy-guide.md#redacting-values)). Without it, all other plans have no filter and
  stream.
