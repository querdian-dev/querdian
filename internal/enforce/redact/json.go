package redact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrResponse marks an upstream body the rewriters do not understand.
var ErrResponse = errors.New("redact: unexpected response")

type object = map[string]json.RawMessage

// envelope decodes a Prometheus-style {"status", "data", ...} body. ok is false for an upstream error
// body (no data and not "status":"success"), which is returned unchanged. Error bodies can carry label sets
// (Prometheus and Loki print series in some query errors); the proxy withholds non-2xx bodies of redacted
// requests itself (proxy.Target.HideUpstreamErrors), so only 2xx bodies reach the rewriters.
func envelope(body []byte) (env object, ok bool, err error) {
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, false, fmt.Errorf("%w: not a JSON object: %w", ErrResponse, err)
	}
	if env == nil {
		return nil, false, fmt.Errorf("%w: null body", ErrResponse)
	}
	if _, has := env["data"]; !has {
		if string(env["status"]) == `"success"` {
			return nil, false, fmt.Errorf("%w: no data member", ErrResponse)
		}
		return env, false, nil
	}
	return env, true, nil
}

// Query rewrites a Prometheus or Loki query/query_range response: metric labels of vector and matrix
// results, and stream labels plus categorize-labels entry metadata (structuredMetadata, parsed) of
// stream results. Scalar and string results carry no labels.
func (r *Redactor) Query(body []byte) ([]byte, error) {
	env, ok, err := envelope(body)
	if err != nil || !ok {
		return body, err
	}
	var data object
	if err := json.Unmarshal(env["data"], &data); err != nil || data == nil {
		return nil, fmt.Errorf("%w: data is not an object", ErrResponse)
	}
	var rt string
	if err := json.Unmarshal(data["resultType"], &rt); err != nil {
		return nil, fmt.Errorf("%w: resultType: %w", ErrResponse, err)
	}
	var out json.RawMessage
	switch rt {
	case "scalar", "string":
		return body, nil
	case "vector", "matrix":
		out, err = r.rewriteItems(data["result"], "metric")
	case "streams":
		out, err = r.rewriteStreams(data["result"])
	default:
		return nil, fmt.Errorf("%w: resultType %q", ErrResponse, rt)
	}
	if err != nil {
		return nil, err
	}
	data["result"] = out
	if env["data"], err = marshal(data); err != nil {
		return nil, err
	}
	if err := r.addNotice(env); err != nil {
		return nil, err
	}
	return marshal(env)
}

// Exemplars rewrites a Prometheus query_exemplars response (seriesLabels and exemplar labels).
func (r *Redactor) Exemplars(body []byte) ([]byte, error) {
	env, ok, err := envelope(body)
	if err != nil || !ok {
		return body, err
	}
	var items []object
	if err := json.Unmarshal(env["data"], &items); err != nil {
		return nil, fmt.Errorf("%w: exemplar data: %w", ErrResponse, err)
	}
	var sets []map[string]string
	exemplars := make([][]object, len(items))
	for i, it := range items {
		ls, err := labelSet(it["seriesLabels"])
		if err != nil {
			return nil, err
		}
		sets = append(sets, ls)
		if raw, ok := it["exemplars"]; ok {
			if err := json.Unmarshal(raw, &exemplars[i]); err != nil {
				return nil, fmt.Errorf("%w: exemplars: %w", ErrResponse, err)
			}
		}
		for _, ex := range exemplars[i] {
			els, err := labelSet(ex["labels"])
			if err != nil {
				return nil, err
			}
			sets = append(sets, els)
		}
	}
	r.Labels(sets...)
	k := 0
	for i, it := range items {
		if it["seriesLabels"], err = marshal(sets[k]); err != nil {
			return nil, err
		}
		k++
		if exemplars[i] == nil {
			continue
		}
		for _, ex := range exemplars[i] {
			if ex["labels"], err = marshal(sets[k]); err != nil {
				return nil, err
			}
			k++
		}
		if it["exemplars"], err = marshal(exemplars[i]); err != nil {
			return nil, err
		}
	}
	if env["data"], err = marshal(items); err != nil {
		return nil, err
	}
	if err := r.addNotice(env); err != nil {
		return nil, err
	}
	return marshal(env)
}

// TailFrame rewrites one Loki tail frame: stream labels and entry metadata of "streams", and the labels
// of "dropped_entries".
func (r *Redactor) TailFrame(frame []byte) ([]byte, error) {
	var env object
	if err := json.Unmarshal(frame, &env); err != nil || env == nil {
		return nil, fmt.Errorf("%w: tail frame is not a JSON object", ErrResponse)
	}
	defer r.Notice() // tail frames have no warnings member; drop the collected names
	var err error
	if raw, ok := env["streams"]; ok && !isNull(raw) {
		if env["streams"], err = r.rewriteStreams(raw); err != nil {
			return nil, err
		}
	}
	if raw, ok := env["dropped_entries"]; ok && !isNull(raw) {
		if env["dropped_entries"], err = r.rewriteItems(raw, "labels"); err != nil {
			return nil, err
		}
	}
	return marshal(env)
}

