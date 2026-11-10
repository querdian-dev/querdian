package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/fanout"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/routes"
)

// Merger combines the buffered responses of a fan-out plan into the client response. It must write the
// response itself and may only return an error before writing anything.
type Merger func(w http.ResponseWriter, r *http.Request, resps []fanout.Response) error

// Proxy executes enforcement plans against backends.
type Proxy struct {
	// Transport sends upstream requests; nil means http.DefaultTransport. T1.11 passes an otelhttp transport.
	Transport http.RoundTripper
	Limits    config.Limits
	// Budget is the process-wide limit on the buffered bodies of all fan-outs (limits.maxInflightMergeBytes).
	// Share one Budget between all proxies of a process. Nil sets no process-wide limit.
	Budget *fanout.Budget
	// MergeWriteTimeout replaces the computed deadline of the write of a merged response when > 0.
	MergeWriteTimeout time.Duration
	// Logger receives one warning per failed merge or fan-out, with the cause (never response bodies,
	// except a truncated upstream error text). Nil discards.
	Logger *slog.Logger
}

func (p *Proxy) logger() *slog.Logger {
	if p.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return p.Logger
}

// Target describes where and how a plan is executed.
type Target struct {
	Backend *config.Backend
	// Endpoint is the matched route; UpstreamPath the validated path remainder, e.g. "/api/v1/query".
	Endpoint     routes.Endpoint
	UpstreamPath string
	Effective    policy.Effective
	// ModifyResponse optionally rewrites the streamed response of a single-request plan (for example the
	// trace-by-ID filter). An error fails the request closed (502); the original body is never sent.
	ModifyResponse func(*http.Response) error
	// HideUpstreamErrors withholds what a failed upstream says: the body of a non-2xx response is replaced by
	// the proxy's own error document (the status is kept), a failed fan-out branch is reported without its
	// body, and a tail is closed without the upstream's close reason. Set when label values are redacted:
	// backends print whole series label sets in some query errors (Prometheus "found duplicate series",
	// Loki "pipeline error ... for series"), and the result rewriters only see successful responses.
	HideUpstreamErrors bool
	// BudgetExhausted is called when Execute rejects the request with 503 because the process-wide
	// Budget is full. The server uses it to record the reason. Nil ignores it.
	BudgetExhausted func()
}

// withheldDetail replaces the upstream error text when Target.HideUpstreamErrors is set.
const withheldDetail = "details withheld because the policy redacts label values"

// Execute runs the plan. Single-request plans are streamed through a ReverseProxy; plans with several requests
// are fanned out, buffered and passed to merge. Errors are written as JSON; Execute never returns after
// writing a response to the client.
func (p *Proxy) Execute(w http.ResponseWriter, r *http.Request, t Target, plan enforce.Plan, merge Merger) {
	base, err := url.Parse(t.Backend.URL)
	if err != nil || len(plan.Requests) == 0 {
		WriteError(w, r, http.StatusBadGateway, errorTypeUpstream, "invalid backend configuration")
		return
	}
	ctx := r.Context()
	if to := p.Limits.UpstreamTimeout.Std(); to > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, to)
		defer cancel()
		r = r.WithContext(ctx)
	}
	SetTraceHeader(w.Header(), ctx)

	if plan.Merge == enforce.MergeNone && len(plan.Requests) == 1 {
		p.stream(w, r, base, t, plan)
		return
	}
	if merge == nil {
		WriteError(w, r, http.StatusBadGateway, errorTypeUpstream, "merge not supported")
		return
	}

	reqs := make([]*http.Request, len(plan.Requests))
	for i, up := range plan.Requests {
		out, err := http.NewRequestWithContext(ctx, r.Method, base.String(), nil)
		if err != nil {
			WriteError(w, r, http.StatusBadGateway, errorTypeUpstream, "cannot build upstream request")
			return
		}
		prepare(out, r, base, t.UpstreamPath, up.Query, t.Backend, t.Effective)
		reqs[i] = out
	}
	resps, lease, err := fanout.Run(ctx, &http.Client{Transport: p.Transport}, reqs, fanout.Options{
		MaxParallel: p.Limits.MaxFanout,
		MaxBytes:    p.Limits.MaxMergeBytes,
		Budget:      p.Budget,
	})
	if err != nil {
		p.logger().WarnContext(ctx, "fan-out failed", "backend", t.Backend.Name, "route", t.Endpoint.Template, "error", err.Error())
		p.writeFanoutError(w, r, t, err)
		return
	}
	defer lease.Release()

	// The merger writes into a buffer. The client gets nothing until the merge is complete.
	out := &bufferedWriter{header: http.Header{}, limit: p.Limits.MaxMergeBytes}
	n := len(resps)
	err = merge(out, r, resps)
	resps = nil //nolint:ineffassign,wastedassign // the bodies are no longer used; the collector can free them
	if err == nil && out.overflow {
		err = errFilterTooLarge
	}
	if err != nil {
		p.logger().WarnContext(ctx, "merge failed", "backend", t.Backend.Name, "route", t.Endpoint.Template,
			"branches", n, "error", err.Error())
		if errors.Is(err, errFilterTooLarge) {
			WriteError(w, r, http.StatusBadGateway, "merge_too_large", "merged response too large")
			return
		}
		WriteError(w, r, http.StatusBadGateway, errorTypeUpstream, "cannot merge upstream responses")
		return
	}
	// From now on the budget holds only the merged output, until it is written or the write fails.
	if !lease.Resize(int64(len(out.body))) {
		p.writeFanoutError(w, r, t, fanout.ErrBudgetExhausted)
		return
	}
	p.writeBuffered(w, r, t, out)
}

