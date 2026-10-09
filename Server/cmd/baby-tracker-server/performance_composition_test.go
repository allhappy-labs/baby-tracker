package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var performanceComposition func(*testing.T)

// Default database gate re-execs exactly one tagged test. The synthetic issuer
// exists only in the tagged child; failure, timeout or absent completion fail.
func TestPerformanceComposition(t *testing.T) {
	if performanceComposition != nil {
		performanceComposition(t)
		return
	}
	if os.Getenv("BT_PERFORMANCE_COMPOSITION_CHILD") == "1" {
		t.Fatal("identitytest composition absent")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-tags", "identitytest", "-race", "-count=1", "-v", "-run", "^TestPerformanceComposition$", ".")
	cmd.Env = append(os.Environ(), "BT_PERFORMANCE_COMPOSITION_CHILD=1")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tagged composition failed: %v\n%s", err, raw)
	}
	if !strings.Contains(string(raw), "performance composition proof complete") {
		t.Fatal("tagged child did not complete")
	}
	t.Log(string(raw))
}
