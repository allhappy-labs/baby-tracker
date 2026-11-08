package loadtest

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// R1: a tracked device that never completed its seed is left out of
// tracked.jsonl, so the audit cannot check it: any fails the run and the
// window verdicts.
func TestUnseededTrackedDevicesFailTheVerdict(t *testing.T) {
	s := passing()
	s.TrackedUnseeded = 1
	for name, v := range map[string]Verdict{"run": Evaluate(s, SpecThresholds()), "window": EvaluateWindow(s, SpecThresholds())} {
		if v.Pass || !slices.ContainsFunc(v.Reasons, func(r string) bool { return strings.HasPrefix(r, "tracked devices unseeded 1") }) {
			t.Fatalf("%s verdict: %v", name, v.Reasons)
		}
	}
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	g.devices[0].tracked = newTrackedState()
	if got := g.summary(g.phases.measure).TrackedUnseeded; got != 1 {
		t.Fatalf("summary counts %d unseeded", got)
	}
	g.devices[0].tracked.markSeeded()
	if got := g.summary(g.phases.measure).TrackedUnseeded; got != 0 {
		t.Fatalf("summary counts %d unseeded after the seed", got)
	}
}

// m1: launches and skips are filed by intended start, so a ramp step or a
// declared window carries its own skipped gate.
func TestSkipsAreGatedPerWindow(t *testing.T) {
	start := time.Now()
	p := newPhases(start)
	p.setMeasureEnd(start.Add(2 * time.Minute))
	first := p.addWindow("first", 1, start, start.Add(time.Minute))
	second := p.addWindow("second", 1, start.Add(time.Minute), start.Add(2*time.Minute))
	for i := range 100 {
		p.observe(start.Add(time.Duration(i)*100*time.Millisecond), time.Millisecond, false)
		p.observe(start.Add(time.Minute+time.Duration(i)*100*time.Millisecond), time.Millisecond, false)
	}
	p.skipped(start.Add(70 * time.Second))
	p.skipped(start.Add(80 * time.Second))
	p.skipped(start.Add(-time.Second)) // warm-up: no span holds it
	g := &generator{phases: p}
	a, b := g.windowSummary(first), g.windowSummary(second)
	if a.LaunchedOps != 100 || a.SkippedOps != 0 || b.LaunchedOps != 100 || b.SkippedOps != 2 {
		t.Fatalf("first %d/%d, second %d/%d", a.SkippedOps, a.LaunchedOps, b.SkippedOps, b.LaunchedOps)
	}
	whole := RunSummary{}
	p.health.into(&whole)
	if whole.LaunchedOps != 200 || whole.SkippedOps != 2 {
		t.Fatalf("measurement %d/%d", whole.SkippedOps, whole.LaunchedOps)
	}
	step := passing()
	step.LaunchedOps, step.SkippedOps = b.LaunchedOps, b.SkippedOps
	if EvaluateWindow(step, SpecThresholds()).Pass {
		t.Fatal("a window with 2% skipped passed")
	}
}

// m5: once the final waits begin, nothing is added to the WaitGroups.
func TestNothingStartsOnceTheRunIsClosing(t *testing.T) {
	g := &generator{}
	ran := make(chan struct{}, 1)
	g.goBG(func() { ran <- struct{}{} })
	<-ran
	g.startClosing()
	g.goBG(func() { ran <- struct{}{} })
	if g.addReader() {
		t.Fatal("a reader was added while closing")
	}
	g.bg.Wait()
	select {
	case <-ran:
		t.Fatal("an operation started while closing")
	case <-time.After(50 * time.Millisecond):
	}
}
