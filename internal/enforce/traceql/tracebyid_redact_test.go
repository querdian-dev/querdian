package traceql

import (
	"bytes"
	"strings"
	"testing"

	v1 "github.com/querdian-dev/querdian/api/v1alpha1"
	"github.com/querdian-dev/querdian/internal/enforce/redact"
	"github.com/querdian-dev/querdian/internal/policy"
)

// Redacted attributes are rewritten, other denied attributes are still removed.
func TestRedactedAttributesRewritten(t *testing.T) {
	reds := map[string]policy.Redaction{"resource.team": {Strategy: v1.RedactPlaceholder}}
	e := eff([]string{"resource.team", "span.weird name"}, term(m("resource.service.name", v1.MatchEqual, "backend")))
	e.Redactions = reds
	newFilter := func() *TraceFilter {
		f, err := NewTraceFilter(e, TraceFilterOptions{Redactor: redact.New(reds)})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	out := mustFilterJSON(t, newFilter(), fx(t, "trace1_v1.json"))
	if strings.Contains(string(out), `"t1"`) || strings.Contains(string(out), "weird name") {
		t.Errorf("redacted or denied value leaked: %s", out)
	}
	if !strings.Contains(string(out), `{"key":"team","value":{"stringValue":"redacted-1"}}`) {
		t.Errorf("team not rewritten: %s", out)
	}
	pout, found, err := newFilter().FilterProto(fx(t, "trace1_v1.pb"))
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if bytes.Contains(pout, []byte("t1")) || bytes.Contains(pout, []byte("weird name")) || !bytes.Contains(pout, []byte("redacted-1")) {
		t.Errorf("protobuf not redacted")
	}
}
