package enforce

import (
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/regexcost"
)

// Limits of the regexes that one request compiles. The weights are regexcost.Weight, as in
// policy.Matcher.Weight: the instructions of the compiled program, the rune ranges of its character classes
// and the cost of case folding. A compile allocates at most about regexcost.MaxBytesPerUnit bytes for each
// unit (regexp and the Prometheus matcher together).
const (
	// MaxRequestWeight is the default of limits.maxRequestRegexWeight: the maximum regex weight that the
	// enforcers compile for one request, as RequestWeight counts it: the client regexes and the policy, and
	// their copies in the rewritten query, which the enforcers parse again. At the largest policy
	// (policy.MaxResultWeight), a query can have 1 selector; at a policy of a tenth of that weight, about
	// 20.
	MaxRequestWeight = config.DefaultMaxRequestRegexWeight
	// MaxQueryRegexWeight is the default of limits.maxQueryRegexWeight: the maximum sum of the weights of
	// the regexes of a client query (all its selectors, all match[] values). The enforcers check it before
	// they parse the query.
	MaxQueryRegexWeight = config.DefaultMaxQueryRegexWeight
)

// RequestWeight returns an upper bound of the regex weight that the enforcers compile for a query with the
// given number of selectors (or spanset filters, at least 1) and client regex weight, under eff:
//
//	(2 x terms + 1) x client weight + (2 x selectors + 1) x policy weight
//
// The client regexes are compiled in the first parse, and again in each copy for a term: once in the
// parse of the rewritten query, and once in the anchor check of LogQL. The policy is compiled once, and
// twice in each selector of the rewritten query.
func RequestWeight(selectors, clientWeight int, eff policy.Effective) int {
	terms := max(len(eff.Terms), 1)
	return (2*terms+1)*clientWeight + (2*max(selectors, 1)+1)*policy.TermsWeight(eff.Terms)
}

// QueryRegexLimit returns the limit of the client regexes of a request under eff.
func QueryRegexLimit(eff policy.Effective) int {
	if eff.RegexLimits.Query > 0 {
		return eff.RegexLimits.Query
	}
	return MaxQueryRegexWeight
}

// RequestRegexLimit returns the limit of RequestWeight for a request under eff.
func RequestRegexLimit(eff policy.Effective) int {
	if eff.RegexLimits.Request > 0 {
		return eff.RegexLimits.Request
	}
	return MaxRequestWeight
}

// CheckRequest returns ErrTooComplex when RequestWeight is over RequestRegexLimit. Call it only when the
// policy is injected; for an unrestricted policy it returns nil.
func CheckRequest(selectors, clientWeight int, eff policy.Effective) error {
	if eff.Unrestricted {
		return nil
	}
	if w := RequestWeight(selectors, clientWeight, eff); w > RequestRegexLimit(eff) {
		return Errorf(ErrTooComplex, "the query (%d selectors, client regex weight %d) and the policy (%d terms, weight %d) need a regex weight of %d, more than the limit of %d; split the query or make the policy smaller",
			max(selectors, 1), clientWeight, len(eff.Terms), policy.TermsWeight(eff.Terms), w, RequestRegexLimit(eff))
	}
	return nil
}

// QueryRegexError is the error for a client query whose regexes are over limit.
func QueryRegexError(limit int) error {
	return Errorf(ErrTooComplex, "the regular expressions of the query are over the weight limit of %d; use shorter regexes, fewer or narrower character classes or fewer repetitions",
		limit)
}

// QueryRegexBytesError is the error for a client query with a regex over a byte limit.
func QueryRegexBytesError() error {
	return Errorf(ErrTooComplex, "a regular expression of the query is longer than %d bytes, or all are longer than %d bytes together",
		regexcost.MaxRegexBytes, regexcost.MaxQueryRegexBytes)
}
