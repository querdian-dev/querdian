// Package controller watches policies and namespaces, compiles them into immutable snapshots and writes
// status conditions (PLAN §3, §6.1). With the multicluster block, it also watches the SpokeCluster and
// SpokePolicy kinds and feeds the table of spoke deadlines (PLAN §18).
package controller

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/policy"
)

// DefaultDebounce is the quiet period after the last change event before a snapshot is rebuilt.
const DefaultDebounce = 250 * time.Millisecond

// Controller owns the current policy snapshot. It is safe for concurrent use.
type Controller struct {
	cfg      *config.Config
	debounce time.Duration
	tracer   trace.Tracer

	onRebuild func(snap *policy.Snapshot, err error)

	// deadlines is the table of spoke deadlines. The SpokeCluster informer feeds it if the multicluster
	// block is set.
	deadlines *policy.Deadlines

	reader  client.Reader
	writer  *statusWriter
	current atomic.Pointer[policy.Snapshot]
	gen     atomic.Int64
	trigger chan struct{} // coalescing change notifications (buffer 1)
	changed chan struct{} // tells the status writer a new snapshot exists (buffer 1)
}

// Option customises a Controller.
type Option func(*Controller)

// WithDebounce overrides the quiet period (default 250ms).
func WithDebounce(d time.Duration) Option { return func(c *Controller) { c.debounce = d } }

// WithTracerProvider sets the tracer provider used for the snapshot.rebuild span (default: the global one).
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *Controller) { c.tracer = tp.Tracer(tracerName) }
}

// WithRebuildHook registers a callback invoked after every rebuild attempt: with the new snapshot on success,
// with a nil snapshot and the error on failure. It runs on the rebuild goroutine and must not block.
func WithRebuildHook(f func(snap *policy.Snapshot, err error)) Option {
	return func(c *Controller) { c.onRebuild = f }
}

// WithDeadlines sets the table of spoke deadlines (PLAN §18.6). The SpokeCluster informer feeds it. With the
// multicluster block, SetupWithManager fails if no table is set.
func WithDeadlines(d *policy.Deadlines) Option { return func(c *Controller) { c.deadlines = d } }

const tracerName = "github.com/querdian-dev/querdian/internal/controller"

// New creates a Controller. Call SetupWithManager before starting the manager.
func New(cfg *config.Config, opts ...Option) *Controller {
	c := &Controller{
		cfg:      cfg,
		debounce: DefaultDebounce,
		tracer:   otel.GetTracerProvider().Tracer(tracerName),
		trigger:  make(chan struct{}, 1),
		changed:  make(chan struct{}, 1),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// AddToScheme registers every type the controller reads or writes.
func AddToScheme(s *runtime.Scheme) error {
	if err := corev1.AddToScheme(s); err != nil {
		return err
	}
	return v1.AddToScheme(s)
}

// Snapshot returns the current snapshot, or nil before the first one was built.
func (c *Controller) Snapshot() *policy.Snapshot { return c.current.Load() }

// Ready reports whether the caches are synced and a first snapshot exists. With the multicluster block, the
// deadline table must also have its first complete state (PLAN §18.6, rule 3).
func (c *Controller) Ready() bool {
	if c.current.Load() == nil {
		return false
	}
	return c.cfg.Multicluster == nil || c.deadlines.Complete()
}

// SetupWithManager registers the snapshot builder (runs on every replica) and the status writer
// (leader only) with the manager.
func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	if c.cfg.Multicluster != nil && c.deadlines == nil {
		return errors.New("controller: the multicluster block needs a deadline table (WithDeadlines)")
	}
	c.reader = mgr.GetCache()
	if err := mgr.Add(&builder{c: c, cache: mgr.GetCache()}); err != nil {
		return err
	}
	c.writer = &statusWriter{c: c, client: mgr.GetClient()}
	return mgr.Add(c.writer)
}

// StatusPatches returns how many status patches this replica has issued (only the leader issues any).
func (c *Controller) StatusPatches() int64 {
	if c.writer == nil {
		return 0
	}
	return c.writer.patches.Load()
}

// notify requests a rebuild; bursts coalesce.
func (c *Controller) notify() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// builder is the non-leader-elected runnable that turns events into snapshots.
type builder struct {
	c     *Controller
	cache crcache.Cache
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: every replica builds its own snapshot.
func (b *builder) NeedLeaderElection() bool { return false }

// Start implements manager.Runnable.
func (b *builder) Start(ctx context.Context) error {
	c := b.c
	kinds := []client.Object{&v1.ClusterAccessPolicy{}, &v1.AccessPolicy{}, &corev1.Namespace{}}
	if c.cfg.Multicluster != nil {
		// Without the block, the new kinds get no informer, so their CRDs can be absent (PLAN §18.4).
		kinds = append(kinds, &v1.SpokePolicy{})
	}
	for _, obj := range kinds {
		inf, err := b.cache.GetInformer(ctx, obj)
		if err != nil {
			return fmt.Errorf("informer for %T: %w", obj, err)
		}
		if _, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc:    func(any) { c.notify() },
			DeleteFunc: func(any) { c.notify() },
			UpdateFunc: func(o, n any) {
				if relevantChange(o, n) {
					c.notify()
				}
			},
		}); err != nil {
			return err
		}
	}
	var spokes toolscache.ResourceEventHandlerRegistration
	if c.cfg.Multicluster != nil {
		inf, err := b.cache.GetInformer(ctx, &v1.SpokeCluster{})
		if err != nil {
			return fmt.Errorf("informer for %T: %w", &v1.SpokeCluster{}, err)
		}
		if spokes, err = inf.AddEventHandler(newSpokeClusterHandler(ctx, c)); err != nil {
			return err
		}
	}
	if !b.cache.WaitForCacheSync(ctx) {
		return fmt.Errorf("cache did not sync")
	}
	if spokes != nil {
		// The cache can be synced before the handler has received each object of the first list. The table is
		// complete only after the handler has received all of them.
		if !toolscache.WaitForCacheSync(ctx.Done(), spokes.HasSynced) {
			return fmt.Errorf("spoke deadline table did not sync")
		}
		c.deadlines.MarkComplete()
	}
	if err := c.rebuild(ctx, "startup"); err != nil {
		return err
	}
	runDebounced(ctx, c.trigger, c.debounce, func(int) {
		_ = c.rebuild(ctx, "event") // keeps the previous snapshot on error; the next event retries
	})
	return nil
}

