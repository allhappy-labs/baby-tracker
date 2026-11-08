package loadtest

import (
	"container/heap"
	"context"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"time"

	"babytracker/server/internal/protocol"
)

// refreshSchedule is the refresh timers of every device, one schedule for
// the whole run: it starts with the warm-up and runs, without a reset at
// window-start, to the end of the measurement (a ramp retimes it at each
// step). The period is devices / RefreshPerSecond, re-armed every period
// ± jitter (jitter = min(30 s, period/4)), so the schedule offers the
// model's rate, uniform in time, from its first second:
//   - when period + jitter would outlast the access token (its lifetime
//     less 60 s), the period shrinks to that bound less the jitter, with the
//     jitter kept: the schedule then offers more than the model, and the
//     effective rate is reported;
//   - the first round gives each device a slot of the first period, in the
//     order its access expires (the soonest first). A slot is never moved
//     for a token: a device whose access would lapse before its next timer
//     refreshes when its warm-up starts (startDevice), a warm-up request.
//
// Latency is measured from each timer's intended time, and every timer is
// filed by it: the warm-up's refreshes go to the warm-up routes. It reuses
// Dispatcher's counters, cap, lag histogram, clock and Observe hook.
type refreshSchedule struct {
	Dispatcher
	g *generator

	mu        sync.Mutex
	period    time.Duration
	jitter    time.Duration
	effective float64 // refreshes per second offered
	until     time.Time
	rng       *rand.Rand
	queue     refreshQueue
	changed   chan struct{} // a retime or a new end: the run loop looks again

	// fire launches one refresh; nil: g.refresh. Tests record instead.
	fire func(ctx context.Context, d *device, due time.Time)
}

// defaultAccessTTL is the hosted access token's lifetime until a refresh
// answer shows the server's.
const defaultAccessTTL = 10 * time.Minute

// refreshPeriod is the period and jitter for rate refreshes per second over
// n devices, bounded by the access token's lifetime.
func (g *generator) refreshPeriod(n int, rate float64) (period, jitter time.Duration) {
	interval := time.Duration(float64(n) / rate * float64(time.Second))
	period, jitter = interval, min(30*time.Second, interval/4)
	if bound := g.accessTTL() - 60*time.Second; period+jitter > bound {
		period = max(bound-jitter, time.Second)
	}
	return period, jitter
}

func (g *generator) newRefreshSchedule(rate float64, from, until time.Time, salt uint64) *refreshSchedule {
	s := &refreshSchedule{Dispatcher: Dispatcher{Start: from, MaxInFlight: maxInFlight}, g: g, until: until,
		rng: rand.New(rand.NewPCG(g.cfg.Seed^0x7e7, salt)), changed: make(chan struct{}, 1)}
	n := len(g.devices)
	if !(rate > 0) || n == 0 {
		return s
	}
	s.period, s.jitter = g.refreshPeriod(n, rate)
	s.effective = float64(n) / s.period.Seconds()
	g.noteRefreshRate(rate, s.effective, s.period)

	// The first round: slots in order of access expiry (ties, and a stale
	// fleet's long-expired tokens, in a seeded random order).
	type first struct {
		d      *device
		expiry time.Time
	}
	order := make([]first, n)
	for slot, i := range s.rng.Perm(n) {
		d := g.devices[i]
		d.mu.Lock()
		order[slot] = first{d: d, expiry: d.AccessExpiresAt}
		d.mu.Unlock()
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i].expiry, order[j].expiry
		switch {
		case !a.After(from) && !b.After(from):
			return false // both already lapsed: keep the random order
		case a.IsZero():
			return false
		case b.IsZero():
			return true
		}
		return a.Before(b)
	})
	for k, f := range order {
		due := from.Add(time.Duration((float64(k) + s.rng.Float64()) / float64(n) * float64(s.period)))
		f.d.setNextRefresh(due)
		s.queue = append(s.queue, refreshTimer{due: due, d: f.d})
	}
	heap.Init(&s.queue)
	return s
}

