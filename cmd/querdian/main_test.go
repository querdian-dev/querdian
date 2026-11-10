package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/querdian-dev/querdian/internal/config"
)

func TestVersionDefault(t *testing.T) {
	if version == "" {
		t.Fatal("version must not be empty")
	}
}

func TestRunVersionAndFlagErrors(t *testing.T) {
	if err := run([]string{"--version"}); err != nil {
		t.Fatalf("--version: %v", err)
	}
	if err := run([]string{"--no-such-flag"}); err == nil {
		t.Fatal("unknown flag must fail")
	}
	if err := run([]string{"--listen=:1", "--management-listen=:1"}); err == nil {
		t.Fatal("identical listeners must fail")
	}
	if err := run([]string{"--config=/nonexistent/config.yaml"}); err == nil {
		t.Fatal("missing config must fail")
	}
}

// A config with the multicluster block passes each startup check and stops only where Querdian needs the
// API server. The run without the block stops at the same point.
func TestRunAcceptsMulticluster(t *testing.T) {
	// An explicit kubeconfig that does not exist makes the Kubernetes config fail, so run never connects.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-kubeconfig"))
	for name, cfg := range map[string]string{
		"multicluster": `
auth: {issuer: "https://idp.example.com", audiences: [q]}
backends:
  - {name: m, type: prometheus, url: "http://p:9090", pathPrefix: /prom, namespaceLabel: namespace, clusterLabel: cluster}
multicluster: {hubCluster: mla}
`,
		"single cluster": `
auth: {issuer: "https://idp.example.com", audiences: [q]}
backends:
  - {name: m, type: prometheus, url: "http://p:9090", pathPrefix: /prom, namespaceLabel: namespace}
`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			err := run([]string{"--config=" + path})
			if err == nil || !strings.Contains(err.Error(), "kubernetes config") {
				t.Fatalf("want a stop at the Kubernetes config, got %v", err)
			}
		})
	}
}

func TestNewLogger(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		if _, err := newLogger(&config.Flags{LogLevel: "debug", LogFormat: format}); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
	}
	if _, err := newLogger(&config.Flags{LogLevel: "loud", LogFormat: "json"}); err == nil {
		t.Fatal("bad level must fail")
	}
}
