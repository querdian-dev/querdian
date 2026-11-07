package observability

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
)

// Metrics holds every metric of PLAN §11.1.
type Metrics struct {
	Requests         *prometheus.CounterVec
	RequestDuration  *prometheus.HistogramVec
	UpstreamRequests *prometheus.CounterVec
	UpstreamDuration *prometheus.HistogramVec
	RewriteDuration  *prometheus.HistogramVec
	PolicyTerms      *prometheus.HistogramVec
	AuthFailures     *prometheus.CounterVec
	SnapshotGen      prometheus.Gauge
	SnapshotRebuilds *prometheus.CounterVec
	Policies         *prometheus.GaugeVec
}

const (
	nativeBucketFactor     = 1.1
	nativeMaxBuckets       = 160
	nativeMinResetDuration = time.Hour
)

func nativeOpts(name, help string, buckets []float64) prometheus.HistogramOpts {
	return prometheus.HistogramOpts{
		Name:                            name,
		Help:                            help,
		Buckets:                         buckets,
		NativeHistogramBucketFactor:     nativeBucketFactor,
		NativeHistogramMaxBucketNumber:  nativeMaxBuckets,
		NativeHistogramMinResetDuration: nativeMinResetDuration,
	}
}

// NewMetrics creates all metrics and registers them on reg. It panics if a
// metric is already registered.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "querdian_requests_total", Help: "Proxy requests by decision.",
		}, []string{"backend", "endpoint", "decision", "reason"}),
		RequestDuration: prometheus.NewHistogramVec(nativeOpts(
			"querdian_request_duration_seconds", "Proxy request duration.", prometheus.DefBuckets,
		), []string{"backend", "endpoint", "decision"}),
		UpstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "querdian_upstream_requests_total", Help: "Upstream requests by status code.",
		}, []string{"backend", "code"}),
		UpstreamDuration: prometheus.NewHistogramVec(nativeOpts(
			"querdian_upstream_duration_seconds", "Upstream request duration.", prometheus.DefBuckets,
		), []string{"backend"}),
		RewriteDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "querdian_rewrite_duration_seconds", Help: "Query rewrite duration.",
			Buckets: []float64{1e-6, 1e-5, 1e-4, 1e-3, 1e-2, 1e-1, 1},
		}, []string{"language", "strategy"}),
		PolicyTerms: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "querdian_policy_terms", Help: "Number of DNF terms per resolved request.",
			Buckets: []float64{1, 2, 3, 4, 6, 8, 16, 32, 64},
		}, []string{"backend"}),
		AuthFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "querdian_auth_failures_total", Help: "Authentication failures by reason.",
		}, []string{"reason"}),
		SnapshotGen: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "querdian_snapshot_generation", Help: "Generation of the active policy snapshot.",
		}),
		SnapshotRebuilds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "querdian_snapshot_rebuilds_total", Help: "Snapshot rebuilds by result.",
		}, []string{"result"}),
		Policies: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "querdian_policies", Help: "Policies by kind and readiness.",
		}, []string{"kind", "ready"}),
	}
	reg.MustRegister(m.Requests, m.RequestDuration, m.UpstreamRequests, m.UpstreamDuration,
		m.RewriteDuration, m.PolicyTerms, m.AuthFailures, m.SnapshotGen, m.SnapshotRebuilds, m.Policies)
	return m
}

// clusterPolicyAge is the collector of querdian_cluster_policy_age_seconds. It reads the ages at each
// scrape, because the age increases without an event.
type clusterPolicyAge struct {
	desc *prometheus.Desc
	ages func(now time.Time) map[string]float64
}

// NewClusterPolicyAge returns the collector of querdian_cluster_policy_age_seconds{cluster} (PLAN §18.6).
// ages gives the time in seconds since the last valid sync of each spoke cluster; policy.Deadlines.Ages
// implements it. A spoke that has not synced has the value +Inf.
func NewClusterPolicyAge(ages func(now time.Time) map[string]float64) prometheus.Collector {
	return &clusterPolicyAge{
		desc: prometheus.NewDesc("querdian_cluster_policy_age_seconds",
			"Time since the last valid policy sync of a spoke cluster.", []string{"cluster"}, nil),
		ages: ages,
	}
}

// NewClusterPolicyExpired returns the collector of querdian_cluster_policy_expired{cluster} (PLAN §18.6):
// 1 if the policies of the spoke are expired or suspended, else 0. policy.Deadlines.ExpiredStates
// implements states.
func NewClusterPolicyExpired(states func(now time.Time) map[string]float64) prometheus.Collector {
	return &clusterPolicyAge{
		desc: prometheus.NewDesc("querdian_cluster_policy_expired",
			"1 if the policies of a spoke cluster are expired or suspended, else 0.", []string{"cluster"}, nil),
		ages: states,
	}
}

// Describe implements prometheus.Collector.
func (c *clusterPolicyAge) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect implements prometheus.Collector.
func (c *clusterPolicyAge) Collect(ch chan<- prometheus.Metric) {
	for cluster, age := range c.ages(time.Now()) {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, age, cluster)
	}
}

// ObserveWithExemplar records v on o and attaches the trace ID of the span in
// ctx as exemplar when that span context is valid and sampled.
func ObserveWithExemplar(ctx context.Context, o prometheus.Observer, v float64) {
	sc := trace.SpanContextFromContext(ctx)
	if eo, ok := o.(prometheus.ExemplarObserver); ok && sc.IsValid() && sc.IsSampled() {
		eo.ObserveWithExemplar(v, prometheus.Labels{"trace_id": sc.TraceID().String()})
		return
	}
	o.Observe(v)
}
