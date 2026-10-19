package main

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"
)

// Histogram buckets of http_request_duration_seconds (the live workloads use the same).
var buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

const latencySigma = 0.45

type series struct {
	sample string // full sample name, e.g. http_requests_total
	labels string // rendered labels without braces
	vals   []float64
}

type family struct {
	name, typ string
	series    []*series
}

func leLabel(b float64) string { return strconv.FormatFloat(b, 'g', -1, 64) }

func (s *service) baseLabels() string {
	return fmt.Sprintf(`job=%q,namespace=%q,pod=%q,instance=%q`, s.Name, s.Namespace, s.Pod, s.Instance)
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// genMetrics simulates the metrics of one service at a fixed resolution. Point i has the age
// (n-i)*step, so i = n is "now".
func genMetrics(s *service, seed uint64, hours int, step time.Duration) []*family {
	n := int(time.Duration(hours) * time.Hour / step)
	pts := n + 1
	r := newRNG(seed, "metrics/"+s.Name)
	dt := step.Seconds()
	base := s.baseLabels()

	reqs := &family{name: "http_requests", typ: "counter"}
	hist := &family{name: "http_request_duration_seconds", typ: "histogram"}
	customers := &family{name: "demo_requests", typ: "counter"}
	cpu := &family{name: "process_cpu_seconds", typ: "counter"}
	gauges := map[string]*family{}
	gauge := func(name string) *series {
		f := &family{name: name, typ: "gauge"}
		se := &series{sample: name, labels: base, vals: make([]float64, pts)}
		f.series = []*series{se}
		gauges[name] = f
		return se
	}
	util := gauge("demo_worker_pool_utilization")
	dbIn := gauge("demo_db_connections_in_use")
	dbMax := gauge("demo_db_connections_max")
	queue := gauge("demo_queue_depth")
	mem := gauge("process_resident_memory_bytes")

	cpuS := &series{sample: "process_cpu_seconds_total", labels: base, vals: make([]float64, pts)}
	cpu.series = []*series{cpuS}
	var cust [3]*series
	for i := range cust {
		cust[i] = &series{sample: "demo_requests_total", labels: base + fmt.Sprintf(`,customer_id="c-%d"`, i+1), vals: make([]float64, pts)}
		customers.series = append(customers.series, cust[i])
	}

	totalStep := make([]float64, pts)
	for ri := range s.Routes {
		rt := &s.Routes[ri]
		other := 400
		if rt.NotFound {
			other = 404
		}
		codes := []int{rt.OKStatus, other, 500, 503}
		cs := make([]*series, len(codes))
		for ci, code := range codes {
			cs[ci] = &series{
				sample: "http_requests_total",
				labels: base + fmt.Sprintf(`,method=%q,route=%q,status="%d"`, rt.Method, rt.Path, code),
				vals:   make([]float64, pts),
			}
			reqs.series = append(reqs.series, cs[ci])
		}
		hl := base + fmt.Sprintf(`,method=%q,route=%q`, rt.Method, rt.Path)
		bs := make([]*series, len(buckets)+1)
		for bi, b := range buckets {
			bs[bi] = &series{sample: "http_request_duration_seconds_bucket", labels: hl + `,le="` + leLabel(b) + `"`, vals: make([]float64, pts)}
		}
		bs[len(buckets)] = &series{sample: "http_request_duration_seconds_bucket", labels: hl + `,le="+Inf"`, vals: make([]float64, pts)}
		sum := &series{sample: "http_request_duration_seconds_sum", labels: hl, vals: make([]float64, pts)}
		cnt := &series{sample: "http_request_duration_seconds_count", labels: hl, vals: make([]float64, pts)}
		hist.series = append(hist.series, bs...)
		hist.series = append(hist.series, sum, cnt)

		var total [4]float64
		cum := make([]float64, len(buckets))
		var sumS, count float64
		for i := 1; i < pts; i++ {
			age := time.Duration(n-i) * step
			c := s.cond(rt, age)
			rate := rt.RPS * diurnal(age) * (1 + 0.06*r.norm()) * dt
			pe := c.p400 + c.p500 + c.p503
			lam := [4]float64{rate * (1 - pe), rate * c.p400, rate * c.p500, rate * c.p503}
			var nstep float64
			for ci := range lam {
				k := float64(r.poisson(lam[ci]))
				total[ci] += k
				nstep += k
			}
			totalStep[i] += nstep
			med := rt.median(c)
			for bi, b := range buckets {
				cum[bi] += nstep * phi((math.Log(b*1000)-math.Log(med))/latencySigma)
			}
			sumS += nstep * med * math.Exp(latencySigma*latencySigma/2) / 1000
			count += nstep
			for ci := range cs {
				cs[ci].vals[i] = total[ci]
			}
			prev := 0.0
			for bi := range buckets {
				v := math.Min(math.Round(cum[bi]), count)
				v = math.Max(v, prev)
				bs[bi].vals[i] = v
				prev = v
			}
			bs[len(buckets)].vals[i] = count
			sum.vals[i] = math.Round(sumS*1000) / 1000
			cnt.vals[i] = count
		}
	}

	var cs [3]float64
	var cpuTotal float64
	for i := 1; i < pts; i++ {
		age := time.Duration(n-i) * step
		f := diurnal(age)
		sat := s.saturation(age)
		for ci := range cust {
			cs[ci] += math.Round(totalStep[i] * s.Customers[ci])
			cust[ci].vals[i] = cs[ci]
		}
		util.vals[i] = math.Round(clamp(0.30*f+0.6*sat+0.03*r.norm(), 0.02, 0.99)*1000) / 1000
		dbIn.vals[i] = clamp(math.Round(5*f+14*sat+0.8*r.norm()), 1, 20)
		dbMax.vals[i] = 20
		queue.vals[i] = math.Max(0, math.Round(1.2*f+70*sat*sat+r.norm()))
		frac := math.Mod(age.Hours(), 3) / 3
		mem.vals[i] = math.Round(9.6e7 + 2.4e7*frac + 1e6*r.norm())
		cpuTotal += (0.08 + 0.22*(f-0.7) + 0.5*sat) * dt
		cpuS.vals[i] = math.Round(cpuTotal*100) / 100
	}
	util.vals[0], dbIn.vals[0], dbMax.vals[0], queue.vals[0], mem.vals[0] = util.vals[1], dbIn.vals[1], 20, queue.vals[1], mem.vals[1]

	out := []*family{reqs, hist, customers, cpu}
	for _, name := range []string{"demo_worker_pool_utilization", "demo_db_connections_in_use", "demo_db_connections_max", "demo_queue_depth", "process_resident_memory_bytes"} {
		out = append(out, gauges[name])
	}
	return out
}

// writeOpenMetrics renders families of several services as one OpenMetrics file for
// `promtool tsdb create-blocks-from openmetrics`. Samples are grouped by family, and within a
// family series by series, each with ascending timestamps. The first point (index 0) is skipped:
// counters start at 0 there and Prometheus would see a series that starts at zero.
func writeOpenMetrics(w io.Writer, perService [][]*family, end time.Time, hours int, step time.Duration) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	n := int(time.Duration(hours) * time.Hour / step)
	names := []string{}
	merged := map[string]*family{}
	for _, fams := range perService {
		for _, f := range fams {
			m, ok := merged[f.name]
			if !ok {
				m = &family{name: f.name, typ: f.typ}
				merged[f.name] = m
				names = append(names, f.name)
			}
			m.series = append(m.series, f.series...)
		}
	}
	buf := make([]byte, 0, 256)
	for _, name := range names {
		f := merged[name]
		fmt.Fprintf(bw, "# TYPE %s %s\n", f.name, f.typ)
		for _, se := range f.series {
			for i := 1; i <= n; i++ {
				ts := end.Add(-time.Duration(n-i) * step).Unix()
				buf = buf[:0]
				buf = append(buf, se.sample...)
				buf = append(buf, '{')
				buf = append(buf, se.labels...)
				buf = append(buf, "} "...)
				buf = strconv.AppendFloat(buf, se.vals[i], 'f', -1, 64)
				buf = append(buf, ' ')
				buf = strconv.AppendInt(buf, ts, 10)
				buf = append(buf, '\n')
				if _, err := bw.Write(buf); err != nil {
					return err
				}
			}
		}
	}
	if _, err := bw.WriteString("# EOF\n"); err != nil {
		return err
	}
	return bw.Flush()
}
