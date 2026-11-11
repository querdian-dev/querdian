package enforce_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp/syntax"
	"runtime"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/enforce/logql"
	"github.com/querdian-dev/querdian/internal/enforce/promql"
	"github.com/querdian-dev/querdian/internal/enforce/traceql"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/regexcost"
	"github.com/querdian-dev/querdian/internal/routes"
)

func TestCheckRequest(t *testing.T) {
	// An Equal value of n characters has the weight n+1.
	eff := func(n int) policy.Effective {
		return policy.Effective{Terms: []policy.Term{{Matchers: []policy.Matcher{{Name: "a", Op: v1.MatchEqual, Value: strings.Repeat("v", n)}}}}}
	}
	// 4 selectors: 9 x the policy weight.
	if err := enforce.CheckRequest(4, 0, eff(enforce.MaxRequestWeight/9-1)); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	if err := enforce.CheckRequest(4, 0, eff(enforce.MaxRequestWeight/9)); !errors.Is(err, enforce.ErrTooComplex) {
		t.Errorf("over the limit: %v", err)
	}
	if err := enforce.CheckRequest(0, 0, eff(enforce.MaxRequestWeight/3-1)); err != nil {
		t.Errorf("no selectors count as one: %v", err)
	}
	if err := enforce.CheckRequest(1<<20, 1<<20, policy.Effective{Unrestricted: true}); err != nil {
		t.Errorf("unrestricted: %v", err)
	}
	// A regex counts with its weight, not with its length.
	re := policy.Effective{Terms: []policy.Term{{Matchers: []policy.Matcher{{Name: "a", Op: v1.MatchRegexMatch, Value: strings.Repeat("(?:a{1000})", 16)}}}}}
	if err := enforce.CheckRequest(4, 0, re); !errors.Is(err, enforce.ErrTooComplex) {
		t.Errorf("regex weight: %v", err)
	}
	// The client regexes count again for each term.
	terms := func(k int) policy.Effective {
		var e policy.Effective
		for i := range k {
			e.Terms = append(e.Terms, policy.Term{Matchers: []policy.Matcher{{Name: "ns", Op: v1.MatchEqual, Value: fmt.Sprint("t", i)}}})
		}
		return e
	}
	if err := enforce.CheckRequest(1, enforce.MaxQueryRegexWeight, terms(1)); err != nil {
		t.Errorf("client regexes with 1 term: %v", err)
	}
	if err := enforce.CheckRequest(1, enforce.MaxQueryRegexWeight, terms(16)); !errors.Is(err, enforce.ErrTooComplex) {
		t.Errorf("client regexes with 16 terms: %v", err)
	}
}

// policyShape gives the matcher of rule i of a test policy, for a backend whose namespace label is name.
type policyShape struct {
	name  string
	count int // number of rules for each backend
	rule  func(name string, i int) v1.Matcher
}

// The shapes are the largest merged policies of their kind within policy.MaxResultWeight. A merge adds 1
// for each value and 6 in total.
var (
	// literalShape: Equal values of 220 characters (weight 221), each with its own prefix, which is an
	// expensive shape for the regex compiler after the merge.
	literalShape = policyShape{"literal", (policy.MaxResultWeight - 6) / 222, func(name string, i int) v1.Matcher {
		id := fmt.Sprint(i, "-")
		return v1.Matcher{Name: name, Op: v1.MatchEqual, Value: id + strings.Repeat("n", 220-len(id))}
	}}
	// repeatShape: regexes near policy.MaxRegexWeight whose program is much larger than their text.
	repeatShape = policyShape{"repeat", 2, func(name string, i int) v1.Matcher {
		return v1.Matcher{Name: name, Op: v1.MatchRegexMatch, Value: fmt.Sprint("x", i) + strings.Repeat("(?:a{1000})", 16)}
	}}
	// classShape: regexes near policy.MaxRegexWeight with large character classes (\pL has about 700
	// rune ranges and about 70000 runes in the fold window), repeated.
	classShape = policyShape{"class", 2, func(name string, i int) v1.Matcher {
		return v1.Matcher{Name: name, Op: v1.MatchRegexMatch, Value: fmt.Sprint("x", i) + `\pL{20}`}
	}}
	// foldShape: case-folded classes over wide ranges.
	foldShape = policyShape{"fold", 5, func(name string, i int) v1.Matcher {
		return v1.Matcher{Name: name, Op: v1.MatchRegexMatch, Value: fmt.Sprintf(`(?i:x%d[\x{41}-\x{1E942}])`, i)}
	}}
	shapes = []policyShape{literalShape, repeatShape, classShape, foldShape}
)

