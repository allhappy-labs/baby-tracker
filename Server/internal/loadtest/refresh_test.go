package loadtest

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"
)

// I1: at 1x on 10k families the model's refresh interval (571 s) plus its
// jitter outlasts the access token less 60 s (540 s). The schedule then
// shortens its period to 510 s, keeps the jitter, and offers a flat rate:
// the per-minute refresh counts stay within ±15% of the effective rate over
// more than three periods, every device's first timer falls before its
// expiry less 90 s, and no timer outlasts the token it refreshes.
func TestRefreshScheduleStaysFlatWhenTheTokenBoundsThePeriod(t *testing.T) {
	const devices = 4000
	from := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	rng := newLockedRand(5, 6)
	states := make([]DeviceState, devices)
	for i := range states {
		// The warm-up (5 min) refreshed every device: tokens expire 5–10
		// minutes after window-start.
		states[i] = DeviceState{FamilyID: "f", DeviceID: fmt.Sprint(i), AccessExpiresAt: from.Add(300*time.Second + rng.Duration(300*time.Second))}
	}
	g := newGenerator(RunConfig{Seed: 3}, FleetHeader{Families: devices / 4}, states)
	until := from.Add(30 * time.Minute)
	s := g.newRefreshSchedule(7, from, until, 1) // 4,000 devices / 7 per s = 571 s
	if s.period != 510*time.Second || s.jitter != 30*time.Second || math.Abs(s.effective-devices/510.0) > 1e-9 {
		t.Fatalf("period %v jitter %v effective %v", s.period, s.jitter, s.effective)
	}
	clk := &fakeClock{now: from}
	s.clock = clk
	var mu sync.Mutex
	fired := map[*device][]time.Time{}
	var all []time.Time
	s.fire = func(_ context.Context, d *device, due time.Time) {
		mu.Lock()
		fired[d] = append(fired[d], due)
		all = append(all, due)
		mu.Unlock()
	}
	s.run(t.Context())
	s.Wait()

	perMinute := make([]int, 30)
	for _, due := range all {
		perMinute[int(due.Sub(from)/time.Minute)]++
	}
	want := s.effective * 60
	for minute, n := range perMinute {
		if math.Abs(float64(n)-want) > 0.15*want {
			t.Errorf("minute %d: %d refreshes, want %.0f ± 15%% (all: %v)", minute, n, want, perMinute)
		}
	}
	ttl := defaultAccessTTL
	for d, dues := range fired {
		sort.Slice(dues, func(i, j int) bool { return dues[i].Before(dues[j]) })
		if dues[0].After(d.AccessExpiresAt.Add(-90 * time.Second)) {
			t.Fatalf("device %s first refreshed at %v, after its expiry %v less 90 s", d.DeviceID, dues[0].Sub(from), d.AccessExpiresAt.Sub(from))
		}
		for i := 1; i < len(dues); i++ {
			if gap := dues[i].Sub(dues[i-1]); gap > ttl-60*time.Second {
				t.Fatalf("device %s waited %v between refreshes, past the token's %v less 60 s", d.DeviceID, gap, ttl)
			}
		}
	}
}

