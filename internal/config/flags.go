package config

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"strings"
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
	UIListen         string
	UIAuth           string
	UIAdminGroup     string
	UIAllowedHosts   stringList
}

// stringList is a flag value that each use of the flag adds to.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
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
	fs.StringVar(&f.UIListen, "ui-listen", "", "listener for the read-only policy dashboard (empty: disabled)")
	fs.StringVar(&f.UIAuth, "ui-auth", "oidc", "dashboard authentication (oidc|none); none must only be reachable from a trusted network")
	fs.StringVar(&f.UIAdminGroup, "ui-admin-group", "", "with --ui-auth=oidc: group a token must carry to open the dashboard (empty: any valid token)")
	fs.Var(&f.UIAllowedHosts, "ui-allowed-host", "host name that the dashboard accepts in the Host header, with any port; "+
		"repeatable (localhost, 127.0.0.1, [::1] and the host of --ui-listen are always accepted)")
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
	if f.UIListen != "" {
		if f.UIListen == f.Listen || f.UIListen == f.ManagementListen {
			return fmt.Errorf("--ui-listen must differ from --listen and --management-listen")
		}
		if f.UIAuth != "oidc" && f.UIAuth != "none" {
			return fmt.Errorf("invalid --ui-auth %q (oidc|none)", f.UIAuth)
		}
	}
	for _, h := range f.UIAllowedHosts {
		if NormalizeHost(h) == "" {
			return fmt.Errorf("invalid --ui-allowed-host %q (a host name or an IP address, without a port)", h)
		}
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

// NormalizeHost returns the host name h in lower case, or the IP address h in canonical form without
// brackets. It returns "" when h is not a host name or an IP address without a port, for an IPv4 address in
// IPv6 form, and for an unspecified address (0.0.0.0, ::).
func NormalizeHost(h string) string {
	h = strings.ToLower(h)
	bracketed := strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]")
	if bracketed {
		h = h[1 : len(h)-1]
	}
	if ip := net.ParseIP(h); ip != nil {
		if strings.Contains(h, ":") == (ip.To4() != nil) {
			return "" // an IPv4 address in IPv6 form
		}
		if (bracketed && ip.To4() != nil) || ip.IsUnspecified() {
			return ""
		}
		return ip.String()
	}
	if bracketed || !validDNSName(h) {
		return ""
	}
	return h
}

// validDNSName reports whether h is a DNS name: labels of 1 to 63 letters, digits and hyphens, with no hyphen
// at the start or the end of a label, and 253 characters or fewer in total. The last label must not have only
// digits, so that a value such as 10.0.0.256 is not a DNS name.
func validDNSName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	last := h[strings.LastIndexByte(h, '.')+1:]
	if strings.Trim(last, "0123456789") == "" {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}
