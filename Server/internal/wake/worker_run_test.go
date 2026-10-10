package wake_test

import (
	"context"
	"errors"
	"log/slog"
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
