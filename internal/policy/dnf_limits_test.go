package policy

import (
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/querdian-dev/querdian/internal/config"
)

// The reference functions are the quadratic forms of subsume and mergeSingleLabel from before the work
// limits. The fast forms must give the same terms in the same order.

func referenceSubsume(in []Term) ([]Term, bool) {
	out := make([]Term, 0, len(in))
	for j, b := range in {
		dropped := false
		for i, a := range in {
			if i != j && a.Tenant == b.Tenant && len(a.Matchers) < len(b.Matchers) && isSubset(a.Matchers, b.Matchers) {
				dropped = true
				break
			}
		}
		if !dropped {
			out = append(out, b)
		}
	}
	return out, len(out) != len(in)
}

// referenceMergeSingleLabel builds a signature string for each matcher.
func referenceMergeSingleLabel(in []Term) ([]Term, bool) {
	type member struct{ term, pos int }
	groups := map[string][]member{}
	var order []string
	for ti, t := range in {
		for pos, m := range t.Matchers {
			if !mergeable(m.Op) {
				continue
			}
			unique := true
			for pi, o := range t.Matchers {
				if pi != pos && o.Name == m.Name {
					unique = false
					break
				}
			}
			if !unique {
				continue
			}
			var b strings.Builder
			b.WriteString(t.Tenant)
			b.WriteByte('\x00')
			b.WriteString(m.Name)
			for pi, o := range t.Matchers {
				if pi != pos {
					b.WriteString("\x00")
					b.WriteString(o.k)
				}
			}
			sig := b.String()
			if _, ok := groups[sig]; !ok {
				order = append(order, sig)
			}
			groups[sig] = append(groups[sig], member{ti, pos})
		}
	}

	consumed := make([]bool, len(in))
	var created []Term
	for _, sig := range order {
		var ms []member
		for _, m := range groups[sig] {
			if !consumed[m.term] {
				ms = append(ms, m)
			}
		}
		if len(ms) < 2 {
			continue
		}
		alts := make([]string, len(ms))
		for i, m := range ms {
			alts[i] = "(?:" + asRegex(in[m.term].Matchers[m.pos]) + ")"
		}
		first := in[ms[0].term]
		merged := newMergedMatcher(first.Matchers[ms[0].pos].Name, strings.Join(alts, "|"), 0)
		rest := make([]Matcher, 0, len(first.Matchers))
		for pi, m := range first.Matchers {
			if pi != ms[0].pos {
				rest = append(rest, m)
			}
		}
		created = append(created, Term{Tenant: first.Tenant, Matchers: uniqueSorted(append(rest, merged))})
		for _, m := range ms {
			consumed[m.term] = true
		}
	}
	if len(created) == 0 {
		return in, false
	}
	out := make([]Term, 0, len(in))
	for i, t := range in {
		if !consumed[i] {
			out = append(out, t)
		}
	}
	return append(out, created...), true
}

// referenceNormalise runs the loop of Normalise with the reference functions. It returns the terms before
// the unrestricted check and the cap.
func referenceNormalise(terms []Term) []Term {
	cur := make([]Term, len(terms))
	for i, t := range terms {
		cur[i] = Term{Tenant: t.Tenant, Matchers: uniqueSorted(t.Matchers)}
	}
	for {
		cur = dedupSorted(cur)
		var subsumed, merged bool
		cur, subsumed = referenceSubsume(cur)
		cur, merged = referenceMergeSingleLabel(cur)
		if !subsumed && !merged {
			return cur
		}
	}
}

