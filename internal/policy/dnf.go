package policy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/maphash"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"unicode/utf8"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/regexcost"
)

// ErrTooComplex is returned when the normalised policy has more terms than limits.maxPolicyTerms.
// It maps to 403 with reason policy_too_complex.
var ErrTooComplex = errors.New("policy_too_complex")

var errWork = fmt.Errorf("%w: the simplification needs more than %d steps", ErrTooComplex, MaxNormaliseWork)

func checkResultWeight(ts []Term) error {
	if n := TermsWeight(ts); n > MaxResultWeight {
		return fmt.Errorf("%w: the matchers of the simplified policy have a weight of %d, more than the limit of %d",
			ErrTooComplex, n, MaxResultWeight)
	}
	return nil
}

// TermsWeight returns the sum of the weights of the matchers of all terms (see Matcher.Weight).
func TermsWeight(ts []Term) int {
	n := 0
	for _, t := range ts {
		for _, m := range t.Matchers {
			n += m.Weight()
		}
	}
	return n
}

// Weight is the cost of the matcher when a backend compiles it: for a regex, regexcost.Weight (the
// instructions of its compiled program, the rune ranges of its character classes and the cost of case
// folding); for Equal and NotEqual, the number of characters of the value plus one, which is also its size
// in a merged regex.
func (m Matcher) Weight() int {
	if m.w > 0 {
		return m.w
	}
	return weight(m.Op, m.Value)
}

func weight(op v1.MatchOp, value string) int {
	if op == v1.MatchRegexMatch || op == v1.MatchRegexNoMatch {
		if n, err := RegexWeight(value); err == nil {
			return n
		}
	}
	return utf8.RuneCountInString(value) + 1
}

// RegexWeight returns the weight (regexcost.Weight) of the anchored regex value, as newMatcher compiles it,
// without a compile. A result over MaxRegexWeight can be an estimate without a parse. An error means that
// value does not parse.
func RegexWeight(value string) (int, error) {
	return regexcost.Weight("^(?:"+value+")$", syntax.Perl, regexcost.CompileAndPrint, MaxRegexWeight)
}

// newMatcher builds a compiled matcher without name validation; the regex is anchored.
func newMatcher(name string, op v1.MatchOp, value string) (Matcher, error) {
	m := Matcher{Name: name, Op: op, Value: value, k: fmt.Sprintf("%q %s %q", name, op, value), w: weight(op, value)}
	if op == v1.MatchRegexMatch || op == v1.MatchRegexNoMatch {
		src := "^(?:" + value + ")$"
		if _, err := regexp.Compile(src); err != nil {
			return Matcher{}, err
		}
		// The snapshot keeps only the source; Matches compiles it again on first use. Thus a snapshot does
		// not keep the compiled programs of all policies.
		m.re = &lazyRegexp{src: src}
	}
	return m, nil
}

