//go:build e2e

package multicluster

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/test/e2e/framework"
)

// Pinned upstream images: the same versions as the single-cluster suites.
const (
	promImage  = "docker.io/prom/prometheus:v3.16.0"
	lokiImage  = "docker.io/grafana/loki:3.7.9"
	tempoImage = "docker.io/grafana/tempo:3.1.1"
)

const (
	hubCluster = "mla"
	clusterA   = "cluster-a"
	clusterB   = "cluster-b"
	clusterC   = "cluster-c"
	// noCluster is hub data from before the migration: it has no cluster label (PLAN §18.10).
	noCluster = ""

	nsTeam  = "team"
	nsOther = "other"

	tenantHeader = "X-Scope-OrgID"
	// tenantAll is the tenant of the setup "one tenant for all clusters".
	tenantAll = "all"
)

// perClusterTenant is the tenant of a cluster in the setup "one tenant for each cluster". Hub data with no
// cluster label is in the tenant of the hub.
func perClusterTenant(cluster string) string {
	if cluster == noCluster {
		cluster = hubCluster
	}
	return "t-" + cluster
}

// The data set: one record for each cluster value and namespace. All clusters use the same namespace names.
var (
	dataClusters   = []string{hubCluster, clusterA, clusterB, clusterC, noCluster}
	dataNamespaces = []string{nsTeam, nsOther}
)

// rec is one record of the data set: a Prometheus series, a Loki stream or a Tempo trace.
type rec struct{ cluster, ns string }

func (r rec) String() string {
	c := r.cluster
	if c == noCluster {
		c = "<none>"
	}
	return c + "/" + r.ns
}

func allRecords() []rec {
	var out []rec
	for _, c := range dataClusters {
		for _, ns := range dataNamespaces {
			out = append(out, rec{c, ns})
		}
	}
	return out
}

// setup is a tenant setup of PLAN §18.9.
type setup string

const (
	setupNone       setup = "no tenant header"
	setupOne        setup = "one tenant for all clusters"
	setupPerCluster setup = "one tenant for each cluster"
)

// backendRef is one backend of the hub config.
type backendRef struct {
	name, prefix string
	typ          config.BackendType
	setup        setup
}

// Loki and Tempo have all three setups. The setups with a tenant header share one multi-tenant instance of
// each type. A plain Prometheus has no tenants and ignores the tenant header, so metrics-per uses the same
// Prometheus as metrics: it shows what Querdian does for a Prometheus-type backend with a tenant header (as
// in front of Thanos or Mimir), but it cannot show that the backend selects the tenant.
var (
	metrics     = backendRef{"metrics", "/metrics", config.BackendPrometheus, setupNone}
	metricsPer  = backendRef{"metrics-per", "/metrics-per", config.BackendPrometheus, setupPerCluster}
	logs        = backendRef{"logs", "/logs", config.BackendLoki, setupNone}
	logsOne     = backendRef{"logs-one", "/logs-one", config.BackendLoki, setupOne}
	logsPer     = backendRef{"logs-per", "/logs-per", config.BackendLoki, setupPerCluster}
	traces      = backendRef{"traces", "/traces", config.BackendTempo, setupNone}
	tracesOne   = backendRef{"traces-one", "/traces-one", config.BackendTempo, setupOne}
	tracesPer   = backendRef{"traces-per", "/traces-per", config.BackendTempo, setupPerCluster}
	allBackends = []backendRef{metrics, metricsPer, logs, logsOne, logsPer, traces, tracesOne, tracesPer}
	// perClusterBackends are the backends of the setup "one tenant for each cluster".
	perClusterBackends = []backendRef{metricsPer, logsPer, tracesPer}
	// sharedBackends are the setups where one tenant (or none) holds the data of all clusters.
	sharedBackends = []backendRef{metrics, logs, logsOne, traces, tracesOne}
)

func names(refs ...backendRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.name
	}
	return out
}

func nsLabel(t config.BackendType) string {
	if t == config.BackendTempo {
		return "resource.namespace"
	}
	return "namespace"
}

func clusterLabel(t config.BackendType) string {
	if t == config.BackendTempo {
		return "resource.cluster"
	}
	return "cluster"
}

// --- containers ------------------------------------------------------------------------------------

