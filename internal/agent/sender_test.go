package agent

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/querdian-dev/querdian/internal/spokesync"
)

func heartbeat(t *testing.T) *spokesync.Request {
	t.Helper()
	d, err := spokesync.Digest(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &spokesync.Request{Digest: d}
}

// TestCertReload: a rotated client cert takes effect at the next request, with no restart. A cert or key
// that does not parse during a rotation does not stop the agent: the last good cert stays in use.
func TestCertReload(t *testing.T) {
	ctx := context.Background()
	p := newPKI(t)
	r := newFakeReceiver(t, p)
	s := newTestSender(t, p, r)

	if _, err := s.Send(ctx, heartbeat(t)); err != nil {
		t.Fatal(err)
	}
	p.setClient(t, "spiffe://mla.example.com/agent/rotated")
	if _, err := s.Send(ctx, heartbeat(t)); err != nil {
		t.Fatal(err)
	}
	// A rotation in progress: the new cert does not match the old key.
	c, _ := p.clientCA.issue(t, false, "spiffe://mla.example.com/agent/half")
	writeFile(t, p.files.CertFile, c)
	if _, err := s.Send(ctx, heartbeat(t)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p.files.KeyFile, []byte("garbage"))
	if _, err := s.Send(ctx, heartbeat(t)); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	ids := append([]string(nil), r.identities...)
	r.mu.Unlock()
	want := []string{
		"spiffe://mla.example.com/agent/cluster-a",
		"spiffe://mla.example.com/agent/rotated",
		"spiffe://mla.example.com/agent/rotated",
		"spiffe://mla.example.com/agent/rotated",
	}
	if len(ids) != len(want) {
		t.Fatalf("identities %v", ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("request %d: identity %q, want %q", i, ids[i], want[i])
		}
	}
}

// TestCAReload: the CA file is read again before each request.
func TestCAReload(t *testing.T) {
	ctx := context.Background()
	p := newPKI(t)
	r := newFakeReceiver(t, p)
	s := newTestSender(t, p, r)
	other := newCA(t, "other")
	writeFile(t, p.files.CAFile, other.pem)
	if _, err := s.Send(ctx, heartbeat(t)); err == nil {
		t.Fatal("a receiver cert from a CA that is not in the CA file must be rejected")
	}
	writeFile(t, p.files.CAFile, p.serverCA.pem)
	if _, err := s.Send(ctx, heartbeat(t)); err != nil {
		t.Fatal(err)
	}
}

func TestSenderVerifiesReceiver(t *testing.T) {
	p := newPKI(t)
	// A receiver with a cert from a CA that the agent does not trust.
	q := newPKI(t)
	q.clientCA = p.clientCA
	r := newFakeReceiver(t, q)
	s := newTestSender(t, p, r)
	if _, err := s.Send(context.Background(), heartbeat(t)); err == nil {
		t.Fatal("untrusted receiver cert accepted")
	}
	if reqs, _ := r.snapshot(); len(reqs) != 0 {
		t.Fatal("the request reached an untrusted receiver")
	}
}

func TestNewSender(t *testing.T) {
	p := newPKI(t)
	for _, u := range []string{
		"http://hub.example.com", "https://", "https://user@hub.example.com", "https://hub.example.com?x=1",
		"https://hub.example.com#f", "://bad",
	} {
		if _, err := NewSender(u, p.files, 0, nil); err == nil {
			t.Errorf("NewSender(%q) must fail", u)
		}
	}
	missing := p.files
	missing.CAFile = ""
	if _, err := NewSender("https://hub.example.com", missing, 0, nil); err == nil {
		t.Error("missing CA file must fail")
	}
	broken := p.files
	broken.KeyFile = broken.CAFile
	if _, err := NewSender("https://hub.example.com", broken, 0, nil); err == nil {
		t.Error("a key that does not parse must fail at startup")
	}
	if _, err := NewSender("https://hub.example.com", p.files, -time.Second, nil); err == nil {
		t.Error("negative timeout must fail")
	}
	s, err := NewSender("https://hub.example.com/prefix/", p.files, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.url != "https://hub.example.com/prefix"+spokesync.Path {
		t.Errorf("url %q", s.url)
	}
}

func TestSendReplies(t *testing.T) {
	ctx := context.Background()
	p := newPKI(t)
	r := newFakeReceiver(t, p)
	var handler http.HandlerFunc
	r.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { handler(w, req) })
	s := newTestSender(t, p, r)

	handler = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		writeJSON(w, http.StatusTooManyRequests, spokesync.ErrorResponse{Error: "too many changed syncs"})
	}
	_, err := s.Send(ctx, heartbeat(t))
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusTooManyRequests || se.Message != "too many changed syncs" ||
		se.RetryAfter != 7*time.Second {
		t.Errorf("429: %v", err)
	}

	handler = func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "https://example.com", http.StatusFound)
	}
	if _, err := s.Send(ctx, heartbeat(t)); !errors.As(err, &se) || se.Code != http.StatusFound {
		t.Errorf("redirect must not be followed: %v", err)
	}

	for name, body := range map[string]string{
		"unknown result": `{"result":"maybe"}`,
		"not json":       `<html>`,
		"empty":          ``,
	} {
		handler = func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
		if _, err := s.Send(ctx, heartbeat(t)); !errors.Is(err, ErrInvalidResponse) {
			t.Errorf("%s: want ErrInvalidResponse, got %v", name, err)
		}
	}

	var gotType string
	handler = func(w http.ResponseWriter, req *http.Request) {
		gotType = req.Header.Get("Content-Type")
		writeJSON(w, http.StatusOK, spokesync.Response{Result: spokesync.ResultOK})
	}
	resp, err := s.Send(ctx, heartbeat(t))
	if err != nil || resp.Result != spokesync.ResultOK || gotType != spokesync.ContentType {
		t.Errorf("ok reply: %v %v %q", resp, err, gotType)
	}
}

func TestRetryAfter(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"": 0, "0": 0, "-1": 0, "3": 3 * time.Second, " 5 ": 5 * time.Second, "999999": time.Hour,
		"Wed, 21 Oct 2015 07:28:00 GMT": 0,
	} {
		if got := retryAfter(in); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}
