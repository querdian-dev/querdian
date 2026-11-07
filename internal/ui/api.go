package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/enforce/redact"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
)

const (
	maxBodyBytes = 64 << 10
	testTimeout  = 5 * time.Second
)

var opSymbol = map[string]string{"Equal": "=", "NotEqual": "!=", "RegexMatch": "=~", "RegexNoMatch": "!~"}

type matcherJSON struct {
	Name  string `json:"name"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

type ruleJSON struct {
	Backend      string          `json:"backend"`
	Type         string          `json:"type"`
	Tenants      []string        `json:"tenants"`
	Unrestricted bool            `json:"unrestricted"`
	Matchers     []matcherJSON   `json:"matchers"`
	DeniedLabels []string        `json:"deniedLabels"` // includes the redacted names
	Redactions   []redactionJSON `json:"redactions"`
	// Origin and Expired are set only with the multicluster block. Origin has Hub or Cluster only. An
	// expired rule grants no data, but its denied and redacted labels still apply (PLAN §18.6, rule 5).
	Origin  *originJSON `json:"origin,omitempty"`
	Expired bool        `json:"expired,omitempty"`
}

// originJSON tells where a policy comes from: the hub, or a policy object in a spoke. The dashboard sets it
// only with the multicluster block, so without the block the API is the same as before.
type originJSON struct {
	Hub bool `json:"hub"`
	// Cluster is the SpokeCluster name. Kind, Namespace and Name identify the source object in the spoke.
	Cluster   string `json:"cluster,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
}

func originOf(src policy.SpokeSource) originJSON {
	if src.Cluster == "" {
		return originJSON{Hub: true}
	}
	return originJSON{Cluster: src.Cluster, Kind: src.Kind, Namespace: src.Namespace, Name: src.Name}
}

// sourceJSON is a policy that has rules for a request or a backend, with its origin. Expired is true if
// the policy comes from a spoke that is expired or suspended now.
type sourceJSON struct {
	ID      string     `json:"id"`
	Origin  originJSON `json:"origin"`
	Expired bool       `json:"expired"`
}

// multicluster reports whether the static config has the multicluster block.
func (h *handler) multicluster() bool { return h.d.Config.Multicluster != nil }

// expired reports whether the rules of a spoke grant nothing at now. Hub rules (no origin) never expire.
func (h *handler) expired(origin string, now time.Time) bool {
	return origin != "" && h.d.Deadlines.Expired(origin, now)
}

// spokeSources maps each policy ID of the snapshot to its spoke source (the zero value for a hub policy).
func spokeSources(s *policy.Snapshot) map[string]policy.SpokeSource {
	byID := map[string]policy.SpokeSource{}
	for _, v := range s.Policies() {
		byID[v.ID] = v.Result.Spoke
	}
	return byID
}

// sources returns the policies of refs with their origins and expiry state, in the order of refs. byID
// comes from spokeSources.
func (h *handler) sources(byID map[string]policy.SpokeSource, refs []policy.Ref, now time.Time) []sourceJSON {
	out := make([]sourceJSON, 0, len(refs))
	for _, ref := range refs {
		src := byID[ref.ID]
		if src.Cluster != ref.Origin {
			// The rules are the truth. This occurs only for rules with no compile result, as in tests.
			src = policy.SpokeSource{Cluster: ref.Origin}
		}
		out = append(out, sourceJSON{ID: ref.ID, Origin: originOf(src), Expired: h.expired(ref.Origin, now)})
	}
	return out
}

// redactionJSON describes one labels.redact entry, with an example of its effect on a value.
type redactionJSON struct {
	Name     string `json:"name"`
	Strategy string `json:"strategy"`
	Octets   int    `json:"octets,omitempty"`
	Effect   string `json:"effect"`
	Before   string `json:"before"`
	After    string `json:"after"` // empty: the label is removed
}

func redactionViews(m map[string]policy.Redaction) []redactionJSON {
	out := make([]redactionJSON, 0, len(m))
	for name, red := range m {
		before, after := redact.Example(red)
		out = append(out, redactionJSON{Name: name, Strategy: string(red.Strategy), Octets: red.Octets,
			Effect: redact.Describe(red), Before: before, After: after})
	}
	slices.SortFunc(out, func(a, b redactionJSON) int { return strings.Compare(a.Name, b.Name) })
	return out
}

