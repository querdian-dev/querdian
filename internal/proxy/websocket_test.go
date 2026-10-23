package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/goleak"

	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// branch scripts one upstream socket: frames to send, then an optional close; otherwise it stays open.
type branch struct {
	frames    []string
	closeWith *websocket.CloseError
}

// fakeLoki is a Loki tail endpoint whose behaviour is chosen by the query parameter.
type fakeLoki struct {
	srv      *httptest.Server
	branches map[string]branch
	closed   chan string // query of every socket the proxy closed
	mu       sync.Mutex
	headers  []http.Header
}

func newFakeLoki(t *testing.T, branches map[string]branch) *fakeLoki {
	t.Helper()
	f := &fakeLoki{branches: branches, closed: make(chan string, 16)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		f.mu.Lock()
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		b := f.branches[q]
		for _, fr := range b.frames {
			if c.Write(r.Context(), websocket.MessageText, []byte(fr)) != nil {
				return
			}
		}
		if b.closeWith != nil {
			_ = c.Close(b.closeWith.Code, b.closeWith.Reason)
			return
		}
		_, _, _ = c.Read(r.Context()) // returns once the proxy closes the socket
		f.closed <- q
		_ = c.CloseNow()
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLoki) waitClosed(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-f.closed:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream socket not closed")
		}
	}
}

// tailServer serves ServeTail for a plan with one request per query.
func tailServer(t *testing.T, f *fakeLoki, queries ...string) *httptest.Server {
	t.Helper()
	return tailServerOpt(t, f, TailOptions{}, queries...)
}

func tailServerOpt(t *testing.T, f *fakeLoki, opt TailOptions, queries ...string) *httptest.Server {
	t.Helper()
	return tailServerHide(t, f, opt, false, queries...)
}

