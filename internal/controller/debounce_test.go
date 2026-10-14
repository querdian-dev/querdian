package controller

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunDebouncedCoalescesBursts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	trigger := make(chan struct{}, 16)
	calls := make(chan int, 4)
	go runDebounced(ctx, trigger, 60*time.Millisecond, func(n int) { calls <- n })

	for i := 0; i < 5; i++ {
		trigger <- struct{}{}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case n := <-calls:
		if n != 5 {
			t.Fatalf("coalesced = %d, want 5", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fn not called")
	}
	select {
	case n := <-calls:
		t.Fatalf("unexpected second call (%d)", n)
	case <-time.After(150 * time.Millisecond):
	}

	trigger <- struct{}{} // a later burst triggers again
	select {
	case n := <-calls:
		if n != 1 {
			t.Fatalf("coalesced = %d, want 1", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fn not called for second burst")
	}
}

func TestRunDebouncedStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	trigger := make(chan struct{}, 1)
	var called atomic.Bool
	done := make(chan struct{})
	go func() {
		runDebounced(ctx, trigger, time.Hour, func(int) { called.Store(true) })
		close(done)
	}()
	trigger <- struct{}{}
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not stop")
	}
	if called.Load() {
		t.Fatal("fn must not run after cancel")
	}
}

func TestEqualStringMap(t *testing.T) {
	if !equalStringMap(map[string]string{"a": "1"}, map[string]string{"a": "1"}) || equalStringMap(map[string]string{"a": "1"}, map[string]string{"a": "2"}) {
		t.Fatal("equalStringMap broken")
	}
}
