package chart

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/querdian-dev/querdian/internal/config"
)

var update = flag.Bool("update", false, "write the golden files in testdata/")

// helmRaw returns the output of `helm template` without changes.
func helmRaw(t *testing.T, args ...string) []byte {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	out, err := exec.Command("helm", append([]string{"template", "r", "."}, args...)...).CombinedOutput() //nolint:gosec // fixed binary, test args
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", args, err, out)
	}
	return out
}

// Without the values of the multi-cluster mode, the rendered output must stay the same as before the mode
// existed. The golden files are the output of the chart before T6.7. Update them only for an intended change
// to the default output: go test ./charts/... -run TestRenderUnchanged -update
//
// The test ignores empty lines: helm versions differ in them (v4.2.0 adds some in the config.yaml block of
// the ConfigMap), and an empty line does not change what the chart deploys.
func TestRenderUnchanged(t *testing.T) {
	for name, args := range map[string][]string{
		"default":      nil,
		"all-features": {"-f", "ci/all-features-values.yaml"},
	} {
		t.Run(name, func(t *testing.T) {
			got := helmRaw(t, args...)
			golden := filepath.Join("testdata", name+".golden.yaml")
			if *update {
				if err := os.WriteFile(golden, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden) //nolint:gosec // test fixture path
			if err != nil {
				t.Fatal(err)
			}
			if withoutEmptyLines(got) != withoutEmptyLines(want) {
				t.Errorf("the render differs from %s; a change to the default output needs -update and a review", golden)
			}
		})
	}
}

// withoutEmptyLines returns b without the lines that are empty or contain only spaces.
func withoutEmptyLines(b []byte) string {
	var out []string
	for line := range strings.Lines(string(b)) {
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimRight(line, "\n"))
		}
	}
	return strings.Join(out, "\n")
}

type rbacRule struct {
	APIGroups     []string `json:"apiGroups"`
	Resources     []string `json:"resources"`
	ResourceNames []string `json:"resourceNames,omitempty"`
	Verbs         []string `json:"verbs"`
}

// leaseRules are the lease rules of a component: create for all names, because create cannot have
// resourceNames, and get and update for its own lease only.
func leaseRules(name string) []rbacRule {
	return []rbacRule{
		rule(coord, []string{"leases"}, []string{"create"}),
		{APIGroups: coord, Resources: []string{"leases"}, ResourceNames: []string{name}, Verbs: []string{"get", "update"}},
	}
}

// rbacRules returns the rules of each ClusterRole and Role by "<kind>/<name>".
func rbacRules(t *testing.T, docs []map[string]any) map[string][]rbacRule {
	t.Helper()
	out := map[string][]rbacRule{}
	for _, d := range docs {
		kind, _ := d["kind"].(string)
		if kind != "ClusterRole" && kind != "Role" {
			continue
		}
		raw, err := yaml.Marshal(d["rules"])
		if err != nil {
			t.Fatal(err)
		}
		var rules []rbacRule
		if err := yaml.UnmarshalStrict(raw, &rules); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		name, _ := d["metadata"].(map[string]any)["name"].(string)
		out[kind+"/"+name] = rules
	}
	return out
}

func rule(groups, resources, verbs []string) rbacRule {
	return rbacRule{APIGroups: groups, Resources: resources, Verbs: verbs}
}

var (
	core   = []string{""}
	policy = []string{"policy.querdian.dev"}
	coord  = []string{"coordination.k8s.io"}
	read   = []string{"get", "list", "watch"}
	status = []string{"update", "patch"}

	proxyRules = []rbacRule{
		rule(policy, []string{"clusteraccesspolicies", "accesspolicies"}, read),
		rule(policy, []string{"clusteraccesspolicies/status", "accesspolicies/status"}, status),
		rule(core, []string{"namespaces"}, read),
	}
	proxyLeaderRules = []rbacRule{
		rule(coord, []string{"leases"}, []string{"get", "list", "watch", "create", "update", "patch"}),
		rule(core, []string{"events"}, []string{"create", "patch"}),
	}
)

