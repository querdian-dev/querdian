// Package config loads and validates Querdian's static configuration file and command line flags.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// BackendType is the kind of upstream a backend points at.
type BackendType string

// Supported backend types.
const (
	BackendPrometheus BackendType = "prometheus"
	BackendLoki       BackendType = "loki"
	BackendTempo      BackendType = "tempo"
)

// TenantFederation says how cross-tenant unions are expressed on a backend (PLAN §6.4).
type TenantFederation string

// Supported tenant federation modes.
const (
	// FederationNone means the backend cannot filter on __tenant_id__.
	FederationNone TenantFederation = "none"
	// FederationTenantIDLabel means the backend supports a __tenant_id__ matcher.
	FederationTenantIDLabel TenantFederation = "tenantIdLabel"
)

// TraceByID controls how Tempo trace-by-ID requests are handled.
type TraceByID string

// Supported trace-by-ID modes.
const (
	// TraceByIDDeny rejects trace-by-ID requests.
	TraceByIDDeny TraceByID = "deny"
	// TraceByIDFilter filters spans of the returned trace by policy.
	TraceByIDFilter TraceByID = "filter"
)

// Config is the static configuration file.
type Config struct {
	Auth     AuthConfig `json:"auth"`
	Backends []Backend  `json:"backends"`
	Limits   Limits     `json:"limits"`
}

// AuthConfig configures OIDC verification (PLAN §10).
type AuthConfig struct {
	Issuer    string   `json:"issuer"`
	Audiences []string `json:"audiences"`
	// CAFile is an optional PEM bundle trusted for the issuer.
	CAFile string       `json:"caFile"`
	Claims ClaimsConfig `json:"claims"`
}

// ClaimsConfig names the token claims carrying identity; dotted paths are allowed.
type ClaimsConfig struct {
	Username string `json:"username"`
	Groups   string `json:"groups"`
}

// Backend is one upstream.
type Backend struct {
	Name             string           `json:"name"`
	Type             BackendType      `json:"type"`
	URL              string           `json:"url"`
	PathPrefix       string           `json:"pathPrefix"`
	NamespaceLabel   string           `json:"namespaceLabel"`
	TenantHeader     string           `json:"tenantHeader"`
	TenantFederation TenantFederation `json:"tenantFederation"`
	TraceByID        TraceByID        `json:"traceByID"`
	// ForwardHeaders lists the client headers passed to the backend, in canonical form; all others are
	// dropped (PLAN §9). Absent means DefaultForwardHeaders for the type; an empty list forwards none.
	ForwardHeaders []string `json:"forwardHeaders"`
}

// DefaultForwardHeaders returns the client headers forwarded when forwardHeaders is absent: content
// negotiation and User-Agent, plus Loki's categorize-labels flag, whose encoding the merges and redaction
// handle.
func DefaultForwardHeaders(t BackendType) []string {
	h := []string{"Accept", "Accept-Encoding", "User-Agent"}
	if t == BackendLoki {
		h = append(h, "X-Loki-Response-Encoding-Flags")
	}
	return h
}

// reservedHeaders may never be listed in forwardHeaders: credentials, tenant selection, headers the proxy
// sets itself and hop-by-hop headers. The backend's own tenantHeader is rejected as well.
var reservedHeaders = func() map[string]bool {
	m := map[string]bool{}
	for _, h := range []string{
		"Authorization", "Proxy-Authorization", "Cookie",
		"X-Scope-OrgID", "THANOS-TENANT",
		"Traceparent", "Tracestate", "Baggage",
		"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port", "X-Real-IP",
		"Connection", "Upgrade", "Keep-Alive", "Proxy-Connection", "TE", "Trailer", "Transfer-Encoding",
		"Host", "Content-Type", "Content-Length",
	} {
		m[textproto.CanonicalMIMEHeaderKey(h)] = true
	}
	return m
}()

// Limits bounds per-request resource usage.
type Limits struct {
	MaxPolicyTerms  int      `json:"maxPolicyTerms"`
	MaxFanout       int      `json:"maxFanout"`
	MaxMergeBytes   int64    `json:"maxMergeBytes"`
	UpstreamTimeout Duration `json:"upstreamTimeout"`
}

// Duration is a time.Duration that unmarshals from strings such as "5m".
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler (sigs.k8s.io/yaml converts YAML to JSON first).
func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %s: %w", b, err)
	}
	*d = Duration(v)
	return nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Defaults for optional settings.
const (
	DefaultUsernameClaim   = "preferred_username"
	DefaultGroupsClaim     = "groups"
	DefaultMaxPolicyTerms  = 16
	DefaultMaxFanout       = 16
	DefaultMaxMergeBytes   = 64 << 20
	DefaultUpstreamTimeout = 5 * time.Minute
)

// Load reads, decodes (unknown fields are an error), defaults and validates the file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path comes from an operator-controlled flag
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(b)
}

