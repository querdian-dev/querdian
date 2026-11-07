package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/agent"
	"github.com/querdian-dev/querdian/internal/config"
)

// serviceAccountNamespaceFile holds the namespace of the pod. The agent uses it for the lease.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// minHeartbeatInterval is the lowest heartbeat interval. The receiver accepts one changed sync from a
// cluster in 10 seconds (PLAN §18.5).
const minHeartbeatInterval = 10 * time.Second

// agentFlags are the flags of `querdian agent`.
type agentFlags struct {
	receiverURL      string
	certFile         string
	keyFile          string
	caFile           string
	interval         time.Duration
	requestTimeout   time.Duration
	leaderElect      bool
	leaseNamespace   string
	leaseName        string
	managementListen string
	logLevel         string
	logFormat        string
}

func registerAgentFlags(fs *flag.FlagSet) *agentFlags {
	f := &agentFlags{}
	fs.StringVar(&f.receiverURL, "receiver-url", "", "base URL of the hub receiver (https; required)")
	fs.StringVar(&f.certFile, "tls-cert-file", "", "client cert of the agent, PEM (required; read again before each request)")
	fs.StringVar(&f.keyFile, "tls-key-file", "", "private key of the client cert, PEM (required; read again before each request)")
	fs.StringVar(&f.caFile, "tls-ca-file", "", "CA certs that verify the receiver cert, PEM (required; read again before each request)")
	fs.DurationVar(&f.interval, "heartbeat-interval", agent.DefaultInterval, "time between two heartbeats (at least 10s)")
	fs.DurationVar(&f.requestTimeout, "request-timeout", agent.DefaultRequestTimeout, "time limit for one request to the receiver")
	fs.BoolVar(&f.leaderElect, "leader-elect", true, "leader election, so that one agent sends at a time")
	fs.StringVar(&f.leaseNamespace, "leader-election-namespace", "", "namespace of the lease (empty: the namespace of the pod)")
	fs.StringVar(&f.leaseName, "leader-election-id", "querdian-agent", "name of the lease")
	fs.StringVar(&f.managementListen, "management-listen", ":9090", "listener for /healthz and /readyz (empty: disabled)")
	fs.StringVar(&f.logLevel, "log-level", "info", "slog level (debug|info|warn|error)")
	fs.StringVar(&f.logFormat, "log-format", "json", "log format (json|text)")
	return f
}

func (f *agentFlags) validate() error {
	if f.receiverURL == "" || f.certFile == "" || f.keyFile == "" || f.caFile == "" {
		return errors.New("--receiver-url, --tls-cert-file, --tls-key-file and --tls-ca-file are required")
	}
	if f.interval < minHeartbeatInterval {
		return fmt.Errorf("--heartbeat-interval must be at least %v", minHeartbeatInterval)
	}
	if f.requestTimeout <= 0 {
		return errors.New("--request-timeout must be positive")
	}
	if f.leaderElect && f.leaseName == "" {
		return errors.New("--leader-election-id must not be empty")
	}
	if f.logFormat != "json" && f.logFormat != "text" {
		return fmt.Errorf("invalid --log-format %q (json|text)", f.logFormat)
	}
	return nil
}

// runAgent is `querdian agent`: the spoke side of the policy transport (PLAN §18.5).
func runAgent(args []string) error {
	fs := flag.NewFlagSet("querdian agent", flag.ContinueOnError)
	f := registerAgentFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if err := f.validate(); err != nil {
		return err
	}
	logger, err := newLogger(&config.Flags{LogLevel: f.logLevel, LogFormat: f.logFormat})
	if err != nil {
		return err
	}
	slog.SetDefault(logger)
	// Send the logs of client-go (leader election) and controller-runtime to the same handler.
	klog.SetSlogLogger(logger)
	ctrllog.SetLogger(logr.FromSlogHandler(logger.Handler()))

	sender, err := agent.NewSender(f.receiverURL,
		agent.TLSFiles{CertFile: f.certFile, KeyFile: f.keyFile, CAFile: f.caFile}, f.requestTimeout, logger)
	if err != nil {
		return err
	}
	leaseNamespace := f.leaseNamespace
	if f.leaderElect && leaseNamespace == "" {
		b, err := os.ReadFile(serviceAccountNamespaceFile)
		if err != nil {
			return fmt.Errorf("--leader-election-namespace is empty and the pod namespace is unknown: %w", err)
		}
		leaseNamespace = strings.TrimSpace(string(b))
	}

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1.AddToScheme(scheme); err != nil {
		return err
	}
	// A direct client with no cache: each heartbeat reads the API server (PLAN §18.6, rule 2).
	kube, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	a, err := agent.New(agent.Options{Reader: kube, Writer: kube, Syncer: sender, Interval: f.interval, Logger: logger})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	running := 1
	if f.managementListen != "" {
		running++
		go func() { errc <- serveHealth(ctx, f.managementListen) }()
	}
	go func() {
		if !f.leaderElect {
			errc <- a.Run(ctx)
			return
		}
		errc <- agent.RunWithLeaderElection(ctx, agent.LeaderElection{
			Config: restCfg, Namespace: leaseNamespace, Name: f.leaseName, Logger: logger,
		}, a.Run)
	}()

	logger.Info("querdian agent started", "version", version, "receiver", f.receiverURL,
		"heartbeat", f.interval.String(), "leaderElect", f.leaderElect)
	err = <-errc // first component to stop: cancel the others and wait for them
	cancel()
	for running--; running > 0; running-- {
		if err2 := <-errc; err == nil {
			err = err2
		}
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// serveHealth serves /healthz and /readyz until ctx ends.
func serveHealth(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /readyz", ok)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
