//go:build e2e

package tempo

import (
	"testing"
)

// testRootOracle probes the root-metadata oracle of docs/research/traceql.md section 8: trace-level
// intrinsics (rootName, rootServiceName, trace:rootService, trace:rootName, traceDuration, trace:duration)
// are evaluated by Tempo against the whole trace, so a filter on them next to the policy tells a user
// whether the trace they can partly see has a given root, although the root fields are redacted from the
// response. Trace 11 has its root "POST /internal/secret-op" of service "billing-gateway" in ns-b; the ns-a
// user is granted only its child span "lookup".
//
// A restricted user whose root is not covered must be rejected by the proxy on every endpoint that
// evaluates a query. Unrestricted users (root covered) keep the intrinsics.
func testRootOracle(t *testing.T, f *fixture) {
	trace11 := trimID(traceID(11))
	sc := scenarioByName("ns-a")
	a := f.as(sc)

	// 1. The raw oracle on Tempo itself: the intrinsic filter distinguishes the traces.
	d := f.direct(sc)
	if got := decodeSearch(t, d.search(`{ resource.k8s.namespace.name = "ns-a" && rootServiceName = "billing-gateway" }`)); len(got.Traces) != 1 {
		t.Fatalf("test premise: Tempo should match trace 11 through rootServiceName, got %d traces", len(got.Traces))
	}

	type probe struct{ yes, no string }
	probes := []probe{
		{`{ rootServiceName = "billing-gateway" }`, `{ rootServiceName = "no-such-service" }`},
		{`{ trace:rootService = "billing-gateway" }`, `{ trace:rootService = "no-such-service" }`},
		{`{ rootServiceName =~ "billing.*" }`, `{ rootServiceName =~ "zzz.*" }`},
		{`{ rootName = "POST /internal/secret-op" }`, `{ rootName = "nothing" }`},
		{`{ trace:rootName =~ "POST /internal/.*" }`, `{ trace:rootName =~ "GET /x.*" }`},
		{`{ traceDuration > 800ms }`, `{ traceDuration > 5s }`},
		{`{ trace:duration > 800ms && status = ok }`, `{ trace:duration > 5s && status = ok }`},
		{`{ span.team = "alpha" && rootServiceName = "billing-gateway" }`, `{ span.team = "alpha" && rootServiceName = "x" }`},
		{`{ name = "lookup" } && { rootServiceName = "billing-gateway" }`, `{ name = "lookup" } && { rootServiceName = "x" }`},
		{`{ } | select(rootServiceName)`, `{ } | select(rootServiceName)`},
		{`{ } | by(trace:rootService) | count() > 0`, `{ } | by(trace:rootService) | count() > 0`},
	}
	for _, p := range probes {
		ry, rn := a.search(p.yes), a.search(p.no)
		if ry.Status == 200 && rn.Status == 200 {
			tracesYes, tracesNo := searchSig(decodeSearch(t, ry)), searchSig(decodeSearch(t, rn))
			_, y := tracesYes[trace11]
			_, n := tracesNo[trace11]
			if y && !n {
				t.Errorf("ROOT ORACLE LEAK: %s returns trace 11 while %s does not: the user learns the root of a trace they only partly see", p.yes, p.no)
			}
		}
		for _, q := range []string{p.yes, p.no} {
			for _, call := range []struct {
				name string
				do   func(string) (int, bool)
			}{
				{"search", func(q string) (int, bool) { r := a.search(q); _, ok := proxyError(r); return r.Status, ok }},
				{"metrics", func(q string) (int, bool) {
					r := a.metricsRange(q + " | rate()")
					_, ok := proxyError(r)
					return r.Status, ok
				}},
				{"tags", func(q string) (int, bool) { r := a.tags(q); _, ok := proxyError(r); return r.Status, ok }},
				{"tag-values", func(q string) (int, bool) {
					r := a.tagValues("resource.service.name", q)
					_, ok := proxyError(r)
					return r.Status, ok
				}},
			} {
				if call.name == "metrics" && (q == `{ } | select(rootServiceName)` || q == `{ } | by(trace:rootService) | count() > 0`) {
					continue
				}
				status, fromProxy := call.do(q)
				if status/100 != 4 || !fromProxy {
					t.Errorf("%s %s: status %d (proxy-originated=%v), want a proxy rejection (4xx)", call.name, q, status, fromProxy)
				}
			}
		}
	}

	// 2. Metrics grouped by a root intrinsic would list root names per group.
	for _, q := range []string{
		`{ } | rate() by (rootServiceName)`,
		`{ } | count_over_time() by (trace:rootName)`,
		`{ rootName = "cron" } | count_over_time()`,
		`{ } | compare({ status = error })`, // adds rootServiceName as a label dimension
	} {
		if r := a.metricsRange(q); r.Status/100 != 4 {
			t.Errorf("metrics %s: status %d: %.200s", q, r.Status, r.Body)
		}
		if r := a.metricsInstant(q); r.Status/100 != 4 {
			t.Errorf("instant metrics %s: status %d: %.200s", q, r.Status, r.Body)
		}
	}

	// 3. The tag-values endpoint lists the values of a trace-level intrinsic of every matching trace.
	for _, name := range []string{"rootServiceName", "rootName", "trace:rootService", "trace:rootName", "traceDuration", "trace:duration"} {
		r := a.tagValues(name, "")
		if r.Status/100 != 4 {
			t.Errorf("tag values %s: status %d: %.300s", name, r.Status, r.Body)
		}
		if _, ok := proxyError(r); r.Status/100 == 4 && !ok {
			t.Errorf("tag values %s: rejection did not come from the proxy: %.200s", name, r.Body)
		}
	}

	// 4. Users whose root is covered (unrestricted) keep the feature.
	admin := f.as(scenarioByName("admin"))
	if r := admin.search(`{ rootServiceName = "billing-gateway" }`); r.Status != 200 || len(decodeSearch(t, r).Traces) != 1 {
		t.Errorf("admin rootServiceName filter: %d %.200s", r.Status, r.Body)
	}
	if r := admin.tagValues("rootServiceName", ""); r.Status != 200 {
		t.Errorf("admin tag values rootServiceName: %d", r.Status)
	}

	// 5. Related trace-level intrinsics that do not reveal root data stay usable.
	for _, q := range []string{`{ trace:id != "x" }`, `{ span:name = "lookup" }`, `{ status = error }`} {
		if r := a.search(q); r.Status != 200 {
			t.Errorf("%s was rejected: %d %.200s", q, r.Status, r.Body)
		}
	}
}
