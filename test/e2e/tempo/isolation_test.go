//go:build e2e

package tempo

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/querdian-dev/querdian/test/e2e/framework"
)

// forbiddenTokens returns the string values that identify data the scenario must never see: service
// names, span names, namespaces and attribute values that occur on ungranted spans but on no granted span.
func (f *fixture) forbiddenTokens(sc scenario) []string {
	allowed := map[string]bool{}
	denied := map[string]bool{}
	collect := func(set map[string]bool, s spanDef) {
		set[s.name], set[s.service], set[s.ns] = true, true, true
		for _, v := range s.spanAttrs {
			set[v] = true
		}
		for _, v := range s.resAttrs {
			set[v] = true
		}
	}
	for _, s := range f.spans {
		if sc.grants(s) {
			collect(allowed, s)
		} else {
			collect(denied, s)
		}
	}
	var out []string
	for k := range denied {
		if !allowed[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// assertNoLeak fails when the body mentions (as a complete JSON string) a value that only ungranted spans carry.
func (f *fixture) assertNoLeak(t testing.TB, sc scenario, what string, body []byte) {
	t.Helper()
	s := string(body)
	for _, tok := range f.forbiddenTokens(sc) {
		if tok != "" && strings.Contains(s, `"`+tok+`"`) {
			t.Errorf("%s: response leaks %q (carried only by spans outside the policy of %s)", what, tok, sc.name)
		}
	}
}

func testSeed(t *testing.T, f *fixture) {
	t1 := decodeSearch(t, f.directT["t1"].search("{}"))
	t2 := decodeSearch(t, f.directT["t2"].search("{}"))
	if len(t1.Traces) != 32 || len(t2.Traces) != 4 {
		t.Fatalf("seeded t1=%d t2=%d traces, want 32 and 4", len(t1.Traces), len(t2.Traces))
	}
	// A cross-namespace trace has its root outside the namespace of the child.
	for _, tr := range t1.Traces {
		if trimID(tr.TraceID) == trimID(traceID(11)) {
			if tr.RootServiceName == nil || *tr.RootServiceName != "billing-gateway" {
				t.Fatalf("trace 11 root = %v", tr.RootServiceName)
			}
		}
	}
	if r := f.directT["t1"].trace(traceID(500), 1, false); r.Status != 404 {
		t.Fatalf("t2 trace visible to tenant t1: %d", r.Status)
	}
}

// expectedSearch is the model's answer to `{}`: per trace the IDs of the granted spans.
func (f *fixture) expectedSearch(sc scenario) map[string][]string {
	out := map[string][]string{}
	for _, s := range f.spans {
		if sc.grants(s) {
			out[trimID(s.trace)] = append(out[trimID(s.trace)], s.id)
		}
	}
	for k := range out {
		slices.Sort(out[k])
	}
	return out
}

func testIsolation(t *testing.T, f *fixture) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			a := f.as(sc)
			r := a.search("{}")
			s := decodeSearch(t, r)
			want := f.expectedSearch(sc)
			if len(s.Traces) != len(want) {
				t.Errorf("%d traces, want %d", len(s.Traces), len(want))
			}
			for _, tr := range s.Traces {
				ids := spanIDsOf(tr.sets())
				if !slices.Equal(ids, want[trimID(tr.TraceID)]) {
					t.Errorf("trace %s spans %v, want %v", tr.TraceID, ids, want[trimID(tr.TraceID)])
				}
			}
			if !sc.unrestricted {
				f.assertNoLeak(t, sc, "search {}", r.Body)
				checkRedaction(t, s)
			} else {
				// Unrestricted: nothing is redacted, the answer equals Tempo's.
				d := decodeSearch(t, f.direct(sc).search("{}"))
				if diff := diffSig(searchSig(d), searchSig(s)); diff != "" {
					t.Errorf("admin differs from direct:\n  %s", diff)
				}
				for _, tr := range s.Traces {
					if tr.RootServiceName == nil || tr.ServiceStats == nil {
						t.Errorf("unrestricted trace %s lost root metadata", tr.TraceID)
					}
				}
			}

			// A query that names data outside the policy yields nothing from it.
			for _, q := range []string{
				`{ resource.service.name = "billing-gateway" }`,
				`{ resource.service.name = "t2-secret-svc" }`,
				`{ resource.k8s.namespace.name = "ns-d" }`,
				`{ name = "audit" }`,
				`{ status = error } | count() > 0`,
				`{ resource.k8s.namespace.name =~ "ns-.*" } | select(resource.k8s.namespace.name, resource.service.name)`,
			} {
				r := a.search(q)
				if r.Status != 200 {
					t.Errorf("%s: %d %s", q, r.Status, r.Body)
					continue
				}
				for _, tr := range decodeSearch(t, r).Traces {
					for _, id := range spanIDsOf(tr.sets()) {
						if sp := f.byID[id]; !sc.grants(sp) {
							t.Errorf("%s: span %s (ns %s, svc %s, tenant %s) is outside the policy", q, id, sp.ns, sp.service, sp.tenant)
						}
					}
				}
				if !sc.unrestricted {
					f.assertNoLeak(t, sc, q, r.Body)
				}
			}

			// Metrics: grouped by namespace only the granted namespaces appear, and totals equal the model.
			mr := a.metricsRange(`{} | count_over_time() by (resource.k8s.namespace.name)`)
			if mr.Status != 200 {
				t.Fatalf("metrics: %d %s", mr.Status, mr.Body)
			}
			got := metricsTotals(t, mr)
			wantNS := map[string]float64{}
			for _, sp := range f.spans {
				if sc.grants(sp) {
					wantNS[`resource.k8s.namespace.name="`+sp.ns+`"`]++
				}
			}
			if !totalsEqual(wantNS, got) {
				t.Errorf("metrics by namespace = %v, want %v", got, wantNS)
			}
			mi := a.metricsInstant(`{ status = error } | count_over_time()`)
			if mi.Status != 200 {
				t.Errorf("instant metrics: %d %s", mi.Status, mi.Body)
			}
		})
	}
}

