package policy

import (
	"math"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

const defaultStale = 48 * time.Hour

var testNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// spoke returns a SpokeCluster that is valid at testNow: synced one hour ago at its current generation.
func spoke(name string, mods ...func(*v1.SpokeCluster)) *v1.SpokeCluster {
	sc := &v1.SpokeCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Generation: 3},
		Spec:       v1.SpokeClusterSpec{Identity: "spiffe://hub/agent/" + name},
		Status: v1.SpokeClusterStatus{
			LastSyncTime:       &metav1.Time{Time: testNow.Add(-time.Hour)},
			ObservedGeneration: 3,
		},
	}
	for _, m := range mods {
		m(sc)
	}
	return sc
}

// versionOf returns the version of the SpokeCluster.
func versionOf(sc *v1.SpokeCluster) SpokeVersion {
	return SpokeVersion{UID: sc.UID, Generation: sc.Generation}
}

func syncedAt(t time.Time) func(*v1.SpokeCluster) {
	return func(sc *v1.SpokeCluster) { sc.Status.LastSyncTime = &metav1.Time{Time: t} }
}

func staleness(d time.Duration) func(*v1.SpokeCluster) {
	return func(sc *v1.SpokeCluster) { sc.Spec.MaxStaleness = &metav1.Duration{Duration: d} }
}

func TestDeadlinesExpiredConditions(t *testing.T) {
	deleted := metav1.NewTime(testNow)
	tests := []struct {
		name    string
		sc      *v1.SpokeCluster
		def     time.Duration
		expired bool
	}{
		{"valid", spoke("a"), defaultStale, false},
		{"suspended", spoke("a", func(sc *v1.SpokeCluster) { sc.Spec.Suspended = true }), defaultStale, true},
		{"in deletion", spoke("a", func(sc *v1.SpokeCluster) { sc.DeletionTimestamp = &deleted }), defaultStale, true},
		{"lastSyncTime absent", spoke("a", func(sc *v1.SpokeCluster) { sc.Status.LastSyncTime = nil }), defaultStale, true},
		{"lastSyncTime 6m in the future", spoke("a", syncedAt(testNow.Add(6*time.Minute))), defaultStale, true},
		{"lastSyncTime just over 5m in the future", spoke("a", syncedAt(testNow.Add(MaxSyncTimeSkew+time.Nanosecond))), defaultStale, true},
		{"lastSyncTime 5m in the future", spoke("a", syncedAt(testNow.Add(MaxSyncTimeSkew))), defaultStale, false},
		{"lastSyncTime 4m in the future", spoke("a", syncedAt(testNow.Add(4*time.Minute))), defaultStale, false},
		{"observedGeneration older", spoke("a", func(sc *v1.SpokeCluster) { sc.Generation = 4 }), defaultStale, true},
		{"observedGeneration newer", spoke("a", func(sc *v1.SpokeCluster) { sc.Status.ObservedGeneration = 4 }), defaultStale, true},
		{"observedGeneration absent", spoke("a", func(sc *v1.SpokeCluster) { sc.Generation, sc.Status.ObservedGeneration = 0, 0 }), defaultStale, true},
		{"spec.maxStaleness zero", spoke("a", staleness(0)), defaultStale, true},
		{"spec.maxStaleness negative", spoke("a", staleness(-time.Hour)), defaultStale, true},
		{"default maxStaleness zero", spoke("a"), 0, true},
		{"default maxStaleness negative", spoke("a"), -time.Hour, true},
		{"spec.maxStaleness replaces an invalid default", spoke("a", staleness(2*time.Hour)), 0, false},
		{"past the default deadline", spoke("a", syncedAt(testNow.Add(-49*time.Hour))), defaultStale, true},
		{"at the deadline", spoke("a", syncedAt(testNow.Add(-defaultStale))), defaultStale, true},
		{"just before the deadline", spoke("a", syncedAt(testNow.Add(-defaultStale+time.Second))), defaultStale, false},
		{"spec.maxStaleness longer than the default", spoke("a", syncedAt(testNow.Add(-100*time.Hour)), staleness(168*time.Hour)), defaultStale, false},
		{"spec.maxStaleness shorter than the default", spoke("a", syncedAt(testNow.Add(-2*time.Hour)), staleness(time.Hour)), defaultStale, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDeadlines()
			d.Update(tc.sc, tc.def)
			if got := d.Expired("a", testNow); got != tc.expired {
				t.Fatalf("Expired = %v, want %v", got, tc.expired)
			}
			if !d.Expired("b", testNow) {
				t.Fatal("a spoke with no entry must be expired")
			}
		})
	}
}