// newMergedMatcher builds a RegexMatch from regexes that are already known to be valid, without compiling.
// w is its weight.
func newMergedMatcher(name, value string, w int) Matcher {
	return Matcher{
		Name: name, Op: v1.MatchRegexMatch, Value: value,
		k:  fmt.Sprintf("%q %s %q", name, v1.MatchRegexMatch, value),
		re: &lazyRegexp{src: "^(?:" + value + ")$"},
		w:  w,
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

	w := work{left: MaxNormaliseWork}
	for {
		cur = dedupSorted(cur)
		var subsumed, merged bool
		cur, subsumed = subsume(cur, &w)
		if w.left < 0 {
			return Effective{}, errWork
		}
		cur, merged = mergeSingleLabel(cur)
		if w.spend(passCost(cur)) {
			return Effective{}, errWork
		}
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

	// The caps come before allDisjoint, which compares each pair of terms.
	if len(cur) > limits.MaxPolicyTerms {
		return Effective{}, fmt.Errorf("%w: %d terms exceed limit %d", ErrTooComplex, len(cur), limits.MaxPolicyTerms)
	}
	// A merge joins the values of many terms into one regex, and the enforcers compile each regex. This cap
	// bounds the regexes that one request compiles.
	if err := checkResultWeight(cur); err != nil {
		return Effective{}, err
	}
	eff.Terms = cur
	eff.Disjoint = allDisjoint(cur)
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
// It keeps the order of the other terms.
//
// Each matcher of such a term a is also a matcher of b. Thus subsume indexes each term by its tenant and its
// rarest matcher, and compares b only with the shorter terms in the index entries of the matchers of b. In
// the normal case, such as one term for each namespace, an entry has few terms. Each candidate costs steps
// of w. When w is used up, subsume stops and the caller rejects the policy.
func subsume(in []Term, w *work) ([]Term, bool) {
	type key struct{ tenant, k string }
	freq := map[key]int{}
	for _, t := range in {
		for _, m := range t.Matchers {
			freq[key{t.Tenant, m.k}]++
		}
	}
	index := map[key][]int{}   // indexes into in, by tenant and rarest matcher
	empty := map[string]bool{} // tenants with a term without matchers
	for i, t := range in {
		if len(t.Matchers) == 0 {
			empty[t.Tenant] = true
			continue
		}
		rarest := key{t.Tenant, t.Matchers[0].k}
		for _, m := range t.Matchers[1:] {
			if k := (key{t.Tenant, m.k}); freq[k] < freq[rarest] {
				rarest = k
			}
		}
		index[rarest] = append(index[rarest], i)
	}
	for _, idx := range index {
		sort.SliceStable(idx, func(x, y int) bool { return len(in[idx[x]].Matchers) < len(in[idx[y]].Matchers) })
	}
	out := make([]Term, 0, len(in))
	for _, b := range in {
		// A term without matchers is a strict subset of each longer term of its tenant.
		dropped := len(b.Matchers) > 0 && empty[b.Tenant]
		for _, m := range b.Matchers {
			if dropped {
				break
			}
			for _, i := range index[key{b.Tenant, m.k}] {
				a := in[i]
				if len(a.Matchers) >= len(b.Matchers) {
					break // the entry is sorted by length
				}
				if w.spend(1 + len(b.Matchers)) {
					return in, false
				}
				if isSubset(a.Matchers, b.Matchers) {
					dropped = true
					break
				}
			}
		}
		if !dropped {
			out = append(out, b)
		}
	}
	return out, len(out) != len(in)
}

// work is the budget of steps of one call of Normalise.
type work struct{ left int }

// spend takes n steps from the budget and reports whether the budget is used up.
func (w *work) spend(n int) bool {
	w.left -= n
	return w.left < 0
}

// passCost is the cost of one pass of Normalise over ts: dedupSorted copies the keys of all matchers, and
// the other steps visit each term and matcher.
func passCost(ts []Term) int {
	n := len(ts)
	for _, t := range ts {
		for _, m := range t.Matchers {
			n += 1 + len(m.k)
		}
	}
	return n
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
//
// The work is linear in the number of matchers for a fixed term size: a signature is a hash, not a copy of
// the other matchers, and a term joins a group only after an exact comparison with the first member.
func mergeSingleLabel(in []Term) ([]Term, bool) {
	type member struct{ term, pos int }
	seed := maphash.MakeSeed()
	var groups [][]member         // in the order of the first member
	buckets := map[uint64][]int{} // signature hash -> indexes into groups
	names := map[string]int{}     // matchers per label name of the current term
	var hs []uint64               // hash per matcher of the current term
	for ti, t := range in {
		clear(names)
		hs = hs[:0]
		var sum uint64
		for _, m := range t.Matchers {
			names[m.Name]++
			h := maphash.String(seed, m.k)
			hs = append(hs, h)
			sum += h
		}
		for pos, m := range t.Matchers {
			if !mergeable(m.Op) || names[m.Name] > 1 {
				continue
			}
			// Equal sets of other matchers have the same sum of hashes. The matchers of a term are sorted and
			// unique, so equal sets are also equal sequences. sameSignature compares them exactly.
			h := signatureHash(seed, t.Tenant, m.Name, sum-hs[pos])
			if constantSignatureHash {
				h = 0 // test hook: all signatures collide
			}
			gi := -1
			for _, g := range buckets[h] {
				first := groups[g][0]
				if sameSignature(in[first.term], first.pos, t, pos) {
					gi = g
					break
				}
			}
			if gi < 0 {
				gi = len(groups)
				groups = append(groups, nil)
				buckets[h] = append(buckets[h], gi)
			}
			groups[gi] = append(groups[gi], member{ti, pos})
		}
	}

	consumed := make([]bool, len(in))
	var created []Term
	for _, g := range groups {
		var ms []member
		for _, m := range g {
			if !consumed[m.term] {
				ms = append(ms, m)
			}
		}
		if len(ms) < 2 {
			continue
		}
		alts := make([]string, len(ms))
		// The program of an alternation is the programs of its parts and one instruction for each part, and
		// the anchors and the fixed instructions of a program (see regexcost.Weight).
		w := 6
		for i, m := range ms {
			alts[i] = "(?:" + asRegex(in[m.term].Matchers[m.pos]) + ")"
			w += in[m.term].Matchers[m.pos].Weight() + 1
		}
		first := in[ms[0].term]
		merged := newMergedMatcher(first.Matchers[ms[0].pos].Name, strings.Join(alts, "|"), w)
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

// constantSignatureHash makes all signature hashes equal. Only tests set it, so that sameSignature must
// separate all groups.
var constantSignatureHash = false

func signatureHash(seed maphash.Seed, tenant, name string, others uint64) uint64 {
	var h maphash.Hash
	h.SetSeed(seed)
	_, _ = h.WriteString(tenant)
	_ = h.WriteByte(0)
	_, _ = h.WriteString(name)
	_ = h.WriteByte(0)
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], others)
	_, _ = h.Write(b[:])
	return h.Sum64()
}

// sameSignature reports whether term a without its matcher at apos and term b without its matcher at bpos
// have the same tenant, the same other matchers, and the same label name at the left-out position.
func sameSignature(a Term, apos int, b Term, bpos int) bool {
	if a.Tenant != b.Tenant || len(a.Matchers) != len(b.Matchers) || a.Matchers[apos].Name != b.Matchers[bpos].Name {
		return false
	}
	i, j := 0, 0
	for {
		if i == apos {
			i++
		}
		if j == bpos {
			j++
		}
		if i == len(a.Matchers) || j == len(b.Matchers) {
			return i == len(a.Matchers) && j == len(b.Matchers)
		}
		if a.Matchers[i].k != b.Matchers[j].k {
			return false
		}
		i++
		j++
	}
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
