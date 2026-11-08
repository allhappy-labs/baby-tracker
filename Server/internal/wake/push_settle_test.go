package wake_test

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/apns"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/database"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/wake"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// slotHoldingPusher records each wake by hex token. Its first wake takes the
// pool's only worker slot (as an export iteration or a purge step would)
// and keeps it until the test frees it; Apple answers that first wake 503
// (its answer pauses the tick, so the tick's other pushes never start) and
// accepts every later one.
type slotHoldingPusher struct {
	slots   *database.WorkerSlots
	mu      sync.Mutex
	sent    map[string]int
	first   string
	release func()
}

func (p *slotHoldingPusher) Wake(ctx context.Context, _ apns.Environment, value []byte) (apns.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := hex.EncodeToString(value)
	p.sent[key]++
	if p.first != "" {
		return apns.Result{Status: 200, Class: apns.Accepted}, nil
	}
	p.first = key
	release, err := p.slots.Acquire(ctx)
	if err != nil {
		return apns.Result{}, err
	}
	p.release = release
	return apns.Result{Status: 503, Reason: "ServiceUnavailable", Class: apns.Retryable}, nil
}

func (p *slotHoldingPusher) count(value []byte) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent[hex.EncodeToString(value)]
}

// free gives the held slot back.
func (p *slotHoldingPusher) free() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.release != nil {
		p.release()
	}
}

