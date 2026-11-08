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
//   - the Querdian controller (informers, snapshot, deadline table, status writer) and the proxy handler
//     chain of internal/server;
//   - the receiver (internal/receiver) on its own mTLS listener, with its own client CA.
//
// The suite does not run the querdian binary. The wiring of the hub is a copy of the wiring in
// cmd/querdian, with these differences:
//
//   - The proxy serves only server.ProxyHandler on an httptest listener. There is no management listener
//     (/readyz, /metrics), no dashboard and no tracing setup. The suite waits for Controller.Ready instead
//     of /readyz.
//   - The proxy manager has no leader election, so the replica is always the status writer. The rebuild
//     debounce is 50ms instead of 250ms. There is no rebuild hook for the snapshot metrics, and the
//     registry has no Go, process or spoke age collectors.
//   - The receiver has no leader election and no probe server, and it uses the default limits. Its cert,
//     key and client CA come from memory, not from files.
//   - The agent uses agent.New and agent.NewSender with a heartbeat of one second, as described below,
//     and the default lease timing. The command flags and the pod namespace file are not used.
//   - All components log to one buffer that the test prints only if it fails.
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
// The first text of TASKS T6.8 named kind. The suite uses envtest API servers instead, for these reasons:
//
//   - The development sandbox of the project has no container runtime, so kind cannot run there. The
//     envtest binaries run in the sandbox and in the CI job, and the `envtest` CI job installs them already.
//   - A kind cluster needs a Querdian image and the Helm chart. The chart for the multi-cluster mode was a
//     parallel task (T6.7) when the suite was written, and the suite does not depend on it.
//   - The behaviour under test is in the Querdian components, not in the kubelet or in the network of a
//     cluster: the policy origin, the cluster matcher, the receiver checks, the agent transport, the
//     deadline table and the closure of tails. envtest gives a real API server with the real CRDs, CEL
//     rules and RBAC, and the components use the same client code as in a pod.
//
// What the suite does not cover: the chart (Deployments, Services, RBAC objects), the querdian binary,
// ServiceAccount tokens and the network path between clusters. The chart tests of T6.7 cover the RBAC of
// the chart. TASKS T6.9 adds a kind smoke test for the other items.
//
// # Tenant setups (PLAN §18.9)
//
// The hub config has eight backends, one for each tenant setup that a backend type supports:
//
//   - no tenant header: Prometheus, a single-tenant Loki and a single-tenant Tempo;
//   - one tenant for all clusters: a multi-tenant Loki and Tempo, tenant "all";
//   - one tenant for each cluster: the same multi-tenant instances, tenant "t-<cluster>", and a Prometheus
//     backend with a tenant header and no federation. The Loki backend uses tenantFederation:
//     tenantIdLabel; Tempo has no such option.
//
// A plain Prometheus has no tenants and ignores the tenant header. The Prometheus backend with a tenant
// header uses the same Prometheus as the backend with no header. It shows the result of Querdian for a
// Prometheus-type backend with tenants (for example Thanos or Mimir): the cluster matcher and the 403 for a
// cross-tenant union. It cannot show that the backend selects the right tenant. On Loki and Tempo, each line
// and each trace names its tenant, so the suite also checks that each result comes from the right tenant.
//
// The setup "several tenants for each cluster" has one case: a user with terms in two tenants of
// cluster-a, on each backend of the setup "one tenant for each cluster". The rules of the row "one tenant
// for each cluster" apply, because the terms span tenants.
//
// These limits of federated tenants apply to the setup "one tenant for each cluster" (docs/limitations.md).
// The suite tests the documented result where one exists, and does not test the unsupported cases:
//
//   - Tempo and Prometheus: a cross-tenant union with differing terms is 403
//     cross_tenant_union_unsupported. The suite tests this result for a user with grants in two clusters
//     and for a user with grants in two tenants of one cluster.
//   - Loki: a cross-tenant union works for query_range with tenantIdLabel, and the suite tests it. Tenant-bound
//     metadata endpoints (labels, series, index/*) and tail with several tenants do not work reliably, so
//     the suite does not test them.
//   - Tempo: tenant-bound terms are 400 unsupported. The suite does not test them.
//
// # Run
//
// `make e2e` runs the single-cluster suites in parallel (`make e2e-single`) and then this suite alone
// (`make e2e-multicluster`). The time limit and the tail tests measure real time, so the load of the other
// suites must not delay them. `make e2e-multicluster` sets KUBEBUILDER_ASSETS with setup-envtest, and
// downloads the envtest binaries on the first run.
package multicluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/controller"
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

