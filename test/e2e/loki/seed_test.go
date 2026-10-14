//go:build e2e

// Package loki holds the Loki end-to-end suite (T3.8): a real Loki 3.5.0 container (black box) seeded
// with log streams of several namespaces and two tenants, a reference Loki holding only the granted
// streams per policy (the oracle), the real proxy handler chain in-process, and a test OIDC issuer.
package loki

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/test/e2e/framework"
)

// Pinned upstream image (T0.4/T0.5/T1.8 research used the same version).
const lokiImage = "docker.io/grafana/loki:3.5.0"

// lokiConfig: single binary, auth enabled (tenants), tenant federation, filesystem storage in the
// container's own (ephemeral, removed with -v) file system, pattern ingester and volume endpoints on. Ingester
// readiness delay and query sharding are switched off: they only add latency for this tiny data set
// (query splitting must stay on: series answers nothing for flushed data without it).
const lokiConfig = `auth_enabled: true
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
limits_config: {allow_structured_metadata: true, reject_old_samples: false, volume_enabled: true}
querier: {multi_tenant_queries_enabled: true, max_concurrent: 32}
query_range: {parallelise_shardable_queries: false}
pattern_ingester: {enabled: true}
analytics: {reporting_enabled: false}
`

const (
	linesPerStream = 24
	lineStep       = 10 * time.Second
)

var (
	tenantNamespaces = map[string][]string{
		"t1": {"ns-a", "ns-b", "ns-c"},
		"t2": {"ns-b", "ns-c", "ns-d"},
	}
	allTenants = []string{"t1", "t2"}
	apps       = []string{"web", "api"}
)

// entry is one log line; ts is in nanoseconds since the epoch.
type entry struct {
	ts   int64
	line string
}

// streamDef is one seeded stream (labels without the ones Loki derives itself).
type streamDef struct {
	tenant  string
	labels  map[string]string
	entries []entry
}

func (s streamDef) lbls() labels.Labels {
	kv := make([]string, 0, 2*len(s.labels))
	for k, v := range s.labels {
		kv = append(kv, k, v)
	}
	return labels.FromStrings(kv...)
}

// allStreams is the deterministic data set: per tenant three namespaces x two apps (web logs logfmt, api
// logs JSON). The namespaces ns-b and ns-c exist in both tenants with different content (overlapping),
// ns-a only in t1 and ns-d only in t2. Every stream carries lines with identical timestamps: a
// "dup-ts" line sharing the timestamp of a regular one and a "heartbeat" line that has the same
// timestamp and text in every stream.
func allStreams(end time.Time) []streamDef {
	var out []streamDef
	for _, tenant := range allTenants {
		for ni, ns := range tenantNamespaces[tenant] {
			for ai, app := range apps {
				env := "prod"
				if app == "api" {
					env = "dev"
				}
				s := streamDef{tenant: tenant, labels: map[string]string{
					"k8s_namespace_name": ns, "app": app, "pod": app + "-" + ns + "-0", "env": env,
				}}
				at := func(i int) int64 { return end.Add(-time.Duration(linesPerStream-1-i) * lineStep).UnixNano() }
				// Streams differ in length (and so in volume) to avoid ties between series sums.
				for i := 3*ni + ai; i < linesPerStream; i++ {
					level, status := "info", 200
					switch i % 4 {
					case 2:
						level = "warn"
					case 3:
						level, status = "error", 500
					}
					var line string
					if app == "web" {
						line = fmt.Sprintf(`level=%s msg="request %d" duration=%dms status=%d tenant=%s ns=%s`,
							level, i, 10+7*i+ni*3, status, tenant, ns)
					} else {
						line = fmt.Sprintf(`{"level":%q,"msg":"call %d","latency":%.2f,"status":%d,"tenant":%q,"ns":%q}`,
							level, i, 0.05*float64(i+1)+0.01*float64(ni+ai), status, tenant, ns)
					}
					s.entries = append(s.entries, entry{at(i), line})
					if i == 9 { // same timestamp as the line above, different text
						dup := fmt.Sprintf(`level=debug msg="dup-ts %s %s" duration=1ms status=200`, tenant, ns)
						if app == "api" {
							dup = fmt.Sprintf(`{"level":"debug","msg":"dup-ts","latency":0.01,"status":200,"tenant":%q,"ns":%q}`, tenant, ns)
						}
						s.entries = append(s.entries, entry{at(i), dup})
					}
					if i == 18 { // same timestamp and text in every stream
						s.entries = append(s.entries, entry{at(i), "heartbeat"})
					}
				}
				out = append(out, s)
			}
		}
	}
	return out
}

// --- term sets ------------------------------------------------------------------------------------

