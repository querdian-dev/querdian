package policy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/regexcost"
)

func strs(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

func eqs(n int) []v1.Matcher {
	out := make([]v1.Matcher, n)
	for i := range out {
		out[i] = eq(fmt.Sprintf("l%d", i), "v")
	}
	return out
}

func redacts(n int) []v1.RedactRule {
	out := make([]v1.RedactRule, n)
	for i := range out {
		out[i] = v1.RedactRule{Name: fmt.Sprintf("r%d", i), Strategy: v1.RedactHash}
	}
	return out
}

// sizeCase changes a valid spec so that one list or string has size n. atLimit is the reason at the limit.
type sizeCase struct {
	name    string
	limit   int
	atLimit string
	cluster func(s *v1.ClusterAccessPolicySpec, n int)
	ns      func(s *v1.AccessPolicySpec, n int) // nil if the field is not in an AccessPolicy
}

var sizeCases = []sizeCase{
	{"subjects.groups", MaxSubjects, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) { s.Subjects.Groups = strs("g", n) },
		func(s *v1.AccessPolicySpec, n int) { s.Subjects.Groups = strs("g", n) }},
	{"subjects.users", MaxSubjects, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) { s.Subjects.Users = strs("u", n) },
		func(s *v1.AccessPolicySpec, n int) { s.Subjects.Users = strs("u", n) }},
	{"group length", MaxSubjectLength, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) { s.Subjects.Groups = []string{strings.Repeat("g", n)} },
		func(s *v1.AccessPolicySpec, n int) { s.Subjects.Groups = []string{strings.Repeat("g", n)} }},
	{"user length", MaxSubjectLength, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) { s.Subjects.Users = []string{strings.Repeat("ü", n)} },
		func(s *v1.AccessPolicySpec, n int) { s.Subjects.Users = []string{strings.Repeat("ü", n)} }},
	{"rules", MaxRules, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) {
			for len(s.Rules) < n {
				s.Rules = append(s.Rules, s.Rules[0])
			}
		},
		func(s *v1.AccessPolicySpec, n int) {
			for len(s.Rules) < n {
				s.Rules = append(s.Rules, s.Rules[0])
			}
		}},
	{"backends", MaxBackends, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) { s.Rules[0].Backends = repeat("logs", n) },
		func(s *v1.AccessPolicySpec, n int) { s.Rules[0].Backends = repeat("metrics", n) }},
	{"backend length", MaxNameLength, ReasonUnknownBackend,
		func(s *v1.ClusterAccessPolicySpec, n int) { s.Rules[0].Backends = []string{strings.Repeat("b", n)} },
		func(s *v1.AccessPolicySpec, n int) { s.Rules[0].Backends = []string{strings.Repeat("b", n)} }},
	{"tenants", MaxTenants, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) { s.Rules[0].Tenants = strs("t", n) }, nil},
	{"tenant length", MaxNameLength, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) { s.Rules[0].Tenants = []string{strings.Repeat("t", n)} }, nil},
	{"matchers", MaxMatchers, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) { s.Rules[0].Matchers = eqs(n) },
		func(s *v1.AccessPolicySpec, n int) { s.Rules[0].Matchers = eqs(n) }},
	{"matcher name length", MaxNameLength, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) {
			s.Rules[0].Matchers = []v1.Matcher{eq(strings.Repeat("a", n), "v")}
		},
		func(s *v1.AccessPolicySpec, n int) {
			s.Rules[0].Matchers = []v1.Matcher{eq(strings.Repeat("a", n), "v")}
		}},
	{"matcher value length", MaxValueLength, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) {
			s.Rules[0].Matchers = []v1.Matcher{eq("a", strings.Repeat("x", n))}
		},
		func(s *v1.AccessPolicySpec, n int) {
			s.Rules[0].Matchers = []v1.Matcher{eq("a", strings.Repeat("x", n))}
		}},
	{"matcher value characters", MaxValueLength, ReasonCompiled, // 2 bytes per character
		func(s *v1.ClusterAccessPolicySpec, n int) {
			s.Rules[0].Matchers = []v1.Matcher{eq("a", strings.Repeat("é", n))}
		},
		func(s *v1.AccessPolicySpec, n int) {
			s.Rules[0].Matchers = []v1.Matcher{eq("a", strings.Repeat("é", n))}
		}},
	{"labels.deny", MaxLabelNames, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) { s.Rules[0].Labels = &v1.LabelFilter{Deny: strs("d", n)} },
		func(s *v1.AccessPolicySpec, n int) { s.Rules[0].Labels = &v1.LabelFilter{Deny: strs("d", n)} }},
	{"denied name length", MaxNameLength, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) {
			s.Rules[0].Labels = &v1.LabelFilter{Deny: []string{strings.Repeat("d", n)}}
		},
		func(s *v1.AccessPolicySpec, n int) {
			s.Rules[0].Labels = &v1.LabelFilter{Deny: []string{strings.Repeat("d", n)}}
		}},
	{"labels.redact", MaxLabelNames, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) { s.Rules[0].Labels = &v1.LabelFilter{Redact: redacts(n)} },
		func(s *v1.AccessPolicySpec, n int) { s.Rules[0].Labels = &v1.LabelFilter{Redact: redacts(n)} }},
	{"redacted name length", MaxNameLength, ReasonCompiled,
		func(s *v1.ClusterAccessPolicySpec, n int) {
			s.Rules[0].Labels = &v1.LabelFilter{Redact: []v1.RedactRule{{Name: strings.Repeat("r", n), Strategy: v1.RedactRemove}}}
		},
		func(s *v1.AccessPolicySpec, n int) {
			s.Rules[0].Labels = &v1.LabelFilter{Redact: []v1.RedactRule{{Name: strings.Repeat("r", n), Strategy: v1.RedactRemove}}}
		}},
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func baseClusterSpec() v1.ClusterAccessPolicySpec {
	return v1.ClusterAccessPolicySpec{Subjects: v1.Subjects{Groups: []string{"g"}}, Rules: []v1.ClusterRule{
		{Backends: []string{"logs"}, Tenants: []string{"t"}, Matchers: []v1.Matcher{eq("k8s_namespace_name", "a")}},
	}}
}

