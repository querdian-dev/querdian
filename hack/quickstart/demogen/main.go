// Command demogen produces the deterministic demo history of the Querdian quickstart: hours of
// metrics (OpenMetrics, to be turned into TSDB blocks with promtool), logs (pushed to Loki over
// OTLP) and traces (pushed to Tempo over OTLP) of the two sample services. See
// docs/quickstart.md. DEMO ONLY.
//
//	demogen metrics -hours 24 > history.om
//	demogen loki  -url http://loki:3100  -tenant demo -hours 24
//	demogen tempo -url http://tempo:4318 -tenant demo -hours 24
//	demogen stats -hours 24
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
)

const usage = "usage: demogen metrics|loki|tempo|stats [flags]"

type options struct {
	hours  int
	end    int64
	seed   uint64
	step   time.Duration
	url    string
	tenant string
	batch  int
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	var o options
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.IntVar(&o.hours, "hours", 24, "hours of history")
	fs.Int64Var(&o.end, "end", 0, "end of the history as Unix seconds (default: now)")
	fs.Uint64Var(&o.seed, "seed", 1, "seed of the random generator")
	fs.DurationVar(&o.step, "step", 30*time.Second, "metrics resolution")
	fs.StringVar(&o.url, "url", "", "push URL (loki: base URL, tempo: OTLP/HTTP base URL)")
	fs.StringVar(&o.tenant, "tenant", "demo", "tenant (X-Scope-OrgID)")
	fs.IntVar(&o.batch, "batch", 0, "records or traces per request (default 1500 logs, 100 traces)")
	_ = fs.Parse(os.Args[2:])
	if o.hours < 1 || o.hours > 24*30 {
		fmt.Fprintln(os.Stderr, "demogen: -hours must be between 1 and 720")
		os.Exit(2)
	}
	end := time.Now()
	if o.end > 0 {
		end = time.Unix(o.end, 0)
	}
	if err := run(context.Background(), cmd, &o, end); err != nil {
		fmt.Fprintln(os.Stderr, "demogen:", err)
		os.Exit(1)
	}
}

func allEvents(o *options, end time.Time) []*events {
	var evs []*events
	for _, s := range services() {
		evs = append(evs, genEvents(s, o.seed, end, o.hours))
	}
	return evs
}

func run(ctx context.Context, cmd string, o *options, end time.Time) error {
	switch cmd {
	case "metrics":
		end = end.Truncate(o.step)
		var per [][]*family
		for _, s := range services() {
			per = append(per, genMetrics(s, o.seed, o.hours, o.step))
		}
		return writeOpenMetrics(os.Stdout, per, end, o.hours, o.step)
	case "loki", "tempo":
		if o.url == "" {
			return fmt.Errorf("-url is required")
		}
		end = end.Truncate(time.Second)
		evs := allEvents(o, end)
		if cmd == "loki" {
			o.batch = defaultInt(o.batch, 1500)
			n, err := pushLogs(ctx, newPusher(o.url+"/otlp/v1/logs", o.tenant), evs, o.batch, 250*time.Millisecond)
			fmt.Printf("pushed %d log lines to %s\n", n, o.url)
			return err
		}
		o.batch = defaultInt(o.batch, 100)
		n, err := pushTraces(ctx, newPusher(o.url+"/v1/traces", o.tenant), evs, o.batch, 300*time.Millisecond)
		fmt.Printf("pushed %d traces to %s\n", n, o.url)
		return err
	case "stats":
		end = end.Truncate(o.step)
		for _, s := range services() {
			fams := genMetrics(s, o.seed, o.hours, o.step)
			nSeries, nSamples := 0, 0
			for _, f := range fams {
				nSeries += len(f.series)
				for _, se := range f.series {
					nSamples += len(se.vals) - 1
				}
			}
			ev := genEvents(s, o.seed, end, o.hours)
			spans := 0
			for _, tr := range ev.traces {
				spans += len(tr.spans)
			}
			fmt.Printf("%s: %d series, %d samples, %d log lines, %d traces, %d spans\n",
				s.Name, nSeries, nSamples, len(ev.logs), len(ev.traces), spans)
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q; %s", cmd, usage)
	}
}

func defaultInt(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}
