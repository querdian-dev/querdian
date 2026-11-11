// The tests in this file are Querdian additions.

package regexparse

import (
	"errors"
	"os"
	"regexp/syntax"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

var corpus = []string{
	"", "a", "abc", "a|b|c", "(?:a)|(?:bc)", "a*", "a+?", "a??", "(a)", "(?P<n>a)", "[a-z]", "[^a-z]", ".", "^a$",
	"a{3}", "a{2,5}", "a{2,}", "(?:ab){10}", "x(?:y|z{4})*w", "(?i)abc", `\d+\s*\w\D\S\W`, `\pL`, `\PL`, `\p{Greek}`,
	`[\p{Greek}\d]`, `[^\pL\pN]`, `(?i)[a-z]+`, `(?i)[^a]`, `[[:alpha:]]`, `[[:^digit:]x]`, `\Q[a\E`, `\x{41}\x42\101`,
	`(?i)k`, `(?s).`, `(?m)^a$`, `(?U)a*`, `abc|abd|aef|bcx|bcy`, `a|b|[c-e]|f`, `(?i)[\x{100}-\x{1FF}]`, `\b\B\A\z`,
	`(`, `)`, `a**`, `a{1001}`, `[z-a]`, `\8`, `(?<n>a)`, `[\x{110000}]`,
}

var flagSets = []syntax.Flags{syntax.Perl, syntax.Perl | syntax.DotNL, syntax.POSIX, syntax.Perl | syntax.FoldCase, syntax.Literal}

// same reports whether the copy and regexp/syntax give the same tree, or both an error with the same code.
func same(t testing.TB, expr string, flags syntax.Flags) {
	t.Helper()
	want, werr := syntax.Parse(expr, flags)
	got, gerr := Parse(expr, Flags(flags))
	switch {
	case werr != nil || gerr != nil:
		var we *syntax.Error
		var ge *Error
		if !errors.As(werr, &we) || !errors.As(gerr, &ge) || string(we.Code) != string(ge.Code) {
			t.Fatalf("Parse(%q, %d): error %v, regexp/syntax: %v", expr, flags, gerr, werr)
		}
	case got.String() != want.String() || !sameTree(want, got):
		t.Fatalf("Parse(%q, %d) = %s, regexp/syntax: %s", expr, flags, got, want)
	}
}

// sameTree compares the trees node by node: Op, Flags, Rune, Min, Max, Cap, Name and Sub.
func sameTree(a *syntax.Regexp, b *Regexp) bool {
	if int(a.Op) != int(b.Op) || int(a.Flags) != int(b.Flags) || a.Min != b.Min || a.Max != b.Max || a.Cap != b.Cap ||
		a.Name != b.Name || !slices.Equal(a.Rune, b.Rune) || len(a.Sub) != len(b.Sub) {
		return false
	}
	for i := range a.Sub {
		if !sameTree(a.Sub[i], b.Sub[i]) {
			return false
		}
	}
	return true
}

// TestSameAsStdlib: the copy gives the same trees as regexp/syntax, so it does not drift from it.
func TestSameAsStdlib(t *testing.T) {
	for _, expr := range corpus {
		for _, flags := range flagSets {
			same(t, expr, flags)
		}
	}
}

func FuzzSameAsStdlib(f *testing.F) {
	for _, expr := range corpus {
		f.Add(expr)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		if len(expr) > 256 {
			return // small inputs keep each parse fast
		}
		for _, flags := range flagSets {
			same(t, expr, flags)
		}
		checkBudget(t, expr)
	})
}

// checkBudget checks the budget: a parse with the counted work as budget succeeds, and with one step less
// it stops with ErrBudget.
func checkBudget(t testing.TB, expr string) {
	t.Helper()
	re, work, err := ParseBudget(expr, Perl, 1<<40)
	if err != nil || re == nil {
		return
	}
	if _, w, err := ParseBudget(expr, Perl, work); err != nil || w != work {
		t.Fatalf("%q: the budget %d of the counted work gives %d, %v", expr, work, w, err)
	}
	var e *Error
	if _, _, err := ParseBudget(expr, Perl, work-1); !errors.As(err, &e) || e.Code != ErrBudget {
		t.Fatalf("%q: the budget %d gives %v, want ErrBudget", expr, work-1, err)
	}
}

func TestBudget(t *testing.T) {
	for _, expr := range corpus {
		checkBudget(t, expr)
	}
}

