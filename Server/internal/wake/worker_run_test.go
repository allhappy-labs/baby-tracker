package wake_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/wake"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recordedLog is one log record: its message and attributes.
type recordedLog struct {
	message string
	attrs   map[string]string
}

// logRecorder is a slog handler keeping every record.
type logRecorder struct {
	mu      sync.Mutex
	records []recordedLog
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *logRecorder) WithGroup(string) slog.Handler            { return r }
func (r *logRecorder) Handle(_ context.Context, record slog.Record) error {
	attrs := map[string]string{}
	record.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.String(); return true })
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, recordedLog{record.Message, attrs})
	return nil
}

func (r *logRecorder) with(message string) []recordedLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []recordedLog
	for _, record := range r.records {
		if record.message == message {
			result = append(result, record)
		}
	}
	return result
}

// waitFor polls condition for up to 5 s.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// runWorker runs w until the test ends.
func runWorker(t *testing.T, w *wake.Worker, ready func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx, ready) }()
	t.Cleanup(func() { cancel(); <-done })
}

// Run ticks only while the node is ready: no tick (no flush) while
// readiness refuses, and the work recorded meanwhile is flushed once it
// allows.
func TestWakeRunTicksOnlyWhileReady(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker := wake.NewTracker(0)
	tracker.Synced(h.FamilyID, device(1), 1, 1, h.Now)
	var allowed atomic.Bool
	var asked atomic.Int32
	ready := func(context.Context) error {
		asked.Add(1)
		if !allowed.Load() {
			return errors.New("not ready")
		}
		return nil
	}
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: &recordingSender{}, Now: func() time.Time { return h.Now },
		Interval: 10 * time.Millisecond}
	runWorker(t, w, ready)
	waitFor(t, "readiness to be asked five times", func() bool { return asked.Load() >= 5 })
	if rows := cursorRows(t, h); len(rows) != 0 {
		t.Fatalf("ticked while not ready: %+v", rows)
	}
	allowed.Store(true)
	waitFor(t, "the flush once ready", func() bool { return len(cursorRows(t, h)) == 1 })
}

// A failed tick is logged by class only: a database failure as "tick
// failed" with its SQLSTATE and timeout flag, a send failure apart, never
// the error's text. Progress the tracker dropped is logged at most once a
// minute.
func TestWakeRunLogsFailuresAndDrops(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	var clockMu sync.Mutex
	clock := h.Now
	now := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	advance := func(d time.Duration) { clockMu.Lock(); defer clockMu.Unlock(); clock = clock.Add(d) }

	closed, err := pgxpool.NewWithConfig(t.Context(), h.Maintenance.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	brokenLog := &logRecorder{}
	broken := wake.NewTracker(1)
	broken.Synced(h.FamilyID, device(1), 0, 0, h.Now)
	runWorker(t, &wake.Worker{Pool: closed, Tracker: broken, Sender: &recordingSender{}, Logger: slog.New(brokenLog), Now: now,
		Interval: 10 * time.Millisecond}, nil)
	waitFor(t, "a tick failure", func() bool { return len(brokenLog.with("wake worker tick failed")) > 0 })
	record := brokenLog.with("wake worker tick failed")[0]
	if len(record.attrs) != 2 || record.attrs["sqlstate"] != "" || record.attrs["timeout"] != "false" {
		t.Fatalf("tick failure logged %+v, want only sqlstate and timeout", record.attrs)
	}
	if n := len(brokenLog.with("wake worker send failed")); n != 0 {
		t.Fatalf("a database failure logged as a send failure %d times", n)
	}

	sendLog := &logRecorder{}
	tracker := wake.NewTracker(1)
	tracker.Synced(h.FamilyID, device(2), 0, 0, h.Now)
	tracker.Changed(h.FamilyID, h.FamilySequence(t))
	tracker.Synced(h.FamilyID, device(3), 0, 0, h.Now) // past the bound of 1: dropped
	tracker.Synced(h.FamilyID, device(4), 0, 0, h.Now) // dropped
	runWorker(t, &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: failingSender{}, Logger: slog.New(sendLog), Now: now,
		Interval: 10 * time.Millisecond}, nil)
	waitFor(t, "a send failure", func() bool { return len(sendLog.with("wake worker send failed")) > 0 })
	if record := sendLog.with("wake worker send failed")[0]; len(record.attrs) != 2 || record.attrs["sqlstate"] != "" {
		t.Fatalf("send failure logged %+v", record.attrs)
	}
	if n := len(sendLog.with("wake worker tick failed")); n != 0 {
		t.Fatalf("a send failure logged as a tick failure %d times", n)
	}
	drops := sendLog.with("wake tracker dropped progress")
	if len(drops) != 1 || drops[0].attrs["dropped_total"] != "2" || drops[0].attrs["dropped_since_last_log"] != "2" {
		t.Fatalf("dropped progress logged %+v", drops)
	}
	// More drops inside the minute are not logged; after it, once.
	tracker.Synced(h.FamilyID, "not-a-uuid", 0, 0, h.Now)
	time.Sleep(50 * time.Millisecond)
	if n := len(sendLog.with("wake tracker dropped progress")); n != 1 {
		t.Fatalf("dropped progress logged %d times inside a minute", n)
	}
	advance(time.Minute)
	waitFor(t, "the next drop log", func() bool { return len(sendLog.with("wake tracker dropped progress")) == 2 })
	if latest := sendLog.with("wake tracker dropped progress")[1]; latest.attrs["dropped_total"] != "3" || latest.attrs["dropped_since_last_log"] != "1" {
		t.Fatalf("second drop log %+v", latest.attrs)
	}
}

