package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestLeadAfterShutdown: if the process stops before the lease callback starts, the sync loop does not
// start.
func TestLeadAfterShutdown(t *testing.T) {
	var runs atomic.Int64
	run := func(context.Context) error { runs.Add(1); return nil }
	for range 1000 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		lead(ctx, context.Background(), testLogger(), "id", run)
	}
	if n := runs.Load(); n != 0 {
		t.Fatalf("the sync loop started %d times after the shutdown", n)
	}
}

// TestLeadStopsRun: the context of run ends when the lease is lost and when the process stops.
func TestLeadStopsRun(t *testing.T) {
	for _, stopProcess := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		lctx, lcancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		started := make(chan struct{})
		go func() {
			defer close(done)
			lead(ctx, lctx, testLogger(), "id", func(rctx context.Context) error {
				close(started)
				<-rctx.Done()
				return nil
			})
		}()
		<-started
		if stopProcess {
			cancel()
		} else {
			lcancel()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("stop process %v: the sync loop did not stop", stopProcess)
		}
		cancel()
		lcancel()
	}
}
