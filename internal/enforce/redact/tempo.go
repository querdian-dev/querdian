package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Tempo rewrites a Tempo search or TraceQL metrics response. Every array named "attributes" or "labels"
// anywhere in the body is treated as an OTLP key/value list ({"key", "value": {"stringValue": ...}}) and
// its redacted attributes are rewritten; the scope of these attributes is not reported, so a redaction of
// either scope applies (see AttrRule). Walking the whole body rather than known paths keeps fields added
// by future Tempo versions covered. A non-string value of a redacted attribute is replaced by a string.
func (r *Redactor) Tempo(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: tempo body: %w", ErrResponse, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data after tempo body", ErrResponse)
	}
	defer r.Notice() // Tempo responses have no warnings member; drop the collected names
	var lists []*[]any
	if err := collectKVLists(doc, &lists); err != nil {
		return nil, err
	}
	// Pass 1: observe, so that placeholders are numbered in random order.
	for _, l := range lists {
		for _, it := range *l {
			kv, ok := it.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: attribute is not an object", ErrResponse)
			}
			key, _ := kv["key"].(string)
			if red, id, ok := r.AttrRule("", key); ok {
				v, err := identity(kv["value"])
				if err != nil {
					return nil, err
				}
				r.Observe(red, id, v)
			}
		}
	}
	r.Commit()
	for _, l := range lists {
		kept := (*l)[:0]
		for _, it := range *l {
			kv := it.(map[string]any)
			key, _ := kv["key"].(string)
			red, id, ok := r.AttrRule("", key)
			if !ok {
				kept = append(kept, kv)
				continue
			}
			v, _ := identity(kv["value"])
			if nv, keep := r.Value(red, id, v); keep {
				kv["value"] = map[string]any{"stringValue": nv}
				kept = append(kept, kv)
			}
		}
		*l = kept
	}
	return marshal(doc)
}

// collectKVLists finds the attribute lists in v. The pointers stay valid because the containing maps
// are updated through them after collection. An array named attributes or labels that is not a
// key/value list is an error: it might carry attribute values in a shape the rewriter does not know.
func collectKVLists(v any, out *[]*[]any) error {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if _, ok := child.(string); ok && k == "promLabels" {
				// TraceQL metrics series repeat their labels as a Prometheus-format string; it cannot be
				// rewritten reliably and the structured "labels" carry the same data.
				delete(t, k)
				continue
			}
			if k == "attributes" || k == "labels" {
				if child == nil {
					continue
				}
				arr, ok := child.([]any)
				if !ok || !isKVList(arr) {
					return fmt.Errorf("%w: %s is not a key/value list", ErrResponse, k)
				}
				p := new([]any)
				*p = arr
				*out = append(*out, p)
				t[k] = kvListRef{p}
				continue
			}
			if err := collectKVLists(child, out); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range t {
			if err := collectKVLists(child, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// kvListRef lets a collected list be replaced after rewriting; it marshals as the list.
type kvListRef struct{ p *[]any }

func (k kvListRef) MarshalJSON() ([]byte, error) { return marshal(*k.p) }

// isKVList reports whether every element is an object with a string "key".
func isKVList(arr []any) bool {
	for _, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			return false
		}
		if _, ok := m["key"].(string); !ok {
			return false
		}
	}
	return true
}

// identity is the string that represents an attribute value for numbering, hashing and IPv4Prefix:
// the string itself for stringValue, the canonical JSON of the value otherwise.
func identity(v any) (string, error) {
	if m, ok := v.(map[string]any); ok {
		if s, ok := m["stringValue"].(string); ok && len(m) == 1 {
			return s, nil
		}
	}
	b, err := marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
