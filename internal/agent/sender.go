package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/querdian-dev/querdian/internal/spokesync"
)

const (
	// DefaultRequestTimeout is the default time limit for one request to the receiver.
	DefaultRequestTimeout = 30 * time.Second
	// maxResponseBytes limits the body of an accepted sync. The conditions of many policies fit.
	maxResponseBytes = 32 << 20
	// maxErrorBytes limits the body of a rejected sync. The agent only logs it.
	maxErrorBytes = 4 << 10
)

// ErrInvalidResponse is the error for a 200 reply that the agent cannot use.
var ErrInvalidResponse = errors.New("invalid response from the receiver")

// StatusError is the error for a reply with a status other than 200.
type StatusError struct {
	// Code is the HTTP status code.
	Code int
	// Message is the error text of the receiver, if the body is an ErrorResponse.
	Message string
	// RetryAfter is the value of the Retry-After header in seconds, or zero.
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("receiver replied %d", e.Code)
	}
	return fmt.Sprintf("receiver replied %d: %s", e.Code, e.Message)
}

// TLSFiles are the PEM files for mTLS to the receiver.
type TLSFiles struct {
	// CertFile is the client cert of the agent. It can contain intermediate certs after the leaf.
	CertFile string
	// KeyFile is the private key of the client cert.
	KeyFile string
	// CAFile contains the CA certs that the agent uses to verify the receiver cert.
	CAFile string
}

// Sender sends sync requests to the receiver over mTLS. Before each request, it reads the TLS files again,
// so a rotated cert or CA takes effect without a restart. If a file cannot be read or does not parse, for
// example during a rotation, the Sender keeps the last good value and logs a warning.
type Sender struct {
	url     string
	files   TLSFiles
	timeout time.Duration
	log     *slog.Logger

	mu    sync.Mutex
	cert  *tls.Certificate
	roots *x509.CertPool
}

// NewSender returns a Sender for the receiver at receiverURL. The URL must use https. The agent sends to
// receiverURL joined with spokesync.Path. The first read of the TLS files must succeed. A timeout of zero
// means DefaultRequestTimeout.
func NewSender(receiverURL string, files TLSFiles, timeout time.Duration, logger *slog.Logger) (*Sender, error) {
	u, err := url.Parse(receiverURL)
	if err != nil {
		return nil, fmt.Errorf("receiver URL: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("receiver URL %q: use https://host[:port][/prefix] with no user, query or fragment", receiverURL)
	}
	if files.CertFile == "" || files.KeyFile == "" || files.CAFile == "" {
		return nil, errors.New("the cert file, the key file and the CA file are required")
	}
	if timeout < 0 {
		return nil, fmt.Errorf("negative request timeout %v", timeout)
	}
	if timeout == 0 {
		timeout = DefaultRequestTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Sender{url: u.JoinPath(spokesync.Path).String(), files: files, timeout: timeout, log: logger}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// reload reads the TLS files. It changes the stored values only if all files parse.
func (s *Sender) reload() error {
	cert, err := tls.LoadX509KeyPair(s.files.CertFile, s.files.KeyFile)
	if err != nil {
		return fmt.Errorf("client cert: %w", err)
	}
	pem, err := os.ReadFile(s.files.CAFile)
	if err != nil {
		return fmt.Errorf("CA file: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return fmt.Errorf("CA file %s: no PEM certificate", s.files.CAFile)
	}
	s.mu.Lock()
	s.cert, s.roots = &cert, roots
	s.mu.Unlock()
	return nil
}

// current reads the TLS files again and returns the values to use for the next request.
func (s *Sender) current() (*tls.Certificate, *x509.CertPool) {
	if err := s.reload(); err != nil {
		s.log.Warn("agent: cannot read the TLS files; the last good values stay in use", "err", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cert, s.roots
}

// client returns an HTTP client for one request. It does not keep connections, so each request does a
// new TLS handshake with the current cert, and verifies the receiver with the current CA.
func (s *Sender) client() (*http.Client, *http.Transport) {
	cert, roots := s.current()
	tr := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    roots,
			// Always present the cert. With Certificates, Go omits a cert that the CA list of the server
			// does not name, and the receiver then rejects the sync with no clear cause.
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cert, nil },
		},
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   s.timeout,
		// The receiver has one path. A redirect is not part of the protocol.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, tr
}

// Send sends req and returns the reply of an accepted sync. A reply with a status other than 200 gives a
// *StatusError. A 200 reply with a body that is not a valid Response gives an error that wraps
// ErrInvalidResponse. Other errors come from the connection.
func (s *Sender) Send(ctx context.Context, req *spokesync.Request) (*spokesync.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode the request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("make the request: %w", err)
	}
	hreq.Header.Set("Content-Type", spokesync.ContentType)
	hreq.Header.Set("Accept", spokesync.ContentType)
	hc, tr := s.client()
	defer tr.CloseIdleConnections()
	hresp, err := hc.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = hresp.Body.Close() }()

	if hresp.StatusCode != http.StatusOK {
		se := &StatusError{Code: hresp.StatusCode, RetryAfter: retryAfter(hresp.Header.Get("Retry-After"))}
		// The body is for the log only. A read error gives an empty message; the status code is sufficient.
		raw, _ := io.ReadAll(io.LimitReader(hresp.Body, maxErrorBytes))
		var er spokesync.ErrorResponse
		if json.Unmarshal(raw, &er) == nil {
			se.Message = er.Error
		}
		return nil, se
	}
	raw, err := io.ReadAll(io.LimitReader(hresp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the reply: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("%w: body larger than %d bytes", ErrInvalidResponse, maxResponseBytes)
	}
	var resp spokesync.Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}
	switch resp.Result {
	case spokesync.ResultOK, spokesync.ResultFullStateRequired:
		return &resp, nil
	default:
		return nil, fmt.Errorf("%w: unknown result %q", ErrInvalidResponse, resp.Result)
	}
}

// retryAfter parses a Retry-After header with a number of seconds. Other forms give zero.
func retryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(min(n, 3600)) * time.Second
}
