package wake

import (
	"fmt"
	"testing"
	"time"
)

const (
	familyA = "a0000000-0000-4000-8000-000000000001"
	familyB = "a0000000-0000-4000-8000-000000000002"
)

func deviceN(n int) string { return fmt.Sprintf("d0000000-0000-4000-8000-%012d", n) }

// The tracker keeps, per device, the highest cursor it presented and the
// highest high water served to it, whatever order requests finish in; per
// family, the highest committed sequence. It is bounded: past maxDevices a
// new device is dropped until the next drain, and a known one still updates.
func TestTrackerKeepsTheHighestProgressAndIsBounded(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	tracker := NewTracker(3)
	tracker.Synced(familyA, deviceN(1), 5, 9, at)
	tracker.Synced(familyA, deviceN(1), 7, 0, at.Add(-time.Minute)) // a later acknowledgement on a page with a token
	tracker.Synced(familyA, deviceN(1), 3, 8, at.Add(time.Second))  // an older request finishing last
	tracker.Synced(familyB, deviceN(2), 0, 4, at)
	tracker.Synced(familyA, deviceN(2), 2, 0, at) // the same device in another family is another entry
	tracker.Synced(familyA, deviceN(3), 1, 1, at) // past the bound: dropped
	tracker.Synced(familyA, deviceN(1), 8, 0, at) // known: still updates
	tracker.Synced("not-a-uuid", deviceN(4), 1, 1, at)
	tracker.Synced(familyA, "00000000-0000-0000-0000-000000000000", 1, 1, at)
	tracker.Synced(familyA, deviceN(5), 1<<53, 0, at) // beyond the protocol's sequences
	tracker.Changed(familyA, 10)
	tracker.Changed(familyA, 6)
	tracker.Changed(familyB, 4)
	tracker.Changed("not-a-uuid", 4)
	work := tracker.drain(at, 0)
	want := []deviceProgress{
		{FamilyID: familyA, DeviceID: deviceN(1), Acked: 8, Delivered: 9, Seen: at.Add(time.Second)},
		{FamilyID: familyA, DeviceID: deviceN(2), Acked: 2, Delivered: 0, Seen: at},
		{FamilyID: familyB, DeviceID: deviceN(2), Acked: 0, Delivered: 4, Seen: at},
	}
	if fmt.Sprint(work.devices) != fmt.Sprint(want) {
		t.Fatalf("drained devices %+v, want %+v", work.devices, want)
	}
	if fmt.Sprint(work.families) != fmt.Sprint([]familyChange{{familyA, 10}, {familyB, 4}}) {
		t.Fatalf("drained families %+v", work.families)
	}
	if tracker.Dropped() != 5 {
		t.Fatalf("dropped %d, want 5 (one past the bound, four invalid)", tracker.Dropped())
	}
	// A drain empties the tracker, so the bound admits new devices again.
	tracker.Synced(familyA, deviceN(3), 1, 1, at)
	if again := tracker.drain(at, 0); len(again.devices) != 1 || len(again.families) != 0 {
		t.Fatalf("second drain %+v", again)
	}
	// A restore merges back: newer values win, and the families return.
	tracker.Synced(familyA, deviceN(1), 9, 0, at)
	tracker.restore(work)
	merged := tracker.drain(at, 0)
	if len(merged.devices) != 3 || merged.devices[0] != (deviceProgress{FamilyID: familyA, DeviceID: deviceN(1), Acked: 9, Delivered: 9, Seen: at.Add(time.Second)}) ||
		len(merged.families) != 2 {
		t.Fatalf("restored %+v", merged)
	}
	// Families are drained in ID order, at most maxFamilies at a time; the
	// rest wait for the next drain.
	tracker.Changed(familyB, 5)
	tracker.Changed(familyA, 11)
	if first := tracker.drain(at, 1); fmt.Sprint(first.families) != fmt.Sprint([]familyChange{{familyA, 11}}) {
		t.Fatalf("first bounded drain %+v", first.families)
	}
	if second := tracker.drain(at, 1); fmt.Sprint(second.families) != fmt.Sprint([]familyChange{{familyB, 5}}) {
		t.Fatalf("second bounded drain %+v", second.families)
	}
	// A family deferred to a time joins the drain once that time has come.
	tracker.deferFamily(familyB, at.Add(time.Minute))
	if early := tracker.drain(at.Add(59*time.Second), 0); len(early.families) != 0 {
		t.Fatalf("a deferred family drained early: %+v", early.families)
	}
	tracker.deferFamily(familyB, at.Add(2*time.Minute)) // the earlier time stands
	if due := tracker.drain(at.Add(time.Minute), 0); fmt.Sprint(due.families) != fmt.Sprint([]familyChange{{familyB, 0}}) {
		t.Fatalf("a due family was not drained: %+v", due.families)
	}
	if after := tracker.drain(at.Add(time.Hour), 0); len(after.families) != 0 {
		t.Fatalf("a deferred family drained twice: %+v", after.families)
	}
}

