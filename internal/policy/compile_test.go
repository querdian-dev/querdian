package policy

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
)

func testConfig(t testing.TB) *config.Config {
	t.Helper()
	c, err := config.Load("../config/testdata/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func eq(n, v string) v1.Matcher { return v1.Matcher{Name: n, Op: v1.MatchEqual, Value: v} }

func capol(name string, subj v1.Subjects, rules ...v1.ClusterRule) v1.ClusterAccessPolicy {
	return v1.ClusterAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 3},
		Spec:       v1.ClusterAccessPolicySpec{Subjects: subj, Rules: rules},
	}
}

func ap(ns, name string, subj v1.Subjects, rules ...v1.Rule) v1.AccessPolicy {
	return v1.AccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v1.AccessPolicySpec{Subjects: subj, Rules: rules},
	}
}

func result(t *testing.T, c *Compiled, kind, name string) Result {
	t.Helper()
	for _, r := range c.Results {
		if r.Kind == kind && r.Name == name {
			return r
		}
	}
	t.Fatalf("no result for %s/%s", kind, name)
	return Result{}
}

func TestClusterPolicyReasons(t *testing.T) {
	cfg := testConfig(t)
	g := v1.Subjects{Groups: []string{"g"}}
	tests := []struct {
		name      string
		rule      v1.ClusterRule
		reason    string
		wantReady bool
	}{
		{"compiled unrestricted", v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true}, ReasonCompiled, true},
		{"compiled tenants", v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eq("k8s_namespace_name", "a")}}, ReasonCompiled, true},
		{"invalid regex", v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{{Name: "a", Op: v1.MatchRegexMatch, Value: "("}}}, ReasonInvalidMatcher, false},
		{"invalid prom label name", v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{eq("1bad", "x")}}, ReasonInvalidMatcher, false},
		{"dotted name invalid on loki", v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t"}, Matchers: []v1.Matcher{eq("a.b", "x")}}, ReasonInvalidMatcher, false},
		{"reserved tenant label", v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{eq(TenantIDLabel, "x")}}, ReasonInvalidMatcher, false},
		{"unscoped tempo attr invalid", v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"t"}, Matchers: []v1.Matcher{eq("service.name", "x")}}, ReasonInvalidMatcher, false},
		{"scoped tempo attr valid", v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"t"}, Matchers: []v1.Matcher{eq("resource.service.name", "x")}}, ReasonCompiled, true},
		{"invalid deny label", v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true, Labels: &v1.LabelFilter{Deny: []string{"bad-name"}}}, ReasonInvalidMatcher, false},
		{"unknown op", v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{{Name: "a", Op: "Like", Value: "x"}}}, ReasonInvalidMatcher, false},
		{"unknown backend", v1.ClusterRule{Backends: []string{"nope"}, Unrestricted: true}, ReasonUnknownBackend, false},
		{"tenant required", v1.ClusterRule{Backends: []string{"logs"}, Unrestricted: true}, ReasonTenantRequired, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{capol("p", g, tc.rule)}})
			r := result(t, c, "ClusterAccessPolicy", "p")
			if r.Reason != tc.reason || r.Ready != tc.wantReady || r.Generation != 3 {
				t.Fatalf("got %+v, want reason %s ready %v", r, tc.reason, tc.wantReady)
			}
			if got := len(c.ByGroup["g"]) > 0; got != tc.wantReady {
				t.Fatalf("rules indexed = %v, want %v (invalid policies must grant nothing)", got, tc.wantReady)
			}
		})
	}
}

func TestInvalidRuleRejectsWholePolicy(t *testing.T) {
	cfg := testConfig(t)
	p := capol("p", v1.Subjects{Users: []string{"u"}},
		v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true},
		v1.ClusterRule{Backends: []string{"nope"}, Unrestricted: true})
	c := Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{p}})
	if len(c.ByUser["u"]) != 0 || result(t, c, "ClusterAccessPolicy", "p").Reason != ReasonUnknownBackend {
		t.Fatalf("whole policy must be rejected: %+v", c)
	}
}

func TestSingleTenantBackendIgnoresTenants(t *testing.T) {
	cfg := testConfig(t)
	c := Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{
		capol("p", v1.Subjects{Groups: []string{"g"}}, v1.ClusterRule{Backends: []string{"metrics"}, Tenants: []string{"t1"}, Unrestricted: true}),
	}})
	if r := c.ByGroup["g"][0]; len(r.Tenants) != 0 {
		t.Fatalf("single-tenant backend must have no tenants: %+v", r.Tenants)
	}
}

