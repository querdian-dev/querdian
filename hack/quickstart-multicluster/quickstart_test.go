// Package quickstartmulticluster only holds the multi-cluster kind quickstart (scripts, manifests, Helm
// values) and the test that checks these files without a cluster. The test renders the files with the
// same shell function as up.sh, so it sees what up.sh applies.
package quickstartmulticluster

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/policy"
	"github.com/querdian-dev/querdian/internal/receiver"
	"github.com/querdian-dev/querdian/internal/spokesync"
)

const (
	hubIP       = "172.18.0.2"
	receiverURL = "https://172.18.0.2:30443"
)

// shell runs a bash snippet after `source lib.sh` in this directory and returns its stdout.
func shell(t *testing.T, script string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	cmd := exec.Command("bash", append([]string{"-c", "set -euo pipefail; source ./lib.sh; " + script, "test"}, args...)...) //nolint:gosec // fixed script, test args
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "XDG_CACHE_HOME=", "CLUSTER_PREFIX=querdian", "MC_WORK_DIR=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bash %q %v: %v\n%s", script, args, err, stderr.String())
	}
	return string(out)
}

// workDir runs a bash snippet after `source lib.sh` with MC_WORK_DIR=dir and HOME=home. It returns the
// error of the snippet.
func workDir(t *testing.T, dir, home, script string) error {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	cmd := exec.Command("bash", "-c", "set -euo pipefail; source ./lib.sh; "+script) //nolint:gosec // fixed script
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CACHE_HOME=", "CLUSTER_PREFIX=querdian", "MC_WORK_DIR="+dir)
	return cmd.Run()
}