type policyJSON struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Namespace  string     `json:"namespace"`
	Name       string     `json:"name"`
	Generation int64      `json:"generation"`
	Ready      bool       `json:"ready"`
	Reason     string     `json:"reason"`
	Message    string     `json:"message"`
	Users      []string   `json:"users"`
	Groups     []string   `json:"groups"`
	Rules      []ruleJSON `json:"rules"`
	// Origin and Expired are set only with the multicluster block. Expired is true for a ready spoke policy
	// whose spoke is expired or suspended now.
	Origin  *originJSON `json:"origin,omitempty"`
	Expired bool        `json:"expired,omitempty"`
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (h *handler) backendType(name string) string {
	if b, ok := h.d.Config.Backend(name); ok {
		return string(b.Type)
	}
	return ""
}

func (h *handler) policyViews(s *policy.Snapshot, now time.Time) []policyJSON {
	views := s.Policies()
	mc := h.multicluster()
	out := make([]policyJSON, 0, len(views))
	for _, v := range views {
		p := policyJSON{ID: v.ID, Kind: v.Result.Kind, Namespace: v.Result.Namespace, Name: v.Result.Name,
			Generation: v.Result.Generation, Ready: v.Result.Ready, Reason: v.Result.Reason, Message: v.Result.Message,
			Users: nz(v.Users), Groups: nz(v.Groups), Rules: []ruleJSON{}}
		if mc {
			o := originOf(v.Result.Spoke)
			p.Origin = &o
			p.Expired = v.Result.Ready && h.expired(v.Result.Spoke.Cluster, now)
		}
		for _, r := range v.Rules {
			rj := ruleJSON{Backend: r.Backend, Type: h.backendType(r.Backend), Tenants: nz(r.Tenants),
				Unrestricted: r.Unrestricted, DeniedLabels: nz(r.DeniedLabels), Matchers: []matcherJSON{},
				Redactions: redactionViews(r.Redactions)}
			if mc {
				rj.Origin = &originJSON{Hub: r.Origin == "", Cluster: r.Origin}
				rj.Expired = h.expired(r.Origin, now)
			}
			for _, m := range r.Matchers {
				rj.Matchers = append(rj.Matchers, matcherJSON{Name: m.Name, Op: opSymbol[string(m.Op)], Value: m.Value})
			}
			p.Rules = append(p.Rules, rj)
		}
		out = append(out, p)
	}
	if mc {
		slices.SortStableFunc(out, compareByOrigin)
	}
	return out
}

// compareByOrigin puts hub policies first, by ID, and then spoke policies by cluster, kind, namespace and
// name, because their IDs are hashes. Both policies must have an origin.
func compareByOrigin(a, b policyJSON) int {
	if a.Origin.Hub != b.Origin.Hub {
		if a.Origin.Hub {
			return -1
		}
		return 1
	}
	for _, c := range [][2]string{{a.Origin.Cluster, b.Origin.Cluster}, {a.Origin.Kind, b.Origin.Kind},
		{a.Origin.Namespace, b.Origin.Namespace}, {a.Origin.Name, b.Origin.Name}, {a.ID, b.ID}} {
		if n := strings.Compare(c[0], c[1]); n != 0 {
			return n
		}
	}
	return 0
}

func (h *handler) overview(w http.ResponseWriter, _ *http.Request) {
	s, ok := h.snap(w)
	if !ok {
		return
	}
	var ready, notReady int
	for _, r := range s.Results() {
		if r.Ready {
			ready++
		} else {
			notReady++
		}
	}
	users, groups := s.Subjects()
	res := map[string]any{
		"version": h.d.Version, "generation": s.Generation,
		"policies": map[string]int{"total": ready + notReady, "ready": ready, "notReady": notReady},
		"subjects": map[string]int{"users": len(users), "groups": len(groups)},
		"backends": len(h.d.Config.Backends),
	}
	if h.multicluster() {
		var spoke, expired int
		for _, p := range h.policyViews(s, time.Now()) {
			if !p.Origin.Hub {
				spoke++
			}
			if p.Expired {
				expired++
			}
		}
		res["spokePolicies"] = map[string]int{"total": spoke, "expired": expired}
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *handler) policies(w http.ResponseWriter, _ *http.Request) {
	s, ok := h.snap(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"generation": s.Generation, "policies": h.policyViews(s, time.Now())})
}