func baseSpec() v1.AccessPolicySpec {
	return v1.AccessPolicySpec{Subjects: v1.Subjects{Groups: []string{"g"}}, Rules: []v1.Rule{
		{Backends: []string{"metrics"}, Matchers: []v1.Matcher{eq("team", "a")}},
	}}
}

// TestPolicySizeLimits: each limit holds at its value and rejects the value plus one, in the compiler and in
// the check that the receiver uses.
func TestPolicySizeLimits(t *testing.T) {
	cfg := testConfig(t)
	for _, tc := range sizeCases {
		for _, n := range []int{tc.limit, tc.limit + 1} {
			want := tc.atLimit
			if n > tc.limit {
				want = ReasonPolicyTooLarge
			}
			t.Run(fmt.Sprintf("%s/%d", tc.name, n), func(t *testing.T) {
				cs := baseClusterSpec()
				tc.cluster(&cs, n)
				cp := v1.ClusterAccessPolicy{Spec: cs}
				cp.Name = "c"
				in := Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{cp}}
				if tc.ns != nil {
					s := baseSpec()
					tc.ns(&s, n)
					in.Policies = []v1.AccessPolicy{{Spec: s}}
					in.Policies[0].Name, in.Policies[0].Namespace = "a", "ns"
					if err := CheckSpecSize(&s); (err != nil) != (n > tc.limit) {
						t.Errorf("CheckSpecSize: %v", err)
					}
				}
				if err := CheckClusterSpecSize(&cs); (err != nil) != (n > tc.limit) {
					t.Errorf("CheckClusterSpecSize: %v", err)
				}
				c := Compile(in)
				for _, r := range c.Results {
					if r.Reason != want {
						t.Errorf("%s: reason %s (%s), want %s", r.Kind, r.Reason, r.Message, want)
					}
					if want == ReasonPolicyTooLarge && (r.Ready || !strings.Contains(r.Message, "limit")) {
						t.Errorf("%s: ready=%v message %q", r.Kind, r.Ready, r.Message)
					}
				}
				if want == ReasonPolicyTooLarge && (len(c.ByGroup) > 0 || len(c.ByUser) > 0) {
					t.Error("a policy over a limit must grant nothing")
				}
			})
		}
	}
}

