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
// +kubebuilder:validation:XValidation:rule="!(self.allClusters && self.unrestricted)",message="allClusters and unrestricted are mutually exclusive"
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
	// AllClusters states that the rule applies to every cluster, including spokes added later (PLAN §18.3).
	// It has no effect without the multicluster config block. The compiler rejects it on spoke policies
	// (reason InvalidClusterScope); admission does not.
	// +optional
	// +kubebuilder:default=false
	AllClusters bool `json:"allClusters"`
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

// SpokeClusterSpec is the desired state of a SpokeCluster. Only hub admins write it (PLAN §18.4).
// +kubebuilder:validation:XValidation:rule="!has(self.defaultTenant) || self.defaultTenant in self.tenants",message="defaultTenant must be in tenants"
// +kubebuilder:validation:XValidation:rule="!has(self.maxStaleness) || duration(self.maxStaleness) > duration('0s')",message="maxStaleness must be positive"
type SpokeClusterSpec struct {
	// Identity is the URI SAN of the agent cert of this spoke, in canonical form. Its last path segment must
	// equal the SpokeCluster name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Identity string `json:"identity"`
	// Tenants is the allow-list of tenants that policies of this spoke may use. Each item is a Kubernetes
	// label value, because a spoke AccessPolicy takes its tenant from a namespace label.
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	// +kubebuilder:default={}
	Tenants []string `json:"tenants"`
	// DefaultTenant is used for spoke AccessPolicy objects whose namespace has no tenant label.
	// +optional
	DefaultTenant string `json:"defaultTenant,omitempty"`
	// MaxStaleness overrides multicluster.maxStaleness of the static config for this spoke.
	// +optional
	MaxStaleness *metav1.Duration `json:"maxStaleness,omitempty"`
	// Suspended stops the grants of this spoke and makes the receiver reject its syncs.
	// +optional
	// +kubebuilder:default=false
	Suspended bool `json:"suspended"`
}

// SpokeClusterStatus is written by the receiver after each valid sync.
type SpokeClusterStatus struct {
	// LastSyncTime is the hub time of the last valid sync.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
	// ObservedGeneration is metadata.generation at the last valid sync.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SpokeCluster registers a spoke cluster on the hub; its name is the cluster label value of the spoke.
// The name is also a Kubernetes label value on SpokePolicy objects, hence the length limit.
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="name must be at most 63 characters"
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=spoke
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspended`
// +kubebuilder:printcolumn:name="LastSync",type=date,JSONPath=`.status.lastSyncTime`
type SpokeCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SpokeClusterSpec   `json:"spec"`
	Status            SpokeClusterStatus `json:"status,omitempty"`
}

// SpokeClusterList is a list of SpokeCluster.
// +kubebuilder:object:root=true
type SpokeClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpokeCluster `json:"items"`
}

// SpokePolicyKind is the kind of the policy object in the spoke.
// +kubebuilder:validation:Enum=ClusterAccessPolicy;AccessPolicy
type SpokePolicyKind string

// Spoke policy kinds.
const (
	SpokeClusterAccessPolicy SpokePolicyKind = "ClusterAccessPolicy"
	SpokeAccessPolicy        SpokePolicyKind = "AccessPolicy"
)

// SpokePolicySource is one policy object of a spoke, as the agent sends it. Exactly one of the two specs is
// set, matching Kind.
//
// The state digest (spokesync.Digest) treats absent, null, empty and zero values alike. Every field added
// here or in the nested specs must therefore mean the same when it is absent and when it is empty or zero,
// and must have no non-zero default.
// +kubebuilder:validation:XValidation:rule="self.kind == 'ClusterAccessPolicy' ? (has(self.clusterAccessPolicy) && !has(self.accessPolicy) && !has(self.namespace) && !has(self.tenantLabel)) : (has(self.accessPolicy) && !has(self.clusterAccessPolicy) && has(self.namespace))",message="kind, namespace and spec do not agree"
type SpokePolicySource struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="kind is immutable"
	Kind SpokePolicyKind `json:"kind"`
	// Namespace of an AccessPolicy; absent for a ClusterAccessPolicy.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="namespace is immutable"
	Namespace string `json:"namespace,omitempty"`
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="name is immutable"
	Name string `json:"name"`
	// Generation is metadata.generation of the object in the spoke.
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
	// TenantLabel is the value of the policy.querdian.dev/tenant label of the namespace (AccessPolicy only).
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
	TenantLabel string `json:"tenantLabel,omitempty"`
	// +optional
	ClusterAccessPolicy *ClusterAccessPolicySpec `json:"clusterAccessPolicy,omitempty"`
	// +optional
	AccessPolicy *AccessPolicySpec `json:"accessPolicy,omitempty"`
}

// SpokePolicySpec is a spoke policy stored on the hub. Only the receiver writes it (PLAN §18.4). The object
// name is a hash of cluster, kind, namespace and name, so these fields are immutable. The kind rule of
// SpokePolicySource ties the presence of namespace to the kind, so namespace cannot be added or removed.
type SpokePolicySpec struct {
	// Cluster is the SpokeCluster name; the receiver takes it from the agent cert.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="cluster is immutable"
	Cluster           string `json:"cluster"`
	SpokePolicySource `json:",inline"`
}

// SpokePolicyStatus is written by the Querdian leader.
type SpokePolicyStatus struct {
	// ObservedGeneration is metadata.generation of this object at the last compile.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// SourceGeneration is spec.generation (the spoke generation) at the last compile.
	// +optional
	SourceGeneration int64 `json:"sourceGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SpokePolicy is the hub copy of one policy object of a spoke.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=sp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.cluster`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.namespace`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type SpokePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SpokePolicySpec   `json:"spec"`
	Status            SpokePolicyStatus `json:"status,omitempty"`
}

// SpokePolicyList is a list of SpokePolicy.
// +kubebuilder:object:root=true
type SpokePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpokePolicy `json:"items"`
}
