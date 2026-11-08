//go:build envtest

package receiver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

var (
	restCfg *rest.Config
	direct  client.Client
	scheme  = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	if err := v1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	var err error
	if restCfg, err = env.Start(); err != nil {
		panic(err)
	}
	if direct, err = client.New(restCfg, client.Options{Scheme: scheme}); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func (ca *testCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

func fullBody(t *testing.T, ps ...v1.SpokePolicySource) string {
	t.Helper()
	b, err := json.Marshal(spokesync.Request{Digest: digestOf(t, ps...), Full: true, Policies: ps})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fakeClock is the hub clock of a test. It moves only when the test moves it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	t     *testing.T
	ca    *testCA
	r     *Receiver
	clock *fakeClock
	url   string
}

// newHarness starts a receiver behind its real TLS server. opts can change the Receiver before it serves.
func newHarness(t *testing.T, o Options, opts ...func(*Receiver)) *harness {
	t.Helper()
	ca := newCA(t, "hub agent CA")
	clock := &fakeClock{t: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	o.Client, o.Logger, o.Now = direct, quiet, clock.Now
	if o.RequestBurst == 0 {
		// The fake clock does not refill the buckets. TestRequestLimit tests the limit.
		o.RequestBurst = 1 << 20
	}
	r := New(o)
	for _, f := range opts {
		f(r)
	}
	srv := NewServer("127.0.0.1:0", TLSConfig(ca.issue(t, true), ca.pool()), r, quiet)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	eventually(t, "server ready", srv.Ready)
	return &harness{t: t, ca: ca, r: r, clock: clock, url: "https://" + srv.Addr().String()}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func identityOf(name string) string { return "spiffe://hub.example/agent/" + name }

// agent returns a client cert of the hub CA for the identity of the cluster name.
func (h *harness) agent(name string) tls.Certificate { return h.ca.issue(h.t, false, identityOf(name)) }

func (h *harness) do(cert tls.Certificate, method, path, ctype, body string) (int, []byte, error) {
	h.t.Helper()
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: h.ca.pool(), Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12,
	}}}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequest(method, h.url+path, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// post sends a sync body and returns the status and, for 200, the decoded response.
func (h *harness) post(cert tls.Certificate, body string) (int, *spokesync.Response) {
	h.t.Helper()
	code, b, err := h.do(cert, http.MethodPost, spokesync.Path, spokesync.ContentType, body)
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	if code != http.StatusOK {
		var er spokesync.ErrorResponse
		if err := json.Unmarshal(b, &er); err != nil || er.Error == "" {
			h.t.Fatalf("status %d without an ErrorResponse: %s", code, b)
		}
		return code, nil
	}
	var resp spokesync.Response
	if err := json.Unmarshal(b, &resp); err != nil {
		h.t.Fatal(err)
	}
	return code, &resp
}

func (h *harness) heartbeat(cert tls.Certificate, digest string) (int, *spokesync.Response) {
	h.t.Helper()
	b, _ := json.Marshal(spokesync.Request{Digest: digest})
	return h.post(cert, string(b))
}

func (h *harness) full(cert tls.Certificate, ps ...v1.SpokePolicySource) (int, *spokesync.Response) {
	h.t.Helper()
	return h.post(cert, fullBody(h.t, ps...))
}

// expect checks the status and the result of a sync.
func expect(t *testing.T, what string, code int, resp *spokesync.Response, wantCode int, want spokesync.Result) {
	t.Helper()
	if code != wantCode {
		t.Fatalf("%s: status %d, want %d", what, code, wantCode)
	}
	if wantCode == http.StatusOK && resp.Result != want {
		t.Fatalf("%s: result %q, want %q", what, resp.Result, want)
	}
}

// spoke registers a SpokeCluster and removes it and its objects at the end of the test. Envtest runs no
// garbage collector, so the test deletes the owned objects itself.
func spoke(t *testing.T, name, identity string) *v1.SpokeCluster {
	t.Helper()
	ctx := context.Background()
	sc := &v1.SpokeCluster{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.SpokeClusterSpec{Identity: identity, Tenants: []string{}}}
	if err := direct.Create(ctx, sc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = direct.Delete(ctx, sc)
		for _, o := range storedOf(t, name) {
			_ = direct.Delete(ctx, &o)
		}
	})
	return sc
}

func getSC(t *testing.T, name string) *v1.SpokeCluster {
	t.Helper()
	var sc v1.SpokeCluster
	if err := direct.Get(context.Background(), client.ObjectKey{Name: name}, &sc); err != nil {
		t.Fatal(err)
	}
	return &sc
}

// lastSync returns status.lastSyncTime, or the zero time.
func lastSync(t *testing.T, name string) time.Time {
	t.Helper()
	if ts := getSC(t, name).Status.LastSyncTime; ts != nil {
		return ts.UTC()
	}
	return time.Time{}
}

func storedOf(t *testing.T, cluster string) []v1.SpokePolicy {
	t.Helper()
	var list v1.SpokePolicyList
	if err := direct.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	var out []v1.SpokePolicy
	for _, o := range list.Items {
		if o.Spec.Cluster == cluster {
			out = append(out, o)
		}
	}
	return out
}

func storedDigest(t *testing.T, cluster string) string {
	t.Helper()
	var src []v1.SpokePolicySource
	for _, o := range storedOf(t, cluster) {
		src = append(src, o.Spec.SpokePolicySource)
	}
	return digestOf(t, src...)
}

func getSP(t *testing.T, cluster string, p v1.SpokePolicySource) *v1.SpokePolicy {
	t.Helper()
	var o v1.SpokePolicy
	if err := direct.Get(context.Background(), client.ObjectKey{Name: spokesync.ObjectName(cluster, &p)}, &o); err != nil {
		t.Fatal(err)
	}
	return &o
}

func TestIdentityRejections(t *testing.T) {
	h := newHarness(t, Options{})
	spoke(t, "id-a", identityOf("id-a"))
	spoke(t, "id-b", identityOf("id-b"))
	spoke(t, "id-b2", identityOf("id-b")) // the same identity on two objects
	spoke(t, "id-d", identityOf("other")) // the identity does not name this cluster
	rogue := newCA(t, "rogue CA")
	body := fullBody(t, capSrc("p"))

	// A SpokeCluster with a deletion timestamp: a finalizer keeps it.
	held := spoke(t, "id-e", identityOf("id-e"))
	held.Finalizers = []string{"test.querdian.dev/hold"}
	if err := direct.Update(context.Background(), held); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sc := getSC(t, "id-e")
		sc.Finalizers = nil
		_ = direct.Update(context.Background(), sc)
	})
	if err := direct.Delete(context.Background(), held); err != nil {
		t.Fatal(err)
	}

	for name, cert := range map[string]tls.Certificate{
		"no URI SAN":       h.ca.issue(t, false),
		"two URI SANs":     h.ca.issue(t, false, identityOf("id-a"), identityOf("id-c")),
		"unknown identity": h.agent("nobody"),
		"identity on two":  h.agent("id-b"),
		"name mismatch":    h.agent("other"),
		"suffix identity":  h.ca.issue(t, false, "spiffe://hub.example/agent/x-id-a"),
		"deletion pending": h.agent("id-e"),
	} {
		if code, _ := h.post(cert, body); code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, code)
		}
	}
	// A cert of a different CA fails in the TLS handshake.
	if _, _, err := h.do(rogue.issue(t, false, identityOf("id-a")), http.MethodPost, spokesync.Path, spokesync.ContentType, body); err == nil {
		t.Error("cert of a different CA accepted")
	}
	for _, n := range []string{"id-a", "id-b", "id-b2", "id-d", "id-e"} {
		if !lastSync(t, n).IsZero() || len(storedOf(t, n)) != 0 {
			t.Errorf("%s: a rejected sync changed the state", n)
		}
	}
	code, resp := h.post(h.agent("id-a"), body)
	expect(t, "valid cert", code, resp, http.StatusOK, spokesync.ResultOK)
	if lastSync(t, "id-a").IsZero() || len(storedOf(t, "id-a")) != 1 {
		t.Fatal("valid sync did not store the state")
	}
}

