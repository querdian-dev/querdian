package framework

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Audience is the audience of all tokens issued by Issuer.
const Audience = "querdian"

// Issuer is a minimal OIDC provider: discovery, JWKS and RS256 token signing.
type Issuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

// NewIssuer starts an issuer that is shut down with the test.
func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ti := &Issuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": ti.srv.URL, "jwks_uri": ti.srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	ti.srv = httptest.NewServer(mux)
	t.Cleanup(ti.srv.Close)
	return ti
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// URL is the issuer URL to put into the proxy configuration.
func (ti *Issuer) URL() string { return ti.srv.URL }

// Token returns a signed bearer token for user with the given groups (claims preferred_username and groups).
func (ti *Issuer) Token(t testing.TB, user string, groups ...string) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	claims, _ := json.Marshal(map[string]any{
		"iss": ti.srv.URL, "aud": Audience, "exp": time.Now().Add(time.Hour).Unix(),
		"preferred_username": user, "groups": groups,
	})
	signing := b64(hdr) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, ti.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + b64(sig)
}
