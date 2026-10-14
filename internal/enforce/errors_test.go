package enforce

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		err    error
		status int
		typ    string
	}{
		{ErrParse, 400, "bad_data"},
		{ErrDeniedLabel, 403, "forbidden"},
		{ErrUnsupported, 400, "bad_data"},
		{ErrTooComplex, 403, "forbidden"},
		{Errorf(ErrDeniedLabel, "label %q", "customer_id"), 403, "forbidden"},
		{fmt.Errorf("wrapped: %w", Errorf(ErrParse, "x")), 400, "bad_data"},
		{errors.New("other"), http.StatusInternalServerError, "internal"},
	}
	for _, tt := range tests {
		if got := HTTPStatus(tt.err); got != tt.status {
			t.Errorf("%v: status %d, want %d", tt.err, got, tt.status)
		}
		if got := ErrorType(tt.err); got != tt.typ {
			t.Errorf("%v: type %q, want %q", tt.err, got, tt.typ)
		}
	}
	e := Errorf(ErrDeniedLabel, "label %q", "customer_id")
	if !errors.Is(e, ErrDeniedLabel) || errors.Is(e, ErrParse) {
		t.Error("errors.Is mismatch")
	}
	if Detail(e) != `label "customer_id"` {
		t.Errorf("detail %q", Detail(e))
	}
	if Detail(ErrParse) != "parse error" {
		t.Error("sentinel detail")
	}
}
