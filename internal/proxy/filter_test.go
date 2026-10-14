package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/querdian-dev/querdian/internal/config"
	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/routes"
)

func filterPlan(f func([]byte) ([]byte, error), limit int64) enforce.Plan {
	return enforce.Plan{
		Requests:               []enforce.Upstream{{Query: url.Values{"match[]": {"up"}}, Term: -1}},
		ResponseFilter:         f,
		ResponseFilterMaxBytes: limit,
	}
}

func runFilter(t *testing.T, status int, hdr http.Header, body string, plan enforce.Plan) (*httptest.ResponseRecorder, http.Header) {
	t.Helper()
	var seen http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		for k, v := range hdr {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer up.Close()
	w := httptest.NewRecorder()
	ep := routes.Lookup(config.BackendPrometheus, "/api/v1/labels", http.MethodGet)
	newProxy().Execute(w, clientRequest(http.MethodGet, "/prom/api/v1/labels", ""), Target{Backend: backend(t, up.URL, ""), Endpoint: ep, UpstreamPath: "/api/v1/labels"}, plan, nil)
	return w, seen
}

func TestResponseFilterApplied(t *testing.T) {
	filter := func(b []byte) ([]byte, error) { return []byte(strings.ReplaceAll(string(b), `"secret",`, "")), nil }
	w, seen := runFilter(t, 200, http.Header{"Etag": {"abc"}}, `{"data":["a","secret","b"]}`, filterPlan(filter, 1<<20))
	want := `{"data":["a","b"]}`
	if w.Code != 200 || w.Body.String() != want {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Length") != strconv.Itoa(len(want)) || w.Header().Get("Etag") != "" {
		t.Fatalf("headers not corrected: %v", w.Header())
	}
	if seen.Get("Accept-Encoding") != "identity" {
		t.Fatalf("upstream Accept-Encoding = %q", seen.Get("Accept-Encoding"))
	}
}

func TestResponseFilterFailsClosed(t *testing.T) {
	boom := func([]byte) ([]byte, error) { return nil, errors.New("boom") }
	ident := func(b []byte) ([]byte, error) { return b, nil }
	tests := []struct {
		name   string
		status int
		hdr    http.Header
		body   string
		plan   enforce.Plan
		want   int
		typ    string
	}{
		{"filter error", 200, nil, "SECRET-BODY", filterPlan(boom, 1<<20), 502, "upstream"},
		{"too large", 200, nil, strings.Repeat("x", 100), filterPlan(ident, 50), 502, "merge_too_large"},
		{"compressed upstream", 200, http.Header{"Content-Encoding": {"gzip"}}, "SECRET-BODY", filterPlan(ident, 1<<20), 502, "upstream"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := runFilter(t, tc.status, tc.hdr, tc.body, tc.plan)
			e := errBodyOf(t, w.Body.Bytes())
			if w.Code != tc.want || e.ErrorType != tc.typ || strings.Contains(w.Body.String(), "SECRET-BODY") {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
		})
	}
	// Non-2xx upstream responses are streamed unchanged and unfiltered.
	w, _ := runFilter(t, 400, nil, `{"status":"error"}`, filterPlan(boom, 10))
	if w.Code != 400 || w.Body.String() != `{"status":"error"}` {
		t.Fatalf("error passthrough: %d %q", w.Code, w.Body.String())
	}
}

func errBodyOf(t *testing.T, b []byte) ErrorBody {
	t.Helper()
	var e ErrorBody
	if err := jsonUnmarshal(b, &e); err != nil {
		t.Fatalf("not an error document: %q", b)
	}
	return e
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
