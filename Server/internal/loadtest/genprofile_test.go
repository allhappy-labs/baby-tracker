package loadtest

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"runtime/metrics"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
)

// A run with --profile-dir writes the generator's profiles beside --out
// (relative), lists them in result.json, and refuses to write over a
// directory that already holds them. Every run reports its release lag per
// class (summing to the gate's) and its Go runtime over the measurement.
func TestRunProfilesTheGeneratorAndReportsItsHealth(t *testing.T) {
	f := newFakeV1(t)
	m := Model{RefreshPerSecond: 0.5, PullPerSecond: 20, WritePerSecond: 2}
	cfg := runConfig(t, f, 5, m)
	cfg.Warmup, cfg.Measure, cfg.ProfileDir = time.Second, 3*time.Second, "profiles"
	summary, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := readResult(t, cfg.Out)
	dir := filepath.Join(filepath.Dir(cfg.Out), "profiles")
	if r.GeneratorProfile == nil || r.GeneratorProfile.Dir != dir || r.GeneratorProfile.Error != "" || r.GeneratorProfile.LagTraces == nil {
		t.Fatalf("generator_profile %+v", r.GeneratorProfile)
	}
	for _, name := range r.GeneratorProfile.Files {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || len(raw) == 0 {
			t.Fatalf("%s: %d bytes, %v", name, len(raw), err)
		}
		if strings.HasSuffix(name, ".pb.gz") && !bytes.HasPrefix(raw, []byte{0x1f, 0x8b}) {
			t.Fatalf("%s is not gzip", name)
		}
	}
	if len(r.GeneratorProfile.Files) != 6 {
		t.Fatalf("files %v", r.GeneratorProfile.Files)
	}

	var arrivals uint64
	for _, class := range []string{"pull", "write", "refresh"} {
		l, ok := r.SchedulerLagByClass[class]
		if !ok || l.Arrivals == 0 || l.P99Ms > l.MaxMs || l.P50Ms > l.P99Ms {
			t.Fatalf("scheduler_lag_by_class[%s] %+v (all: %v)", class, l, r.SchedulerLagByClass)
		}
	}
	for _, l := range r.SchedulerLagByClass {
		arrivals += l.Arrivals
	}
	if arrivals != summary.SchedulerLag.Total {
		t.Fatalf("the classes hold %d arrivals, the gate %d", arrivals, summary.SchedulerLag.Total)
	}
	rt := r.GeneratorRuntime
	if rt == nil || rt.GOMAXPROCS < 1 || rt.NumCPU < 1 || rt.Seconds < cfg.Measure.Seconds()-0.5 || rt.Seconds > cfg.Measure.Seconds()+0.5 ||
		rt.UserCores <= 0 || rt.Goroutines == 0 || rt.HeapObjectsBytes == 0 || rt.SchedLatencyMaxMs < rt.SchedLatencyP99Ms {
		t.Fatalf("generator_runtime %+v", rt)
	}

	t.Logf("generator_runtime %+v; lags %+v", *rt, r.SchedulerLagByClass)

	// A second run into the same directory is refused before it starts.
	again := runConfig(t, f, 5, m)
	again.Warmup, again.Measure, again.ProfileDir = time.Second, time.Second, dir
	if _, err := Run(t.Context(), again); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a run over an earlier profile: %v", err)
	}
}

// A measured arrival released more than 20 ms late writes the flight
// recorder's window once; another within the spacing does not; a lag at the
// bound does not.
func TestALateArrivalSnapshotsTheExecutionTrace(t *testing.T) {
	dir := t.TempDir()
	p, err := startGeneratorProfile(dir)
	if err != nil {
		t.Fatal(err)
	}
	p.lagged(lagTraceThreshold) // at the bound: nothing
	time.Sleep(700 * time.Millisecond)
	p.lagged(45 * time.Millisecond)
	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, "generator-lag-trace-1.out"))
		return err == nil
	})
	p.lagged(90 * time.Millisecond) // within the spacing
	time.Sleep(700 * time.Millisecond)
	traces, err := p.stop()
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 || traces[0].File != "generator-lag-trace-1.out" || traces[0].LagMs != 45 || traces[0].Error != "" {
		t.Fatalf("traces %+v", traces)
	}
	raw, err := os.ReadFile(filepath.Join(dir, traces[0].File))
	if err != nil || !bytes.HasPrefix(raw, []byte("go 1.")) {
		t.Fatalf("the trace is not a Go execution trace (%d bytes, %v)", len(raw), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "generator-lag-trace-2.out")); err == nil {
		t.Fatal("a second trace within the spacing")
	}
}

// The class lags count only the measurement's arrivals (by intended
// start), as the gate does.
func TestClassLagsCountTheMeasurementOnly(t *testing.T) {
	g := testGenerator(t, newFakeV1(t), 1)
	g.phases = newPhases(time.Now())
	g.phases.setMeasureEnd(time.Now().Add(time.Minute))
	observe := g.observeClass("pull", g.phases.observe)
	observe(time.Now().Add(-time.Second), 3*time.Millisecond, false)  // warm-up
	observe(time.Now().Add(time.Second), 5*time.Millisecond, false)   // measurement
	observe(time.Now().Add(2*time.Minute), 7*time.Millisecond, false) // drain
	observe(time.Now().Add(2*time.Second), 9*time.Millisecond, true)  // measurement, dropped
	l := g.lags.summary()["pull"]
	if l.Arrivals != 2 || l.MaxMs < 9 || l.MaxMs > 9.5 {
		t.Fatalf("pull %+v", l)
	}
	var s RunSummary
	g.phases.health.into(&s)
	if s.SchedulerLag.Total != l.Arrivals {
		t.Fatalf("the gate holds %d, the class %d", s.SchedulerLag.Total, l.Arrivals)
	}
}

// A directory holding an earlier run's lag trace is refused too; a CPU
// profile that cannot start (another is running) leaves no empty file to
// refuse the next run.
func TestProfileDirRefusalsLeaveNothingBehind(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lagTraceName(2)), []byte("go 1.26 trace"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := startGeneratorProfile(dir); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a directory with a lag trace: %v", err)
	}

	var other bytes.Buffer
	if err := pprof.StartCPUProfile(&other); err != nil {
		t.Skipf("a CPU profile already runs (go test -cpuprofile?): %v", err)
	}
	defer pprof.StopCPUProfile()
	fresh := t.TempDir()
	if _, err := startGeneratorProfile(fresh); err == nil {
		t.Fatal("a second CPU profile started")
	}
	if _, err := os.Lstat(filepath.Join(fresh, profileCPU)); !os.IsNotExist(err) {
		t.Fatalf("the failed start left %s: %v", profileCPU, err)
	}
}

// The share of wakeups over 10 ms counts the bucket straddling 10 ms by the
// part of it above, and an unbounded last bucket whole.
func TestShareAboveSplitsTheStraddlingBucket(t *testing.T) {
	h := &metrics.Float64Histogram{Buckets: []float64{0, 0.005, 0.015, math.Inf(1)}, Counts: []uint64{2, 2, 1}}
	if got := histShareAbove(h, 0.010); math.Abs(got-0.4) > 1e-9 { // (2×½ + 1) / 5
		t.Fatalf("share %v, want 0.4", got)
	}
	if got := histShareAbove(h, 0.020); math.Abs(got-0.2) > 1e-9 { // the unbounded bucket
		t.Fatalf("share %v, want 0.2", got)
	}
	if got := histShareAbove(h, 0); got != 1 {
		t.Fatalf("share %v, want 1", got)
	}
}
