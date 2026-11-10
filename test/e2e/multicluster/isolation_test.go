//go:build e2e

package multicluster

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/controller"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/test/e2e/framework"
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
			// A query that selects a different cluster returns nothing of it.
			for _, other := range []string{clusterB, hubCluster, clusterC} {
				f.onlyClusters(t, groupTeam+clusterA, ref, other, clusterA)
			}
			f.onlyClusters(t, groupTeam+clusterB, ref, clusterA, clusterB)

			// A spoke ClusterAccessPolicy with unrestricted: true compiles to cluster="<spoke>" (PLAN §18.3).
			f.sees(t, groupAdmin+clusterA, ref, inCluster(clusterA))
			f.sees(t, groupAdmin+clusterB, ref, inCluster(clusterB))
			for _, other := range []string{"", clusterB, hubCluster, clusterC} {
				f.onlyClusters(t, groupAdmin+clusterA, ref, other, clusterA)
			}

			// A spoke matcher on the cluster label stays ANDed with the implicit matcher, so a spoke admin
			// cannot grant the data of a different cluster. The policy is valid; it matches nothing.
			for _, other := range []string{"", clusterB} {
				f.onlyClusters(t, groupSneaky, ref, other, clusterA)
			}

			// A subject with the same AccessPolicy in two spokes gets namespace team of the two spokes only.
			f.sees(t, groupBoth, ref, []rec{{clusterA, nsTeam}, {clusterB, nsTeam}})

			// A hub AccessPolicy compiles to cluster="mla": no spoke data, and no hub data without a cluster
			// label (PLAN §18.10, old hub data).
			f.sees(t, groupHubTeam, ref, []rec{{hubCluster, nsTeam}})
			f.onlyClusters(t, groupHubTeam, ref, clusterA, hubCluster)
		})
	}
	t.Run("sneaky-policy-is-valid", func(t *testing.T) {
		// The policy with the matcher cluster="cluster-b" is Ready: it does not fail, it only narrows.
		f.spokeReady(t, clusterA, "sneaky", metav1.ConditionTrue, policy.ReasonCompiled)
	})
	for _, ref := range perClusterBackends {
		t.Run(ref.name, func(t *testing.T) {
			f.sees(t, groupIso+clusterA, ref, []rec{{clusterA, nsTeam}})
			f.sees(t, groupIso+clusterB, ref, []rec{{clusterB, nsTeam}})
			f.onlyClusters(t, groupIso+clusterA, ref, clusterB, clusterA)
		})
	}
	t.Run("metadata", func(t *testing.T) { testMetadataIsolation(t, f) })
}

// testMetadataIsolation: the metadata endpoints and Tempo trace by ID do not show the data of a different
// cluster either.
func testMetadataIsolation(t *testing.T, f *fixture) {
	from, to := strconv.FormatInt(f.b.end.Add(-10*time.Minute).Unix(), 10), strconv.FormatInt(f.b.end.Add(10*time.Minute).Unix(), 10)
	window := url.Values{"start": {from}, "end": {to}}
	team := groupTeam + clusterA

	t.Run("prometheus-series", func(t *testing.T) {
		q := url.Values{"match[]": {"mc_records"}, "start": {from}, "end": {to}}
		var d struct {
			Data []map[string]string `json:"data"`
		}
		r := f.as(t, team).Get(metrics.prefix+"/api/v1/series", q)
		if r.Status != http.StatusOK {
			t.Fatalf("series: %d %s", r.Status, r.Body)
		}
		r.JSON(t, &d)
		var got []string
		for _, s := range d.Data {
			got = append(got, rec{s["cluster"], s["namespace"]}.String())
		}
		if want := want(metrics, rec{clusterA, nsTeam}); !slices.Equal(got, want) {
			t.Errorf("series of %s: got %v; want %v", team, got, want)
		}
	})
	t.Run("prometheus-label-values", func(t *testing.T) {
		got := labelValues(t, f, team, metrics.prefix+"/api/v1/label/cluster/values", window)
		if !slices.Equal(got, []string{clusterA}) {
			t.Errorf("label values of %s: got %v; want [%s]", team, got, clusterA)
		}
	})
	t.Run("tempo-tag-values", func(t *testing.T) {
		got := tagValues(t, f, team, traces.prefix+"/api/v2/search/tag/"+clusterLabel(config.BackendTempo)+"/values", window)
		if !slices.Equal(got, []string{clusterA}) {
			t.Errorf("tag values of %s: got %v; want [%s]", team, got, clusterA)
		}
	})
	t.Run("tempo-trace-by-id", func(t *testing.T) {
		c := f.as(t, team)
		accept := map[string]string{"Accept": "application/json"}
		ownRec := rec{clusterA, nsTeam}
		ownID := f.b.traceID(t, ownRec, noTenant)
		own := c.GetWith(traces.prefix+"/api/traces/"+ownID, window, accept)
		if own.Status != http.StatusOK {
			t.Errorf("trace of %s for %s: got %d %.300s; want 200 with the trace", ownRec, team, own.Status, own.Body)
		} else {
			// The seeded trace has one span, with the resource attributes of its record.
			spans := traceSpans(t, own)
			if len(spans) == 0 {
				t.Errorf("trace of %s for %s: no spans in %.300s", ownRec, team, own.Body)
			}
			for _, sp := range spans {
				if sp.traceID != ownID || sp.r != ownRec {
					t.Errorf("trace of %s for %s: got a span of trace %s and record %s; want trace %s and record %s",
						ownRec, team, sp.traceID, sp.r, ownID, ownRec)
				}
			}
		}
		for _, r := range []rec{{clusterB, nsTeam}, {hubCluster, nsTeam}, {noCluster, nsTeam}} {
			other := c.GetWith(traces.prefix+"/api/traces/"+f.b.traceID(t, r, noTenant), window, accept)
			if other.Status != http.StatusNotFound {
				t.Errorf("trace of %s for %s: got %d %.300s; want 404", r, team, other.Status, other.Body)
			}
		}
	})
}

