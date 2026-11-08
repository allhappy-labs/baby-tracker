package main

import (
	"log/slog"
	"math"
	"runtime/debug"
	"strconv"
	"strings"
)

// memoryLimitPercent is the share of the container's memory the Go heap is
// steered under: the rest covers goroutine stacks, the runtime's own
// structures and cgo/OS overhead outside the GC's accounting.
const memoryLimitPercent = 85

// applyMemoryLimit gives the Go runtime a soft memory limit of 85% of the
// cgroup v2 memory.max, so the GC works harder before the container is
// OOM-killed instead of after. An explicit GOMEMLIMIT wins (the runtime has
// already applied it). memory.max "max" (no limit), a missing file (macOS,
// cgroup v1) or an unreadable value leave the limit as it is. It returns the
// limit in effect afterwards and logs one line with it. Call it before any
// pool opens, so every allocation runs under the limit.
func applyMemoryLimit(getenv func(string) string, read func(string) ([]byte, error), logger *slog.Logger) int64 {
	var source string
	memoryMax := int64(0)
	switch {
	case getenv("GOMEMLIMIT") != "":
		source = "GOMEMLIMIT"
	default:
		raw, err := read(cgroupMemoryMax)
		if err != nil {
			source = "no_cgroup"
			break
		}
		value := strings.TrimSpace(string(raw))
		if value == "max" {
			source = "cgroup_unlimited"
			break
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			source = "cgroup_unreadable"
			break
		}
		memoryMax = parsed
	}
	var limit int64
	if memoryMax > 0 {
		if memoryMax <= math.MaxInt64/memoryLimitPercent {
			limit = memoryMax * memoryLimitPercent / 100
		} else {
			limit = memoryMax / 100 * memoryLimitPercent // no overflow near 2^63
		}
		debug.SetMemoryLimit(limit)
		source = "cgroup"
	} else {
		limit = debug.SetMemoryLimit(-1)
	}
	logger.Info("memory limit", slog.Int64("gomemlimit", limit), slog.String("source", source), slog.Int64("cgroup_memory_max", memoryMax))
	return limit
}
