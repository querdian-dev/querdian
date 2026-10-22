package enforce

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
)

// FilterSeriesResponse removes the denied label names from every label set in the "data" array of a Prometheus
// or Loki "series" API response and drops label sets that became identical. Bodies larger than maxBytes,
// non-object bodies and a "data" member that is not an array of label sets are errors: the caller must then
// fail the request and never forward the original body. A body that is an upstream error (no "data" and not
// "status":"success") is returned unchanged. All other members of the envelope are preserved (member order may
// change). Redacted labels are denied too and are therefore removed here as well; query results are rewritten
// only for labels.redact (internal/enforce/redact).
func FilterSeriesResponse(body []byte, denied []string, maxBytes int) ([]byte, error) {
	if len(body) > maxBytes {
		return nil, fmt.Errorf("series response exceeds %d bytes", maxBytes)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("series response is not a JSON object: %w", err)
	}
	raw, ok := env["data"]
	if !ok {
		if string(env["status"]) == `"success"` {
			return nil, fmt.Errorf("series response has no data member")
		}
		return body, nil
	}
	var sets []map[string]string
	if err := json.Unmarshal(raw, &sets); err != nil {
		return nil, fmt.Errorf("series response data is not an array of label sets: %w", err)
	}
	kept := make([]map[string]string, 0, len(sets))
	seen := make(map[string]struct{}, len(sets))
	for _, set := range sets {
		out := make(map[string]string, len(set))
		for k, v := range set {
			if !slices.Contains(denied, k) {
				out[k] = v
			}
		}
		// encoding/json sorts map keys, so the encoding is a canonical identity for the set.
		key, err := marshalNoEscape(out)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[string(key)]; dup {
			continue
		}
		seen[string(key)] = struct{}{}
		kept = append(kept, out)
	}
	var err error
	if env["data"], err = marshalNoEscape(kept); err != nil {
		return nil, err
	}
	return marshalNoEscape(env)
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
