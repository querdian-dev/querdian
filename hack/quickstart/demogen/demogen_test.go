package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var testEnd = time.Unix(1792442000, 0)

func TestDeterministic(t *testing.T) {
	for _, s := range services() {
		a := genEvents(s, 1, testEnd, 6)
		b := genEvents(s, 1, testEnd, 6)
		if len(a.logs) != len(b.logs) || len(a.traces) != len(b.traces) || len(a.logs) == 0 {
			t.Fatalf("%s: %d/%d logs, %d/%d traces", s.Name, len(a.logs), len(b.logs), len(a.traces), len(b.traces))
		}
		for i := range a.logs {
			if a.logs[i].render(s) != b.logs[i].render(s) {
				t.Fatalf("%s: log %d differs", s.Name, i)
			}
		}
		c := genEvents(s, 2, testEnd, 6)
		if len(c.logs) == len(a.logs) && c.logs[len(c.logs)/2].render(s) == a.logs[len(a.logs)/2].render(s) {
			t.Fatalf("%s: another seed gives the same data", s.Name)
		}
	}
}

func TestLogsAndTracesLink(t *testing.T) {
	for _, s := range services() {
		ev := genEvents(s, 1, testEnd, 24)
		ids := map[string]trace{}
		spans := 0
		for _, tr := range ev.traces {
			if len(tr.id) != 32 || tr.id[0] == '0' {
				t.Fatalf("bad trace id %q", tr.id)
			}
			ids[tr.id] = tr
			spans += len(tr.spans)
			if len(tr.spans) < 2 && tr.spans[0].errMsg == "" {
				// single-span traces are only allowed for rejected or shed requests
				if !strings.Contains(tr.spans[0].name, " ") {
					t.Fatalf("odd trace %v", tr.spans[0])
				}
			}
			root := tr.spans[0]
			for _, sp := range tr.spans[1:] {
				if sp.parent != root.id || sp.start.Before(root.start) || sp.start.Add(sp.dur).After(root.start.Add(root.dur).Add(time.Millisecond)) {
					t.Fatalf("%s: child %q outside root", s.Name, sp.name)
				}
			}
		}
		errs, linked := 0, 0
		var prev time.Time
		for i := range ev.logs {
			l := &ev.logs[i]
			if l.t.Before(prev) {
				t.Fatalf("%s: logs not sorted", s.Name)
			}
			prev = l.t
			line := l.render(s)
			if s.Format == "json" {
				var m map[string]any
				if err := json.Unmarshal([]byte(line), &m); err != nil {
					t.Fatalf("bad JSON log %q: %v", line, err)
				}
			} else if !strings.HasPrefix(line, "time=") {
				t.Fatalf("bad logfmt line %q", line)
			}
			if l.level == "error" {
				errs++
				if l.traceID == "" {
					t.Fatalf("%s: error log without trace id: %s", s.Name, line)
				}
			}
			if l.traceID != "" {
				linked++
				if _, ok := ids[l.traceID]; !ok {
					t.Fatalf("%s: log refers to unknown trace %s", s.Name, l.traceID)
				}
				if !strings.Contains(line, l.traceID) {
					t.Fatalf("trace id missing from the rendered line: %s", line)
				}
			}
		}
		if errs == 0 || linked == 0 || len(ids) < 1000 || spans < 3*len(ids)/2 {
			t.Fatalf("%s: errors=%d linked=%d traces=%d spans=%d", s.Name, errs, linked, len(ids), spans)
		}
	}
}

// The incidents must be visible in the data they were designed for.
func TestIncidents(t *testing.T) {
	ev := genEvents(services()[1], 1, testEnd, 24) // payment-service
	errorsIn := func(from, to time.Duration) (int, int) {
		bad, all := 0, 0
		for _, tr := range ev.traces {
			a := testEnd.Sub(tr.spans[0].start)
			if a < from || a > to {
				continue
			}
			all++
			if tr.spans[0].errMsg != "" {
				bad++
			}
		}
		return bad, all
	}
	inBad, inAll := errorsIn(6*hour+40*minute, 7*hour+20*minute)
	outBad, outAll := errorsIn(10*hour, 12*hour)
	if inAll == 0 || outAll == 0 || float64(inBad)/float64(inAll) < 5*float64(outBad)/float64(outAll) {
		t.Fatalf("incident not visible: %d/%d inside, %d/%d outside", inBad, inAll, outBad, outAll)
	}
}

func TestOpenMetrics(t *testing.T) {
	const step = 60 * time.Second
	var per [][]*family
	for _, s := range services() {
		per = append(per, genMetrics(s, 1, 2, step))
	}
	// monotone counters and buckets
	for _, fams := range per {
		for _, f := range fams {
			if f.typ == "gauge" {
				continue
			}
			for _, se := range f.series {
				for i := 2; i < len(se.vals); i++ {
					if se.vals[i] < se.vals[i-1] {
						t.Fatalf("%s{%s} decreases at %d", se.sample, se.labels, i)
					}
				}
			}
		}
	}
	var buf bytes.Buffer
	if err := writeOpenMetrics(&buf, per, testEnd, 2, step); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "# TYPE http_requests counter\n") || !strings.HasSuffix(out, "# EOF\n") {
		t.Fatalf("unexpected framing: %.60q ... %.20q", out, out[len(out)-20:])
	}
	for _, want := range []string{`namespace="order-service"`, `namespace="payment-service"`, `route="/orders"`, `status="503"`, `le="+Inf"`, `customer_id="c-3"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s", want)
		}
	}
}

func TestTracesBody(t *testing.T) {
	s := services()[0]
	ev := genEvents(s, 1, testEnd, 1)
	body, err := tracesBody(s, ev.traces[:5])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["resourceSpans"]; !ok {
		t.Fatal("no resourceSpans")
	}
	lb, err := logsBody(s, ev.logs[:5])
	if err != nil || !strings.Contains(string(lb), "resourceLogs") {
		t.Fatalf("logs body: %v", err)
	}
}
