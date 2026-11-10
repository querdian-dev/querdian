package main

import (
	"context"
	"flag"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/querdian-dev/querdian/internal/observability"
)

func TestRunAgentFlagErrors(t *testing.T) {
	files := []string{"--receiver-url=https://hub.example.com", "--tls-cert-file=/nonexistent/tls.crt",
		"--tls-key-file=/nonexistent/tls.key", "--tls-ca-file=/nonexistent/ca.crt"}
	for name, args := range map[string][]string{
		"unknown flag":       {"--no-such-flag"},
		"no receiver":        {},
		"short interval":     append(append([]string{}, files...), "--heartbeat-interval=1s"),
		"zero timeout":       append(append([]string{}, files...), "--request-timeout=0s"),
		"bad log format":     append(append([]string{}, files...), "--log-format=xml"),
		"bad log level":      append(append([]string{}, files...), "--log-level=loud"),
		"empty lease name":   append(append([]string{}, files...), "--leader-election-id="),
		"extra argument":     append(append([]string{}, files...), "extra"),
		"missing cert files": files,
		"http receiver":      {"--receiver-url=http://hub", "--tls-cert-file=a", "--tls-key-file=b", "--tls-ca-file=c"},
	} {
		if err := run(append([]string{"agent"}, args...)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestAgentFlagDefaults(t *testing.T) {
	f := registerAgentFlags(flag.NewFlagSet("t", flag.ContinueOnError))
	if f.interval != time.Minute || !f.leaderElect || f.leaseName != "querdian-agent" || f.managementListen != ":9090" {
		t.Errorf("defaults: %+v", f)
	}
}

func TestServeManagement(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	reg := prometheus.NewRegistry()
	observability.NewAgentMetrics(reg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveManagement(ctx, addr, reg) }()
	get := func(path string) string {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := http.Get("http://" + addr + path) //nolint:noctx // test
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				return string(b)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("GET %s: no reply", path)
		return ""
	}
	if body := get("/readyz"); !strings.Contains(body, "ok") {
		t.Errorf("readyz body %q", body)
	}
	body := get("/metrics")
	for _, want := range []string{`querdian_agent_syncs_total{result="ok"} 0`, `querdian_agent_syncs_total{result="retry"} 0`,
		"querdian_agent_last_sync_timestamp_seconds 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics: %q missing in\n%s", want, body)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
