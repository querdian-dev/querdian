//go:build e2e

// Package multicluster is the end-to-end suite of the multi-cluster enforcement (TASKS T6.8, PLAN §18).
//
// # Topology
//
// The suite runs one hub and two spokes (cluster-a and cluster-b). A third spoke (cluster-c) joins
// during the test. Each cluster has its own Kubernetes API server: an envtest control plane (etcd and
// kube-apiserver) with the Querdian CRDs. The hub runs these components in the test process, each with
// its own controller-runtime manager, as the separate Deployments of a real hub do:
//
//   - the Querdian controller (informers, snapshot, deadline table, status writer) and the real proxy
//     handler chain of internal/server, wired as in cmd/querdian;
//   - the receiver (internal/receiver) on its own mTLS listener, with its own client CA.
//
// Each spoke runs the agent (internal/agent) with leader election, as a Kubernetes user that has only the
// RBAC of PLAN §18.5. The agent reads its cert files before each request and sends its syncs over mTLS to
// the receiver. The heartbeat interval is one second, so that the tests of the time limit and of the
// suspension stay short. The `querdian agent` command requires at least 10 seconds; the suite calls the
// agent package directly for this reason.
//
// The backends are real Prometheus, Loki and Tempo containers. They hold one record for each cluster value
// and namespace: the hub (cluster="mla"), cluster-a, cluster-b, cluster-c, and hub data with no cluster
// label. All clusters use the same namespace names (team and other).
//
// # Why no kind cluster
//
// TASKS T6.8 names kind. The suite uses envtest API servers instead, for these reasons:
//
//   - The development sandbox of the project has no container runtime, so kind cannot run there. The
//     envtest binaries run in the sandbox and in the CI job, and the `envtest` CI job installs them already.
//   - A kind cluster needs a Querdian image and the Helm chart. The chart for the multi-cluster mode is a
//     parallel task (T6.7), and the suite must not depend on it.
//   - The behaviour under test is in the Querdian components, not in the kubelet or in the network of a
//     cluster: the policy origin, the cluster matcher, the receiver checks, the agent transport, the
//     deadline table and the closure of tails. envtest gives a real API server with the real CRDs, CEL
//     rules and RBAC, and the components use the same client code as in a pod.
//
// What the suite does not cover: the Deployments, Services and RBAC objects of the chart, and the network
// path between clusters. The chart tests of T6.7 cover the RBAC of the chart.
//
// # Tenant setups (PLAN §18.9)
//
// The hub config has seven backends, one for each tenant setup that a backend type supports:
//
//   - no tenant header: Prometheus, a single-tenant Loki and a single-tenant Tempo;
//   - one tenant for all clusters: a multi-tenant Loki and Tempo, tenant "all";
//   - one tenant for each cluster: the same multi-tenant instances, tenant "t-<cluster>". The Loki backend
//     uses tenantFederation: tenantIdLabel; Tempo has no such option.
//
// Prometheus has no tenants, so it has only the first setup. The setup "several tenants for each cluster"
// follows the rules of the setup "one tenant for each cluster" when the terms of a user span tenants, so
// the suite does not repeat it.
//
// These limits of federated tenants apply to the setup "one tenant for each cluster" (docs/limitations.md).
// The suite tests the documented result where one exists, and does not test the unsupported cases:
//
//   - Tempo: a cross-tenant union with differing terms is 403 cross_tenant_union_unsupported. The suite
//     tests this result for a user with grants in two clusters.
//   - Loki: a cross-tenant union works for query_range with tenantIdLabel, and the suite tests it. Tenant-bound
//     metadata endpoints (labels, series, index/*) and tail with several tenants do not work reliably, so
//     the suite does not test them.
//   - Tempo: tenant-bound terms are 400 unsupported. The suite does not test them.
//
// # Run
//
// `make e2e` runs all end-to-end suites, and `make e2e-multicluster` runs this suite only. Both targets
// set KUBEBUILDER_ASSETS with setup-envtest.
package multicluster

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/querdian-dev/querdian/test/e2e/framework"
)

// fixture is the running topology.
type fixture struct {
	b      *backends
	h      *hub
	spokes map[string]*spoke
}