func TestSpokePolicySizeLimits(t *testing.T) {
	cfg := mcConfig(t)
	for _, n := range []int{MaxMatchers, MaxMatchers + 1} {
		cp := spokeCAP("cluster-a", "c", gSubj, v1.ClusterRule{Backends: metrics, Matchers: eqs(n)})
		rules := make([]v1.Rule, n)
		for i := range rules {
			rules[i] = v1.Rule{Backends: metrics}
		}
		ap := spokeAP("cluster-a", "ns", "a", "", gSubj, rules...)
		c := Compile(Input{Config: cfg, SpokeClusters: []v1.SpokeCluster{spokeA}, SpokePolicies: []v1.SpokePolicy{cp, ap}})
		want := ReasonCompiled
		if n > MaxMatchers {
			want = ReasonPolicyTooLarge
		}
		for _, p := range []v1.SpokePolicy{cp, ap} {
			if r := spokeResult(t, c, p); r.Reason != want {
				t.Errorf("%d: %s: reason %s (%s), want %s", n, p.Spec.Kind, r.Reason, r.Message, want)
			}
		}
	}
}

// TestCRDLimitsAgree: the markers in api/v1alpha1 give the same limits as the constants of this package.
func TestCRDLimitsAgree(t *testing.T) {
	type check struct {
		path string // dot path below spec; [] is the item of a list
		key  string
		want int
	}
	common := []check{
		{"subjects.groups", "maxItems", MaxSubjects},
		{"subjects.groups[]", "maxLength", MaxSubjectLength},
		{"subjects.users", "maxItems", MaxSubjects},
		{"subjects.users[]", "maxLength", MaxSubjectLength},
		{"rules", "maxItems", MaxRules},
		{"rules[].backends", "maxItems", MaxBackends},
		{"rules[].backends[]", "maxLength", MaxNameLength},
		{"rules[].matchers", "maxItems", MaxMatchers},
		{"rules[].matchers[].name", "maxLength", MaxNameLength},
		{"rules[].matchers[].value", "maxLength", MaxValueLength},
		{"rules[].labels.deny", "maxItems", MaxLabelNames},
		{"rules[].labels.deny[]", "maxLength", MaxNameLength},
		{"rules[].labels.redact", "maxItems", MaxLabelNames},
		{"rules[].labels.redact[].name", "maxLength", MaxNameLength},
	}
	cluster := append(append([]check(nil), common...),
		check{"rules[].tenants", "maxItems", MaxTenants},
		check{"rules[].tenants[]", "maxLength", MaxNameLength})
	files := []struct {
		file, prefix string
		checks       []check
	}{
		{"policy.querdian.dev_clusteraccesspolicies.yaml", "", cluster},
		{"policy.querdian.dev_accesspolicies.yaml", "", common},
		{"policy.querdian.dev_spokepolicies.yaml", "clusterAccessPolicy.", cluster},
		{"policy.querdian.dev_spokepolicies.yaml", "accessPolicy.", common},
	}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", f.file))
		if err != nil {
			t.Fatal(err)
		}
		var crd map[string]any
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatal(err)
		}
		versions := crd["spec"].(map[string]any)["versions"].([]any)
		spec := versions[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)["properties"].(map[string]any)["spec"]
		for _, c := range f.checks {
			node := schemaAt(t, spec, f.prefix+c.path)
			got, ok := node[c.key].(float64)
			if !ok || int(got) != c.want {
				t.Errorf("%s: %s%s: %s = %v, want %d", f.file, f.prefix, c.path, c.key, node[c.key], c.want)
			}
		}
	}
}

func schemaAt(t *testing.T, node any, path string) map[string]any {
	t.Helper()
	cur := node.(map[string]any)
	for _, part := range strings.Split(path, ".") {
		name, list := strings.CutSuffix(part, "[]")
		props, _ := cur["properties"].(map[string]any)
		next, ok := props[name].(map[string]any)
		if !ok {
			t.Fatalf("no schema for %q in %s", name, path)
		}
		cur = next
		if list {
			cur = cur["items"].(map[string]any)
		}
	}
	return cur
}

