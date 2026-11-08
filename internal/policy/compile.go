package policy

import (
	"fmt"
	"slices"
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
	// NamespaceLabels maps namespace name to its labels. It describes hub namespaces only.
	NamespaceLabels map[string]map[string]string
	// SpokeClusters and SpokePolicies are the hub objects of a multi-cluster deployment (PLAN §18.4). Without
	// the multicluster block, every SpokePolicy grants nothing.
	SpokeClusters []v1.SpokeCluster
	SpokePolicies []v1.SpokePolicy
}

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
func validName(t config.BackendType, name string) bool { return config.ValidLabelName(t, name) }

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

// compileLabels returns the denied names (including redacted ones) and the redactions of a rule.
func compileLabels(t config.BackendType, f *v1.LabelFilter) ([]string, map[string]Redaction, *compileError) {
	if f == nil {
		return nil, nil, nil
	}
	for _, n := range f.Deny {
		if !validName(t, n) {
			return nil, nil, fail(ReasonInvalidMatcher, "invalid denied label name %q for %s backend", n, t)
		}
	}
	deny := append([]string(nil), f.Deny...)
	var red map[string]Redaction
	for _, r := range f.Redact {
		if !validName(t, r.Name) {
			return nil, nil, fail(ReasonInvalidMatcher, "invalid redacted label name %q for %s backend", r.Name, t)
		}
		if _, dup := red[r.Name]; dup {
			return nil, nil, fail(ReasonInvalidMatcher, "label %q is redacted twice", r.Name)
		}
		cr := Redaction{Strategy: r.Strategy}
		switch r.Strategy {
		case v1.RedactRemove, v1.RedactPlaceholder, v1.RedactHash:
			if r.Octets != nil {
				return nil, nil, fail(ReasonInvalidMatcher, "redact %q: octets is only allowed for %s", r.Name, v1.RedactIPv4Prefix)
			}
		case v1.RedactIPv4Prefix:
			if r.Octets == nil || *r.Octets < 1 || *r.Octets > 3 {
				return nil, nil, fail(ReasonInvalidMatcher, "redact %q: %s requires octets between 1 and 3", r.Name, v1.RedactIPv4Prefix)
			}
			cr.Octets = int(*r.Octets)
		default:
			return nil, nil, fail(ReasonInvalidMatcher, "redact %q: unknown strategy %q", r.Name, r.Strategy)
		}
		if red == nil {
			red = map[string]Redaction{}
		}
		red[r.Name] = cr
		deny = append(deny, r.Name)
	}
	return sortedUnique(deny), red, nil
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
	spokes := indexSpokeClusters(in.SpokeClusters)
	for i := range in.SpokePolicies {
		p := &in.SpokePolicies[i]
		res := Result{Kind: KindSpokePolicy, Name: p.Name, Generation: p.Generation}
		rules, subj, err := compileSpokePolicy(in.Config, spokes, p)
		if err != nil {
			res.Reason, res.Message = err.reason, err.msg
		} else {
			res.Ready, res.Reason, res.Message = true, ReasonCompiled, "policy is active"
			acc = append(acc, accepted{len(c.Results), rules, subj.Users, subj.Groups})
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
	return compileClusterRules(cfg, "ClusterAccessPolicy/"+p.Name, p.Spec.Rules, origin{})
}

func compilePolicy(cfg *config.Config, p *v1.AccessPolicy, nsLabels map[string]string) ([]*CompiledRule, *compileError) {
	tenant := strings.TrimSpace(nsLabels[TenantLabel])
	return compileNamespacedRules(cfg, "AccessPolicy/"+p.Namespace+"/"+p.Name, p.Namespace, p.Spec.Rules, tenant, origin{})
}

// origin is the source of a policy (PLAN §18.3): the hub (zero value) or a trusted spoke.
type origin struct {
	// spoke is the SpokeCluster name; empty for a hub policy.
	spoke string
	// tenants is the tenant allow-list of the spoke (SpokeCluster.spec.tenants).
	tenants map[string]bool
	// version is the SpokeCluster version that tenants comes from.
	version SpokeVersion
}

// implicitCluster returns the value of the implicit cluster matcher for a rule, if the rule has one: the spoke
// name for each spoke rule, and hubCluster for a hub AccessPolicy rule under multicluster.
func implicitCluster(cfg *config.Config, o origin, namespaced bool) (string, bool) {
	switch {
	case o.spoke != "":
		return o.spoke, true
	case namespaced && cfg.Multicluster != nil:
		return cfg.Multicluster.HubCluster, true
	}
	return "", false
}

// clusterMatcher builds the implicit matcher clusterLabel="value" for a backend.
func clusterMatcher(b *config.Backend, value string) (Matcher, *compileError) {
	if value == "" {
		// clusterLabel="" also matches data without a cluster label.
		return Matcher{}, fail(ReasonInvalidClusterScope, "the cluster value for backend %q is empty", b.Name)
	}
	return compileMatcher(b.Type, v1.Matcher{Name: b.ClusterLabel, Op: v1.MatchEqual, Value: value})
}

// checkClusterScope applies the scope rule of PLAN §18.3 to one backend of a ClusterAccessPolicy rule. Without
// the multicluster block, allClusters has no effect and every rule passes.
func checkClusterScope(cfg *config.Config, o origin, ri int, b *config.Backend, r *v1.ClusterRule, ms []Matcher) *compileError {
	if cfg.Multicluster == nil {
		return nil
	}
	if o.spoke != "" {
		if r.AllClusters {
			return fail(ReasonInvalidClusterScope, "rules[%d]: allClusters is not allowed in a spoke policy", ri)
		}
		return nil // the implicit cluster matcher gives the scope
	}
	if r.Unrestricted {
		if r.AllClusters {
			return fail(ReasonInvalidClusterScope, "rules[%d]: allClusters and unrestricted are mutually exclusive", ri)
		}
		return nil
	}
	var named, bounded bool
	for _, m := range ms {
		if m.Name != b.ClusterLabel {
			continue
		}
		named = true
		if boundedClusterMatcher(m) {
			bounded = true
		}
	}
	if r.AllClusters {
		if named {
			return fail(ReasonInvalidClusterScope, "rules[%d]: allClusters must not be used with a matcher on cluster label %q of backend %q", ri, b.ClusterLabel, b.Name)
		}
		return nil
	}
	if !bounded {
		return fail(ReasonInvalidClusterScope, "rules[%d]: backend %q: add a matcher on cluster label %q that names a fixed set of clusters (Equal, or RegexMatch such as a|b), or set allClusters", ri, b.Name, b.ClusterLabel)
	}
	return nil
}

// maxScopeClusters bounds the cluster names that one RegexMatch of a hub rule may name.
const maxScopeClusters = 1024

// boundedClusterMatcher reports whether a matcher on the cluster label names a fixed, finite set of non-empty
// cluster names (PLAN §18.3). NotEqual, RegexNoMatch and an open regex such as .+ or cluster-.* also match
// each new spoke, and an empty value matches data with no cluster label, so none of them states a scope.
func boundedClusterMatcher(m Matcher) bool {
	switch m.Op {
	case v1.MatchEqual:
		return m.Value != ""
	case v1.MatchRegexMatch:
		set, ok := LiteralSet(m.Value, maxScopeClusters)
		return ok && !slices.Contains(set, "")
	}
	return false
}

func compileClusterRules(cfg *config.Config, policy string, rules []v1.ClusterRule, o origin) ([]*CompiledRule, *compileError) {
	var out []*CompiledRule
	for ri := range rules {
		r := &rules[ri]
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
			if !r.Unrestricted && len(r.Matchers) == 0 {
				// The CRD rejects this too; the compiler does not rely on CEL (PLAN §18.4).
				return nil, fail(ReasonInvalidMatcher, "rules[%d]: a rule without matchers must set unrestricted", ri)
			}
			ms, err := compileMatchers(b.Type, r.Matchers)
			if err != nil {
				return nil, err
			}
			deny, red, err := compileLabels(b.Type, r.Labels)
			if err != nil {
				return nil, err
			}
			if err := checkClusterScope(cfg, o, ri, b, r, ms); err != nil {
				return nil, err
			}
			cr := &CompiledRule{Policy: policy, Backend: bn, Origin: o.spoke, OriginVersion: o.version, Unrestricted: r.Unrestricted, Matchers: ms, DeniedLabels: deny, Redactions: red}
			if b.TenantHeader != "" {
				cr.Tenants = sortedUnique(r.Tenants)
				if err := checkTenantsAllowed(o, ri, bn, cr.Tenants); err != nil {
					return nil, err
				}
			}
			if v, ok := implicitCluster(cfg, o, false); ok {
				cm, err := clusterMatcher(b, v)
				if err != nil {
					return nil, err
				}
				// A spoke rule never compiles to Unrestricted: its widest grant is the whole spoke.
				cr.Unrestricted = false
				cr.Matchers = append([]Matcher{cm}, ms...)
			}
			out = append(out, cr)
		}
	}
	return out, nil
}

// checkTenantsAllowed applies the tenant allow-list of a spoke (PLAN §18.9). Hub policies have no allow-list.
func checkTenantsAllowed(o origin, ri int, bn string, tenants []string) *compileError {
	if o.spoke == "" {
		return nil
	}
	for _, t := range tenants {
		if !o.tenants[t] {
			return fail(ReasonTenantNotAllowed, "rules[%d]: backend %q: tenant %q is not in the tenants of SpokeCluster %q", ri, bn, t, o.spoke)
		}
	}
	return nil
}

// compileNamespacedRules compiles the rules of an AccessPolicy in namespace ns. tenant is the tenant of the
// namespace (empty if none; trimmed for hub namespaces, exact for spokes) and is used only for backends with
// a tenant header.
func compileNamespacedRules(cfg *config.Config, policy, ns string, rules []v1.Rule, tenant string, o origin) ([]*CompiledRule, *compileError) {
	var out []*CompiledRule
	for ri, r := range rules {
		for _, bn := range sortedUnique(r.Backends) {
			b, ok := cfg.Backend(bn)
			if !ok {
				return nil, fail(ReasonUnknownBackend, "rules[%d]: backend %q is not in the static config", ri, bn)
			}
			var tenants []string
			if b.TenantHeader != "" {
				if tenant == "" {
					if o.spoke != "" {
						return nil, fail(ReasonTenantNotAssigned, "rules[%d]: namespace %q has no label %s and SpokeCluster %q has no defaultTenant, but backend %q requires a tenant", ri, ns, TenantLabel, o.spoke, bn)
					}
					return nil, fail(ReasonTenantNotAssigned, "rules[%d]: namespace %q lacks label %s required by backend %q", ri, ns, TenantLabel, bn)
				}
				tenants = []string{tenant}
				if err := checkTenantsAllowed(o, ri, bn, tenants); err != nil {
					return nil, err
				}
			}
			implicit := make([]Matcher, 0, 2)
			nsm, err := compileMatcher(b.Type, v1.Matcher{Name: b.NamespaceLabel, Op: v1.MatchEqual, Value: ns})
			if err != nil {
				return nil, err
			}
			implicit = append(implicit, nsm)
			if v, ok := implicitCluster(cfg, o, true); ok {
				cm, err := clusterMatcher(b, v)
				if err != nil {
					return nil, err
				}
				implicit = append(implicit, cm)
			}
			ms, err := compileMatchers(b.Type, r.Matchers)
			if err != nil {
				return nil, err
			}
			deny, red, err := compileLabels(b.Type, r.Labels)
			if err != nil {
				return nil, err
			}
			out = append(out, &CompiledRule{
				Policy: policy, Backend: bn, Origin: o.spoke, OriginVersion: o.version, Tenants: tenants,
				Matchers: append(implicit, ms...), DeniedLabels: deny, Redactions: red,
			})
		}
	}
	return out, nil
}