// drain holds the mutex only to swap its maps out: while it builds and sorts
// what it took (here 200,000 devices, the bound), a request's Synced and
// Changed go straight through, and land in the next drain.
func TestDrainSortsOutsideTheLock(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	tracker := NewTracker(0)
	for i := range defaultMaxDevices {
		tracker.Synced(familyA, deviceN(i+1), 1, 1, at)
	}
	tracker.Changed(familyB, 3)
	var waited time.Duration
	tracker.afterTake = func() {
		done := make(chan time.Duration, 1)
		go func() {
			start := time.Now()
			tracker.Synced(familyB, deviceN(1), 2, 2, at)
			tracker.Changed(familyA, 4)
			done <- time.Since(start)
		}()
		select {
		case waited = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Synced waited for the drain: the mutex is held past the swap")
		}
	}
	work := tracker.drain(at, 0)
	tracker.afterTake = nil
	if len(work.devices) != defaultMaxDevices || len(work.families) != 1 || work.families[0].FamilyID != familyB {
		t.Fatalf("drained %d devices and %+v", len(work.devices), work.families)
	}
	for i := 1; i < len(work.devices); i++ {
		if work.devices[i-1].DeviceID >= work.devices[i].DeviceID {
			t.Fatalf("devices not sorted at %d", i)
		}
	}
	next := tracker.drain(at, 0)
	if len(next.devices) != 1 || next.devices[0].FamilyID != familyB || len(next.families) != 1 || next.families[0] != (familyChange{familyA, 4}) {
		t.Fatalf("the calls made during the drain: %+v", next)
	}
	t.Logf("Synced and Changed during a 200,000-device drain took %v", waited)
}

// A drain of an empty tracker takes nothing the requests still write to:
// progress recorded while it builds its slices lands in the next drain, once,
// and never in a map the drain is reading (found by Task 11's
// TestMaintenanceAndRequestsNeverDeadlock under -race: the empty maps were
// returned without being swapped out).
func TestDrainOfAnEmptyTrackerSharesNoMap(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	tracker := NewTracker(0)
	tracker.afterTake = func() {
		tracker.Synced(familyA, deviceN(1), 1, 1, at)
		tracker.Changed(familyA, 2)
	}
	if work := tracker.drain(at, 0); len(work.devices) != 0 || len(work.families) != 0 {
		t.Fatalf("an empty drain took what was recorded after its swap: %+v", work)
	}
	tracker.afterTake = nil
	next := tracker.drain(at, 0)
	if len(next.devices) != 1 || len(next.families) != 1 || next.families[0] != (familyChange{familyA, 2}) {
		t.Fatalf("the next drain: %+v, want the progress recorded during the empty one", next)
	}
	if again := tracker.drain(at, 0); len(again.devices) != 0 || len(again.families) != 0 {
		t.Fatalf("drained twice: %+v", again)
	}
}

// BenchmarkDrainLockHold is the time drain holds the mutex at the bound
// (the swap), against the whole drain (swap, copy and sort).
func BenchmarkDrainLockHold(b *testing.B) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var held, total time.Duration
	for range b.N {
		b.StopTimer()
		tracker := NewTracker(0)
		for i := range defaultMaxDevices {
			tracker.Synced(familyA, deviceN(i+1), 1, 1, at)
		}
		b.StartTimer()
		start := time.Now()
		tracker.afterTake = func() { held += time.Since(start) }
		tracker.drain(at, 0)
		total += time.Since(start)
	}
	b.ReportMetric(float64(held.Microseconds())/float64(b.N), "µs-locked/op")
	b.ReportMetric(float64(total.Microseconds())/float64(b.N), "µs-total/op")
}
