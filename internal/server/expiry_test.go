package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	enforcelogql "github.com/querdian-dev/querdian/internal/enforce/logql"
	"github.com/querdian-dev/querdian/internal/policy"
)

// policiesWithOrigin compiles the cluster policies and sets the Origin of the rules of the named policies,
// as the compiler does for a SpokePolicy (T6.2). origins maps a policy name to its spoke. The rules get the
// version of validSpoke(spoke).
func (h *harness) policiesWithOrigin(origins map[string]string, cps ...v1.ClusterAccessPolicy) {
	c := policy.Compile(policy.Input{Config: h.cfg, ClusterPolicies: cps})
	for _, idx := range []map[string][]*policy.CompiledRule{c.ByUser, c.ByGroup} {
		for _, rules := range idx {
			for _, r := range rules {
				r.Origin = origins[strings.TrimPrefix(r.Policy, "ClusterAccessPolicy/")]
				if r.Origin != "" {
					sc := validSpoke(r.Origin)
					r.OriginVersion = policy.SpokeVersion{UID: sc.UID, Generation: sc.Generation}
				}
			}
		}
	}
	h.snaps.set(policy.NewSnapshot(c, h.cfg, 1))
}

func restricted(backend string, deny []string, tenants []string, ms ...v1.Matcher) v1.ClusterRule {
	r := v1.ClusterRule{Backends: []string{backend}, Tenants: tenants, Matchers: ms}
	if len(deny) > 0 {
		r.Labels = &v1.LabelFilter{Deny: deny}
	}
	return r
}

func match(n, v string) v1.Matcher { return v1.Matcher{Name: n, Op: v1.MatchEqual, Value: v} }

// validSpoke returns a SpokeCluster that synced now at its current generation.
func validSpoke(name string, mods ...func(*v1.SpokeCluster)) *v1.SpokeCluster {
	sc := &v1.SpokeCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Generation: 1},
		Status:     v1.SpokeClusterStatus{LastSyncTime: &metav1.Time{Time: time.Now()}, ObservedGeneration: 1},
	}
	for _, m := range mods {
		m(sc)
	}
	return sc
}

func suspended(sc *v1.SpokeCluster) { sc.Spec.Suspended = true }

