//go:build e2e

package multicluster

import (
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

// shortMaxStaleness is the time limit of cluster-a in testTimeLimit.
const shortMaxStaleness = 5 * time.Second

// testTimeLimit: the grants of a spoke stop after maxStaleness without a valid sync, and the next sync
// restores them with no manual work (PLAN §18.6, TASKS T6.8).
func testTimeLimit(t *testing.T, f *fixture) {
	s := f.spokes[clusterA]
	team := groupTeam + clusterA
	f.h.patchSpoke(t, clusterA, func(sp *v1.SpokeClusterSpec) {
		sp.MaxStaleness = &metav1.Duration{Duration: shortMaxStaleness}
	})
	t.Cleanup(func() {
		s.stopAgent()
		s.start(t, f.h)
		f.h.patchSpoke(t, clusterA, func(sp *v1.SpokeClusterSpec) { sp.MaxStaleness = nil })
		f.waitSpokeActive(t, clusterA)
	})
	// A spec change stops the grants until the next sync for the new generation.
	eventually(t, 30*time.Second, "sync of the new generation", func() (bool, string) {
		return f.h.synced(t, clusterA), "observedGeneration differs from generation"
	})
	f.eventuallySees(t, team, metrics, []rec{{clusterA, nsTeam}})

	// The agent stops: no more syncs.
	s.stopAgent()
	last := f.h.spokeCluster(t, clusterA).Status.LastSyncTime.Time
	limit := last.Add(shortMaxStaleness)
	if time.Until(limit) > 2*time.Second {
		// The grant is still active before the limit.
		f.sees(t, team, metrics, []rec{{clusterA, nsTeam}})
	}

	// After the limit: 403 policy_source_expired. The check runs for each request.
	var stopped time.Time
	eventually(t, shortMaxStaleness+15*time.Second, "grant of cluster-a expired", func() (bool, string) {
		res := f.b.query(t, f.as(t, team), metrics, "")
		if denied(res, msgExpired) {
			stopped = time.Now()
			return true, ""
		}
		return false, res.String()
	})
	if stopped.Before(limit) {
		t.Errorf("the grant stopped at %s, before the limit %s (lastSyncTime %s + maxStaleness %s)",
			stopped.Format(time.RFC3339Nano), limit.Format(time.RFC3339Nano), last.Format(time.RFC3339), shortMaxStaleness)
	}
	t.Logf("grant stopped %s after lastSyncTime (limit %s)", stopped.Sub(last).Round(10*time.Millisecond), shortMaxStaleness)
	for _, ref := range sharedBackends {
		f.isDenied(t, team, ref, msgExpired)
		f.isDenied(t, groupAdmin+clusterA, ref, msgExpired)
		// Expiry only narrows: a user with grants in two spokes keeps the other spoke, and hub grants do not
		// expire.
		f.sees(t, groupBoth, ref, []rec{{clusterB, nsTeam}})
		f.sees(t, groupHubTeam, ref, []rec{{hubCluster, nsTeam}})
		f.sees(t, groupOps, ref, inCluster(hubCluster, clusterA))
	}

	// The next valid sync restores the grants.
	s.start(t, f.h)
	for _, ref := range sharedBackends {
		f.eventuallySees(t, team, ref, []rec{{clusterA, nsTeam}})
		f.sees(t, groupBoth, ref, []rec{{clusterA, nsTeam}, {clusterB, nsTeam}})
	}
	if res := f.b.query(t, f.as(t, team), metrics, ""); res.status != http.StatusOK {
		t.Errorf("after the sync: %s", res)
	}
}
