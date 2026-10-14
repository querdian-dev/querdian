package fanout

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
)

// ApproximateHeader is set to "true" on responses of a MergeSumCounts merge that are only an estimate (the
// policy terms are not provably disjoint). None of the summed Loki APIs (index/stats, index/volume,
// index/volume_range, patterns) has a documented warnings field, so the marker is an HTTP header rather than
// a body field; the body keeps the exact upstream format.
const ApproximateHeader = "X-Querdian-Approximate"

// DefaultVolumeLimit is the number of series index/volume returns when the request has no limit.
const DefaultVolumeLimit = 100

// NewSumCountsMerger returns the merger for MergeSumCounts plans (Loki index/stats, index/volume,
// index/volume_range and patterns). The returned function has the signature of proxy.Merger.
//
// Summation per response type (the type is detected from the JSON):
//
//   - index/stats, a flat object of counters {"streams","chunks","bytes","entries"}: every counter is summed.
//   - index/volume (resultType vector): series with the same metric label set are summed. Denied labels are
//     removed from the metric label sets first (index/volume reports whole series label sets), which can
//     merge series. The series are ordered by value, largest first, and cut to the request's limit
//     (default [DefaultVolumeLimit]).
//   - index/volume_range (resultType matrix): series are matched like above and summed per timestamp; the
//     series are ordered by total, largest first, and cut to the limit.
//   - patterns, {"status":"success","data":[{"pattern","samples":[[ts,count]]}]}: counts of equal patterns are
//     summed per timestamp; patterns are ordered by total, largest first.
//   - "stats" of the volume envelope are summed (see mergeStats); "warnings" are kept.
//
// The sums are exact when the policy terms are disjoint, because every upstream record is then counted by
// exactly one branch. Otherwise a record matched by several terms is counted once per term and the result
// is an estimate: approximate(r) reports true, then the response header [ApproximateHeader] is set to "true" and
// the span of r's context gets merge.approximate=true. A nil approximate function means "approximate"
// (the safe answer). Truncation to limit after summation is only exact when no branch cut series off itself.
//
// denied returns the denied labels of the request and may be nil. Malformed or disagreeing upstream
// responses are an error; nothing is written then.
func NewSumCountsMerger(denied DeniedFunc, approximate func(r *http.Request) bool) func(w http.ResponseWriter, r *http.Request, resps []Response) error {
	return func(w http.ResponseWriter, r *http.Request, resps []Response) error {
		deny := map[string]struct{}{}
		if denied != nil {
			for _, d := range denied(r) {
				deny[d] = struct{}{}
			}
		}
		limit := DefaultVolumeLimit
		if params, ok := requestParams(r); ok {
			if v := params.Get("limit"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 {
					return fmt.Errorf("fanout: invalid limit %q", v)
				}
				limit = n
			}
		}
		out, in, n, err := sumCounts(resps, deny, limit)
		if err != nil {
			return err
		}
		approx := approximate == nil || approximate(r)
		if approx {
			w.Header().Set(ApproximateHeader, "true")
		}
		if err := writeJSON(w, out); err != nil {
			return err
		}
		annotate(r, "sum_counts", in, n, attribute.Bool("merge.approximate", approx))
		return nil
	}
}

type sumShape int

const (
	sumUnknown sumShape = iota
	sumStats
	sumVector
	sumMatrix
	sumPatterns
)

