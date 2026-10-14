package fanout

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
)

// DefaultLokiLimit is the number of entries Loki returns when a log query carries no limit parameter.
const DefaultLokiLimit = 100

// MergeLokiStreams merges the query/query_range responses of a fanned-out log query (resultType "streams",
// PLAN §7.3). It has the signature of proxy.Merger.
//
// Semantics, for both directions:
//
//   - Streams with identical label sets are merged into one stream.
//   - Entries of one merged stream are deduplicated by (timestamp, line). Entries of different streams are never
//     deduplicated against each other.
//   - Entries are ordered by timestamp (descending for direction=backward, the Loki default; ascending for
//     forward). Equal timestamps are ordered by stream labels, then line, so the order is total and
//     deterministic.
//   - The result is truncated to the request's limit entries (default [DefaultLokiLimit]). This is exact: the
//     first L entries of the union of the branches are contained in the union of each branch's first L entries,
//     which is what every branch returns, so merge(topL(A), topL(B)) == topL(A union B).
//   - Streams are ordered by their first entry in that order.
//   - "stats" are summed over the branches (see mergeStats); under overlapping policy terms shared work is
//     counted once per branch, so stats are an approximation. "warnings" are kept (union, first-seen order),
//     "encodingFlags" and the per-entry metadata of the categorize-labels encoding are preserved. Branches
//     that disagree on encodingFlags cannot be merged and yield an error.
//
// limit and direction are taken from the normalised client parameters: r.Form if the server set it to them,
// otherwise the URL query of a GET request. For a POST without r.Form the parameters are unknown (the proxy has
// consumed the body) and MergeLokiStreams returns an error rather than guessing. An unparseable limit or an
// unknown direction is an error too. Any malformed upstream response is an error; nothing is written then.
func MergeLokiStreams(w http.ResponseWriter, r *http.Request, resps []Response) error {
	params, ok := requestParams(r)
	if !ok {
		return errors.New("fanout: request parameters unavailable for stream merge")
	}
	limit := DefaultLokiLimit
	if v := params.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("fanout: invalid limit %q", v)
		}
		limit = n
	}
	var backward bool
	switch strings.ToLower(params.Get("direction")) {
	case "", "backward":
		backward = true
	case "forward":
	default:
		return fmt.Errorf("fanout: invalid direction %q", params.Get("direction"))
	}

	merged, err := mergeStreams(resps, limit, backward)
	if err != nil {
		return err
	}
	out, err := merged.encode()
	if err != nil {
		return err
	}
	if err := writeJSON(w, out); err != nil {
		return err
	}
	annotate(r, "loki_streams", merged.in, merged.entries,
		attribute.Int("merge.deduped", merged.deduped),
		attribute.Bool("merge.truncated", merged.truncated))
	return nil
}

type lokiEntry struct {
	ts     int64
	line   string
	raw    []json.RawMessage // full entry: timestamp, line and optional metadata
	stream int               // index into the stream table
}

type lokiStream struct {
	labels map[string]string
	key    string
}

type mergedStreams struct {
	top       map[string]json.RawMessage // first branch's top-level fields, minus data
	data      map[string]json.RawMessage // first branch's data fields, minus result and stats
	warnings  []string
	stats     any
	streams   []lokiStream
	order     [][]lokiEntry // per stream, in output order
	in        int
	entries   int
	deduped   int
	truncated bool
}

