package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type object = map[string]any

func otlpValue(v any) object {
	switch t := v.(type) {
	case int:
		return object{"intValue": strconv.Itoa(t)}
	case float64:
		return object{"doubleValue": t}
	default:
		return object{"stringValue": fmt.Sprint(t)}
	}
}

func otlpAttrs(attrs []attr) []object {
	out := make([]object, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, object{"key": a.k, "value": otlpValue(a.v)})
	}
	return out
}

func ns(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

// resourceAttrs are what the collection pipeline of the quickstart would attach to the pod.
func resourceAttrs(s *service, logs bool) []attr {
	a := []attr{{"service.name", s.Name}, {"k8s.namespace.name", s.Namespace}, {"k8s.pod.name", s.Pod}}
	if logs {
		return append(a, attr{"k8s.container.name", "app"})
	}
	return append(a, attr{"service.version", "1.4.2"}, attr{"deployment.environment.name", "demo"})
}

// tracesBody renders traces of one service as an OTLP/JSON export request.
func tracesBody(s *service, trs []trace) ([]byte, error) {
	spans := []object{}
	for _, tr := range trs {
		for i := range tr.spans {
			sp := &tr.spans[i]
			o := object{
				"traceId": tr.id, "spanId": sp.id, "name": sp.name, "kind": sp.kind,
				"startTimeUnixNano": ns(sp.start), "endTimeUnixNano": ns(sp.start.Add(sp.dur)),
				"attributes": otlpAttrs(sp.attrs),
			}
			if sp.parent != "" {
				o["parentSpanId"] = sp.parent
			}
			if sp.errMsg != "" {
				o["status"] = object{"code": 2, "message": sp.errMsg}
				o["events"] = []object{{
					"timeUnixNano": ns(sp.start.Add(sp.dur)), "name": "exception",
					"attributes": otlpAttrs([]attr{{"exception.type", sp.errType}, {"exception.message", sp.errMsg}}),
				}}
			}
			spans = append(spans, o)
		}
	}
	return json.Marshal(object{"resourceSpans": []object{{
		"resource": object{"attributes": otlpAttrs(resourceAttrs(s, false))},
		"scopeSpans": []object{{
			"scope": object{"name": "demogen"},
			"spans": spans,
		}},
	}}})
}

// logsBody renders log lines of one service as an OTLP/JSON logs export request.
func logsBody(s *service, lines []logLine) ([]byte, error) {
	recs := make([]object, 0, len(lines))
	for i := range lines {
		recs = append(recs, object{
			"timeUnixNano": ns(lines[i].t),
			"body":         object{"stringValue": lines[i].render(s)},
		})
	}
	return json.Marshal(object{"resourceLogs": []object{{
		"resource": object{"attributes": otlpAttrs(resourceAttrs(s, true))},
		"scopeLogs": []object{{
			"scope":      object{"name": "demogen"},
			"logRecords": recs,
		}},
	}}})
}

type pusher struct {
	client *http.Client
	url    string
	tenant string
}

// post sends one request and retries on connection errors, 429 and 5xx (a backend that is
// still starting, or a rate limit hit by the bulk load).
func (p *pusher) post(ctx context.Context, body []byte) error {
	var last error
	for attempt := 0; attempt < 40; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(min(time.Duration(attempt)*500*time.Millisecond, 5*time.Second)):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scope-OrgID", p.tenant)
		resp, err := p.client.Do(req) // #nosec G704 -- the URL is a flag of this demo tool
		if err != nil {
			last = err
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode/100 == 2:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			last = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
		default:
			return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
		}
	}
	return fmt.Errorf("giving up after retries: %w", last)
}

func newPusher(url, tenant string) *pusher {
	return &pusher{client: &http.Client{Timeout: 30 * time.Second}, url: url, tenant: tenant}
}

// pushLogs sends the logs of all services in time order per service, in batches, paced to stay
// below Loki's default ingestion rate limit.
func pushLogs(ctx context.Context, p *pusher, evs []*events, batch int, pace time.Duration) (int, error) {
	n := 0
	for _, ev := range evs {
		for i := 0; i < len(ev.logs); i += batch {
			j := min(i+batch, len(ev.logs))
			body, err := logsBody(ev.svc, ev.logs[i:j])
			if err != nil {
				return n, err
			}
			if err := p.post(ctx, body); err != nil {
				return n, err
			}
			n += j - i
			time.Sleep(pace)
		}
	}
	return n, nil
}

// pushTraces sends the traces of all services in time order, paced so that the number of
// traces held live by the ingester stays below Tempo's default per-tenant limit.
func pushTraces(ctx context.Context, p *pusher, evs []*events, batch int, pace time.Duration) (int, error) {
	n := 0
	for _, ev := range evs {
		for i := 0; i < len(ev.traces); i += batch {
			j := min(i+batch, len(ev.traces))
			body, err := tracesBody(ev.svc, ev.traces[i:j])
			if err != nil {
				return n, err
			}
			if err := p.post(ctx, body); err != nil {
				return n, err
			}
			n += j - i
			time.Sleep(pace)
		}
	}
	return n, nil
}
