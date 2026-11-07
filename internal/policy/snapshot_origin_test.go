package policy

import (
	"slices"
	"testing"
	"time"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

// TestSnapshotShowsSpokeOrigin: the dashboard can name a spoke policy by its source, and PoliciesFor finds
// the same policies as ResolveAt, also the policies of an expired spoke.
func TestSnapshotShowsSpokeOrigin(t *testing.T) {
	cfg := mcConfig(t)
	hub := capol("hub", gSubj, v1.ClusterRule{Backends: metrics, AllClusters: true, Matchers: []v1.Matcher{ns("a")}})
	read := spokeAP("cluster-a", "order", "read", "tenant-a", gSubj, v1.Rule{Backends: metrics})
	lost := spokeCAP("cluster-x", "lost", gSubj, v1.ClusterRule{Backends: metrics, Matchers: []v1.Matcher{ns("b")}})
	c := Compile(Input{Config: cfg, ClusterPolicies: []v1.ClusterAccessPolicy{hub},
		SpokeClusters: []v1.SpokeCluster{spokeA}, SpokePolicies: []v1.SpokePolicy{read, lost}})
	s := NewSnapshot(c, cfg, 1)
	readID, lostID := KindSpokePolicy+"/"+read.Name, KindSpokePolicy+"/"+lost.Name

	byID := map[string]View{}
	for _, v := range s.Policies() {
		byID[v.ID] = v
	}
	if v := byID["ClusterAccessPolicy/hub"]; v.Result.Spoke != (SpokeSource{}) || len(v.Rules) != 1 || v.Rules[0].Origin != "" {
		t.Fatalf("hub policy: %+v", v)
	}
	v := byID[readID]
	if v.Result.Spoke != (SpokeSource{Cluster: "cluster-a", Kind: "AccessPolicy", Namespace: "order", Name: "read"}) ||
		len(v.Rules) != 1 || v.Rules[0].Origin != "cluster-a" {
		t.Fatalf("spoke policy: %+v", v)
	}
	// A policy that failed to compile still names its source.
	if v := byID[lostID]; v.Result.Ready || v.Result.Spoke != (SpokeSource{Cluster: "cluster-x", Kind: "ClusterAccessPolicy", Name: "lost"}) {
		t.Fatalf("spoke policy of an unknown cluster: %+v", v)
	}

	want := []Ref{{ID: "ClusterAccessPolicy/hub"}, {ID: readID, Origin: "cluster-a"}}
	if got := s.PoliciesFor("", []string{"g", "g", ""}, "metrics"); !slices.Equal(got, want) {
		t.Fatalf("PoliciesFor: %+v", got)
	}
	if got := s.PoliciesFor("g", nil, "metrics"); len(got) != 0 {
		t.Fatalf("users and groups are separate keyspaces: %+v", got)
	}
	if got := s.PoliciesFor("", []string{"g"}, "logs"); len(got) != 0 {
		t.Fatalf("other backend: %+v", got)
	}

	// With no deadline table, the spoke is expired. ResolveAt still lists its policy, and PoliciesFor
	// gives the same IDs.
	eff, err := s.ResolveAt("", []string{"g"}, "metrics", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(want))
	for _, r := range want {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(eff.Policies, ids) || len(eff.Origins) != 0 {
		t.Fatalf("ResolveAt: policies %v, origins %v", eff.Policies, eff.Origins)
	}
}
