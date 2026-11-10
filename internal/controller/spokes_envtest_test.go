//go:build envtest

package controller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

const testHub = "mla"

// mcCfg is the example config with the multicluster block and a cluster label on each backend.
func mcCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "config", "testdata", "example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	d := config.Duration(48 * time.Hour)
	cfg.Multicluster = &config.Multicluster{HubCluster: testHub, MaxStaleness: &d}
	labels := map[string]string{"metrics": "cluster", "logs": "k8s_cluster_name", "traces": "resource.k8s.cluster.name"}
	for i := range cfg.Backends {
		cfg.Backends[i].ClusterLabel = labels[cfg.Backends[i].Name]
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// startWith runs a manager with a Controller for cfg until the test ends and waits for readiness.
func startWith(t *testing.T, rc *rest.Config, cfg *config.Config, mgrOpts ctrl.Options, opts ...Option) *Controller {
	t.Helper()
	mgrOpts.Scheme = scheme
	mgrOpts.Metrics = metricsserver.Options{BindAddress: "0"}
	mgr, err := ctrl.NewManager(rc, mgrOpts)
	if err != nil {
		t.Fatal(err)
	}
	c := New(cfg, append([]Option{WithDebounce(50 * time.Millisecond)}, opts...)...)
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

// startMC starts a Controller with the multicluster block and the deadline table dl.
func startMC(t *testing.T, dl *policy.Deadlines, opts ...Option) *Controller {
	t.Helper()
	return startWith(t, restCfg, mcCfg(t), ctrl.Options{}, append([]Option{WithDeadlines(dl)}, opts...)...)
}

func spokeIdentity(name string) string { return "spiffe://mla.example.com/agent/" + name }

func createSpoke(t *testing.T, name string, spec v1.SpokeClusterSpec) *v1.SpokeCluster {
	t.Helper()
	if spec.Identity == "" {
		spec.Identity = spokeIdentity(name)
	}
	if spec.Tenants == nil {
		spec.Tenants = []string{}
	}
	sc := &v1.SpokeCluster{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := direct.Create(context.Background(), sc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = direct.Delete(context.Background(), &v1.SpokeCluster{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	return sc
}

func getSpoke(t *testing.T, name string) *v1.SpokeCluster {
	t.Helper()
	sc := &v1.SpokeCluster{}
	if err := direct.Get(context.Background(), client.ObjectKey{Name: name}, sc); err != nil {
		t.Fatal(err)
	}
	return sc
}

// patchSpokeSpec changes the spec of a SpokeCluster and returns the new object. A spec change increases
// metadata.generation.
func patchSpokeSpec(t *testing.T, name string, mut func(*v1.SpokeClusterSpec)) *v1.SpokeCluster {
	t.Helper()
	sc := getSpoke(t, name)
	base := sc.DeepCopy()
	mut(&sc.Spec)
	if err := direct.Patch(context.Background(), sc, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	return sc
}

// patchSpokeStatus writes the status of a SpokeCluster as the receiver does.
func patchSpokeStatus(t *testing.T, name string, mut func(sc *v1.SpokeCluster)) *v1.SpokeCluster {
	t.Helper()
	sc := getSpoke(t, name)
	base := sc.DeepCopy()
	mut(sc)
	if err := direct.Status().Patch(context.Background(), sc, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	return sc
}

// syncSpoke writes a valid sync at the time at: lastSyncTime and observedGeneration of the current object.
func syncSpoke(t *testing.T, name string, at time.Time) *v1.SpokeCluster {
	t.Helper()
	return patchSpokeStatus(t, name, func(sc *v1.SpokeCluster) {
		sc.Status.LastSyncTime = &metav1.Time{Time: at}
		sc.Status.ObservedGeneration = sc.Generation
	})
}

func createSpokePolicy(t *testing.T, cluster string, src v1.SpokePolicySource) *v1.SpokePolicy {
	t.Helper()
	if src.Generation == 0 {
		src.Generation = 1
	}
	p := &v1.SpokePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: spokesync.ObjectName(cluster, &src)},
		Spec:       v1.SpokePolicySpec{Cluster: cluster, SpokePolicySource: src},
	}
	if err := direct.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = direct.Delete(context.Background(), &v1.SpokePolicy{ObjectMeta: metav1.ObjectMeta{Name: p.Name}})
	})
	return p
}

// spokeLogsAP is a spoke AccessPolicy that grants namespace ns on the logs backend (a backend with a tenant
// header) to group.
func spokeLogsAP(ns, name, tenantLabel, group string) v1.SpokePolicySource {
	return v1.SpokePolicySource{
		Kind: v1.SpokeAccessPolicy, Namespace: ns, Name: name, TenantLabel: tenantLabel,
		AccessPolicy: &v1.AccessPolicySpec{
			Subjects: v1.Subjects{Groups: []string{group}, Users: []string{}},
			Rules:    []v1.Rule{{Backends: []string{"logs"}}},
		},
	}
}

// spokeMetricsCAP is a spoke ClusterAccessPolicy that grants namespace ns on the metrics backend to group.
func spokeMetricsCAP(name, ns, group string) v1.SpokePolicySource {
	return v1.SpokePolicySource{
		Kind: v1.SpokeClusterAccessPolicy, Name: name,
		ClusterAccessPolicy: &v1.ClusterAccessPolicySpec{
			Subjects: v1.Subjects{Groups: []string{group}, Users: []string{}},
			Rules: []v1.ClusterRule{{Backends: []string{"metrics"},
				Matchers: []v1.Matcher{{Name: "namespace", Op: v1.MatchEqual, Value: ns}}}},
		},
	}
}

func resolveNow(c *Controller, dl *policy.Deadlines, group, backend string) (policy.Effective, error) {
	return c.Snapshot().ResolveAt("u", []string{group}, backend, dl, time.Now())
}

func spokeResult(c *Controller, name string) (policy.Result, bool) {
	for _, r := range c.Snapshot().Results() {
		if r.Kind == policy.KindSpokePolicy && r.Name == name {
			return r, true
		}
	}
	return policy.Result{}, false
}

// expiredFor waits until Expired of the spoke equals want.
func expiredFor(t *testing.T, dl *policy.Deadlines, name string, want bool, what string) {
	t.Helper()
	eventually(t, 10*time.Second, what, func() bool { return dl.Expired(name, time.Now()) == want })
}

// Each field that the expiry check reads reaches the table: lastSyncTime, observedGeneration,
// metadata.generation, suspended and spec.maxStaleness (PLAN §18.6, rule 4).
func TestSpokeFieldsReachDeadlineTable(t *testing.T) {
	dl := policy.NewDeadlines()
	startMC(t, dl)
	const name = "fields"
	createSpoke(t, name, v1.SpokeClusterSpec{MaxStaleness: &metav1.Duration{Duration: time.Hour}})
	expiredFor(t, dl, name, true, "a spoke with no sync is expired")
	eventually(t, 10*time.Second, "entry exists", func() bool { _, ok := dl.Ages(time.Now())[name]; return ok })

	syncSpoke(t, name, time.Now())
	expiredFor(t, dl, name, false, "a fresh sync is valid")

	t.Run("lastSyncTime", func(t *testing.T) {
		old := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
		patchSpokeStatus(t, name, func(sc *v1.SpokeCluster) { sc.Status.LastSyncTime = &metav1.Time{Time: old} })
		expiredFor(t, dl, name, true, "an old lastSyncTime expires the spoke")
		if age := dl.Ages(time.Now())[name]; age < 7190 || age > 7300 {
			t.Fatalf("the table has the wrong lastSyncTime: age %v s", age)
		}
		patchSpokeStatus(t, name, func(sc *v1.SpokeCluster) { sc.Status.LastSyncTime = &metav1.Time{Time: time.Now()} })
		expiredFor(t, dl, name, false, "a new lastSyncTime restores the spoke")
	})

	t.Run("observedGeneration", func(t *testing.T) {
		patchSpokeStatus(t, name, func(sc *v1.SpokeCluster) { sc.Status.ObservedGeneration = sc.Generation + 1 })
		expiredFor(t, dl, name, true, "a different observedGeneration expires the spoke")
		syncSpoke(t, name, time.Now())
		expiredFor(t, dl, name, false, "a sync restores the spoke")
	})

	t.Run("metadata.generation", func(t *testing.T) {
		before := getSpoke(t, name).Generation
		after := patchSpokeSpec(t, name, func(s *v1.SpokeClusterSpec) { s.Tenants = []string{"tenant-x"} }).Generation
		if after == before {
			t.Fatal("a spec change must increase the generation")
		}
		expiredFor(t, dl, name, true, "a new generation expires the spoke until the next sync")
		syncSpoke(t, name, time.Now())
		expiredFor(t, dl, name, false, "a sync of the new generation restores the spoke")
	})

	t.Run("suspended", func(t *testing.T) {
		patchSpokeSpec(t, name, func(s *v1.SpokeClusterSpec) { s.Suspended = true })
		// The sync of the new generation isolates the field: only suspended is left.
		syncSpoke(t, name, time.Now())
		expiredFor(t, dl, name, true, "a suspended spoke is expired")
		eventually(t, 10*time.Second, "sync seen", func() bool { return dl.Ages(time.Now())[name] < 60 })
		if !dl.Expired(name, time.Now()) {
			t.Fatal("a suspended spoke must stay expired after a sync")
		}
		patchSpokeSpec(t, name, func(s *v1.SpokeClusterSpec) { s.Suspended = false })
		syncSpoke(t, name, time.Now())
		expiredFor(t, dl, name, false, "grants return after the suspension and a new sync")
	})

	t.Run("spec.maxStaleness", func(t *testing.T) {
		syncSpoke(t, name, time.Now().Add(-30*time.Minute))
		expiredFor(t, dl, name, false, "30 minutes is within one hour")
		patchSpokeSpec(t, name, func(s *v1.SpokeClusterSpec) { s.MaxStaleness = &metav1.Duration{Duration: 10 * time.Minute} })
		patchSpokeStatus(t, name, func(sc *v1.SpokeCluster) { sc.Status.ObservedGeneration = sc.Generation })
		expiredFor(t, dl, name, true, "a smaller maxStaleness expires the spoke")
		patchSpokeSpec(t, name, func(s *v1.SpokeClusterSpec) { s.MaxStaleness = &metav1.Duration{Duration: 2 * time.Hour} })
		patchSpokeStatus(t, name, func(sc *v1.SpokeCluster) { sc.Status.ObservedGeneration = sc.Generation })
		expiredFor(t, dl, name, false, "a larger maxStaleness restores the spoke")
	})

	t.Run("delete", func(t *testing.T) {
		if err := direct.Delete(context.Background(), &v1.SpokeCluster{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
		eventually(t, 10*time.Second, "entry removed", func() bool { _, ok := dl.Ages(time.Now())[name]; return !ok })
		if !dl.Expired(name, time.Now()) {
			t.Fatal("a deleted spoke must be expired")
		}
	})
}

// A heartbeat (a status write of the receiver) changes the table, but it starts no recompile.
func TestSpokeHeartbeatStartsNoRecompile(t *testing.T) {
	var rebuilds atomic.Int64
	dl := policy.NewDeadlines()
	c := startMC(t, dl, WithRebuildHook(func(*policy.Snapshot, error) { rebuilds.Add(1) }))
	const name = "heartbeat"
	createSpoke(t, name, v1.SpokeClusterSpec{})
	syncSpoke(t, name, time.Now())
	p := createSpokePolicy(t, name, spokeMetricsCAP("hb", "a", "hb-group"))
	eventually(t, 10*time.Second, "grant active", func() bool { _, err := resolveNow(c, dl, "hb-group", "metrics"); return err == nil })
	eventually(t, 10*time.Second, "status written", func() bool {
		got := &v1.SpokePolicy{}
		return direct.Get(context.Background(), client.ObjectKeyFromObject(p), got) == nil && got.Status.ObservedGeneration == got.Generation
	})
	time.Sleep(300 * time.Millisecond) // let pending rebuilds finish
	before, gen := rebuilds.Load(), c.Snapshot().Generation

	for i := 1; i <= 3; i++ {
		at := time.Now().Add(-time.Duration(i) * time.Hour).Truncate(time.Second)
		syncSpoke(t, name, at)
		eventually(t, 10*time.Second, "heartbeat in table", func() bool {
			age := dl.Ages(time.Now())[name]
			return age >= float64(i)*3600-5 && age <= float64(i)*3600+60
		})
	}
	time.Sleep(300 * time.Millisecond) // six times the debounce
	if got := rebuilds.Load(); got != before {
		t.Fatalf("heartbeats started %d rebuilds", got-before)
	}
	if c.Snapshot().Generation != gen {
		t.Fatal("heartbeats changed the snapshot")
	}
}

// A smaller allow-list removes the grant, and a change of defaultTenant reaches the compiler (PLAN §18.9).
func TestSpokeTenantChangesRecompile(t *testing.T) {
	dl := policy.NewDeadlines()
	c := startMC(t, dl)
	const name = "tenants"
	createSpoke(t, name, v1.SpokeClusterSpec{Tenants: []string{"tenant-a", "tenant-b"}, DefaultTenant: "tenant-a"})
	syncSpoke(t, name, time.Now())
	labelled := createSpokePolicy(t, name, spokeLogsAP("app", "labelled", "tenant-a", "allow-group"))
	byDefault := createSpokePolicy(t, name, spokeLogsAP("other", "by-default", "", "default-group"))

	eventually(t, 10*time.Second, "grant on tenant-a", func() bool {
		eff, err := resolveNow(c, dl, "allow-group", "logs")
		return err == nil && len(eff.Tenants) == 1 && eff.Tenants[0] == "tenant-a"
	})
	eventually(t, 10*time.Second, "default tenant used", func() bool {
		eff, err := resolveNow(c, dl, "default-group", "logs")
		return err == nil && len(eff.Tenants) == 1 && eff.Tenants[0] == "tenant-a"
	})

	t.Run("defaultTenant", func(t *testing.T) {
		patchSpokeSpec(t, name, func(s *v1.SpokeClusterSpec) { s.DefaultTenant = "tenant-b" })
		syncSpoke(t, name, time.Now())
		eventually(t, 10*time.Second, "new default tenant", func() bool {
			eff, err := resolveNow(c, dl, "default-group", "logs")
			return err == nil && len(eff.Tenants) == 1 && eff.Tenants[0] == "tenant-b"
		})
	})

	t.Run("smaller allow-list", func(t *testing.T) {
		patchSpokeSpec(t, name, func(s *v1.SpokeClusterSpec) { s.Tenants = []string{"tenant-b"} })
		// The sync of the new generation comes at once, before the rebuild. At no time after the spec change
		// may the removed tenant be granted: the table keeps the spoke expired until the snapshot has the
		// new generation.
		syncSpoke(t, name, time.Now())
		deadline := time.Now().Add(10 * time.Second)
		for {
			eff, err := resolveNow(c, dl, "allow-group", "logs")
			if err == nil && slices.Contains(eff.Tenants, "tenant-a") {
				t.Fatalf("removed tenant-a is granted after the spec change: %+v", eff)
			}
			if r, ok := spokeResult(c, labelled.Name); ok && r.Reason == policy.ReasonTenantNotAllowed {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the rebuild did not apply the smaller allow-list")
			}
			time.Sleep(time.Millisecond)
		}
		expiredFor(t, dl, name, false, "spoke synced and compiled")
		eventually(t, 10*time.Second, "grant removed", func() bool {
			r, ok := spokeResult(c, labelled.Name)
			_, err := resolveNow(c, dl, "allow-group", "logs")
			return ok && r.Reason == policy.ReasonTenantNotAllowed && errors.Is(err, policy.ErrNoGrant)
		})
		if _, err := resolveNow(c, dl, "default-group", "logs"); err != nil {
			t.Fatalf("the policy on tenant-b must keep its grant: %v", err)
		}
		if r, ok := spokeResult(c, byDefault.Name); !ok || r.Reason != policy.ReasonCompiled {
			t.Fatalf("the policy with the default tenant must stay valid: %+v", r)
		}
	})
}

// A SpokeCluster that is deleted and created again with a smaller allow-list never grants the removed tenant.
// The new object has generation 1, as the old one had, so only the UID shows that the snapshot is old.
func TestSpokeRecreatedWithSmallerAllowList(t *testing.T) {
	dl := policy.NewDeadlines()
	c := startMC(t, dl)
	const name = "recreated"
	old := createSpoke(t, name, v1.SpokeClusterSpec{Tenants: []string{"tenant-a", "tenant-b"}})
	syncSpoke(t, name, time.Now())
	labelled := createSpokePolicy(t, name, spokeLogsAP("app", "labelled", "tenant-a", "re-group"))
	eventually(t, 10*time.Second, "grant on tenant-a", func() bool {
		eff, err := resolveNow(c, dl, "re-group", "logs")
		return err == nil && slices.Contains(eff.Tenants, "tenant-a")
	})

	if err := direct.Delete(context.Background(), &v1.SpokeCluster{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		t.Fatal(err)
	}
	recreated := createSpoke(t, name, v1.SpokeClusterSpec{Tenants: []string{"tenant-b"}})
	if recreated.UID == old.UID || recreated.Generation != old.Generation {
		t.Fatalf("want a new UID with the same generation: old %s/%d, new %s/%d", old.UID, old.Generation, recreated.UID, recreated.Generation)
	}
	// The sync of the new object comes at once, before the rebuild.
	syncSpoke(t, name, time.Now())
	deadline := time.Now().Add(10 * time.Second)
	for {
		eff, err := resolveNow(c, dl, "re-group", "logs")
		if err == nil && slices.Contains(eff.Tenants, "tenant-a") {
			t.Fatalf("removed tenant-a is granted after the re-creation: %+v", eff)
		}
		if r, ok := spokeResult(c, labelled.Name); ok && r.Reason == policy.ReasonTenantNotAllowed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rebuild did not apply the allow-list of the new object")
		}
		time.Sleep(time.Millisecond)
	}
	expiredFor(t, dl, name, false, "re-created spoke synced and compiled")
}

// The table is complete before the replica is ready, and it then holds each SpokeCluster that existed at the
// start. A spoke that never synced (for example an unreachable spoke) does not stop readiness.
func TestReadinessWaitsForDeadlineTable(t *testing.T) {
	createSpoke(t, "ready-synced", v1.SpokeClusterSpec{})
	syncSpoke(t, "ready-synced", time.Now())
	createSpoke(t, "ready-unreachable", v1.SpokeClusterSpec{})

	dl := policy.NewDeadlines()
	var mu sync.Mutex
	var atComplete map[string]float64
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if dl.Complete() {
				mu.Lock()
				atComplete = dl.Ages(time.Now())
				mu.Unlock()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	c := startMC(t, dl)
	close(stop)
	<-watched
	if !dl.Complete() || !c.Ready() {
		t.Fatal("a ready replica must have a complete table")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, n := range []string{"ready-synced", "ready-unreachable"} {
		if _, ok := atComplete[n]; !ok {
			t.Fatalf("the table was complete without the entry of %s: %v", n, atComplete)
		}
	}
	if dl.Expired("ready-synced", time.Now()) || !dl.Expired("ready-unreachable", time.Now()) {
		t.Fatal("wrong expiry state after the first fill")
	}
}

// The leader writes the Ready condition of each SpokePolicy, with observedGeneration and sourceGeneration.
// The hub object keeps the full reason and message.
func TestSpokePolicyStatusWritten(t *testing.T) {
	dl := policy.NewDeadlines()
	c := startWith(t, restCfg, mcCfg(t), ctrl.Options{
		LeaderElection: true, LeaderElectionID: "querdian-test-spoke-status", LeaderElectionNamespace: "default",
	}, WithDeadlines(dl))
	const name = "status"
	createSpoke(t, name, v1.SpokeClusterSpec{Tenants: []string{"tenant-a"}})
	syncSpoke(t, name, time.Now())
	src := spokeMetricsCAP("ok", "a", "status-group")
	src.Generation = 5
	good := createSpokePolicy(t, name, src)
	bad := createSpokePolicy(t, name, spokeLogsAP("app", "wrong-tenant", "tenant-z", "status-group"))
	orphan := createSpokePolicy(t, "status-missing", spokeMetricsCAP("orphan", "a", "status-group"))

	get := func(p *v1.SpokePolicy) *v1.SpokePolicy {
		got := &v1.SpokePolicy{}
		if err := direct.Get(context.Background(), client.ObjectKeyFromObject(p), got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	cond := func(p *v1.SpokePolicy) *metav1.Condition {
		for i := range p.Status.Conditions {
			if p.Status.Conditions[i].Type == ReadyCondition {
				return &p.Status.Conditions[i]
			}
		}
		return nil
	}
	check := func(p *v1.SpokePolicy, status metav1.ConditionStatus, reason, msgPart string, source int64) func() bool {
		return func() bool {
			got := get(p)
			cd := cond(got)
			return cd != nil && cd.Status == status && cd.Reason == reason && strings.Contains(cd.Message, msgPart) &&
				cd.ObservedGeneration == got.Generation && got.Status.ObservedGeneration == got.Generation &&
				got.Status.SourceGeneration == source
		}
	}
	eventually(t, 30*time.Second, "valid policy", check(good, metav1.ConditionTrue, policy.ReasonCompiled, "", 5))
	eventually(t, 10*time.Second, "tenant not allowed", check(bad, metav1.ConditionFalse, policy.ReasonTenantNotAllowed, `tenant "tenant-z"`, 1))
	eventually(t, 10*time.Second, "no SpokeCluster", check(orphan, metav1.ConditionFalse, policy.ReasonSpokeClusterNotFound, `"status-missing"`, 1))

	// A new spoke generation reaches sourceGeneration.
	p := get(good)
	base := p.DeepCopy()
	p.Spec.Generation = 6
	if err := direct.Patch(context.Background(), p, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "new source generation", check(good, metav1.ConditionTrue, policy.ReasonCompiled, "", 6))
	if c.StatusPatches() == 0 {
		t.Fatal("the leader must count its patches")
	}
}

// End to end on a real snapshot through ResolveAt: a fresh spoke grants, a suspended spoke does not, an
// expired spoke does not, and the next valid sync restores the grant (PLAN §18.6, §18.7).
func TestSpokeLifecycleEndToEnd(t *testing.T) {
	dl := policy.NewDeadlines()
	c := startMC(t, dl)
	const name = "lifecycle-spoke"
	createSpoke(t, name, v1.SpokeClusterSpec{MaxStaleness: &metav1.Duration{Duration: time.Hour}})
	syncSpoke(t, name, time.Now())
	createSpokePolicy(t, name, spokeMetricsCAP("team", "payments", "e2e-group"))

	eventually(t, 10*time.Second, "fresh spoke grants", func() bool {
		eff, err := resolveNow(c, dl, "e2e-group", "metrics")
		if err != nil || len(eff.Terms) != 1 || len(eff.Origins) != 1 || eff.Origins[0] != name {
			return false
		}
		want := map[string]string{"cluster": name, "namespace": "payments"}
		for _, m := range eff.Terms[0].Matchers {
			if want[m.Name] == m.Value && m.Op == v1.MatchEqual {
				delete(want, m.Name)
			}
		}
		return len(want) == 0
	})

	patchSpokeSpec(t, name, func(s *v1.SpokeClusterSpec) { s.Suspended = true })
	eventually(t, 10*time.Second, "suspended spoke grants nothing", func() bool {
		_, err := resolveNow(c, dl, "e2e-group", "metrics")
		return errors.Is(err, policy.ErrPolicySourceExpired)
	})
	// A sync while suspended (the receiver rejects it, but a forged status must also fail) gives nothing.
	syncSpoke(t, name, time.Now())
	time.Sleep(200 * time.Millisecond)
	if _, err := resolveNow(c, dl, "e2e-group", "metrics"); !errors.Is(err, policy.ErrPolicySourceExpired) {
		t.Fatalf("a suspended spoke must stay expired, got %v", err)
	}

	patchSpokeSpec(t, name, func(s *v1.SpokeClusterSpec) { s.Suspended = false })
	time.Sleep(200 * time.Millisecond)
	if _, err := resolveNow(c, dl, "e2e-group", "metrics"); !errors.Is(err, policy.ErrPolicySourceExpired) {
		t.Fatalf("grants must return only after a new valid sync, got %v", err)
	}
	syncSpoke(t, name, time.Now().Add(-2*time.Hour))
	eventually(t, 10*time.Second, "old sync seen", func() bool { return dl.Ages(time.Now())[name] > 3600 })
	if _, err := resolveNow(c, dl, "e2e-group", "metrics"); !errors.Is(err, policy.ErrPolicySourceExpired) {
		t.Fatalf("a sync older than maxStaleness must expire the spoke, got %v", err)
	}
	// The same table and snapshot at a later time: a valid sync now expires after maxStaleness.
	syncSpoke(t, name, time.Now())
	eventually(t, 10*time.Second, "valid sync restores the grant", func() bool {
		_, err := resolveNow(c, dl, "e2e-group", "metrics")
		return err == nil
	})
	if _, err := c.Snapshot().ResolveAt("u", []string{"e2e-group"}, "metrics", dl, time.Now().Add(61*time.Minute)); !errors.Is(err, policy.ErrPolicySourceExpired) {
		t.Fatalf("the grant must expire after maxStaleness, got %v", err)
	}
}

// recordingCache records each kind for which the controller asks the cache.
type recordingCache struct {
	crcache.Cache
	mu    sync.Mutex
	kinds map[string]bool
}

func (r *recordingCache) note(v any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kinds[fmt.Sprintf("%T", v)] = true
}

func (r *recordingCache) GetInformer(ctx context.Context, obj client.Object, opts ...crcache.InformerGetOption) (crcache.Informer, error) {
	r.note(obj)
	return r.Cache.GetInformer(ctx, obj, opts...)
}

func (r *recordingCache) GetInformerForKind(ctx context.Context, gvk schema.GroupVersionKind, opts ...crcache.InformerGetOption) (crcache.Informer, error) {
	r.note(gvk.Kind)
	return r.Cache.GetInformerForKind(ctx, gvk, opts...)
}

func (r *recordingCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	r.note(obj)
	return r.Cache.Get(ctx, key, obj, opts...)
}

func (r *recordingCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.note(list)
	return r.Cache.List(ctx, list, opts...)
}

func (r *recordingCache) seen() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for k := range r.kinds {
		out = append(out, k)
	}
	return strings.Join(out, ",")
}

func recordingOptions(rec **recordingCache) ctrl.Options {
	return ctrl.Options{NewCache: func(rc *rest.Config, o crcache.Options) (crcache.Cache, error) {
		inner, err := crcache.New(rc, o)
		if err != nil {
			return nil, err
		}
		*rec = &recordingCache{Cache: inner, kinds: map[string]bool{}}
		return *rec, nil
	}}
}

// Without the multicluster block, the controller asks the cache for no informer of the new kinds, and a
// SpokePolicy has no result.
func TestNoSpokeInformersWithoutBlock(t *testing.T) {
	createSpoke(t, "no-block", v1.SpokeClusterSpec{})
	syncSpoke(t, "no-block", time.Now())
	createSpokePolicy(t, "no-block", spokeMetricsCAP("x", "a", "no-block-group"))

	var rec *recordingCache
	c := startWith(t, restCfg, appCfg, recordingOptions(&rec))
	p := capFor("no-block-hub", "no-block-hub-group")
	if err := direct.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = direct.Delete(context.Background(), p) })
	eventually(t, 10*time.Second, "hub status written", func() bool {
		cd := readyCondition(t, p, func() *v1.PolicyStatus { return &p.Status })
		return cd != nil
	})
	for _, r := range c.Snapshot().Results() {
		if r.Kind == policy.KindSpokePolicy {
			t.Fatalf("without the block, no SpokePolicy may be compiled: %+v", r)
		}
	}
	if s := rec.seen(); strings.Contains(s, "Spoke") {
		t.Fatalf("the cache was asked for a new kind: %s", s)
	}
	if !strings.Contains(rec.seen(), "ClusterAccessPolicy") {
		t.Fatalf("the recording cache saw nothing: %s", rec.seen())
	}

	// With the block, the same check sees both kinds, so the recording works.
	var recMC *recordingCache
	startWith(t, restCfg, mcCfg(t), recordingOptions(&recMC), WithDeadlines(policy.NewDeadlines()))
	if s := recMC.seen(); !strings.Contains(s, "SpokeCluster") || !strings.Contains(s, "SpokePolicy") {
		t.Fatalf("with the block, the cache must be asked for the new kinds: %s", s)
	}
}

// Without the multicluster block, the controller becomes ready on an API server that does not have the new
// CRDs.
func TestStartsWithoutSpokeCRDs(t *testing.T) {
	crds := filepath.Join("..", "..", "config", "crd")
	env := &envtest.Environment{CRDDirectoryPaths: []string{
		filepath.Join(crds, "policy.querdian.dev_accesspolicies.yaml"),
		filepath.Join(crds, "policy.querdian.dev_clusteraccesspolicies.yaml"),
	}, ErrorIfCRDPathMissing: true}
	rc, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	k, err := client.New(rc, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.List(context.Background(), &v1.SpokeClusterList{}); err == nil {
		t.Fatal("the API server must not have the SpokeCluster kind")
	}
	c := startWith(t, rc, appCfg, ctrl.Options{})
	if c.Snapshot() == nil {
		t.Fatal("no snapshot")
	}
}
