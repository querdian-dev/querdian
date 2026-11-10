package policy

import (
	"slices"
	"testing"
)

// LiteralSet gives a set only for a regex with a finite set of matches within the limit. Each other regex
// gives false, so the cluster scope check rejects it.
func TestLiteralSet(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want []string // nil: no finite set
	}{
		{"cluster-a", []string{"cluster-a"}},
		{"cluster-a|cluster-b", []string{"cluster-a", "cluster-b"}},
		{"cluster-(a|b)", []string{"cluster-a", "cluster-b"}},
		{"cluster-[ab]", []string{"cluster-a", "cluster-b"}},
		{"a|", []string{"", "a"}},
		{"a|a", []string{"a"}},
		{"(?i)a", nil},
		{"[a-z]{3}", nil}, // 17576 strings, above the limit
		{"[^a]", nil},
		{"a)|(b", nil}, // does not parse
		{"a.*", nil},
		{"a+", nil},
		{"a?", []string{"", "a"}},
		{"^a$", nil},
	} {
		got, ok := LiteralSet(tc.expr, 64)
		if ok != (tc.want != nil) || (ok && !slices.Equal(got, tc.want)) {
			t.Errorf("LiteralSet(%q) = %q, %v; want %q", tc.expr, got, ok, tc.want)
		}
	}
}
