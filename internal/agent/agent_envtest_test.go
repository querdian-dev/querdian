//go:build envtest

package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

var (
	env      *envtest.Environment
	adminCfg *rest.Config
	admin    client.Client
)

func TestMain(m *testing.M) {
	env = &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	var err error
	if adminCfg, err = env.Start(); err != nil {
		panic(err)
	}
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, v1.AddToScheme, rbacv1.AddToScheme, coordinationv1.AddToScheme} {
		if err := add(s); err != nil {
			panic(err)
		}
	}
	if admin, err = client.New(adminCfg, client.Options{Scheme: s}); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	if code != 0 {
		panic(fmt.Sprintf("exit %d", code))
	}
}

var runs atomic.Int64

// uniq returns base with a suffix. envtest has no namespace controller, so a deleted namespace stays, and
// a second run with -count needs new names.
func uniq(base string) string { return fmt.Sprintf("%s-%d", base, runs.Add(1)) }

func mustCreate(t *testing.T, objs ...client.Object) {
	t.Helper()
	for _, o := range objs {
		if err := admin.Create(context.Background(), o); err != nil {
			t.Fatalf("create %T %s: %v", o, o.GetName(), err)
		}
		t.Cleanup(func() { _ = admin.Delete(context.Background(), o) })
	}
}

// agentUser creates a user with exactly the RBAC in the package comment and returns a client for it.
func agentUser(t *testing.T, name, leaseNamespace string) (*rest.Config, client.Client) {
	t.Helper()
	mustCreate(t,
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{v1.GroupVersion.Group}, Resources: []string{"clusteraccesspolicies", "accesspolicies"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{v1.GroupVersion.Group}, Resources: []string{"clusteraccesspolicies/status", "accesspolicies/status"}, Verbs: []string{"update", "patch"}},
		}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: leaseNamespace, Name: name}, Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{coordinationv1.GroupName}, Resources: []string{"leases"}, Verbs: []string{"get", "create", "update"}},
		}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: leaseNamespace, Name: name},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}}},
	)
	u, err := env.AddUser(envtest.User{Name: name}, adminCfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg := u.Config()
	c, err := client.New(cfg, client.Options{Scheme: admin.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, c
}

// TestAgentWithMinimalRBAC runs the agent against a real API server, with leader election and only the
// RBAC of the package comment. It checks the full state, the lease and the status write-back.
func TestAgentWithMinimalRBAC(t *testing.T) {
	ctx := context.Background()
	agentNS, nsA, nsB := uniq("querdian-agent"), uniq("env-team-a"), uniq("env-team-b")
	mustCreate(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: agentNS}},
		testNS(nsA, "tenant-a"), testNS(nsB, ""))
	capObj, apA, apB := testCAP("env-platform", 0), testAP(nsA, "read", 0), testAP(nsB, "read", 0)
	capObj.Spec.Subjects.Users = []string{}
	mustCreate(t, capObj, apA, apB)
	cfg, c := agentUser(t, "querdian-agent-test", agentNS)

	p := newPKI(t)
	r := newFakeReceiver(t, p)
	r.conditions = func(stored []v1.SpokePolicySource) []spokesync.Condition {
		out := make([]spokesync.Condition, 0, len(stored))
		for _, s := range stored {
			out = append(out, spokesync.Condition{Kind: s.Kind, Namespace: s.Namespace, Name: s.Name,
				SourceGeneration: s.Generation, Ready: s.Namespace != nsB, Reason: "Compiled"})
		}
		return out
	}
	a, err := New(Options{Reader: c, Writer: c, Syncer: newTestSender(t, p, r), Interval: 200 * time.Millisecond, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- RunWithLeaderElection(rctx, LeaderElection{Config: cfg, Namespace: agentNS, Name: "agent-test",
			Identity: "agent-1", Logger: testLogger()}, a.Run)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	eventually(t, 30*time.Second, "full state", func() bool {
		_, last := r.snapshot()
		return !last.IsZero()
	})
	r.mu.Lock()
	stored := append([]v1.SpokePolicySource(nil), r.stored...)
	r.mu.Unlock()
	byKey := map[string]v1.SpokePolicySource{}
	for _, s := range stored {
		byKey[spokesync.Key(&s)] = s
	}
	got, ok := byKey[spokesync.Key(&v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Namespace: nsA, Name: "read"})]
	if !ok || got.TenantLabel != "tenant-a" || got.Generation != 1 {
		t.Errorf("env-team-a AccessPolicy: %+v", got)
	}
	gotCAP, ok := byKey[spokesync.Key(&v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: "env-platform"})]
	if !ok || gotCAP.TenantLabel != "" || gotCAP.Namespace != "" {
		t.Errorf("ClusterAccessPolicy: %+v", gotCAP)
	}
	// The digest of the API server objects equals the digest of what the receiver stored.
	if d, _ := spokesync.Digest(stored); d != r.digest {
		t.Error("stored digest differs")
	}

	lease := &coordinationv1.Lease{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: agentNS, Name: "agent-test"}, lease); err != nil {
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "agent-1" {
		t.Errorf("lease holder: %v", lease.Spec.HolderIdentity)
	}

	eventually(t, 30*time.Second, "status written", func() bool {
		ap := &v1.AccessPolicy{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(apB), ap); err != nil {
			return false
		}
		cond := apimeta.FindStatusCondition(ap.Status.Conditions, ReadyCondition)
		return cond != nil && cond.Status == metav1.ConditionFalse && cond.ObservedGeneration == 1
	})
	// The agent writes the conditions one after the other; the ClusterAccessPolicy can come later.
	eventually(t, 30*time.Second, "ClusterAccessPolicy status written", func() bool {
		cp := &v1.ClusterAccessPolicy{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(capObj), cp); err != nil {
			return false
		}
		cond := apimeta.FindStatusCondition(cp.Status.Conditions, ReadyCondition)
		return cond != nil && cond.Status == metav1.ConditionTrue && cond.ObservedGeneration == 1 &&
			cp.Status.ObservedGeneration == 1
	})

	// A spec change: the next heartbeat sends the new generation.
	if err := admin.Get(ctx, client.ObjectKeyFromObject(apA), apA); err != nil {
		t.Fatal(err)
	}
	apA.Spec.Rules[0].Backends = []string{"logs", "metrics"}
	if err := admin.Update(ctx, apA); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "new generation sent", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, s := range r.stored {
			if s.Namespace == nsA && s.Generation == 2 {
				return true
			}
		}
		return false
	})

	// The RBAC has no write access to the spec of a policy.
	ap := &v1.AccessPolicy{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(apB), ap); err != nil {
		t.Fatal(err)
	}
	ap.Spec.Subjects.Users = []string{"mallory"}
	if err := c.Update(ctx, ap); !apierrors.IsForbidden(err) {
		t.Errorf("spec update by the agent: want Forbidden, got %v", err)
	}
}

