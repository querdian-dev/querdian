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
	"time"
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
	_, lease, err := Run(context.Background(), d, reqs(t, "http://upstream", n), Options{MaxParallel: n, MaxBytes: maxBytes, Budget: budget})
	if !errors.Is(err, ErrMergeTooLarge) || lease != nil {
		t.Fatalf("want ErrMergeTooLarge and no lease, got %v %v", err, lease)
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
	out, lease, err := Run(context.Background(), sizedDoer{sizes}, reqs(t, "http://upstream", n), Options{MaxParallel: n, MaxBytes: maxBytes, Budget: budget})
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
	if got := budget.InUse(); got != maxBytes-1 || lease.Bytes() != maxBytes-1 {
		t.Fatalf("budget in use = %d, lease = %d, want %d until release", got, lease.Bytes(), maxBytes-1)
	}
	out[0].Body = out[0].Body[:0] // a change of the bodies does not change what the lease releases
	lease.Release()
	lease.Release()
	if got := budget.InUse(); got != 0 {
		t.Fatalf("budget in use after release = %d", got)
	}

	sizes[0] += 2 // total is maxBytes + 1
	if _, _, err := Run(context.Background(), sizedDoer{sizes}, reqs(t, "http://upstream", n), Options{MaxParallel: n, MaxBytes: maxBytes}); !errors.Is(err, ErrMergeTooLarge) {
		t.Fatalf("total above the cap: want ErrMergeTooLarge, got %v", err)
	}
}

// Concurrent calls share the process-wide budget. When it is full, the oldest reading call waits for space
// and each other call fails with ErrBudgetExhausted. The budget is never above its limit, and of concurrent
// calls that all need much space at least one succeeds.
func TestRunGlobalBudget(t *testing.T) {
	const n, size, calls = 4, 64 << 10, 8
	budget := NewBudget(2 * n * size) // room for two calls
	sizes := []int{size, size, size, size}
	run := func(ctx context.Context) (*Lease, error) {
		_, lease, err := Run(ctx, sizedDoer{sizes}, reqs(t, "http://upstream", n), Options{MaxParallel: n, MaxBytes: n * size, Budget: budget})
		return lease, err
	}

	// Two calls hold their bodies, so the budget is full.
	var held []*Lease
	for range 2 {
		lease, err := run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, lease)
	}

	// Of the calls that find the budget full, only the oldest waits; here it waits until its context ends.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	var wg sync.WaitGroup
	errs := make([]error, calls)
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = run(ctx)
		}()
	}
	wg.Wait()
	cancel()
	exhausted := 0
	for i, err := range errs {
		switch {
		case errors.Is(err, ErrBudgetExhausted):
			exhausted++
		case !errors.Is(err, context.DeadlineExceeded):
			t.Errorf("call %d: want ErrBudgetExhausted or the end of its context, got %v", i, err)
		}
	}
	if exhausted < calls-1 {
		t.Errorf("%d calls failed with ErrBudgetExhausted, want at least %d", exhausted, calls-1)
	}
	if got := budget.InUse(); got != 2*n*size {
		t.Fatalf("failed calls must release their bytes: in use = %d", got)
	}

	// A waiting call continues when another call releases its bytes.
	done := make(chan error, 1)
	go func() {
		lease, err := run(context.Background())
		lease.Release()
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	held[0].Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiting call: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting call did not continue after the release")
	}

	// Concurrent calls with room for one: at least one succeeds, the others succeed or fail closed.
	var ok atomic.Int32
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := run(ctx)
			if got := budget.InUse(); got > budget.limit {
				t.Errorf("budget in use %d is above the limit %d", got, budget.limit)
			}
			switch {
			case err == nil:
				ok.Add(1)
				lease.Release()
			case !errors.Is(err, ErrBudgetExhausted):
				t.Errorf("call %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() == 0 {
		t.Fatal("no call succeeded")
	}
	held[1].Release()
	if got := budget.InUse(); got != 0 {
		t.Fatalf("budget in use at the end = %d", got)
	}
}

// A lease can shrink, and grow only when the budget has space. A released lease cannot grow again.
func TestLeaseResize(t *testing.T) {
	b := NewBudget(100)
	l := &Lease{b: b}
	if !l.Resize(60) || b.InUse() != 60 {
		t.Fatalf("grow to 60: in use %d", b.InUse())
	}
	if !l.Resize(20) || b.InUse() != 20 || l.Bytes() != 20 {
		t.Fatalf("shrink to 20: in use %d", b.InUse())
	}
	if l.Resize(101) || b.InUse() != 20 {
		t.Fatalf("grow past the limit must fail and change nothing: in use %d", b.InUse())
	}
	l.Release()
	if l.Resize(10) || b.InUse() != 0 {
		t.Fatalf("a released lease must not grow: in use %d", b.InUse())
	}
	var nilLease *Lease
	nilLease.Release()
	if nilLease.Resize(1) || nilLease.Bytes() != 0 {
		t.Fatal("nil lease must hold nothing")
	}
}

// A nil budget sets no limit, and a budget below 1 byte allows no body.
func TestBudgetEdges(t *testing.T) {
	ctx := context.Background()
	var nilBudget *Budget
	if nilBudget.reserve(ctx, nilBudget.enter(), 1<<40) != nil || nilBudget.InUse() != 0 {
		t.Fatal("nil budget must allow everything")
	}
	nilBudget.release(1)
	if NewBudget(0).reserve(ctx, 0, 1) == nil {
		t.Fatal("zero budget must allow no bytes")
	}
	b := NewBudget(10)
	if b.reserve(ctx, 0, 10) != nil || !errors.Is(b.reserve(ctx, 0, 1), ErrBudgetExhausted) {
		t.Fatal("budget must allow exactly its limit")
	}
	// The oldest reading call does not wait for more than the limit.
	run := b.enter()
	defer b.leave(run)
	if !errors.Is(b.reserve(ctx, run, 11), ErrBudgetExhausted) {
		t.Fatal("a reservation above the limit must fail at once")
	}
}