// traceSpan is one span of a Tempo trace: its trace ID (hex) and the record of its resource attributes.
type traceSpan struct {
	traceID string
	r       rec
}

// traceSpans decodes the JSON body of /api/traces/<id> (version 1: the resource spans are in batches).
func traceSpans(t *testing.T, r *framework.Response) []traceSpan {
	t.Helper()
	type attr struct {
		Key   string `json:"key"`
		Value struct {
			StringValue string `json:"stringValue"`
		} `json:"value"`
	}
	var d struct {
		Batches []struct {
			Resource struct {
				Attributes []attr `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Spans []struct {
					TraceID string `json:"traceId"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"batches"`
	}
	r.JSON(t, &d)
	var out []traceSpan
	for _, b := range d.Batches {
		var res rec
		for _, a := range b.Resource.Attributes {
			switch a.Key {
			case "cluster":
				res.cluster = a.Value.StringValue
			case "namespace":
				res.ns = a.Value.StringValue
			}
		}
		for _, ss := range b.ScopeSpans {
			for _, sp := range ss.Spans {
				// The JSON form of Tempo encodes the ID bytes in base64.
				raw, err := base64.StdEncoding.DecodeString(sp.TraceID)
				if err != nil {
					t.Fatalf("trace id %q: %v", sp.TraceID, err)
				}
				out = append(out, traceSpan{hex.EncodeToString(raw), res})
			}
		}
	}
	return out
}

func labelValues(t *testing.T, f *fixture, group, path string, q url.Values) []string {
	t.Helper()
	r := f.as(t, group).Get(path, q)
	if r.Status != http.StatusOK {
		t.Fatalf("%s: %d %s", path, r.Status, r.Body)
	}
	var d struct {
		Data []string `json:"data"`
	}
	r.JSON(t, &d)
	sort.Strings(d.Data)
	return d.Data
}

func tagValues(t *testing.T, f *fixture, group, path string, q url.Values) []string {
	t.Helper()
	r := f.as(t, group).Get(path, q)
	if r.Status != http.StatusOK {
		t.Fatalf("%s: %d %s", path, r.Status, r.Body)
	}
	var d struct {
		TagValues []struct {
			Value string `json:"value"`
		} `json:"tagValues"`
	}
	r.JSON(t, &d)
	out := make([]string, 0, len(d.TagValues))
	for _, v := range d.TagValues {
		out = append(out, v.Value)
	}
	sort.Strings(out)
	return out
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
		// Tempo and Prometheus have no tenant federation: the documented result is 403
		// cross_tenant_union_unsupported.
		f.isDenied(t, groupIsoBoth, tracesPer, msgCrossTenant)
		f.isDenied(t, groupIsoBoth, metricsPer, msgCrossTenant)
	})
	t.Run("several-tenants-for-each-cluster", func(t *testing.T) {
		// Terms in two tenants of cluster-a: namespace team from tenant "all", namespace other from the tenant
		// of cluster-a. Loki binds each term to its tenant; the backends with no federation reject the union.
		f.seesItems(t, groupMulti, logsPer, wantItems(
			item{rec{clusterA, nsTeam}, tenantAll}, item{rec{clusterA, nsOther}, perClusterTenant(clusterA)}))
		f.isDenied(t, groupMulti, tracesPer, msgCrossTenant)
		f.isDenied(t, groupMulti, metricsPer, msgCrossTenant)
	})
	t.Run("tenant-not-allowed", func(t *testing.T) {
		// A spoke rule for a tenant that is not in SpokeCluster.spec.tenants grants nothing. The agent writes
		// the condition of the hub back to the spoke object.
		f.isDenied(t, groupBadTenant, logsPer, msgNoGrant)
		f.spokeReady(t, clusterA, "bad-tenant", metav1.ConditionFalse, policy.ReasonTenantNotAllowed)
	})
}

// spokeReady waits until the spoke ClusterAccessPolicy has the Ready condition that the agent writes back.
func (f *fixture) spokeReady(t *testing.T, cluster, name string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	s := f.spokes[cluster]
	eventually(t, 30*time.Second, reason+" on the spoke object "+name, func() (bool, string) {
		var p v1.ClusterAccessPolicy
		if err := s.api.admin.Get(context.Background(), client.ObjectKey{Name: name}, &p); err != nil {
			return false, err.Error()
		}
		return readyReason(p.Status.Conditions, status, reason)
	})
}

// readyReason checks the Ready condition.
func readyReason(conds []metav1.Condition, status metav1.ConditionStatus, reason string) (bool, string) {
	c := apimeta.FindStatusCondition(conds, controller.ReadyCondition)
	if c == nil {
		return false, "no Ready condition"
	}
	return c.Status == status && c.Reason == reason, string(c.Status) + " " + c.Reason + ": " + c.Message
}
