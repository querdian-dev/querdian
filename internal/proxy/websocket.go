package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/fanout"
)

// Defaults for TailOptions.
const (
	defaultTailDialTimeout  = 10 * time.Second
	defaultTailWriteTimeout = 10 * time.Second
	defaultTailFrameBytes   = 4 << 20
	// maxCloseReason is the room left in a close frame (125 bytes) after the two status bytes.
	maxCloseReason = 123
)

// TailOptions tune ServeTail. The zero value selects safe defaults.
type TailOptions struct {
	// OriginPatterns lists extra Origin host patterns accepted from browser clients (path.Match syntax).
	// Without it only same-host and Origin-less (non-browser) clients are accepted.
	OriginPatterns []string
	// DedupeSize bounds the dedupe LRU (default fanout.DefaultTailDedupeSize).
	DedupeSize int
	// MaxFrameBytes bounds a single upstream frame (default 4 MiB).
	MaxFrameBytes int64
	// DialTimeout bounds the upstream handshakes (default 10s).
	DialTimeout time.Duration
	// WriteTimeout bounds one write to the client (default 10s).
	WriteTimeout time.Duration
}

// ServeTail serves the Loki tail endpoint (PLAN §7.3): it opens one upstream WebSocket per plan request
// (query already rewritten by the enforcer), accepts the client upgrade, and forwards the merged, deduplicated
// frames (fanout.TailMerger) until either side closes or fails. All sockets are closed on any error: a client
// close closes every upstream; an upstream close or invalid frame closes the client with a reason.
//
// Upstream handshakes carry no client headers: Authorization, Cookie and tenant headers never reach the
// backend, the tenant header is set from Target.Effective, and only traceparent is propagated (PLAN §9).
// Failures before the client upgrade are written as the usual JSON error (502 upstream). The Server must call
// ServeTail instead of Execute for endpoints with routes.Endpoint.Websocket set. ServeTail returns when the
// tail has ended and all goroutines have exited.
func (p *Proxy) ServeTail(w http.ResponseWriter, r *http.Request, t Target, plan enforce.Plan, opt TailOptions) {
	opt = opt.withDefaults()
	base, err := url.Parse(t.Backend.URL)
	if err != nil || len(plan.Requests) == 0 {
		WriteError(w, r, http.StatusBadGateway, errorTypeUpstream, "invalid backend configuration")
		return
	}
	SetTraceHeader(w.Header(), r.Context())

	ups, err := p.dialAll(r, base, t, plan, opt)
	if err != nil {
		WriteError(w, r, http.StatusBadGateway, errorTypeUpstream, "upstream websocket failed")
		return
	}
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: opt.OriginPatterns})
	if err != nil {
		// Accept has already written the HTTP error response.
		closeAll(nil, 0, "", ups)
		return
	}
	runTail(r.Context(), client, ups, opt)
}

func (o TailOptions) withDefaults() TailOptions {
	if o.MaxFrameBytes < 1 {
		o.MaxFrameBytes = defaultTailFrameBytes
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = defaultTailDialTimeout
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = defaultTailWriteTimeout
	}
	return o
}

// dialAll opens every upstream socket concurrently; on any failure the ones already open are closed.
func (p *Proxy) dialAll(r *http.Request, base *url.URL, t Target, plan enforce.Plan, opt TailOptions) ([]*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(r.Context(), opt.DialTimeout)
	defer cancel()
	client := &http.Client{Transport: p.Transport}

	conns := make([]*websocket.Conn, len(plan.Requests))
	errs := make([]error, len(plan.Requests))
	var wg sync.WaitGroup
	for i, up := range plan.Requests {
		out := &http.Request{Method: http.MethodGet, Header: http.Header{}}
		prepare(out, r, base, t.UpstreamPath, up.Query, t.Backend, t.Effective)
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, resp, err := websocket.Dial(ctx, out.URL.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: out.Header})
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if err != nil {
				errs[i] = err
				return
			}
			c.SetReadLimit(opt.MaxFrameBytes)
			conns[i] = c
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		for _, c := range conns {
			if c != nil {
				_ = c.CloseNow()
			}
		}
		return nil, err
	}
	return conns, nil
}

// runTail pumps frames until the tail ends, then closes every socket and waits for all goroutines.
func runTail(parent context.Context, client *websocket.Conn, ups []*websocket.Conn, opt TailOptions) {
	// The I/O context is only cancelled after the close handshakes: cancelling a coder/websocket read
	// context aborts the connection, which would lose the close reason.
	ioCtx, cancelIO := context.WithCancel(parent)
	defer cancelIO()
	clientGone := client.CloseRead(ioCtx)

	var (
		merger   = fanout.NewTailMerger(opt.DedupeSize)
		stop     = make(chan struct{})
		once     sync.Once
		code     = websocket.StatusNormalClosure
		reason   string
		writeMu  sync.Mutex
		wg       sync.WaitGroup
		failWith = func(c websocket.StatusCode, why string) {
			once.Do(func() {
				code, reason = c, why
				close(stop)
			})
		}
	)

	for _, u := range ups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				_, data, err := u.Read(ioCtx)
				if err != nil {
					if ioCtx.Err() == nil {
						failWith(closeFor(err))
					}
					return
				}
				frame, ok, err := merger.Merge(data)
				if err != nil {
					failWith(websocket.StatusInternalError, "invalid upstream frame")
					return
				}
				if !ok {
					continue
				}
				writeMu.Lock()
				wctx, cancel := context.WithTimeout(ioCtx, opt.WriteTimeout)
				err = client.Write(wctx, websocket.MessageText, frame)
				cancel()
				writeMu.Unlock()
				if err != nil {
					// Client gone or too slow; the watcher below handles a clean client close.
					if ioCtx.Err() == nil {
						failWith(websocket.StatusPolicyViolation, "client write failed")
					}
					return
				}
			}
		}()
	}

	select {
	case <-clientGone.Done(): // client closed or misbehaved: close the upstreams
		once.Do(func() { close(stop) })
	case <-stop:
	case <-parent.Done():
		failWith(websocket.StatusGoingAway, "server shutting down")
	}
	// code and reason are final once stop is closed (once.Do orders the writes before the close).
	closeAll(client, code, reason, ups)
	cancelIO()
	wg.Wait()
}

// closeAll closes the client (when non-nil) with code and reason and every upstream normally, concurrently.
func closeAll(client *websocket.Conn, code websocket.StatusCode, reason string, ups []*websocket.Conn) {
	var wg sync.WaitGroup
	if client != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = client.Close(code, truncateReason(reason))
		}()
	}
	for _, u := range ups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = u.Close(websocket.StatusNormalClosure, "")
		}()
	}
	wg.Wait()
}

// closeFor maps an upstream read error to the close code and reason sent to the client.
func closeFor(err error) (websocket.StatusCode, string) {
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		return websocket.StatusInternalError, "upstream connection failed"
	}
	c := ce.Code
	switch {
	case c >= 1000 && c <= 1003, c >= 1007 && c <= 1011, c >= 3000 && c <= 4999:
	default:
		c = websocket.StatusInternalError
	}
	why := "upstream closed"
	if ce.Reason != "" {
		why += ": " + ce.Reason
	}
	return c, why
}

// truncateReason fits s into a close frame without splitting a UTF-8 sequence.
func truncateReason(s string) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= maxCloseReason {
		return s
	}
	s = s[:maxCloseReason]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
