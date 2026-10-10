package wake_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/database"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/wake"
)

// A device the hub held when a write landed (re-announced through the hub,
// so not pushed), whose connection then closes before it pulled (the app was
// backgrounded, or its network dropped), with no further write in the family:
// it is pushed once its hub window ends (review I1, probe A).
func TestConnectedThenDisconnectedIsPushed(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	live := true
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return live }, Now: func() time.Time { return clock }}
	backgrounded(t, h, tracker, token(0xa1), clock)
	pushTick(t, w)
	write(t, h, tracker, 1)
	if stats := pushTick(t, w); stats.Woken != 1 || stats.Pushed != 0 {
		t.Fatalf("connected tick %+v, want the hub wake only", stats)
	}
	live = false
	for _, step := range []time.Duration{2 * time.Second, time.Minute, 2 * time.Minute, 21 * time.Minute, time.Hour} {
		clock = clock.Add(step)
		pushTick(t, w)
	}
	if got := pusher.count(token(0xa1)); got != 1 {
		t.Fatalf("the device left behind after its connection closed was pushed %d times, want once", got)
	}
	// Still connected at the recheck: nothing, and no further rechecks (the
	// ticks after it do not reach the database).
	h2 := testkit.New(t)
	caretest.Seed(t, h2, caretest.Baby(caretest.BabyA))
	tracker2, pusher2 := wake.NewTracker(0), newFakePusher()
	clock2 := h2.Now
	pool, wire := testkit.CountedPool(t, h2.Maintenance)
	w2 := &wake.Worker{Pool: pool, Tracker: tracker2, Sender: discardSender{}, Pusher: pusher2,
		Connected: func(string) bool { return true }, Now: func() time.Time { return clock2 }}
	backgrounded(t, h2, tracker2, token(0xa2), clock2)
	pushTick(t, w2)
	write(t, h2, tracker2, 1)
	pushTick(t, w2)
	for i, step := range []time.Duration{time.Minute, time.Minute, 21 * time.Minute} {
		clock2 = clock2.Add(step)
		wire.Reset()
		if stats := pushTick(t, w2); stats != (wake.Stats{}) {
			t.Fatalf("a connected device's recheck did %+v, want nothing", stats)
		}
		if i > 0 && wire.Trips() != 0 {
			t.Fatalf("tick %d after the recheck took %d round trips, want none (no further recheck)", i, wire.Trips())
		}
	}
	if pusher2.total() != 0 {
		t.Fatalf("a connected device was pushed: %v", pusher2.sent)
	}
}

// The recheck of a device re-announced while push-due survives an earlier
// rescan of its family (the tracker holds one deferral per family). D is
// re-announced through the hub while push-due (recheck at T0+60 s), but the
// family is rescanned at T0+50 s (another phone held by an APNs pause is
// released at the pause's end) while D still looks connected; the hub notices
// D's dead socket only after that rescan, and there is no further write. D
// is pushed (re-review R1, probe RR-A).
func TestRecheckSurvivesAnEarlierRescanOfTheFamily(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	var mu sync.Mutex
	live := map[string]bool{}
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(device string) bool { mu.Lock(); defer mu.Unlock(); return live[device] }, Now: func() time.Time { return clock }}
	d := backgrounded(t, h, tracker, token(0xa1), clock)
	x := backgrounded(t, h, tracker, token(0xa2), clock)
	y := backgrounded(t, h, tracker, token(0xa3), clock)
	live[d.DeviceID] = true
	pushTick(t, w)
	// T0-10 s: a write D pulls at once; the first push (X or Y) gets a 429
	// (a 60 s pause), the other is released and held to the pause's end.
	write(t, h, tracker, 1)
	s := h.FamilySequence(t)
	tracker.Synced(h.FamilyID, d.DeviceID, s, s, clock)
	first := ""
	pusher.setAnswer(func(value []byte) (apns.Result, error) {
		if first == "" { // the first send goes alone, under the pusher's lock
			first = hex.EncodeToString(value)
			return apns.Result{Status: 429, Reason: "TooManyRequests", Class: apns.Retryable}, nil
		}
		return apns.Result{Status: 200, Class: apns.Accepted}, nil
	})
	if stats := pushTick(t, w); stats.Pushed != 1 || stats.PushFailed != 1 {
		t.Fatalf("T0-10s %+v", stats)
	}
	// The 429'd phone's user opens it: connected and caught up.
	opened, other := x, y
	if first == hex.EncodeToString(token(0xa3)) {
		opened, other = y, x
	}
	mu.Lock()
	live[opened.DeviceID] = true
	mu.Unlock()
	// T0: a write; D is hub-due and push-due (connected: the hub, and a
	// recheck at T0+60 s); the other phone is held by the pause (to T0+50 s).
	clock = clock.Add(10 * time.Second)
	write(t, h, tracker, 2)
	s = h.FamilySequence(t)
	tracker.Synced(h.FamilyID, opened.DeviceID, s, s, clock)
	if stats := pushTick(t, w); stats.Woken != 1 || stats.Pushed != 0 {
		t.Fatalf("T0 %+v", stats)
	}
	// T0+50 s: the pause ends and the held phone is pushed; D still looks
	// connected.
	clock = clock.Add(50 * time.Second)
	if stats := pushTick(t, w); stats.Pushed != 1 {
		t.Fatalf("T0+50s %+v", stats)
	}
	tracker.Synced(h.FamilyID, other.DeviceID, s, s, clock) // woken by its push, it pulled
	mu.Lock()
	live[d.DeviceID] = false // the hub notices D's dead socket only now
	mu.Unlock()
	// D's recheck is due at T0+60 s: the tick at T0+82 s pushes it.
	for _, step := range []time.Duration{2 * time.Second, 30 * time.Second} {
		clock = clock.Add(step)
		pushTick(t, w)
	}
	if got := pusher.count(token(0xa1)); got != 1 {
		t.Fatalf("D, re-announced while its socket was dying, was pushed %d times by T0+82s, want once", got)
	}
	for _, step := range []time.Duration{time.Minute, 3 * time.Minute, 21 * time.Minute, time.Hour} {
		clock = clock.Add(step)
		pushTick(t, w)
	}
	if got := pusher.count(token(0xa1)); got != 1 {
		t.Fatalf("D was pushed %d times for one write, want once", got)
	}
}

