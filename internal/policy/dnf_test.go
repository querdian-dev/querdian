package policy

import (
	"errors"
	"math/rand"
	"sort"
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
)

// tm builds a term from specs like "a=1", "b!=2", "c=~x|y", "d!~z".
func tm(tenant string, specs ...string) Term {
	t := Term{Tenant: tenant}
	for _, s := range specs {
		var name, val string
		var op v1.MatchOp
		switch {
		case strings.Contains(s, "!~"):
			name, val, op = cut(s, "!~"), after(s, "!~"), v1.MatchRegexNoMatch
		case strings.Contains(s, "=~"):
			name, val, op = cut(s, "=~"), after(s, "=~"), v1.MatchRegexMatch
		case strings.Contains(s, "!="):
			name, val, op = cut(s, "!="), after(s, "!="), v1.MatchNotEqual
		default:
			name, val, op = cut(s, "="), after(s, "="), v1.MatchEqual
		}
		m, err := newMatcher(name, op, val)
		if err != nil {
			panic(err)
		}
		t.Matchers = append(t.Matchers, m)
	}
	return t
}

func cut(s, sep string) string   { a, _, _ := strings.Cut(s, sep); return a }
func after(s, sep string) string { _, b, _ := strings.Cut(s, sep); return b }

func render(ts []Term) string {
	out := make([]string, len(ts))
	for i, t := range ts {
		ms := make([]string, len(t.Matchers))
		for j, m := range t.Matchers {
			sym := map[v1.MatchOp]string{v1.MatchEqual: "=", v1.MatchNotEqual: "!=", v1.MatchRegexMatch: "=~", v1.MatchRegexNoMatch: "!~"}[m.Op]
			ms[j] = m.Name + sym + m.Value
		}
		sort.Strings(ms)
		prefix := ""
		if t.Tenant != "" {
			prefix = t.Tenant + ":"
		}
		out[i] = prefix + strings.Join(ms, ",")
	}
	return strings.Join(out, " | ")
}

var lim = config.Limits{MaxPolicyTerms: 16}

