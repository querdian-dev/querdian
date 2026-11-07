package controller

import (
	"context"
	"log/slog"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

// spokeClusterHandler feeds the deadline table from the SpokeCluster informer and starts a recompile when
// the spec of a SpokeCluster changes (PLAN §18.6, rule 3).
//
// The table gets each add, update and delete directly. This includes the status writes of the receiver
// (lastSyncTime, observedGeneration). These writes do not change metadata.generation, so the filter of the
// other kinds (relevantChange) ignores them. A heartbeat changes only the table and starts no recompile,
// because the compiler reads only the name and the spec of a SpokeCluster.
type spokeClusterHandler struct {
	c *Controller
	// defaultMaxStaleness is multicluster.maxStaleness of the static config.
	defaultMaxStaleness time.Duration
	// ctx is for the cache list of the fallback in forget.
	ctx context.Context
}

func newSpokeClusterHandler(ctx context.Context, c *Controller) *spokeClusterHandler {
	h := &spokeClusterHandler{c: c, ctx: ctx}
	if d := c.cfg.Multicluster.MaxStaleness; d != nil {
		h.defaultMaxStaleness = time.Duration(*d)
	}
	// With no value, the default stays zero, and Deadlines treats a zero maxStaleness as expired.
	return h
}

// OnAdd implements toolscache.ResourceEventHandler.
func (h *spokeClusterHandler) OnAdd(obj any, _ bool) {
	h.track(obj)
	h.c.notify()
}

// OnUpdate implements toolscache.ResourceEventHandler.
func (h *spokeClusterHandler) OnUpdate(oldObj, newObj any) {
	h.track(newObj)
	if spokeSpecChanged(oldObj, newObj) {
		h.c.notify()
	}
}

// OnDelete implements toolscache.ResourceEventHandler.
func (h *spokeClusterHandler) OnDelete(obj any) {
	h.forget(obj)
	h.c.notify()
}

// track stores the state of one SpokeCluster in the table. A SpokeCluster that the compiler must reject gets
// no entry, so its spoke stays expired even if a defect in the compiler gives it rules. This is a second
// layer after spokeOrigin in the policy package (PLAN §18.4, §18.5 identity rule 4).
func (h *spokeClusterHandler) track(obj any) {
	sc, ok := obj.(*v1.SpokeCluster)
	if !ok {
		h.forget(obj)
		return
	}
	mc := h.c.cfg.Multicluster
	if sc.Name == mc.HubCluster || !spokesync.IdentityMatchesName(sc.Spec.Identity, sc.Name) {
		h.c.deadlines.Delete(sc.Name)
		return
	}
	h.c.deadlines.Update(sc, h.defaultMaxStaleness)
}

// forget removes the entry of a deleted SpokeCluster. The informer can give a tombstone instead of the
// object. If no name can be found, forget removes each entry whose SpokeCluster is not in the cache, and if
// the cache cannot be read, it removes all entries. The next event of each spoke adds its entry again.
func (h *spokeClusterHandler) forget(obj any) {
	if name, ok := spokeClusterName(obj); ok {
		h.c.deadlines.Delete(name)
		return
	}
	slog.Warn("spoke deadline table: a SpokeCluster event has no name; removing the entries of unknown spokes")
	var list v1.SpokeClusterList
	known := map[string]bool{}
	if err := h.c.reader.List(h.ctx, &list); err == nil {
		for i := range list.Items {
			known[list.Items[i].Name] = true
		}
	}
	for name := range h.c.deadlines.Ages(time.Now()) {
		if !known[name] {
			h.c.deadlines.Delete(name)
		}
	}
}

// spokeClusterName returns the name of a SpokeCluster event object, also from a tombstone.
func spokeClusterName(obj any) (string, bool) {
	if t, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		if o, ok := t.Obj.(client.Object); ok && o.GetName() != "" {
			return o.GetName(), true
		}
		ns, name, err := toolscache.SplitMetaNamespaceKey(t.Key)
		if err != nil || ns != "" || name == "" {
			return "", false
		}
		return name, true
	}
	if o, ok := obj.(client.Object); ok && o.GetName() != "" {
		return o.GetName(), true
	}
	return "", false
}

// spokeSpecChanged reports whether an update of a SpokeCluster can change the compiled policies. The compiler
// reads the name and the spec only. If the objects are not of the expected type, the result is true.
func spokeSpecChanged(oldObj, newObj any) bool {
	o, ok1 := oldObj.(*v1.SpokeCluster)
	n, ok2 := newObj.(*v1.SpokeCluster)
	if !ok1 || !ok2 {
		return true
	}
	return o.Name != n.Name || o.Generation != n.Generation || !apiequality.Semantic.DeepEqual(o.Spec, n.Spec)
}
