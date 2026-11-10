package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

// TestNoRenewAfterAPIServerLoss: an agent that loses its API server after a good sync sends nothing more,
// so the receiver does not renew lastSync with old data (PLAN §18.6, rule 2). When the API server returns,
// the sync starts again.
func TestNoRenewAfterAPIServerLoss(t *testing.T) {
	p := newPKI(t)
	r := newFakeReceiver(t, p)
	var down atomic.Bool
	funcs := &interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, o ...client.ListOption) error {
		if down.Load() {
			return errors.New("connection refused")
		}
		return c.List(ctx, l, o...)
	}}
	startAgent(t, newFakeClient(t, funcs, spokeObjects()...), newTestSender(t, p, r), 20*time.Millisecond)
	eventually(t, 5*time.Second, "good syncs", func() bool { reqs, _ := r.snapshot(); return len(reqs) >= 4 })

	down.Store(true)
	time.Sleep(100 * time.Millisecond) // let a request in progress finish
	before, last := r.snapshot()
	time.Sleep(500 * time.Millisecond) // many heartbeat intervals and retries
	after, lastAfter := r.snapshot()
	if len(after) != len(before) || !lastAfter.Equal(last) {
		t.Fatalf("agent without its API server sent %d more requests; lastSync %v -> %v", len(after)-len(before), last, lastAfter)
	}

	down.Store(false)
	eventually(t, 5*time.Second, "sync after the API server returns", func() bool {
		_, l := r.snapshot()
		return l.After(last)
	})
}

// TestStatusConflictRetried: a status patch that fails with a conflict is written at the next heartbeat.
func TestStatusConflictRetried(t *testing.T) {
	ctx := context.Background()
	p := newPKI(t)
	r := newFakeReceiver(t, p)
	var patches atomic.Int64
	c := newFakeClient(t, &interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if patches.Add(1) == 1 {
				gr := schema.GroupResource{Group: v1.GroupVersion.Group, Resource: "clusteraccesspolicies"}
				return apierrors.NewConflict(gr, obj.GetName(), errors.New("changed"))
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}, testCAP("platform", 2))
	r.conditions = func([]v1.SpokePolicySource) []spokesync.Condition {
		return []spokesync.Condition{{Kind: v1.SpokeClusterAccessPolicy, Name: "platform", SourceGeneration: 2, Ready: true, Reason: "Compiled"}}
	}
	startAgent(t, c, newTestSender(t, p, r), 20*time.Millisecond)
	eventually(t, 5*time.Second, "status written after a conflict", func() bool {
		cp := &v1.ClusterAccessPolicy{}
		_ = c.Get(ctx, client.ObjectKey{Name: "platform"}, cp)
		return apimeta.IsStatusConditionTrue(cp.Status.Conditions, ReadyCondition)
	})
	if patches.Load() < 2 {
		t.Fatalf("want a conflict and a second patch, got %d patches", patches.Load())
	}
}

// TestStatusNeverGoesBack: a condition for an older generation than the status shows is not written.
func TestStatusNeverGoesBack(t *testing.T) {
	ctx := context.Background()
	p := newPKI(t)
	r := newFakeReceiver(t, p)
	obj := testCAP("platform", 3)
	obj.Status = v1.PolicyStatus{ObservedGeneration: 3, Conditions: []metav1.Condition{{
		Type: ReadyCondition, Status: metav1.ConditionTrue, Reason: "Compiled", ObservedGeneration: 3,
		LastTransitionTime: metav1.Now(),
	}}}
	var patches atomic.Int64
	c := newFakeClient(t, &interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			patches.Add(1)
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}, obj)
	r.conditions = func([]v1.SpokePolicySource) []spokesync.Condition {
		return []spokesync.Condition{{Kind: v1.SpokeClusterAccessPolicy, Name: "platform", SourceGeneration: 2, Reason: "Invalid"}}
	}
	startAgent(t, c, newTestSender(t, p, r), 20*time.Millisecond)
	eventually(t, 5*time.Second, "heartbeats", func() bool { reqs, _ := r.snapshot(); return len(reqs) >= 4 })
	cp := &v1.ClusterAccessPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Name: "platform"}, cp); err != nil {
		t.Fatal(err)
	}
	if patches.Load() != 0 || cp.Status.ObservedGeneration != 3 || !apimeta.IsStatusConditionTrue(cp.Status.Conditions, ReadyCondition) {
		t.Fatalf("status went back: patches=%d status=%+v", patches.Load(), cp.Status)
	}
}

// TestReadRejectsNewerNamespace: a namespace that is newer than its AccessPolicy was deleted and created
// again between the two lists. Its tenant label can be different, so the read fails.
func TestReadRejectsNewerNamespace(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		apAge   time.Duration
		wantErr bool
	}{
		{"namespace newer", -time.Hour, true},
		{"namespace older", time.Hour, false},
	} {
		ns := testNS("team-a", "tenant-x")
		ns.CreationTimestamp = metav1.NewTime(now)
		ap := testAP("team-a", "read", 1)
		ap.CreationTimestamp = metav1.NewTime(now.Add(tc.apAge))
		_, err := ReadState(context.Background(), newFakeClient(t, nil, ns, ap))
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, want error %v", tc.name, err, tc.wantErr)
		}
	}
}

// TestStatusSkipsRecreatedObject: an object that was deleted and created again after the read has a
// different UID, and its generation can be equal or higher. The agent does not write the condition of the
// old object to the new object.
func TestStatusSkipsRecreatedObject(t *testing.T) {
	ctx := context.Background()
	var patches atomic.Int64
	obj := testCAP("platform", 2)
	obj.UID = "uid-new"
	c := newFakeClient(t, &interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			patches.Add(1)
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}, obj)
	a, err := New(Options{Reader: c, Writer: c, Syncer: syncFunc(nil), Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	cond := spokesync.Condition{Kind: v1.SpokeClusterAccessPolicy, Name: "platform", SourceGeneration: 2, Ready: true, Reason: "Compiled"}
	key := spokesync.Key(&v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: "platform"})
	for _, uid := range []types.UID{"uid-old", ""} {
		known := map[string]readObject{key: {uid: uid, status: &v1.PolicyStatus{}}}
		if err := a.writeCondition(ctx, &cond, known); err != nil {
			t.Fatal(err)
		}
	}
	cp := &v1.ClusterAccessPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Name: "platform"}, cp); err != nil {
		t.Fatal(err)
	}
	if patches.Load() != 0 || len(cp.Status.Conditions) != 0 {
		t.Fatalf("condition written to a different object: patches=%d status=%+v", patches.Load(), cp.Status)
	}

	// The same UID: the agent writes the condition.
	known := map[string]readObject{key: {uid: "uid-new", status: &v1.PolicyStatus{}}}
	if err := a.writeCondition(ctx, &cond, known); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "platform"}, cp); err != nil {
		t.Fatal(err)
	}
	if patches.Load() != 1 || !apimeta.IsStatusConditionTrue(cp.Status.Conditions, ReadyCondition) {
		t.Fatalf("condition not written: patches=%d status=%+v", patches.Load(), cp.Status)
	}
}
