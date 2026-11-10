package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/fanout"
	"github.com/querdian-dev/querdian/internal/routes"
)

// fixedDoer answers each request with size bytes.
type fixedDoer int

func (d fixedDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(make([]byte, d)))}, nil
}

// stallBody returns size bytes, then waits until release is closed, then ends.
type stallBody struct {
	left    int
	release <-chan struct{}
}

func (b *stallBody) Read(p []byte) (int, error) {
	if b.left == 0 {
		<-b.release
		return 0, io.EOF
	}
	n := min(len(p), b.left)
	b.left -= n
	return n, nil
}

func (b *stallBody) Close() error { return nil }

type stallDoer struct {
	size    int
	release <-chan struct{}
}

func (d stallDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &stallBody{left: d.size, release: d.release}}, nil
}

// holdBudget starts a fan-out that reserves size bytes of b and then keeps on reading. The returned function
// ends it and releases its bytes.
func holdBudget(t *testing.T, b *fanout.Budget, size int) func() {
	t.Helper()
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r, _ := http.NewRequest(http.MethodGet, "http://older/", nil)
		_, lease, err := fanout.Run(context.Background(), stallDoer{size: size, release: release}, []*http.Request{r},
			fanout.Options{MaxParallel: 1, MaxBytes: int64(size), Budget: b})
		if err != nil {
			t.Error(err)
		}
		lease.Release()
	}()
	for deadline := time.Now().Add(5 * time.Second); b.InUse() < int64(size); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the older fan-out did not reserve its bytes")
		}
	}
	return func() {
		close(release)
		<-done
	}
}

// fanoutTarget returns a Target for an upstream that answers each request with body.
func fanoutTarget(t *testing.T, body string) Target {
	t.Helper()
	up := httptest.NewServer((&recorder{}).handler(200, body))
	t.Cleanup(up.Close)
	ep := routes.Lookup(config.BackendPrometheus, "/api/v1/query", http.MethodGet)
	return Target{Backend: backend(t, up.URL, ""), Endpoint: ep, UpstreamPath: "/api/v1/query"}
}

