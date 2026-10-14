//go:build envtest

package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/controller"
)

var (
	restCfg *rest.Config
	scheme  = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	if err := controller.AddToScheme(scheme); err != nil {
		panic(err)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	var err error
	if restCfg, err = env.Start(); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	if code != 0 {
		panic("tests failed")
	}
}

// TestEndToEndWithRealController: envtest API server + controller + httptest upstream + test issuer.
func TestEndToEndWithRealController(t *testing.T) {
	h := newHarness(t)
	ctl := controller.New(h.cfg, controller.WithDebounce(50*time.Millisecond))
	srv := httptest.NewServer(h.server(func(d *Deps) { d.Snapshots = ctl }).ProxyHandler())
	defer srv.Close()
	tok := h.issuer.token(t, "alice", "e2e-admins")

	// Before the controller has produced a snapshot the proxy answers 503 and touches no upstream.
	resp, body := get(t, srv, "/prom/api/v1/query?query=up", tok)
	if resp.StatusCode != 503 || h.metricsUp.count() != 0 {
		t.Fatalf("before ready: %d %s", resp.StatusCode, body)
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, 30*time.Second, "controller ready", ctl.Ready)

	// Ready, but no policy yet: default deny.
	resp, body = get(t, srv, "/prom/api/v1/query?query=up", tok)
	if resp.StatusCode != 403 || h.metricsUp.count() != 0 {
		t.Fatalf("no policy: %d %s", resp.StatusCode, body)
	}

	direct, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	pol := clusterPolicy("e2e-admins", v1.Subjects{Groups: []string{"e2e-admins"}, Users: []string{}}, unrestricted("metrics"), unrestricted("logs", "t1"))
	if err := direct.Create(context.Background(), &pol); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = direct.Delete(context.Background(), &pol) })

	waitFor(t, 5*time.Second, "policy active", func() bool {
		resp, _ := get(t, srv, "/prom/api/v1/query?query=up", tok)
		return resp.StatusCode == 200
	})
	if got := h.metricsUp.last(t).URL.Query().Get("query"); got != "up" {
		t.Fatalf("upstream query = %q", got)
	}
	resp, _ = get(t, srv, "/loki/loki/api/v1/labels", tok)
	if resp.StatusCode != 200 || h.logsUp.last(t).Header.Get("X-Scope-OrgID") != "t1" {
		t.Fatalf("logs: %d tenant %q", resp.StatusCode, h.logsUp.last(t).Header.Get("X-Scope-OrgID"))
	}

	// Deleting the policy revokes access again.
	if err := direct.Delete(context.Background(), &pol); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "policy revoked", func() bool {
		resp, _ := get(t, srv, "/prom/api/v1/query?query=up", tok)
		return resp.StatusCode == 403
	})
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