// manyPolicies returns np ClusterAccessPolicy objects for user u with nr rules each. Each rule has extra
// matchers, the same in all rules, and a namespace matcher of its own, so all terms merge into one.
func manyPolicies(np, nr, extra int, value string) []v1.ClusterAccessPolicy {
	var shared []v1.Matcher
	for i := 0; i < extra; i++ {
		shared = append(shared, eq(fmt.Sprintf("a%02d", i), value))
	}
	out := make([]v1.ClusterAccessPolicy, np)
	for p := range out {
		rules := make([]v1.ClusterRule, nr)
		for r := range rules {
			// Short names, so that 4096 of them merge within MaxResultWeight.
			ms := append([]v1.Matcher{ns(fmt.Sprintf("%02x%02x", p, r))}, shared...)
			rules[r] = v1.ClusterRule{Backends: []string{"metrics"}, Matchers: ms}
		}
		out[p] = capol(fmt.Sprintf("p%d", p), v1.Subjects{Users: []string{"u"}}, rules...)
	}
	return out
}

// TestResolveAtInputLimits: ResolveAt rejects too much input for Normalise, and accepts input at the limits.
func TestResolveAtInputLimits(t *testing.T) {
	perPolicy := MaxResolveTerms / MaxRules
	tests := []struct {
		name string
		cps  []v1.ClusterAccessPolicy
		ok   bool
	}{
		{"terms at the limit", manyPolicies(perPolicy, MaxRules, 0, "production"), true},
		{"terms over the limit", append(manyPolicies(perPolicy, MaxRules, 0, "production"), manyPolicies(1, 1, 0, "production")[0]), false},
		{"matchers at the limit", manyPolicies(perPolicy, MaxRules, MaxResolveMatchers/MaxResolveTerms-1, "production"), true},
		{"matchers over the limit", manyPolicies(perPolicy, MaxRules, MaxResolveMatchers/MaxResolveTerms, "production"), false},
		{"bytes over the limit", manyPolicies(1, MaxRules, MaxResolveBytes/MaxRules/MaxValueLength, strings.Repeat("x", MaxValueLength)), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := snap(t, tc.cps, nil, nil)
			eff, err := s.Resolve("u", nil, "metrics")
			if tc.ok {
				if err != nil || len(eff.Terms) != 1 {
					t.Fatalf("want one merged term, got %v, %d terms", err, len(eff.Terms))
				}
				return
			}
			if !errors.Is(err, ErrTooComplex) {
				t.Fatalf("want ErrTooComplex, got %v", err)
			}
		})
	}
}

// largestPolicy is one ClusterAccessPolicy at every size limit, on a backend with tenants.
func largestPolicy() v1.ClusterAccessPolicy {
	ms := make([]v1.Matcher, MaxMatchers)
	for i := range ms {
		ms[i] = eq(fmt.Sprintf("l%d", i), strings.Repeat(fmt.Sprint(i%10), MaxValueLength))
	}
	rules := make([]v1.ClusterRule, MaxRules)
	for i := range rules {
		rules[i] = v1.ClusterRule{
			Backends: repeat("logs", MaxBackends), Tenants: strs("t", MaxTenants), Matchers: ms,
			Labels: &v1.LabelFilter{Deny: strs("d", MaxLabelNames), Redact: redacts(MaxLabelNames)},
		}
	}
	return capol("largest", v1.Subjects{Users: []string{"u"}, Groups: strs("g", MaxSubjects)}, rules...)
}

// TestResolveAtBounded: a request on the largest valid policy, and a request at the input limits of
// ResolveAt, use bounded memory and time.
func TestResolveAtBounded(t *testing.T) {
	tests := []struct {
		name string
		cps  []v1.ClusterAccessPolicy
		be   string
	}{
		{"largest policy", []v1.ClusterAccessPolicy{largestPolicy()}, "logs"},
		{"input limits", manyPolicies(MaxResolveTerms/MaxRules, MaxRules, MaxResolveMatchers/MaxResolveTerms-1, "production"), "metrics"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := snap(t, tc.cps, nil, nil)
			if r := s.Results()[0]; !r.Ready {
				t.Fatalf("the policy must be valid: %s %s", r.Reason, r.Message)
			}
			start := time.Now()
			alloc := allocBytes(func() { _, _ = s.Resolve("u", nil, tc.be) })
			if d := time.Since(start); d > timeScale*5*time.Second {
				t.Errorf("ResolveAt took %v", d)
			}
			if alloc > 128<<20 {
				t.Errorf("ResolveAt allocated %d MiB", alloc>>20)
			}
		})
	}
}