func TestSyncLifecycle(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, Options{})
	sc := spoke(t, "life", identityOf("life"))
	cert := h.agent("life")
	a, b := capSrc("a"), apSrc("ns", "b")

	h.clock.Advance(time.Minute)
	code, resp := h.heartbeat(cert, digestOf(t))
	expect(t, "empty heartbeat", code, resp, http.StatusOK, spokesync.ResultOK)
	got := getSC(t, "life")
	if !got.Status.LastSyncTime.Time.Equal(h.clock.Now()) || got.Status.ObservedGeneration != got.Generation {
		t.Fatalf("status after heartbeat: %+v", got.Status)
	}

	h.clock.Advance(time.Minute)
	code, resp = h.full(cert, a, b)
	expect(t, "full sync", code, resp, http.StatusOK, spokesync.ResultOK)
	if !lastSync(t, "life").Equal(h.clock.Now()) {
		t.Fatal("full sync did not renew")
	}
	if storedDigest(t, "life") != digestOf(t, a, b) {
		t.Fatal("stored state differs")
	}
	for _, p := range []v1.SpokePolicySource{a, b} {
		o := getSP(t, "life", p)
		ref := o.OwnerReferences
		if o.Labels[spokesync.ClusterLabel] != "life" || o.Spec.Cluster != "life" || len(ref) != 1 || ref[0].UID != sc.UID ||
			ref[0].Kind != "SpokeCluster" || ref[0].Name != "life" || ref[0].Controller == nil || !*ref[0].Controller {
			t.Fatalf("object %s: labels %v, owners %+v", o.Name, o.Labels, ref)
		}
	}

	h.clock.Advance(time.Minute)
	renewed := h.clock.Now()
	code, resp = h.heartbeat(cert, digestOf(t, a, b))
	expect(t, "equal heartbeat", code, resp, http.StatusOK, spokesync.ResultOK)
	h.clock.Advance(time.Minute)
	code, resp = h.heartbeat(cert, digestOf(t, a))
	expect(t, "different heartbeat", code, resp, http.StatusOK, spokesync.ResultFullStateRequired)
	if !lastSync(t, "life").Equal(renewed) {
		t.Fatal("a different digest renewed lastSyncTime")
	}

	// Conditions: the Querdian leader writes them; the reply carries them with the spoke generation.
	setReady := func(p v1.SpokePolicySource, status metav1.ConditionStatus, reason, msg string) {
		o := getSP(t, "life", p)
		o.Status.SourceGeneration = p.Generation
		apimeta.SetStatusCondition(&o.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: reason, Message: msg})
		if err := direct.Status().Update(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	setReady(a, metav1.ConditionFalse, policy.ReasonInvalidMatcher, "matcher \"app\": regex does not compile")
	setReady(b, metav1.ConditionTrue, policy.ReasonCrossTenantUnionUnsupported, "unions across tenants with differing rules: metrics")
	code, resp = h.heartbeat(cert, digestOf(t, a, b))
	expect(t, "heartbeat with conditions", code, resp, http.StatusOK, spokesync.ResultOK)
	if len(resp.Conditions) != 2 {
		t.Fatalf("conditions: %+v", resp.Conditions)
	}
	for _, c := range resp.Conditions {
		switch c.Name {
		case "a":
			if c.Ready || c.Reason != policy.ReasonInvalidMatcher || !strings.Contains(c.Message, "regex") || c.SourceGeneration != 1 {
				t.Errorf("a: %+v", c)
			}
		case "b":
			if c.Namespace != "ns" || !c.Ready || c.Reason != policy.ReasonCompiled || strings.Contains(c.Message, "tenant") || c.SourceGeneration != 2 {
				t.Errorf("b leaks or loses data: %+v", c)
			}
		}
	}

	// A full sync with the stored state writes nothing and is not a changed sync.
	before := getSP(t, "life", a).ResourceVersion
	code, resp = h.full(cert, a, b)
	expect(t, "unchanged full sync", code, resp, http.StatusOK, spokesync.ResultOK)
	if getSP(t, "life", a).ResourceVersion != before {
		t.Fatal("an unchanged full sync wrote an object")
	}

	// A changed policy and a removed policy.
	h.clock.Advance(time.Minute)
	a2 := capSrc("a")
	a2.Generation = 2
	a2.ClusterAccessPolicy.Rules[0].Matchers[0].Value = "y"
	code, resp = h.full(cert, a2)
	expect(t, "changed full sync", code, resp, http.StatusOK, spokesync.ResultOK)
	if storedDigest(t, "life") != digestOf(t, a2) || len(storedOf(t, "life")) != 1 {
		t.Fatal("changed state not stored")
	}
	if len(resp.Conditions) != 1 || resp.Conditions[0].Name != "a" {
		t.Fatalf("conditions after the delete: %+v", resp.Conditions)
	}

	// An object without the cluster label is not in the form that the receiver writes: the heartbeat asks
	// for the full state, and the full sync repairs the object.
	o := getSP(t, "life", a2)
	delete(o.Labels, spokesync.ClusterLabel)
	if err := direct.Update(ctx, o); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Minute)
	code, resp = h.heartbeat(cert, digestOf(t, a2))
	expect(t, "heartbeat with a damaged label", code, resp, http.StatusOK, spokesync.ResultFullStateRequired)
	code, resp = h.full(cert, a2)
	expect(t, "repair", code, resp, http.StatusOK, spokesync.ResultOK)
	if getSP(t, "life", a2).Labels[spokesync.ClusterLabel] != "life" {
		t.Fatal("label not repaired")
	}

	// An empty full state removes everything.
	h.clock.Advance(time.Minute)
	code, resp = h.full(cert)
	expect(t, "empty full sync", code, resp, http.StatusOK, spokesync.ResultOK)
	if len(storedOf(t, "life")) != 0 {
		t.Fatal("empty state did not delete")
	}
}

