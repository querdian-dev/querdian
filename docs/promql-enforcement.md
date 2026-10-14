# PromQL enforcement

Implementation: `internal/enforce/promql`. Spec: PLAN §7.2. Queries are rewritten on the parsed AST
(`prometheus/promql/parser`) and the result is parsed again before it is sent upstream; if it does
not re-parse the request is rejected (`ErrUnsupported`).

## Query endpoints (`query`, `query_range`, `query_exemplars`)

| Policy | Strategy | Rewrite |
|---|---|---|
| unrestricted, or a term without matchers | `inject` / `passthrough` | query unchanged (the tenant header still applies) |
| one term | `inject` | the term's matchers are appended to every vector selector (also inside range vectors and subqueries); a user matcher on the same label stays ANDed and can only narrow |
| several terms | `or_lift` | see below |

`query_exemplars` evaluates nothing: Prometheus extracts the selectors of the expression and returns
the exemplars of the series they match. The lifted query contains one selector per term, so the result
is the union of the terms.

### Or-lifting (several terms)

* Instant selector `v` (not inside a range vector): `(v{t1} or v{t2} or ...)`.
* Function with a range-vector argument: `(f(.., m{t1}, ..) or f(.., m{t2}, ..))`.
* Subqueries: the inner expression is rewritten recursively.
* `offset`, `@`, `start()` and `end()` are kept on every copy.
* Selector copies = selectors of the original query x terms; each copy carries exactly its original
  matchers plus the matchers of one term (including a possible `__tenant_id__` binding). The number of
  copies is capped (2048); beyond that the request is rejected (`ErrTooComplex`).
* A term without matchers is unrestricted for the tenant header: the union is unrestricted, the query is
  forwarded unchanged. A term that only contains the `__tenant_id__` binding is an ordinary restriction.

### Exactness

`or` removes duplicates by label set, so the lifted query is exactly the query over the union of the
terms when every duplicate is the same series with the same sample. This holds for instant selectors
(a series matching several terms has the same labels and value on every side) and for all functions
that work per series (every function over a range vector). The proxy never guesses: when a query
cannot be lifted exactly it is rejected with `unsupported` (HTTP 400) instead of risking a wrong or
over-granting result:

* a range vector that is not the direct argument of a function, e.g. `up[5m]` as the whole query
  (`or` is not defined on range vectors). A single-term policy accepts it;
* a function over a range vector with another argument that contains a selector, e.g.
  `quantile_over_time(scalar(y), x[1h])`: that argument would have to see the union of all terms, but
  the copy for term *i* can only carry term *i*.

### Documented deviations (never a leak)

* **`absent()` / `absent_over_time()` lose label inference.** Prometheus builds the labels of the
  result from the equality matchers of a plain selector argument. The lifted argument is an `or`
  expression, so the result has no labels (with one term the labels of the user's equality matchers
  *and* of the policy matchers are inferred). The value stays exact: 1 iff no series of the union
  exists. `absent_over_time(m)` is rewritten to `absent(last_over_time(m{t1}) or ...)` because a
  per-term copy would report "absent" for each term without data even when another term has data.
* A function that drops the metric name makes Prometheus fail when two distinct series differ only in
  their name. Across terms this collision is resolved silently by `or`. It requires a term that itself
  matches on `__name__`.

## Metadata endpoints (`series`, `labels`, `label/:name/values`)

Strategy `match_expand`. The API returns the union of all `match[]` selectors, so the enforcer sends
one selector per (user `match[]` x term), which is exactly `(user selectors) AND (term 1 OR term 2 ...)`:

| Client sends | Upstream receives |
|---|---|
| no `match[]` | one `{t_i}` per term (omission is never a pass-through) |
| `match[]=u1&match[]=u2` | `u_j` + `t_i` for every pair (identical results are sent once) |
| unrestricted policy, or a term without matchers | the user's `match[]` unchanged (none if there is none) |

* A user matcher on a policy label stays ANDed and can only narrow.
* A selector must contain a matcher that does not match the empty string. If user and term matchers
  do not (for example only `env!="dev"`), `__name__=~".+"` is added; this can only narrow.
* More than 256 expanded selectors are rejected (`ErrTooComplex`).
* Denied labels: a denied label in a user `match[]` selector is rejected (403), as is
  `label/<denied>/values` (403). `series` responses are filtered like `labels` (`promql.FilterSeriesResponse`
  removes the denied names from every label set and collapses sets that became identical). Query results are
  deliberately NOT filtered: a series returned by a query keeps all its labels, including denied ones.
* `labels` responses: the plan carries `Plan.ResponseFilter` (`promql.FilterLabelsResponse`) which removes
  denied names from `data`. The proxy buffers the (2xx) body up to `Plan.ResponseFilterMaxBytes`
  (`promql.MaxLabelsResponseBytes`, 16 MiB), applies the filter and fails closed (502) when the filter
  returns an error or the cap is exceeded. All other plans have no filter and stream.
