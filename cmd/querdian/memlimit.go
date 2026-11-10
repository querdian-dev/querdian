package main

import (
	"fmt"
	"runtime/debug"
	"strconv"
)

// memoryLimitEnv holds the memory limit of the container in bytes. The Helm chart sets it from
// resources.limits.memory with the downward API. An empty value or 0 means no limit: Querdian then sets no
// Go memory limit.
const memoryLimitEnv = "QUERDIAN_MEMORY_LIMIT"

// memoryLimitPercent is the part of the container memory limit that the Go runtime uses as its soft memory
// limit. The rest is for memory outside the Go heap (stacks, runtime data, buffers of the kernel).
const memoryLimitPercent = 90

// goMemoryLimit returns the soft memory limit for the Go runtime from the environment. It returns 0 when
// GOMEMLIMIT is set (the runtime reads it itself) or the container limit is empty or 0.
func goMemoryLimit(getenv func(string) string) (int64, error) {
	if getenv("GOMEMLIMIT") != "" {
		return 0, nil
	}
	v := getenv(memoryLimitEnv)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a number of bytes (0 for no limit), got %q", memoryLimitEnv, v)
	}
	return n / 100 * memoryLimitPercent, nil
}

// applyMemoryLimit sets the soft memory limit of the Go runtime from the environment, see goMemoryLimit.
func applyMemoryLimit(getenv func(string) string) error {
	n, err := goMemoryLimit(getenv)
	if err != nil || n == 0 {
		return err
	}
	debug.SetMemoryLimit(n)
	return nil
}