// countSyncer counts requests and returns ok.
type countSyncer struct{ n atomic.Int64 }

func (s *countSyncer) Send(context.Context, *spokesync.Request) (*spokesync.Response, error) {
	s.n.Add(1)
	return &spokesync.Response{Result: spokesync.ResultOK}, nil
}

// TestLeaderElection: of two agents, only the lease holder sends. When it stops, the other one takes over.
func TestLeaderElection(t *testing.T) {
	ctx := context.Background()
	leNS := uniq("querdian-agent-le")
	mustCreate(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: leNS}})
	cfg, c := agentUser(t, "querdian-agent-le", leNS)

	type runner struct {
		syncer *countSyncer
		cancel context.CancelFunc
		done   chan error
	}
	start := func(id string) *runner {
		s := &countSyncer{}
		a, err := New(Options{Reader: c, Writer: c, Syncer: s, Interval: 50 * time.Millisecond, Logger: testLogger()})
		if err != nil {
			t.Fatal(err)
		}
		rctx, cancel := context.WithCancel(ctx)
		r := &runner{syncer: s, cancel: cancel, done: make(chan error, 1)}
		go func() {
			r.done <- RunWithLeaderElection(rctx, LeaderElection{Config: cfg, Namespace: leNS, Name: "agent-le",
				Identity: id, LeaseDuration: 2 * time.Second, RenewDeadline: time.Second, RetryPeriod: 200 * time.Millisecond,
				Logger: testLogger()}, a.Run)
		}()
		t.Cleanup(func() {
			cancel()
			if err := <-r.done; err != nil {
				t.Error(err)
			}
		})
		return r
	}
	first := start("agent-a")
	eventually(t, 30*time.Second, "first agent sends", func() bool { return first.syncer.n.Load() > 0 })
	second := start("agent-b")
	time.Sleep(time.Second)
	if n := second.syncer.n.Load(); n != 0 {
		t.Fatalf("the agent without the lease sent %d requests", n)
	}

	first.cancel()
	if err := <-first.done; err != nil {
		t.Fatal(err)
	}
	first.done <- nil // for the cleanup
	stopped := first.syncer.n.Load()
	eventually(t, 30*time.Second, "second agent takes over", func() bool { return second.syncer.n.Load() > 0 })
	time.Sleep(200 * time.Millisecond)
	if first.syncer.n.Load() != stopped {
		t.Error("the stopped agent still sends")
	}
}
