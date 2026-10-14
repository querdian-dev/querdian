package observability

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Span attribute keys for privacy-gated data (PLAN §11.2).
const (
	AttrEndUserID      = attribute.Key("enduser.id")
	AttrQueryOriginal  = attribute.Key("querdian.query.original")
	AttrQueryRewritten = attribute.Key("querdian.query.rewritten")
)

// Privacy holds the tracing privacy flags. The zero value records nothing.
type Privacy struct {
	// RecordUser enables the enduser.id span attribute.
	RecordUser bool
	// RecordQueries enables the original/rewritten query span attributes.
	RecordQueries bool
}

// SetUser sets enduser.id on span only if RecordUser is enabled.
func (p Privacy) SetUser(span trace.Span, user string) {
	if p.RecordUser && user != "" {
		span.SetAttributes(AttrEndUserID.String(user))
	}
}

// SetQueries sets the original and rewritten query attributes on span only if
// RecordQueries is enabled.
func (p Privacy) SetQueries(span trace.Span, original, rewritten string) {
	if p.RecordQueries {
		span.SetAttributes(AttrQueryOriginal.String(original), AttrQueryRewritten.String(rewritten))
	}
}

// LogUser reports whether user identity may be logged (same flag as tracing).
func (p Privacy) LogUser() bool { return p.RecordUser }

// LogQueries reports whether queries may be logged (same flag as tracing).
func (p Privacy) LogQueries() bool { return p.RecordQueries }
