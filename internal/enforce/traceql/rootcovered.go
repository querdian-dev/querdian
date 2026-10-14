package traceql

import "github.com/querdian-dev/querdian/internal/policy"

// RootCovered reports whether the root span of every trace returned by a policy-injected search is known
// to satisfy the effective policy. It is the input rootCovered of fanout.NewTempoSearchMerger.
//
// It is true only when the policy admits every span: eff.Unrestricted, or at least one term without
// matchers and without tenant binding. In every other case it is false and trace-level search metadata
// (root names, service statistics, trace extent) is redacted.
//
// Why no weaker proof is attempted. The injected DNF only guarantees that the spans a search lists satisfy
// the policy; the root span of the same trace is a different span. A policy on resource attributes
// (resource.k8s.namespace.name = "a") does not help: the spans of one trace routinely come from several
// services and namespaces, so the matched child and the root can carry different resource attributes. Span
// attributes differ per span by definition. Verifying the root would need a second query per trace that
// applies the policy to the root span only, and the search API reports no root-span attributes to check
// against. Redaction is therefore the only sound default.
func RootCovered(eff policy.Effective) bool {
	if eff.Unrestricted {
		return true
	}
	for _, t := range eff.Terms {
		if len(t.Matchers) == 0 && t.Tenant == "" {
			return true
		}
	}
	return false
}
