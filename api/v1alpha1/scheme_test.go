package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestAddToScheme(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, o := range []runtime.Object{&ClusterAccessPolicy{}, &AccessPolicy{}, &ClusterAccessPolicyList{}, &AccessPolicyList{}} {
		if gvks, _, err := s.ObjectKinds(o); err != nil || len(gvks) != 1 || gvks[0].Group != "policy.querdian.dev" {
			t.Fatalf("%T not registered correctly: %v %v", o, gvks, err)
		}
	}
}