// lokiConfig is a single binary Loki with filesystem storage in the container. %s is auth_enabled.
const lokiConfig = `auth_enabled: %s
server: {http_listen_port: 3100}
common:
  path_prefix: /tmp/loki
  replication_factor: 1
  ring: {kvstore: {store: inmemory}, instance_addr: 127.0.0.1}
  storage: {filesystem: {chunks_directory: /tmp/loki/chunks, rules_directory: /tmp/loki/rules}}
schema_config:
  configs:
    - {from: "2024-01-01", store: tsdb, object_store: filesystem, schema: v13, index: {prefix: index_, period: 24h}}
ingester:
  lifecycler: {min_ready_duration: 0s}
limits_config: {reject_old_samples: false}
querier: {multi_tenant_queries_enabled: true}
query_range: {parallelise_shardable_queries: false}
analytics: {reporting_enabled: false}
`

// tempoConfig is a monolithic Tempo 3 with local storage and OTLP/HTTP ingest. %s is multitenancy_enabled.
const tempoConfig = `stream_over_http_enabled: true
multitenancy_enabled: %s
server:
  http_listen_port: 3200
distributor:
  receivers:
    otlp:
      protocols:
        http:
          endpoint: 0.0.0.0:4318
live_store:
  max_trace_idle: 2s
  max_block_duration: 10s
storage:
  trace:
    backend: local
    wal:
      path: /var/tempo/wal
    local:
      path: /var/tempo/blocks
overrides:
  defaults:
    compaction:
      block_retention: 1h
`

// noTenant is the tenant marker of the data in the single-tenant instances.
const noTenant = "none"

// expectedTenant is the tenant that must hold a record that a user reads through ref. Prometheus has one
// store, so its results carry no tenant.
func expectedTenant(ref backendRef, r rec) string {
	switch {
	case ref.typ == config.BackendPrometheus:
		return ""
	case ref.setup == setupOne:
		return tenantAll
	case ref.setup == setupPerCluster:
		return perClusterTenant(r.cluster)
	}
	return noTenant
}

// backends are the running upstream containers. The single-tenant instances serve the setup with no
// tenant header; the multi-tenant instances serve both setups with a tenant header.
type backends struct {
	prom                 *framework.Container
	lokiSingle, lokiMT   *framework.Container
	tempoSingle, tempoMT *framework.Container
	end                  time.Time
	// traceIDs maps the trimmed trace ID of each seeded trace to its record and tenant. The IDs are unique
	// over all instances and tenants, so a trace in a result shows the tenant that it came from.
	traceIDs map[string]item
}

func (b *backends) url(ref backendRef) string {
	switch {
	case ref.typ == config.BackendPrometheus:
		return b.prom.URL("9090")
	case ref.typ == config.BackendLoki && ref.setup == setupNone:
		return b.lokiSingle.URL("3100")
	case ref.typ == config.BackendLoki:
		return b.lokiMT.URL("3100")
	case ref.setup == setupNone:
		return b.tempoSingle.URL("3200")
	default:
		return b.tempoMT.URL("3200")
	}
}

func writeConfig(t testing.TB, name, content string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "querdian-mc-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // must be traversable by the container user
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil { //nolint:gosec // must be readable by the container user
		t.Fatal(err)
	}
	return dir
}

// startContainers starts the five containers. It does not wait for readiness; waitAndSeed does.
func startContainers(t testing.TB) *backends {
	t.Helper()
	framework.RequireRuntime(t)
	b := &backends{}
	lokiSpec := func(auth string) framework.ContainerSpec {
		dir := writeConfig(t, "loki.yaml", fmt.Sprintf(lokiConfig, auth))
		return framework.ContainerSpec{Image: lokiImage, Ports: []string{"3100"},
			Args: []string{"-config.file=/etc/loki/loki.yaml"}, Mounts: []string{dir + ":/etc/loki:ro"}}
	}
	tempoSpec := func(mt string) framework.ContainerSpec {
		dir := writeConfig(t, "tempo.yaml", fmt.Sprintf(tempoConfig, mt))
		return framework.ContainerSpec{Image: tempoImage, Ports: []string{"3200", "4318"},
			Args: []string{"-config.file=/etc/tempo/tempo.yaml"}, Mounts: []string{dir + ":/etc/tempo:ro"}}
	}
	b.prom = framework.RunContainer(t, framework.ContainerSpec{Image: promImage, Ports: []string{"9090"}, Args: []string{
		"--config.file=/etc/prometheus/prometheus.yml", "--storage.tsdb.path=/prometheus",
		"--web.enable-remote-write-receiver", "--storage.tsdb.retention.time=1d",
	}})
	b.lokiSingle = framework.RunContainer(t, lokiSpec("false"))
	b.lokiMT = framework.RunContainer(t, lokiSpec("true"))
	b.tempoSingle = framework.RunContainer(t, tempoSpec("false"))
	b.tempoMT = framework.RunContainer(t, tempoSpec("true"))
	return b
}

