package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/auth"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/observability"
	"github.com/querdian-dev/querdian/internal/policy"
)

// testIssuer is a minimal OIDC provider: discovery, JWKS and RS256 token signing.
type testIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newTestIssuer(t testing.TB) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ti := &testIssuer{key: key}
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

func (ti *testIssuer) token(t testing.TB, user string, groups ...string) string {
	t.Helper()
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"})
	claims, _ := json.Marshal(map[string]any{
		"iss": ti.srv.URL, "aud": "querdian", "exp": time.Now().Add(time.Hour).Unix(),
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

// recordingUpstream records requests and answers with a fixed JSON body.
type recordingUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
	body []string
	// reply, when set, chooses the response; otherwise a fixed success body is returned.
	reply func(r *http.Request) (status int, contentType, body string)
	// handler, when set, takes precedence over reply and writes the response itself.
	handler http.HandlerFunc
}

// setReply installs a custom response function.
func (u *recordingUpstream) setReply(f func(r *http.Request) (int, string, string)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.reply = f
}

// setHandler installs a handler for responses reply cannot express (for example a hijacked connection).
func (u *recordingUpstream) setHandler(f http.HandlerFunc) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.handler = f
}

func newUpstream(t testing.TB) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1<<16)
		n, _ := r.Body.Read(b)
		u.mu.Lock()
		u.reqs = append(u.reqs, r)
		u.body = append(u.body, string(b[:n]))
		u.mu.Unlock()
		u.mu.Lock()
		reply, handler := u.reply, u.handler
		u.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(b[:n])) // let reply functions parse the form
		if handler != nil {
			handler(w, r)
			return
		}
		if reply != nil {
			status, ct, body := reply(r)
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":[]}`))
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *recordingUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

func (u *recordingUpstream) last(t testing.TB) *http.Request {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		t.Fatal("upstream received no request")
	}
	return u.reqs[len(u.reqs)-1]
}

type staticSnapshots struct {
	mu   sync.Mutex
	snap *policy.Snapshot
}

func (s *staticSnapshots) Snapshot() *policy.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

func (s *staticSnapshots) set(snap *policy.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = snap
}

// harness bundles a configured Server with its fakes.
type harness struct {
	issuer    *testIssuer
	metricsUp *recordingUpstream
	logsUp    *recordingUpstream
	tracesUp  *recordingUpstream
	cfg       *config.Config
	snaps     *staticSnapshots
	metrics   *observability.Metrics
	reg       *prometheus.Registry
	auth      *auth.Authenticator
}

func newHarness(t testing.TB) *harness {
	t.Helper()
	h := &harness{issuer: newTestIssuer(t), metricsUp: newUpstream(t), logsUp: newUpstream(t), tracesUp: newUpstream(t), snaps: &staticSnapshots{}}
	yaml := "auth: {issuer: \"" + h.issuer.srv.URL + "\", audiences: [querdian]}\n" +
		"backends:\n" +
		"  - {name: metrics, type: prometheus, url: \"" + h.metricsUp.URL + "\", pathPrefix: /prom, namespaceLabel: namespace}\n" +
		"  - {name: logs, type: loki, url: \"" + h.logsUp.URL + "\", pathPrefix: /loki, namespaceLabel: k8s_namespace_name, tenantHeader: X-Scope-OrgID, tenantFederation: tenantIdLabel}\n" +
		"  - {name: traces, type: tempo, url: \"" + h.tracesUp.URL + "\", pathPrefix: /tempo, namespaceLabel: resource.k8s.namespace.name, tenantHeader: X-Scope-OrgID, traceByID: filter}\n"
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	h.reg = prometheus.NewRegistry()
	h.metrics = observability.NewMetrics(h.reg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.auth, err = auth.New(ctx, cfg.Auth, auth.WithFailureCounter(h.metrics.AuthFailures), auth.WithRetryInterval(10*time.Millisecond, 50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !h.auth.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("issuer discovery did not complete")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return h
}

// policies compiles the given cluster policies into the harness snapshot.
func (h *harness) policies(cps ...v1.ClusterAccessPolicy) {
	h.snaps.set(policy.NewSnapshot(policy.Compile(policy.Input{Config: h.cfg, ClusterPolicies: cps}), h.cfg, 1))
}

func clusterPolicy(name string, subj v1.Subjects, rules ...v1.ClusterRule) v1.ClusterAccessPolicy {
	return v1.ClusterAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.ClusterAccessPolicySpec{Subjects: subj, Rules: rules}}
}

func unrestricted(backend string, tenants ...string) v1.ClusterRule {
	return v1.ClusterRule{Backends: []string{backend}, Tenants: tenants, Unrestricted: true}
}

func (h *harness) server(extra ...func(*Deps)) *Server {
	d := Deps{Config: h.cfg, Snapshots: h.snaps, Auth: h.auth, Metrics: h.metrics, Gatherer: h.reg}
	for _, f := range extra {
		f(&d)
	}
	return New(d)
}

func v1Subjects(groups ...string) v1.Subjects { return v1.Subjects{Groups: groups, Users: []string{}} }