func eq(n, v string) v1.Matcher  { return framework.Eq(n, v) }
func ne(n, v string) v1.Matcher  { return framework.Ne(n, v) }
func re(n, v string) v1.Matcher  { return framework.Re(n, v) }
func nre(n, v string) v1.Matcher { return framework.NRe(n, v) }

const nsLabel = "k8s_namespace_name"

// tterm is one DNF term of a term set. tenant "" means the term applies to every tenant of the set.
type tterm struct {
	tenant string
	ms     []v1.Matcher
}

// termSet is a policy scenario: a group whose members are granted the terms, plus the expected
// visibility used to seed the reference instance (computed here, not by the policy engine).
type termSet struct {
	name    string
	group   string
	tenants []string // tenants of the grant (tenant header at the upstream)
	terms   []tterm
	denied  []string
	// unrestricted grants everything of the tenants.
	unrestricted bool
	// multi: expected to resolve to more than one DNF term (fan-out / or-lifting); checked at runtime.
	multi bool
	// disjoint: the terms provably never select the same stream (exact sum endpoints).
	disjoint bool
}

var termSets = []termSet{
	{name: "one", group: "g-one", tenants: []string{"t1"}, terms: []tterm{{"", []v1.Matcher{eq(nsLabel, "ns-a")}}}, disjoint: true},
	{name: "multi-matcher", group: "g-mm", tenants: []string{"t1"}, terms: []tterm{{"", []v1.Matcher{re(nsLabel, "ns-(a|b)"), ne("app", "api")}}}, disjoint: true},
	{name: "two", group: "g-two", tenants: []string{"t1"}, multi: true, disjoint: true, terms: []tterm{
		{"", []v1.Matcher{eq(nsLabel, "ns-a")}},
		{"", []v1.Matcher{eq(nsLabel, "ns-b"), eq("app", "api")}},
	}},
	{name: "overlap", group: "g-overlap", tenants: []string{"t1"}, multi: true, terms: []tterm{
		{"", []v1.Matcher{eq(nsLabel, "ns-a")}},
		{"", []v1.Matcher{eq("app", "web")}},
	}},
	{name: "three", group: "g-three", tenants: []string{"t1"}, multi: true, terms: []tterm{
		{"", []v1.Matcher{eq(nsLabel, "ns-a")}},
		{"", []v1.Matcher{eq(nsLabel, "ns-b"), eq("app", "api")}},
		{"", []v1.Matcher{re("pod", "web-ns-c-.*")}},
	}},
	{name: "mt-same", group: "g-mts", tenants: []string{"t1", "t2"}, disjoint: true, terms: []tterm{
		{"", []v1.Matcher{eq(nsLabel, "ns-b")}},
	}},
	{name: "mt-diff", group: "g-mtd", tenants: []string{"t1", "t2"}, multi: true, disjoint: true, terms: []tterm{
		{"t1", []v1.Matcher{eq(nsLabel, "ns-a")}},
		{"t2", []v1.Matcher{eq(nsLabel, "ns-d")}},
	}},
	{name: "mt-diff-overlap", group: "g-mto", tenants: []string{"t1", "t2"}, multi: true, terms: []tterm{
		{"t1", []v1.Matcher{eq(nsLabel, "ns-b")}},
		{"t2", []v1.Matcher{eq(nsLabel, "ns-b")}},
		{"t2", []v1.Matcher{eq("app", "api")}},
	}},
	{name: "mt-multi", group: "g-mtm", tenants: []string{"t1", "t2"}, multi: true, disjoint: true, terms: []tterm{
		{"t1", []v1.Matcher{eq(nsLabel, "ns-a")}},
		{"t1", []v1.Matcher{eq(nsLabel, "ns-b"), eq("app", "api")}},
		{"t2", []v1.Matcher{eq(nsLabel, "ns-c")}},
	}},
	{name: "mt-not", group: "g-mtn", tenants: []string{"t1", "t2"}, multi: true, disjoint: true, terms: []tterm{
		{"t1", []v1.Matcher{nre(nsLabel, "ns-(a|c)")}},
		{"t2", []v1.Matcher{ne(nsLabel, "ns-b"), eq("app", "web")}},
	}},
	{name: "unrestricted", group: "g-unr", tenants: []string{"t1"}, unrestricted: true},
	{name: "unrestricted-mt", group: "g-unrmt", tenants: []string{"t1", "t2"}, unrestricted: true},
	{name: "deny-one", group: "g-deny1", tenants: []string{"t1"}, denied: []string{"pod"}, disjoint: true,
		terms: []tterm{{"", []v1.Matcher{eq(nsLabel, "ns-a")}}}},
	{name: "deny-multi", group: "g-deny2", tenants: []string{"t1"}, denied: []string{"pod"}, multi: true, disjoint: true,
		terms: []tterm{
			{"", []v1.Matcher{eq(nsLabel, "ns-a")}},
			{"", []v1.Matcher{eq(nsLabel, "ns-b"), eq("app", "api")}},
		}},
}

