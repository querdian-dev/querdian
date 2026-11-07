// Package policy compiles access policies into an immutable, indexed form and resolves them per request.
package policy

import (
	"regexp"
	"sync"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
)

// TenantLabel is the namespace label that assigns a namespace to a backend tenant.
const TenantLabel = "policy.querdian.dev/tenant"

// TenantIDLabel is the reserved label used to bind terms to tenants under federation (PLAN §6.4).
const TenantIDLabel = config.TenantIDLabel

// Status reasons (PLAN §4.4).
const (
	ReasonCompiled                    = "Compiled"
	ReasonInvalidMatcher              = "InvalidMatcher"
	ReasonUnknownBackend              = "UnknownBackend"
	ReasonTenantRequired              = "TenantRequired"
	ReasonTenantNotAssigned           = "TenantNotAssigned"
	ReasonCrossTenantUnionUnsupported = "CrossTenantUnionUnsupported"

	// ReasonInvalidClusterScope: a rule does not state its cluster scope as PLAN §18.3 requires.
	ReasonInvalidClusterScope = "InvalidClusterScope"
	// ReasonTenantNotAllowed: a spoke policy uses a tenant that is not in SpokeCluster.spec.tenants (§18.9).
	ReasonTenantNotAllowed = "TenantNotAllowed"
	// ReasonMulticlusterDisabled: a SpokePolicy exists, but the static config has no multicluster block.
	ReasonMulticlusterDisabled = "MulticlusterDisabled"
	// ReasonSpokeClusterNotFound: no SpokeCluster has the name in SpokePolicy.spec.cluster.
	ReasonSpokeClusterNotFound = "SpokeClusterNotFound"
	// ReasonSpokeClusterInvalid: the SpokeCluster cannot be trusted (hub name, identity, duplicate).
	ReasonSpokeClusterInvalid = "SpokeClusterInvalid"
	// ReasonInvalidSpokePolicy: the SpokePolicy object itself is malformed (kind, spec, object name).
	ReasonInvalidSpokePolicy = "InvalidSpokePolicy"
)

// KindSpokePolicy is the Result kind of a spoke policy. Its Result names the hub SpokePolicy object.
const KindSpokePolicy = "SpokePolicy"

// Matcher is a compiled label constraint. Regex matchers are fully anchored.
type Matcher struct {
	Name  string
	Op    v1.MatchOp
	Value string
	// re is set for RegexMatch and RegexNoMatch, anchored as ^(?:Value)$.
	re *lazyRegexp
	// k is a canonical identity string, computed once at construction.
	k string
}

// Matches reports whether a label value (empty when the label is absent) satisfies the matcher.
func (m Matcher) Matches(value string) bool {
	switch m.Op {
	case v1.MatchEqual:
		return value == m.Value
	case v1.MatchNotEqual:
		return value != m.Value
	case v1.MatchRegexMatch:
		return m.re.get().MatchString(value)
	case v1.MatchRegexNoMatch:
		return !m.re.get().MatchString(value)
	}
	return false
}

// lazyRegexp compiles its (already validated) source on first use. Merged matchers are built on every
// request, and most are never evaluated in-process.
type lazyRegexp struct {
	src  string
	once sync.Once
	re   *regexp.Regexp
}

func (l *lazyRegexp) get() *regexp.Regexp {
	l.once.Do(func() { l.re = regexp.MustCompile(l.src) })
	return l.re
}

