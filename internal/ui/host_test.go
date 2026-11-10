package ui

import (
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
	d.AllowedHosts = []string{"Querdian.Example.COM", "10.1.2.3", "[fd00::1]"}
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
		{"ingress origin", map[string]string{"Origin": "https://querdian.example.com"}, http.StatusOK},
		{"user typed", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK},
		{"foreign origin", map[string]string{"Origin": "http://evil.example.net"}, http.StatusForbidden},
		{"foreign origin with port", map[string]string{"Origin": "http://evil.example.net:8081"}, http.StatusForbidden},
		{"null origin", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"origin not http", map[string]string{"Origin": "file://localhost"}, http.StatusForbidden},
		{"origin with a path", map[string]string{"Origin": "http://localhost:8081/x"}, http.StatusForbidden},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"cross-site with a local origin", map[string]string{"Origin": "http://localhost:8081", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site", map[string]string{"Origin": "http://localhost:3000", "Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
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
	d.Auth = func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	}
	h := Handler(d)
	if c := request(h, "GET", "/api/overview", "evil.example.net", nil).Code; c != http.StatusForbidden || calls != 0 {
		t.Fatalf("foreign host: %d, auth calls %d", c, calls)
	}
	if c := request(h, "GET", "/api/overview", "localhost", nil).Code; c != http.StatusUnauthorized || calls != 1 {
		t.Fatalf("local host: %d, auth calls %d", c, calls)
	}
}

// FuzzHostOf: only the loopback names, with an optional port of digits, pass the default allow-list.
func FuzzHostOf(f *testing.F) {
	for _, s := range []string{"localhost", "localhost:8081", "[::1]:8081", "127.0.0.1", "::1", "[[::1]]", "evil.net:80",
		"localhost:", "[localhost]", "LOCALHOST:1", "localhost:8081:1", ""} {
		f.Add(s)
	}
	allowed := allowedHosts("", nil)
	loopback := regexp.MustCompile(`^(localhost|127\.0\.0\.1|\[::1\])(:[0-9]*)?$`)
	f.Fuzz(func(t *testing.T, s string) {
		h := hostOf(s)
		if h != strings.ToLower(h) {
			t.Fatalf("hostOf(%q) = %q is not in lower case", s, h)
		}
		if allowed[h] != loopback.MatchString(strings.ToLower(s)) {
			t.Fatalf("hostOf(%q) = %q, allowed %v", s, h, allowed[h])
		}
	})
}
