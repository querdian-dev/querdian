package fanout

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// DeniedFunc returns the label names denied by policy for the request being merged (Effective.DeniedLabels).
// Mergers are registered once per server, but the denied list is per user, so the server supplies a function
// that reads it from the request context. It may return nil.
type DeniedFunc func(r *http.Request) []string

// NewStringSetMerger returns the merger for MergeStringSet plans (Loki labels, label values, detected_labels,
// detected_fields and detected_field values). The returned function has the signature of proxy.Merger.
//
// It unions the branches, sorts and deduplicates, and removes denied labels. Which response shape applies is
// detected from the JSON (all non-empty branches must agree):
//
//   - {"status":"success","data":["a","b"]} for labels and label values. For label values (request path ending in
//     "/values") the entries are values, not label names, and are not filtered.
//   - {"detectedLabels":[{"label":"app","cardinality":2}]}: merged by label. Cardinalities are summed, which is
//     exact for disjoint policy terms and an upper bound otherwise (documented approximation).
//   - {"fields":[{"label","type","cardinality","parsers"}],"limit":1000}: merged by label like detectedLabels;
//     conflicting types become "string", parsers are unioned, the list is cut to limit after sorting.
//   - {"values":[...],"limit":1000} for detected_field values (not filtered, cut to limit).
//   - {} (Loki's answer for "nothing found") counts as an empty branch; if all branches are empty the result is
//     {} for the detected_* shapes. An empty labels result is rendered with "data":[].
//
// Entries named in denied are dropped from labels, detectedLabels and fields. A nil denied function denies
// nothing. Malformed or disagreeing upstream responses are an error; nothing is written then.
func NewStringSetMerger(denied DeniedFunc) func(w http.ResponseWriter, r *http.Request, resps []Response) error {
	return func(w http.ResponseWriter, r *http.Request, resps []Response) error {
		var deny map[string]struct{}
		if denied != nil {
			deny = make(map[string]struct{})
			for _, d := range denied(r) {
				deny[d] = struct{}{}
			}
		}
		return mergeStringSet(w, r, resps, deny)
	}
}

type setShape int

const (
	shapeUnknown setShape = iota
	shapeList
	shapeDetectedLabels
	shapeFields
	shapeFieldValues
)

type fieldAcc struct {
	label       string
	cardinality json.Number
	typ         string
	parsers     []string
	hasParsers  bool
	extra       map[string]any
}

func mergeStringSet(w http.ResponseWriter, r *http.Request, resps []Response, deny map[string]struct{}) error {
	if len(resps) == 0 {
		return fmt.Errorf("fanout: nothing to merge")
	}
	isValues := strings.HasSuffix(r.URL.Path, "/values")
	var (
		shape  setShape
		list   []string
		fields = map[string]*fieldAcc{}
		limit  int64
		in     int
	)
	for i, resp := range resps {
		top, err := decodeObject(resp.Body)
		if err != nil {
			return fmt.Errorf("branch %d: %w", i, err)
		}
		if err := checkStatus(top); err != nil {
			return fmt.Errorf("branch %d: %w", i, err)
		}
		var bs setShape
		switch {
		case has(top, "detectedLabels"):
			bs = shapeDetectedLabels
		case has(top, "fields"):
			bs = shapeFields
		case has(top, "values") && !has(top, "status"):
			bs = shapeFieldValues
		case has(top, "status"):
			bs = shapeList
		case len(top) == 0:
			continue
		default:
			return fmt.Errorf("branch %d: %w", i, malformed("unrecognised response shape"))
		}
		if shape != shapeUnknown && shape != bs {
			return fmt.Errorf("branch %d: %w", i, malformed("response shapes differ between branches"))
		}
		shape = bs

		if raw, ok := top["limit"]; ok {
			var l int64
			if err := json.Unmarshal(raw, &l); err != nil {
				return fmt.Errorf("branch %d: %w", i, malformed("limit: %v", err))
			}
			limit = max(limit, l)
		}
		switch bs {
		case shapeList:
			var vs []string
			if raw, ok := top["data"]; ok {
				if err := json.Unmarshal(raw, &vs); err != nil {
					return fmt.Errorf("branch %d: %w", i, malformed("data: %v", err))
				}
			}
			in += len(vs)
			list = append(list, vs...)
		case shapeFieldValues:
			var vs []string
			if err := json.Unmarshal(top["values"], &vs); err != nil {
				return fmt.Errorf("branch %d: %w", i, malformed("values: %v", err))
			}
			in += len(vs)
			list = append(list, vs...)
		case shapeDetectedLabels, shapeFields:
			key := "detectedLabels"
			if bs == shapeFields {
				key = "fields"
			}
			var items []map[string]any
			if err := decodeValue(top[key], &items); err != nil {
				return fmt.Errorf("branch %d: %w", i, malformed("%s: %v", key, err))
			}
			in += len(items)
			for _, it := range items {
				if err := addField(fields, it); err != nil {
					return fmt.Errorf("branch %d: %w", i, err)
				}
			}
		}
	}

	var (
		out any
		n   int
	)
	switch shape {
	case shapeUnknown:
		out = map[string]any{}
	case shapeList:
		list = sortedUnique(list)
		if !isValues {
			list = dropDenied(list, deny)
		}
		n = len(list)
		out = map[string]any{"status": "success", "data": nonNil(list)}
	case shapeFieldValues:
		list = sortedUnique(list)
		if limit > 0 && int64(len(list)) > limit {
			list = list[:limit]
		}
		n = len(list)
		if n == 0 {
			out = map[string]any{}
		} else {
			out = map[string]any{"values": list, "limit": limit}
		}
	case shapeDetectedLabels, shapeFields:
		items := fieldList(fields, deny, shape == shapeFields)
		if shape == shapeFields && limit > 0 && int64(len(items)) > limit {
			items = items[:limit]
		}
		n = len(items)
		switch {
		case n == 0:
			out = map[string]any{}
		case shape == shapeFields:
			out = map[string]any{"fields": items, "limit": limit}
		default:
			out = map[string]any{"detectedLabels": items}
		}
	}
	if err := writeJSON(w, out); err != nil {
		return err
	}
	annotate(r, "string_set", in, n)
	return nil
}