// waitAndSeed waits until all containers are ready, in parallel, and seeds the data set.
func (b *backends) waitAndSeed(t testing.TB) {
	t.Helper()
	ready := []string{
		b.prom.URL("9090") + "/-/ready", b.lokiSingle.URL("3100") + "/ready", b.lokiMT.URL("3100") + "/ready",
		b.tempoSingle.URL("3200") + "/ready", b.tempoMT.URL("3200") + "/ready",
	}
	var wg sync.WaitGroup
	for _, u := range ready {
		wg.Add(1)
		go func() { defer wg.Done(); waitReady(u, 3*time.Minute) }()
	}
	wg.Wait()
	for _, u := range ready {
		framework.WaitHTTP(t, u, 5*time.Second) // fails the test with the cause if a wait above timed out
	}
	b.end = time.Now().Truncate(time.Second)
	b.seed(t)
}

// waitReady polls url until it answers 200 or the timeout expires. The caller checks the result again with
// framework.WaitHTTP on the test goroutine.
func waitReady(u string, timeout time.Duration) {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		resp, err := http.Get(u) //nolint:gosec,noctx // test-only
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
	}
}

// --- seeding ---------------------------------------------------------------------------------------

// labelsOf are the labels of a record. A record with no cluster has no cluster label.
func labelsOf(r rec) map[string]string {
	l := map[string]string{"namespace": r.ns, "app": "mc"}
	if r.cluster != noCluster {
		l["cluster"] = r.cluster
	}
	return l
}

// tenantsOf returns the tenant of each record in the multi-tenant instances: each record is in tenant "all"
// and in the tenant of its cluster.
func tenantsOf(r rec) []string { return []string{tenantAll, perClusterTenant(r.cluster)} }

func (b *backends) seed(t testing.TB) {
	t.Helper()
	recs := allRecords()
	b.seedPrometheus(t, recs)

	// Loki: all records in the single-tenant instance; each record twice in the multi-tenant instance.
	pushLoki(t, b.lokiSingle.URL("3100"), "", recs, b.end, "seed")
	byTenant := map[string][]rec{}
	for _, r := range recs {
		for _, tn := range tenantsOf(r) {
			byTenant[tn] = append(byTenant[tn], r)
		}
	}
	for tn, rs := range byTenant {
		pushLoki(t, b.lokiMT.URL("3100"), tn, rs, b.end, "seed")
	}

	// Tempo: one single-span trace for each record and tenant. Trace IDs are unique over the data set.
	b.traceIDs = map[string]item{}
	n := 0
	traceID := func(r rec, tenant string) string {
		n++
		id := fmt.Sprintf("a%031x", n)
		b.traceIDs[trimID(id)] = item{r, tenant}
		return id
	}
	var single []traceDef
	for _, r := range recs {
		single = append(single, traceDef{id: traceID(r, noTenant), r: r})
	}
	pushTempo(t, b.tempoSingle.URL("4318"), "", single, b.end)
	tenants := make([]string, 0, len(byTenant))
	for tn := range byTenant {
		tenants = append(tenants, tn)
	}
	sort.Strings(tenants)
	for _, tn := range tenants {
		var ts []traceDef
		for _, r := range byTenant[tn] {
			ts = append(ts, traceDef{id: traceID(r, tn), r: r})
		}
		pushTempo(t, b.tempoMT.URL("4318"), tn, ts, b.end)
	}

	// Wait until each instance and tenant returns its records.
	type check struct {
		ref    backendRef
		c      *framework.Client
		tenant string
		want   int
	}
	var checks []check
	direct := func(ref backendRef, tenant string) *framework.Client {
		c := framework.NewClient(t, b.url(ref))
		if tenant != "" {
			c.Headers = map[string]string{tenantHeader: tenant}
		}
		return c
	}
	checks = append(checks, check{metrics, direct(metrics, ""), "", len(recs)},
		check{logs, direct(logs, ""), "", len(recs)}, check{traces, direct(traces, ""), "", len(recs)})
	for _, tn := range tenants {
		checks = append(checks, check{logsOne, direct(logsOne, tn), tn, len(byTenant[tn])},
			check{tracesOne, direct(tracesOne, tn), tn, len(byTenant[tn])})
	}
	for _, c := range checks {
		eventually(t, 2*time.Minute, fmt.Sprintf("seed of %s tenant %q", c.ref.name, c.tenant), func() (bool, string) {
			res := b.queryAt(t, c.c, backendRef{typ: c.ref.typ}, "")
			return res.status == 200 && len(res.itemSet()) == c.want, res.String()
		})
	}
}

