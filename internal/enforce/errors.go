package enforce

import (
	"errors"
	"fmt"
	"net/http"
)

// Error is a typed enforcement error. The package-level sentinels are the only values of this type;
// use errors.Is to test for them. Detail-carrying errors are created with Errorf.
type Error struct {
	// Status is the HTTP status code (PLAN §9).
	Status int
	// Type is the Prometheus-style errorType in the JSON error body.
	Type string
	// Msg is the generic message of the sentinel.
	Msg string
}

// Error implements error.
func (e *Error) Error() string { return e.Msg }

// Sentinel errors (PLAN §7.1, §9).
var (
	// ErrParse: the query could not be parsed (400 bad_data).
	ErrParse = &Error{Status: http.StatusBadRequest, Type: "bad_data", Msg: "parse error"}
	// ErrDeniedLabel: the query references a label denied by policy (403 forbidden).
	ErrDeniedLabel = &Error{Status: http.StatusForbidden, Type: "forbidden", Msg: "label denied by policy"}
	// ErrUnsupported: valid but unsupported syntax or parameter (400 bad_data, with reason).
	ErrUnsupported = &Error{Status: http.StatusBadRequest, Type: "bad_data", Msg: "unsupported"}
	// ErrTooComplex: the query or policy exceeds complexity limits (403 forbidden).
	ErrTooComplex = &Error{Status: http.StatusForbidden, Type: "forbidden", Msg: "too complex"}
)

type detailed struct {
	base   *Error
	detail string
}

func (d *detailed) Error() string { return d.base.Msg + ": " + d.detail }
func (d *detailed) Unwrap() error { return d.base }

// Errorf returns an error that matches base via errors.Is and carries a formatted detail.
func Errorf(base *Error, format string, args ...any) error {
	return &detailed{base: base, detail: fmt.Sprintf(format, args...)}
}

// Detail returns the error text without the sentinel prefix, suitable for the JSON "error" field.
// For errors without a detail it returns the sentinel message.
func Detail(err error) string {
	var d *detailed
	if errors.As(err, &d) {
		return d.detail
	}
	return err.Error()
}

// HTTPStatus maps err to its HTTP status. Errors that are not enforcement errors map to 500.
func HTTPStatus(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return http.StatusInternalServerError
}

// ErrorType maps err to its JSON errorType. Errors that are not enforcement errors map to "internal".
func ErrorType(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Type
	}
	return "internal"
}
