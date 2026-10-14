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
	m := Matcher{Name: name, Op: op, Value: value}
	if op == v1.MatchRegexMatch || op == v1.MatchRegexNoMatch {
		re, err := regexp.Compile("^(?:" + value + ")$")
		if err != nil {
			return Matcher{}, err
		}
		m.re = re
	}
	return m, nil
}

func (m Matcher) key() string { return fmt.Sprintf("%q %s %q", m.Name, m.Op, m.Value) }

func sortMatchers(ms []Matcher) []Matcher {
	out := append([]Matcher(nil), ms...)
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

func (t Term) key() string {
	parts := make([]string, len(t.Matchers))
	for i, m := range t.Matchers {
		parts[i] = m.key()
	}
	sort.Strings(parts)
	return fmt.Sprintf("%q|%s", t.Tenant, strings.Join(parts, " & "))
}

// Normalise reduces a DNF to an equivalent, smaller one (PLAN §6.3, steps 1-6). The result matches a
// (tenant, label set) pair if and only if the input does.
//
// Deviation from the letter of step 1: an unrestricted rule (a term without matchers) absorbs only the terms
// of its own tenant. The result is Unrestricted only if every tenant present has such a term. Absorbing
// across tenants would over-grant (unrestricted on t1 must not widen a restricted grant on t2).
func Normalise(terms []Term, limits config.Limits) (Effective, error) {
	tenantSet := map[string]bool{}
	for _, t := range terms {
		if t.Tenant != "" {
			tenantSet[t.Tenant] = true
		}
	}
	eff := Effective{Tenants: sortedKeys(tenantSet)}

	cur := make([]Term, len(terms))
	for i, t := range terms {
		cur[i] = Term{Tenant: t.Tenant, Matchers: sortMatchers(t.Matchers)}
	}
	for {
		var changed bool
		cur = dedup(cur)
		cur, changed = subsume(cur)
		merged, mchanged := mergeSingleLabel(cur)
		cur = merged
		if !changed && !mchanged {
			break
		}
	}

	// Step 1 (per tenant): all tenants unrestricted -> Unrestricted.
	unrestricted := map[string]bool{}
	for _, t := range cur {
		if len(t.Matchers) == 0 {
			unrestricted[t.Tenant] = true
		}
	}
	tenantsInTerms := map[string]bool{}
	for _, t := range cur {
		tenantsInTerms[t.Tenant] = true
	}
	if len(cur) > 0 && len(unrestricted) == len(tenantsInTerms) {
		eff.Unrestricted = true
		eff.Disjoint = true
		return eff, nil
	}

	sort.Slice(cur, func(i, j int) bool { return cur[i].key() < cur[j].key() })
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

// dedup removes identical terms (step 2).
func dedup(in []Term) []Term {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, t := range in {
		// Duplicate matchers inside a term are redundant too.
		t.Matchers = uniqueMatchers(t.Matchers)
		k := t.key()
		if !seen[k] {
			seen[k] = true
			out = append(out, t)
		}
	}
	return out
}

func uniqueMatchers(ms []Matcher) []Matcher {
	out := ms[:0:0]
	for i, m := range ms {
		if i > 0 && m.key() == ms[i-1].key() {
			continue
		}
		out = append(out, m)
	}
	return out
}

// subsume drops term b if another term a of the same tenant has a strict subset of b's matchers (step 3).
func subsume(in []Term) ([]Term, bool) {
	keys := make([]map[string]bool, len(in))
	for i, t := range in {
		keys[i] = map[string]bool{}
		for _, m := range t.Matchers {
			keys[i][m.key()] = true
		}
	}
	var out []Term
	changed := false
	for j, b := range in {
		dropped := false
		for i, a := range in {
			if i == j || a.Tenant != b.Tenant || len(a.Matchers) >= len(b.Matchers) {
				continue
			}
			subset := true
			for k := range keys[i] {
				if !keys[j][k] {
					subset = false
					break
				}
			}
			if subset {
				dropped = true
				break
			}
		}
		if dropped {
			changed = true
		} else {
			out = append(out, b)
		}
	}
	return out, changed
}

func mergeable(op v1.MatchOp) bool { return op == v1.MatchEqual || op == v1.MatchRegexMatch }

func asRegex(m Matcher) string {
	if m.Op == v1.MatchEqual {
		return regexp.QuoteMeta(m.Value)
	}
	return m.Value
}

// mergeSingleLabel merges two same-tenant terms that differ only in one label, where both use Equal or
// RegexMatch, into a single RegexMatch alternation (step 4). It performs one merge per call.
func mergeSingleLabel(in []Term) ([]Term, bool) {
	for i := 0; i < len(in); i++ {
		for j := i + 1; j < len(in); j++ {
			a, b := in[i], in[j]
			if a.Tenant != b.Tenant || len(a.Matchers) != len(b.Matchers) || len(a.Matchers) == 0 {
				continue
			}
			for ai, ma := range a.Matchers {
				if !mergeable(ma.Op) || countName(a.Matchers, ma.Name) != 1 {
					continue
				}
				for bi, mb := range b.Matchers {
					if mb.Name != ma.Name || !mergeable(mb.Op) || countName(b.Matchers, mb.Name) != 1 {
						continue
					}
					if !sameExcept(a.Matchers, ai, b.Matchers, bi) {
						continue
					}
					merged, err := newMatcher(ma.Name, v1.MatchRegexMatch, "(?:"+asRegex(ma)+")|(?:"+asRegex(mb)+")")
					if err != nil {
						continue // cannot happen for compiled inputs; leave terms unmerged (still correct)
					}
					ms := make([]Matcher, 0, len(a.Matchers))
					for k, m := range a.Matchers {
						if k != ai {
							ms = append(ms, m)
						}
					}
					ms = sortMatchers(append(ms, merged))
					out := make([]Term, 0, len(in)-1)
					for k, t := range in {
						switch k {
						case i:
							out = append(out, Term{Tenant: a.Tenant, Matchers: ms})
						case j:
						default:
							out = append(out, t)
						}
					}
					return out, true
				}
			}
		}
	}
	return in, false
}

func countName(ms []Matcher, name string) int {
	n := 0
	for _, m := range ms {
		if m.Name == name {
			n++
		}
	}
	return n
}

func sameExcept(a []Matcher, ai int, b []Matcher, bi int) bool {
	set := map[string]int{}
	for k, m := range a {
		if k != ai {
			set[m.key()]++
		}
	}
	for k, m := range b {
		if k != bi {
			set[m.key()]--
		}
	}
	for _, v := range set {
		if v != 0 {
			return false
		}
	}
	return true
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