func has(m map[string]json.RawMessage, k string) bool { _, ok := m[k]; return ok }

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func sortedUnique(s []string) []string {
	slices.Sort(s)
	return slices.Compact(s)
}

func dropDenied(s []string, deny map[string]struct{}) []string {
	if len(deny) == 0 {
		return s
	}
	return slices.DeleteFunc(s, func(v string) bool { _, d := deny[v]; return d })
}

// addField merges one detectedLabels/fields item into acc, keyed by its "label".
func addField(acc map[string]*fieldAcc, it map[string]any) error {
	label, ok := it["label"].(string)
	if !ok {
		return malformed("item without string label")
	}
	card, _ := it["cardinality"].(json.Number)
	if _, err := card.Int64(); card != "" && err != nil {
		return malformed("cardinality of %q is not an integer", label)
	}
	typ, _ := it["type"].(string)
	var parsers []string
	hasParsers := false
	switch p := it["parsers"].(type) {
	case nil:
	case []any:
		hasParsers = true
		for _, v := range p {
			s, ok := v.(string)
			if !ok {
				return malformed("parsers of %q contain a non-string", label)
			}
			parsers = append(parsers, s)
		}
	default:
		return malformed("parsers of %q is not an array", label)
	}
	f, ok := acc[label]
	if !ok {
		extra := map[string]any{}
		for k, v := range it {
			switch k {
			case "label", "cardinality", "type", "parsers":
			default:
				extra[k] = v
			}
		}
		acc[label] = &fieldAcc{label: label, cardinality: card, typ: typ, parsers: parsers, hasParsers: hasParsers, extra: extra}
		return nil
	}
	if card != "" {
		if f.cardinality == "" {
			f.cardinality = card
		} else {
			f.cardinality = addNumbers(f.cardinality, card)
		}
	}
	if f.typ != typ {
		f.typ = "string"
	}
	f.hasParsers = f.hasParsers || hasParsers
	f.parsers = append(f.parsers, parsers...)
	return nil
}

// fieldList renders the accumulated items sorted by label, without denied labels. withType selects the
// detected_fields item layout.
func fieldList(acc map[string]*fieldAcc, deny map[string]struct{}, withType bool) []map[string]any {
	labels := make([]string, 0, len(acc))
	for l := range acc {
		if _, d := deny[l]; !d {
			labels = append(labels, l)
		}
	}
	slices.Sort(labels)
	out := make([]map[string]any, 0, len(labels))
	for _, l := range labels {
		f := acc[l]
		it := map[string]any{"label": f.label}
		for k, v := range f.extra {
			it[k] = v
		}
		if f.cardinality != "" {
			it["cardinality"] = f.cardinality
		}
		if withType {
			it["type"] = f.typ
			if f.hasParsers {
				it["parsers"] = sortedUnique(f.parsers)
			} else {
				it["parsers"] = nil
			}
		}
		out = append(out, it)
	}
	return out
}