// relevantChange ignores updates that cannot change compilation, in particular our own status writes.
func relevantChange(oldObj, newObj any) bool {
	o, ok1 := oldObj.(client.Object)
	n, ok2 := newObj.(client.Object)
	if !ok1 || !ok2 {
		return true
	}
	if _, isNS := n.(*corev1.Namespace); isNS {
		return !equalStringMap(o.GetLabels(), n.GetLabels())
	}
	return o.GetGeneration() != n.GetGeneration()
}

func equalStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// rebuild lists everything from the cache, compiles it and atomically swaps the snapshot.
func (c *Controller) rebuild(ctx context.Context, trigger string) error {
	err := c.doRebuild(ctx, trigger)
	if c.onRebuild != nil {
		if err != nil {
			c.onRebuild(nil, err)
		} else {
			c.onRebuild(c.current.Load(), nil)
		}
	}
	return err
}

func (c *Controller) doRebuild(ctx context.Context, trigger string) error {
	ctx, span := c.tracer.Start(ctx, "snapshot.rebuild", trace.WithNewRoot())
	defer span.End()
	start := time.Now()

	var cps v1.ClusterAccessPolicyList
	var aps v1.AccessPolicyList
	var nss corev1.NamespaceList
	var scs v1.SpokeClusterList
	var sps v1.SpokePolicyList
	lists := []client.ObjectList{&cps, &aps, &nss}
	if c.cfg.Multicluster != nil {
		lists = append(lists, &scs, &sps)
	}
	for _, l := range lists {
		if err := c.reader.List(ctx, l); err != nil {
			span.RecordError(err)
			return fmt.Errorf("list %T: %w", l, err)
		}
	}
	labels := make(map[string]map[string]string, len(nss.Items))
	for i := range nss.Items {
		labels[nss.Items[i].Name] = nss.Items[i].Labels
	}
	compiled := policy.Compile(policy.Input{
		Config: c.cfg, ClusterPolicies: cps.Items, Policies: aps.Items, NamespaceLabels: labels,
		SpokeClusters: scs.Items, SpokePolicies: sps.Items,
	})
	snap := policy.NewSnapshot(compiled, c.cfg, c.gen.Add(1))
	c.current.Store(snap)

	invalid := 0
	for _, r := range compiled.Results {
		if !r.Ready {
			invalid++
		}
	}
	span.SetAttributes(
		attribute.String("querdian.trigger", trigger),
		attribute.Int64("snapshot.generation", snap.Generation),
		attribute.Int("policies.total", len(compiled.Results)),
		attribute.Int("policies.invalid", invalid),
		attribute.Int64("compile.duration_ms", time.Since(start).Milliseconds()),
	)
	select {
	case c.changed <- struct{}{}:
	default:
	}
	return nil
}
