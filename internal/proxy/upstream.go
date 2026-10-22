package proxy

import (
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/propagation"

	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/policy"
)

// TenantValue renders the tenant header value: a single tenant, or t1|t2|... for federation.
func TenantValue(eff policy.Effective) string { return strings.Join(eff.Tenants, "|") }

// prepare rewrites out (a copy of the client request) into the upstream request: target URL, parameters,
// headers and body. Only the backend's forwardHeaders survive from the client (PLAN §9); config validation keeps
// credentials, tenant, trace and hop-by-hop headers out of that list. Only the proxy sets the tenant header;
// trace context is propagated as W3C tracecontext only.
func prepare(out, in *http.Request, base *url.URL, upstreamPath string, params url.Values, b *config.Backend, eff policy.Effective) {
	out.URL = &url.URL{
		Scheme: base.Scheme,
		Host:   base.Host,
		Path:   strings.TrimSuffix(base.Path, "/") + upstreamPath,
	}
	out.Host = base.Host

	for k := range out.Header {
		if !slices.Contains(b.ForwardHeaders, http.CanonicalHeaderKey(k)) {
			delete(out.Header, k)
		}
	}
	if b.TenantHeader != "" {
		out.Header.Del(b.TenantHeader)
		if v := TenantValue(eff); v != "" {
			out.Header.Set(b.TenantHeader, v)
		}
	}
	propagation.TraceContext{}.Inject(in.Context(), propagation.HeaderCarrier(out.Header))

	enc := params.Encode()
	switch in.Method {
	case http.MethodPost:
		out.URL.RawQuery = ""
		out.Body = io.NopCloser(strings.NewReader(enc))
		out.ContentLength = int64(len(enc))
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(enc)), nil }
		out.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	default:
		out.URL.RawQuery = enc
		out.Body = nil
		out.GetBody = nil
		out.ContentLength = 0
		out.Header.Del("Content-Type")
	}
}
