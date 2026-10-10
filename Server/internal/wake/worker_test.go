package wake_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/database"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/wake"
	"github.com/jackc/pgx/v5/pgxpool"
)

type recordingSender struct {
	mu    sync.Mutex
	calls [][]wake.Wake
}

func (s *recordingSender) Wake(_ context.Context, wakes []wake.Wake) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, wakes)
	return nil
}

func (s *recordingSender) woken() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	for _, call := range s.calls {
		for _, w := range call {
			for _, d := range w.DeviceIDs {
				counts[d]++
			}
		}
	}
	return counts
}

func (s *recordingSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// last is the last call's wakes.
func (s *recordingSender) last() []wake.Wake {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return nil
	}
	return s.calls[len(s.calls)-1]
}

func device(n int) string { return fmt.Sprintf("d1000000-0000-4000-8000-%012d", n) }

// writtenFamily is the outsider's own family, with one write.
func writtenFamily(t *testing.T, h *testkit.Harness) string {
	t.Helper()
	created, err := h.Family.Create(t.Context(), h.Outsider, h.CommandID(), "B")
	if err != nil {
		t.Fatal(err)
	}
	outsider := h.Refresh("outsider")
	request := h.SyncRequest(nil, nil)
	request.Mutations = []protocol.Mutation{caretest.Baby("11000000-0000-4000-8000-0000000000b2")}
	if _, err := h.Service.ApplyAndPull(t.Context(), outsider, request); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

type cursorRow struct {
	Acked, Delivered, Woken int64
	Seen                    time.Time
	WokenAt                 *time.Time
	Xmin                    string
}

// cursorRows is device_sync_cursors keyed "family/device".
func cursorRows(t *testing.T, h *testkit.Harness) map[string]cursorRow {
	t.Helper()
	rows, err := h.Maintenance.Query(t.Context(), `SELECT family_id::text, device_id::text, acked_sequence, delivered_sequence, woken_sequence,
		seen_at, woken_at, xmin::text FROM device_sync_cursors`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[string]cursorRow{}
	for rows.Next() {
		var family, dev string
		var row cursorRow
		if err := rows.Scan(&family, &dev, &row.Acked, &row.Delivered, &row.Woken, &row.Seen, &row.WokenAt, &row.Xmin); err != nil {
			t.Fatal(err)
		}
		result[family+"/"+dev] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func tick(t *testing.T, w *wake.Worker) wake.Stats {
	t.Helper()
	stats, err := w.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func TestWakeWorkerNeverStorms(t *testing.T) { // Review Focus 5
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, sender := wake.NewTracker(0), &recordingSender{}
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: sender, Now: func() time.Time { return clock }}
	devices := []string{"d1000000-0000-4000-8000-000000000001", "d1000000-0000-4000-8000-000000000002",
		"d1000000-0000-4000-8000-000000000003", "d1000000-0000-4000-8000-000000000004"}
	seq := h.FamilySequence(t)
	for _, d := range devices {
		tracker.Synced(h.FamilyID, d, seq, seq, clock)
	}
	if _, err := w.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := range 100 { // 100 writes over 50 s
		caretest.Seed(t, h, caretest.Feed(caretest.BabyA, i))
		tracker.Changed(h.FamilyID, h.FamilySequence(t))
		clock = clock.Add(500 * time.Millisecond)
		stats, err := w.Tick(t.Context())
		if err != nil || stats.Woken > 500 {
			t.Fatalf("tick %d: %+v %v", i, stats, err)
		}
	}
	for _, d := range devices {
		if sender.woken()[d] != 1 {
			t.Fatalf("device %s woken %d times in 50 s, want once", d, sender.woken()[d])
		}
	}
	if len(sender.calls) != 1 || len(sender.calls[0]) != 1 {
		t.Fatalf("hub told %d times (%+v), want one family wake", len(sender.calls), sender.calls)
	}
	// A minute on, the three devices still behind are woken again; the one
	// that pulled to the high water is not.
	high := h.FamilySequence(t)
	tracker.Synced(h.FamilyID, devices[0], high, high, clock)
	tracker.Changed(h.FamilyID, high)
	clock = clock.Add(time.Minute)
	if _, err := w.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	counts := sender.woken()
	if counts[devices[0]] != 1 || counts[devices[1]] != 2 || counts[devices[2]] != 2 || counts[devices[3]] != 2 || len(sender.calls) != 2 {
		t.Fatalf("second round %+v over %d calls; want the caught-up device left alone", counts, len(sender.calls))
	}
	if last := sender.last(); len(last) != 1 || last[0].FamilyID != h.FamilyID || last[0].Sequence != high {
		t.Fatalf("second round announced %+v, want the family's high water %d", last, high)
	}
}

// A tick wakes at most MaxWakesPerTick devices and costs at most four round
// trips (BEGIN, the flush and scans in one batch, the mark, COMMIT); the
// rest of a large backlog is woken by the next ticks, each device once.
func TestWakeTickIsBoundedAndPinsItsRoundTrips(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	familyB := writtenFamily(t, h)
	pool, wire := testkit.CountedPool(t, h.Maintenance)
	tracker, sender := wake.NewTracker(0), &recordingSender{}
	clock := h.Now
	w := &wake.Worker{Pool: pool, Tracker: tracker, Sender: sender, Now: func() time.Time { return clock }}
	// Warm the connection's statement cache with one lagging device (flush,
	// scans and mark), and finish the startup backstop.
	tracker.Synced(h.FamilyID, device(0), 0, 0, clock)
	tracker.Changed(h.FamilyID, h.FamilySequence(t))
	if stats := tick(t, w); stats.Woken != 1 {
		t.Fatalf("warm tick %+v", stats)
	}
	clock = clock.Add(time.Second)
	for i := 1; i <= 1500; i++ {
		family := h.FamilyID
		if i%2 == 0 {
			family = familyB
		}
		tracker.Synced(family, device(i), 0, 0, clock)
	}
	tracker.Changed(h.FamilyID, h.FamilySequence(t))
	tracker.Changed(familyB, 1)
	wire.Reset()
	stats := tick(t, w)
	if stats.Woken != 500 || stats.Flushed != 1500 {
		t.Fatalf("first tick %+v, want 1,500 flushed and 500 woken", stats)
	}
	if trips := wire.Trips(); trips > 4 {
		t.Fatalf("tick took %d round trips, want at most 4: %q", trips, wire.Statements())
	}
	total := stats.Woken
	for i := range 3 {
		stats := tick(t, w)
		if stats.Woken > 500 {
			t.Fatalf("tick %d woke %d", i+2, stats.Woken)
		}
		total += stats.Woken
	}
	counts := sender.woken()
	if total != 1500 || len(counts) != 1501 {
		t.Fatalf("woke %d over four ticks (%d devices), want the 1,500 backlog", total, len(counts))
	}
	for d, n := range counts {
		if n != 1 {
			t.Fatalf("device %s woken %d times", d, n)
		}
	}
	for _, call := range sender.calls {
		woken := 0
		for _, w := range call {
			woken += len(w.DeviceIDs)
		}
		if woken > 500 {
			t.Fatalf("one call woke %d", woken)
		}
	}
	// An idle tick costs nothing.
	wire.Reset()
	if stats := tick(t, w); stats != (wake.Stats{}) || wire.Trips() != 0 {
		t.Fatalf("idle tick %+v took %d round trips", stats, wire.Trips())
	}
}

// A failed tick puts back everything it drained: the next tick flushes every
// device recorded before the failure (newer values win) and scans its
// changed families.
func TestWakeTickRollbackRequeuesProgress(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	seq := h.FamilySequence(t)
	closed, err := pgxpool.NewWithConfig(t.Context(), h.Maintenance.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	tracker, sender := wake.NewTracker(0), &recordingSender{}
	clock := h.Now
	tracker.Synced(h.FamilyID, device(1), seq, seq, clock)
	tracker.Synced(h.FamilyID, device(2), 0, 0, clock)
	tracker.Synced(h.FamilyID, device(3), seq, 0, clock)
	tracker.Changed(h.FamilyID, seq)
	broken := &wake.Worker{Pool: closed, Tracker: tracker, Sender: sender, Now: func() time.Time { return clock }}
	if _, err := broken.Tick(t.Context()); err == nil {
		t.Fatal("a tick over a closed pool succeeded")
	}
	tracker.Synced(h.FamilyID, device(3), 0, 0, clock.Add(time.Second)) // an older request finishing after the failure
	live := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: sender, Now: func() time.Time { return clock }}
	if stats := tick(t, live); stats.Flushed != 3 || stats.Woken != 1 {
		t.Fatalf("live tick %+v, want 3 flushed and the lagging device woken", stats)
	}
	rows := cursorRows(t, h)
	want := map[string][2]int64{device(1): {int64(seq), int64(seq)}, device(2): {0, 0}, device(3): {int64(seq), 0}}
	if len(rows) != len(want) {
		t.Fatalf("rows %+v", rows)
	}
	for d, values := range want {
		row := rows[h.FamilyID+"/"+d]
		if row.Acked != values[0] || row.Delivered != values[1] {
			t.Fatalf("device %s row %+v, want acked/delivered %v", d, row, values)
		}
	}
	if counts := sender.woken(); len(counts) != 1 || counts[device(2)] != 1 {
		t.Fatalf("woken %+v, want the device behind", counts)
	}
}

// After a restart the tracker is empty and no Changed call names the
// family, so the startup backstop pages through device_sync_cursors in key
// order (one page per tick): a device behind and never woken is woken at
// once; a device at the high water is not; a device woken inside the window
// before the restart is not woken again inside it, and is woken once after
// it for the newer sequence; nobody is woken twice for one sequence.
func TestWakeWorkerBackstopWakesLaggingDevicesAfterRestart(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	clock := h.Now
	now := func() time.Time { return clock }
	seq0 := h.FamilySequence(t)
	before := wake.NewTracker(0)
	first := &wake.Worker{Pool: h.Maintenance, Tracker: before, Sender: &recordingSender{}, Now: now}
	for i := 1; i <= 5; i++ {
		before.Synced(h.FamilyID, device(i), seq0, seq0, clock)
	}
	tick(t, first)
	caretest.Seed(t, h, caretest.Feed(caretest.BabyA, 1))
	seq1 := h.FamilySequence(t)
	before.Synced(h.FamilyID, device(3), seq1, seq1, clock) // device 3 pulls seq1 before any wake
	tick(t, first)
	before.Changed(h.FamilyID, seq1)
	if stats := tick(t, first); stats.Woken != 4 {
		t.Fatalf("pre-restart wake %+v, want devices 1, 2, 4 and 5", stats)
	}
	before.Synced(h.FamilyID, device(4), seq1, seq1, clock) // device 4 pulls after its wake
	tick(t, first)
	// A write the old process never ticked for: everyone is behind seq2
	// except device 6, which pulled it.
	caretest.Seed(t, h, caretest.Feed(caretest.BabyA, 2))
	seq2 := h.FamilySequence(t)
	before.Synced(h.FamilyID, device(6), seq2, seq2, clock)
	tick(t, first)

	// The restart, ten seconds after the pre-restart wake.
	clock = clock.Add(10 * time.Second)
	after := &recordingSender{}
	restarted := &wake.Worker{Pool: h.Maintenance, Tracker: wake.NewTracker(0), Sender: after, Now: now, MaxWakesPerTick: 1}
	for range 8 {
		tick(t, restarted)
	}
	if counts := after.woken(); len(counts) != 1 || counts[device(3)] != 1 {
		t.Fatalf("inside the window the backstop woke %+v, want only device 3 (behind, never woken)", counts)
	}
	// A minute after the pre-restart wake, the devices it covered are woken
	// once more, for seq2, one per tick; device 6 never is.
	clock = h.Now.Add(time.Minute)
	for range 8 {
		tick(t, restarted)
	}
	counts := after.woken()
	for _, d := range []int{1, 2, 3, 4, 5} {
		if counts[device(d)] != 1 {
			t.Fatalf("after the window: %+v, want devices 1 to 5 once each", counts)
		}
	}
	if counts[device(6)] != 0 {
		t.Fatalf("the device at the high water was woken: %+v", counts)
	}
	for _, call := range after.calls {
		for _, w := range call {
			if w.Sequence != seq2 || len(w.DeviceIDs) != 1 {
				t.Fatalf("restart wake %+v, want one device per tick at %d", w, seq2)
			}
		}
	}
	// Nothing is behind any more: further ticks wake no one.
	clock = clock.Add(time.Hour)
	for range 3 {
		tick(t, restarted)
	}
	if after.callCount() != 5 {
		t.Fatalf("%d calls, want 5", after.callCount())
	}
}

// Progress of a family that is not active (deletion pending) is dropped by
// the flush.
func TestFlushSkipsInactiveFamilies(t *testing.T) {
	h := testkit.New(t)
	familyB := writtenFamily(t, h)
	if _, err := h.Family.ScheduleDeletion(t.Context(), h.Owner, h.CommandID(), true); err != nil {
		t.Fatal(err)
	}
	tracker := wake.NewTracker(0)
	tracker.Synced(h.FamilyID, device(1), 0, 0, h.Now)
	tracker.Synced(familyB, device(2), 1, 1, h.Now)
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: &recordingSender{}, Now: func() time.Time { return h.Now }}
	if stats := tick(t, w); stats.Flushed != 1 {
		t.Fatalf("flushed %+v, want the active family's device only", stats)
	}
	rows := cursorRows(t, h)
	if _, ok := rows[familyB+"/"+device(2)]; !ok || len(rows) != 1 {
		t.Fatalf("rows %+v, want only the active family's device", rows)
	}
}

// Debounced, not dropped: a change inside a device's window is announced
// once the window ends, without another write.
func TestWakeWorkerWakesTheTrailingChangeAfterTheWindow(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, sender := wake.NewTracker(0), &recordingSender{}
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: sender, Now: func() time.Time { return clock }}
	seq := h.FamilySequence(t)
	tracker.Synced(h.FamilyID, device(1), seq, seq, clock)
	tick(t, w)
	caretest.Seed(t, h, caretest.Feed(caretest.BabyA, 1))
	tracker.Changed(h.FamilyID, h.FamilySequence(t))
	clock = clock.Add(time.Second)
	tick(t, w)
	wokenAt := clock
	caretest.Seed(t, h, caretest.Feed(caretest.BabyA, 2))
	trailing := h.FamilySequence(t)
	tracker.Changed(h.FamilyID, trailing)
	clock = clock.Add(10 * time.Second)
	tick(t, w)
	clock = wokenAt.Add(59 * time.Second)
	tick(t, w)
	if n := sender.woken()[device(1)]; n != 1 {
		t.Fatalf("woken %d times inside the window", n)
	}
	clock = wokenAt.Add(time.Minute)
	tick(t, w)
	if n := sender.woken()[device(1)]; n != 2 || sender.last()[0].Sequence != trailing {
		t.Fatalf("the trailing change: woken %d times, last %+v", n, sender.last())
	}
	// Once per sequence: the family announced again at the same sequence,
	// an hour on, wakes nobody.
	clock = clock.Add(time.Hour)
	tracker.Changed(h.FamilyID, trailing)
	tick(t, w)
	if n := sender.woken()[device(1)]; n != 2 {
		t.Fatalf("woken %d times for one trailing change", n)
	}
}

// The flush rewrites a row only when a cursor advanced, and seen_at (the
// one indexed column the flush writes) at most once a day, so the worker's
// updates stay heap-only (Task 2 M3).
func TestFlushAdvancesSeenAtAtMostDaily(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker := wake.NewTracker(0)
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: &recordingSender{}, Now: func() time.Time { return clock }}
	key := h.FamilyID + "/" + device(1)
	tracker.Synced(h.FamilyID, device(1), 1, 1, h.Now)
	tick(t, w)
	first := cursorRows(t, h)[key]
	if !first.Seen.Equal(h.Now) {
		t.Fatalf("first flush %+v", first)
	}
	tracker.Synced(h.FamilyID, device(1), 1, 1, h.Now.Add(time.Hour)) // nothing advanced
	if stats := tick(t, w); stats.Flushed != 0 || cursorRows(t, h)[key].Xmin != first.Xmin {
		t.Fatalf("an unchanged device was rewritten: %+v", stats)
	}
	tracker.Synced(h.FamilyID, device(1), 2, 1, h.Now.Add(23*time.Hour))
	tick(t, w)
	if row := cursorRows(t, h)[key]; row.Acked != 2 || !row.Seen.Equal(h.Now) {
		t.Fatalf("within a day: %+v, want acked 2 and seen_at unchanged", row)
	}
	tracker.Synced(h.FamilyID, device(1), 2, 1, h.Now.Add(25*time.Hour))
	if stats := tick(t, w); stats.Flushed != 1 {
		t.Fatalf("a day on: %+v", stats)
	}
	if row := cursorRows(t, h)[key]; !row.Seen.Equal(h.Now.Add(25 * time.Hour)) {
		t.Fatalf("a day on: %+v, want seen_at advanced", row)
	}
}

// A request holding its family row (FOR NO KEY UPDATE, the request lock)
// never makes the worker wait: the worker locks no family row, and its
// inserts' key-share check does not conflict with the request's lock.
func TestWakeTickNeverWaitsOnARequestHoldingTheFamilyRow(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, sender := wake.NewTracker(0), &recordingSender{}
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: sender, Now: func() time.Time { return h.Now }}
	tracker.Synced(h.FamilyID, device(1), 0, 0, h.Now)
	tick(t, w)
	tx, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority', '1', true)`); err != nil {
		t.Fatal(err)
	}
	var locked int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM (SELECT 1 FROM families WHERE id = $1 FOR NO KEY UPDATE) f`, h.FamilyID).Scan(&locked); err != nil || locked != 1 {
		t.Fatalf("lock the family row: %d %v", locked, err)
	}
	tracker.Synced(h.FamilyID, device(1), 1, 0, h.Now) // an update, to the high water
	tracker.Synced(h.FamilyID, device(2), 0, 0, h.Now) // an insert, behind: woken and marked
	tracker.Changed(h.FamilyID, h.FamilySequence(t))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	stats, err := w.Tick(ctx)
	if err != nil || stats.Flushed != 2 || stats.Woken != 1 {
		t.Fatalf("tick beside a held family row: %+v %v", stats, err)
	}
}

// Every tick that does work runs inside a maintenance worker slot, so the
// readiness audit keeps its connection; a tick that cannot get one puts its
// work back. An idle tick takes no slot.
func TestWakeTickRunsInsideAWorkerSlot(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	config := h.Maintenance.Config().Copy()
	config.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	slots, err := database.NewWorkerSlots(pool)
	if err != nil {
		t.Fatal(err)
	}
	tracker, sender := wake.NewTracker(0), &recordingSender{}
	w := &wake.Worker{Pool: pool, Slots: slots, Tracker: tracker, Sender: sender, Now: func() time.Time { return h.Now }}
	tick(t, w) // finishes the startup backstop
	release, err := slots.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats := tick(t, w); stats != (wake.Stats{}) {
		t.Fatalf("idle tick %+v", stats)
	}
	tracker.Synced(h.FamilyID, device(1), 0, 0, h.Now)
	tracker.Changed(h.FamilyID, h.FamilySequence(t))
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := w.Tick(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a tick without a free slot: %v", err)
	}
	if rows := cursorRows(t, h); len(rows) != 0 {
		t.Fatalf("a tick without a slot wrote %+v", rows)
	}
	release()
	if stats := tick(t, w); stats.Flushed != 1 || stats.Woken != 1 {
		t.Fatalf("the tick after the slot freed %+v", stats)
	}
}

// Wakes are one per family, in family order, each naming its devices in
// order with the family's high water.
func TestWakesAreOnePerFamily(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	familyB := writtenFamily(t, h)
	tracker, sender := wake.NewTracker(0), &recordingSender{}
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: sender, Now: func() time.Time { return h.Now }}
	for i := 4; i >= 1; i-- {
		family := h.FamilyID
		if i > 2 {
			family = familyB
		}
		tracker.Synced(family, device(i), 0, 0, h.Now)
		tracker.Changed(family, 1)
	}
	tick(t, w)
	families := []string{h.FamilyID, familyB}
	sort.Strings(families)
	wants := map[string]wake.Wake{
		h.FamilyID: {FamilyID: h.FamilyID, DeviceIDs: []string{device(1), device(2)}, Sequence: h.FamilySequence(t)},
		familyB:    {FamilyID: familyB, DeviceIDs: []string{device(3), device(4)}, Sequence: 1},
	}
	if sender.callCount() != 1 || fmt.Sprint(sender.last()) != fmt.Sprint([]wake.Wake{wants[families[0]], wants[families[1]]}) {
		t.Fatalf("wakes %+v", sender.calls)
	}
}