// setRate retimes the schedule at `at` to rate (a ramp step): every pending
// timer's offset from `at` scales by the new period over the old, so the
// timers stay uniform at the new rate. When the rate rises the period
// shrinks, so no device waits longer than the old period plus jitter. A
// falling rate would stretch a pending offset past the token's lifetime
// ((P_old + J) × P_new / P_old), so a ramp's steps may not fall
// (checkRisingSteps).
func (s *refreshSchedule) setRate(at time.Time, rate float64) {
	if !(rate > 0) || len(s.g.devices) == 0 {
		return
	}
	s.mu.Lock()
	period, jitter := s.g.refreshPeriod(len(s.g.devices), rate)
	scale := float64(period) / float64(s.period)
	for i := range s.queue {
		if t := &s.queue[i]; t.due.After(at) {
			t.due = at.Add(time.Duration(float64(t.due.Sub(at)) * scale))
			t.d.setNextRefresh(t.due)
		}
	}
	heap.Init(&s.queue)
	s.period, s.jitter = period, jitter
	s.effective = float64(len(s.g.devices)) / period.Seconds()
	s.g.noteRefreshRate(rate, s.effective, period)
	s.mu.Unlock()
	s.poke()
}

// setUntil ends the schedule at t.
func (s *refreshSchedule) setUntil(t time.Time) {
	s.mu.Lock()
	s.until = t
	s.mu.Unlock()
	s.poke()
}

func (s *refreshSchedule) poke() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// waitFor sleeps until t, ctx's end or a change to the schedule. It
// reports whether to fire at t (false: look again, or ctx ended).
func (s *refreshSchedule) waitFor(ctx context.Context, clk clock, t time.Time) bool {
	if s.clock != nil {
		return clk.SleepUntil(ctx, t) // virtual time: nothing changes the schedule concurrently
	}
	wait := time.Until(t)
	if wait <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-s.changed:
		return false
	case <-timer.C:
		return true
	}
}

