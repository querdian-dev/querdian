package traceql

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Kind is the type of a lexical token.
type Kind int

// Token kinds.
const (
	// KindIllegal is the zero value and never produced by Lex.
	KindIllegal Kind = iota
	// KindString is a double-quoted or backtick string literal; Value holds the decoded text.
	KindString
	// KindNumber is an integer or decimal number such as 200, 0.5 or .99.
	KindNumber
	// KindDuration is a number with a unit, such as 200ms or 1.5s.
	KindDuration
	// KindIdent is a bare word: true, false, nil, error, count, by, select, ...
	KindIdent
	// KindAttribute is a scoped or unscoped attribute reference (span.x, resource."a b", .x).
	// Scope holds the scope ("" for unscoped) and Value the decoded attribute name.
	KindAttribute
	// KindIntrinsic is an intrinsic in colon form (span:name, Scope "span", Value "name") or in
	// legacy unscoped form (duration, Scope "", Value "duration").
	KindIntrinsic

	// KindLBrace is '{'.
	KindLBrace
	// KindRBrace is '}'.
	KindRBrace
	// KindLParen is '('.
	KindLParen
	// KindRParen is ')'.
	KindRParen
	// KindPipe is '|'.
	KindPipe
	// KindComma is ','.
	KindComma

	// KindEq is '='.
	KindEq
	// KindNeq is '!='.
	KindNeq
	// KindGt is '>' (comparison, or structural child).
	KindGt
	// KindGte is '>='.
	KindGte
	// KindLt is '<' (comparison, or structural parent).
	KindLt
	// KindLte is '<='.
	KindLte
	// KindRegex is '=~'.
	KindRegex
	// KindNotRegex is '!~' (not-regex, or negated sibling; the parser decides).
	KindNotRegex
	// KindAnd is '&&'.
	KindAnd
	// KindOr is '||'.
	KindOr
	// KindNot is '!'.
	KindNot
	// KindAdd is '+'.
	KindAdd
	// KindSub is '-'.
	KindSub
	// KindMul is '*'.
	KindMul
	// KindDiv is '/'.
	KindDiv
	// KindMod is '%'.
	KindMod
	// KindPow is '^'.
	KindPow

	// KindDescendant is '>>'.
	KindDescendant
	// KindAncestor is '<<'.
	KindAncestor
	// KindSibling is '~'.
	KindSibling
	// KindUnionChild is '&>'.
	KindUnionChild
	// KindUnionParent is '&<'.
	KindUnionParent
	// KindUnionDescendant is '&>>'.
	KindUnionDescendant
	// KindUnionAncestor is '&<<'.
	KindUnionAncestor
	// KindUnionSibling is '&~'.
	KindUnionSibling
	// KindNotChild is '!>'.
	KindNotChild
	// KindNotParent is '!<'.
	KindNotParent
	// KindNotDescendant is '!>>'.
	KindNotDescendant
	// KindNotAncestor is '!<<'.
	KindNotAncestor
)

var kindNames = map[Kind]string{
	KindIllegal: "illegal", KindString: "string", KindNumber: "number", KindDuration: "duration",
	KindIdent: "ident", KindAttribute: "attribute", KindIntrinsic: "intrinsic",
	KindLBrace: "{", KindRBrace: "}", KindLParen: "(", KindRParen: ")", KindPipe: "|", KindComma: ",",
	KindEq: "=", KindNeq: "!=", KindGt: ">", KindGte: ">=", KindLt: "<", KindLte: "<=",
	KindRegex: "=~", KindNotRegex: "!~", KindAnd: "&&", KindOr: "||", KindNot: "!",
	KindAdd: "+", KindSub: "-", KindMul: "*", KindDiv: "/", KindMod: "%", KindPow: "^",
	KindDescendant: ">>", KindAncestor: "<<", KindSibling: "~",
	KindUnionChild: "&>", KindUnionParent: "&<", KindUnionDescendant: "&>>",
	KindUnionAncestor: "&<<", KindUnionSibling: "&~",
	KindNotChild: "!>", KindNotParent: "!<", KindNotDescendant: "!>>", KindNotAncestor: "!<<",
}