type failingSender struct{}

func (failingSender) Wake(context.Context, []wake.Wake) error { return errors.New("hub refused") }

// A lagging device of a family that is no longer active is never woken: the
// lag scan reads only active families.
func TestLagScanSkipsInactiveFamilies(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, sender := wake.NewTracker(0), &recordingSender{}
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: sender, Now: func() time.Time { return h.Now }}
	seq := h.FamilySequence(t)
	tracker.Synced(h.FamilyID, device(1), seq, seq, h.Now)
	tick(t, w) // the row, at the high water; the backstop finishes
	caretest.Seed(t, h, caretest.Feed(caretest.BabyA, 1))
	if _, err := h.Family.ScheduleDeletion(t.Context(), h.Owner, h.CommandID(), true); err != nil {
		t.Fatal(err)
	}
	tracker.Changed(h.FamilyID, h.FamilySequence(t))
	if stats := tick(t, w); stats.Woken != 0 || sender.callCount() != 0 {
		t.Fatalf("a deletion-pending family's device was woken: %+v %+v", stats, sender.calls)
	}
}

// Progress the tracker recorded after the last tick is flushed when Run
// stops, before it returns (C42): a graceful restart loses none. The stop
// logs one line with the devices it wrote.
func TestRunFlushesTrackerProgressOnStop(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker := wake.NewTracker(0)
	logs := &logRecorder{}
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: &recordingSender{}, Now: func() time.Time { return h.Now },
		Logger: slog.New(logs), Interval: time.Hour}
	seq := uint64(h.FamilySequence(t))
	tracker.Synced(h.FamilyID, device(1), seq, seq, h.Now)
	tick(t, w) // the last tick: device 1's row
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx, nil) }()
	for n := 2; n <= 4; n++ {
		tracker.Synced(h.FamilyID, device(n), seq, seq, h.Now)
	}
	if rows := cursorRows(t, h); len(rows) != 1 {
		t.Fatalf("rows before the stop %+v, want only the last tick's", rows)
	}
	cancel()
	<-done
	rows := cursorRows(t, h)
	for n := 2; n <= 4; n++ {
		if row, ok := rows[h.FamilyID+"/"+device(n)]; !ok || row.Acked != int64(seq) || row.Delivered != int64(seq) {
			t.Fatalf("device %d after the stop: %+v (%v), want its progress written before Run returned", n, row, ok)
		}
	}
	if tracker.Len() != 0 {
		t.Fatalf("the tracker still holds %d entries after the stop flush", tracker.Len())
	}
	if lines := logs.with("wake progress flushed at stop"); len(lines) != 1 || lines[0].attrs["devices"] != "3" {
		t.Fatalf("stop logs %+v, want one line naming 3 devices", lines)
	}
}

