package receiver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/querdian-dev/querdian/internal/spokesync"
)

// maxDepth is the maximum nesting of JSON objects and arrays. The schema needs less than 10 levels.
const maxDepth = 32

// decodeRequest decodes a sync request strictly. It rejects unknown fields, duplicate object keys, data after
// the request, and field names that encoding/json accepts only because it ignores case.
func decodeRequest(body []byte) (*spokesync.Request, error) {
	if err := checkKeys(body); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var req spokesync.Request
	if err := dec.Decode(&req); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the request")
	}
	// encoding/json matches field names without regard to case. A round trip through the typed value finds
	// such names, because the typed value encodes each field with its exact name only.
	orig, err := generic(body)
	if err != nil {
		return nil, err
	}
	typed, err := json.Marshal(&req)
	if err != nil {
		return nil, err
	}
	back, err := generic(typed)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(orig, back) {
		return nil, errors.New("the request has a field that is not in the schema")
	}
	return &req, nil
}

// generic decodes JSON into maps and slices and removes empty values, as spokesync.Digest does.
func generic(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	v, _ = prune(v)
	return v, nil
}

// prune removes empty values from objects and reports whether v is not empty. Array elements stay.
func prune(v any) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if pv, keep := prune(e); keep {
				t[k] = pv
			} else {
				delete(t, k)
			}
		}
		return t, len(t) > 0
	case []any:
		for i, e := range t {
			t[i], _ = prune(e)
		}
		return t, len(t) > 0
	case string:
		return t, t != ""
	case bool:
		return t, t
	case json.Number:
		return t, t.String() != "0"
	}
	return nil, false
}

// checkKeys rejects duplicate keys in a JSON object and nesting deeper than maxDepth. encoding/json keeps
// the last of two equal keys, so a duplicate could hide a value from a reader that keeps the first.
func checkKeys(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	return checkValue(dec, 0)
}

func checkValue(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if depth >= maxDepth {
		return errors.New("the request is nested too deeply")
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := k.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if seen[key] {
				return fmt.Errorf("duplicate key %q", key)
			}
			seen[key] = true
			if err := checkValue(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := checkValue(dec, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected delimiter")
	}
	_, err = dec.Token() // the closing delimiter
	return err
}
