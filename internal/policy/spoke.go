package policy

import (
	"k8s.io/apimachinery/pkg/util/validation"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

// spokeEntry is one SpokeCluster name of the input. dup is true if the name occurs more than once; such a
// name is not trusted.
type spokeEntry struct {
	sc  *v1.SpokeCluster
	dup bool
}

func indexSpokeClusters(in []v1.SpokeCluster) map[string]*spokeEntry {
	out := make(map[string]*spokeEntry, len(in))
	for i := range in {
		sc := &in[i]
		if e, ok := out[sc.Name]; ok {
			e.dup = true
			continue
		}
		out[sc.Name] = &spokeEntry{sc: sc}
	}
	return out
}

// spokeOrigin finds and checks the SpokeCluster of a spoke policy (PLAN §18.4, §18.5). Any doubt gives an
// error, so the policy grants nothing.
func spokeOrigin(cfg *config.Config, spokes map[string]*spokeEntry, cluster string) (origin, *v1.SpokeCluster, *compileError) {
	e, ok := spokes[cluster]
	if !ok {
		return origin{}, nil, fail(ReasonSpokeClusterNotFound, "no SpokeCluster %q exists", cluster)
	}
	if e.dup {
		return origin{}, nil, fail(ReasonSpokeClusterInvalid, "SpokeCluster %q occurs more than once", cluster)
	}
	if cluster == cfg.Multicluster.HubCluster {
		// Otherwise a spoke admin could grant hub data.
		return origin{}, nil, fail(ReasonSpokeClusterInvalid, "SpokeCluster %q has the name of the hub cluster", cluster)
	}
	if !spokesync.IdentityMatchesName(e.sc.Spec.Identity, cluster) {
		return origin{}, nil, fail(ReasonSpokeClusterInvalid, "the last path segment of the identity of SpokeCluster %q is not the SpokeCluster name", cluster)
	}
	allowed := make(map[string]bool, len(e.sc.Spec.Tenants))
	for _, t := range e.sc.Spec.Tenants {
		if t != "" {
			allowed[t] = true
		}
	}
	return origin{spoke: cluster, tenants: allowed, version: SpokeVersion{UID: e.sc.UID, Generation: e.sc.Generation}}, e.sc, nil
}

// checkSpokeSource checks the structure of a SpokePolicy. The CRD has CEL rules for most of these checks, but
// the compiler does not rely on CEL.
func checkSpokeSource(p *v1.SpokePolicy) *compileError {
	src := &p.Spec.SpokePolicySource
	if c := p.Spec.Cluster; len(c) == 0 || len(c) > 63 || len(validation.IsDNS1123Subdomain(c)) > 0 {
		return fail(ReasonInvalidSpokePolicy, "spec.cluster %q is not a valid SpokeCluster name", c)
	}
	if len(validation.IsDNS1123Subdomain(src.Name)) > 0 {
		return fail(ReasonInvalidSpokePolicy, "spec.name %q is not a valid object name", src.Name)
	}
	switch src.Kind {
	case v1.SpokeClusterAccessPolicy:
		if src.ClusterAccessPolicy == nil || src.AccessPolicy != nil || src.Namespace != "" || src.TenantLabel != "" {
			return fail(ReasonInvalidSpokePolicy, "kind, namespace and spec do not agree")
		}
	case v1.SpokeAccessPolicy:
		if src.AccessPolicy == nil || src.ClusterAccessPolicy != nil || src.Namespace == "" {
			return fail(ReasonInvalidSpokePolicy, "kind, namespace and spec do not agree")
		}
		if errs := validation.IsDNS1123Label(src.Namespace); len(errs) > 0 {
			return fail(ReasonInvalidSpokePolicy, "namespace %q is not a valid namespace name", src.Namespace)
		}
	default:
		return fail(ReasonInvalidSpokePolicy, "unknown kind %q", src.Kind)
	}
	// The receiver names each object with spokesync.ObjectName. A different name means that the object did
	// not come from the receiver, and two such objects could describe the same spoke policy.
	if p.Name != spokesync.ObjectName(p.Spec.Cluster, src) {
		return fail(ReasonInvalidSpokePolicy, "the object name does not agree with the cluster, kind, namespace and name")
	}
	return nil
}

// checkSpokeSpecSize applies the size limits to the spec of a SpokePolicy. checkSpokeSource has made sure
// that the spec of the kind is set.
func checkSpokeSpecSize(src *v1.SpokePolicySource) *compileError {
	var err error
	if src.Kind == v1.SpokeClusterAccessPolicy {
		err = CheckClusterSpecSize(src.ClusterAccessPolicy)
	} else {
		err = CheckSpecSize(src.AccessPolicy)
	}
	if err != nil {
		return fail(ReasonPolicyTooLarge, "%v", err)
	}
	return nil
}

// compileSpokePolicy compiles one SpokePolicy (PLAN §18.3). It returns the subjects of the policy with its
// rules. Each rule carries the implicit cluster matcher of its spoke and has Origin set.
func compileSpokePolicy(cfg *config.Config, spokes map[string]*spokeEntry, p *v1.SpokePolicy) ([]*CompiledRule, v1.Subjects, *compileError) {
	if cfg.Multicluster == nil {
		return nil, v1.Subjects{}, fail(ReasonMulticlusterDisabled, "the static config has no multicluster block, so spoke policies grant nothing")
	}
	if err := checkSpokeSource(p); err != nil {
		return nil, v1.Subjects{}, err
	}
	if err := checkSpokeSpecSize(&p.Spec.SpokePolicySource); err != nil {
		return nil, v1.Subjects{}, err
	}
	o, sc, err := spokeOrigin(cfg, spokes, p.Spec.Cluster)
	if err != nil {
		return nil, v1.Subjects{}, err
	}
	src := &p.Spec.SpokePolicySource
	policy := KindSpokePolicy + "/" + p.Name
	if src.Kind == v1.SpokeClusterAccessPolicy {
		rules, err := compileClusterRules(cfg, policy, src.ClusterAccessPolicy.Rules, o)
		return rules, src.ClusterAccessPolicy.Subjects, err
	}
	// The tenant comes from the namespace label in the spoke, then from defaultTenant (PLAN §18.9). Hub
	// namespace labels never apply to a spoke namespace. The text is not trimmed: the allow-list check needs
	// an exact match.
	tenant := src.TenantLabel
	if tenant == "" {
		tenant = sc.Spec.DefaultTenant
	}
	rules, err := compileNamespacedRules(cfg, policy, src.Namespace, src.AccessPolicy.Rules, tenant, o)
	return rules, src.AccessPolicy.Subjects, err
}