// The recheck survives a restart: a new worker's startup backstop finds a
// connected device woken within the minute and still owed a push, and
// rechecks it then; its connection was dying, so it is pushed.
func TestRecheckSurvivesARestart(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	live := true
	connected := func(string) bool { return live }
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: connected, Now: func() time.Time { return clock }}
	backgrounded(t, h, tracker, token(0xa4), clock)
	pushTick(t, w)
	write(t, h, tracker, 1)
	if stats := pushTick(t, w); stats.Woken != 1 || stats.Pushed != 0 {
		t.Fatalf("connected tick %+v, want the hub wake only", stats)
	}
	// The worker restarts 10 s later (its tracker and the recheck are gone);
	// the device still looks connected at the new worker's first tick.
	clock = clock.Add(10 * time.Second)
	restarted := &wake.Worker{Pool: h.Maintenance, Tracker: wake.NewTracker(0), Sender: discardSender{}, Pusher: pusher,
		Connected: connected, Now: func() time.Time { return clock }}
	if stats := pushTick(t, restarted); stats.Pushed != 0 {
		t.Fatalf("restart tick %+v", stats)
	}
	live = false
	for _, step := range []time.Duration{2 * time.Second, time.Minute, 21 * time.Minute} {
		clock = clock.Add(step)
		pushTick(t, restarted)
	}
	if got := pusher.count(token(0xa4)); got != 1 {
		t.Fatalf("after the restart the device was pushed %d times, want once", got)
	}
}

// The post-send statements give back a reservation only if the row still
// carries this tick's (pushed_sequence = seq AND pushed_at = the tick's
// time): a row reserved again between the send and the settle keeps the
// later reservation, both for an undelivered push (owed) and for a push
// never sent (released) (re-review R2).
func TestSettleLeavesAReservationMadeSince(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	a := backgrounded(t, h, tracker, token(0xf1), clock)
	b := backgrounded(t, h, tracker, token(0xf2), clock)
	pushTick(t, w)
	write(t, h, tracker, 1)
	reservedAt, later := clock, clock.Add(time.Second)
	// While Apple has the first push, both rows are reserved again (as a
	// second worker would); Apple answers 503, which pauses the second push.
	pusher.setAnswer(func([]byte) (apns.Result, error) {
		if _, err := h.Maintenance.Exec(context.Background(), `UPDATE device_sync_cursors SET pushed_at = $2
			WHERE family_id = $1 AND pushed_at = $3`, h.FamilyID, later, reservedAt); err != nil {
			t.Error(err)
		}
		return apns.Result{Status: 503, Reason: "ServiceUnavailable", Class: apns.Retryable}, nil
	})
	if stats := pushTick(t, w); stats.Pushed != 1 || stats.PushFailed != 1 {
		t.Fatalf("%+v, want the first push only (its 503 paused the second)", stats)
	}
	sequence := h.FamilySequence(t)
	rows := pushRows(t, h, h.FamilyID)
	for _, device := range []string{a.DeviceID, b.DeviceID} {
		if row := rows[device]; row.Pushed != int64(sequence) || row.PushedAt == nil || !row.PushedAt.Equal(later) {
			t.Fatalf("row %s %+v, want the later reservation kept (sequence %d at %v)", device, row, sequence, later)
		}
	}
}

