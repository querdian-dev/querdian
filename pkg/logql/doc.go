// Package logql implements a clean-room LogQL lexer and parser (and, in
// later tasks, an injector) for the Querdian access proxy.
//
// # Clean-room provenance
//
// The package is written only from the public LogQL documentation and from
// black-box experiments recorded in docs/research/logql.md. No source code,
// grammar or tests of grafana/loki, and nothing from evoila/janus, were read.
// Public documentation used:
//
//   - https://grafana.com/docs/loki/latest/query/
//   - https://grafana.com/docs/loki/latest/query/log_queries/
//   - https://grafana.com/docs/loki/latest/query/metric_queries/
//   - https://grafana.com/docs/loki/latest/query/query_examples/
//   - https://grafana.com/docs/loki/latest/query/query_reference/
//
// # Lexer
//
// [Lex] turns a query into a slice of [Token]s. Every token carries the byte
// offsets [Token.Pos] and [Token.End] into the input so that later stages can
// rewrite the query by position. Whitespace and "#" line comments are skipped
// and produce no tokens. Unknown input yields an [*Error] with the byte
// position of the problem.
//
// # Deliberately strict choices
//
// Where docs/research/logql.md is ambiguous or silent the lexer rejects, so
// that the proxy fails closed:
//
//   - Strings: only the escapes \" \\ \a \b \f \n \r \t \v, \xHH (two hex
//     digits) and \NNN (three octal digits, value at most 255) are accepted.
//     \u and \U were not exercised against Loki and are rejected, as are \'
//     and every other escape. Single-quoted strings are rejected. A raw
//     newline, NUL or other control character (except tab) inside a
//     double-quoted string is rejected.
//   - Invalid UTF-8 anywhere in the input is rejected.
//   - Whitespace is space, tab, CR and LF only.
//   - Identifiers match [A-Za-z_][A-Za-z0-9_]*. ':' and '.' are not
//     identifier characters; a stray ':' or '.' is an error.
//   - Numbers: decimal digits with optional fraction and exponent (1, 1.5,
//     .5, 1e3). Hex, "Inf" and "NaN" literals are not recognised ("Inf" is an
//     identifier). A number glued to letters that form neither a valid
//     duration nor a valid bytes literal is an error.
//   - Durations: one or more number+unit groups, units ns us µs μs ms s m h d
//     w y (lower case, no spaces), e.g. 1h30m, 0.25s. The lexer accepts
//     decimals everywhere; the contexts that reject them (range selectors
//     such as [1.5m], and the microsecond unit there) are the parser's job
//     (docs/research/logql.md section 4).
//   - Bytes: one number plus one unit out of B k kB KB Ki KiB M MB Mi MiB G GB
//     Gi GiB T TB Ti TiB. "1kb", "2PB" and a bare "K" were not verified or were
//     rejected by Loki and are rejected. Compound bytes literals are rejected.
//   - "1m" is a duration (minute) and "1M" is bytes (mega); case matters.
//   - Keywords are case-insensitive. Token.Value holds the lower-cased
//     spelling for [Keyword] tokens. Any keyword may legitimately be used as
//     a label name (e.g. {json="x"}); deciding that is the parser's job, and
//     denied-label scans must therefore inspect both [Ident] and [Keyword]
//     tokens.
//   - A comment before the query is lexed like any other comment (not
//     exercised against Loki).
//   - Flags such as --strict are lexed as two '-' operators and an identifier;
//     the parser reassembles them.
//
// # Parser
//
// [Parse] returns a [*Query] with its [QueryKind] (log or metric), the root
// [Expr], and the positions of every stream selector ([Query.Selectors]) and
// range aggregation ([Query.RangeAggs]). [Query.String] renders a canonical
// form: Parse(String(Parse(x))) is equivalent to Parse(x). Parentheses are
// not kept in the tree; the canonical form parenthesises every nested binary
// operation. Anything not recognised is an error (*[ParseError] or the
// lexer's *[Error]); there is no lenient pass-through.
//
// The metric layer (vector aggregations with by/without before or after the
// arguments, binary operators with bool/on/ignoring/group_left/group_right,
// range aggregations with unwrap, offset and literals, vector(), label_replace)
// and stream selectors are parsed into the tree. The log pipeline is
// validated structurally but kept as an opaque token slice with byte
// positions, see [LogExpr.Pipeline].
//
// Operator precedence, lowest first: or; and, unless; comparisons; + -;
// * / %; ^ (right associative). Unary minus binds tightest and is accepted
// only directly in front of a number. and and unless sharing a level is an
// assumption (docs/research/logql.md section 1).
//
// # Parser strictness beyond the lexer
//
//   - Range selectors and offset accept only integer durations and no us
//     unit ([1.5m], [1us] are errors); [5m:1m] subqueries are errors.
//   - offset must directly follow the range inside the function.
//   - A literal on either side of and, or, unless is an error; bool is only
//     accepted on comparisons; group_left/group_right need on or ignoring and
//     are rejected on set operators.
//   - A stream selector needs at least one = or =~ matcher that cannot match
//     the empty string (or != ""); regexes must compile.
//   - Keywords may be label names in selectors, by/without/on/ignoring lists,
//     json/logfmt/drop/keep/label_format names and label filters. A stage
//     keyword directly followed by a comparison operator is a label filter.
//   - "or" directly after a line filter value chains another value
//     (|= "a" or "b"); elsewhere in a pipeline it separates label filters.
//     Label filters may be joined by and, or, ',' or juxtaposition.
//   - logfmt accepts exactly --strict and --keep-empty (each at most once,
//     before the parameters, no whitespace inside the flag); json accepts no
//     flags and every json parameter needs ="expr".
//   - After unwrap only label filters may follow; unwrap needs an unwrapped
//     range function and an unwrapped range function needs unwrap. Not
//     documented for rate with unwrap, so it is rejected. Grouping after the
//     parentheses is accepted only for unwrapped functions.
//   - The only functions are the range aggregations rate, count_over_time,
//     bytes_rate, bytes_over_time, absent_over_time, rate_counter, sum_over_time,
//     avg_over_time, max_over_time, min_over_time, stdvar_over_time,
//     stddev_over_time, quantile_over_time, first_over_time, last_over_time;
//     the vector aggregations sum avg min max count stddev stdvar topk bottomk
//     sort sort_desc; and vector, label_replace. Everything else (distinct,
//     approx_topk, abs, count_values, ...) is rejected. topk/bottomk/vector
//     take plain numbers, label_replace takes exactly five arguments with
//     string literals.
//   - A log query must start with '{'; a bare or parenthesised selector as an
//     operand of a metric expression is an error.
//   - Nesting deeper than 100 levels is rejected.
package logql