func TestAccessPolicy(t *testing.T) {
	cfg := testConfig(t)
	subj := v1.Subjects{Groups: []string{"team"}, Users: []string{"alice"}}
	pol := ap("order", "p", subj,
		v1.Rule{Backends: []string{"metrics", "logs", "traces"}, Matchers: []v1.Matcher{{Name: "service", Op: v1.MatchNotEqual, Value: ""}}})
	pol.Spec.Rules[0].Matchers = nil // matchers are backend-specific; use implicit matcher only
	ns := map[string]map[string]string{"order": {TenantLabel: "tenant1"}}

	c := Compile(Input{Config: cfg, Policies: []v1.AccessPolicy{pol}, NamespaceLabels: ns})
	if r := result(t, c, "AccessPolicy", "p"); r.Reason != ReasonCompiled || !r.Ready || r.Namespace != "order" {
		t.Fatalf("unexpected result %+v", r)
	}
	want := map[string]struct {
		label  string
		tenant []string
	}{
		"metrics": {"namespace", nil},
		"logs":    {"k8s_namespace_name", []string{"tenant1"}},
		"traces":  {"resource.k8s.namespace.name", []string{"tenant1"}},
	}
	rules := c.ByGroup["team"]
	if len(rules) != 3 || len(c.ByUser["alice"]) != 3 {
		t.Fatalf("want 3 rules per subject, got %d/%d", len(rules), len(c.ByUser["alice"]))
	}
	for _, r := range rules {
		w := want[r.Backend]
		m := r.Matchers[0]
		if m.Name != w.label || m.Op != v1.MatchEqual || m.Value != "order" || r.Unrestricted {
			t.Errorf("%s: implicit namespace matcher wrong: %+v", r.Backend, r.Matchers)
		}
		if strings.Join(r.Tenants, ",") != strings.Join(w.tenant, ",") {
			t.Errorf("%s: tenants = %v, want %v", r.Backend, r.Tenants, w.tenant)
		}
		if r.Policy != "AccessPolicy/order/p" {
			t.Errorf("policy ref = %q", r.Policy)
		}
	}
}

func TestAccessPolicyUserMatchersAnded(t *testing.T) {
	cfg := testConfig(t)
	pol := ap("order", "p", v1.Subjects{Users: []string{"u"}},
		v1.Rule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{eq("service", "api")}})
	c := Compile(Input{Config: cfg, Policies: []v1.AccessPolicy{pol}})
	ms := c.ByUser["u"][0].Matchers
	if len(ms) != 2 || ms[0].Name != "namespace" || ms[1].Name != "service" {
		t.Fatalf("want implicit namespace matcher first then user matcher, got %+v", ms)
	}
}

func TestTenantNotAssigned(t *testing.T) {
	cfg := testConfig(t)
	pol := ap("order", "p", v1.Subjects{Users: []string{"u"}}, v1.Rule{Backends: []string{"logs"}})
	for name, ns := range map[string]map[string]map[string]string{
		"namespace unknown":    {},
		"label missing":        {"order": {"other": "x"}},
		"label empty":          {"order": {TenantLabel: " "}},
		"namespace has others": {"order": {}},
	} {
		c := Compile(Input{Config: cfg, Policies: []v1.AccessPolicy{pol}, NamespaceLabels: ns})
		r := result(t, c, "AccessPolicy", "p")
		if r.Reason != ReasonTenantNotAssigned || r.Ready || len(c.ByUser["u"]) != 0 {
			t.Errorf("%s: got %+v", name, r)
		}
	}
	// Flips to Compiled once the label appears.
	c := Compile(Input{Config: cfg, Policies: []v1.AccessPolicy{pol}, NamespaceLabels: map[string]map[string]string{"order": {TenantLabel: "t1"}}})
	if r := result(t, c, "AccessPolicy", "p"); r.Reason != ReasonCompiled {
		t.Fatalf("got %+v", r)
	}
	// Single-tenant backend needs no tenant label.
	pol2 := ap("order", "q", v1.Subjects{Users: []string{"u"}}, v1.Rule{Backends: []string{"metrics"}})
	c = Compile(Input{Config: cfg, Policies: []v1.AccessPolicy{pol2}})
	if r := result(t, c, "AccessPolicy", "q"); r.Reason != ReasonCompiled {
		t.Fatalf("got %+v", r)
	}
}

