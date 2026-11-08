package loadtest

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// clock is the dispatcher's time source. Production uses the wall clock;
// tests use virtual time so the open-loop property is proven exactly.
type clock interface {
	Now() time.Time
	// SleepUntil blocks until t and reports true, or reports false as soon
	// as ctx ends.
	SleepUntil(ctx context.Context, t time.Time) bool
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) SleepUntil(ctx context.Context, t time.Time) bool {
	wait := time.Until(t)
	if wait <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Dispatcher launches an operation at every intended start. It never waits
// for one to finish before launching the next: the open loop is what keeps a
// slow server from lowering the offered load (coordinated omission). When
// MaxInFlight operations are running, an arrival is dropped and counted,
// which fails the run (Evaluate), rather than delayed.
//
// Every operation receives its intended start, and latency is measured from
// it (Recorder.Done), so time an operation spends queued behind a stalled
// server counts against the server.
type Dispatcher struct {
	Start       time.Time
	MaxInFlight int64 // must be positive
	// Observe, when set, sees every arrival as it is released: its intended
	// start, how late it was released and whether it was dropped. Run calls
	// it from its own goroutine.
	Observe func(intended time.Time, lag time.Duration, dropped bool)

	clock    clock // nil: the wall clock
	lagMu    sync.Mutex
	lag      Histogram
	inFlight atomic.Int64
	dropped  atomic.Uint64
	launched atomic.Uint64
	wg       sync.WaitGroup
}

// Run launches op at Start+offset for every arrival before until, and
// returns when the schedule is exhausted or ctx ends. It does not wait for
// launched operations; call Wait for that.
func (d *Dispatcher) Run(ctx context.Context, a *Arrivals, until time.Duration, op func(context.Context, time.Time)) {
	if d.MaxInFlight <= 0 {
		panic("loadtest: Dispatcher.MaxInFlight must be positive")
	}
	clk := d.clock
	if clk == nil {
		clk = wallClock{}
	}
	for {
		offset, ok := a.Next()
		if !ok || offset >= until {
			return
		}
		intended := d.Start.Add(offset)
		if !clk.SleepUntil(ctx, intended) {
			return
		}
		lag := clk.Now().Sub(intended)
		d.lagMu.Lock()
		d.lag.Record(lag)
		d.lagMu.Unlock()
		if !d.acquire() {
			d.dropped.Add(1)
			if d.Observe != nil {
				d.Observe(intended, lag, true)
			}
			continue
		}
		if d.Observe != nil {
			d.Observe(intended, lag, false)
		}
		d.launched.Add(1)
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer d.inFlight.Add(-1)
			op(ctx, intended)
		}()
	}
}

// acquire takes an in-flight slot if one is free. The compare-and-swap keeps
// the cap exact even if several Runs share one Dispatcher.
func (d *Dispatcher) acquire() bool {
	for {
		n := d.inFlight.Load()
		if n >= d.MaxInFlight {
			return false
		}
		if d.inFlight.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// Wait blocks until every launched operation has returned.
func (d *Dispatcher) Wait() { d.wg.Wait() }

// Dropped counts arrivals not launched because MaxInFlight was reached.
func (d *Dispatcher) Dropped() uint64 { return d.dropped.Load() }

// Launched counts operations started.
func (d *Dispatcher) Launched() uint64 { return d.launched.Load() }

// LagHistogram is how late each arrival (launched or dropped) was released
// against its intended start: the generator's own health.
func (d *Dispatcher) LagHistogram() Histogram {
	d.lagMu.Lock()
	defer d.lagMu.Unlock()
	return d.lag
}
