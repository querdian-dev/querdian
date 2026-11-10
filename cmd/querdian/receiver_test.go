package main

import (
	"path/filepath"
	"testing"
)

func TestReceiverFlags(t *testing.T) {
	req := []string{"--tls-cert-file=c", "--tls-key-file=k", "--client-ca-file=ca"}
	f, err := parseReceiverFlags(req)
	if err != nil {
		t.Fatal(err)
	}
	if f.listen != ":8443" || !f.leaderElect || f.maxPolicies <= 0 || f.maxBodyBytes <= 0 {
		t.Fatalf("defaults: %+v", f)
	}
	for name, args := range map[string][]string{
		"no client CA":     {"--tls-cert-file=c", "--tls-key-file=k"},
		"no cert":          {"--tls-key-file=k", "--client-ca-file=ca"},
		"no key":           {"--tls-cert-file=c", "--client-ca-file=ca"},
		"same listeners":   append([]string{"--listen=:1", "--management-listen=:1"}, req...),
		"empty listen":     append([]string{"--listen="}, req...),
		"zero body":        append([]string{"--max-body-bytes=0"}, req...),
		"negative count":   append([]string{"--max-policies=-1"}, req...),
		"zero in flight":   append([]string{"--max-in-flight=0"}, req...),
		"log format":       append([]string{"--log-format=xml"}, req...),
		"log level":        append([]string{"--log-level=loud"}, req...),
		"unknown flag":     append([]string{"--config=x"}, req...),
		"extra argument":   append(req, "extra"),
		"proxy flag shape": {"--cluster=x"},
	} {
		if _, err := parseReceiverFlags(args); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRunReceiverDispatch(t *testing.T) {
	// The subcommand runs before the proxy flags and the config file are read, and it fails on a missing
	// cert before it contacts a cluster.
	dir := t.TempDir()
	err := run([]string{"receiver", "--tls-cert-file=" + filepath.Join(dir, "c"), "--tls-key-file=" + filepath.Join(dir, "k"),
		"--client-ca-file=" + filepath.Join(dir, "ca")})
	if err == nil {
		t.Fatal("missing cert files must fail")
	}
	if err := run([]string{"receiver"}); err == nil {
		t.Fatal("receiver without the required flags must fail")
	}
}
