//go:build e2e

package loki

import (
	"fmt"
	"strings"
	"testing"
)

// testParserBoundaries pins the token boundaries that decide which selectors the proxy sees: where a "#"
// comment ends, and that a "#" inside a string starts no comment. If Loki and pkg/logql disagreed on them,
// a selector could reach Loki without the policy. Today the rewritten query is rendered from the proxy's
// tokens, so comment text never reaches Loki at all; these cases keep that outcome (a user of ns-a never
// gets ns-b data) should the rendering change, and record what Loki itself executes, so a Loki that
// changes these rules fails here first.
func testParserBoundaries(t *testing.T, f *fixture) {
	d := f.direct("t1")
	p := f.asUser("u-team-a", "team-a") // ns-a only
	nsA := func(pipeline string) string {
		return fmt.Sprintf(`count_over_time({k8s_namespace_name="ns-a"} %s[1h])`, pipeline)
	}
	const orB = ` or count_over_time({k8s_namespace_name="ns-b"}[1h])`
	type tc struct {
		name      string
		q         string
		lokiRunsB bool // Loki evaluates the ns-b selector
	}
	cases := []tc{
		{"comment ends at LF", nsA("") + " # x\n" + orB, true},
		{"# in string", nsA(`|= "#" `) + orB, true},
		{"# in raw string", nsA("|= `#` ") + orB, true},
		{"# after escaped quote", nsA(`|= "\"#" `) + orB, true},
		{"comment after escaped backslash", nsA(`|= "\\" `) + "# x\n" + orB, true},
	}
	// Loki 3.5.0 ends a comment at LF only (black-box); none of these may end it.
	for _, term := range []string{"\r", "\v", "\f", "\x00", "\x1c", "\x1e", "\u0085", " ", " "} {
		cases = append(cases, tc{fmt.Sprintf("comment not ended by %q", term), nsA("") + " # x" + term + orB, false})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if dr := d.instant(c.q); strings.Contains(string(dr.Body), `"ns-b"`) != c.lokiRunsB {
				t.Errorf("Loki changed: evaluates the ns-b selector = %v, want %v (status %d)", !c.lokiRunsB, c.lokiRunsB, dr.Status)
			}
			if pr := p.instant(c.q); strings.Contains(string(pr.Body), `"ns-b"`) {
				t.Errorf("leak: ns-b data for a user of ns-a: %d %.300s", pr.Status, pr.Body)
			}
		})
	}
}