func TestSpokeExpiryOnQueries(t *testing.T) {
	h := newHarness(t)
	h.policiesWithOrigin(map[string]string{"spoke": "cluster-a"},
		clusterPolicy("hub", v1Subjects("hub"), restricted("metrics", nil, nil, match("namespace", "a"))),
		clusterPolicy("spoke", v1Subjects("spoke"), restricted("metrics", []string{"secret"}, nil, match("namespace", "b"))),
	)
	dl := policy.NewDeadlines()
	dl.Update(validSpoke("cluster-a"), time.Hour)
	srv := httptest.NewServer(h.server(allEnforcers, func(d *Deps) { d.Deadlines = dl }).ProxyHandler())
	defer srv.Close()

	query := func(groups ...string) (int, string) {
		resp, body := get(t, srv, "/prom/api/v1/query?query=up", h.issuer.token(t, "u", groups...))
		return resp.StatusCode, body
	}
	upQuery := func() string { return h.metricsUp.last(t).URL.Query().Get("query") }
	if code, body := query("spoke"); code != 200 || !strings.Contains(upQuery(), `namespace="b"`) {
		t.Fatalf("valid spoke: %d %s %s", code, body, upQuery())
	}

	dl.Update(validSpoke("cluster-a", suspended), time.Hour)
	before := h.metricsUp.count()
	code, body := query("spoke")
	if code != http.StatusForbidden || h.metricsUp.count() != before {
		t.Fatalf("suspended spoke: %d %s", code, body)
	}
	if e := errBody(t, body); e.ErrorType != "forbidden" || strings.Contains(body, "cluster-a") {
		t.Fatalf("error body must not name the cluster: %s", body)
	}
	if got := testutil.ToFloat64(h.metrics.Requests.WithLabelValues("metrics", "/api/v1/query", "deny", "policy_source_expired")); got != 1 {
		t.Fatalf("policy_source_expired count = %v", got)
	}

	// With a hub grant, the request uses the hub terms only, and the deny label of the spoke stays.
	if code, body := query("hub", "spoke"); code != 200 {
		t.Fatalf("hub and expired spoke: %d %s", code, body)
	}
	if q := upQuery(); !strings.Contains(q, `namespace="a"`) || strings.Contains(q, `namespace="b"`) {
		t.Fatalf("expired spoke term used: %s", q)
	}
	resp, _ := get(t, srv, "/prom/api/v1/query?query=up%7Bsecret%3D%22x%22%7D", h.issuer.token(t, "u", "hub", "spoke"))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("deny label of the expired spoke must stay: %d", resp.StatusCode)
	}
	if resp, _ := get(t, srv, "/prom/api/v1/query?query=up%7Bsecret%3D%22x%22%7D", h.issuer.token(t, "u", "hub")); resp.StatusCode != 200 {
		t.Fatalf("hub alone does not deny the label: %d", resp.StatusCode)
	}

	// Deleted, then re-created: expired until a valid sync, then restored (rule 7).
	dl.Delete("cluster-a")
	if code, _ := query("spoke"); code != http.StatusForbidden {
		t.Fatal("deleted spoke must be expired")
	}
	dl.Update(validSpoke("cluster-a", func(sc *v1.SpokeCluster) { sc.Status = v1.SpokeClusterStatus{} }), time.Hour)
	if code, _ := query("spoke"); code != http.StatusForbidden {
		t.Fatal("re-created spoke with no sync must be expired")
	}
	dl.Update(validSpoke("cluster-a"), time.Hour)
	if code, _ := query("spoke"); code != 200 {
		t.Fatal("valid sync must restore the grant")
	}

	// Without a table, each spoke rule is expired.
	noTable := httptest.NewServer(h.server(allEnforcers).ProxyHandler())
	defer noTable.Close()
	if resp, _ := get(t, noTable, "/prom/api/v1/query?query=up", h.issuer.token(t, "u", "spoke")); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no table: %d", resp.StatusCode)
	}
}

func TestReadyRequiresCompleteDeadlineTable(t *testing.T) {
	h := newHarness(t)
	h.policies()
	dl := policy.NewDeadlines()
	h.cfg.Multicluster = &config.Multicluster{HubCluster: "mla"}
	mgmt := httptest.NewServer(h.server(func(d *Deps) { d.Deadlines = dl }).ManagementHandler())
	defer mgmt.Close()
	if resp, _ := get(t, mgmt, "/readyz", ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("incomplete table: %d", resp.StatusCode)
	}
	dl.MarkComplete()
	if resp, _ := get(t, mgmt, "/readyz", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("complete table: %d", resp.StatusCode)
	}
	noTable := httptest.NewServer(h.server().ManagementHandler())
	defer noTable.Close()
	if resp, _ := get(t, noTable, "/readyz", ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no table with multicluster: %d", resp.StatusCode)
	}
	h.cfg.Multicluster = nil
	if resp, _ := get(t, noTable, "/readyz", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("no table without multicluster: %d", resp.StatusCode)
	}
}