// metricsTotals sums the samples per series label set.
func metricsTotals(t testing.TB, r *framework.Response) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	var m metricsOut
	r.JSON(t, &m)
	for _, s := range m.Series {
		var ls []string
		for _, l := range s.Labels {
			if l.Key == "__name__" {
				continue
			}
			var v struct {
				StringValue string `json:"stringValue"`
			}
			_ = jsonUnmarshal(l.Value, &v)
			ls = append(ls, l.Key+`="`+v.StringValue+`"`)
		}
		slices.Sort(ls)
		for _, p := range s.Samples {
			if p.Value != nil {
				out[strings.Join(ls, ",")] += *p.Value
			}
		}
	}
	return out
}

func totalsEqual(want, got map[string]float64) bool {
	if len(want) != len(got) {
		return false
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || g < w-1e-6 || g > w+1e-6 {
			return false
		}
	}
	return true
}

// checkRedaction verifies PLAN §7.4 / T4.4 for a restricted user: root fields and service statistics are
// gone, and trace extent/start are recomputed from the returned (granted) spans only.
func checkRedaction(t *testing.T, s searchOut) {
	t.Helper()
	for _, tr := range s.Traces {
		if tr.RootServiceName != nil || tr.RootTraceName != nil || tr.ServiceStats != nil {
			t.Errorf("trace %s: root metadata not redacted (rootServiceName=%v rootTraceName=%v serviceStats=%v)",
				tr.TraceID, tr.RootServiceName, tr.RootTraceName, tr.ServiceStats)
		}
		var minStart, maxEnd uint64
		for _, ss := range tr.sets() {
			for _, sp := range ss.Spans {
				st, _ := strconv.ParseUint(sp.StartTimeUnixNano, 10, 64)
				du, _ := strconv.ParseUint(sp.DurationNanos, 10, 64)
				if minStart == 0 || st < minStart {
					minStart = st
				}
				maxEnd = max(maxEnd, st+du)
			}
		}
		if tr.StartTimeUnixNano != strconv.FormatUint(minStart, 10) {
			t.Errorf("trace %s: startTimeUnixNano %s, want earliest granted span start %d", tr.TraceID, tr.StartTimeUnixNano, minStart)
		}
		wantMs := int64((maxEnd - minStart + 999_999) / 1_000_000)
		if tr.DurationMs == nil || *tr.DurationMs != wantMs {
			t.Errorf("trace %s: durationMs %v, want %d (extent of granted spans)", tr.TraceID, tr.DurationMs, wantMs)
		}
	}
}

