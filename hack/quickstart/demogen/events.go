package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

type attr struct {
	k string
	v any // string, int or float64
}

type span struct {
	id, parent, name string
	kind             int // OTLP SpanKind: 2 server, 3 client
	start            time.Time
	dur              time.Duration
	attrs            []attr
	errType, errMsg  string
}

type trace struct {
	id    string
	spans []span
}

type logLine struct {
	t       time.Time
	level   string
	msg     string
	fields  []attr
	traceID string
}

// events holds the sampled access logs, background logs and traces of one service.
type events struct {
	svc    *service
	logs   []logLine
	traces []trace
}

var dbErrors = []string{
	"pq: could not obtain a connection from the pool",
	"pq: canceling statement due to statement timeout",
	"pq: deadlock detected",
}

// genEvents simulates individual requests (the same model as the metrics) for the access log
// and the traces. Errors, slow requests and a fraction of the others are traced, so every error
// log line carries a trace id that exists in Tempo.
func genEvents(s *service, seed uint64, end time.Time, hours int) *events {
	r := newRNG(seed, "events/"+s.Name)
	ev := &events{svc: s}
	span0 := time.Duration(hours) * time.Hour
	begin := end.Add(-span0)

	ev.logs = append(ev.logs, logLine{t: begin.Add(2 * time.Second), level: "info", msg: "service started",
		fields: []attr{{"version", "1.4.2"}, {"port", 8080}}})

	var totalRPS float64
	for i := range s.Routes {
		totalRPS += s.Routes[i].RPS
	}

	for age := span0.Seconds(); ; {
		age -= r.exp(1 / (logRate * diurnal(time.Duration(age*float64(time.Second)))))
		if age <= 0 {
			break
		}
		a := time.Duration(age * float64(time.Second))
		pick := r.float() * totalRPS
		ri := 0
		for acc := 0.0; ri < len(s.Routes)-1; ri++ {
			acc += s.Routes[ri].RPS
			if pick < acc {
				break
			}
		}
		ev.request(r, &s.Routes[ri], end.Add(-a), a)
	}

	// Background lines: pool statistics every minute, a cache refresh every 7 minutes, and the
	// service's reaction to its incidents.
	for t := begin.Add(30 * time.Second); t.Before(end); t = t.Add(time.Minute) {
		a := end.Sub(t)
		sat := s.saturation(a)
		in := clamp(math.Round(5*diurnal(a)+14*sat+0.8*r.norm()), 1, 20)
		ev.logs = append(ev.logs, logLine{t: t.Add(time.Duration(r.intn(4000)) * time.Millisecond), level: "debug", msg: "connection pool stats",
			fields: []attr{{"in_use", int(in)}, {"idle", int(20 - in)}, {"waiting", int(math.Round(70 * sat * sat))}}})
	}
	for t := begin.Add(3 * time.Minute); t.Before(end); t = t.Add(7 * time.Minute) {
		ev.logs = append(ev.logs, logLine{t: t, level: "info", msg: "cache refreshed",
			fields: []attr{{"entries", 800 + r.intn(400)}, {"took_ms", float64(30+r.intn(60)) + 0.5}}})
	}
	for i := range s.Incidents {
		in := &s.Incidents[i]
		open := end.Add(-(in.Start - in.Ramp/2))
		closed := end.Add(-(in.End + in.Ramp/2))
		if open.Before(begin) {
			continue
		}
		if in.Downstream {
			dep := in.Message
			ev.logs = append(ev.logs, logLine{t: open, level: "warn", msg: "circuit breaker opened", fields: []attr{{"reason", dep}}})
			ev.logs = append(ev.logs, logLine{t: closed, level: "info", msg: "circuit breaker closed", fields: []attr{{"reason", "dependency recovered"}}})
		} else {
			ev.logs = append(ev.logs, logLine{t: open, level: "warn", msg: "connection pool saturated", fields: []attr{{"reason", in.Message}}})
			ev.logs = append(ev.logs, logLine{t: closed, level: "info", msg: "connection pool recovered"})
		}
	}
	sort.SliceStable(ev.logs, func(i, j int) bool { return ev.logs[i].t.Before(ev.logs[j].t) })
	sort.SliceStable(ev.traces, func(i, j int) bool { return ev.traces[i].spans[0].start.Before(ev.traces[j].spans[0].start) })
	return ev
}

