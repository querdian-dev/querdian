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

// Until M6 is complete, a multicluster block must stop startup instead of being ignored.
func TestRunRejectsMulticluster(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := `
auth: {issuer: "https://idp.example.com", audiences: [q]}
backends:
  - {name: m, type: prometheus, url: "http://p:9090", pathPrefix: /prom, namespaceLabel: namespace, clusterLabel: cluster}
multicluster: {hubCluster: mla}
`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"--config=" + path})
	if err == nil || !strings.Contains(err.Error(), "multicluster is not implemented yet") {
		t.Fatalf("want the multicluster guard, got %v", err)
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
