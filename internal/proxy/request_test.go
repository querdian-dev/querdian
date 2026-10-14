package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/routes"
)

func TestNormalise(t *testing.T) {
	prom := func(path, method string) routes.Endpoint {
		return routes.Lookup(config.BackendPrometheus, path, method)
	}
	form := "application/x-www-form-urlencoded"
	tests := []struct {
		name    string
		ep      routes.Endpoint
		method  string
		target  string
		body    string
		ctype   string
		wantErr string
		check   func(t *testing.T, got map[string][]string)
	}{
		{"GET query", prom("/api/v1/query", "GET"), "GET", "/q?query=up&time=1", "", "", "", func(t *testing.T, g map[string][]string) {
			if g["query"][0] != "up" || g["time"][0] != "1" {
				t.Errorf("got %v", g)
			}
		}},
		{"POST form", prom("/api/v1/query", "POST"), "POST", "/q", "query=up", form, "", func(t *testing.T, g map[string][]string) {
			if g["query"][0] != "up" {
				t.Errorf("got %v", g)
			}
		}},
		{"POST form with charset", prom("/api/v1/query", "POST"), "POST", "/q", "query=up", form + "; charset=utf-8", "", nil},
		{"POST URL query and form merge distinct params", prom("/api/v1/query", "POST"), "POST", "/q?time=1", "query=up", form, "", func(t *testing.T, g map[string][]string) {
			if g["query"][0] != "up" || g["time"][0] != "1" {
				t.Errorf("got %v", g)
			}
		}},
		{"POST URL query plus form query", prom("/api/v1/query", "POST"), "POST", "/q?query=a", "query=b", form, "at most once", nil},
		{"GET duplicate query", prom("/api/v1/query", "GET"), "GET", "/q?query=a&query=b", "", "", "at most once", nil},
		{"POST wrong content type", prom("/api/v1/query", "POST"), "POST", "/q", `{"query":"up"}`, "application/json", "Content-Type", nil},
		{"POST no content type", prom("/api/v1/query", "POST"), "POST", "/q", "query=up", "", "Content-Type", nil},
		{"POST body too large", prom("/api/v1/query", "POST"), "POST", "/q", "query=" + strings.Repeat("a", 2<<20), form, "too large", nil},
		{"malformed query string", prom("/api/v1/query", "GET"), "GET", "/q?query=%zz", "", "", "invalid query string", nil},
		{"semicolon separator rejected", prom("/api/v1/query", "GET"), "GET", "/q?query=a;query=b", "", "", "invalid query string", nil},
		{"malformed form", prom("/api/v1/query", "POST"), "POST", "/q", "query=%zz", form, "invalid form body", nil},
		{"unknown parameter", prom("/api/v1/query", "GET"), "GET", "/q?query=up&bogus=1", "", "", "parameter not allowed: bogus", nil},
		{"storeMatch never allowed", prom("/api/v1/query", "GET"), "GET", "/q?query=up&storeMatch[]=x", "", "", "storeMatch[]", nil},
		{"unknown parameter in POST body", prom("/api/v1/query", "POST"), "POST", "/q", "query=up&storeMatch[]=x", form, "storeMatch[]", nil},
		{"thanos parameter allowed", prom("/api/v1/query", "GET"), "GET", "/q?query=up&dedup=true&partial_response=true", "", "", "", nil},
		{"multi-valued match[] allowed", prom("/api/v1/series", "GET"), "GET", "/q?match[]=a&match[]=b", "", "", "", func(t *testing.T, g map[string][]string) {
			if len(g["match[]"]) != 2 {
				t.Errorf("got %v", g)
			}
		}},
		{"missing enforcement param is not an error here", prom("/api/v1/series", "GET"), "GET", "/q", "", "", "", nil},
		{"method not allowed", prom("/api/v1/series", "GET"), "DELETE", "/q", "", "", "method DELETE not allowed", nil},
		{"deny endpoint", prom("/api/v1/admin/tsdb/delete_series", "GET"), "GET", "/q", "", "", "not allowed", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			if tc.ctype != "" {
				r.Header.Set("Content-Type", tc.ctype)
			}
			got, err := Normalise(r, tc.ep, 0)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !IsRequestError(err) {
					t.Fatalf("want request error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

// A decoded space is "canonical" for path purposes; route lookup denies it because no route matches.
func TestUpstreamPathUnknownRouteIsDeniedByLookup(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/prom/api/v1/query%20x", nil)
	p, err := UpstreamPath(r, "/prom")
	if err != nil {
		t.Fatal(err)
	}
	if ep := routes.Lookup(config.BackendPrometheus, p, http.MethodGet); ep.Kind != routes.KindDeny {
		t.Fatalf("want deny, got %v", ep.Kind)
	}
}

func TestUpstreamPath(t *testing.T) {
	tests := []struct {
		target  string
		want    string
		wantErr bool
	}{
		{"/prom/api/v1/query", "/api/v1/query", false},
		{"/prom/api/v1/label/job/values", "/api/v1/label/job/values", false},
		{"/prom", "", true},
		{"/prom/", "", true},
		{"/promx/api/v1/query", "", true},
		{"/other/api/v1/query", "", true},
		{"/prom//api/v1/query", "", true},
		{"/prom/api/../api/v1/query", "", true},
		{"/prom/api/v1/query/", "", true},
		{"/prom/api%2Fv1/query", "", true},
		{"/prom/api%2fv1/query", "", true},
		{"/prom/api%5Cv1/query", "", true},
		{"/prom/api/v1/query%00", "", true},
		{"/prom/api/v1/%2e%2e/query", "", true},
	}
	for _, tc := range tests {
		r := httptest.NewRequest(http.MethodGet, tc.target, nil)
		got, err := UpstreamPath(r, "/prom")
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: want error, got %q", tc.target, got)
			} else if !IsRequestError(err) {
				t.Errorf("%s: not a request error: %v", tc.target, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.target, got, err, tc.want)
		}
	}
}
