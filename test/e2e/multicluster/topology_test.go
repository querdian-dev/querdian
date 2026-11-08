//go:build e2e

package multicluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/agent"
	"github.com/querdian-dev/querdian/internal/auth"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/controller"
	"github.com/querdian-dev/querdian/internal/enforce"
	enforcelogql "github.com/querdian-dev/querdian/internal/enforce/logql"
	enforcepromql "github.com/querdian-dev/querdian/internal/enforce/promql"
	enforcetraceql "github.com/querdian-dev/querdian/internal/enforce/traceql"
	"github.com/querdian-dev/querdian/internal/observability"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/receiver"
	"github.com/querdian-dev/querdian/internal/server"
	"github.com/querdian-dev/querdian/internal/spokesync"
	"github.com/querdian-dev/querdian/test/e2e/framework"
)

const (
	// heartbeat is the agent interval. The binary requires at least 10s; the suite uses the agent package
	// directly, so that the time limit and the suspension tests stay short.
	heartbeat = time.Second
	// agentNamespace holds the leader election lease of the agent in each spoke.
	agentNamespace = "querdian-system"
	// defaultMaxStaleness is multicluster.maxStaleness of the hub config. The expiry test sets a short
	// spec.maxStaleness on one SpokeCluster.
	defaultMaxStaleness = time.Hour
)

// scheme has every kind that the suite reads or writes.
var scheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, v1.AddToScheme, rbacv1.AddToScheme, coordinationv1.AddToScheme} {
		if err := add(s); err != nil {
			panic(err)
		}
	}
	return s
}()

// --- logs ------------------------------------------------------------------------------------------

// logSink keeps the last log lines of all in-process components. The test prints them only if it fails.
// The components log from goroutines that can outlive a subtest, so they must not write to t.Log.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, strings.Split(strings.TrimRight(string(b), "\n"), "\n")...)
	if n := len(s.lines); n > 3000 {
		s.lines = append([]string(nil), s.lines[n-2000:]...)
	}
	return len(b), nil
}

func (s *logSink) dump(t testing.TB) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := max(0, len(s.lines)-400)
	for _, l := range s.lines[start:] {
		t.Log("component: " + l)
	}
}

var (
	sink       = &logSink{}
	logger     = slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelInfo}))
	globalLogs sync.Once
)

// --- PKI -------------------------------------------------------------------------------------------

// testCA is a certificate authority for one run.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

var serial = struct {
	sync.Mutex
	n int64
}{n: 100}

func nextSerial() *big.Int {
	serial.Lock()
	defer serial.Unlock()
	serial.n++
	return big.NewInt(serial.n)
}

func newCA(t testing.TB, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: nextSerial(), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key}
}

func (ca *testCA) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// issue signs a leaf cert and returns the cert and key as PEM. A server cert is for 127.0.0.1; a client
// cert carries one URI SAN, the agent identity (PLAN §18.5).
func (ca *testCA) issue(t testing.TB, server bool, uri string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: nextSerial(), Subject: pkix.Name{CommonName: "leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	if uri != "" {
		u, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

// writeAtomic replaces a file in one step, so that the agent never reads half a file.
func writeAtomic(t testing.TB, path string, b []byte) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// identity is the agent identity of a spoke. The last path segment must be the SpokeCluster name.
func identity(prefix, cluster string) string {
	return "spiffe://mla.example.com/" + prefix + "/" + cluster
}

// --- envtest ---------------------------------------------------------------------------------------

// apiServer is one envtest API server (etcd and kube-apiserver) with the Querdian CRDs.
type apiServer struct {
	env   *envtest.Environment
	cfg   *rest.Config
	admin client.Client
}

// startAPIServers starts one API server for each name, in parallel, and stops them with the test.
func startAPIServers(t testing.TB, names ...string) map[string]*apiServer {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: run `make e2e` or `make e2e-multicluster`, which install the envtest binaries")
	}
	out := make(map[string]*apiServer, len(names))
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make([]error, len(names))
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env := &envtest.Environment{
				CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd")},
				ErrorIfCRDPathMissing: true,
			}
			cfg, err := env.Start()
			if err != nil {
				errs[i] = fmt.Errorf("start the API server of %s: %w", name, err)
				return
			}
			c, err := client.New(cfg, client.Options{Scheme: scheme})
			if err != nil {
				_ = env.Stop()
				errs[i] = err
				return
			}
			mu.Lock()
			out[name] = &apiServer{env: env, cfg: cfg, admin: c}
			mu.Unlock()
		}()
	}
	wg.Wait()
	t.Cleanup(func() {
		for _, s := range out {
			_ = s.env.Stop()
		}
	})
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	return out
}

