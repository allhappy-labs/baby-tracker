package main

import (
	"log/slog"
	"math"
	"runtime/debug"
)

// gcPercentUnderLimit is the GOGC the server runs with when a memory limit
// steers the heap. At the default 100 the heap is small against the limit, so
// the GC collected about once a second at 2x the 10,000-family model and took
// 15-17% of the app's CPU (Phase 5 load report, section 10). 400 lets the heap
// grow four times its live size between collections; the memory limit still
// makes the GC work harder before the container runs out.
const gcPercentUnderLimit = 400

// applyGCPercent sets GOGC to gcPercentUnderLimit when the runtime has a
// memory limit (limit, as applyMemoryLimit returned it) and GOGC is not set.
// Without a limit nothing would bound the larger heap, so the default stays.
// It returns the GOGC in effect afterwards and logs one line with it.
func applyGCPercent(getenv func(string) string, limit int64, logger *slog.Logger) int {
	var source string
	switch {
	case getenv("GOGC") != "":
		source = "GOGC"
	case limit <= 0 || limit == math.MaxInt64:
		source = "no_memory_limit"
	default:
		debug.SetGCPercent(gcPercentUnderLimit)
		source = "memory_limit"
	}
	percent := debug.SetGCPercent(-1)
	debug.SetGCPercent(percent)
	logger.Info("gc percent", slog.Int("gogc", percent), slog.String("source", source))
	return percent
}