// Bounds of the write of a merged response: mergeWriteBase plus one second for each MiB, at most
// mergeWriteMax. A client that reads more slowly than about 1 MiB/s fails.
const (
	mergeWriteBase = 10 * time.Second
	mergeWriteMax  = 2 * time.Minute
)

// mergeWriteTimeout returns the deadline of the write of a merged response of size bytes.
func (p *Proxy) mergeWriteTimeout(size int) time.Duration {
	if p.MergeWriteTimeout > 0 {
		return p.MergeWriteTimeout
	}
	return min(mergeWriteBase+time.Duration(size>>20)*time.Second, mergeWriteMax)
}

// writeBuffered writes a buffered merged response with a write deadline, so that a client that does not
// read cannot hold the buffer and its budget for an unbounded time. When the deadline cannot be set, the
// request fails closed.
func (p *Proxy) writeBuffered(w http.ResponseWriter, r *http.Request, t Target, out *bufferedWriter) {
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(p.mergeWriteTimeout(len(out.body)))); err != nil {
		p.logger().WarnContext(r.Context(), "cannot set write deadline", "backend", t.Backend.Name, "error", err.Error())
		WriteError(w, r, http.StatusInternalServerError, "internal", "cannot bound the response write")
		return
	}
	// The connection can serve the next request; it must not keep this deadline.
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	for k, v := range out.header {
		w.Header()[k] = v
	}
	status := out.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(out.body)))
	w.WriteHeader(status)
	_, err := w.Write(out.body)
	if err == nil {
		err = rc.Flush()
	}
	if err != nil {
		p.logger().WarnContext(r.Context(), "response write failed", "backend", t.Backend.Name, "route", t.Endpoint.Template,
			"error", err.Error())
	}
}

// DefaultFilterBytes caps response buffering for plans with a ResponseFilter that set no limit.
const DefaultFilterBytes = 16 << 20

// errFilterTooLarge marks a response that exceeded the ResponseFilter buffering cap.
var errFilterTooLarge = errors.New("response too large to filter")

func (p *Proxy) stream(w http.ResponseWriter, r *http.Request, base *url.URL, t Target, plan enforce.Plan) {
	up := plan.Requests[0]
	filter := plan.ResponseFilter
	rp := &httputil.ReverseProxy{
		Transport:     p.Transport,
		FlushInterval: -1, // stream responses as they arrive
		Rewrite: func(pr *httputil.ProxyRequest) {
			prepare(pr.Out, pr.In, base, t.UpstreamPath, up.Query, t.Backend, t.Effective)
			if filter != nil {
				pr.Out.Header.Set("Accept-Encoding", "identity") // the filter needs the plain body
				pr.Out.Header.Set("Accept", "application/json")  // and a format it can decode
			}
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			// Upgrade is never forwarded and tail uses ServeTail, so a protocol switch here would tunnel
			// raw upstream bytes past every filter.
			if resp.StatusCode == http.StatusSwitchingProtocols {
				return errors.New("unexpected protocol switch")
			}
			resp.Header.Del(TraceHeader) // ours is authoritative
			if t.HideUpstreamErrors && (resp.StatusCode < 200 || resp.StatusCode > 299) {
				hideErrorBody(resp)
			}
			if t.ModifyResponse != nil {
				if err := t.ModifyResponse(resp); err != nil {
					return err
				}
			}
			if filter == nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
				return nil
			}
			return applyFilter(resp, filter, plan.ResponseFilterMaxBytes)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, errFilterTooLarge) {
				WriteError(w, r, http.StatusBadGateway, "merge_too_large", "upstream response too large")
				return
			}
			WriteError(w, r, http.StatusBadGateway, errorTypeUpstream, "upstream request failed")
		},
	}
	rp.ServeHTTP(w, r)
}

