package policy

import (
	"regexp/syntax"
	"slices"
)

// LiteralSet returns the strings a fully anchored regex matches when that set is finite and has at most
// limit members. Case folding, repetition, anchors and every other construct report false.
func LiteralSet(expr string, limit int) ([]string, bool) {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return nil, false
	}
	set, ok := expand(re.Simplify(), limit)
	if !ok {
		return nil, false
	}
	slices.Sort(set)
	return slices.Compact(set), true
}

func expand(re *syntax.Regexp, limit int) ([]string, bool) {
	switch re.Op {
	case syntax.OpEmptyMatch:
		return []string{""}, true
	case syntax.OpLiteral:
		if re.Flags&syntax.FoldCase != 0 {
			return nil, false
		}
		return []string{string(re.Rune)}, true
	case syntax.OpCharClass:
		var out []string
		for i := 0; i+1 < len(re.Rune); i += 2 {
			for r := re.Rune[i]; r <= re.Rune[i+1]; r++ {
				if len(out) == limit {
					return nil, false
				}
				out = append(out, string(r))
			}
		}
		return out, true
	case syntax.OpCapture:
		return expand(re.Sub[0], limit)
	case syntax.OpQuest:
		sub, ok := expand(re.Sub[0], limit)
		if !ok || len(sub) == limit {
			return nil, false
		}
		return append(sub, ""), true
	case syntax.OpAlternate:
		var out []string
		for _, s := range re.Sub {
			sub, ok := expand(s, limit)
			if !ok || len(out)+len(sub) > limit {
				return nil, false
			}
			out = append(out, sub...)
		}
		return out, true
	case syntax.OpConcat:
		out := []string{""}
		for _, s := range re.Sub {
			sub, ok := expand(s, limit)
			if !ok || len(out)*len(sub) > limit {
				return nil, false
			}
			next := make([]string, 0, len(out)*len(sub))
			for _, p := range out {
				for _, x := range sub {
					next = append(next, p+x)
				}
			}
			out = next
		}
		return out, true
	default:
		return nil, false
	}
}
