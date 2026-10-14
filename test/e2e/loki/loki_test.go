//go:build e2e

package loki

import "testing"

// TestLokiE2E runs against one Loki holding all data, one reference Loki holding only the granted streams
// of every term set (the oracle), and the real proxy handler chain in-process.
func TestLokiE2E(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() {
		slowMu.Lock()
		defer slowMu.Unlock()
		for _, s := range slow {
			t.Logf("SLOWREQ %.400s", s)
		}
	})
	t.Run("isolation", func(t *testing.T) { testIsolation(t, f) })
	t.Run("equivalence", func(t *testing.T) { testEquivalence(t, f) })
	t.Run("metadata", func(t *testing.T) { testMetadata(t, f) })
	t.Run("tenants", func(t *testing.T) { testTenants(t, f) })
	t.Run("denied-labels", func(t *testing.T) { testDenied(t, f) })
	t.Run("endpoints-and-auth", func(t *testing.T) { testEndpoints(t, f) })
	t.Run("differential", func(t *testing.T) { testDifferential(t, f, 250) })
	// Last: it pushes new lines (to the main instance for every stream, to the reference for the granted ones only).
	t.Run("tail", func(t *testing.T) { testTail(t, f) })
}