// TestCrossSpokeIsolation shows that the cert of one spoke cannot change the objects of a different spoke,
// also for names that collide when they are joined: cluster a-b with policy c, and cluster a with policy b-c.
func TestCrossSpokeIsolation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, Options{})
	spoke(t, "a-b", identityOf("a-b"))
	spoke(t, "a", identityOf("a"))
	certAB, certA := h.agent("a-b"), h.agent("a")
	c, bc := capSrc("c"), capSrc("b-c")

	code, resp := h.full(certAB, c)
	expect(t, "a-b full", code, resp, http.StatusOK, spokesync.ResultOK)
	code, resp = h.full(certA, bc)
	expect(t, "a full", code, resp, http.StatusOK, spokesync.ResultOK)
	objAB := getSP(t, "a-b", c)
	if objAB.Name == getSP(t, "a", bc).Name {
		t.Fatal("object names collide")
	}

	// An object of a-b with the label of a. The label is not trusted, so a must not delete it.
	planted := &v1.SpokePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "planted", Labels: map[string]string{spokesync.ClusterLabel: "a"}},
		Spec:       v1.SpokePolicySpec{Cluster: "a-b", SpokePolicySource: capSrc("x")},
	}
	if err := direct.Create(ctx, planted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = direct.Delete(ctx, planted) })

	// Spoke a replaces its state with the empty state and with the policies of a-b.
	h.clock.Advance(time.Minute)
	code, resp = h.full(certA)
	expect(t, "a empty", code, resp, http.StatusOK, spokesync.ResultOK)
	code, resp = h.heartbeat(certA, digestOf(t))
	expect(t, "a heartbeat ignores a-b", code, resp, http.StatusOK, spokesync.ResultOK)
	h.clock.Advance(time.Minute)
	code, resp = h.full(certA, c, capSrc("x"))
	expect(t, "a with the names of a-b", code, resp, http.StatusOK, spokesync.ResultOK)
	if getSP(t, "a", c).Name == objAB.Name {
		t.Fatal("a wrote the object of a-b")
	}
	if got := getSP(t, "a-b", c); got.ResourceVersion != objAB.ResourceVersion || got.Spec.Cluster != "a-b" {
		t.Fatal("the object of a-b changed")
	}
	var p v1.SpokePolicy
	if err := direct.Get(ctx, client.ObjectKey{Name: "planted"}, &p); err != nil || p.Spec.Cluster != "a-b" {
		t.Fatalf("planted object of a-b changed: %v", err)
	}
	if storedDigest(t, "a-b") != digestOf(t, c, capSrc("x")) {
		t.Fatal("the state of a-b changed")
	}

	// An object with the name that a would use, but with the cluster a-b: the sync fails and changes nothing.
	z := capSrc("z")
	squat := &v1.SpokePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: spokesync.ObjectName("a", &z)},
		Spec:       v1.SpokePolicySpec{Cluster: "a-b", SpokePolicySource: capSrc("q")},
	}
	if err := direct.Create(ctx, squat); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = direct.Delete(ctx, squat) })
	before := lastSync(t, "a")
	h.clock.Advance(time.Minute)
	if code, _ := h.full(certA, z); code != http.StatusInternalServerError {
		t.Fatalf("sync onto an object of a different cluster: status %d", code)
	}
	if err := direct.Get(ctx, client.ObjectKey{Name: squat.Name}, &p); err != nil || p.Spec.Cluster != "a-b" || p.Spec.Name != "q" {
		t.Fatalf("object of a-b changed: %v", err)
	}
	if !lastSync(t, "a").Equal(before) {
		t.Fatal("failed sync renewed lastSyncTime")
	}
}

