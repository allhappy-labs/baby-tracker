package maintenance_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/maintenance"
)

// fakeSweep is a Sweep whose Run is a function.
type fakeSweep struct {
	name  string
	run   func(ctx context.Context, now time.Time) (int, error)
	calls atomic.Int32
}

func (s *fakeSweep) Name() string { return s.name }
func (s *fakeSweep) Run(ctx context.Context, now time.Time) (int, error) {
	s.calls.Add(1)
	return s.run(ctx, now)
}

func counting(name string, n int) *fakeSweep {
	return &fakeSweep{name: name, run: func(context.Context, time.Time) (int, error) { return n, nil }}
}

// records is a slog handler that keeps every record.
type records struct {
	mu   sync.Mutex
	list []slog.Record
}

func (r *records) Enabled(context.Context, slog.Level) bool { return true }
func (r *records) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, record.Clone())
	return nil
}
func (r *records) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *records) WithGroup(string) slog.Handler      { return r }

// at returns the records at or above level.
func (r *records) at(level slog.Level) []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []slog.Record
	for _, record := range r.list {
		if record.Level >= level {
			out = append(out, record)
		}
	}
	return out
}

func attr(record slog.Record, key string) string {
	value := ""
	record.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			value = a.Value.String()
			return false
		}
		return true
	})
	return value
}

func TestLoopRunsOnlyWhenReady(t *testing.T) {
	sweep := counting("a", 1)
	var ready atomic.Bool
	loop := maintenance.Loop{Name: "test", Sweeps: []maintenance.Sweep{sweep}, Interval: 10 * time.Millisecond, Now: time.Now,
		Ready: func(context.Context) error {
			if ready.Load() {
				return nil
			}
			return errors.New("not ready")
		}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); loop.Run(ctx) }()
	time.Sleep(150 * time.Millisecond)
	if n := sweep.calls.Load(); n != 0 {
		t.Fatalf("%d runs while not ready", n)
	}
	ready.Store(true)
	deadline := time.Now().Add(5 * time.Second)
	for sweep.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if sweep.calls.Load() == 0 {
		t.Fatal("no run once ready")
	}
}

// A pass that falls due while the node is not ready runs as soon as it is,
// not one whole Interval later (an hourly roll must not slip an hour behind
// a startup that was briefly not ready).
func TestLoopRunsADuePassOnceReady(t *testing.T) {
	sweep := counting("a", 1)
	var ready atomic.Bool
	loop := maintenance.Loop{Name: "test", Sweeps: []maintenance.Sweep{sweep}, Interval: time.Hour, ReadyRetry: 10 * time.Millisecond, Now: time.Now,
		Ready: func(context.Context) error {
			if ready.Load() {
				return nil
			}
			return errors.New("not ready")
		}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); loop.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	ready.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for sweep.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if n := sweep.calls.Load(); n != 1 {
		t.Fatalf("%d runs once ready, want the due pass at once and no other within the hour", n)
	}
}

// A pass that fails is retried after FailureRetry, not a whole Interval
// later (a roll that hit 55P03 must not leave an hour of audit rows to the
// default partition).
func TestLoopRetriesAFailedPassSoon(t *testing.T) {
	var calls atomic.Int32
	sweep := &fakeSweep{name: "roll", run: func(context.Context, time.Time) (int, error) {
		if calls.Add(1) == 1 {
			return 0, errors.New("55P03")
		}
		return 1, nil
	}}
	loop := maintenance.Loop{Name: "test", Sweeps: []maintenance.Sweep{sweep}, Interval: time.Hour, FailureRetry: 20 * time.Millisecond, Now: time.Now,
		Logger: slog.New(&records{})}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	loop.Run(ctx)
	if n := calls.Load(); n != 2 {
		t.Fatalf("%d passes in 500ms, want the failed pass and its retry, then nothing within the hour", n)
	}
}

// Pass reports the failing sweeps' errors with every count.
func TestLoopPassReportsFailures(t *testing.T) {
	loop := maintenance.Loop{Name: "test", Interval: time.Second, Now: time.Now, Logger: slog.New(&records{}),
		Sweeps: []maintenance.Sweep{counting("a", 1), &fakeSweep{name: "b", run: func(context.Context, time.Time) (int, error) { return 2, errors.New("boom") }}}}
	counts, err := loop.Pass(t.Context())
	if err == nil || counts["a"] != 1 || counts["b"] != 2 {
		t.Fatalf("pass %v (%v), want both counts and b's error", counts, err)
	}
	if counts, err := (maintenance.Loop{Name: "ok", Interval: time.Second, Sweeps: []maintenance.Sweep{counting("a", 1)}}).Pass(t.Context()); err != nil || counts["a"] != 1 {
		t.Fatalf("a clean pass: %v (%v)", counts, err)
	}
}

