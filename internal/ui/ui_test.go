package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	enforcepromql "github.com/querdian-dev/querdian/internal/enforce/promql"
	"github.com/querdian-dev/querdian/internal/policy"
)

type provider struct{ s *policy.Snapshot }

func (p provider) Snapshot() *policy.Snapshot { return p.s }

func newHandler(t *testing.T, ready bool) http.Handler {
	t.Helper()
	cfg, err := config.Load("../config/testdata/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var snap *policy.Snapshot
	if ready {
		cps := []v1.ClusterAccessPolicy{{
			ObjectMeta: metav1.ObjectMeta{Name: "team-a", Generation: 1},
			Spec: v1.ClusterAccessPolicySpec{
				Subjects: v1.Subjects{Groups: []string{"team-a"}},
				Rules: []v1.ClusterRule{{Backends: []string{"metrics"},
					Matchers: []v1.Matcher{{Name: "namespace", Op: v1.MatchEqual, Value: "<script>a</script>"}},
					Labels: &v1.LabelFilter{Deny: []string{"secret"},
						Redact: []v1.RedactRule{{Name: "customer_id", Strategy: v1.RedactPlaceholder}}}}},
			},
		}}
		snap = policy.NewSnapshot(policy.Compile(policy.Input{Config: cfg, ClusterPolicies: cps}), cfg, 5)
	}
	return Handler(Deps{Config: cfg, Snapshots: provider{snap},
		Enforcers: map[config.BackendType]enforce.Enforcer{config.BackendPrometheus: enforcepromql.New()}})
}

func do(h http.Handler, method, path, ct, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestReadEndpoints(t *testing.T) {
	h := newHandler(t, true)
	for _, p := range []string{"/api/overview", "/api/policies", "/api/subjects", "/api/access?groups=team-a", "/api/backends"} {
		w := do(h, "GET", p, "", "")
		if w.Code != 200 || !json.Valid(w.Body.Bytes()) {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	w := do(h, "GET", "/api/access?groups=team-a", "", "")
	var a struct {
		Access []accessJSON `json:"access"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &a)
	if !a.Access[0].Granted || a.Access[1].Granted || a.Access[1].Reason != "no_grant" {
		t.Fatalf("unexpected access: %+v", a.Access)
	}
	if do(h, "POST", "/api/overview", "", "").Code != http.StatusMethodNotAllowed {
		t.Fatal("overview must be GET only")
	}
}

func TestNotReady(t *testing.T) {
	if c := do(newHandler(t, false), "GET", "/api/policies", "", "").Code; c != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", c)
	}
}

func TestSecurityHeadersAndAssets(t *testing.T) {
	w := do(newHandler(t, true), "GET", "/", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Querdian Console") {
		t.Fatalf("index: %d", w.Code)
	}
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Cache-Control"} {
		if w.Header().Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	if strings.Contains(w.Body.String(), "<script>") && !strings.Contains(w.Body.String(), `src="app.js"`) {
		t.Error("inline script found")
	}
}

func TestTestBench(t *testing.T) {
	h := newHandler(t, true)
	post := func(body string) map[string]any {
		w := do(h, "POST", "/api/test", "application/json", body)
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		var m map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		return m
	}

	m := post(`{"groups":["team-a"],"backend":"metrics","query":"up"}`)
	ups := m["upstream"].([]any)
	if m["allowed"] != true || !strings.Contains(ups[0].(map[string]any)["query"].(string), `namespace="<script>a</script>"`) {
		t.Fatalf("rewrite: %v", m)
	}
	if m = post(`{"groups":["team-a"],"backend":"metrics","query":"up{secret=\"x\"}"}`); m["allowed"] != false || m["status"].(float64) < 400 {
		t.Fatalf("denied label must be refused: %v", m)
	}
	if m = post(`{"groups":["team-a"],"backend":"metrics","query":"up{"}`); m["allowed"] != false {
		t.Fatalf("unparseable query must be refused: %v", m)
	}
	if m = post(`{"user":"nobody","backend":"metrics","query":"up"}`); m["allowed"] != false || m["reason"] != "no_grant" {
		t.Fatalf("unknown user must fail closed: %v", m)
	}

	for name, c := range map[string]struct {
		ct, body string
		want     int
	}{
		"wrong content type": {"text/plain", `{}`, http.StatusUnsupportedMediaType},
		"unknown field":      {"application/json", `{"x":1}`, http.StatusBadRequest},
		"unknown backend":    {"application/json", `{"backend":"nope"}`, http.StatusBadRequest},
		"non-query path":     {"application/json", `{"backend":"metrics","path":"/nope"}`, http.StatusBadRequest},
		"too large":          {"application/json", `{"query":"` + strings.Repeat("a", maxBodyBytes) + `"}`, http.StatusBadRequest},
	} {
		if w := do(h, "POST", "/api/test", c.ct, c.body); w.Code != c.want {
			t.Errorf("%s: want %d, got %d", name, c.want, w.Code)
		}
	}
}

func TestRedactionViews(t *testing.T) {
	h := newHandler(t, true)
	want := redactionJSON{Name: "customer_id", Strategy: "Placeholder", Effect: "numbered placeholders", Before: "42", After: "redacted-1"}

	var p struct {
		Policies []policyJSON `json:"policies"`
	}
	_ = json.Unmarshal(do(h, "GET", "/api/policies", "", "").Body.Bytes(), &p)
	r := p.Policies[0].Rules[0]
	if len(r.Redactions) != 1 || r.Redactions[0] != want || strings.Join(r.DeniedLabels, ",") != "customer_id,secret" {
		t.Fatalf("policy rule: %+v", r)
	}

	var a struct {
		Access []accessJSON `json:"access"`
	}
	_ = json.Unmarshal(do(h, "GET", "/api/access?groups=team-a", "", "").Body.Bytes(), &a)
	if len(a.Access[0].Redactions) != 1 || a.Access[0].Redactions[0] != want || len(a.Access[1].Redactions) != 0 {
		t.Fatalf("access: %+v", a.Access)
	}

	for path, mode := range map[string]string{"": "rewrite", "/api/v1/query_range": "rewrite", "/api/v1/series": "remove"} {
		w := do(h, "POST", "/api/test", "application/json", `{"groups":["team-a"],"backend":"metrics","path":"`+path+`","query":"up"}`)
		var m struct {
			Allowed       bool            `json:"allowed"`
			RedactionMode string          `json:"redactionMode"`
			Redactions    []redactionJSON `json:"redactions"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		if !m.Allowed || m.RedactionMode != mode || len(m.Redactions) != 1 {
			t.Errorf("%q: %s", path, w.Body)
		}
	}
	w := do(h, "POST", "/api/test", "application/json", `{"groups":["team-a"],"backend":"metrics","query":"sum by (customer_id) (up)"}`)
	if !strings.Contains(w.Body.String(), `"allowed":false`) {
		t.Errorf("grouping by a redacted label must be refused: %s", w.Body)
	}
}
