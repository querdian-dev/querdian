package policy

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
)

var (
	// ErrNoGrant means no rule grants the caller anything on the backend (403).
	ErrNoGrant = errors.New("no_grant")
	// ErrCrossTenantUnion means the caller's union spans several tenants with differing terms on a backend
	// that cannot filter on __tenant_id__ (403, reason cross_tenant_union_unsupported).
	ErrCrossTenantUnion = errors.New("cross_tenant_union_unsupported")
	// ErrUnknownBackend means the backend name is not in the static config.
	ErrUnknownBackend = errors.New("unknown_backend")
	// ErrPolicySourceExpired means that rules grant the caller access on the backend, but each rule that gives
	// a term comes from a spoke that is expired (403, reason policy_source_expired, PLAN §18.6).
	ErrPolicySourceExpired = errors.New("policy_source_expired")
)

// Resolve is ResolveAt with no deadline table: each rule from a spoke is expired. Hub rules are not
// affected, so without the multicluster block the result is the same as with a table.
func (s *Snapshot) Resolve(user string, groups []string, backend string) (Effective, error) {
	return s.ResolveAt(user, groups, backend, nil, time.Time{})
}

// ResolveAt computes the exact union of all rules granted to the user and groups on a backend (PLAN §6.2,
// §6.4). Users and groups are looked up in separate keyspaces.
//
// A rule from a spoke (Origin is set) that is expired in dl at now, or that was compiled from a different
// version of its SpokeCluster than the one in dl, gives no term, but its DeniedLabels and
// Redactions stay in the result, so expiry only narrows access (PLAN §18.6, rule 5). If no term remains
// because rules expired, ResolveAt returns ErrPolicySourceExpired. It never returns an Effective with no
// terms that is not Unrestricted.
func (s *Snapshot) ResolveAt(user string, groups []string, backend string, dl *Deadlines, now time.Time) (Effective, error) {
	b, ok := s.cfg.Backend(backend)
	if !ok {
		return Effective{}, fmt.Errorf("%w: %q", ErrUnknownBackend, backend)
	}

	var rules []*CompiledRule
	if user != "" {
		rules = append(rules, s.byUser[user][backend]...)
	}
	for i, g := range groups {
		if g == "" || seenBefore(groups, i) {
			continue
		}
		rules = append(rules, s.byGroup[g][backend]...)
	}
	if len(rules) == 0 {
		return Effective{}, ErrNoGrant
	}

	var terms []Term
	var nMatchers, nBytes int
	var denied, policies, origins []string
	var versions map[string]SpokeVersion
	expired := false
	var redactions map[string]Redaction
	seen := make(map[*CompiledRule]bool, len(rules))
	for _, r := range rules {
		if seen[r] {
			continue
		}
		seen[r] = true
		denied = append(denied, r.DeniedLabels...)
		for name, red := range r.Redactions {
			if redactions == nil {
				redactions = map[string]Redaction{}
			}
			if cur, ok := redactions[name]; ok {
				red = Stricter(cur, red)
			}
			redactions[name] = red
		}
		policies = append(policies, r.Policy)
		if r.Origin != "" {
			// A spoke rule must never be unrestricted (PLAN §18.3). If the compiler gives one, it grants
			// nothing here.
			if r.Unrestricted {
				continue
			}
			if dl.ExpiredFor(r.Origin, r.OriginVersion, now) {
				expired = true
				continue
			}
			origins = append(origins, r.Origin)
			if versions == nil {
				versions = map[string]SpokeVersion{}
			}
			versions[r.Origin] = r.OriginVersion
		}
		// Normalise does more than linear work. Check its input size first, so that the work and the memory
		// of a request stay bounded for any snapshot.
		n := max(len(r.Tenants), 1)
		nMatchers += n * len(r.Matchers)
		for _, m := range r.Matchers {
			nBytes += n * len(m.k)
		}
		if len(terms)+n > MaxResolveTerms || nMatchers > MaxResolveMatchers || nBytes > MaxResolveBytes {
			return Effective{}, fmt.Errorf("%w: the matched rules have more than %d terms, %d matchers or %d bytes of matchers",
				ErrTooComplex, MaxResolveTerms, MaxResolveMatchers, MaxResolveBytes)
		}
		if len(r.Tenants) == 0 {
			terms = append(terms, Term{Matchers: r.Matchers})
			continue
		}
		for _, t := range r.Tenants {
			terms = append(terms, Term{Matchers: r.Matchers, Tenant: t})
		}
	}

	// The ErrNoGrant check above sees the rules before the expiry filter, and Normalise accepts an empty
	// term list. This check makes sure that no rule with expired terms becomes an empty grant.
	if len(terms) == 0 {
		if expired {
			return Effective{}, ErrPolicySourceExpired
		}
		return Effective{}, ErrNoGrant
	}
	eff, err := Normalise(terms, s.cfg.Limits)
	if err != nil {
		return Effective{}, err
	}
	if !eff.Unrestricted && len(eff.Terms) == 0 {
		return Effective{}, ErrNoGrant
	}
	if len(eff.Tenants) > 1 {
		if eff, err = bindTenants(eff, b); err != nil {
			return Effective{}, err
		}
		// The binding adds a matcher to each term.
		if err := checkResultWeight(eff.Terms); err != nil {
			return Effective{}, err
		}
	} else {
		// Zero or one tenant: the tenant header carries it, terms need no binding.
		for i := range eff.Terms {
			eff.Terms[i].Tenant = ""
		}
	}
	eff.DeniedLabels = sortedUnique(denied)
	eff.Redactions = redactions
	eff.Policies = sortedUnique(policies)
	eff.Origins = sortedUnique(origins)
	eff.OriginVersions = versions
	eff.RegexLimits = RegexLimits{Query: s.cfg.Limits.MaxQueryRegexWeight, Request: s.cfg.Limits.MaxRequestRegexWeight}
	return eff, nil
}

