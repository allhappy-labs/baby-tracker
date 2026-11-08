package loadtest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is virtual time: SleepUntil jumps straight to the deadline (plus
// oversleep, a scheduler that always wakes late), so a whole schedule runs
// instantly and every timestamp is exact.
type fakeClock struct {
	mu        sync.Mutex
	now       time.Time
	oversleep time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *fakeClock) SleepUntil(ctx context.Context, t time.Time) bool {
	if ctx.Err() != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.now) {
		c.now = t
	}
	c.now = c.now.Add(c.oversleep)
	return true
}

// runVirtual runs the schedule in virtual time. Run must return without any
// operation finishing: one that waits for an operation (a closed loop) hangs
// on a stalled server, and is reported as such instead of timing out the
// package.
func runVirtual(t *testing.T, d *Dispatcher, a *Arrivals, until time.Duration, op func(context.Context, time.Time)) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(t.Context(), a, until, op)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("Run did not return while the server stalled: the dispatcher waited for an operation (launched %d)", d.Launched())
	}
}

func offsetsBefore(rate float64, seed uint64, until time.Duration) []time.Duration {
	var out []time.Duration
	a := NewArrivals(rate, seed)
	for {
		at, ok := a.Next()
		if !ok || at >= until {
			return out
		}
		out = append(out, at)
	}
}

// The coordinated-omission proof. The server stalls for the whole run and
// answers everything 5 s after the last arrival. A closed loop would send one
// request and record one 15 s latency; the open loop must still send every
// scheduled request, and each must be charged from its intended start to the
// answer — the stall inflates latency, it never lowers the request count.
func TestStalledServerInflatesLatencyNotRequestCount(t *testing.T) {
	start := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: start}
	rec := NewRecorder()
	rec.now = clk.Now
	d := &Dispatcher{Start: start, MaxInFlight: 1 << 20, clock: clk}
	stalled := make(chan struct{})
	var sent atomic.Int64
	const window = 10 * time.Second
	t.Cleanup(func() {
		select {
		case <-stalled:
		default:
			close(stalled)
		}
	})
	runVirtual(t, d, NewArrivals(100, 11), window, func(_ context.Context, intended time.Time) {
		sent.Add(1)
		<-stalled // the server has not answered yet
		rec.Done(RouteSyncPull, intended, OutcomeOK)
	})

	offsets := offsetsBefore(100, 11, window)
	if len(offsets) < 900 {
		t.Fatalf("schedule too short to prove anything: %d", len(offsets))
	}
	if d.Launched() != uint64(len(offsets)) || d.Dropped() != 0 {
		t.Fatalf("launched %d dropped %d while the server stalled, want %d launched", d.Launched(), d.Dropped(), len(offsets))
	}
	if lag := d.LagHistogram(); lag.Total != uint64(len(offsets)) || lag.MaxMicros != 0 {
		t.Fatalf("virtual-time lag: total %d max %d µs", lag.Total, lag.MaxMicros)
	}

	answered := start.Add(window + 5*time.Second)
	clk.Set(answered)
	close(stalled)
	d.Wait()
	if sent.Load() != int64(len(offsets)) {
		t.Fatalf("%d requests reached the server, want %d", sent.Load(), len(offsets))
	}

	var want Histogram
	for _, off := range offsets {
		want.Record(answered.Sub(start.Add(off)))
	}
	got := rec.Snapshot()[RouteSyncPull]
	if got.Latency != want {
		t.Fatalf("latency not charged from the intended start: total %d p50 %v max %v, want total %d p50 %v max %v",
			got.Latency.Total, got.Latency.Quantile(0.5), got.Latency.Quantile(1), want.Total, want.Quantile(0.5), want.Quantile(1))
	}
	if p50 := got.Latency.Quantile(0.5); p50 < 9*time.Second {
		t.Fatalf("p50 %v: the stall was hidden", p50)
	}
	if got.Outcomes[OutcomeOK] != uint64(len(offsets)) {
		t.Fatalf("outcomes %v", got.Outcomes)
	}
}