type execStep struct {
	st  step
	dur float64 // ms
	err string
}

// request simulates one request that ends at t.
func (ev *events) request(r *rng, rt *route, t time.Time, age time.Duration) {
	s := ev.svc
	c := s.cond(rt, age)
	status := rt.OKStatus
	u := r.float()
	switch {
	case u < c.p503:
		status = 503
	case u < c.p503+c.p500:
		status = 500
	case u < c.p503+c.p500+c.p400:
		status = 400
		if rt.NotFound {
			status = 404
		}
	}

	var steps []execStep
	for _, st := range rt.Steps {
		if st.P < 1 && r.float() > st.P {
			continue
		}
		steps = append(steps, execStep{st: st, dur: st.Median * c.mult(st.Dep) * math.Exp(0.35*r.norm())})
	}
	errText, errDep := "", ""
	switch status {
	case 400:
		steps = nil
	case 404:
		if len(steps) > 1 {
			steps = steps[:1]
		}
	case 503:
		fi := -1
		for i, es := range steps {
			if es.st.Dep == "payment" || es.st.Dep == "provider" {
				fi = i
				break
			}
		}
		if fi < 0 {
			errText = "request shed: worker pool saturated"
			steps = nil
			break
		}
		steps = steps[:fi+1]
		errDep = steps[fi].st.Dep
		peer := "acme-pay"
		if errDep == "payment" {
			peer = "payment-service"
		}
		if r.float() < 0.6 {
			steps[fi].dur = 2000 + 1000*r.float()
			errText = "timeout after 2s waiting for " + peer
		} else {
			steps[fi].dur = steps[fi].st.Median * (0.5 + r.float())
			errText = peer + " returned 502"
		}
		steps[fi].err = errText
	case 500:
		fi := -1
		for i, es := range steps {
			if es.st.Dep == "db" {
				fi = i
				break
			}
		}
		if fi < 0 {
			errText = "internal error: nil order total"
			break
		}
		steps = steps[:fi+1]
		errDep = "db"
		errText = dbErrors[r.intn(len(dbErrors))]
		steps[fi].dur *= 1 + 2*r.float()
		steps[fi].err = errText
	}

	rootMs := 3 * math.Exp(0.3*r.norm())
	for _, es := range steps {
		rootMs += es.dur + 0.3
	}
	dur := time.Duration(rootMs * float64(time.Millisecond))
	startT := t.Add(-dur)

	traced := status >= 500 || dur >= time.Second || r.float() < 0.12
	tid := ""
	if traced {
		tid = r.hex(16)
		concrete := strings.ReplaceAll(rt.Path, "{id}", fmt.Sprintf("%c-%05d", s.Name[0], r.intn(100000)))
		rootID := r.hex(8)
		root := span{id: rootID, name: rt.key(), kind: 2, start: startT, dur: dur,
			attrs: []attr{{"http.request.method", rt.Method}, {"http.route", rt.Path}, {"url.path", concrete}, {"http.response.status_code", status}}}
		if status >= 500 {
			root.errType, root.errMsg = "HTTPError", fmt.Sprintf("%d %s", status, errText)
		}
		tr := trace{id: tid, spans: []span{root}}
		off := 1.0
		for _, es := range steps {
			kind := 3
			sp := span{id: r.hex(8), parent: rootID, name: es.st.Name, kind: kind,
				start: startT.Add(time.Duration(off * float64(time.Millisecond))), dur: time.Duration(es.dur * float64(time.Millisecond))}
			for _, kvp := range es.st.Attrs {
				sp.attrs = append(sp.attrs, attr{kvp[0], kvp[1]})
			}
			if es.err != "" {
				sp.errType, sp.errMsg = errTypeFor(es.st.Dep), es.err
				if es.st.Dep == "payment" || es.st.Dep == "provider" {
					sp.attrs = append(sp.attrs, attr{"http.response.status_code", 502})
				}
			}
			tr.spans = append(tr.spans, sp)
			off += es.dur + 0.3
		}
		ev.traces = append(ev.traces, tr)
	}

	// Access log line (the sampled access log is not every request).
	level, msg := "info", "request completed"
	fields := []attr{{"method", rt.Method}, {"route", rt.Path}, {"status", status}, {"duration_ms", math.Round(rootMs*10) / 10}}
	switch {
	case status >= 500:
		level, msg = "error", "request failed"
		fields = append(fields, attr{"error", errText})
	case status >= 400:
		level, msg = "warn", "request rejected"
		reason := "validation failed"
		if status == 404 {
			reason = "not found"
		}
		fields = append(fields, attr{"reason", reason})
	case dur >= time.Second:
		level, msg = "warn", "slow request"
	}
	if errDep != "" {
		dl := logLine{t: t.Add(-time.Millisecond), level: "error", traceID: tid}
		switch errDep {
		case "db":
			dl.msg = "database error"
			dl.fields = []attr{{"error", errText}}
		case "payment":
			dl.msg = "downstream call failed"
			dl.fields = []attr{{"dependency", "payment-service"}, {"error", errText}}
		default:
			dl.msg = "provider call failed"
			dl.fields = []attr{{"provider", "acme-pay"}, {"error", errText}}
		}
		ev.logs = append(ev.logs, dl)
	}
	ev.logs = append(ev.logs, logLine{t: t, level: level, msg: msg, fields: fields, traceID: tid})
}

