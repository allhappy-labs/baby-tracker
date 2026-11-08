package wake

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/diagnostics"
)

// Len is every entry the tracker holds until the next drain: the devices'
// progress, the changed families and the deferred ones.
func TestTrackerLenCountsHeldEntries(t *testing.T) {
	tracker := NewTracker(2)
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if tracker.Len() != 0 {
		t.Fatalf("empty tracker len %d", tracker.Len())
	}
	tracker.Synced(familyA, deviceN(1), 1, 1, at)
	tracker.Synced(familyA, deviceN(1), 2, 2, at) // the same device
	tracker.Synced(familyA, deviceN(2), 1, 1, at)
	tracker.Synced(familyA, deviceN(3), 1, 1, at) // past the bound: dropped
	tracker.Changed(familyA, 3)
	if got := tracker.Len(); got != 3 {
		t.Fatalf("len %d, want 2 devices and 1 family", got)
	}
	tracker.deferFamily(familyB, at.Add(time.Minute))
	if got := tracker.Len(); got != 4 {
		t.Fatalf("len %d with a deferred family, want 4", got)
	}
	tracker.drain(at, 0)
	if got := tracker.Len(); got != 1 {
		t.Fatalf("len %d after the drain, want the deferred family alone", got)
	}
	var none *Tracker
	if none.Len() != 0 {
		t.Fatal("a nil tracker holds entries")
	}
}

// The report counts the sends the client never put on the wire and the lost
// answers apart from Apple's refusals, for the worker's cumulative totals.
func TestPushReportCountsNotSentAndAmbiguous(t *testing.T) {
	var report pushReport
	job := pushJob{family: "f", device: "d", sequence: 2}
	report.record(job, apns.Result{Class: apns.Accepted, Status: 200}, nil)
	report.record(job, apns.Result{}, fmt.Errorf("%w: dial", apns.ErrNotSent))
	report.record(job, apns.Result{Class: apns.Ambiguous}, nil)
	report.record(job, apns.Result{}, errors.New("stream reset"))
	report.record(job, apns.Result{Class: apns.Retryable, Status: 503}, nil)
	if report.notSentCount != 1 || report.ambiguous != 2 || report.sent != 4 || report.failed != 4 {
		t.Fatalf("notSent %d ambiguous %d sent %d failed %d, want 1, 2, 4, 4", report.notSentCount, report.ambiguous, report.sent, report.failed)
	}
}

// A tick's counts land together: a copy taken while ticks are added never
// shows NotSent or Ambiguous ahead of PushFailed, nor PushFailed ahead of
// Ticks.
func TestTotalsCopyIsNeverTorn(t *testing.T) {
	w := &Worker{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20000 {
			w.totals.addTick(Stats{Pushed: 2, PushFailed: 2}, tickCounts{notSent: 1, ambiguous: 1}, true, 0)
		}
	}()
	for {
		select {
		case <-done:
			if got := w.Totals(); got.Ticks != 20000 || got.TicksSucceeded != 20000 || got.PushFailed != 40000 || got.NotSent != 20000 || got.Ambiguous != 20000 {
				t.Fatalf("final totals %+v", got)
			}
			return
		default:
		}
		got := w.Totals()
		if got.NotSent+got.Ambiguous != got.PushFailed || got.PushFailed != 2*got.Ticks || got.TicksSucceeded != got.Ticks {
			t.Fatalf("a torn copy: %+v", got)
		}
	}
}

// SettlesPending mirrors the device rows kept now, through keeps and drops.
func TestTotalsMirrorTheKeptSettleRows(t *testing.T) {
	w := &Worker{}
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	w.keepSettle(settleOf(start, 3))
	if got := w.Totals().SettlesPending; got != 3 {
		t.Fatalf("settles pending %d, want 3", got)
	}
	for i := range 4 {
		w.keepSettle(settleOf(start.Add(time.Duration(i+1)*time.Second), maxPushTargets))
	}
	if got := w.Totals().SettlesPending; got != uint64(w.settleRows) || got != uint64(maxSettleRows) {
		t.Fatalf("settles pending %d, kept rows %d, want %d", got, w.settleRows, maxSettleRows)
	}
}

// A tick without work is not timed; one with work is (the capacity
// histogram's Observe, its buckets and clamping), with its families.
func TestTickDurationTimesOnlyWorkingTicks(t *testing.T) {
	var c totalCounters
	c.addTick(Stats{}, tickCounts{}, true, time.Millisecond)
	c.addTick(Stats{}, tickCounts{worked: true, families: 3}, true, 2*time.Millisecond)
	c.addTick(Stats{}, tickCounts{worked: true, families: 1}, false, 20*time.Millisecond)
	var want diagnostics.Histogram
	want.Observe(2 * time.Millisecond)
	want.Observe(20 * time.Millisecond)
	if got := c.totals; got.Ticks != 3 || got.TickDurationNS != want || got.Families != 4 || got.FamiliesPeak != 3 {
		t.Fatalf("totals %+v, want durations %+v", got, want)
	}
}
