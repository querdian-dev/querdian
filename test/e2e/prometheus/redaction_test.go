//go:build e2e

package prometheus

import (
	"strings"
	"testing"
)

// testRedaction checks the redaction paths that only a real Prometheus exercises: query errors that print
// whole series (withheld for redacting policies) and U__-escaped names on label values (decoded by
// Prometheus 3, so they must be denied like the plain name).
func testRedaction(t *testing.T, f *fixture) {
	a := f.as("redact-1")

	t.Run("results", func(t *testing.T) {
		r := a.instant(`temperature`)
		if r.Status != 200 || strings.Contains(string(r.Body), "cust-") || !strings.Contains(string(r.Body), `"customer_id":"redacted-`) {
			t.Fatalf("status %d %s", r.Status, r.Body)
		}
	})

	// One-to-one matching on an empty label set with two series on one side: Prometheus fails with
	// "found duplicate series for the match group {} ..." and prints both series, customer_id included.
	t.Run("upstream-error", func(t *testing.T) {
		direct := f.direct.instant(`vector(1) * on() temperature{namespace="ns-a"}`)
		if direct.Status != 422 || !strings.Contains(string(direct.Body), "cust-ns-a-") {
			t.Fatalf("precondition: the backend error must list the series: %d %s", direct.Status, direct.Body)
		}
		r := a.instant(`vector(1) * on() temperature`)
		if r.Status != 422 || strings.Contains(string(r.Body), "cust-") || !strings.Contains(string(r.Body), "details withheld") {
			t.Fatalf("status %d %s", r.Status, r.Body)
		}
		// Without redaction the backend text is passed through (here: deny only).
		r = f.as("deny-1").instant(`vector(1) * on() temperature`)
		if r.Status != 422 || !strings.Contains(string(r.Body), "found duplicate series") {
			t.Fatalf("deny-1: status %d %s", r.Status, r.Body)
		}
	})

	t.Run("escaped-label-values", func(t *testing.T) {
		const escaped = "U__customer__id"
		direct := f.direct.labelValues(escaped)
		if direct.Status != 200 || !strings.Contains(string(direct.Body), "cust-ns-a-") {
			t.Fatalf("precondition: Prometheus must decode %s: %d %s", escaped, direct.Status, direct.Body)
		}
		for _, group := range []string{"redact-1", "deny-1"} {
			for _, name := range []string{"customer_id", escaped} {
				if r := f.as(group).labelValues(name); r.Status != 403 || !decode(t, r).fromProxy() {
					t.Errorf("%s: label/%s/values: %d %s", group, name, r.Status, r.Body)
				}
			}
		}
		// An escaped name that is not denied still works.
		if r := a.labelValues("U__job", `up`); r.Status != 200 || !strings.Contains(string(r.Body), "api") {
			t.Errorf("U__job: %d %s", r.Status, r.Body)
		}
	})
}
