// Package spokesync is the wire protocol between the spoke agent and the hub receiver (PLAN §18.5), plus the
// helpers that both sides must compute identically: the state digest, the SpokePolicy object name and the
// identity check.
package spokesync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

const (
	// Path is the only path of the receiver; it accepts only POST with ContentType.
	Path = "/v1alpha1/sync"
	// ContentType is the media type of requests and responses.
	ContentType = "application/json"
	// ClusterLabel is the Kubernetes label on every SpokePolicy object that names its SpokeCluster.
	ClusterLabel = "policy.querdian.dev/cluster"
)

// Request is one sync from an agent. A heartbeat carries only Digest; a full sync sets Full and carries the
// complete state, which replaces the earlier state of the spoke.
type Request struct {
	// Digest is Digest(Policies) of the agent's complete state.
	Digest string `json:"digest"`
	// Full marks a full sync, so that an empty state is distinct from a heartbeat.
	Full bool `json:"full,omitempty"`
	// Policies is the complete state; only with Full.
	Policies []v1.SpokePolicySource `json:"policies,omitempty"`
}

// Result is the outcome of an accepted sync.
type Result string

const (
	// ResultOK means the stored state equals the agent's state and lastSyncTime was renewed.
	ResultOK Result = "ok"
	// ResultFullStateRequired means the digest differs from the stored state; the agent must send a full
	// sync. lastSyncTime was not renewed.
	ResultFullStateRequired Result = "fullStateRequired"
)

// Response is the body of an accepted sync (HTTP 200).
type Response struct {
	Result Result `json:"result"`
	// Conditions are the Ready conditions of the last compile, one per stored policy (PLAN §18.5). They can
	// describe an older generation than the one just sent; the next heartbeat carries the new state.
	Conditions []Condition `json:"conditions,omitempty"`
}

// Condition is the Ready condition of one spoke policy, for the agent to write to the spoke object.
type Condition struct {
	Kind      v1.SpokePolicyKind `json:"kind"`
	Namespace string             `json:"namespace,omitempty"`
	Name      string             `json:"name"`
	// SourceGeneration is the spoke generation that the condition describes.
	SourceGeneration int64  `json:"sourceGeneration"`
	Ready            bool   `json:"ready"`
	Reason           string `json:"reason"`
	Message          string `json:"message,omitempty"`
}

// ErrorResponse is the body of a rejected sync (any status other than 200). The receiver keeps the error
// text generic; it never echoes another spoke's data.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Key identifies a policy object within one spoke.
func Key(p *v1.SpokePolicySource) string {
	return string(p.Kind) + "\x00" + p.Namespace + "\x00" + p.Name
}

// ObjectName is the name of the SpokePolicy object for a spoke policy: a hash, so that names of different
// clusters and policies cannot collide and always fit the Kubernetes limit.
func ObjectName(cluster string, p *v1.SpokePolicySource) string {
	sum := sha256.Sum256([]byte(cluster + "\x00" + Key(p)))
	return "sp-" + hex.EncodeToString(sum[:])
}

// Digest is a canonical hash of a spoke's complete state. It does not depend on the order of the policies,
// and it treats absent, null, empty and zero values alike, so that an object read back from the API server
// (with defaults applied) has the digest of the object the agent sent. Duplicate keys are an error.
func Digest(policies []v1.SpokePolicySource) (string, error) {
	sorted := make([]*v1.SpokePolicySource, len(policies))
	for i := range policies {
		sorted[i] = &policies[i]
	}
	sort.Slice(sorted, func(i, j int) bool { return Key(sorted[i]) < Key(sorted[j]) })
	canon := make([]any, len(sorted))
	for i, p := range sorted {
		if i > 0 && Key(p) == Key(sorted[i-1]) {
			return "", fmt.Errorf("duplicate policy %s %s/%s", p.Kind, p.Namespace, p.Name)
		}
		b, err := json.Marshal(p)
		if err != nil {
			return "", err
		}
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			return "", err
		}
		canon[i], _ = prune(v)
	}
	b, err := json.Marshal(canon) // map keys are sorted by encoding/json
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// prune drops empty values from objects and reports whether v itself is non-empty. Array elements are kept
// (pruned inside), because their position and count carry meaning.
func prune(v any) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if pv, keep := prune(e); keep {
				t[k] = pv
			} else {
				delete(t, k)
			}
		}
		return t, len(t) > 0
	case []any:
		for i, e := range t {
			t[i], _ = prune(e)
		}
		return t, len(t) > 0
	case string:
		return t, t != ""
	case bool:
		return t, t
	case json.Number:
		return t, t.String() != "0"
	}
	return nil, false
}

// IdentityMatchesName reports whether a SpokeCluster identity (a URI SAN) is canonical and its last path
// segment is exactly name (PLAN §18.5). The check works on the raw text: an identity with an escape, a
// query, a fragment, user info or a backslash never matches, and neither does one without a host. A suffix
// of a segment does not match.
//
// The receiver compares spec.identity with CertIdentity of the client cert. Because only canonical
// identities match here, both comparisons see the same text.
func IdentityMatchesName(identity, name string) bool {
	if name == "" || strings.ContainsAny(identity, "%?#@\\") || !canonical(identity) {
		return false
	}
	i := strings.LastIndexByte(identity, '/')
	return i >= 0 && identity[i+1:] == name
}

// CertIdentity returns the text that the receiver compares with spec.identity for a URI SAN of a client cert.
func CertIdentity(u *url.URL) string { return u.String() }

// canonical reports whether s is an absolute URI with a host and a path, in the form that url.URL.String
// gives back.
func canonical(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != "" && u.Opaque == "" && u.Path != "" && u.String() == s
}
