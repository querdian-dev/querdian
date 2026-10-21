package policy

import (
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
	Backend      string
	Tenants      []string
	Unrestricted bool
	Matchers     []Matcher
	DeniedLabels []string
}

// View is a read-only description of one policy as compiled into the snapshot.
type View struct {
	// ID is "ClusterAccessPolicy/<name>" or "AccessPolicy/<namespace>/<name>".
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
							Backend:      cr.Backend,
							Tenants:      append([]string(nil), cr.Tenants...),
							Unrestricted: cr.Unrestricted,
							Matchers:     append([]Matcher(nil), cr.Matchers...),
							DeniedLabels: append([]string(nil), cr.DeniedLabels...),
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
