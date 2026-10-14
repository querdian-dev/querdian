//go:build e2e

package prometheus

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/querdian-dev/querdian/test/e2e/framework"
)

// apiResp is the Prometheus HTTP API envelope.
type apiResp struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
	TraceID   *string         `json:"traceId"`
}

// fromProxy reports whether an error body was written by the proxy: it always has a traceId member
// (empty here because the test proxy runs without a tracer provider); Prometheus errors never do.
func (a apiResp) fromProxy() bool { return a.TraceID != nil }

func decode(t testing.TB, r *framework.Response) apiResp {
	t.Helper()
	var a apiResp
	if err := json.Unmarshal(r.Body, &a); err != nil {
		t.Fatalf("status %d, body is not JSON: %q", r.Status, r.Body)
	}
	return a
}

// result is a normalised query result: label set -> list of "timestamp=value" points. For a scalar the
// key is "". With valuesOnly the label sets are replaced by the rank of the series' value list, which
// compares results whose labels legitimately differ (absent()).
type result map[string][]string

func labelKey(m map[string]string) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		fmt.Fprintf(&b, "%s=%q,", k, m[k])
	}
	return b.String()
}

// normalise turns a successful query/query_range response into a result.
func normalise(t testing.TB, r *framework.Response, valuesOnly bool) result {
	t.Helper()
	a := decode(t, r)
	if r.Status != 200 || a.Status != "success" {
		t.Fatalf("not a success: %d %s", r.Status, r.Body)
	}
	var d struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(a.Data, &d); err != nil {
		t.Fatal(err)
	}
	out := result{}
	switch d.ResultType {
	case "scalar", "string":
		var p []any
		if err := json.Unmarshal(d.Result, &p); err != nil {
			t.Fatal(err)
		}
		out[""] = []string{fmt.Sprintf("%v=%v", p[0], p[1])}
	case "vector":
		var items []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		}
		if err := json.Unmarshal(d.Result, &items); err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			k := labelKey(it.Metric)
			if _, dup := out[k]; dup {
				t.Fatalf("duplicate label set %s in result", k)
			}
			out[k] = []string{fmt.Sprintf("%v=%v", it.Value[0], it.Value[1])}
		}
	case "matrix":
		var items []struct {
			Metric map[string]string `json:"metric"`
			Values [][]any           `json:"values"`
		}
		if err := json.Unmarshal(d.Result, &items); err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			k := labelKey(it.Metric)
			if _, dup := out[k]; dup {
				t.Fatalf("duplicate label set %s in result", k)
			}
			for _, v := range it.Values {
				out[k] = append(out[k], fmt.Sprintf("%v=%v", v[0], v[1]))
			}
		}
	default:
		t.Fatalf("unexpected result type %q", d.ResultType)
	}
	if valuesOnly {
		lists := make([]string, 0, len(out))
		for _, v := range out {
			lists = append(lists, strings.Join(v, ";"))
		}
		sort.Strings(lists)
		out = result{}
		for i, l := range lists {
			out[strconv.Itoa(i)] = strings.Split(l, ";")
		}
	}
	return out
}

// close compares two "ts=value" points: timestamps exactly, values with a relative tolerance of 1e-9
// (aggregations may add in a different order).
func closePoint(a, b string) bool {
	if a == b {
		return true
	}
	as, bs := strings.SplitN(a, "=", 2), strings.SplitN(b, "=", 2)
	if as[0] != bs[0] {
		return false
	}
	x, err1 := strconv.ParseFloat(as[1], 64)
	y, err2 := strconv.ParseFloat(bs[1], 64)
	if err1 != nil || err2 != nil {
		return false
	}
	if math.IsNaN(x) && math.IsNaN(y) {
		return true
	}
	return math.Abs(x-y) <= 1e-9*math.Max(1, math.Max(math.Abs(x), math.Abs(y)))
}

// diff returns "" when got equals want, otherwise a human-readable description of the first differences.
func diff(want, got result) string {
	var msgs []string
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		w, wok := want[k]
		g, gok := got[k]
		switch {
		case !wok:
			msgs = append(msgs, fmt.Sprintf("unexpected series {%s}: %v", k, head(g)))
		case !gok:
			msgs = append(msgs, fmt.Sprintf("missing series {%s}: %v", k, head(w)))
		case len(w) != len(g):
			msgs = append(msgs, fmt.Sprintf("series {%s}: %d points, want %d", k, len(g), len(w)))
		default:
			for i := range w {
				if !closePoint(w[i], g[i]) {
					msgs = append(msgs, fmt.Sprintf("series {%s} point %d: got %s want %s", k, i, g[i], w[i]))
					break
				}
			}
		}
		if len(msgs) >= 5 {
			break
		}
	}
	return strings.Join(msgs, "\n  ")
}

func head(v []string) []string {
	if len(v) > 3 {
		return v[:3]
	}
	return v
}

// union merges per-term results the way PromQL's `or` defines it for series with equal label sets: a
// series present in several terms must carry the same points (it is the same series); the first wins.
// A conflict is reported as an error string.
func union(parts []result) (result, string) {
	out := result{}
	for _, p := range parts {
		for k, v := range p {
			if prev, ok := out[k]; ok {
				if d := diff(result{k: prev}, result{k: v}); d != "" {
					return nil, "terms disagree on series {" + k + "}: " + d
				}
				continue
			}
			out[k] = v
		}
	}
	return out, ""
}
