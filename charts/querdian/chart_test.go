// Package chart tests the Helm chart: CRD sync with config/crd and that the rendered static
// configuration is accepted by the real config loader. Render tests need the helm binary.
package chart

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/querdian-dev/querdian/internal/config"
)

func TestCRDsMatchConfigCRD(t *testing.T) {
	src, err := filepath.Glob("../../config/crd/*.yaml")
	if err != nil || len(src) == 0 {
		t.Fatalf("no CRDs found in config/crd: %v", err)
	}
	dst, _ := filepath.Glob("crds/*.yaml")
	if len(dst) != len(src) {
		t.Fatalf("crds/ has %d files, config/crd has %d; run `make chart-crds`", len(dst), len(src))
	}
	for _, s := range src {
		want, err := os.ReadFile(s) //nolint:gosec // test fixture path
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join("crds", filepath.Base(s)))
		if err != nil {
			t.Fatalf("missing %s in crds/; run `make chart-crds`", filepath.Base(s))
		}
		if !bytes.Equal(want, got) {
			t.Errorf("crds/%s differs from config/crd; run `make chart-crds`", filepath.Base(s))
		}
	}
}

func helmTemplate(t *testing.T, args ...string) []map[string]any {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	out, err := exec.Command("helm", append([]string{"template", "r", "."}, args...)...).CombinedOutput() //nolint:gosec // fixed binary, test args
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", args, err, out)
	}
	var docs []map[string]any
	for _, d := range strings.Split(string(out), "\n---\n") {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(d), &m); err != nil {
			t.Fatalf("invalid YAML: %v\n%s", err, d)
		}
		if m != nil {
			docs = append(docs, m)
		}
	}
	return docs
}

func TestRenderedConfigIsValid(t *testing.T) {
	for name, args := range map[string][]string{
		"defaults":     nil,
		"all-features": {"-f", "ci/all-features-values.yaml"},
	} {
		t.Run(name, func(t *testing.T) {
			var found bool
			for _, d := range helmTemplate(t, args...) {
				if d["kind"] != "ConfigMap" {
					continue
				}
				found = true
				raw := d["data"].(map[string]any)["config.yaml"].(string)
				cfg, err := config.Parse([]byte(raw))
				if err != nil {
					t.Fatalf("rendered config rejected by config.Parse: %v\n%s", err, raw)
				}
				if name == "all-features" && cfg.Auth.CAFile != "/etc/querdian-oidc-ca/ca.crt" {
					t.Errorf("auth.caFile = %q", cfg.Auth.CAFile)
				}
			}
			if !found {
				t.Fatal("no ConfigMap rendered")
			}
		})
	}
}

func TestSchemaRejectsUnknownAndInvalid(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	for _, set := range []string{
		"unknownKey=1",
		"replicaCount=0",
		"logLevel=trace",
		"config.backends[0].type=mimir",
		"config.auth.audiences={}",
		"networkPolicy.bogus=true",
	} {
		if out, err := exec.Command("helm", "template", "r", ".", "--set", set).CombinedOutput(); err == nil { //nolint:gosec // test args
			t.Errorf("--set %s was accepted:\n%s", set, out)
		}
	}
}

func TestSecureDefaults(t *testing.T) {
	for _, d := range helmTemplate(t) {
		if d["kind"] != "Deployment" {
			continue
		}
		raw, _ := yaml.Marshal(d)
		for _, want := range []string{"readOnlyRootFilesystem: true", "runAsNonRoot: true", "RuntimeDefault", "- ALL", "replicas: 2", "--leader-elect=true", "/healthz", "/readyz", "checksum/config"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("deployment lacks %q", want)
			}
		}
		return
	}
	t.Fatal("no Deployment rendered")
}