func toMatchers(t testing.TB, ms []v1.Matcher) []*labels.Matcher {
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

// visible reports whether the stream is granted by the set (independent of the policy engine).
func (ts termSet) visible(t testing.TB, s streamDef) bool {
	t.Helper()
	granted := false
	for _, tn := range ts.tenants {
		granted = granted || tn == s.tenant
	}
	if !granted {
		return false
	}
	if ts.unrestricted {
		return true
	}
	l := s.lbls()
next:
	for _, term := range ts.terms {
		if term.tenant != "" && term.tenant != s.tenant {
			continue
		}
		for _, m := range toMatchers(t, term.ms) {
			if !m.Matches(l.Get(m.Name)) {
				continue next
			}
		}
		return true
	}
	return false
}

// refTenant is the tenant of the reference instance that holds the granted streams of one tenant of a set.
func refTenant(set, tenant string) string { return set + "-" + tenant }

// header is the X-Scope-OrgID of the oracle for the set ("a|b" for several tenants).
func (ts termSet) refHeader() string {
	h := ""
	for i, tn := range ts.tenants {
		if i > 0 {
			h += "|"
		}
		h += refTenant(ts.name, tn)
	}
	return h
}

// rename maps the reference tenant names in __tenant_id__ values back to the real ones.
func (ts termSet) rename() map[string]string {
	m := map[string]string{}
	for _, tn := range ts.tenants {
		m[refTenant(ts.name, tn)] = tn
	}
	return m
}

// --- containers and seeding ------------------------------------------------------------------------

// startLokis starts the main and the reference Loki concurrently and returns when both are ready.
func startLokis(t testing.TB) (mainURL, refURL string) {
	t.Helper()
	framework.RequireRuntime(t)
	dir, err := os.MkdirTemp("", "querdian-loki-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg := filepath.Join(dir, "loki.yaml")
	if err := os.WriteFile(cfg, []byte(lokiConfig), 0o644); err != nil { //nolint:gosec // must be readable by the container user
		t.Fatal(err)
	}
	spec := framework.ContainerSpec{
		Image:  lokiImage,
		Ports:  []string{"3100"},
		Args:   []string{"-config.file=/etc/loki/c.yaml"},
		Mounts: []string{cfg + ":/etc/loki/c.yaml:ro"},
	}
	main := framework.RunContainer(t, spec)
	ref := framework.RunContainer(t, spec)
	for _, c := range []*framework.Container{main, ref} {
		framework.WaitHTTP(t, c.URL("3100")+"/ready", 3*time.Minute)
	}
	return main.URL("3100"), ref.URL("3100")
}

// push sends the streams of one tenant, retrying while the ingester ring is still settling.
func push(t testing.TB, base, tenant string, streams []streamDef) {
	t.Helper()
	type pstream struct {
		Stream map[string]string `json:"stream"`
		Values [][]string        `json:"values"`
	}
	var body struct {
		Streams []pstream `json:"streams"`
	}
	for _, s := range streams {
		ps := pstream{Stream: s.labels}
		for _, e := range s.entries {
			ps.Values = append(ps.Values, []string{strconv.FormatInt(e.ts, 10), e.line})
		}
		body.Streams = append(body.Streams, ps)
	}
	if len(body.Streams) == 0 {
		return
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for range 40 {
		req, err := http.NewRequest(http.MethodPost, base+"/loki/api/v1/push", bytes.NewReader(raw)) //nolint:noctx // test-only
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scope-OrgID", tenant)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			return
		}
		last = fmt.Sprintf("%d %s", resp.StatusCode, b.String())
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("push to %s as %s: %s", base, tenant, last)
}

// flush forces the ingesters to write their chunks so that index/stats and volume see the data.
func flush(t testing.TB, base string) {
	t.Helper()
	resp, err := http.Post(base+"/flush", "", nil) //nolint:noctx // test-only
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

// seed loads the full data into main (tenants t1, t2) and the granted subsets into ref (tenant
// "<set>-<tenant>").
func seed(t testing.TB, mainURL, refURL string, end time.Time) {
	t.Helper()
	all := allStreams(end)
	for _, tn := range allTenants {
		var ss []streamDef
		for _, s := range all {
			if s.tenant == tn {
				ss = append(ss, s)
			}
		}
		push(t, mainURL, tn, ss)
	}
	for _, set := range termSets {
		for _, tn := range set.tenants {
			var ss []streamDef
			for _, s := range all {
				if s.tenant == tn && set.visible(t, s) {
					ss = append(ss, s)
				}
			}
			push(t, refURL, refTenant(set.name, tn), ss)
		}
	}
	flush(t, mainURL)
	flush(t, refURL)
	time.Sleep(3 * time.Second)
}
