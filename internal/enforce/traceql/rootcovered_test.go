package traceql

import (
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/policy"
)

func TestRootCovered(t *testing.T) {
	restricted := term(m("resource.k8s.namespace.name", v1.MatchEqual, "a"))
	tests := []struct {
		name string
		eff  policy.Effective
		want bool
	}{
		{"unrestricted", policy.Effective{Unrestricted: true}, true},
		{"empty term", eff(nil, restricted, term()), true},
		{"tenant-bound empty term", eff(nil, policy.Term{Tenant: "t1"}), false},
		{"resource attribute only", eff(nil, restricted), false},
		{"span attribute", eff(nil, term(m("span.a", v1.MatchEqual, "x"))), false},
		{"no terms", policy.Effective{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RootCovered(tt.eff); got != tt.want {
				t.Fatalf("RootCovered = %v, want %v", got, tt.want)
			}
		})
	}
}
