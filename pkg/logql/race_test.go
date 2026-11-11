//go:build race

package logql

// timeScale widens the time bounds of the tests. The race detector makes code much slower.
const timeScale = 20
