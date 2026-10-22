package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Subjects selects the users and groups a policy applies to.
// +kubebuilder:validation:XValidation:rule="size(self.groups) + size(self.users) > 0",message="at least one group or user required"
type Subjects struct {
	// Groups are OIDC group names. Matched only against the groups claim.
	// +optional
	// +kubebuilder:default={}
	Groups []string `json:"groups"`
	// Users are OIDC usernames. Matched only against the username claim.
	// +optional
	// +kubebuilder:default={}
	Users []string `json:"users"`
}

// MatchOp is a label match operator.
// +kubebuilder:validation:Enum=Equal;NotEqual;RegexMatch;RegexNoMatch
type MatchOp string

const (
	// MatchEqual is label = value.
	MatchEqual MatchOp = "Equal"
	// MatchNotEqual is label != value.
	MatchNotEqual MatchOp = "NotEqual"
	// MatchRegexMatch is label =~ value (fully anchored).
	MatchRegexMatch MatchOp = "RegexMatch"
	// MatchRegexNoMatch is label !~ value (fully anchored).
	MatchRegexNoMatch MatchOp = "RegexNoMatch"
)

// Matcher is a single label (or attribute) constraint.
type Matcher struct {
	// Name is a label (Prometheus/Loki) or attribute (Tempo, e.g. resource.service.name).
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Op is the match operator.
	Op MatchOp `json:"op"`
	// Value is the match value. Regexes are fully anchored (RE2 syntax).
	Value string `json:"value"`
}

// LabelFilter narrows a grant by hiding labels.
type LabelFilter struct {
	// Deny lists label/attribute names hidden from label endpoints; queries referencing them are rejected.
	// +optional
	Deny []string `json:"deny,omitempty"`
	// Redact lists labels/attributes whose values are rewritten in query results. A redacted name is
	// also denied: queries referencing it are rejected and label endpoints hide it.
	// +optional
	// +listType=map
	// +listMapKey=name
	Redact []RedactRule `json:"redact,omitempty"`
}

// RedactStrategy says how a redacted label value is rewritten in responses.
// +kubebuilder:validation:Enum=Remove;Placeholder;Hash;IPv4Prefix
type RedactStrategy string

const (
	// RedactRemove drops the label from every returned label set.
	RedactRemove RedactStrategy = "Remove"
	// RedactPlaceholder replaces each distinct value with redacted-<n>, numbered per response in random order.
	RedactPlaceholder RedactStrategy = "Placeholder"
	// RedactHash replaces each value with a keyed hash whose key is random per request.
	RedactHash RedactStrategy = "Hash"
	// RedactIPv4Prefix keeps the leading octets of an IPv4 address (10.1.x.x); other values become "redacted".
	RedactIPv4Prefix RedactStrategy = "IPv4Prefix"
)

// RedactRule rewrites the values of one label in query results.
// +kubebuilder:validation:XValidation:rule="(self.strategy == 'IPv4Prefix') == has(self.octets)",message="octets is required for IPv4Prefix and not allowed otherwise"
type RedactRule struct {
	// Name is a label (Prometheus/Loki) or attribute (Tempo, e.g. span.client.address).
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Strategy is how values are rewritten.
	Strategy RedactStrategy `json:"strategy"`
	// Octets is the number of leading IPv4 octets kept (IPv4Prefix only).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3
	Octets *int32 `json:"octets,omitempty"`
}

// ClusterRule is a rule of a ClusterAccessPolicy.
// +kubebuilder:validation:XValidation:rule="self.unrestricted == (size(self.matchers) == 0)",message="set either unrestricted or matchers"
type ClusterRule struct {
	// Backends are backend names from the static config.
	// +kubebuilder:validation:MinItems=1
	Backends []string `json:"backends"`
	// Tenants this rule grants access to (required for backends with a tenant header).
	// +optional
	Tenants []string `json:"tenants,omitempty"`
	// Unrestricted grants access to all data of the given tenants.
	// +optional
	// +kubebuilder:default=false
	Unrestricted bool `json:"unrestricted"`
	// Matchers are ANDed.
	// +optional
	// +kubebuilder:default={}
	Matchers []Matcher `json:"matchers"`
	// Labels optionally hides labels.
	// +optional
	Labels *LabelFilter `json:"labels,omitempty"`
}

// Rule is a rule of a namespaced AccessPolicy.
type Rule struct {
	// Backends are backend names from the static config.
	// +kubebuilder:validation:MinItems=1
	Backends []string `json:"backends"`
	// Matchers are ANDed with the implicit namespace matcher.
	// +optional
	Matchers []Matcher `json:"matchers,omitempty"`
	// Labels optionally hides labels.
	// +optional
	Labels *LabelFilter `json:"labels,omitempty"`
}

// ClusterAccessPolicySpec is the desired state of a ClusterAccessPolicy.
type ClusterAccessPolicySpec struct {
	Subjects Subjects `json:"subjects"`
	// +kubebuilder:validation:MinItems=1
	Rules []ClusterRule `json:"rules"`
}

// AccessPolicySpec is the desired state of an AccessPolicy.
type AccessPolicySpec struct {
	Subjects Subjects `json:"subjects"`
	// +kubebuilder:validation:MinItems=1
	Rules []Rule `json:"rules"`
}

// PolicyStatus is the observed state of a policy.
type PolicyStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterAccessPolicy grants cluster-wide access; managed by platform admins.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cap
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
type ClusterAccessPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ClusterAccessPolicySpec `json:"spec"`
	Status            PolicyStatus            `json:"status,omitempty"`
}

// ClusterAccessPolicyList is a list of ClusterAccessPolicy.
// +kubebuilder:object:root=true
type ClusterAccessPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterAccessPolicy `json:"items"`
}

// AccessPolicy grants access scoped to its own namespace; managed by teams.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=ap
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
type AccessPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AccessPolicySpec `json:"spec"`
	Status            PolicyStatus     `json:"status,omitempty"`
}

// AccessPolicyList is a list of AccessPolicy.
// +kubebuilder:object:root=true
type AccessPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AccessPolicy `json:"items"`
}
