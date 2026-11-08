package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"runtime/debug"
	"testing"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeCgroup serves memory.max with content and nothing else.
func fakeCgroup(content string) func(string) ([]byte, error) {
	return cgroupFiles(map[string]string{cgroupMemoryMax: content + "\n"})
}

func TestMemoryLimitDefaultsTo85PercentOfTheCgroup(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(-1))
	got := applyMemoryLimit(func(string) string { return "" }, fakeCgroup("8589934592"), discard)
	if got != 8589934592*85/100 {
		t.Fatalf("limit %d", got)
	}
	if in := debug.SetMemoryLimit(-1); in != got {
		t.Fatalf("runtime limit %d, returned %d", in, got)
	}
}

func TestExplicitGOMEMLIMITWins(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(-1))
	before := debug.SetMemoryLimit(-1)
	got := applyMemoryLimit(func(k string) string {
		if k == "GOMEMLIMIT" {
			return "400MiB"
		}
		return ""
	}, fakeCgroup("536870912"), discard)
	if got != before || debug.SetMemoryLimit(-1) != before {
		t.Fatal("an explicit GOMEMLIMIT was overridden")
	}
}

func TestUnlimitedCgroupLeavesTheLimit(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(-1))
	debug.SetMemoryLimit(math.MaxInt64)
	logger, source := sourceRecorder(t)
	if got := applyMemoryLimit(func(string) string { return "" }, fakeCgroup("max"), logger); got != math.MaxInt64 || debug.SetMemoryLimit(-1) != math.MaxInt64 {
		t.Fatalf("memory.max \"max\" changed the limit to %d", got)
	}
	if got := source(); got != "cgroup_unlimited" {
		t.Fatalf("source %q", got)
	}
}

// sourceRecorder returns a logger and a function reading the "source" of the
// last line it logged.
func sourceRecorder(t *testing.T) (*slog.Logger, func() string) {
	var out bytes.Buffer
	return slog.New(slog.NewJSONHandler(&out, nil)), func() string {
		var line struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(out.Bytes(), &line); err != nil {
			t.Fatalf("%v: %s", err, out.String())
		}
		out.Reset()
		return line.Source
	}
}

// A memory.max that is missing (macOS, cgroup v1) or not a positive integer
// leaves the limit as it is rather than guessing.
func TestUnreadableCgroupLeavesTheLimit(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(-1))
	const before = int64(3 << 30)
	debug.SetMemoryLimit(before)
	logger, source := sourceRecorder(t)
	for name, c := range map[string]struct {
		read   func(string) ([]byte, error)
		source string
	}{
		"missing":  {func(string) ([]byte, error) { return nil, errors.New("no cgroup") }, "no_cgroup"},
		"garbage":  {fakeCgroup("lots"), "cgroup_unreadable"},
		"negative": {fakeCgroup("-1"), "cgroup_unreadable"},
		"zero":     {fakeCgroup("0"), "cgroup_unreadable"},
		"float":    {fakeCgroup("8.5e9"), "cgroup_unreadable"},
		"empty":    {fakeCgroup(""), "cgroup_unreadable"},
	} {
		if got := applyMemoryLimit(func(string) string { return "" }, c.read, logger); got != before || debug.SetMemoryLimit(-1) != before {
			t.Errorf("%s memory.max changed the limit to %d", name, got)
		}
		if got := source(); got != c.source {
			t.Errorf("%s: source %q, want %q", name, got, c.source)
		}
	}
}

// The startup line states the limit and where it came from.
func TestMemoryLimitLogLine(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(-1))
	var out bytes.Buffer
	applyMemoryLimit(func(string) string { return "" }, fakeCgroup("8589934592"), slog.New(slog.NewJSONHandler(&out, nil)))
	var line struct {
		Msg       string `json:"msg"`
		Limit     int64  `json:"gomemlimit"`
		Source    string `json:"source"`
		MemoryMax int64  `json:"cgroup_memory_max"`
	}
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if line.Msg != "memory limit" || line.Limit != 8589934592*85/100 || line.Source != "cgroup" || line.MemoryMax != 8589934592 {
		t.Fatalf("line %+v", line)
	}
}
