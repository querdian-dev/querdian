package receiver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// ShutdownTimeout is the time that running syncs get to finish when the server stops.
const ShutdownTimeout = 30 * time.Second

// TLSConfig returns the server TLS config of the receiver. It requires a client cert that chains to
// clientCAs. The receiver has its own client CA, which no spoke can issue certs from (PLAN §18.5).
func TLSConfig(cert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS12,
	}
}

// LoadTLSConfig reads the server cert, the key and the client CA bundle from files and returns TLSConfig.
func LoadTLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("server certificate: %w", err)
	}
	pem, err := os.ReadFile(clientCAFile) //nolint:gosec // path comes from an operator-controlled flag
	if err != nil {
		return nil, fmt.Errorf("client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("client CA: no certificates in %s", clientCAFile)
	}
	return TLSConfig(cert, pool), nil
}

// Server serves a handler over TLS. It is a leader-elected manager runnable, so only the leader serves and
// only the leader reports ready (PLAN §18.5).
type Server struct {
	addr    string
	tls     *tls.Config
	handler http.Handler
	log     *slog.Logger

	ready atomic.Bool
	bound atomic.Pointer[net.Addr]
}

// NewServer creates a Server that listens on addr.
func NewServer(addr string, tlsCfg *tls.Config, h http.Handler, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{addr: addr, tls: tlsCfg, handler: h, log: log}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (s *Server) NeedLeaderElection() bool { return true }

// Ready reports whether the server accepts syncs.
func (s *Server) Ready() bool { return s.ready.Load() }

// Addr returns the bound address, or nil before the server listens.
func (s *Server) Addr() net.Addr {
	if a := s.bound.Load(); a != nil {
		return *a
	}
	return nil
}

// Start implements manager.Runnable. It serves until ctx ends. Requests use ctx as their base, so a loss of
// the leadership also stops the syncs that run.
func (s *Server) Start(ctx context.Context) error {
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	a := l.Addr()
	s.bound.Store(&a)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(tls.NewListener(l, s.tls)) }()
	s.ready.Store(true)
	s.log.Info("receiver serves", "listen", a.String())

	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	s.ready.Store(false)
	sctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	if serr := srv.Shutdown(sctx); serr != nil && err == nil {
		err = serr
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// ProbeHandler returns the handler for /healthz and /readyz. ready decides /readyz.
func ProbeHandler(ready func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "not the leader", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// ProbeServer serves ProbeHandler on every replica. It is a manager runnable without leader election.
type ProbeServer struct {
	addr    string
	handler http.Handler
}

// NewProbeServer creates a ProbeServer that listens on addr.
func NewProbeServer(addr string, ready func() bool) *ProbeServer {
	return &ProbeServer{addr: addr, handler: ProbeHandler(ready)}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (p *ProbeServer) NeedLeaderElection() bool { return false }

// Start implements manager.Runnable.
func (p *ProbeServer) Start(ctx context.Context) error {
	l, err := net.Listen("tcp", p.addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: p.handler, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