func TestConcurrentSyncConflict(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	h := newHarness(t, Options{}, func(r *Receiver) {
		r.afterWrites = func(_ context.Context, cluster string) error {
			if cluster == "busy" {
				close(entered)
				<-release
			}
			return nil
		}
	})
	spoke(t, "busy", identityOf("busy"))
	spoke(t, "free", identityOf("free"))
	busy, free := h.agent("busy"), h.agent("free")

	first := make(chan int, 1)
	go func() {
		code, _, err := h.do(busy, http.MethodPost, spokesync.Path, spokesync.ContentType, fullBody(t, capSrc("p")))
		if err != nil {
			code = -1
		}
		first <- code
	}()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("first sync did not start")
	}
	start := time.Now()
	if code, _ := h.heartbeat(busy, digestOf(t, capSrc("p"))); code != http.StatusConflict {
		t.Errorf("second sync of the same cluster: status %d, want 409", code)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the second sync waited")
	}
	// The identity check above answers first. The lock for each cluster also holds for a second identity
	// of the same cluster, for example during an identity change.
	req := &spokesync.Request{Digest: digestOf(t, capSrc("p"))}
	if _, err := h.r.sync(context.Background(), identityOf("busy"), req); statusOf(unwrap(err)) != http.StatusConflict {
		t.Errorf("cluster lock: %v", err)
	}
	code, resp := h.full(free, capSrc("p"))
	expect(t, "sync of a different cluster", code, resp, http.StatusOK, spokesync.ResultOK)
	close(release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first sync: status %d", code)
	}
	code, resp = h.heartbeat(busy, digestOf(t, capSrc("p")))
	expect(t, "after the first sync", code, resp, http.StatusOK, spokesync.ResultOK)
}

