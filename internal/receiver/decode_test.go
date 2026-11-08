package receiver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

// testLimit is the value limit of a receiver with the default policy limit.
var testLimit = valueLimit(DefaultMaxPolicies)

// goodBody is a valid full sync with one policy.
const goodBody = `{"digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","full":true,` +
	`"policies":[{"kind":"ClusterAccessPolicy","name":"a","generation":1,` +
	`"clusterAccessPolicy":{"subjects":{"groups":["g"],"users":[]},"rules":[{"backends":["m"],"unrestricted":true,"allClusters":false,"matchers":[]}]}}]}`

// emptyRules is a full sync with n empty rule objects.
func emptyRules(n int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"digest":"x","full":true,"policies":[{"kind":"ClusterAccessPolicy","name":"a","generation":1,` +
		`"clusterAccessPolicy":{"subjects":{"groups":["g"]},"rules":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{}`)
	}
	b.WriteString(`]}}]}`)
	return b.Bytes()
}

func TestDecodeRequestStrict(t *testing.T) {
	for name, body := range map[string]string{
		"Kelvin sign in a key":   strings.Replace(goodBody, `"kind"`, `"Kind"`, 1),
		"long s in a key":        strings.Replace(goodBody, `"subjects"`, `"ſubjects"`, 1),
		"upper case nested":      strings.Replace(goodBody, `"backends"`, `"Backends"`, 1),
		"top level null":         `null`,
		"top level string":       `"x"`,
		"object for an array":    strings.Replace(goodBody, `"groups":["g"]`, `"groups":{"a":"g"}`, 1),
		"array for an object":    strings.Replace(goodBody, `"subjects":{"groups":["g"],"users":[]}`, `"subjects":[]`, 1),
		"object for a string":    strings.Replace(goodBody, `"name":"a"`, `"name":{}`, 1),
		"array for a bool":       strings.Replace(goodBody, `"full":true`, `"full":[]`, 1),
		"too many values":        string(emptyRules(testLimit / objectWeight)),
		"null policy":            `{"digest":"x","full":true,"policies":[null]}`,
		"null group":             strings.Replace(goodBody, `"groups":["g"]`, `"groups":[null]`, 1),
		"too many scalar values": `{"digest":"x","full":true,"policies":[{"kind":"ClusterAccessPolicy","name":"a","generation":1,"clusterAccessPolicy":{"subjects":{"groups":[` + strings.Repeat(`"",`, testLimit) + `""]}}}]}`,
	} {
		if _, err := decodeRequest([]byte(body), testLimit); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := decodeRequest(emptyRules(testLimit/objectWeight-20), testLimit); err != nil {
		t.Fatalf("a request just below the limit: %v", err)
	}
	long := strings.Replace(goodBody, `"name":"a"`, `"`+strings.Repeat("x", 10000)+`":"a"`, 1)
	if _, err := decodeRequest([]byte(long), testLimit); err == nil || len(err.Error()) > 200 {
		t.Fatalf("an unknown long key must give a short error: %d bytes", len(err.Error()))
	}
}

// TestDecodeLargeState: a state of DefaultMaxPolicies policies with two rules each fits the value limit with
// room to spare.
func TestDecodeLargeState(t *testing.T) {
	two := int32(2)
	ps := make([]v1.SpokePolicySource, DefaultMaxPolicies)
	for i := range ps {
		rule := v1.Rule{
			Backends: []string{"metrics", "logs"},
			Matchers: []v1.Matcher{{Name: "app", Op: v1.MatchEqual, Value: "checkout"}, {Name: "env", Op: v1.MatchRegexMatch, Value: "prod|stage"}},
			Labels:   &v1.LabelFilter{Deny: []string{"secret"}, Redact: []v1.RedactRule{{Name: "ip", Strategy: v1.RedactIPv4Prefix, Octets: &two}}},
		}
		ps[i] = v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Namespace: "team", Name: fmt.Sprintf("policy-%d", i), Generation: 3, TenantLabel: "tenant-a",
			AccessPolicy: &v1.AccessPolicySpec{Subjects: v1.Subjects{Groups: []string{"team-a", "team-b"}, Users: []string{"alice"}}, Rules: []v1.Rule{rule, rule}}}
	}
	b, err := json.Marshal(spokesync.Request{Digest: digestOf(t, ps...), Full: true, Policies: ps})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > DefaultMaxBodyBytes {
		t.Fatalf("the state is larger than the body limit: %d bytes", len(b))
	}
	req, err := decodeRequest(b, testLimit)
	if err != nil {
		t.Fatalf("large state: %v", err)
	}
	if err := validateRequest(req, DefaultMaxPolicies); err != nil {
		t.Fatalf("large state: %v", err)
	}
	// Twice the state must still fit, so that the limit is not near a real state.
	double := append(append([]v1.SpokePolicySource(nil), ps...), ps...)
	b, _ = json.Marshal(spokesync.Request{Digest: "x", Full: true, Policies: double})
	if _, err := decodeRequest(b, testLimit); err != nil {
		t.Fatalf("twice the large state: %v", err)
	}
}

// FuzzDecodeRequest: decodeRequest does not panic, and each body that it accepts gives the same request
// after json.Marshal and a second decode. The oracle is the earlier round-trip check: the accepted body and
// the encoded request have the same content with empty values removed. Thus no field name that differs in
// case and no ignored value can pass.
func FuzzDecodeRequest(f *testing.F) {
	f.Add([]byte(goodBody))
	f.Add([]byte(`{"digest":"x"}`))
	f.Add([]byte(`{"Digest":"x"}`))
	f.Add([]byte(`{"digest":"x","digest":"y"}`))
	f.Add([]byte(`{"digest":"x","full":true,"policies":[{"kind":"AccessPolicy","namespace":"n","name":"a","generation":2,"tenantLabel":"t",` +
		`"accessPolicy":{"subjects":{"users":["u"]},"rules":[{"backends":["l"],"matchers":[{"name":"app","op":"Equal","value":"x"}],` +
		`"labels":{"deny":["s"],"redact":[{"name":"ip","strategy":"IPv4Prefix","octets":1}]}}]}}]}`))
	f.Add([]byte(strings.Replace(goodBody, `"kind"`, `"Kind"`, 1)))
	f.Add([]byte(`{"digest":"x","full":true,"policies":[{"generation":-0}]}`))
	f.Add(emptyRules(3))
	f.Add([]byte(`{"digest":"x","full":true,"policies":[null]}`))
	f.Add([]byte(strings.Replace(goodBody, `"groups":["g"]`, `"groups":[null]`, 1)))
	f.Add([]byte(`{"digest":null,"full":null,"policies":null}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := decodeRequest(body, testLimit)
		if err != nil {
			return
		}
		enc, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		again, err := decodeRequest(enc, testLimit)
		if err != nil {
			t.Fatalf("the encoded request is rejected: %v\n%s", err, enc)
		}
		if enc2, _ := json.Marshal(again); !bytes.Equal(enc, enc2) {
			t.Fatalf("the round trip changed the request:\n%s\n%s", enc, enc2)
		}
		if a, b := oracleValue(t, body), oracleValue(t, enc); !reflect.DeepEqual(a, b) {
			t.Fatalf("the body has content that the request does not have:\nbody    %v\nrequest %v", a, b)
		}
	})
}

