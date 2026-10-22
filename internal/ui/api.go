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
	Backend      string        `json:"backend"`
	Type         string        `json:"type"`
	Tenants      []string      `json:"tenants"`
	Unrestricted bool          `json:"unrestricted"`
	Matchers     []matcherJSON `json:"matchers"`
	DeniedLabels []string      `json:"deniedLabels"`
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

func (h *handler) policyViews(s *policy.Snapshot) []policyJSON {
	views := s.Policies()
	out := make([]policyJSON, 0, len(views))
	for _, v := range views {
		p := policyJSON{ID: v.ID, Kind: v.Result.Kind, Namespace: v.Result.Namespace, Name: v.Result.Name,
			Generation: v.Result.Generation, Ready: v.Result.Ready, Reason: v.Result.Reason, Message: v.Result.Message,
			Users: nz(v.Users), Groups: nz(v.Groups), Rules: []ruleJSON{}}
		for _, r := range v.Rules {
			rj := ruleJSON{Backend: r.Backend, Type: h.backendType(r.Backend), Tenants: nz(r.Tenants),
				Unrestricted: r.Unrestricted, DeniedLabels: nz(r.DeniedLabels), Matchers: []matcherJSON{}}
			for _, m := range r.Matchers {
				rj.Matchers = append(rj.Matchers, matcherJSON{Name: m.Name, Op: opSymbol[string(m.Op)], Value: m.Value})
			}
			p.Rules = append(p.Rules, rj)
		}
		out = append(out, p)
	}
	return out
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
	writeJSON(w, http.StatusOK, map[string]any{
		"version": h.d.Version, "generation": s.Generation,
		"policies": map[string]int{"total": ready + notReady, "ready": ready, "notReady": notReady},
		"subjects": map[string]int{"users": len(users), "groups": len(groups)},
		"backends": len(h.d.Config.Backends),
	})
}

func (h *handler) policies(w http.ResponseWriter, _ *http.Request) {
	s, ok := h.snap(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"generation": s.Generation, "policies": h.policyViews(s)})
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
	Backend      string     `json:"backend"`
	Type         string     `json:"type"`
	Granted      bool       `json:"granted"`
	Reason       string     `json:"reason,omitempty"`
	Unrestricted bool       `json:"unrestricted"`
	Tenants      []string   `json:"tenants"`
	Terms        []termJSON `json:"terms"`
	DeniedLabels []string   `json:"deniedLabels"`
	Policies     []string   `json:"policies"`
}

func resolveReason(err error) string {
	switch {
	case errors.Is(err, policy.ErrNoGrant):
		return "no_grant"
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
	out := make([]accessJSON, 0, len(h.d.Config.Backends))
	for i := range h.d.Config.Backends {
		b := &h.d.Config.Backends[i]
		a := accessJSON{Backend: b.Name, Type: string(b.Type), Tenants: []string{}, Terms: []termJSON{},
			DeniedLabels: []string{}, Policies: []string{}}
		eff, err := s.Resolve(user, groups, b.Name)
		if err != nil {
			a.Reason = resolveReason(err)
			out = append(out, a)
			continue
		}
		a.Granted, a.Unrestricted = true, eff.Unrestricted
		a.Tenants, a.DeniedLabels, a.Policies = nz(eff.Tenants), nz(eff.DeniedLabels), nz(eff.Policies)
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
	for _, p := range s.Policies() {
		for _, r := range p.Rules {
			if !slices.Contains(reach[r.Backend], p.ID) {
				reach[r.Backend] = append(reach[r.Backend], p.ID)
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
	}
	out := make([]backendJSON, 0, len(h.d.Config.Backends))
	for _, b := range h.d.Config.Backends {
		out = append(out, backendJSON{Name: b.Name, Type: string(b.Type), NamespaceLabel: b.NamespaceLabel,
			TenantHeader: b.TenantHeader, Federation: string(b.TenantFederation), Policies: nz(reach[b.Name])})
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

	eff, err := s.Resolve(req.User, req.Groups, b.Name)
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
	writeJSON(w, http.StatusOK, res)
}
