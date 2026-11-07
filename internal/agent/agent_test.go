package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func testCAP(name string, gen int64) *v1.ClusterAccessPolicy {
	return &v1.ClusterAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: gen},
		Spec: v1.ClusterAccessPolicySpec{
			Subjects: v1.Subjects{Groups: []string{"sre"}},
			Rules: []v1.ClusterRule{{Backends: []string{"metrics"},
				Matchers: []v1.Matcher{{Name: "app", Op: v1.MatchEqual, Value: "x"}}}},
		},
	}
}

func testAP(ns, name string, gen int64) *v1.AccessPolicy {
	return &v1.AccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: gen},
		Spec: v1.AccessPolicySpec{
			Subjects: v1.Subjects{Users: []string{"alice"}},
			Rules:    []v1.Rule{{Backends: []string{"logs"}}},
		},
	}
}

func testNS(name, tenant string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if tenant != "" {
		ns.Labels = map[string]string{policy.TenantLabel: tenant}
	}
	return ns
}

// spokeObjects is a small spoke state: one ClusterAccessPolicy and two AccessPolicy objects.
func spokeObjects() []client.Object {
	return []client.Object{
		testCAP("platform", 2),
		testNS("team-a", "tenant-a"), testAP("team-a", "read", 3),
		testNS("team-b", ""), testAP("team-b", "read", 1),
	}
}