func create(t testing.TB, c client.Client, objs ...client.Object) {
	t.Helper()
	for _, o := range objs {
		if err := c.Create(context.Background(), o); err != nil {
			t.Fatalf("create %T %s: %v", o, o.GetName(), err)
		}
	}
}

// --- hub -------------------------------------------------------------------------------------------

// hub runs the hub components in-process, as separate controller-runtime managers like separate
// Deployments: the Querdian proxy with its controller (snapshot, deadline table, status writer), and the
// receiver with its own mTLS listener.
type hub struct {
	api       *apiServer
	cfg       *config.Config
	ctl       *controller.Controller
	deadlines *policy.Deadlines
	issuer    *framework.Issuer
	proxyURL  string

	receiverURL string
	clientCA    *testCA // signs agent certs; only the hub can issue from it (PLAN §18.5)
	serverCA    []byte  // PEM of the CA of the receiver server cert
}

// hubConfig is the static config of the hub: the multicluster block and eight backends, one for each tenant
// setup of each backend type (PLAN §18.9).
func hubConfig(t testing.TB, issuer string, b *backends) *config.Config {
	t.Helper()
	raw := config.Config{
		Auth:         config.AuthConfig{Issuer: issuer, Audiences: []string{framework.Audience}},
		Multicluster: &config.Multicluster{HubCluster: hubCluster},
	}
	for _, ref := range allBackends {
		be := config.Backend{
			Name: ref.name, Type: ref.typ, URL: b.url(ref), PathPrefix: ref.prefix,
			NamespaceLabel: nsLabel(ref.typ), ClusterLabel: clusterLabel(ref.typ),
		}
		if ref.setup != setupNone {
			be.TenantHeader = tenantHeader
		}
		if ref.setup == setupPerCluster && ref.typ == config.BackendLoki {
			be.TenantFederation = config.FederationTenantIDLabel
		}
		if ref.typ == config.BackendTempo {
			// Filter mode, so that the suite can show that a trace of a different cluster is not found.
			be.TraceByID = config.TraceByIDFilter
		}
		raw.Backends = append(raw.Backends, be)
	}
	y, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	// config.Duration has no text encoding, so set the duration in the document as an operator writes it.
	var doc map[string]any
	if err := yaml.Unmarshal(y, &doc); err != nil {
		t.Fatal(err)
	}
	doc["multicluster"] = map[string]any{"hubCluster": hubCluster, "maxStaleness": defaultMaxStaleness.String()}
	if y, err = yaml.Marshal(doc); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(y)
	if err != nil {
		t.Fatalf("hub config: %v\n%s", err, y)
	}
	return cfg
}

