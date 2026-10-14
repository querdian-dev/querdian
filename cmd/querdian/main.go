// Command querdian is a multi-tenant access proxy for Prometheus, Loki and Tempo.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/querdian-dev/querdian/internal/auth"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/controller"
	"github.com/querdian-dev/querdian/internal/enforce"
	enforcelogql "github.com/querdian-dev/querdian/internal/enforce/logql"
	enforcepromql "github.com/querdian-dev/querdian/internal/enforce/promql"
	enforcetraceql "github.com/querdian-dev/querdian/internal/enforce/traceql"
	"github.com/querdian-dev/querdian/internal/observability"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/server"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

const shutdownTimeout = 30 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "querdian:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("querdian", flag.ContinueOnError)
	flags := config.RegisterFlags(fs)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println("querdian", version)
		return nil
	}
	if err := flags.Validate(); err != nil {
		return err
	}
	cfg, err := config.Load(flags.ConfigFile)
	if err != nil {
		return err
	}
	logger, err := newLogger(flags)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := observability.Setup(ctx)
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(sctx)
	}()

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	metrics := observability.NewMetrics(reg)

	scheme := runtime.NewScheme()
	if err := controller.AddToScheme(scheme); err != nil {
		return err
	}
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: "0"}, // we serve /metrics ourselves
		HealthProbeBindAddress:        "0",
		LeaderElection:                flags.LeaderElect,
		LeaderElectionID:              "querdian-leader-election",
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return err
	}
	ctl := controller.New(cfg, controller.WithRebuildHook(snapshotMetrics(metrics)))
	if err := ctl.SetupWithManager(mgr); err != nil {
		return err
	}

	authn, err := auth.New(ctx, cfg.Auth, auth.WithFailureCounter(metrics.AuthFailures))
	if err != nil {
		return err
	}
	srv := server.New(server.Deps{
		Config:    cfg,
		Snapshots: ctl,
		Auth:      authn,
		// Prometheus enforces single-term policies only until T2.2; several terms are rejected (fail closed).
		Enforcers: map[config.BackendType]enforce.Enforcer{
			config.BackendPrometheus: enforcepromql.New(),
			config.BackendLoki:       enforcelogql.New(),
			config.BackendTempo:      enforcetraceql.New(),
		},
		TailPlanners: map[config.BackendType]server.TailPlanner{config.BackendLoki: enforcelogql.PlanTail},
		Metrics:      metrics,
		Gatherer:     reg,
		Privacy:      observability.Privacy{RecordUser: flags.RecordUser, RecordQueries: flags.RecordQueries},
		Logger:       logger,
	})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	go func() { errc <- mgr.Start(ctx) }()
	go func() { errc <- srv.Run(ctx, flags.Listen, flags.ManagementListen, shutdownTimeout) }()

	logger.Info("querdian started", "version", version, "listen", flags.Listen, "management", flags.ManagementListen)
	err = <-errc // first component to stop: cancel the other and wait for it
	cancel()
	if err2 := <-errc; err == nil {
		err = err2
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func newLogger(f *config.Flags) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(f.LogLevel)); err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler = slog.NewJSONHandler(os.Stderr, opts)
	if f.LogFormat == "text" {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(observability.NewTraceHandler(h)), nil
}

// snapshotMetrics updates the snapshot and policy metrics after every rebuild.
func snapshotMetrics(m *observability.Metrics) func(*policy.Snapshot, error) {
	return func(snap *policy.Snapshot, err error) {
		if err != nil || snap == nil {
			m.SnapshotRebuilds.WithLabelValues("error").Inc()
			return
		}
		m.SnapshotRebuilds.WithLabelValues("ok").Inc()
		m.SnapshotGen.Set(float64(snap.Generation))
		counts := map[[2]string]float64{}
		for _, r := range snap.Results() {
			ready := "false"
			if r.Ready {
				ready = "true"
			}
			counts[[2]string{r.Kind, ready}]++
		}
		m.Policies.Reset()
		for _, kind := range []string{"ClusterAccessPolicy", "AccessPolicy"} {
			for _, ready := range []string{"true", "false"} {
				m.Policies.WithLabelValues(kind, ready).Set(counts[[2]string{kind, ready}])
			}
		}
	}
}
