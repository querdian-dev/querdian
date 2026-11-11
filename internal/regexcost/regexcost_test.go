package regexcost

import (
	"fmt"
	"os"
	"regexp"
	"regexp/syntax"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
)

const noLimit = 1 << 30

func compiled(t testing.TB, expr string, flags syntax.Flags) int {
	t.Helper()
	re, err := syntax.Parse(expr, flags)
	if err != nil {
		t.Fatal(err)
	}
	p, err := syntax.Compile(re.Simplify())
	if err != nil {
		t.Fatal(err)
	}
	return len(p.Inst)
}

var exprs = []string{
	"", "a", "abc", "a|b|c", "(?:a)|(?:bc)", "a*", "a+", "a?", "(a)", "[a-z]", ".", "^a$",
	"a{3}", "a{2,5}", "a{2,}", "a{0,}", "(?:ab){1000}", "(?:a{1000})(?:b{1000})", "(?:a{10}){10}",
	"x(?:y|z{4})*w", "(?i)abc", `\d+\s*`, "(?:.*a.*b){50}", "[a-z]{1000}", "a{0,1000}",
	`\pL`, `[^\pL\pN]`, `(?i)[a-z]+`, `\p{Greek}{3}`, `[[:alpha:]]`, `prod-.*`,
}

// adversarial are expressions whose compile cost is large for their length.
var adversarial = []string{
	strings.Repeat(`(?:a{1000})`, 30),
	strings.Repeat(`\pL`, 20),
	strings.Repeat(`\PL`, 20),
	strings.Repeat(`[\pL\pN\pP\pS\pM]`, 15),
	strings.Repeat(`(?i:[\pL\pN\pP\pS\pM])`, 3),
	strings.Repeat(`(?i)[^\pL]`, 3),
	strings.Repeat(`[^a]`, 4000),
	`\p{L}{1000}`, `\p{L}{20}`, `(?i)\p{L}{20}`, `(?i)[\pL\pN]{20}`,
	`(?:\p{L}|\p{N}|\p{P}){100}`,
	`(?i:[\x{41}-\x{1E942}])`, strings.Repeat(`(?i:[\x{100}-\x{17F}])`, 3),
	strings.Repeat(`[a-c]`, 3000), strings.Repeat(`(?:x|y)`, 2000),
	strings.Repeat(`\d\w\s`, 1000),
	strings.Repeat("[ab]"+strings.Repeat("x", 2000), 7),
	"(?i)[" + strings.Repeat(`\x{100}-\x{1E900}`, 20) + "]",
	"(?i)" + strings.Repeat(`[\x{100}-\x{1E900}]`, 20),
	"(?i)[" + strings.Repeat(`a-zA-Z0-9\x{400}-\x{4FF}`, 200) + "]",
	`\s\s\d\d` + strings.Repeat("y", 3000),
	// Nested groups around a wide concatenation: the height check of the parser reads the concatenation
	// once for each level.
	strings.Repeat("(?:", 1300) + strings.Repeat(".", 1300) + strings.Repeat(").", 1300),
	// Nested groups after literal runes around a long literal: the parser copies the literal at each level.
	strings.Repeat("a(?:", 100) + strings.Repeat("b", 7000) + strings.Repeat(")", 100),
	// Nested groups around two branches that start with the same starred group: factor compares the
	// branches again at each level.
	strings.Repeat("(?:", 600) + "(?:" + strings.Repeat("a*", 900) + ")*x|(?:" + strings.Repeat("a*", 900) + ")*y" +
		strings.Repeat(")|z", 600),
}

// TestWeightIsUpperBound: the weight is at least the instruction count of the compiled program.
func TestWeightIsUpperBound(t *testing.T) {
	for _, flags := range []syntax.Flags{syntax.Perl, syntax.Perl | syntax.DotNL} {
		for _, e := range append(append([]string(nil), exprs...), adversarial...) {
			for _, expr := range []string{e, "^(?:" + e + ")$"} {
				got, err := Weight(expr, flags, Compile, noLimit)
				if err != nil {
					t.Fatal(err)
				}
				if want := compiled(t, expr, flags); got < want {
					t.Errorf("Weight(%.40q) = %d, compiled program has %d instructions", expr, got, want)
				}
			}
		}
	}
}

