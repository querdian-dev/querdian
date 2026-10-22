package traceql

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// jsonObj is a JSON object whose members are kept raw so that everything the filter does not touch is
// preserved verbatim (64-bit integers are strings in Tempo's JSON, but raw copying is safe either way).
type jsonObj = map[string]json.RawMessage

// FilterJSON filters a JSON trace body. Both shapes are recognised by their keys: api/traces answers
// {"batches":[ResourceSpans...]} (the OTLP name resourceSpans is accepted too) and api/v2/traces answers
// {"trace":{"resourceSpans":[...]},"metrics":...}. It returns the re-encoded body and whether any span
// remains. Input that is not one of these shapes, or that contains the pre-1.0 OTLP member
// instrumentationLibrarySpans (which the filter does not understand), is an error: it is never passed on.
func (f *TraceFilter) FilterJSON(body []byte) (out []byte, found bool, err error) {
	var top jsonObj
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, false, fmt.Errorf("decode json trace: %w", err)
	}
	holder := top
	if raw, ok := top["trace"]; ok {
		holder = nil
		if err := json.Unmarshal(raw, &holder); err != nil || holder == nil {
			return nil, false, errors.New("decode json trace: bad trace member")
		}
	}
	key := ""
	for _, k := range []string{"batches", "resourceSpans"} {
		if _, ok := holder[k]; ok {
			if key != "" {
				return nil, false, errors.New("decode json trace: both batches and resourceSpans")
			}
			key = k
		}
	}
	if key == "" {
		// Tempo's empty answer: {"trace":{}} (v2) or {} .
		return nil, false, nil
	}
	var rss []jsonObj
	if err := json.Unmarshal(holder[key], &rss); err != nil {
		return nil, false, fmt.Errorf("decode json trace: %w", err)
	}
	kept := make([]jsonObj, 0, len(rss))
	for _, rs := range rss {
		if _, ok := rs["instrumentationLibrarySpans"]; ok {
			return nil, false, errors.New("decode json trace: unsupported instrumentationLibrarySpans")
		}
		ok, err := f.filterResourceSpans(rs)
		if err != nil {
			return nil, false, err
		}
		if ok {
			kept = append(kept, rs)
		}
	}
	if len(kept) == 0 {
		return nil, false, nil
	}
	if holder[key], err = json.Marshal(kept); err != nil {
		return nil, false, err
	}
	if _, ok := top["trace"]; ok {
		if top["trace"], err = json.Marshal(holder); err != nil {
			return nil, false, err
		}
	}
	out, err = json.Marshal(top)
	return out, err == nil, err
}

// filterResourceSpans prunes one resource-spans object in place and reports whether a span remains.
func (f *TraceFilter) filterResourceSpans(rs jsonObj) (bool, error) {
	var resource jsonObj
	var res attrSet
	if raw, ok := rs["resource"]; ok {
		if err := json.Unmarshal(raw, &resource); err != nil {
			return false, fmt.Errorf("decode json trace: resource: %w", err)
		}
		a, err := jsonAttrSet(resource["attributes"])
		if err != nil {
			return false, err
		}
		res = a
	}
	var scopes []jsonObj
	if raw, ok := rs["scopeSpans"]; ok {
		if err := json.Unmarshal(raw, &scopes); err != nil {
			return false, fmt.Errorf("decode json trace: scopeSpans: %w", err)
		}
	}
	keptScopes := make([]jsonObj, 0, len(scopes))
	for _, ss := range scopes {
		var spans []jsonObj
		if raw, ok := ss["spans"]; ok {
			if err := json.Unmarshal(raw, &spans); err != nil {
				return false, fmt.Errorf("decode json trace: spans: %w", err)
			}
		}
		keptSpans := make([]jsonObj, 0, len(spans))
		for _, sp := range spans {
			sa, err := jsonAttrSet(sp["attributes"])
			if err != nil {
				return false, err
			}
			if !f.match(res, sa) {
				continue
			}
			if err := f.stripJSONSpan(sp); err != nil {
				return false, err
			}
			keptSpans = append(keptSpans, sp)
		}
		if len(keptSpans) == 0 {
			continue
		}
		b, err := json.Marshal(keptSpans)
		if err != nil {
			return false, err
		}
		ss["spans"] = b
		if raw, ok := ss["scope"]; ok {
			var scope jsonObj
			if err := json.Unmarshal(raw, &scope); err != nil {
				return false, fmt.Errorf("decode json trace: scope: %w", err)
			}
			if err := f.stripJSONAttrs(scope, "span"); err != nil {
				return false, err
			}
			if ss["scope"], err = json.Marshal(scope); err != nil {
				return false, err
			}
		}
		keptScopes = append(keptScopes, ss)
	}
	if len(keptScopes) == 0 {
		return false, nil
	}
	b, err := json.Marshal(keptScopes)
	if err != nil {
		return false, err
	}
	rs["scopeSpans"] = b
	if resource != nil {
		if err := f.stripJSONAttrs(resource, "resource"); err != nil {
			return false, err
		}
		if rs["resource"], err = json.Marshal(resource); err != nil {
			return false, err
		}
	}
	return true, nil
}