func TestNormaliseTable(t *testing.T) {
	tests := []struct {
		name         string
		in           []Term
		want         string
		unrestricted bool
		disjoint     bool
		tenants      string
	}{
		{"empty input", nil, "", false, true, ""},
		{"single term", []Term{tm("", "a=1")}, "a=1", false, true, ""},
		{"empty term is unrestricted", []Term{tm("")}, "", true, true, ""},
		{"unrestricted absorbs terms", []Term{tm("", "a=1"), tm(""), tm("", "b=2")}, "", true, true, ""},
		{"unrestricted per tenant absorbs only its tenant", []Term{tm("t1"), tm("t1", "a=1"), tm("t2", "b=2")}, "t1: | t2:b=2", false, true, "t1,t2"},
		{"unrestricted on all tenants", []Term{tm("t1"), tm("t2"), tm("t2", "a=1")}, "", true, true, "t1,t2"},
		{"tenants collected even when unrestricted", []Term{tm("t2"), tm("t1")}, "", true, true, "t1,t2"},
		{"dedup identical", []Term{tm("", "a=1"), tm("", "a=1")}, "a=1", false, true, ""},
		{"dedup ignores matcher order", []Term{tm("", "a=1", "b=2"), tm("", "b=2", "a=1")}, "a=1,b=2", false, true, ""},
		{"dedup duplicate matcher in term", []Term{tm("", "a=1", "a=1")}, "a=1", false, true, ""},
		{"no dedup across tenants", []Term{tm("t1", "a=1"), tm("t2", "a=1")}, "t1:a=1 | t2:a=1", false, true, "t1,t2"},
		{"subsumption drops narrower", []Term{tm("", "a=1"), tm("", "a=1", "b=2")}, "a=1", false, true, ""},
		{"subsumption chain", []Term{tm("", "a=1", "b=2", "c=3"), tm("", "a=1", "b=2"), tm("", "a=1")}, "a=1", false, true, ""},
		{"subsumption needs subset not overlap", []Term{tm("", "a=1", "b=2"), tm("", "a=1", "c=3")}, "a=1,b=2 | a=1,c=3", false, false, ""},
		{"subsumption only within tenant", []Term{tm("t1", "a=1"), tm("t2", "a=1", "b=2")}, "t1:a=1 | t2:a=1,b=2", false, true, "t1,t2"},
		{"subsumption by empty matcher set", []Term{tm("t1"), tm("t1", "a=1"), tm("t1", "b!=2")}, "", true, true, "t1"},
		{"merge two equals", []Term{tm("", "a=1"), tm("", "a=2")}, "a=~(?:1)|(?:2)", false, true, ""},
		{"merge equal and regex", []Term{tm("", "a=1"), tm("", "a=~x|y")}, "a=~(?:1)|(?:x|y)", false, true, ""},
		{"merge with shared other matcher", []Term{tm("", "n=ns", "a=1"), tm("", "n=ns", "a=2")}, "a=~(?:1)|(?:2),n=ns", false, true, ""},
		{"merge to fixpoint of three", []Term{tm("", "a=1"), tm("", "a=2"), tm("", "a=3")}, "a=~(?:(?:1)|(?:2))|(?:3)", false, true, ""},
		{"merge escapes regex metacharacters in Equal", []Term{tm("", "a=1.5"), tm("", "a=2")}, `a=~(?:1\.5)|(?:2)`, false, true, ""},
		{"no merge different other matcher", []Term{tm("", "n=x", "a=1"), tm("", "n=y", "a=2")}, "a=1,n=x | a=2,n=y", false, true, ""},
		{"no merge across tenants", []Term{tm("t1", "a=1"), tm("t2", "a=2")}, "t1:a=1 | t2:a=2", false, true, "t1,t2"},
		{"no merge NotEqual", []Term{tm("", "a!=1"), tm("", "a!=2")}, "a!=1 | a!=2", false, false, ""},
		{"no merge RegexNoMatch", []Term{tm("", "a!~1"), tm("", "a=2")}, "a=2 | a!~1", false, false, ""},
		{"no merge when two matchers on label", []Term{tm("", "a=1", "a!=9"), tm("", "a=2", "a!=9")}, "a!=9,a=1 | a!=9,a=2", false, true, ""},
		{"no merge different arity", []Term{tm("", "a=1", "b=2"), tm("", "a=3")}, "a=1,b=2 | a=3", false, true, ""},
		{"merge enables subsumption", []Term{tm("", "a=1"), tm("", "a=2"), tm("", "a=~(?:1)|(?:2)", "b=3")}, "a=~(?:1)|(?:2)", false, true, ""},
		{"disjoint equal values", []Term{tm("", "n=a", "x=1"), tm("", "n=b", "y=2")}, "n=a,x=1 | n=b,y=2", false, true, ""},
		{"not disjoint when one lacks the label", []Term{tm("", "n=a"), tm("", "x=1")}, "n=a | x=1", false, false, ""},
		{"not disjoint equal vs regex", []Term{tm("", "n=a", "x=1"), tm("", "n=~b", "y=2")}, "n=a,x=1 | n=~b,y=2", false, false, ""},
		{"disjoint by tenant", []Term{tm("t1", "x=1"), tm("t2", "y=1")}, "t1:x=1 | t2:y=1", false, true, "t1,t2"},
		{"same tenant overlapping not disjoint", []Term{tm("t1", "x=1"), tm("t1", "y=1")}, "t1:x=1 | t1:y=1", false, false, "t1"},
		{"output sorted deterministically", []Term{tm("", "z=1", "a=1"), tm("", "b=1")}, "a=1,z=1 | b=1", false, false, ""},
		{"all steps combined", []Term{
			tm("t1", "ns=a"), tm("t1", "ns=a", "x=1"), tm("t1", "ns=b"), tm("t1", "ns=b"), tm("t2", "ns=c"), tm("t2")},
			"t1:ns=~(?:a)|(?:b) | t2:", false, true, "t1,t2"},
	}
	if len(tests) < 30 {
		t.Fatalf("need at least 30 cases, have %d", len(tests))
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eff, err := Normalise(tc.in, lim)
			if err != nil {
				t.Fatal(err)
			}
			if got := render(eff.Terms); got != tc.want {
				t.Errorf("terms = %q, want %q", got, tc.want)
			}
			if eff.Unrestricted != tc.unrestricted {
				t.Errorf("unrestricted = %v, want %v", eff.Unrestricted, tc.unrestricted)
			}
			if eff.Unrestricted && len(eff.Terms) != 0 {
				t.Error("unrestricted must drop terms")
			}
			if eff.Disjoint != tc.disjoint {
				t.Errorf("disjoint = %v, want %v", eff.Disjoint, tc.disjoint)
			}
			if got := strings.Join(eff.Tenants, ","); got != tc.tenants {
				t.Errorf("tenants = %q, want %q", got, tc.tenants)
			}
		})
	}
}

func TestNormaliseCap(t *testing.T) {
	var in []Term
	for i := 0; i < 5; i++ {
		in = append(in, tm("", "n!="+string(rune('a'+i))))
	}
	if _, err := Normalise(in, config.Limits{MaxPolicyTerms: 5}); err != nil {
		t.Fatalf("at the cap must pass: %v", err)
	}
	_, err := Normalise(in, config.Limits{MaxPolicyTerms: 4})
	if !errors.Is(err, ErrTooComplex) {
		t.Fatalf("want ErrTooComplex, got %v", err)
	}
	// The cap applies to the normalised form, so reducible input passes.
	red := []Term{tm("", "a=1"), tm("", "a=1", "b=2"), tm("", "a=1", "c=3")}
	if _, err := Normalise(red, config.Limits{MaxPolicyTerms: 1}); err != nil {
		t.Fatalf("reducible input must pass: %v", err)
	}
}

