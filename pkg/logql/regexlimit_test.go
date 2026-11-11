package logql

import (
	"errors"
	"regexp/syntax"
	"strings"
	"testing"
	"time"

	"github.com/querdian-dev/querdian/internal/regexcost"
)

// selectorRegexWeight returns the program size of the regex matchers of the stream selectors of q.
func selectorRegexWeight(t *testing.T, q *Query) int {
	t.Helper()
	n := 0
	for _, s := range q.Selectors {
		for _, m := range s.Matchers {
			if m.Op != "=~" && m.Op != "!~" {
				continue
			}
			w, err := regexcost.Weight("^(?:"+m.Value+")$", syntax.Perl, regexcost.Compile, 1<<30)
			if err != nil {
				t.Fatalf("%q: a regex that parsed does not parse: %v", m.Value, err)
			}
			n += w
		}
	}
	return n
}

func TestParseRegexLimit(t *testing.T) {
	w, err := regexcost.Weight("^(?:a{1000})$", syntax.Perl, regexcost.Compile, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	q := `{a=~"a{1000}"} != "x"`
	if _, err := ParseWithRegexLimit(q, w); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if _, err := ParseWithRegexLimit(q, w-1); !errors.Is(err, ErrRegexTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
	lr := `label_replace(count_over_time({a="b"}[1m]), "d", "$1", "s", "x{1000}y{1000}")`
	if _, err := ParseWithRegexLimit(lr, 1000); !errors.Is(err, ErrRegexTooLarge) {
		t.Fatalf("label_replace over the limit: %v", err)
	}
	if _, err := ParseWithRegexLimit(lr, 3000); err != nil {
		t.Fatalf("label_replace: %v", err)
	}
	// The limit counts all regexes of the query, and the rejection is fast although each compile would be
	// slow.
	big := `{a=~"` + strings.Repeat("(?:a{1000})", 1400) + `"} or ` + `{a=~"` + strings.Repeat("(?:a{1000})", 1400) + `"}`
	big = "count_over_time(" + strings.ReplaceAll(big, " or ", "[1m]) or count_over_time(") + "[1m])"
	start := time.Now()
	if _, err := Parse(big); !errors.Is(err, ErrRegexTooLarge) {
		t.Fatalf("want ErrRegexTooLarge, got %v", err)
	}
	if d := time.Since(start); d > timeScale*time.Second {
		t.Errorf("the rejection took %v", d)
	}
}

// TestParseClientBytes: the byte limits of client regexes apply before any parse of a regex, but not in
// Parse, which reads queries that the rewriter made.
func TestParseClientBytes(t *testing.T) {
	long := strings.Repeat("a", regexcost.MaxRegexBytes)
	if _, err := ParseClient(`{a=~"`+long+`"}`, 1<<30, regexcost.MaxQueryRegexBytes); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	q := `{a=~"` + long + `a"}`
	if _, err := ParseClient(q, 1<<30, regexcost.MaxQueryRegexBytes); !errors.Is(err, ErrRegexTooLarge) {
		t.Errorf("over the limit: %v", err)
	}
	if _, err := Parse(q); err != nil {
		t.Errorf("Parse: %v", err)
	}
	if _, err := ParseClient(`{a=~"aa", b=~"aa"}`, 1<<30, 3); !errors.Is(err, ErrRegexTooLarge) {
		t.Errorf("over the limit of the query: %v", err)
	}
}

// TestParseRegexInvalid: a regex that does not parse is not compiled after the measure, and the error is a
// parse error, not ErrRegexTooLarge.
func TestParseRegexInvalid(t *testing.T) {
	bad := `(?i)` + strings.Repeat(`[\\x{100}-\\x{1E900}]`, 2) + `(`
	var ms []string
	for i := range 20 {
		ms = append(ms, `l`+strings.Repeat("x", i)+`=~"`+bad+`"`)
	}
	start := time.Now()
	_, err := Parse(`{a="b", ` + strings.Join(ms, ", ") + `}`)
	var pe *ParseError
	if !errors.As(err, &pe) || errors.Is(err, ErrRegexTooLarge) {
		t.Fatalf("want a parse error, got %v", err)
	}
	if d := time.Since(start); d > timeScale*time.Second {
		t.Errorf("the rejection took %v", d)
	}
}

// FuzzParseRegexLimit: a query that parses with a limit has selector regexes of at most that size.
func FuzzParseRegexLimit(f *testing.F) {
	for _, c := range corpus {
		f.Add(c.q, 100)
	}
	f.Add(`{a=~"a{1000}", b!~"(?:x{10}){10}"}`, 1500)
	f.Fuzz(func(t *testing.T, src string, limit int) {
		limit = max(0, limit%(1<<16))
		q, err := ParseWithRegexLimit(src, limit)
		if err != nil {
			if errors.Is(err, ErrRegexTooLarge) {
				if q2, err2 := ParseWithRegexLimit(src, MaxRegexWeight*16); err2 == nil && selectorRegexWeight(t, q2) <= limit && !strings.Contains(strings.ToLower(src), "label_replace") {
					t.Fatalf("%q rejected with limit %d, but its regexes have %d", src, limit, selectorRegexWeight(t, q2))
				}
			}
			return
		}
		if w := selectorRegexWeight(t, q); w > limit {
			t.Fatalf("%q parsed with limit %d, but its regexes have %d", src, limit, w)
		}
	})
}

func TestParseClientBytesError(t *testing.T) {
	_, err := ParseClient(`{a=~"`+strings.Repeat("a", regexcost.MaxRegexBytes+1)+`"}`, 1<<30, regexcost.MaxQueryRegexBytes)
	if !errors.Is(err, ErrRegexTooLong) || !errors.Is(err, ErrRegexTooLarge) {
		t.Fatalf("want ErrRegexTooLong and ErrRegexTooLarge, got %v", err)
	}
	if _, err := ParseClient(`{a=~"a{1000}"}`, 10, regexcost.MaxQueryRegexBytes); errors.Is(err, ErrRegexTooLong) {
		t.Fatalf("a weight error is not a byte error: %v", err)
	}
}