// String returns a human readable name; for operators and punctuation it is the symbol itself.
func (k Kind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// Token is one lexical element. Start and End are byte offsets into the input
// (input[Start:End] == Text), so later stages can rewrite the query by position.
type Token struct {
	// Kind is the token type.
	Kind Kind
	// Start is the byte offset of the first byte of the token.
	Start int
	// End is the byte offset just past the last byte of the token.
	End int
	// Text is the raw source text of the token.
	Text string
	// Scope is the scope of an attribute or intrinsic ("span", "resource", "trace", ...), or ""
	// when unscoped. Empty for all other kinds.
	Scope string
	// Value is the decoded string for KindString, the attribute name for KindAttribute (quotes
	// removed, escapes decoded) and the intrinsic name for KindIntrinsic. Empty otherwise.
	Value string
}

// Error is a lexical error with the byte offset of the offending input.
type Error struct {
	// Pos is the byte offset in the input where the problem was detected.
	Pos int
	// Msg describes the problem.
	Msg string
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("traceql: lex error at byte %d: %s", e.Pos, e.Msg)
}

var attrScopes = map[string]bool{
	"span": true, "resource": true, "event": true, "link": true, "instrumentation": true,
}

var intrinsicNames = map[string]map[string]bool{
	"span":            {"name": true, "status": true, "statusMessage": true, "kind": true, "duration": true, "childCount": true, "id": true, "parentID": true},
	"trace":           {"duration": true, "rootName": true, "rootService": true, "id": true},
	"event":           {"name": true, "timeSinceStart": true},
	"link":            {"traceID": true, "spanID": true},
	"instrumentation": {"name": true, "version": true},
}

var legacyIntrinsics = map[string]bool{
	"name": true, "status": true, "kind": true, "duration": true, "statusMessage": true,
	"childCount": true, "traceDuration": true, "rootName": true, "rootServiceName": true,
}

var durationUnits = []string{"ns", "us", "ms", "s", "m", "h", "d"}

// operators are listed longest first for maximal munch.
var operators = []struct {
	text string
	kind Kind
}{
	{"!>>", KindNotDescendant}, {"!<<", KindNotAncestor}, {"&>>", KindUnionDescendant}, {"&<<", KindUnionAncestor},
	{">>", KindDescendant}, {"<<", KindAncestor}, {">=", KindGte}, {"<=", KindLte}, {"!=", KindNeq},
	{"=~", KindRegex}, {"!~", KindNotRegex}, {"&&", KindAnd}, {"||", KindOr},
	{"!>", KindNotChild}, {"!<", KindNotParent}, {"&>", KindUnionChild}, {"&<", KindUnionParent}, {"&~", KindUnionSibling},
	{"=", KindEq}, {">", KindGt}, {"<", KindLt}, {"~", KindSibling}, {"!", KindNot},
	{"+", KindAdd}, {"-", KindSub}, {"*", KindMul}, {"/", KindDiv}, {"%", KindMod}, {"^", KindPow},
	{"|", KindPipe}, {"(", KindLParen}, {")", KindRParen}, {"{", KindLBrace}, {"}", KindRBrace}, {",", KindComma},
}

type lexer struct {
	in  string
	pos int
}

// Lex splits a TraceQL query into tokens. Whitespace separates tokens and is dropped; comments
// are not supported. On invalid input it returns an *Error carrying the byte offset.
func Lex(input string) ([]Token, error) {
	if !utf8.ValidString(input) {
		for i := 0; i < len(input); {
			r, n := utf8.DecodeRuneInString(input[i:])
			if r == utf8.RuneError && n == 1 {
				return nil, &Error{Pos: i, Msg: "invalid UTF-8"}
			}
			i += n
		}
	}
	l := &lexer{in: input}
	var toks []Token
	for {
		for l.pos < len(l.in) && isSpace(l.in[l.pos]) {
			l.pos++
		}
		if l.pos >= len(l.in) {
			return toks, nil
		}
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		toks = append(toks, t)
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

// nameRuneAt reports whether the rune at i may appear in an unquoted attribute name (excluding
// '.'), and its width.
func (l *lexer) nameRuneAt(i int) (bool, int) {
	r, n := utf8.DecodeRuneInString(l.in[i:])
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r), n
}

func (l *lexer) errf(pos int, format string, args ...any) error {
	return &Error{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

func (l *lexer) tok(kind Kind, start int) Token {
	return Token{Kind: kind, Start: start, End: l.pos, Text: l.in[start:l.pos]}
}

func (l *lexer) next() (Token, error) {
	start := l.pos
	c := l.in[start]
	switch {
	case c == '"' || c == '`':
		return l.lexString(start)
	case isDigit(c):
		return l.lexNumber(start)
	case c == '.':
		if start+1 < len(l.in) && isDigit(l.in[start+1]) {
			if t, ok := l.tryLeadingDotNumber(start); ok {
				return t, nil
			}
		}
		return l.lexAttribute(start, "", start+1)
	case isIdentStart(c):
		return l.lexWord(start)
	}
	rest := l.in[start:]
	if strings.HasPrefix(rest, "//") || strings.HasPrefix(rest, "/*") {
		return Token{}, l.errf(start, "comments are not supported")
	}
	for _, op := range operators {
		if strings.HasPrefix(rest, op.text) {
			l.pos += len(op.text)
			return l.tok(op.kind, start), nil
		}
	}
	if c == '&' {
		return Token{}, l.errf(start, "unexpected '&' (expected && or a union structural operator)")
	}
	if c == '#' {
		return Token{}, l.errf(start, "comments are not supported")
	}
	return Token{}, l.errf(start, "unexpected character %q", rune(c))
}

// lexString lexes a "..." string (only \" and \\ escapes) or a raw `...` string.
func (l *lexer) lexString(start int) (Token, error) {
	text, end, err := l.scanQuoted(start)
	if err != nil {
		return Token{}, err
	}
	l.pos = end
	t := l.tok(KindString, start)
	t.Value = text
	return t, nil
}

// scanQuoted decodes the quoted string starting at start and returns its value and end offset.
func (l *lexer) scanQuoted(start int) (string, int, error) {
	quote := l.in[start]
	var sb strings.Builder
	i := start + 1
	for i < len(l.in) {
		c := l.in[i]
		switch {
		case c == quote:
			return sb.String(), i + 1, nil
		case c < 0x20 || c == 0x7f:
			return "", 0, l.errf(i, "control character in string")
		case c == '\\' && quote == '"':
			if i+1 >= len(l.in) {
				return "", 0, l.errf(i, "unterminated escape sequence")
			}
			switch l.in[i+1] {
			case '"', '\\':
				sb.WriteByte(l.in[i+1])
			default:
				return "", 0, l.errf(i, "invalid escape sequence (only \\\" and \\\\ are allowed)")
			}
			i += 2
		default:
			sb.WriteByte(c)
			i++
		}
	}
	return "", 0, l.errf(start, "unterminated string")
}

// tryLeadingDotNumber lexes ".digits" as a number when it is not the beginning of a longer
// attribute name.
func (l *lexer) tryLeadingDotNumber(start int) (Token, bool) {
	i := start + 1
	for i < len(l.in) && isDigit(l.in[i]) {
		i++
	}
	if i < len(l.in) {
		if ok, _ := l.nameRuneAt(i); ok || l.in[i] == '.' || l.in[i] == '-' || l.in[i] == '/' {
			return Token{}, false
		}
	}
	l.pos = i
	return l.tok(KindNumber, start), true
}

// lexNumber lexes digits[.digits] and an optional duration unit.
func (l *lexer) lexNumber(start int) (Token, error) {
	i := start
	for i < len(l.in) && isDigit(l.in[i]) {
		i++
	}
	if i < len(l.in) && l.in[i] == '.' {
		j := i + 1
		for j < len(l.in) && isDigit(l.in[j]) {
			j++
		}
		if j == i+1 {
			return Token{}, l.errf(i, "digits expected after decimal point")
		}
		i = j
	}
	kind := KindNumber
	if i < len(l.in) {
		c := l.in[i]
		switch {
		case isIdentPart(c) || c >= utf8.RuneSelf:
			unit := ""
			for _, u := range durationUnits {
				if strings.HasPrefix(l.in[i:], u) && len(u) > len(unit) {
					unit = u // longest wins so "ms" beats "m"
				}
			}
			if unit == "" {
				return Token{}, l.errf(i, "invalid number suffix (units: %s)", strings.Join(durationUnits, ", "))
			}
			i += len(unit)
			kind = KindDuration
			if i < len(l.in) && (isIdentPart(l.in[i]) || l.in[i] == '.' || l.in[i] >= utf8.RuneSelf) {
				return Token{}, l.errf(i, "invalid duration (compound durations are not supported)")
			}
		case c == '.':
			return Token{}, l.errf(i, "unexpected '.' after number")
		}
	}
	l.pos = i
	return l.tok(kind, start), nil
}

// lexWord lexes an identifier, a scoped attribute (scope.name), a colon intrinsic or a legacy
// intrinsic.
func (l *lexer) lexWord(start int) (Token, error) {
	i := start
	for i < len(l.in) && isIdentPart(l.in[i]) {
		i++
	}
	word := l.in[start:i]
	if i < len(l.in) {
		switch l.in[i] {
		case '.':
			if word == "parent" {
				return Token{}, l.errf(start, "parent scope is not supported")
			}
			if !attrScopes[word] {
				return Token{}, l.errf(start, "unknown attribute scope %q", word)
			}
			return l.lexAttribute(start, word, i+1)
		case ':':
			names, ok := intrinsicNames[word]
			if !ok {
				return Token{}, l.errf(start, "unknown intrinsic scope %q", word)
			}
			j := i + 1
			for j < len(l.in) && isIdentPart(l.in[j]) {
				j++
			}
			name := l.in[i+1 : j]
			if !names[name] {
				return Token{}, l.errf(i+1, "unknown intrinsic %s:%s", word, name)
			}
			l.pos = j
			t := l.tok(KindIntrinsic, start)
			t.Scope, t.Value = word, name
			return t, nil
		}
	}
	l.pos = i
	if legacyIntrinsics[word] {
		t := l.tok(KindIntrinsic, start)
		t.Value = word
		return t, nil
	}
	return l.tok(KindIdent, start), nil
}

// lexAttribute lexes the attribute name beginning at nameStart (just after the '.').
func (l *lexer) lexAttribute(start int, scope string, nameStart int) (Token, error) {
	var name string
	end := nameStart
	if nameStart < len(l.in) && l.in[nameStart] == '"' {
		v, e, err := l.scanQuoted(nameStart)
		if err != nil {
			return Token{}, err
		}
		name, end = v, e
	} else {
		segStart := nameStart
		for end < len(l.in) {
			if l.in[end] == '.' {
				if end == segStart {
					return Token{}, l.errf(end, "empty segment in attribute name")
				}
				end++
				segStart = end
				continue
			}
			ok, n := l.nameRuneAt(end)
			if !ok {
				break
			}
			end += n
		}
		if end == segStart {
			if end == nameStart {
				return Token{}, l.errf(start, "attribute name expected")
			}
			return Token{}, l.errf(end-1, "attribute name must not end with '.'")
		}
		name = l.in[nameStart:end]
		if end < len(l.in) && (l.in[end] == '-' || l.in[end] == '/') {
			return Token{}, l.errf(end, "ambiguous %q after attribute name; quote the attribute name", rune(l.in[end]))
		}
	}
	l.pos = end
	t := l.tok(KindAttribute, start)
	t.Scope, t.Value = scope, name
	return t, nil
}