// Parse decodes, defaults and validates configuration bytes.
func Parse(b []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(bytes.TrimSpace(b), &c); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Auth.Claims.Username == "" {
		c.Auth.Claims.Username = DefaultUsernameClaim
	}
	if c.Auth.Claims.Groups == "" {
		c.Auth.Claims.Groups = DefaultGroupsClaim
	}
	if c.Limits.MaxPolicyTerms == 0 {
		c.Limits.MaxPolicyTerms = DefaultMaxPolicyTerms
	}
	if c.Limits.MaxFanout == 0 {
		c.Limits.MaxFanout = DefaultMaxFanout
	}
	if c.Limits.MaxMergeBytes == 0 {
		c.Limits.MaxMergeBytes = DefaultMaxMergeBytes
	}
	if c.Limits.UpstreamTimeout == 0 {
		c.Limits.UpstreamTimeout = Duration(DefaultUpstreamTimeout)
	}
	for i := range c.Backends {
		b := &c.Backends[i]
		if b.TenantFederation == "" {
			b.TenantFederation = FederationNone
		}
		// Fail closed: Tempo trace-by-ID is denied unless explicitly set to filter.
		if b.Type == BackendTempo && b.TraceByID == "" {
			b.TraceByID = TraceByIDDeny
		}
		if b.ForwardHeaders == nil {
			b.ForwardHeaders = DefaultForwardHeaders(b.Type)
		}
		for j, h := range b.ForwardHeaders {
			b.ForwardHeaders[j] = textproto.CanonicalMIMEHeaderKey(h)
		}
	}
}

// Validate checks every rule of PLAN §5.1 and returns all violations joined.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if u, err := url.Parse(c.Auth.Issuer); err != nil || c.Auth.Issuer == "" || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		add("auth.issuer must be an http(s) URL, got %q", c.Auth.Issuer)
	}
	if len(c.Auth.Audiences) == 0 {
		add("auth.audiences must not be empty")
	}
	for _, a := range c.Auth.Audiences {
		if a == "" {
			add("auth.audiences must not contain empty values")
		}
	}
	if c.Limits.MaxPolicyTerms < 1 || c.Limits.MaxFanout < 1 || c.Limits.MaxMergeBytes < 1 || c.Limits.UpstreamTimeout <= 0 {
		add("limits must be positive")
	}
	if len(c.Backends) == 0 {
		add("at least one backend is required")
	}

	names := map[string]bool{}
	for i := range c.Backends {
		b := &c.Backends[i]
		p := fmt.Sprintf("backends[%d]", i)
		if b.Name == "" {
			add("%s.name must not be empty", p)
		} else if names[b.Name] {
			add("%s.name %q is not unique", p, b.Name)
		}
		names[b.Name] = true

		switch b.Type {
		case BackendPrometheus, BackendLoki, BackendTempo:
		default:
			add("%s.type %q unknown (prometheus|loki|tempo)", p, b.Type)
		}
		if u, err := url.Parse(b.URL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			add("%s.url must be an http(s) URL, got %q", p, b.URL)
		}
		if !strings.HasPrefix(b.PathPrefix, "/") || b.PathPrefix == "/" || strings.HasSuffix(b.PathPrefix, "/") {
			add("%s.pathPrefix %q must start with '/' , be non-root and have no trailing '/'", p, b.PathPrefix)
		}
		if b.NamespaceLabel == "" {
			add("%s.namespaceLabel must not be empty", p)
		}
		switch b.TenantFederation {
		case FederationNone:
		case FederationTenantIDLabel:
			if b.TenantHeader == "" {
				add("%s.tenantFederation tenantIdLabel requires tenantHeader", p)
			}
		default:
			add("%s.tenantFederation %q unknown (none|tenantIdLabel)", p, b.TenantFederation)
		}
		if b.Type == BackendTempo {
			if b.TraceByID != TraceByIDDeny && b.TraceByID != TraceByIDFilter {
				add("%s.traceByID %q unknown (deny|filter)", p, b.TraceByID)
			}
		} else if b.TraceByID != "" {
			add("%s.traceByID is only valid on tempo backends", p)
		}
		for _, h := range b.ForwardHeaders {
			switch {
			case !validHeaderName(h):
				add("%s.forwardHeaders: %q is not a valid header name", p, h)
			case reservedHeaders[h] || (b.TenantHeader != "" && h == textproto.CanonicalMIMEHeaderKey(b.TenantHeader)):
				add("%s.forwardHeaders: %q is a credential, tenant, proxy-set or hop-by-hop header", p, h)
			}
		}
	}
	// Prefixes must be unique and must not nest, otherwise routing would be ambiguous.
	for i := range c.Backends {
		for j := i + 1; j < len(c.Backends); j++ {
			a, b := c.Backends[i].PathPrefix, c.Backends[j].PathPrefix
			if a == b || strings.HasPrefix(a+"/", b+"/") || strings.HasPrefix(b+"/", a+"/") {
				add("backends[%d].pathPrefix %q overlaps backends[%d].pathPrefix %q", i, a, j, b)
			}
		}
	}
	return errors.Join(errs...)
}

// validHeaderName reports whether s is an RFC 9110 token, the syntax of a header field name.
func validHeaderName(s string) bool {
	for _, c := range []byte(s) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return s != ""
}

// Backend returns the backend with the given name.
func (c *Config) Backend(name string) (*Backend, bool) {
	for i := range c.Backends {
		if c.Backends[i].Name == name {
			return &c.Backends[i], true
		}
	}
	return nil, false
}
