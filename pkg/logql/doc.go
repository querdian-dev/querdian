// Package logql implements a clean-room LogQL lexer (and, in later tasks, a
// parser and injector) for the Querdian access proxy.
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
package logql
