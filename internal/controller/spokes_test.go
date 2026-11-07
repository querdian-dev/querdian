package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/policy"
)

func mcTestConfig() *config.Config {
	d := config.Duration(time.Hour)
	return &config.Config{Multicluster: &config.Multicluster{HubCluster: "mla", MaxStaleness: &d}}
}

// syncedSpoke is a SpokeCluster with a valid sync now.
func syncedSpoke(name, identity string) *v1.SpokeCluster {
	now := metav1.Now()
	return &v1.SpokeCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 2},
		Spec:       v1.SpokeClusterSpec{Identity: identity},
		Status:     v1.SpokeClusterStatus{LastSyncTime: &now, ObservedGeneration: 2},
	}
}

func testHandler(t *testing.T, objs ...*v1.SpokeCluster) (*spokeClusterHandler, *policy.Deadlines) {
	t.Helper()
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithObjects(o.DeepCopy())
	}
	dl := policy.NewDeadlines()
	c := New(mcTestConfig(), WithDeadlines(dl))
	c.reader = b.Build()
	return newSpokeClusterHandler(context.Background(), c), dl
}

func TestSpokeHandlerFeedsTable(t *testing.T) {
	h, dl := testHandler(t)
	sc := syncedSpoke("cluster-a", "spiffe://mla.example.com/agent/cluster-a")
	h.OnAdd(sc, true)
	if dl.Expired("cluster-a", time.Now()) {
		t.Fatal("a fresh spoke must not be expired")
	}
	if len(h.c.trigger) != 1 {
		t.Fatal("an add must start a recompile")
	}
	<-h.c.trigger

	// A heartbeat changes the table but starts no recompile.
	old := sc.DeepCopy()
	sc.Status.LastSyncTime = &metav1.Time{Time: time.Now().Add(-2 * time.Hour)}
	h.OnUpdate(old, sc)
	if !dl.Expired("cluster-a", time.Now()) {
		t.Fatal("an old lastSyncTime must reach the table")
	}
	if len(h.c.trigger) != 0 {
		t.Fatal("a heartbeat must not start a recompile")
	}

	// A spec change starts a recompile.
	old = sc.DeepCopy()
	sc.Spec.Tenants = []string{"t1"}
	h.OnUpdate(old, sc)
	if len(h.c.trigger) != 1 {
		t.Fatal("a spec change must start a recompile")
	}
	<-h.c.trigger

	h.OnDelete(sc)
	if _, ok := dl.Ages(time.Now())["cluster-a"]; ok {
		t.Fatal("a delete must remove the entry")
	}
	if len(h.c.trigger) != 1 {
		t.Fatal("a delete must start a recompile")
	}
}

// A SpokeCluster that the compiler must reject gets no entry, so its spoke stays expired even if a defect in
// the compiler gives it rules.
func TestSpokeHandlerRejectsUntrustedSpokes(t *testing.T) {
	for name, sc := range map[string]*v1.SpokeCluster{
		"hub name":               syncedSpoke("mla", "spiffe://mla.example.com/agent/mla"),
		"identity mismatch":      syncedSpoke("cluster-a", "spiffe://mla.example.com/agent/cluster-b"),
		"identity not canonical": syncedSpoke("cluster-a", "mla.example.com/agent/cluster-a"),
		"empty identity":         syncedSpoke("cluster-a", ""),
	} {
		t.Run(name, func(t *testing.T) {
			h, dl := testHandler(t)
			h.OnAdd(sc, true)
			if _, ok := dl.Ages(time.Now())[sc.Name]; ok || !dl.Expired(sc.Name, time.Now()) {
				t.Fatal("an untrusted SpokeCluster must have no entry")
			}
			// A valid entry also goes away when the object becomes untrusted.
			good := syncedSpoke(sc.Name, "spiffe://mla.example.com/agent/"+sc.Name)
			if sc.Name == "mla" {
				return
			}
			h.OnAdd(good, true)
			if dl.Expired(sc.Name, time.Now()) {
				t.Fatal("the trusted object must not be expired")
			}
			h.OnUpdate(good, sc)
			if _, ok := dl.Ages(time.Now())[sc.Name]; ok {
				t.Fatal("an update to an untrusted object must remove the entry")
			}
		})
	}
}

func TestSpokeHandlerTombstones(t *testing.T) {
	h, dl := testHandler(t)
	a := syncedSpoke("cluster-a", "spiffe://mla.example.com/agent/cluster-a")
	b := syncedSpoke("cluster-b", "spiffe://mla.example.com/agent/cluster-b")
	h.OnAdd(a, true)
	h.OnAdd(b, true)
	h.OnDelete(toolscache.DeletedFinalStateUnknown{Key: "cluster-a", Obj: a})
	h.OnDelete(toolscache.DeletedFinalStateUnknown{Key: "cluster-b"})
	if n := len(dl.Ages(time.Now())); n != 0 {
		t.Fatalf("tombstones must remove the entries, %d remain", n)
	}
}

