//go:build e2e

package tempo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"testing"
	"time"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/pkg/traceql"
	"github.com/querdian-dev/querdian/test/e2e/framework"
)

// Pinned upstream image (T0.5 research used the same version).
const tempoImage = "docker.io/grafana/tempo:2.10.10"

// tempoConfig is a single-binary Tempo with multitenancy, local storage, OTLP/HTTP ingest and the
// local-blocks processor that TraceQL metrics queries need. Short idle periods make traces searchable
// within seconds.
const tempoConfig = `stream_over_http_enabled: true
multitenancy_enabled: true
server:
  http_listen_port: 3200
distributor:
  receivers:
    otlp:
      protocols:
        http:
          endpoint: 0.0.0.0:4318
ingester:
  trace_idle_period: 2s
  max_block_duration: 10s
  lifecycler:
    ring:
      replication_factor: 1
compactor:
  compaction:
    block_retention: 1h
metrics_generator:
  registry:
    external_labels: {}
  storage:
    path: /var/tempo/generator/wal
  traces_storage:
    path: /var/tempo/generator/traces
  processor:
    local_blocks:
      filter_server_spans: false
      flush_to_storage: true
storage:
  trace:
    backend: local
    wal:
      path: /var/tempo/wal
    local:
      path: /var/tempo/blocks
overrides:
  defaults:
    metrics_generator:
      processors: [local-blocks]
`

// OTLP status codes and span kinds.
const (
	statusUnset = 0
	statusOK    = 1
	statusError = 2
	kindServer  = 2
	kindClient  = 3
)

// spanDef is one seeded span with everything the test needs to evaluate a policy itself.
type spanDef struct {
	tenant    string
	trace     string // 32 hex digits
	id        string // 16 hex digits, unique over the whole data set
	parent    string // "" for a root span
	name      string
	service   string
	ns        string
	start     time.Duration // offset from the seed base time
	dur       time.Duration
	kind      int
	status    int
	spanAttrs map[string]string // string span attributes
	code      int               // http.status_code (int attribute)
	resAttrs  map[string]string // extra resource attributes besides service.name and k8s.namespace.name
}

// resource returns the string resource attributes.
func (s spanDef) resource() map[string]string {
	m := map[string]string{"service.name": s.service, "k8s.namespace.name": s.ns}
	for k, v := range s.resAttrs {
		m[k] = v
	}
	return m
}

// lookup resolves a policy matcher name (resource.x, span.x) to a string attribute value.
// An absent attribute (and an integer one) is reported as not present: that is TraceQL's own rule
// (docs/research/traceql.md section 9) which both the injected query and the trace-by-ID filter follow.
func (s spanDef) lookup(name string) (string, bool) {
	scope, n := traceql.SplitAttr(name)
	switch scope {
	case "resource":
		v, ok := s.resource()[n]
		return v, ok
	case "span":
		v, ok := s.spanAttrs[n]
		return v, ok
	}
	if v, ok := s.spanAttrs[n]; ok {
		return v, true
	}
	v, ok := s.resource()[n]
	return v, ok
}

// matches reports whether the span satisfies one matcher (value must be present).
func matches(s spanDef, m v1.Matcher) bool {
	v, ok := s.lookup(m.Name)
	if !ok {
		return false
	}
	switch m.Op {
	case v1.MatchEqual:
		return v == m.Value
	case v1.MatchNotEqual:
		return v != m.Value
	case v1.MatchRegexMatch:
		return regexp.MustCompile("^(?:" + m.Value + ")$").MatchString(v)
	case v1.MatchRegexNoMatch:
		return !regexp.MustCompile("^(?:" + m.Value + ")$").MatchString(v)
	}
	return false
}

// term is a DNF term (AND of matchers) restricted to one tenant.
type term struct {
	tenant   string
	matchers []v1.Matcher
}

// granted reports whether the span is visible under the union of terms for the tenant.
func granted(s spanDef, tenant string, terms []term) bool {
next:
	for _, t := range terms {
		if t.tenant != tenant || s.tenant != tenant {
			continue
		}
		for _, m := range t.matchers {
			if !matches(s, m) {
				continue next
			}
		}
		return true
	}
	return false
}

