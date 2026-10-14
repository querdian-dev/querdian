package framework

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/yaml"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/auth"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	enforcelogql "github.com/querdian-dev/querdian/internal/enforce/logql"
	enforcepromql "github.com/querdian-dev/querdian/internal/enforce/promql"
	enforcetraceql "github.com/querdian-dev/querdian/internal/enforce/traceql"
	"github.com/querdian-dev/querdian/internal/observability"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/server"
)

// Policies is the set of CRD objects a proxy enforces. It is compiled without Kubernetes.
type Policies struct {
	Cluster []v1.ClusterAccessPolicy
	// Namespaced policies; NamespaceLabels carries the namespace labels (e.g. policy.TenantLabel).
	Namespaced      []v1.AccessPolicy
	NamespaceLabels map[string]map[string]string
}

// ProxyOptions configures StartProxy.
type ProxyOptions struct {
	// Issuer is the OIDC provider; a new one is started when nil.
	Issuer *Issuer
	// Backends are the static-config backends (URL must point at the upstream container).
	Backends []config.Backend
	// Limits overrides the defaults when non-zero fields are set.
	Limits config.Limits
	// Policies is the initial snapshot.
	Policies Policies
}

// Proxy is an in-process Querdian proxy listening on a random loopback port.
type Proxy struct {
	// URL is the base URL of the proxy listener (backends live under their pathPrefix).
	URL string
	// Issuer signs the tokens the proxy accepts.
	Issuer *Issuer
	// Config is the validated static config the proxy runs with.
	Config *config.Config

	t    testing.TB
	snap atomic.Pointer[policy.Snapshot]
	gen  atomic.Int64
}

// StartProxy builds the static config, compiles the policies into a snapshot and serves the real
// internal/server handler chain (all three enforcers registered like cmd/querdian does) on httptest.
func StartProxy(t testing.TB, o ProxyOptions) *Proxy {
	t.Helper()
	iss := o.Issuer
	if iss == nil {
		iss = NewIssuer(t)
	}
	raw, err := yaml.Marshal(config.Config{
		Auth:     config.AuthConfig{Issuer: iss.URL(), Audiences: []string{Audience}},
		Backends: o.Backends,
		Limits:   o.Limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		t.Fatalf("proxy config: %v\n%s", err, raw)
	}
	p := &Proxy{Issuer: iss, Config: cfg, t: t}
	p.SetPolicies(o.Policies)

	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	authn, err := auth.New(ctx, cfg.Auth, auth.WithFailureCounter(metrics.AuthFailures),
		auth.WithRetryInterval(10*time.Millisecond, 50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !authn.Ready(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("issuer discovery did not complete")
		}
	}
	srv := server.New(server.Deps{
		Config:    cfg,
		Snapshots: snapshotFunc(p.snap.Load),
		Auth:      authn,
		Enforcers: map[config.BackendType]enforce.Enforcer{
			config.BackendPrometheus: enforcepromql.New(),
			config.BackendLoki:       enforcelogql.New(),
			config.BackendTempo:      enforcetraceql.New(),
		},
		TailPlanners: map[config.BackendType]server.TailPlanner{config.BackendLoki: enforcelogql.PlanTail},
		Metrics:      metrics,
		Gatherer:     reg,
		Logger:       slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	hs := httptest.NewServer(srv.ProxyHandler())
	t.Cleanup(hs.Close)
	p.URL = hs.URL
	return p
}

type snapshotFunc func() *policy.Snapshot

func (f snapshotFunc) Snapshot() *policy.Snapshot { return f() }

// SetPolicies compiles pol and atomically swaps the snapshot (the proxy picks it up for new requests).
// Policies that fail to compile grant nothing (fail closed); the test fails if a policy is not Ready.
func (p *Proxy) SetPolicies(pol Policies) {
	p.t.Helper()
	c := policy.Compile(policy.Input{Config: p.Config, ClusterPolicies: pol.Cluster, Policies: pol.Namespaced, NamespaceLabels: pol.NamespaceLabels})
	for _, r := range c.Results {
		if !r.Ready {
			p.t.Fatalf("policy %s/%s not ready: %s: %s", r.Kind, r.Name, r.Reason, r.Message)
		}
	}
	p.snap.Store(policy.NewSnapshot(c, p.Config, p.gen.Add(1)))
}

// As returns a client that sends requests to the proxy with a token for user in groups.
func (p *Proxy) As(user string, groups ...string) *Client {
	return &Client{Base: p.URL, Token: p.Issuer.Token(p.t, user, groups...), t: p.t, http: http.DefaultClient}
}

// Anonymous returns a client without credentials.
func (p *Proxy) Anonymous() *Client { return &Client{Base: p.URL, t: p.t, http: http.DefaultClient} }

// testLogWriter forwards proxy log lines (merge/fan-out failures) to the test log, so a 502 in an E2E run
// can be traced to its cause.
type testLogWriter struct{ t testing.TB }

func (w testLogWriter) Write(b []byte) (int, error) {
	w.t.Helper()
	w.t.Log("proxy: " + strings.TrimRight(string(b), "\n"))
	return len(b), nil
}
