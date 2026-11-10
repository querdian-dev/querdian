package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/receiver"
)

// receiverFlags are the flags of `querdian receiver`.
type receiverFlags struct {
	listen           string
	managementListen string
	tlsCertFile      string
	tlsKeyFile       string
	clientCAFile     string
	leaderElect      bool
	leaderElectionNS string
	maxBodyBytes     int64
	maxPolicies      int
	maxInFlight      int
	logLevel         string
	logFormat        string
}

func parseReceiverFlags(args []string) (*receiverFlags, error) {
	f := &receiverFlags{}
	fs := flag.NewFlagSet("querdian receiver", flag.ContinueOnError)
	fs.StringVar(&f.listen, "listen", ":8443", "mTLS listener for spoke agents")
	fs.StringVar(&f.managementListen, "management-listen", ":9090", "listener for /healthz and /readyz")
	fs.StringVar(&f.tlsCertFile, "tls-cert-file", "", "server certificate (required)")
	fs.StringVar(&f.tlsKeyFile, "tls-key-file", "", "server private key (required)")
	fs.StringVar(&f.clientCAFile, "client-ca-file", "", "CA bundle for agent client certificates; only the hub must be able to issue from it (required)")
	fs.BoolVar(&f.leaderElect, "leader-elect", true, "leader election; only the leader serves and reports ready")
	fs.StringVar(&f.leaderElectionNS, "leader-election-namespace", "", "namespace of the leader election lease (default: the namespace of the pod)")
	fs.Int64Var(&f.maxBodyBytes, "max-body-bytes", receiver.DefaultMaxBodyBytes, "maximum size of a sync request body")
	fs.IntVar(&f.maxPolicies, "max-policies", receiver.DefaultMaxPolicies, "maximum number of policies in a full sync")
	fs.IntVar(&f.maxInFlight, "max-in-flight", receiver.DefaultMaxInFlight, "maximum number of requests in progress")
	fs.StringVar(&f.logLevel, "log-level", "info", "slog level (debug|info|warn|error)")
	fs.StringVar(&f.logFormat, "log-format", "json", "log format (json|text)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	switch {
	case f.tlsCertFile == "" || f.tlsKeyFile == "" || f.clientCAFile == "":
		return nil, errors.New("--tls-cert-file, --tls-key-file and --client-ca-file are required")
	case f.listen == "" || f.managementListen == "":
		return nil, errors.New("--listen and --management-listen must not be empty")
	case f.listen == f.managementListen:
		return nil, errors.New("--listen and --management-listen must differ")
	case f.maxBodyBytes <= 0 || f.maxPolicies <= 0 || f.maxInFlight <= 0:
		return nil, errors.New("--max-body-bytes, --max-policies and --max-in-flight must be positive")
	case f.logFormat != "json" && f.logFormat != "text":
		return nil, fmt.Errorf("invalid --log-format %q (json|text)", f.logFormat)
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(f.logLevel)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q: %w", f.logLevel, err)
	}
	return f, nil
}

// runReceiver runs `querdian receiver`, the hub end of the policy transport (PLAN §18.5).
func runReceiver(args []string) error {
	f, err := parseReceiverFlags(args)
	if err != nil {
		return err
	}
	logger, err := newLogger(&config.Flags{LogLevel: f.logLevel, LogFormat: f.logFormat})
	if err != nil {
		return err
	}
	slog.SetDefault(logger)
	tlsCfg, err := receiver.LoadTLSConfig(f.tlsCertFile, f.tlsKeyFile, f.clientCAFile)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		return err
	}
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	// The receiver reads directly from the API server, never from a cache (PLAN §18.5).
	direct, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        "0",
		LeaderElection:                f.leaderElect,
		LeaderElectionID:              "querdian-receiver-leader-election",
		LeaderElectionNamespace:       f.leaderElectionNS,
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return err
	}
	rcv := receiver.New(receiver.Options{
		Client:       direct,
		MaxBodyBytes: f.maxBodyBytes,
		MaxPolicies:  f.maxPolicies,
		MaxInFlight:  f.maxInFlight,
		Logger:       logger,
	})
	srv := receiver.NewServer(f.listen, tlsCfg, rcv, logger)
	if err := mgr.Add(srv); err != nil {
		return err
	}
	if err := mgr.Add(receiver.NewProbeServer(f.managementListen, srv.Ready)); err != nil {
		return err
	}
	if !f.leaderElect {
		// The sync lock and the limit on changed syncs are in the memory of one process (PLAN §18.5).
		logger.Warn("leader election is disabled; run only one receiver replica")
	}
	logger.Info("querdian receiver started", "version", version, "listen", f.listen, "management", f.managementListen)
	if err := mgr.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
