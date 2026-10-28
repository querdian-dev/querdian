// Package auth implements OIDC bearer-token authentication (PLAN §10).
package auth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/querdian-dev/querdian/internal/config"
)

// Identity is the authenticated caller.
type Identity struct {
	// Username is the value of the configured username claim.
	Username string
	// Groups are the values of the configured groups claim (possibly empty).
	Groups []string
}

type ctxKey struct{}

// WithIdentity returns a context carrying id.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// IdentityFrom returns the Identity stored in ctx by the middleware.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// Option customises an Authenticator.
type Option func(*Authenticator)

// WithFailureCounter sets the counter incremented per failed authentication.
// It must have exactly one label, "reason" (querdian_auth_failures_total).
// A nil counter (the default) disables metrics.
func WithFailureCounter(c *prometheus.CounterVec) Option {
	return func(a *Authenticator) { a.failures = c }
}

// WithLogger sets the logger that reports issuer discovery attempts. A nil logger (the default) discards.
func WithLogger(l *slog.Logger) Option {
	return func(a *Authenticator) { a.logger = l }
}

// WithRetryInterval sets the initial and maximum delay between background
// discovery attempts (the delay doubles from initial up to max).
func WithRetryInterval(initial, maxInterval time.Duration) Option {
	return func(a *Authenticator) { a.retryInitial, a.retryMax = initial, maxInterval }
}

// Authenticator verifies bearer tokens against an OIDC issuer.
type Authenticator struct {
	cfg      config.AuthConfig
	client   *http.Client
	failures *prometheus.CounterVec
	logger   *slog.Logger

	retryInitial, retryMax time.Duration

	verifier atomic.Pointer[oidc.IDTokenVerifier]
}

// New creates an Authenticator and starts issuer discovery in the background,
// retrying with backoff until it succeeds or ctx is cancelled. It only fails
// for unusable local configuration (e.g. an unreadable CA file). ctx must
// outlive the Authenticator: go-oidc uses it for later JWKS refreshes.
func New(ctx context.Context, cfg config.AuthConfig, opts ...Option) (*Authenticator, error) {
	a := &Authenticator{cfg: cfg, retryInitial: time.Second, retryMax: 30 * time.Second}
	for _, o := range opts {
		o(a)
	}
	if a.logger == nil {
		a.logger = slog.New(slog.DiscardHandler)
	}
	if a.failures != nil {
		// Fail early if the vector does not have the expected single label.
		if _, err := a.failures.GetMetricWithLabelValues(string(ReasonInvalidToken)); err != nil {
			return nil, fmt.Errorf("auth: failure counter must have a single label %q: %w", "reason", err)
		}
	}
	a.client = &http.Client{Timeout: 30 * time.Second}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("auth: reading caFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("auth: caFile %q contains no PEM certificates", cfg.CAFile)
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		a.client.Transport = tr
	}
	go a.discover(oidc.ClientContext(ctx, a.client))
	return a, nil
}

func (a *Authenticator) discover(ctx context.Context) {
	delay := a.retryInitial
	for {
		p, err := oidc.NewProvider(ctx, a.cfg.Issuer)
		if err == nil {
			// Audience (any-of) and issuer are checked in Authenticate.
			a.verifier.Store(p.Verifier(&oidc.Config{SkipClientIDCheck: true, SkipIssuerCheck: true}))
			a.logger.InfoContext(ctx, "issuer discovery complete", "issuer", a.cfg.Issuer)
			return
		}
		a.logger.WarnContext(ctx, "issuer discovery failed", "issuer", a.cfg.Issuer, "retry_in", delay, "error", err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, a.retryMax)
	}
}

// Ready reports whether issuer discovery has completed.
func (a *Authenticator) Ready() bool { return a.verifier.Load() != nil }

// ErrNotReady is returned by Authenticate while issuer discovery is pending.
var ErrNotReady = errors.New("auth: issuer discovery not complete")

// Authenticate verifies the bearer token in r and returns the caller's
// Identity. Failures are *Error values, except ErrNotReady while discovery
// is pending.
func (a *Authenticator) Authenticate(r *http.Request) (Identity, error) {
	v := a.verifier.Load()
	if v == nil {
		return Identity{}, ErrNotReady
	}
	h := r.Header.Get("Authorization")
	if h == "" {
		return Identity{}, newError(ReasonMissingToken, nil)
	}
	scheme, raw, _ := strings.Cut(h, " ")
	raw = strings.TrimSpace(raw)
	if !strings.EqualFold(scheme, "Bearer") || raw == "" {
		return Identity{}, newError(ReasonInvalidToken, nil)
	}
	tok, err := v.Verify(r.Context(), raw)
	if err != nil {
		var exp *oidc.TokenExpiredError
		if errors.As(err, &exp) {
			return Identity{}, newError(ReasonExpired, err)
		}
		return Identity{}, newError(ReasonInvalidToken, err)
	}
	if tok.Issuer != a.cfg.Issuer {
		return Identity{}, newError(ReasonIssuer, nil)
	}
	if !anyAudience(tok.Audience, a.cfg.Audiences) {
		return Identity{}, newError(ReasonAudience, nil)
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return Identity{}, newError(ReasonInvalidToken, err)
	}
	user, err := extractUsername(claims, a.cfg.Claims.Username)
	if err != nil {
		return Identity{}, err
	}
	groups, err := extractGroups(claims, a.cfg.Claims.Groups)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Username: user, Groups: groups}, nil
}

func anyAudience(got, want []string) bool {
	for _, g := range got {
		for _, w := range want {
			if g == w {
				return true
			}
		}
	}
	return false
}

func (a *Authenticator) countFailure(r Reason) {
	if a.failures != nil {
		a.failures.WithLabelValues(string(r)).Inc()
	}
}
