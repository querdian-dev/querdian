package promql

import (
	"errors"
	"math/rand/v2"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
)

// FuzzOrLift generates random queries and 1-4 random terms and checks the or-lifting invariants: the
// output re-parses and every selector copy carries exactly its original matchers plus exactly one
// term (checkLifted), so the number of copies is original x terms.
func FuzzOrLift(f *testing.F) {
	for _, s := range []uint64{0, 1, 2, 3, 42, 1 << 40} {
		f.Add(s, uint8(3), uint8(2))
	}
	pool := [][]policy.Matcher{
		{{Name: "ns", Op: v1.MatchEqual, Value: "a"}},
		{{Name: "ns", Op: v1.MatchRegexMatch, Value: "b|c"}, {Name: "env", Op: v1.MatchNotEqual, Value: "dev"}},
		{{Name: "team", Op: v1.MatchRegexNoMatch, Value: "x.*"}},
		{{Name: "__name__", Op: v1.MatchRegexMatch, Value: "up|http_.*"}},
		{{Name: "ns", Op: v1.MatchEqual, Value: "d"}, {Name: policy.TenantIDLabel, Op: v1.MatchEqual, Value: "t1"}},
		{{Name: policy.TenantIDLabel, Op: v1.MatchEqual, Value: "t2"}},
	}
	f.Fuzz(func(t *testing.T, seed uint64, depth, nterms uint8) {
		g := gen{rand.New( //nolint:gosec // deterministic test generator, not security-relevant
			rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
		q := g.vector(int(depth % 5))
		var eff policy.Effective
		for i := 0; i < 1+int(nterms%4); i++ {
			eff.Terms = append(eff.Terms, policy.Term{Matchers: pool[g.r.IntN(len(pool))]})
		}
		p, err := plan(q, eff)
		if err != nil {
			if errors.Is(err, enforce.ErrParse) {
				t.Skipf("generator produced invalid query %q: %v", q, err)
			}
			t.Fatalf("%q: %v", q, err)
		}
		out := p.Requests[0].Query.Get("query")
		if len(eff.Terms) == 1 {
			checkOutput(t, out, eff.Terms[0].Matchers)
			return
		}
		checkLifted(t, q, out, eff)
	})
}