// --- trace by ID ----------------------------------------------------------------------------------------

func testTraceByID(t *testing.T, f *fixture) {
	traces := make([]string, 0, len(f.byTrace))
	for id := range f.byTrace {
		traces = append(traces, id)
	}
	sort.Strings(traces)
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			a := f.as(sc)
			for _, tid := range traces {
				var want []string
				for _, s := range f.byTrace[tid] {
					if sc.grants(s) {
						want = append(want, s.id)
					}
				}
				slices.Sort(want)
				for _, v := range []int{1, 2} {
					for _, pb := range []bool{false, true} {
						name := fmt.Sprintf("%s v%d proto=%v", tid, v, pb)
						r := a.trace(tid, v, pb)
						if len(want) == 0 {
							if sc.unrestricted && v == 2 { // Tempo itself answers 200 with an empty trace
								continue
							}
							if r.Status != 404 {
								t.Errorf("%s: status %d, want 404 (nothing granted): %.200s", name, r.Status, r.Body)
							}
							continue
						}
						if r.Status != 200 {
							t.Errorf("%s: status %d: %.200s", name, r.Status, r.Body)
							continue
						}
						got := decodeTrace(t, r, v, pb)
						ids := make([]string, 0, len(got))
						for id, sp := range got {
							ids = append(ids, id)
							for _, d := range sc.deny {
								scope, n, _ := strings.Cut(d, ".")
								if sp.Attrs[scope+"/"+n] {
									t.Errorf("%s: span %s still carries denied attribute %s", name, id, d)
								}
							}
						}
						slices.Sort(ids)
						if !slices.Equal(ids, want) {
							t.Errorf("%s: spans %v, want %v", name, ids, want)
						}
						ct := r.Header.Get("Content-Type")
						if pb && !strings.Contains(ct, "protobuf") {
							t.Errorf("%s: content type %q, want protobuf", name, ct)
						}
						if !pb && !strings.Contains(ct, "json") {
							t.Errorf("%s: content type %q, want json", name, ct)
						}
						if !sc.unrestricted && !pb {
							f.assertNoLeak(t, sc, name, r.Body)
						}
					}
				}
			}
		})
	}

	t.Run("params", func(t *testing.T) {
		a := f.as(scenarioByName("ns-a"))
		id := traceID(1)
		if r := a.c.Get("/tempo/api/traces/"+id, url.Values{"q": {"{}"}}); r.Status != 400 {
			t.Errorf("client q on trace by id: %d", r.Status)
		}
		if r := a.c.Get("/tempo/api/v2/traces/"+id, url.Values{"q": {"{}"}}); r.Status != 400 {
			t.Errorf("client q on v2 trace by id: %d", r.Status)
		}
		if r := a.c.Get("/tempo/api/traces/"+id, url.Values{"start": {"1"}, "end": {"9999999999"}}); r.Status != 200 {
			t.Errorf("start/end on trace by id: %d %s", r.Status, r.Body)
		}
		if r := a.c.Get("/tempo/api/traces/not-a-trace-id", nil); r.Status/100 != 4 {
			t.Errorf("malformed id: %d", r.Status)
		}
	})
}

// --- tags -------------------------------------------------------------------------------------------------

