package agent

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

// ReadState reads all ClusterAccessPolicy, AccessPolicy and Namespace objects with r and returns one
// SpokePolicySource for each policy, sorted by spokesync.Key. r must read directly from the API server.
//
// ReadState returns an error if one list fails, or if the namespace of an AccessPolicy is not in the
// namespace list. A partial state is never returned: the receiver replaces its state with the state that
// the agent sends, so a missing policy revokes its grant and a missing tenant label can select a different
// tenant.
func ReadState(ctx context.Context, r client.Reader) ([]v1.SpokePolicySource, error) {
	policies, _, err := readState(ctx, r)
	return policies, err
}

// readObject is the UID and the status of one policy object from the read.
type readObject struct {
	uid    types.UID
	status *v1.PolicyStatus
}

// readState is ReadState. It also returns the UID and the status of each object by spokesync.Key, so that
// the agent does not read an object again to find that its status is current, and does not write a
// condition to a different object with the same name.
func readState(ctx context.Context, r client.Reader) ([]v1.SpokePolicySource, map[string]readObject, error) {
	var caps v1.ClusterAccessPolicyList
	if err := r.List(ctx, &caps); err != nil {
		return nil, nil, fmt.Errorf("list ClusterAccessPolicy: %w", err)
	}
	// List AccessPolicy objects before namespaces. A namespace stays until all its objects are deleted,
	// so the namespace of each AccessPolicy is normally in the later namespace list. If the namespace was
	// deleted and created again between the two lists, it is newer than the AccessPolicy, and the read
	// fails. The creation time has a resolution of one second, so the next heartbeat repairs a re-creation
	// in the same second.
	var aps v1.AccessPolicyList
	if err := r.List(ctx, &aps); err != nil {
		return nil, nil, fmt.Errorf("list AccessPolicy: %w", err)
	}
	var nss corev1.NamespaceList
	if err := r.List(ctx, &nss); err != nil {
		return nil, nil, fmt.Errorf("list Namespace: %w", err)
	}
	nsByName := make(map[string]*corev1.Namespace, len(nss.Items))
	for i := range nss.Items {
		nsByName[nss.Items[i].Name] = &nss.Items[i]
	}

	out := make([]v1.SpokePolicySource, 0, len(caps.Items)+len(aps.Items))
	status := make(map[string]readObject, len(caps.Items)+len(aps.Items))
	for i := range caps.Items {
		p := &caps.Items[i]
		if p.Generation < 1 {
			return nil, nil, fmt.Errorf("ClusterAccessPolicy %q has no generation", p.Name)
		}
		out = append(out, v1.SpokePolicySource{
			Kind:                v1.SpokeClusterAccessPolicy,
			Name:                p.Name,
			Generation:          p.Generation,
			ClusterAccessPolicy: p.Spec.DeepCopy(),
		})
		status[spokesync.Key(&out[len(out)-1])] = readObject{uid: p.UID, status: &p.Status}
	}
	for i := range aps.Items {
		p := &aps.Items[i]
		if p.Generation < 1 {
			return nil, nil, fmt.Errorf("AccessPolicy %s/%s has no generation", p.Namespace, p.Name)
		}
		ns, ok := nsByName[p.Namespace]
		if !ok {
			return nil, nil, fmt.Errorf("namespace %q of AccessPolicy %q is not in the namespace list", p.Namespace, p.Name)
		}
		if ns.CreationTimestamp.After(p.CreationTimestamp.Time) {
			return nil, nil, fmt.Errorf("namespace %q is newer than its AccessPolicy %q", p.Namespace, p.Name)
		}
		out = append(out, v1.SpokePolicySource{
			Kind:         v1.SpokeAccessPolicy,
			Namespace:    p.Namespace,
			Name:         p.Name,
			Generation:   p.Generation,
			TenantLabel:  ns.Labels[policy.TenantLabel],
			AccessPolicy: p.Spec.DeepCopy(),
		})
		status[spokesync.Key(&out[len(out)-1])] = readObject{uid: p.UID, status: &p.Status}
	}
	sort.Slice(out, func(i, j int) bool { return spokesync.Key(&out[i]) < spokesync.Key(&out[j]) })
	return out, status, nil
}
