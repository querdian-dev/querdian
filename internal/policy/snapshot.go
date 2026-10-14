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