func startHub(t testing.TB, api *apiServer, b *backends) *hub {
	t.Helper()
	globalLogs.Do(func() {
		ctrllog.SetLogger(logr.FromSlogHandler(logger.Handler()))
		klog.SetSlogLogger(logger)
	})
	h := &hub{api: api, deadlines: policy.NewDeadlines(), issuer: framework.NewIssuer(t)}
	h.cfg = hubConfig(t, h.issuer.URL(), b)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	run := func(mgr ctrl.Manager) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := mgr.Start(ctx); err != nil && ctx.Err() == nil {
				logger.Error("manager stopped", "err", err)
			}
		}()
	}
	newMgr := func() ctrl.Manager {
		mgr, err := ctrl.NewManager(api.cfg, ctrl.Options{
			Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		})
		if err != nil {
			t.Fatal(err)
		}
		return mgr
	}

	// Querdian: the controller and the proxy handler chain. This is a copy of the wiring in cmd/querdian, not
	// the command itself; the package comment lists the differences.
	mgr := newMgr()
	h.ctl = controller.New(h.cfg, controller.WithDeadlines(h.deadlines), controller.WithDebounce(50*time.Millisecond))
	if err := h.ctl.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	run(mgr)
	reg := prometheus.NewRegistry()
	metrics := observability.NewMetrics(reg)
	authn, err := auth.New(ctx, h.cfg.Auth, auth.WithFailureCounter(metrics.AuthFailures),
		auth.WithRetryInterval(10*time.Millisecond, 50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(server.Deps{
		Config:    h.cfg,
		Snapshots: h.ctl,
		Deadlines: h.deadlines,
		Auth:      authn,
		Enforcers: map[config.BackendType]enforce.Enforcer{
			config.BackendPrometheus: enforcepromql.New(),
			config.BackendLoki:       enforcelogql.New(),
			config.BackendTempo:      enforcetraceql.New(),
		},
		TailPlanners: map[config.BackendType]server.TailPlanner{config.BackendLoki: enforcelogql.PlanTail},
		Metrics:      metrics,
		Gatherer:     reg,
		Logger:       logger,
	})
	hs := httptest.NewServer(srv.ProxyHandler())
	t.Cleanup(hs.Close)
	h.proxyURL = hs.URL

	// Receiver: its own manager, its own client CA and a direct client (no cache).
	h.clientCA = newCA(t, "agent-client-ca")
	serverCA := newCA(t, "receiver-server-ca")
	h.serverCA = serverCA.pem()
	certPEM, keyPEM := serverCA.issue(t, true, "")
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(h.clientCA.cert)
	direct, err := client.New(api.cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	rmgr := newMgr()
	// The heartbeat of the suite is one second, which is more than the request limit of an identity
	// permits. The receiver tests cover that limit.
	rsrv := receiver.NewServer("127.0.0.1:0", receiver.TLSConfig(cert, pool),
		receiver.New(receiver.Options{Client: direct, Logger: logger, RequestBurst: 1 << 20}), logger)
	if err := rmgr.Add(rsrv); err != nil {
		t.Fatal(err)
	}
	run(rmgr)

	eventually(t, time.Minute, "hub ready", func() (bool, string) {
		return h.ctl.Ready() && authn.Ready() && rsrv.Ready() && rsrv.Addr() != nil, "controller, auth or receiver not ready"
	})
	h.receiverURL = "https://" + rsrv.Addr().String()
	return h
}

// spokeCluster returns the hub object of a spoke.
func (h *hub) spokeCluster(t testing.TB, name string) *v1.SpokeCluster {
	t.Helper()
	sc := &v1.SpokeCluster{}
	if err := h.api.admin.Get(context.Background(), client.ObjectKey{Name: name}, sc); err != nil {
		t.Fatal(err)
	}
	return sc
}

// patchSpoke changes the spec of a SpokeCluster as a hub admin. A spec change increases metadata.generation.
func (h *hub) patchSpoke(t testing.TB, name string, mut func(*v1.SpokeClusterSpec)) {
	t.Helper()
	sc := h.spokeCluster(t, name)
	base := sc.DeepCopy()
	mut(&sc.Spec)
	if err := h.api.admin.Patch(context.Background(), sc, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
}

// spokePolicies returns the number of SpokePolicy objects whose spec.cluster is cluster.
func (h *hub) spokePolicies(t testing.TB, cluster string) int {
	t.Helper()
	var list v1.SpokePolicyList
	if err := h.api.admin.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	n := 0
	for i := range list.Items {
		if list.Items[i].Spec.Cluster == cluster {
			n++
		}
	}
	return n
}

// synced reports whether the SpokeCluster had a valid sync for its current generation.
func (h *hub) synced(t testing.TB, name string) bool {
	sc := h.spokeCluster(t, name)
	return sc.Status.LastSyncTime != nil && sc.Status.ObservedGeneration == sc.Generation
}

// --- spokes ----------------------------------------------------------------------------------------

// spoke is one spoke cluster: its API server, an agent user with the minimal RBAC of PLAN §18.5 and the
// cert files of the agent.
type spoke struct {
	name     string
	api      *apiServer
	agentCfg *rest.Config
	agent    client.Client
	certFile string
	keyFile  string
	caFile   string

	mu   sync.Mutex
	stop func()
}

// newSpoke creates the agent user and the cert files, and registers the SpokeCluster on the hub.
func newSpoke(t testing.TB, h *hub, name string, api *apiServer) *spoke {
	t.Helper()
	s := &spoke{name: name, api: api}
	t.Cleanup(s.stopAgent)
	create(t, api.admin, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: agentNamespace}})
	user := "querdian-agent"
	create(t, api.admin,
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: user}, Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{v1.GroupVersion.Group}, Resources: []string{"clusteraccesspolicies", "accesspolicies"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{v1.GroupVersion.Group}, Resources: []string{"clusteraccesspolicies/status", "accesspolicies/status"}, Verbs: []string{"update", "patch"}},
		}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: user},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: user},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: agentNamespace, Name: user}, Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{coordinationv1.GroupName}, Resources: []string{"leases"}, Verbs: []string{"get", "create", "update"}},
		}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: agentNamespace, Name: user},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: user},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}}},
	)
	u, err := api.env.AddUser(envtest.User{Name: user}, api.cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.agentCfg = u.Config()
	if s.agent, err = client.New(s.agentCfg, client.Options{Scheme: scheme}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	s.certFile, s.keyFile, s.caFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), filepath.Join(dir, "ca.crt")
	s.setIdentity(t, h, identity("agent", name))
	writeAtomic(t, s.caFile, h.serverCA)

	create(t, h.api.admin, &v1.SpokeCluster{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.SpokeClusterSpec{
		Identity:      identity("agent", name),
		Tenants:       []string{tenantAll, perClusterTenant(name)},
		DefaultTenant: tenantAll,
	}})
	return s
}

