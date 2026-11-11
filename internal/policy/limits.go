package policy

import (
	"fmt"
	"unicode/utf8"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/regexcost"
)

// Size limits of a policy spec. The kubebuilder markers in api/v1alpha1/types.go have the same values, and
// TestCRDLimitsAgree makes sure that they stay equal. The API server does not validate stored objects again
// when the CRD changes, and the compiler does not rely on the CRD. Thus the compiler applies these limits
// too: a policy over a limit grants nothing (ReasonPolicyTooLarge). The receiver applies them to spoke
// policies. Lengths count characters (runes), as the API server does.
const (
	// MaxSubjects is the maximum number of items in subjects.users, and also in subjects.groups.
	MaxSubjects = 256
	// MaxSubjectLength is the maximum length of a user or group name.
	MaxSubjectLength = 1024
	// MaxRules is the maximum number of rules of a policy.
	MaxRules = 64
	// MaxBackends is the maximum number of backends of a rule.
	MaxBackends = 16
	// MaxTenants is the maximum number of tenants of a ClusterAccessPolicy rule.
	MaxTenants = 64
	// MaxMatchers is the maximum number of matchers of a rule.
	MaxMatchers = 64
	// MaxLabelNames is the maximum number of items in labels.deny, and also in labels.redact.
	MaxLabelNames = 64
	// MaxNameLength is the maximum length of a backend name, a tenant, a matcher name and a denied or
	// redacted label name.
	MaxNameLength = 256
	// MaxValueLength is the maximum length of a matcher value.
	MaxValueLength = 16384
)

// Limits of the regexes of a policy spec. The CRD cannot express them. The cost of a regex compile does not
// grow with the length of the regex but with its weight (regexcost.Weight): the instructions of the
// compiled program (a{1000} has 7 bytes and 1000 instructions), the rune ranges of its character classes
// (\pL has about 700) and the cost of case folding. A compile allocates at most about
// regexcost.MaxBytesPerUnit bytes for each unit. The compiler compiles each regex matcher of a policy to
// validate it.
const (
	// MaxRegexWeight is the maximum weight (Matcher.Weight) of one RegexMatch or RegexNoMatch matcher.
	MaxRegexWeight = 16384
	// MaxPolicyRegexWeight is the maximum sum of the weights of the regex matchers of one policy spec. Each
	// snapshot rebuild compiles them, once for each backend of the rule.
	MaxPolicyRegexWeight = 32768
)

// Limits of the work for one request (PLAN §6.3). ResolveAt counts the terms, matchers and bytes of the
// matched rules before Normalise, Normalise counts its steps, and Normalise and ResolveAt limit the weight of
// the result. A request over one of these limits gets ErrTooComplex (403 policy_too_complex). The first
// three limits bound the work of ResolveAt. The enforcers compile the regexes of the result: the result
// limit bounds the work of one compile, and enforce.CheckRequest bounds the copies of the result in a
// rewritten query. A policy can be valid and still give a request over a limit, for example with
// MaxRules x MaxTenants terms of MaxMatchers matchers each on one backend.
const (
	// MaxResolveTerms is the maximum number of terms before simplification.
	MaxResolveTerms = 4096
	// MaxResolveMatchers is the maximum number of matchers, summed over all terms before simplification.
	MaxResolveMatchers = 32768
	// MaxResolveBytes is the maximum length in bytes of the matcher keys, summed over all terms before
	// simplification. Each pass of Normalise copies the matcher keys of each term into a term key.
	MaxResolveBytes = 1 << 20
	// MaxNormaliseWork is the maximum number of steps of Normalise. A pass costs one step for each term, and
	// for each matcher one step plus one step for each byte of its key. A comparison of two terms for
	// subsumption costs one step plus one step for each matcher of the longer term.
	MaxNormaliseWork = 1 << 22
	// MaxResultWeight is the maximum sum of the weights (Matcher.Weight) of the matchers of the simplified
	// policy, over all its terms. A merge joins many values into one regex, so the result can be much larger
	// than each value of the input.
	MaxResultWeight = 32768
)

// CheckClusterSpecSize returns an error that names the first size limit that s is over, or nil.
func CheckClusterSpecSize(s *v1.ClusterAccessPolicySpec) error {
	if err := checkSubjectsSize(&s.Subjects); err != nil {
		return err
	}
	if err := checkCount("rules", len(s.Rules), MaxRules); err != nil {
		return err
	}
	regexWeight := 0
	for i := range s.Rules {
		r := &s.Rules[i]
		p := fmt.Sprintf("rules[%d]", i)
		if err := checkStrings(p+".backends", r.Backends, MaxBackends, MaxNameLength); err != nil {
			return err
		}
		if err := checkStrings(p+".tenants", r.Tenants, MaxTenants, MaxNameLength); err != nil {
			return err
		}
		if err := checkMatchersSize(p, r.Matchers, &regexWeight); err != nil {
			return err
		}
		if err := checkLabelsSize(p, r.Labels); err != nil {
			return err
		}
	}
	return nil
}

