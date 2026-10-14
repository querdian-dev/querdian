//go:build e2e

package prometheus

import (
	"bytes"
	"math"
	"net/http"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"google.golang.org/protobuf/encoding/protowire"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/test/e2e/framework"
)

// Pinned upstream image (T0.4/T0.5 research used the same version).
const promImage = "docker.io/prom/prometheus:v3.5.0"

const (
	seedPoints = 121 // 30 minutes at 15s
	seedStep   = 15 * time.Second
)

var (
	namespaces = []string{"ns-a", "ns-b", "ns-c"}
	jobs       = []string{"api", "worker"}
)

// seriesDef is one seeded series; value(i) is the sample at index i (timestamp end-(seedPoints-1-i)*step).
type seriesDef struct {
	labels    labels.Labels
	value     func(i int) float64
	exemplars bool
}

// allSeries is the deterministic data set: three namespaces x two jobs plus a few special series.
func allSeries() []seriesDef {
	var out []seriesDef
	add := func(value func(int) float64, exemplars bool, kv ...string) {
		out = append(out, seriesDef{labels: labels.FromStrings(kv...), value: value, exemplars: exemplars})
	}
	for ni, ns := range namespaces {
		for ji, job := range jobs {
			pod := job + "-" + ns + "-0"
			up := 1.0
			if ns == "ns-c" && job == "worker" {
				up = 0
			}
			add(func(int) float64 { return up }, false, "__name__", "up", "namespace", ns, "job", job, "pod", pod)
			rate := float64(1 + ni + 2*ji)
			add(func(i int) float64 { return float64(i) * rate * 10 }, true,
				"__name__", "http_requests_total", "namespace", ns, "job", job, "pod", pod, "code", "200")
			add(func(i int) float64 { return float64(i) * rate }, false,
				"__name__", "http_requests_total", "namespace", ns, "job", job, "pod", pod, "code", "500")
			env := "prod"
			if ji == 1 {
				env = "dev"
			}
			add(func(i int) float64 { return float64(10+ni*3+ji*5) + float64(i%7) }, false,
				"__name__", "temperature", "namespace", ns, "job", job, "pod", pod, "env", env, "customer_id", "cust-"+ns+"-"+job)
			add(func(i int) float64 { return float64(1 + i%4 + ni*2 + ji) }, false, "__name__", "queue_depth", "namespace", ns, "job", job)

			// Histogram: cumulative buckets with fixed per-second increments per le.
			incs := []float64{float64(1 + ni), 1, 1, float64(ji + 1)}
			les := []string{"0.1", "0.5", "1", "+Inf"}
			for bi, le := range les {
				cum := 0.0
				for k := 0; k <= bi; k++ {
					cum += incs[k]
				}
				add(func(i int) float64 { return float64(i) * cum }, false,
					"__name__", "request_duration_seconds_bucket", "namespace", ns, "job", job, "le", le)
			}
			total := incs[0] + incs[1] + incs[2] + incs[3]
			add(func(i int) float64 { return float64(i) * total }, false, "__name__", "request_duration_seconds_count", "namespace", ns, "job", job)
			add(func(i int) float64 { return float64(i) * total * 0.3 }, false, "__name__", "request_duration_seconds_sum", "namespace", ns, "job", job)
		}
	}
	add(func(i int) float64 { return float64(i % 40) }, false, "__name__", "resets_total", "namespace", "ns-a", "job", "api")
	add(func(i int) float64 { return float64(i) }, false, "__name__", "ns_b_only_total", "namespace", "ns-b", "job", "api")
	add(func(int) float64 { return 1 }, false, "__name__", "cluster_info", "version", "1")
	return out
}

// toLabelMatchers converts policy matchers to Prometheus matchers (used to compute expected visibility).
func toLabelMatchers(t testing.TB, ms []v1.Matcher) []*labels.Matcher {
	t.Helper()
	var out []*labels.Matcher
	for _, m := range ms {
		var typ labels.MatchType
		switch m.Op {
		case v1.MatchEqual:
			typ = labels.MatchEqual
		case v1.MatchNotEqual:
			typ = labels.MatchNotEqual
		case v1.MatchRegexMatch:
			typ = labels.MatchRegexp
		case v1.MatchRegexNoMatch:
			typ = labels.MatchNotRegexp
		}
		lm, err := labels.NewMatcher(typ, m.Name, m.Value)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, lm)
	}
	return out
}