func (b *backends) seedPrometheus(t testing.TB, recs []rec) {
	t.Helper()
	var wr []byte
	for _, r := range recs {
		l := labelsOf(r)
		l["__name__"] = "mc_records"
		keys := make([]string, 0, len(l))
		for k := range l {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var ts []byte
		for _, k := range keys {
			var lb []byte
			lb = protowire.AppendTag(lb, 1, protowire.BytesType)
			lb = protowire.AppendString(lb, k)
			lb = protowire.AppendTag(lb, 2, protowire.BytesType)
			lb = protowire.AppendString(lb, l[k])
			ts = protowire.AppendTag(ts, 1, protowire.BytesType)
			ts = protowire.AppendBytes(ts, lb)
		}
		for i := 8; i >= 0; i-- {
			var sm []byte
			sm = protowire.AppendTag(sm, 1, protowire.Fixed64Type)
			sm = protowire.AppendFixed64(sm, math.Float64bits(1))
			sm = protowire.AppendTag(sm, 2, protowire.VarintType)
			sm = protowire.AppendVarint(sm, uint64(b.end.Add(-time.Duration(i)*30*time.Second).UnixMilli())) //nolint:gosec // positive
			ts = protowire.AppendTag(ts, 2, protowire.BytesType)
			ts = protowire.AppendBytes(ts, sm)
		}
		wr = protowire.AppendTag(wr, 1, protowire.BytesType)
		wr = protowire.AppendBytes(wr, ts)
	}
	req, err := http.NewRequest(http.MethodPost, b.prom.URL("9090")+"/api/v1/write", bytes.NewReader(snappyLiteral(wr))) //nolint:noctx // test-only
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
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("remote write: %d %s", resp.StatusCode, body)
	}
}

// snappyLiteral wraps data in a valid snappy block with literal elements only (no compression).
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

// pushLoki sends one stream for each record, with three lines that end at end. tenant "" sends no tenant
// header. Each line starts with the marker and names the tenant, so that a result shows the tenant that it
// came from. The ingester ring can need a moment after readiness, so failed pushes are retried.
func pushLoki(t testing.TB, base, tenant string, recs []rec, end time.Time, marker string) {
	t.Helper()
	type stream struct {
		Stream map[string]string `json:"stream"`
		Values [][]string        `json:"values"`
	}
	var body struct {
		Streams []stream `json:"streams"`
	}
	for _, r := range recs {
		s := stream{Stream: labelsOf(r)}
		for i := 2; i >= 0; i-- {
			ts := end.Add(-time.Duration(i) * 10 * time.Second).UnixNano()
			s.Values = append(s.Values, []string{strconv.FormatInt(ts, 10), fmt.Sprintf("%s tenant=%s record=%s line=%d", marker, lineTenant(tenant), r, i)})
		}
		body.Streams = append(body.Streams, s)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for range 60 {
		req, err := http.NewRequest(http.MethodPost, base+"/loki/api/v1/push", bytes.NewReader(raw)) //nolint:noctx // test-only
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if tenant != "" {
			req.Header.Set(tenantHeader, tenant)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return
			}
			last = fmt.Sprintf("%d %s", resp.StatusCode, b)
		} else {
			last = err.Error()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("push to Loki %s as %q: %s", base, tenant, last)
}

// lineTenant is the tenant marker in a Loki line.
func lineTenant(tenant string) string {
	if tenant == "" {
		return noTenant
	}
	return tenant
}

// tenantOfLine returns the tenant marker of a Loki line, or "?" if the line has none.
func tenantOfLine(line string) string {
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, "tenant="); ok {
			return v
		}
	}
	return "?"
}