// With a cap on in-flight work, a stalled server turns arrivals into counted
// drops — never into a slower schedule.
func TestStalledServerAtTheCapDropsAndCounts(t *testing.T) {
	start := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: start}
	d := &Dispatcher{Start: start, MaxInFlight: 50, clock: clk}
	stalled := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-stalled:
		default:
			close(stalled)
		}
	})
	runVirtual(t, d, NewArrivals(100, 12), 5*time.Second, func(context.Context, time.Time) { <-stalled })
	n := uint64(len(offsetsBefore(100, 12, 5*time.Second)))
	if d.Launched() != 50 || d.Dropped() != n-50 {
		t.Fatalf("launched %d dropped %d of %d arrivals, want 50 and %d", d.Launched(), d.Dropped(), n, n-50)
	}
	if lag := d.LagHistogram(); lag.Total != n {
		t.Fatalf("lag recorded for %d arrivals, want every one (%d)", lag.Total, n)
	}
	close(stalled)
	d.Wait()
}

// Lag is the release time against the intended start, for every arrival.
func TestDispatcherRecordsSchedulerLag(t *testing.T) {
	start := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: start, oversleep: 30 * time.Millisecond}
	d := &Dispatcher{Start: start, MaxInFlight: 1 << 20, clock: clk}
	runVirtual(t, d, NewArrivals(10, 13), 10*time.Second, func(context.Context, time.Time) {})
	d.Wait()
	lag := d.LagHistogram()
	if n := uint64(len(offsetsBefore(10, 13, 10*time.Second))); lag.Total != n || d.Launched() != n {
		t.Fatalf("lag samples %d, launched %d, want %d", lag.Total, d.Launched(), n)
	}
	if lag.Quantile(0) < 30*time.Millisecond {
		t.Fatalf("lag min %v: a scheduler that always wakes 30 ms late was not seen", lag.Quantile(0))
	}
	s := passing()
	s.SchedulerLag = lag
	expectReason(t, s, "scheduler lag p99")
}

// The coordinated-omission proof for a late generator. The scheduler always
// wakes 30 ms late and the server answers instantly. Every operation must
// still receive its intended start (never the release time), so the 30 ms
// the generator lost is charged to the request instead of disappearing.
func TestLateGeneratorStillChargesFromIntendedStart(t *testing.T) {
	start := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: start, oversleep: 30 * time.Millisecond}
	rec := NewRecorder()
	rec.now = clk.Now
	d := &Dispatcher{Start: start, MaxInFlight: 1 << 20, clock: clk}
	var mu sync.Mutex
	got := map[time.Time]int{}
	runVirtual(t, d, NewArrivals(20, 14), 10*time.Second, func(_ context.Context, intended time.Time) {
		mu.Lock()
		got[intended]++
		mu.Unlock()
		rec.Done(RouteSyncPull, intended, OutcomeOK) // the server answers at once
	})
	d.Wait()

	offsets := offsetsBefore(20, 14, 10*time.Second)
	want := map[time.Time]int{}
	for _, off := range offsets {
		want[start.Add(off)]++
	}
	if len(got) != len(want) {
		t.Fatalf("op received %d distinct start times, want the %d intended starts", len(got), len(want))
	}
	for at, n := range want {
		if got[at] != n {
			t.Fatalf("intended start %v reached op %d times, want %d: op was given the release time", at.Sub(start), got[at], n)
		}
	}
	s := rec.Snapshot()[RouteSyncPull]
	if s.Latency.Total != uint64(len(offsets)) || s.Latency.Quantile(0) < 30*time.Millisecond {
		t.Fatalf("latency total %d min %v: the generator's 30 ms lateness was not charged", s.Latency.Total, s.Latency.Quantile(0))
	}
}