func TestDeadlinesDeleteAndRecreate(t *testing.T) {
	d := NewDeadlines()
	d.Update(spoke("a"), defaultStale)
	if d.Expired("a", testNow) {
		t.Fatal("valid spoke expired")
	}
	d.Delete("a")
	if !d.Expired("a", testNow) {
		t.Fatal("deleted spoke must be expired")
	}
	// The re-created object has a new generation and no status until the receiver accepts a sync.
	recreated := &v1.SpokeCluster{ObjectMeta: metav1.ObjectMeta{Name: "a", Generation: 1}}
	d.Update(recreated, defaultStale)
	if !d.Expired("a", testNow) {
		t.Fatal("re-created spoke with no sync must be expired")
	}
	// A re-created object that keeps an old status from an earlier object is still expired when the
	// generation differs.
	d.Update(spoke("a", func(sc *v1.SpokeCluster) { sc.Generation = 1 }), defaultStale)
	if !d.Expired("a", testNow) {
		t.Fatal("re-created spoke with an old status must be expired")
	}
	// The next valid sync restores the grants (PLAN §18.6, rule 7).
	recreated.Status = v1.SpokeClusterStatus{LastSyncTime: &metav1.Time{Time: testNow}, ObservedGeneration: 1}
	d.Update(recreated, defaultStale)
	if d.Expired("a", testNow) {
		t.Fatal("valid sync must restore the spoke")
	}
}

func TestDeadlinesUpdateReplacesEntry(t *testing.T) {
	d := NewDeadlines()
	d.Update(spoke("a", func(sc *v1.SpokeCluster) { sc.Spec.Suspended = true }), defaultStale)
	if !d.Expired("a", testNow) {
		t.Fatal("suspended spoke must be expired")
	}
	// Suspension ends, but the generation changed: grants return only after a new sync (PLAN §18.7).
	d.Update(spoke("a", func(sc *v1.SpokeCluster) { sc.Generation = 4 }), defaultStale)
	if !d.Expired("a", testNow) {
		t.Fatal("spoke must stay expired until the next sync")
	}
	d.Update(spoke("a", func(sc *v1.SpokeCluster) { sc.Generation, sc.Status.ObservedGeneration = 4, 4 }), defaultStale)
	if d.Expired("a", testNow) {
		t.Fatal("synced spoke must not be expired")
	}
	// Ignored input does not change the table.
	d.Update(nil, defaultStale)
	d.Update(&v1.SpokeCluster{}, defaultStale)
	if d.Expired("a", testNow) || !d.Expired("", testNow) {
		t.Fatal("ignored input changed the table")
	}
}

func TestDeadlinesNilTable(t *testing.T) {
	var d *Deadlines
	d.Update(spoke("a"), defaultStale)
	d.Delete("a")
	d.MarkComplete()
	if !d.Expired("a", testNow) || d.Complete() || d.Ages(testNow) != nil || !d.AnyExpired(map[string]SpokeVersion{"a": {}}, testNow) {
		t.Fatal("a nil table must expire each spoke and never be complete")
	}
}

func TestDeadlinesComplete(t *testing.T) {
	d := NewDeadlines()
	if d.Complete() {
		t.Fatal("new table must not be complete")
	}
	d.MarkComplete()
	if !d.Complete() {
		t.Fatal("table must be complete after MarkComplete")
	}
}

func TestDeadlinesAnyExpired(t *testing.T) {
	d := NewDeadlines()
	d.Update(spoke("a"), defaultStale)
	d.Update(spoke("b"), defaultStale)
	both := map[string]SpokeVersion{"a": versionOf(spoke("a")), "b": versionOf(spoke("b"))}
	if d.AnyExpired(both, testNow) || d.AnyExpired(nil, testNow) {
		t.Fatal("no spoke is expired")
	}
	d.Update(spoke("b", func(sc *v1.SpokeCluster) { sc.Spec.Suspended = true }), defaultStale)
	if !d.AnyExpired(both, testNow) {
		t.Fatal("b is suspended")
	}
	// A new spec of a, synced at once: a is valid, but not for a grant from the old spec (PLAN §18.9).
	d.Update(spoke("b"), defaultStale)
	d.Update(spoke("a", func(sc *v1.SpokeCluster) { sc.Generation, sc.Status.ObservedGeneration = 4, 4 }), defaultStale)
	if d.Expired("a", testNow) || !d.AnyExpired(both, testNow) {
		t.Fatal("a grant from the old spec of a must expire")
	}
}

// ExpiredFor also expires a rule that was compiled from a different version of the SpokeCluster.
func TestDeadlinesExpiredFor(t *testing.T) {
	d := NewDeadlines()
	sc := spoke("a")
	d.Update(sc, defaultStale)
	v := versionOf(sc)
	if d.ExpiredFor("a", v, testNow) {
		t.Fatal("same version must be valid")
	}
	for name, other := range map[string]SpokeVersion{
		"older generation":         {UID: v.UID, Generation: v.Generation - 1},
		"newer generation":         {UID: v.UID, Generation: v.Generation + 1},
		"same generation, new UID": {UID: "uid-old", Generation: v.Generation},
		"zero version":             {},
	} {
		if !d.ExpiredFor("a", other, testNow) {
			t.Fatalf("%s: must be expired", name)
		}
	}
	if !d.ExpiredFor("a", v, testNow.Add(defaultStale)) || !d.ExpiredFor("b", SpokeVersion{}, testNow) {
		t.Fatal("an expired or missing spoke must be expired for each version")
	}
	if !(*Deadlines)(nil).ExpiredFor("a", v, testNow) {
		t.Fatal("nil table")
	}
}

