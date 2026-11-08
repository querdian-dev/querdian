//go:build e2e

package multicluster

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/test/e2e/framework"
)

// The groups of the suite. A user has the name of its group.
const (
	// Hub policies.
	groupHubTeam  = "hub-team" // hub AccessPolicy in namespace team: cluster="mla", namespace="team"
	groupOps      = "ops"      // hub ClusterAccessPolicy with the fixed scope cluster=~"mla|cluster-a"
	groupGlobal   = "global"   // hub ClusterAccessPolicy with allClusters: true
	groupUnscoped = "unscoped" // hub ClusterAccessPolicy with no cluster scope: InvalidClusterScope
	// Spoke policies. The spoke name follows the prefix, for example team-cluster-a.
	groupTeam  = "team-"  // spoke AccessPolicy in namespace team
	groupAdmin = "admin-" // spoke ClusterAccessPolicy with unrestricted: true
	groupIso   = "iso-"   // spoke ClusterAccessPolicy for the setup "one tenant for each cluster"
	// groupBoth has the spoke AccessPolicy team in cluster-a and in cluster-b.
	groupBoth = "both"
	// groupIsoBoth has the spoke ClusterAccessPolicy iso in cluster-a and in cluster-b.
	groupIsoBoth = "iso-both"
	// groupSneaky has a spoke ClusterAccessPolicy in cluster-a with the matcher cluster="cluster-b".
	groupSneaky = "sneaky"
	// groupBadTenant has a spoke ClusterAccessPolicy in cluster-a for the tenant of cluster-b.
	groupBadTenant = "bad-tenant"
	// groupMulti has a spoke ClusterAccessPolicy in cluster-a with terms in two tenants of cluster-a: the
	// setup "several tenants for each cluster" (PLAN §18.9).
	groupMulti = "multi"
)

// tenantsFor returns the tenants of a rule for a backend in a cluster.
func tenantsFor(ref backendRef, cluster string) []string {
	switch ref.setup {
	case setupOne:
		return []string{tenantAll}
	case setupPerCluster:
		return []string{perClusterTenant(cluster)}
	}
	return nil
}

// rulesFor returns one rule for each backend. matchers gives the matchers for the backend type.
func rulesFor(refs []backendRef, cluster string, matchers func(config.BackendType) []v1.Matcher, mod func(*v1.ClusterRule)) []v1.ClusterRule {
	out := make([]v1.ClusterRule, 0, len(refs))
	for _, ref := range refs {
		r := v1.ClusterRule{Backends: []string{ref.name}, Tenants: tenantsFor(ref, cluster), Matchers: []v1.Matcher{}}
		if matchers != nil {
			r.Matchers = matchers(ref.typ)
		}
		if mod != nil {
			mod(&r)
		}
		out = append(out, r)
	}
	return out
}

func clusterPolicy(name string, groups []string, rules []v1.ClusterRule) *v1.ClusterAccessPolicy {
	return &v1.ClusterAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.ClusterAccessPolicySpec{Subjects: framework.Group(groups...), Rules: rules}}
}

func namespace(name, tenant string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if tenant != "" {
		ns.Labels = map[string]string{policy.TenantLabel: tenant}
	}
	return ns
}

func nsMatcher(value string) func(config.BackendType) []v1.Matcher {
	return func(t config.BackendType) []v1.Matcher { return []v1.Matcher{framework.Eq(nsLabel(t), value)} }
}

// hubObjects are the policies of the hub.
func hubObjects() []client.Object {
	return []client.Object{
		namespace(nsTeam, tenantAll),
		namespace(nsOther, tenantAll),
		&v1.AccessPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: nsTeam, Name: "hub-read"}, Spec: v1.AccessPolicySpec{
			Subjects: framework.Group(groupHubTeam),
			Rules:    []v1.Rule{{Backends: names(sharedBackends...)}},
		}},
		clusterPolicy("ops", []string{groupOps}, rulesFor(sharedBackends, hubCluster, func(t config.BackendType) []v1.Matcher {
			return []v1.Matcher{framework.Re(clusterLabel(t), hubCluster+"|"+clusterA)}
		}, nil)),
		clusterPolicy("global", []string{groupGlobal}, rulesFor(sharedBackends, hubCluster, nsMatcher(nsTeam),
			func(r *v1.ClusterRule) { r.AllClusters = true })),
		clusterPolicy("unscoped", []string{groupUnscoped}, rulesFor([]backendRef{metrics}, hubCluster, nsMatcher(nsTeam), nil)),
	}
}

// spokeObjects are the policies of one spoke. Namespace team has the tenant label in cluster-a and in
// cluster-c. In cluster-b it has no label, so the tenant comes from defaultTenant (PLAN §18.9).
func spokeObjects(cluster string) []client.Object {
	teamTenant := tenantAll
	if cluster == clusterB {
		teamTenant = ""
	}
	teamGroups, isoGroups := []string{groupTeam + cluster}, []string{groupIso + cluster}
	if cluster == clusterA || cluster == clusterB {
		teamGroups = append(teamGroups, groupBoth)
		isoGroups = append(isoGroups, groupIsoBoth)
	}
	objs := []client.Object{
		namespace(nsTeam, teamTenant),
		namespace(nsOther, tenantAll),
		&v1.AccessPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: nsTeam, Name: "read"}, Spec: v1.AccessPolicySpec{
			Subjects: framework.Group(teamGroups...),
			Rules:    []v1.Rule{{Backends: names(sharedBackends...)}},
		}},
		clusterPolicy("admin", []string{groupAdmin + cluster}, rulesFor(sharedBackends, cluster, nil,
			func(r *v1.ClusterRule) { r.Unrestricted = true })),
		clusterPolicy("iso", isoGroups, rulesFor(perClusterBackends, cluster, nsMatcher(nsTeam), nil)),
	}
	if cluster == clusterA {
		objs = append(objs,
			// A spoke matcher on the cluster label can only narrow (PLAN §18.3).
			clusterPolicy("sneaky", []string{groupSneaky}, rulesFor(sharedBackends, cluster, func(t config.BackendType) []v1.Matcher {
				return []v1.Matcher{framework.Eq(clusterLabel(t), clusterB)}
			}, nil)),
			// The tenant of cluster-b is not in the allow-list of cluster-a (PLAN §18.9).
			clusterPolicy("bad-tenant", []string{groupBadTenant}, rulesFor([]backendRef{logsPer}, clusterB, nsMatcher(nsTeam), nil)),
			// Namespace team from tenant "all" and namespace other from the tenant of cluster-a, on each backend
			// of the setup "one tenant for each cluster". Both tenants are in the allow-list of cluster-a.
			clusterPolicy("multi", []string{groupMulti}, append(
				rulesFor(perClusterBackends, cluster, nsMatcher(nsTeam), func(r *v1.ClusterRule) { r.Tenants = []string{tenantAll} }),
				rulesFor(perClusterBackends, cluster, nsMatcher(nsOther), nil)...)),
		)
	}
	return objs
}

// spokePolicyCount is the number of policies in spokeObjects.
func spokePolicyCount(cluster string) int {
	n := 0
	for _, o := range spokeObjects(cluster) {
		switch o.(type) {
		case *v1.AccessPolicy, *v1.ClusterAccessPolicy:
			n++
		}
	}
	return n
}