func newFakeClient(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) client.WithWatch {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&v1.ClusterAccessPolicy{}, &v1.AccessPolicy{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

// startAgent runs an agent until the test ends.
func startAgent(t *testing.T, c client.Client, s Syncer, interval time.Duration) *Agent {
	t.Helper()
	a, err := New(Options{Reader: c, Writer: c, Syncer: s, Interval: interval, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	a.minBackoff = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = a.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return a
}

func newTestSender(t *testing.T, p *pki, r *fakeReceiver) *Sender {
	t.Helper()
	s, err := NewSender(r.url(), p.files, 5*time.Second, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReadState(t *testing.T) {
	ctx := context.Background()
	got, err := ReadState(ctx, newFakeClient(t, nil, spokeObjects()...))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 policies, got %d", len(got))
	}
	// Sorted by spokesync.Key: AccessPolicy before ClusterAccessPolicy, then namespace.
	a, b, c := got[0], got[1], got[2]
	if a.Kind != v1.SpokeAccessPolicy || a.Namespace != "team-a" || a.Name != "read" || a.Generation != 3 ||
		a.TenantLabel != "tenant-a" || a.AccessPolicy == nil || a.ClusterAccessPolicy != nil ||
		a.AccessPolicy.Subjects.Users[0] != "alice" {
		t.Errorf("team-a AccessPolicy: %+v", a)
	}
	if b.Kind != v1.SpokeAccessPolicy || b.Namespace != "team-b" || b.TenantLabel != "" || b.Generation != 1 {
		t.Errorf("team-b AccessPolicy: %+v", b)
	}
	if c.Kind != v1.SpokeClusterAccessPolicy || c.Namespace != "" || c.Name != "platform" || c.Generation != 2 ||
		c.ClusterAccessPolicy == nil || c.AccessPolicy != nil || c.TenantLabel != "" ||
		c.ClusterAccessPolicy.Rules[0].Matchers[0].Value != "x" {
		t.Errorf("ClusterAccessPolicy: %+v", c)
	}

	empty, err := ReadState(ctx, newFakeClient(t, nil))
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty spoke: %v %v", empty, err)
	}

	// An AccessPolicy whose namespace is not in the list makes the read incomplete.
	if _, err := ReadState(ctx, newFakeClient(t, nil, testAP("gone", "x", 1))); err == nil {
		t.Error("missing namespace must fail the read")
	}
	if _, err := ReadState(ctx, newFakeClient(t, nil, testCAP("nogen", 0))); err == nil {
		t.Error("an object with no generation must fail the read")
	}
	for _, kind := range []string{"ClusterAccessPolicyList", "AccessPolicyList", "NamespaceList"} {
		c := newFakeClient(t, &interceptor.Funcs{List: failList(kind, -1)}, spokeObjects()...)
		if _, err := ReadState(ctx, c); err == nil {
			t.Errorf("a failed %s must fail the read", kind)
		}
	}
}

// failList fails the list of kind for the first n calls (n < 0: always).
func failList(kind string, n int) func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
	var calls atomic.Int64
	return func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
		gvk, err := apiutilGVK(c, l)
		if err != nil {
			return err
		}
		if gvk == kind && (n < 0 || calls.Add(1) <= int64(n)) {
			return errors.New("injected list failure")
		}
		return c.List(ctx, l, opts...)
	}
}

func apiutilGVK(c client.WithWatch, l client.ObjectList) (string, error) {
	gvk, err := c.GroupVersionKindFor(l)
	return gvk.Kind, err
}

func TestHeartbeatThenFullState(t *testing.T) {
	p := newPKI(t)
	r := newFakeReceiver(t, p)
	startAgent(t, newFakeClient(t, nil, spokeObjects()...), newTestSender(t, p, r), 30*time.Millisecond)

	eventually(t, 5*time.Second, "three requests", func() bool { reqs, _ := r.snapshot(); return len(reqs) >= 3 })
	reqs, last := r.snapshot()
	if reqs[0].Full || len(reqs[0].Policies) != 0 || reqs[0].Digest == "" {
		t.Errorf("first request must be a heartbeat with a digest only: %+v", reqs[0])
	}
	if !reqs[1].Full || len(reqs[1].Policies) != 3 || reqs[1].Digest != reqs[0].Digest {
		t.Errorf("second request must be the full state: full=%v policies=%d", reqs[1].Full, len(reqs[1].Policies))
	}
	if reqs[2].Full || reqs[2].Digest != reqs[0].Digest {
		t.Errorf("third request must be a heartbeat with the same digest: %+v", reqs[2])
	}
	if last.IsZero() {
		t.Error("the receiver did not renew lastSync")
	}
	for _, id := range r.identities {
		if id != "spiffe://mla.example.com/agent/cluster-a" {
			t.Errorf("client identity %q", id)
		}
	}
}

func TestStateChangeSendsFullState(t *testing.T) {
	p := newPKI(t)
	r := newFakeReceiver(t, p)
	c := newFakeClient(t, nil, spokeObjects()...)
	startAgent(t, c, newTestSender(t, p, r), 30*time.Millisecond)
	eventually(t, 5*time.Second, "first full sync", func() bool { reqs, _ := r.snapshot(); return len(reqs) >= 2 })

	// A deleted policy changes the digest; the next heartbeat leads to a full state without it.
	if err := c.Delete(context.Background(), testAP("team-b", "read", 1)); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "second full sync", func() bool {
		reqs, _ := r.snapshot()
		n := 0
		for _, q := range reqs {
			if q.Full {
				n++
			}
		}
		return n >= 2
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stored) != 2 {
		t.Fatalf("receiver state has %d policies, want 2", len(r.stored))
	}
}

func TestEmptySpokeSendsEmptyFullState(t *testing.T) {
	p := newPKI(t)
	r := newFakeReceiver(t, p)
	// The receiver has a state from earlier; the spoke now has no policies after a complete read.
	r.stored = []v1.SpokePolicySource{*sourceOf(testCAP("old", 1))}
	r.digest, _ = spokesync.Digest(r.stored)
	startAgent(t, newFakeClient(t, nil), newTestSender(t, p, r), 30*time.Millisecond)
	eventually(t, 5*time.Second, "full sync", func() bool {
		reqs, _ := r.snapshot()
		return len(reqs) >= 2 && reqs[1].Full
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.stored) != 0 {
		t.Fatalf("an empty spoke after a complete read must send an empty state, receiver has %d", len(r.stored))
	}
}

func sourceOf(p *v1.ClusterAccessPolicy) *v1.SpokePolicySource {
	return &v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: p.Name, Generation: p.Generation, ClusterAccessPolicy: &p.Spec}
}

// TestNoSendWithoutAPIServer: an agent that cannot reach its API server sends nothing, so the receiver
// does not renew lastSync (PLAN §18.6, rule 2).
func TestNoSendWithoutAPIServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens at addr now
	c, err := client.New(&rest.Config{Host: "https://" + addr, Timeout: time.Second}, client.Options{Scheme: testScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	p := newPKI(t)
	r := newFakeReceiver(t, p)
	startAgent(t, c, newTestSender(t, p, r), 20*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	if reqs, last := r.snapshot(); len(reqs) != 0 || !last.IsZero() {
		t.Fatalf("agent without an API server sent %d requests (lastSync %v)", len(reqs), last)
	}
}

// TestNoSendBeforeFirstCompleteRead: the first request comes only after a complete read, and it never
// carries a partial state.
func TestNoSendBeforeFirstCompleteRead(t *testing.T) {
	for _, kind := range []string{"ClusterAccessPolicyList", "AccessPolicyList", "NamespaceList"} {
		t.Run(kind, func(t *testing.T) {
			p := newPKI(t)
			r := newFakeReceiver(t, p)
			var lists atomic.Int64
			fail := failList(kind, 5)
			funcs := &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, o ...client.ListOption) error {
				lists.Add(1)
				return fail(ctx, c, l, o...)
			}}
			// A long interval: only the retry after the read failures can lead to a send.
			startAgent(t, newFakeClient(t, funcs, spokeObjects()...), newTestSender(t, p, r), time.Hour)
			eventually(t, 5*time.Second, "full sync", func() bool {
				reqs, _ := r.snapshot()
				return len(reqs) >= 2
			})
			reqs, last := r.snapshot()
			if lists.Load() < 6 {
				t.Errorf("only %d list calls; the agent did not retry the read", lists.Load())
			}
			if !reqs[1].Full || len(reqs[1].Policies) != 3 {
				t.Fatalf("the first full state must be complete: full=%v policies=%d", reqs[1].Full, len(reqs[1].Policies))
			}
			if last.IsZero() {
				t.Error("lastSync not renewed after the complete read")
			}
		})
	}
}

// flakySyncer fails the first n requests with a network error.
type flakySyncer struct {
	next  Syncer
	n     atomic.Int64
	calls atomic.Int64
}

func (f *flakySyncer) Send(ctx context.Context, req *spokesync.Request) (*spokesync.Response, error) {
	f.calls.Add(1)
	if f.n.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	return f.next.Send(ctx, req)
}

func TestRetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject []int
		onFull bool
	}{
		{name: "409 on heartbeat", inject: []int{http.StatusConflict, http.StatusConflict}},
		{name: "409 on full state", onFull: true},
		{name: "429", inject: []int{http.StatusTooManyRequests}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPKI(t)
			r := newFakeReceiver(t, p)
			var syncer Syncer = newTestSender(t, p, r)
			if tc.onFull {
				syncer = &injectFull{Syncer: syncer, r: r}
			}
			r.setInject(tc.inject...)
			// A long interval: only a retry can reach the ok state in the test time.
			startAgent(t, newFakeClient(t, nil, spokeObjects()...), syncer, time.Hour)
			eventually(t, 5*time.Second, "accepted sync", func() bool { _, last := r.snapshot(); return !last.IsZero() })
			reqs, _ := r.snapshot()
			if len(reqs) < 3 || !reqs[len(reqs)-1].Full {
				t.Errorf("want the retries and then a full state, got %d requests", len(reqs))
			}
		})
	}
	t.Run("network error", func(t *testing.T) {
		p := newPKI(t)
		r := newFakeReceiver(t, p)
		f := &flakySyncer{next: newTestSender(t, p, r)}
		f.n.Store(3)
		startAgent(t, newFakeClient(t, nil, spokeObjects()...), f, time.Hour)
		eventually(t, 5*time.Second, "accepted sync", func() bool { _, last := r.snapshot(); return !last.IsZero() })
		if f.calls.Load() < 5 {
			t.Errorf("calls = %d, want 3 failures, a heartbeat and a full state", f.calls.Load())
		}
	})
	t.Run("other error waits for the next heartbeat", func(t *testing.T) {
		p := newPKI(t)
		r := newFakeReceiver(t, p)
		r.setInject(http.StatusInternalServerError)
		startAgent(t, newFakeClient(t, nil, spokeObjects()...), newTestSender(t, p, r), time.Hour)
		eventually(t, 5*time.Second, "first request", func() bool { reqs, _ := r.snapshot(); return len(reqs) == 1 })
		time.Sleep(300 * time.Millisecond)
		if reqs, last := r.snapshot(); len(reqs) != 1 || !last.IsZero() {
			t.Fatalf("after a 500, want no retry before the next heartbeat; got %d requests", len(reqs))
		}
	})
}