func TestDeadlinesAges(t *testing.T) {
	d := NewDeadlines()
	d.Update(spoke("a"), defaultStale)
	d.Update(spoke("never", func(sc *v1.SpokeCluster) { sc.Status.LastSyncTime = nil }), defaultStale)
	d.Update(spoke("future", syncedAt(testNow.Add(time.Minute))), defaultStale)
	ages := d.Ages(testNow)
	if len(ages) != 3 || ages["a"] != 3600 || !math.IsInf(ages["never"], 1) || ages["future"] != -60 {
		t.Fatalf("ages = %v", ages)
	}
	d.Delete("never")
	if _, ok := d.Ages(testNow)["never"]; ok {
		t.Fatal("deleted spoke still has an age")
	}
}

// TestDeadlinesConcurrent runs readers and writers together; run with -race.
func TestDeadlinesConcurrent(*testing.T) {
	d := NewDeadlines()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := range 200 {
				d.Update(spoke("a", func(sc *v1.SpokeCluster) { sc.Spec.Suspended = (i+j)%2 == 0 }), defaultStale)
				if j%50 == 0 {
					d.Delete("a")
					d.MarkComplete()
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				_ = d.Expired("a", testNow)
				_ = d.Ages(testNow)
				_ = d.Complete()
			}
		}()
	}
	wg.Wait()
}

// A suspended spoke has a fresh age but is expired; the expired metric shows it.
func TestDeadlinesExpiredStates(t *testing.T) {
	now := testNow
	d := NewDeadlines()
	fresh := spoke("cluster-a")
	suspended := spoke("cluster-b")
	suspended.Spec.Suspended = true
	d.Update(fresh, defaultStale)
	d.Update(suspended, defaultStale)
	got := d.ExpiredStates(now)
	if len(got) != 2 || got["cluster-a"] != 0 || got["cluster-b"] != 1 {
		t.Fatalf("ExpiredStates = %v", got)
	}
	if (*Deadlines)(nil).ExpiredStates(now) != nil {
		t.Fatal("nil table")
	}
}

// After TrackCompiled, a spoke is expired until the snapshot was compiled from its current generation, also
// when a sync of that generation has already made it valid (PLAN §18.6, §18.9).
func TestDeadlinesTrackCompiled(t *testing.T) {
	now := testNow
	d := NewDeadlines()
	sc := spoke("cluster-a")
	d.Update(sc, defaultStale)
	if d.Expired("cluster-a", now) {
		t.Fatal("without tracking the fresh spoke must be valid")
	}
	d.TrackCompiled()
	if !d.Expired("cluster-a", now) {
		t.Fatal("tracked spoke must be expired before the first SetCompiled")
	}
	d.SetCompiled(map[string]SpokeVersion{"cluster-a": versionOf(sc)})
	if d.Expired("cluster-a", now) {
		t.Fatal("compiled version equals the entry: must be valid")
	}
	// A spec change and an immediate sync of the new generation, before the rebuild.
	next := sc.DeepCopy()
	next.Generation++
	next.Status.ObservedGeneration = next.Generation
	d.Update(next, defaultStale)
	if !d.Expired("cluster-a", now) {
		t.Fatal("the snapshot has the old generation: must be expired")
	}
	d.SetCompiled(map[string]SpokeVersion{"cluster-a": versionOf(next)})
	if d.Expired("cluster-a", now) {
		t.Fatal("after the rebuild the spoke must be valid again")
	}
	// The object is deleted and created again with the same name, and a sync of the new object arrives
	// before the rebuild. The new object can have the generation that the snapshot has.
	recreated := spoke("cluster-a", func(sc *v1.SpokeCluster) {
		sc.UID = "uid-recreated"
		sc.Generation, sc.Status.ObservedGeneration = next.Generation, next.Generation
	})
	d.Delete("cluster-a")
	d.Update(recreated, defaultStale)
	if !d.Expired("cluster-a", now) {
		t.Fatal("the snapshot has the old object: must be expired")
	}
	d.SetCompiled(map[string]SpokeVersion{"cluster-a": versionOf(recreated)})
	if d.Expired("cluster-a", now) {
		t.Fatal("after the rebuild the re-created spoke must be valid")
	}
	d.SetCompiled(map[string]SpokeVersion{})
	if !d.Expired("cluster-a", now) {
		t.Fatal("a spoke missing from the compiled set must be expired")
	}
	// SetCompiled before TrackCompiled has no effect, and a nil table is safe.
	u := NewDeadlines()
	u.SetCompiled(map[string]SpokeVersion{"x": {Generation: 1}})
	u.Update(spoke("cluster-a"), defaultStale)
	if u.Expired("cluster-a", now) {
		t.Fatal("SetCompiled must not start tracking")
	}
	var nilTable *Deadlines
	nilTable.TrackCompiled()
	nilTable.SetCompiled(nil)
}
