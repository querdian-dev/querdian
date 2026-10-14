package fanout

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func reqs(t *testing.T, url string, n int) []*http.Request {
	t.Helper()
	out := make([]*http.Request, n)
	for i := range out {
		r, err := http.NewRequest(http.MethodGet, url+"/?i="+string(rune('0'+i)), nil)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = r
	}
	return out
}

func TestRunOrderAndAcceptHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		time.Sleep(time.Duration(5-len(r.URL.RawQuery)%5) * time.Millisecond)
		_, _ = w.Write([]byte("r" + string(rune(r.URL.Query().Get("i")[0])))) //nolint:gosec // test echo
	}))
	defer srv.Close()
	rs := reqs(t, srv.URL, 5)
	for _, r := range rs {
		r.Header.Set("Accept", "text/plain")
	}
	out, err := Run(context.Background(), srv.Client(), rs, Options{MaxParallel: 5, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for i, o := range out {
		if want := "r" + string(rune('0'+i)); string(o.Body) != want || o.Status != 200 {
			t.Errorf("out[%d] = %q (%d), want %q", i, o.Body, o.Status, want)
		}
	}
}

func TestRunBoundedParallelism(t *testing.T) {
	var cur, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		n := cur.Add(1)
		for {
			m := peak.Load()
			if n <= m || peak.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		cur.Add(-1)
	}))
	defer srv.Close()
	if _, err := Run(context.Background(), srv.Client(), reqs(t, srv.URL, 8), Options{MaxParallel: 2, MaxBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got > 2 || got < 1 {
		t.Fatalf("max concurrency = %d, want <= 2", got)
	}
}

func TestRunOneFailureCancelsOthers(t *testing.T) {
	cancelled := make(chan struct{})
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("i") == "0" {
			<-started // make sure the slow branch is running before failing
			http.Error(w, "boom: bad query", http.StatusBadRequest)
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(10 * time.Second):
		}
	}))
	defer srv.Close()
	rs := reqs(t, srv.URL, 2)
	_, err := Run(context.Background(), srv.Client(), rs, Options{MaxParallel: 2, MaxBytes: 1 << 20})
	var ue *UpstreamError
	if !errors.As(err, &ue) || ue.Status != http.StatusBadRequest || !strings.Contains(ue.Detail, "boom") {
		t.Fatalf("want UpstreamError 400, got %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("other branch was not cancelled")
	}
}

func TestRunTransportErrorDoesNotLeakURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused
	r, _ := http.NewRequest(http.MethodGet, url+"/secret-path?token=1", nil)
	_, err := Run(context.Background(), http.DefaultClient, []*http.Request{r}, Options{MaxParallel: 1, MaxBytes: 10})
	var ue *UpstreamError
	if !errors.As(err, &ue) || strings.Contains(ue.Error(), "secret-path") || strings.Contains(ue.Error(), "token") {
		t.Fatalf("error leaks request URL or is wrong type: %v", err)
	}
}

func TestRunByteCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 600))) //nolint:gosec // test data
	}))
	defer srv.Close()
	_, err := Run(context.Background(), srv.Client(), reqs(t, srv.URL, 2), Options{MaxParallel: 1, MaxBytes: 1000})
	if !errors.Is(err, ErrMergeTooLarge) {
		t.Fatalf("want ErrMergeTooLarge, got %v", err)
	}
	// A single body over the cap fails too, and exactly at the cap passes.
	if _, err := Run(context.Background(), srv.Client(), reqs(t, srv.URL, 1), Options{MaxParallel: 1, MaxBytes: 599}); !errors.Is(err, ErrMergeTooLarge) {
		t.Fatalf("single body over cap: %v", err)
	}
	if _, err := Run(context.Background(), srv.Client(), reqs(t, srv.URL, 1), Options{MaxParallel: 1, MaxBytes: 600}); err != nil {
		t.Fatalf("exactly at cap must pass: %v", err)
	}
}

func TestRunParentCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Run(ctx, srv.Client(), reqs(t, srv.URL, 3), Options{MaxParallel: 1, MaxBytes: 10})
	if err == nil {
		t.Fatal("want error on parent cancel")
	}
}