// C1 (re-review): the plan's runs warm up for 10 minutes. The refresh
// schedule runs from the warm-up's start through the measurement without a
// reset, so the measurement opens on its steady rate: over 30 measured
// minutes, at 1x and 2x on 40,000 devices, every minute is within ±15% of
// the effective rate and no second holds more than twice the per-second
// rate. A stale fleet's lapsed tokens never pile timers anywhere: a device
// whose access would lapse before its next timer refreshes as its warm-up
// starts (needsRefresh), so its access never outlives its token.
func TestRefreshScheduleIsFlatAcrossATenMinuteWarmup(t *testing.T) {
	const (
		devices = 40000
		warmup  = 10 * time.Minute
		measure = 30 * time.Minute
	)
	for _, tc := range []struct {
		name string
		rate float64
	}{{"1x", 70}, {"2x", 140}} {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
			windowStart := started.Add(warmup)
			states := make([]DeviceState, devices)
			for i := range states {
				// The stale fleet of a later run: every token lapsed hours ago.
				states[i] = DeviceState{FamilyID: "f", DeviceID: fmt.Sprint(i), AccessExpiresAt: started.Add(-3 * time.Hour)}
			}
			g := newGenerator(RunConfig{Seed: 9}, FleetHeader{Families: devices / 4}, states)
			s := g.newRefreshSchedule(tc.rate, started, windowStart.Add(measure), 0)
			s.clock = &fakeClock{now: started}
			var mu sync.Mutex
			fired := map[*device][]time.Time{}
			s.fire = func(_ context.Context, d *device, due time.Time) {
				mu.Lock()
				fired[d] = append(fired[d], due)
				mu.Unlock()
			}
			s.run(t.Context())
			s.Wait()

			perMinute := make([]int, int(measure/time.Minute))
			perSecond := map[int64]int{}
			for _, dues := range fired {
				for _, due := range dues {
					if due.Before(windowStart) {
						continue
					}
					perMinute[int(due.Sub(windowStart)/time.Minute)]++
					perSecond[due.Unix()]++
				}
			}
			wantMinute := s.effective * 60
			for minute, n := range perMinute {
				if math.Abs(float64(n)-wantMinute) > 0.15*wantMinute {
					t.Errorf("minute %d: %d refreshes, want %.0f ± 15%%", minute, n, wantMinute)
				}
			}
			// No second over twice the model's rate (tc.rate), not the effective
			// one: the busiest second observed is 108/s at 1x and 180/s at 2x.
			for second, n := range perSecond {
				if float64(n) > 2*tc.rate {
					t.Errorf("second %d: %d refreshes, over twice the model's rate %.0f/s", second-windowStart.Unix(), n, tc.rate)
				}
			}

			// Each device starts its warm-up at U(0, warm-up): the scheduled
			// refreshes before then (if any) and its start's own refresh (if
			// needsRefresh asks for one) leave its access valid at every
			// moment after its start, with 60 s to spare.
			ttl := defaultAccessTTL
			rng := newLockedRand(9, 10)
			startRefreshes := 0
			for _, d := range g.devices {
				dues := fired[d]
				sort.Slice(dues, func(i, j int) bool { return dues[i].Before(dues[j]) })
				start := started.Add(rng.Duration(warmup))
				expiry, next := d.AccessExpiresAt, time.Time{}
				for _, due := range dues {
					if !due.After(start) {
						expiry = due.Add(ttl)
					} else if next.IsZero() {
						next = due
					}
				}
				d.AccessExpiresAt, d.nextRefresh = expiry, next
				last := time.Time{}
				if d.needsRefresh(start) {
					startRefreshes++
					last = start
				} else {
					last = expiry.Add(-ttl)
				}
				for _, due := range dues {
					if !due.After(start) {
						continue
					}
					if gap := due.Sub(last); gap > ttl-60*time.Second {
						t.Fatalf("device %s: %v between refreshes after its start, past the token's %v less 60 s", d.DeviceID, gap, ttl)
					}
					last = due
				}
			}
			if startRefreshes == 0 {
				t.Fatal("no device refreshed at its start, though every token had lapsed")
			}
		})
	}
}

