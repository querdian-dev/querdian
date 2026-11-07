package policy

import (
	"math"
	"sync"
	"time"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

// MaxSyncTimeSkew is the largest distance by which a lastSyncTime can be in the future of the clock of
// this replica. A spoke with a lastSyncTime further in the future is expired (PLAN §18.6, rule 4).
const MaxSyncTimeSkew = 5 * time.Minute

// spokeDeadline keeps the fields of one SpokeCluster that the expiry check needs.
type spokeDeadline struct {
	// valid is false if the object itself puts the spoke in the expired state, independent of the time:
	// suspended, in deletion, generation not synced, or a maxStaleness that is zero or negative.
	valid bool
	// lastSync is the zero time if status.lastSyncTime is absent.
	lastSync     time.Time
	maxStaleness time.Duration
}

// Deadlines is the table of spoke deadlines (PLAN §18.6, rule 3). It is outside the snapshot, so a
// heartbeat changes it without a recompile. It is safe for concurrent use. A nil *Deadlines is a table
// with no entries: each spoke is expired.
//
// The informers of the SpokeCluster kind feed the table: Update for each add and update, Delete for each
// delete, and MarkComplete after the first full list. Resolve, the dashboard and the Loki tail read it.
type Deadlines struct {
	mu       sync.RWMutex
	spokes   map[string]spokeDeadline
	complete bool
}

// NewDeadlines returns an empty table.
func NewDeadlines() *Deadlines {
	return &Deadlines{spokes: map[string]spokeDeadline{}}
}

// Update stores the state of one SpokeCluster. defaultMaxStaleness is multicluster.maxStaleness of the
// static config; spec.maxStaleness replaces it when set. Update replaces the earlier entry of the same
// name completely, so a re-created SpokeCluster starts with the state of the new object. It ignores a nil
// object and an object with no name.
func (d *Deadlines) Update(sc *v1.SpokeCluster, defaultMaxStaleness time.Duration) {
	if d == nil || sc == nil || sc.Name == "" {
		return
	}
	e := spokeDeadline{maxStaleness: defaultMaxStaleness}
	if sc.Spec.MaxStaleness != nil {
		e.maxStaleness = sc.Spec.MaxStaleness.Duration
	}
	if sc.Status.LastSyncTime != nil {
		e.lastSync = sc.Status.LastSyncTime.Time
	}
	// The receiver writes observedGeneration only after a valid sync, and metadata.generation is at least
	// 1 on the API server. A value of zero or less means that no valid sync has occurred.
	e.valid = !sc.Spec.Suspended &&
		sc.DeletionTimestamp == nil &&
		sc.Status.ObservedGeneration > 0 &&
		sc.Status.ObservedGeneration == sc.Generation &&
		e.maxStaleness > 0
	d.mu.Lock()
	defer d.mu.Unlock()
	d.spokes[sc.Name] = e
}

// Delete removes the entry of a SpokeCluster. The spoke is then expired.
func (d *Deadlines) Delete(name string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.spokes, name)
}

// MarkComplete records that the table holds its first complete state. A replica with the multicluster
// block reports ready only after this call.
func (d *Deadlines) MarkComplete() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.complete = true
}

// Complete reports whether MarkComplete was called. It is false for a nil table.
func (d *Deadlines) Complete() bool {
	if d == nil {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.complete
}

// Expired reports whether the policies of the spoke with this name are expired at now (PLAN §18.6,
// rule 4). It is true if any of these conditions is true: the table has no entry for the spoke, the
// spoke is suspended or in deletion, observedGeneration differs from metadata.generation, maxStaleness is
// zero or negative, lastSyncTime is absent or more than MaxSyncTimeSkew in the future, or maxStaleness
// has passed since lastSyncTime. Hub rules have no origin and never expire; do not call Expired for them.
func (d *Deadlines) Expired(origin string, now time.Time) bool {
	if d == nil {
		return true
	}
	d.mu.RLock()
	e, ok := d.spokes[origin]
	d.mu.RUnlock()
	switch {
	case !ok, !e.valid, e.lastSync.IsZero():
		return true
	case e.lastSync.After(now.Add(MaxSyncTimeSkew)):
		return true
	}
	return !now.Before(e.lastSync.Add(e.maxStaleness))
}

// AnyExpired reports whether at least one of the spokes is expired at now.
func (d *Deadlines) AnyExpired(origins []string, now time.Time) bool {
	for _, o := range origins {
		if d.Expired(o, now) {
			return true
		}
	}
	return false
}

// Ages returns the time in seconds since the last sync of each spoke in the table, for the metric
// querdian_cluster_policy_age_seconds. The value is +Inf for a spoke with no lastSyncTime, and negative
// for a lastSyncTime in the future.
func (d *Deadlines) Ages(now time.Time) map[string]float64 {
	if d == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string]float64, len(d.spokes))
	for name, e := range d.spokes {
		if e.lastSync.IsZero() {
			out[name] = math.Inf(1)
			continue
		}
		out[name] = now.Sub(e.lastSync).Seconds()
	}
	return out
}
