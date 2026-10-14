//go:build e2e

package loki

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// --- isolation ------------------------------------------------------------------------------------------

func testIsolation(t *testing.T, f *fixture) {
	for _, u := range isolationUsers {
		t.Run(u.user, func(t *testing.T) {
			a := f.asUser(u.user, u.group)
			var want []streamDef
			wantLines := 0
			for _, s := range f.all {
				if s.tenant == "t1" && slices.Contains(u.allowed, s.labels[nsLabel]) {
					want = append(want, s)
					wantLines += len(s.entries)
				}
			}
			foreign := "ns-c"
			if u.user == "alice" {
				foreign = "ns-b"
			}

			// Log query, both directions: exactly the granted lines, nothing of t2 or other namespaces.
			for _, dir := range []string{"backward", "forward"} {
				r := a.queryRange(`{app=~".+"}`, url.Values{"direction": {dir}})
				if r.Status != 200 {
					t.Fatalf("status %d %.300s", r.Status, r.Body)
				}
				es := entries(t, r, nil)
				if len(es) != wantLines {
					t.Errorf("%s: %d lines, want %d", dir, len(es), wantLines)
				}
				for _, e := range es {
					if !strings.Contains(e.stream, `k8s_namespace_name="`) || !containsAny(e.stream, u.allowed) {
						t.Fatalf("leak: stream {%s}", e.stream)
					}
					if strings.Contains(e.line, "tenant=t2") || strings.Contains(e.line, `"tenant":"t2"`) {
						t.Fatalf("leak: line of tenant t2: %s", e.line)
					}
				}
				checkOrder(t, r, dir == "forward")
			}

			// Metric: per-namespace line counts equal the data.
			res := normalise(t, a.instant(`sum by (k8s_namespace_name) (count_over_time({app=~".+"}[1h]))`), nil)
			if len(res) != len(u.allowed) {
				t.Errorf("count by namespace: %v", res)
			}
			for _, ns := range u.allowed {
				n := 0
				for _, s := range want {
					if s.labels[nsLabel] == ns {
						n += len(s.entries)
					}
				}
				if got := res[labelKey(map[string]string{nsLabel: ns})]; len(got) != 1 || !strings.HasSuffix(got[0], "="+strconv.Itoa(n)) {
					t.Errorf("namespace %s: %v, want %d", ns, got, n)
				}
			}

			// labels, label values, series, detected_*.
			for _, name := range []string{nsLabel} {
				if got := stringData(t, a.labelValues(name, "")); !slices.Equal(got, u.allowed) {
					t.Errorf("%s values = %v, want %v", name, got, u.allowed)
				}
			}
			for _, v := range stringData(t, a.labelValues("pod", "")) {
				if !containsAny(v, u.allowed) {
					t.Errorf("pod %q leaks", v)
				}
			}
			for _, m := range seriesData(t, a.series(), nil) {
				if !containsAny(m, u.allowed) {
					t.Errorf("series leaks %s", m)
				}
			}
			if r := a.detectedLabels(""); r.Status != 200 {
				t.Errorf("detected_labels %d %.200s", r.Status, r.Body)
			} else if c := detectedLabels(t, r)[nsLabel]; c != len(u.allowed) {
				t.Errorf("detected_labels reports %d namespaces, want %d", c, len(u.allowed))
			}
			if got := stringData(t, a.labelValues(nsLabel, `{`+nsLabel+`="`+foreign+`"}`)); len(got) != 0 {
				t.Errorf("label values for a foreign selector: %v", got)
			}
			if st := indexStats(t, a.stats(`{app=~".+"}`)); st.Entries == 0 || st.Entries > wantLines {
				t.Errorf("index/stats entries %d, want 1..%d (an estimate, exactness is checked against the oracle)", st.Entries, wantLines)
			}
			vol := volumes(t, a.volume(`{app=~".+"}`, url.Values{"aggregateBy": {"labels"}, "targetLabels": {nsLabel}}), nil, nil)
			for k := range vol {
				if !containsAny(k, u.allowed) && k != `k8s_namespace_name="",` {
					t.Errorf("volume leaks %s", k)
				}
			}

			// Adversarial queries must return nothing foreign.
			for _, q := range []string{
				`{` + nsLabel + `="` + foreign + `"}`,
				`{` + nsLabel + `=~"ns-.*", ` + nsLabel + `!~"` + strings.Join(u.allowed, "|") + `"}`,
				`{app=~".+"} # }{` + nsLabel + `="` + foreign + `"`,
				`{` + nsLabel + `="` + foreign + `"} or {app=~".+"}`,
				`{app=~".+"} |= "ns=` + foreign + `"`,
				`{app=~".+"} | label_format ` + nsLabel + `="` + foreign + `" | ` + nsLabel + `="` + foreign + `"`,
			} {
				r := a.queryRange(q, nil)
				if r.Status == 200 {
					for _, e := range entries(t, r, nil) {
						if !containsAny(e.stream, u.allowed) {
							t.Errorf("%s leaks {%s}", q, e.stream)
						}
						if strings.Contains(e.line, "ns="+foreign) || strings.Contains(e.line, `"ns":"`+foreign+`"`) {
							t.Errorf("%s leaks line %s", q, e.line)
						}
					}
				}
			}
			for _, q := range []string{
				`count_over_time({` + nsLabel + `="` + foreign + `"}[1h])`,
				`sum(rate({` + nsLabel + `="` + foreign + `"}[5m]))`,
				`label_replace(count_over_time({` + nsLabel + `="` + foreign + `"}[1h]), "x", "y", "app", "(.*)")`,
			} {
				if r := a.instant(q); r.Status == 200 && len(normalise(t, r, nil)) != 0 {
					t.Errorf("%s returned data", q)
				}
			}
			// label_replace may rewrite the namespace label of the output, but only of granted data.
			r := a.instant(`sum by (ns) (label_replace(count_over_time({app=~".+"}[1h]), "ns", "$1", "` + nsLabel + `", "(.*)"))`)
			if r.Status == 200 && len(normalise(t, r, nil)) != len(u.allowed) {
				t.Errorf("label_replace result: %v", normalise(t, r, nil))
			}

			// A client-supplied tenant header is ignored; POST works the same and is enforced.
			req, _ := http.NewRequest(http.MethodGet, a.c.Base+"/loki/loki/api/v1/query_range?"+a.window(url.Values{"query": {`{app=~".+"}`}, "limit": {"5000"}}).Encode(), nil) //nolint:noctx // test-only
			req.Header.Set("X-Scope-OrgID", "t2")
			if r := a.c.Do(req); r.Status != 200 || len(entries(t, r, nil)) != wantLines {
				t.Errorf("X-Scope-OrgID spoofing changed the result: %d", r.Status)
			}
			pr := a.do(http.MethodPost, "/loki/api/v1/query_range", a.window(url.Values{"query": {`{` + nsLabel + `="` + foreign + `"}`}}))
			if pr.Status == 200 && len(entries(t, pr, nil)) != 0 {
				t.Errorf("POST leak")
			}
			// The query given twice (URL and form) is ambiguous and rejected.
			both := a.c.PostForm("/loki/loki/api/v1/query_range?query="+url.QueryEscape(`{app="web"}`), url.Values{"query": {`{app="api"}`}})
			if both.Status != 400 {
				t.Errorf("query in URL and form: %d %.200s", both.Status, both.Body)
			}
		})
	}
}

