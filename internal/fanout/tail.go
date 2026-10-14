package fanout

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// DefaultTailDedupeSize is the number of recently seen entries a TailMerger remembers by default.
const DefaultTailDedupeSize = 10000

// ErrTailFrame is returned by TailMerger.Merge for frames that are not valid Loki tail frames. Callers must
// close the tail (fail closed) rather than forward the frame.
var ErrTailFrame = errors.New("invalid tail frame")

// tailStream is one stream of a Loki tail frame. Values are kept raw so that optional trailing elements
// (structured metadata) survive the merge unchanged.
type tailStream struct {
	Stream map[string]string `json:"stream"`
	Values []json.RawMessage `json:"values"`
}

// tailFrame is the JSON document of one Loki tail WebSocket message.
type tailFrame struct {
	Streams        []tailStream      `json:"streams"`
	DroppedEntries []json.RawMessage `json:"dropped_entries,omitempty"`
}

// TailMerger merges the frames of several upstream tail sockets into client frames. Entries are
// deduplicated by (stream labels, timestamp, line) with a bounded LRU, and dropped_entries are propagated.
// It is safe for concurrent use.
type TailMerger struct {
	mu   sync.Mutex
	max  int
	ll   *list.List
	seen map[[sha256.Size]byte]*list.Element
}

// NewTailMerger returns a merger remembering at most size entries (size < 1 selects DefaultTailDedupeSize).
func NewTailMerger(size int) *TailMerger {
	if size < 1 {
		size = DefaultTailDedupeSize
	}
	return &TailMerger{max: size, ll: list.New(), seen: make(map[[sha256.Size]byte]*list.Element)}
}

// Merge parses one upstream frame, drops entries that were already forwarded and returns the frame to send to
// the client. ok is false when nothing is left to send. An invalid frame yields an error wrapping ErrTailFrame.
func (m *TailMerger) Merge(frame []byte) (out []byte, ok bool, err error) {
	var in tailFrame
	if err := json.Unmarshal(frame, &in); err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrTailFrame, err)
	}
	res := tailFrame{Streams: []tailStream{}, DroppedEntries: in.DroppedEntries}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range in.Streams {
		key := labelKey(s.Stream)
		kept := make([]json.RawMessage, 0, len(s.Values))
		for _, v := range s.Values {
			var parts []json.RawMessage
			var ts, line string
			if json.Unmarshal(v, &parts) != nil || len(parts) < 2 ||
				json.Unmarshal(parts[0], &ts) != nil || json.Unmarshal(parts[1], &line) != nil {
				return nil, false, fmt.Errorf("%w: malformed entry", ErrTailFrame)
			}
			if m.remember(key, ts, line) {
				kept = append(kept, v)
			}
		}
		if len(kept) > 0 {
			res.Streams = append(res.Streams, tailStream{Stream: s.Stream, Values: kept})
		}
	}

	if len(res.Streams) == 0 && len(res.DroppedEntries) == 0 {
		return nil, false, nil
	}
	out, err = json.Marshal(res)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrTailFrame, err)
	}
	return out, true, nil
}

// remember records the entry and reports whether it is new. The caller holds m.mu.
func (m *TailMerger) remember(labels, ts, line string) bool {
	h := sha256.New()
	for _, p := range []string{labels, ts, line} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	var k [sha256.Size]byte
	h.Sum(k[:0])
	if el, dup := m.seen[k]; dup {
		m.ll.MoveToFront(el)
		return false
	}
	m.seen[k] = m.ll.PushFront(k)
	if m.ll.Len() > m.max {
		old := m.ll.Back()
		m.ll.Remove(old)
		if key, isKey := old.Value.([sha256.Size]byte); isKey {
			delete(m.seen, key)
		}
	}
	return true
}

// labelKey renders a label set canonically (sorted, length-prefixed) so that equal sets compare equal.
func labelKey(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%d:%s=%d:%s,", len(k), k, len(l[k]), l[k])
	}
	return b.String()
}