// CompiledRule is one rule of one policy, for exactly one backend.
type CompiledRule struct {
	// Policy identifies the source: "ClusterAccessPolicy/<name>", "AccessPolicy/<namespace>/<name>" or
	// "SpokePolicy/<object name>". The object name of a SpokePolicy is a hash of the cluster, kind, namespace
	// and name of the spoke policy (spokesync.ObjectName), so the names of different clusters cannot collide.
	Policy string
	// Backend is the backend name from the static config.
	Backend string
	// Origin is the SpokeCluster name for a rule from a spoke policy, and empty for a hub rule (PLAN §18).
	// Rules of a spoke expire with it; hub rules never expire.
	Origin string
	// Tenants is sorted and deduplicated; empty for single-tenant backends.
	Tenants      []string
	Unrestricted bool
	// Matchers are ANDed. For AccessPolicy rules the implicit namespace matcher comes first, then the
	// implicit cluster matcher (PLAN §18.3) of a spoke rule or, with multicluster, of a hub rule. For spoke
	// ClusterAccessPolicy rules the implicit cluster matcher comes first.
	Matchers []Matcher
	// DeniedLabels is sorted and deduplicated. It includes the names of Redactions.
	DeniedLabels []string
	// Redactions maps a label name to how its values are rewritten in query results; nil if none.
	Redactions map[string]Redaction
}

// Redaction is the compiled form of a redact rule.
type Redaction struct {
	Strategy v1.RedactStrategy
	Octets   int // IPv4Prefix only: leading octets kept (1..3)
}

// strictness orders redactions: a higher value hides more. Placeholder and Hash leak the same (equality
// within one response) and rank equal; ties are broken towards Placeholder by Stricter.
func (r Redaction) strictness() int {
	switch r.Strategy {
	case v1.RedactRemove:
		return 10
	case v1.RedactPlaceholder, v1.RedactHash:
		return 5
	case v1.RedactIPv4Prefix:
		return 4 - r.Octets // 1 octet hides more than 3
	}
	return 0
}

// Stricter returns the redaction that hides more of a and b.
func Stricter(a, b Redaction) Redaction {
	sa, sb := a.strictness(), b.strictness()
	switch {
	case sa > sb:
		return a
	case sb > sa:
		return b
	case a.Strategy == v1.RedactPlaceholder:
		return a
	}
	return b
}

// Result is the per-object outcome used to write status conditions.
type Result struct {
	// Kind is "ClusterAccessPolicy", "AccessPolicy" or KindSpokePolicy. For KindSpokePolicy, Name and
	// Generation are those of the hub SpokePolicy object, so the status writer can find it.
	Kind       string
	Namespace  string // empty for cluster-scoped objects
	Name       string
	Generation int64
	// Ready is the Ready condition status: true for Compiled and CrossTenantUnionUnsupported.
	Ready   bool
	Reason  string
	Message string
	// Spoke names the source object of a KindSpokePolicy, from the spec of the hub object. It is the zero
	// value for a hub policy. It is for display only; the compiler does not trust it before its checks.
	Spoke SpokeSource
}

// SpokeSource names a policy object in a spoke: the SpokeCluster name, the kind ("ClusterAccessPolicy" or
// "AccessPolicy"), the namespace (empty for a ClusterAccessPolicy) and the name.
type SpokeSource struct {
	Cluster   string
	Kind      string
	Namespace string
	Name      string
}

// Compiled is the result of compiling all policies. Invalid policies contribute no rules.
type Compiled struct {
	// ByUser and ByGroup are separate keyspaces: a user named like a group gets nothing from it.
	ByUser  map[string][]*CompiledRule
	ByGroup map[string][]*CompiledRule
	Results []Result
}

// Term is one conjunction of the effective policy: matchers ANDed, bound to a tenant.
// A term without matchers matches everything within its tenant.
type Term struct {
	Matchers []Matcher // AND
	Tenant   string    // "" for single-tenant backends
}

// Effective is the exact union of all rules granted to a request on one backend (PLAN §6.2).
type Effective struct {
	Unrestricted bool
	Tenants      []string // sorted, deduplicated; for the tenant header
	Terms        []Term   // DNF: OR of Terms (empty iff Unrestricted)
	Disjoint     bool     // all term pairs provably disjoint
	DeniedLabels []string // union of labels.deny and labels.redact names over matched rules
	// Redactions is the strictest redaction per label over matched rules; nil if none.
	Redactions map[string]Redaction
	Policies   []string // names, for tracing and logging
	// Origins are the spoke clusters whose rules gave terms, sorted and deduplicated; empty if only hub rules
	// gave terms. An open Loki tail closes when one of them expires (PLAN §18.6, rule 8).
	Origins []string
}
