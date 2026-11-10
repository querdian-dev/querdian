// Package agent is `querdian agent`, the spoke side of the policy transport (PLAN §18.5).
//
// The agent runs in each spoke cluster. Before each heartbeat, it reads all ClusterAccessPolicy,
// AccessPolicy and Namespace objects directly from the spoke API server. It does not use a cache. If the
// read fails or is not complete, the agent sends nothing, so the receiver does not renew lastSyncTime
// (PLAN §18.6, rule 2). The agent never sends a state that comes from a failed or partial read.
//
// At each heartbeat, the agent sends the digest of the state. If the receiver replies fullStateRequired,
// the agent sends the full state at once. The agent writes the Ready conditions in the reply to the status
// of the spoke objects. On 409, 429 and network errors, the agent retries with backoff, at least each
// minute. On other errors, it retries at the next heartbeat, but also at least each minute. Each attempt
// has a time limit, so a request that does not return cannot stop the sync. An attempt that exceeds the
// time limit is a failure, and the agent retries with backoff.
//
// The agent uses leader election, so one agent sends at a time. The client cert, the key and the CA of
// the receiver come from files. The agent reads these files again before each request, so a rotated cert
// takes effect without a restart.
//
// The agent writes the Ready condition and status.observedGeneration of the spoke policies. Do not run
// a Querdian proxy with a status writer on the same spoke objects: the two writers would change the
// status again and again.
//
// # RBAC
//
// The agent needs this RBAC (PLAN §18.5). It needs no other permission, and it has no wildcard on
// status:
//
//	ClusterRole:
//	- apiGroups: ["policy.querdian.dev"]
//	  resources: ["clusteraccesspolicies", "accesspolicies"]
//	  verbs: ["get", "list", "watch"]
//	- apiGroups: [""]
//	  resources: ["namespaces"]
//	  verbs: ["get", "list", "watch"]
//	- apiGroups: ["policy.querdian.dev"]
//	  resources: ["clusteraccesspolicies/status", "accesspolicies/status"]
//	  verbs: ["update", "patch"]
//	Role in the namespace of the agent (leader election):
//	- apiGroups: ["coordination.k8s.io"]
//	  resources: ["leases"]
//	  verbs: ["get", "create", "update"]
//
// The list follows PLAN §18.5. The agent uses a subset of it: get and list on the policy kinds and
// namespaces, patch on the two status subresources, and get, create and update on its Lease. The agent
// records no events.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/querdian-dev/querdian/internal/observability"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

const (
	// DefaultInterval is the default heartbeat interval (PLAN §18.5).
	DefaultInterval = time.Minute
	// maxRetryDelay is the longest wait after a failure that is retried with backoff (TASKS T6.6).
	maxRetryDelay = time.Minute
	// defaultMinBackoff is the first wait after a failure that is retried with backoff.
	defaultMinBackoff = time.Second
	// minAttemptTimeout and maxAttemptTimeout are the limits of the time limit of one attempt. The time
	// limit is two heartbeat intervals between these limits.
	minAttemptTimeout = time.Minute
	maxAttemptTimeout = 5 * time.Minute
)

// Syncer sends one sync request to the receiver. *Sender is the implementation.
type Syncer interface {
	Send(ctx context.Context, req *spokesync.Request) (*spokesync.Response, error)
}

// Options configures an Agent.
type Options struct {
	// Reader reads directly from the spoke API server. It must not be a cache reader (PLAN §18.6, rule 2).
	Reader client.Reader
	// Writer writes the status of the spoke policies.
	Writer client.StatusClient
	// Syncer sends the requests to the receiver.
	Syncer Syncer
	// Interval is the heartbeat interval. Zero means DefaultInterval.
	Interval time.Duration
	// Logger is the logger. Nil means slog.Default().
	Logger *slog.Logger
	// Metrics records the sync attempts. Nil means metrics on a registry that nobody reads.
	Metrics *observability.AgentMetrics
}

// Agent syncs the policies of one spoke to the hub receiver.
type Agent struct {
	reader     client.Reader
	writer     client.StatusClient
	syncer     Syncer
	interval   time.Duration
	minBackoff time.Duration
	// attemptTimeout is the time limit of one attempt: the read, the requests and the status writes.
	attemptTimeout time.Duration
	log            *slog.Logger
	metrics        *observability.AgentMetrics
}

