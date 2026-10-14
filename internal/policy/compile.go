package policy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
)

// Input is everything Compile needs; it performs no I/O.
type Input struct {
	Config          *config.Config
	ClusterPolicies []v1.ClusterAccessPolicy
	Policies        []v1.AccessPolicy
	// NamespaceLabels maps namespace name to its labels.
	NamespaceLabels map[string]map[string]string
}

var (
	promLabelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	// tempoAttrName is a deliberately conservative subset that needs no quoting when rendered.
	tempoAttrName = regexp.MustCompile(`^(resource|span)\.[a-zA-Z0-9_][a-zA-Z0-9_.\-/]*$`)
)

// compileError carries the status reason for a failing policy.
type compileError struct {
	reason string
	msg    string
}

func (e *compileError) Error() string { return e.reason + ": " + e.msg }

func fail(reason, format string, a ...any) *compileError {
	return &compileError{reason: reason, msg: fmt.Sprintf(format, a...)}
}

// validName checks a label (Prometheus/Loki) or attribute (Tempo) name for the backend type.
func validName(t config.BackendType, name string) bool {
	switch t {
	case config.BackendTempo:
		return tempoAttrName.MatchString(name)
	default:
		return name != TenantIDLabel && promLabelName.MatchString(name)
	}
}

func compileMatcher(t config.BackendType, m v1.Matcher) (Matcher, *compileError) {
	if !validName(t, m.Name) {
		return Matcher{}, fail(ReasonInvalidMatcher, "invalid label name %q for %s backend", m.Name, t)
	}
	switch m.Op {
	case v1.MatchEqual, v1.MatchNotEqual, v1.MatchRegexMatch, v1.MatchRegexNoMatch:
	default:
		return Matcher{}, fail(ReasonInvalidMatcher, "matcher %q: unknown operator %q", m.Name, m.Op)
	}
	cm, err := newMatcher(m.Name, m.Op, m.Value)
	if err != nil {
		return Matcher{}, fail(ReasonInvalidMatcher, "matcher %q: regex does not compile: %v", m.Name, err)
	}
	return cm, nil
}

func compileMatchers(t config.BackendType, in []v1.Matcher) ([]Matcher, *compileError) {
	out := make([]Matcher, 0, len(in))
	for _, m := range in {
		cm, err := compileMatcher(t, m)
		if err != nil {
			return nil, err
		}
		out = append(out, cm)
	}
	return out, nil
}

func compileDeny(t config.BackendType, f *v1.LabelFilter) ([]string, *compileError) {
	if f == nil {
		return nil, nil
	}
	for _, n := range f.Deny {
		if !validName(t, n) {
			return nil, fail(ReasonInvalidMatcher, "invalid denied label name %q for %s backend", n, t)
		}
	}
	return sortedUnique(f.Deny), nil
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	j := 1
	for i := 1; i < len(out); i++ {
		if out[i] != out[i-1] {
			out[j] = out[i]
			j++
		}
	}
	return out[:j]
}

// Compile turns CRD objects, namespace labels and the static config into indexed rules and per-object
// status results (PLAN §6.1). A policy with any invalid rule is rejected as a whole and grants nothing.
func Compile(in Input) *Compiled {
	c := &Compiled{ByUser: map[string][]*CompiledRule{}, ByGroup: map[string][]*CompiledRule{}}
	var acc []accepted

	for i := range in.ClusterPolicies {
		p := &in.ClusterPolicies[i]
		res := Result{Kind: "ClusterAccessPolicy", Name: p.Name, Generation: p.Generation}
		rules, err := compileClusterPolicy(in.Config, p)
		if err != nil {
			res.Reason, res.Message = err.reason, err.msg
		} else {
			res.Ready, res.Reason, res.Message = true, ReasonCompiled, "policy is active"
			acc = append(acc, accepted{len(c.Results), rules, p.Spec.Subjects.Users, p.Spec.Subjects.Groups})
		}
		c.Results = append(c.Results, res)
	}
	for i := range in.Policies {
		p := &in.Policies[i]
		res := Result{Kind: "AccessPolicy", Namespace: p.Namespace, Name: p.Name, Generation: p.Generation}
		rules, err := compilePolicy(in.Config, p, in.NamespaceLabels[p.Namespace])
		if err != nil {
			res.Reason, res.Message = err.reason, err.msg
		} else {
			res.Ready, res.Reason, res.Message = true, ReasonCompiled, "policy is active"
			acc = append(acc, accepted{len(c.Results), rules, p.Spec.Subjects.Users, p.Spec.Subjects.Groups})
		}
		c.Results = append(c.Results, res)
	}

	for _, a := range acc {
		for _, u := range sortedUnique(a.users) {
			c.ByUser[u] = append(c.ByUser[u], a.rules...)
		}
		for _, g := range sortedUnique(a.groups) {
			c.ByGroup[g] = append(c.ByGroup[g], a.rules...)
		}
	}
	c.markCrossTenant(in.Config, acc)
	return c
}

// accepted is a successfully compiled policy awaiting indexing.
type accepted struct {
	result int // index into Compiled.Results
	rules  []*CompiledRule
	users  []string
	groups []string
}

