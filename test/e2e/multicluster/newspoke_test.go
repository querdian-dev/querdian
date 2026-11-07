//go:build e2e

package multicluster

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/policy"
)

// testHubScopeAndNewSpoke: each hub ClusterAccessPolicy rule must state its cluster scope, and a new spoke
// does not widen a hub rule with a cluster matcher (PLAN §18.3, TASKS T6.8).
func testHubScopeAndNewSpoke(t *testing.T, f *fixture) {
	ops := inCluster(hubCluster, clusterA)
	global := inNamespace(nsTeam, allRecords())

	// A hub rule with no cluster scope is invalid and grants nothing.
	f.isDenied(t, groupUnscoped, metrics, msgNoGrant)
	eventually(t, 30*time.Second, "InvalidClusterScope on the hub policy", func() (bool, string) {
		var p v1.ClusterAccessPolicy
		if err := f.h.api.admin.Get(context.Background(), client.ObjectKey{Name: "unscoped"}, &p); err != nil {
			return false, err.Error()
		}
		return readyReason(p.Status.Conditions, metav1.ConditionFalse, policy.ReasonInvalidClusterScope)
	})

	before := func(t *testing.T) {
		for _, ref := range sharedBackends {
			// cluster=~"mla|cluster-a" names a fixed set of clusters.
			f.sees(t, groupOps, ref, ops)
			// allClusters: true matches all clusters, also hub data with no cluster label.
			f.sees(t, groupGlobal, ref, global)
		}
	}
	t.Run("before", before)

	// cluster-c joins: a new API server, a SpokeCluster, the policies of the spoke and its agent. Its data
	// is in the backends from the start.
	apis := startAPIServers(t, clusterC)
	s := newSpoke(t, f.h, clusterC, apis[clusterC])
	create(t, s.api.admin, spokeObjects(clusterC)...)
	s.start(t, f.h)
	f.waitSpokeActive(t, clusterC)

	t.Run("after", func(t *testing.T) {
		before(t)
		for _, ref := range sharedBackends {
			f.sees(t, groupTeam+clusterC, ref, []rec{{clusterC, nsTeam}})
			f.sees(t, groupAdmin+clusterC, ref, inCluster(clusterC))
			// The rules of the other spokes and of the hub do not grow.
			f.sees(t, groupBoth, ref, []rec{{clusterA, nsTeam}, {clusterB, nsTeam}})
			f.sees(t, groupHubTeam, ref, []rec{{hubCluster, nsTeam}})
			f.noRecordsOf(t, groupOps, ref, clusterC, clusterC)
		}
	})
}
