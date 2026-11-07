//go:build e2e

package multicluster

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/controller"
	"github.com/querdian-dev/querdian/internal/policy"
)

// testIsolation: a policy of spoke A never returns data of spoke B on all three backends, also for the
// same namespace name. A hub AccessPolicy sees hub data with cluster="mla" only (TASKS T6.8).
func testIsolation(t *testing.T, f *fixture) {
	for _, ref := range sharedBackends {
		t.Run(ref.name, func(t *testing.T) {
			// A spoke AccessPolicy for namespace team: only namespace team of its own cluster, although
			// namespace team exists in each cluster.
			f.sees(t, groupTeam+clusterA, ref, []rec{{clusterA, nsTeam}})
			f.sees(t, groupTeam+clusterB, ref, []rec{{clusterB, nsTeam}})
			// A query that selects the other cluster returns nothing of it.
			f.noRecordsOf(t, groupTeam+clusterA, ref, clusterB, clusterB)
			f.noRecordsOf(t, groupTeam+clusterB, ref, clusterA, clusterA)

			// A spoke ClusterAccessPolicy with unrestricted: true compiles to cluster="<spoke>" (PLAN §18.3).
			f.sees(t, groupAdmin+clusterA, ref, inCluster(clusterA))
			f.sees(t, groupAdmin+clusterB, ref, inCluster(clusterB))
			f.noRecordsOf(t, groupAdmin+clusterA, ref, clusterB, clusterB, hubCluster, noCluster)
			f.noRecordsOf(t, groupAdmin+clusterA, ref, hubCluster, clusterB, hubCluster, noCluster)

			// A spoke matcher on the cluster label stays ANDed with the implicit matcher, so a spoke admin
			// cannot grant the data of a different cluster.
			f.noRecordsOf(t, groupSneaky, ref, "", clusterB, hubCluster, clusterC, noCluster)
			f.noRecordsOf(t, groupSneaky, ref, clusterB, clusterB, hubCluster, clusterC, noCluster)

			// A subject with the same AccessPolicy in two spokes gets namespace team of the two spokes only.
			f.sees(t, groupBoth, ref, []rec{{clusterA, nsTeam}, {clusterB, nsTeam}})

			// A hub AccessPolicy compiles to cluster="mla": no spoke data, and no hub data without a cluster
			// label (PLAN §18.10, old hub data).
			f.sees(t, groupHubTeam, ref, []rec{{hubCluster, nsTeam}})
			f.noRecordsOf(t, groupHubTeam, ref, clusterA, clusterA)
		})
	}
	t.Run("logs-per", func(t *testing.T) {
		f.sees(t, groupIso+clusterA, logsPer, []rec{{clusterA, nsTeam}})
		f.sees(t, groupIso+clusterB, logsPer, []rec{{clusterB, nsTeam}})
		f.noRecordsOf(t, groupIso+clusterA, logsPer, clusterB, clusterB)
	})
	t.Run("traces-per", func(t *testing.T) {
		f.sees(t, groupIso+clusterA, tracesPer, []rec{{clusterA, nsTeam}})
		f.sees(t, groupIso+clusterB, tracesPer, []rec{{clusterB, nsTeam}})
		f.noRecordsOf(t, groupIso+clusterA, tracesPer, clusterB, clusterB)
	})
}

// testTenantSetups covers the setups of PLAN §18.9 that the backends support (see the package comment).
func testTenantSetups(t *testing.T, f *fixture) {
	t.Run("no-tenant-header", func(t *testing.T) {
		// Terms of several clusters merge where §6.3 permits it: one request, the records of both spokes.
		for _, ref := range []backendRef{metrics, logs, traces} {
			f.sees(t, groupBoth, ref, []rec{{clusterA, nsTeam}, {clusterB, nsTeam}})
			f.sees(t, groupTeam+clusterB, ref, []rec{{clusterB, nsTeam}})
		}
	})
	t.Run("one-tenant-for-all-clusters", func(t *testing.T) {
		for _, ref := range []backendRef{logsOne, tracesOne} {
			// In cluster-b, namespace team has no tenant label: the tenant comes from defaultTenant.
			f.sees(t, groupTeam+clusterB, ref, []rec{{clusterB, nsTeam}})
			// The terms of two spokes in one tenant: also on Tempo, which has no tenant federation.
			f.sees(t, groupBoth, ref, []rec{{clusterA, nsTeam}, {clusterB, nsTeam}})
		}
	})
	t.Run("one-tenant-for-each-cluster", func(t *testing.T) {
		// Loki with tenantIdLabel: a cross-tenant union binds each term to its tenant.
		f.sees(t, groupIsoBoth, logsPer, []rec{{clusterA, nsTeam}, {clusterB, nsTeam}})
		// Tempo has no tenant federation: the documented result is 403 cross_tenant_union_unsupported.
		f.isDenied(t, groupIsoBoth, tracesPer, msgCrossTenant)
	})
	t.Run("tenant-not-allowed", func(t *testing.T) {
		// A spoke rule for a tenant that is not in SpokeCluster.spec.tenants grants nothing. The agent writes
		// the condition of the hub back to the spoke object.
		f.isDenied(t, groupBadTenant, logsPer, msgNoGrant)
		s := f.spokes[clusterA]
		eventually(t, 30*time.Second, "TenantNotAllowed on the spoke object", func() (bool, string) {
			var p v1.ClusterAccessPolicy
			if err := s.api.admin.Get(context.Background(), client.ObjectKey{Name: "bad-tenant"}, &p); err != nil {
				return false, err.Error()
			}
			return readyReason(p.Status.Conditions, metav1.ConditionFalse, policy.ReasonTenantNotAllowed)
		})
	})
}

// readyReason checks the Ready condition.
func readyReason(conds []metav1.Condition, status metav1.ConditionStatus, reason string) (bool, string) {
	c := apimeta.FindStatusCondition(conds, controller.ReadyCondition)
	if c == nil {
		return false, "no Ready condition"
	}
	return c.Status == status && c.Reason == reason, c.Reason + ": " + c.Message
}
