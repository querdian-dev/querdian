package controller

import (
	"context"
	"fmt"
	"sync/atomic"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/policy"
)

// ReadyCondition is the single condition type written on policies.
const ReadyCondition = "Ready"

// statusWriter patches status.conditions and observedGeneration, only when something changed. It is a
// leader-elected runnable: with several replicas only the leader writes.
type statusWriter struct {
	c       *Controller
	client  client.Client
	patches atomic.Int64 // number of status patches issued; for tests and metrics
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (w *statusWriter) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (w *statusWriter) Start(ctx context.Context) error {
	// A snapshot may exist already (this replica just became leader); write once, then follow changes.
	w.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.c.changed:
			w.pass(ctx)
		}
	}
}

func (w *statusWriter) pass(ctx context.Context) {
	snap := w.c.Snapshot()
	if snap == nil {
		return
	}
	for _, r := range snap.Results() {
		// Errors (conflicts, deleted objects) are retried on the next snapshot.
		_, _ = w.apply(ctx, r)
	}
}

// apply patches one object if its status differs from the result. It reports whether a patch was sent.
func (w *statusWriter) apply(ctx context.Context, r policy.Result) (bool, error) {
	var obj client.Object
	var status *v1.PolicyStatus
	key := types.NamespacedName{Namespace: r.Namespace, Name: r.Name}
	switch r.Kind {
	case "ClusterAccessPolicy":
		o := &v1.ClusterAccessPolicy{}
		if err := w.client.Get(ctx, key, o); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		obj, status = o, &o.Status
	case "AccessPolicy":
		o := &v1.AccessPolicy{}
		if err := w.client.Get(ctx, key, o); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		obj, status = o, &o.Status
	default:
		return false, fmt.Errorf("unknown kind %q", r.Kind)
	}
	if obj.GetGeneration() != r.Generation {
		return false, nil // the object changed since compilation; the next snapshot covers it
	}
	if !statusDiffers(status, r) {
		return false, nil
	}
	base := obj.DeepCopyObject().(client.Object)
	cond := metav1.ConditionFalse
	if r.Ready {
		cond = metav1.ConditionTrue
	}
	status.ObservedGeneration = r.Generation
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: ReadyCondition, Status: cond, Reason: r.Reason, Message: r.Message, ObservedGeneration: r.Generation,
	})
	if err := w.client.Status().Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return false, err
	}
	w.patches.Add(1)
	return true, nil
}

func statusDiffers(s *v1.PolicyStatus, r policy.Result) bool {
	if s.ObservedGeneration != r.Generation {
		return true
	}
	c := apimeta.FindStatusCondition(s.Conditions, ReadyCondition)
	if c == nil {
		return true
	}
	want := metav1.ConditionFalse
	if r.Ready {
		want = metav1.ConditionTrue
	}
	return c.Status != want || c.Reason != r.Reason || c.Message != r.Message || c.ObservedGeneration != r.Generation
}
