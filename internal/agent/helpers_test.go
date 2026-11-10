package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

// testCA is a CA for test certs.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

var serial int64 = 1

func newCA(t *testing.T, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a PEM cert and key. A server cert is for 127.0.0.1; a client cert has the URI SAN uri.
func (ca *testCA) issue(t *testing.T, server bool, uri string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})
}

// pki is the test setup of the agent: a CA for the receiver, a CA for the agents, and the agent files.
type pki struct {
	serverCA, clientCA *testCA
	files              TLSFiles
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	p := &pki{serverCA: newCA(t, "server-ca"), clientCA: newCA(t, "client-ca")}
	dir := t.TempDir()
	p.files = TLSFiles{
		CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"), CAFile: filepath.Join(dir, "ca.crt"),
	}
	writeFile(t, p.files.CAFile, p.serverCA.pem)
	p.setClient(t, "spiffe://mla.example.com/agent/cluster-a")
	return p
}

// setClient writes a new client cert with the URI SAN uri, as a cert rotation does.
func (p *pki) setClient(t *testing.T, uri string) {
	t.Helper()
	c, k := p.clientCA.issue(t, false, uri)
	writeFile(t, p.files.CertFile, c)
	writeFile(t, p.files.KeyFile, k)
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeReceiver implements the receiver side of the protocol in internal/spokesync, with mTLS. It stores
// the state of one spoke, renews lastSync on each accepted sync, and can inject error replies.
type fakeReceiver struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	stored     []v1.SpokePolicySource
	digest     string
	lastSync   time.Time
	requests   []spokesync.Request
	identities []string
	inject     []int // status codes to reply before the protocol, one for each request
	conditions func([]v1.SpokePolicySource) []spokesync.Condition
}

func newFakeReceiver(t *testing.T, p *pki) *fakeReceiver {
	t.Helper()
	r := &fakeReceiver{t: t}
	empty, _ := spokesync.Digest(nil)
	r.digest = empty
	r.srv = httptest.NewUnstartedServer(http.HandlerFunc(r.handle))
	sc, sk := p.serverCA.issue(t, true, "")
	cert, err := tls.X509KeyPair(sc, sk)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(p.clientCA.cert)
	r.srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool,
	}
	r.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	r.srv.StartTLS()
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeReceiver) url() string { return r.srv.URL }

func (r *fakeReceiver) handle(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != spokesync.Path || req.Method != http.MethodPost || req.Header.Get("Content-Type") != spokesync.ContentType {
		writeJSON(w, http.StatusNotFound, spokesync.ErrorResponse{Error: "unknown path"})
		return
	}
	body, _ := io.ReadAll(req.Body)
	var sr spokesync.Request
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.DisallowUnknownFields()
	if err := d.Decode(&sr); err != nil {
		writeJSON(w, http.StatusBadRequest, spokesync.ErrorResponse{Error: "bad request"})
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, sr)
	r.identities = append(r.identities, req.TLS.PeerCertificates[0].URIs[0].String())
	if len(r.inject) > 0 {
		code := r.inject[0]
		r.inject = r.inject[1:]
		w.Header().Set("Retry-After", "0")
		writeJSON(w, code, spokesync.ErrorResponse{Error: "injected"})
		return
	}
	if sr.Full {
		got, err := spokesync.Digest(sr.Policies)
		if err != nil || got != sr.Digest {
			writeJSON(w, http.StatusBadRequest, spokesync.ErrorResponse{Error: "digest"})
			return
		}
		r.stored, r.digest = sr.Policies, sr.Digest
	}
	if sr.Digest != r.digest {
		writeJSON(w, http.StatusOK, spokesync.Response{Result: spokesync.ResultFullStateRequired})
		return
	}
	r.lastSync = time.Now()
	var conds []spokesync.Condition
	if r.conditions != nil {
		conds = r.conditions(r.stored)
	}
	writeJSON(w, http.StatusOK, spokesync.Response{Result: spokesync.ResultOK, Conditions: conds})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", spokesync.ContentType)
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// snapshot returns a copy of the recorded requests and the last sync time.
func (r *fakeReceiver) snapshot() ([]spokesync.Request, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]spokesync.Request(nil), r.requests...), r.lastSync
}

func (r *fakeReceiver) setInject(codes ...int) {
	r.mu.Lock()
	r.inject = codes
	r.mu.Unlock()
}

func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// syncFunc is a Syncer from a function.
type syncFunc func(context.Context, *spokesync.Request) (*spokesync.Response, error)

func (f syncFunc) Send(ctx context.Context, req *spokesync.Request) (*spokesync.Response, error) {
	return f(ctx, req)
}

// logBuffer is a log output that goroutines can share.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
