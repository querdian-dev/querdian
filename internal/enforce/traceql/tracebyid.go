package traceql

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	commonv1 "github.com/querdian-dev/querdian/internal/tempopb/common/v1"
	tracev1 "github.com/querdian-dev/querdian/internal/tempopb/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
	"github.com/querdian-dev/querdian/pkg/traceql"
)

// Trace by ID (PLAN §7.4, T4.5), routes api/traces/:id (response: Trace) and api/v2/traces/:id (response:
// TraceByIDResponse), JSON and protobuf. Tempo has no policy-aware trace lookup, so for traceByID: filter
// the upstream response is rewritten in-process by a TraceFilter.
//
// # Decisions
//
//   - Mode. NewWithOptions takes the backend's traceByID mode. deny (the default of New) answers 403;
//     filter plans one upstream request with only start/end forwarded. A client q (and every other
//     parameter) is rejected with 400; the route allow-list already does so before the enforcer runs.
//   - Matching. A span is kept when at least one DNF term is satisfied; a term is satisfied when all its
//     matchers are. Matchers are evaluated against the span's attributes and those of its resource
//     according to the matcher scope (resource.x, span.x, or .x for either). Regexes are anchored
//     (^(?:v)$, RE2) like the injected query.
//   - Missing attributes, one rule for every operator. A matcher is satisfied only by an attribute that
//     is present, and has a string value, and passes the test. An absent attribute never satisfies Equal,
//     NotEqual, RegexMatch or RegexNoMatch. That is TraceQL's own behaviour (verified black-box on Tempo
//     2.9.0: `!=`/`!~` do not return spans lacking the attribute, `= ""` and `=~ ""` match nothing, and an
//     integer attribute never satisfies a string comparison, also not `!=`), so the filter returns the
//     spans a search for the same policy would match, never more. The policy-level reading "absent equals
//     empty" would over-grant here and is not used. For the unscoped form .x with the attribute present on
//     both span and resource, positive operators need one of the values to match, negative operators need
//     every present value to pass (the conservative reading).
//   - Pruning. Spans outside the policy are dropped, then scope-spans and resource-spans without spans.
//     When nothing remains the answer is 404 with an empty body (what Tempo sends for an unknown trace),
//     also for v2 where Tempo itself answers 200 with an empty trace.
//   - Denied attributes. Span, event, link, scope and resource attributes whose name is denied by the
//     policy (same matching as the query check) are removed from the spans that are kept.
//   - Anything else in the response is passed on unchanged (v2 metrics, status, message).

// Options configure the enforcer.
type Options struct {
	// TraceByID is the backend's traceByID mode. The empty value means deny.
	TraceByID config.TraceByID
}

// NewWithOptions returns the TraceQL enforcer for a Tempo backend with the given options. New is equivalent
// to NewWithOptions(Options{}).
func NewWithOptions(o Options) enforce.Enforcer { return enforcer{opt: o} }

func (e enforcer) planTraceByID(ep routes.Endpoint, params url.Values, eff policy.Effective) (enforce.Plan, error) {
	if e.opt.TraceByID != config.TraceByIDFilter {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrDeniedLabel, "trace lookup by ID is disabled")
	}
	for k := range params {
		if k != "start" && k != "end" {
			return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "parameter %q is not supported on trace lookup by ID", k)
		}
	}
	f, err := NewTraceFilter(eff, TraceFilterOptions{V2: ep.Template == tmplTraceV2})
	if err != nil {
		return enforce.Plan{}, enforce.Errorf(enforce.ErrUnsupported, "%v", err)
	}
	strategy := "filter"
	if f.Passthrough() {
		strategy = "passthrough"
	}
	return enforce.Plan{Requests: []enforce.Upstream{{Query: cloneValues(params), Term: -1}}, Merge: enforce.MergeNone, Strategy: strategy}, nil
}

const (
	tmplTraceV1 = "/api/traces/:id"
	tmplTraceV2 = "/api/v2/traces/:id"
)

// TraceFilterOptions configure a TraceFilter.
type TraceFilterOptions struct {
	// V2 selects the api/v2/traces response shape (protobuf TraceByIDResponse); false is api/traces (Trace).
	// JSON bodies of both shapes are recognised by their keys.
	V2 bool
	// MaxBytes caps the (decompressed) upstream body that ModifyResponse buffers; values < 1 use 64 MiB.
	MaxBytes int64
}

const defaultMaxTraceBytes = 64 << 20

