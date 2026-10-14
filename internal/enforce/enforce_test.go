package enforce

import (
	"bytes"
	"testing"
)

func TestPlanResponseFilterIsOptional(t *testing.T) {
	// Existing callers build Plans without the response filter: the zero value means "stream as is".
	p := Plan{Requests: []Upstream{{Term: -1}}, Merge: MergeNone, Strategy: "inject"}
	if p.ResponseFilter != nil || p.ResponseFilterMaxBytes != 0 {
		t.Fatal("zero Plan must not request response filtering")
	}
	p.ResponseFilter = func(b []byte) ([]byte, error) { return bytes.ToUpper(b), nil }
	if got, err := p.ResponseFilter([]byte("a")); err != nil || string(got) != "A" {
		t.Errorf("got %q, %v", got, err)
	}
}