// addNotice appends the redaction notice to the "warnings" member of a Prometheus/Loki response, which
// Grafana shows on the panel. Nothing is added when no redacted label occurred in the response.
func (r *Redactor) addNotice(env object) error {
	n := r.Notice()
	if n == "" {
		return nil
	}
	var ws []string
	if raw, ok := env["warnings"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &ws); err != nil {
			return fmt.Errorf("%w: warnings: %w", ErrResponse, err)
		}
	}
	ws = append(ws, n)
	var err error
	env["warnings"], err = marshal(ws)
	return err
}

// rewriteItems rewrites the label set under field of every object in the array raw.
func (r *Redactor) rewriteItems(raw json.RawMessage, field string) (json.RawMessage, error) {
	var items []object
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%w: result: %w", ErrResponse, err)
	}
	sets := make([]map[string]string, len(items))
	for i, it := range items {
		ls, err := labelSet(it[field])
		if err != nil {
			return nil, err
		}
		sets[i] = ls
	}
	r.Labels(sets...)
	var err error
	for i, it := range items {
		if it[field], err = marshal(sets[i]); err != nil {
			return nil, err
		}
	}
	return marshal(items)
}

// rewriteStreams rewrites Loki streams: the stream labels and, in the categorize-labels encoding, the
// structuredMetadata and parsed maps of each entry ([ts, line, {metadata}]).
func (r *Redactor) rewriteStreams(raw json.RawMessage) (json.RawMessage, error) {
	var streams []object
	if err := json.Unmarshal(raw, &streams); err != nil {
		return nil, fmt.Errorf("%w: streams: %w", ErrResponse, err)
	}
	type entryMeta struct {
		stream, entry int
		meta          object
		fields        map[string]map[string]string
	}
	var sets []map[string]string
	streamSets := make([]map[string]string, len(streams))
	entries := make([][]json.RawMessage, len(streams))
	var metas []entryMeta
	for i, st := range streams {
		ls, err := labelSet(st["stream"])
		if err != nil {
			return nil, err
		}
		streamSets[i] = ls
		sets = append(sets, ls)
		if raw, ok := st["values"]; ok && !isNull(raw) {
			if err := json.Unmarshal(raw, &entries[i]); err != nil {
				return nil, fmt.Errorf("%w: stream values: %w", ErrResponse, err)
			}
		}
		for j, e := range entries[i] {
			var tuple []json.RawMessage
			if err := json.Unmarshal(e, &tuple); err != nil || len(tuple) < 2 || len(tuple) > 3 {
				return nil, fmt.Errorf("%w: stream entry is not [ts, line(, metadata)]", ErrResponse)
			}
			if len(tuple) == 2 {
				continue
			}
			var meta object
			if err := json.Unmarshal(tuple[2], &meta); err != nil || meta == nil {
				return nil, fmt.Errorf("%w: entry metadata is not an object", ErrResponse)
			}
			em := entryMeta{stream: i, entry: j, meta: meta, fields: map[string]map[string]string{}}
			for name, v := range meta {
				ls, err := labelSet(v)
				if err != nil {
					return nil, err
				}
				em.fields[name] = ls
				sets = append(sets, ls)
			}
			metas = append(metas, em)
		}
	}
	r.Labels(sets...)
	var err error
	for _, em := range metas {
		for name, ls := range em.fields {
			if em.meta[name], err = marshal(ls); err != nil {
				return nil, err
			}
		}
		var tuple []json.RawMessage
		_ = json.Unmarshal(entries[em.stream][em.entry], &tuple) // decoded above
		if tuple[2], err = marshal(em.meta); err != nil {
			return nil, err
		}
		if entries[em.stream][em.entry], err = marshal(tuple); err != nil {
			return nil, err
		}
	}
	for i, st := range streams {
		if st["stream"], err = marshal(streamSets[i]); err != nil {
			return nil, err
		}
		if entries[i] != nil {
			if st["values"], err = marshal(entries[i]); err != nil {
				return nil, err
			}
		}
	}
	return marshal(streams)
}

// labelSet decodes a label map; absent or null is an empty set.
func labelSet(raw json.RawMessage) (map[string]string, error) {
	ls := map[string]string{}
	if len(raw) == 0 || isNull(raw) {
		return ls, nil
	}
	if err := json.Unmarshal(raw, &ls); err != nil {
		return nil, fmt.Errorf("%w: label set: %w", ErrResponse, err)
	}
	if ls == nil {
		ls = map[string]string{}
	}
	return ls, nil
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
