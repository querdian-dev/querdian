package policy

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
)

// ErrTooComplex is returned when the normalised policy has more terms than limits.maxPolicyTerms.
// It maps to 403 with reason policy_too_complex.
var ErrTooComplex = errors.New("policy_too_complex")

// newMatcher builds a compiled matcher without name validation; the regex is anchored.
func newMatcher(name string, op v1.MatchOp, value string) (Matcher, error) {
	m := Matcher{Name: name, Op: op, Value: value, k: fmt.Sprintf("%q %s %q", name, op, value)}
	if op == v1.MatchRegexMatch || op == v1.MatchRegexNoMatch {
		src := "^(?:" + value + ")$"
		re, err := regexp.Compile(src)
		if err != nil {
			return Matcher{}, err
		}
		m.re = &lazyRegexp{src: src, re: re}
		m.re.once.Do(func() {}) // already compiled
	}
	return m, nil
}

// newMergedMatcher builds a RegexMatch from regexes that are already known to be valid, without compiling.
func newMergedMatcher(name, value string) Matcher {
	return Matcher{
		Name: name, Op: v1.MatchRegexMatch, Value: value,
		k:  fmt.Sprintf("%q %s %q", name, v1.MatchRegexMatch, value),
		re: &lazyRegexp{src: "^(?:" + value + ")$"},
	}
}

func sortMatchers(ms []Matcher) []Matcher {
	out := append([]Matcher(nil), ms...)
	sort.Slice(out, func(i, j int) bool { return out[i].k < out[j].k })
	return out
}

// key identifies a term; its matchers must already be canonical (sorted and unique).
func (t Term) key() string {
	var b strings.Builder
	b.WriteString(t.Tenant)
	b.WriteByte('|')
	for i, m := range t.Matchers {
		if i > 0 {
			b.WriteString(" & ")
		}
		b.WriteString(m.k)
	}
	return b.String()
}

// Normalise reduces a DNF to an equivalent, smaller one (PLAN §6.3, steps 1-6). The result matches a
// (tenant, label set) pair if and only if the input does. The output is canonical: equal inputs in any order
// yield identical outputs.
//
// Deviation from the letter of step 1: an unrestricted rule (a term without matchers) absorbs only the terms
// of its own tenant. The result is Unrestricted only if every tenant present has such a term. Absorbing
// across tenants would over-grant (unrestricted on t1 must not widen a restricted grant on t2).
func Normalise(terms []Term, limits config.Limits) (Effective, error) {
	tenantSet := map[string]bool{}
	cur := make([]Term, len(terms))
	for i, t := range terms {
		if t.Tenant != "" {
			tenantSet[t.Tenant] = true
		}
		cur[i] = Term{Tenant: t.Tenant, Matchers: uniqueSorted(t.Matchers)}
	}
	eff := Effective{Tenants: sortedKeys(tenantSet)}

	for {
		cur = dedupSorted(cur)
		var subsumed, merged bool
		cur, subsumed = subsume(cur)
		cur, merged = mergeSingleLabel(cur)
		if !subsumed && !merged {
			break
		}
	}

	// Step 1 (per tenant): every tenant unrestricted -> Unrestricted.
	unrestricted, present := map[string]bool{}, map[string]bool{}
	for _, t := range cur {
		present[t.Tenant] = true
		if len(t.Matchers) == 0 {
			unrestricted[t.Tenant] = true
		}
	}
	if len(cur) > 0 && len(unrestricted) == len(present) {
		eff.Unrestricted = true
		eff.Disjoint = true
		return eff, nil
	}

	eff.Terms = cur
	eff.Disjoint = allDisjoint(cur)
	if len(cur) > limits.MaxPolicyTerms {
		return Effective{}, fmt.Errorf("%w: %d terms exceed limit %d", ErrTooComplex, len(cur), limits.MaxPolicyTerms)
	}
	return eff, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// uniqueSorted returns the matchers sorted canonically with duplicates removed.
func uniqueSorted(ms []Matcher) []Matcher {
	out := sortMatchers(ms)
	j := 0
	for i, m := range out {
		if i > 0 && m.k == out[j-1].k {
			continue
		}
		out[j] = m
		j++
	}
	return out[:j]
}

// dedupSorted removes identical terms (step 2) and returns the terms sorted by key.
func dedupSorted(in []Term) []Term {
	type keyed struct {
		k string
		t Term
	}
	ks := make([]keyed, len(in))
	for i, t := range in {
		ks[i] = keyed{t.key(), t}
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i].k < ks[j].k })
	out := make([]Term, 0, len(ks))
	for i, k := range ks {
		if i > 0 && k.k == ks[i-1].k {
			continue
		}
		out = append(out, k.t)
	}
	return out
}

// isSubset reports whether every matcher of a (sorted, unique) is in b (sorted, unique).
func isSubset(a, b []Matcher) bool {
	j := 0
	for _, m := range a {
		for j < len(b) && b[j].k < m.k {
			j++
		}
		if j == len(b) || b[j].k != m.k {
			return false
		}
		j++
	}
	return true
}

// subsume drops term b if another term a of the same tenant has a strict subset of b's matchers (step 3).
func subsume(in []Term) ([]Term, bool) {
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

func mergeable(op v1.MatchOp) bool { return op == v1.MatchEqual || op == v1.MatchRegexMatch }

func asRegex(m Matcher) string {
	if m.Op == v1.MatchEqual {
		return regexp.QuoteMeta(m.Value)
	}
	return m.Value
}

// mergeSingleLabel merges same-tenant terms that are identical except for one label, where every one of them
// uses Equal or RegexMatch for that label, into a single RegexMatch alternation (step 4). Terms are grouped by
// signature (tenant, label, all other matchers), so one pass merges whole groups.
func mergeSingleLabel(in []Term) ([]Term, bool) {
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
		merged := newMergedMatcher(first.Matchers[ms[0].pos].Name, strings.Join(alts, "|"))
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

// allDisjoint reports whether every pair of terms is provably disjoint (step 5): different tenants, or an
// Equal matcher on the same label with different values in both terms.
func allDisjoint(ts []Term) bool {
	for i := range ts {
		for j := i + 1; j < len(ts); j++ {
			if !disjoint(ts[i], ts[j]) {
				return false
			}
		}
	}
	return true
}

func disjoint(a, b Term) bool {
	if a.Tenant != b.Tenant {
		return true
	}
	for _, ma := range a.Matchers {
		if ma.Op != v1.MatchEqual {
			continue
		}
		for _, mb := range b.Matchers {
			if mb.Op == v1.MatchEqual && mb.Name == ma.Name && mb.Value != ma.Value {
				return true
			}
		}
	}
	return false
}