// fullPathSnapshot returns a snapshot in which user u has, on each backend, count+extra rules of the shape.
// Each rule is in a policy of its own, so that no policy is over a limit of its own.
func fullPathSnapshot(t testing.TB, shape policyShape, extra int) *policy.Snapshot {
	t.Helper()
	cfg, err := config.Load("../config/testdata/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cps []v1.ClusterAccessPolicy
	add := func(r v1.ClusterRule) {
		cps = append(cps, v1.ClusterAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprint("p", len(cps))},
			Spec: v1.ClusterAccessPolicySpec{Subjects: v1.Subjects{Users: []string{"u"}}, Rules: []v1.ClusterRule{r}}})
	}
	for i := 0; i < shape.count+extra; i++ {
		add(v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{shape.rule("namespace", i)}})
		add(v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t"}, Matchers: []v1.Matcher{shape.rule("k8s_namespace_name", i)}})
		add(v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"t"}, Matchers: []v1.Matcher{shape.rule("resource.ns", i)}})
	}
	return policy.NewSnapshot(policy.Compile(policy.Input{Config: cfg, ClusterPolicies: cps}), cfg, 1)
}

// fullPathRequest is one request through ResolveAt and an enforcer.
type fullPathRequest struct {
	name    string
	backend string
	plan    func(eff policy.Effective) (enforce.Plan, error)
}

// selectors returns first and n-1 copies of rest, joined with sep.
func selectors(first, rest, sep string, n int) string {
	return first + strings.Repeat(sep+rest, n-1)
}

// clientRegexes returns k matchers l0=~"a{1000}", ... for a client query. Each has the weight clientUnit.
func clientRegexes(k int) string {
	var sb strings.Builder
	for i := range k {
		fmt.Fprintf(&sb, `, l%d=~"a{1000}"`, i)
	}
	return sb.String()
}

