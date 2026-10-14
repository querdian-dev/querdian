package logql

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Kind is the type of a lexical token.
type Kind int

// Token kinds.
const (
	// EOF is a sentinel kind for consumers (the parser); [Lex] does not
	// append it to the token slice.
	EOF Kind = iota
	// String is a double-quoted string; Value holds the decoded content.
	String
	// RawString is a backtick string; Value holds the content without quotes.
	RawString
	// Number is a decimal number such as 1, 1.5, .5 or 1e3.
	Number
	// Duration is a duration literal such as 5m or 1h30m.
	Duration
	// Bytes is a bytes literal such as 20KB or 1.5MiB.
	Bytes
	// Ident is an identifier that is not a keyword.
	Ident
	// Keyword is a reserved word; Value holds its lower-case spelling.
	Keyword
	// Op is an operator such as |=, !~, ==, +, or |.
	Op
	// LBrace is '{'.
	LBrace
	// RBrace is '}'.
	RBrace
	// LParen is '('.
	LParen
	// RParen is ')'.
	RParen
	// LBracket is '['.
	LBracket
	// RBracket is ']'.
	RBracket
	// Comma is ','.
	Comma
)

var kindNames = [...]string{
	EOF: "EOF", String: "String", RawString: "RawString", Number: "Number",
	Duration: "Duration", Bytes: "Bytes", Ident: "Ident", Keyword: "Keyword",
	Op: "Op", LBrace: "LBrace", RBrace: "RBrace", LParen: "LParen",
	RParen: "RParen", LBracket: "LBracket", RBracket: "RBracket", Comma: "Comma",
}

