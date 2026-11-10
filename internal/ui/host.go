package ui

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/querdian-dev/querdian/internal/config"
)

// defaultHosts are the loopback names that the dashboard always accepts, with any port. A port-forward and
// the kind quickstart use them.
var defaultHosts = []string{"localhost", "127.0.0.1", "::1"}

// allowedHosts returns the set of host names that the dashboard accepts in the Host header: the loopback
// names, the host of listen when it is a specific host, and extra. The names have no port, IPv6 addresses
// have no brackets, and all names are in lower case.
func allowedHosts(listen string, extra []string) map[string]bool {
	set := map[string]bool{}
	for _, h := range defaultHosts {
		set[h] = true
	}
	if h, _, err := net.SplitHostPort(listen); err == nil && h != "" {
		if ip := net.ParseIP(h); ip == nil || !ip.IsUnspecified() {
			set[strings.ToLower(h)] = true
		}
	}
	for _, h := range extra {
		if h = config.NormalizeHost(h); h != "" {
			set[h] = true
		}
	}
	return set
}

// hostOf returns the host part of the value of a Host header, in lower case and without the brackets of an
// IPv6 address. It returns "" for an empty value, a port that is not a number, an IPv6 address without
// brackets and brackets around a value that is not an IPv6 address.
func hostOf(hostport string) string {
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
			return ""
		}
	}
	if h == "" || strings.HasPrefix(hostport, "[") != strings.Contains(h, ":") {
		return ""
	}
	return strings.ToLower(h)
}

// guard rejects each request whose Host header does not name an allowed host. This protects the dashboard
// against DNS rebinding. For methods other than GET, guard also rejects a request from a different
// origin: an Origin header with a host that is not allowed, or fetch metadata that is not same-origin.
func guard(allowed map[string]bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := hostOf(r.Host); h == "" || !allowed[h] {
			writeErr(w, http.StatusForbidden, "host not allowed")
			return
		}
		if r.Method != http.MethodGet && !sameOrigin(allowed, r) {
			writeErr(w, http.StatusForbidden, "cross-origin request not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin reports whether the Origin and Sec-Fetch-Site headers, when they are present, show a request
// from the dashboard itself. A client that is not a browser sends neither header.
func sameOrigin(allowed map[string]bool, r *http.Request) bool {
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
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" || u.User != nil {
		return false
	}
	h := hostOf(u.Host)
	return h != "" && allowed[h]
}