// fullPathRequests returns requests whose queries have n selectors (or spanset filters) each. The first
// selector also has k client regexes of weight clientUnit (none in TraceQL, where the proxy compiles no
// client regex).
func fullPathRequests(n, k int) []fullPathRequest {
	ctx := context.Background()
	prom := func(path string, params url.Values) func(policy.Effective) (enforce.Plan, error) {
		return func(eff policy.Effective) (enforce.Plan, error) {
			return promql.New().Plan(ctx, routes.Lookup(config.BackendPrometheus, path, "GET"), params, eff)
		}
	}
	loki := func(path string, params url.Values) func(policy.Effective) (enforce.Plan, error) {
		return func(eff policy.Effective) (enforce.Plan, error) {
			return logql.New().Plan(ctx, routes.Lookup(config.BackendLoki, path, "GET"), params, eff)
		}
	}
	tempo := func(path string, params url.Values) func(policy.Effective) (enforce.Plan, error) {
		return func(eff policy.Effective) (enforce.Plan, error) {
			e := traceql.NewWithOptions(traceql.Options{TraceByID: config.TraceByIDFilter})
			return e.Plan(ctx, routes.Lookup(config.BackendTempo, path, "GET"), params, eff)
		}
	}
	cr := clientRegexes(k)
	return []fullPathRequest{
		{"promql query", "metrics", prom("/api/v1/query", url.Values{"query": {
			selectors(`up{job="a"`+cr+`}`, `up{job="a"}`, " + ", n)}})},
		{"promql series", "metrics", prom("/api/v1/series", url.Values{"match[]": strings.Split(
			selectors(`up{job="a"`+cr+`}`, `up`, ";", n), ";")})},
		{"logql query", "logs", loki("/loki/api/v1/query_range", url.Values{"query": {
			"sum(" + selectors(`count_over_time({app="a"`+cr+`}[5m])`, `count_over_time({app="a"}[5m])`, ") + sum(", n) + ")"},
			"limit": {"10"}, "direction": {"backward"}})},
		{"logql series", "logs", loki("/loki/api/v1/series", url.Values{"match[]": strings.Split(
			selectors(`{app="a"`+cr+`}`, `{app="a"}`, ";", n), ";")})},
		{"traceql search", "traces", tempo("/api/search", url.Values{"q": {selectors(`{ span.a = "b" }`, `{ span.a = "b" }`, " && ", n)}})},
		{"traceql trace by id", "traces", tempo("/api/traces/0123456789abcdef0123456789abcdef", url.Values{})},
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

// maxRequestBytes is the bound of the allocated bytes of one request in these tests:
// regexcost.MaxBytesPerUnit for each unit of enforce.MaxRequestWeight and some room for the rest.
const maxRequestBytes = regexcost.MaxBytesPerUnit*enforce.MaxRequestWeight + 64<<20

// run runs one request and checks the bounds of time and memory. It returns the error of the request.
func run(t *testing.T, s *policy.Snapshot, rq fullPathRequest) error {
	t.Helper()
	var err error
	start := time.Now()
	alloc := allocBytes(func() {
		var eff policy.Effective
		if eff, err = s.Resolve("u", nil, rq.backend); err == nil {
			_, err = rq.plan(eff)
		}
	})
	if d := time.Since(start); d > timeScale*5*time.Second || alloc > maxRequestBytes {
		t.Errorf("%s took %v and %d MiB", rq.name, d, alloc>>20)
	}
	return err
}

// clientUnit is the weight of one client regex of clientRegexes: a{1000}, its anchors, the parser work and
// the fixed
// instructions of a program.
const clientUnit = 1007

// TestFullPathBounded runs ResolveAt and each enforcer for the largest inputs that pass all limits: a merged
// policy just below policy.MaxResultWeight of each shape, in a query with client regexes and selectors up
// to enforce.MaxRequestWeight. Time and memory must stay below fixed bounds. One selector more, one client
// regex more, or one rule more must give ErrTooComplex before an enforcer compiles the regex.
func TestFullPathBounded(t *testing.T) {
	if w, _ := regexcost.Weight("a{1000}", syntax.Perl|syntax.DotNL, regexcost.CompileAndPrint, 1<<30); w+2 != clientUnit {
		t.Fatalf("clientUnit is %d, want %d", clientUnit, w+2)
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			s := fullPathSnapshot(t, shape, 0)
			for _, r := range s.Results() {
				if !r.Ready {
					t.Fatalf("%s: %s %s", r.Name, r.Reason, r.Message)
				}
			}
			eff, err := s.Resolve("u", nil, "metrics")
			if err != nil {
				t.Fatal(err)
			}
			w := policy.TermsWeight(eff.Terms)
			if w > policy.MaxResultWeight || w < policy.MaxResultWeight*3/4 {
				t.Fatalf("the policy has a weight of %d, want just below %d", w, policy.MaxResultWeight)
			}
			// One selector and the most client regexes; then no client regex and the most selectors.
			k := min(enforce.MaxQueryRegexWeight, (enforce.MaxRequestWeight-3*w)/3) / clientUnit
			most := (enforce.MaxRequestWeight/w - 1) / 2
			if k < 1 || most < 1 {
				t.Fatalf("k=%d, most=%d", k, most)
			}
			for _, rq := range append(fullPathRequests(1, k), fullPathRequests(most, 0)...) {
				if err := run(t, s, rq); err != nil {
					t.Errorf("%s: %v", rq.name, err)
				}
			}
			for _, rq := range fullPathRequests(most+1, 0) {
				if rq.name == "traceql trace by id" {
					continue // no query
				}
				if err := run(t, s, rq); !errors.Is(err, enforce.ErrTooComplex) {
					t.Errorf("%s with one selector more: want ErrTooComplex, got %v", rq.name, err)
				}
			}
			for _, rq := range fullPathRequests(1, k+1) {
				if strings.HasPrefix(rq.name, "traceql") {
					continue // no client regex
				}
				if err := run(t, s, rq); !errors.Is(err, enforce.ErrTooComplex) {
					t.Errorf("%s with one client regex more: want ErrTooComplex, got %v", rq.name, err)
				}
			}
			over := fullPathSnapshot(t, shape, 1)
			for _, be := range []string{"metrics", "logs", "traces"} {
				if _, err := over.Resolve("u", nil, be); !errors.Is(err, policy.ErrTooComplex) {
					t.Errorf("%s with one rule more: want policy.ErrTooComplex, got %v", be, err)
				}
			}
		})
	}
}