// The first pass waits Offset (staggering loops that share a pool), then
// passes follow every Interval.
func TestLoopStartsAfterItsOffset(t *testing.T) {
	sweep := counting("a", 1)
	var first atomic.Int64
	started := time.Now()
	sweep.run = func(context.Context, time.Time) (int, error) {
		first.CompareAndSwap(0, int64(time.Since(started)))
		return 1, nil
	}
	loop := maintenance.Loop{Name: "test", Sweeps: []maintenance.Sweep{sweep}, Interval: 20 * time.Millisecond, Offset: 200 * time.Millisecond, Now: time.Now}
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	loop.Run(ctx)
	if at := time.Duration(first.Load()); at < 200*time.Millisecond {
		t.Fatalf("first pass after %v, want at least the 200ms offset", at)
	}
	if n := sweep.calls.Load(); n < 3 {
		t.Fatalf("%d passes in the 200ms after the offset at a 20ms interval", n)
	}
}

func TestLoopIsolatesAFailingSweep(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	var seen []time.Time
	var mu sync.Mutex
	record := func(n int, err error) func(context.Context, time.Time) (int, error) {
		return func(_ context.Context, at time.Time) (int, error) {
			mu.Lock()
			seen = append(seen, at)
			mu.Unlock()
			return n, err
		}
	}
	a := &fakeSweep{name: "a", run: record(2, nil)}
	b := &fakeSweep{name: "b", run: record(0, errors.New("boom"))}
	c := &fakeSweep{name: "c", run: record(3, nil)}
	log := &records{}
	loop := maintenance.Loop{Name: "retention", Sweeps: []maintenance.Sweep{a, b, c}, Interval: time.Second, Now: func() time.Time { return now },
		Logger: slog.New(log)}
	got := loop.Once(t.Context())
	if got["a"] != 2 || got["c"] != 3 || c.calls.Load() != 1 {
		t.Fatalf("pass %v (c ran %d times): a failing sweep stopped the others", got, c.calls.Load())
	}
	failures := log.at(slog.LevelWarn)
	if len(failures) != 1 || attr(failures[0], "sweep") != "b" || attr(failures[0], "loop") != "retention" {
		t.Fatalf("logged %d warnings (%v), want one naming sweep b", len(failures), failures)
	}
	if len(seen) != 3 || !seen[0].Equal(now) || !seen[1].Equal(now) || !seen[2].Equal(now) {
		t.Fatalf("sweeps saw %v, want the loop clock %s", seen, now)
	}
}

func TestLoopBoundsEachSweep(t *testing.T) {
	blocking := &fakeSweep{name: "blocking", run: func(ctx context.Context, _ time.Time) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}}
	after := counting("after", 1)
	loop := maintenance.Loop{Name: "test", Sweeps: []maintenance.Sweep{blocking, after}, Interval: time.Second, Timeout: 50 * time.Millisecond, Now: time.Now,
		Logger: slog.New(&records{})}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second) // an unbounded sweep fails here, not at the binary's timeout
	defer cancel()
	start := time.Now()
	got := loop.Once(ctx)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a blocking sweep held the pass for %v, want its 50ms timeout", elapsed)
	}
	if got["after"] != 1 {
		t.Fatalf("the sweep after a timed-out one did not run: %v", got)
	}
	// Timeout 0 means 30 s.
	var deadline time.Time
	probe := &fakeSweep{name: "probe", run: func(ctx context.Context, _ time.Time) (int, error) {
		deadline, _ = ctx.Deadline()
		return 0, nil
	}}
	start = time.Now()
	maintenance.Loop{Name: "test", Sweeps: []maintenance.Sweep{probe}, Interval: time.Second, Now: time.Now}.Once(t.Context())
	if bound := deadline.Sub(start); bound < 29*time.Second || bound > 31*time.Second {
		t.Fatalf("default per-sweep bound %v, want 30s", bound)
	}
}