// --- data set ----------------------------------------------------------------------------------

var (
	namespaces = []string{"ns-a", "ns-b", "ns-c"}
	// services by namespace.
	services = map[string][]string{
		"ns-a": {"frontend", "cart"},
		"ns-b": {"payment", "db"},
		"ns-c": {"worker", "search"},
		"ns-d": {"t2-svc"},
	}
	teams = []string{"alpha", "beta"}
)

type builder struct {
	spans []spanDef
	next  int
}

func (b *builder) id() string { b.next++; return fmt.Sprintf("%016x", b.next) }

func traceID(n int) string { return fmt.Sprintf("%032x", n) }

// add appends a span and returns its ID.
func (b *builder) add(s spanDef) string {
	s.id = b.id()
	if s.kind == 0 {
		s.kind = kindServer
	}
	if s.status == 0 {
		s.status = statusOK
	}
	b.spans = append(b.spans, s)
	return s.id
}

// allSpans is the deterministic data set: handcrafted traces plus generated ones.
func allSpans() []spanDef {
	b := &builder{}
	ms := time.Millisecond
	cust := func(ns string, n int) map[string]string {
		return map[string]string{"customer_id": fmt.Sprintf("cust-%s-%d", ns, n)}
	}
	attrs := func(ns string, n int, method, team string) map[string]string {
		m := cust(ns, n)
		m["http.method"] = method
		m["team"] = team
		return m
	}

	// Single-namespace traces.
	for i, ns := range namespaces {
		tr := traceID(1 + i)
		fe, be := services[ns][0], services[ns][1]
		root := b.add(spanDef{tenant: "t1", trace: tr, name: "GET /" + ns, service: fe, ns: ns, start: 0, dur: 200 * ms,
			spanAttrs: attrs(ns, i, "GET", "alpha"), code: 200})
		b.add(spanDef{tenant: "t1", trace: tr, parent: root, name: "load", service: be, ns: ns, start: 10 * ms, dur: 80 * ms,
			spanAttrs: attrs(ns, i, "GET", "beta"), code: 200})
		b.add(spanDef{tenant: "t1", trace: tr, parent: root, name: "query", service: be, ns: ns, start: 20 * ms, dur: 150 * ms,
			kind: kindClient, status: statusError, spanAttrs: attrs(ns, i, "POST", "alpha"), code: 500})
	}

	// Cross-namespace traces. X1: root in ns-a, error child and grandchild in ns-b.
	{
		tr := traceID(10)
		root := b.add(spanDef{tenant: "t1", trace: tr, name: "POST /checkout", service: "frontend", ns: "ns-a", dur: 400 * ms,
			spanAttrs: attrs("ns-a", 10, "POST", "alpha"), code: 200})
		ch := b.add(spanDef{tenant: "t1", trace: tr, parent: root, name: "charge", service: "payment", ns: "ns-b", start: 20 * ms, dur: 300 * ms,
			status: statusError, spanAttrs: attrs("ns-b", 10, "POST", "alpha"), code: 502})
		b.add(spanDef{tenant: "t1", trace: tr, parent: ch, name: "insert", service: "db", ns: "ns-b", start: 40 * ms, dur: 100 * ms,
			kind: kindClient, spanAttrs: attrs("ns-b", 10, "POST", "beta"), code: 200})
	}
	// X2: ROOT in ns-b ("billing-gateway", secret root name); children in ns-a and ns-c.
	{
		tr := traceID(11)
		root := b.add(spanDef{tenant: "t1", trace: tr, name: "POST /internal/secret-op", service: "billing-gateway", ns: "ns-b", dur: 900 * ms,
			spanAttrs: attrs("ns-b", 11, "POST", "alpha"), code: 200})
		b.add(spanDef{tenant: "t1", trace: tr, parent: root, name: "lookup", service: "cart", ns: "ns-a", start: 30 * ms, dur: 100 * ms,
			spanAttrs: attrs("ns-a", 11, "GET", "alpha"), code: 200})
		b.add(spanDef{tenant: "t1", trace: tr, parent: root, name: "audit", service: "worker", ns: "ns-c", start: 50 * ms, dur: 800 * ms,
			status: statusError, spanAttrs: attrs("ns-c", 11, "PUT", "beta"), code: 500})
	}
	// X3: root in ns-c, children in ns-a and ns-b.
	{
		tr := traceID(12)
		root := b.add(spanDef{tenant: "t1", trace: tr, name: "cron", service: "worker", ns: "ns-c", dur: 60 * ms,
			spanAttrs: attrs("ns-c", 12, "GET", "alpha"), code: 200})
		b.add(spanDef{tenant: "t1", trace: tr, parent: root, name: "ping", service: "frontend", ns: "ns-a", start: 5 * ms, dur: 20 * ms,
			spanAttrs: attrs("ns-a", 12, "GET", "beta"), code: 200})
		b.add(spanDef{tenant: "t1", trace: tr, parent: root, name: "ping", service: "payment", ns: "ns-b", start: 6 * ms, dur: 25 * ms,
			spanAttrs: attrs("ns-b", 12, "GET", "alpha"), code: 404})
	}

	// Generated traces (fixed seed). A random tree of 2..6 spans, namespace chosen per span with a bias
	// to the root namespace so that both pure and mixed traces occur.
	rng := rand.New(rand.NewSource(7)) //nolint:gosec // deterministic test data
	methods := []string{"GET", "POST", "PUT"}
	codes := []int{200, 200, 201, 404, 500}
	for n := range 26 {
		tr := traceID(100 + n)
		rootNS := namespaces[rng.Intn(len(namespaces))]
		count := 2 + rng.Intn(5)
		ids := make([]string, 0, count)
		for k := range count {
			ns := rootNS
			if k > 0 && rng.Intn(3) == 0 {
				ns = namespaces[rng.Intn(len(namespaces))]
			}
			svc := services[ns][rng.Intn(2)]
			code := codes[rng.Intn(len(codes))]
			st := statusOK
			if code >= 500 {
				st = statusError
			}
			s := spanDef{tenant: "t1", trace: tr, name: fmt.Sprintf("op-%d", rng.Intn(4)), service: svc, ns: ns,
				start: time.Duration(k*7) * ms, dur: time.Duration(10+rng.Intn(300)) * ms, status: st, code: code,
				spanAttrs: attrs(ns, n, methods[rng.Intn(3)], teams[rng.Intn(2)])}
			if k == 0 {
				s.name = "root-" + strconv.Itoa(n)
			} else {
				s.parent = ids[rng.Intn(k)]
				if rng.Intn(3) == 0 {
					s.kind = kindClient
				}
			}
			ids = append(ids, b.add(s))
		}
	}

	// Tenant t2: same namespace name ns-a as tenant t1 (tenant isolation must hold although the namespace
	// policy matches), plus ns-d.
	for n := range 4 {
		tr := traceID(500 + n)
		ns := "ns-a"
		if n%2 == 1 {
			ns = "ns-d"
		}
		root := b.add(spanDef{tenant: "t2", trace: tr, name: "t2-root", service: "t2-secret-svc", ns: ns, dur: 100 * ms,
			spanAttrs: attrs(ns, n, "GET", "alpha"), code: 200})
		b.add(spanDef{tenant: "t2", trace: tr, parent: root, name: "t2-child", service: "t2-svc", ns: ns, start: 10 * ms, dur: 50 * ms,
			status: statusError, spanAttrs: attrs(ns, n, "POST", "alpha"), code: 500})
	}

	// Tenant t3: single-span traces, one per namespace and team. Black-box (GHSA-jcqw-3c3f-xrmj): Tempo 2.x
	// answers tag values unfiltered for a q with || (terms that do not merge into one regex), Tempo 3.x for
	// a q with =~.
	for i, ns := range namespaces {
		for j, team := range teams {
			st := statusOK
			if team == "beta" {
				st = statusError
			}
			b.add(spanDef{tenant: "t3", trace: traceID(600 + 2*i + j), name: "op-" + ns + "-" + team, service: "svc-" + ns, ns: ns,
				dur: 100 * ms, status: st, spanAttrs: map[string]string{"team": team}, code: 200})
		}
	}
	return b.spans
}

