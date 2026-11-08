package main

import (
	"log/slog"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	cgroupCPUMax    = "/sys/fs/cgroup/cpu.max"
	cgroupMemoryMax = "/sys/fs/cgroup/memory.max"
)

// cgroupLimits reads the process's cgroup v2 limits through read: the CPU
// quota in CPUs ("quota period" in cpu.max; 0 for "max", no quota) and the
// memory limit in bytes (memory.max; 0 for "max", no limit). ok is false
// when either file is missing or not understood (no cgroup v2: macOS, or
// cgroup v1).
func cgroupLimits(read func(string) ([]byte, error)) (cpuQuota float64, memoryMax int64, ok bool) {
	cpu, err := read(cgroupCPUMax)
	if err != nil {
		return 0, 0, false
	}
	memory, err := read(cgroupMemoryMax)
	if err != nil {
		return 0, 0, false
	}
	fields := strings.Fields(string(cpu))
	if len(fields) != 2 {
		return 0, 0, false
	}
	period, err := strconv.ParseFloat(fields[1], 64)
	if err != nil || period <= 0 {
		return 0, 0, false
	}
	if fields[0] != "max" {
		quota, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || quota <= 0 {
			return 0, 0, false
		}
		cpuQuota = quota / period
	}
	if value := strings.TrimSpace(string(memory)); value != "max" {
		memoryMax, err = strconv.ParseInt(value, 10, 64)
		if err != nil || memoryMax < 0 {
			return 0, 0, false
		}
	}
	return cpuQuota, memoryMax, true
}

// logRuntimeLimits states at startup the Go runtime's limits beside the
// container's, so a run's log shows whether GOMAXPROCS and GOMEMLIMIT follow
// the caps. debug.SetMemoryLimit(-1) reads the limit without changing it.
// The cgroup values are 0 when unlimited or unknown (cgroup_v2 false).
func logRuntimeLimits(logger *slog.Logger, read func(string) ([]byte, error)) {
	quota, memory, ok := cgroupLimits(read)
	logger.Info("runtime limits", slog.Int("gomaxprocs", runtime.GOMAXPROCS(0)), slog.Int64("gomemlimit", debug.SetMemoryLimit(-1)),
		slog.Float64("cgroup_cpu_quota", quota), slog.Int64("cgroup_memory_max", memory), slog.Bool("cgroup_v2", ok))
}