// deepGroups returns n nested groups around a concatenation of n runes. Its height check reads the wide
// concatenation once for each level.
func deepGroups(n int) string {
	return strings.Repeat("(?:", n) + strings.Repeat(".", n) + strings.Repeat(").", n)
}

// deepLiteral returns n nested groups, each after a literal rune, around a literal of width runes. The
// parser copies the long literal again at each level.
func deepLiteral(n, width int) string {
	return strings.Repeat("a(?:", n) + strings.Repeat("b", width) + strings.Repeat(")", n)
}

// bytesPerWork bounds the memory of one counted step of work, as nanosPerWork bounds the time.
const bytesPerWork = 2048 / 32

// allocBytes returns the bytes that f allocates.
func allocBytes(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// sharedPrefixes returns n nested groups around an alternation of two branches that start with the same
// starred group of width runes. factor compares the two branches again at each level.
func sharedPrefixes(n, width int) string {
	branch := func(c string) string { return "(?:" + strings.Repeat("a*", width) + ")*" + c }
	return strings.Repeat("(?:", n) + branch("x") + "|" + branch("y") + strings.Repeat(")|z", n)
}

// timing reports whether the time checks run. Time on a shared CI machine is not stable, so the time checks
// run only on request: QUERDIAN_COST_TIMING=1. The memory and work checks always run.
func timing() bool { return os.Getenv("QUERDIAN_COST_TIMING") == "1" && !raceEnabled }

// FuzzWorkBounds: the memory of a parse stays within a fixed part plus bytesPerWork for each counted step,
// and a parse with its counted work as budget succeeds. There is no time check: time is not stable under
// the parallel workers of the fuzzer.
func FuzzWorkBounds(f *testing.F) {
	for _, expr := range corpus {
		f.Add(expr)
	}
	f.Add(deepGroups(300))
	f.Add(deepLiteral(30, 300))
	f.Add(sharedPrefixes(60, 60))
	f.Add("(?i)[" + strings.Repeat(`\x{42}-\x{1E942}`, 3) + "]")
	f.Fuzz(func(t *testing.T, expr string) {
		if len(expr) > 8<<10 {
			return
		}
		var work int64
		if b := allocBytes(func() { _, work, _ = ParseBudget(expr, Perl, 1<<40) }); b > 1<<20+uint64(work)*bytesPerWork { //nolint:gosec // work is not negative
			t.Fatalf("%.40q: %d bytes for %d steps of work", expr, b, work)
		}
		checkBudget(t, expr)
	})
}

// TestWorkBoundsTime: the memory of a parse stays within a fixed part plus bytesPerWork for each counted step,
// and with QUERDIAN_COST_TIMING=1 (see timing) its time within a fixed part plus a part for each step of its
// budget, also for inputs whose full parse would be slow, because the budget stops it.
func TestWorkBoundsTime(t *testing.T) {
	const budget = 1 << 20
	for _, expr := range []string{
		"(?i)[" + strings.Repeat(`\x{42}-\x{1E942}`, 200) + "]",
		`\Q[\E(?i)[` + strings.Repeat(`\x{42}-\x{1E942}`, 200) + "]",
		strings.Repeat(`(?i)[^\pL]`, 2000),
		strings.Repeat(`[\pL\pN\pP\pS\pM]`, 2000),
		strings.Repeat("a|b", 100000),
		strings.Repeat("x", 1<<20),
		deepGroups(13500),
		deepLiteral(1000, 100000),
		sharedPrefixes(600, 900),
	} {
		var work int64
		var err error
		var d time.Duration
		b := allocBytes(func() { // the time excludes the GC of allocBytes
			start := time.Now()
			_, work, err = ParseBudget(expr, Perl, budget)
			d = time.Since(start)
		})
		if b > 1<<20+uint64(work)*bytesPerWork { //nolint:gosec // work is not negative
			t.Errorf("%.30q: %d bytes for %d steps of work", expr, b, work)
		}
		// A step is charged before it runs, so the work can be over the budget only by the step that stopped
		// the parse.
		var e *Error
		if work > budget && (!errors.As(err, &e) || e.Code != ErrBudget) {
			t.Errorf("%.30q: work %d over the budget, error %v", expr, work, err)
		}
		// About 15 ns for each step; 100 ns gives room for a busy machine.
		if timing() && d > time.Millisecond+budget*100*time.Nanosecond {
			t.Errorf("%.30q: %v for a budget of %d", expr, d, budget)
		}
	}
}