// run fires the timers until the schedule ends or ctx does. It never waits
// for a refresh to finish (open loop); a timer past MaxInFlight is dropped
// and counted.
func (s *refreshSchedule) run(ctx context.Context) {
	var clk clock = wallClock{}
	if s.clock != nil {
		clk = s.clock
	}
	fire := s.fire
	if fire == nil {
		fire = func(ctx context.Context, d *device, due time.Time) {
			if !s.g.refresh(ctx, d, due) {
				s.g.skip("refresh_in_flight", due)
			}
		}
	}
	for ctx.Err() == nil {
		s.mu.Lock()
		if s.queue.Len() == 0 {
			s.mu.Unlock()
			return
		}
		next := s.queue[0]
		if !next.due.Before(s.until) {
			heap.Pop(&s.queue)
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()
		if !s.waitFor(ctx, clk, next.due) {
			continue // changed (look again) or ended (the loop's condition)
		}
		s.mu.Lock()
		if s.queue.Len() == 0 || s.queue[0] != next {
			s.mu.Unlock()
			continue
		}
		heap.Pop(&s.queue)
		due := next.due.Add(s.period + time.Duration((s.rng.Float64()*2-1)*float64(s.jitter)))
		if due.Before(s.until) {
			heap.Push(&s.queue, refreshTimer{due: due, d: next.d})
			next.d.setNextRefresh(due)
		}
		s.mu.Unlock()

		lag := clk.Now().Sub(next.due)
		s.lagMu.Lock()
		s.lag.Record(lag)
		s.lagMu.Unlock()
		launched := s.acquire()
		if s.Observe != nil {
			s.Observe(next.due, lag, !launched)
		}
		if launched {
			s.launched.Add(1)
			s.wg.Add(1)
			go func(d *device, due time.Time) {
				defer s.wg.Done()
				defer s.inFlight.Add(-1)
				fire(ctx, d, due)
			}(next.d, next.due)
		} else {
			s.dropped.Add(1)
		}
	}
}

type refreshTimer struct {
	due time.Time
	d   *device
}

type refreshQueue []refreshTimer

func (q refreshQueue) Len() int           { return len(q) }
func (q refreshQueue) Less(i, j int) bool { return q[i].due.Before(q[j].due) }
func (q refreshQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *refreshQueue) Push(x any)        { *q = append(*q, x.(refreshTimer)) }
func (q *refreshQueue) Pop() any {
	old := *q
	item := old[len(old)-1]
	*q = old[:len(old)-1]
	return item
}

// accessTTL is the access token lifetime the last refresh answer showed.
func (g *generator) accessTTL() time.Duration {
	if ttl := time.Duration(g.ttlNanos.Load()); ttl > 2*time.Minute {
		return ttl
	}
	return defaultAccessTTL
}

// refreshRetries bounds the retries of one refresh or acknowledgement.
const refreshRetries = 3

// retryable reports a failure a retry of the same request may cure: the
// answer was lost or the server shed or failed the request.
func retryable(r result) bool {
	switch r.Outcome {
	case OutcomeTransport, OutcomeTimeout, OutcomeServerBusy, OutcomeNotReady, OutcomeHTTP5xx,
		OutcomeSecurityBusy, OutcomeCredentialRotationBusy:
		return true
	}
	return false
}

// retryDelay is how long to wait before retrying r: Retry-After when the
// server sent one, else a second.
func retryDelay(r result) time.Duration {
	if r.RetryAfter > 0 {
		return r.RetryAfter
	}
	return time.Second
}

// refresh rotates d's session through refresh-recoverable and
// refresh-ack. The attempt ID is stored (PendingAttemptID, checkpointed)
// before the request: a lost answer is retried with the same attempt and
// the same parent, so the server returns the child it committed. On the
// answer the tokens are replaced and the attempt cleared; the ack follows
// with the child's access token. A refusal (a 4xx) ends the attempt: it is
// cleared, so the next refresh starts a new one. It reports whether the
// tokens were replaced; false also when another refresh of d is in flight.
//
// A live device's socket is left alone, as on iOS: the server's sweep
// retires it (1008, its session consumed; classifyClose) and the device
// redials with the child's token. The refresh never waits for a dial.
func (g *generator) refresh(ctx context.Context, d *device, intended time.Time) bool {
	d.mu.Lock()
	if d.refreshing {
		d.mu.Unlock()
		return false
	}
	d.refreshing = true
	d.mu.Unlock()
	replaced := g.rotate(ctx, d, intended)
	d.mu.Lock()
	d.refreshing = false
	d.mu.Unlock()
	return replaced
}

// rotate is refresh's exchange; the caller holds d.refreshing. The
// exchange holds d's session exclusively, from before the parent's tokens
// are read until the child's replace them: it waits for the device's pulls
// and snapshot pages in flight (their wait is the client's, so intended
// moves by it), and a pull arriving meanwhile waits for the child's
// access. The ack runs with the child's access and holds nothing.
func (g *generator) rotate(ctx context.Context, d *device, intended time.Time) bool {
	start := time.Now()
	d.session.Lock()
	g.noteSessionWait(RouteRefresh, time.Since(start))
	intended = intended.Add(time.Since(start))
	held := true
	unlock := func() {
		if held {
			held = false
			d.session.Unlock()
		}
	}
	defer unlock()
	d.mu.Lock()
	if d.PendingAttemptID == "" {
		d.PendingAttemptID = newUUID()
	}
	continuity := protocol.DeviceContinuityClaim{DeviceID: d.DeviceID, Credential: d.DeviceCredential}
	body := protocol.RecoverableRefreshRequest{
		RequestMeta:  protocol.RequestMeta{ProtocolVersion: "v1", Capabilities: protocol.RefreshRecoveryCapabilities()},
		RefreshToken: d.RefreshToken, RefreshAttemptID: d.PendingAttemptID, DeviceContinuity: continuity}
	d.mu.Unlock()

	var answer protocol.RecoverableRefreshResponse
	for try := 0; ; try++ {
		answer = protocol.RecoverableRefreshResponse{}
		res := g.c.do(ctx, request{method: http.MethodPost, path: "/v1/auth/refresh-recoverable", device: d,
			capabilities: protocol.RefreshRecoveryCapabilities(), body: body}, &answer)
		if res.ok() && (answer.AccessToken == "" || answer.RefreshToken == "" || answer.RefreshAttemptID != body.RefreshAttemptID) {
			res.Outcome = OutcomeProtocol
		}
		retrying := !res.ok() && retryable(res) && try < refreshRetries && ctx.Err() == nil
		g.record(RouteRefresh, intended, settled(res, retrying))
		if res.ok() {
			break
		}
		if !retrying {
			if res.Status >= 400 && res.Status < 500 {
				// Refused: the server committed nothing under this attempt.
				d.mu.Lock()
				if d.PendingAttemptID == body.RefreshAttemptID {
					d.PendingAttemptID = ""
				}
				d.mu.Unlock()
			}
			return false
		}
		wait := retryDelay(res)
		intended = time.Now().Add(wait)
		if !sleepFor(ctx, wait) {
			return false
		}
	}
	now := time.Now()
	expires, _ := time.Parse(time.RFC3339Nano, answer.ExpiresAt)
	reauthenticate, _ := time.Parse(time.RFC3339Nano, answer.ReauthenticateAt)
	if ttl := expires.Sub(now); ttl > 0 {
		g.ttlNanos.Store(int64(ttl))
	}
	d.mu.Lock()
	d.AccessToken, d.RefreshToken = answer.AccessToken, answer.RefreshToken
	d.AccessExpiresAt, d.ReauthenticateAt = expires, reauthenticate
	if answer.DeviceCredential != "" {
		d.DeviceCredential = answer.DeviceCredential
		continuity.Credential = answer.DeviceCredential
	}
	d.PendingAttemptID = ""
	d.refreshedAt = now
	access := d.AccessToken
	d.mu.Unlock()
	unlock()

	// The ack retires the server's recovery receipt. One that never lands
	// leaves the receipt to the server's retention; the session is fine.
	ack := protocol.RefreshAckRequest{RequestMeta: body.RequestMeta, RefreshAttemptID: body.RefreshAttemptID, DeviceContinuity: continuity}
	ackIntended := now
	for try := 0; ; try++ {
		res := g.c.do(ctx, request{method: http.MethodPost, path: "/v1/auth/refresh-ack", device: d, bearer: access,
			capabilities: protocol.RefreshRecoveryCapabilities(), body: ack}, nil)
		retrying := !res.ok() && retryable(res) && try < refreshRetries && ctx.Err() == nil
		g.record(RouteRefreshAck, ackIntended, settled(res, retrying))
		if !retrying {
			break
		}
		wait := retryDelay(res)
		ackIntended = time.Now().Add(wait)
		if !sleepFor(ctx, wait) {
			break
		}
	}
	return true
}

// skipAccessNotRenewed is a request not sent because its device's session
// could not be renewed first (renewBeforeUse).
const skipAccessNotRenewed = "access_not_renewed"

// renewBeforeUse renews d's session before a request with its access when
// it must (mustRenewBeforeUse), as iOS's FamilySyncEngine runs
// prepareSession before any work: a refresh lost in a restart window is
// re-sent under its pending attempt once the server is back, before the
// parent's access can lapse in use. A refresh of d already in flight is
// left to the request: its session hold waits for that exchange (and is
// recorded as the route's session wait), then reads the child's access. It
// reports how long the renewal took (the client's own wait: the caller
// moves its intended time by it) and whether d may send; false (the
// renewal failed) means the request is not sent, as iOS does not run work
// whose preparation threw.
func (g *generator) renewBeforeUse(ctx context.Context, d *device, intended time.Time) (time.Duration, bool) {
	start := time.Now()
	d.mu.Lock()
	if d.refreshing || !d.mustRenewLocked(start) {
		d.mu.Unlock()
		return 0, true
	}
	d.refreshing = true
	d.mu.Unlock()
	g.rotate(ctx, d, intended)
	d.mu.Lock()
	d.refreshing = false
	d.mu.Unlock()
	return time.Since(start), !d.mustRenewBeforeUse(time.Now())
}

// expired handles a 401 access_expired: the request is not resent (its
// outcome is already recorded as unexpected); the device refreshes.
func (g *generator) expired(ctx context.Context, d *device) {
	g.refresh(ctx, d, time.Now())
}
