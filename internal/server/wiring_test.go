package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/auth"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	enforcelogql "github.com/querdian-dev/querdian/internal/enforce/logql"
	enforcepromql "github.com/querdian-dev/querdian/internal/enforce/promql"
	enforcetraceql "github.com/querdian-dev/querdian/internal/enforce/traceql"
)

func allEnforcers(d *Deps) {
	d.Enforcers = map[config.BackendType]enforce.Enforcer{
		config.BackendPrometheus: enforcepromql.New(),
		config.BackendLoki:       enforcelogql.New(),
		config.BackendTempo:      enforcetraceql.New(),
	}
}

func eqm(name, value string) v1.Matcher {
	return v1.Matcher{Name: name, Op: v1.MatchEqual, Value: value}
}

// A log query under a two-term policy is fanned out and the Loki stream responses are merged.
func TestLokiFanoutMergeEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.policies(
		clusterPolicy("by-ns", v1Subjects("g"), v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eqm("k8s_namespace_name", "a")}}),
		clusterPolicy("by-app", v1Subjects("g"), v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eqm("app", "x")}}),
	)
	h.logsUp.setReply(func(r *http.Request) (int, string, string) {
		q := r.URL.Query().Get("query")
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
			q = r.Form.Get("query")
		}
		line, ts := "from-app", "2000"
		if strings.Contains(q, "k8s_namespace_name") {
			line, ts = "from-ns", "1000"
		}
		return 200, "application/json", `{"status":"success","data":{"resultType":"streams","result":[{"stream":{"s":"1"},"values":[["` + ts + `","` + line + `"]]}],"stats":{}}}`
	})
	srv := httptest.NewServer(h.server(allEnforcers).ProxyHandler())
	defer srv.Close()
	tok := h.issuer.token(t, "u", "g")

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			before := h.logsUp.count()
			var req *http.Request
			qs := url.Values{"query": {`{job="j"}`}, "limit": {"10"}, "direction": {"forward"}}
			if method == http.MethodGet {
				req, _ = http.NewRequest(method, srv.URL+"/loki/loki/api/v1/query_range?"+qs.Encode(), nil)
			} else {
				req, _ = http.NewRequest(method, srv.URL+"/loki/loki/api/v1/query_range", strings.NewReader(qs.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			var body struct {
				Data struct {
					Result []struct {
						Values [][]string `json:"values"`
					} `json:"result"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != 200 {
				t.Fatalf("status %d err %v", resp.StatusCode, err)
			}
			if h.logsUp.count()-before != 2 {
				t.Fatalf("expected 2 fan-out requests, got %d", h.logsUp.count()-before)
			}
			var lines []string
			for _, s := range body.Data.Result {
				for _, v := range s.Values {
					lines = append(lines, v[1])
				}
			}
			if strings.Join(lines, ",") != "from-ns,from-app" { // forward direction: oldest first
				t.Fatalf("merged lines = %v", lines)
			}
		})
	}
}

// Denied labels are removed from Prometheus series responses and the labels list.
func TestPrometheusMetadataFilteringEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.policies(clusterPolicy("deny", v1Subjects("g"), v1.ClusterRule{Backends: []string{"metrics"}, Unrestricted: true,
		Labels: &v1.LabelFilter{Deny: []string{"secret"}}}))
	h.metricsUp.setReply(func(r *http.Request) (int, string, string) {
		if strings.HasSuffix(r.URL.Path, "/series") {
			return 200, "application/json", `{"status":"success","data":[{"__name__":"up","job":"a","secret":"s"},{"__name__":"up","job":"a","secret":"t"}]}`
		}
		return 200, "application/json", `{"status":"success","data":["__name__","job","secret"]}`
	})
	srv := httptest.NewServer(h.server(allEnforcers).ProxyHandler())
	defer srv.Close()
	tok := h.issuer.token(t, "u", "g")

	_, body := get(t, srv, "/prom/api/v1/series?match[]=up", tok)
	if strings.Contains(body, "secret") || !strings.Contains(body, `"job":"a"`) || strings.Count(body, `"job":"a"`) != 1 {
		t.Fatalf("series not filtered/deduped: %s", body)
	}
	_, body = get(t, srv, "/prom/api/v1/labels", tok)
	if strings.Contains(body, "secret") || !strings.Contains(body, `"job"`) {
		t.Fatalf("labels not filtered: %s", body)
	}
	resp, body := get(t, srv, "/prom/api/v1/label/secret/values", tok)
	if resp.StatusCode != 403 {
		t.Fatalf("denied label values: %d %s", resp.StatusCode, body)
	}
	// Query results are deliberately NOT filtered.
	h.metricsUp.setReply(func(*http.Request) (int, string, string) {
		return 200, "application/json", `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"secret":"s"},"value":[1,"1"]}]}}`
	})
	_, body = get(t, srv, "/prom/api/v1/query?query=up", tok)
	if !strings.Contains(body, `"secret":"s"`) {
		t.Fatalf("query results must be passed through unfiltered: %s", body)
	}
}

const traceJSON = `{"batches":[
 {"resource":{"attributes":[{"key":"k8s.namespace.name","value":{"stringValue":"a"}}]},"scopeSpans":[{"spans":[{"traceId":"AAAAAAAAAAAAAAAAAAAAAQ==","spanId":"AAAAAAAAAAE=","name":"keep"}]}]},
 {"resource":{"attributes":[{"key":"k8s.namespace.name","value":{"stringValue":"b"}}]},"scopeSpans":[{"spans":[{"traceId":"AAAAAAAAAAAAAAAAAAAAAQ==","spanId":"AAAAAAAAAAI=","name":"drop"}]}]}
]}`

func TestTraceByIDFilteredEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.policies(
		clusterPolicy("ns-a", v1Subjects("a"), v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eqm("resource.k8s.namespace.name", "a")}}),
		clusterPolicy("ns-c", v1Subjects("c"), v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eqm("resource.k8s.namespace.name", "c")}}),
		clusterPolicy("all", v1Subjects("admin"), unrestricted("traces", "t1")),
	)
	h.tracesUp.setReply(func(*http.Request) (int, string, string) { return 200, "application/json", traceJSON })
	srv := httptest.NewServer(h.server(allEnforcers).ProxyHandler())
	defer srv.Close()
	path := "/tempo/api/traces/0000000000000000000000000000001"

	resp, body := get(t, srv, path, h.issuer.token(t, "u", "a"), "Accept", "application/json")
	if resp.StatusCode != 200 || !strings.Contains(body, "keep") || strings.Contains(body, "drop") {
		t.Fatalf("restricted user: %d %s", resp.StatusCode, body)
	}
	if cl := resp.Header.Get("Content-Length"); cl == "" {
		t.Fatalf("content-length must be corrected after filtering: %v", resp.Header)
	}
	resp, body = get(t, srv, path, h.issuer.token(t, "u", "c"), "Accept", "application/json")
	if resp.StatusCode != 404 || strings.Contains(body, "keep") || strings.Contains(body, "drop") {
		t.Fatalf("no covered spans must be 404: %d %s", resp.StatusCode, body)
	}
	resp, body = get(t, srv, path, h.issuer.token(t, "u", "admin"), "Accept", "application/json")
	if resp.StatusCode != 200 || !strings.Contains(body, "keep") || !strings.Contains(body, "drop") {
		t.Fatalf("unrestricted user sees the full trace: %d %s", resp.StatusCode, body)
	}
}

func TestTempoTagListFilteredEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.policies(clusterPolicy("deny", v1Subjects("g"), v1.ClusterRule{Backends: []string{"traces"}, Tenants: []string{"t1"}, Unrestricted: true,
		Labels: &v1.LabelFilter{Deny: []string{"resource.secret"}}}))
	h.tracesUp.setReply(func(*http.Request) (int, string, string) {
		return 200, "application/json", `{"scopes":[{"name":"resource","tags":["service.name","secret"]}]}`
	})
	srv := httptest.NewServer(h.server(allEnforcers).ProxyHandler())
	defer srv.Close()
	resp, body := get(t, srv, "/tempo/api/v2/search/tags", h.issuer.token(t, "u", "g"))
	if resp.StatusCode != 200 || strings.Contains(body, "secret") || !strings.Contains(body, "service.name") {
		t.Fatalf("tag list not filtered: %d %s", resp.StatusCode, body)
	}
}

func TestRequireGroup(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for name, c := range map[string]struct {
		group  string
		id     *auth.Identity
		status int
	}{
		"no group required": {"", nil, http.StatusNoContent},
		"member":            {"admins", &auth.Identity{Username: "u", Groups: []string{"admins"}}, http.StatusNoContent},
		"not a member":      {"admins", &auth.Identity{Username: "u", Groups: []string{"x"}}, http.StatusForbidden},
		"no identity":       {"admins", nil, http.StatusForbidden},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.id != nil {
			r = r.WithContext(auth.WithIdentity(r.Context(), *c.id))
		}
		w := httptest.NewRecorder()
		RequireGroup(c.group, next).ServeHTTP(w, r)
		if w.Code != c.status {
			t.Errorf("%s: want %d, got %d", name, c.status, w.Code)
		}
	}
}
