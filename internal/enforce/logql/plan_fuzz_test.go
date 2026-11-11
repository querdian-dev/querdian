package logql

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"runtime"
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/regexcost"
	lq "github.com/querdian-dev/querdian/pkg/logql"
)

// lgen builds random LogQL from a deterministic source.
type lgen struct{ r *rand.Rand }

func (g lgen) pick(s ...string) string { return s[g.r.IntN(len(s))] }

func (g lgen) selector() string {
	ms := []string{g.pick(`app="a"`, `app=~"x.+"`, `job="j"`)}
	for i := g.r.IntN(3); i > 0; i-- {
		ms = append(ms, g.pick(`env!="dev"`, `ns="a"`, `ns=~"b|c"`, `team!~"x.*"`, `l=~"a{10}"`))
	}
	return "{" + strings.Join(ms, ", ") + "}" + g.pick("", ` |= "err"`, ` | logfmt`, ` != "x" | json`)
}

func (g lgen) metric(d int) string {
	if d <= 0 || g.r.IntN(3) == 0 {
		return fmt.Sprintf("%s(%s [5m])", g.pick("count_over_time", "rate", "bytes_rate"), g.selector())
	}
	switch g.r.IntN(3) {
	case 0:
		return fmt.Sprintf("sum(%s)", g.metric(d-1))
	case 1:
		return fmt.Sprintf("%s %s %s", g.metric(d-1), g.pick("or", "+", "and"), g.metric(d-1))
	}
	return fmt.Sprintf("max by (app) (%s)", g.metric(d-1))
}

func (g lgen) query(d int) string {
	if g.r.IntN(2) == 0 {
		return g.selector()
	}
	return g.metric(d)
}

var fuzzPool = []policy.Term{
	term(m("ns", v1.MatchEqual, "a")),
	term(m("ns", v1.MatchRegexMatch, "b|c"), m("env", v1.MatchNotEqual, "dev")),
	term(m("team", v1.MatchRegexNoMatch, "x.*"), m("ns", v1.MatchEqual, "d")),
	term(m("ns", v1.MatchRegexMatch, "(?:e{100})|f")),
	term(m("ns", v1.MatchEqual, "g"), m(policy.TenantIDLabel, v1.MatchEqual, "t1")),
}

// selectorPolicyWeight returns the sum of the weights of the matchers in the stream selectors of q that are
// equal to a matcher of a term of eff.
func selectorPolicyWeight(t *testing.T, q *lq.Query, eff policy.Effective) int {
	t.Helper()
	weights := map[string]int{}
	ops := map[v1.MatchOp]string{v1.MatchEqual: "=", v1.MatchNotEqual: "!=", v1.MatchRegexMatch: "=~", v1.MatchRegexNoMatch: "!~"}
	for _, tm := range eff.Terms {
		for _, pm := range tm.Matchers {
			weights[pm.Name+ops[pm.Op]+pm.Value] = pm.Weight()
		}
	}
	n := 0
	for _, s := range q.Selectors {
		for _, sm := range s.Matchers {
			n += weights[sm.Name+sm.Op+sm.Value]
		}
	}
	return n
}

// FuzzPlan runs Plan on the query, series and labels endpoints (Plan, planSeries, optionalQueryBranches)
// with random queries and policies. Each output query must parse, and the rewrite must add at most
// (selectors) x (weight of all terms) of policy matchers, the property of enforce.CheckInjection.
func FuzzPlan(f *testing.F) {
	for _, s := range []uint64{0, 1, 2, 7, 42, 1 << 40} {
		f.Add(s, uint8(2), uint8(1))
	}
	f.Fuzz(func(t *testing.T, seed uint64, depth, nterms uint8) {
		g := lgen{rand.New( //nolint:gosec // deterministic test generator, not security-relevant
			rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
		var eff policy.Effective
		for i := 0; i < 1+int(nterms%4); i++ {
			eff.Terms = append(eff.Terms, fuzzPool[g.r.IntN(len(fuzzPool))])
		}
		eff.Tenants = []string{"t1", "t2"}
		q := g.query(int(depth % 4))
		sel := g.selector()
		cases := []struct {
			name, param string
			plan        func() (enforce.Plan, error)
			inputs      []string
		}{
			{"query", "query", func() (enforce.Plan, error) { return plan(t, q, eff) }, []string{q}},
			{"series", "match[]", func() (enforce.Plan, error) {
				return planFor(t, epFor(tmplSeries, "match[]", ""), url.Values{"match[]": {sel, sel}}, eff)
			}, []string{sel, sel}},
			{"labels", "query", func() (enforce.Plan, error) {
				return planFor(t, epFor(tmplLabels, "query", ""), url.Values{"query": {sel}}, eff)
			}, []string{sel}},
		}
		for _, c := range cases {
			var a, b runtime.MemStats
			runtime.ReadMemStats(&a)
			p, err := c.plan()
			runtime.ReadMemStats(&b)
			if err != nil {
				if errors.Is(err, enforce.ErrParse) {
					t.Fatalf("%s: generator produced a query that does not parse: %v", c.name, err)
				}
				continue // unsupported shapes and limits fail closed
			}
			selectors, userWeight, clientWeight, length := 0, 0, 0, 0
			for _, in := range c.inputs {
				iq, err := lq.Parse(in)
				if err != nil {
					t.Fatal(err)
				}
				selectors += len(iq.Selectors)
				userWeight += selectorPolicyWeight(t, iq, eff)
				clientWeight += iq.RegexWeight
				length += len(in)
			}
			// The cost of the plan: regexcost.MaxBytesPerUnit for each unit of the request weight, and a part
			// for the length of the query and for each term.
			bound := 1<<20 + regexcost.MaxBytesPerUnit*enforce.RequestWeight(selectors, clientWeight, eff) + 4096*length*len(eff.Terms)
			if got := b.TotalAlloc - a.TotalAlloc; got > uint64(bound) { //nolint:gosec // bound is positive
				t.Fatalf("%s: the plan allocated %d bytes, more than %d: %v", c.name, got, bound, c.inputs)
			}
			added := 0
			for _, r := range p.Requests {
				for _, out := range r.Query[c.param] {
					oq, err := lq.Parse(out)
					if err != nil {
						t.Fatalf("%s: output %q does not parse: %v", c.name, out, err)
					}
					added += selectorPolicyWeight(t, oq, eff)
				}
			}
			// Each term gets its own copy of the client matchers (fan-out, or-lift, match[] expansion).
			added -= len(eff.Terms) * userWeight
			if bound := max(selectors, 1) * policy.TermsWeight(eff.Terms); added > bound {
				t.Fatalf("%s: the rewrite adds a policy weight of %d, more than %d: %v", c.name, added, bound, c.inputs)
			}
		}
	})
}