// String returns the name of the kind.
func (k Kind) String() string {
	if k >= 0 && int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Token is one lexical token. Text is always exactly src[Pos:End].
type Token struct {
	Kind Kind
	// Text is the verbatim source text of the token.
	Text string
	// Value is the decoded content for String and RawString tokens and the
	// lower-cased spelling for Keyword tokens; otherwise it equals Text.
	Value string
	// Pos is the byte offset of the first byte of the token.
	Pos int
	// End is the byte offset one past the last byte of the token.
	End int
}

// Is reports whether t is a Keyword or Ident whose spelling equals name,
// ignoring case. name should be given in lower case.
func (t Token) Is(name string) bool {
	return (t.Kind == Keyword || t.Kind == Ident) && strings.EqualFold(t.Text, name)
}

// Error is a lexical error at a byte position of the input.
type Error struct {
	// Pos is the byte offset of the offending input.
	Pos int
	// Msg describes the problem.
	Msg string
}

// Error implements the error interface.
func (e *Error) Error() string {
	return fmt.Sprintf("logql: lex error at byte %d: %s", e.Pos, e.Msg)
}

// keywords are the reserved words (docs/research/logql.md section 2),
// matched case-insensitively.
var keywords = map[string]struct{}{
	"json": {}, "logfmt": {}, "pattern": {}, "regexp": {}, "unpack": {},
	"line_format": {}, "label_format": {}, "drop": {}, "keep": {},
	"decolorize": {}, "unwrap": {}, "by": {}, "without": {}, "on": {},
	"ignoring": {}, "group_left": {}, "group_right": {}, "bool": {},
	"and": {}, "or": {}, "unless": {}, "offset": {}, "ip": {},
}

var durationUnits = []string{"ns", "us", "µs", "μs", "ms", "s", "m", "h", "d", "w", "y"}

var bytesUnits = map[string]struct{}{
	"B": {}, "k": {}, "kB": {}, "KB": {}, "Ki": {}, "KiB": {},
	"M": {}, "MB": {}, "Mi": {}, "MiB": {},
	"G": {}, "GB": {}, "Gi": {}, "GiB": {},
	"T": {}, "TB": {}, "Ti": {}, "TiB": {},
}

var singleKinds = map[byte]Kind{
	'{': LBrace, '}': RBrace, '(': LParen, ')': RParen,
	'[': LBracket, ']': RBracket, ',': Comma,
}

var simpleEscapes = map[byte]byte{
	'"': '"', '\\': '\\', 'a': '\a', 'b': '\b', 'f': '\f',
	'n': '\n', 'r': '\r', 't': '\t', 'v': '\v',
}

// Lex tokenises src. Whitespace and '#' comments are skipped. On failure it
// returns an *[Error] carrying the byte position of the problem.
func Lex(src string) ([]Token, error) {
	for i := 0; i < len(src); {
		r, n := utf8.DecodeRuneInString(src[i:])
		if r == utf8.RuneError && n == 1 {
			return nil, &Error{Pos: i, Msg: "invalid UTF-8"}
		}
		i += n
	}
	l := &lexer{src: src}
	for {
		l.skipSpace()
		if l.pos >= len(src) {
			return l.toks, nil
		}
		if err := l.next(); err != nil {
			return nil, err
		}
	}
}

type lexer struct {
	src  string
	pos  int
	toks []Token
}

// emit appends a token ending at l.pos. An empty value means "same as text".
func (l *lexer) emit(k Kind, start int, value string) {
	text := l.src[start:l.pos]
	if value == "" && k != String && k != RawString {
		value = text
	}
	l.toks = append(l.toks, Token{Kind: k, Text: text, Value: value, Pos: start, End: l.pos})
}

func (l *lexer) errf(pos int, format string, args ...any) error {
	return &Error{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

func (l *lexer) skipSpace() {
	for l.pos < len(l.src) {
		switch l.src[l.pos] {
		case ' ', '\t', '\r', '\n':
			l.pos++
		case '#':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		default:
			return
		}
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || isDigit(c) }

// peek returns the byte at pos+off or 0 past the end.
func (l *lexer) peek(off int) byte {
	if l.pos+off < len(l.src) {
		return l.src[l.pos+off]
	}
	return 0
}

func (l *lexer) next() error {
	start := l.pos
	c := l.src[l.pos]
	switch {
	case c == '"':
		return l.lexString()
	case c == '`':
		return l.lexRaw()
	case isDigit(c) || (c == '.' && isDigit(l.peek(1))):
		return l.lexNumber()
	case isIdentStart(c):
		for isIdentPart(l.peek(0)) {
			l.pos++
		}
		lower := strings.ToLower(l.src[start:l.pos])
		if _, ok := keywords[lower]; ok {
			l.emit(Keyword, start, lower)
		} else {
			l.emit(Ident, start, "")
		}
		return nil
	}
	if k, ok := singleKinds[c]; ok {
		l.pos++
		l.emit(k, start, "")
		return nil
	}
	if n := l.opLen(); n > 0 {
		l.pos += n
		l.emit(Op, start, "")
		return nil
	}
	switch c {
	case '\'':
		return l.errf(start, "single-quoted strings are not allowed")
	case ':':
		return l.errf(start, "unexpected ':'")
	}
	r, _ := utf8.DecodeRuneInString(l.src[start:])
	return l.errf(start, "unexpected character %q", r)
}

// opLen returns the length of the operator at the current position, or 0.
func (l *lexer) opLen() int {
	if l.pos+1 < len(l.src) {
		switch l.src[l.pos : l.pos+2] {
		case "|=", "|~", "|>", "!=", "!~", "!>", "=~", "==", ">=", "<=":
			return 2
		}
	}
	switch l.src[l.pos] {
	case '|', '=', '>', '<', '+', '-', '*', '/', '%', '^':
		return 1
	}
	return 0
}

func (l *lexer) lexRaw() error {
	start := l.pos
	l.pos++
	end := strings.IndexByte(l.src[l.pos:], '`')
	if end < 0 {
		return l.errf(start, "unterminated raw string")
	}
	content := l.src[l.pos : l.pos+end]
	l.pos += end + 1
	l.emit(RawString, start, content)
	return nil
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func (l *lexer) lexString() error {
	start := l.pos
	l.pos++
	var sb strings.Builder
	for {
		if l.pos >= len(l.src) {
			return l.errf(start, "unterminated string")
		}
		c := l.src[l.pos]
		switch {
		case c == '"':
			l.pos++
			l.emit(String, start, sb.String())
			return nil
		case c == '\\':
			if err := l.lexEscape(&sb); err != nil {
				return err
			}
		case c < 0x20 && c != '\t':
			return l.errf(l.pos, "control character in string")
		default:
			sb.WriteByte(c)
			l.pos++
		}
	}
}

// lexEscape decodes the escape sequence at l.pos (which points at the
// backslash) into sb.
func (l *lexer) lexEscape(sb *strings.Builder) error {
	esc := l.pos
	l.pos++
	if l.pos >= len(l.src) {
		return l.errf(esc, "unterminated string")
	}
	c := l.src[l.pos]
	if v, ok := simpleEscapes[c]; ok {
		sb.WriteByte(v)
		l.pos++
		return nil
	}
	switch {
	case c == 'x':
		hi, ok1 := hexVal(l.peek(1))
		lo, ok2 := hexVal(l.peek(2))
		if !ok1 || !ok2 {
			return l.errf(esc, "invalid \\x escape")
		}
		sb.WriteByte(hi<<4 | lo)
		l.pos += 3
		return nil
	case c >= '0' && c <= '7':
		v := 0
		for i := range 3 {
			d := l.peek(i)
			if d < '0' || d > '7' {
				return l.errf(esc, "invalid octal escape")
			}
			v = v<<3 | int(d-'0')
		}
		if v > 255 {
			return l.errf(esc, "octal escape out of range")
		}
		sb.WriteByte(byte(v))
		l.pos += 3
		return nil
	}
	return l.errf(esc, "invalid escape sequence")
}

// scanDecimal consumes digits with an optional fraction.
func (l *lexer) scanDecimal() {
	for isDigit(l.peek(0)) {
		l.pos++
	}
	if l.peek(0) == '.' && isDigit(l.peek(1)) {
		l.pos++
		for isDigit(l.peek(0)) {
			l.pos++
		}
	}
}

// lexNumber lexes a Number, Duration or Bytes token.
func (l *lexer) lexNumber() error {
	start := l.pos
	l.scanDecimal()
	// Exponent: e/E, optional sign, at least one digit.
	if c := l.peek(0); c == 'e' || c == 'E' {
		i := 1
		if s := l.peek(i); s == '+' || s == '-' {
			i++
		}
		if isDigit(l.peek(i)) {
			l.pos += i
			for isDigit(l.peek(0)) {
				l.pos++
			}
			return l.finishNumber(start, Number)
		}
	}
	// Duration: number unit (number unit)*.
	if l.durationUnitAt(l.pos) > 0 {
		for {
			l.pos += l.durationUnitAt(l.pos)
			if c := l.peek(0); !isDigit(c) && (c != '.' || !isDigit(l.peek(1))) {
				break
			}
			l.scanDecimal()
			if l.durationUnitAt(l.pos) == 0 {
				return l.errf(start, "invalid duration literal")
			}
		}
		return l.finishNumber(start, Duration)
	}
	// Bytes: number unit.
	if isIdentStart(l.peek(0)) {
		us := l.pos
		for isIdentPart(l.peek(0)) {
			l.pos++
		}
		if _, ok := bytesUnits[l.src[us:l.pos]]; !ok {
			return l.errf(start, "invalid number, duration or bytes literal %q", l.src[start:l.pos])
		}
		return l.finishNumber(start, Bytes)
	}
	return l.finishNumber(start, Number)
}

// durationUnitAt returns the byte length of a duration unit at pos, or 0.
func (l *lexer) durationUnitAt(pos int) int {
	rest := l.src[pos:]
	best := 0
	for _, u := range durationUnits {
		if len(u) > best && strings.HasPrefix(rest, u) {
			best = len(u)
		}
	}
	return best
}

// finishNumber rejects a literal glued to further identifier characters, a
// '.' or non-ASCII bytes, and otherwise emits the token.
func (l *lexer) finishNumber(start int, k Kind) error {
	if c := l.peek(0); isIdentPart(c) || c == '.' || c >= utf8.RuneSelf {
		return l.errf(start, "invalid literal followed by %q", l.src[l.pos:min(l.pos+1, len(l.src))])
	}
	l.emit(k, start, "")
	return nil
}