func TestDispatcherNeverWaitsForSlowOperations(t *testing.T) {
	d := &Dispatcher{Start: time.Now(), MaxInFlight: 10_000}
	var started atomic.Int64
	d.Run(t.Context(), NewArrivals(200, 3), 2*time.Second, func(ctx context.Context, intended time.Time) {
		started.Add(1)
		time.Sleep(300 * time.Millisecond) // each op is slower than the gap
	})
	d.Wait()
	if n := started.Load(); n < 340 || n > 460 {
		t.Fatalf("launched %d, want ~400: the dispatcher waited", n)
	}
	lag := d.LagHistogram()
	// 50 ms, not the SLO's 20 ms: this runs under -race on a shared machine;
	// the property here is "never waits for an operation", not timer precision.
	if p99 := lag.Quantile(0.99); p99 > 50*time.Millisecond {
		t.Fatalf("scheduler lag p99 %v", p99)
	}
}

func TestDispatcherCountsDropsWhenSaturated(t *testing.T) {
	d := &Dispatcher{Start: time.Now(), MaxInFlight: 1}
	d.Run(t.Context(), NewArrivals(100, 5), time.Second, func(ctx context.Context, _ time.Time) { time.Sleep(200 * time.Millisecond) })
	d.Wait()
	if d.Dropped() == 0 {
		t.Fatal("saturation was absorbed instead of counted")
	}
}

func TestDispatcherStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	d := &Dispatcher{Start: time.Now(), MaxInFlight: 10_000}
	began := time.Now()
	d.Run(ctx, NewArrivals(100, 6), time.Hour, func(context.Context, time.Time) {})
	d.Wait()
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("Run ignored the context for %v", took)
	}
	if d.Launched() == 0 || d.Launched() > 200 {
		t.Fatalf("launched %d in ~200 ms at 100/s", d.Launched())
	}
}

func TestDispatcherRejectsANonPositiveCap(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MaxInFlight 0 was accepted: every arrival would be dropped")
		}
	}()
	(&Dispatcher{Start: time.Now()}).Run(t.Context(), NewArrivals(1, 1), time.Second, func(context.Context, time.Time) {})
}

func TestRecorderMeasuresFromIntendedStart(t *testing.T) {
	r := NewRecorder()
	r.Done(RouteSyncPull, time.Now().Add(-250*time.Millisecond), OutcomeOK) // intended 250 ms ago
	s := r.Snapshot()[RouteSyncPull]
	if got := s.Latency.Quantile(1); got < 250*time.Millisecond {
		t.Fatalf("latency %v measured from send, not intent", got)
	}
}

// Snapshot merges every shard and hands out copies: a later Done never
// changes a snapshot already taken.
func TestRecorderMergesShardsUnderConcurrency(t *testing.T) {
	start := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	r := NewRecorder()
	r.now = func() time.Time { return start.Add(time.Second) }
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 1000 {
				outcome := OutcomeOK
				if i%100 == 0 {
					outcome = "server_busy"
				}
				route := RouteSyncPull
				if g%2 == 1 {
					route = RouteRefresh
				}
				r.Done(route, start, outcome)
			}
		})
	}
	wg.Wait()
	snap := r.Snapshot()
	for _, route := range []string{RouteSyncPull, RouteRefresh} {
		s := snap[route]
		if s.Latency.Total != 8000 || s.Outcomes[OutcomeOK] != 7920 || s.Outcomes["server_busy"] != 80 {
			t.Fatalf("%s: total %d outcomes %v", route, s.Latency.Total, s.Outcomes)
		}
		if q := s.Latency.Quantile(1); q != time.Second {
			t.Fatalf("%s: max %v, want 1s", route, q)
		}
	}
	r.Done(RouteSyncPull, start, "late")
	if snap[RouteSyncPull].Outcomes["late"] != 0 || snap[RouteSyncPull].Latency.Total != 8000 {
		t.Fatal("a snapshot changed after it was taken")
	}
}
