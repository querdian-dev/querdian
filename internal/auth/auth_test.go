package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel/trace"

	"github.com/querdian-dev/querdian/internal/config"
)

// testIssuer is a minimal OIDC provider: discovery + JWKS + token signing.
type testIssuer struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	failDisc atomic.Int32 // number of discovery requests to fail with 500
}

func newTestIssuer(t *testing.T, tlsServer bool) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ti := &testIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if ti.failDisc.Add(-1) >= 0 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                ti.srv.URL,
			"jwks_uri":                              ti.srv.URL + "/jwks",
			"authorization_endpoint":                ti.srv.URL + "/auth",
			"token_endpoint":                        ti.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(key.N.Bytes()),
			"e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	if tlsServer {
		ti.srv = httptest.NewTLSServer(mux)
	} else {
		ti.srv = httptest.NewServer(mux)
	}
	t.Cleanup(ti.srv.Close)
	return ti
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (ti *testIssuer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	body, _ := json.Marshal(claims)
	in := b64(hdr) + "." + b64(body)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, ti.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + b64(sig)
}

func (ti *testIssuer) claims(extra map[string]any) map[string]any {
	c := map[string]any{
		"iss": ti.srv.URL, "aud": "querdian", "sub": "s1",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"preferred_username": "alice",
		"groups":             []string{"dev", "ops"},
	}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func newAuth(t *testing.T, ti *testIssuer, mutate func(*config.AuthConfig), opts ...Option) *Authenticator {
	t.Helper()
	cfg := config.AuthConfig{
		Issuer:    ti.srv.URL,
		Audiences: []string{"other", "querdian"},
		Claims:    config.ClaimsConfig{Username: "preferred_username", Groups: "groups"},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	opts = append([]Option{WithRetryInterval(5*time.Millisecond, 20*time.Millisecond)}, opts...)
	a, err := New(ctx, cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, a)
	return a
}

func waitReady(t *testing.T, a *Authenticator) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !a.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("authenticator never became ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func do(ctx context.Context, h http.Handler, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)
	if token != "" {
		r.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestMiddlewareSuccess(t *testing.T) {
	ti := newTestIssuer(t, false)
	a := newAuth(t, ti, nil)
	var got Identity
	var ok bool
	h := a.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok = IdentityFrom(r.Context())
	}))
	w := do(context.Background(), h, "bearer "+ti.sign(t, ti.claims(nil)))
	if w.Code != http.StatusOK || !ok {
		t.Fatalf("code=%d ok=%v", w.Code, ok)
	}
	if got.Username != "alice" || strings.Join(got.Groups, ",") != "dev,ops" {
		t.Fatalf("identity = %+v", got)
	}
	if _, ok := IdentityFrom(context.Background()); ok {
		t.Fatal("identity from empty context")
	}
}

func TestFailuresAndCounter(t *testing.T) {
	ti := newTestIssuer(t, false)
	cv := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "querdian_auth_failures_total"}, []string{"reason"})
	a := newAuth(t, ti, nil, WithFailureCounter(cv))
	h := a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("next must not be called")
	}))

	other := newTestIssuer(t, false)
	forged := other.sign(t, ti.claims(nil)) // same kid, different key

	cases := []struct {
		name   string
		header string
		reason Reason
	}{
		{"missing", "", ReasonMissingToken},
		{"wrong scheme", "Basic abc", ReasonInvalidToken},
		{"garbage", "Bearer not-a-jwt", ReasonInvalidToken},
		{"bad signature", "Bearer " + forged, ReasonInvalidToken},
		{"expired", "Bearer " + ti.sign(t, ti.claims(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})), ReasonExpired},
		{"audience", "Bearer " + ti.sign(t, ti.claims(map[string]any{"aud": "nope"})), ReasonAudience},
		{"issuer", "Bearer " + ti.sign(t, ti.claims(map[string]any{"iss": "https://evil.example"})), ReasonIssuer},
		{"no username", "Bearer " + ti.sign(t, ti.claims(map[string]any{"preferred_username": nil})), ReasonMissingUsername},
		{"bad groups", "Bearer " + ti.sign(t, ti.claims(map[string]any{"groups": 5})), ReasonInvalidToken},
	}
	want := map[Reason]float64{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			span := trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: trace.TraceID{1, 2, 3}, SpanID: trace.SpanID{4}, TraceFlags: trace.FlagsSampled,
			})
			ctx := trace.ContextWithSpanContext(context.Background(), span)
			w := do(ctx, h, tc.header)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("code = %d", w.Code)
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["status"] != "error" || body["errorType"] != "unauthorized" || body["error"] == "" {
				t.Fatalf("body = %v", body)
			}
			if body["traceId"] != span.TraceID().String() {
				t.Fatalf("traceId = %q", body["traceId"])
			}
			if tok := strings.TrimPrefix(tc.header, "Bearer "); tok != "" && strings.Contains(w.Body.String(), tok) {
				t.Fatal("response leaks token")
			}
			want[tc.reason]++
			if got := testutil.ToFloat64(cv.WithLabelValues(string(tc.reason))); got != want[tc.reason] {
				t.Fatalf("counter[%s] = %v, want %v", tc.reason, got, want[tc.reason])
			}
		})
	}
	for _, r := range Reasons {
		if want[r] == 0 {
			t.Errorf("reason %s not covered", r)
		}
	}
}

func TestEmptyTraceID(t *testing.T) {
	ti := newTestIssuer(t, false)
	a := newAuth(t, ti, nil)
	w := do(context.Background(), a.Middleware(http.NotFoundHandler()), "")
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if v, ok := body["traceId"]; !ok || v != "" {
		t.Fatalf("traceId = %q present=%v", v, ok)
	}
}