// oneSlotPool is a maintenance pool of two connections: one worker slot.
func oneSlotPool(t *testing.T, h *testkit.Harness) (*pgxpool.Pool, *database.WorkerSlots) {
	t.Helper()
	pool, err := database.OpenWithMax(t.Context(), h.Maintenance.Config().ConnString(), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	slots, err := database.NewWorkerSlots(pool)
	if err != nil {
		t.Fatal(err)
	}
	return pool, slots
}

func tickWithin(w *wake.Worker, bound time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	_, err := w.Tick(ctx)
	return err
}

// The post-send settle is durable (final review R2-I1). The only worker slot
// is held past the tick's deadline while Apple answers the first push 503
// and the second push never starts, so the settle cannot run in its tick.
// The settle is kept; it survives a later tick that also fails for want of
// the slot; once the slot is free, the next tick applies it, with no
// further write and no rescan asked: the push never started is sent at
// once, and the push Apple refused is sent again when its window ends.
func TestASettleWithoutAWorkerSlotIsReplayed(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	pool, slots := oneSlotPool(t, h)
	tracker := wake.NewTracker(0)
	pusher := &slotHoldingPusher{slots: slots, sent: map[string]int{}}
	var clockMu sync.Mutex
	clock := h.Now
	now := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	advance := func(at time.Time) { clockMu.Lock(); defer clockMu.Unlock(); clock = at }
	w := &wake.Worker{Pool: pool, Slots: slots, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: now}
	backgrounded(t, h, tracker, token(0x71), clock)
	backgrounded(t, h, tracker, token(0x72), clock)
	pushTick(t, w)
	write(t, h, tracker, 1)
	sentAt := now()
	if err := tickWithin(w, 2*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("with the only slot held after the send: %v, want the settle to give up with the tick", err)
	}
	refused, unsent := token(0x71), token(0x72)
	if pusher.first == hex.EncodeToString(unsent) {
		refused, unsent = unsent, refused
	}
	if pusher.count(refused) != 1 || pusher.count(unsent) != 0 {
		t.Fatalf("pushes %v, want the first alone", pusher.sent)
	}
	if totals := w.Totals(); totals.SettlesPending != 2 || totals.SettlesFailed != 1 {
		t.Fatalf("totals %+v, want both rows kept and one failed settle", totals)
	}
	// A later tick, the slot still held: it fails too, and keeps the settle.
	advance(sentAt.Add(30 * time.Second))
	if err := tickWithin(w, 500*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a tick with the slot still held: %v, want it to fail", err)
	}
	if totals := w.Totals(); totals.SettlesPending != 2 || totals.SettlesFailed != 2 {
		t.Fatalf("totals %+v, want both rows still kept and two failed settles", totals)
	}
	pusher.free()
	advance(sentAt.Add(time.Minute))
	pushTick(t, w)
	if pusher.count(unsent) != 1 || pusher.count(refused) != 1 {
		t.Fatalf("a minute later, the slot free: pushes %v, want the push never started sent (and no second push inside the refused one's window)", pusher.sent)
	}
	if totals := w.Totals(); totals.SettlesPending != 0 || totals.SettlesFailed != 2 {
		t.Fatalf("totals %+v, want the kept settle applied", totals)
	}
	for _, at := range []time.Duration{2 * time.Minute, wake.APNsInterval - time.Second} {
		advance(sentAt.Add(at))
		pushTick(t, w)
	}
	if pusher.count(refused) != 1 {
		t.Fatalf("inside the window: pushes %v", pusher.sent)
	}
	for _, at := range []time.Duration{wake.APNsInterval + 2*time.Minute, wake.APNsInterval + time.Hour + 2*time.Minute} {
		advance(sentAt.Add(at))
		pushTick(t, w)
	}
	if pusher.count(refused) != 2 || pusher.count(unsent) != 1 {
		t.Fatalf("after the window: pushes %v, want the refused push sent once more and nothing else", pusher.sent)
	}
}

// A kept settle survives a tick that scanned its family while the settle
// still could not apply (here the settle waits past its lock timeout on
// rows another transaction holds, while the tick's own scans take no lock
// there): once it applies, its family is scanned again, so the push never
// started is sent at once and the push Apple refused when its window ends,
// with no further write.
func TestAKeptSettleRescansItsFamilyOnceItApplies(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	backgrounded(t, h, tracker, token(0x75), clock)
	backgrounded(t, h, tracker, token(0x76), clock)
	pushTick(t, w)
	write(t, h, tracker, 1)
	sentAt := clock
	var holder pgx.Tx
	refused := ""
	pusher.setAnswer(func(value []byte) (apns.Result, error) {
		if refused == "" {
			refused = hex.EncodeToString(value)
			var err error
			if holder, err = h.Maintenance.Begin(context.Background()); err != nil {
				t.Error(err)
			} else if _, err := holder.Exec(context.Background(), `SELECT 1 FROM device_sync_cursors WHERE family_id = $1 FOR UPDATE`, h.FamilyID); err != nil {
				t.Error(err)
			}
			return apns.Result{Status: 503, Reason: "ServiceUnavailable", Class: apns.Retryable}, nil
		}
		return apns.Result{Status: 200, Class: apns.Accepted}, nil
	})
	if _, err := w.Tick(t.Context()); err == nil {
		t.Fatal("the settle applied over rows another transaction holds")
	}
	defer func() {
		if holder != nil {
			_ = holder.Rollback(context.Background())
		}
	}()
	// A tick that scans the family (its pause-end hold is due) while the
	// settle still cannot apply: it fails the settle again, and finds nothing
	// due (both rows still carry the reservation).
	clock = sentAt.Add(30 * time.Second)
	if stats, err := w.Tick(t.Context()); err == nil || stats.Pushed != 0 {
		t.Fatalf("a tick with the rows still held: %+v %v, want the settle failed and nothing pushed", stats, err)
	}
	if totals := w.Totals(); totals.SettlesFailed != 2 || totals.SettlesPending != 2 {
		t.Fatalf("totals %+v, want two failed settles and both rows kept", totals)
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	holder = nil
	unsent := token(0x75)
	if refused == hex.EncodeToString(unsent) {
		unsent = token(0x76)
	}
	clock = sentAt.Add(time.Minute)
	pushTick(t, w)
	clock = sentAt.Add(2 * time.Minute)
	pushTick(t, w)
	if pusher.count(unsent) != 1 || pusher.total() != 2 {
		t.Fatalf("once the settle applied: pushes %v, want the push never started sent", pusher.sent)
	}
	clock = sentAt.Add(wake.APNsInterval + time.Minute)
	pushTick(t, w)
	if pusher.total() != 3 || pusher.count(unsent) != 1 {
		t.Fatalf("after the window: pushes %v, want the refused push sent once more", pusher.sent)
	}
}

// Settles kept from two ticks apply together, each row guarded by its own
// tick's reservation: an earlier tick's owed and released rows (reserved at
// T) and a later tick's owed row (reserved at T+1 min) all give back what
// their tick took.
func TestKeptSettlesOfTwoTicksApplyTogether(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	first := backgrounded(t, h, tracker, token(0x78), clock)
	second := backgrounded(t, h, tracker, token(0x79), clock)
	late, err := h.NewDevice("caregiver") // registers its token only after the first tick
	if err != nil {
		t.Fatal(err)
	}
	seq := h.FamilySequence(t)
	tracker.Synced(h.FamilyID, late.DeviceID, seq, seq, clock)
	pushTick(t, w)
	before := pushRows(t, h, h.FamilyID)
	write(t, h, tracker, 1)
	// Tick at T: the first push is refused (503) and its row is held by
	// another transaction, so the settle (owe it, release the other) cannot
	// apply.
	var holder pgx.Tx
	sends := 0
	refused := ""
	pusher.setAnswer(func(value []byte) (apns.Result, error) {
		sends++
		switch sends {
		case 1:
			refused = hex.EncodeToString(value)
			device := first.DeviceID
			if refused == hex.EncodeToString(token(0x79)) {
				device = second.DeviceID
			}
			var err error
			if holder, err = h.Maintenance.Begin(context.Background()); err != nil {
				t.Error(err)
			} else if _, err := holder.Exec(context.Background(), `SELECT 1 FROM device_sync_cursors WHERE family_id = $1 AND device_id = $2 FOR UPDATE`, h.FamilyID, device); err != nil {
				t.Error(err)
			}
		case 2:
			// Tick at T+1 min: its push is the late device's; the row is free
			// again by the time this tick settles.
			if err := holder.Rollback(context.Background()); err != nil {
				t.Error(err)
			}
		}
		return apns.Result{Status: 503, Reason: "ServiceUnavailable", Class: apns.Retryable}, nil
	})
	if _, err := w.Tick(t.Context()); err == nil {
		t.Fatal("the settle applied over a row another transaction holds")
	}
	registerToken(t, h, late, token(0x7a), apns.Production)
	clock = clock.Add(time.Minute)
	tracker.Changed(h.FamilyID, h.FamilySequence(t))
	if stats, err := w.Tick(t.Context()); err != nil || stats.Pushed != 1 || pusher.count(token(0x7a)) != 1 {
		t.Fatalf("the later tick: %+v %v, want the late device's push and every kept settle applied", stats, err)
	}
	rows := pushRows(t, h, h.FamilyID)
	for _, device := range []string{first.DeviceID, second.DeviceID, late.DeviceID} {
		if rows[device].Pushed != before[device].Pushed {
			t.Fatalf("row %s %+v, want its sequence given back (%d)", device, rows[device], before[device].Pushed)
		}
	}
}

// Settles apply one by one (C35): three kept settles, the second's UPDATE
// fails (its row is held past the lock timeout), and settles 1 and 3 still
// apply; the second stays kept, counted once in SettlesFailed, and applies
// once its row is free.
func TestOneFailingSettleDoesNotBlockTheOthers(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	var devices []string
	for i := range 3 {
		devices = append(devices, backgrounded(t, h, tracker, token(byte(0x81+i)), clock).DeviceID)
	}
	write(t, h, tracker, 1)
	if stats := pushTick(t, w); stats.Pushed != 3 {
		t.Fatalf("%+v, want the three devices pushed (reserved)", stats)
	}
	sequence := uint64(h.FamilySequence(t))
	for _, device := range devices {
		if row := pushRows(t, h, h.FamilyID)[device]; row.Pushed != int64(sequence) || row.PushedAt == nil || !row.PushedAt.Equal(clock) {
			t.Fatalf("row %s %+v, want reserved at %d", device, row, sequence)
		}
		w.KeepReleaseForTest(h.FamilyID, []string{device}, sequence, clock)
	}
	holder, err := h.Maintenance.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(t.Context(), `SELECT 1 FROM device_sync_cursors WHERE family_id = $1 AND device_id = $2 FOR UPDATE`, h.FamilyID, devices[1]); err != nil {
		t.Fatal(err)
	}
	applied, err := w.ApplySettlesForTest(t.Context())
	if err == nil || applied != 2 {
		t.Fatalf("applied %d (%v), want settles 1 and 3 applied and the second's failure", applied, err)
	}
	rows := pushRows(t, h, h.FamilyID)
	for i, device := range devices {
		released := rows[device].Pushed == 0 && rows[device].PushedAt == nil
		if released != (i != 1) {
			t.Fatalf("settle %d: row %+v, want only the second still reserved", i+1, rows[device])
		}
	}
	if totals := w.Totals(); totals.SettlesFailed != 1 || totals.SettlesPending != 1 {
		t.Fatalf("totals %+v, want one failed settle and its one row kept", totals)
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if applied, err := w.ApplySettlesForTest(t.Context()); err != nil || applied != 1 {
		t.Fatalf("once its row is free: applied %d (%v), want the kept settle", applied, err)
	}
	if row := pushRows(t, h, h.FamilyID)[devices[1]]; row.Pushed != 0 || row.PushedAt != nil {
		t.Fatalf("row %+v, want the second settle applied", row)
	}
	if totals := w.Totals(); totals.SettlesFailed != 1 || totals.SettlesPending != 0 {
		t.Fatalf("totals %+v, want nothing kept", totals)
	}
}

// SettlesFailed counts the settles that were tried and failed, not those
// left untried: the first settle waits on its held row until the context
// ends, so the second is kept without being tried. Both stay kept; one
// failure is counted.
func TestUntriedSettlesAreNotCountedAsFailed(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	var devices []string
	for i := range 2 {
		devices = append(devices, backgrounded(t, h, tracker, token(byte(0x8a+i)), clock).DeviceID)
	}
	write(t, h, tracker, 1)
	if stats := pushTick(t, w); stats.Pushed != 2 {
		t.Fatalf("%+v, want both devices pushed (reserved)", stats)
	}
	sequence := uint64(h.FamilySequence(t))
	for _, device := range devices {
		w.KeepReleaseForTest(h.FamilyID, []string{device}, sequence, clock)
	}
	holder, err := h.Maintenance.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(t.Context(), `SELECT 1 FROM device_sync_cursors WHERE family_id = $1 AND device_id = $2 FOR UPDATE`, h.FamilyID, devices[0]); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if applied, err := w.ApplySettlesForTest(ctx); err == nil || applied != 0 {
		t.Fatalf("applied %d (%v), want the first settle failed at the context's end and the second untried", applied, err)
	}
	if totals := w.Totals(); totals.SettlesFailed != 1 || totals.SettlesPending != 2 {
		t.Fatalf("totals %+v, want one failure counted and both settles kept", totals)
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if applied, err := w.ApplySettlesForTest(t.Context()); err != nil || applied != 2 {
		t.Fatalf("once the row is free: applied %d (%v), want both", applied, err)
	}
	if totals := w.Totals(); totals.SettlesFailed != 1 || totals.SettlesPending != 0 {
		t.Fatalf("totals %+v, want nothing kept and still one failure", totals)
	}
}

// A settle still kept when the worker stops is applied on the way out,
// within its own short bound: a deploy (a graceful restart) loses no
// settle.
func TestRunAppliesKeptSettlesWhenItStops(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	pool, slots := oneSlotPool(t, h)
	tracker := wake.NewTracker(0)
	pusher := &slotHoldingPusher{slots: slots, sent: map[string]int{}}
	clock := h.Now
	w := &wake.Worker{Pool: pool, Slots: slots, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }, Logger: slog.New(slog.DiscardHandler),
		Interval: 10 * time.Millisecond}
	first := backgrounded(t, h, tracker, token(0x73), clock)
	second := backgrounded(t, h, tracker, token(0x74), clock)
	pushTick(t, w)
	before := pushRows(t, h, h.FamilyID)
	write(t, h, tracker, 1)
	if err := tickWithin(w, time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("with the only slot held after the send: %v", err)
	}
	pusher.free()
	// Run never ticks (the node is not ready); stopping it applies the kept
	// settle.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx, func(context.Context) error { return errors.New("not ready") }) }()
	time.Sleep(50 * time.Millisecond)
	if rows := pushRows(t, h, h.FamilyID); rows[first.DeviceID].PushedAt == nil || rows[second.DeviceID].PushedAt == nil {
		t.Fatalf("rows %+v, want both still reserved before the stop", rows)
	}
	cancel()
	<-done
	rows := pushRows(t, h, h.FamilyID)
	for _, device := range []string{first.DeviceID, second.DeviceID} {
		if rows[device].Pushed != before[device].Pushed {
			t.Fatalf("row %s %+v after the stop, want its sequence given back (%d)", device, rows[device], before[device].Pushed)
		}
	}
}
