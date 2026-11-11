// Package regexcost measures the cost of a regular expression before it is compiled.
//
// The time and memory of a compile (regexp.Compile, and the Prometheus matcher, which parses and prints the
// expression again) do not grow with the length of the expression. They grow with:
//
//   - the instructions of the compiled program: a counted repetition such as a{1000} gives 1000 instructions
//     for 7 bytes;
//   - the rune ranges of each character class: a Unicode class has hundreds, and the one-pass compiler
//     copies the ranges for each instruction, so the ranges count again for each repetition;
//   - the work of the parser, for example the case folding of a wide range rune by rune;
//   - for the Prometheus matcher only: the runes of each character class in the case-folding range of
//     Unicode, because its printer examines each of them.
//
// Weight parses the expression with a copy of the Go parser that counts its work and stops when the work is
// over the limit (internal/regexparse). The weight then comes from the parse tree and the counted work, not
// from the text. Each unit of weight costs at most about MaxBytesPerUnit bytes and MaxNanosPerUnit
// nanoseconds in the compilers that Querdian and its backends use (see the tests).
package regexcost

import (
	"errors"
	"regexp/syntax"

	"github.com/querdian-dev/querdian/internal/regexparse"
)

// Mode says which compilers get the expression.
type Mode int

const (
	// Compile is for an expression that only regexp.Compile gets (LogQL selectors in the proxy).
	Compile Mode = iota
	// CompileAndPrint is for an expression that the Prometheus matcher can also get, which prints the
	// parsed expression again (policy regexes and PromQL client regexes).
	CompileAndPrint
)

const (
	// WorkPerUnit is the parser work (steps counted by regexparse) for one unit of weight.
	WorkPerUnit = 32
	// ClassRunesPerUnit is the number of class runes in the fold window that the printer of the Prometheus
	// matcher examines for one unit of weight.
	ClassRunesPerUnit = 64
	// MaxBytesPerUnit is the bound of the bytes for each unit of weight that a compile in regexp and a
	// compile in the Prometheus matcher allocate together, over FixedBytes, as the tests measure it. It is
	// for documentation and tests.
	MaxBytesPerUnit = 2048
	// FixedBytes is the part of the cost of a compile that does not grow with the weight. The Prometheus
	// matcher expands small classes into sets of up to 256 strings.
	FixedBytes = 256 << 10
	// MaxNanosPerUnit is the bound of the time for each unit of weight of the same two compiles, without the
	// race detector, as the tests measure it. It is for documentation and tests.
	MaxNanosPerUnit = 2000

	// MaxRegexBytes is the largest client regex or policy regex value, in bytes, that Querdian parses. It
	// is a second layer, which does not depend on the work counter of the parser: at this length, the
	// slowest known parse path of regexp/syntax (a deep group around a wide concatenation, which costs
	// depth x width) takes some tens of milliseconds. 200 namespace names of 18 characters, joined with
	// |, fit.
	MaxRegexBytes = 8 << 10
	// MaxQueryRegexBytes is the largest sum of the bytes of the client regexes of one query (all its
	// selectors, all match[] values).
	MaxQueryRegexBytes = 32 << 10

	// The fold window of regexp/syntax: runes outside it have no case folding.
	minFold = 0x0041
	maxFold = 0x1e943
)

// Weight returns the cost of expr, parsed with flags, in units of weight: the instructions of the compiled
// program, plus the rune ranges of each character class (multiplied through repetitions), plus the work of
// the parser divided by WorkPerUnit, plus for CompileAndPrint the runes of each class in the fold window
// divided by ClassRunesPerUnit.
//
// The parse stops when its work is over limit units. Weight then returns limit+1 without an error. Thus
// the cost of Weight itself is bounded by limit. The caller compares the result with its limit. An
// expression that does not parse gives an error; the caller must then not compile it.
func Weight(expr string, flags syntax.Flags, mode Mode, limit int) (int, error) {
	limit = max(limit, 0)
	re, work, err := regexparse.ParseBudget(expr, regexparse.Flags(flags), int64(limit)*WorkPerUnit)
	var perr *regexparse.Error
	if errors.As(err, &perr) && perr.Code == regexparse.ErrBudget {
		return limit + 1, nil
	}
	if err != nil {
		return 0, err
	}
	n := 4 + size(re) + int((work+WorkPerUnit-1)/WorkPerUnit) // a program has some fixed instructions
	if mode == CompileAndPrint {
		n += (classRunes(re) + ClassRunesPerUnit - 1) / ClassRunesPerUnit
	}
	return n, nil
}

// classRunes returns the runes in the fold window of each character class of the tree. The Prometheus
// matcher prints the parsed expression again, and the printer examines the fold partners of each such
// rune, one by one, once for each class node. A negated class such as [^/] covers almost the whole window.
func classRunes(re *regexparse.Regexp) int {
	n := 0
	if re.Op == regexparse.OpCharClass {
		for i := 0; i+1 < len(re.Rune); i += 2 {
			if lo, hi := max(re.Rune[i], minFold), min(re.Rune[i+1], maxFold); lo <= hi {
				n += int(hi-lo) + 1
			}
		}
	}
	for _, sub := range re.Sub {
		n += classRunes(sub)
	}
	return n
}

// size follows calcSize of regexp/syntax for the instructions, with some more room, and adds the rune
// ranges of each character class. The parser rejects programs of more than about 3.3 million instructions;
// the ranges of a class are at most a few thousand, so the sums cannot overflow.
func size(re *regexparse.Regexp) int {
	n := 0
	switch re.Op {
	case regexparse.OpLiteral:
		n = len(re.Rune)
	case regexparse.OpCharClass:
		n = 1 + len(re.Rune)/2
	case regexparse.OpCapture, regexparse.OpStar, regexparse.OpPlus, regexparse.OpQuest:
		n = 2 + size(re.Sub[0])
	case regexparse.OpConcat:
		for _, sub := range re.Sub {
			n += size(sub)
		}
	case regexparse.OpAlternate:
		for _, sub := range re.Sub {
			n += size(sub)
		}
		n += len(re.Sub) - 1
	case regexparse.OpRepeat:
		sub := size(re.Sub[0])
		switch {
		case re.Max == -1 && re.Min == 0:
			n = 2 + sub
		case re.Max == -1:
			n = 1 + re.Min*sub
		default:
			n = re.Max*sub + re.Max - re.Min
		}
		// The parser builds the subexpression once also for x{0}.
		n = max(n, sub)
	}
	return max(1, n)
}
