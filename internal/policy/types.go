// Package policy compiles access policies into an immutable, indexed form and resolves them per request.
package policy

import (
	"regexp"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

// TenantLabel is the namespace label that assigns a namespace to a backend tenant.
const TenantLabel = "policy.querdian.dev/tenant"

// TenantIDLabel is the reserved label used to bind terms to tenants under federation (PLAN §6.4).
const TenantIDLabel = "__tenant_id__"

// Status reasons (PLAN §4.4).
const (
	ReasonCompiled                    = "Compiled"
	ReasonInvalidMatcher              = "InvalidMatcher"
	ReasonUnknownBackend              = "UnknownBackend"
	ReasonTenantRequired              = "TenantRequired"
	ReasonTenantNotAssigned           = "TenantNotAssigned"
	ReasonCrossTenantUnionUnsupported = "CrossTenantUnionUnsupported"
)

// Matcher is a compiled label constraint. Regex matchers are fully anchored.
type Matcher struct {
	Name  string
	Op    v1.MatchOp
	Value string
	// re is set for RegexMatch and RegexNoMatch, anchored as ^(?:Value)$.
	re *regexp.Regexp
}

// Matches reports whether a label value (empty when the label is absent) satisfies the matcher.
func (m Matcher) Matches(value string) bool {
	switch m.Op {
	case v1.MatchEqual:
		return value == m.Value
	case v1.MatchNotEqual:
		return value != m.Value
	case v1.MatchRegexMatch:
		return m.re.MatchString(value)
	case v1.MatchRegexNoMatch:
		return !m.re.MatchString(value)
	}
	return false
}

// CompiledRule is one rule of one policy, for exactly one backend.
type CompiledRule struct {
	// Policy identifies the source: "ClusterAccessPolicy/<name>" or "AccessPolicy/<namespace>/<name>".
	Policy string
	// Backend is the backend name from the static config.
	Backend string
	// Tenants is sorted and deduplicated; empty for single-tenant backends.
	Tenants      []string
	Unrestricted bool
	// Matchers are ANDed. For AccessPolicy rules the implicit namespace matcher comes first.
	Matchers []Matcher
	// DeniedLabels is sorted and deduplicated.
	DeniedLabels []string
}

// Result is the per-object outcome used to write status conditions.
type Result struct {
	Kind       string // "ClusterAccessPolicy" or "AccessPolicy"
	Namespace  string // empty for cluster-scoped objects
	Name       string
	Generation int64
	// Ready is the Ready condition status: true for Compiled and CrossTenantUnionUnsupported.
	Ready   bool
	Reason  string
	Message string
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
	DeniedLabels []string // union of labels.deny over matched rules
	Policies     []string // names, for tracing and logging
}
