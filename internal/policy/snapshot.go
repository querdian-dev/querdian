package policy

import (
	"maps"
	"sort"

	"github.com/querdian-dev/querdian/internal/config"
)

// Snapshot is an immutable, indexed view of all compiled policies. It is safe for concurrent use.
type Snapshot struct {
	// Generation increases with every rebuild; used for tracing and metrics.
	Generation int64
	cfg        *config.Config
	// byUser and byGroup are separate keyspaces, each indexed by backend name.
	byUser  map[string]map[string][]*CompiledRule
	byGroup map[string]map[string][]*CompiledRule
	results []Result
}

// NewSnapshot freezes a Compiled result. The Compiled value must not be modified afterwards.
func NewSnapshot(c *Compiled, cfg *config.Config, generation int64) *Snapshot {
	return &Snapshot{
		Generation: generation,
		cfg:        cfg,
		byUser:     indexByBackend(c.ByUser),
		byGroup:    indexByBackend(c.ByGroup),
		results:    append([]Result(nil), c.Results...),
	}
}

func indexByBackend(in map[string][]*CompiledRule) map[string]map[string][]*CompiledRule {
	out := make(map[string]map[string][]*CompiledRule, len(in))
	for k, rules := range in {
		m := map[string][]*CompiledRule{}
		for _, r := range rules {
			m[r.Backend] = append(m[r.Backend], r)
		}
		out[k] = m
	}
	return out
}

// Results returns the per-object compile results, sorted for stable output.
func (s *Snapshot) Results() []Result {
	out := append([]Result(nil), s.results...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return out
}

// RuleView is a read-only copy of one compiled rule, for display.
type RuleView struct {
	Backend string
	// Origin is the SpokeCluster name for a rule from a spoke policy, and empty for a hub rule.
	Origin string
	// OriginVersion is the SpokeCluster version that the rule was compiled from. Give it to
	// Deadlines.ExpiredFor, as ResolveAt does, so that the display agrees with enforcement.
	OriginVersion SpokeVersion
	Tenants       []string
	Unrestricted  bool
	Matchers      []Matcher
	DeniedLabels  []string // includes the names of Redactions
	Redactions    map[string]Redaction
}

// View is a read-only description of one policy as compiled into the snapshot.
type View struct {
	// ID is "ClusterAccessPolicy/<name>", "AccessPolicy/<namespace>/<name>" or "SpokePolicy/<object name>".
	// The object name of a SpokePolicy is a hash; Result.Spoke names its source object.
	ID     string
	Result Result
	Users  []string
	Groups []string
	Rules  []RuleView
}

// Policies lists every policy object with its compile result, subjects and compiled rules, sorted by ID.
// Policies that failed to compile are listed without rules. The result is a copy.
func (s *Snapshot) Policies() []View {
	byID := map[string]*View{}
	for _, r := range s.results {
		id := policyID(r)
		byID[id] = &View{ID: id, Result: r}
	}
	seenRule := map[*CompiledRule]bool{}
	collect := func(subjects map[string]map[string][]*CompiledRule, group bool) {
		for subject, backends := range subjects {
			for _, rules := range backends {
				for _, cr := range rules {
					pv := byID[cr.Policy]
					if pv == nil {
						pv = &View{ID: cr.Policy}
						byID[cr.Policy] = pv
					}
					if !seenRule[cr] {
						seenRule[cr] = true
						pv.Rules = append(pv.Rules, RuleView{
							Backend:       cr.Backend,
							Origin:        cr.Origin,
							OriginVersion: cr.OriginVersion,
							Tenants:       append([]string(nil), cr.Tenants...),
							Unrestricted:  cr.Unrestricted,
							Matchers:      append([]Matcher(nil), cr.Matchers...),
							DeniedLabels:  append([]string(nil), cr.DeniedLabels...),
							Redactions:    maps.Clone(cr.Redactions),
						})
					}
					if group {
						pv.Groups = appendUnique(pv.Groups, subject)
					} else {
						pv.Users = appendUnique(pv.Users, subject)
					}
				}
			}
		}
	}
	collect(s.byUser, false)
	collect(s.byGroup, true)

	out := make([]View, 0, len(byID))
	for _, pv := range byID {
		sort.Strings(pv.Users)
		sort.Strings(pv.Groups)
		sort.SliceStable(pv.Rules, func(i, j int) bool { return pv.Rules[i].Backend < pv.Rules[j].Backend })
		out = append(out, *pv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Ref names a policy that has rules for a request, with the origin of those rules.
type Ref struct {
	ID string
	// Origin is the SpokeCluster name for a spoke policy, and empty for a hub policy.
	Origin string
	// OriginVersion is the SpokeCluster version that the rules of the policy were compiled from. All rules
	// of one policy in one snapshot come from the same version.
	OriginVersion SpokeVersion
}

// PoliciesFor returns the policies with rules for the user and groups on a backend, sorted by ID. It finds
// the same rules as ResolveAt, but it does not check the deadlines, so the result also contains the
// policies of expired spokes. It is for display only; use ResolveAt to decide about access.
func (s *Snapshot) PoliciesFor(user string, groups []string, backend string) []Ref {
	var rules []*CompiledRule
	if user != "" {
		rules = append(rules, s.byUser[user][backend]...)
	}
	for _, g := range groups {
		if g != "" {
			rules = append(rules, s.byGroup[g][backend]...)
		}
	}
	seen := map[string]bool{}
	var out []Ref
	for _, r := range rules {
		if !seen[r.Policy] {
			seen[r.Policy] = true
			out = append(out, Ref{ID: r.Policy, Origin: r.Origin, OriginVersion: r.OriginVersion})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Subjects returns the sorted users and groups named by any compiled rule.
func (s *Snapshot) Subjects() (users, groups []string) {
	for u := range s.byUser {
		users = append(users, u)
	}
	for g := range s.byGroup {
		groups = append(groups, g)
	}
	sort.Strings(users)
	sort.Strings(groups)
	return users, groups
}

func policyID(r Result) string {
	if r.Namespace == "" {
		return r.Kind + "/" + r.Name
	}
	return r.Kind + "/" + r.Namespace + "/" + r.Name
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