func (h *handler) subjects(w http.ResponseWriter, _ *http.Request) {
	s, ok := h.snap(w)
	if !ok {
		return
	}
	users, groups := s.Subjects()
	writeJSON(w, http.StatusOK, map[string]any{"users": nz(users), "groups": nz(groups)})
}

type termJSON struct {
	Matchers []matcherJSON `json:"matchers"`
	Tenant   string        `json:"tenant"`
}

type accessJSON struct {
	Backend      string          `json:"backend"`
	Type         string          `json:"type"`
	Granted      bool            `json:"granted"`
	Reason       string          `json:"reason,omitempty"`
	Unrestricted bool            `json:"unrestricted"`
	Tenants      []string        `json:"tenants"`
	Terms        []termJSON      `json:"terms"`
	DeniedLabels []string        `json:"deniedLabels"` // includes the redacted names
	Redactions   []redactionJSON `json:"redactions"`
	Policies     []string        `json:"policies"`
	// Sources is set only with the multicluster block, and is absent if it is empty: each policy with rules
	// for the request on this backend, also if its spoke is expired. Expired rules give no terms, but their
	// labels stay in DeniedLabels and Redactions.
	Sources []sourceJSON `json:"sources,omitempty"`
}

func resolveReason(err error) string {
	switch {
	case errors.Is(err, policy.ErrNoGrant):
		return "no_grant"
	case errors.Is(err, policy.ErrPolicySourceExpired):
		return "policy_source_expired"
	case errors.Is(err, policy.ErrCrossTenantUnion):
		return "cross_tenant_union_unsupported"
	case errors.Is(err, policy.ErrTooComplex):
		return "policy_too_complex"
	case errors.Is(err, policy.ErrUnknownBackend):
		return "unknown_backend"
	}
	return "resolve_failed"
}

