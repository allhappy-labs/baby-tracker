package loadtest

import (
	"testing"
	"time"
)

// A socket's close, judged as FamilySyncWebSocket.decide judges it: the
// server's lifetime 1000 is redialled at once; its 1001 and the sweep's
// 1008 on a socket the device's latest refresh retired (within the grace),
// or while its refresh is in flight, after the first backoff, U(0, 1 s);
// anything else is a loss.
func TestLiveCloseClassification(t *testing.T) {
	refreshed := time.Now()
	before, after := refreshed.Add(-time.Minute), refreshed.Add(time.Second)
	cases := []struct {
		name        string
		code        int
		reason      string
		dialed      time.Time
		refreshedAt time.Time
		since       time.Duration // the close, after the refresh
		refreshing  bool          // the device's refresh is in flight at the close
		want        closeKind
	}{
		{"lifetime", closeNormal, "maximum lifetime", before, refreshed, 0, false, closeRedialNow},
		{"1000 without a reason", closeNormal, "", before, time.Time{}, 0, false, closeRedialNow},
		{"1000 for another reason", closeNormal, "token renewed", before, refreshed, 0, false, closeLost},
		{"going away", closeGoingAway, "server shutdown", before, time.Time{}, 0, false, closeRedialSoon},
		{"sweep, the first sweep after the refresh", closePolicyViolation, sweepCloseReason, before, refreshed, 5 * time.Second, false, closeRedialSoon},
		{"sweep, past the old 10 s grace", closePolicyViolation, sweepCloseReason, before, refreshed, 25 * time.Second, false, closeRedialSoon},
		{"sweep, at the grace's end", closePolicyViolation, sweepCloseReason, before, refreshed, renewalGrace - time.Millisecond, false, closeRedialSoon},
		{"sweep, beyond the grace", closePolicyViolation, sweepCloseReason, before, refreshed, renewalGrace, false, closeLost},
		{"sweep, a socket dialled after the refresh", closePolicyViolation, sweepCloseReason, after, refreshed, 5 * time.Second, false, closeLost},
		{"sweep, never refreshed", closePolicyViolation, sweepCloseReason, before, time.Time{}, 5 * time.Second, false, closeLost},
		{"sweep could not answer", closePolicyViolation, "authorization unavailable", before, refreshed, 5 * time.Second, false, closeLost},
		{"no close frame", -1, "", before, refreshed, 0, false, closeLost},
		{"sweep, the device's refresh in flight", closePolicyViolation, sweepCloseReason, before, time.Time{}, 0, true, closeRedialSoon},
		{"sweep, a second refresh in flight", closePolicyViolation, sweepCloseReason, before, refreshed, time.Hour, true, closeRedialSoon},
		{"sweep could not answer, refresh in flight", closePolicyViolation, "authorization unavailable", before, refreshed, 0, true, closeLost},
		{"no close frame, refresh in flight", -1, "", before, refreshed, 0, true, closeLost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyClose(tc.code, tc.reason, tc.dialed, tc.refreshedAt, tc.refreshing, refreshed.Add(tc.since)); got != tc.want {
				t.Fatalf("%d %q: %v, want %v", tc.code, tc.reason, got, tc.want)
			}
		})
	}
	// The grace covers the hub's slowest retirement of a consumed session's
	// socket (realtime/hub.go): a pass whose read missed the refresh (its
	// 5 s timeout), the next tick (5 s), that pass (5 s), and the close
	// write (5 s).
	if renewalGrace < 20*time.Second {
		t.Fatalf("renewal grace %v is shorter than the hub's 20 s worst case", renewalGrace)
	}
}

// A refresh leaves the socket alone, as on iOS: the server retires it (the
// sweep's 1008, the session consumed), having freed the device's slot
// first, and the device redials with the child's token. Against a server
// holding one slot per device, no handshake is refused and nothing is
// lost. The refresh never waits for a dial.
func TestRefreshLeavesTheSocketForTheServerToRetire(t *testing.T) {
	f := newFakeV1(t)
	f.oneSocketPerDevice()
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	d.live.Store(true)
	if !g.connectLive(t.Context(), d, 0, 0).opened {
		t.Fatal("dial failed")
	}
	waitFor(t, func() bool { return f.openSockets() == 1 })
	started := time.Now()
	if !g.refresh(t.Context(), d, time.Now()) {
		t.Fatal("refresh failed")
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("the refresh took %v", took)
	}
	time.Sleep(300 * time.Millisecond)
	if f.openSockets() != 1 || f.count("GET /v1/live") != 1 {
		t.Fatalf("after the refresh: %d open, %d dials; the device touched its socket", f.openSockets(), f.count("GET /v1/live"))
	}
	f.closeSocketsWith(closePolicyViolation, sweepCloseReason)
	waitFor(t, func() bool { return f.openSockets() == 1 && f.count("GET /v1/live") == 2 })
	if dials := f.all("/v1/live"); dials[1].Header.Get("Authorization") != "Bearer access-child-1" {
		t.Fatalf("redialled with %q", dials[1].Header.Get("Authorization"))
	}
	if g.liveLosses.Load() != 0 || g.renewals.Load() != 1 || g.outcomes(RouteLive)[OutcomeRateLimited] != 0 {
		t.Fatalf("losses %d renewals %d handshakes %v", g.liveLosses.Load(), g.renewals.Load(), g.outcomes(RouteLive))
	}
}

// The server's lifetime close is redialled at once (iOS: closed 1000,
// delay 0); the slot was freed before the close, so the redial is never
// refused.
func TestLifetimeCloseRedialsAtOnce(t *testing.T) {
	f := newFakeV1(t)
	f.oneSocketPerDevice()
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	d.live.Store(true)
	if !g.connectLive(t.Context(), d, 0, 0).opened {
		t.Fatal("dial failed")
	}
	waitFor(t, func() bool { return f.openSockets() == 1 })
	for i := range 5 {
		dials := f.count("GET /v1/live")
		closed := time.Now()
		f.closeSocketsWith(closeNormal, "maximum lifetime")
		waitFor(t, func() bool { return f.count("GET /v1/live") > dials && f.openSockets() == 1 })
		// U(0, 1 s) under 200 ms five times running is a 0.03% chance.
		if took := time.Since(closed); took > 200*time.Millisecond {
			t.Fatalf("close %d: redialled after %v", i, took)
		}
	}
	if g.liveLosses.Load() != 0 || g.renewals.Load() != 5 || g.outcomes(RouteLive)[OutcomeRateLimited] != 0 {
		t.Fatalf("losses %d renewals %d handshakes %v", g.liveLosses.Load(), g.renewals.Load(), g.outcomes(RouteLive))
	}
}
