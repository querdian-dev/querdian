package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"

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
}

// Target describes where and how a plan is executed.
type Target struct {
	Backend *config.Backend
	// Endpoint is the matched route; UpstreamPath the validated path remainder, e.g. "/api/v1/query".
	Endpoint     routes.Endpoint
	UpstreamPath string
	Effective    policy.Effective
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
		p.stream(w, r, base, t, plan.Requests[0])
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
		writeFanoutError(w, r, err)
		return
	}
	if err := merge(w, r, resps); err != nil {
		WriteError(w, r, http.StatusBadGateway, errorTypeUpstream, "cannot merge upstream responses")
	}
}

func (p *Proxy) stream(w http.ResponseWriter, r *http.Request, base *url.URL, t Target, up enforce.Upstream) {
	rp := &httputil.ReverseProxy{
		Transport:     p.Transport,
		FlushInterval: -1, // stream responses as they arrive
		Rewrite: func(pr *httputil.ProxyRequest) {
			prepare(pr.Out, pr.In, base, t.UpstreamPath, up.Query, t.Backend, t.Effective)
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del(TraceHeader) // ours is authoritative
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			WriteError(w, r, http.StatusBadGateway, errorTypeUpstream, "upstream request failed")
		},
	}
	rp.ServeHTTP(w, r)
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
