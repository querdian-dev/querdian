package ui

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

const testBody = `{"groups":["team-a"],"backend":"metrics","query":"up"}`

// request is a request to h with the Host header host and the other headers in hdr.
func request(h http.Handler, method, path, host string, hdr map[string]string) *httptest.ResponseRecorder {
	var body string
	if method == http.MethodPost {
		body = testBody
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = host
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var guardedPaths = []struct{ method, path string }{
	{"GET", "/"},
	{"GET", "/app.css"},
	{"GET", "/app.js"},
	{"GET", "/api/overview"},
	{"GET", "/api/policies"},
	{"GET", "/api/subjects"},
	{"GET", "/api/access?groups=team-a"},
	{"GET", "/api/backends"},
	{"POST", "/api/test"},
}

// TestHostAllowList: the dashboard answers only for an allowed host in the Host header. A different host is
// what a browser sends after DNS rebinding.
func TestHostAllowList(t *testing.T) {
	d := newDeps(t, true)
	d.Listen = ":8081"
	d.AllowedHosts = []string{"Querdian.Example.COM", "10.1.2.3", "[fd00::1]", "fd00:0:0::2"}
	h := Handler(d)
	for _, tc := range []struct {
		host string
		want int
	}{
		{"evil.example.net", http.StatusForbidden},
		{"evil.example.net:8081", http.StatusForbidden},
		{"localhost.evil.example.net", http.StatusForbidden},
		{"example.com", http.StatusForbidden},
		{"", http.StatusForbidden},
		{":8081", http.StatusForbidden},
		{"localhost:abc", http.StatusForbidden},
		{"localhost.", http.StatusForbidden},
		{"::1", http.StatusForbidden},
		{"[localhost]:8081", http.StatusForbidden},
		{"[127.0.0.1]", http.StatusForbidden},
		{"0.0.0.0:8081", http.StatusForbidden},
		{"localhost:8081", http.StatusOK},
		{"localhost", http.StatusOK},
		{"LocalHost:8081", http.StatusOK},
		{"127.0.0.1", http.StatusOK},
		{"127.0.0.1:30081", http.StatusOK},
		{"[::1]:8081", http.StatusOK},
		{"[::1]", http.StatusOK},
		{"querdian.example.com", http.StatusOK},
		{"QUERDIAN.example.com:443", http.StatusOK},
		{"10.1.2.3:8081", http.StatusOK},
		{"[fd00::1]:8081", http.StatusOK},
		{"[FD00:0::1]:8081", http.StatusOK},
		{"[fd00::2]", http.StatusOK},
		{"[fd00:0000::2]:8081", http.StatusOK},
		{"[0:0:0:0:0:0:0:1]:8081", http.StatusOK},
		{"[::ffff:127.0.0.1]:8081", http.StatusForbidden},
		{"[::ffff:10.1.2.3]", http.StatusForbidden},
		{"fd00::1", http.StatusForbidden},
		{"[fd00::3]", http.StatusForbidden},
	} {
		for _, p := range guardedPaths {
			w := request(h, p.method, p.path, tc.host, nil)
			if w.Code != tc.want {
				t.Errorf("Host %q, %s %s: got %d, want %d (%s)", tc.host, p.method, p.path, w.Code, tc.want, w.Body)
			}
			if tc.want == http.StatusForbidden {
				if b := w.Body.String(); !strings.Contains(b, "host not allowed") || strings.Contains(b, "team-a") {
					t.Errorf("Host %q, %s %s: body %s", tc.host, p.method, p.path, b)
				}
				if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("Cache-Control") != "no-store" {
					t.Errorf("Host %q, %s %s: the refusal lacks the security headers", tc.host, p.method, p.path)
				}
			}
		}
	}
}

// TestUnspecifiedAllowedHostAddsNothing: an unspecified address in the allowed hosts is not a host name, so
// it adds nothing.
func TestUnspecifiedAllowedHostAddsNothing(t *testing.T) {
	d := newDeps(t, true)
	d.AllowedHosts = []string{"0.0.0.0", "::", "[::]", "10.0.0.256", "1.2.3"}
	h := Handler(d)
	for _, host := range []string{"0.0.0.0:8081", "0.0.0.0", "[::]:8081", "[::]", "10.0.0.256", "1.2.3:8081"} {
		if c := request(h, "GET", "/api/overview", host, nil).Code; c != http.StatusForbidden {
			t.Errorf("Host %q: got %d, want 403", host, c)
		}
	}
}

// TestListenHostIsAllowed: the host of a specific --ui-listen address is allowed. An unspecified address is not
// a host name, so it adds nothing.
func TestListenHostIsAllowed(t *testing.T) {
	for _, tc := range []struct {
		listen, host string
		want         int
	}{
		{"192.0.2.10:8081", "192.0.2.10:8081", http.StatusOK},
		{"[2001:db8::10]:8081", "[2001:db8::10]:8081", http.StatusOK},
		{"Dashboard.Internal:8081", "dashboard.internal:8081", http.StatusOK},
		{"[2001:db8:0::10]:8081", "[2001:DB8::10]:8081", http.StatusOK},
		{"[2001:db8::10]:8081", "[2001:db8:0:0::10]", http.StatusOK},
		{"0.0.0.0:8081", "0.0.0.0:8081", http.StatusForbidden},
		{"[::]:8081", "[::]:8081", http.StatusForbidden},
		{":8081", "evil.example.net:8081", http.StatusForbidden},
		{"192.0.2.10:8081", "192.0.2.11:8081", http.StatusForbidden},
	} {
		d := newDeps(t, true)
		d.Listen = tc.listen
		if c := request(Handler(d), "GET", "/api/overview", tc.host, nil).Code; c != tc.want {
			t.Errorf("listen %q, Host %q: got %d, want %d", tc.listen, tc.host, c, tc.want)
		}
	}
}

// TestCrossOriginPost: a request with a method other than GET must not come from a different origin.
func TestCrossOriginPost(t *testing.T) {
	d := newDeps(t, true)
	d.AllowedHosts = []string{"querdian.example.com"}
	h := Handler(d)
	for _, tc := range []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"no browser headers", nil, http.StatusOK},
		{"same origin", map[string]string{"Origin": "http://localhost:8081", "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"user typed", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK},
		{"foreign origin", map[string]string{"Origin": "http://evil.example.net"}, http.StatusForbidden},
		{"foreign origin with port", map[string]string{"Origin": "http://evil.example.net:8081"}, http.StatusForbidden},
		{"null origin", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"origin not http", map[string]string{"Origin": "file://localhost"}, http.StatusForbidden},
		{"origin with a path", map[string]string{"Origin": "http://localhost:8081/x"}, http.StatusForbidden},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"cross-site with a local origin", map[string]string{"Origin": "http://localhost:8081", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site", map[string]string{"Origin": "http://localhost:3000", "Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"other port", map[string]string{"Origin": "http://localhost:3000"}, http.StatusForbidden},
		{"other allowed host", map[string]string{"Origin": "http://127.0.0.1:8081"}, http.StatusForbidden},
		{"other allowed host, ingress", map[string]string{"Origin": "https://querdian.example.com"}, http.StatusForbidden},
		{"other scheme port", map[string]string{"Origin": "https://localhost"}, http.StatusForbidden},
		{"origin with a query", map[string]string{"Origin": "http://localhost:8081?x"}, http.StatusForbidden},
		{"origin with an empty query", map[string]string{"Origin": "http://localhost:8081?"}, http.StatusForbidden},
		{"origin with an empty fragment", map[string]string{"Origin": "http://localhost:8081#"}, http.StatusForbidden},
		{"origin with a slash", map[string]string{"Origin": "http://localhost:8081/"}, http.StatusForbidden},
		{"origin with a user", map[string]string{"Origin": "http://u@localhost:8081"}, http.StatusForbidden},
		{"origin in upper case", map[string]string{"Origin": "HTTP://localhost:8081"}, http.StatusForbidden},
		{"origin with a bad port", map[string]string{"Origin": "http://localhost:x"}, http.StatusForbidden},
		{"two origins", map[string]string{"Origin": "http://localhost:8081, http://localhost:8081"}, http.StatusForbidden},
		{"unknown fetch site", map[string]string{"Sec-Fetch-Site": "other"}, http.StatusForbidden},
	} {
		w := request(h, "POST", "/api/test", "localhost:8081", tc.hdr)
		if w.Code != tc.want {
			t.Errorf("%s: got %d, want %d (%s)", tc.name, w.Code, tc.want, w.Body)
		}
		if tc.want == http.StatusForbidden && !strings.Contains(w.Body.String(), "cross-origin request not allowed") {
			t.Errorf("%s: body %s", tc.name, w.Body)
		}
	}
	// Behind an Ingress, the Origin has the default port of its scheme, and the Host header has no port. IP
	// addresses are compared in canonical form.
	for _, tc := range []struct {
		host, origin string
		want         int
	}{
		{"querdian.example.com", "https://querdian.example.com", http.StatusOK},
		{"querdian.example.com:443", "https://querdian.example.com", http.StatusOK},
		{"querdian.example.com", "https://querdian.example.com:443", http.StatusOK},
		{"querdian.example.com", "http://querdian.example.com", http.StatusOK},
		{"querdian.example.com:443", "http://querdian.example.com", http.StatusForbidden},
		{"querdian.example.com:8443", "https://querdian.example.com", http.StatusForbidden},
		{"querdian.example.com", "https://localhost", http.StatusForbidden},
		{"[::1]:8081", "http://[0::1]:8081", http.StatusOK},
		{"[::1]:8081", "http://localhost:8081", http.StatusForbidden},
	} {
		if c := request(h, "POST", "/api/test", tc.host, map[string]string{"Origin": tc.origin}).Code; c != tc.want {
			t.Errorf("Host %q, Origin %q: got %d, want %d", tc.host, tc.origin, c, tc.want)
		}
	}
	// A GET request is read-only and a browser cannot read the answer of a cross-site request, so the Host
	// check alone applies.
	w := request(h, "GET", "/api/overview", "localhost:8081", map[string]string{"Sec-Fetch-Site": "cross-site"})
	if w.Code != http.StatusOK {
		t.Fatalf("GET with cross-site fetch metadata: %d", w.Code)
	}
}

// TestHostCheckPrecedesAuth: the Host check runs before the authentication, so a request with a different host
// never reaches the authentication.
func TestHostCheckPrecedesAuth(t *testing.T) {
	d := newDeps(t, true)
	var calls int
	d.Auth = AuthWith(func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	})
	h := Handler(d)
	if c := request(h, "GET", "/api/overview", "evil.example.net", nil).Code; c != http.StatusForbidden || calls != 0 {
		t.Fatalf("foreign host: %d, auth calls %d", c, calls)
	}
	if c := request(h, "GET", "/api/overview", "localhost", nil).Code; c != http.StatusUnauthorized || calls != 1 {
		t.Fatalf("local host: %d, auth calls %d", c, calls)
	}
}

// TestMissingAuthModeFailsClosed: without an authentication mode, the dashboard serves nothing.
func TestMissingAuthModeFailsClosed(t *testing.T) {
	nilHandler := AuthWith(func(http.Handler) http.Handler { return nil })
	for name, a := range map[string]Auth{"zero": {}, "nil middleware": AuthWith(nil), "nil handler": nilHandler} {
		d := newDeps(t, true)
		d.Auth = a
		h := Handler(d)
		for _, p := range guardedPaths {
			w := request(h, p.method, p.path, "localhost:8081", nil)
			if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "team-a") {
				t.Errorf("%s, %s %s: got %d %s", name, p.method, p.path, w.Code, w.Body)
			}
		}
	}
}

// TestRefusalsAreLogged: each refusal of the Host and Origin checks is logged at debug level, with at most
// 64 bytes of the Host header and no other request data.
func TestRefusalsAreLogged(t *testing.T) {
	var buf strings.Builder
	d := newDeps(t, true)
	d.Logger = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := Handler(d)
	long := strings.Repeat("a", 100) + ".example.net"
	request(h, "GET", "/api/overview?groups=secret-group", long, nil)
	request(h, "POST", "/api/test", "localhost:8081", map[string]string{"Origin": "http://evil.example.net"})
	request(h, "GET", "/api/overview", "localhost:8081", nil)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 log lines, got %q", lines)
	}
	for _, want := range []string{`"level":"DEBUG"`, `"reason":"host not allowed"`, `"host":"` + long[:64] + `"`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("host refusal log lacks %s: %s", want, lines[0])
		}
	}
	for _, want := range []string{`"level":"DEBUG"`, `"reason":"cross-origin request"`, `"host":"localhost:8081"`} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("origin refusal log lacks %s: %s", want, lines[1])
		}
	}
	for _, bad := range []string{long[:65], "secret-group", "evil.example.net"} {
		if strings.Contains(buf.String(), bad) {
			t.Errorf("the log contains %q: %s", bad, buf.String())
		}
	}
}

