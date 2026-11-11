package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/fanout"
)

// stallDoer answers with size bytes and then waits until release is closed before the body ends.
type stallDoer struct {
	size    int
	release chan struct{}
}

func (d stallDoer) Do(*http.Request) (*http.Response, error) {
	body := io.MultiReader(bytes.NewReader(make([]byte, d.size)), waitReader(d.release))
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(body)}, nil
}

type waitReader chan struct{}

func (w waitReader) Read([]byte) (int, error) {
	<-w
	return 0, io.EOF
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

	// An older fan-out holds the whole budget and still reads.
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		r, _ := http.NewRequest(http.MethodGet, "http://other/", nil)
		_, held, err := fanout.Run(context.Background(), stallDoer{size: 1 << 20, release: release}, []*http.Request{r},
			fanout.Options{MaxParallel: 1, MaxBytes: 1 << 20, Budget: s.budget})
		held.Release()
		done <- err
	}()
	for deadline := time.Now().Add(5 * time.Second); s.budget.InUse() < 1<<20; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the older fan-out did not reserve its bytes")
		}
	}
	resp, body := get(t, srv, path, tok)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if got := testutil.ToFloat64(h.metrics.Requests.WithLabelValues("logs", "/loki/api/v1/query_range", "error", "merge_budget_exhausted")); got != 1 {
		t.Fatalf("requests_total{reason=merge_budget_exhausted} = %v", got)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if resp, body := get(t, srv, path, tok); resp.StatusCode != http.StatusOK {
		t.Fatalf("after release: status %d: %s", resp.StatusCode, body)
	}
	// The proxy writes and flushes the merged response and releases its bytes only when Execute
	// returns. The client can have the response before that.
	for deadline := time.Now().Add(5 * time.Second); s.budget.InUse() != 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("budget in use after the request = %d", s.budget.InUse())
		}
	}
}
