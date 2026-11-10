package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/fanout"
)

// fixedDoer answers each request with size bytes.
type fixedDoer int

func (d fixedDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(make([]byte, d)))}, nil
}

// One budget covers all backends. When it is full, a fan-out fails closed with 503 and the reason
// merge_budget_exhausted; when it is free again, the same request passes.
func TestFanoutBudgetExhaustedEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.cfg.Limits.MaxMergeBytes = 1 << 20
	h.cfg.Limits.MaxInflightMergeBytes = 1 << 20
	h.policies(
		clusterPolicy("by-ns", v1Subjects("g"), v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eqm("k8s_namespace_name", "a")}}),
		clusterPolicy("by-app", v1Subjects("g"), v1.ClusterRule{Backends: []string{"logs"}, Tenants: []string{"t1"}, Matchers: []v1.Matcher{eqm("app", "x")}}),
	)
	h.logsUp.setReply(func(*http.Request) (int, string, string) {
		return 200, "application/json", `{"status":"success","data":{"resultType":"streams","result":[]}}`
	})
	s := h.server(allEnforcers)
	srv := httptest.NewServer(s.ProxyHandler())
	defer srv.Close()
	tok := h.issuer.token(t, "u", "g")
	path := "/loki/loki/api/v1/query_range?" + url.Values{"query": {`{job="j"}`}}.Encode()

	// Another request holds the whole budget.
	r, _ := http.NewRequest(http.MethodGet, "http://other/", nil)
	held, err := fanout.Run(context.Background(), fixedDoer(1<<20), []*http.Request{r}, fanout.Options{MaxParallel: 1, MaxBytes: 1 << 20, Budget: s.budget})
	if err != nil {
		t.Fatal(err)
	}
	resp, body := get(t, srv, path, tok)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if got := testutil.ToFloat64(h.metrics.Requests.WithLabelValues("logs", "/loki/api/v1/query_range", "error", "merge_budget_exhausted")); got != 1 {
		t.Fatalf("requests_total{reason=merge_budget_exhausted} = %v", got)
	}

	s.budget.Release(held)
	if resp, body := get(t, srv, path, tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("after release: status %d: %s", resp.StatusCode, body)
	}
	if got := s.budget.InUse(); got != 0 {
		t.Fatalf("budget in use after the request = %d", got)
	}
}
