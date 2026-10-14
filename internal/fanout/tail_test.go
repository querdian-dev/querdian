package fanout

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestTailMergerDedupe(t *testing.T) {
	m := NewTailMerger(10)
	a := `{"streams":[{"stream":{"a":"1","b":"2"},"values":[["1","x"],["2","y","meta"]]}]}`
	out, ok, err := m.Merge([]byte(a))
	if err != nil || !ok || !strings.Contains(string(out), `["2","y","meta"]`) {
		t.Fatalf("out=%s ok=%v err=%v", out, ok, err)
	}
	// Same entries, label order swapped: nothing left to send.
	b := `{"streams":[{"stream":{"b":"2","a":"1"},"values":[["1","x"]]}]}`
	if _, ok, err := m.Merge([]byte(b)); err != nil || ok {
		t.Fatalf("duplicate forwarded: ok=%v err=%v", ok, err)
	}
	// Same timestamp and line under other labels is distinct.
	c := `{"streams":[{"stream":{"a":"other"},"values":[["1","x"]]}]}`
	if _, ok, _ := m.Merge([]byte(c)); !ok {
		t.Fatal("entry of another stream dropped")
	}
}

func TestTailMergerDroppedOnly(t *testing.T) {
	m := NewTailMerger(0)
	out, ok, err := m.Merge([]byte(`{"streams":[],"dropped_entries":[{"labels":{"a":"1"},"timestamp":"5"}]}`))
	if err != nil || !ok || !strings.Contains(string(out), `"dropped_entries":[{"labels":{"a":"1"},"timestamp":"5"}]`) {
		t.Fatalf("out=%s ok=%v err=%v", out, ok, err)
	}
}

func TestTailMergerEviction(t *testing.T) {
	m := NewTailMerger(2)
	frame := func(i int) []byte {
		return fmt.Appendf(nil, `{"streams":[{"stream":{"a":"1"},"values":[["%d","l"]]}]}`, i)
	}
	for i := range 3 {
		if _, ok, _ := m.Merge(frame(i)); !ok {
			t.Fatalf("entry %d dropped", i)
		}
	}
	if _, ok, _ := m.Merge(frame(0)); !ok {
		t.Fatal("evicted entry should be forwarded again")
	}
	if _, ok, _ := m.Merge(frame(2)); ok {
		t.Fatal("recent entry should still be deduplicated")
	}
}

func TestTailMergerInvalid(t *testing.T) {
	m := NewTailMerger(2)
	for _, in := range []string{"x", `{"streams":[{"stream":{},"values":[["1"]]}]}`, `{"streams":[{"stream":{},"values":[[1,2]]}]}`} {
		if _, _, err := m.Merge([]byte(in)); !errors.Is(err, ErrTailFrame) {
			t.Errorf("%s: err = %v", in, err)
		}
	}
}
