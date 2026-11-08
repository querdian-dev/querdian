package receiver

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

func capSrc(name string) v1.SpokePolicySource {
	return v1.SpokePolicySource{Kind: v1.SpokeClusterAccessPolicy, Name: name, Generation: 1,
		ClusterAccessPolicy: &v1.ClusterAccessPolicySpec{
			Subjects: v1.Subjects{Groups: []string{"g"}},
			Rules:    []v1.ClusterRule{{Backends: []string{"metrics"}, Matchers: []v1.Matcher{{Name: "app", Op: v1.MatchEqual, Value: "x"}}}},
		}}
}

func apSrc(ns, name string) v1.SpokePolicySource {
	return v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Namespace: ns, Name: name, Generation: 2, TenantLabel: "t1",
		AccessPolicy: &v1.AccessPolicySpec{Subjects: v1.Subjects{Users: []string{"u"}}, Rules: []v1.Rule{{Backends: []string{"logs"}}}}}
}

func digestOf(t *testing.T, ps ...v1.SpokePolicySource) string {
	t.Helper()
	d, err := spokesync.Digest(ps)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDecodeRequest(t *testing.T) {
	d := digestOf(t)
	good := `{"digest":"` + d + `","full":true,"policies":[{"kind":"ClusterAccessPolicy","name":"a","generation":1,` +
		`"clusterAccessPolicy":{"subjects":{"groups":["g"],"users":[]},"rules":[{"backends":["m"],"unrestricted":true,"allClusters":false,"matchers":[]}]}}]}`
	if _, err := decodeRequest([]byte(good), testLimit); err != nil {
		t.Fatalf("good request: %v", err)
	}
	for name, body := range map[string]string{
		"unknown field":        `{"digest":"` + d + `","cluster":"other"}`,
		"unknown nested field": strings.Replace(good, `"name":"a"`, `"name":"a","owner":"x"`, 1),
		"unknown spec field":   strings.Replace(good, `"backends":["m"]`, `"backends":["m"],"tenant":"x"`, 1),
		"case variant":         `{"Digest":"` + d + `"}`,
		"case variant nested":  strings.Replace(good, `"name":"a"`, `"Name":"a"`, 1),
		"duplicate key":        `{"digest":"` + d + `","digest":"` + d + `"}`,
		"duplicate nested key": strings.Replace(good, `"name":"a"`, `"name":"a","name":"b"`, 1),
		"trailing data":        `{"digest":"` + d + `"}{}`,
		"trailing garbage":     `{"digest":"` + d + `"} x`,
		"not an object":        `[]`,
		"wrong type":           `{"digest":"` + d + `","full":"true"}`,
		"float generation":     strings.Replace(good, `"generation":1`, `"generation":1.0`, 1),
		"empty":                ``,
		"deep nesting":         strings.Repeat("[", 40) + strings.Repeat("]", 40),
	} {
		if _, err := decodeRequest([]byte(body), testLimit); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidatePolicy(t *testing.T) {
	octets := func(n int32) *int32 { return &n }
	if err := validatePolicy(ptr(capSrc("a"))); err != nil {
		t.Fatalf("valid cap: %v", err)
	}
	if err := validatePolicy(ptr(apSrc("ns", "b"))); err != nil {
		t.Fatalf("valid ap: %v", err)
	}
	allClusters := capSrc("a")
	allClusters.ClusterAccessPolicy.Rules[0].AllClusters = true
	if err := validatePolicy(&allClusters); err != nil {
		t.Fatalf("allClusters with matchers is valid for CEL; the compiler rejects it on spokes: %v", err)
	}
	for name, mut := range map[string]func(c, a *v1.SpokePolicySource){
		"cap with namespace": func(c, _ *v1.SpokePolicySource) { c.Namespace = "ns" },
		"ap with empty ns":   func(_, a *v1.SpokePolicySource) { a.Namespace = "" },
		"cap with ap spec":   func(c, _ *v1.SpokePolicySource) { c.AccessPolicy = apSrc("x", "y").AccessPolicy },
		"cap without spec":   func(c, _ *v1.SpokePolicySource) { c.ClusterAccessPolicy = nil },
		"ap with cap spec":   func(_, a *v1.SpokePolicySource) { a.ClusterAccessPolicy = capSrc("x").ClusterAccessPolicy },
		"ap kind cap spec": func(_, a *v1.SpokePolicySource) {
			a.AccessPolicy, a.ClusterAccessPolicy = nil, capSrc("x").ClusterAccessPolicy
		},
		"cap with tenant label": func(c, _ *v1.SpokePolicySource) { c.TenantLabel = "t" },
		"unknown kind":          func(c, _ *v1.SpokePolicySource) { c.Kind = "Role" },
		"empty name":            func(c, _ *v1.SpokePolicySource) { c.Name = "" },
		"bad name":              func(c, _ *v1.SpokePolicySource) { c.Name = "A/b" },
		"bad namespace":         func(_, a *v1.SpokePolicySource) { a.Namespace = "a.b" },
		"bad tenant label":      func(_, a *v1.SpokePolicySource) { a.TenantLabel = "a b" },
		"zero generation":       func(c, _ *v1.SpokePolicySource) { c.Generation = 0 },
		"no subjects":           func(c, _ *v1.SpokePolicySource) { c.ClusterAccessPolicy.Subjects = v1.Subjects{} },
		"ap no subjects":        func(_, a *v1.SpokePolicySource) { a.AccessPolicy.Subjects = v1.Subjects{} },
		"no rules":              func(c, _ *v1.SpokePolicySource) { c.ClusterAccessPolicy.Rules = nil },
		"ap no rules":           func(_, a *v1.SpokePolicySource) { a.AccessPolicy.Rules = nil },
		"no backends":           func(c, _ *v1.SpokePolicySource) { c.ClusterAccessPolicy.Rules[0].Backends = nil },
		"ap no backends":        func(_, a *v1.SpokePolicySource) { a.AccessPolicy.Rules[0].Backends = nil },
		"neither":               func(c, _ *v1.SpokePolicySource) { c.ClusterAccessPolicy.Rules[0].Matchers = nil },
		"unrestricted+matchers": func(c, _ *v1.SpokePolicySource) { c.ClusterAccessPolicy.Rules[0].Unrestricted = true },
		"allClusters+unrestrict": func(c, _ *v1.SpokePolicySource) {
			r := &c.ClusterAccessPolicy.Rules[0]
			r.Matchers, r.Unrestricted, r.AllClusters = nil, true, true
		},
		"empty matcher name": func(c, _ *v1.SpokePolicySource) { c.ClusterAccessPolicy.Rules[0].Matchers[0].Name = "" },
		"unknown op": func(_, a *v1.SpokePolicySource) {
			a.AccessPolicy.Rules[0].Matchers = []v1.Matcher{{Name: "a", Op: "Like"}}
		},
		"redact no name": func(_, a *v1.SpokePolicySource) {
			a.AccessPolicy.Rules[0].Labels = redact(v1.RedactRule{Strategy: v1.RedactHash})
		},
		"redact twice": func(_, a *v1.SpokePolicySource) {
			a.AccessPolicy.Rules[0].Labels = redact(v1.RedactRule{Name: "x", Strategy: v1.RedactHash}, v1.RedactRule{Name: "x", Strategy: v1.RedactRemove})
		},
		"redact strategy": func(_, a *v1.SpokePolicySource) {
			a.AccessPolicy.Rules[0].Labels = redact(v1.RedactRule{Name: "x", Strategy: "Mask"})
		},
		"ipv4 no octets": func(c, _ *v1.SpokePolicySource) {
			c.ClusterAccessPolicy.Rules[0].Labels = redact(v1.RedactRule{Name: "x", Strategy: v1.RedactIPv4Prefix})
		},
		"octets on hash": func(_, a *v1.SpokePolicySource) {
			a.AccessPolicy.Rules[0].Labels = redact(v1.RedactRule{Name: "x", Strategy: v1.RedactHash, Octets: octets(1)})
		},
		"octets range": func(_, a *v1.SpokePolicySource) {
			a.AccessPolicy.Rules[0].Labels = redact(v1.RedactRule{Name: "x", Strategy: v1.RedactIPv4Prefix, Octets: octets(4)})
		},
	} {
		c, a := capSrc("a"), apSrc("ns", "b")
		mut(&c, &a)
		if validatePolicy(&c) == nil && validatePolicy(&a) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

var quiet = slog.New(slog.DiscardHandler)

func ptr[T any](v T) *T { return &v }

func redact(rs ...v1.RedactRule) *v1.LabelFilter { return &v1.LabelFilter{Redact: rs} }

func TestValidateRequest(t *testing.T) {
	a, b := capSrc("a"), apSrc("ns", "b")
	for name, tc := range map[string]struct {
		req    spokesync.Request
		status int
	}{
		"heartbeat":            {spokesync.Request{Digest: digestOf(t, a)}, 0},
		"full":                 {spokesync.Request{Digest: digestOf(t, a, b), Full: true, Policies: []v1.SpokePolicySource{a, b}}, 0},
		"empty full":           {spokesync.Request{Digest: digestOf(t), Full: true}, 0},
		"bad digest":           {spokesync.Request{Digest: "sha256:XYZ"}, 400},
		"upper-case digest":    {spokesync.Request{Digest: strings.ToUpper(digestOf(t))}, 400},
		"no digest":            {spokesync.Request{}, 400},
		"heartbeat with state": {spokesync.Request{Digest: digestOf(t, a), Policies: []v1.SpokePolicySource{a}}, 400},
		"digest differs":       {spokesync.Request{Digest: digestOf(t, a), Full: true, Policies: []v1.SpokePolicySource{a, b}}, 400},
		"duplicate keys":       {spokesync.Request{Digest: digestOf(t, a), Full: true, Policies: []v1.SpokePolicySource{a, a}}, 400},
		"too many":             {spokesync.Request{Digest: digestOf(t, a, b), Full: true, Policies: []v1.SpokePolicySource{a, b, capSrc("c")}}, 413},
	} {
		err := validateRequest(&tc.req, 2)
		if got := statusOf(err); got != tc.status {
			t.Errorf("%s: status %d, want %d (%v)", name, got, tc.status, err)
		}
	}
}

func statusOf(err error) int {
	if err == nil {
		return 0
	}
	if r, ok := err.(*rejection); ok { //nolint:errorlint // validateRequest returns a bare rejection
		return r.status
	}
	return -1
}

func TestIdentity(t *testing.T) {
	ca := newCA(t, "hub")
	if _, err := Identity(nil); err == nil {
		t.Error("no TLS accepted")
	}
	one := ca.issue(t, false, "spiffe://hub/agent/a")
	if id, err := Identity(connState(one)); err != nil || id != "spiffe://hub/agent/a" {
		t.Errorf("one URI SAN: %q, %v", id, err)
	}
	unverified := connState(one)
	unverified.VerifiedChains = nil
	if _, err := Identity(unverified); err == nil {
		t.Error("unverified cert accepted")
	}
	if _, err := Identity(connState(ca.issue(t, false))); err == nil {
		t.Error("cert with no URI SAN accepted")
	}
	if _, err := Identity(connState(ca.issue(t, false, "spiffe://hub/agent/a", "spiffe://hub/agent/b"))); err == nil {
		t.Error("cert with two URI SANs accepted")
	}
}

// TestHandleEarlyRejections covers the rejections that come before any read of the API server, so the
// receiver has no client here.
func TestHandleEarlyRejections(t *testing.T) {
	ca := newCA(t, "hub")
	cert := ca.issue(t, false, "spiffe://hub/agent/a")
	r := New(Options{MaxBodyBytes: 1024, Logger: quiet})
	hb := `{"digest":"` + digestOf(t) + `"}`
	for _, tc := range []struct {
		name, method, path, ctype, body string
		state                           bool
		status                          int
	}{
		{"unknown path", http.MethodPost, "/v1alpha1/other", "application/json", hb, true, 404},
		{"query", http.MethodPost, spokesync.Path + "?x=1", "application/json", hb, true, 404},
		{"method", http.MethodPut, spokesync.Path, "application/json", hb, true, 405},
		{"get", http.MethodGet, spokesync.Path, "application/json", "", true, 405},
		{"content type", http.MethodPost, spokesync.Path, "text/plain", hb, true, 415},
		{"no content type", http.MethodPost, spokesync.Path, "", hb, true, 415},
		{"content type param", http.MethodPost, spokesync.Path, "application/json; v=2", hb, true, 415},
		{"no client cert", http.MethodPost, spokesync.Path, "application/json", hb, false, 403},
		{"body too large", http.MethodPost, spokesync.Path, "application/json", `{"digest":"` + strings.Repeat("a", 2000) + `"}`, true, 413},
		{"unknown field", http.MethodPost, spokesync.Path, "application/json", `{"digest":"x","cluster":"b"}`, true, 400},
		{"kind mismatch", http.MethodPost, spokesync.Path, "application/json; charset=utf-8",
			`{"digest":"` + digestOf(t) + `","full":true,"policies":[{"kind":"AccessPolicy","name":"a","generation":1,"clusterAccessPolicy":{"subjects":{"groups":["g"]},"rules":[{"backends":["m"],"unrestricted":true}]}}]}`, true, 400},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		if tc.ctype != "" {
			req.Header.Set("Content-Type", tc.ctype)
		}
		if tc.state {
			req.TLS = connState(cert)
		} else {
			req.TLS = nil
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Errorf("%s: status %d, want %d: %s", tc.name, w.Code, tc.status, w.Body)
		}
		var er spokesync.ErrorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &er); err != nil || er.Error == "" {
			t.Errorf("%s: body is not an ErrorResponse: %s", tc.name, w.Body)
		}
	}
	req := httptest.NewRequest(http.MethodPost, spokesync.Path, strings.NewReader(hb))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.TLS = connState(cert)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("content encoding: status %d", w.Code)
	}
}

// TestRequestLimit: each identity has a burst of RequestBurst requests and then one request for each
// RequestInterval. The limit applies to an identity that is not registered, and before the body is read.
func TestRequestLimit(t *testing.T) {
	ca := newCA(t, "hub")
	certA := ca.issue(t, false, "spiffe://hub/agent/a")
	certB := ca.issue(t, false, "spiffe://hub/agent/b")
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	r := New(Options{Logger: quiet, Now: func() time.Time { return now }})
	send := func(cert tls.Certificate, body string) int {
		req := httptest.NewRequest(http.MethodPost, spokesync.Path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.TLS = connState(cert)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests && w.Header().Get("Retry-After") == "" {
			t.Error("429 without Retry-After")
		}
		return w.Code
	}
	// The body is rejected before any read of the API server, so the receiver needs no client.
	const bad = `{"cluster":"x"}`
	for i := range DefaultRequestBurst {
		if code := send(certA, bad); code != http.StatusBadRequest {
			t.Fatalf("request %d: status %d", i+1, code)
		}
	}
	if code := send(certA, bad); code != http.StatusTooManyRequests {
		t.Fatalf("request after the burst: status %d", code)
	}
	// A body that the decoder would reject gets 429 too: the limit applies before the body is read.
	if code := send(certA, `not json`); code != http.StatusTooManyRequests {
		t.Fatalf("limit after the body read: status %d", code)
	}
	if code := send(certB, bad); code != http.StatusBadRequest {
		t.Fatalf("other identity: status %d", code)
	}
	now = now.Add(DefaultRequestInterval)
	if code := send(certA, bad); code != http.StatusBadRequest {
		t.Fatalf("after one interval: status %d", code)
	}
	if code := send(certA, bad); code != http.StatusTooManyRequests {
		t.Fatalf("one token for each interval: status %d", code)
	}
	// A clock that goes back gives no tokens.
	now = now.Add(-time.Hour)
	if code := send(certA, bad); code != http.StatusTooManyRequests {
		t.Fatalf("clock back: status %d", code)
	}
	// Buckets that are full again are removed, so the map does not grow with old identities.
	now = now.Add(2 * time.Hour)
	send(certB, bad)
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("buckets = %d, want 1", n)
	}
}

func TestConditions(t *testing.T) {
	mk := func(name string, gen int64, status metav1.ConditionStatus, reason, msg string) *v1.SpokePolicy {
		src := capSrc(name)
		o := &v1.SpokePolicy{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.SpokePolicySpec{Cluster: "c", SpokePolicySource: src}}
		o.Status.SourceGeneration = gen
		if reason != "" {
			o.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: status, Reason: reason, Message: msg}}
		}
		return o
	}
	own := map[string]*v1.SpokePolicy{
		"a": mk("a", 4, metav1.ConditionTrue, policy.ReasonCompiled, "policy is active"),
		"b": mk("b", 2, metav1.ConditionFalse, policy.ReasonInvalidMatcher, "bad regex"),
		"c": mk("c", 3, metav1.ConditionTrue, policy.ReasonCrossTenantUnionUnsupported, "unions across tenants: metrics"),
		"d": mk("d", 1, "", "", ""),                                         // not compiled yet
		"e": mk("e", 0, metav1.ConditionTrue, policy.ReasonCompiled, "old"), // no source generation
	}
	got := conditions(own, nil)
	if len(got) != 3 {
		t.Fatalf("conditions: %+v", got)
	}
	if got[0].Name != "a" || !got[0].Ready || got[0].SourceGeneration != 4 {
		t.Errorf("a: %+v", got[0])
	}
	if got[1].Name != "b" || got[1].Ready || got[1].Reason != policy.ReasonInvalidMatcher || got[1].Message != "bad regex" || got[1].SourceGeneration != 2 {
		t.Errorf("b: %+v", got[1])
	}
	if got[2].Name != "c" || !got[2].Ready || got[2].Reason != policy.ReasonCompiled || strings.Contains(got[2].Message, "tenant") {
		t.Errorf("c leaks the cross-tenant condition: %+v", got[2])
	}
	if got := conditions(own, map[string]*v1.SpokePolicy{"b": own["b"]}); len(got) != 1 || got[0].Name != "b" {
		t.Errorf("keep filter: %+v", got)
	}
}

func TestProbeHandler(t *testing.T) {
	ready := false
	h := ProbeHandler(func() bool { return ready })
	get := func(path string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w.Code
	}
	if get("/healthz") != 200 || get("/readyz") != 503 {
		t.Fatal("probes before leadership")
	}
	ready = true
	if get("/readyz") != 200 {
		t.Fatal("readyz as leader")
	}
}

func TestLoadTLSConfig(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t, "hub")
	srv := ca.issue(t, true)
	write := func(name string, typ string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(srv.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certFile := write("tls.crt", "CERTIFICATE", srv.Certificate[0])
	keyFile := write("tls.key", "PRIVATE KEY", keyDER)
	caFile := write("ca.crt", "CERTIFICATE", ca.der)
	empty := write("empty.crt", "NOTHING", []byte("x"))
	cfg, err := LoadTLSConfig(certFile, keyFile, caFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth.String() != "RequireAndVerifyClientCert" || cfg.ClientCAs == nil {
		t.Fatalf("client auth: %v", cfg.ClientAuth)
	}
	if _, err := LoadTLSConfig(certFile, keyFile, empty); err == nil {
		t.Error("CA file without certificates accepted")
	}
	if _, err := LoadTLSConfig(certFile, keyFile, filepath.Join(dir, "missing")); err == nil {
		t.Error("missing CA file accepted")
	}
	if _, err := LoadTLSConfig(certFile, caFile, caFile); err == nil {
		t.Error("bad key accepted")
	}
}
