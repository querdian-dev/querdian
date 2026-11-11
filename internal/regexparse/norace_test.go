//go:build !race

package regexparse

// raceEnabled is true when the race detector is on. It makes time slower, so the tests do not check time then.
const raceEnabled = false