func TestNormaliseDoesNotMutateInput(t *testing.T) {
	in := []Term{tm("", "b=2", "a=1"), tm("", "a=1")}
	before := render(in)
	if _, err := Normalise(in, lim); err != nil {
		t.Fatal(err)
	}
	if render(in) != before || in[0].Matchers[0].Name != "b" {
		t.Fatal("input was mutated")
	}
}

// --- semantic equivalence -------------------------------------------------------------------------------

var (
	propLabels  = []string{"a", "b", "c"}
	propValues  = []string{"", "1", "2", "3", "x.y"}
	propRegexes = []string{"1", "1|2", "2|3", ".*", "", "x.y", "x\\.y|3"}
	propTenants = []string{"t1", "t2"}
)

func randTerm(r *rand.Rand, tenants []string) Term {
	t := Term{Tenant: tenants[r.Intn(len(tenants))]}
	n := r.Intn(4) // 0 matchers = unrestricted for that tenant
	for i := 0; i < n; i++ {
		name := propLabels[r.Intn(len(propLabels))]
		var op v1.MatchOp
		var val string
		switch r.Intn(4) {
		case 0:
			op, val = v1.MatchEqual, propValues[r.Intn(len(propValues))]
		case 1:
			op, val = v1.MatchNotEqual, propValues[r.Intn(len(propValues))]
		case 2:
			op, val = v1.MatchRegexMatch, propRegexes[r.Intn(len(propRegexes))]
		default:
			op, val = v1.MatchRegexNoMatch, propRegexes[r.Intn(len(propRegexes))]
		}
		m, err := newMatcher(name, op, val)
		if err != nil {
			panic(err)
		}
		t.Matchers = append(t.Matchers, m)
	}
	return t
}

func matchTerms(ts []Term, tenant string, labels map[string]string) bool {
	for _, t := range ts {
		if t.Tenant != tenant {
			continue
		}
		ok := true
		for _, m := range t.Matchers {
			if !m.Matches(labels[m.Name]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func checkEquivalence(t *testing.T, seed int64) {
	t.Helper()
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test data
	n := 1 + r.Intn(7)
	in := make([]Term, n)
	for i := range in {
		in[i] = randTerm(r, propTenants)
	}
	eff, err := Normalise(in, config.Limits{MaxPolicyTerms: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	tenantOK := map[string]bool{}
	for _, tn := range eff.Tenants {
		tenantOK[tn] = true
	}
	for k := 0; k < 60; k++ {
		labels := map[string]string{}
		for _, l := range propLabels {
			if r.Intn(4) > 0 { // sometimes absent
				labels[l] = propValues[r.Intn(len(propValues))]
			}
		}
		for _, tenant := range propTenants {
			want := matchTerms(in, tenant, labels)
			var got bool
			if eff.Unrestricted {
				got = tenantOK[tenant]
			} else {
				got = matchTerms(eff.Terms, tenant, labels)
			}
			if got != want {
				t.Fatalf("seed %d: tenant %s labels %v: normalised=%v input=%v\nin:  %s\nout: %s (unrestricted=%v)",
					seed, tenant, labels, got, want, render(in), render(eff.Terms), eff.Unrestricted)
			}
		}
	}
	// Disjoint must be sound: if claimed, no label set matches two normalised terms of one tenant.
	if eff.Disjoint && !eff.Unrestricted {
		for k := 0; k < 200; k++ {
			labels := map[string]string{}
			for _, l := range propLabels {
				labels[l] = propValues[r.Intn(len(propValues))]
			}
			// Terms of different tenants never match the same (tenant, labels), so count per tenant.
			for _, tenant := range propTenants {
				c := 0
				for _, term := range eff.Terms {
					if term.Tenant == tenant && matchTerms([]Term{term}, tenant, labels) {
						c++
					}
				}
				if c > 1 {
					t.Fatalf("seed %d: Disjoint claimed but %d terms of %s match %v: %s", seed, c, tenant, labels, render(eff.Terms))
				}
			}
		}
	}
}

func TestNormaliseSemanticEquivalence(t *testing.T) {
	for seed := int64(0); seed < 3000; seed++ {
		checkEquivalence(t, seed)
	}
}

func FuzzNormaliseEquivalence(f *testing.F) {
	for _, s := range []int64{0, 1, 42, 1 << 40} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, seed int64) { checkEquivalence(t, seed) })
}