// oracleValue decodes JSON into maps and slices and removes empty values. Numbers are float64, so that -0,
// 0 and 0.0 are equal, as they are in the typed decode.
func oracleValue(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("oracle decode: %v", err)
	}
	v, _ = oraclePrune(v)
	return v
}

func oraclePrune(v any) (any, bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if pv, keep := oraclePrune(e); keep {
				x[k] = pv
			} else {
				delete(x, k)
			}
		}
		return x, len(x) > 0
	case []any:
		for i, e := range x {
			x[i], _ = oraclePrune(e)
		}
		return x, len(x) > 0
	case string:
		return x, x != ""
	case bool:
		return x, x
	case float64:
		return x, x != 0
	}
	return nil, false
}

// TestRequestSchemaKinds: the request has only the kinds that checkKeys and fieldsOf support. A map, an
// interface, an embedded field or a custom decoder needs a review of the decoder first.
func TestRequestSchemaKinds(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type, string)
	walk = func(rt reflect.Type, path string) {
		if seen[rt] {
			return
		}
		seen[rt] = true
		if rt.Implements(unmarshalerType) || reflect.PointerTo(rt).Implements(unmarshalerType) {
			t.Errorf("%s: %s has a custom decoder", path, rt)
		}
		switch rt.Kind() {
		case reflect.Pointer, reflect.Slice:
			walk(rt.Elem(), path+"[]")
		case reflect.Struct:
			for i := range rt.NumField() {
				f := rt.Field(i)
				if f.Anonymous {
					t.Errorf("%s: embedded field %s", path, f.Name)
				}
				if f.IsExported() && f.Tag.Get("json") != "-" {
					walk(f.Type, path+"."+f.Name)
				}
			}
		case reflect.String, reflect.Bool, reflect.Int32, reflect.Int64:
		default:
			t.Errorf("%s: kind %s is not supported by the decoder", path, rt.Kind())
		}
	}
	walk(requestType, "Request")
}

// The schema walk finds the fields of each struct in the request by their exact JSON names.
func TestFieldsOf(t *testing.T) {
	got := fieldsOf(reflect.TypeFor[v1.SpokePolicySource]())
	for _, n := range []string{"kind", "namespace", "name", "generation", "tenantLabel", "clusterAccessPolicy", "accessPolicy"} {
		if _, ok := got[n]; !ok {
			t.Errorf("field %q missing", n)
		}
	}
	if len(got) != 7 {
		t.Errorf("fields = %v", got)
	}
	// The type is built at run time, because go vet rejects the repeated tags that the test needs.
	str := reflect.TypeFor[string]()
	field := func(name, tag string) reflect.StructField {
		return reflect.StructField{Name: name, Type: str, Tag: reflect.StructTag(tag)}
	}
	inner := reflect.StructOf([]reflect.StructField{field("A", `json:"a"`), field("B", `json:"b"`)})
	other := reflect.StructOf([]reflect.StructField{field("B", `json:"b"`)})
	outer := reflect.StructOf([]reflect.StructField{
		{Name: "Inner", Type: inner, Anonymous: true},
		{Name: "Other", Type: other, Anonymous: true},
		field("A", `json:"a"`),
		field("C", `json:"-"`),
		field("D", ""),
		field("Dup1", `json:"dup"`),
		field("Dup2", `json:"dup"`),
	})
	got = fieldsOf(outer)
	want := map[string]reflect.Type{"a": str, "D": str}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %v, want %v (b is ambiguous, dup is ambiguous)", got, want)
	}
}
