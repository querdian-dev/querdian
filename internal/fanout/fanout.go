// Package fanout executes several upstream requests in parallel with all-or-nothing semantics, a cap on
// the total buffered response size of one call and an optional process-wide cap on the buffered bytes of
// all calls (PLAN §9). Merging the responses is done elsewhere.
package fanout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// ErrMergeTooLarge is returned when the buffered responses exceed the byte cap (502 merge_too_large).
var ErrMergeTooLarge = errors.New("merge_too_large")

// ErrBudgetExhausted is returned when the process-wide Budget has no space for more buffered bytes
// (503 merge_budget_exhausted).
var ErrBudgetExhausted = errors.New("merge_budget_exhausted")

// readChunk is the largest number of bytes that one branch reads in one step. A branch counts each chunk
// before it keeps it, so all branches together read at most one chunk each past the byte cap.
const readChunk = 32 << 10

// Buffer growth: the buffer of a branch doubles while it is smaller than growDoubleLimit, then grows by a
// quarter. It never grows past the bytes that the branch can still keep plus one chunk.
const (
	minBuffer       = 4 << 10
	growDoubleLimit = 1 << 20
)

// Budget is a process-wide limit on the buffered body bytes of all concurrent Run calls. It is safe for
// concurrent use. A nil *Budget sets no limit.
type Budget struct {
	limit int64
	used  atomic.Int64
}

// NewBudget returns a Budget of limit bytes. A limit < 1 allows no body bytes.
func NewBudget(limit int64) *Budget { return &Budget{limit: limit} }

// InUse returns the number of reserved bytes.
func (b *Budget) InUse() int64 {
	if b == nil {
		return 0
	}
	return b.used.Load()
}

// Release returns the body bytes of resps, which a successful Run reserved, to the budget. Call it once,
// when the bodies are no longer used.
func (b *Budget) Release(resps []Response) {
	var n int64
	for _, r := range resps {
		n += int64(len(r.Body))
	}
	b.release(n)
}

// reserve adds n bytes. It adds nothing and returns false when the total would be above the limit.
func (b *Budget) reserve(n int64) bool {
	if b == nil {
		return true
	}
	for {
		cur := b.used.Load()
		if n > b.limit-cur {
			return false
		}
		if b.used.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

func (b *Budget) release(n int64) {
	if b != nil && n != 0 {
		b.used.Add(-n)
	}
}

// maxErrorSnippet bounds how much of an upstream error body is surfaced to the client.
const maxErrorSnippet = 256

// UpstreamError is a failed branch: a transport error or a non-2xx response.
type UpstreamError struct {
	// Index is the position of the failed request in the input slice.
	Index int
	// Status is the upstream status code, 0 for transport errors.
	Status int
	// Detail is a short, sanitised description (status and truncated body, or the transport error).
	Detail string
	Err    error
}

// Error implements error.
func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream request %d failed: %s", e.Index, e.Detail)
}

// Unwrap returns the transport error, if any.
func (e *UpstreamError) Unwrap() error { return e.Err }

// Doer sends HTTP requests; *http.Client implements it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Response is one fully buffered upstream response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Options bound a Run.
type Options struct {
	// MaxParallel limits concurrent requests (values < 1 mean 1).
	MaxParallel int
	// MaxBytes caps the total of all response bodies (values < 1 mean no body is allowed). The bytes are
	// counted while they are read, so the read stops when the total goes above the cap.
	MaxBytes int64
	// Budget is the process-wide limit that each body byte is also reserved in; nil means no such limit.
	// When it is full, Run fails with ErrBudgetExhausted. After a successful Run the bytes of the returned
	// bodies stay reserved until the caller calls Budget.Release.
	Budget *Budget
}

// meter counts the body bytes of one Run in the cap of the call and in the process-wide budget.
type meter struct {
	maxBytes int64
	used     atomic.Int64 // bytes kept by all branches of this call
	budget   *Budget
	held     atomic.Int64 // bytes of this call reserved in budget
}

// keep counts n bytes that a branch has read. It returns an error, and the branch must drop the bytes,
// when the total of the call is above maxBytes or the budget is full.
func (m *meter) keep(n int) error {
	if m.used.Add(int64(n)) > m.maxBytes {
		return ErrMergeTooLarge
	}
	if !m.budget.reserve(int64(n)) {
		return ErrBudgetExhausted
	}
	m.held.Add(int64(n))
	return nil
}

// room returns the number of bytes that the call can still keep.
func (m *meter) room() int64 { return max(0, m.maxBytes-m.used.Load()) }

// Run sends all requests and returns their responses in input order. Any failure (transport error, non-2xx
// status, byte cap exceeded, budget full) cancels the remaining requests and fails the whole call. The
// request contexts are replaced by one derived from ctx. Every request is sent with Accept: application/json.
func Run(ctx context.Context, d Doer, reqs []*http.Request, opt Options) ([]Response, error) {
	if opt.MaxParallel < 1 {
		opt.MaxParallel = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		out      = make([]Response, len(reqs))
		sem      = make(chan struct{}, opt.MaxParallel)
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
		m        = &meter{maxBytes: opt.MaxBytes, budget: opt.Budget}
	)
	fail := func(err error) {
		once.Do(func() {
			firstErr = err
			cancel()
		})
	}

	for i, r := range reqs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int, r *http.Request) {
			defer wg.Done()
			defer func() { <-sem }()
			resp, err := one(ctx, d, r, i, m)
			if err != nil {
				fail(err)
				return
			}
			out[i] = resp
		}(i, r)
	}
	wg.Wait()

	if firstErr != nil {
		m.budget.release(m.held.Load())
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		// The parent context ended (client gone or timeout) before every request started.
		m.budget.release(m.held.Load())
		return nil, &UpstreamError{Index: -1, Detail: err.Error(), Err: err}
	}
	return out, nil
}

