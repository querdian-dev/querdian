package fanout

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// endlessBody returns 'x' bytes without end. Its first Read waits until ready is closed.
type endlessBody struct {
	ready <-chan struct{}
	read  *atomic.Int64
	once  sync.Once
}

func (b *endlessBody) Read(p []byte) (int, error) {
	b.once.Do(func() { <-b.ready })
	for i := range p {
		p[i] = 'x'
	}
	b.read.Add(int64(len(p)))
	return len(p), nil
}

func (b *endlessBody) Close() error { return nil }

// barrierDoer answers each request with an endlessBody. No body gives data before all n requests started.
type barrierDoer struct {
	n       int32
	started atomic.Int32
	ready   chan struct{}
	read    atomic.Int64
}

func newBarrierDoer(n int) *barrierDoer {
	return &barrierDoer{n: int32(n), ready: make(chan struct{})} //nolint:gosec // small test value
}

func (d *barrierDoer) Do(*http.Request) (*http.Response, error) {
	if d.started.Add(1) == d.n {
		close(d.ready)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &endlessBody{ready: d.ready, read: &d.read}}, nil
}

// sizedDoer answers request i with sizes[i] bytes. reqs encodes i as the rune '0'+i in the raw query.
type sizedDoer struct{ sizes []int }

func (d sizedDoer) Do(r *http.Request) (*http.Response, error) {
	i := int(strings.TrimPrefix(r.URL.RawQuery, "i=")[0] - '0')
	body := io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("y"), d.sizes[i])))
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}, nil
}

// Parallel branches share the byte cap while they read: together they read at most one chunk each past it.
func TestRunParallelBranchesShareTheCap(t *testing.T) {
	const n, maxBytes = 16, 1 << 20
	d := newBarrierDoer(n)
	budget := NewBudget(4 * maxBytes)
	_, err := Run(context.Background(), d, reqs(t, "http://upstream", n), Options{MaxParallel: n, MaxBytes: maxBytes, Budget: budget})
	if !errors.Is(err, ErrMergeTooLarge) {
		t.Fatalf("want ErrMergeTooLarge, got %v", err)
	}
	if got, limit := d.read.Load(), int64(maxBytes+n*readChunk); got > limit {
		t.Fatalf("branches read %d bytes, want at most %d", got, limit)
	}
	if got := budget.InUse(); got != 0 {
		t.Fatalf("a failed Run must release its budget, %d bytes still reserved", got)
	}
}

// A total just below the cap is still merged, and a total one byte above it is not.
func TestRunTotalJustBelowCap(t *testing.T) {
	const n, maxBytes = 16, 1 << 20
	sizes := make([]int, n)
	for i := range sizes {
		sizes[i] = maxBytes / n
	}
	sizes[0]-- // total is maxBytes - 1
	budget := NewBudget(maxBytes)
	out, err := Run(context.Background(), sizedDoer{sizes}, reqs(t, "http://upstream", n), Options{MaxParallel: n, MaxBytes: maxBytes, Budget: budget})
	if err != nil {
		t.Fatalf("total below the cap must merge: %v", err)
	}
	for i, o := range out {
		if len(o.Body) != sizes[i] {
			t.Fatalf("body %d has %d bytes, want %d", i, len(o.Body), sizes[i])
		}
		if int64(cap(o.Body)) > int64(maxBytes)+readChunk {
			t.Fatalf("body %d has capacity %d, above the cap plus one chunk", i, cap(o.Body))
		}
	}
	if got := budget.InUse(); got != maxBytes-1 {
		t.Fatalf("budget in use = %d, want %d until release", got, maxBytes-1)
	}
	budget.Release(out)
	if got := budget.InUse(); got != 0 {
		t.Fatalf("budget in use after release = %d", got)
	}

	sizes[0] += 2 // total is maxBytes + 1
	if _, err := Run(context.Background(), sizedDoer{sizes}, reqs(t, "http://upstream", n), Options{MaxParallel: n, MaxBytes: maxBytes}); !errors.Is(err, ErrMergeTooLarge) {
		t.Fatalf("total above the cap: want ErrMergeTooLarge, got %v", err)
	}
}

// Concurrent calls share the process-wide budget: when it is full, a call fails with ErrBudgetExhausted
// and the budget is never above its limit.
func TestRunGlobalBudget(t *testing.T) {
	const n, size, calls = 4, 64 << 10, 8
	budget := NewBudget(2 * n * size) // room for two calls
	sizes := []int{size, size, size, size}

	// Two calls hold their bodies, so the budget is full.
	var held [][]Response
	for range 2 {
		out, err := Run(context.Background(), sizedDoer{sizes}, reqs(t, "http://upstream", n), Options{MaxParallel: n, MaxBytes: n * size, Budget: budget})
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, out)
	}

	var wg sync.WaitGroup
	errs := make([]error, calls)
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = Run(context.Background(), sizedDoer{sizes}, reqs(t, "http://upstream", n), Options{MaxParallel: n, MaxBytes: n * size, Budget: budget})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, ErrBudgetExhausted) {
			t.Errorf("call %d: want ErrBudgetExhausted, got %v", i, err)
		}
	}
	if got := budget.InUse(); got != 2*n*size {
		t.Fatalf("failed calls must release their bytes: in use = %d", got)
	}

	// After one release, a call succeeds again.
	budget.Release(held[0])
	out, err := Run(context.Background(), sizedDoer{sizes}, reqs(t, "http://upstream", n), Options{MaxParallel: n, MaxBytes: n * size, Budget: budget})
	if err != nil {
		t.Fatalf("call after the release: %v", err)
	}
	budget.Release(out)

	// Concurrent calls succeed or fail closed, and the budget is never above its limit.
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := Run(context.Background(), sizedDoer{sizes}, reqs(t, "http://upstream", n), Options{MaxParallel: n, MaxBytes: n * size, Budget: budget})
			if got := budget.InUse(); got > budget.limit {
				t.Errorf("budget in use %d is above the limit %d", got, budget.limit)
			}
			switch {
			case err == nil:
				budget.Release(out)
			case !errors.Is(err, ErrBudgetExhausted):
				t.Errorf("call %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	budget.Release(held[1])
	if got := budget.InUse(); got != 0 {
		t.Fatalf("budget in use at the end = %d", got)
	}
}

// A nil budget sets no limit, and a budget below 1 byte allows no body.
func TestBudgetEdges(t *testing.T) {
	var nilBudget *Budget
	if !nilBudget.reserve(1<<40) || nilBudget.InUse() != 0 {
		t.Fatal("nil budget must allow everything")
	}
	nilBudget.Release([]Response{{Body: []byte("x")}})
	if NewBudget(0).reserve(1) {
		t.Fatal("zero budget must allow no bytes")
	}
	b := NewBudget(10)
	if !b.reserve(10) || b.reserve(1) {
		t.Fatal("budget must allow exactly its limit")
	}
}