func containsAny(s string, vals []string) bool {
	for _, v := range vals {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// --- federation -------------------------------------------------------------------------------------------

// testTenants checks the tenant-bound sets against the data itself (not the oracle): user-written tenant
// matchers and results carry exactly the granted (tenant, namespace) pairs.
func testTenants(t *testing.T, f *fixture) {
	type pair struct{ tenant, ns string }
	check := func(t *testing.T, set termSet, q string, grantedOnly bool) map[pair]bool {
		a := f.as(set)
		r := a.queryRange(q, nil)
		if r.Status != 200 {
			t.Fatalf("%s: %d %.300s", q, r.Status, r.Body)
		}
		got := map[pair]bool{}
		for _, e := range entries(t, r, nil) {
			lbl := parseKey(e.stream)
			got[pair{lbl["__tenant_id__"], lbl[nsLabel]}] = true
		}
		if grantedOnly {
			for p := range got {
				ok := false
				for _, s := range f.visibleStreams(t, set) {
					ok = ok || (s.tenant == p.tenant && s.labels[nsLabel] == p.ns)
				}
				if !ok {
					t.Errorf("%s: %+v is outside the grant", q, p)
				}
			}
		}
		return got
	}
	t.Run("mt-diff", func(t *testing.T) {
		set := setByName("mt-diff")
		got := check(t, set, `{app=~".+"}`, true)
		if len(got) != 2 || !got[pair{"t1", "ns-a"}] || !got[pair{"t2", "ns-d"}] {
			t.Errorf("pairs = %v", got)
		}
		// Terms differ per tenant: ns-b of t1 is not granted although the same namespace exists in t2's grant list.
		if got := check(t, set, `{app=~".+", `+nsLabel+`="ns-b"}`, true); len(got) != 0 {
			t.Errorf("ns-b: %v", got)
		}
		// A user-written tenant matcher narrows further and never widens.
		if got := check(t, set, `{app=~".+", __tenant_id__="t2"}`, true); len(got) != 1 || !got[pair{"t2", "ns-d"}] {
			t.Errorf("__tenant_id__=t2: %v", got)
		}
		if got := check(t, set, `{app=~".+", __tenant_id__=~"t1|t2", `+nsLabel+`=~"ns-a|ns-b|ns-c"}`, true); len(got) != 1 || !got[pair{"t1", "ns-a"}] {
			t.Errorf("combined: %v", got)
		}
		// Metric query: per-tenant counts.
		res := normalise(t, f.as(set).instant(`sum by (__tenant_id__, `+nsLabel+`) (count_over_time({app=~".+"}[1h]))`), nil)
		if len(res) != 2 {
			t.Errorf("metric pairs: %v", res)
		}
	})
	t.Run("mt-same header-binding-free", func(t *testing.T) {
		set := setByName("mt-same")
		got := check(t, set, `{app=~".+"}`, true)
		if len(got) != 2 || !got[pair{"t1", "ns-b"}] || !got[pair{"t2", "ns-b"}] {
			t.Errorf("pairs = %v", got)
		}
	})
	t.Run("single tenant has no tenant label", func(t *testing.T) {
		a := f.as(setByName("one"))
		r := a.queryRange(`{app=~".+", __tenant_id__="t2"}`, nil)
		if r.Status == 200 && len(entries(t, r, nil)) != 0 {
			t.Errorf("t2 data reachable through a t1-only grant")
		}
		// Client-sent header cannot add a tenant.
		req, _ := http.NewRequest(http.MethodGet, a.c.Base+"/loki/loki/api/v1/query_range?"+a.window(url.Values{"query": {`{app=~".+"}`}, "limit": {"5000"}}).Encode(), nil) //nolint:noctx // test-only
		req.Header.Set("X-Scope-OrgID", "t1|t2")
		for _, e := range entries(t, a.c.Do(req), nil) {
			if !strings.Contains(e.stream, `k8s_namespace_name="ns-a"`) {
				t.Errorf("leak %s", e.stream)
			}
		}
	})
}

func parseKey(k string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(k, `",`) {
		name, val, ok := strings.Cut(part, `="`)
		if ok {
			out[name] = strings.TrimSuffix(val, `"`)
		}
	}
	return out
}

// --- denied labels ----------------------------------------------------------------------------------------

func testDenied(t *testing.T, f *fixture) {
	for _, name := range []string{"deny-one", "deny-multi"} {
		set := setByName(name)
		t.Run(name, func(t *testing.T) {
			a := f.as(set)
			forbidden := []string{
				`{pod=~".+"}`,
				`{app="web", pod="web-ns-a-0"}`,
				`{app="web"} | json | pod="x"`,
				`{app="web"} | label_format x=pod`,
				`{app="web"} | line_format "{{.pod}}"`,
				`{app="web"} | line_format "{{ . }}"`,
				`{app="web"} | line_format "{{ range $k, $v := . }}{{$k}}{{end}}"`,
				`{app="web"} | label_format x="{{ . }}"`,
				`sum by (pod) (count_over_time({app="web"}[5m]))`,
				`sum without (pod) (count_over_time({app="web"}[5m]))`,
				`count_over_time({app="web"}[5m]) / on(pod) count_over_time({app="api"}[5m])`,
				`label_replace(count_over_time({app="web"}[5m]), "x", "$1", "pod", "(.*)")`,
				`label_replace(count_over_time({app="web"}[5m]), "pod", "x", "app", "(.*)")`,
				`count_over_time({app="web"} | logfmt | unwrap pod [5m])`,
			}
			for _, q := range forbidden {
				for _, call := range []func(string) int{
					func(q string) int { return a.queryRange(q, nil).Status },
					func(q string) int { return a.instant(q).Status },
				} {
					if st := call(q); st == 200 {
						t.Errorf("%s: want 403, got 200", q)
					}
				}
				if r := a.queryRange(q, nil); r.Status != 403 && r.Status != 400 {
					// log queries on the instant endpoint answer 400 upstream; range must be proxy-originated 403/400
					t.Errorf("%s: %d %.200s", q, r.Status, r.Body)
				} else if !fromProxy(r) {
					t.Errorf("%s: rejection did not come from the proxy: %.200s", q, r.Body)
				}
			}
			// The two rejection classes are 403: a plain denied label and a template reading all labels.
			for _, q := range []string{`{pod=~".+"}`, `{app="web"} | line_format "{{ . }}"`, `sum by (pod) (rate({app="web"}[1m]))`} {
				if r := a.queryRange(q, nil); r.Status != 403 {
					t.Errorf("%s: want 403, got %d", q, r.Status)
				}
			}
			if r := a.labelValues("pod", ""); r.Status != 403 {
				t.Errorf("label values of the denied label: %d", r.Status)
			}
			if r := a.labelValues("app", `{pod="x"}`); r.Status != 403 {
				t.Errorf("selector with the denied label on label values: %d", r.Status)
			}
			if r := a.series(`{pod="web-ns-a-0"}`); r.Status != 403 {
				t.Errorf("series with a denied matcher: %d", r.Status)
			}
			if r := a.volume(`{app="web"}`, url.Values{"aggregateBy": {"labels"}, "targetLabels": {"pod"}}); r.Status != 403 {
				t.Errorf("volume targetLabels=pod: %d", r.Status)
			}
			if r := a.detectedValues("pod", `{app="web"}`); r.Status != 403 {
				t.Errorf("detected_field/pod/values: %d", r.Status)
			}

			// Filtered responses: labels, detected_labels, detected_fields, series.
			if ls := stringData(t, a.labels("")); slices.Contains(ls, "pod") || !slices.Contains(ls, nsLabel) {
				t.Errorf("labels = %v", ls)
			}
			if d := detectedLabels(t, a.detectedLabels("")); d["pod"] != 0 || len(d) == 0 {
				t.Errorf("detected_labels = %v", d)
			}
			if _, ok := detectedFields(t, a.detectedFields(`{app="web"}`))["pod"]; ok {
				t.Errorf("detected_fields lists the denied label")
			}
			ser := seriesData(t, a.series(`{app=~".+"}`), nil)
			if len(ser) == 0 {
				t.Fatal("series is empty")
			}
			seen := map[string]bool{}
			for _, m := range ser {
				if strings.Contains(m, `pod=`) {
					t.Errorf("series response contains the denied label: %s", m)
				}
				if seen[m] {
					t.Errorf("duplicate label set after filtering: %s", m)
				}
				seen[m] = true
			}
			// Query results are NOT filtered (documented): the denied label is still returned.
			r := a.queryRange(`{app="web"}`, nil)
			found := false
			for _, e := range entries(t, r, nil) {
				found = found || strings.Contains(e.stream, `pod="web-ns-a-0"`)
			}
			if !found {
				t.Errorf("query results must keep denied labels untouched (PLAN 7.3)")
			}
			// Unaffected queries and templates naming allowed labels still work.
			for _, q := range []string{`{app="web"} | logfmt | line_format "{{.level}} {{.app}}"`, `sum by (app) (count_over_time({app=~".+"}[5m]))`} {
				if r := a.queryRange(q, nil); r.Status != 200 {
					t.Errorf("%s: %d %.200s", q, r.Status, r.Body)
				}
			}
		})
	}
}

// --- endpoints and authentication --------------------------------------------------------------------------

func testEndpoints(t *testing.T, f *fixture) {
	a := f.as(setByName("one"))
	for _, p := range []string{"/loki/api/v1/rules", "/prometheus/api/v1/rules", "/loki/api/v1/delete", "/api/prom/query", "/ready", "/config", "/metrics", "/flush"} {
		if r := a.c.Get(a.prefix+p, nil); r.Status != 403 {
			t.Errorf("%s: want 403, got %d", p, r.Status)
		}
	}
	for _, p := range []string{"/loki/api/v1/push", "/otlp/v1/logs"} {
		if r := a.c.PostForm(a.prefix+p, url.Values{"x": {"y"}}); r.Status != 403 && r.Status != 405 {
			t.Errorf("POST %s: %d", p, r.Status)
		}
	}
	if r := f.proxy.Anonymous().Get("/loki/loki/api/v1/labels", nil); r.Status != 401 {
		t.Errorf("anonymous: %d", r.Status)
	}
	nobody := api{c: f.proxy.As("nobody", "no-such-group"), prefix: "/loki", end: f.end}
	if r := nobody.labels(""); r.Status != 403 {
		t.Errorf("no grant: %d %.200s", r.Status, r.Body)
	}
	// Unparseable queries are rejected by the proxy, never passed through.
	if r := a.queryRange(`{app="web"`, nil); r.Status != 400 || !fromProxy(r) {
		t.Errorf("parse error: %d %.200s", r.Status, r.Body)
	}
	if r := a.get("query_range", a.window(url.Values{})); r.Status != 400 {
		t.Errorf("missing query: %d", r.Status)
	}
	if r := a.get("status/buildinfo", nil); r.Status != 200 {
		t.Errorf("buildinfo: %d", r.Status)
	}
	// Unrestricted equals direct access.
	admin := f.as(setByName("unrestricted"))
	direct := f.direct("t1")
	for _, q := range []string{`{app=~".+"}`, `{app="web"} | logfmt | level="error"`} {
		if d := diff(normalise(t, direct.queryRange(q, nil), nil), normalise(t, admin.queryRange(q, nil), nil)); d != "" {
			t.Errorf("unrestricted differs from direct for %s: %s", q, d)
		}
	}
}
