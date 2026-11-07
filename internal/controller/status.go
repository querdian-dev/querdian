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

// statusWriter patches status.conditions and observedGeneration (and sourceGeneration of a SpokePolicy),
// only when something changed. It is a leader-elected runnable: with several replicas only the leader
// writes.
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
//
// For a SpokePolicy, it also writes status.sourceGeneration (spec.generation, the generation of the spoke
// object). The hub object keeps the full reason and message; the receiver filters what goes back to the
// spoke (PLAN §18.5).
func (w *statusWriter) apply(ctx context.Context, r policy.Result) (bool, error) {
	var obj client.Object
	var t statusTarget
	key := types.NamespacedName{Namespace: r.Namespace, Name: r.Name}
	switch r.Kind {
	case "ClusterAccessPolicy":
		o := &v1.ClusterAccessPolicy{}
		if err := w.client.Get(ctx, key, o); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		obj, t = o, statusTarget{observed: &o.Status.ObservedGeneration, conditions: &o.Status.Conditions}
	case "AccessPolicy":
		o := &v1.AccessPolicy{}
		if err := w.client.Get(ctx, key, o); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		obj, t = o, statusTarget{observed: &o.Status.ObservedGeneration, conditions: &o.Status.Conditions}
	case policy.KindSpokePolicy:
		o := &v1.SpokePolicy{}
		if err := w.client.Get(ctx, key, o); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		obj, t = o, statusTarget{
			observed: &o.Status.ObservedGeneration, conditions: &o.Status.Conditions,
			source: &o.Status.SourceGeneration, sourceWant: o.Spec.Generation,
		}
	default:
		return false, fmt.Errorf("unknown kind %q", r.Kind)
	}
	// spec.generation of a SpokePolicy is in the spec, so a change of it also changes metadata.generation. If
	// the generations agree, sourceWant is the value that the compiler saw.
	if obj.GetGeneration() != r.Generation {
		return false, nil // the object changed since compilation; the next snapshot covers it
	}
	if !t.differs(r) {
		return false, nil
	}
	base := obj.DeepCopyObject().(client.Object)
	t.set(r)
	if err := w.client.Status().Patch(ctx, obj, client.MergeFrom(base)); err != nil {
		return false, err
	}
	w.patches.Add(1)
	return true, nil
}

// statusTarget points into the status of one fetched object.
type statusTarget struct {
	observed   *int64
	conditions *[]metav1.Condition
	// source is status.sourceGeneration of a SpokePolicy and nil for the hub kinds.
	source     *int64
	sourceWant int64
}

func readyStatus(r policy.Result) metav1.ConditionStatus {
	if r.Ready {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func (t statusTarget) differs(r policy.Result) bool {
	if *t.observed != r.Generation || (t.source != nil && *t.source != t.sourceWant) {
		return true
	}
	c := apimeta.FindStatusCondition(*t.conditions, ReadyCondition)
	if c == nil {
		return true
	}
	return c.Status != readyStatus(r) || c.Reason != r.Reason || c.Message != r.Message || c.ObservedGeneration != r.Generation
}

func (t statusTarget) set(r policy.Result) {
	*t.observed = r.Generation
	if t.source != nil {
		*t.source = t.sourceWant
	}
	apimeta.SetStatusCondition(t.conditions, metav1.Condition{
		Type: ReadyCondition, Status: readyStatus(r), Reason: r.Reason, Message: r.Message, ObservedGeneration: r.Generation,
	})
}