// traceDef is one seeded trace with one span.
type traceDef struct {
	id string
	r  rec
}

// pushTempo sends the traces with OTLP/HTTP JSON. tenant "" sends no tenant header.
func pushTempo(t testing.TB, base, tenant string, traces []traceDef, end time.Time) {
	t.Helper()
	kv := func(k, v string) map[string]any {
		return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
	}
	var rs []map[string]any
	for i, tr := range traces {
		attrs := []map[string]any{kv("service.name", "svc-"+strings.ReplaceAll(tr.r.String(), "/", "-")), kv("namespace", tr.r.ns)}
		if tr.r.cluster != noCluster {
			attrs = append(attrs, kv("cluster", tr.r.cluster))
		}
		start := end.Add(-30 * time.Second)
		rs = append(rs, map[string]any{
			"resource": map[string]any{"attributes": attrs},
			"scopeSpans": []map[string]any{{"scope": map[string]any{"name": "e2e"}, "spans": []map[string]any{{
				"traceId": tr.id, "spanId": fmt.Sprintf("b%015x", i+1), "name": "op", "kind": 2,
				"startTimeUnixNano": strconv.FormatInt(start.UnixNano(), 10),
				"endTimeUnixNano":   strconv.FormatInt(start.Add(100*time.Millisecond).UnixNano(), 10),
				"status":            map[string]any{"code": 1},
			}}}},
		})
	}
	raw, err := json.Marshal(map[string]any{"resourceSpans": rs})
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for range 60 {
		req, err := http.NewRequest(http.MethodPost, base+"/v1/traces", bytes.NewReader(raw)) //nolint:noctx // test-only
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if tenant != "" {
			req.Header.Set(tenantHeader, tenant)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return
			}
			last = fmt.Sprintf("%d %s", resp.StatusCode, b)
		} else {
			last = err.Error()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("push to Tempo %s as %q: %s", base, tenant, last)
}

// traceID returns the ID of the seeded trace of the record in the tenant (noTenant for the single-tenant
// instance).
func (b *backends) traceID(t testing.TB, r rec, tenant string) string {
	t.Helper()
	for id, it := range b.traceIDs {
		if it.r == r && it.tenant == tenant {
			return id // the IDs start with "a", so the trimmed ID is the full ID
		}
	}
	t.Fatalf("no seeded trace for %s in tenant %q", r, tenant)
	return ""
}

// trimID makes trace IDs comparable: Tempo removes leading zeros in search results.
func trimID(s string) string {
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0"
	}
	return s
}

// --- queries ---------------------------------------------------------------------------------------

// item is one record in a result, with the tenant that it came from ("" for Prometheus).
type item struct {
	r      rec
	tenant string
}

func (i item) String() string {
	if i.tenant == "" {
		return i.r.String()
	}
	return i.r.String() + "@" + i.tenant
}

// result is the answer to one query: the status and the records in the data.
type result struct {
	status int
	items  []item
	body   string
}

func (r result) String() string {
	return fmt.Sprintf("status %d, records %v, body %.300s", r.status, r.itemSet(), r.body)
}

// recs returns the records of the result without the tenants.
func (r result) recs() []rec {
	out := make([]rec, len(r.items))
	for i, x := range r.items {
		out[i] = x.r
	}
	return out
}