// setIdentity writes a new agent cert with the identity uri. The agent reads the files before each request.
func (s *spoke) setIdentity(t testing.TB, h *hub, uri string) {
	t.Helper()
	c, k := h.clientCA.issue(t, false, uri)
	writeAtomic(t, s.keyFile, k)
	writeAtomic(t, s.certFile, c)
}

// sender returns a Sender that uses the current cert files of the agent, for direct sync requests.
func (s *spoke) sender(t testing.TB, h *hub) *agent.Sender {
	t.Helper()
	snd, err := agent.NewSender(h.receiverURL, agent.TLSFiles{CertFile: s.certFile, KeyFile: s.keyFile, CAFile: s.caFile}, 10*time.Second, logger)
	if err != nil {
		t.Fatal(err)
	}
	return snd
}

// start runs the agent with leader election until stopAgent or the end of the test.
func (s *spoke) start(t testing.TB, h *hub) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		t.Fatalf("agent of %s runs already", s.name)
	}
	a, err := agent.New(agent.Options{Reader: s.agent, Writer: s.agent, Syncer: s.sender(t, h), Interval: heartbeat, Logger: logger.With("agent", s.name)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := agent.RunWithLeaderElection(ctx, agent.LeaderElection{
			Config: s.agentCfg, Namespace: agentNamespace, Name: "querdian-agent", Identity: s.name + "-agent",
			Logger: logger.With("agent", s.name),
		}, a.Run)
		if err != nil {
			logger.Error("agent stopped", "agent", s.name, "err", err)
		}
	}()
	s.stop = func() { cancel(); <-done }
}

// stopAgent stops the agent and waits until it has stopped.
func (s *spoke) stopAgent() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
}

// --- polling ---------------------------------------------------------------------------------------

// eventually polls cond until it is true or the timeout expires. The message of the last poll goes into
// the failure.
func eventually(t testing.TB, timeout time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		ok, msg := cond()
		if ok {
			return
		}
		last = msg
		if time.Now().After(deadline) {
			t.Fatalf("%s: not true after %s: %s", what, timeout, last)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// consistently checks cond for the whole duration and fails at the first false.
func consistently(t testing.TB, d time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if ok, msg := cond(); !ok {
			t.Fatalf("%s: %s", what, msg)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// syncRequest is a heartbeat with the digest of an empty state, for direct requests to the receiver.
func syncRequest(t testing.TB) *spokesync.Request {
	t.Helper()
	d, err := spokesync.Digest(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &spokesync.Request{Digest: d}
}