// TraceFilter removes the spans a policy does not admit from a trace-by-ID response. Create one per
// request with NewTraceFilter; it is stateless afterwards and safe for concurrent use.
//
// # Hooking it in
//
// After enforce.Plan succeeded for an endpoint with Kind routes.KindTraceByID (the server must call the
// enforcer for it instead of answering enforcer_unavailable, and may skip the plan for the
// deny mode as it does today), build the filter from the same effective policy and install its
// ModifyResponse on the httputil.ReverseProxy of the single-request path (proxy.Proxy.stream), for
// example through a new optional field proxy.Target.ModifyResponse that stream() calls after deleting
// the X-Trace-Id header. The request is streamed to Tempo with the client's Accept header, so the upstream
// answer is JSON or protobuf as requested; ModifyResponse buffers it (capped), filters it in the same
// format and replaces the response, answering 404 when nothing remains. Errors make the ReverseProxy
// ErrorHandler answer 502, so an unfilterable body (unknown media type, undecodable protobuf, over the
// cap) is never forwarded.
type TraceFilter struct {
	terms  [][]condition
	all    bool // some term admits every span
	denied []string
	opt    TraceFilterOptions
}

type condition struct {
	scope string // "resource", "span" or "" for either
	name  string
	op    v1.MatchOp
	value string
	re    *regexp.Regexp
}

// NewTraceFilter compiles eff for in-process evaluation. It fails for terms the Tempo enforcer cannot
// express either (tenant-bound terms, __tenant_id__ matchers, unknown operators, names without a
// resource./span./. scope).
func NewTraceFilter(eff policy.Effective, opt TraceFilterOptions) (*TraceFilter, error) {
	if opt.MaxBytes < 1 {
		opt.MaxBytes = defaultMaxTraceBytes
	}
	f := &TraceFilter{denied: eff.DeniedLabels, opt: opt}
	if eff.Unrestricted {
		f.all = true
		return f, nil
	}
	if len(eff.Terms) == 0 {
		return nil, errors.New("effective policy has no terms")
	}
	for _, t := range eff.Terms {
		if t.Tenant != "" {
			return nil, errors.New("tenant-bound terms are not supported on Tempo")
		}
		var conds []condition
		for _, m := range t.Matchers {
			c, err := compileCondition(m)
			if err != nil {
				return nil, err
			}
			conds = append(conds, c)
		}
		if len(conds) == 0 {
			f.all = true
			continue
		}
		f.terms = append(f.terms, conds)
	}
	return f, nil
}

func compileCondition(m policy.Matcher) (condition, error) {
	if m.Name == policy.TenantIDLabel {
		return condition{}, fmt.Errorf("%s matchers are not supported on Tempo", policy.TenantIDLabel)
	}
	scope, name := traceql.SplitAttr(m.Name)
	if name == "" || (scope != "" && scope != "span" && scope != "resource") || (scope == "" && m.Name[0] != '.') {
		return condition{}, fmt.Errorf("unsupported attribute name %q", m.Name)
	}
	c := condition{scope: scope, name: name, op: m.Op, value: m.Value}
	switch m.Op {
	case v1.MatchEqual, v1.MatchNotEqual:
	case v1.MatchRegexMatch, v1.MatchRegexNoMatch:
		re, err := regexp.Compile("^(?:" + m.Value + ")$")
		if err != nil {
			return condition{}, fmt.Errorf("matcher %q: %w", m.Name, err)
		}
		c.re = re
	default:
		return condition{}, fmt.Errorf("unsupported matcher operator %q", m.Op)
	}
	return c, nil
}

// Passthrough reports whether the filter leaves every response unchanged (unrestricted policy without
// denied attributes).
func (f *TraceFilter) Passthrough() bool { return f.all && len(f.denied) == 0 }

// attrVal is an attribute value as far as matching is concerned.
type attrVal struct {
	s        string
	isString bool
}

// attrSet maps attribute names to values; a name given twice is marked as non-string so that it never
// satisfies a matcher.
type attrSet map[string]attrVal

func (a attrSet) add(key string, v attrVal) {
	if _, dup := a[key]; dup {
		a[key] = attrVal{}
		return
	}
	a[key] = v
}

// pass reports whether the string value s passes the test of c.
func (c condition) pass(s string) bool {
	switch c.op {
	case v1.MatchEqual:
		return s == c.value
	case v1.MatchNotEqual:
		return s != c.value
	case v1.MatchRegexMatch:
		return c.re.MatchString(s)
	case v1.MatchRegexNoMatch:
		return !c.re.MatchString(s)
	}
	return false
}

