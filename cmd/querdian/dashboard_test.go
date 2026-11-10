package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/querdian-dev/querdian/internal/auth"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/ui"
)

type noSnapshot struct{}

func (noSnapshot) Snapshot() *policy.Snapshot { return nil }

// newAuthenticator returns an Authenticator whose issuer discovery is complete. The test issuer has no keys,
// so it accepts no token.
func newAuthenticator(t *testing.T) (*auth.Authenticator, *config.Config) {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": srv.URL, "jwks_uri": srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[]}`))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfg, err := config.Parse([]byte("auth: {issuer: \"" + srv.URL + "\", audiences: [q]}\n" +
		"backends:\n  - {name: m, type: prometheus, url: \"http://p:9090\", pathPrefix: /prom, namespaceLabel: namespace}\n"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a, err := auth.New(ctx, cfg.Auth, auth.WithRetryInterval(10*time.Millisecond, 50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); !a.Ready(); {
		if time.Now().After(deadline) {
			t.Fatal("issuer discovery did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return a, cfg
}

func dashboardFor(t *testing.T, args ...string) http.Handler {
	t.Helper()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	flags := config.RegisterFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := flags.Validate(); err != nil {
		t.Fatal(err)
	}
	authn, cfg := newAuthenticator(t)
	return dashboard(flags, ui.Deps{Config: cfg, Snapshots: noSnapshot{}}, authn, slog.New(slog.DiscardHandler))
}

func get(h http.Handler, host, path, token string) int {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = host
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// TestDashboardWiring: --ui-auth=oidc gives a dashboard that rejects each request without a valid token, and
// both modes check the Host header first.
func TestDashboardWiring(t *testing.T) {
	if h := dashboardFor(t); h != nil {
		t.Fatal("a dashboard without --ui-listen")
	}

	for _, args := range [][]string{{"--ui-listen=:8081"}, {"--ui-listen=:8081", "--ui-auth=oidc"}} {
		h := dashboardFor(t, args...)
		for _, p := range []string{"/", "/app.js", "/api/overview", "/api/policies"} {
			if c := get(h, "localhost:8081", p, ""); c != http.StatusUnauthorized {
				t.Errorf("%v, %s without a token: got %d, want 401", args, p, c)
			}
			if c := get(h, "localhost:8081", p, "not-a-jwt"); c != http.StatusUnauthorized {
				t.Errorf("%v, %s with a bad token: got %d, want 401", args, p, c)
			}
			if c := get(h, "evil.example.net", p, ""); c != http.StatusForbidden {
				t.Errorf("%v, %s with a foreign host: got %d, want 403", args, p, c)
			}
		}
	}

	h := dashboardFor(t, "--ui-listen=:8081", "--ui-auth=none", "--ui-allowed-host=querdian.example.com")
	for host, want := range map[string]int{"localhost:8081": http.StatusOK, "querdian.example.com": http.StatusOK,
		"evil.example.net": http.StatusForbidden} {
		if c := get(h, host, "/", ""); c != want {
			t.Errorf("--ui-auth=none, Host %q: got %d, want %d", host, c, want)
		}
	}
}
