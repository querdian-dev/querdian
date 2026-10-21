package config

import (
	"flag"
	"strings"
	"testing"
	"time"
)

func TestLoadExample(t *testing.T) {
	c, err := Load("testdata/example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Backends) != 3 || c.Backends[2].TraceByID != TraceByIDFilter {
		t.Fatalf("unexpected backends: %+v", c.Backends)
	}
	if c.Limits.UpstreamTimeout.Std() != 5*time.Minute || c.Limits.MaxMergeBytes != 67108864 {
		t.Fatalf("unexpected limits: %+v", c.Limits)
	}
	if b, ok := c.Backend("logs"); !ok || b.TenantFederation != FederationTenantIDLabel {
		t.Fatalf("logs backend: %+v %v", b, ok)
	}
}

const base = `
auth: {issuer: "https://idp.example.com", audiences: [q]}
backends:
  - {name: m, type: prometheus, url: "http://p:9090", pathPrefix: /prom, namespaceLabel: namespace}
`

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.Claims.Username != DefaultUsernameClaim || c.Auth.Claims.Groups != DefaultGroupsClaim ||
		c.Limits.MaxPolicyTerms != 16 || c.Backends[0].TenantFederation != FederationNone {
		t.Fatalf("defaults not applied: %+v", c)
	}
	tempo := strings.Replace(base, "type: prometheus", "type: tempo", 1)
	c, err = Parse([]byte(tempo))
	if err != nil || c.Backends[0].TraceByID != TraceByIDDeny {
		t.Fatalf("tempo traceByID should default to deny: %v %+v", err, c)
	}
}

func TestValidation(t *testing.T) {
	two := func(second string) string {
		return base + "  - " + second + "\n"
	}
	tests := []struct {
		name, in, want string
	}{
		{"unknown field", base + "extra: 1\n", "unknown field"},
		{"unknown nested field", strings.Replace(base, "namespaceLabel: namespace", "namespaceLabel: namespace, bogus: 1", 1), "unknown field"},
		{"no issuer", strings.Replace(base, `issuer: "https://idp.example.com"`, `issuer: ""`, 1), "auth.issuer"},
		{"bad issuer scheme", strings.Replace(base, "https://idp.example.com", "ftp://idp", 1), "auth.issuer"},
		{"empty audiences", strings.Replace(base, "audiences: [q]", "audiences: []", 1), "audiences must not be empty"},
		{"empty audience value", strings.Replace(base, "audiences: [q]", `audiences: [""]`, 1), "empty values"},
		{"no backends", "auth: {issuer: \"https://i\", audiences: [q]}\n", "at least one backend"},
		{"empty name", strings.Replace(base, "name: m", `name: ""`, 1), "name must not be empty"},
		{"duplicate name", two(`{name: m, type: loki, url: "http://l", pathPrefix: /loki, namespaceLabel: n}`), "not unique"},
		{"unknown type", strings.Replace(base, "type: prometheus", "type: mimir", 1), "type"},
		{"bad url", strings.Replace(base, "http://p:9090", "p:9090", 1), "url"},
		{"url without scheme host", strings.Replace(base, "http://p:9090", "http://", 1), "url"},
		{"prefix no slash", strings.Replace(base, "/prom,", "prom,", 1), "pathPrefix"},
		{"prefix root", strings.Replace(base, "/prom,", "/,", 1), "pathPrefix"},
		{"prefix trailing slash", strings.Replace(base, "/prom,", "/prom/,", 1), "pathPrefix"},
		{"duplicate prefix", two(`{name: l, type: loki, url: "http://l", pathPrefix: /prom, namespaceLabel: n}`), "overlaps"},
		{"nested prefix", two(`{name: l, type: loki, url: "http://l", pathPrefix: /prom/loki, namespaceLabel: n}`), "overlaps"},
		{"similar but not nested prefix ok", "", ""},
		{"no namespace label", strings.Replace(base, "namespaceLabel: namespace", `namespaceLabel: ""`, 1), "namespaceLabel"},
		{"unknown federation", strings.Replace(base, "namespaceLabel: namespace", "namespaceLabel: namespace, tenantFederation: x", 1), "tenantFederation"},
		{"federation without header", strings.Replace(base, "namespaceLabel: namespace", "namespaceLabel: namespace, tenantFederation: tenantIdLabel", 1), "requires tenantHeader"},
		{"traceByID on non-tempo", strings.Replace(base, "namespaceLabel: namespace", "namespaceLabel: namespace, traceByID: filter", 1), "only valid on tempo"},
		{"traceByID invalid", strings.Replace(strings.Replace(base, "prometheus", "tempo", 1), "namespaceLabel: namespace", "namespaceLabel: namespace, traceByID: maybe", 1), "traceByID"},
		{"negative limit", base + "limits: {maxFanout: -1}\n", "limits must be positive"},
		{"bad duration", base + "limits: {upstreamTimeout: soon}\n", "invalid duration"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.in == "" {
				in := two(`{name: l, type: loki, url: "http://l", pathPrefix: /promo, namespaceLabel: n}`)
				if _, err := Parse([]byte(in)); err != nil {
					t.Fatalf("/prom and /promo must not overlap: %v", err)
				}
				return
			}
			_, err := Parse([]byte(tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestFlags(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	f := RegisterFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if f.ConfigFile != "/etc/querdian/config.yaml" || f.Listen != ":8080" || f.ManagementListen != ":9090" ||
		!f.LeaderElect || f.LogLevel != "info" || f.LogFormat != "json" || f.RecordUser || f.RecordQueries {
		t.Fatalf("defaults wrong: %+v", f)
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Flags){
		"same listeners":   func(f *Flags) { f.Listen = f.ManagementListen },
		"bad level":        func(f *Flags) { f.LogLevel = "loud" },
		"ui same as proxy": func(f *Flags) { f.UIListen = f.Listen },
		"bad ui auth":      func(f *Flags) { f.UIListen = ":8081"; f.UIAuth = "basic" },
		"bad format":       func(f *Flags) { f.LogFormat = "xml" },
		"empty config":     func(f *Flags) { f.ConfigFile = "" },
	} {
		g := *f
		mut(&g)
		if g.Validate() == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	f = RegisterFlags(fs)
	if err := fs.Parse([]string{"--tracing.record-user", "--leader-elect=false", "--log-format=text"}); err != nil {
		t.Fatal(err)
	}
	if !f.RecordUser || f.LeaderElect || f.LogFormat != "text" {
		t.Fatalf("parse wrong: %+v", f)
	}
}