// Each mode gets exactly these RBAC rules (PLAN §18.5, T6.7). The receiver writes SpokePolicy and only the
// status of SpokeCluster; the proxy reads the spoke objects and writes only the status of SpokePolicy; the
// agent gets the minimum of the internal/agent package comment. No rule has a wildcard.
func TestRBACRulesByMode(t *testing.T) {
	cases := map[string]struct {
		args []string
		want map[string][]rbacRule
	}{
		"single cluster": {
			want: map[string][]rbacRule{
				"ClusterRole/r-querdian":          proxyRules,
				"Role/r-querdian-leader-election": proxyLeaderRules,
			},
		},
		"hub": {
			args: []string{"-f", "ci/hub-values.yaml"},
			want: map[string][]rbacRule{
				"ClusterRole/r-querdian": append(slices.Clone(proxyRules),
					rule(policy, []string{"spokeclusters", "spokepolicies"}, read),
					rule(policy, []string{"spokepolicies/status"}, status),
				),
				"Role/r-querdian-leader-election": proxyLeaderRules,
				"ClusterRole/r-querdian-receiver": {
					rule(policy, []string{"spokeclusters"}, []string{"get", "list"}),
					rule(policy, []string{"spokeclusters/status"}, []string{"update"}),
					rule(policy, []string{"spokepolicies"}, []string{"list", "create", "update", "delete"}),
				},
				"Role/r-querdian-receiver-leader-election": append(leaseRules("querdian-receiver-leader-election"),
					rule(core, []string{"events"}, []string{"create", "patch"}),
				),
			},
		},
		"hub without receiver": {
			args: []string{"-f", "ci/hub-values.yaml", "--set", "receiver.enabled=false"},
			want: map[string][]rbacRule{
				"ClusterRole/r-querdian": append(slices.Clone(proxyRules),
					rule(policy, []string{"spokeclusters", "spokepolicies"}, read),
					rule(policy, []string{"spokepolicies/status"}, status),
				),
				"Role/r-querdian-leader-election": proxyLeaderRules,
			},
		},
		"spoke": {
			args: []string{"-f", "ci/spoke-values.yaml"},
			want: map[string][]rbacRule{
				"ClusterRole/r-querdian-agent": {
					rule(policy, []string{"clusteraccesspolicies", "accesspolicies"}, read),
					rule(core, []string{"namespaces"}, read),
					rule(policy, []string{"clusteraccesspolicies/status", "accesspolicies/status"}, status),
				},
				"Role/r-querdian-agent-leader-election": leaseRules("r-querdian-agent"),
			},
		},
		"spoke without RBAC": {
			args: []string{"-f", "ci/spoke-values.yaml", "--set", "rbac.create=false"},
			want: map[string][]rbacRule{},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			docs := helmTemplate(t, tc.args...)
			got := rbacRules(t, docs)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("RBAC rules:\n got  %+v\n want %+v", got, tc.want)
			}
			for role, rules := range got {
				for _, r := range rules {
					for _, v := range slices.Concat(r.APIGroups, r.Resources, r.Verbs) {
						if strings.Contains(v, "*") {
							t.Errorf("%s has a wildcard: %+v", role, r)
						}
					}
				}
			}
			assertBindings(t, docs)
		})
	}
}

// assertBindings checks that each binding refers to a rendered role and a rendered ServiceAccount of the
// component that uses the role.
func assertBindings(t *testing.T, docs []map[string]any) {
	t.Helper()
	names := map[string]bool{}
	for _, d := range docs {
		names[d["kind"].(string)+"/"+d["metadata"].(map[string]any)["name"].(string)] = true
	}
	for _, d := range docs {
		kind := d["kind"].(string)
		if kind != "ClusterRoleBinding" && kind != "RoleBinding" {
			continue
		}
		ref := d["roleRef"].(map[string]any)
		if !names[ref["kind"].(string)+"/"+ref["name"].(string)] {
			t.Errorf("%s refers to the missing %s/%s", kind, ref["kind"], ref["name"])
		}
		subjects := d["subjects"].([]any)
		if len(subjects) != 1 {
			t.Fatalf("%s has %d subjects", kind, len(subjects))
		}
		sa := subjects[0].(map[string]any)["name"].(string)
		if !names["ServiceAccount/"+sa] {
			t.Errorf("%s binds the missing ServiceAccount %s", kind, sa)
		}
		// The role and the ServiceAccount belong to the same component.
		if base := strings.TrimSuffix(ref["name"].(string), "-leader-election"); base != sa {
			t.Errorf("%s binds %s to ServiceAccount %s", kind, ref["name"], sa)
		}
	}
}

