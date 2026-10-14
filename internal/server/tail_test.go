package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/querdian-dev/querdian/internal/config"
	enforcelogql "github.com/querdian-dev/querdian/internal/enforce/logql"
)

// TestTailThroughServer: client WebSocket -> auth -> policy -> tail planner -> fake Loki WebSocket.
func TestTailThroughServer(t *testing.T) {
	var mu sync.Mutex
	var gotHeader http.Header
	var gotQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHeader, gotQuery = r.Header.Clone(), r.URL.Query().Get("query")
		mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		frame := `{"streams":[{"stream":{"app":"a"},"values":[["1","hello"]]}]}`
		_ = c.Write(r.Context(), websocket.MessageText, []byte(frame))
		_ = c.Close(websocket.StatusNormalClosure, "done")
	}))
	defer up.Close()

	h := newHarness(t)
	h.cfg.Backends[1].URL = up.URL
	h.policies(clusterPolicy("tail", v1Subjects("tailers"), unrestricted("logs", "t1")))
	srv := httptest.NewServer(h.server(func(d *Deps) {
		d.TailPlanners = map[config.BackendType]TailPlanner{config.BackendLoki: enforcelogql.PlanTail}
	}).ProxyHandler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/loki/loki/api/v1/tail?query=%7Bapp%3D%22a%22%7D"
	hdr := http.Header{"Authorization": {"Bearer " + h.issuer.token(t, "u", "tailers")}, "X-Scope-OrgID": {"evil"}}
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.CloseNow() }()
	_, msg, err := c.Read(ctx)
	if err != nil || !strings.Contains(string(msg), "hello") {
		t.Fatalf("read: %v %q", err, msg)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotHeader.Get("X-Scope-OrgID") != "t1" || gotHeader.Get("Authorization") != "" || gotQuery != `{app="a"}` {
		t.Fatalf("upstream saw header=%v query=%q", gotHeader, gotQuery)
	}
}

func TestTailRejectedWithoutPlannerOrAuth(t *testing.T) {
	h := newHarness(t)
	h.policies(clusterPolicy("tail", v1Subjects("tailers"), unrestricted("logs", "t1")))
	tok := h.issuer.token(t, "u", "tailers")

	noPlanner := httptest.NewServer(h.server().ProxyHandler())
	defer noPlanner.Close()
	resp, body := get(t, noPlanner, "/loki/loki/api/v1/tail?query=%7Bapp%3D%22a%22%7D", tok)
	if resp.StatusCode != 503 || h.logsUp.count() != 0 {
		t.Fatalf("no planner: %d %s", resp.StatusCode, body)
	}
	resp, _ = get(t, noPlanner, "/loki/loki/api/v1/tail?query=%7Bapp%3D%22a%22%7D", "")
	if resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
}