// applyFilter replaces a 2xx response body with the filtered one. Any problem fails the request: the
// unfiltered body must never reach the client.
func applyFilter(resp *http.Response, filter func([]byte) ([]byte, error), limit int64) error {
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && ce != "identity" {
		_ = resp.Body.Close()
		return errors.New("compressed upstream response cannot be filtered")
	}
	if limit <= 0 {
		limit = DefaultFilterBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return errFilterTooLarge
	}
	out, err := filter(body)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	resp.Header.Del("Etag")
	return nil
}

// hideErrorBody replaces the body of a failed upstream response with the proxy's error document, keeping the
// status code. Nothing of the upstream body, its encoding or its trailers reaches the client.
func hideErrorBody(resp *http.Response) {
	_ = resp.Body.Close()
	tid := ""
	if resp.Request != nil {
		tid = TraceID(resp.Request.Context())
	}
	body, _ := json.Marshal(ErrorBody{Status: "error", ErrorType: errorTypeUpstream, Error: "upstream error; " + withheldDetail, TraceID: tid})
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Trailer = nil
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("X-Content-Type-Options", "nosniff")
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Etag")
}

// FilterMerger returns a merger that runs m into a buffer, passes a 2xx result through filter and writes the
// filtered body. Like ResponseFilter it fails closed: an error of m or filter, or a merged body larger than
// maxBytes, is returned before anything is written, so the unfiltered body never reaches the client. A nil
// m stays nil.
func FilterMerger(m Merger, filter func([]byte) ([]byte, error), maxBytes int64) Merger {
	if m == nil {
		return nil
	}
	if maxBytes <= 0 {
		maxBytes = DefaultFilterBytes
	}
	return func(w http.ResponseWriter, r *http.Request, resps []fanout.Response) error {
		buf := &bufferedWriter{header: http.Header{}, limit: maxBytes}
		if err := m(buf, r, resps); err != nil {
			return err
		}
		if buf.overflow {
			return errFilterTooLarge
		}
		body := buf.body
		if buf.status == 0 {
			buf.status = http.StatusOK
		}
		if buf.status >= 200 && buf.status <= 299 {
			var err error
			if body, err = filter(body); err != nil {
				return err
			}
		}
		for k, v := range buf.header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Del("Etag")
		w.WriteHeader(buf.status)
		fanout.WriteBody(w, body)
		return nil
	}
}

// bufferedWriter collects a merger's response, up to limit bytes.
type bufferedWriter struct {
	header   http.Header
	status   int
	body     []byte
	limit    int64
	overflow bool
}

func (b *bufferedWriter) Header() http.Header { return b.header }

func (b *bufferedWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	if int64(len(b.body)+len(p)) > b.limit {
		b.overflow = true
		return 0, errFilterTooLarge
	}
	b.body = append(b.body, p...)
	return len(p), nil
}

// TakeBody implements fanout.BodyTaker. When nothing was written before, it keeps body without a copy.
func (b *bufferedWriter) TakeBody(body []byte) {
	if len(b.body) > 0 || b.overflow {
		_, _ = b.Write(body)
		return
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	if int64(len(body)) > b.limit {
		b.overflow = true
		return
	}
	b.body = body
}

// writeFanoutError reports a failed fan-out. With t.HideUpstreamErrors, a branch that failed with a status is
// reported without the snippet of its body. A full budget is also reported through t.BudgetExhausted.
func (p *Proxy) writeFanoutError(w http.ResponseWriter, r *http.Request, t Target, err error) {
	var ue *fanout.UpstreamError
	switch {
	case errors.Is(err, fanout.ErrMergeTooLarge):
		WriteError(w, r, http.StatusBadGateway, "merge_too_large", "merged response too large")
	case errors.Is(err, fanout.ErrBudgetExhausted):
		if t.BudgetExhausted != nil {
			t.BudgetExhausted()
		}
		// 1 to 3 seconds, so that the clients do not all try again at the same time.
		w.Header().Set("Retry-After", strconv.Itoa(1+rand.IntN(3))) //nolint:gosec // jitter, not a secret
		WriteError(w, r, http.StatusServiceUnavailable, errorTypeUnavailable,
			"too many large responses are in progress; try again later")
	case errors.As(err, &ue) && t.HideUpstreamErrors && ue.Status != 0:
		WriteError(w, r, http.StatusBadGateway, errorTypeUpstream,
			fmt.Sprintf("upstream request %d failed: status %d; %s", ue.Index, ue.Status, withheldDetail))
	case errors.As(err, &ue):
		WriteError(w, r, http.StatusBadGateway, errorTypeUpstream, ue.Error())
	default:
		WriteError(w, r, http.StatusBadGateway, errorTypeUpstream, "upstream request failed")
	}
}

// Unavailable writes the 503 used while no snapshot is ready.
func Unavailable(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusServiceUnavailable, errorTypeUnavailable, "policy snapshot not ready")
}
