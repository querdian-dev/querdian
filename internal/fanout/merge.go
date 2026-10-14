package fanout

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Merge functions in this package all have the shape of proxy.Merger,
//
//	func(w http.ResponseWriter, r *http.Request, resps []Response) error
//
// (internal/proxy imports this package, so the type cannot be named here). They share these rules:
//
//   - They write the client response themselves and return an error only before writing anything.
//     Malformed upstream JSON, an unexpected shape or an unsuccessful "status" is an error, never partial data.
//   - Responses are decoded strictly: trailing data after the JSON document is malformed.
//   - r is the client request. Merge parameters (limit, direction) are read from r.Form when the server has
//     set it to the normalised parameters, otherwise from the URL query of a GET request; see requestParams.
//   - The merge kind, the input and output sizes and, for sums, the approximate flag are recorded as attributes
//     on the span in the context of r (see annotate).

// errMalformed is wrapped by every error caused by a response that cannot be merged.
var errMalformed = errors.New("fanout: malformed upstream response")

func malformed(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errMalformed, fmt.Sprintf(format, a...))
}

// decodeObject decodes body as exactly one JSON object, keeping numbers verbatim.
func decodeObject(body []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&m); err != nil {
		return nil, malformed("%v", err)
	}
	if m == nil {
		return nil, malformed("not a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, malformed("trailing data after JSON document")
	}
	return m, nil
}

// decodeValue decodes raw into v, rejecting trailing data.
func decodeValue(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return malformed("%v", err)
	}
	return nil
}

// checkStatus requires "status":"success" when the object has a status field. Loki only returns success
// documents with a 2xx code, so anything else is malformed.
func checkStatus(top map[string]json.RawMessage) error {
	raw, ok := top["status"]
	if !ok {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s != "success" {
		return malformed("status is not \"success\"")
	}
	return nil
}

// writeJSON marshals v and writes it as a 200 response. Nothing is written when marshalling fails.
func writeJSON(w http.ResponseWriter, v any) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("fanout: encode merged response: %w", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
	return nil
}

// requestParams returns the normalised request parameters visible to a merger: r.Form when it is non-nil
// (the server stores the normalised parameters there, which is the only way a merger can see the parameters of a
// POST because the proxy has consumed the body), otherwise the URL query of a GET request. For any other case
// ok is false and the merger must fail rather than guess.
func requestParams(r *http.Request) (url.Values, bool) {
	if r.Form != nil {
		return r.Form, true
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		q, err := url.ParseQuery(r.URL.RawQuery)
		return q, err == nil
	}
	return nil, false
}

// annotate records merge attributes on the span of r's context (a no-op without a recording span).
func annotate(r *http.Request, kind string, itemsIn, itemsOut int, extra ...attribute.KeyValue) {
	span := trace.SpanFromContext(r.Context())
	if !span.IsRecording() {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("merge.kind", kind),
		attribute.Int("merge.items_in", itemsIn),
		attribute.Int("merge.items_out", itemsOut),
	}
	span.SetAttributes(append(attrs, extra...)...)
}

// sumNumbers adds b into a. Both are decoded JSON values (json.Number, bool, string, map, slice). Numbers are
// added (integers stay integers), booleans are OR-ed, objects are merged key by key, any other value keeps
// the first non-nil one. Arrays are not merged element-wise; the first one wins.
func sumNumbers(a, b any) any {
	switch av := a.(type) {
	case nil:
		return b
	case json.Number:
		if bv, ok := b.(json.Number); ok {
			return addNumbers(av, bv)
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return av || bv
		}
	case map[string]any:
		if bv, ok := b.(map[string]any); ok {
			for k, v := range bv {
				av[k] = sumNumbers(av[k], v)
			}
		}
	}
	return a
}

func addNumbers(a, b json.Number) json.Number {
	if x, err := strconv.ParseInt(string(a), 10, 64); err == nil {
		if y, err := strconv.ParseInt(string(b), 10, 64); err == nil {
			return json.Number(strconv.FormatInt(x+y, 10))
		}
	}
	x, err1 := a.Float64()
	y, err2 := b.Float64()
	if err1 != nil || err2 != nil {
		return a
	}
	return json.Number(strconv.FormatFloat(x+y, 'g', -1, 64))
}

// mergeStats sums the "stats" objects of the branches (nil entries are skipped) and returns nil when there is
// none. Counters and durations are summed; the *PerSecond rates of the summary are recomputed from the summed
// totals and execTime when possible. Under overlapping terms the sums count shared work twice (approximate).
func mergeStats(raws []json.RawMessage) (any, error) {
	var sum any
	for _, raw := range raws {
		if raw == nil {
			continue
		}
		var v any
		if err := decodeValue(raw, &v); err != nil {
			return nil, err
		}
		if _, ok := v.(map[string]any); !ok {
			return nil, malformed("stats is not an object")
		}
		sum = sumNumbers(sum, v)
	}
	if m, ok := sum.(map[string]any); ok {
		if s, ok := m["summary"].(map[string]any); ok {
			fixRates(s)
		}
	}
	return sum, nil
}

func fixRates(s map[string]any) {
	exec, ok := s["execTime"].(json.Number)
	if !ok {
		return
	}
	secs, err := exec.Float64()
	if err != nil || secs <= 0 {
		return
	}
	for rate, total := range map[string]string{
		"bytesProcessedPerSecond": "totalBytesProcessed",
		"linesProcessedPerSecond": "totalLinesProcessed",
	} {
		n, ok := s[total].(json.Number)
		if !ok {
			continue
		}
		f, err := n.Float64()
		if err != nil {
			continue
		}
		s[rate] = json.Number(strconv.FormatInt(int64(f/secs), 10))
	}
}

// uniqueStrings returns the values of all inputs in order of first appearance without duplicates.
func uniqueStrings(lists ...[]string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, l := range lists {
		for _, s := range l {
			if _, ok := seen[s]; !ok {
				seen[s] = struct{}{}
				out = append(out, s)
			}
		}
	}
	return out
}

// labelSetKey returns a canonical, collision-free key for a label set.
func labelSetKey(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(strconv.Quote(k))
		sb.WriteByte('=')
		sb.WriteString(strconv.Quote(m[k]))
		sb.WriteByte(',')
	}
	return sb.String()
}