// stripJSONSpan removes denied attributes from the span, its events and its links.
func (f *TraceFilter) stripJSONSpan(sp jsonObj) error {
	if len(f.denied) == 0 {
		return nil
	}
	if err := f.stripJSONAttrs(sp, "span"); err != nil {
		return err
	}
	for _, k := range []string{"events", "links"} {
		raw, ok := sp[k]
		if !ok {
			continue
		}
		var items []jsonObj
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("decode json trace: %s: %w", k, err)
		}
		for _, it := range items {
			if err := f.stripJSONAttrs(it, "span"); err != nil {
				return err
			}
		}
		b, err := json.Marshal(items)
		if err != nil {
			return err
		}
		sp[k] = b
	}
	return nil
}

type jsonKV struct {
	Key   string `json:"key"`
	Value struct {
		String *string `json:"stringValue"`
	} `json:"value"`
}

func jsonAttrSet(raw json.RawMessage) (attrSet, error) {
	a := attrSet{}
	if len(raw) == 0 {
		return a, nil
	}
	var kvs []jsonKV
	if err := json.Unmarshal(raw, &kvs); err != nil {
		return nil, fmt.Errorf("decode json trace: attributes: %w", err)
	}
	for _, kv := range kvs {
		v := attrVal{}
		if kv.Value.String != nil {
			v = attrVal{s: *kv.Value.String, isString: true}
		}
		a.add(kv.Key, v)
	}
	return a, nil
}

// jsonIdentity is the string representing an attribute value for redaction: the string itself, or the
// compacted JSON of any other value.
func jsonIdentity(raw json.RawMessage) string {
	var sv struct {
		StringValue *string `json:"stringValue"`
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) == nil && len(probe) == 1 && json.Unmarshal(raw, &sv) == nil && sv.StringValue != nil {
		return *sv.StringValue
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

// stripJSONAttrs removes denied attributes from obj["attributes"].
func (f *TraceFilter) stripJSONAttrs(obj jsonObj, scope string) error {
	if len(f.denied) == 0 {
		return nil
	}
	raw, ok := obj["attributes"]
	if !ok {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("decode json trace: attributes: %w", err)
	}
	kept := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		var kv struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(it, &kv); err != nil {
			return fmt.Errorf("decode json trace: attribute: %w", err)
		}
		if red := f.opt.Redactor; red != nil {
			if rule, id, ok := red.AttrRule(scope, kv.Key); ok {
				nv, keep := red.Value(rule, id, jsonIdentity(kv.Value))
				if !keep {
					continue
				}
				b, err := json.Marshal(map[string]any{"key": kv.Key, "value": map[string]string{"stringValue": nv}})
				if err != nil {
					return err
				}
				kept = append(kept, b)
				continue
			}
		}
		if !f.isDenied(scope, kv.Key) {
			kept = append(kept, it)
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	obj["attributes"] = b
	return nil
}
