package loadtest

import (
	"testing"
	"time"
)

// I3: the sweep's 1008 is planned only on a socket dialled before the
// device's refresh (whose session the refresh consumed), with the sweep's
// reason. A 1008 on a socket dialled after the refresh, or for another
// reason, is a loss.
func TestPlanned1008IsScopedToTheSocketTheRefreshConsumed(t *testing.T) {
	cases := []struct {
		name          string
		refreshBefore bool // the refresh happened before the dial
		reason        string
		loss          int64
	}{
		{"old socket, sweep reason", false, "authorization changed", 0},
		{"socket dialled after the refresh", true, "authorization changed", 1},
		{"old socket, another reason", false, "recovery epoch changed", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeV1(t)
			g := testGenerator(t, f, 1)
			d := g.devices[0]
			d.live.Store(true)
			if tc.refreshBefore {
				d.refreshedAt = time.Now()
				time.Sleep(5 * time.Millisecond)
			}
			if !g.connectLive(t.Context(), d, 0, 0).opened {
				t.Fatal("dial failed")
			}
			waitFor(t, func() bool { return f.openSockets() == 1 })
			if !tc.refreshBefore {
				d.mu.Lock()
				d.refreshedAt = time.Now() // the refresh answered; the renewal has not retired the socket yet
				d.mu.Unlock()
			}
			dials := f.count("GET /v1/live")
			f.closeSocketsWith(closePolicyViolation, tc.reason)
			waitFor(t, func() bool { return f.count("GET /v1/live") > dials })
			if got := g.liveLosses.Load(); got != tc.loss {
				t.Fatalf("losses %d, want %d", got, tc.loss)
			}
		})
	}
}

// M1: a frame that is not a hint is a protocol failure, not a leak.
func TestUndecodableLiveFrameIsProtocol(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	d.live.Store(true)
	g.connectLive(t.Context(), d, 0, 0)
	waitFor(t, func() bool { return f.openSockets() == 1 })
	f.sendRaw([]byte(`{"not":"a hint"}`))
	waitFor(t, func() bool { return g.outcomes(RouteSyncHintPull)[OutcomeProtocol] == 1 })
	if g.outcomes(RouteSyncHintPull)[OutcomeCrossFamily] != 0 {
		t.Fatal("a malformed frame counted as cross-family")
	}
}

// M5: a window's generator health is its own: the lag and drops of the
// arrivals intended in it and the losses in it, not the run's.
func TestWindowsJudgeTheirOwnHealth(t *testing.T) {
	start := time.Now()
	p := newPhases(start)
	before := p.addWindow("before", 1, start, start.Add(time.Minute))
	after := p.addWindow("after", 1, start.Add(time.Minute), start.Add(2*time.Minute))
	p.observe(start.Add(10*time.Second), 2*time.Millisecond, false)
	p.observe(start.Add(70*time.Second), 900*time.Millisecond, true)
	p.lose(start.Add(80 * time.Second))
	g := &generator{phases: p}
	b, a := g.windowSummary(before), g.windowSummary(after)
	if b.SchedulerLag.Total != 1 || b.Dropped != 0 || b.UnexpectedLiveLosses != 0 {
		t.Fatalf("before: lag %d dropped %d losses %d", b.SchedulerLag.Total, b.Dropped, b.UnexpectedLiveLosses)
	}
	if a.SchedulerLag.Total != 1 || a.Dropped != 1 || a.UnexpectedLiveLosses != 1 {
		t.Fatalf("after: lag %d dropped %d losses %d", a.SchedulerLag.Total, a.Dropped, a.UnexpectedLiveLosses)
	}
}

// M5: a storm redial pre-empted by the device's own other dial (a
// renewal) counts as reconnected once that dial opens.
func TestStormCountsAPreemptedRedialAsReconnected(t *testing.T) {
	f := newFakeV1(t)
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	d.live.Store(true)
	g.connectLive(t.Context(), d, 0, 0)
	waitFor(t, func() bool { return f.openSockets() == 1 })
	// Another dial loop runs for the device when the storm redials it.
	d.mu.Lock()
	d.dialing = true
	d.mu.Unlock()
	go func() {
		time.Sleep(300 * time.Millisecond)
		d.mu.Lock()
		d.dialing = false
		d.mu.Unlock()
		g.connectLive(t.Context(), d, 0, 0)
	}()
	r := g.runStorm(t.Context())
	if r.Sockets != 1 || r.Reconnected != 1 || r.SecondsToAll < 0.25 {
		t.Fatalf("storm %+v", r)
	}
}