func splitGroups(v string) []string {
	var out []string
	for _, g := range strings.Split(v, ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

func (h *handler) access(w http.ResponseWriter, r *http.Request) {
	s, ok := h.snap(w)
	if !ok {
		return
	}
	user := r.URL.Query().Get("user")
	groups := splitGroups(r.URL.Query().Get("groups"))
	now := time.Now()
	var byID map[string]policy.SpokeSource
	if h.multicluster() {
		byID = spokeSources(s)
	}
	out := make([]accessJSON, 0, len(h.d.Config.Backends))
	for i := range h.d.Config.Backends {
		b := &h.d.Config.Backends[i]
		a := accessJSON{Backend: b.Name, Type: string(b.Type), Tenants: []string{}, Terms: []termJSON{},
			DeniedLabels: []string{}, Redactions: []redactionJSON{}, Policies: []string{}}
		if byID != nil {
			a.Sources = h.sources(byID, s.PoliciesFor(user, groups, b.Name), now)
		}
		eff, err := s.ResolveAt(user, groups, b.Name, h.d.Deadlines, now)
		if err != nil {
			a.Reason = resolveReason(err)
			out = append(out, a)
			continue
		}
		a.Granted, a.Unrestricted = true, eff.Unrestricted
		a.Tenants, a.DeniedLabels, a.Policies = nz(eff.Tenants), nz(eff.DeniedLabels), nz(eff.Policies)
		a.Redactions = redactionViews(eff.Redactions)
		for _, t := range eff.Terms {
			tj := termJSON{Tenant: t.Tenant, Matchers: []matcherJSON{}}
			for _, m := range t.Matchers {
				tj.Matchers = append(tj.Matchers, matcherJSON{Name: m.Name, Op: opSymbol[string(m.Op)], Value: m.Value})
			}
			a.Terms = append(a.Terms, tj)
		}
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "groups": nz(groups), "access": out})
}

func (h *handler) backends(w http.ResponseWriter, _ *http.Request) {
	s, ok := h.snap(w)
	if !ok {
		return
	}
	reach := map[string][]string{}
	refs := map[string][]policy.Ref{}
	byID := map[string]policy.SpokeSource{}
	for _, p := range s.Policies() {
		byID[p.ID] = p.Result.Spoke
		for _, r := range p.Rules {
			if !slices.Contains(reach[r.Backend], p.ID) {
				reach[r.Backend] = append(reach[r.Backend], p.ID)
				refs[r.Backend] = append(refs[r.Backend], policy.Ref{ID: p.ID, Origin: r.Origin})
			}
		}
	}
	type backendJSON struct {
		Name           string   `json:"name"`
		Type           string   `json:"type"`
		NamespaceLabel string   `json:"namespaceLabel"`
		TenantHeader   string   `json:"tenantHeader"`
		Federation     string   `json:"tenantFederation"`
		Policies       []string `json:"policies"`
		// Sources is set only with the multicluster block, and is absent if it is empty: Policies with their
		// origins and expiry state.
		Sources []sourceJSON `json:"sources,omitempty"`
	}
	now := time.Now()
	out := make([]backendJSON, 0, len(h.d.Config.Backends))
	for _, b := range h.d.Config.Backends {
		bj := backendJSON{Name: b.Name, Type: string(b.Type), NamespaceLabel: b.NamespaceLabel,
			TenantHeader: b.TenantHeader, Federation: string(b.TenantFederation), Policies: nz(reach[b.Name])}
		if h.multicluster() {
			bj.Sources = h.sources(byID, refs[b.Name], now)
		}
		out = append(out, bj)
	}
	writeJSON(w, http.StatusOK, map[string]any{"backends": out})
}

type testRequest struct {
	User    string   `json:"user"`
	Groups  []string `json:"groups"`
	Backend string   `json:"backend"`
	Path    string   `json:"path"`
	Query   string   `json:"query"`
}

type upstreamJSON struct {
	Term  int    `json:"term"`
	Query string `json:"query"`
}

// defaultPath is the enforced query endpoint used when the request names none.
var defaultPath = map[config.BackendType]string{
	config.BackendPrometheus: "/api/v1/query",
	config.BackendLoki:       "/loki/api/v1/query_range",
	config.BackendTempo:      "/api/search",
}

func (h *handler) test(w http.ResponseWriter, r *http.Request) {
	if mt := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]); mt != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	s, ok := h.snap(w)
	if !ok {
		return
	}
	var req testRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	b, found := h.d.Config.Backend(req.Backend)
	if !found {
		writeErr(w, http.StatusBadRequest, "unknown backend")
		return
	}
	if req.Path == "" {
		req.Path = defaultPath[b.Type]
	}
	ep := routes.Lookup(b.Type, req.Path, http.MethodGet)
	if ep.Kind != routes.KindEnforce || ep.Websocket {
		writeErr(w, http.StatusBadRequest, "path is not an enforced query endpoint of this backend type")
		return
	}
	res := map[string]any{"backend": b.Name, "type": string(b.Type), "path": req.Path, "param": ep.Param, "input": req.Query}
	deny := func(status int, reason, detail string) {
		res["allowed"], res["status"], res["reason"], res["detail"] = false, status, reason, detail
		writeJSON(w, http.StatusOK, res)
	}

	eff, err := s.ResolveAt(req.User, req.Groups, b.Name, h.d.Deadlines, time.Now())
	if err != nil {
		deny(http.StatusForbidden, resolveReason(err), err.Error())
		return
	}
	res["policies"] = nz(eff.Policies)
	params := url.Values{ep.Param: {req.Query}}
	var plan enforce.Plan
	if eff.Unrestricted && len(eff.DeniedLabels) == 0 {
		plan = enforce.Plan{Requests: []enforce.Upstream{{Query: params, Term: -1}}, Strategy: "passthrough"}
	} else {
		e := h.d.Enforcers[b.Type]
		if e == nil {
			deny(http.StatusServiceUnavailable, "enforcer_unavailable", "enforcement is not available for this backend type")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), testTimeout)
		defer cancel()
		if plan, err = e.Plan(ctx, ep, params, eff); err != nil {
			deny(enforce.HTTPStatus(err), enforce.ErrorType(err), enforce.Detail(err))
			return
		}
	}
	ups := make([]upstreamJSON, 0, len(plan.Requests))
	for _, u := range plan.Requests {
		ups = append(ups, upstreamJSON{Term: u.Term, Query: u.Query.Get(ep.Param)})
	}
	res["allowed"], res["strategy"], res["upstream"] = true, plan.Strategy, ups
	// How the response would be redacted: values rewritten in query results, names removed elsewhere.
	res["redactions"] = redactionViews(eff.Redactions)
	res["redactionMode"] = "none"
	if len(eff.Redactions) > 0 {
		res["redactionMode"] = "remove"
		if redact.RewritesValues(b.Type, ep.Template) {
			res["redactionMode"] = "rewrite"
		}
	}
	writeJSON(w, http.StatusOK, res)
}
