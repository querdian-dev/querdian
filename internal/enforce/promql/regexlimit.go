package promql

import (
	"regexp/syntax"

	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/util/strutil"

	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/regexcost"
)

// checkQueryRegexes returns the total weight of the regex matchers of the client inputs (a query or all
// match[] values), or an ErrTooComplex error when it is over limit. The Prometheus parser
// compiles each regex matcher while it parses, so this check reads the tokens of the lexer and runs before
// the parser. It changes nothing; an input that does not lex stops the scan, and the parser rejects it at
// the same place. A regex that does not parse gives ErrParse.
func checkQueryRegexes(limit int, inputs ...string) (int, error) {
	total, bytes := 0, 0
	for _, in := range inputs {
		l := parser.Lex(in)
		var prev parser.ItemType
		// Each item except EOF takes at least one byte, so the loop ends; the bound is a second guard.
		for range len(in) + 2 {
			var it parser.Item
			l.NextItem(&it)
			if it.Typ == parser.EOF || it.Typ == parser.ERROR {
				break
			}
			if it.Typ == parser.COMMENT {
				continue
			}
			if it.Typ == parser.STRING && (prev == parser.EQL_REGEX || prev == parser.NEQ_REGEX) {
				// The same unquote and flags as the parser and labels.NewFastRegexMatcher; the anchors add 2.
				// A regex that does not unquote or parse is rejected here: the parser would compile it and
				// go on after the error. The limit of Weight is what is left, so that all parses of Weight
				// together stay within the limit.
				v, err := strutil.Unquote(it.Val)
				if err != nil {
					return 0, enforce.Errorf(enforce.ErrParse, "invalid string %s: %v", it.Val, err)
				}
				// The byte limits do not depend on the work counter of the regex parser.
				if bytes += len(v); len(v) > regexcost.MaxRegexBytes || bytes > regexcost.MaxQueryRegexBytes {
					return 0, enforce.QueryRegexBytesError()
				}
				n, err := regexcost.Weight(v, syntax.Perl|syntax.DotNL, regexcost.CompileAndPrint, limit-total)
				if err != nil {
					return 0, enforce.Errorf(enforce.ErrParse, "invalid regular expression: %v", err)
				}
				if total += n + 2; total > limit {
					return 0, enforce.QueryRegexError(limit)
				}
			}
			prev = it.Typ
		}
	}
	return total, nil
}
