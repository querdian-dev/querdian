package promql

import (
	"errors"
	"regexp/syntax"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/regexcost"
)

// parsedRegexWeight returns the program size of the regex matchers of the parsed query, as checkQueryRegexes
// counts it.
func parsedRegexWeight(t *testing.T, expr parser.Expr) int {
	t.Helper()
	n := 0
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		vs, ok := node.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		for _, m := range vs.LabelMatchers {
			if m.Type != labels.MatchRegexp && m.Type != labels.MatchNotRegexp {
				continue
			}
			w, err := regexcost.Weight(m.Value, syntax.Perl|syntax.DotNL, regexcost.CompileAndPrint, 1<<30)
			if err != nil {
				t.Fatalf("a regex that parsed does not parse: %v", err)
			}
			n += w + 2
		}
		return nil
	})
	return n
}

func TestQueryRegexLimit(t *testing.T) {
	w, err := regexcost.Weight("a{1000}", syntax.Perl|syntax.DotNL, regexcost.CompileAndPrint, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	k := enforce.MaxQueryRegexWeight / (w + 2) // the 2 anchors
	query := func(k int) string {
		ms := make([]string, k)
		for i := range ms {
			ms[i] = `l` + strings.Repeat("x", i) + `=~"a{1000}"`
		}
		return "up{" + strings.Join(ms, ",") + "}"
	}
	if _, err := plan(query(k), nsTerm()); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if _, err := plan(query(k+1), nsTerm()); !errors.Is(err, enforce.ErrTooComplex) {
		t.Fatalf("over the limit: %v", err)
	}
	// Comments, other quotes and a regex in a match[] value count too.
	for _, q := range []string{
		"up{a=~ # comment\n'" + strings.Repeat("(?:a{1000})", 40) + "'}",
		"up{a!~`" + strings.Repeat("(?:a{1000})", 40) + "`}",
		`up{a=~"` + strings.Repeat(`\\pL`, 30) + `"}`,
		`up{a=~"` + strings.Repeat(`[^/]`, 20) + `"}`,
	} {
		if _, err := checkQueryRegexes(enforce.MaxQueryRegexWeight, q); !errors.Is(err, enforce.ErrTooComplex) {
			t.Errorf("%.30q: %v", q, err)
		}
	}
	if _, err := checkQueryRegexes(enforce.MaxQueryRegexWeight, `up{a=~"`+strings.Repeat("(?:a{1000})", 20)+`"}`, `up{b=~"`+strings.Repeat("(?:a{1000})", 20)+`"}`); !errors.Is(err, enforce.ErrTooComplex) {
		t.Errorf("match[] values: %v", err)
	}
}

// TestQueryRegexBytes: the byte limits of client regexes apply before any parse of a regex.
func TestQueryRegexBytes(t *testing.T) {
	long := strings.Repeat("a", regexcost.MaxRegexBytes)
	if _, err := checkQueryRegexes(enforce.MaxQueryRegexWeight, `up{a=~"`+long+`"}`); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	if _, err := checkQueryRegexes(enforce.MaxQueryRegexWeight, `up{a=~"`+long+`a"}`); !errors.Is(err, enforce.ErrTooComplex) {
		t.Errorf("over the limit: %v", err)
	}
	var many []string
	for range regexcost.MaxQueryRegexBytes/1000 + 1 {
		many = append(many, `up{a=~"`+strings.Repeat("a", 1000)+`"}`)
	}
	if _, err := checkQueryRegexes(1<<30, many...); !errors.Is(err, enforce.ErrTooComplex) {
		t.Errorf("over the limit of a query: %v", err)
	}
}

// TestQueryRegexInvalid: a client regex that does not parse is rejected before the Prometheus parser, which
// would compile it and go on after the error.
func TestQueryRegexInvalid(t *testing.T) {
	bad := `(?i)` + strings.Repeat(`[\\x{100}-\\x{1E900}]`, 2) + `(`
	var ms []string
	for i := range 20 {
		ms = append(ms, `l`+strings.Repeat("x", i)+`=~"`+bad+`"`)
	}
	start := time.Now()
	_, err := plan("up{"+strings.Join(ms, ",")+"}", nsTerm())
	if !errors.Is(err, enforce.ErrParse) {
		t.Fatalf("want ErrParse, got %v", err)
	}
	if d := time.Since(start); d > timeScale*time.Second {
		t.Errorf("the rejection took %v", d)
	}
	if _, err := checkQueryRegexes(enforce.MaxQueryRegexWeight, `up{a=~"\\xZZ"}`); !errors.Is(err, enforce.ErrParse) {
		t.Errorf("an escape that does not unquote: %v", err)
	}
}

// FuzzQueryRegexLimit: a query that passes checkQueryRegexes and parses has regex matchers of at most
// enforce.MaxQueryRegexWeight, and the plan for it allocates at most regexcost.MaxBytesPerUnit for each unit
// of the request weight, plus a part for the length of the query.
func FuzzQueryRegexLimit(f *testing.F) {
	for _, q := range []string{`up{a=~"a{1000}"}`, `sum(rate(x{b!~'c.*'}[5m])) by (d)`, "up{a=~ # c\n`(?:a{100}){5}`}",
		`{__name__=~"x{1000}y{1000}"} or z{w=~"q{500}"}`, `label_replace(up, "a", "$1", "b", "(.{1000})")`} {
		f.Add(q)
	}
	f.Fuzz(func(t *testing.T, q string) {
		if len(q) > 4096 {
			return
		}
		_, checkErr := checkQueryRegexes(enforce.MaxQueryRegexWeight, q)
		switch {
		case errors.Is(checkErr, enforce.ErrParse):
			// The check rejects only what the parser rejects too.
			if _, err := promParser.ParseExpr(q); err == nil {
				t.Fatalf("%q: the check gives %v, but the query parses", q, checkErr)
			}
			return
		case checkErr != nil && !errors.Is(checkErr, enforce.ErrTooComplex):
			t.Fatalf("unexpected error %v", checkErr)
		case checkErr != nil:
			return // the parser would compile too much
		}
		expr, err := promParser.ParseExpr(q)
		if err != nil {
			return
		}
		w := parsedRegexWeight(t, expr)
		if w > enforce.MaxQueryRegexWeight {
			t.Fatalf("%q passed the check, but its regexes have a weight of %d", q, w)
		}
		eff := effWith(policy.Matcher{Name: "namespace", Op: v1.MatchRegexMatch, Value: `team-[a-z]+|\pL{3}`})
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		_, _ = plan(q, eff)
		runtime.ReadMemStats(&b)
		bound := 1<<20 + regexcost.MaxBytesPerUnit*enforce.RequestWeight(vectorSelectors(expr), w, eff) + 1024*len(q)
		if got := b.TotalAlloc - a.TotalAlloc; got > uint64(bound) { //nolint:gosec // bound is positive
			t.Fatalf("%q: the plan allocated %d bytes, more than %d", q, got, bound)
		}
	})
}
