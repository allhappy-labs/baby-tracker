package wake_test

import (
	"context"
	"testing"
	"time"

	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/wake"
)

// Totals accumulate every tick's Stats from the worker's start, and count
// every tick, an idle one included, in Ticks; TicksSucceeded leaves out a
// tick whose work failed (the identitytest wake route reports it, so a
// failed tick never stands in for one that scanned and sent).
func TestWorkerTotalsAccumulateAcrossTicks(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, sender := wake.NewTracker(0), &recordingSender{}
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: sender, Now: func() time.Time { return clock }}
	if got := w.Totals(); got != (wake.Totals{}) {
		t.Fatalf("a new worker's totals %+v", got)
	}
	tracker.Synced(h.FamilyID, device(1), 0, 0, clock)
	tracker.Synced(h.FamilyID, device(2), 0, 0, clock)
	tracker.Changed(h.FamilyID, h.FamilySequence(t))
	first := tick(t, w)
	clock = clock.Add(time.Second)
	tracker.Synced(h.FamilyID, device(3), 0, 0, clock)
	tracker.Changed(h.FamilyID, h.FamilySequence(t))
	second := tick(t, w)
	if first.Flushed == 0 || second.Flushed == 0 || first.Woken == 0 {
		t.Fatalf("ticks %+v %+v, want both to flush and the first to wake", first, second)
	}
	totals := w.Totals()
	want := wake.Totals{Ticks: 2, TicksSucceeded: 2, Flushed: uint64(first.Flushed + second.Flushed), Woken: uint64(first.Woken + second.Woken),
		Families: 2, FamiliesPeak: 1}
	durations := totals.TickDurationNS
	totals.TickDurationNS = want.TickDurationNS
	if totals != want {
		t.Fatalf("totals %+v, want %+v", totals, want)
	}
	if durations.Count != 2 || durations.TotalNS == 0 {
		t.Fatalf("tick durations %+v, want both ticks observed", durations)
	}
	tick(t, w) // idle
	if totals := w.Totals(); totals.Ticks != 3 || totals.TicksSucceeded != 3 || totals.Flushed != want.Flushed || totals.TickDurationNS.Count != 2 {
		t.Fatalf("after an idle tick %+v, want 3 ticks, all succeeded, the same flushed, and the idle one not timed", totals)
	}
	// A tick with work whose transaction cannot begin fails: it ran, but
	// did not succeed.
	clock = clock.Add(time.Second)
	tracker.Synced(h.FamilyID, device(4), 0, 0, clock)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := w.Tick(cancelled); err == nil {
		t.Fatal("a tick under a cancelled context succeeded")
	}
	if totals := w.Totals(); totals.Ticks != 4 || totals.TicksSucceeded != 3 {
		t.Fatalf("after a failed tick %+v, want 4 ticks and 3 succeeded", totals)
	}
	tick(t, w) // the restored work
	if totals := w.Totals(); totals.Ticks != 5 || totals.TicksSucceeded != 4 {
		t.Fatalf("after the retry %+v, want 5 ticks and 4 succeeded", totals)
	}
	var none *wake.Worker
	if got := none.Totals(); got != (wake.Totals{}) {
		t.Fatalf("nil worker totals %+v", got)
	}
}