// New returns an Agent. It returns an error if a required option is missing.
func New(o Options) (*Agent, error) {
	if o.Reader == nil || o.Writer == nil || o.Syncer == nil {
		return nil, errors.New("agent: Reader, Writer and Syncer are required")
	}
	if o.Interval < 0 {
		return nil, fmt.Errorf("agent: negative interval %v", o.Interval)
	}
	if o.Interval == 0 {
		o.Interval = DefaultInterval
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Metrics == nil {
		o.Metrics = observability.NewAgentMetrics(prometheus.NewRegistry())
	}
	return &Agent{
		reader: o.Reader, writer: o.Writer, syncer: o.Syncer, interval: o.Interval,
		minBackoff: defaultMinBackoff, attemptTimeout: min(max(2*o.Interval, minAttemptTimeout), maxAttemptTimeout),
		log: o.Logger, metrics: o.Metrics,
	}, nil
}

// outcome is the result of one sync attempt.
type outcome int

const (
	// synced: the receiver had the same state and accepted the heartbeat. Wait for the next heartbeat.
	synced outcome = iota
	// syncedFull: the receiver accepted the full state. Wait for the next heartbeat.
	syncedFull
	// retry: a read failure, a timeout, a 409, a 429 or a network error. Retry with backoff.
	retry
	// skip: a different error. Log it and wait for the next heartbeat, but at most one minute.
	skip
)

// label returns the value of the result label of querdian_agent_syncs_total.
func (o outcome) label() string {
	switch o {
	case synced:
		return observability.AgentSyncOK
	case syncedFull:
		return observability.AgentSyncFullState
	case retry:
		return observability.AgentSyncRetry
	default:
		return observability.AgentSyncError
	}
}

// Run syncs until ctx ends. It always returns nil. Call it only while this agent holds the lease.
func (a *Agent) Run(ctx context.Context) error {
	var backoff time.Duration
	for ctx.Err() == nil {
		out, hint := a.syncOnce(ctx)
		var wait time.Duration
		if out == retry {
			backoff = nextBackoff(backoff, a.minBackoff, a.retryCap())
			wait = retryDelay(backoff, hint, a.retryCap())
		} else {
			backoff = 0
			wait = a.interval
			if out == skip {
				wait = a.retryCap()
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
	}
	return nil
}

// retryCap is the longest wait before a retry: the heartbeat interval, but at most one minute.
func (a *Agent) retryCap() time.Duration { return min(a.interval, maxRetryDelay) }

// nextBackoff doubles the backoff, from first up to limit.
func nextBackoff(cur, first, limit time.Duration) time.Duration {
	if cur <= 0 {
		return min(first, limit)
	}
	return min(2*cur, limit)
}

// retryDelay is the wait before a retry. A Retry-After hint can make it longer, but never longer than limit.
func retryDelay(backoff, hint, limit time.Duration) time.Duration {
	return min(max(backoff, hint), limit)
}

// syncOnce does one attempt with the time limit attemptTimeout and records its result. It returns the
// outcome and a Retry-After hint from the receiver.
func (a *Agent) syncOnce(ctx context.Context) (outcome, time.Duration) {
	actx, cancel := context.WithTimeout(ctx, a.attemptTimeout)
	defer cancel()
	out, hint := a.attempt(ctx, actx)
	if ctx.Err() == nil {
		a.metrics.Syncs.WithLabelValues(out.label()).Inc()
	}
	return out, hint
}

// attempt does one attempt: a direct read, a heartbeat and, if necessary, a full sync. All calls use actx,
// which has the time limit of the attempt. ctx is the context of Run: when ctx ends, the agent stops and
// does not log the failure. When only actx ends, the attempt is a failure that is logged and retried.
func (a *Agent) attempt(ctx, actx context.Context) (outcome, time.Duration) {
	policies, status, err := readState(actx, a.reader)
	if err != nil {
		if ctx.Err() == nil {
			a.log.Warn("agent: cannot read the spoke API server; nothing sent", "err", err)
		}
		return retry, 0
	}
	digest, err := spokesync.Digest(policies)
	if err != nil {
		a.log.Error("agent: cannot calculate the digest; nothing sent", "err", err)
		return retry, 0
	}
	resp, err := a.syncer.Send(actx, &spokesync.Request{Digest: digest})
	if err != nil {
		return a.failure(ctx, "heartbeat", err)
	}
	out := synced
	if resp.Result == spokesync.ResultFullStateRequired {
		out = syncedFull
		a.log.Info("agent: the receiver has a different state; send the full state", "policies", len(policies))
		resp, err = a.syncer.Send(actx, &spokesync.Request{Digest: digest, Full: true, Policies: policies})
		if err != nil {
			return a.failure(ctx, "full sync", err)
		}
		if resp.Result != spokesync.ResultOK {
			a.log.Error("agent: the receiver did not accept the full state", "result", string(resp.Result))
			return skip, 0
		}
	}
	a.metrics.LastSync.SetToCurrentTime()
	a.log.Debug("agent: sync accepted", "digest", digest, "policies", len(policies))
	a.writeConditions(ctx, actx, resp.Conditions, status)
	return out, 0
}

// failure logs a failed request and classifies it. ctx is the context of Run.
func (a *Agent) failure(ctx context.Context, what string, err error) (outcome, time.Duration) {
	if ctx.Err() != nil {
		return retry, 0
	}
	var se *StatusError
	switch {
	case errors.As(err, &se) && (se.Code == http.StatusConflict || se.Code == http.StatusTooManyRequests):
		a.log.Info("agent: the receiver asked for a retry", "request", what, "status", se.Code, "error", se.Message)
		return retry, se.RetryAfter
	case errors.As(err, &se):
		a.log.Error("agent: the receiver rejected the sync; retry at the next heartbeat",
			"request", what, "status", se.Code, "error", se.Message)
		return skip, 0
	case errors.Is(err, ErrInvalidResponse):
		a.log.Error("agent: invalid reply from the receiver; retry at the next heartbeat", "request", what, "err", err)
		return skip, 0
	default:
		a.log.Warn("agent: cannot reach the receiver", "request", what, "err", err)
		return retry, 0
	}
}