func TestWeightCountsClassesAndFolding(t *testing.T) {
	for _, tc := range []struct {
		expr     string
		mode     Mode
		min, max int
	}{
		{`[a-z]+`, CompileAndPrint, 2, 12},
		{`prod-.*`, CompileAndPrint, 6, 16},
		{`\pL`, Compile, 600, 900},                            // about 700 ranges and the work of the parser
		{`\pL`, CompileAndPrint, 1600, 2000},                  // and about 70000 runes in the fold window / 64
		{`\pL{10}`, Compile, 6000, 8000},                      // the ranges count for each repetition
		{`[^/]`, CompileAndPrint, 1900, 2050},                 // about 125000 runes / 64
		{`[^/]`, Compile, 2, 12},                              // the printer does not run for LogQL
		{`(?i)[a-z]`, CompileAndPrint, 2, 20},                 // a few folded runes
		{`(?i)[\x{100}-\x{1E900}]`, Compile, 3500, 4500},      // folding of about 125000 runes, with partners
		{`[\x{100}-\x{1E900}]`, Compile, 2, 12},               // no folding
		{`(?i)[\x00-\x{10FFFF}]`, Compile, 2, 12},             // the parser adds such a range without folding
		{`\Q[\E(?i)[\x{100}-\x{1E900}]`, Compile, 3500, 4500}, // quoting does not hide the folding
	} {
		got, err := Weight(tc.expr, syntax.Perl, tc.mode, noLimit)
		if err != nil || got < tc.min || got > tc.max {
			t.Errorf("Weight(%q, %d) = %d, %v; want %d..%d", tc.expr, tc.mode, got, err, tc.min, tc.max)
		}
	}
	// Normal regexes stay small.
	alts := make([]string, 100)
	for i := range alts {
		alts[i] = fmt.Sprintf("team-namespace-%03d", i)
	}
	for _, e := range []string{`[a-z]+`, `prod-.*`, `(?i)error`, `.*foo.*`, strings.Join(alts, "|"), `(?i)[a-z]+`,
		`(?i)[a-z0-9-]+\.[a-z]+`, `(?i)(warn|error)\s+\d+`, `(?i)^[a-z]+-[a-z]+-\d+-\w+$`, `[\p{Greek}\p{Latin}]+`,
		`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`, `[0-9a-f]{32}`, `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`} {
		if w, err := Weight("^(?:"+e+")$", syntax.Perl, CompileAndPrint, 16384); err != nil || w > 2500 {
			t.Errorf("Weight(%.40q) = %d, %v", e, w, err)
		}
	}
	// Large Unicode classes and negated classes cost more, but several of them fit into the limit of a
	// client query.
	for _, e := range []string{`\pL+`, `[^/]+`, `[^:]+:9100`, `team-[^-]+`, `(?i)[^/]+`} {
		if w, err := Weight("^(?:"+e+")$", syntax.Perl, CompileAndPrint, 16384); err != nil || w > 4100 {
			t.Errorf("Weight(%.40q) = %d, %v", e, w, err)
		}
	}
}

func TestWeightError(t *testing.T) {
	for _, expr := range []string{"(", "a{1001}", "a**", strings.Repeat("(?:a{1000})", 3400)} {
		if _, err := Weight(expr, syntax.Perl, Compile, noLimit); err == nil {
			t.Errorf("Weight(%.20q) gave no error", expr)
		}
	}
}

func allocs(f func()) (uint64, time.Duration) {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	start := time.Now()
	f()
	d := time.Since(start)
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc, d
}