// visible reports whether a series is granted by the union of terms (nil terms = everything).
func visible(s labels.Labels, terms [][]*labels.Matcher) bool {
	if terms == nil {
		return true
	}
next:
	for _, term := range terms {
		for _, m := range term {
			if !m.Matches(s.Get(m.Name)) {
				continue next
			}
		}
		return true
	}
	return false
}

// startPrometheus runs a Prometheus with remote-write receiver and exemplar storage and seeds the series
// accepted by keep (nil keeps all). end is the timestamp of the last sample.
func startPrometheus(t testing.TB, end time.Time, keep func(labels.Labels) bool) *framework.Container {
	t.Helper()
	c := framework.RunContainer(t, framework.ContainerSpec{
		Image: promImage,
		Ports: []string{"9090"},
		Args: []string{
			"--config.file=/etc/prometheus/prometheus.yml",
			"--storage.tsdb.path=/prometheus",
			"--web.enable-remote-write-receiver",
			"--enable-feature=exemplar-storage",
			"--storage.tsdb.retention.time=1d",
		},
		ReadyPath: "/-/ready",
	})
	var sel []seriesDef
	for _, s := range allSeries() {
		if keep == nil || keep(s.labels) {
			sel = append(sel, s)
		}
	}
	remoteWrite(t, c.URL("9090"), sel, end)
	return c
}

// remoteWrite pushes the series (and exemplars) with the Prometheus remote-write 1.0 protocol. The
// protobuf and the snappy block framing are encoded by hand to avoid extra dependencies.
func remoteWrite(t testing.TB, base string, series []seriesDef, end time.Time) {
	t.Helper()
	var wr []byte
	for _, s := range series {
		var ts []byte
		lbls := s.labels.Copy()
		names := make([]string, 0, lbls.Len())
		lbls.Range(func(l labels.Label) { names = append(names, l.Name) })
		sort.Strings(names)
		for _, n := range names {
			var l []byte
			l = protowire.AppendTag(l, 1, protowire.BytesType)
			l = protowire.AppendString(l, n)
			l = protowire.AppendTag(l, 2, protowire.BytesType)
			l = protowire.AppendString(l, lbls.Get(n))
			ts = protowire.AppendTag(ts, 1, protowire.BytesType)
			ts = protowire.AppendBytes(ts, l)
		}
		for i := range seedPoints {
			tms := end.Add(-time.Duration(seedPoints-1-i) * seedStep).UnixMilli()
			var sm []byte
			sm = protowire.AppendTag(sm, 1, protowire.Fixed64Type)
			sm = protowire.AppendFixed64(sm, math.Float64bits(s.value(i)))
			sm = protowire.AppendTag(sm, 2, protowire.VarintType)
			sm = protowire.AppendVarint(sm, uint64(tms))
			ts = protowire.AppendTag(ts, 2, protowire.BytesType)
			ts = protowire.AppendBytes(ts, sm)
		}
		if s.exemplars {
			for _, back := range []int{0, 10, 60} {
				var ex []byte
				var l []byte
				l = protowire.AppendTag(l, 1, protowire.BytesType)
				l = protowire.AppendString(l, "trace_id")
				l = protowire.AppendTag(l, 2, protowire.BytesType)
				l = protowire.AppendString(l, "trace-"+lbls.Get("namespace")+"-"+strconv.Itoa(back))
				ex = protowire.AppendTag(ex, 1, protowire.BytesType)
				ex = protowire.AppendBytes(ex, l)
				ex = protowire.AppendTag(ex, 2, protowire.Fixed64Type)
				ex = protowire.AppendFixed64(ex, math.Float64bits(1))
				ex = protowire.AppendTag(ex, 3, protowire.VarintType)
				ex = protowire.AppendVarint(ex, uint64(end.Add(-time.Duration(back)*seedStep).UnixMilli()))
				ts = protowire.AppendTag(ts, 3, protowire.BytesType)
				ts = protowire.AppendBytes(ts, ex)
			}
		}
		wr = protowire.AppendTag(wr, 1, protowire.BytesType)
		wr = protowire.AppendBytes(wr, ts)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/write", bytes.NewReader(snappyLiteral(wr))) //nolint:noctx // test-only
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		t.Fatalf("remote write: %d %s", resp.StatusCode, b.String())
	}
}

// snappyLiteral wraps data in a valid snappy block that only uses literal elements (no compression).
func snappyLiteral(data []byte) []byte {
	out := protowire.AppendVarint(nil, uint64(len(data)))
	for len(data) > 0 {
		n := min(len(data), 60)
		out = append(out, byte(n-1)<<2) //nolint:gosec // n <= 60
		out = append(out, data[:n]...)
		data = data[n:]
	}
	return out
}