func TestAccessPolicyErrors(t *testing.T) {
	cfg := testConfig(t)
	ns := map[string]map[string]string{"order": {TenantLabel: "t1"}}
	for name, tc := range map[string]struct {
		rule   v1.Rule
		reason string
	}{
		"unknown backend": {v1.Rule{Backends: []string{"nope"}}, ReasonUnknownBackend},
		"bad regex":       {v1.Rule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{{Name: "a", Op: v1.MatchRegexNoMatch, Value: "[a"}}}, ReasonInvalidMatcher},
		"bad deny":        {v1.Rule{Backends: []string{"metrics"}, Labels: &v1.LabelFilter{Deny: []string{"a b"}}}, ReasonInvalidMatcher},
	} {
		c := Compile(Input{Config: cfg, Policies: []v1.AccessPolicy{ap("order", "p", v1.Subjects{Users: []string{"u"}}, tc.rule)}, NamespaceLabels: ns})
		if r := result(t, c, "AccessPolicy", "p"); r.Reason != tc.reason || r.Ready {
			t.Errorf("%s: got %+v", name, r)
		}
	}
}

func TestInvalidImplicitNamespaceLabel(t *testing.T) {
	cfg := testConfig(t)
	cfg.Backends[0].NamespaceLabel = "not a label"
	c := Compile(Input{Config: cfg, Policies: []v1.AccessPolicy{ap("order", "p", v1.Subjects{Users: []string{"u"}}, v1.Rule{Backends: []string{"metrics"}})}})
	if r := result(t, c, "AccessPolicy", "p"); r.Reason != ReasonInvalidMatcher {
		t.Fatalf("got %+v", r)
	}
}

func TestUsersAndGroupsSeparateKeyspaces(t *testing.T) {
	cfg := testConfig(t)
	c := Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{
		capol("by-group", v1.Subjects{Groups: []string{"admins"}}, v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true}),
		capol("by-user", v1.Subjects{Users: []string{"bob"}}, v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true}),
	}})
	if len(c.ByUser["admins"]) != 0 || len(c.ByGroup["admins"]) != 1 {
		t.Fatalf("user named like a group must not inherit its grant: %+v %+v", c.ByUser, c.ByGroup)
	}
	if len(c.ByGroup["bob"]) != 0 || len(c.ByUser["bob"]) != 1 {
		t.Fatal("group named like a user must not inherit its grant")
	}
}

func TestRegexAnchoredAndMatches(t *testing.T) {
	cfg := testConfig(t)
	c := Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{
		capol("p", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{
			{Name: "namespace", Op: v1.MatchRegexMatch, Value: "a|b"},
			{Name: "pod", Op: v1.MatchRegexNoMatch, Value: "x.*"},
			eq("env", "prod"),
			{Name: "tier", Op: v1.MatchNotEqual, Value: "db"},
		}}),
	}})
	ms := c.ByUser["u"][0].Matchers
	for i, tc := range []struct {
		val  string
		want bool
	}{{"a", true}, {"b", true}, {"ab", false}, {"xa", false}, {"", false}} {
		if got := ms[0].Matches(tc.val); got != tc.want {
			t.Errorf("namespace=~a|b on %q = %v, want %v (case %d)", tc.val, got, tc.want, i)
		}
	}
	if ms[1].Matches("xyz") || !ms[1].Matches("yx") || !ms[1].Matches("") {
		t.Error("RegexNoMatch is anchored and matches absent labels")
	}
	if !ms[2].Matches("prod") || ms[2].Matches("") || !ms[3].Matches("") || ms[3].Matches("db") {
		t.Error("Equal/NotEqual semantics wrong")
	}
}

func TestDeniedLabelsSortedDeduped(t *testing.T) {
	cfg := testConfig(t)
	c := Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{
		capol("p", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{"metrics", "metrics"}, Unrestricted: true,
			Labels: &v1.LabelFilter{Deny: []string{"z", "a", "z"}}}),
	}})
	rs := c.ByUser["u"]
	if len(rs) != 1 || strings.Join(rs[0].DeniedLabels, ",") != "a,z" {
		t.Fatalf("got %+v", rs)
	}
}

