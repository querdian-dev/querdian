package config

import (
	"flag"
	"fmt"
	"log/slog"
)

// Flags are the process-level settings (PLAN §5.2).
type Flags struct {
	ConfigFile       string
	Listen           string
	ManagementListen string
	LeaderElect      bool
	LogLevel         string
	LogFormat        string
	RecordUser       bool
	RecordQueries    bool
}

// RegisterFlags defines all flags on fs, with PLAN §5.2 defaults, and returns the target struct.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	f := &Flags{}
	fs.StringVar(&f.ConfigFile, "config", "/etc/querdian/config.yaml", "static config file")
	fs.StringVar(&f.Listen, "listen", ":8080", "proxy listener")
	fs.StringVar(&f.ManagementListen, "management-listen", ":9090", "listener for /metrics, /healthz, /readyz")
	fs.BoolVar(&f.LeaderElect, "leader-elect", true, "leader election for the status writer")
	fs.StringVar(&f.LogLevel, "log-level", "info", "slog level (debug|info|warn|error)")
	fs.StringVar(&f.LogFormat, "log-format", "json", "log format (json|text)")
	fs.BoolVar(&f.RecordUser, "tracing.record-user", false, "add enduser.id to spans")
	fs.BoolVar(&f.RecordQueries, "tracing.record-queries", false, "add original and rewritten query to spans")
	return f
}

// Validate checks flag values.
func (f *Flags) Validate() error {
	if f.ConfigFile == "" || f.Listen == "" || f.ManagementListen == "" {
		return fmt.Errorf("--config, --listen and --management-listen must not be empty")
	}
	if f.Listen == f.ManagementListen {
		return fmt.Errorf("--listen and --management-listen must differ")
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(f.LogLevel)); err != nil {
		return fmt.Errorf("invalid --log-level %q: %w", f.LogLevel, err)
	}
	if f.LogFormat != "json" && f.LogFormat != "text" {
		return fmt.Errorf("invalid --log-format %q (json|text)", f.LogFormat)
	}
	return nil
}
