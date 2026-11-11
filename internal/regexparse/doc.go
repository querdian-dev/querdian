// Package regexparse is a copy of the parser of the Go package regexp/syntax with a work budget. See SOURCE
// for the Go version and the changes, and LICENSE for the license of the Go code.
//
// Querdian parses each regular expression of a policy or a client query with ParseBudget before any other
// code compiles it. The budget stops the parse as soon as its work (for example the case folding of wide
// ranges, rune by rune) is over the limit, so that the measure of a regular expression stays cheap. The
// tree is the same as the tree of regexp/syntax.Parse for the same flags (see the differential tests).
package regexparse