func checkSameAsReference(t *testing.T, seed int64) {
	t.Helper()
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test data
	n := 1 + r.Intn(12)
	in := make([]Term, n)
	for i := range in {
		if r.Intn(2) == 0 {
			in[i] = randTerm(r, propTenants)
		} else {
			in[i] = randMixedTerm(r, propTenants)
		}
	}
	eff, err := Normalise(in, config.Limits{MaxPolicyTerms: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	ref := referenceNormalise(in)
	if eff.Unrestricted {
		for _, term := range ref {
			if len(term.Matchers) > 0 {
				// Unrestricted needs a term without matchers in each tenant; that term subsumes the others.
				t.Fatalf("seed %d: unrestricted, but the reference keeps %s", seed, render(ref))
			}
		}
		return
	}
	if got, want := render(eff.Terms), render(ref); got != want {
		t.Fatalf("seed %d:\nin:   %s\ngot:  %s\nwant: %s", seed, render(in), got, want)
	}
}

func TestNormaliseSameAsReference(t *testing.T) {
	for seed := int64(0); seed < 3000; seed++ {
		checkSameAsReference(t, seed)
	}
}

func FuzzNormaliseSameAsReference(f *testing.F) {
	for _, s := range []int64{0, 7, 99, 1 << 33} {
		f.Add(s)
	}
	f.Fuzz(checkSameAsReference)
}

// TestNormaliseSameAsReferenceWithCollisions: with one hash for all signatures, sameSignature alone must
// separate the groups.
func TestNormaliseSameAsReferenceWithCollisions(t *testing.T) {
	constantSignatureHash = true
	defer func() { constantSignatureHash = false }()
	for seed := int64(0); seed < 3000; seed++ {
		checkSameAsReference(t, seed)
	}
}

// TestNormaliseResultLimit: a merge into a regex with a weight of more than MaxResultWeight is rejected.
func TestNormaliseResultLimit(t *testing.T) {
	const valueLen = 100
	// An Equal value has the weight valueLen+1, and a merge adds 1 for each value.
	most := MaxResultWeight / (valueLen + 2)
	for _, n := range []int{most, most + 1} {
		var in []Term
		for i := 0; i < n; i++ {
			id := fmt.Sprint(i, "-")
			in = append(in, tm("", "namespace="+id+strings.Repeat("n", valueLen-len(id))))
		}
		eff, err := Normalise(in, lim)
		if n == most && (err != nil || len(eff.Terms) != 1 || TermsWeight(eff.Terms) > MaxResultWeight) {
			t.Fatalf("at the limit: %v, %d terms", err, len(eff.Terms))
		}
		if n > most && (!errors.Is(err, ErrTooComplex) || !strings.Contains(err.Error(), "weight")) {
			t.Fatalf("over the limit: want ErrTooComplex, got %v", err)
		}
	}
}

// TestMergedWeightIsUpperBound: the weight of a merged matcher is at least the size of its compiled program.
func TestMergedWeightIsUpperBound(t *testing.T) {
	for seed := int64(0); seed < 2000; seed++ {
		r := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic test data
		var in []Term
		for i := 0; i < 2+r.Intn(6); i++ {
			m := baseSpace.randMatcher(r)
			in = append(in, Term{Matchers: []Matcher{m}})
		}
		eff, err := Normalise(in, config.Limits{MaxPolicyTerms: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		for _, term := range eff.Terms {
			for _, m := range term.Matchers {
				exact := weight(m.Op, m.Value)
				if m.Weight() < exact {
					t.Fatalf("seed %d: %s has weight %d, its program %d", seed, m.k, m.Weight(), exact)
				}
			}
		}
	}
}

// TestMergeSingleLabelLongTerms: a term with many matchers on different labels must not cost memory that
// grows with the square of its matchers.
func TestMergeSingleLabelLongTerms(t *testing.T) {
	var ms []Matcher
	for i := 0; i < 4000; i++ {
		ms = append(ms, tm("", fmt.Sprintf("l%d=v", i)).Matchers[0])
	}
	in := []Term{{Matchers: uniqueSorted(ms)}}
	alloc := allocBytes(func() {
		if _, merged := mergeSingleLabel(in); merged {
			t.Fatal("one term must not merge")
		}
	})
	if alloc > 16<<20 {
		t.Fatalf("mergeSingleLabel allocated %d bytes for one term with 4000 matchers", alloc)
	}
}

// mergeChain returns n terms that merge one by one: each term is the merge of all terms before it, with one
// label changed to a new value. Each pass of Normalise thus merges only two terms.
func mergeChain(t *testing.T, n int) []Term {
	t.Helper()
	labels := []string{"p", "q", "r", "s"}
	cur := tm("", "p=p0", "q=q0", "r=r0", "s=s0")
	out := []Term{cur}
	for i := 1; i < n; i++ {
		l := labels[i%len(labels)]
		next := Term{}
		for _, m := range cur.Matchers {
			if m.Name == l {
				m = tm("", fmt.Sprintf("%s=%s%d", l, l, i)).Matchers[0]
			}
			next.Matchers = append(next.Matchers, m)
		}
		eff, err := Normalise([]Term{cur, next}, lim)
		if err != nil || len(eff.Terms) != 1 {
			t.Fatalf("step %d: %v, %d terms", i, err, len(eff.Terms))
		}
		out = append(out, next)
		cur = eff.Terms[0]
	}
	return out
}

// TestNormaliseWorkLimit: input that needs many passes is rejected, and the rejection is fast.
func TestNormaliseWorkLimit(t *testing.T) {
	chain := mergeChain(t, 64)
	// The chain alone is within the limit.
	if eff, err := Normalise(chain, lim); err != nil || len(eff.Terms) != 1 {
		t.Fatalf("the chain must merge to one term: %v", err)
	}
	// Other terms make each pass expensive.
	in := chain
	for i := len(in); i < MaxResolveTerms; i++ {
		in = append(in, tm("", fmt.Sprintf("namespace!=%d", i)))
	}
	var err error
	start := time.Now()
	alloc := allocBytes(func() { _, err = Normalise(in, config.Limits{MaxPolicyTerms: 1 << 20}) })
	if !errors.Is(err, ErrTooComplex) || !strings.Contains(err.Error(), "steps") {
		t.Fatalf("want ErrTooComplex for the work limit, got %v", err)
	}
	if d := time.Since(start); d > timeScale*5*time.Second || alloc > 256<<20 {
		t.Fatalf("the rejection took %v and %d bytes", d, alloc)
	}
}

// TestNormaliseManyTermsMerge: many terms that differ in one label merge within the work limit, also when
// all terms have the same first matcher (the implicit cluster matcher of hub AccessPolicy rules).
func TestNormaliseManyTermsMerge(t *testing.T) {
	var in []Term
	for i := 0; i < MaxResolveTerms; i++ {
		in = append(in, tm("", "cluster=mla", fmt.Sprintf("namespace=ns%d", i)))
	}
	// A longer term in each namespace is subsumed.
	for i := 0; i < 64; i++ {
		in = append(in, tm("", "cluster=mla", fmt.Sprintf("namespace=ns%d", i), "team=a"))
	}
	eff, err := Normalise(in, lim)
	if err != nil {
		t.Fatal(err)
	}
	if len(eff.Terms) != 1 || len(eff.Terms[0].Matchers) != 2 {
		t.Fatalf("want one merged term, got %d terms", len(eff.Terms))
	}
}

func allocBytes(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}
