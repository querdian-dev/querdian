package fanout

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/querdian-dev/querdian/pkg/traceql"
)

// Tempo response rewriting (PLAN §7.4, T4.4). The response shapes were recorded black-box against Tempo
// 2.9.0 (testdata/tempo/record.sh).
//
// # Search (api/search)
//
// The injected policy (pkg/traceql.Inject) guarantees that every span listed in spanSet/spanSets satisfies
// the policy. It says nothing about the other spans of the trace, in particular not about the root span.
// Tempo nevertheless reports trace-level data derived from all spans of the trace:
//
//   - rootServiceName, rootTraceName: name of the root span, also when only a child span matched;
//   - serviceStats: every service of the trace with span and error counts;
//   - durationMs: extent of the whole trace (not the root span duration);
//   - startTimeUnixNano: earliest span start of the whole trace.
//
// Decision: unless the root span is known to satisfy the policy, RedactTempoSearch removes rootServiceName,
// rootTraceName and serviceStats (including the literal "<root span not yet received>" placeholder) and
// recomputes startTimeUnixNano and durationMs from the returned matched spans only (earliest matched start;
// extent from the earliest start to the latest end of the matched spans, rounded up to whole milliseconds).
// The recomputed values are derived exclusively from policy-matching spans and so cannot leak, while the
// Grafana result table keeps a start time and a duration. Both are therefore not the Tempo trace-level
// values any more; this is documented behaviour. A trace without any returned span loses both fields.
//
// The rewrite is an allow-list: per trace only traceID, spanSet, spanSets and the two recomputed fields
// survive, at top level only traces and metrics. Fields added by future Tempo versions are dropped rather
// than leaked. Matched spans and their attributes are copied byte for byte.
//
// When the root span is known to be covered by the policy (RootCovered in internal/enforce/traceql: the
// effective policy is unrestricted) the body is returned unchanged.
//
// # Tag lists (api/v2/search/tags, api/v2/search/tag/:name/values)
//
// The enforcer injects the policy into q so that only permitted spans are scanned, but the tag endpoints
// still list every attribute name of those spans, including denied ones. FilterTempoTags removes denied
// names (see deniedTag for the matching rules) and FilterTempoTagValues refuses a denied attribute.

// ErrTempoResponse is returned for upstream bodies that do not have the expected Tempo shape. The proxy maps
// every Merger error to 502, so a malformed upstream response is never passed on unredacted.
var ErrTempoResponse = errors.New("unexpected tempo response")

// TempoMerger has the signature of internal/proxy.Merger and is assignable to it.
type TempoMerger = func(w http.ResponseWriter, r *http.Request, resps []Response) error

// NewTempoSearchMerger returns the merger for enforce.MergeTempoSearch. rootCovered says whether the root
// span of every returned trace is known to satisfy the policy (see RedactTempoSearch); when false, trace-level
// metadata is redacted. The plan has exactly one upstream request, so exactly one response is expected.
func NewTempoSearchMerger(rootCovered bool) TempoMerger {
	return func(w http.ResponseWriter, _ *http.Request, resps []Response) error {
		if len(resps) != 1 {
			return fmt.Errorf("%w: %d responses for a single request", ErrTempoResponse, len(resps))
		}
		out, err := RedactTempoSearch(resps[0].Body, rootCovered)
		if err != nil {
			return err
		}
		writeRawJSON(w, out)
		return nil
	}
}

// NewTempoTagsMerger returns the merger for the tag endpoints. For api/v2/search/tags pass valuesFor == "";
// for api/v2/search/tag/:name/values pass the path parameter name. denied are the effective DeniedLabels.
func NewTempoTagsMerger(denied []string, valuesFor string) TempoMerger {
	return func(w http.ResponseWriter, _ *http.Request, resps []Response) error {
		if len(resps) != 1 {
			return fmt.Errorf("%w: %d responses for a single request", ErrTempoResponse, len(resps))
		}
		var (
			out []byte
			err error
		)
		if valuesFor == "" {
			out, err = FilterTempoTags(resps[0].Body, denied)
		} else {
			out, err = FilterTempoTagValues(resps[0].Body, valuesFor, denied)
		}
		if err != nil {
			return err
		}
		writeRawJSON(w, out)
		return nil
	}
}