// A merged response is written with a write deadline, and the budget holds only the merged output while it
// is written.
func TestFanoutWriteHasDeadline(t *testing.T) {
	p := newProxy()
	p.Budget = fanout.NewBudget(1 << 20)
	target := fanoutTarget(t, strings.Repeat("x", 400))
	w := newRecorder()
	p.Execute(w, clientRequest(http.MethodGet, "/prom/api/v1/query?query=x", ""), target, fanPlan(2),
		func(w http.ResponseWriter, _ *http.Request, _ []fanout.Response) error {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"n":1}`))
			return nil
		})
	if w.Code != 200 || w.Body.String() != `{"n":1}` || w.Header().Get("Content-Length") != "7" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("got %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	// One deadline for the write, one to clear it.
	if w.deadlines != 2 || p.Budget.InUse() != 0 {
		t.Fatalf("deadlines set %d, budget in use %d", w.deadlines, p.Budget.InUse())
	}
}

// Without a write deadline a client could hold the budget for ever, so the request fails closed.
func TestFanoutWriteWithoutDeadlineFailsClosed(t *testing.T) {
	p := newProxy()
	p.Budget = fanout.NewBudget(1 << 20)
	w := httptest.NewRecorder() // does not support write deadlines
	p.Execute(w, clientRequest(http.MethodGet, "/prom/api/v1/query?query=x", ""), fanoutTarget(t, "secret-body"), fanPlan(2), concat)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "secret-body") || p.Budget.InUse() != 0 {
		t.Fatalf("got %d %q, budget in use %d", w.Code, w.Body.String(), p.Budget.InUse())
	}
}

// When the merged output is larger than the bodies and the budget has no space for the difference, the
// request fails closed with 503.
func TestFanoutMergedOutputOverBudget(t *testing.T) {
	p := newProxy()
	p.Budget = fanout.NewBudget(1000)
	var held *fanout.Lease
	var exhausted int
	target := fanoutTarget(t, strings.Repeat("x", 100))
	target.Rejected = func(reason string) {
		if reason == ReasonMergeBudgetExhausted {
			exhausted++
		}
	}
	w := newRecorder()
	p.Execute(w, clientRequest(http.MethodGet, "/prom/api/v1/query?query=x", ""), target, fanPlan(2),
		func(w http.ResponseWriter, _ *http.Request, _ []fanout.Response) error {
			// Another request takes the rest of the budget while this one merges.
			r, _ := http.NewRequest(http.MethodGet, "http://other/", nil)
			var err error
			if _, held, err = fanout.Run(context.Background(), fixedDoer(800), []*http.Request{r}, fanout.Options{MaxParallel: 1, MaxBytes: 800, Budget: p.Budget}); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(strings.Repeat("y", 900)))
			return nil
		})
	var body ErrorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusServiceUnavailable || body.ErrorType != "unavailable" || exhausted != 1 {
		t.Fatalf("got %d %s, callback calls %d", w.Code, w.Body.String(), exhausted)
	}
	if got := p.Budget.InUse(); got != 800 {
		t.Fatalf("budget in use = %d, want only the 800 bytes of the other request", got)
	}
	held.Release()
}

// A client that does not read its merged response cannot hold the budget after the write deadline, and
// other requests succeed after it.
func TestFanoutClientThatDoesNotRead(t *testing.T) {
	const branch = 16 << 20 // two branches: 32 MiB, far more than the socket buffers hold
	p := newProxy()
	p.Limits.MaxMergeBytes = 2 * branch
	p.Budget = fanout.NewBudget(2 * branch)
	p.MergeWriteTimeout = 300 * time.Millisecond
	target := fanoutTarget(t, strings.Repeat("x", branch))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.Execute(w, r, target, fanPlan(2), concat)
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.(*net.TCPConn).SetReadBuffer(4 << 10)
	if _, err := conn.Write([]byte("GET /api/v1/query?query=x HTTP/1.1\r\nHost: q\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// Read only the status line, then stop.
	if line, err := bufio.NewReaderSize(conn, 64).ReadString('\n'); err != nil || !strings.Contains(line, "200") {
		t.Fatalf("status line %q: %v", line, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for p.Budget.InUse() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("budget still holds %d bytes after the write deadline", p.Budget.InUse())
		}
		time.Sleep(10 * time.Millisecond)
	}

	resp, err := srv.Client().Get(srv.URL + "/api/v1/query?query=y")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	n, _ := io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 || n != 2*branch || p.Budget.InUse() != 0 {
		t.Fatalf("other request: %d, %d bytes, budget in use %d", resp.StatusCode, n, p.Budget.InUse())
	}
}

// probeWriter records the bytes in use of a budget when the response is written, and fails the write.
type probeWriter struct {
	header http.Header
	budget *fanout.Budget
	status int
	inUse  []int64
}

func (p *probeWriter) Header() http.Header  { return p.header }
func (p *probeWriter) WriteHeader(code int) { p.status = code }
func (p *probeWriter) Write([]byte) (int, error) {
	p.inUse = append(p.inUse, p.budget.InUse())
	return 0, io.ErrClosedPipe
}

// deadlineProbeWriter is a probeWriter that accepts a write deadline.
type deadlineProbeWriter struct{ probeWriter }

func (*deadlineProbeWriter) SetWriteDeadline(time.Time) error { return nil }

// Each error response of a fan-out is written after the budget is released, so a client that does not
// read an error response holds no budget. The server gets the fixed reason of a rejection by the proxy.
func TestFanoutErrorResponsesHoldNoBudget(t *testing.T) {
	failMerge := func(http.ResponseWriter, *http.Request, []fanout.Response) error { return io.ErrUnexpectedEOF }
	tooLarge := func(w http.ResponseWriter, _ *http.Request, _ []fanout.Response) error {
		_, _ = w.Write(make([]byte, 2000))
		return nil
	}
	for _, tc := range []struct {
		name       string
		merge      Merger
		deadline   bool
		wantStatus int
		wantReason string
	}{
		{"merge error", failMerge, true, http.StatusBadGateway, ""},
		{"merged output too large", tooLarge, true, http.StatusBadGateway, ""},
		{"no write deadline", concat, false, http.StatusInternalServerError, ReasonWriteNotBounded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProxy()
			p.Limits.MaxMergeBytes = 1000
			p.Budget = fanout.NewBudget(1000)
			var reason string
			target := fanoutTarget(t, strings.Repeat("x", 400))
			target.Rejected = func(r string) { reason = r }
			probe := &deadlineProbeWriter{probeWriter{header: http.Header{}, budget: p.Budget}}
			var w http.ResponseWriter = probe
			if !tc.deadline {
				w = &probe.probeWriter
			}
			p.Execute(w, clientRequest(http.MethodGet, "/prom/api/v1/query?query=x", ""), target, fanPlan(2), tc.merge)
			if probe.status != tc.wantStatus || reason != tc.wantReason {
				t.Fatalf("status %d, reason %q; want %d, %q", probe.status, reason, tc.wantStatus, tc.wantReason)
			}
			if len(probe.inUse) == 0 {
				t.Fatal("no error response was written")
			}
			for _, n := range probe.inUse {
				if n != 0 {
					t.Fatalf("budget held %d bytes while the error response was written", n)
				}
			}
			if got := p.Budget.InUse(); got != 0 {
				t.Fatalf("budget in use after the request = %d", got)
			}
		})
	}
}