func (c condition) negative() bool { return c.op == v1.MatchNotEqual || c.op == v1.MatchRegexNoMatch }

// satisfied implements the missing-attribute rule documented above.
func (c condition) satisfied(res, span attrSet) bool {
	var vals []attrVal
	if c.scope != "span" {
		if v, ok := res[c.name]; ok {
			vals = append(vals, v)
		}
	}
	if c.scope != "resource" {
		if v, ok := span[c.name]; ok {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return false
	}
	if c.negative() {
		for _, v := range vals {
			if !v.isString || !c.pass(v.s) {
				return false
			}
		}
		return true
	}
	for _, v := range vals {
		if v.isString && c.pass(v.s) {
			return true
		}
	}
	return false
}

// Match reports whether a span with the given resource and span attributes satisfies the policy.
func (f *TraceFilter) match(res, span attrSet) bool {
	if f.all {
		return true
	}
nextTerm:
	for _, t := range f.terms {
		for _, c := range t {
			if !c.satisfied(res, span) {
				continue nextTerm
			}
		}
		return true
	}
	return false
}

// isDenied reports whether an attribute of the given scope ("resource" or "span") is denied.
func (f *TraceFilter) isDenied(scope, name string) bool { return denied(f.denied, scope, name) }

// FilterProto filters a protobuf body: tempopb.Trace for v1, tempopb.TraceByIDResponse for V2. It returns
// the re-encoded body and whether any span remains. Undecodable input is an error.
//
// Only the ResourceSpans message tree (pkg/tempopb/trace/v1, common/v1, resource/v1) is decoded with the
// Tempo types. The two wrapper messages are handled at wire level (protowire) because importing the root
// package pkg/tempopb pulls in github.com/grafana/tempo/pkg/pool, which is AGPL-3.0 and fails the licence
// gate. Trace is {1: repeated ResourceSpans}; TraceByIDResponse is {1: Trace, 2: metrics, 3: status,
// 4: message} and everything but field 1 is copied verbatim. Unknown fields of Trace are dropped.
func (f *TraceFilter) FilterProto(body []byte) (out []byte, found bool, err error) {
	if !f.opt.V2 {
		return f.filterTraceMsg(body)
	}
	for b := body; len(b) > 0; {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, false, fmt.Errorf("decode protobuf trace: %w", protowire.ParseError(n))
		}
		m := protowire.ConsumeFieldValue(num, typ, b[n:])
		if m < 0 {
			return nil, false, fmt.Errorf("decode protobuf trace: %w", protowire.ParseError(m))
		}
		if num == 1 && typ == protowire.BytesType {
			inner, _ := protowire.ConsumeBytes(b[n:])
			filtered, ok, err := f.filterTraceMsg(inner)
			if err != nil {
				return nil, false, err
			}
			if ok {
				found = true
				out = protowire.AppendTag(out, 1, protowire.BytesType)
				out = protowire.AppendBytes(out, filtered)
			}
		} else {
			out = append(out, b[:n+m]...)
		}
		b = b[n+m:]
	}
	if !found {
		return nil, false, nil
	}
	return out, true, nil
}

// filterTraceMsg filters an encoded tempopb.Trace.
func (f *TraceFilter) filterTraceMsg(body []byte) ([]byte, bool, error) {
	var rss []*tracev1.ResourceSpans
	for b := body; len(b) > 0; {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, false, fmt.Errorf("decode protobuf trace: %w", protowire.ParseError(n))
		}
		m := protowire.ConsumeFieldValue(num, typ, b[n:])
		if m < 0 {
			return nil, false, fmt.Errorf("decode protobuf trace: %w", protowire.ParseError(m))
		}
		if num == 1 && typ == protowire.BytesType {
			raw, _ := protowire.ConsumeBytes(b[n:])
			rs := &tracev1.ResourceSpans{}
			if err := rs.Unmarshal(raw); err != nil {
				return nil, false, fmt.Errorf("decode protobuf trace: %w", err)
			}
			rss = append(rss, rs)
		}
		b = b[n+m:]
	}
	kept := f.filterResourceSpansProto(rss)
	if len(kept) == 0 {
		return nil, false, nil
	}
	var out []byte
	for _, rs := range kept {
		enc, err := rs.Marshal()
		if err != nil {
			return nil, false, fmt.Errorf("encode protobuf trace: %w", err)
		}
		out = protowire.AppendTag(out, 1, protowire.BytesType)
		out = protowire.AppendBytes(out, enc)
	}
	return out, true, nil
}