// TestInFlightLimit shows that the receiver rejects a request before it reads the body when too many
// requests are in progress.
func TestInFlightLimit(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	h := newHarness(t, Options{MaxInFlight: 1}, func(r *Receiver) {
		r.afterWrites = func(context.Context, string) error {
			close(entered)
			<-release
			return nil
		}
	})
	spoke(t, "slot-a", identityOf("slot-a"))
	spoke(t, "slot-b", identityOf("slot-b"))
	first := make(chan int, 1)
	go func() {
		code, _, err := h.do(h.agent("slot-a"), http.MethodPost, spokesync.Path, spokesync.ContentType, fullBody(t, capSrc("p")))
		if err != nil {
			code = -1
		}
		first <- code
	}()
	<-entered
	if code, _ := h.full(h.agent("slot-b"), capSrc("p")); code != http.StatusServiceUnavailable {
		t.Errorf("request above the limit: status %d, want 503", code)
	}
	close(release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first sync: status %d", code)
	}
	if !lastSync(t, "slot-b").IsZero() {
		t.Fatal("the rejected request renewed lastSyncTime")
	}
}

// TestSpecChangeDuringSync shows that a sync that overlaps a change of SpokeCluster.spec validates nothing.
func TestSpecChangeDuringSync(t *testing.T) {
	ctx := context.Background()
	var armed atomic.Bool
	h := newHarness(t, Options{}, func(r *Receiver) {
		r.afterWrites = func(ctx context.Context, cluster string) error {
			if cluster == "drift" && armed.CompareAndSwap(true, false) {
				sc := &v1.SpokeCluster{}
				if err := direct.Get(ctx, client.ObjectKey{Name: cluster}, sc); err != nil {
					return err
				}
				sc.Spec.Tenants = []string{"t9"}
				return direct.Update(ctx, sc)
			}
			return nil
		}
	})
	spoke(t, "drift", identityOf("drift"))
	cert := h.agent("drift")
	a, c := capSrc("a"), capSrc("c")

	code, resp := h.full(cert, a)
	expect(t, "first sync", code, resp, http.StatusOK, spokesync.ResultOK)
	before := getSC(t, "drift")

	armed.Store(true)
	h.clock.Advance(time.Minute)
	if code, _ := h.full(cert, c); code != http.StatusConflict {
		t.Fatalf("sync during a spec change: status %d, want 409", code)
	}
	after := getSC(t, "drift")
	if after.Generation == before.Generation {
		t.Fatal("the spec did not change")
	}
	if !after.Status.LastSyncTime.Equal(before.Status.LastSyncTime) || after.Status.ObservedGeneration != before.Status.ObservedGeneration {
		t.Fatalf("the sync validated something: %+v", after.Status)
	}
	if len(storedOf(t, "drift")) != 2 {
		t.Fatal("the sync continued with the deletes after the spec change")
	}
	code, resp = h.heartbeat(cert, digestOf(t, c))
	expect(t, "heartbeat after the failed sync", code, resp, http.StatusOK, spokesync.ResultFullStateRequired)

	// The status write itself has the resourceVersion precondition.
	stale := getSC(t, "drift")
	cur := stale.DeepCopy()
	cur.Spec.Tenants = []string{"t8"}
	if err := direct.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if err := h.r.renew(ctx, stale); statusOf(unwrap(err)) != http.StatusConflict {
		t.Fatalf("renew with a stale read: %v", err)
	}

	h.clock.Advance(time.Minute)
	code, resp = h.full(cert, c)
	expect(t, "next sync", code, resp, http.StatusOK, spokesync.ResultOK)
	if got := getSC(t, "drift"); got.Status.ObservedGeneration != got.Generation || !got.Status.LastSyncTime.Time.Equal(h.clock.Now()) {
		t.Fatalf("next sync did not validate: %+v", got.Status)
	}
}

