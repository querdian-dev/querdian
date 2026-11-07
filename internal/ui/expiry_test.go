package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	enforcepromql "github.com/querdian-dev/querdian/internal/enforce/promql"
	"github.com/querdian-dev/querdian/internal/policy"
)

// TestDashboardUsesSpokeExpiry: the access view and the dry run use the same expiry check as the proxy.
func TestDashboardUsesSpokeExpiry(t *testing.T) {
	cfg, err := config.Load("../config/testdata/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cps := []v1.ClusterAccessPolicy{{
		ObjectMeta: metav1.ObjectMeta{Name: "spoke", Generation: 1},
		Spec: v1.ClusterAccessPolicySpec{Subjects: v1.Subjects{Groups: []string{"team"}},
			Rules: []v1.ClusterRule{{Backends: []string{"metrics"},
				Matchers: []v1.Matcher{{Name: "namespace", Op: v1.MatchEqual, Value: "b"}}}}},
	}}
	c := policy.Compile(policy.Input{Config: cfg, ClusterPolicies: cps})
	for _, r := range c.ByGroup["team"] {
		r.Origin = "cluster-a" // as the compiler sets it for a SpokePolicy
	}
	snap := policy.NewSnapshot(c, cfg, 1)
	dl := policy.NewDeadlines()
	sc := &v1.SpokeCluster{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Generation: 1},
		Status: v1.SpokeClusterStatus{LastSyncTime: &metav1.Time{Time: time.Now()}, ObservedGeneration: 1}}
	dl.Update(sc, time.Hour)
	h := Handler(Deps{Config: cfg, Snapshots: provider{snap}, Deadlines: dl,
		Enforcers: map[config.BackendType]enforce.Enforcer{config.BackendPrometheus: enforcepromql.New()}})

	access := func() accessJSON {
		var a struct {
			Access []accessJSON `json:"access"`
		}
		_ = json.Unmarshal(do(h, "GET", "/api/access?groups=team", "", "").Body.Bytes(), &a)
		return a.Access[0]
	}
	dryRun := func() map[string]any {
		var m map[string]any
		_ = json.Unmarshal(do(h, "POST", "/api/test", "application/json", `{"groups":["team"],"backend":"metrics","query":"up"}`).Body.Bytes(), &m)
		return m
	}
	if a := access(); !a.Granted {
		t.Fatalf("valid spoke: %+v", a)
	}
	if m := dryRun(); m["allowed"] != true {
		t.Fatalf("valid spoke: %v", m)
	}

	sc.Spec.Suspended = true
	dl.Update(sc, time.Hour)
	if a := access(); a.Granted || a.Reason != "policy_source_expired" {
		t.Fatalf("suspended spoke: %+v", a)
	}
	m := dryRun()
	if m["allowed"] != false || m["status"] != float64(403) || m["reason"] != "policy_source_expired" {
		t.Fatalf("suspended spoke: %v", m)
	}
	if b, _ := json.Marshal(m); strings.Contains(string(b), "cluster-a") {
		t.Fatalf("dry run must not name the cluster: %s", b)
	}

	// Without a table, each spoke rule is expired.
	h = Handler(Deps{Config: cfg, Snapshots: provider{snap}})
	if a := access(); a.Granted || a.Reason != "policy_source_expired" {
		t.Fatalf("no table: %+v", a)
	}
}