// itemSet returns the sorted records with their tenants as text, for comparisons. A Loki result has one
// entry for each line, so equal entries are removed.
func (r result) itemSet() []string {
	out := make([]string, 0, len(r.items))
	for _, x := range r.items {
		out = append(out, x.String())
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// query asks a backend through the proxy (prefix of ref) for all records. If cluster is not empty, the
// query itself selects that cluster, as a user who tries to read the data of a different cluster.
func (b *backends) query(t testing.TB, c *framework.Client, ref backendRef, cluster string) result {
	t.Helper()
	return b.queryPath(t, c, ref.prefix, ref.typ, cluster)
}

// queryAt asks a backend directly (the client base URL is the upstream).
func (b *backends) queryAt(t testing.TB, c *framework.Client, ref backendRef, cluster string) result {
	t.Helper()
	return b.queryPath(t, c, "", ref.typ, cluster)
}

func (b *backends) queryPath(t testing.TB, c *framework.Client, prefix string, typ config.BackendType, cluster string) result {
	t.Helper()
	from, to := b.end.Add(-10*time.Minute), b.end.Add(10*time.Minute)
	var resp *framework.Response
	switch typ {
	case config.BackendPrometheus:
		q := "mc_records"
		if cluster != "" {
			q = fmt.Sprintf(`mc_records{cluster=%q}`, cluster)
		}
		resp = c.Get(prefix+"/api/v1/query", url.Values{"query": {q}, "time": {strconv.FormatInt(b.end.Unix(), 10)}})
	case config.BackendLoki:
		q := `{app="mc"}`
		if cluster != "" {
			q = fmt.Sprintf(`{app="mc", cluster=%q}`, cluster)
		}
		resp = c.Get(prefix+"/loki/api/v1/query_range", url.Values{"query": {q}, "limit": {"1000"},
			"start": {strconv.FormatInt(from.UnixNano(), 10)}, "end": {strconv.FormatInt(to.UnixNano(), 10)}})
	default:
		q := "{}"
		if cluster != "" {
			q = fmt.Sprintf(`{ resource.cluster = %q }`, cluster)
		}
		resp = c.Get(prefix+"/api/search", url.Values{"q": {q}, "limit": {"1000"},
			"start": {strconv.FormatInt(from.Unix(), 10)}, "end": {strconv.FormatInt(to.Unix(), 10)}})
	}
	res := result{status: resp.Status, body: string(resp.Body)}
	if resp.Status != http.StatusOK {
		return res
	}
	switch typ {
	case config.BackendPrometheus, config.BackendLoki:
		var d struct {
			Data struct {
				Result []struct {
					Metric map[string]string `json:"metric"`
					Stream map[string]string `json:"stream"`
					Values [][]any           `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}
		resp.JSON(t, &d)
		for _, x := range d.Data.Result {
			if typ == config.BackendPrometheus {
				res.items = append(res.items, item{r: rec{x.Metric["cluster"], x.Metric["namespace"]}})
				continue
			}
			r := rec{x.Stream["cluster"], x.Stream["namespace"]}
			for _, v := range x.Values {
				line, _ := v[1].(string)
				res.items = append(res.items, item{r, tenantOfLine(line)})
			}
		}
	default:
		var d struct {
			Traces []struct {
				TraceID string `json:"traceID"`
			} `json:"traces"`
		}
		resp.JSON(t, &d)
		for _, x := range d.Traces {
			it, ok := b.traceIDs[trimID(x.TraceID)]
			if !ok {
				t.Fatalf("search returned an unknown trace %s", x.TraceID)
			}
			res.items = append(res.items, it)
		}
	}
	return res
}

// want returns the sorted text form of the records, each with the tenant that must hold it on ref, for
// comparisons with result.itemSet.
func want(ref backendRef, recs ...rec) []string {
	items := make([]item, len(recs))
	for i, r := range recs {
		items[i] = item{r, expectedTenant(ref, r)}
	}
	return wantItems(items...)
}

// wantItems returns the sorted text form of the items.
func wantItems(items ...item) []string {
	out := make([]string, 0, len(items))
	for _, x := range items {
		out = append(out, x.String())
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// inCluster returns the records of the data set in the clusters.
func inCluster(clusters ...string) []rec {
	var out []rec
	for _, r := range allRecords() {
		if slices.Contains(clusters, r.cluster) {
			out = append(out, r)
		}
	}
	return out
}

// inNamespace keeps the records of one namespace.
func inNamespace(ns string, recs []rec) []rec {
	var out []rec
	for _, r := range recs {
		if r.ns == ns {
			out = append(out, r)
		}
	}
	return out
}