// TestMultiCluster starts the topology and runs the scenarios in order. The scenarios change the state of
// the spokes and restore it at their end; stolen-cert runs last because it changes the agent identity.
func TestMultiCluster(t *testing.T) {
	t.Cleanup(func() {
		if t.Failed() {
			sink.dump(t)
		}
	})
	start := time.Now()
	b := startContainers(t)
	apis := startAPIServers(t, hubCluster, clusterA, clusterB)
	b.waitAndSeed(t)
	t.Logf("backends seeded and API servers started after %s", time.Since(start).Round(time.Second))

	f := &fixture{b: b, spokes: map[string]*spoke{}}
	f.h = startHub(t, apis[hubCluster], b)
	create(t, f.h.api.admin, hubObjects()...)
	for _, name := range []string{clusterA, clusterB} {
		s := newSpoke(t, f.h, name, apis[name])
		create(t, s.api.admin, spokeObjects(name)...)
		s.start(t, f.h)
		f.spokes[name] = s
	}
	for _, name := range []string{clusterA, clusterB} {
		f.waitSpokeActive(t, name)
	}
	t.Logf("topology ready after %s", time.Since(start).Round(time.Second))

	t.Run("isolation", func(t *testing.T) { testIsolation(t, f) })
	t.Run("tenant-setups", func(t *testing.T) { testTenantSetups(t, f) })
	t.Run("hub-scope-and-new-spoke", func(t *testing.T) { testHubScopeAndNewSpoke(t, f) })
	t.Run("time-limit", func(t *testing.T) { testTimeLimit(t, f) })
	t.Run("stolen-cert", func(t *testing.T) { testStolenCert(t, f) })
	t.Logf("suite done after %s", time.Since(start).Round(time.Second))
}

// waitSpokeActive waits until the hub has all policies of the spoke, the spoke had a valid sync, and the
// team user of the spoke can read its data.
func (f *fixture) waitSpokeActive(t testing.TB, name string) {
	t.Helper()
	eventually(t, time.Minute, "first sync of "+name, func() (bool, string) {
		n := f.h.spokePolicies(t, name)
		return n == spokePolicyCount(name) && f.h.synced(t, name), "SpokePolicy objects or the sync state are not complete"
	})
	f.eventuallySees(t, groupTeam+name, metrics, []rec{{name, nsTeam}})
}

// as returns a proxy client for the user with the name of group, in group.
func (f *fixture) as(t testing.TB, group string) *framework.Client {
	c := framework.NewClient(t, f.h.proxyURL)
	c.Token = f.h.issuer.Token(t, group, group)
	return c
}

// sees checks that group gets exactly the records on ref.
func (f *fixture) sees(t testing.TB, group string, ref backendRef, recs []rec) {
	t.Helper()
	res := f.b.query(t, f.as(t, group), ref, "")
	if res.status != http.StatusOK || !slices.Equal(res.recordSet(), want(recs...)) {
		t.Errorf("%s on %s: got %s; want status 200 and records %v", group, ref.name, res, want(recs...))
	}
}

// eventuallySees waits until group gets exactly the records on ref.
func (f *fixture) eventuallySees(t testing.TB, group string, ref backendRef, recs []rec) {
	t.Helper()
	eventually(t, 30*time.Second, group+" on "+ref.name, func() (bool, string) {
		res := f.b.query(t, f.as(t, group), ref, "")
		return res.status == http.StatusOK && slices.Equal(res.recordSet(), want(recs...)), res.String()
	})
}

// Reasons of a 403, as the error text of the proxy gives them (internal/server/handler.go).
const (
	msgExpired     = "the policy source of this grant has expired"
	msgNoGrant     = "no policy grants access to this backend"
	msgCrossTenant = "access across tenants with differing rules is not supported on this backend"
)

// denied reports whether res is a 403 with the error text msg.
func denied(res result, msg string) bool {
	if res.status != http.StatusForbidden {
		return false
	}
	var body struct {
		Error string `json:"error"`
	}
	return json.Unmarshal([]byte(res.body), &body) == nil && body.Error == msg
}

// isDenied checks that group gets a 403 with the error text msg on ref.
func (f *fixture) isDenied(t testing.TB, group string, ref backendRef, msg string) {
	t.Helper()
	if res := f.b.query(t, f.as(t, group), ref, ""); !denied(res, msg) {
		t.Errorf("%s on %s: got %s; want 403 %q", group, ref.name, res, msg)
	}
}

// eventuallyDenied waits until group gets a 403 with the error text msg on ref.
func (f *fixture) eventuallyDenied(t testing.TB, group string, ref backendRef, msg string, timeout time.Duration) {
	t.Helper()
	eventually(t, timeout, group+" denied on "+ref.name, func() (bool, string) {
		res := f.b.query(t, f.as(t, group), ref, "")
		return denied(res, msg), res.String()
	})
}

// noRecordsOf checks that a query of group on ref returns no record of the clusters. A 403 also passes.
func (f *fixture) noRecordsOf(t testing.TB, group string, ref backendRef, selectCluster string, clusters ...string) {
	t.Helper()
	res := f.b.query(t, f.as(t, group), ref, selectCluster)
	if res.status != http.StatusOK && res.status != http.StatusForbidden {
		t.Errorf("%s on %s (query for cluster %q): got %s; want 200 or 403", group, ref.name, selectCluster, res)
		return
	}
	for _, r := range res.recs {
		if slices.Contains(clusters, r.cluster) {
			t.Errorf("%s on %s (query for cluster %q): got the record %s of a different cluster: %s",
				group, ref.name, selectCluster, r, res)
		}
	}
}