// A ramp step retimes the run's one schedule: the pending timers' offsets
// scale with the period, so they stay uniform at the new rate.
func TestRampStepRetimesTheSchedule(t *testing.T) {
	const devices = 4000
	from := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	states := make([]DeviceState, devices)
	for i := range states {
		states[i] = DeviceState{FamilyID: "f", DeviceID: fmt.Sprint(i)}
	}
	g := newGenerator(RunConfig{Seed: 2}, FleetHeader{Families: devices / 4}, states)
	s := g.newRefreshSchedule(14, from, from.Add(time.Hour), 0) // period 286 s
	old := s.period
	s.setRate(from, 28)
	if s.period != old/2 || math.Abs(s.effective-28) > 1e-6 {
		t.Fatalf("period %v (was %v), effective %v", s.period, old, s.effective)
	}
	perTenth := make([]int, 10)
	for _, timer := range s.queue {
		offset := timer.due.Sub(from)
		if offset < 0 || offset >= s.period {
			t.Fatalf("a timer at %v, outside the new period %v", offset, s.period)
		}
		perTenth[int(offset*10/s.period)]++
		if !timer.d.nextRefresh.Equal(timer.due) {
			t.Fatal("a device's next refresh was not moved with its timer")
		}
	}
	for i, n := range perTenth {
		if n < 360 || n > 440 {
			t.Fatalf("tenth %d of the new period holds %d of 4,000 timers: %v", i, n, perTenth)
		}
	}
}

// needsRefresh: a device refreshes before its first request when its
// access lapses (less 60 s) before now or before its next timer, or a lost
// refresh answer is pending.
func TestDeviceRefreshesAtStartWhenItsAccessLapsesBeforeItsTimer(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name               string
		expiry, next       time.Time
		pending            string
		wantRefreshAtStart bool
	}{
		{"lapsed", now.Add(-time.Hour), now.Add(time.Minute), "", true},
		{"lapses within 60 s", now.Add(30 * time.Second), now.Add(10 * time.Second), "", true},
		{"lapses before its timer", now.Add(5 * time.Minute), now.Add(6 * time.Minute), "", true},
		{"outlives its timer", now.Add(8 * time.Minute), now.Add(6 * time.Minute), "", false},
		{"no timer yet", now.Add(8 * time.Minute), time.Time{}, "", false},
		{"lost answer pending", now.Add(8 * time.Minute), now.Add(6 * time.Minute), "attempt", true},
	} {
		d := &device{DeviceState: DeviceState{AccessExpiresAt: tc.expiry, PendingAttemptID: tc.pending}, nextRefresh: tc.next}
		if got := d.needsRefresh(now); got != tc.wantRefreshAtStart {
			t.Errorf("%s: needsRefresh %v", tc.name, got)
		}
	}
}

// At 2x the interval is well under the token's lifetime: the period is the
// model's and the effective rate is the model's.
func TestRefreshScheduleKeepsTheModelsPeriodWhenTheTokenAllows(t *testing.T) {
	states := make([]DeviceState, 4000)
	g := newGenerator(RunConfig{Seed: 3}, FleetHeader{Families: 1000}, states)
	s := g.newRefreshSchedule(14, time.Now(), time.Now().Add(time.Hour), 1)
	rate := 14.0
	if want := time.Duration(4000 / rate * float64(time.Second)); s.period != want || math.Abs(s.effective-14) > 1e-9 {
		t.Fatalf("period %v (want %v), effective %v", s.period, want, s.effective)
	}
}

// M8: a refusal ends the attempt (the next refresh starts a new one); a
// lost answer keeps it (TestAnUnansweredRefreshKeepsItsAttemptInTheFleet).
func TestRefusedRefreshClearsItsAttempt(t *testing.T) {
	f := newFakeV1(t)
	f.hook("POST /v1/auth/refresh-recoverable", func(w http.ResponseWriter, _ *http.Request, _ []byte) bool {
		writeErrorResponse(w, http.StatusUnauthorized, "refresh_reused")
		return true
	})
	g := testGenerator(t, f, 1)
	d := g.devices[0]
	if g.refresh(t.Context(), d, time.Now()) {
		t.Fatal("a refused refresh succeeded")
	}
	if d.state().PendingAttemptID != "" {
		t.Fatal("the refused attempt is still pending")
	}
	if f.count("POST /v1/auth/refresh-recoverable") != 1 {
		t.Fatalf("a refusal was retried: %d", f.count("POST /v1/auth/refresh-recoverable"))
	}
}
