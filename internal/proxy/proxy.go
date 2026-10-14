package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

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
}

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
	resps, err := fanout.Run(ctx, &http.Client{Transport: p.Transport}, reqs, fanout.Options{
		MaxParallel: p.Limits.MaxFanout,
		MaxBytes:    p.Limits.MaxMergeBytes,
	})
	if err != nil {
		p.logger().WarnContext(ctx, "fan-out failed", "backend", t.Backend.Name, "route", t.Endpoint.Template, "error", err.Error())
		writeFanoutError(w, r, err)
		return
	}
	if err := merge(w, r, resps); err != nil {
		p.logger().WarnContext(ctx, "merge failed", "backend", t.Backend.Name, "route", t.Endpoint.Template,
			"branches", len(resps), "error", err.Error())
		WriteError(w, r, http.StatusBadGateway, errorTypeUpstream, "cannot merge upstream responses")
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
			}
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del(TraceHeader) // ours is authoritative
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

func writeFanoutError(w http.ResponseWriter, r *http.Request, err error) {
	var ue *fanout.UpstreamError
	switch {
	case errors.Is(err, fanout.ErrMergeTooLarge):
		WriteError(w, r, http.StatusBadGateway, "merge_too_large", "merged response too large")
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
