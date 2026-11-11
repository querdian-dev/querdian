package receiver

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

// validDigest reports whether d has the form that spokesync.Digest makes.
func validDigest(d string) bool {
	h, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(h) != 64 || strings.ToLower(h) != h {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

// validatePolicy checks one spoke policy against the schema and the CEL rules of the SpokePolicy CRD. The
// receiver does this in Go too, so that a change of the CRD or of the API server cannot make it accept a
// policy that it must reject (PLAN §18.5, "Unknown input").
func validatePolicy(p *v1.SpokePolicySource) error {
	if p.Name == "" {
		return fmt.Errorf("a policy has no name")
	}
	if errs := validation.IsDNS1123Subdomain(p.Name); len(errs) > 0 {
		return fmt.Errorf("policy name %q is not valid: %s", p.Name, strings.Join(errs, "; "))
	}
	id := string(p.Kind) + " " + p.Namespace + "/" + p.Name
	if p.Generation < 1 {
		return fmt.Errorf("%s: generation must be 1 or more", id)
	}
	switch p.Kind {
	case v1.SpokeClusterAccessPolicy:
		if p.Namespace != "" || p.ClusterAccessPolicy == nil || p.AccessPolicy != nil || p.TenantLabel != "" {
			return fmt.Errorf("%s: kind, namespace and spec do not agree", id)
		}
		if err := validateClusterSpec(p.ClusterAccessPolicy); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
	case v1.SpokeAccessPolicy:
		if p.Namespace == "" || p.AccessPolicy == nil || p.ClusterAccessPolicy != nil {
			return fmt.Errorf("%s: kind, namespace and spec do not agree", id)
		}
		if errs := validation.IsDNS1123Label(p.Namespace); len(errs) > 0 {
			return fmt.Errorf("%s: namespace is not valid: %s", id, strings.Join(errs, "; "))
		}
		if errs := validation.IsValidLabelValue(p.TenantLabel); len(errs) > 0 {
			return fmt.Errorf("%s: tenant label is not valid: %s", id, strings.Join(errs, "; "))
		}
		if err := validateSpec(p.AccessPolicy); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
	default:
		return fmt.Errorf("unknown kind %q", p.Kind)
	}
	return nil
}

func validateSubjects(s *v1.Subjects) error {
	if len(s.Groups)+len(s.Users) == 0 {
		return fmt.Errorf("at least one group or user required")
	}
	return nil
}

func validateClusterSpec(s *v1.ClusterAccessPolicySpec) error {
	// The size limits come first, so that the other checks see a bounded spec.
	if err := policy.CheckClusterSpecSize(s); err != nil {
		return err
	}
	if err := validateSubjects(&s.Subjects); err != nil {
		return err
	}
	if len(s.Rules) == 0 {
		return fmt.Errorf("at least one rule required")
	}
	for i := range s.Rules {
		r := &s.Rules[i]
		if len(r.Backends) == 0 {
			return fmt.Errorf("rules[%d]: at least one backend required", i)
		}
		if r.Unrestricted != (len(r.Matchers) == 0) {
			return fmt.Errorf("rules[%d]: set either unrestricted or matchers", i)
		}
		if r.AllClusters && r.Unrestricted {
			return fmt.Errorf("rules[%d]: allClusters and unrestricted are mutually exclusive", i)
		}
		if err := validateMatchers(r.Matchers); err != nil {
			return fmt.Errorf("rules[%d]: %w", i, err)
		}
		if err := validateLabels(r.Labels); err != nil {
			return fmt.Errorf("rules[%d]: %w", i, err)
		}
	}
	return nil
}

func validateSpec(s *v1.AccessPolicySpec) error {
	// The size limits come first, so that the other checks see a bounded spec.
	if err := policy.CheckSpecSize(s); err != nil {
		return err
	}
	if err := validateSubjects(&s.Subjects); err != nil {
		return err
	}
	if len(s.Rules) == 0 {
		return fmt.Errorf("at least one rule required")
	}
	for i := range s.Rules {
		r := &s.Rules[i]
		if len(r.Backends) == 0 {
			return fmt.Errorf("rules[%d]: at least one backend required", i)
		}
		if err := validateMatchers(r.Matchers); err != nil {
			return fmt.Errorf("rules[%d]: %w", i, err)
		}
		if err := validateLabels(r.Labels); err != nil {
			return fmt.Errorf("rules[%d]: %w", i, err)
		}
	}
	return nil
}

func validateMatchers(ms []v1.Matcher) error {
	for i, m := range ms {
		if m.Name == "" {
			return fmt.Errorf("matchers[%d]: name is empty", i)
		}
		switch m.Op {
		case v1.MatchEqual, v1.MatchNotEqual, v1.MatchRegexMatch, v1.MatchRegexNoMatch:
		default:
			return fmt.Errorf("matchers[%d]: unknown operator %q", i, m.Op)
		}
	}
	return nil
}

func validateLabels(f *v1.LabelFilter) error {
	if f == nil {
		return nil
	}
	seen := map[string]bool{}
	for i, r := range f.Redact {
		if r.Name == "" {
			return fmt.Errorf("labels.redact[%d]: name is empty", i)
		}
		if seen[r.Name] {
			return fmt.Errorf("labels.redact[%d]: label %q is redacted twice", i, r.Name)
		}
		seen[r.Name] = true
		switch r.Strategy {
		case v1.RedactRemove, v1.RedactPlaceholder, v1.RedactHash, v1.RedactIPv4Prefix:
		default:
			return fmt.Errorf("labels.redact[%d]: unknown strategy %q", i, r.Strategy)
		}
		if (r.Strategy == v1.RedactIPv4Prefix) != (r.Octets != nil) {
			return fmt.Errorf("labels.redact[%d]: octets is required for IPv4Prefix and not allowed otherwise", i)
		}
		if r.Octets != nil && (*r.Octets < 1 || *r.Octets > 3) {
			return fmt.Errorf("labels.redact[%d]: octets must be between 1 and 3", i)
		}
	}
	return nil
}

// validateRequest checks a decoded request. It returns a rejection with the HTTP status.
func validateRequest(req *spokesync.Request, maxPolicies int) error {
	if !validDigest(req.Digest) {
		return reject(http.StatusBadRequest, "the digest is not valid")
	}
	if !req.Full {
		if len(req.Policies) > 0 {
			return reject(http.StatusBadRequest, "a heartbeat must not carry policies")
		}
		return nil
	}
	if len(req.Policies) > maxPolicies {
		return reject(http.StatusRequestEntityTooLarge, "too many policies")
	}
	for i := range req.Policies {
		if err := validatePolicy(&req.Policies[i]); err != nil {
			return reject(http.StatusBadRequest, "invalid policy: "+err.Error())
		}
	}
	d, err := spokesync.Digest(req.Policies)
	if err != nil {
		return reject(http.StatusBadRequest, "invalid state: "+err.Error())
	}
	if d != req.Digest {
		return reject(http.StatusBadRequest, "the digest does not agree with the policies")
	}
	return nil
}
