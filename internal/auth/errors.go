package auth

import "errors"

// Reason classifies why authentication failed. Its values are the label
// values of querdian_auth_failures_total{reason}.
type Reason string

// Failure reasons.
const (
	// ReasonMissingToken means no Authorization header was sent.
	ReasonMissingToken Reason = "missing_token"
	// ReasonInvalidToken means the token is malformed, badly signed or otherwise unusable.
	ReasonInvalidToken Reason = "invalid_token"
	// ReasonExpired means the token's exp claim is in the past.
	ReasonExpired Reason = "expired"
	// ReasonAudience means none of the configured audiences is in the token's aud claim.
	ReasonAudience Reason = "audience"
	// ReasonIssuer means the token was issued by a different issuer.
	ReasonIssuer Reason = "issuer"
	// ReasonMissingUsername means the configured username claim is absent or empty.
	ReasonMissingUsername Reason = "missing_username"
)

// Reasons lists every Reason, e.g. to pre-initialise metric label values.
var Reasons = []Reason{
	ReasonMissingToken, ReasonInvalidToken, ReasonExpired,
	ReasonAudience, ReasonIssuer, ReasonMissingUsername,
}

// Error is a typed authentication failure. Its message never contains token
// contents; the underlying cause (which might) is only reachable via Unwrap.
type Error struct {
	// Reason classifies the failure.
	Reason Reason
	cause  error
}

// Error implements error.
func (e *Error) Error() string { return "auth: " + string(e.Reason) }

// Unwrap returns the underlying cause, if any. Do not log or expose it to clients.
func (e *Error) Unwrap() error { return e.cause }

func newError(r Reason, cause error) *Error { return &Error{Reason: r, cause: cause} }

// ReasonOf returns the Reason of err, or ReasonInvalidToken if err is not an *Error.
func ReasonOf(err error) Reason {
	var ae *Error
	if errors.As(err, &ae) {
		return ae.Reason
	}
	return ReasonInvalidToken
}

// clientMessage is the fixed, token-free text sent to clients.
func clientMessage(r Reason) string {
	switch r {
	case ReasonMissingToken:
		return "missing bearer token"
	case ReasonExpired:
		return "token expired"
	case ReasonAudience:
		return "token audience not accepted"
	case ReasonIssuer:
		return "token issuer not accepted"
	case ReasonMissingUsername:
		return "token carries no username"
	case ReasonInvalidToken:
		return "invalid token"
	default:
		return "invalid token"
	}
}