// unwrap returns the rejection in err.
func unwrap(err error) error {
	var r *rejection
	if errors.As(err, &r) {
		return r
	}
	return err
}

// TestStopBetweenWritesAndDeletes stops a sync after the writes. The stored digest then differs from both
// states, so no heartbeat renews lastSyncTime while a revoked grant stays.
func TestStopBetweenWritesAndDeletes(t *testing.T) {
	var crash atomic.Bool
	h := newHarness(t, Options{}, func(r *Receiver) {
		r.afterWrites = func(context.Context, string) error {
			if crash.CompareAndSwap(true, false) {
				return errors.New("stopped")
			}
			return nil
		}
	})
	spoke(t, "crash", identityOf("crash"))
	cert := h.agent("crash")
	a, b, c := capSrc("a"), capSrc("b"), capSrc("c")
	a2 := capSrc("a")
	a2.Generation = 2

	code, resp := h.full(cert, a, b)
	expect(t, "first sync", code, resp, http.StatusOK, spokesync.ResultOK)
	renewed := lastSync(t, "crash")

	crash.Store(true)
	h.clock.Advance(time.Minute)
	if code, _ := h.full(cert, a2, c); code != http.StatusInternalServerError {
		t.Fatalf("stopped sync: status %d", code)
	}
	if storedDigest(t, "crash") != digestOf(t, a2, b, c) {
		t.Fatal("the writes did not happen before the stop")
	}
	for _, d := range []string{digestOf(t, a2, c), digestOf(t, a, b)} {
		h.clock.Advance(time.Minute)
		code, resp = h.heartbeat(cert, d)
		expect(t, "heartbeat after the stop", code, resp, http.StatusOK, spokesync.ResultFullStateRequired)
	}
	if !lastSync(t, "crash").Equal(renewed) {
		t.Fatal("lastSyncTime was renewed after the stop")
	}
	code, resp = h.full(cert, a2, c)
	expect(t, "repair", code, resp, http.StatusOK, spokesync.ResultOK)
	if storedDigest(t, "crash") != digestOf(t, a2, c) || !lastSync(t, "crash").Equal(h.clock.Now()) {
		t.Fatal("repair failed")
	}
}

