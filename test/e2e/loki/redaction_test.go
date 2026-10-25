//go:build e2e

package loki

import (
	"strings"
	"testing"
)

// testRedaction checks redaction against a real Loki: stream labels in results, and query errors that print
// whole series ("pipeline error ... for series"), which are withheld for redacting policies.
func testRedaction(t *testing.T, f *fixture) {
	a := f.asUser("u-redact", "redact-pod")
	const pod = "web-ns-a-0"

	t.Run("results", func(t *testing.T) {
		r := a.queryRange(`{app="web"}`, nil)
		if r.Status != 200 || strings.Contains(string(r.Body), pod) || !strings.Contains(string(r.Body), `"pod":"redacted-`) {
			t.Fatalf("status %d %.400s", r.Status, r.Body)
		}
	})

	// web lines are logfmt: the json parser fails on them, and Loki rejects the metric query with a
	// pipeline error that lists the series labels, pod included.
	t.Run("upstream-error", func(t *testing.T) {
		direct := f.direct("t1").instant(`count_over_time({` + nsLabel + `="ns-a", app="web"} | json [1h])`)
		if direct.Status != 400 || !strings.Contains(string(direct.Body), pod) {
			t.Fatalf("precondition: the backend error must list the series: %d %.400s", direct.Status, direct.Body)
		}
		r := a.instant(`count_over_time({app="web"} | json [1h])`)
		if r.Status != 400 || strings.Contains(string(r.Body), pod) || !strings.Contains(string(r.Body), "details withheld") {
			t.Fatalf("status %d %.400s", r.Status, r.Body)
		}
		// Without redaction the backend text is passed through.
		r = f.asUser("u-team-a", "team-a").instant(`count_over_time({app="web"} | json [1h])`)
		if r.Status != 400 || !strings.Contains(string(r.Body), "pipeline error") {
			t.Fatalf("team-a: status %d %.400s", r.Status, r.Body)
		}
	})
}
