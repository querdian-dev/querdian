//go:build e2e

package tempo

import (
	"net/http"
	"testing"
)

// testParserBoundaries pins where strings and quoted attribute names end. The injector rewrites the query by
// byte positions, so if Tempo and pkg/traceql ended a quoted token at different quotes, a spanset filter
// could reach Tempo without the policy. Black-box (Tempo 2.9.0): \" and \\ are escapes in both, so the
// first two cases are unterminated and the others are complete. Each case checks Tempo's own verdict
// (a changed Tempo fails here first) and that the proxy reaches the same one, with the quoted tag name of
// a tag-values lookup rejected by the proxy outright.
func testParserBoundaries(t *testing.T, f *fixture) {
	sc := scenarioByName("ns-a-deny")
	d, p := f.direct(sc), f.as(sc)
	for _, c := range []struct {
		q  string
		ok bool // Tempo accepts the query
	}{
		{`{ span."a\" = "x" }`, false},
		{`{ span.a = "x\" }`, false},
		{`{ span."a\"b" = "x" }`, true},
		{`{ span.a = "x\"y" }`, true},
		{`{ span."a\\" = "x" }`, true},
		{`{ span.a = "x\\" }`, true},
	} {
		t.Run(c.q, func(t *testing.T) {
			if got := d.search(c.q).Status == 200; got != c.ok {
				t.Errorf("Tempo changed: accepts = %v, want %v", got, c.ok)
			}
			if r := p.search(c.q); (r.Status == 200) != c.ok {
				t.Errorf("proxy disagrees with Tempo: %d %.300s", r.Status, r.Body)
			}
		})
	}
	// Tempo does not decode quoted names on tag values (they find nothing); the proxy rejects them.
	for _, name := range []string{`span."customer_id"`, `."customer_id"`} {
		req, err := http.NewRequest(http.MethodGet, p.c.Base, nil) //nolint:noctx // test-only
		if err != nil {
			t.Fatal(err)
		}
		req.URL.Opaque = p.prefix + "/api/v2/search/tag/" + name + "/values" // the quotes go out unescaped
		req.URL.RawQuery = p.window().Encode()
		if r := p.c.Do(req); r.Status != 400 {
			t.Errorf("tag values %s: %d %.300s", name, r.Status, r.Body)
		}
	}
}