func TestErrorDoesNotLeakToken(t *testing.T) {
	ti := newTestIssuer(t, false)
	a := newAuth(t, ti, nil)
	tok := ti.sign(t, ti.claims(map[string]any{"exp": 1}))
	var err error
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	_, err = a.Authenticate(r)
	if err == nil || strings.Contains(err.Error(), tok) || ReasonOf(err) != ReasonExpired {
		t.Fatalf("err = %v", err)
	}
}

func TestNilCounterAndBadCounter(t *testing.T) {
	ti := newTestIssuer(t, false)
	a := newAuth(t, ti, nil) // nil counter must not panic
	do(context.Background(), a.Middleware(http.NotFoundHandler()), "")

	bad := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "x"}, []string{"a", "b"})
	if _, err := New(context.Background(), config.AuthConfig{Issuer: ti.srv.URL}, WithFailureCounter(bad)); err == nil {
		t.Fatal("expected error for counter with wrong labels")
	}
}

func TestCustomCA(t *testing.T) {
	ti := newTestIssuer(t, true)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ti.srv.Certificate().Raw})
	if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	a := newAuth(t, ti, func(c *config.AuthConfig) { c.CAFile = caFile })
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+ti.sign(t, ti.claims(nil)))
	id, err := a.Authenticate(r)
	if err != nil || id.Username != "alice" {
		t.Fatalf("id=%+v err=%v", id, err)
	}

	// Without the CA the issuer is untrusted: never becomes ready.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, err := New(ctx, config.AuthConfig{Issuer: ti.srv.URL}, WithRetryInterval(5*time.Millisecond, 10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if b.Ready() {
		t.Fatal("ready without trusted CA")
	}

	if _, err := New(ctx, config.AuthConfig{Issuer: ti.srv.URL, CAFile: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("expected error for missing caFile")
	}
	bogus := filepath.Join(t.TempDir(), "bogus.pem")
	_ = os.WriteFile(bogus, []byte("nope"), 0o600)
	if _, err := New(ctx, config.AuthConfig{Issuer: ti.srv.URL, CAFile: bogus}); err == nil {
		t.Fatal("expected error for bogus caFile")
	}
}

func TestDiscoveryRetries(t *testing.T) {
	ti := newTestIssuer(t, false)
	ti.failDisc.Store(3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := New(ctx, config.AuthConfig{
		Issuer: ti.srv.URL, Audiences: []string{"querdian"},
		Claims: config.ClaimsConfig{Username: "preferred_username", Groups: "groups"},
	}, WithRetryInterval(5*time.Millisecond, 10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if a.Ready() {
		t.Fatal("ready before first successful discovery")
	}
	// Not ready: fail closed with 503.
	w := do(context.Background(), a.Middleware(http.NotFoundHandler()), "Bearer x")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d", w.Code)
	}
	waitReady(t, a)
}

func TestClaimExtraction(t *testing.T) {
	claims := map[string]any{
		"preferred_username": "alice",
		"email":              "a@example.com",
		"empty":              "",
		"num":                float64(3),
		"realm_access":       map[string]any{"roles": []any{"r1", "r2"}, "single": "solo"},
		"a.b":                "literal-dotted",
		"a":                  map[string]any{"b": "nested"},
		"x.y":                map[string]any{"z": "mixed"},
		"deep":               map[string]any{"l1": map[string]any{"l2": "v"}},
		"mixed":              []any{"ok", 1},
	}
	users := []struct {
		path, want string
		wantErr    bool
	}{
		{"preferred_username", "alice", false},
		{"email", "a@example.com", false},
		{"deep.l1.l2", "v", false},
		{"a.b", "literal-dotted", false},
		{"x.y.z", "mixed", false},
		{"realm_access.single", "solo", false},
		{"missing", "", true},
		{"deep.l1.nope", "", true},
		{"deep.l1.l2.l3", "", true},
		{"empty", "", true},
		{"num", "", true},
		{"realm_access.roles", "", true},
		{"", "", true},
	}
	for _, tc := range users {
		t.Run("user/"+tc.path, func(t *testing.T) {
			got, err := extractUsername(claims, tc.path)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %q err=%v", got, err)
			}
			if err != nil && ReasonOf(err) != ReasonMissingUsername {
				t.Fatalf("reason = %s", ReasonOf(err))
			}
		})
	}
	groups := []struct {
		path    string
		want    []string
		wantErr bool
	}{
		{"realm_access.roles", []string{"r1", "r2"}, false},
		{"realm_access.single", []string{"solo"}, false},
		{"email", []string{"a@example.com"}, false},
		{"empty", nil, false},
		{"missing", nil, false},
		{"num", nil, true},
		{"mixed", nil, true},
		{"deep.l1", nil, true},
	}
	for _, tc := range groups {
		t.Run("groups/"+tc.path, func(t *testing.T) {
			got, err := extractGroups(claims, tc.path)
			if (err != nil) != tc.wantErr || strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v err=%v", got, err)
			}
		})
	}
}

func TestGroupsAsString(t *testing.T) {
	ti := newTestIssuer(t, false)
	a := newAuth(t, ti, nil)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+ti.sign(t, ti.claims(map[string]any{"groups": "solo"})))
	id, err := a.Authenticate(r)
	if err != nil || len(id.Groups) != 1 || id.Groups[0] != "solo" {
		t.Fatalf("id=%+v err=%v", id, err)
	}
}