// A push Apple did not deliver (not sent, 429, a server error, a refused
// provider token or configuration) is owed again: pushed once more for the
// same write when its window ends, never sooner, with no further write. A
// lost answer (Ambiguous) may have been delivered: never repeated (review I2,
// probe B).
func TestUndeliveredPushIsRetriedAfterTheWindow(t *testing.T) {
	for _, test := range []struct {
		name    string
		answer  func([]byte) (apns.Result, error)
		retried bool
	}{
		{"503", answering(apns.Result{Status: 503, Reason: "ServiceUnavailable", Class: apns.Retryable}, nil), true},
		{"429", answering(apns.Result{Status: 429, Reason: "TooManyRequests", Class: apns.Retryable}, nil), true},
		{"not sent", answering(apns.Result{Class: apns.ConfigurationFailure}, errors.Join(apns.ErrNotSent, errors.New("signing failed"))), true},
		{"403 provider token", answering(apns.Result{Status: 403, Reason: "ExpiredProviderToken", Class: apns.ConfigurationFailure}, nil), true},
		{"400 configuration", answering(apns.Result{Status: 400, Reason: "BadTopic", Class: apns.ConfigurationFailure}, nil), true},
		{"ambiguous", answering(apns.Result{Class: apns.Ambiguous}, fmt.Errorf("%w: %w", apns.ErrOutcomeUnknown, apns.ErrCauseTimeout)), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := testkit.New(t)
			caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
			tracker, pusher := wake.NewTracker(0), newFakePusher()
			clock := h.Now
			w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
				Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
			device := backgrounded(t, h, tracker, token(0xb1), clock)
			pushTick(t, w)
			write(t, h, tracker, 1)
			sentAt := clock
			pusher.setAnswer(test.answer)
			if stats := pushTick(t, w); stats.PushFailed != 1 {
				t.Fatalf("first %+v", stats)
			}
			pusher.setAnswer(nil)
			if row := pushRows(t, h, h.FamilyID)[device.DeviceID]; row.PushedAt == nil || !row.PushedAt.Equal(sentAt) {
				t.Fatalf("reservation %+v, want its time kept (no second push inside the window)", row)
			}
			// Inside the window: nothing. (An owed push gets no forced rescan
			// anywhere: its own deferral must bring it back; the kept pushed_at
			// above is what holds it off inside the window on any rescan.)
			clock = sentAt.Add(wake.APNsInterval - time.Second)
			if !test.retried {
				tracker.Changed(h.FamilyID, h.FamilySequence(t))
			}
			pushTick(t, w)
			if got := pusher.count(token(0xb1)); got != 1 {
				t.Fatalf("pushed %d times inside the window, want once", got)
			}
			// When the window ends, with no further write and (for an owed
			// push) no rescan asked; a lost answer is not repeated even on one.
			clock = sentAt.Add(wake.APNsInterval)
			if !test.retried {
				tracker.Changed(h.FamilyID, h.FamilySequence(t))
			}
			pushTick(t, w)
			clock = clock.Add(time.Hour)
			pushTick(t, w)
			want := 1
			if test.retried {
				want = 2
			}
			if got := pusher.count(token(0xb1)); got != want {
				t.Fatalf("pushed %d times for one write, want %d", got, want)
			}
		})
	}
}

