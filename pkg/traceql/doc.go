// Package traceql is a clean-room TraceQL front end used by Querdian to locate and rewrite
// spanset filters. This file documents provenance and the deliberate strictness of the lexer.
//
// # Provenance
//
// The package is written only from public documentation and black-box experiments against a
// Tempo container (see docs/research/traceql.md). No Grafana Tempo source code, grammar or tests
// were consulted. Public pages used:
//
//   - https://grafana.com/docs/tempo/latest/traceql/
//   - https://grafana.com/docs/tempo/latest/traceql/construct-traceql-queries/
//   - https://grafana.com/docs/tempo/latest/api_docs/
//   - https://grafana.com/docs/tempo/latest/metrics-from-traces/metrics-queries/
//
// # Lexer strictness (fail closed)
//
// Where the research is ambiguous the lexer rejects the input rather than guessing, because a
// token stream that differs from the upstream's reading could hide an attribute from policy
// checks. In detail:
//
//   - Comments are rejected (# is a syntax error upstream; the acceptance of // is unexplained),
//     so #, // and /* are lex errors.
//   - Double-quoted strings and quoted attribute names accept only the escapes \" and \\. Any
//     other escape (including \n), raw control characters and invalid UTF-8 are errors.
//     Backtick strings are raw (no escapes) and may not contain control characters either.
//   - Unquoted attribute names consist of letters, digits, '_' and '.' (no empty segments).
//     Upstream also accepts '-' and '/' unquoted; here an attribute name immediately followed
//     by '-' or '/' is an error ("quote the name") because it could equally be arithmetic.
//   - An identifier directly followed by '.' must be one of the scopes span, resource, event,
//     link or instrumentation. parent.* is rejected (unsupported upstream), as are upper-case
//     or unknown scopes. An identifier directly followed by ':' must be a known intrinsic scope
//     and the name must be a known intrinsic of that scope.
//   - Durations are a single number plus one unit of ns, us, ms, s, m, h, d. Compound forms such
//     as 1h30m, numbers glued to letters, and exponents are errors.
//   - A '.' followed by digits only (".99") is lexed as a number, as in the documented
//     quantile_over_time(duration, .99). A parser must therefore not accept a number token where
//     an attribute is expected; ".99abc" is lexed as an attribute.
//   - The bare words name, status, kind, duration, statusMessage, childCount, traceDuration,
//     rootName and rootServiceName are the legacy unscoped intrinsics and are emitted as
//     Intrinsic tokens with an empty scope. Everything else (true, false, nil, error, by, ...)
//     is an Ident; keyword handling belongs to the parser.
//   - A lone '&' is an error; '|' and '||' are distinct tokens.
//   - Operators use maximal munch, so "!~" is one token whether it means "not regex" or
//     "negated sibling"; the parser disambiguates by position.
package traceql
