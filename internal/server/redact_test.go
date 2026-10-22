package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

var placeholderRe = regexp.MustCompile(`"customer_id":"redacted-\d+"`)

// Query results of a rule with labels.redact carry rewritten values; the label cannot be queried.
func TestPrometheusRedactionEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.policies(clusterPolicy("redact", v1Subjects("g"), v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{eqm("namespace", "a")},
		Labels: &v1.LabelFilter{Redact: []v1.RedactRule{{Name: "customer_id", Strategy: v1.RedactPlaceholder}}}}))
	var accept string
	h.metricsUp.setReply(func(r *http.Request) (int, string, string) {
		accept = r.Header.Get("Accept")
		switch {
		case strings.HasSuffix(r.URL.Path, "/query_exemplars"):
			return 200, "application/json", `{"status":"success","data":[{"seriesLabels":{"customer_id":"42"},"exemplars":[{"labels":{"customer_id":"42"},"value":"1","timestamp":1}]}]}`
		case strings.HasSuffix(r.URL.Path, "/query_range"):
			return 200, "application/json", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"customer_id":"42","job":"a"},"values":[[1,"1"]]}]}}`
		}
		return 200, "application/json", `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"customer_id":"42","job":"a"},"value":[1,"1"]},{"metric":{"customer_id":"43","job":"a"},"value":[1,"1"]}]}}`
	})
	srv := httptest.NewServer(h.server(allEnforcers).ProxyHandler())
	defer srv.Close()
	tok := h.issuer.token(t, "u", "g")

	for _, path := range []string{
		"/prom/api/v1/query?query=up",
		"/prom/api/v1/query_range?query=up&start=0&end=1&step=1",
		"/prom/api/v1/query_exemplars?query=up",
	} {
		resp, body := get(t, srv, path, tok, "Accept", "application/x-protobuf")
		if resp.StatusCode != 200 || strings.Contains(body, `"42"`) || strings.Contains(body, `"43"`) || !placeholderRe.MatchString(body) {
			t.Errorf("%s: %d %s", path, resp.StatusCode, body)
		}
		if !strings.Contains(body, `"warnings":["querdian: label values redacted by policy: customer_id (numbered placeholders)"]`) {
			t.Errorf("%s: Grafana notice missing: %s", path, body)
		}
		if accept != "application/json" {
			t.Errorf("%s: upstream Accept = %q, want application/json", path, accept)
		}
	}
	resp, body := get(t, srv, "/prom/api/v1/query?query="+url.QueryEscape(`up{customer_id="42"}`), tok)
	if resp.StatusCode != 403 {
		t.Errorf("a redacted label must be denied in queries: %d %s", resp.StatusCode, body)
	}

	// An upstream body that cannot be redacted fails closed.
	h.metricsUp.setReply(func(*http.Request) (int, string, string) { return 200, "application/json", `{"customer_id":"42"` })
	resp, body = get(t, srv, "/prom/api/v1/query?query=up", tok)
	if resp.StatusCode != 502 || strings.Contains(body, "42") {
		t.Errorf("malformed body: %d %s", resp.StatusCode, body)
	}
	// Upstream errors are passed through.
	h.metricsUp.setReply(func(*http.Request) (int, string, string) {
		return 422, "application/json", `{"status":"error","errorType":"execution","error":"x"}`
	})
	if resp, body = get(t, srv, "/prom/api/v1/query?query=up", tok); resp.StatusCode != 422 {
		t.Errorf("upstream error: %d %s", resp.StatusCode, body)
	}
}

// A fanned-out Loki query is merged first and then redacted with one numbering for all branches.
func TestLokiFanoutRedactionEndToEnd(t *testing.T) {
	h := newHarness(t)
	red := &v1.LabelFilter{Redact: []v1.RedactRule{{Name: "customer_id", Strategy: v1.RedactPlaceholder}}}
	h.policies(
		clusterPolicy("by-ns", v1Subjects("g"), v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eqm("k8s_namespace_name", "a")}, Labels: red}),
		clusterPolicy("by-app", v1Subjects("g"), v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eqm("app", "x")}}),
	)
	h.logsUp.setReply(func(r *http.Request) (int, string, string) {
		cust, ts := "c1", "2000"
		if strings.Contains(r.URL.Query().Get("query"), "k8s_namespace_name") {
			cust, ts = "c2", "1000"
		}
		return 200, "application/json", `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"customer_id":"` + cust + `"},"values":[["` + ts + `","line"]]}],"stats":{}}}`
	})
	srv := httptest.NewServer(h.server(allEnforcers).ProxyHandler())
	defer srv.Close()

	resp, body := get(t, srv, "/loki/loki/api/v1/query_range?query="+url.QueryEscape(`{job="j"}`)+"&limit=10", h.issuer.token(t, "u", "g"))
	if resp.StatusCode != 200 || strings.Contains(body, `"c1"`) || strings.Contains(body, `"c2"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"customer_id":"redacted-1"`) || !strings.Contains(body, `"customer_id":"redacted-2"`) {
		t.Fatalf("both branches must be numbered together: %s", body)
	}
}

// Tempo search attributes and trace-by-ID attributes are rewritten; other denied attributes are removed.
func TestTempoRedactionEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.policies(clusterPolicy("redact", v1Subjects("g"), v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"t1"}, Unrestricted: true,
		Labels: &v1.LabelFilter{Redact: []v1.RedactRule{{Name: "resource.k8s.namespace.name", Strategy: v1.RedactHash}}}}))
	h.tracesUp.setReply(func(r *http.Request) (int, string, string) {
		if strings.HasPrefix(r.URL.Path, "/api/search") {
			return 200, "application/json", `{"traces":[{"traceID":"1","spanSets":[{"spans":[{"spanID":"2","attributes":[{"key":"k8s.namespace.name","value":{"stringValue":"a"}}]}],"matched":1}]}],"metrics":{}}`
		}
		return 200, "application/json", traceJSON
	})
	srv := httptest.NewServer(h.server(allEnforcers).ProxyHandler())
	defer srv.Close()
	tok := h.issuer.token(t, "u", "g")
	hashed := regexp.MustCompile(`"key":"k8s.namespace.name","value":\{"stringValue":"redacted-[0-9a-f]{16}"\}`)

	resp, body := get(t, srv, "/tempo/api/search?q="+url.QueryEscape(`{}`), tok)
	if resp.StatusCode != 200 || strings.Contains(body, `"stringValue":"a"`) || !hashed.MatchString(body) {
		t.Fatalf("search: %d %s", resp.StatusCode, body)
	}
	resp, body = get(t, srv, "/tempo/api/traces/0000000000000000000000000000001", tok, "Accept", "application/json")
	if resp.StatusCode != 200 || strings.Contains(body, `"stringValue":"a"`) || strings.Contains(body, `"stringValue":"b"`) ||
		len(hashed.FindAllString(body, -1)) != 2 || !strings.Contains(body, `"keep"`) {
		t.Fatalf("trace by id: %d %s", resp.StatusCode, body)
	}
}