func errTypeFor(dep string) string {
	switch dep {
	case "db":
		return "pq.Error"
	case "cache":
		return "redis.Error"
	default:
		return "UpstreamError"
	}
}

type pair struct {
	k, v  string
	quote bool
}

const timeLayout = "2006-01-02T15:04:05.000Z"

func fmtFloat(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }

// render formats a log line in the service's native format (JSON or logfmt).
func (l *logLine) render(svc *service) string {
	var b strings.Builder
	ts := l.t.UTC().Format(timeLayout)
	var ps []pair
	ps = append(ps, pair{"time", ts, true}, pair{"level", l.level, true}, pair{"service", svc.Name, true}, pair{"msg", l.msg, true})
	// request fields first, then the trace id, then the extras
	var rest []attr
	for _, f := range l.fields {
		switch f.k {
		case "method", "route", "status", "duration_ms":
			ps = append(ps, valuePair(f))
		default:
			rest = append(rest, f)
		}
	}
	if l.traceID != "" {
		ps = append(ps, pair{"trace_id", l.traceID, true})
	}
	for _, f := range rest {
		ps = append(ps, valuePair(f))
	}
	if svc.Format == "json" {
		b.WriteByte('{')
		for i, p := range ps {
			if i > 0 {
				b.WriteByte(',')
			}
			k, _ := json.Marshal(p.k)
			b.Write(k)
			b.WriteByte(':')
			if p.quote {
				v, _ := json.Marshal(p.v)
				b.Write(v)
			} else {
				b.WriteString(p.v)
			}
		}
		b.WriteByte('}')
		return b.String()
	}
	for i, p := range ps {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		if p.quote && (p.v == "" || strings.ContainsAny(p.v, " \"=")) {
			b.WriteString(strconv.Quote(p.v))
		} else {
			b.WriteString(p.v)
		}
	}
	return b.String()
}

// valuePair renders a typed field; numbers stay unquoted in JSON.
func valuePair(f attr) pair {
	out := pair{k: f.k}
	switch v := f.v.(type) {
	case string:
		out.v, out.quote = v, true
	case int:
		out.v = strconv.Itoa(v)
	case float64:
		out.v = fmtFloat(v)
	}
	return out
}
