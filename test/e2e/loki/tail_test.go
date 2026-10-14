//go:build e2e

package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/querdian-dev/querdian/internal/config"

	"github.com/querdian-dev/querdian/test/e2e/framework"
)

func wsURL(base, path string, q url.Values) string {
	return "ws" + strings.TrimPrefix(base, "http") + path + "?" + q.Encode()
}

// tailer reads tail frames in the background into a set of "labels ts line" keys (duplicates counted).
type tailer struct {
	c    *websocket.Conn
	mu   sync.Mutex
	keys map[string]int
	done chan error
}

func dialTail(t testing.TB, url string, hdr http.Header, ren map[string]string) *tailer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		st := 0
		if resp != nil {
			st = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", url, err, st)
	}
	c.SetReadLimit(8 << 20)
	tl := &tailer{c: c, keys: map[string]int{}, done: make(chan error, 1)}
	t.Cleanup(func() { _ = c.CloseNow() })
	go func() {
		for {
			_, msg, err := c.Read(context.Background())
			if err != nil {
				tl.done <- err
				return
			}
			var fr struct {
				Streams []struct {
					Stream map[string]string `json:"stream"`
					Values [][]string        `json:"values"`
				} `json:"streams"`
			}
			if json.Unmarshal(msg, &fr) != nil {
				continue
			}
			tl.mu.Lock()
			for _, s := range fr.Streams {
				for _, v := range s.Values {
					tl.keys[labelKey(renameTenant(s.Stream, ren))+" "+v[0]+" "+v[1]]++
				}
			}
			tl.mu.Unlock()
		}
	}()
	return tl
}

// snapshot returns the delivered entries whose text contains sub.
func (tl *tailer) snapshot(sub string) map[string]int {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	out := map[string]int{}
	for k, v := range tl.keys {
		if strings.Contains(k, sub) {
			out[k] = v
		}
	}
	return out
}

func (tl *tailer) waitFor(sub string, n int, d time.Duration) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if len(tl.snapshot(sub)) >= n {
			return true
		}
	}
	return false
}

func bearer(a api) http.Header { return http.Header{"Authorization": {"Bearer " + a.c.Token}} }