// down.sh deletes the work directory, so up.sh must never mark a directory that holds files of the user.
func TestWorkDirOwnership(t *testing.T) {
	home := t.TempDir()
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	cycle := "claim_work_dir; mkdir -p \"$CERT_DIR\"; touch \"$CERT_DIR/agent-ca.key\"; remove_work_dir"

	// A new directory and an empty directory: up.sh makes and marks them, down.sh deletes them.
	for _, d := range []string{filepath.Join(t.TempDir(), "new", "work"), t.TempDir()} {
		if err := workDir(t, d, home, cycle); err != nil || exists(d) {
			t.Fatalf("%s: %v, exists after removal: %v", d, err, exists(d))
		}
	}

	// A directory with a file of the user, as MC_WORK_DIR=/tmp would be.
	used := t.TempDir()
	mine := filepath.Join(used, "notes.txt")
	if err := os.WriteFile(mine, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := workDir(t, used, home, "claim_work_dir"); err == nil {
		t.Fatal("claim_work_dir accepted a directory with files of the user")
	}
	if exists(filepath.Join(used, ".querdian-quickstart-multicluster")) {
		t.Fatal("claim_work_dir marked a directory with files of the user")
	}
	if err := workDir(t, used, home, "remove_work_dir"); err != nil || !exists(mine) {
		t.Fatalf("remove_work_dir changed a directory without a marker: %v", err)
	}

	// A second up.sh may use the directory of the first one.
	again := t.TempDir()
	if err := workDir(t, again, home, "claim_work_dir; mkdir -p \"$CERT_DIR\"; claim_work_dir"); err != nil {
		t.Fatalf("second claim: %v", err)
	}

	// A file is not a directory.
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := workDir(t, file, home, "claim_work_dir"); err == nil {
		t.Fatal("claim_work_dir accepted a file")
	}

	// remove_work_dir never deletes $HOME, also with a marker.
	if err := os.WriteFile(filepath.Join(home, ".querdian-quickstart-multicluster"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := workDir(t, home, home, "remove_work_dir"); err == nil || !exists(home) {
		t.Fatalf("remove_work_dir with MC_WORK_DIR=$HOME: %v", err)
	}
}

// subst renders a file with subst() of lib.sh, as up.sh does.
func subst(t *testing.T, file string, kv ...string) string {
	t.Helper()
	return shell(t, `subst "$@"`, append([]string{file}, kv...)...)
}

func libVar(t *testing.T, expr string) string {
	t.Helper()
	return strings.TrimSpace(shell(t, `printf '%s' "`+expr+`"`))
}

func spokes(t *testing.T) []string { return strings.Fields(libVar(t, "${SPOKES[*]}")) }

func hubName(t *testing.T) string { return libVar(t, "$HUB_NAME") }

func docs(t *testing.T, raw string) [][]byte {
	t.Helper()
	var out [][]byte
	for _, d := range strings.Split(raw, "\n---\n") {
		if strings.TrimSpace(stripComments(d)) != "" {
			out = append(out, []byte(d))
		}
	}
	return out
}

func stripComments(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// strictObject decodes one manifest into its typed object with unknown fields rejected.
func strictObject(t *testing.T, file string, d []byte) any {
	t.Helper()
	var meta metav1.TypeMeta
	if err := yaml.Unmarshal(d, &meta); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	var obj any
	switch meta.Kind {
	case "ConfigMap":
		obj = &corev1.ConfigMap{}
	case "Service":
		obj = &corev1.Service{}
	case "ServiceAccount":
		obj = &corev1.ServiceAccount{}
	case "Deployment":
		obj = &appsv1.Deployment{}
	case "DaemonSet":
		obj = &appsv1.DaemonSet{}
	case "ClusterRole":
		obj = &rbacv1.ClusterRole{}
	case "ClusterRoleBinding":
		obj = &rbacv1.ClusterRoleBinding{}
	case "AccessPolicy":
		obj = &v1.AccessPolicy{}
	case "ClusterAccessPolicy":
		obj = &v1.ClusterAccessPolicy{}
	case "SpokeCluster":
		obj = &v1.SpokeCluster{}
	default:
		t.Fatalf("%s: unexpected kind %q", file, meta.Kind)
	}
	if err := yaml.UnmarshalStrict(d, obj); err != nil {
		t.Fatalf("%s (%s): %v", file, meta.Kind, err)
	}
	return obj
}

// rendered returns the typed objects of each manifest of this quickstart, rendered as up.sh renders them.
func rendered(t *testing.T) map[string][]any {
	t.Helper()
	files := map[string][]string{
		"manifests/hub-prometheus.yaml":     {"HUB_NAME=hub"},
		"manifests/hub-otel-collector.yaml": {"HUB_NAME=hub", "TENANT=demo"},
		"manifests/ingest-gateway.yaml":     {"TENANT=demo"},
		"manifests/spoke-collector.yaml":    {"CLAIM=hub", "GATEWAY_URL=https://" + hubIP + ":31001"},
		"manifests/hub-policies.yaml":       nil,
		"manifests/spoke-policies.yaml":     {"TEAM=team-a", "SPOKE=spoke-a"},
		"manifests/spokecluster.yaml":       {"SPOKE=spoke-a", "IDENTITY=spiffe://quickstart.local/agent/1/spoke-a", "TENANT=demo"},
	}
	out := map[string][]any{}
	for f, kv := range files {
		for _, d := range docs(t, subst(t, f, kv...)) {
			out[f] = append(out[f], strictObject(t, f, d))
		}
	}
	return out
}

func TestManifestsDecodeStrictly(t *testing.T) {
	objs := rendered(t)
	if len(objs) != 7 {
		t.Fatalf("rendered %d files", len(objs))
	}
	// The single-cluster manifests that up.sh reuses must also render without a placeholder left.
	for _, f := range []string{"00-base", "keycloak", "loki", "tempo", "grafana", "workloads"} {
		if raw := subst(t, "../quickstart/manifests/"+f+".yaml"); !strings.Contains(raw, "kind:") {
			t.Errorf("%s: empty render", f)
		}
	}
}

func TestSubstStopsOnALeftPlaceholder(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	cmd := exec.Command("bash", "-c", "source ./lib.sh; subst manifests/spokecluster.yaml SPOKE=x") //nolint:gosec // fixed script
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "placeholder left") {
		t.Fatalf("subst with a missing value: err=%v\n%s", err, out)
	}
}

func configMap(t *testing.T, objs []any, name string) map[string]any {
	t.Helper()
	for _, o := range objs {
		if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == name {
			var m map[string]any
			if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &m); err != nil {
				t.Fatalf("ConfigMap %s: config.yaml: %v", name, err)
			}
			return m
		}
	}
	t.Fatalf("no ConfigMap %s", name)
	return nil
}

func get(m any, path ...string) any {
	for _, p := range path {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[p]
	}
	return m
}

func strs(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

// actions returns "key=value:action" for each action of an attributes or resource processor.
func actions(t *testing.T, proc any, list string) []string {
	t.Helper()
	l, ok := get(proc, list).([]any)
	if !ok {
		t.Fatalf("processor has no %s list: %v", list, proc)
	}
	var out []string
	for _, a := range l {
		am := a.(map[string]any)
		v := ""
		if x, ok := am["value"]; ok {
			v = fmt.Sprint(x)
		}
		out = append(out, fmt.Sprintf("%v=%s:%v", am["key"], v, am["action"]))
	}
	sort.Strings(out)
	return out
}

// The gateway is the only thing between a spoke and the cluster label. Each spoke has its own receiver
// that trusts only its own ingest CA, and each pipeline overwrites the label with the value of its spoke
// for all three signals.
func TestGatewayOverwritesTheClusterLabel(t *testing.T) {
	objs := rendered(t)["manifests/ingest-gateway.yaml"]
	cfg := configMap(t, objs, "ingest-gateway")
	ss := spokes(t)
	receivers, _ := cfg["receivers"].(map[string]any)
	if len(receivers) != len(ss) {
		t.Fatalf("receivers %v, want one for each of %v", receivers, ss)
	}
	var svc *corev1.Service
	for _, o := range objs {
		if s, ok := o.(*corev1.Service); ok {
			svc = s
		}
	}
	if svc == nil || svc.Spec.Type != corev1.ServiceTypeNodePort {
		t.Fatalf("the gateway needs a NodePort Service: %+v", svc)
	}
	cas := map[string]bool{}
	for _, s := range ss {
		r := get(cfg, "receivers", "otlp/"+s, "protocols", "http")
		if r == nil {
			t.Fatalf("no OTLP/HTTP receiver otlp/%s", s)
		}
		ca := fmt.Sprint(get(r, "tls", "client_ca_file"))
		if ca != "/etc/ingest-gateway/ca-"+s+".crt" || cas[ca] {
			t.Errorf("otlp/%s: client_ca_file = %q, want the ingest CA of %s only", s, ca, s)
		}
		cas[ca] = true
		if get(r, "tls", "cert_file") == nil || get(r, "tls", "key_file") == nil {
			t.Errorf("otlp/%s: no server cert", s)
		}
		_, port, _ := net.SplitHostPort(fmt.Sprint(get(r, "endpoint")))
		var found bool
		for _, p := range svc.Spec.Ports {
			if p.Name == s && p.TargetPort.String() == port && p.NodePort >= 30000 {
				found = true
			}
		}
		if !found {
			t.Errorf("the Service has no NodePort %q for the port %s of otlp/%s", s, port, s)
		}
		if got, want := actions(t, get(cfg, "processors", "resource/"+s), "attributes"),
			[]string{"cluster=" + s + ":upsert", "k8s.cluster.name=" + s + ":upsert"}; !slices.Equal(got, want) {
			t.Errorf("resource/%s: %v, want %v", s, got, want)
		}
		if got, want := actions(t, get(cfg, "processors", "attributes/"+s), "actions"),
			[]string{"cluster=" + s + ":upsert", "k8s.cluster.name=:delete"}; !slices.Equal(got, want) {
			t.Errorf("attributes/%s: %v, want %v", s, got, want)
		}
	}
	pipelines, _ := get(cfg, "service", "pipelines").(map[string]any)
	signals := map[string]int{}
	for name, p := range pipelines {
		recv := strs(get(p, "receivers"))
		if len(recv) != 1 {
			t.Errorf("pipeline %s: receivers %v, want exactly one", name, recv)
			continue
		}
		s := strings.TrimPrefix(recv[0], "otlp/")
		procs := strs(get(p, "processors"))
		if !slices.Contains(procs, "resource/"+s) || !slices.Contains(procs, "attributes/"+s) {
			t.Errorf("pipeline %s: processors %v lack the cluster processors of %s", name, procs, s)
		}
		// The overwrite must come before the batch processor and the export; no processor of another
		// spoke may follow.
		for _, o := range ss {
			if o != s && (slices.Contains(procs, "resource/"+o) || slices.Contains(procs, "attributes/"+o)) {
				t.Errorf("pipeline %s of %s uses a processor of %s", name, s, o)
			}
		}
		if procs[len(procs)-1] != "batch" {
			t.Errorf("pipeline %s: the last processor is %s", name, procs[len(procs)-1])
		}
		// Prometheus copies the resource attributes to target_info, so the metrics pipelines delete the
		// resource attribute k8s.cluster.name after the upsert.
		if strings.HasPrefix(name, "metrics/") {
			i, j := slices.Index(procs, "resource/"+s), slices.Index(procs, "resource/metrics")
			if j < 0 || j < i {
				t.Errorf("pipeline %s: processors %v; resource/metrics must follow resource/%s", name, procs, s)
			}
		}
		signals[strings.SplitN(name, "/", 2)[0]+"/"+s]++
	}
	for _, s := range ss {
		for _, sig := range []string{"metrics", "logs", "traces"} {
			if signals[sig+"/"+s] != 1 {
				t.Errorf("%s of %s: %d pipelines, want 1", sig, s, signals[sig+"/"+s])
			}
		}
	}
	if got := actions(t, get(cfg, "processors", "resource/metrics"), "attributes"); !slices.Equal(got, []string{"k8s.cluster.name=:delete"}) {
		t.Errorf("resource/metrics: %v", got)
	}
	raw, _ := yaml.Marshal(cfg)
	if strings.Contains(string(raw), "action: insert") {
		t.Error("an insert action keeps a value of the spoke; use upsert")
	}
}

// Hub telemetry carries the hub cluster label (docs/multicluster.md, step 2), and the spoke collector
// claims a false cluster on each signal (the gateway must overwrite it).
func TestCollectorsSetTheExpectedLabels(t *testing.T) {
	objs := rendered(t)
	hub := hubName(t)
	hc := configMap(t, objs["manifests/hub-otel-collector.yaml"], "otel-collector")
	if got := actions(t, get(hc, "processors", "resource/hub-cluster"), "attributes"); !slices.Equal(got,
		[]string{"cluster=" + hub + ":upsert", "k8s.cluster.name=" + hub + ":upsert"}) {
		t.Errorf("resource/hub-cluster: %v", got)
	}
	for _, p := range []string{"logs", "traces"} {
		if procs := strs(get(hc, "service", "pipelines", p, "processors")); !slices.Contains(procs, "resource/hub-cluster") {
			t.Errorf("hub collector %s: processors %v lack resource/hub-cluster", p, procs)
		}
	}
	var prom string
	for _, o := range objs["manifests/hub-prometheus.yaml"] {
		if cm, ok := o.(*corev1.ConfigMap); ok {
			prom = cm.Data["prometheus.yml"]
		}
	}
	var pc struct {
		ScrapeConfigs []struct {
			HonorLabels    bool `json:"honor_labels"`
			RelabelConfigs []struct {
				SourceLabels []string `json:"source_labels"`
				TargetLabel  string   `json:"target_label"`
				Replacement  string   `json:"replacement"`
				Action       string   `json:"action"`
			} `json:"relabel_configs"`
		} `json:"scrape_configs"`
	}
	if err := yaml.Unmarshal([]byte(prom), &pc); err != nil || len(pc.ScrapeConfigs) == 0 {
		t.Fatalf("hub Prometheus config: %v\n%s", err, prom)
	}
	for _, sc := range pc.ScrapeConfigs {
		var n int
		for _, r := range sc.RelabelConfigs {
			if r.TargetLabel == "cluster" {
				n++
				if r.Action != "replace" || r.Replacement != hub || len(r.SourceLabels) != 0 {
					t.Errorf("hub Prometheus: relabel of cluster = %+v, want a fixed %q", r, hub)
				}
			}
		}
		if n != 1 || sc.HonorLabels {
			t.Errorf("hub Prometheus: %d relabel rules for cluster, honor_labels %v; want 1 and false", n, sc.HonorLabels)
		}
	}
	sc := configMap(t, objs["manifests/spoke-collector.yaml"], "otel-collector")
	for _, p := range []string{"metrics", "logs", "traces"} {
		procs := strs(get(sc, "service", "pipelines", p, "processors"))
		if !slices.Contains(procs, "resource/claim") || !slices.Contains(procs, "attributes/claim") {
			t.Errorf("spoke collector %s: processors %v lack the false claim", p, procs)
		}
		if got := strs(get(sc, "service", "pipelines", p, "exporters")); !slices.Equal(got, []string{"otlphttp/hub"}) {
			t.Errorf("spoke collector %s: exporters %v, want only the gateway", p, got)
		}
	}
	if got := fmt.Sprint(get(sc, "exporters", "otlphttp/hub", "endpoint")); got != "https://"+hubIP+":31001" {
		t.Errorf("spoke collector endpoint = %q", got)
	}
	for _, f := range []string{"ca_file", "cert_file", "key_file"} {
		if get(sc, "exporters", "otlphttp/hub", "tls", f) == nil {
			t.Errorf("spoke collector: no tls.%s", f)
		}
	}
	if got := fmt.Sprint(get(sc, "receivers", "prometheus", "config", "scrape_configs")); !strings.Contains(got, "$$1:$$2") {
		t.Errorf("the relabel replacement must escape $ in a collector config: %s", got)
	}
}

func helm(t *testing.T, args ...string) [][]byte {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	out, err := exec.Command("helm", append([]string{"template", "querdian", "../../charts/querdian", "-n", "querdian"}, args...)...).CombinedOutput() //nolint:gosec // fixed binary, test args
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", args, err, out)
	}
	return docs(t, string(out))
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type object struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
	Spec struct {
		Type  string `json:"type"`
		Ports []struct {
			Name     string `json:"name"`
			NodePort int    `json:"nodePort"`
		} `json:"ports"`
		Template struct {
			Spec struct {
				Containers []struct {
					Args []string `json:"args"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func objects(t *testing.T, raw [][]byte) map[string]object {
	t.Helper()
	out := map[string]object{}
	for _, d := range raw {
		var o object
		if err := yaml.Unmarshal(d, &o); err != nil {
			t.Fatal(err)
		}
		if o.Kind != "" {
			out[o.Kind+"/"+o.Metadata.Name] = o
		}
	}
	return out
}

// hubConfig renders the hub release with the values files of up.sh and returns the parsed static config.
func hubConfig(t *testing.T) (*config.Config, map[string]object) {
	t.Helper()
	base := writeTemp(t, "values.yaml", subst(t, "../quickstart/querdian-values.yaml"))
	objs := objects(t, helm(t, "-f", base, "-f", "hub-values.yaml"))
	cm, ok := objs["ConfigMap/querdian"]
	if !ok {
		t.Fatalf("no ConfigMap/querdian; objects: %v", keysOf(objs))
	}
	cfg, err := config.Parse([]byte(cm.Data["config.yaml"]))
	if err != nil {
		t.Fatalf("the hub config is rejected: %v\n%s", err, cm.Data["config.yaml"])
	}
	return cfg, objs
}

func keysOf(m map[string]object) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// up.sh assigns `i=$(spoke_index "$s")` under set -e, so the function must return 0 for each spoke, not
// only for the last one. shell() runs the snippet under set -euo pipefail, so a non-zero status fails the
// test there.
func TestSpokeIndex(t *testing.T) {
	ss := spokes(t)
	if len(ss) < 2 {
		t.Fatalf("SPOKES = %v, want at least two spokes for this test", ss)
	}
	out := shell(t, `for s in "${SPOKES[@]}"; do i=$(spoke_index "$s"); echo "$s=$i"; done`)
	var want strings.Builder
	for i, s := range ss {
		fmt.Fprintf(&want, "%s=%d\n", s, i+1)
	}
	if out != want.String() {
		t.Errorf("spoke_index output = %q, want %q", out, want.String())
	}
	if got := shell(t, `if spoke_index spoke-unknown; then echo found; else echo missing; fi`); got != "missing\n" {
		t.Errorf("spoke_index of an unknown spoke: %q, want %q", got, "missing\n")
	}
}

// up.sh calls pf_stop under set -e. A port-forward that stopped before pf_stop must not stop the script.
func TestPfStopIgnoresAStoppedProcess(t *testing.T) {
	if got := shell(t, `sleep 0 & PF_PIDS+=($!); wait $!; pf_stop; echo "stopped ${#PF_PIDS[@]}"`); got != "stopped 0\n" {
		t.Errorf("pf_stop with a stopped process: %q, want %q", got, "stopped 0\n")
	}
}

// braceList finds a brace group with a comma, such as {a="x", b="y"}, but not a parameter expansion.
var braceList = regexp.MustCompile(`(^|[^$])\{[^}]*,`)

// bash 3.2 (macOS) does brace expansion on {a,b} inside "$(...)", also inside quotes: the command
// substitution then runs once for each alternative. For a PromQL query, the matcher list becomes two
// broken queries. The scripts must put such text into a variable first.
//
// This is a simple line check. It finds a brace group whose first comma comes before its first "}". It
// does not find a "$(" later in a quoted word ("q=$(...)"), a continuation line after a "\", or a comma
// after a nested group ({a:{b:1},c:2}). It also fails for text after the closing ")", for a comment at
// the end of a line, and for single-quoted text; a variable is the fix there too.
func TestNoBraceListInCommandSubstitution(t *testing.T) {
	files, err := filepath.Glob("*.sh")
	if err != nil {
		t.Fatal(err)
	}
	single, err := filepath.Glob("../quickstart/*.sh")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, single...)
	if len(files) < 2 {
		t.Fatalf("found only the scripts %v", files)
	}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // a quickstart script from filepath.Glob
		if err != nil {
			t.Fatal(err)
		}
		for n, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			for rest := line; ; {
				i := strings.Index(rest, `"$(`)
				if i < 0 {
					break
				}
				rest = rest[i+3:]
				if braceList.MatchString(rest) {
					t.Errorf("%s:%d: brace group with a comma inside \"$(...)\"; bash 3.2 expands it: %s", f, n+1, strings.TrimSpace(line))
					break
				}
			}
		}
	}
}

func TestHubRelease(t *testing.T) {
	cfg, objs := hubConfig(t)
	if cfg.Multicluster == nil || cfg.Multicluster.HubCluster != hubName(t) {
		t.Fatalf("multicluster = %+v, want hubCluster %q (lib.sh HUB_NAME)", cfg.Multicluster, hubName(t))
	}
	// The cluster labels must be the names that the gateway and the hub collector write.
	want := map[string]string{"metrics": "cluster", "logs": "k8s_cluster_name", "traces": "resource.k8s.cluster.name"}
	for name, label := range want {
		b, ok := cfg.Backend(name)
		if !ok || b.ClusterLabel != label {
			t.Errorf("backend %s: clusterLabel = %q, want %q", name, b.ClusterLabel, label)
		}
	}
	svc, ok := objs["Service/querdian-receiver"]
	if !ok || svc.Spec.Type != "NodePort" || len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].NodePort != 30443 {
		t.Errorf("receiver Service = %+v, want NodePort 30443", svc.Spec)
	}
	if _, ok := objs["Deployment/querdian-receiver"]; !ok {
		t.Error("no receiver Deployment")
	}
	if _, ok := objs["Service/querdian-ui"]; !ok {
		t.Errorf("no dashboard Service; objects: %v", keysOf(objs))
	}
}

func TestSpokeRelease(t *testing.T) {
	values := writeTemp(t, "spoke.yaml", subst(t, "spoke-values.yaml", "RECEIVER_URL="+receiverURL))
	objs := objects(t, helm(t, "-f", values))
	var deployments []string
	for k := range objs {
		if strings.HasPrefix(k, "Deployment/") {
			deployments = append(deployments, k)
		}
		if k == "ConfigMap/querdian" {
			t.Error("a spoke must not get the proxy config")
		}
	}
	if !slices.Equal(deployments, []string{"Deployment/querdian-agent"}) {
		t.Fatalf("deployments = %v, want only the agent", deployments)
	}
	args := objs["Deployment/querdian-agent"].Spec.Template.Spec.Containers[0].Args
	for _, want := range []string{"agent", "--receiver-url=" + receiverURL, "--heartbeat-interval=15s"} {
		if !slices.Contains(args, want) {
			t.Errorf("agent args %v lack %q", args, want)
		}
	}
}

// The policies of the quickstart, compiled with the hub config as the hub compiles them: each team gets
// its namespace in its own cluster, SRE gets the fixed set of spokes, and a suspended spoke gives
// policy_source_expired.
func TestPolicyModel(t *testing.T) {
	cfg, _ := hubConfig(t)
	objs := rendered(t)
	var in policy.Input
	in.Config = cfg
	for _, o := range objs["manifests/hub-policies.yaml"] {
		switch p := o.(type) {
		case *v1.AccessPolicy:
			p.Generation = 1
			in.Policies = append(in.Policies, *p)
		case *v1.ClusterAccessPolicy:
			p.Generation = 1
			in.ClusterPolicies = append(in.ClusterPolicies, *p)
		}
	}
	in.NamespaceLabels = map[string]map[string]string{"order-service": {"policy.querdian.dev/tenant": "demo"}}
	now := time.Now()
	for _, s := range spokes(t) {
		team := "team-" + strings.TrimPrefix(s, "spoke-")
		for _, d := range docs(t, subst(t, "manifests/spoke-policies.yaml", "TEAM="+team, "SPOKE="+s)) {
			ap := strictObject(t, "spoke-policies.yaml", d).(*v1.AccessPolicy)
			src := v1.SpokePolicySource{Kind: v1.SpokeAccessPolicy, Namespace: ap.Namespace, Name: ap.Name,
				Generation: 1, TenantLabel: "demo", AccessPolicy: &ap.Spec}
			sp := v1.SpokePolicy{Spec: v1.SpokePolicySpec{Cluster: s, SpokePolicySource: src}}
			sp.Name, sp.Generation = spokesync.ObjectName(s, &src), 1
			in.SpokePolicies = append(in.SpokePolicies, sp)
		}
		identity := libVar(t, "$(agent_identity "+s+")")
		for _, d := range docs(t, subst(t, "manifests/spokecluster.yaml", "SPOKE="+s, "IDENTITY="+identity, "TENANT=demo")) {
			sc := strictObject(t, "spokecluster.yaml", d).(*v1.SpokeCluster)
			if !sc.Spec.Suspended {
				t.Errorf("SpokeCluster %s: up.sh must register it with suspended: true", s)
			}
			sc.Spec.Suspended = false // the state after migration step 6
			sc.Generation, sc.Status.ObservedGeneration = 2, 2
			sc.Status.LastSyncTime = &metav1.Time{Time: now}
			in.SpokeClusters = append(in.SpokeClusters, *sc)
		}
	}
	c := policy.Compile(in)
	for _, r := range c.Results {
		if !r.Ready || r.Reason != policy.ReasonCompiled {
			t.Errorf("%s %s/%s: %s: %s", r.Kind, r.Namespace, r.Name, r.Reason, r.Message)
		}
	}
	if len(c.Results) != 5 {
		t.Fatalf("results = %d, want 5", len(c.Results))
	}
	snap := policy.NewSnapshot(c, cfg, 1)
	table := func(suspended string) *policy.Deadlines {
		d := policy.NewDeadlines()
		for i := range in.SpokeClusters {
			sc := in.SpokeClusters[i].DeepCopy()
			sc.Spec.Suspended = sc.Name == suspended
			d.Update(sc, time.Duration(*cfg.Multicluster.MaxStaleness))
		}
		return d
	}
	terms := func(group, backend string, dl *policy.Deadlines) (string, error) {
		eff, err := snap.ResolveAt("u", []string{group}, backend, dl, now)
		if err != nil {
			return "", err
		}
		if eff.Unrestricted {
			return "unrestricted", nil
		}
		var ts []string
		for _, tm := range eff.Terms {
			var ms []string
			for _, m := range tm.Matchers {
				ms = append(ms, fmt.Sprintf("%s %s %s", m.Name, m.Op, m.Value))
			}
			sort.Strings(ms)
			ts = append(ts, strings.Join(ms, " & "))
		}
		sort.Strings(ts)
		return strings.Join(ts, " | "), nil
	}
	clusterLabel := map[string]string{"metrics": "cluster", "logs": "k8s_cluster_name", "traces": "resource.k8s.cluster.name"}
	nsLabel := map[string]string{"metrics": "namespace", "logs": "k8s_namespace_name", "traces": "resource.k8s.namespace.name"}
	all := table("")
	for backend, cl := range clusterLabel {
		ns := nsLabel[backend]
		want := map[string]string{
			"team-a":          fmt.Sprintf("%s Equal spoke-a & %s Equal order-service", cl, ns),
			"team-b":          fmt.Sprintf("%s Equal spoke-b & %s Equal order-service", cl, ns),
			"team-hub":        fmt.Sprintf("%s Equal hub & %s Equal order-service", cl, ns),
			"sre":             cl + " RegexMatch spoke-a|spoke-b",
			"platform-admins": "unrestricted",
		}
		for group, w := range want {
			got, err := terms(group, backend, all)
			if err != nil || got != w {
				t.Errorf("%s on %s: %q (%v), want %q", group, backend, got, err, w)
			}
		}
		// Suspended spoke-a: team-a gets policy_source_expired; the hub grants and spoke-b stay.
		sus := table("spoke-a")
		if _, err := terms("team-a", backend, sus); !errors.Is(err, policy.ErrPolicySourceExpired) {
			t.Errorf("team-a on %s with spoke-a suspended: err = %v, want ErrPolicySourceExpired", backend, err)
		}
		for _, group := range []string{"team-b", "team-hub", "sre", "platform-admins"} {
			a, _ := terms(group, backend, all)
			if b, err := terms(group, backend, sus); err != nil || a != b {
				t.Errorf("%s on %s changed with spoke-a suspended: %q -> %q (%v)", group, backend, a, b, err)
			}
		}
	}
}

func parseCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(raw)
	if b == nil {
		t.Fatalf("%s: no PEM", path)
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return c
}

func pool(t *testing.T, paths ...string) *x509.CertPool {
	t.Helper()
	p := x509.NewCertPool()
	for _, f := range paths {
		p.AddCert(parseCert(t, f))
	}
	return p
}

// handshake runs a TLS handshake between a server with cfg and a client with the given cert, over a pipe.
// It returns the server-side connection state and the first error.
func handshake(t *testing.T, cfg *tls.Config, serverCA *x509.CertPool, certFile, keyFile string) (*tls.ConnectionState, error) {
	t.Helper()
	cc := &tls.Config{RootCAs: serverCA, ServerName: hubIP, MinVersion: tls.VersionTLS13}
	if certFile != "" {
		kp, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			t.Fatal(err)
		}
		cc.Certificates = []tls.Certificate{kp}
	}
	// A TCP connection on the loopback interface: unlike net.Pipe it buffers, so the alert of a server that
	// rejects the cert after a finished TLS 1.3 client handshake does not block.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck // test listener
	errc := make(chan error, 1)
	go func() {
		c, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
		if err != nil {
			errc <- err
			return
		}
		defer c.Close() //nolint:errcheck // test connection
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		errc <- tls.Client(c, cc).Handshake()
	}()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck // test connection
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	srv := tls.Server(conn, cfg)
	serr := srv.Handshake()
	cerr := <-errc
	if serr != nil {
		return nil, serr
	}
	if cerr != nil {
		return nil, cerr
	}
	st := srv.ConnectionState()
	return &st, nil
}

// The certs of certs.sh: the receiver accepts only the agent certs and finds the identity of the
// SpokeCluster; an ingest CA accepts only the ingest cert of its own spoke (PLAN 18.5).
func TestCerts(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed")
	}
	dir := filepath.Join(t.TempDir(), "certs")
	ss := spokes(t)
	td := libVar(t, "$TRUST_DOMAIN")
	cmd := exec.Command("./certs.sh", append([]string{dir, hubIP, td}, ss...)...) //nolint:gosec // fixed script, test args
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("certs.sh: %v\n%s", err, out)
	}
	f := func(name string) string { return filepath.Join(dir, name) }
	serverCA := pool(t, f("hub-server-ca.crt"))
	for _, name := range []string{"receiver", "gateway"} {
		c := parseCert(t, f(name+".crt"))
		if _, err := c.Verify(x509.VerifyOptions{Roots: serverCA, DNSName: hubIP, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	rcfg, err := receiver.LoadTLSConfig(f("receiver.crt"), f("receiver.key"), f("agent-ca.crt"))
	if err != nil {
		t.Fatalf("receiver.LoadTLSConfig: %v", err)
	}
	for _, s := range ss {
		st, err := handshake(t, rcfg, serverCA, f("agent-"+s+".crt"), f("agent-"+s+".key"))
		if err != nil {
			t.Fatalf("receiver handshake with agent-%s: %v", s, err)
		}
		id, err := receiver.Identity(st)
		want := libVar(t, "$(agent_identity "+s+")")
		if err != nil || id != want || !spokesync.IdentityMatchesName(id, s) {
			t.Errorf("agent-%s: identity %q (%v), want %q with the last segment %s", s, id, err, want, s)
		}
		if _, err := handshake(t, rcfg, serverCA, f("ingest-"+s+".crt"), f("ingest-"+s+".key")); err == nil {
			t.Errorf("the receiver accepts the ingest cert of %s", s)
		}
		ingest := parseCert(t, f("ingest-"+s+".crt"))
		if len(ingest.URIs) != 1 || ingest.URIs[0].String() == want || ingest.URIs[0].String() != libVar(t, "$(ingest_identity "+s+")") {
			t.Errorf("ingest-%s: URIs %v; it must have one URI SAN that differs from the agent identity", s, ingest.URIs)
		}
		// A gateway port of spoke s: the server cert of the gateway, only the ingest CA of s.
		kp, err := tls.LoadX509KeyPair(f("gateway.crt"), f("gateway.key"))
		if err != nil {
			t.Fatal(err)
		}
		gcfg := &tls.Config{Certificates: []tls.Certificate{kp}, ClientAuth: tls.RequireAndVerifyClientCert,
			ClientCAs: pool(t, f("ingest-ca-"+s+".crt")), MinVersion: tls.VersionTLS12}
		if _, err := handshake(t, gcfg, serverCA, f("ingest-"+s+".crt"), f("ingest-"+s+".key")); err != nil {
			t.Errorf("gateway port of %s refuses its own ingest cert: %v", s, err)
		}
		if _, err := handshake(t, gcfg, serverCA, f("agent-"+s+".crt"), f("agent-"+s+".key")); err == nil {
			t.Errorf("gateway port of %s accepts the agent cert", s)
		}
		if _, err := handshake(t, gcfg, serverCA, "", ""); err == nil {
			t.Errorf("gateway port of %s accepts a client without a cert", s)
		}
		for _, o := range ss {
			if o == s {
				continue
			}
			if _, err := handshake(t, gcfg, serverCA, f("ingest-"+o+".crt"), f("ingest-"+o+".key")); err == nil {
				t.Errorf("gateway port of %s accepts the ingest cert of %s", s, o)
			}
		}
	}
	fi, err := os.Stat(f("agent-ca.key"))
	if err != nil {
		t.Fatalf("agent-ca.key: %v", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("agent-ca.key: mode %v, want no access for group and others", fi.Mode())
	}
}