// filterResourceSpansProto prunes rss and returns the resource spans that keep at least one span.
func (f *TraceFilter) filterResourceSpansProto(in []*tracev1.ResourceSpans) []*tracev1.ResourceSpans {
	var rss []*tracev1.ResourceSpans
	for _, rs := range in {
		if rs == nil {
			continue
		}
		var res attrSet
		if rs.Resource != nil {
			res = protoAttrs(rs.Resource.Attributes)
		}
		var scopes []*tracev1.ScopeSpans
		for _, ss := range rs.ScopeSpans {
			if ss == nil {
				continue
			}
			var spans []*tracev1.Span
			for _, sp := range ss.Spans {
				if sp != nil && f.match(res, protoAttrs(sp.Attributes)) {
					f.stripSpan(sp)
					spans = append(spans, sp)
				}
			}
			if len(spans) == 0 {
				continue
			}
			ss.Spans = spans
			if ss.Scope != nil {
				ss.Scope.Attributes = f.stripKVs("span", ss.Scope.Attributes)
			}
			scopes = append(scopes, ss)
		}
		if len(scopes) == 0 {
			continue
		}
		rs.ScopeSpans = scopes
		if rs.Resource != nil {
			rs.Resource.Attributes = f.stripKVs("resource", rs.Resource.Attributes)
		}
		rss = append(rss, rs)
	}
	return rss
}

func (f *TraceFilter) stripSpan(sp *tracev1.Span) {
	sp.Attributes = f.stripKVs("span", sp.Attributes)
	for _, e := range sp.Events {
		if e != nil {
			e.Attributes = f.stripKVs("span", e.Attributes)
		}
	}
	for _, l := range sp.Links {
		if l != nil {
			l.Attributes = f.stripKVs("span", l.Attributes)
		}
	}
}

func (f *TraceFilter) stripKVs(scope string, kvs []*commonv1.KeyValue) []*commonv1.KeyValue {
	if len(f.denied) == 0 {
		return kvs
	}
	out := kvs[:0:0]
	for _, kv := range kvs {
		if kv != nil && !f.isDenied(scope, kv.Key) {
			out = append(out, kv)
		}
	}
	return out
}

func protoAttrs(kvs []*commonv1.KeyValue) attrSet {
	a := make(attrSet, len(kvs))
	for _, kv := range kvs {
		if kv == nil {
			continue
		}
		v := attrVal{}
		if kv.Value != nil {
			if sv, ok := kv.Value.Value.(*commonv1.AnyValue_StringValue); ok {
				v = attrVal{s: sv.StringValue, isString: true}
			}
		}
		a.add(kv.Key, v)
	}
	return a
}

// ModifyResponse is an httputil.ReverseProxy ModifyResponse hook: it filters a successful (2xx)
// trace-by-ID response as described on TraceFilter. Other statuses (404, errors) pass through unchanged.
// The upstream Content-Type decides the format (application/json or application/protobuf; anything else
// is an error); a gzip Content-Encoding is decoded and removed. A response without remaining spans becomes
// 404 with an empty body.
func (f *TraceFilter) ModifyResponse(resp *http.Response) error {
	if f.Passthrough() || resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil
	}
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return fmt.Errorf("trace response: bad content type: %w", err)
	}
	var filter func([]byte) ([]byte, bool, error)
	switch mt {
	case "application/protobuf", "application/x-protobuf":
		filter = f.FilterProto
	case "application/json":
		filter = f.FilterJSON
	default:
		return fmt.Errorf("trace response: unsupported content type %q", mt)
	}
	var src io.Reader = resp.Body
	switch resp.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return fmt.Errorf("trace response: %w", err)
		}
		defer func() { _ = zr.Close() }()
		src = zr
	default:
		return fmt.Errorf("trace response: unsupported content encoding %q", resp.Header.Get("Content-Encoding"))
	}
	body, err := io.ReadAll(io.LimitReader(src, f.opt.MaxBytes+1))
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("trace response: %w", err)
	}
	if int64(len(body)) > f.opt.MaxBytes {
		return errors.New("trace response: too large")
	}
	out, found, err := filter(body)
	if err != nil {
		return err
	}
	resp.Header.Del("Content-Encoding")
	if !found {
		resp.StatusCode = http.StatusNotFound
		resp.Status = strconv.Itoa(http.StatusNotFound) + " " + http.StatusText(http.StatusNotFound)
		resp.Header.Del("Content-Type")
		out = nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}