// FuzzHostOf: only the loopback names, with an optional port of digits, pass the default allow-list.
func FuzzHostOf(f *testing.F) {
	for _, s := range []string{"localhost", "localhost:8081", "[::1]:8081", "127.0.0.1", "::1", "[[::1]]", "evil.net:80",
		"localhost:", "[localhost]", "LOCALHOST:1", "localhost:8081:1", "", "[0:0::1]", "[::ffff:127.0.0.1]", "[::0.0.0.1]"} {
		f.Add(s)
	}
	allowed := allowedHosts("", nil)
	// An IPv6 literal can end with dotted digits: [::0.0.0.1] is the address ::1. The IPv4-mapped form
	// [::ffff:127.0.0.1] is a different address and is not loopback here.
	loopback := regexp.MustCompile(`^(localhost|127\.0\.0\.1|\[([0-9a-f:.]+)\])(:[0-9]*)?$`)
	f.Fuzz(func(t *testing.T, s string) {
		h := hostOf(s)
		if h != strings.ToLower(h) {
			t.Fatalf("hostOf(%q) = %q is not in lower case", s, h)
		}
		m := loopback.FindStringSubmatch(strings.ToLower(s))
		want := m != nil && (m[2] == "" || net.ParseIP(m[2]).Equal(net.IPv6loopback))
		if allowed[h] != want {
			t.Fatalf("hostOf(%q) = %q, allowed %v", s, h, allowed[h])
		}
	})
}