// The server's final flush (X-1): progress the draining requests record
// after Run's own stop flush is written by FlushTracker once Run returned,
// as main's deferred stop does after the HTTP drain. Nothing to write
// writes and logs nothing; a nil worker is a no-op.
func TestFlushTrackerWritesProgressRecordedAfterRunStopped(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker := wake.NewTracker(0)
	logs := &logRecorder{}
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: &recordingSender{}, Now: func() time.Time { return h.Now },
		Logger: slog.New(logs), Interval: time.Hour}
	seq := uint64(h.FamilySequence(t))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx, nil) }()
	tracker.Synced(h.FamilyID, device(1), seq, seq, h.Now)
	cancel()
	<-done
	if rows := cursorRows(t, h); len(rows) != 1 {
		t.Fatalf("rows after Run's stop %+v, want device 1's", rows)
	}
	// The drain: requests still record progress after the worker stopped.
	tracker.Synced(h.FamilyID, device(2), seq, seq, h.Now)
	tracker.Synced(h.FamilyID, device(3), seq, seq, h.Now)
	if rows := cursorRows(t, h); len(rows) != 1 {
		t.Fatalf("rows before the final flush %+v", rows)
	}
	w.FlushTracker(context.Background())
	rows := cursorRows(t, h)
	for n := 1; n <= 3; n++ {
		if row, ok := rows[h.FamilyID+"/"+device(n)]; !ok || row.Acked != int64(seq) {
			t.Fatalf("device %d after the final flush: %+v (%v)", n, row, ok)
		}
	}
	lines := logs.with("wake progress flushed at stop")
	if len(lines) != 2 || lines[0].attrs["devices"] != "1" || lines[1].attrs["devices"] != "2" {
		t.Fatalf("flush logs %+v, want Run's (1 device) and the final one (2)", lines)
	}
	w.FlushTracker(context.Background())
	if got := len(logs.with("wake progress flushed at stop")); got != 2 {
		t.Fatalf("an empty flush logged (%d lines)", got)
	}
	var none *wake.Worker
	none.FlushTracker(context.Background())
}

// Run's stop applies the kept settles first and flushes the tracker after
// (the plan's order): on the wire the settle's release precedes the flush.
func TestRunAppliesKeptSettlesBeforeItsStopFlush(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	pool, wire := testkit.CountedPool(t, h.Maintenance)
	tracker := wake.NewTracker(0)
	w := &wake.Worker{Pool: pool, Tracker: tracker, Sender: &recordingSender{}, Now: func() time.Time { return h.Now }, Interval: time.Hour}
	seq := uint64(h.FamilySequence(t))
	w.KeepReleaseForTest(h.FamilyID, []string{device(9)}, seq, h.Now)
	tracker.Synced(h.FamilyID, device(1), seq, seq, h.Now)
	wire.Reset()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx, nil) }()
	cancel()
	<-done
	release, flush := -1, -1
	for i, statement := range wire.Statements() {
		switch {
		case release < 0 && strings.Contains(statement, "SET pushed_sequence = v.ps, pushed_at = v.pa"):
			release = i
		case flush < 0 && strings.Contains(statement, "INSERT INTO device_sync_cursors"):
			flush = i
		}
	}
	if release < 0 || flush < 0 || release > flush {
		t.Fatalf("release at %d, flush at %d in %q: want the kept settle applied before the stop flush", release, flush, wire.Statements())
	}
	if totals := w.Totals(); totals.SettlesPending != 0 {
		t.Fatalf("totals %+v, want the settle applied", totals)
	}
}
