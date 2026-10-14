package logql

import (
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
)

// Loki 3.5.0 returns nothing for a positive __tenant_id__ matcher inside a metric query (found by the T3.8
// E2E suite); the binding must be expressed as a negative matcher over the other tenants. Log queries keep
// the positive form, which Loki evaluates correctly.
func TestMetricTenantBindingIsNegated(t *testing.T) {
	eff := effOf(
		term(m("ns", v1.MatchEqual, "a"), m("__tenant_id__", v1.MatchEqual, "t1")),
		term(m("ns", v1.MatchEqual, "b"), m("__tenant_id__", v1.MatchEqual, "t2.x")),
	)
	eff.Tenants = []string{"t1", "t2.x"}
	p, err := plan(t, `sum(rate({app="x"}[5m]))`, eff)
	if err != nil {
		t.Fatal(err)
	}
	q := p.Requests[0].Query.Get("query")
	if strings.Contains(q, `__tenant_id__=`) || !strings.Contains(q, `__tenant_id__!~"t2\\.x"`) || !strings.Contains(q, `__tenant_id__!~"t1"`) {
		t.Errorf("tenant binding not negated: %s", q)
	}
	lp, err := plan(t, `{app="x"}`, eff)
	if err != nil {
		t.Fatal(err)
	}
	if got := lp.Requests[0].Query.Get("query"); !strings.Contains(got, `__tenant_id__="t1"`) {
		t.Errorf("log query must keep the positive binding: %s", got)
	}
}
