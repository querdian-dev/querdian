package promql

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
)

// gen builds random PromQL over all expression node types from a deterministic source.
type gen struct{ r *rand.Rand }

func (g gen) pick(s ...string) string { return s[g.r.IntN(len(s))] }

func (g gen) modifiers() string {
	var b strings.Builder
	switch g.r.IntN(4) {
	case 1:
		b.WriteString(" @ 1609746000")
	case 2:
		b.WriteString(" @ start()")
	case 3:
		b.WriteString(" @ end()")
	}
	if g.r.IntN(3) == 0 {
		b.WriteString(g.pick(" offset 5m", " offset -1m", " offset 1h"))
	}
	return b.String()
}

func (g gen) selector() string {
	name := g.pick("up", "http_requests_total", "node_cpu", "")
	var ms []string
	for i := g.r.IntN(3); i > 0; i-- {
		ms = append(ms, g.pick(`job="x"`, `job=~"a.*"`, `env!="dev"`, `inst!~"b.+"`, `namespace="other"`))
	}
	if name == "" {
		ms = append(ms, `job="x"`)
	}
	s := name
	if len(ms) > 0 {
		s += "{" + strings.Join(ms, ",") + "}"
	}
	return s
}

// vector returns an instant-vector expression.
func (g gen) vector(d int) string {
	if d <= 0 {
		return g.selector() + g.modifiers()
	}
	switch g.r.IntN(9) {
	case 0:
		return g.selector() + g.modifiers()
	case 1:
		return fmt.Sprintf("%s(%s)", g.pick("rate", "irate", "increase", "avg_over_time", "max_over_time", "absent_over_time", "deriv"), g.matrix(d-1))
	case 2:
		return fmt.Sprintf("%s %s (%s)", g.pick("sum", "max", "count", "avg", "group"), g.pick("by (job)", "without (instance)", "by (a, b)", ""), g.vector(d-1))
	case 3:
		return fmt.Sprintf("(%s)", g.vector(d-1))
	case 4:
		return "-" + g.vector(d-1)
	case 5:
		vm := g.pick("", " on(job)", " ignoring(inst)", " on(job) group_left(team)", " on() group_right", " ignoring(x) group_left")
		op := g.pick("+", "-", "*", "/", "and", "or", "unless", ">", "== bool")
		if strings.Contains(vm, "group") && (op == "and" || op == "or" || op == "unless") {
			vm = " on(job)"
		}
		return fmt.Sprintf("%s %s%s %s", g.vector(d-1), op, vm, g.vector(d-1))
	case 6:
		return fmt.Sprintf("%s * %s", g.vector(d-1), g.pick("2", "0.5", "scalar("+g.vector(d-1)+")", "time()"))
	case 7:
		return fmt.Sprintf(`label_replace(%s, "dst", "$1", "src", "(.*)")`, g.vector(d-1))
	default:
		return fmt.Sprintf("%s(%s)", g.pick("abs", "ceil", "sort", "timestamp"), g.vector(d-1))
	}
}

// matrix returns a range-vector expression.
func (g gen) matrix(d int) string {
	if d <= 0 || g.r.IntN(2) == 0 {
		return g.selector() + "[5m]" + g.modifiers()
	}
	return fmt.Sprintf("%s[10m:%s]%s", g.vector(d-1), g.pick("", "1m", "30s"), g.modifiers())
}

// FuzzInject generates queries over all node types and checks the re-parse and selector-coverage
// invariants of the rewritten query.
func FuzzInject(f *testing.F) {
	for _, s := range []uint64{0, 1, 2, 3, 42, 1 << 40} {
		f.Add(s, uint8(3))
	}
	ms := []policy.Matcher{
		{Name: "ns", Op: v1.MatchEqual, Value: "a"},
		{Name: "env", Op: v1.MatchRegexNoMatch, Value: "x.*"},
		{Name: policy.TenantIDLabel, Op: v1.MatchEqual, Value: "t"},
	}
	f.Fuzz(func(t *testing.T, seed uint64, depth uint8) {
		g := gen{rand.New( //nolint:gosec // deterministic test generator, not security-relevant
			rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
		q := g.vector(int(depth % 5))
		p, err := plan(q, effWith(ms...))
		if err != nil {
			if errors.Is(err, enforce.ErrParse) {
				t.Skipf("generator produced invalid query %q: %v", q, err)
			}
			t.Fatalf("%q: %v", q, err)
		}
		out := p.Requests[0].Query.Get("query")
		checkOutput(t, out, ms)
		if countSelectors(t, out) != countSelectors(t, q) {
			t.Fatalf("selector count changed: %q -> %q", q, out)
		}
	})
}