func seenBefore(groups []string, i int) bool {
	for _, g := range groups[:i] {
		if g == groups[i] {
			return true
		}
	}
	return false
}

// bindTenants handles unions across several tenants (PLAN §6.4). The tenant header already carries all
// tenants (t1|t2|...); this decides whether terms must be tied to a tenant.
func bindTenants(eff Effective, b *config.Backend) (Effective, error) {
	if eff.Unrestricted {
		return eff, nil // every tenant is unrestricted
	}
	// Identical terms in every tenant: no binding needed.
	perTenant := map[string][]Term{}
	for _, t := range eff.Terms {
		perTenant[t.Tenant] = append(perTenant[t.Tenant], t)
	}
	if identicalAcrossTenants(perTenant) {
		first := perTenant[eff.Tenants[0]]
		terms := make([]Term, len(first))
		for i, t := range first {
			terms[i] = Term{Matchers: t.Matchers}
		}
		eff.Terms = terms
		eff.Disjoint = allDisjoint(terms)
		return eff, nil
	}
	if b.TenantFederation != config.FederationTenantIDLabel {
		return Effective{}, ErrCrossTenantUnion
	}
	terms := make([]Term, len(eff.Terms))
	for i, t := range eff.Terms {
		bind, err := newMatcher(TenantIDLabel, v1.MatchEqual, t.Tenant)
		if err != nil {
			return Effective{}, err
		}
		ms := append(append([]Matcher(nil), t.Matchers...), bind)
		terms[i] = Term{Matchers: sortMatchers(ms), Tenant: t.Tenant}
	}
	eff.Terms = terms
	eff.Disjoint = allDisjoint(terms) // different tenants are always disjoint
	return eff, nil
}

func identicalAcrossTenants(perTenant map[string][]Term) bool {
	var first string
	for i, tenant := range sortedTenantKeys(perTenant) {
		keys := make([]string, len(perTenant[tenant]))
		for j, t := range perTenant[tenant] {
			keys[j] = Term{Matchers: t.Matchers}.key()
		}
		sort.Strings(keys)
		joined := strings.Join(keys, "\n")
		if i == 0 {
			first = joined
		} else if joined != first {
			return false
		}
	}
	return true
}

func sortedTenantKeys(m map[string][]Term) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
