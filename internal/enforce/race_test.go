//go:build race

package enforce_test

// timeScale widens the time bounds of the tests. The race detector makes code much slower.
const timeScale = 20