// CheckSpecSize returns an error that names the first size limit that s is over, or nil.
func CheckSpecSize(s *v1.AccessPolicySpec) error {
	if err := checkSubjectsSize(&s.Subjects); err != nil {
		return err
	}
	if err := checkCount("rules", len(s.Rules), MaxRules); err != nil {
		return err
	}
	regexWeight := 0
	for i := range s.Rules {
		r := &s.Rules[i]
		p := fmt.Sprintf("rules[%d]", i)
		if err := checkStrings(p+".backends", r.Backends, MaxBackends, MaxNameLength); err != nil {
			return err
		}
		if err := checkMatchersSize(p, r.Matchers, &regexWeight); err != nil {
			return err
		}
		if err := checkLabelsSize(p, r.Labels); err != nil {
			return err
		}
	}
	return nil
}

func checkSubjectsSize(s *v1.Subjects) error {
	if err := checkStrings("subjects.groups", s.Groups, MaxSubjects, MaxSubjectLength); err != nil {
		return err
	}
	return checkStrings("subjects.users", s.Users, MaxSubjects, MaxSubjectLength)
}

// checkMatchersSize checks the matchers of one rule. total is the sum of the regex weights of the spec so
// far. The weight of a regex comes from a parse without a compile, after the length check.
func checkMatchersSize(p string, ms []v1.Matcher, total *int) error {
	if err := checkCount(p+".matchers", len(ms), MaxMatchers); err != nil {
		return err
	}
	for i, m := range ms {
		if err := checkLength(fmt.Sprintf("%s.matchers[%d].name", p, i), m.Name, MaxNameLength); err != nil {
			return err
		}
		field := fmt.Sprintf("%s.matchers[%d].value", p, i)
		if err := checkLength(field, m.Value, MaxValueLength); err != nil {
			return err
		}
		if m.Op != v1.MatchRegexMatch && m.Op != v1.MatchRegexNoMatch {
			continue
		}
		// The byte limit does not depend on the work counter of the regex parser.
		if len(m.Value) > regexcost.MaxRegexBytes {
			return fmt.Errorf("%s: a regex value is longer than %d bytes", field, regexcost.MaxRegexBytes)
		}
		w, err := RegexWeight(m.Value)
		if err != nil {
			continue // the compiler rejects it as InvalidMatcher, and the receiver leaves it to the compiler
		}
		if w > MaxRegexWeight {
			return fmt.Errorf("%s: the compiled regex has %d instructions, more than the limit of %d", field, w, MaxRegexWeight)
		}
		if *total += w; *total > MaxPolicyRegexWeight {
			return fmt.Errorf("%s: the compiled regexes of the policy have %d instructions, more than the limit of %d",
				field, *total, MaxPolicyRegexWeight)
		}
	}
	return nil
}

func checkLabelsSize(p string, f *v1.LabelFilter) error {
	if f == nil {
		return nil
	}
	if err := checkStrings(p+".labels.deny", f.Deny, MaxLabelNames, MaxNameLength); err != nil {
		return err
	}
	if err := checkCount(p+".labels.redact", len(f.Redact), MaxLabelNames); err != nil {
		return err
	}
	for i, r := range f.Redact {
		if err := checkLength(fmt.Sprintf("%s.labels.redact[%d].name", p, i), r.Name, MaxNameLength); err != nil {
			return err
		}
	}
	return nil
}

// checkStrings checks the number of items of a list and the length of each item.
func checkStrings(field string, items []string, maxItems, maxLength int) error {
	if err := checkCount(field, len(items), maxItems); err != nil {
		return err
	}
	for i, s := range items {
		if err := checkLength(fmt.Sprintf("%s[%d]", field, i), s, maxLength); err != nil {
			return err
		}
	}
	return nil
}

func checkCount(field string, n, limit int) error {
	if n > limit {
		return fmt.Errorf("%s has %d items, more than the limit of %d", field, n, limit)
	}
	return nil
}

func checkLength(field, s string, limit int) error {
	// A string of limit bytes or less has limit runes or less, so most strings need no count.
	if len(s) > limit && utf8.RuneCountInString(s) > limit {
		return fmt.Errorf("%s is longer than the limit of %d characters", field, limit)
	}
	return nil
}
