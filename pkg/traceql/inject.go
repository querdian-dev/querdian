package traceql

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Condition is one attribute constraint of a policy term, e.g. resource.k8s.namespace.name = "a".
type Condition struct {
	// Attr is the attribute as written in a policy: a scope (span, resource, event, link or
	// instrumentation) and a name separated by '.', e.g. "resource.service.name", or a name with
	// a leading '.' for an unscoped attribute (".service.name").
	Attr string
	// Op is one of KindEq, KindNeq, KindRegex or KindNotRegex.
	Op Kind
	// Value is the string operand. For KindRegex and KindNotRegex it is a regular expression;
	// TraceQL anchors it implicitly at both ends (docs/research/traceql.md §1).
	Value string
}

// Term is a conjunction (AND) of conditions. A Term must not be empty.
type Term []Condition

// RenderDNF renders terms as a disjunctive normal form that is safe to embed anywhere a boolean
// filter expression is expected. Because && and || have equal precedence and associate left to
// right in TraceQL (docs/research/traceql.md §2), every term is parenthesised explicitly:
//
//	(a = "x" && b = "y") || (c = "z")
//
// Attribute names are rendered unquoted when they consist of letters, digits, '_' and inner '.'
// and quoted otherwise (span."dash-name"); string values are quoted with only \" and \\ escaped.
// Names or values that cannot be rendered safely (empty, control characters, invalid UTF-8,
// unknown scope) yield an error, as does an empty term list or an empty term.
func RenderDNF(terms []Term) (string, error) {
	if len(terms) == 0 {
		return "", errors.New("traceql: no terms to render")
	}
	parts := make([]string, len(terms))
	for i, t := range terms {
		if len(t) == 0 {
			return "", errors.New("traceql: empty term")
		}
		conds := make([]string, len(t))
		for j, c := range t {
			s, err := renderCondition(c)
			if err != nil {
				return "", err
			}
			conds[j] = s
		}
		parts[i] = "(" + strings.Join(conds, " && ") + ")"
	}
	return strings.Join(parts, " || "), nil
}

func renderCondition(c Condition) (string, error) {
	var op string
	switch c.Op {
	case KindEq, KindNeq, KindRegex, KindNotRegex:
		op = c.Op.String()
	default:
		return "", fmt.Errorf("traceql: unsupported operator %v", c.Op)
	}
	attr, err := renderAttr(c.Attr)
	if err != nil {
		return "", err
	}
	val, err := quote(c.Value)
	if err != nil {
		return "", fmt.Errorf("traceql: value of %s: %w", c.Attr, err)
	}
	return attr + " " + op + " " + val, nil
}

// SplitAttr splits an attribute reference as used in policies and tag names into its scope and
// name: "span.x" gives ("span", "x"), ".x" gives ("", "x") and a name without a known scope
// prefix ("x") gives ("", "x").
func SplitAttr(s string) (scope, name string) {
	if strings.HasPrefix(s, ".") {
		return "", s[1:]
	}
	if i := strings.IndexByte(s, '.'); i > 0 && attrScopes[s[:i]] {
		return s[:i], s[i+1:]
	}
	return "", s
}

func renderAttr(a string) (string, error) {
	scope, name := SplitAttr(a)
	if name == "" {
		return "", fmt.Errorf("traceql: empty attribute name in %q", a)
	}
	if scope == "" && !strings.HasPrefix(a, ".") {
		return "", fmt.Errorf("traceql: attribute %q has no known scope", a)
	}
	if simpleName(name) {
		return scope + "." + name, nil
	}
	q, err := quote(name)
	if err != nil {
		return "", fmt.Errorf("traceql: attribute %q: %w", a, err)
	}
	return scope + "." + q, nil
}

// simpleName reports whether name needs no quoting: non-empty dot-separated segments of
// letters, digits and '_', not starting with a digit.
func simpleName(name string) bool {
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		return false
	}
	segStart := true
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '.':
			if segStart {
				return false
			}
			segStart = true
			continue
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		default:
			return false
		}
		segStart = false
	}
	return !segStart
}

// quote renders s as a TraceQL double-quoted string. Only \" and \\ are escaped; control
// characters and invalid UTF-8 cannot be represented and are rejected.
func quote(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("invalid UTF-8")
	}
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			return "", fmt.Errorf("control character %U not allowed", r)
		case r == '"' || r == '\\':
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	sb.WriteByte('"')
	return sb.String(), nil
}

// Inject returns the source of q with the policy filter dnf (as produced by RenderDNF) added to
// every spanset filter:
//
//	{ X }  becomes  { (X) && (dnf) }
//	{ }    becomes  { dnf }
//
// The original text is preserved byte for byte outside the rewritten filters. Every filter of
// q.Filters is rewritten, including those nested in structural operators, spanset operators,
// pipelines, select and metrics functions and the filter argument of compare(...). A query
// without any filter (for example `count() > 10`, which Tempo treats as `{ } | count() > 10`)
// gets the prefix `{ dnf } | `, which is equivalent to the implicit all-spans filter plus the
// policy. Query-level with(...) hints are left untouched.
//
// dnf must be a boolean filter expression whose top level is explicitly parenthesised so that
// it is independent of operator precedence; Inject verifies that `{ dnf }` parses as exactly one
// filter and returns an error otherwise. The result is re-parsed as a final check, so a
// non-nil error is the only way a rewrite can fail.
func Inject(q *Query, dnf string) (string, error) {
	if q == nil {
		return "", errors.New("traceql: nil query")
	}
	probeSrc := "{ " + dnf + " }"
	probe, err := Parse(probeSrc)
	if err != nil {
		return "", fmt.Errorf("traceql: invalid policy filter: %w", err)
	}
	if len(probe.Filters) != 1 || probe.Filters[0].Expr == nil || probe.Filters[0].Start != 0 || probe.Filters[0].End != len(probeSrc) {
		return "", errors.New("traceql: policy filter is not a single filter expression")
	}
	if len(probe.With) != 0 {
		return "", errors.New("traceql: policy filter must not contain hints")
	}

	var out string
	if len(q.Filters) == 0 {
		out = "{ " + dnf + " } | " + q.Source
	} else {
		var sb strings.Builder
		pos := 0
		for _, f := range q.Filters {
			if f.Start < pos || f.End > len(q.Source) {
				return "", errors.New("traceql: overlapping or out-of-range filters")
			}
			sb.WriteString(q.Source[pos:f.Start])
			if len(f.Tokens) == 0 {
				sb.WriteString("{ " + dnf + " }")
			} else {
				s, e := f.InnerRange()
				sb.WriteString("{ (" + q.Source[s:e] + ") && (" + dnf + ") }")
			}
			pos = f.End
		}
		sb.WriteString(q.Source[pos:])
		out = sb.String()
	}
	check, err := Parse(out)
	if err != nil {
		return "", fmt.Errorf("traceql: rewritten query does not parse: %w", err)
	}
	if want := max(len(q.Filters), 1); len(check.Filters) != want {
		return "", fmt.Errorf("traceql: rewritten query has %d filters, want %d", len(check.Filters), want)
	}
	return out, nil
}
