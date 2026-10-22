// Package redact rewrites the values of redacted labels (policy labels.redact) in query results.
//
// Redaction is the output-side complement of denied labels: every redacted name is also denied, so a
// query cannot filter, group or join on it, and the values that still appear in returned label sets are
// rewritten here according to the strategy of the effective policy:
//
//   - Remove: the label is dropped. Label sets that become identical are kept as separate entries (only
//     the number of entries remains visible), except where the response is a set (series), which is
//     deduplicated.
//   - Placeholder: each distinct value becomes redacted-<n>. Numbers are per label and per Redactor; the
//     values seen in one response are numbered in random order, so the number does not reveal the sort
//     order of the hidden values.
//   - Hash: each value becomes redacted-<16 hex> of HMAC-SHA256 under a key that is random per Redactor.
//     Equal values are recognisable within one response, never across requests.
//   - IPv4Prefix: an IPv4 address keeps its first n octets (10.1.x.x); any other value becomes "redacted".
//
// A Redactor is created per client request (per connection for tail) and is not safe for concurrent use.
// All rewriters fail on input they do not understand: the caller must then fail the request and never
// forward the original body.
package redact

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/pkg/traceql"
)

// Fallback replaces values that a strategy cannot represent (IPv4Prefix of a non-IPv4 value).
const Fallback = "redacted"

const prefix = "redacted-"

// Redactor rewrites label values for one request.
type Redactor struct {
	rules map[string]policy.Redaction
	key   []byte
	// ordinals per label name: value -> n; pending holds values observed but not yet numbered.
	ordinals map[string]map[string]int
	pending  map[string][]string
	// hit collects the redaction names that matched a label since the last notice (see Notice).
	hit map[string]struct{}
}

// New returns a Redactor for the redactions of an effective policy, or nil if there are none.
func New(rules map[string]policy.Redaction) *Redactor {
	if len(rules) == 0 {
		return nil
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key) // crypto/rand.Read never returns an error
	return &Redactor{
		rules:    rules,
		key:      key,
		ordinals: map[string]map[string]int{},
		pending:  map[string][]string{},
		hit:      map[string]struct{}{},
	}
}

// Rule returns the redaction for a Prometheus or Loki label name.
func (r *Redactor) Rule(name string) (policy.Redaction, bool) {
	red, ok := r.rules[name]
	return red, ok
}

// AttrRule returns the redaction for a Tempo attribute. scope is "span", "resource" or "" when the
// response does not say (search results); an unknown scope matches a redaction of any scope, and the
// strictest one wins. A key that carries a scope itself ("resource.service.name" in metrics series) is
// split first. The returned id identifies the attribute for numbering and hashing.
func (r *Redactor) AttrRule(scope, key string) (red policy.Redaction, id string, ok bool) {
	if scope == "" {
		if s, n := traceql.SplitAttr(key); s == "span" || s == "resource" {
			scope, key = s, n
		}
	}
	for name, cand := range r.rules {
		s, n := traceql.SplitAttr(name)
		if n != key || (scope != "" && s != scope) {
			continue
		}
		if ok {
			cand = policy.Stricter(red, cand)
		}
		red, ok = cand, true
		r.hit[name] = struct{}{}
	}
	return red, "." + key, ok
}

// Observe records a value that will be rewritten with the redaction of id. It only matters for
// Placeholder: Commit then numbers all observed values in random order.
func (r *Redactor) Observe(red policy.Redaction, id, value string) {
	if red.Strategy != v1.RedactPlaceholder {
		return
	}
	if _, ok := r.ordinals[id][value]; ok {
		return
	}
	r.pending[id] = append(r.pending[id], value)
}

// Commit numbers the observed values.
func (r *Redactor) Commit() {
	for id, vals := range r.pending {
		slices.Sort(vals)
		vals = slices.Compact(vals)
		shuffle(vals)
		for _, v := range vals {
			r.number(id, v)
		}
	}
	clear(r.pending)
}

func (r *Redactor) number(id, value string) int {
	m := r.ordinals[id]
	if m == nil {
		m = map[string]int{}
		r.ordinals[id] = m
	}
	n, ok := m[value]
	if !ok {
		n = len(m) + 1
		m[value] = n
	}
	return n
}

// Value returns the rewritten value, or keep == false if the label is to be removed. Placeholder values
// that were not observed before Commit are numbered in order of first use.
func (r *Redactor) Value(red policy.Redaction, id, value string) (out string, keep bool) {
	switch red.Strategy {
	case v1.RedactPlaceholder:
		return prefix + strconv.Itoa(r.number(id, value)), true
	case v1.RedactHash:
		m := hmac.New(sha256.New, r.key)
		m.Write([]byte(id))
		m.Write([]byte{0})
		m.Write([]byte(value))
		return prefix + hex.EncodeToString(m.Sum(nil)[:8]), true
	case v1.RedactIPv4Prefix:
		return ipv4Prefix(value, red.Octets), true
	}
	return "", false // Remove, and fail closed for anything unknown
}

// Labels rewrites the label sets in place. Placeholder numbering covers all sets together.
func (r *Redactor) Labels(sets ...map[string]string) {
	for _, set := range sets {
		for k, v := range set {
			if red, ok := r.rules[k]; ok {
				r.hit[k] = struct{}{}
				r.Observe(red, k, v)
			}
		}
	}
	r.Commit()
	for _, set := range sets {
		for k, v := range set {
			red, ok := r.rules[k]
			if !ok {
				continue
			}
			if nv, keep := r.Value(red, k, v); keep {
				set[k] = nv
			} else {
				delete(set, k)
			}
		}
	}
}

func ipv4Prefix(v string, octets int) string {
	a, err := netip.ParseAddr(v)
	if err != nil || !a.Is4() || octets < 1 || octets > 3 {
		return Fallback
	}
	b := a.As4()
	parts := make([]string, 4)
	for i := range parts {
		if i < octets {
			parts[i] = strconv.Itoa(int(b[i]))
		} else {
			parts[i] = "x"
		}
	}
	return strings.Join(parts, ".")
}

// shuffle permutes vals uniformly (Fisher-Yates) with crypto/rand, so the numbering cannot be predicted.
func shuffle(vals []string) {
	var b [8]byte
	for i := len(vals) - 1; i > 0; i-- {
		_, _ = rand.Read(b[:])
		// j < i+1 <= len(vals) fits int; the modulo bias is negligible for 64 random bits.
		j := int(binary.LittleEndian.Uint64(b[:]) % uint64(i+1)) //nolint:gosec // bounded by len(vals)
		vals[i], vals[j] = vals[j], vals[i]
	}
}

// NoticePrefix starts every notice that the proxy adds to a response.
const NoticePrefix = "querdian: "

// Notice returns the user-facing notice for the labels redacted since the last call, or "" if none
// matched, and resets the collection. It names labels and strategies, never values.
func (r *Redactor) Notice() string {
	if len(r.hit) == 0 {
		return ""
	}
	parts := make([]string, 0, len(r.hit))
	for name := range r.hit {
		parts = append(parts, name+" ("+describe(r.rules[name])+")")
	}
	clear(r.hit)
	slices.Sort(parts)
	return NoticePrefix + "label values redacted by policy: " + strings.Join(parts, ", ")
}

func describe(red policy.Redaction) string {
	switch red.Strategy {
	case v1.RedactRemove:
		return "removed"
	case v1.RedactPlaceholder:
		return "numbered placeholders"
	case v1.RedactHash:
		return "hashed per request"
	case v1.RedactIPv4Prefix:
		return "first " + strconv.Itoa(red.Octets) + " octets kept"
	}
	return "removed"
}