// --- OTLP/HTTP JSON push ---------------------------------------------------------------------

type kv struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func strKV(k, v string) kv { return kv{k, map[string]any{"stringValue": v}} }
func intKV(k string, v int) kv {
	return kv{k, map[string]any{"intValue": strconv.Itoa(v)}}
}

// otlpBody renders the spans (all of one tenant) as an OTLP/HTTP JSON ExportTraceServiceRequest. Spans of
// one (trace, service, namespace, resource attributes) share a ResourceSpans entry.
func otlpBody(spans []spanDef, base time.Time) []byte {
	type res struct {
		attrs []kv
		spans []map[string]any
	}
	var order []string
	byRes := map[string]*res{}
	for _, s := range spans {
		key := s.service + "|" + s.ns
		r := byRes[key]
		if r == nil {
			r = &res{}
			rm := s.resource()
			keys := make([]string, 0, len(rm))
			for k := range rm {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				r.attrs = append(r.attrs, strKV(k, rm[k]))
			}
			byRes[key] = r
			order = append(order, key)
		}
		var at []kv
		keys := make([]string, 0, len(s.spanAttrs))
		for k := range s.spanAttrs {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			at = append(at, strKV(k, s.spanAttrs[k]))
		}
		if s.code != 0 {
			at = append(at, intKV("http.status_code", s.code))
		}
		sp := map[string]any{
			"traceId": s.trace, "spanId": s.id, "name": s.name, "kind": s.kind,
			"startTimeUnixNano": strconv.FormatInt(base.Add(s.start).UnixNano(), 10),
			"endTimeUnixNano":   strconv.FormatInt(base.Add(s.start+s.dur).UnixNano(), 10),
			"attributes":        at,
			"status":            map[string]any{"code": s.status},
		}
		if s.parent != "" {
			sp["parentSpanId"] = s.parent
		}
		r.spans = append(r.spans, sp)
	}
	var rs []map[string]any
	for _, k := range order {
		r := byRes[k]
		rs = append(rs, map[string]any{
			"resource":   map[string]any{"attributes": r.attrs},
			"scopeSpans": []map[string]any{{"scope": map[string]any{"name": "e2e"}, "spans": r.spans}},
		})
	}
	b, err := json.Marshal(map[string]any{"resourceSpans": rs})
	if err != nil {
		panic(err)
	}
	return b
}