// waitSpokeActive waits until the hub has all policies of the spoke, the spoke had a valid sync, the status
// writer has a Ready condition on each SpokePolicy of the spoke for its current generation, and the team
// user of the spoke can read its data. The status writer runs after the snapshot swap, so the checks after
// this wait see a snapshot with all policies of the spoke.
func (f *fixture) waitSpokeActive(t testing.TB, name string) {
	t.Helper()
	eventually(t, time.Minute, "first sync of "+name, func() (bool, string) {
		var list v1.SpokePolicyList
		if err := f.h.api.admin.List(context.Background(), &list); err != nil {
			return false, err.Error()
		}
		n := 0
		for i := range list.Items {
			p := &list.Items[i]
			if p.Spec.Cluster != name {
				continue
			}
			n++
			if p.Status.ObservedGeneration != p.Generation ||
				apimeta.FindStatusCondition(p.Status.Conditions, controller.ReadyCondition) == nil {
				return false, fmt.Sprintf("SpokePolicy %s (%s %s) has no Ready condition for generation %d", p.Name, p.Spec.Kind, p.Spec.Name, p.Generation)
			}
		}
		if n != spokePolicyCount(name) {
			return false, fmt.Sprintf("%d of %d SpokePolicy objects", n, spokePolicyCount(name))
		}
		return f.h.synced(t, name), "no valid sync for the current generation"
	})
	f.eventuallySees(t, groupTeam+name, metrics, []rec{{name, nsTeam}})
}

// as returns a proxy client for the user with the name of group, in group.
func (f *fixture) as(t testing.TB, group string) *framework.Client {
	c := framework.NewClient(t, f.h.proxyURL)
	c.Token = f.h.issuer.Token(t, group, group)
	return c
}

// sees checks that group gets exactly the records on ref, each from the tenant that must hold it.
func (f *fixture) sees(t testing.TB, group string, ref backendRef, recs []rec) {
	t.Helper()
	f.seesItems(t, group, ref, want(ref, recs...))
}

// seesItems checks that group gets exactly the items (records with tenants, see wantItems) on ref.
func (f *fixture) seesItems(t testing.TB, group string, ref backendRef, items []string) {
	t.Helper()
	res := f.b.query(t, f.as(t, group), ref, "")
	if res.status != http.StatusOK || !slices.Equal(res.itemSet(), items) {
		t.Errorf("%s on %s: got %s; want status 200 and records %v", group, ref.name, res, items)
	}
}

// eventuallySees waits until group gets exactly the records on ref.
func (f *fixture) eventuallySees(t testing.TB, group string, ref backendRef, recs []rec) {
	t.Helper()
	eventually(t, 30*time.Second, group+" on "+ref.name, func() (bool, string) {
		res := f.b.query(t, f.as(t, group), ref, "")
		return res.status == http.StatusOK && slices.Equal(res.itemSet(), want(ref, recs...)), res.String()
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

// onlyClusters sends a query of group on ref that selects the cluster selectCluster ("" for no selection).
// It checks that the answer is 200 and that each record is in one of the allowed clusters. An empty result
// passes. The check uses an allow-list, so a record of any other cluster value fails, also of a cluster
// that the test does not name.
func (f *fixture) onlyClusters(t testing.TB, group string, ref backendRef, selectCluster string, allowed ...string) {
	t.Helper()
	res := f.b.query(t, f.as(t, group), ref, selectCluster)
	if res.status != http.StatusOK {
		t.Errorf("%s on %s (query for cluster %q): got %s; want 200", group, ref.name, selectCluster, res)
		return
	}
	for _, r := range res.recs() {
		if !slices.Contains(allowed, r.cluster) {
			t.Errorf("%s on %s (query for cluster %q): got the record %s, which is not in the clusters %q: %s",
				group, ref.name, selectCluster, r, allowed, res)
		}
	}
}
