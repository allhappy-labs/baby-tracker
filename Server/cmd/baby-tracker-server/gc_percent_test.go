package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"math"
	"runtime/debug"
	"testing"
)

// With a memory limit and no GOGC, the GC lets the heap grow to five times
// its live size; the line says so.
func TestGCPercentRisesUnderAMemoryLimit(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(100))
	var out bytes.Buffer
	got := applyGCPercent(func(string) string { return "" }, 7<<30, slog.New(slog.NewJSONHandler(&out, nil)))
	if got != gcPercentUnderLimit || currentGCPercent() != gcPercentUnderLimit {
		t.Fatalf("gogc %d, runtime %d", got, currentGCPercent())
	}
	var line struct {
		Msg    string `json:"msg"`
		GOGC   int    `json:"gogc"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if line.Msg != "gc percent" || line.GOGC != gcPercentUnderLimit || line.Source != "memory_limit" {
		t.Fatalf("line %+v", line)
	}
}

// An explicit GOGC wins (the runtime has already applied it), and without a
// memory limit nothing would bound a larger heap, so the default stays.
func TestGCPercentKeptWithGOGCOrNoLimit(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(100))
	for name, c := range map[string]struct {
		gogc  string
		limit int64
	}{
		"GOGC set": {"150", 7 << 30},
		"no limit": {"", math.MaxInt64},
		"unknown":  {"", 0},
	} {
		debug.SetGCPercent(150)
		got := applyGCPercent(func(k string) string {
			if k == "GOGC" {
				return c.gogc
			}
			return ""
		}, c.limit, discard)
		if got != 150 || currentGCPercent() != 150 {
			t.Errorf("%s: gogc %d, runtime %d", name, got, currentGCPercent())
		}
	}
}

func currentGCPercent() int {
	percent := debug.SetGCPercent(-1)
	debug.SetGCPercent(percent)
	return percent
}
