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
//
// When the budget is full, the oldest Run that still reads waits for space until its context ends. Each
// other Run fails with ErrBudgetExhausted and so releases its bytes. While the oldest Run waits, no other Run
// gets new space. Thus the oldest Run always gets its bytes in a bounded time (each lease ends after the
// merge and the write, which have deadlines), and concurrent large fan-outs cannot all fail together.
type Budget struct {
	limit int64

	mu      sync.Mutex
	used    int64
	nextRun uint64
	reading map[uint64]struct{} // Run calls that still read, by start order
	waiter  uint64              // the Run that waits for space; 0 is none
	freed   chan struct{}       // closed and replaced at each release
}

// NewBudget returns a Budget of limit bytes. A limit < 1 allows no body bytes.
func NewBudget(limit int64) *Budget {
	return &Budget{limit: limit, reading: map[uint64]struct{}{}, freed: make(chan struct{})}
}

// InUse returns the number of reserved bytes.
func (b *Budget) InUse() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// enter registers a Run that starts to read and returns its number (never 0).
func (b *Budget) enter() uint64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextRun++
	b.reading[b.nextRun] = struct{}{}
	return b.nextRun
}

// leave removes a Run that does not read any more.
func (b *Budget) leave(run uint64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.reading, run)
	if b.waiter == run {
		b.waiter = 0
	}
}

// oldest returns the number of the oldest Run that still reads. b.mu must be held.
func (b *Budget) oldest() uint64 {
	var o uint64
	for r := range b.reading {
		if o == 0 || r < o {
			o = r
		}
	}
	return o
}

// reserve adds n bytes for run (0 is a caller that is not a Run). When the budget has no space, the oldest
// reading Run waits until there is space or ctx ends; any other caller gets ErrBudgetExhausted. Nothing is
// added on error.
func (b *Budget) reserve(ctx context.Context, run uint64, n int64) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	for {
		if n <= b.limit-b.used && (b.waiter == 0 || b.waiter == run) {
			b.used += n
			if b.waiter == run {
				b.waiter = 0
			}
			b.mu.Unlock()
			return nil
		}
		if run == 0 || run != b.oldest() || n > b.limit {
			b.mu.Unlock()
			return ErrBudgetExhausted
		}
		b.waiter = run
		freed := b.freed
		b.mu.Unlock()
		select {
		case <-freed:
		case <-ctx.Done():
			b.mu.Lock()
			if b.waiter == run {
				b.waiter = 0
			}
			b.mu.Unlock()
			return ctx.Err()
		}
		b.mu.Lock()
	}
}

func (b *Budget) release(n int64) {
	if b == nil || n == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	close(b.freed)
	b.freed = make(chan struct{})
}

// Lease holds the bytes that a successful Run reserved in a Budget. It keeps its own count, so a change of
// the response bodies does not change what it releases. It is safe for concurrent use. A nil *Lease holds
// nothing.
type Lease struct {
	mu sync.Mutex
	b  *Budget
	n  int64
}

// Bytes returns the number of bytes that the lease holds.
func (l *Lease) Bytes() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// Resize changes the reservation of the lease to n bytes (n < 0 means 0). It returns false, and changes
// nothing, when the budget has no space for the increase or the lease was released.
func (l *Lease) Resize(n int64) bool {
	if l == nil {
		return false
	}
	n = max(n, 0)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n < 0 {
		return false
	}
	switch {
	case n > l.n:
		if l.b.reserve(context.Background(), 0, n-l.n) != nil {
			return false
		}
	case n < l.n:
		l.b.release(l.n - n)
	}
	l.n = n
	return true
}

// Release returns all bytes of the lease to the budget. Further calls do nothing.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n > 0 {
		l.b.release(l.n)
	}
	l.n = -1 // released
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
	// bodies stay reserved in the returned Lease until the caller releases or resizes it.
	Budget *Budget
}

// meter counts the body bytes of one Run in the cap of the call and in the process-wide budget.
type meter struct {
	maxBytes int64
	used     atomic.Int64 // bytes kept by all branches of this call
	budget   *Budget
	run      uint64       // number of this call in budget
	held     atomic.Int64 // bytes of this call reserved in budget
}

// keep counts n bytes that a branch has read. It returns an error, and the branch must drop the bytes,
// when the total of the call is above maxBytes, the budget is full, or ctx ends while the call waits for
// space in the budget.
func (m *meter) keep(ctx context.Context, n int) error {
	if m.used.Add(int64(n)) > m.maxBytes {
		return ErrMergeTooLarge
	}
	if err := m.budget.reserve(ctx, m.run, int64(n)); err != nil {
		return err
	}
	m.held.Add(int64(n))
	return nil
}

// room returns the number of bytes that the call can still keep.
func (m *meter) room() int64 { return max(0, m.maxBytes-m.used.Load()) }

// Run sends all requests and returns their responses in input order. Any failure (transport error, non-2xx
// status, byte cap exceeded, budget full) cancels the remaining requests and fails the whole call. The
// request contexts are replaced by one derived from ctx. Every request is sent with Accept: application/json.
// On success the Lease holds the bytes of the bodies in opt.Budget; the caller must release it. On failure
// Run has released all bytes and the Lease is nil.
func Run(ctx context.Context, d Doer, reqs []*http.Request, opt Options) ([]Response, *Lease, error) {
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
		m        = &meter{maxBytes: opt.MaxBytes, budget: opt.Budget, run: opt.Budget.enter()}
	)
	defer opt.Budget.leave(m.run)
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
		return nil, nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		// The parent context ended (client gone or timeout) before every request started.
		m.budget.release(m.held.Load())
		return nil, nil, &UpstreamError{Index: -1, Detail: err.Error(), Err: err}
	}
	return out, &Lease{b: m.budget, n: m.held.Load()}, nil
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
			if kerr := m.keep(ctx, n); kerr != nil {
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