// One phone registered on two device rows (one token) gets one push a tick
// for both, and both rows are reserved by it; a refusal of that token
// forgets it on both rows (review M1, probe C).
func TestDuplicateTokenIsPushedOnce(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	first := backgrounded(t, h, tracker, token(0xc1), clock)
	second := backgrounded(t, h, tracker, token(0xc1), clock)
	pushTick(t, w)
	write(t, h, tracker, 1)
	if stats := pushTick(t, w); stats.Pushed != 1 {
		t.Fatalf("%+v, want one send", stats)
	}
	if got := pusher.count(token(0xc1)); got != 1 {
		t.Fatalf("one token on two device rows got %d pushes in one tick", got)
	}
	rows := pushRows(t, h, h.FamilyID)
	if rows[first.DeviceID].PushedAt == nil || rows[second.DeviceID].PushedAt == nil {
		t.Fatalf("rows %+v, want both reserved by the one send", rows)
	}
	// The next write after the window: one push again; Apple's 503 owes it
	// again on both rows (each keeps the push's time, and gets back the
	// sequence it had).
	clock = clock.Add(wake.APNsInterval)
	pusher.setAnswer(answering(apns.Result{Status: 503, Reason: "ServiceUnavailable", Class: apns.Retryable}, nil))
	write(t, h, tracker, 2)
	if stats := pushTick(t, w); stats.Pushed != 1 || pusher.count(token(0xc1)) != 2 {
		t.Fatalf("%+v, %d sends: want one more", stats, pusher.count(token(0xc1)))
	}
	owed := pushRows(t, h, h.FamilyID)
	for _, device := range []string{first.DeviceID, second.DeviceID} {
		if row := owed[device]; row.Pushed != rows[device].Pushed || row.PushedAt == nil || !row.PushedAt.Equal(clock) {
			t.Fatalf("row %s %+v after a 503, want the sequence given back (%d) and the time kept", device, row, rows[device].Pushed)
		}
	}
	// When that window ends, one push for both rows; Apple's refusal forgets
	// the token on both.
	clock = clock.Add(wake.APNsInterval)
	pusher.setAnswer(answering(apns.Result{Status: 410, Reason: "Unregistered", Class: apns.InvalidToken}, nil))
	if stats := pushTick(t, w); stats.Pushed != 1 || pusher.count(token(0xc1)) != 3 {
		t.Fatalf("%+v, %d sends: want the owed push, once", stats, pusher.count(token(0xc1)))
	}
	if storedToken(t, h, first.DeviceID) != nil || storedToken(t, h, second.DeviceID) != nil {
		t.Fatal("a refused token was kept on one of its rows")
	}
}

// A send the client refused before writing is logged as an error once a
// tick ("apns wake not sent") and is not counted as a send (review M4).
func TestNotSentIsLoggedAndNotCountedAsSent(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher, logs := wake.NewTracker(0), newFakePusher(), &countingHandler{}
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher, Logger: slog.New(logs),
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	backgrounded(t, h, tracker, token(0xd1), clock)
	pushTick(t, w)
	write(t, h, tracker, 1)
	pusher.setAnswer(answering(apns.Result{Class: apns.ConfigurationFailure}, errors.Join(apns.ErrNotSent, errors.New("signing failed"))))
	if stats := pushTick(t, w); stats.Pushed != 0 || stats.PushFailed != 1 {
		t.Fatalf("%+v, want no send made and one failed", stats)
	}
	if logs.count("apns wake not sent") != 1 || logs.count("apns wakes backing off") != 0 || logs.count("apns configuration refused") != 0 {
		t.Fatalf("logs %v, want one apns wake not sent", logs.records)
	}
}

// slotPusher takes the pool's only worker slot while Apple answers and
// holds it: the tick's post-send statement must wait for a slot.
type slotPusher struct {
	slots   *database.WorkerSlots
	release func()
}

func (p *slotPusher) Wake(ctx context.Context, _ apns.Environment, _ []byte) (apns.Result, error) {
	release, err := p.slots.Acquire(ctx)
	if err != nil {
		return apns.Result{}, err
	}
	p.release = release
	return apns.Result{Status: 410, Reason: "Unregistered", Class: apns.InvalidToken}, nil
}

// With Slots and a Pusher, the post-send statement (here forgetting a
// refused token) runs inside a worker slot: with every slot held it waits,
// and gives up with the tick's context, without touching the pool (review
// M5).
func TestPushSettleRunsInsideAWorkerSlot(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	pool, err := database.OpenWithMax(t.Context(), h.Maintenance.Config().ConnString(), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	slots, err := database.NewWorkerSlots(pool)
	if err != nil {
		t.Fatal(err)
	}
	tracker, pusher := wake.NewTracker(0), &slotPusher{slots: slots}
	clock := h.Now
	w := &wake.Worker{Pool: pool, Slots: slots, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	device := backgrounded(t, h, tracker, token(0xe1), clock)
	pushTick(t, w)
	write(t, h, tracker, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = w.Tick(ctx)
	if pusher.release == nil {
		t.Fatal("the pusher was not called")
	}
	defer pusher.release()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("with every slot held after the send: %v, want the settle to wait for one", err)
	}
	if storedToken(t, h, device.DeviceID) == nil {
		t.Fatal("the refused token was forgotten outside a worker slot")
	}
	// The settle was kept (final review R2-I1): once a slot is free, the next
	// tick forgets the token, with nothing else to do.
	pusher.release()
	clock = clock.Add(time.Minute)
	pushTick(t, w)
	if storedToken(t, h, device.DeviceID) != nil {
		t.Fatal("the refused token was kept after its settle could run")
	}
}