// push sends spans to Tempo's OTLP/HTTP receiver under the tenant (X-Scope-OrgID).
func push(t testing.TB, otlpURL, tenant string, spans []spanDef, base time.Time) {
	t.Helper()
	if len(spans) == 0 {
		return
	}
	req, err := http.NewRequest(http.MethodPost, otlpURL+"/v1/traces", bytes.NewReader(otlpBody(spans, base))) //nolint:noctx // test-only
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scope-OrgID", tenant)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("otlp push to %s: %d %s", tenant, resp.StatusCode, b)
	}
}

// startTempo starts a Tempo container without waiting for readiness (it needs ~15s); use waitReady.
func startTempo(t testing.TB) *framework.Container {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "tempo.yaml")
	if err := os.WriteFile(cfg, []byte(tempoConfig), 0o644); err != nil { //nolint:gosec // must be readable by the container user
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // must be traversable by the container user
		t.Fatal(err)
	}
	return framework.RunContainer(t, framework.ContainerSpec{
		Image:  tempoImage,
		Ports:  []string{"3200", "4318"},
		Args:   []string{"-config.file=/etc/tempo/tempo.yaml"},
		Mounts: []string{dir + ":/etc/tempo:ro"},
	})
}

func waitReady(t testing.TB, c *framework.Container) {
	t.Helper()
	framework.WaitHTTP(t, c.URL("3200")+"/ready", 90*time.Second)
}
