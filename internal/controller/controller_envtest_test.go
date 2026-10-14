//go:build envtest

package controller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/policy"
)

var (
	restCfg *rest.Config
	direct  client.Client
	scheme  = runtime.NewScheme()
	appCfg  *config.Config
)

func TestMain(m *testing.M) {
	if err := AddToScheme(scheme); err != nil {
		panic(err)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	var err error
	if restCfg, err = env.Start(); err != nil {
		panic(err)
	}
	if direct, err = client.New(restCfg, client.Options{Scheme: scheme}); err != nil {
		panic(err)
	}
	if appCfg, err = config.Load(filepath.Join("..", "config", "testdata", "example.yaml")); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	if code != 0 {
		panic(fmt.Sprintf("exit %d", code))
	}
}

// start runs a manager with a Controller until the test ends and waits for readiness.
func start(t *testing.T, leaderElect bool, opts ...Option) *Controller {
	t.Helper()
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		LeaderElection:          leaderElect,
		LeaderElectionID:        "querdian-test-leader",
		LeaderElectionNamespace: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := New(appCfg, append([]Option{WithDebounce(50 * time.Millisecond)}, opts...)...)
	if err := c.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = mgr.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, 30*time.Second, "controller ready", c.Ready)
	return c
}

func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func capFor(name, group string, matchers ...v1.Matcher) *v1.ClusterAccessPolicy {
	return &v1.ClusterAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.ClusterAccessPolicySpec{
			Subjects: v1.Subjects{Groups: []string{group}, Users: []string{}},
			Rules:    []v1.ClusterRule{{Backends: []string{"metrics"}, Matchers: matchers, Unrestricted: len(matchers) == 0}},
		},
	}
}

func TestSnapshotFollowsPolicies(t *testing.T) {
	ctx := context.Background()
	c := start(t, false)
	group := "lifecycle-" + t.Name()
	p := capFor("lifecycle", group)
	t.Cleanup(func() { _ = direct.Delete(ctx, p) })

	gen0 := c.Snapshot().Generation
	if _, err := c.Snapshot().Resolve("u", []string{group}, "metrics"); !errors.Is(err, policy.ErrNoGrant) {
		t.Fatalf("want no grant before create, got %v", err)
	}

	if err := direct.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	eventually(t, time.Second, "create reflected", func() bool {
		eff, err := c.Snapshot().Resolve("u", []string{group}, "metrics")
		return err == nil && eff.Unrestricted
	})
	t.Logf("create reflected after %v", time.Since(t0))
	gen1 := c.Snapshot().Generation
	if gen1 <= gen0 {
		t.Fatalf("generation did not increase: %d -> %d", gen0, gen1)
	}

	if err := direct.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Rules[0].Unrestricted = false
	p.Spec.Rules[0].Matchers = []v1.Matcher{{Name: "namespace", Op: v1.MatchEqual, Value: "a"}}
	if err := direct.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, "update reflected", func() bool {
		eff, err := c.Snapshot().Resolve("u", []string{group}, "metrics")
		return err == nil && !eff.Unrestricted && len(eff.Terms) == 1
	})
	if c.Snapshot().Generation <= gen1 {
		t.Fatal("generation did not increase on update")
	}

	if err := direct.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, "delete reflected", func() bool {
		_, err := c.Snapshot().Resolve("u", []string{group}, "metrics")
		return errors.Is(err, policy.ErrNoGrant)
	})
}

func readyCondition(t *testing.T, obj client.Object, status func() *v1.PolicyStatus) *metav1.Condition {
	t.Helper()
	if err := direct.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Fatal(err)
	}
	return apimeta.FindStatusCondition(status().Conditions, ReadyCondition)
}