// TestWeightIsCheap: Weight of large and class-heavy expressions takes little time and memory, also when
// their compile would be expensive, because the estimate from the text stops the parse.
func TestWeightIsCheap(t *testing.T) {
	for _, expr := range []string{
		strings.Repeat("(?:a{1000})", 3300),
		strings.Repeat(`\pL`, 5400),
		strings.Repeat(`[\pL\pN\pP\pS\pM]`, 900),
		strings.Repeat(`(?i:[\x{41}-\x{1E942}])`, 700),
		"(?i)[" + strings.Repeat(`\x{42}-\x{1E942}`, 1000) + "]",
		strings.Repeat(`(?i)[^\pL]`, 1600),
		strings.Repeat("a", 1<<20),
	} {
		var w int
		b, d := allocs(func() { w, _ = Weight(expr, syntax.Perl, CompileAndPrint, 16384) })
		if w <= 16384 {
			t.Errorf("Weight(%.20q) = %d, want over the limit", expr, w)
		}
		if b > 16<<20 || (timing() && d > 500*time.Millisecond) {
			t.Errorf("Weight(%.20q) took %v and %d KiB", expr, d, b>>10)
		}
	}
}

// costPerUnit compiles expr as Querdian and its backends do, and returns the allocated bytes over
// FixedBytes for each unit of weight, and the time over fixedNanos for each unit. Both are the smallest of
// three runs, to make them stable on a busy machine.
func costPerUnit(t testing.TB, expr string) (bytes, nanos float64) {
	t.Helper()
	w, err := Weight("^(?:"+expr+")$", syntax.Perl, CompileAndPrint, noLimit)
	if err != nil {
		t.Fatal(err)
	}
	b := uint64(1 << 62)
	d := time.Duration(1 << 62)
	for range 3 {
		bi, di := allocs(func() {
			_, _ = regexp.Compile("^(?:" + expr + ")$")
			_, _ = labels.NewFastRegexMatcher(expr)
		})
		b, d = min(b, bi), min(d, di)
	}
	return (float64(b) - FixedBytes) / float64(w), (float64(d.Nanoseconds()) - fixedNanos) / float64(w)
}

// fixedNanos is the part of the time of a compile that does not grow with the weight.
const fixedNanos = 2e6

// timing reports whether the time checks run. Time on a shared CI machine is not stable, so the time checks
// run only on request: QUERDIAN_COST_TIMING=1. The memory checks always run.
func timing() bool { return os.Getenv("QUERDIAN_COST_TIMING") == "1" && !raceEnabled }

// checkCost checks the bounds of costPerUnit: memory always, and time when timing is on.
func checkCost(t testing.TB, expr string, checkTime bool) {
	t.Helper()
	bytes, nanos := costPerUnit(t, expr)
	// The fuzzer allocates in the same process, and a busy machine can make one measure slow; a bound must
	// fail in three measures to count.
	for i := 0; i < 2 && (bytes > MaxBytesPerUnit || nanos > MaxNanosPerUnit); i++ {
		b, n := costPerUnit(t, expr)
		bytes, nanos = min(bytes, b), min(nanos, n)
	}
	if bytes > MaxBytesPerUnit {
		t.Errorf("%.40q: %.0f bytes for each unit of weight", expr, bytes)
	}
	if checkTime && nanos > MaxNanosPerUnit {
		t.Errorf("%.40q: %.0f nanoseconds for each unit of weight", expr, nanos)
	}
}

// TestCostPerUnit: for normal and adversarial expressions, a compile in regexp and in the Prometheus matcher
// together takes at most FixedBytes + MaxBytesPerUnit x weight bytes and, with QUERDIAN_COST_TIMING=1,
// fixedNanos + MaxNanosPerUnit x weight nanoseconds.
func TestCostPerUnit(t *testing.T) {
	for _, e := range append(append([]string(nil), exprs...), adversarial...) {
		checkCost(t, e, timing())
	}
}

func FuzzWeight(f *testing.F) {
	for _, e := range append(append([]string(nil), exprs...), adversarial...) {
		f.Add(e)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		got, err := Weight(expr, syntax.Perl, CompileAndPrint, 1<<16)
		if err != nil || got > 1<<16 {
			return // compiling a large program in the fuzzer is slow
		}
		if want := compiled(t, expr, syntax.Perl); got < want {
			t.Fatalf("Weight(%q) = %d, compiled program has %d instructions", expr, got, want)
		}
		if _, err := regexp.Compile("^(?:" + expr + ")$"); err != nil {
			return
		}
		checkCost(t, expr, false) // no time check: time is not stable under the parallel workers of the fuzzer
	})
}
