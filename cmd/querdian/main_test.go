package main

import (
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
