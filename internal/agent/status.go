package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

// ReadyCondition is the condition type that the agent writes on spoke policies.
const ReadyCondition = "Ready"

const (
	// maxReasonLen and maxMessageLen are the limits of metav1.Condition.
	maxReasonLen  = 1024
	maxMessageLen = 32768
)

// reasonRE is the pattern of metav1.Condition.Reason.
var reasonRE = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)

// writeConditions writes the conditions from the receiver to the spoke objects with actx. known holds the
// UID and the status of each object from the read of this attempt, by spokesync.Key. A failure of one
// write does not stop the others, and it does not change the sync result. The next heartbeat tries again.
// A failure is not logged after ctx, the context of Run, ends.
func (a *Agent) writeConditions(ctx, actx context.Context, conds []spokesync.Condition, known map[string]readObject) {
	for i := range conds {
		if actx.Err() != nil {
			if ctx.Err() == nil {
				a.log.Warn("agent: the attempt is out of time; the next heartbeat writes the other conditions",
					"left", len(conds)-i)
			}
			return
		}
		c := &conds[i]
		if err := a.writeCondition(actx, c, known); err != nil && ctx.Err() == nil {
			a.log.Warn("agent: cannot write the status", "kind", string(c.Kind), "namespace", c.Namespace,
				"name", c.Name, "err", err)
		}
	}
}

// writeCondition writes one condition as the Ready condition of its object, with observedGeneration set to
// the generation that the condition describes. It skips:
//   - an object that does not exist;
//   - an object with a different UID than in the read (the object was deleted and created again);
//   - an object whose generation is lower than that of the condition (the object was deleted and created
//     again);
//   - an object whose status describes a newer generation than the condition (for example, a reply from
//     an older compile after a failover on the hub);
//   - an object that has the condition already.
func (a *Agent) writeCondition(ctx context.Context, c *spokesync.Condition, known map[string]readObject) error {
	if c.Name == "" {
		return fmt.Errorf("condition with no name")
	}
	if c.SourceGeneration < 1 {
		return fmt.Errorf("condition with generation %d", c.SourceGeneration)
	}
	if len(c.Reason) > maxReasonLen || !reasonRE.MatchString(c.Reason) {
		return fmt.Errorf("condition with invalid reason %q", c.Reason)
	}
	var obj client.Object
	var status *v1.PolicyStatus
	switch c.Kind {
	case v1.SpokeClusterAccessPolicy:
		if c.Namespace != "" {
			return fmt.Errorf("ClusterAccessPolicy condition with namespace %q", c.Namespace)
		}
		o := &v1.ClusterAccessPolicy{}
		obj, status = o, &o.Status
	case v1.SpokeAccessPolicy:
		if c.Namespace == "" {
			return fmt.Errorf("AccessPolicy condition with no namespace")
		}
		o := &v1.AccessPolicy{}
		obj, status = o, &o.Status
	default:
		return fmt.Errorf("condition with unknown kind %q", c.Kind)
	}
	want := metav1.Condition{
		Type:               ReadyCondition,
		Status:             metav1.ConditionFalse,
		Reason:             c.Reason,
		Message:            truncate(c.Message, maxMessageLen),
		ObservedGeneration: c.SourceGeneration,
	}
	if c.Ready {
		want.Status = metav1.ConditionTrue
	}
	key := spokesync.Key(&v1.SpokePolicySource{Kind: c.Kind, Namespace: c.Namespace, Name: c.Name})
	k, ok := known[key]
	if !ok || k.uid == "" || !needsWrite(k.status, &want) {
		// Not in the read of this attempt (deleted), no UID to compare, or current already: no API call.
		return nil
	}
	if err := a.reader.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: c.Name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if obj.GetUID() != k.uid || c.SourceGeneration > obj.GetGeneration() || !needsWrite(status, &want) {
		return nil
	}
	base := obj.DeepCopyObject().(client.Object)
	status.ObservedGeneration = want.ObservedGeneration
	apimeta.SetStatusCondition(&status.Conditions, want)
	// The optimistic lock makes the patch fail if the object changed after the read. The next heartbeat
	// writes the condition again.
	return a.writer.Status().Patch(ctx, obj, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// needsWrite reports whether status must change to show want. A status that describes a newer generation
// never goes back.
func needsWrite(status *v1.PolicyStatus, want *metav1.Condition) bool {
	if status.ObservedGeneration > want.ObservedGeneration {
		return false
	}
	have := apimeta.FindStatusCondition(status.Conditions, ReadyCondition)
	return have == nil || status.ObservedGeneration != want.ObservedGeneration || have.Status != want.Status ||
		have.Reason != want.Reason || have.Message != want.Message || have.ObservedGeneration != want.ObservedGeneration
}

// truncate shortens s to at most n bytes and keeps valid UTF-8.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