// If an event has no usable name, the handler removes each entry whose SpokeCluster is not in the cache.
func TestSpokeHandlerUnknownEventPrunes(t *testing.T) {
	kept := syncedSpoke("cluster-a", "spiffe://mla.example.com/agent/cluster-a")
	h, dl := testHandler(t, kept)
	h.OnAdd(kept, true)
	h.OnAdd(syncedSpoke("cluster-b", "spiffe://mla.example.com/agent/cluster-b"), true)
	h.OnDelete(toolscache.DeletedFinalStateUnknown{Key: "a/b/c"})
	ages := dl.Ages(time.Now())
	if _, ok := ages["cluster-b"]; ok {
		t.Fatal("the entry of a spoke that is not in the cache must go")
	}
	if _, ok := ages["cluster-a"]; !ok {
		t.Fatal("the entry of a spoke in the cache must stay")
	}
	// An object of the wrong type in an update also counts as an event with no name.
	h.OnUpdate(kept, "not an object")
	if _, ok := dl.Ages(time.Now())["cluster-a"]; !ok {
		t.Fatal("the entry of a spoke in the cache must stay")
	}
}

func TestSpokeHandlerWithoutDefaultMaxStalenessExpires(t *testing.T) {
	h, dl := testHandler(t)
	h.c.cfg.Multicluster.MaxStaleness = nil
	h = newSpokeClusterHandler(context.Background(), h.c)
	h.OnAdd(syncedSpoke("cluster-a", "spiffe://mla.example.com/agent/cluster-a"), true)
	if !dl.Expired("cluster-a", time.Now()) {
		t.Fatal("with no maxStaleness, the spoke must be expired")
	}
}

func TestSpokeSpecChanged(t *testing.T) {
	a := syncedSpoke("cluster-a", "spiffe://mla.example.com/agent/cluster-a")
	b := a.DeepCopy()
	b.Status.ObservedGeneration = 9
	b.ResourceVersion = "42"
	if spokeSpecChanged(a, b) {
		t.Fatal("a status change is not a spec change")
	}
	b = a.DeepCopy()
	b.Spec.DefaultTenant = "t"
	if !spokeSpecChanged(a, b) {
		t.Fatal("a defaultTenant change is a spec change")
	}
	b = a.DeepCopy()
	b.Generation++
	if !spokeSpecChanged(a, b) {
		t.Fatal("a generation change counts as a spec change")
	}
	if !spokeSpecChanged(a, "x") {
		t.Fatal("an unknown object must count as a change")
	}
}

func TestReadyWaitsForDeadlineTable(t *testing.T) {
	dl := policy.NewDeadlines()
	c := New(mcTestConfig(), WithDeadlines(dl))
	if c.Ready() {
		t.Fatal("not ready without a snapshot")
	}
	c.current.Store(policy.NewSnapshot(policy.Compile(policy.Input{Config: c.cfg}), c.cfg, 1))
	if c.Ready() {
		t.Fatal("with the multicluster block, a replica must wait for the deadline table")
	}
	dl.MarkComplete()
	if !c.Ready() {
		t.Fatal("ready after the table is complete")
	}

	// Without the block, the table is not used.
	single := New(&config.Config{})
	single.current.Store(policy.NewSnapshot(policy.Compile(policy.Input{Config: single.cfg}), single.cfg, 1))
	if !single.Ready() {
		t.Fatal("without the block, the snapshot alone makes the replica ready")
	}
}

func TestStatusTargetSourceGeneration(t *testing.T) {
	var st v1.SpokePolicyStatus
	tg := statusTarget{observed: &st.ObservedGeneration, conditions: &st.Conditions, source: &st.SourceGeneration, sourceWant: 4}
	r := policy.Result{Kind: policy.KindSpokePolicy, Name: "x", Generation: 3, Reason: policy.ReasonTenantNotAllowed, Message: "full message"}
	if !tg.differs(r) {
		t.Fatal("an empty status differs")
	}
	tg.set(r)
	if st.ObservedGeneration != 3 || st.SourceGeneration != 4 || len(st.Conditions) != 1 ||
		st.Conditions[0].Message != "full message" || st.Conditions[0].ObservedGeneration != 3 {
		t.Fatalf("unexpected status %+v", st)
	}
	if tg.differs(r) {
		t.Fatal("the status equals the result")
	}
	tg.sourceWant = 5
	if !tg.differs(r) {
		t.Fatal("a new spoke generation must give a patch")
	}
}

func TestSetupNeedsDeadlinesWithMulticluster(t *testing.T) {
	c := New(mcTestConfig())
	if err := c.SetupWithManager(nil); err == nil {
		t.Fatal("the multicluster block without a deadline table must fail")
	}
}