func testTail(t *testing.T, f *fixture) {
	const lines = 3
	// Loki's tail rejects several tenants ("multiple org IDs present"): grants over several tenants cannot be
	// tailed. The proxy must fail closed and never deliver data then.
	t.Run("multi-tenant-grants-fail-closed", func(t *testing.T) {
		for _, name := range []string{"mt-same", "mt-diff", "unrestricted-mt"} {
			a := f.as(setByName(name))
			c, resp, err := websocket.Dial(context.Background(), wsURL(f.proxy.URL, "/loki/loki/api/v1/tail", url.Values{"query": {`{app=~".+"}`}}), &websocket.DialOptions{HTTPHeader: bearer(a)})
			if err == nil {
				_ = c.CloseNow()
				t.Errorf("%s: tail over several tenants was accepted", name)
			} else if resp == nil || resp.StatusCode < 400 {
				t.Errorf("%s: %v", name, err)
			} else {
				t.Logf("%s: HTTP %d", name, resp.StatusCode)
			}
		}
	})
	for _, name := range []string{"one", "overlap", "three", "multi-matcher"} {
		set := setByName(name)
		t.Run("merge-"+name, func(t *testing.T) {
			user := f.as(set)
			start := f.now()
			q := url.Values{"query": {`{app=~".+"}`}, "start": {nanos(start)}, "limit": {"1000"}}
			mine := dialTail(t, wsURL(f.proxy.URL, "/loki/loki/api/v1/tail", q), bearer(user), set.rename())
			ref := dialTail(t, wsURL(f.refURL, "/loki/api/v1/tail", q), http.Header{"X-Scope-OrgID": {set.refHeader()}}, set.rename())
			// Loki's tailers attach to the ingesters asynchronously: push marker lines until both sockets deliver one.
			var probe streamDef
			for _, s := range f.all {
				if set.visible(t, s) {
					probe = s
					break
				}
			}
			warm := false
			for i := range 40 {
				e := []entry{{f.now().UnixNano(), fmt.Sprintf("tail-warmup-%s-%d", name, i)}}
				push(t, f.mainURL, probe.tenant, []streamDef{{tenant: probe.tenant, labels: probe.labels, entries: e}})
				push(t, f.refURL, refTenant(set.name, probe.tenant), []streamDef{{tenant: probe.tenant, labels: probe.labels, entries: e}})
				time.Sleep(500 * time.Millisecond)
				if len(mine.snapshot("tail-warmup")) > 0 && len(ref.snapshot("tail-warmup")) > 0 {
					warm = true
					break
				}
			}
			if !warm {
				t.Fatal("the tail sockets did not deliver the marker lines")
			}

			// New lines in every stream of the main instance and in the granted streams of the reference.
			stamp := f.now()
			for _, tn := range allTenants {
				var all, granted []streamDef
				for _, s := range f.all {
					if s.tenant != tn {
						continue
					}
					c := streamDef{tenant: s.tenant, labels: s.labels}
					for i := range lines {
						c.entries = append(c.entries, entry{stamp.Add(time.Duration(i) * time.Millisecond).UnixNano(), fmt.Sprintf("tail-%s-%s-%d", name, s.labels["pod"], i)})
					}
					all = append(all, c)
					if set.visible(t, s) {
						granted = append(granted, c)
					}
				}
				push(t, f.mainURL, tn, all)
				if slicesContains(set.tenants, tn) {
					push(t, f.refURL, refTenant(set.name, tn), granted)
				}
			}
			want := 0
			for _, s := range f.all {
				if set.visible(t, s) {
					want += lines
				}
			}
			sub := " tail-" + name + "-"
			ref.waitFor(sub, want, 20*time.Second)
			mine.waitFor(sub, want, 20*time.Second)
			time.Sleep(time.Second) // late duplicates
			w, g := ref.snapshot(sub), mine.snapshot(sub)
			if len(w) != want {
				t.Fatalf("reference tail delivered %d of %d lines", len(w), want)
			}
			for k, n := range g {
				if n > 1 {
					t.Errorf("duplicate delivered %d times: %.200s", n, k)
				}
				if _, ok := w[k]; !ok {
					t.Errorf("tail delivers an entry outside the grant: %.300s", k)
				}
			}
			for k := range w {
				if _, ok := g[k]; !ok {
					t.Errorf("tail misses %.300s", k)
				}
			}
			// Client close: the proxy tears down its upstream sockets and keeps serving.
			_ = mine.c.Close(websocket.StatusNormalClosure, "bye")
		})
	}

	t.Run("rejected", func(t *testing.T) {
		dial := func(a api, q url.Values) int {
			_, resp, err := websocket.Dial(context.Background(), wsURL(f.proxy.URL, "/loki/loki/api/v1/tail", q), &websocket.DialOptions{HTTPHeader: bearer(a)})
			if err == nil {
				t.Errorf("tail %v was accepted", q)
				return 0
			}
			if resp == nil {
				t.Fatalf("no HTTP response: %v", err)
			}
			return resp.StatusCode
		}
		if st := dial(f.as(setByName("deny-one")), url.Values{"query": {`{pod=~".+"}`}}); st != 403 {
			t.Errorf("denied label: %d", st)
		}
		if st := dial(f.as(setByName("one")), url.Values{"query": {`rate({app="web"}[1m])`}}); st != 400 {
			t.Errorf("metric query: %d", st)
		}
		if st := dial(f.as(setByName("one")), url.Values{"query": {`{app="web"}`}, "foo": {"bar"}}); st != 400 {
			t.Errorf("unknown parameter: %d", st)
		}
		if st := dial(api{c: f.proxy.As("nobody", "none")}, url.Values{"query": {`{app="web"}`}}); st != 403 {
			t.Errorf("no grant: %d", st)
		}
	})

	t.Run("upstream-drop-closes-client", func(t *testing.T) {
		// A cutting relay between the proxy and Loki: when it drops the connections the client must be closed.
		target, _ := url.Parse(f.mainURL)
		rp := httputil.NewSingleHostReverseProxy(target)
		var mu sync.Mutex
		var conns []net.Conn
		relay := httptest.NewUnstartedServer(rp)
		relay.Config.ConnState = func(c net.Conn, st http.ConnState) {
			if st == http.StateNew {
				mu.Lock()
				conns = append(conns, c)
				mu.Unlock()
			}
		}
		relay.Start()
		t.Cleanup(relay.Close)
		p2 := framework.StartProxy(t, framework.ProxyOptions{Issuer: f.proxy.Issuer, Backends: []config.Backend{lokiBackend(relay.URL)}, Policies: f.pol})
		a := api{c: p2.As("u-"+setByName("overlap").group, setByName("overlap").group), prefix: "/loki", end: f.end}
		tl := dialTail(t, wsURL(p2.URL, "/loki/loki/api/v1/tail", url.Values{"query": {`{app=~".+"}`}, "start": {nanos(f.now())}}), bearer(a), nil)
		time.Sleep(2 * time.Second)
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		select {
		case err := <-tl.done:
			t.Logf("client closed after the upstream drop: %v", err)
		case <-time.After(20 * time.Second):
			t.Fatal("client socket still open after all upstream sockets were dropped")
		}
	})
}

func slicesContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