// TestRegexProgramSizeProbe: a regex with a small text and a large program (a counted repetition, repeated)
// makes its policy PolicyTooLarge, and a client query with such a regex gets ErrTooComplex; both without a
// compile of the regex, so with bounded time and memory.
func TestRegexProgramSizeProbe(t *testing.T) {
	val := strings.Repeat("(?:a{1000})", 1400) // 15400 bytes, 1.4 million instructions
	cfg, err := config.Load("../config/testdata/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cps := []v1.ClusterAccessPolicy{{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: v1.ClusterAccessPolicySpec{
		Subjects: v1.Subjects{Users: []string{"u"}},
		Rules: []v1.ClusterRule{
			{Backends: []string{"metrics"}, Matchers: []v1.Matcher{{Name: "namespace", Op: v1.MatchRegexMatch, Value: val}}},
			{Backends: []string{"logs"}, Tenants: []string{"t"}, Matchers: []v1.Matcher{{Name: "namespace", Op: v1.MatchRegexMatch, Value: val}}},
			{Backends: []string{"traces"}, Tenants: []string{"t"}, Matchers: []v1.Matcher{{Name: "resource.ns", Op: v1.MatchRegexMatch, Value: val}}},
		}}}}
	var s *policy.Snapshot
	alloc := allocBytes(func() {
		s = policy.NewSnapshot(policy.Compile(policy.Input{Config: cfg, ClusterPolicies: cps}), cfg, 1)
	})
	if r := s.Results()[0]; r.Reason != policy.ReasonPolicyTooLarge || alloc > 64<<20 {
		t.Fatalf("compile: %s %s, %d MiB", r.Reason, r.Message, alloc>>20)
	}
	for _, rq := range fullPathRequests(8, 0) {
		if err := run(t, s, rq); !errors.Is(err, policy.ErrNoGrant) {
			t.Errorf("%s: want no grant, got %v", rq.name, err)
		}
	}

	// The same regex in a client query, a regex with many large classes, and a client regex within its own
	// limit under a policy with many terms.
	one := policy.Effective{Terms: []policy.Term{{Matchers: []policy.Matcher{{Name: "namespace", Op: v1.MatchEqual, Value: "a"}}}}}
	var many policy.Effective
	for i := range 16 {
		many.Terms = append(many.Terms, policy.Term{Matchers: []policy.Matcher{{Name: "namespace", Op: v1.MatchEqual, Value: fmt.Sprint("t", i)}}})
	}
	for _, c := range []struct {
		name, regex string
		eff         policy.Effective
	}{
		{"repetition", val, one},
		{"classes", strings.Repeat(`\\pL`, 8000), one},
		{"classes in brackets", strings.Repeat(`[\\pL\\pN\\pP\\pS\\pM]`, 1000), one},
		{"16 terms", strings.Repeat("(?:a{1000})", 15), many},
	} {
		t.Run(c.name, func(t *testing.T) { checkClientQueries(t, `{app="a", b=~"`+c.regex+`"}`, c.eff) })
	}
}

