//go:build !race

package policy

// timeScale widens the time bounds of the tests. The race detector makes code much slower.
const timeScale = 1
