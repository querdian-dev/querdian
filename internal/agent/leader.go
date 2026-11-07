package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Default timing of the lease, as in the core Kubernetes components.
const (
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewDeadline = 10 * time.Second
	DefaultRetryPeriod   = 2 * time.Second
)

// LeaderElection configures the lease that makes sure that one agent sends at a time.
type LeaderElection struct {
	// Config is the connection to the spoke API server.
	Config *rest.Config
	// Namespace and Name identify the Lease object.
	Namespace, Name string
	// Identity is the holder identity of this agent. Empty means the host name and a random suffix.
	Identity string
	// LeaseDuration, RenewDeadline and RetryPeriod are the lease timing. Zero means the default.
	LeaseDuration, RenewDeadline, RetryPeriod time.Duration
	// Logger is the logger. Nil means slog.Default().
	Logger *slog.Logger
}

// RunWithLeaderElection calls run while this process holds the lease. If the lease is lost, the context of
// run ends, RunWithLeaderElection waits until run returns, and then tries again to get the lease. It returns
// when ctx ends. When ctx ends, it first stops run and waits for it, and only then releases the lease, so
// a different agent cannot start while this agent still sends. The lock records no events, so the agent
// needs no permission on events.
func RunWithLeaderElection(ctx context.Context, le LeaderElection, run func(context.Context) error) error {
	if le.Config == nil || le.Namespace == "" || le.Name == "" {
		return errors.New("leader election: Config, Namespace and Name are required")
	}
	logger := le.Logger
	if logger == nil {
		logger = slog.Default()
	}
	id := le.Identity
	if id == "" {
		var err error
		if id, err = newIdentity(); err != nil {
			return err
		}
	}
	cl, err := coordinationv1client.NewForConfig(le.Config)
	if err != nil {
		return err
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Namespace: le.Namespace, Name: le.Name},
		Client:     cl,
		LockConfig: resourcelock.ResourceLockConfig{Identity: id},
	}

	// done is closed when the current sync loop returns; nil before the first loop starts.
	var mu sync.Mutex
	var done chan struct{}
	current := func() chan struct{} {
		mu.Lock()
		defer mu.Unlock()
		return done
	}
	// The elector releases the lease when electCtx ends. electCtx ends only after the sync loop returns.
	electCtx, electCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer electCancel()
	go func() {
		<-ctx.Done()
		if d := current(); d != nil {
			<-d
		}
		electCancel()
	}()

	for ctx.Err() == nil {
		elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
			Lock:            lock,
			Name:            le.Name,
			LeaseDuration:   orDefault(le.LeaseDuration, DefaultLeaseDuration),
			RenewDeadline:   orDefault(le.RenewDeadline, DefaultRenewDeadline),
			RetryPeriod:     orDefault(le.RetryPeriod, DefaultRetryPeriod),
			ReleaseOnCancel: true,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(lctx context.Context) {
					d := make(chan struct{})
					mu.Lock()
					done = d
					mu.Unlock()
					defer close(d)
					// The loop stops when the lease is lost (lctx) or when the process stops (ctx).
					rctx, rcancel := context.WithCancel(lctx)
					defer rcancel()
					defer context.AfterFunc(ctx, rcancel)()
					if rctx.Err() != nil {
						return // the lease was lost, or the process stopped, before this callback started
					}
					logger.Info("agent: this agent holds the lease", "identity", id)
					if err := run(rctx); err != nil {
						logger.Error("agent: sync loop stopped", "err", err)
					}
				},
				OnStoppedLeading: func() {},
				OnNewLeader: func(holder string) {
					if holder != id {
						logger.Info("agent: a different agent holds the lease", "holder", holder)
					}
				},
			},
		})
		if err != nil {
			return err
		}
		elector.Run(electCtx)
		// Run does not wait for OnStartedLeading. Wait here, so that two sync loops never run together.
		if d := current(); d != nil {
			<-d
		}
	}
	return nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}

// newIdentity returns the host name with a random suffix, so that two processes never share an identity.
func newIdentity() (string, error) {
	host, err := os.Hostname()
	if err != nil {
		return "", err
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return host + "_" + hex.EncodeToString(b), nil
}
