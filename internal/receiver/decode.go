package receiver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"

	"github.com/querdian-dev/querdian/internal/spokesync"
)

const (
	// maxDepth is the maximum nesting of JSON objects and arrays. The schema needs less than 10 levels.
	maxDepth = 32
	// valuesPerPolicy is the number of JSON values that a request can have for each policy of the policy
	// limit. An object counts as objectWeight values, because the typed decode makes a struct for it: an
	// empty rule object of 3 bytes gives about 300 bytes. This limit, not the body size, sets the memory of
	// one request: about 80 bytes for each value, so about 40 MiB with the default limits. A policy with two
	// rules, two matchers each and label filters needs about 95 values (TestDecodeLargeState).
	valuesPerPolicy = 256
	objectWeight    = 4
)

// valueLimit is the value limit of a request for a policy limit of maxPolicies.
func valueLimit(maxPolicies int) int { return valuesPerPolicy*maxPolicies + 64 }

var (
	requestType     = reflect.TypeFor[spokesync.Request]()
	unmarshalerType = reflect.TypeFor[json.Unmarshaler]()
)

// decodeRequest decodes a sync request strictly. It rejects unknown fields, duplicate object keys, data after
// the request, field names that encoding/json accepts only because it ignores case, and requests with more
// than maxValues values. The checks run on a stream of tokens before the typed decode, so a request uses
// only a fixed multiple of maxValues in memory.
func decodeRequest(body []byte, maxValues int) (*spokesync.Request, error) {
	if err := checkKeys(body, maxValues); err != nil {
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
	return &req, nil
}

// checkKeys walks the JSON tokens of body against the type spokesync.Request. Each object key must be the
// exact JSON name of a field of the struct at that position. encoding/json matches field names without
// regard to case, so without this check a variant such as "Name" or "name" with a Kelvin sign could set a
// field. checkKeys also rejects duplicate keys, nesting deeper than maxDepth, more than maxValues values and
// a top level that is not an object. The typed decode checks the types of the scalars.
func checkKeys(body []byte, maxValues int) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return errors.New("the request is not a JSON object")
	}
	budget := maxValues - objectWeight
	return checkContainer(dec, '{', requestType, 0, &budget)
}

// checkValue reads one value of type t. An array element must not be null unless its type has a null value:
// the typed decode makes a null element into a zero value, so the request would not be what the body says.
func checkValue(dec *json.Decoder, t reflect.Type, element bool, depth int, budget *int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil && element && !nullable(t) {
		return errors.New("an array element is null")
	}
	d, ok := tok.(json.Delim)
	cost := 1
	if d == '{' {
		cost = objectWeight
	}
	if *budget -= cost; *budget < 0 {
		return errors.New("the request has too many values")
	}
	if !ok {
		return nil
	}
	return checkContainer(dec, d, t, depth, budget)
}

// checkContainer reads the content of an object or array after its opening delimiter d, and the closing
// delimiter.
func checkContainer(dec *json.Decoder, d json.Delim, t reflect.Type, depth int, budget *int) error {
	if depth >= maxDepth {
		return errors.New("the request is nested too deeply")
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Implements(unmarshalerType) || reflect.PointerTo(t).Implements(unmarshalerType) {
		// A custom decoder could match field names without regard to case. No such type in the schema takes
		// an object or an array.
		return fmt.Errorf("unexpected %s", containerName(d))
	}
	switch {
	case d == '{' && t.Kind() == reflect.Struct:
		fields := fieldsOf(t)
		seen := map[string]bool{}
		for dec.More() {
			key, err := objectKey(dec)
			if err != nil {
				return err
			}
			ft, ok := fields[key]
			if !ok {
				return fmt.Errorf("unknown field %s", quoteShort(key))
			}
			if seen[key] {
				return fmt.Errorf("duplicate key %s", quoteShort(key))
			}
			seen[key] = true
			if err := checkValue(dec, ft, false, depth+1, budget); err != nil {
				return err
			}
		}
	case d == '[' && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array):
		for dec.More() {
			if err := checkValue(dec, t.Elem(), true, depth+1, budget); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected %s", containerName(d))
	}
	_, err := dec.Token() // the closing delimiter
	return err
}

// nullable reports whether encoding/json encodes the zero value of t as null.
func nullable(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return true
	}
	return false
}

func objectKey(dec *json.Decoder) (string, error) {
	k, err := dec.Token()
	if err != nil {
		return "", err
	}
	key, ok := k.(string)
	if !ok {
		return "", errors.New("object key is not a string")
	}
	return key, nil
}

func containerName(d json.Delim) string {
	if d == '[' {
		return "array"
	}
	return "object"
}

// quoteShort quotes at most 64 bytes of s, for error messages that go to the agent and to the log.
func quoteShort(s string) string {
	if len(s) > 64 {
		return fmt.Sprintf("%q...", s[:64])
	}
	return fmt.Sprintf("%q", s)
}

// structFields caches fieldsOf for each struct type.
var structFields sync.Map // reflect.Type -> map[string]reflect.Type

// fieldsOf supports the schema of spokesync.Request: structs, slices, strings, bools, integers and pointers,
// with no maps, no embedded fields and no custom decoders. TestRequestSchemaKinds fails when a new field kind
// gets into the request.
//
// fieldsOf returns the exact JSON names of the fields of struct type t that encoding/json decodes, with
// their types. The fields of an embedded struct with no JSON name are promoted. A name that two embedded
// structs promote is ambiguous: encoding/json ignores it, and fieldsOf leaves it out, so it is rejected.
func fieldsOf(t reflect.Type) map[string]reflect.Type {
	if v, ok := structFields.Load(t); ok {
		return v.(map[string]reflect.Type)
	}
	direct := map[string]reflect.Type{}
	promoted := map[string]reflect.Type{}
	// Two fields of the same depth with one name are ambiguous. A direct field hides a promoted one.
	directDup, promotedDup := map[string]bool{}, map[string]bool{}
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			et := f.Type
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				for n, ft := range fieldsOf(et) {
					if _, dup := promoted[n]; dup {
						promotedDup[n] = true
					}
					promoted[n] = ft
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if _, dup := direct[name]; dup {
			directDup[name] = true
		}
		direct[name] = f.Type
	}
	for n, ft := range promoted {
		if _, ok := direct[n]; !ok && !promotedDup[n] {
			direct[n] = ft
		}
	}
	for n := range directDup {
		delete(direct, n)
	}
	structFields.Store(t, direct)
	return direct
}