func TestNamespaceTenantLabelFlipsStatus(t *testing.T) {
	ctx := context.Background()
	c := start(t, false)
	_ = c
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-flip"}}
	if err := direct.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	ap := &v1.AccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns.Name},
		Spec: v1.AccessPolicySpec{
			Subjects: v1.Subjects{Groups: []string{"flip-team"}, Users: []string{}},
			Rules:    []v1.Rule{{Backends: []string{"logs"}}},
		},
	}
	if err := direct.Create(ctx, ap); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "TenantNotAssigned", func() bool {
		cond := readyCondition(t, ap, func() *v1.PolicyStatus { return &ap.Status })
		return cond != nil && cond.Reason == policy.ReasonTenantNotAssigned && cond.Status == metav1.ConditionFalse &&
			ap.Status.ObservedGeneration == ap.Generation
	})
	if _, err := c.Snapshot().Resolve("u", []string{"flip-team"}, "logs"); !errors.Is(err, policy.ErrNoGrant) {
		t.Fatalf("invalid policy must grant nothing, got %v", err)
	}

	if err := direct.Get(ctx, types.NamespacedName{Name: ns.Name}, ns); err != nil {
		t.Fatal(err)
	}
	ns.Labels = map[string]string{policy.TenantLabel: "t1"}
	if err := direct.Update(ctx, ns); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "Compiled", func() bool {
		cond := readyCondition(t, ap, func() *v1.PolicyStatus { return &ap.Status })
		return cond != nil && cond.Reason == policy.ReasonCompiled && cond.Status == metav1.ConditionTrue
	})
	eventually(t, time.Second, "grant active", func() bool {
		eff, err := c.Snapshot().Resolve("u", []string{"flip-team"}, "logs")
		return err == nil && len(eff.Tenants) == 1 && eff.Tenants[0] == "t1"
	})
}

func TestOnlyLeaderWritesStatus(t *testing.T) {
	ctx := context.Background()
	a := start(t, true)
	b := start(t, true)

	p := capFor("leader-check", "leader-group")
	if err := direct.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = direct.Delete(ctx, p) })
	eventually(t, 30*time.Second, "status written", func() bool {
		cond := readyCondition(t, p, func() *v1.PolicyStatus { return &p.Status })
		return cond != nil && cond.Reason == policy.ReasonCompiled
	})
	// Every replica built the snapshot, but only one wrote status.
	for _, c := range []*Controller{a, b} {
		eventually(t, time.Second, "replica snapshot", func() bool {
			_, err := c.Snapshot().Resolve("u", []string{"leader-group"}, "metrics")
			return err == nil
		})
	}
	pa, pb := a.StatusPatches(), b.StatusPatches()
	if (pa == 0) == (pb == 0) {
		t.Fatalf("exactly one replica must write status; patches a=%d b=%d", pa, pb)
	}
}

func TestStatusWrittenOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	c := start(t, false)
	p := capFor("no-churn", "no-churn-group")
	if err := direct.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = direct.Delete(ctx, p) })
	eventually(t, 10*time.Second, "status written", func() bool {
		cond := readyCondition(t, p, func() *v1.PolicyStatus { return &p.Status })
		return cond != nil
	})
	before := c.StatusPatches()
	// Unrelated churn triggers rebuilds but must not re-patch an unchanged status.
	other := capFor("other-churn", "other-churn-group")
	if err := direct.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = direct.Delete(ctx, other) })
	eventually(t, 10*time.Second, "other status written", func() bool {
		cond := readyCondition(t, other, func() *v1.PolicyStatus { return &other.Status })
		return cond != nil
	})
	if got := c.StatusPatches() - before; got != 1 {
		t.Fatalf("expected exactly 1 patch for the new policy, got %d", got)
	}
}

func TestRebuildEmitsSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(rec))
	c := start(t, false, WithTracerProvider(tp))
	_ = c
	var found bool
	for _, s := range rec.Ended() {
		if s.Name() != "snapshot.rebuild" {
			continue
		}
		found = true
		attrs := map[string]bool{}
		for _, a := range s.Attributes() {
			attrs[string(a.Key)] = true
		}
		for _, k := range []string{"querdian.trigger", "policies.total", "policies.invalid", "compile.duration_ms"} {
			if !attrs[k] {
				t.Errorf("span lacks attribute %s: %v", k, s.Attributes())
			}
		}
		if s.Parent().IsValid() {
			t.Error("snapshot.rebuild must be a root span")
		}
	}
	if !found {
		t.Fatalf("no snapshot.rebuild span recorded; got %d spans", len(rec.Ended()))
	}
}