// TestUnknownInput rejects the whole sync for each condition under "Unknown input" in PLAN §18.5, and
// lastSyncTime and the stored state do not change.
func TestUnknownInput(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, Options{MaxBodyBytes: 64 << 10, MaxPolicies: 5})
	spoke(t, "strict", identityOf("strict"))
	cert := h.agent("strict")
	a, b := capSrc("a"), apSrc("ns", "b")
	code, resp := h.full(cert, a)
	expect(t, "first sync", code, resp, http.StatusOK, spokesync.ResultOK)

	state := func() (time.Time, string) { return lastSync(t, "strict"), storedDigest(t, "strict") }
	syncTime, digest := state()
	check := func(name string, code, want int) {
		t.Helper()
		if code != want {
			t.Errorf("%s: status %d, want %d", name, code, want)
		}
		if gt, gd := state(); !gt.Equal(syncTime) || gd != digest {
			t.Errorf("%s: the rejected sync changed the state", name)
		}
	}
	rawText := func(name, method, path, ctype, body string, want int, text string) {
		t.Helper()
		h.clock.Advance(time.Minute)
		code, b, err := h.do(cert, method, path, ctype, body)
		if err != nil {
			t.Fatal(err)
		}
		check(name, code, want)
		if !strings.Contains(string(b), text) {
			t.Errorf("%s: the error %s does not contain %q", name, b, text)
		}
	}
	raw := func(name, method, path, ctype, body string, want int) {
		t.Helper()
		rawText(name, method, path, ctype, body, want, "")
	}
	withDigest := func(body string, ps ...v1.SpokePolicySource) string {
		return strings.Replace(body, "DIGEST", digestOf(t, ps...), 1)
	}
	good := fullBody(t, a, b)

	raw("unknown path", http.MethodPost, "/v1alpha1/other", spokesync.ContentType, good, 404)
	raw("query", http.MethodPost, spokesync.Path+"?cluster=x", spokesync.ContentType, good, 404)
	raw("unknown method", http.MethodPut, spokesync.Path, spokesync.ContentType, good, 405)
	raw("unknown content type", http.MethodPost, spokesync.Path, "application/yaml", good, 415)
	raw("unknown field", http.MethodPost, spokesync.Path, spokesync.ContentType, strings.Replace(good, `"full":true`, `"full":true,"cluster":"x"`, 1), 400)
	raw("unknown nested field", http.MethodPost, spokesync.Path, spokesync.ContentType, strings.Replace(good, `"name":"a"`, `"name":"a","cluster":"x"`, 1), 400)
	rawText("duplicate JSON key", http.MethodPost, spokesync.Path, spokesync.ContentType, strings.Replace(good, `"name":"a"`, `"name":"a","name":"z"`, 1), 400, "duplicate key")
	rawText("unknown kind", http.MethodPost, spokesync.Path, spokesync.ContentType,
		withDigest(`{"digest":"DIGEST","full":true,"policies":[{"kind":"Role","name":"a","generation":1}]}`), 400, "unknown kind")

	kindSpec := b
	kindSpec.Kind = v1.SpokeClusterAccessPolicy
	apNoNS := apSrc("", "b")
	capNS := capSrc("a")
	capNS.Namespace = "ns"
	tooMany := []v1.SpokePolicySource{a, b, capSrc("c"), capSrc("d"), capSrc("e"), capSrc("f")}
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"kind does not agree with the spec": {fullBody(t, kindSpec), 400},
		"AccessPolicy without namespace":    {fullBody(t, apNoNS), 400},
		"ClusterAccessPolicy with ns":       {fullBody(t, capNS), 400},
		"duplicate keys":                    {withDigest(mustJSON(t, spokesync.Request{Digest: "DIGEST", Full: true, Policies: []v1.SpokePolicySource{a, a}}), a), 400},
		"digest does not agree":             {withDigest(mustJSON(t, spokesync.Request{Digest: "DIGEST", Full: true, Policies: []v1.SpokePolicySource{a, b}}), a), 400},
		"heartbeat with policies":           {withDigest(mustJSON(t, spokesync.Request{Digest: "DIGEST", Policies: []v1.SpokePolicySource{a}}), a), 400},
		"body above the limit":              {strings.Replace(good, `"name":"a"`, `"name":"a`+strings.Repeat("a", 70<<10)+`"`, 1), 413},
		"object count above the limit":      {fullBody(t, tooMany...), 413},
	} {
		raw(name, http.MethodPost, spokesync.Path, spokesync.ContentType, tc.body, tc.want)
	}

	// More than one changed sync in 10 seconds.
	h.clock.Advance(time.Minute)
	code, resp = h.full(cert, a, b)
	expect(t, "changed sync", code, resp, http.StatusOK, spokesync.ResultOK)
	syncTime, digest = state()
	h.clock.Advance(9 * time.Second)
	code, _ = h.full(cert, a)
	check("second changed sync in 10 seconds", code, 429)
	code, resp = h.heartbeat(cert, digestOf(t, a, b))
	expect(t, "heartbeat is not a changed sync", code, resp, http.StatusOK, spokesync.ResultOK)
	syncTime, digest = state()
	h.clock.Advance(time.Second)
	code, resp = h.full(cert, a)
	expect(t, "changed sync after 10 seconds", code, resp, http.StatusOK, spokesync.ResultOK)
	syncTime, digest = state()

	// Any sync while the SpokeCluster is suspended.
	sc := getSC(t, "strict")
	sc.Spec.Suspended = true
	if err := direct.Update(ctx, sc); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(time.Minute)
	code, _ = h.heartbeat(cert, digestOf(t, a))
	check("heartbeat while suspended", code, 403)
	code, _ = h.full(cert)
	check("full sync while suspended", code, 403)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestLeaderElection shows that only the leader serves and reports ready.