func BenchmarkResolveAtLimits(b *testing.B) {
	for _, bc := range []struct {
		name string
		cps  []v1.ClusterAccessPolicy
		be   string
	}{
		{"largest policy", []v1.ClusterAccessPolicy{largestPolicy()}, "logs"},
		{"input limits", manyPolicies(MaxResolveTerms/MaxRules, MaxRules, MaxResolveMatchers/MaxResolveTerms-1, "production"), "metrics"},
	} {
		b.Run(bc.name, func(b *testing.B) {
			s := snap(b, bc.cps, nil, nil)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = s.Resolve("u", nil, bc.be)
			}
		})
	}
}

// weightPrefix makes a short regex with a large weight; atWeightLimit adds literal runes to it.
var weightPrefix = strings.Repeat("x{1000}", 15)

func atWeightLimit(n int) string { return weightPrefix + strings.Repeat("a", n) }

// mostLiteral returns the largest n for which atWeightLimit(n) has a weight of at most MaxRegexWeight.
func mostLiteral(t testing.TB) int {
	t.Helper()
	n := sort.Search(MaxValueLength, func(n int) bool {
		w, err := RegexWeight(atWeightLimit(n + 1))
		return err != nil || w > MaxRegexWeight
	})
	if n == 0 || n >= MaxValueLength || len(atWeightLimit(n+1)) > regexcost.MaxRegexBytes {
		t.Fatalf("mostLiteral = %d", n)
	}
	return n
}

