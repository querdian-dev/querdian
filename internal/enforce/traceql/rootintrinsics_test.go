package traceql

import (
	"errors"
	"testing"

	"github.com/querdian-dev/querdian/internal/enforce"
	"github.com/querdian-dev/querdian/internal/policy"
)

func TestTraceLevelIntrinsicsRejectedUnlessRootCovered(t *testing.T) {
	for _, qq := range []string{
		`{ rootServiceName = "x" }`, `{ trace:rootService = "x" }`, `{ rootName =~ "a.*" }`, `{ trace:rootName != "x" }`,
		`{ traceDuration > 1s }`, `{ trace:duration < 1s }`, `{ } | select(rootName)`, `{ } | by(rootServiceName)`,
		`{ } | rate() by (trace:rootService)`, `{ span.a = "x" } && { rootName = "y" }`,
	} {
		if _, err := plan(t, "/api/search", q("q", qq), one); !errors.Is(err, enforce.ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", qq, err)
		}
		// Root covered (term without matchers, or unrestricted): allowed.
		for _, e := range []policy.Effective{eff(nil, term()), {Unrestricted: true}} {
			if _, err := plan(t, "/api/search", q("q", qq), e); err != nil {
				t.Errorf("%s: rejected although the root is covered: %v", qq, err)
			}
		}
	}
	for _, name := range []string{"rootServiceName", "rootName", "trace:rootService", "trace:rootName", "traceDuration", "trace:duration"} {
		if _, err := plan(t, "/api/v2/search/tag/"+name+"/values", nil, one); !errors.Is(err, enforce.ErrUnsupported) {
			t.Errorf("tag values %s: err = %v", name, err)
		}
	}
	// Other intrinsics stay usable.
	for _, qq := range []string{`{ trace:id = "1" }`, `{ name = "x" }`, `{ span:duration > 1s }`} {
		if _, err := plan(t, "/api/search", q("q", qq), one); err != nil {
			t.Errorf("%s: %v", qq, err)
		}
	}
}
