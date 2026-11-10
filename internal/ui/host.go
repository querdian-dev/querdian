package ui

import (
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/querdian-dev/querdian/internal/config"
)

// defaultHosts are the loopback names that the dashboard always accepts, with any port. A port-forward and
// the kind quickstart use them.
var defaultHosts = []string{"localhost", "127.0.0.1", "::1"}

// maxLoggedHost is the number of bytes of a refused Host header that the debug log shows.
const maxLoggedHost = 64

// allowedHosts returns the set of host names that the dashboard accepts in the Host header: the loopback
// names, the host of listen when it is a specific host, and extra. The names have no port and are in lower
// case. IP addresses are in canonical form, without brackets.
func allowedHosts(listen string, extra []string) map[string]bool {
	set := map[string]bool{}
	for _, h := range defaultHosts {
		set[h] = true
	}
	if h, _, err := net.SplitHostPort(listen); err == nil && h != "" {
		if ip := net.ParseIP(h); ip == nil {
			set[strings.ToLower(h)] = true
		} else if !ip.IsUnspecified() {
			set[ip.String()] = true
		}
	}
	for _, h := range extra {
		if h = config.NormalizeHost(h); h != "" {
			set[h] = true
		}
	}
	return set
}

// splitHost splits the value of a Host header, or the host of an Origin, into the host and the port. The host
// is in lower case, and an IP address is in canonical form without brackets. It reports false for an empty
// host, a port that is not a number, an IPv6 address without brackets, brackets around a value that is not
// an IPv6 address, and an IPv4 address in IPv6 form.
func splitHost(hostport string) (host, port string, ok bool) {
	h, port, err := net.SplitHostPort(hostport)
	if err != nil {
		// No port.
		h, port = hostport, ""
		if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
			h = h[1 : len(h)-1]
		}
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return "", "", false
		}
	}
	bracketed := strings.HasPrefix(hostport, "[")
	if h == "" || bracketed != strings.Contains(h, ":") {
		return "", "", false
	}
	if ip := net.ParseIP(h); ip != nil {
		if bracketed && ip.To4() != nil {
			return "", "", false
		}
		return ip.String(), port, true
	}
	if bracketed {
		return "", "", false
	}
	return strings.ToLower(h), port, true
}

// hostOf returns the host part of the value of a Host header as splitHost does, or "".
func hostOf(hostport string) string {
	h, _, _ := splitHost(hostport)
	return h
}

// guard rejects each request whose Host header does not name an allowed host. This protects the dashboard
// against DNS rebinding. For methods other than GET, guard also rejects a request from a different origin.
// It logs each refusal at debug level, with the start of the Host header only.
func guard(allowed map[string]bool, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := hostOf(r.Host); h == "" || !allowed[h] {
			log.DebugContext(r.Context(), "dashboard request refused", "reason", "host not allowed", "host", truncate(r.Host))
			writeErr(w, http.StatusForbidden, "host not allowed")
			return
		}
		if r.Method != http.MethodGet && !sameOrigin(r) {
			log.DebugContext(r.Context(), "dashboard request refused", "reason", "cross-origin request", "host", truncate(r.Host))
			writeErr(w, http.StatusForbidden, "cross-origin request not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func truncate(s string) string {
	if len(s) > maxLoggedHost {
		return s[:maxLoggedHost]
	}
	return s
}

// sameOrigin reports whether the Origin and Sec-Fetch-Site headers, when they are present, show a request
// from the dashboard itself: the Origin must have the same host and port as the Host header. A client that
// is not a browser sends neither header.
func sameOrigin(r *http.Request) bool {
	if v := r.Header.Values("Sec-Fetch-Site"); len(v) > 1 || (len(v) == 1 && v[0] != "same-origin" && v[0] != "none") {
		return false
	}
	o := r.Header.Values("Origin")
	if len(o) == 0 {
		return true
	}
	if len(o) > 1 {
		return false
	}
	u, err := url.Parse(o[0])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	oh, op, ok := splitHost(u.Host)
	if !ok {
		return false
	}
	rh, rp, ok := splitHost(r.Host)
	if !ok {
		return false
	}
	def := map[string]string{"http": "80", "https": "443"}[u.Scheme]
	if op == "" {
		op = def
	}
	if rp == "" {
		rp = def
	}
	return oh == rh && op == rp
}