// tailServerHide is tailServerOpt with Target.HideUpstreamErrors set to hide.
func tailServerHide(t *testing.T, f *fakeLoki, opt TailOptions, hide bool, queries ...string) *httptest.Server {
	t.Helper()
	plan := enforce.Plan{Merge: enforce.MergeTailStreams, Strategy: "fanout"}
	for i, q := range queries {
		plan.Requests = append(plan.Requests, enforce.Upstream{Query: url.Values{"query": {q}}, Term: i})
	}
	tgt := Target{
		Backend:            &config.Backend{URL: f.srv.URL, TenantHeader: "X-Scope-OrgID"},
		Endpoint:           routes.Endpoint{Websocket: true},
		UpstreamPath:       "/loki/api/v1/tail",
		Effective:          policy.Effective{Tenants: []string{"t1"}},
		HideUpstreamErrors: hide,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(&Proxy{}).ServeTail(w, r, tgt, plan, opt)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dial(t *testing.T, srv *httptest.Server, hdr http.Header) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, srv.URL+"/loki/api/v1/tail?query=x", &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func readText(t *testing.T, c *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

const (
	frameA = `{"streams":[{"stream":{"app":"x","pod":"a"},"values":[["1","one"],["2","two"]]}]}`
	frameB = `{"streams":[{"stream":{"pod":"a","app":"x"},"values":[["2","two"],["3","three"]]}],"dropped_entries":[{"labels":{"app":"x"},"timestamp":"9"}]}`
)

func TestTailForwardsAndSanitisesHeaders(t *testing.T) {
	f := newFakeLoki(t, map[string]branch{`{a="1"}`: {frames: []string{frameA}}})
	srv := tailServer(t, f, `{a="1"}`)
	c := dial(t, srv, http.Header{
		"Authorization": {"Bearer secret"}, "Cookie": {"s=1"}, "X-Scope-OrgID": {"evil"}, "Baggage": {"k=v"},
		"Traceparent": {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
	})
	if got := readText(t, c); got != frameA {
		t.Fatalf("frame = %s", got)
	}
	f.mu.Lock()
	h := f.headers[0]
	f.mu.Unlock()
	if h.Get("Authorization") != "" || h.Get("Cookie") != "" || h.Get("Baggage") != "" {
		t.Fatalf("client headers leaked: %v", h)
	}
	if h.Get("X-Scope-OrgID") != "t1" {
		t.Fatalf("tenant header = %q", h.Get("X-Scope-OrgID"))
	}
	// Client close closes the upstream.
	_ = c.Close(websocket.StatusNormalClosure, "")
	f.waitClosed(t, 1)
}

func TestTailMergesAndDedupes(t *testing.T) {
	f := newFakeLoki(t, map[string]branch{
		`{a="1"}`: {frames: []string{frameA}},
		`{a="2"}`: {frames: []string{frameB}},
	})
	srv := tailServer(t, f, `{a="1"}`, `{a="2"}`)
	c := dial(t, srv, nil)

	seen := map[string]int{}
	var dropped bool
	deadline := time.Now().Add(5 * time.Second)
	for len(seen) < 3 && time.Now().Before(deadline) {
		got := readText(t, c)
		for _, e := range []string{`["1","one"]`, `["2","two"]`, `["3","three"]`} {
			if n := strings.Count(got, e); n > 0 {
				seen[e] += n
			}
		}
		if strings.Contains(got, `"dropped_entries":[{"labels":{"app":"x"},"timestamp":"9"}]`) {
			dropped = true
		}
	}
	if len(seen) != 3 || seen[`["2","two"]`] != 1 {
		t.Fatalf("entries = %v (duplicate must be forwarded once)", seen)
	}
	if !dropped {
		t.Fatal("dropped_entries not propagated")
	}
	// All upstreams are closed when the client goes away.
	_ = c.Close(websocket.StatusNormalClosure, "")
	f.waitClosed(t, 2)
}

func TestTailUpstreamCloseClosesClientWithReason(t *testing.T) {
	f := newFakeLoki(t, map[string]branch{
		`{a="1"}`: {closeWith: &websocket.CloseError{Code: websocket.StatusGoingAway, Reason: "loki restarting"}},
		`{a="2"}`: {},
	})
	srv := tailServer(t, f, `{a="1"}`, `{a="2"}`)
	c := dial(t, srv, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusGoingAway || !strings.Contains(err.Error(), "loki restarting") {
		t.Fatalf("err = %v", err)
	}
	f.waitClosed(t, 1) // the surviving upstream is closed as well
}

// With redaction the upstream's close reason is withheld: it is upstream text that no rewriter sees.
func TestTailUpstreamCloseReasonHidden(t *testing.T) {
	f := newFakeLoki(t, map[string]branch{
		`{a="1"}`: {closeWith: &websocket.CloseError{Code: websocket.StatusInternalError, Reason: `failed for series {customer_id="42"}`}},
	})
	srv := tailServerHide(t, f, TailOptions{}, true, `{a="1"}`)
	c := dial(t, srv, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusInternalError || strings.Contains(err.Error(), "customer_id") ||
		!strings.Contains(err.Error(), "upstream closed") {
		t.Fatalf("err = %v", err)
	}
}

func TestTailInvalidFrameFailsClosed(t *testing.T) {
	f := newFakeLoki(t, map[string]branch{`{a="1"}`: {frames: []string{"not json"}}})
	srv := tailServer(t, f, `{a="1"}`)
	c := dial(t, srv, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("err = %v", err)
	}
	f.waitClosed(t, 1)
}

func TestTailUpstreamDialFailure(t *testing.T) {
	f := newFakeLoki(t, nil)
	srv := tailServer(t, f, `{a="1"}`)
	f.srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, srv.URL+"/x", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("err=%v resp=%v", err, resp)
	}
	_ = resp.Body.Close()
}

func TestTailRewrite(t *testing.T) {
	f := newFakeLoki(t, map[string]branch{`{a="1"}`: {frames: []string{frameA}}})
	srv := tailServerOpt(t, f, TailOptions{Rewrite: func(b []byte) ([]byte, error) {
		return []byte(strings.ReplaceAll(string(b), `"pod":"a"`, `"pod":"redacted-1"`)), nil
	}}, `{a="1"}`)
	c := dial(t, srv, nil)
	if got := readText(t, c); strings.Contains(got, `"pod":"a"`) || !strings.Contains(got, `"pod":"redacted-1"`) {
		t.Fatalf("frame = %s", got)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	f.waitClosed(t, 1)
}

func TestTailRewriteErrorFailsClosed(t *testing.T) {
	f := newFakeLoki(t, map[string]branch{`{a="1"}`: {frames: []string{frameA}}})
	srv := tailServerOpt(t, f, TailOptions{Rewrite: func([]byte) ([]byte, error) { return nil, errors.New("boom") }}, `{a="1"}`)
	c := dial(t, srv, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusInternalError {
		t.Fatalf("frame %s err = %v", b, err)
	}
	f.waitClosed(t, 1)
}