// checkClientQueries runs the query q on the PromQL and LogQL endpoints and expects ErrTooComplex within
// small bounds of time and memory.
func checkClientQueries(t *testing.T, q string, eff policy.Effective) {
	t.Helper()
	ctx := context.Background()
	for name, plan := range map[string]func() (enforce.Plan, error){
		"promql query": func() (enforce.Plan, error) {
			return promql.New().Plan(ctx, routes.Lookup(config.BackendPrometheus, "/api/v1/query", "GET"), url.Values{"query": {"up" + q}}, eff)
		},
		"promql series": func() (enforce.Plan, error) {
			return promql.New().Plan(ctx, routes.Lookup(config.BackendPrometheus, "/api/v1/series", "GET"), url.Values{"match[]": {"up" + q}}, eff)
		},
		"logql query": func() (enforce.Plan, error) {
			return logql.New().Plan(ctx, routes.Lookup(config.BackendLoki, "/loki/api/v1/query_range", "GET"), url.Values{"query": {q}}, eff)
		},
		"logql series": func() (enforce.Plan, error) {
			return logql.New().Plan(ctx, routes.Lookup(config.BackendLoki, "/loki/api/v1/series", "GET"), url.Values{"match[]": {q}}, eff)
		},
		"logql labels": func() (enforce.Plan, error) {
			return logql.New().Plan(ctx, routes.Lookup(config.BackendLoki, "/loki/api/v1/labels", "GET"), url.Values{"query": {q}}, eff)
		},
	} {
		var err error
		start := time.Now()
		alloc := allocBytes(func() { _, err = plan() })
		if !errors.Is(err, enforce.ErrTooComplex) {
			t.Errorf("%s: want ErrTooComplex, got %v", name, err)
		}
		// The rejection can cost at most the parse work of the query limit.
		if d := time.Since(start); d > timeScale*time.Second || alloc > regexcost.MaxBytesPerUnit*enforce.MaxQueryRegexWeight {
			t.Errorf("%s took %v and %d MiB", name, d, alloc>>20)
		}
	}
}

func BenchmarkFullPath(b *testing.B) {
	for _, shape := range shapes {
		s := fullPathSnapshot(b, shape, 0)
		eff, err := s.Resolve("u", nil, "metrics")
		if err != nil {
			b.Fatal(err)
		}
		w := policy.TermsWeight(eff.Terms)
		k := min(enforce.MaxQueryRegexWeight, (enforce.MaxRequestWeight-3*w)/3) / clientUnit
		most := (enforce.MaxRequestWeight/w - 1) / 2
		for _, c := range []struct {
			name         string
			selectors, k int
		}{{"client", 1, k}, {"selectors", most, 0}} {
			for _, rq := range fullPathRequests(c.selectors, c.k) {
				b.Run(shape.name+"/"+c.name+"/"+rq.name, func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						eff, err := s.Resolve("u", nil, rq.backend)
						if err != nil {
							b.Fatal(err)
						}
						if _, err := rq.plan(eff); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

// TestConfiguredRegexLimits: the limits of the static config, which ResolveAt puts into the effective policy,
// apply in each enforcer.
func TestConfiguredRegexLimits(t *testing.T) {
	ctx := context.Background()
	q := `{app="a", b=~"[^/]+"}` // a weight of about 2000
	term := []policy.Term{{Matchers: []policy.Matcher{{Name: "namespace", Op: v1.MatchEqual, Value: "a"}}}}
	plans := map[string]func(policy.Effective) error{
		"promql": func(eff policy.Effective) error {
			_, err := promql.New().Plan(ctx, routes.Lookup(config.BackendPrometheus, "/api/v1/query", "GET"), url.Values{"query": {"up" + q}}, eff)
			return err
		},
		"logql": func(eff policy.Effective) error {
			_, err := logql.New().Plan(ctx, routes.Lookup(config.BackendLoki, "/loki/api/v1/query_range", "GET"), url.Values{"query": {q}}, eff)
			return err
		},
	}
	for name, plan := range plans {
		for _, tc := range []struct {
			limits policy.RegexLimits
			ok     bool
		}{
			{policy.RegexLimits{}, true},                      // the defaults
			{policy.RegexLimits{Query: 100}, name == "logql"}, // LogQL does not count the class runes
			{policy.RegexLimits{Query: 4000}, true},           //
			{policy.RegexLimits{Request: 10}, false},          // the request limit counts the policy too
			{policy.RegexLimits{Query: 4000, Request: 1 << 20}, true},
		} {
			err := plan(policy.Effective{Terms: term, RegexLimits: tc.limits})
			if tc.ok != (err == nil) || (err != nil && !errors.Is(err, enforce.ErrTooComplex)) {
				t.Errorf("%s %+v: %v", name, tc.limits, err)
			}
		}
	}
}