// openTail starts a tail through the server against a fake Loki that keeps the socket open.
func openTail(t *testing.T, h *harness, s *Server, groups ...string) *websocket.Conn {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"streams":[{"stream":{"app":"a"},"values":[["1","hello"]]}]}`))
		_, _, _ = c.Read(r.Context()) // until the proxy closes the socket
	}))
	t.Cleanup(up.Close)
	h.cfg.Backends[1].URL = up.URL
	srv := httptest.NewServer(s.ProxyHandler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/loki/loki/api/v1/tail?query=%7Bapp%3D%22a%22%7D"
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + h.issuer.token(t, "u", groups...)}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	if _, msg, err := c.Read(ctx); err != nil || !strings.Contains(string(msg), "hello") {
		t.Fatalf("read: %v %q", err, msg)
	}
	return c
}

// waitTailClosed returns the time until the server closes the tail, and fails if it is not a 1008 close.
func waitTailClosed(t *testing.T, c *websocket.Conn, limit time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), limit+5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation || strings.Contains(err.Error(), "cluster-a") {
		t.Fatalf("close: %v", err)
	}
	return time.Since(start)
}

func tailHarness(t *testing.T, dl *policy.Deadlines) (*harness, *Server) {
	t.Helper()
	h := newHarness(t)
	h.policiesWithOrigin(map[string]string{"spoke": "cluster-a"},
		clusterPolicy("hub", v1Subjects("hub"), restricted("logs", nil, []string{"t1"}, match("k8s_namespace_name", "a"))),
		clusterPolicy("spoke", v1Subjects("spoke"), restricted("logs", nil, []string{"t1"}, match("k8s_namespace_name", "b"))),
	)
	s := h.server(func(d *Deps) {
		d.Deadlines = dl
		d.TailPlanners = map[config.BackendType]TailPlanner{config.BackendLoki: enforcelogql.PlanTail}
	})
	return h, s
}

// TestTailClosesOnSpokeSuspension uses the default check interval: the tail must close within 10 seconds.
func TestTailClosesOnSpokeSuspension(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the default check interval")
	}
	dl := policy.NewDeadlines()
	dl.Update(validSpoke("cluster-a"), time.Hour)
	h, s := tailHarness(t, dl)
	c := openTail(t, h, s, "hub", "spoke")
	dl.Update(validSpoke("cluster-a", suspended), time.Hour)
	if d := waitTailClosed(t, c, 10*time.Second); d > 10*time.Second {
		t.Fatalf("tail closed after %v", d)
	}
}

func TestTailClosesOnSpokeExpiry(t *testing.T) {
	dl := policy.NewDeadlines()
	// 3 s gives a slow -race runner time for the dial and the resolve before the spoke expires.
	dl.Update(validSpoke("cluster-a"), 3*time.Second)
	h, s := tailHarness(t, dl)
	s.tailRevokeCheck = 50 * time.Millisecond
	c := openTail(t, h, s, "spoke")
	waitTailClosed(t, c, 5*time.Second)
}

// TestTailClosesOnSpokeSpecChange: a new spec of the SpokeCluster (for example a smaller tenant allow-list)
// closes the tail, also when a sync of the new spec makes the spoke valid before the next check (PLAN
// §18.9). A status change alone, such as a heartbeat, does not close it.
func TestTailClosesOnSpokeSpecChange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		next   func(*v1.SpokeCluster)
		closes bool
	}{
		{"new generation", func(sc *v1.SpokeCluster) { sc.Generation, sc.Status.ObservedGeneration = 2, 2 }, true},
		{"re-created", func(sc *v1.SpokeCluster) { sc.UID = "uid-recreated" }, true},
		{"heartbeat", func(*v1.SpokeCluster) {}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dl := policy.NewDeadlines()
			dl.Update(validSpoke("cluster-a"), time.Hour)
			h, s := tailHarness(t, dl)
			s.tailRevokeCheck = 50 * time.Millisecond
			c := openTail(t, h, s, "spoke")
			dl.Update(validSpoke("cluster-a", tc.next), time.Hour)
			if dl.Expired("cluster-a", time.Now()) {
				t.Fatal("the new state must be valid in the table")
			}
			if tc.closes {
				waitTailClosed(t, c, 5*time.Second)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != -1 {
				t.Fatalf("tail closed: %v", err)
			}
		})
	}
}

// TestTailWithHubTermsOnlyIsNotChecked: a spoke that was expired at the start gave no terms, so the tail
// does not watch it.
func TestTailWithHubTermsOnlyIsNotChecked(t *testing.T) {
	dl := policy.NewDeadlines()
	h, s := tailHarness(t, dl)
	s.tailRevokeCheck = 20 * time.Millisecond
	c := openTail(t, h, s, "hub", "spoke")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != -1 {
		t.Fatalf("hub tail closed: %v", err)
	}
}