// objects returns the rendered objects by "<kind>/<name>".
func objects(docs []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, d := range docs {
		out[d["kind"].(string)+"/"+d["metadata"].(map[string]any)["name"].(string)] = d
	}
	return out
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Mode spoke installs only the agent: no proxy, no proxy RBAC, no proxy Service. The agent and the proxy are
// mutually exclusive, because both write the status of the spoke policies; TestModeValuesRejected checks that
// agent values with mode proxy stop the render.
func TestSpokeModeInstallsOnlyTheAgent(t *testing.T) {
	got := keys(objects(helmTemplate(t, "-f", "ci/spoke-values.yaml")))
	want := []string{
		"ClusterRole/r-querdian-agent",
		"ClusterRoleBinding/r-querdian-agent",
		"Deployment/r-querdian-agent",
		"NetworkPolicy/r-querdian-agent",
		"Role/r-querdian-agent-leader-election",
		"RoleBinding/r-querdian-agent-leader-election",
		"ServiceAccount/r-querdian-agent",
	}
	if !slices.Equal(got, want) {
		t.Errorf("objects:\n got  %v\n want %v", got, want)
	}
}

func container(t *testing.T, deploy map[string]any) (spec, c map[string]any) {
	t.Helper()
	spec = deploy["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	cs := spec["containers"].([]any)
	if len(cs) != 1 {
		t.Fatalf("%d containers", len(cs))
	}
	return spec, cs[0].(map[string]any)
}

func args(c map[string]any) []string {
	var out []string
	for _, a := range c["args"].([]any) {
		out = append(out, a.(string))
	}
	return out
}

func TestAgentDeployment(t *testing.T) {
	objs := objects(helmTemplate(t, "-f", "ci/spoke-values.yaml", "--set", "agent.heartbeatInterval=30s"))
	spec, c := container(t, objs["Deployment/r-querdian-agent"])
	if spec["serviceAccountName"] != "r-querdian-agent" {
		t.Errorf("serviceAccountName = %v", spec["serviceAccountName"])
	}
	want := []string{
		"agent",
		"--receiver-url=https://querdian-receiver.mla.example.com:8443",
		"--tls-cert-file=/etc/querdian-agent/tls/tls.crt",
		"--tls-key-file=/etc/querdian-agent/tls/tls.key",
		"--tls-ca-file=/etc/querdian-agent/receiver-ca/ca.crt",
		"--heartbeat-interval=30s",
		"--leader-elect=true",
		"--leader-election-id=r-querdian-agent",
		"--management-listen=:9090",
		"--log-level=info",
		"--log-format=json",
	}
	if got := args(c); !slices.Equal(got, want) {
		t.Errorf("args:\n got  %v\n want %v", got, want)
	}
	raw, _ := yaml.Marshal(spec)
	for _, s := range []string{"secretName: querdian-agent-tls", "readOnlyRootFilesystem: true", "runAsNonRoot: true", "/readyz"} {
		if !strings.Contains(string(raw), s) {
			t.Errorf("agent pod lacks %q", s)
		}
	}
	// A subPath mount would stop the rotation of the cert files.
	if strings.Contains(string(raw), "subPath") {
		t.Error("agent pod uses subPath")
	}
}

func TestReceiverDeployment(t *testing.T) {
	objs := objects(helmTemplate(t, "-f", "ci/hub-values.yaml"))
	d := objs["Deployment/r-querdian-receiver"]
	if d == nil {
		t.Fatalf("no receiver Deployment; objects: %v", keys(objs))
	}
	// Only the leader is ready, so a rolling update with maxUnavailable 0 would never finish.
	if s := d["spec"].(map[string]any)["strategy"].(map[string]any); s["type"] != "Recreate" {
		t.Errorf("strategy = %v", s)
	}
	spec, c := container(t, d)
	if spec["serviceAccountName"] != "r-querdian-receiver" {
		t.Errorf("serviceAccountName = %v", spec["serviceAccountName"])
	}
	want := []string{
		"receiver",
		"--listen=:8443",
		"--management-listen=:9090",
		"--tls-cert-file=/etc/querdian-receiver/tls/tls.crt",
		"--tls-key-file=/etc/querdian-receiver/tls/tls.key",
		"--client-ca-file=/etc/querdian-receiver/client-ca/ca.crt",
		"--leader-elect=true",
		"--log-level=info",
		"--log-format=json",
	}
	if got := args(c); !slices.Equal(got, want) {
		t.Errorf("args:\n got  %v\n want %v", got, want)
	}
	raw, _ := yaml.Marshal(spec)
	for _, s := range []string{"secretName: querdian-receiver-tls", "secretName: querdian-agent-ca", "/readyz"} {
		if !strings.Contains(string(raw), s) {
			t.Errorf("receiver pod lacks %q", s)
		}
	}
	svc := objs["Service/r-querdian-receiver"]
	if svc == nil {
		t.Fatal("no receiver Service")
	}
	if p := svc["spec"].(map[string]any)["ports"].([]any)[0].(map[string]any); p["targetPort"] != "receiver" {
		t.Errorf("receiver Service port = %v", p)
	}
	if objs["ServiceAccount/r-querdian-receiver"] == nil || objs["ServiceAccount/r-querdian"] == nil {
		t.Error("the receiver and the proxy need their own ServiceAccount")
	}
}

// A NodePort receiver can have a fixed node port, so that the receiver URL of the agents stays the same after a
// reinstall (the multi-cluster kind quickstart uses it). The port is set only with type NodePort.
func TestReceiverNodePort(t *testing.T) {
	port := func(args ...string) any {
		objs := objects(helmTemplate(t, append([]string{"-f", "ci/hub-values.yaml"}, args...)...))
		svc := objs["Service/r-querdian-receiver"]
		if svc == nil {
			t.Fatal("no receiver Service")
		}
		return svc["spec"].(map[string]any)["ports"].([]any)[0].(map[string]any)["nodePort"]
	}
	if got := port("--set", "receiver.service.type=NodePort", "--set", "receiver.service.nodePort=30443"); got != float64(30443) {
		t.Errorf("NodePort: nodePort = %v, want 30443", got)
	}
	if got := port("--set", "receiver.service.type=NodePort"); got != nil {
		t.Errorf("NodePort without a value: nodePort = %v, want none", got)
	}
	if got := port("--set", "receiver.service.type=ClusterIP", "--set", "receiver.service.nodePort=30443"); got != nil {
		t.Errorf("ClusterIP: nodePort = %v, want none", got)
	}
}

// The selectors of the proxy must not select the receiver pods, and the reverse. Otherwise the proxy Service
// sends queries to the receiver, or the receiver Service sends syncs to the proxy.
func TestSelectorsAreDisjoint(t *testing.T) {
	objs := objects(helmTemplate(t, "-f", "ci/hub-values.yaml"))
	podLabels := func(deploy string) map[string]any {
		return objs[deploy]["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
	}
	selects := func(sel, labels map[string]any) bool {
		for k, v := range sel {
			if labels[k] != v {
				return false
			}
		}
		return true
	}
	proxyPods, receiverPods := podLabels("Deployment/r-querdian"), podLabels("Deployment/r-querdian-receiver")
	for _, tc := range []struct {
		selector string
		sel      map[string]any
		own      map[string]any
		other    map[string]any
	}{
		{"Service/r-querdian", objs["Service/r-querdian"]["spec"].(map[string]any)["selector"].(map[string]any), proxyPods, receiverPods},
		{"Service/r-querdian-management", objs["Service/r-querdian-management"]["spec"].(map[string]any)["selector"].(map[string]any), proxyPods, receiverPods},
		{"PodDisruptionBudget/r-querdian", objs["PodDisruptionBudget/r-querdian"]["spec"].(map[string]any)["selector"].(map[string]any)["matchLabels"].(map[string]any), proxyPods, receiverPods},
		{"NetworkPolicy/r-querdian", objs["NetworkPolicy/r-querdian"]["spec"].(map[string]any)["podSelector"].(map[string]any)["matchLabels"].(map[string]any), proxyPods, receiverPods},
		{"Service/r-querdian-receiver", objs["Service/r-querdian-receiver"]["spec"].(map[string]any)["selector"].(map[string]any), receiverPods, proxyPods},
		{"NetworkPolicy/r-querdian-receiver", objs["NetworkPolicy/r-querdian-receiver"]["spec"].(map[string]any)["podSelector"].(map[string]any)["matchLabels"].(map[string]any), receiverPods, proxyPods},
	} {
		if !selects(tc.sel, tc.own) {
			t.Errorf("%s does not select its own pods", tc.selector)
		}
		if selects(tc.sel, tc.other) {
			t.Errorf("%s also selects the pods of the other component", tc.selector)
		}
	}
}

func TestHubConfigIsValid(t *testing.T) {
	for _, d := range helmTemplate(t, "-f", "ci/hub-values.yaml") {
		if d["kind"] != "ConfigMap" {
			continue
		}
		raw := d["data"].(map[string]any)["config.yaml"].(string)
		cfg, err := config.Parse([]byte(raw))
		if err != nil {
			t.Fatalf("rendered config rejected by config.Parse: %v\n%s", err, raw)
		}
		if cfg.Multicluster == nil || cfg.Multicluster.HubCluster != "mla" {
			t.Fatalf("multicluster = %+v", cfg.Multicluster)
		}
		for _, b := range cfg.Backends {
			if b.ClusterLabel == "" {
				t.Errorf("backend %s has no clusterLabel", b.Name)
			}
		}
		return
	}
	t.Fatal("no ConfigMap rendered")
}

func TestPrometheusRule(t *testing.T) {
	objs := objects(helmTemplate(t, "-f", "ci/hub-values.yaml", "--set", "prometheusRule.policyAgeThresholdSeconds=7200"))
	pr := objs["PrometheusRule/r-querdian"]
	if pr == nil {
		t.Fatalf("no PrometheusRule; objects: %v", keys(objs))
	}
	exprs := map[string]string{}
	for _, g := range pr["spec"].(map[string]any)["groups"].([]any) {
		for _, r := range g.(map[string]any)["rules"].([]any) {
			r := r.(map[string]any)
			exprs[r["alert"].(string)] = r["expr"].(string)
			if !strings.Contains(r["annotations"].(map[string]any)["summary"].(string), "{{ $labels.cluster }}") {
				t.Errorf("%s: the summary lacks the cluster template", r["alert"])
			}
		}
	}
	want := map[string]string{
		"QuerdianSpokePolicyStale":   "max by (cluster) (querdian_cluster_policy_age_seconds) > 7200",
		"QuerdianSpokePolicyExpired": "max by (cluster) (querdian_cluster_policy_expired) == 1",
	}
	if !reflect.DeepEqual(exprs, want) {
		t.Errorf("alerts:\n got  %v\n want %v", exprs, want)
	}
	if objs := objects(helmTemplate(t, "-f", "ci/hub-values.yaml", "--set", "prometheusRule.enabled=false")); objs["PrometheusRule/r-querdian"] != nil {
		t.Error("PrometheusRule rendered with prometheusRule.enabled=false")
	}
}

// Wrong combinations stop the render; nothing installs in a state that fails open or does not work.
func TestModeValuesRejected(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	hub := []string{"-f", "ci/hub-values.yaml"}
	spoke := []string{"-f", "ci/spoke-values.yaml"}
	with := func(base []string, set ...string) []string {
		out := slices.Clone(base)
		for _, s := range set {
			out = append(out, "--set", s)
		}
		return out
	}
	// want is a part of the expected error, so that a case cannot pass because of a different error.
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"unknown mode":                      {with(nil, "mode=hub"), "value must be one of 'proxy', 'spoke'"},
		"receiver without multicluster":     {with(nil, "receiver.enabled=true", "receiver.tls.existingSecret=a", "receiver.clientCA.existingSecret=b"), "receiver.enabled needs config.multicluster"},
		"receiver without server cert":      {with(hub, "receiver.tls.existingSecret="), "needs receiver.tls.existingSecret"},
		"receiver without client CA":        {with(hub, "receiver.clientCA.existingSecret="), "needs receiver.clientCA.existingSecret"},
		"receiver replicas without lease":   {with(hub, "receiver.leaderElection.enabled=false", "receiver.replicaCount=2"), "needs receiver.replicaCount=1"},
		"receiver ports equal":              {with(hub, "receiver.managementPort=8443"), "must differ"},
		"rule without multicluster":         {with(nil, "prometheusRule.enabled=true"), "prometheusRule.enabled needs config.multicluster"},
		"multicluster without clusterLabel": {with(nil, "config.multicluster.hubCluster=mla"), "missing property 'clusterLabel'"},
		"clusterLabel without multicluster": {[]string{"-f", "testdata/clusterlabel-without-multicluster.yaml"}, "'not' failed"},
		"multicluster without hubCluster":   {with(hub, "config.multicluster.hubCluster="), "/config/multicluster/hubCluster"},
		"spoke without receiver URL":        {with(spoke, "agent.receiverURL="), "needs agent.receiverURL"},
		"spoke with http receiver URL":      {with(spoke, "agent.receiverURL=http://hub:8443"), "/agent/receiverURL"},
		"spoke without client cert":         {with(spoke, "agent.tls.existingSecret="), "needs agent.tls.existingSecret"},
		"spoke without receiver CA":         {with(spoke, "agent.receiverCA.existingSecret="), "needs agent.receiverCA.existingSecret"},
		"spoke with receiver":               {with(spoke, "receiver.enabled=true"), "not possible with mode spoke"},
		"spoke with prometheusRule":         {with(spoke, "prometheusRule.enabled=true"), "not possible with mode spoke"},
		"spoke with multicluster":           {with(spoke, "config.multicluster.hubCluster=mla"), "missing property 'clusterLabel'"},
		"spoke with serviceMonitor":         {with(spoke, "serviceMonitor.enabled=true"), "mode spoke installs only the agent"},
		"spoke with dashboard":              {with(spoke, "ui.enabled=true"), "mode spoke installs only the agent"},
		"agent replicas without lease":      {with(spoke, "agent.leaderElection.enabled=false", "agent.replicaCount=2"), "needs agent.replicaCount=1"},
		"agent heartbeat not a duration":    {with(spoke, "agent.heartbeatInterval=often"), "/agent/heartbeatInterval"},
		"agent heartbeat below 10s":         {with(spoke, "agent.heartbeatInterval=9s"), "must be at least 10s"},
		"agent heartbeat of 1s":             {with(spoke, "agent.heartbeatInterval=1s"), "must be at least 10s"},
		// The agent accepts each Go duration, but the chart accepts only <n>s, <n>m and <n>h, so that it can
		// compare the value with 10s. The other forms can hide a value below 10s. The schema rejects them
		// first; the template check stays as a second check.
		"agent heartbeat in milliseconds": {with(spoke, "agent.heartbeatInterval=9000ms"), "does not match pattern"},
		"agent heartbeat with a fraction": {with(spoke, "agent.heartbeatInterval=0.1m"), "does not match pattern"},
		"agent heartbeat leading zero":    {with(spoke, "agent.heartbeatInterval=05s"), "does not match pattern"},
		"agent heartbeat of zero":         {with(spoke, "agent.heartbeatInterval=0s"), "does not match pattern"},
		"agent heartbeat of two units":    {with(spoke, "agent.heartbeatInterval=1m30s"), "does not match pattern"},
		"agent heartbeat too many digits": {with(spoke, "agent.heartbeatInterval=99999999999999999999s"), "does not match pattern"},
		// The agent and the proxy are mutually exclusive: both write the status of the spoke policies.
		"agent and proxy":       {with(spoke, "mode=proxy"), "A spoke runs only the agent"},
		"agent URL with proxy":  {with(nil, "agent.receiverURL=https://hub:8443"), "A spoke runs only the agent"},
		"agent cert with proxy": {with(nil, "agent.tls.existingSecret=a"), "A spoke runs only the agent"},
		"agent CA with proxy":   {with(nil, "agent.receiverCA.existingSecret=a"), "A spoke runs only the agent"},
		"agent with hub proxy":  {with(hub, "agent.receiverURL=https://hub:8443"), "A spoke runs only the agent"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := exec.Command("helm", append([]string{"template", "r", "."}, tc.args...)...).CombinedOutput() //nolint:gosec // test args
			if err == nil {
				t.Fatalf("accepted:\n%s", out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("rejected for a different reason; want %q:\n%s", tc.want, out)
			}
		})
	}
	// The limit of 10s accepts the values at and above it.
	for _, d := range []string{"10s", "1m", "90s", "1h", "999999h"} {
		helmTemplate(t, with(spoke, "agent.heartbeatInterval="+d)...)
	}
}