func testTags(t *testing.T, f *fixture) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			a := f.as(sc)
			want := f.grantedAttrNames(sc)
			for _, d := range sc.deny {
				scope, n, _ := strings.Cut(d, ".")
				delete(want, scope+"/"+n)
			}
			for _, q := range []string{"", `{ span.http.method = "POST" }`, `{ status = error }`} {
				r := a.tags(q)
				got := tagNames(t, r)
				for k := range got {
					if strings.HasPrefix(k, "intrinsic/") {
						continue
					}
					if !want[k] {
						t.Errorf("tags(%q): lists %s which no granted span carries (or which is denied)", q, k)
					}
				}
				if q == "" && !sc.unrestricted {
					f.assertNoLeak(t, sc, "tags", r.Body)
				}
			}
			// Values of several attributes are exactly those of the granted spans (also vs the oracle).
			for _, name := range []string{"resource.k8s.namespace.name", "resource.service.name", "span.team", "span.http.method", "name"} {
				if denied(sc, name) {
					continue
				}
				exp := map[string]bool{}
				for _, s := range f.spans {
					if !sc.grants(s) {
						continue
					}
					switch {
					case name == "name":
						exp[s.name] = true
					case strings.HasPrefix(name, "span."):
						if v, ok := s.spanAttrs[strings.TrimPrefix(name, "span.")]; ok {
							exp[v] = true
						}
					default:
						if v, ok := s.resource()[strings.TrimPrefix(name, "resource.")]; ok {
							exp[v] = true
						}
					}
				}
				// Tempo evaluates q for tag values approximately (black-box finding: with a policy filter it can omit
				// values of granted spans, and for != filters it may return the attribute's own key as a value), so
				// the check is containment, not equality: every value must belong to a granted span.
				r := a.tagValues(name, "")
				got := tagValueSet(t, r)
				_, bare, _ := strings.Cut(name, ".")
				for v := range got {
					if !exp[v] && v != bare {
						t.Errorf("tag values %s: %q is not a value of a granted span", name, v)
					}
				}
				if len(got) == 0 && len(exp) > 0 && sc.unrestricted {
					t.Errorf("tag values %s empty", name)
				}
				if !sc.unrestricted {
					f.assertNoLeak(t, sc, "tag values "+name, r.Body)
				}
				// With q the answer is the sub-set of the granted spans matching q.
				r = a.tagValues(name, `{ status = error }`)
				for v := range tagValueSet(t, r) {
					if !exp[v] && v != bare {
						t.Errorf("tag values %s with q: %q is not a granted value", name, v)
					}
				}
			}
		})
	}
}

func denied(sc scenario, name string) bool {
	for _, d := range sc.deny {
		if d == name {
			return true
		}
	}
	return false
}

// --- denied attributes -----------------------------------------------------------------------------------

func testDenied(t *testing.T, f *fixture) {
	sc := scenarioByName("ns-a-deny")
	a := f.as(sc)
	for _, q := range []string{
		`{ span.customer_id = "cust-ns-a-0" }`,
		`{ span.customer_id =~ ".*" }`,
		`{ .customer_id != "x" }`,
		`{ span."customer_id" = "x" }`,
		`{ } | select(span.customer_id)`,
		`{ } | by(span.customer_id)`,
		`{ } | rate() by (span.customer_id)`,
		`{ span.http.method = "GET" } | select(.customer_id, span.http.method)`,
	} {
		for _, call := range []func(string) *framework.Response{a.search, a.metricsRange, a.metricsInstant} {
			r := call(q)
			if r.Status != 403 && r.Status != 400 {
				t.Errorf("%s: status %d, want 403/400: %.200s", q, r.Status, r.Body)
			}
			if r.Status == 200 {
				t.Errorf("%s: passed through", q)
			}
		}
		if r := a.tags(q); r.Status == 200 {
			t.Errorf("tags with q=%s: passed through", q)
		}
	}
	if r := a.tagValues("span.customer_id", ""); r.Status != 403 {
		t.Errorf("tag values of denied attribute: %d %s", r.Status, r.Body)
	}
	if r := a.tagValues("customer_id", ""); r.Status != 403 {
		t.Errorf("tag values of denied (unscoped) attribute: %d %s", r.Status, r.Body)
	}
	// The resource-scoped attribute of the same name is a different attribute and not denied.
	if r := a.search(`{ resource.customer_id = "x" }`); r.Status != 200 {
		t.Errorf("resource.customer_id: %d %s", r.Status, r.Body)
	}
	// The other users are not affected.
	if r := f.as(scenarioByName("ns-ab")).search(`{ span.customer_id = "cust-ns-a-0" }`); r.Status != 200 {
		t.Errorf("customer_id for ns-ab: %d", r.Status)
	}
	// Search results of the denied user never carry the attribute.
	r := a.search(`{ } | select(span.http.method)`)
	if strings.Contains(string(r.Body), "customer_id") || strings.Contains(string(r.Body), "cust-ns-") {
		t.Errorf("customer_id in search response: %.300s", r.Body)
	}
}

