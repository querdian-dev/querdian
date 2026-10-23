package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
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
	// Upstream errors keep their status but not their text: Prometheus prints whole series in some of them,
	// and no result rewriter sees an error body.
	const upstreamErr = `{"status":"error","errorType":"execution","error":"found duplicate series for the match group {} on the right hand-side of the operation: [{customer_id=\"42\"}, {customer_id=\"43\"}]"}`
	h.metricsUp.setReply(func(*http.Request) (int, string, string) { return 422, "application/json", upstreamErr })
	resp, body = get(t, srv, "/prom/api/v1/query?query=up", tok)
	if resp.StatusCode != 422 || strings.Contains(body, "customer_id") || !strings.Contains(body, "details withheld") {
		t.Errorf("upstream error: %d %s", resp.StatusCode, body)
	}
	// Without redaction the upstream error text is passed through unchanged.
	h.policies(clusterPolicy("plain", v1Subjects("g"), v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{eqm("namespace", "a")}}))
	if resp, body = get(t, srv, "/prom/api/v1/query?query=up", tok); resp.StatusCode != 422 || body != upstreamErr {
		t.Errorf("upstream error without redaction: %d %s", resp.StatusCode, body)
	}
}

// Loki errors of redacted requests are withheld too ("pipeline error ... for series" lists the series): a
// streamed request keeps the status, a fanned-out one drops the snippet of the failed branch.
func TestLokiRedactionWithholdsUpstreamErrors(t *testing.T) {
	h := newHarness(t)
	red := &v1.LabelFilter{Redact: []v1.RedactRule{{Name: "customer_id", Strategy: v1.RedactRemove}}}
	byNS := clusterPolicy("by-ns", v1Subjects("g"), v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eqm("k8s_namespace_name", "a")}, Labels: red})
	byApp := clusterPolicy("by-app", v1Subjects("g"), v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eqm("app", "x")}})
	const upstreamErr = `pipeline error: 'JSONParserErr' for series: '{app="x", customer_id="c1"}'.`
	h.logsUp.setReply(func(*http.Request) (int, string, string) { return 400, "text/plain; charset=utf-8", upstreamErr })
	srv := httptest.NewServer(h.server(allEnforcers).ProxyHandler())
	defer srv.Close()
	tok := h.issuer.token(t, "u", "g")
	path := "/loki/loki/api/v1/query_range?query=" + url.QueryEscape(`{job="j"}`)

	tests := []struct {
		name     string
		policies []v1.ClusterAccessPolicy
		status   int
		withheld bool
	}{
		{"streamed", []v1.ClusterAccessPolicy{byNS}, 400, true},
		{"fanned out", []v1.ClusterAccessPolicy{byNS, byApp}, 502, true},
		{"no redaction", []v1.ClusterAccessPolicy{byApp}, 400, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.policies(tt.policies...)
			resp, body := get(t, srv, path, tok)
			if resp.StatusCode != tt.status {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tt.status, body)
			}
			if tt.withheld {
				if strings.Contains(body, "customer_id") || !strings.Contains(body, "details withheld") ||
					resp.Header.Get("Content-Type") != "application/json" {
					t.Errorf("%s: %s", resp.Header.Get("Content-Type"), body)
				}
			} else if body != upstreamErr {
				t.Errorf("without redaction the upstream text is passed through: %s", body)
			}
		})
	}
}

// A backend that switches protocols on a query route must not tunnel unredacted bytes to the client: the
// client's Upgrade is never forwarded, and a 101 fails closed.
func TestRedactionSurvivesProtocolSwitch(t *testing.T) {
	h := newHarness(t)
	h.policies(clusterPolicy("redact", v1Subjects("g"), v1.ClusterRule{Backends: []string{"metrics"}, Matchers: []v1.Matcher{eqm("namespace", "a")},
		Labels: &v1.LabelFilter{Redact: []v1.RedactRule{{Name: "customer_id", Strategy: v1.RedactPlaceholder}}}}))
	const result = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"customer_id":"42"},"value":[1,"1"]}]}}`
	var always atomic.Bool
	h.metricsUp.setHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" && !always.Load() {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, result)
			return
		}
		c, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n" + result)
		_ = buf.Flush()
	})
	srv := httptest.NewServer(h.server(allEnforcers).ProxyHandler())
	defer srv.Close()
	tok := h.issuer.token(t, "u", "g")
	upgrade := []string{"Connection", "Upgrade", "Upgrade", "websocket"}

	// The backend upgrades only when asked to; it never is.
	resp, body := get(t, srv, "/prom/api/v1/query?query=up", tok, upgrade...)
	if resp.StatusCode != 200 || strings.Contains(body, `"42"`) || !placeholderRe.MatchString(body) {
		t.Errorf("client upgrade: %d %q", resp.StatusCode, body)
	}
	// The backend upgrades regardless.
	always.Store(true)
	for _, hdr := range [][]string{nil, upgrade} {
		resp, body = get(t, srv, "/prom/api/v1/query?query=up", tok, hdr...)
		if resp.StatusCode != 502 || strings.Contains(body, `"42"`) {
			t.Errorf("forced 101 (client headers %v): %d %q", hdr, resp.StatusCode, body)
		}
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