// markCrossTenant flags active policies that contribute to a subject whose grants span several tenants of
// a backend without __tenant_id__ support with differing terms; such unions are rejected at request time
// (PLAN §6.4). The policy stays active, so Ready remains true.
func (c *Compiled) markCrossTenant(cfg *config.Config, acc []accepted) {
	affected := map[string]map[string]bool{} // policy -> backends
	check := func(rules []*CompiledRule) {
		byBackend := map[string][]*CompiledRule{}
		for _, r := range rules {
			byBackend[r.Backend] = append(byBackend[r.Backend], r)
		}
		for bn, rs := range byBackend {
			b, _ := cfg.Backend(bn)
			if b == nil || b.TenantHeader == "" || b.TenantFederation != config.FederationNone {
				continue
			}
			perTenant := map[string]map[string]bool{}
			for _, r := range rs {
				for _, t := range r.Tenants {
					if perTenant[t] == nil {
						perTenant[t] = map[string]bool{}
					}
					perTenant[t][r.canonical()] = true
				}
			}
			if len(perTenant) < 2 || allEqual(perTenant) {
				continue
			}
			for _, r := range rs {
				if affected[r.Policy] == nil {
					affected[r.Policy] = map[string]bool{}
				}
				affected[r.Policy][bn] = true
			}
		}
	}
	for _, rs := range c.ByUser {
		check(rs)
	}
	for _, rs := range c.ByGroup {
		check(rs)
	}
	for _, a := range acc {
		if len(a.rules) == 0 {
			continue
		}
		bs := affected[a.rules[0].Policy]
		if len(bs) == 0 {
			continue
		}
		names := make([]string, 0, len(bs))
		for n := range bs {
			names = append(names, n)
		}
		sort.Strings(names)
		r := &c.Results[a.result]
		r.Reason = ReasonCrossTenantUnionUnsupported
		r.Message = "policy is active, but unions across tenants with differing rules will be rejected on backends without tenant federation: " + strings.Join(names, ", ")
	}
}

// canonical renders the rule's term independent of matcher order and tenant.
func (r *CompiledRule) canonical() string {
	if r.Unrestricted {
		return "*"
	}
	parts := make([]string, len(r.Matchers))
	for i, m := range r.Matchers {
		parts[i] = m.k
	}
	sort.Strings(parts)
	return strings.Join(parts, " & ")
}

func allEqual(perTenant map[string]map[string]bool) bool {
	var first map[string]bool
	for _, set := range perTenant {
		if first == nil {
			first = set
			continue
		}
		if len(set) != len(first) {
			return false
		}
		for k := range set {
			if !first[k] {
				return false
			}
		}
	}
	return true
}

func compileClusterPolicy(cfg *config.Config, p *v1.ClusterAccessPolicy) ([]*CompiledRule, *compileError) {
	policy := "ClusterAccessPolicy/" + p.Name
	var out []*CompiledRule
	for ri, r := range p.Spec.Rules {
		for _, bn := range sortedUnique(r.Backends) {
			b, ok := cfg.Backend(bn)
			if !ok {
				return nil, fail(ReasonUnknownBackend, "rules[%d]: backend %q is not in the static config", ri, bn)
			}
			if b.TenantHeader != "" && len(r.Tenants) == 0 {
				return nil, fail(ReasonTenantRequired, "rules[%d]: backend %q uses tenant header %q, so tenants are required", ri, bn, b.TenantHeader)
			}
			if r.Unrestricted && len(r.Matchers) > 0 {
				return nil, fail(ReasonInvalidMatcher, "rules[%d]: unrestricted rules must not have matchers", ri)
			}
			ms, err := compileMatchers(b.Type, r.Matchers)
			if err != nil {
				return nil, err
			}
			deny, err := compileDeny(b.Type, r.Labels)
			if err != nil {
				return nil, err
			}
			cr := &CompiledRule{Policy: policy, Backend: bn, Unrestricted: r.Unrestricted, Matchers: ms, DeniedLabels: deny}
			if b.TenantHeader != "" {
				cr.Tenants = sortedUnique(r.Tenants)
			}
			out = append(out, cr)
		}
	}
	return out, nil
}

func compilePolicy(cfg *config.Config, p *v1.AccessPolicy, nsLabels map[string]string) ([]*CompiledRule, *compileError) {
	policy := "AccessPolicy/" + p.Namespace + "/" + p.Name
	var out []*CompiledRule
	for ri, r := range p.Spec.Rules {
		for _, bn := range sortedUnique(r.Backends) {
			b, ok := cfg.Backend(bn)
			if !ok {
				return nil, fail(ReasonUnknownBackend, "rules[%d]: backend %q is not in the static config", ri, bn)
			}
			var tenants []string
			if b.TenantHeader != "" {
				t := strings.TrimSpace(nsLabels[TenantLabel])
				if t == "" {
					return nil, fail(ReasonTenantNotAssigned, "rules[%d]: namespace %q lacks label %s required by backend %q", ri, p.Namespace, TenantLabel, bn)
				}
				tenants = []string{t}
			}
			ns, err := compileMatcher(b.Type, v1.Matcher{Name: b.NamespaceLabel, Op: v1.MatchEqual, Value: p.Namespace})
			if err != nil {
				return nil, err
			}
			ms, err := compileMatchers(b.Type, r.Matchers)
			if err != nil {
				return nil, err
			}
			deny, err := compileDeny(b.Type, r.Labels)
			if err != nil {
				return nil, err
			}
			out = append(out, &CompiledRule{
				Policy: policy, Backend: bn, Tenants: tenants,
				Matchers: append([]Matcher{ns}, ms...), DeniedLabels: deny,
			})
		}
	}
	return out, nil
}