// --- endpoints and authentication ---------------------------------------------------------------------------

func testEndpoints(t *testing.T, f *fixture) {
	a := f.as(scenarioByName("ns-a"))
	status := func(what string, r *framework.Response, want ...int) {
		t.Helper()
		if !slices.Contains(want, r.Status) {
			t.Errorf("%s: status %d, want %v: %.200s", what, r.Status, want, r.Body)
		}
	}
	status("anonymous", f.proxy.Anonymous().Get("/tempo/api/search", nil), 401)
	status("user without policy", f.proxy.As("nobody", "nogroup").Get("/tempo/api/search", url.Values{"q": {"{}"}}), 403)
	status("legacy tags", a.searchRaw(url.Values{"tags": {"service.name=cart"}}), 400)
	status("legacy tags with q", a.searchRaw(url.Values{"tags": {"x=y"}, "q": {"{}"}}), 400)
	status("legacy minDuration", a.searchRaw(url.Values{"minDuration": {"1ms"}, "q": {"{}"}}), 400)
	status("legacy maxDuration", a.searchRaw(url.Values{"maxDuration": {"1s"}}), 400)
	status("two q", a.searchRaw(url.Values{"q": {"{}", "{ status = error }"}}), 400)
	status("invalid q", a.search("{ span.x = "), 400)
	status("unknown param", a.searchRaw(url.Values{"q": {"{}"}, "foo": {"bar"}}), 400)
	status("metrics without q", a.c.Get("/tempo/api/metrics/query_range", a.window()), 400)
	status("v1 tag endpoint", a.c.Get("/tempo/api/search/tags", nil), 403, 404)
	status("overrides", a.c.Get("/tempo/api/overrides", nil), 403, 404)
	status("status", a.c.Get("/tempo/status/config", nil), 403, 404)
	status("echo", a.c.Get("/tempo/api/echo", nil), 200)
	status("tags without q", a.tags(""), 200)

	// A client-supplied tenant header never reaches Tempo: tenant t2 is not visible through a t1 user.
	req := a.c
	r := req.GetWith("/tempo/api/search", url.Values{"q": {`{ resource.service.name = "t2-secret-svc" }`}}, map[string]string{"X-Scope-OrgID": "t2"})
	if r.Status != 200 || len(decodeSearch(t, r).Traces) != 0 {
		t.Errorf("tenant header spoof: %d %s", r.Status, r.Body)
	}
	// Tenant t2's user sees t2 data of ns-a only, not the t1 data of the same namespace.
	t2 := f.as(scenarioByName("t2-ns-a"))
	s := decodeSearch(t, t2.search("{}"))
	for _, tr := range s.Traces {
		for _, id := range spanIDsOf(tr.sets()) {
			if f.byID[id].tenant != "t2" {
				t.Errorf("t2 user sees span of tenant %s", f.byID[id].tenant)
			}
		}
	}
	if len(s.Traces) != 2 {
		t.Errorf("t2 user sees %d traces, want 2", len(s.Traces))
	}
}