func sumCounts(resps []Response, deny map[string]struct{}, limit int) (out any, in, n int, err error) {
	if len(resps) == 0 {
		return nil, 0, 0, fmt.Errorf("fanout: nothing to merge")
	}
	var (
		shape    sumShape
		first    map[string]json.RawMessage
		flat     any
		vec      = map[string]*vecSeries{}
		mat      = map[string]*matSeries{}
		pats     = map[string]*patSeries{}
		statsRaw []json.RawMessage
		warnings [][]string
		isEmpty  = true
	)
	for i, resp := range resps {
		top, derr := decodeObject(resp.Body)
		if derr != nil {
			return nil, 0, 0, fmt.Errorf("branch %d: %w", i, derr)
		}
		if derr := checkStatus(top); derr != nil {
			return nil, 0, 0, fmt.Errorf("branch %d: %w", i, derr)
		}
		bs, derr := sumShapeOf(top)
		if derr != nil {
			return nil, 0, 0, fmt.Errorf("branch %d: %w", i, derr)
		}
		if bs == sumUnknown {
			continue // {}
		}
		if shape != sumUnknown && shape != bs {
			return nil, 0, 0, fmt.Errorf("branch %d: %w", i, malformed("response types differ between branches"))
		}
		shape = bs
		if first == nil {
			first = top
		}
		isEmpty = false
		if raw, ok := top["warnings"]; ok {
			var ws []string
			if derr := json.Unmarshal(raw, &ws); derr != nil {
				return nil, 0, 0, fmt.Errorf("branch %d: %w", i, malformed("warnings: %v", derr))
			}
			warnings = append(warnings, ws)
		}
		switch bs {
		case sumStats:
			var v any
			if derr := decodeValue(resp.Body, &v); derr != nil {
				return nil, 0, 0, fmt.Errorf("branch %d: %w", i, derr)
			}
			m := v.(map[string]any)
			for k, val := range m {
				if _, ok := val.(json.Number); !ok {
					return nil, 0, 0, fmt.Errorf("branch %d: %w", i, malformed("counter %q is not a number", k))
				}
			}
			in++
			flat = sumNumbers(flat, v)
		case sumVector, sumMatrix:
			var data map[string]json.RawMessage
			_ = json.Unmarshal(top["data"], &data)
			statsRaw = append(statsRaw, data["stats"])
			var cnt int
			if bs == sumVector {
				cnt, derr = addVector(vec, data["result"], deny)
			} else {
				cnt, derr = addMatrix(mat, data["result"], deny)
			}
			if derr != nil {
				return nil, 0, 0, fmt.Errorf("branch %d: %w", i, derr)
			}
			in += cnt
		case sumPatterns:
			cnt, derr := addPatterns(pats, top["data"])
			if derr != nil {
				return nil, 0, 0, fmt.Errorf("branch %d: %w", i, derr)
			}
			in += cnt
		}
	}
	if isEmpty {
		return map[string]any{}, 0, 0, nil
	}

	switch shape {
	case sumStats:
		return flat, in, len(flat.(map[string]any)), nil
	case sumPatterns:
		res := patternResult(pats)
		return withWarnings(map[string]any{"status": "success", "data": res}, first, warnings, "data"), in, len(res), nil
	}
	stats, serr := mergeStats(statsRaw)
	if serr != nil {
		return nil, 0, 0, serr
	}
	var (
		result any
		rt     = "vector"
	)
	if shape == sumVector {
		res := vectorResult(vec, limit)
		result, n = res, len(res)
	} else {
		rt = "matrix"
		res := matrixResult(mat, limit)
		result, n = res, len(res)
	}
	data := map[string]any{"resultType": rt, "result": result}
	var firstData map[string]json.RawMessage
	_ = json.Unmarshal(first["data"], &firstData)
	for k, v := range firstData {
		if _, set := data[k]; !set && k != "stats" {
			data[k] = v
		}
	}
	if stats != nil {
		data["stats"] = stats
	}
	return withWarnings(map[string]any{"status": "success", "data": data}, first, warnings, "data"), in, n, nil
}

// withWarnings copies the first branch's remaining top-level fields into doc and sets the merged warnings.
func withWarnings(doc map[string]any, first map[string]json.RawMessage, warnings [][]string, skip string) map[string]any {
	for k, v := range first {
		if _, set := doc[k]; !set && k != skip && k != "warnings" {
			doc[k] = v
		}
	}
	if ws := uniqueStrings(warnings...); len(ws) > 0 {
		doc["warnings"] = ws
	}
	return doc
}