// writeRawJSON writes an already encoded JSON body as a 200 response.
func writeRawJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// searchTrace is the allow-listed shape of one trace in a rewritten search response.
type searchTrace struct {
	TraceID           json.RawMessage `json:"traceID"`
	StartTimeUnixNano string          `json:"startTimeUnixNano,omitempty"`
	DurationMs        *int64          `json:"durationMs,omitempty"`
	SpanSet           json.RawMessage `json:"spanSet,omitempty"`
	SpanSets          json.RawMessage `json:"spanSets,omitempty"`
}

type searchResponse struct {
	Traces  *[]searchTrace  `json:"traces,omitempty"`
	Metrics json.RawMessage `json:"metrics,omitempty"`
}

// RedactTempoSearch rewrites an api/search JSON response body as described in the package notes above.
// With rootCovered the body is returned unchanged; otherwise trace-level metadata is removed or recomputed
// from the matched spans. Malformed bodies yield an error wrapping ErrTempoResponse (fail closed).
func RedactTempoSearch(body []byte, rootCovered bool) ([]byte, error) {
	if rootCovered {
		return body, nil
	}
	var in struct {
		Traces *[]struct {
			TraceID  json.RawMessage `json:"traceID"`
			SpanSet  json.RawMessage `json:"spanSet"`
			SpanSets json.RawMessage `json:"spanSets"`
		} `json:"traces"`
		Metrics json.RawMessage `json:"metrics"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTempoResponse, err)
	}
	out := searchResponse{Metrics: in.Metrics}
	if in.Traces != nil {
		traces := make([]searchTrace, 0, len(*in.Traces))
		for _, t := range *in.Traces {
			st := searchTrace{TraceID: t.TraceID, SpanSet: t.SpanSet, SpanSets: t.SpanSets}
			if isNull(st.SpanSet) {
				st.SpanSet = nil
			}
			if isNull(st.SpanSets) {
				st.SpanSets = nil
			}
			start, dur, ok, err := matchedExtent(st.SpanSet, st.SpanSets)
			if err != nil {
				return nil, err
			}
			if ok {
				st.StartTimeUnixNano = strconv.FormatUint(start, 10)
				ms := int64(dur / 1_000_000) //nolint:gosec // at most 1.8e13
				if dur%1_000_000 != 0 {
					ms++
				}
				st.DurationMs = &ms
			}
			traces = append(traces, st)
		}
		out.Traces = &traces
	}
	return json.Marshal(out)
}

func isNull(r json.RawMessage) bool {
	return len(r) == 0 || bytes.Equal(bytes.TrimSpace(r), []byte("null"))
}

type spanSetJSON struct {
	Spans []struct {
		Start    flexUint `json:"startTimeUnixNano"`
		Duration flexUint `json:"durationNanos"`
	} `json:"spans"`
}

// flexUint decodes a uint64 that Tempo's JSON writes as a string (protobuf JSON mapping) but that may
// also appear as a number.
type flexUint uint64

func (f *flexUint) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad number %q", ErrTempoResponse, s)
	}
	*f = flexUint(v)
	return nil
}

// matchedExtent returns the earliest start and the extent (nanoseconds) of all spans in spanSet/spanSets.
// ok is false when there is no span.
func matchedExtent(spanSet, spanSets json.RawMessage) (start, dur uint64, ok bool, err error) {
	var sets []spanSetJSON
	if !isNull(spanSets) {
		if err := json.Unmarshal(spanSets, &sets); err != nil {
			return 0, 0, false, fmt.Errorf("%w: spanSets: %w", ErrTempoResponse, err)
		}
	}
	if !isNull(spanSet) {
		var s spanSetJSON
		if err := json.Unmarshal(spanSet, &s); err != nil {
			return 0, 0, false, fmt.Errorf("%w: spanSet: %w", ErrTempoResponse, err)
		}
		sets = append(sets, s)
	}
	minStart, maxEnd := uint64(math.MaxUint64), uint64(0)
	for _, s := range sets {
		for _, sp := range s.Spans {
			st := uint64(sp.Start)
			end := st + uint64(sp.Duration)
			if end < st { // overflow
				return 0, 0, false, fmt.Errorf("%w: span end overflows", ErrTempoResponse)
			}
			ok = true
			minStart = min(minStart, st)
			maxEnd = max(maxEnd, end)
		}
	}
	if !ok {
		return 0, 0, false, nil
	}
	return minStart, maxEnd - minStart, true, nil
}

// FilterTempoTags removes denied attribute names from an api/v2/search/tags JSON body. A tag is dropped
// when deniedTag matches it within its scope; a scope that becomes empty is dropped. The legacy v1 shape
// (tagNames) is filtered as well, matching names of any scope. Other fields (metrics) are preserved.
func FilterTempoTags(body []byte, denied []string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTempoResponse, err)
	}
	if raw, ok := top["scopes"]; ok && !isNull(raw) {
		var scopes []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &scopes); err != nil {
			return nil, fmt.Errorf("%w: scopes: %w", ErrTempoResponse, err)
		}
		kept := make([]map[string]json.RawMessage, 0, len(scopes))
		for _, sc := range scopes {
			var name string
			if err := json.Unmarshal(sc["name"], &name); err != nil {
				return nil, fmt.Errorf("%w: scope name: %w", ErrTempoResponse, err)
			}
			var tags []string
			if raw := sc["tags"]; !isNull(raw) {
				if err := json.Unmarshal(raw, &tags); err != nil {
					return nil, fmt.Errorf("%w: scope tags: %w", ErrTempoResponse, err)
				}
			}
			filtered := make([]string, 0, len(tags))
			for _, t := range tags {
				if !deniedTag(denied, name, t) {
					filtered = append(filtered, t)
				}
			}
			if len(filtered) == 0 && len(tags) > 0 {
				continue
			}
			b, _ := json.Marshal(filtered)
			sc["tags"] = b
			kept = append(kept, sc)
		}
		b, _ := json.Marshal(kept)
		top["scopes"] = b
	}
	if raw, ok := top["tagNames"]; ok && !isNull(raw) {
		var names []string
		if err := json.Unmarshal(raw, &names); err != nil {
			return nil, fmt.Errorf("%w: tagNames: %w", ErrTempoResponse, err)
		}
		filtered := make([]string, 0, len(names))
		for _, n := range names {
			if !deniedTag(denied, "", n) {
				filtered = append(filtered, n)
			}
		}
		b, _ := json.Marshal(filtered)
		top["tagNames"] = b
	}
	return json.Marshal(top)
}

// FilterTempoTagValues validates an api/v2/search/tag/:name/values JSON body for the attribute name (the
// path parameter, possibly scoped such as resource.service.name). The enforcer already answers 403 for a
// denied name; this is the second line of defence and fails when name is denied. Values of permitted
// attributes are returned unchanged: they are computed over spans that satisfy the injected policy.
func FilterTempoTagValues(body []byte, name string, denied []string) ([]byte, error) {
	scope, n := traceql.SplitAttr(name)
	if deniedTag(denied, scope, n) {
		return nil, fmt.Errorf("attribute %s denied by policy", name)
	}
	var probe struct {
		TagValues json.RawMessage `json:"tagValues"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTempoResponse, err)
	}
	return body, nil
}

// deniedTag reports whether the listed tag in the given scope (a Tempo scope name such as "span",
// "resource", "intrinsic" or "", or the scope part of a path parameter) is covered by a denied entry.
//
// Denied entries are scoped names (span.x, resource.x) or unscoped (x, .x). Rules:
//   - an unscoped denied name hides the tag in every scope;
//   - a scoped denied name hides it in that scope only;
//   - the intrinsic scope (and an empty scope) is treated as "any scope" because an intrinsic such as
//     "name" or "span:name" is not distinguishable by a span./resource. prefix; the part of a "scope:name"
//     intrinsic after the colon is compared too. This can only hide more than strictly needed;
//   - a denied entry equal to the listed text hides it verbatim.
func deniedTag(denied []string, scope, tag string) bool {
	intrinsic := scope == "" || scope == "intrinsic"
	bare := tag
	if i := strings.IndexByte(tag, ':'); i > 0 {
		bare = tag[i+1:]
	}
	for _, d := range denied {
		if d == tag {
			return true
		}
		ds, dn := traceql.SplitAttr(d)
		if dn == tag || (intrinsic && dn == bare) {
			if intrinsic || ds == "" || ds == scope {
				return true
			}
		}
	}
	return false
}