func mergeStreams(resps []Response, limit int, backward bool) (*mergedStreams, error) {
	if len(resps) == 0 {
		return nil, errors.New("fanout: nothing to merge")
	}
	var (
		m        = &mergedStreams{}
		index    = map[string]int{}
		seen     = map[dedupKey]struct{}{}
		all      []lokiEntry
		flags    []string
		flagsSet bool
		statsRaw []json.RawMessage
		warnings [][]string
	)
	for i, resp := range resps {
		top, err := decodeObject(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("branch %d: %w", i, err)
		}
		if err := checkStatus(top); err != nil {
			return nil, fmt.Errorf("branch %d: %w", i, err)
		}
		var data map[string]json.RawMessage
		if err := decodeValue(top["data"], &data); err != nil || data == nil {
			return nil, fmt.Errorf("branch %d: %w", i, malformed("missing data object"))
		}
		var rt string
		if err := json.Unmarshal(data["resultType"], &rt); err != nil || rt != "streams" {
			return nil, fmt.Errorf("branch %d: %w", i, malformed("resultType is not \"streams\""))
		}
		var bflags []string
		if raw, ok := data["encodingFlags"]; ok {
			if err := json.Unmarshal(raw, &bflags); err != nil {
				return nil, fmt.Errorf("branch %d: %w", i, malformed("encodingFlags: %v", err))
			}
		}
		if !flagsSet {
			flags, flagsSet = bflags, true
		} else if !slices.Equal(flags, bflags) {
			return nil, fmt.Errorf("branch %d: %w", i, malformed("encodingFlags differ between branches"))
		}
		if raw, ok := top["warnings"]; ok {
			var ws []string
			if err := json.Unmarshal(raw, &ws); err != nil {
				return nil, fmt.Errorf("branch %d: %w", i, malformed("warnings: %v", err))
			}
			warnings = append(warnings, ws)
		}
		statsRaw = append(statsRaw, data["stats"])

		var result []struct {
			Stream map[string]string   `json:"stream"`
			Values [][]json.RawMessage `json:"values"`
		}
		if err := decodeValue(data["result"], &result); err != nil {
			return nil, fmt.Errorf("branch %d: %w", i, malformed("result: %v", err))
		}
		if i == 0 {
			m.top = top
			m.data = data
		}
		for _, s := range result {
			if s.Stream == nil {
				return nil, fmt.Errorf("branch %d: %w", i, malformed("stream without labels"))
			}
			key := labelSetKey(s.Stream)
			si, ok := index[key]
			if !ok {
				si = len(m.streams)
				index[key] = si
				m.streams = append(m.streams, lokiStream{labels: s.Stream, key: key})
			}
			for _, v := range s.Values {
				e, err := parseEntry(v, si)
				if err != nil {
					return nil, fmt.Errorf("branch %d: %w", i, err)
				}
				m.in++
				dk := dedupKey{stream: si, ts: e.ts, line: e.line}
				if _, dup := seen[dk]; dup {
					m.deduped++
					continue
				}
				seen[dk] = struct{}{}
				all = append(all, e)
			}
		}
	}

	sort.Slice(all, func(a, b int) bool {
		x, y := all[a], all[b]
		if x.ts != y.ts {
			return (x.ts > y.ts) == backward
		}
		if kx, ky := m.streams[x.stream].key, m.streams[y.stream].key; kx != ky {
			return kx < ky
		}
		return x.line < y.line
	})
	if len(all) > limit {
		all = all[:limit]
		m.truncated = true
	}
	m.entries = len(all)

	pos := map[int]int{} // stream table index -> output position
	for _, e := range all {
		p, ok := pos[e.stream]
		if !ok {
			p = len(m.order)
			pos[e.stream] = p
			m.order = append(m.order, nil)
			m.order[p] = []lokiEntry{}
		}
		m.order[p] = append(m.order[p], e)
	}
	// Re-point the stream table at output order.
	outStreams := make([]lokiStream, len(m.order))
	for si, p := range pos {
		outStreams[p] = m.streams[si]
	}
	m.streams = outStreams

	stats, err := mergeStats(statsRaw)
	if err != nil {
		return nil, err
	}
	m.stats = stats
	m.warnings = uniqueStrings(warnings...)
	return m, nil
}

type dedupKey struct {
	stream int
	ts     int64
	line   string
}

func parseEntry(v []json.RawMessage, stream int) (lokiEntry, error) {
	if len(v) < 2 {
		return lokiEntry{}, malformed("entry has %d elements", len(v))
	}
	var tsStr, line string
	if err := json.Unmarshal(v[0], &tsStr); err != nil {
		return lokiEntry{}, malformed("entry timestamp is not a string")
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return lokiEntry{}, malformed("entry timestamp %q is not an integer", tsStr)
	}
	if err := json.Unmarshal(v[1], &line); err != nil {
		return lokiEntry{}, malformed("entry line is not a string")
	}
	return lokiEntry{ts: ts, line: line, raw: v, stream: stream}, nil
}

// encode renders the merged document: the first branch's fields with result, stats and warnings replaced.
func (m *mergedStreams) encode() (map[string]any, error) {
	type outStream struct {
		Stream map[string]string   `json:"stream"`
		Values [][]json.RawMessage `json:"values"`
	}
	result := make([]outStream, len(m.order))
	for p, es := range m.order {
		vals := make([][]json.RawMessage, len(es))
		for i, e := range es {
			vals[i] = e.raw
		}
		result[p] = outStream{Stream: m.streams[p].labels, Values: vals}
	}
	data := make(map[string]any, len(m.data)+2)
	for k, v := range m.data {
		data[k] = v
	}
	data["result"] = result
	if m.stats != nil {
		data["stats"] = m.stats
	} else {
		delete(data, "stats")
	}
	top := make(map[string]any, len(m.top)+1)
	for k, v := range m.top {
		top[k] = v
	}
	top["data"] = data
	if len(m.warnings) > 0 {
		top["warnings"] = m.warnings
	} else {
		delete(top, "warnings")
	}
	return top, nil
}
