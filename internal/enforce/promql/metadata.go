package promql

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
)

// matchParam is the enforcement parameter of the metadata endpoints.
const matchParam = "match[]"

// maxMatchExpansion caps (user match[] selectors) x (policy terms) per request.
const maxMatchExpansion = 256

// MaxLabelsResponseBytes is the largest "labels" response body FilterLabelsResponse accepts. The proxy
// should use it as Plan.ResponseFilterMaxBytes.
const MaxLabelsResponseBytes = 16 << 20

// planMetadata handles series, labels and label/:name/values (PLAN §7.2). The API returns the union of
// all match[] selectors, so one selector per (user match[] x term) expresses
// (user selectors) AND (term 1 OR term 2 ...) exactly. Without a user match[] there is one selector
// per term; omission is never a pass-through.
//
// A term without matchers (or Unrestricted) restricts nothing, so only the user's own selectors are
// kept. Denied labels are rejected in user selectors and in the name of label/:name/values; the names of
// the "labels" response are filtered by the Plan's ResponseFilter.
func planMetadata(ep routes.Endpoint, params url.Values, eff policy.Effective) (enforce.Plan, error) {
	if len(eff.Terms) == 0 && !eff.Unrestricted {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "effective policy grants no terms")
	}
	isValues := ep.PathParam != "" || strings.HasSuffix(ep.Template, "/values")
	isSeries := !isValues && (ep.Template == "series" || strings.HasSuffix(ep.Template, "/series"))
	isLabels := !isValues && (ep.Template == "labels" || strings.HasSuffix(ep.Template, "/labels"))
	if isValues && (slices.Contains(eff.DeniedLabels, ep.PathParam) || slices.Contains(eff.DeniedLabels, backendLabelName(ep.PathParam))) {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrDeniedLabel, "label %s denied by policy", ep.PathParam)
	}

	user := params[matchParam]
	userSel := make([][]*labels.Matcher, len(user))
	for i, s := range user {
		ms, err := promParser.ParseMetricSelector(s)
		if err != nil {
			return enforce.Plan{}, enforce.Errorf(enforce.ErrParse, "%s: %v", matchParam, err)
		}
		for _, m := range ms {
			if slices.Contains(eff.DeniedLabels, m.Name) {
				return enforce.Plan{}, enforce.Errorf(enforce.ErrDeniedLabel, "label %s denied by policy (matcher)", m.Name)
			}
		}
		userSel[i] = ms
	}

	strategy := "match_expand"
	var out []string
	if eff.Unrestricted || unrestrictedTerm(eff.Terms) {
		strategy = "passthrough"
		out = slices.Clone(user)
	} else {
		termMs := make([][]*labels.Matcher, len(eff.Terms))
		for i, t := range eff.Terms {
			ms, err := toLabelMatchers(t.Matchers)
			if err != nil {
				return enforce.Plan{}, err
			}
			termMs[i] = ms
		}
		if len(userSel) == 0 {
			userSel = [][]*labels.Matcher{nil}
		}
		if len(userSel)*len(termMs) > maxMatchExpansion {
			return enforce.Plan{}, enforce.Errorf(enforce.ErrTooComplex, "%d match[] selectors x %d policy terms exceed the limit of %d", len(userSel), len(termMs), maxMatchExpansion)
		}
		seen := map[string]bool{}
		for _, u := range userSel {
			for _, t := range termMs {
				sel, err := renderSelector(slices.Concat(u, t))
				if err != nil {
					return enforce.Plan{}, err
				}
				if !seen[sel] {
					seen[sel] = true
					out = append(out, sel)
				}
			}
		}
	}

	q := make(url.Values, len(params))
	for k, v := range params {
		q[k] = slices.Clone(v)
	}
	delete(q, matchParam)
	if len(out) > 0 {
		q[matchParam] = out
	}
	plan := enforce.Plan{
		Requests: []enforce.Upstream{{Query: q, Term: -1}},
		Merge:    enforce.MergeNone,
		Strategy: strategy,
	}
	if isLabels && len(eff.DeniedLabels) > 0 {
		denied := slices.Clone(eff.DeniedLabels)
		plan.ResponseFilter = func(body []byte) ([]byte, error) { return FilterLabelsResponse(body, denied) }
		plan.ResponseFilterMaxBytes = MaxLabelsResponseBytes
	}
	if isSeries && len(eff.DeniedLabels) > 0 {
		denied := slices.Clone(eff.DeniedLabels)
		plan.ResponseFilter = func(body []byte) ([]byte, error) { return FilterSeriesResponse(body, denied) }
		plan.ResponseFilterMaxBytes = MaxLabelsResponseBytes
	}
	return plan, nil
}

// backendLabelName returns the label name Prometheus 3 looks up for the path segment of label/:name/values:
// a name starting with "U__" is decoded with the values escaping scheme first (U__customer__id is
// customer_id), so the deny check must see the decoded name too. Backends without this decoding use the raw
// segment, which planMetadata checks as well.
func backendLabelName(name string) string {
	if strings.HasPrefix(name, "U__") {
		return model.UnescapeName(name, model.ValueEncodingEscaping)
	}
	return name
}

// renderSelector renders ms as a series selector and verifies that it parses again. A selector must
// contain a matcher that does not match the empty string; if the policy and user matchers do not, a
// matcher on the (always present) metric name is added, which can only narrow the selection.
func renderSelector(ms []*labels.Matcher) (string, error) {
	ms = slices.Clone(ms)
	if !slices.ContainsFunc(ms, func(m *labels.Matcher) bool { return !m.Matches("") }) {
		ms = append(ms, labels.MustNewMatcher(labels.MatchRegexp, metricNameLabel, ".+"))
	}
	s := (&parser.VectorSelector{LabelMatchers: ms}).String()
	if _, err := promParser.ParseMetricSelector(s); err != nil {
		return "", enforce.Errorf(enforce.ErrUnsupported, "expanded match[] %s does not re-parse: %v", s, err)
	}
	return s, nil
}

// FilterLabelsResponse removes the denied label names from the "data" array of a Prometheus "labels"
// API response and returns the new body. All other members of the envelope are preserved (member order
// may change). The whole body is held in memory, so it is limited to MaxLabelsResponseBytes; larger
// bodies, bodies that are not a JSON object, and a "data" member that is not an array of strings yield
// an error and the caller must not forward the original body. A body without "data" and without
// "status":"success" (an upstream error) is returned unchanged.
func FilterLabelsResponse(body []byte, denied []string) ([]byte, error) {
	if len(body) > MaxLabelsResponseBytes {
		return nil, fmt.Errorf("labels response exceeds %d bytes", MaxLabelsResponseBytes)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("labels response is not a JSON object: %w", err)
	}
	raw, ok := env["data"]
	if !ok {
		if string(env["status"]) == `"success"` {
			return nil, fmt.Errorf("labels response has no data member")
		}
		return body, nil
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, fmt.Errorf("labels response data is not an array of strings: %w", err)
	}
	kept := make([]string, 0, len(names))
	for _, n := range names {
		if !slices.Contains(denied, n) {
			kept = append(kept, n)
		}
	}
	var err error
	if env["data"], err = marshalNoEscape(kept); err != nil {
		return nil, err
	}
	return marshalNoEscape(env)
}

// FilterSeriesResponse removes the denied label names from every label set of a Prometheus "series" API
// response; see enforce.FilterSeriesResponse. Query results are deliberately not filtered.
func FilterSeriesResponse(body []byte, denied []string) ([]byte, error) {
	return enforce.FilterSeriesResponse(body, denied, MaxLabelsResponseBytes)
}

func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
