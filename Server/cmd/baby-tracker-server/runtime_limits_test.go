package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"runtime"
	"runtime/debug"
	"testing"
)

func cgroupFiles(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		content, ok := files[path]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(content), nil
	}
}

func TestCgroupLimitsParsesV2Files(t *testing.T) {
	files := map[string]string{"/sys/fs/cgroup/cpu.max": "400000 100000\n", "/sys/fs/cgroup/memory.max": "8589934592\n"}
	quota, memory, ok := cgroupLimits(func(p string) ([]byte, error) { return []byte(files[p]), nil })
	if !ok || quota != 4 || memory != 8589934592 {
		t.Fatalf("got %v %v %v", quota, memory, ok)
	}
	files["/sys/fs/cgroup/memory.max"] = "max\n"
	if _, memory, _ := cgroupLimits(func(p string) ([]byte, error) { return []byte(files[p]), nil }); memory != 0 {
		t.Fatalf("unlimited memory read as %d", memory)
	}
	for _, c := range []struct {
		name   string
		files  map[string]string
		quota  float64
		memory int64
		ok     bool
	}{
		{"unlimited", map[string]string{"/sys/fs/cgroup/cpu.max": "max 100000\n", "/sys/fs/cgroup/memory.max": "max\n"}, 0, 0, true},
		{"a fractional quota", map[string]string{"/sys/fs/cgroup/cpu.max": "150000 100000\n", "/sys/fs/cgroup/memory.max": "1073741824\n"}, 1.5, 1 << 30, true},
		{"no cgroup v2 (macOS, cgroup v1)", map[string]string{}, 0, 0, false},
		{"garbage", map[string]string{"/sys/fs/cgroup/cpu.max": "four\n", "/sys/fs/cgroup/memory.max": "lots\n"}, 0, 0, false},
		{"a zero period", map[string]string{"/sys/fs/cgroup/cpu.max": "400000 0\n", "/sys/fs/cgroup/memory.max": "max\n"}, 0, 0, false},
		{"a negative memory", map[string]string{"/sys/fs/cgroup/cpu.max": "max 100000\n", "/sys/fs/cgroup/memory.max": "-1\n"}, 0, 0, false},
	} {
		quota, memory, ok := cgroupLimits(cgroupFiles(c.files))
		if quota != c.quota || memory != c.memory || ok != c.ok {
			t.Errorf("%s: got %v %v %v, want %v %v %v", c.name, quota, memory, ok, c.quota, c.memory, c.ok)
		}
	}
}

// The startup line states the Go runtime's limits beside the container's, so
// a run's log shows whether GOMAXPROCS and GOMEMLIMIT follow the caps.
func TestRuntimeLimitsLogLine(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	logRuntimeLimits(logger, cgroupFiles(map[string]string{"/sys/fs/cgroup/cpu.max": "400000 100000\n", "/sys/fs/cgroup/memory.max": "8589934592\n"}))
	var line struct {
		Msg       string  `json:"msg"`
		Procs     int     `json:"gomaxprocs"`
		MemLimit  int64   `json:"gomemlimit"`
		CPUQuota  float64 `json:"cgroup_cpu_quota"`
		MemoryMax int64   `json:"cgroup_memory_max"`
		CgroupV2  bool    `json:"cgroup_v2"`
	}
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if line.Msg != "runtime limits" || line.Procs != runtime.GOMAXPROCS(0) || line.MemLimit != debug.SetMemoryLimit(-1) ||
		line.CPUQuota != 4 || line.MemoryMax != 8589934592 || !line.CgroupV2 {
		t.Fatalf("line %+v from %s", line, out.String())
	}
	out.Reset()
	logRuntimeLimits(logger, func(string) ([]byte, error) { return nil, errors.New("no cgroup") })
	if err := json.Unmarshal(out.Bytes(), &line); err != nil || line.CgroupV2 || line.CPUQuota != 0 || line.MemoryMax != 0 {
		t.Fatalf("without cgroup files: %+v %v", line, err)
	}
}