// TestRegexWeightLimits: a regex matcher over MaxRegexWeight, or regexes of a policy over
// MaxPolicyRegexWeight, make the policy PolicyTooLarge, without a compile of the regex.
func TestRegexWeightLimits(t *testing.T) {
	cfg := testConfig(t)
	// "^(?:aaa)$" has the weight 3+2, and a program has 4 fixed instructions.
	lit := func(n int) v1.Matcher {
		return v1.Matcher{Name: "a", Op: v1.MatchRegexMatch, Value: strings.Repeat("a", n)}
	}
	// "^(?:x{1000}...)$" with k repetitions has the weight 1000k+6.
	rep := func(k int) v1.Matcher {
		return v1.Matcher{Name: "a", Op: v1.MatchRegexNoMatch, Value: strings.Repeat("x{1000}", k)}
	}
	k := (MaxPolicyRegexWeight / 4) / 1000 // 4 matchers of this size fit, 5 do not
	tests := []struct {
		name string
		ms   []v1.Matcher
		want string
	}{
		{"regex at the limit", []v1.Matcher{{Name: "a", Op: v1.MatchRegexMatch, Value: atWeightLimit(mostLiteral(t))}}, ReasonCompiled},
		{"regex over the limit", []v1.Matcher{{Name: "a", Op: v1.MatchRegexMatch, Value: atWeightLimit(mostLiteral(t) + 1)}}, ReasonPolicyTooLarge},
		{"regex bytes at the limit", []v1.Matcher{lit(regexcost.MaxRegexBytes)}, ReasonCompiled},
		{"regex bytes over the limit", []v1.Matcher{lit(regexcost.MaxRegexBytes + 1)}, ReasonPolicyTooLarge},
		{"Equal value over the regex byte limit", []v1.Matcher{eq("a", strings.Repeat("x", regexcost.MaxRegexBytes+1))}, ReasonCompiled},
		{"character classes", []v1.Matcher{{Name: "a", Op: v1.MatchRegexMatch, Value: strings.Repeat(`\pL`, 30)}}, ReasonPolicyTooLarge},
		{"case folding of wide ranges", []v1.Matcher{{Name: "a", Op: v1.MatchRegexMatch, Value: "(?i)" + strings.Repeat(`[\x{100}-\x{1E900}]`, 3)}}, ReasonPolicyTooLarge},
		{"normal regexes", []v1.Matcher{{Name: "a", Op: v1.MatchRegexMatch, Value: `prod-.*`}, {Name: "b", Op: v1.MatchRegexMatch, Value: `[a-z]+`},
			{Name: "c", Op: v1.MatchRegexMatch, Value: `(?i)team-[a-z]+`}, {Name: "d", Op: v1.MatchRegexMatch, Value: `(?i)^[a-z]+-[a-z]+-\d+-\w+$`},
			{Name: "e", Op: v1.MatchRegexMatch, Value: `(?i)(warn|error)\s+\d+`}}, ReasonCompiled},
		{"short regex, large program", []v1.Matcher{rep(MaxRegexWeight/1000 + 1)}, ReasonPolicyTooLarge},
		{"policy at the limit", []v1.Matcher{rep(k), rep(k), rep(k), rep(k)}, ReasonCompiled},
		{"policy over the limit", []v1.Matcher{rep(k), rep(k), rep(k), rep(k), rep(k)}, ReasonPolicyTooLarge},
		{"Equal values do not count", []v1.Matcher{eq("a", strings.Repeat("x{1000}", 2000))}, ReasonCompiled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := baseClusterSpec()
			s.Rules[0].Matchers = append(s.Rules[0].Matchers, tc.ms...)
			cp := v1.ClusterAccessPolicy{Spec: s}
			cp.Name = "c"
			var c *Compiled
			alloc := allocBytes(func() { c = Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{cp}}) })
			if r := c.Results[0]; r.Reason != tc.want {
				t.Fatalf("reason %s (%s), want %s", r.Reason, r.Message, tc.want)
			}
			if alloc > 256<<20 {
				t.Errorf("Compile allocated %d MiB", alloc>>20)
			}
		})
	}
	// The same matchers in two rules count together.
	s := baseClusterSpec()
	r := s.Rules[0]
	r.Matchers = []v1.Matcher{rep(k), rep(k), rep(k)}
	s.Rules = []v1.ClusterRule{r, r}
	if err := CheckClusterSpecSize(&s); err == nil {
		t.Error("the regexes of all rules must count together")
	}
}

// TestResultWeightAfterBinding: the __tenant_id__ matchers that bindTenants adds count for the result limit.
func TestResultWeightAfterBinding(t *testing.T) {
	// Two terms of 4 Equal matchers each; a value of n characters has the weight n+1.
	rule := func(tenant string, n int) v1.ClusterRule {
		var ms []v1.Matcher
		for i := range 4 {
			ms = append(ms, eq(fmt.Sprint("a", i), tenant+strings.Repeat("v", n-len(tenant))))
		}
		return v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{tenant}, Matchers: ms}
	}
	for _, tc := range []struct {
		n  int
		ok bool
	}{
		{MaxResultWeight/8 - 2, true},  // MaxResultWeight - 8 before the binding, - 2 after
		{MaxResultWeight/8 - 1, false}, // MaxResultWeight before the binding, + 6 after
	} {
		s := snap(t, []v1.ClusterAccessPolicy{capol("p", v1.Subjects{Users: []string{"u"}}, rule("t1", tc.n), rule("t2", tc.n))}, nil, nil)
		_, err := s.Resolve("u", nil, "logs")
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, ErrTooComplex)) {
			t.Errorf("n=%d: %v", tc.n, err)
		}
	}
}

// TestResolveAtRegexLimits: ResolveAt gives the regex limits of the static config to the enforcers.
func TestResolveAtRegexLimits(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxQueryRegexWeight, cfg.Limits.MaxRequestRegexWeight = 123, 456
	s := NewSnapshot(Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{
		capol("p", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{ns("a")}}),
	}}), cfg, 1)
	eff, err := s.Resolve("u", nil, "metrics")
	if err != nil || eff.RegexLimits != (RegexLimits{Query: 123, Request: 456}) {
		t.Fatalf("%v %+v", err, eff.RegexLimits)
	}
}