func TestLeaderElection(t *testing.T) {
	ca := newCA(t, "hub agent CA")
	lease := 2 * time.Second
	renew, retry := time.Second, 200*time.Millisecond
	type replica struct {
		srv    *Server
		cancel context.CancelFunc
		done   chan struct{}
	}
	start := func() *replica {
		mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
			Scheme:                        scheme,
			Metrics:                       metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress:        "0",
			LeaderElection:                true,
			LeaderElectionID:              "querdian-receiver-test",
			LeaderElectionNamespace:       "default",
			LeaderElectionReleaseOnCancel: true,
			LeaseDuration:                 &lease,
			RenewDeadline:                 &renew,
			RetryPeriod:                   &retry,
		})
		if err != nil {
			t.Fatal(err)
		}
		rp := &replica{srv: NewServer("127.0.0.1:0", TLSConfig(ca.issue(t, true), ca.pool()), New(Options{Client: direct, Logger: quiet, RequestBurst: 1 << 20}), quiet), done: make(chan struct{})}
		if err := mgr.Add(rp.srv); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		rp.cancel = cancel
		go func() { defer close(rp.done); _ = mgr.Start(ctx) }()
		t.Cleanup(func() { cancel(); <-rp.done })
		return rp
	}
	r1, r2 := start(), start()
	eventually(t, "a leader", func() bool { return r1.srv.Ready() || r2.srv.Ready() })
	leader, other := r1, r2
	if r2.srv.Ready() {
		leader, other = r2, r1
	}
	time.Sleep(2 * lease)
	if !leader.srv.Ready() || other.srv.Ready() || other.srv.Addr() != nil {
		t.Fatalf("leader ready %v, other ready %v, other listens %v", leader.srv.Ready(), other.srv.Ready(), other.srv.Addr())
	}
	ph := ProbeHandler(other.srv.Ready)
	if code := probe(ph); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz of the replica that is not the leader: %d", code)
	}
	leader.cancel()
	<-leader.done
	if leader.srv.Ready() {
		t.Fatal("old leader still ready")
	}
	eventually(t, "failover", other.srv.Ready)
	if code := probe(ProbeHandler(other.srv.Ready)); code != http.StatusOK {
		t.Fatalf("readyz of the new leader: %d", code)
	}
}

func probe(h http.Handler) int {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return w.Code
}