func TestCrossTenantUnionUnsupported(t *testing.T) {
	cfg := testConfig(t) // traces: tenantHeader set, federation none; logs: federation tenantIdLabel
	g := v1.Subjects{Groups: []string{"g"}}
	same := capol("same", g, v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"t1", "t2"}, Matchers: []v1.Matcher{eq("resource.a", "b")}})
	c := Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{same}})
	if r := result(t, c, "ClusterAccessPolicy", "same"); r.Reason != ReasonCompiled {
		t.Fatalf("identical terms across tenants are fine: %+v", r)
	}

	a := capol("a", g, v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eq("resource.a", "b")}})
	b := capol("b", g, v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"t2"}, Matchers: []v1.Matcher{eq("resource.a", "c")}})
	other := capol("other", v1.Subjects{Groups: []string{"unrelated"}}, v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true})
	c = Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{a, b, other}})
	for _, n := range []string{"a", "b"} {
		r := result(t, c, "ClusterAccessPolicy", n)
		if r.Reason != ReasonCrossTenantUnionUnsupported || !r.Ready || !strings.Contains(r.Message, "traces") {
			t.Errorf("%s: got %+v", n, r)
		}
	}
	if r := result(t, c, "ClusterAccessPolicy", "other"); r.Reason != ReasonCompiled {
		t.Errorf("unrelated policy affected: %+v", r)
	}
	if len(c.ByGroup["g"]) != 2 {
		t.Error("policy must stay active")
	}

	// Unrestricted on one tenant and restricted on another also differs.
	u := capol("u", g, v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"t1"}, Unrestricted: true})
	c = Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{u, b}})
	if r := result(t, c, "ClusterAccessPolicy", "u"); r.Reason != ReasonCrossTenantUnionUnsupported {
		t.Errorf("got %+v", r)
	}

	// Backends with federation support are never flagged.
	l1 := capol("l1", g, v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eq("a", "b")}})
	l2 := capol("l2", g, v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t2"}, Matchers: []v1.Matcher{eq("a", "c")}})
	c = Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{l1, l2}})
	if r := result(t, c, "ClusterAccessPolicy", "l1"); r.Reason != ReasonCompiled {
		t.Errorf("got %+v", r)
	}
}

func i32(v int32) *int32 { return &v }

func TestRedactCompiles(t *testing.T) {
	cfg := testConfig(t)
	c := Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{
		capol("p", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true,
			Labels: &v1.LabelFilter{Deny: []string{"d"}, Redact: []v1.RedactRule{
				{Name: "c", Strategy: v1.RedactPlaceholder},
				{Name: "ip", Strategy: v1.RedactIPv4Prefix, Octets: i32(2)},
			}}}),
	}})
	if r := result(t, c, "ClusterAccessPolicy", "p"); !r.Ready {
		t.Fatalf("not ready: %+v", r)
	}
	rs := c.ByUser["u"]
	if len(rs) != 1 || strings.Join(rs[0].DeniedLabels, ",") != "c,d,ip" {
		t.Fatalf("redacted names must be denied too: %+v", rs)
	}
	want := map[string]Redaction{"c": {Strategy: v1.RedactPlaceholder}, "ip": {Strategy: v1.RedactIPv4Prefix, Octets: 2}}
	if len(rs[0].Redactions) != 2 || rs[0].Redactions["c"] != want["c"] || rs[0].Redactions["ip"] != want["ip"] {
		t.Fatalf("redactions = %+v", rs[0].Redactions)
	}
}

func TestRedactInvalid(t *testing.T) {
	cfg := testConfig(t)
	for name, rr := range map[string][]v1.RedactRule{
		"bad name":            {{Name: "a-b", Strategy: v1.RedactRemove}},
		"tenant label":        {{Name: TenantIDLabel, Strategy: v1.RedactRemove}},
		"duplicate":           {{Name: "a", Strategy: v1.RedactRemove}, {Name: "a", Strategy: v1.RedactHash}},
		"octets on remove":    {{Name: "a", Strategy: v1.RedactRemove, Octets: i32(1)}},
		"ipv4 without octets": {{Name: "a", Strategy: v1.RedactIPv4Prefix}},
		"ipv4 octets 4":       {{Name: "a", Strategy: v1.RedactIPv4Prefix, Octets: i32(4)}},
		"ipv4 octets 0":       {{Name: "a", Strategy: v1.RedactIPv4Prefix, Octets: i32(0)}},
		"unknown strategy":    {{Name: "a", Strategy: "Mask"}},
	} {
		c := Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{
			capol("p", v1.Subjects{Users: []string{"u"}}, v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true,
				Labels: &v1.LabelFilter{Redact: rr}}),
		}})
		if r := result(t, c, "ClusterAccessPolicy", "p"); r.Ready || r.Reason != ReasonInvalidMatcher {
			t.Errorf("%s: want InvalidMatcher, got %+v", name, r)
		}
	}
}

func TestStricter(t *testing.T) {
	rm := Redaction{Strategy: v1.RedactRemove}
	ph := Redaction{Strategy: v1.RedactPlaceholder}
	h := Redaction{Strategy: v1.RedactHash}
	ip1 := Redaction{Strategy: v1.RedactIPv4Prefix, Octets: 1}
	ip3 := Redaction{Strategy: v1.RedactIPv4Prefix, Octets: 3}
	for _, tc := range []struct{ a, b, want Redaction }{
		{rm, ph, rm}, {ph, rm, rm}, {h, ph, ph}, {ph, h, ph}, {h, ip1, h}, {ip3, ip1, ip1}, {ip1, ip3, ip1}, {h, h, h},
	} {
		if got := Stricter(tc.a, tc.b); got != tc.want {
			t.Errorf("Stricter(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