func sumShapeOf(top map[string]json.RawMessage) (sumShape, error) {
	if len(top) == 0 {
		return sumUnknown, nil
	}
	raw, hasData := top["data"]
	if _, hasStatus := top["status"]; !hasStatus {
		if hasData {
			return 0, malformed("unrecognised response shape")
		}
		return sumStats, nil
	}
	if !hasData {
		return 0, malformed("missing data")
	}
	var rawStr = string(raw)
	if len(rawStr) > 0 && rawStr[0] == '[' {
		return sumPatterns, nil
	}
	var data struct {
		ResultType string `json:"resultType"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return 0, malformed("data: %v", err)
	}
	switch data.ResultType {
	case "vector":
		return sumVector, nil
	case "matrix":
		return sumMatrix, nil
	}
	return 0, malformed("unsupported resultType %q", data.ResultType)
}

// numeric validates a sample value string and returns it as json.Number.
func numeric(s string) (json.Number, error) {
	if _, err := strconv.ParseFloat(s, 64); err != nil {
		return "", malformed("sample value %q is not a number", s)
	}
	return json.Number(s), nil
}

// stripDenied returns metric without denied labels.
func stripDenied(metric map[string]string, deny map[string]struct{}) map[string]string {
	if len(deny) == 0 {
		return metric
	}
	out := make(map[string]string, len(metric))
	for k, v := range metric {
		if _, d := deny[k]; !d {
			out[k] = v
		}
	}
	return out
}

type vecSeries struct {
	metric map[string]string
	ts     json.Number
	sum    json.Number
}

func addVector(acc map[string]*vecSeries, raw json.RawMessage, deny map[string]struct{}) (int, error) {
	var res []struct {
		Metric map[string]string `json:"metric"`
		Value  []json.RawMessage `json:"value"`
	}
	if err := decodeValue(raw, &res); err != nil {
		return 0, malformed("result: %v", err)
	}
	for _, s := range res {
		if s.Metric == nil || len(s.Value) != 2 {
			return 0, malformed("malformed vector sample")
		}
		var ts json.Number
		var v string
		if err := decodeValue(s.Value[0], &ts); err != nil {
			return 0, malformed("sample timestamp")
		}
		if err := json.Unmarshal(s.Value[1], &v); err != nil {
			return 0, malformed("sample value is not a string")
		}
		val, err := numeric(v)
		if err != nil {
			return 0, err
		}
		metric := stripDenied(s.Metric, deny)
		key := labelSetKey(metric)
		if cur, ok := acc[key]; ok {
			cur.sum = addNumbers(cur.sum, val)
			if num(ts) > num(cur.ts) {
				cur.ts = ts
			}
		} else {
			acc[key] = &vecSeries{metric: metric, ts: ts, sum: val}
		}
	}
	return len(res), nil
}

func num(n json.Number) float64 { f, _ := n.Float64(); return f }

func vectorResult(acc map[string]*vecSeries, limit int) []map[string]any {
	keys := make([]string, 0, len(acc))
	for k := range acc {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if c := cmp.Compare(num(acc[b].sum), num(acc[a].sum)); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	if len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		s := acc[k]
		out = append(out, map[string]any{"metric": s.metric, "value": []any{s.ts, string(s.sum)}})
	}
	return out
}

type matSeries struct {
	metric  map[string]string
	samples map[string]json.Number // timestamp (verbatim) -> sum
	total   float64
}

func addMatrix(acc map[string]*matSeries, raw json.RawMessage, deny map[string]struct{}) (int, error) {
	var res []struct {
		Metric map[string]string   `json:"metric"`
		Values [][]json.RawMessage `json:"values"`
	}
	if err := decodeValue(raw, &res); err != nil {
		return 0, malformed("result: %v", err)
	}
	for _, s := range res {
		if s.Metric == nil {
			return 0, malformed("matrix series without metric")
		}
		metric := stripDenied(s.Metric, deny)
		key := labelSetKey(metric)
		cur, ok := acc[key]
		if !ok {
			cur = &matSeries{metric: metric, samples: map[string]json.Number{}}
			acc[key] = cur
		}
		for _, v := range s.Values {
			if len(v) != 2 {
				return 0, malformed("malformed matrix sample")
			}
			var ts json.Number
			var str string
			if err := decodeValue(v[0], &ts); err != nil {
				return 0, malformed("sample timestamp")
			}
			if err := json.Unmarshal(v[1], &str); err != nil {
				return 0, malformed("sample value is not a string")
			}
			val, err := numeric(str)
			if err != nil {
				return 0, err
			}
			if old, ok := cur.samples[string(ts)]; ok {
				val = addNumbers(old, val)
			}
			cur.samples[string(ts)] = val
		}
	}
	return len(res), nil
}

func matrixResult(acc map[string]*matSeries, limit int) []map[string]any {
	keys := make([]string, 0, len(acc))
	for k, s := range acc {
		keys = append(keys, k)
		s.total = 0
		for _, v := range s.samples {
			s.total += num(v)
		}
	}
	slices.SortFunc(keys, func(a, b string) int {
		if c := cmp.Compare(acc[b].total, acc[a].total); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	if len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		s := acc[k]
		tss := make([]string, 0, len(s.samples))
		for ts := range s.samples {
			tss = append(tss, ts)
		}
		slices.SortFunc(tss, func(a, b string) int { return cmp.Compare(num(json.Number(a)), num(json.Number(b))) })
		vals := make([]any, len(tss))
		for i, ts := range tss {
			vals[i] = []any{json.Number(ts), string(s.samples[ts])}
		}
		out = append(out, map[string]any{"metric": s.metric, "values": vals})
	}
	return out
}

type patSeries struct {
	pattern string
	samples map[string]json.Number
	total   float64
}

func addPatterns(acc map[string]*patSeries, raw json.RawMessage) (int, error) {
	var res []struct {
		Pattern string              `json:"pattern"`
		Samples [][]json.RawMessage `json:"samples"`
	}
	if err := decodeValue(raw, &res); err != nil {
		return 0, malformed("patterns: %v", err)
	}
	for _, p := range res {
		cur, ok := acc[p.Pattern]
		if !ok {
			cur = &patSeries{pattern: p.Pattern, samples: map[string]json.Number{}}
			acc[p.Pattern] = cur
		}
		for _, s := range p.Samples {
			if len(s) != 2 {
				return 0, malformed("malformed pattern sample")
			}
			var ts, cnt json.Number
			if err := decodeValue(s[0], &ts); err != nil {
				return 0, malformed("pattern sample timestamp")
			}
			if err := decodeValue(s[1], &cnt); err != nil {
				return 0, malformed("pattern sample count")
			}
			if old, ok := cur.samples[string(ts)]; ok {
				cnt = addNumbers(old, cnt)
			}
			cur.samples[string(ts)] = cnt
		}
	}
	return len(res), nil
}

func patternResult(acc map[string]*patSeries) []map[string]any {
	keys := make([]string, 0, len(acc))
	for k, p := range acc {
		keys = append(keys, k)
		p.total = 0
		for _, v := range p.samples {
			p.total += num(v)
		}
	}
	slices.SortFunc(keys, func(a, b string) int {
		if c := cmp.Compare(acc[b].total, acc[a].total); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		p := acc[k]
		tss := make([]string, 0, len(p.samples))
		for ts := range p.samples {
			tss = append(tss, ts)
		}
		slices.SortFunc(tss, func(a, b string) int { return cmp.Compare(num(json.Number(a)), num(json.Number(b))) })
		samples := make([]any, len(tss))
		for i, ts := range tss {
			samples[i] = []any{json.Number(ts), p.samples[ts]}
		}
		out = append(out, map[string]any{"pattern": p.pattern, "samples": samples})
	}
	return out
}