// injectFull replies 409 to the first full state.
type injectFull struct {
	Syncer
	r    *fakeReceiver
	done atomic.Bool
}

func (s *injectFull) Send(ctx context.Context, req *spokesync.Request) (*spokesync.Response, error) {
	if req.Full && !s.done.Swap(true) {
		s.r.setInject(http.StatusConflict)
	}
	return s.Syncer.Send(ctx, req)
}

func TestRetryDelay(t *testing.T) {
	a, err := New(Options{Reader: newFakeClient(t, nil), Writer: newFakeClient(t, nil), Syncer: &flakySyncer{}, Interval: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if a.retryCap() != time.Minute {
		t.Errorf("retry cap with a 5m interval = %v, want 1m", a.retryCap())
	}
	a.interval = 20 * time.Second
	if a.retryCap() != 20*time.Second {
		t.Errorf("retry cap with a 20s interval = %v", a.retryCap())
	}
	var b time.Duration
	var seen []time.Duration
	for range 10 {
		b = nextBackoff(b, time.Second, time.Minute)
		seen = append(seen, b)
	}
	if seen[0] != time.Second || seen[1] != 2*time.Second || seen[9] != time.Minute {
		t.Errorf("backoff sequence %v", seen)
	}
	if retryDelay(time.Second, 10*time.Second, time.Minute) != 10*time.Second {
		t.Error("Retry-After must make the delay longer")
	}
	if retryDelay(time.Second, time.Hour, time.Minute) != time.Minute {
		t.Error("Retry-After must not make the delay longer than one minute")
	}
	if _, err := New(Options{}); err == nil {
		t.Error("New without options must fail")
	}
	if _, err := New(Options{Reader: a.reader, Writer: a.writer, Syncer: a.syncer, Interval: -1}); err == nil {
		t.Error("negative interval must fail")
	}
}

func TestStatusWriteBack(t *testing.T) {
	ctx := context.Background()
	p := newPKI(t)
	r := newFakeReceiver(t, p)
	var patches atomic.Int64
	c := newFakeClient(t, &interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			patches.Add(1)
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}, spokeObjects()...)
	r.conditions = func([]v1.SpokePolicySource) []spokesync.Condition {
		return []spokesync.Condition{
			{Kind: v1.SpokeClusterAccessPolicy, Name: "platform", SourceGeneration: 2, Ready: true, Reason: "Compiled"},
			{Kind: v1.SpokeAccessPolicy, Namespace: "team-a", Name: "read", SourceGeneration: 2, Ready: false,
				Reason: "TenantNotAllowed", Message: "tenant tenant-a is not allowed"},
			// No such object: skipped.
			{Kind: v1.SpokeAccessPolicy, Namespace: "team-a", Name: "deleted", SourceGeneration: 1, Ready: true, Reason: "Compiled"},
			// Newer than the object: a different object with the same name. Skipped.
			{Kind: v1.SpokeAccessPolicy, Namespace: "team-b", Name: "read", SourceGeneration: 7, Ready: true, Reason: "Compiled"},
			// Invalid input: skipped.
			{Kind: "Role", Name: "x", SourceGeneration: 1, Reason: "Compiled"},
			{Kind: v1.SpokeClusterAccessPolicy, Name: "platform", SourceGeneration: 2, Reason: "bad reason!"},
		}
	}
	startAgent(t, c, newTestSender(t, p, r), 20*time.Millisecond)

	eventually(t, 5*time.Second, "status written", func() bool {
		ap := &v1.AccessPolicy{}
		_ = c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "read"}, ap)
		return apimeta.FindStatusCondition(ap.Status.Conditions, ReadyCondition) != nil
	})
	cp := &v1.ClusterAccessPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Name: "platform"}, cp); err != nil {
		t.Fatal(err)
	}
	cc := apimeta.FindStatusCondition(cp.Status.Conditions, ReadyCondition)
	if cc == nil || cc.Status != metav1.ConditionTrue || cc.Reason != "Compiled" || cc.ObservedGeneration != 2 ||
		cp.Status.ObservedGeneration != 2 {
		t.Errorf("ClusterAccessPolicy status: %+v", cp.Status)
	}
	ap := &v1.AccessPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "read"}, ap); err != nil {
		t.Fatal(err)
	}
	ac := apimeta.FindStatusCondition(ap.Status.Conditions, ReadyCondition)
	// The condition describes generation 2; the object is at generation 3. The condition keeps 2.
	if ac == nil || ac.Status != metav1.ConditionFalse || ac.Reason != "TenantNotAllowed" ||
		ac.Message != "tenant tenant-a is not allowed" || ac.ObservedGeneration != 2 || ap.Status.ObservedGeneration != 2 {
		t.Errorf("AccessPolicy status: %+v", ap.Status)
	}
	other := &v1.AccessPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "team-b", Name: "read"}, other); err != nil {
		t.Fatal(err)
	}
	if len(other.Status.Conditions) != 0 {
		t.Errorf("a condition with a higher generation must be skipped: %+v", other.Status)
	}

	// More heartbeats with the same conditions write nothing.
	n := patches.Load()
	reqs, _ := r.snapshot()
	eventually(t, 5*time.Second, "more heartbeats", func() bool { more, _ := r.snapshot(); return len(more) >= len(reqs)+3 })
	if patches.Load() != n {
		t.Errorf("unchanged conditions were written again: %d patches, want %d", patches.Load(), n)
	}
	if n != 2 {
		t.Errorf("want 2 status patches, got %d", n)
	}
}
