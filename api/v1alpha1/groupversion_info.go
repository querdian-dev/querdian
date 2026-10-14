// Package v1alpha1 contains the policy.querdian.dev/v1alpha1 API types.
// +kubebuilder:object:generate=true
// +groupName=policy.querdian.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group/version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "policy.querdian.dev", Version: "v1alpha1"}

	// SchemeBuilder adds the types in this group-version to a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme registers the types in this group-version with a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&ClusterAccessPolicy{}, &ClusterAccessPolicyList{},
		&AccessPolicy{}, &AccessPolicyList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