func one(ctx context.Context, d Doer, r *http.Request, idx int, m *meter) (Response, error) {
	r = r.Clone(ctx)
	r.Header.Set("Accept", "application/json")
	resp, err := d.Do(r)
	if err != nil {
		return Response{}, &UpstreamError{Index: idx, Detail: sanitize(err), Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorSnippet))
		return Response{}, &UpstreamError{Index: idx, Status: resp.StatusCode, Detail: fmt.Sprintf("status %d: %s", resp.StatusCode, snippet)}
	}

	body, err := readBody(ctx, resp.Body, m)
	if err != nil {
		if errors.Is(err, ErrMergeTooLarge) || errors.Is(err, ErrBudgetExhausted) {
			return Response{}, err
		}
		return Response{}, &UpstreamError{Index: idx, Detail: sanitize(err), Err: err}
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// readBody reads r to the end in chunks of at most readChunk bytes. Each chunk is counted in m before it
// is kept. The read stops at the first chunk that m does not accept, or when ctx ends.
func readBody(ctx context.Context, r io.Reader, m *meter) ([]byte, error) {
	var buf []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(buf) == cap(buf) {
			buf = grow(buf, m.room())
		}
		n, err := r.Read(buf[len(buf):min(cap(buf), len(buf)+readChunk)])
		if n > 0 {
			if kerr := m.keep(n); kerr != nil {
				return nil, kerr
			}
			buf = buf[:len(buf)+n]
		}
		if errors.Is(err, io.EOF) {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// grow returns a copy of buf with more capacity. The capacity doubles for small buffers and grows by a
// quarter for large ones, but never goes past len(buf) + room + readChunk, so that a branch does not
// allocate much more than the bytes it can keep.
func grow(buf []byte, room int64) []byte {
	c := int64(cap(buf))
	next := c + c/4
	if c < growDoubleLimit {
		next = 2 * c
	}
	next = max(next, minBuffer)
	next = min(next, int64(len(buf))+room+readChunk)
	out := make([]byte, len(buf), next)
	copy(out, buf)
	return out
}

// sanitize drops the request URL that net/http embeds in transport errors.
func sanitize(err error) string {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		if inner := ue.Unwrap(); inner != nil {
			return inner.Error()
		}
	}
	return err.Error()
}
