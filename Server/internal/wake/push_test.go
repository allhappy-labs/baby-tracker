package wake_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/apns"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/config"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/wake"
)

// discardSender is a hub that reaches nobody.
type discardSender struct{}

func (discardSender) Wake(context.Context, []wake.Wake) error { return nil }

// fakePusher records each wake by hex token and answers as told (Accepted by
// default). It tracks how many wakes run at once.
type fakePusher struct {
	mu       sync.Mutex
	sent     map[string]int // hex token -> pushes
	envs     map[string]apns.Environment
	answer   func(token []byte) (apns.Result, error)
	delay    time.Duration
	inFlight atomic.Int32
	peak     atomic.Int32
}

func newFakePusher() *fakePusher {
	return &fakePusher{sent: map[string]int{}, envs: map[string]apns.Environment{}}
}

func (p *fakePusher) Wake(_ context.Context, env apns.Environment, token []byte) (apns.Result, error) {
	now := p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	for {
		peak := p.peak.Load()
		if now <= peak || p.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := hex.EncodeToString(token)
	p.sent[key]++
	p.envs[key] = env
	if p.answer != nil {
		return p.answer(token)
	}
	return apns.Result{Status: 200, Class: apns.Accepted}, nil
}

func (p *fakePusher) count(token []byte) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent[hex.EncodeToString(token)]
}

func (p *fakePusher) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, n := range p.sent {
		total += n
	}
	return total
}

func (p *fakePusher) setAnswer(answer func([]byte) (apns.Result, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answer = answer
}

func answering(result apns.Result, err error) func([]byte) (apns.Result, error) {
	return func([]byte) (apns.Result, error) { return result, err }
}

func token(fill byte) []byte { return bytes.Repeat([]byte{fill}, 32) }

// registerToken stores p's device token as the push-token route does (the
// device's own row), through the migrator under the authority flag.
func registerToken(t *testing.T, h *testkit.Harness, p access.Principal, value []byte, env apns.Environment) {
	t.Helper()
	h.AuthorityExec(t, `INSERT INTO device_push_tokens (device_id, account_id, environment, token, registered_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (device_id) DO UPDATE SET environment = EXCLUDED.environment, token = EXCLUDED.token, registered_at = EXCLUDED.registered_at`,
		p.DeviceID, p.AccountID, string(env), value)
}

// storedToken is the device's registered token (nil when none).
func storedToken(t *testing.T, h *testkit.Harness, device string) []byte {
	t.Helper()
	tx, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority', '1', true)`); err != nil {
		t.Fatal(err)
	}
	var value []byte
	if err := tx.QueryRow(t.Context(), `SELECT token FROM device_push_tokens WHERE device_id = $1`, device).Scan(&value); err != nil {
		return nil
	}
	return value
}

type pushRow struct {
	Pushed   int64
	PushedAt *time.Time
	Woken    int64
	WokenAt  *time.Time
}

func pushRows(t *testing.T, h *testkit.Harness, family string) map[string]pushRow {
	t.Helper()
	rows, err := h.Maintenance.Query(t.Context(), `SELECT device_id::text, pushed_sequence, pushed_at, woken_sequence, woken_at
		FROM device_sync_cursors WHERE family_id = $1`, family)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[string]pushRow{}
	for rows.Next() {
		var device string
		var row pushRow
		if err := rows.Scan(&device, &row.Pushed, &row.PushedAt, &row.Woken, &row.WokenAt); err != nil {
			t.Fatal(err)
		}
		result[device] = row
	}
	return result
}

// backgrounded is a new device of the family's caregiver (a family session
// there), registered for pushes, at the family's high water as of now.
func backgrounded(t *testing.T, h *testkit.Harness, tracker *wake.Tracker, value []byte, at time.Time) access.Principal {
	t.Helper()
	p, err := h.NewDevice("caregiver")
	if err != nil {
		t.Fatal(err)
	}
	registerToken(t, h, p, value, apns.Production)
	seq := h.FamilySequence(t)
	tracker.Synced(h.FamilyID, p.DeviceID, seq, seq, at)
	return p
}

// write is one care write and its committed change.
func write(t *testing.T, h *testkit.Harness, tracker *wake.Tracker, n int) {
	t.Helper()
	caretest.Seed(t, h, caretest.Feed(caretest.BabyA, n))
	tracker.Changed(h.FamilyID, h.FamilySequence(t))
}

func pushTick(t *testing.T, w *wake.Worker) wake.Stats {
	t.Helper()
	stats, err := w.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

func TestWakeWorkerPushBudgetNeverStorms(t *testing.T) { // Review Focus 5 (APNs)
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	// Four phones of one family: owner and caregiver hold live connections; two
	// more caregivers are backgrounded with registered tokens.
	sleeper1, sleeper2 := h.AddMember(t, "sleeper1", "caregiver"), h.AddMember(t, "sleeper2", "caregiver")
	tokens := map[string][]byte{sleeper1.DeviceID: token(0x51), sleeper2.DeviceID: token(0x52)}
	registerToken(t, h, sleeper1, tokens[sleeper1.DeviceID], apns.Production)
	registerToken(t, h, sleeper2, tokens[sleeper2.DeviceID], apns.Production)
	// The connected phones also hold tokens: they must still never be pushed.
	registerToken(t, h, h.Owner, token(0x53), apns.Production)
	registerToken(t, h, h.Caregiver, token(0x54), apns.Production)
	connected := map[string]bool{h.Owner.DeviceID: true, h.Caregiver.DeviceID: true}
	tracker, pusher, sender := wake.NewTracker(0), newFakePusher(), &recordingSender{}
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: sender, Pusher: pusher,
		Connected: func(device string) bool { return connected[device] }, Now: func() time.Time { return clock }}
	seq := h.FamilySequence(t)
	for _, p := range []access.Principal{h.Owner, h.Caregiver, sleeper1, sleeper2} {
		tracker.Synced(h.FamilyID, p.DeviceID, seq, seq, clock)
	}
	for i := range 120 { // a write every 30 s for an hour
		write(t, h, tracker, i)
		stats, err := w.Tick(t.Context())
		if err != nil || stats.Pushed > wake.MaxPushesPerTick {
			t.Fatalf("tick %d: %+v %v", i, stats, err)
		}
		clock = clock.Add(30 * time.Second)
	}
	for device, value := range tokens {
		if got := pusher.count(value); got != 3 {
			t.Fatalf("backgrounded device %s pushed %d times in an hour, want 3 (one per 20 minutes)", device, got)
		}
	}
	if pusher.total() != 6 {
		t.Fatalf("a connected device was pushed: %v", pusher.sent)
	}
	// The connected phones were re-announced through the hub, at most once a
	// minute; the backgrounded ones never were.
	woken := sender.woken()
	if woken[sleeper1.DeviceID] != 0 || woken[sleeper2.DeviceID] != 0 || woken[h.Owner.DeviceID] == 0 || woken[h.Owner.DeviceID] > 60 {
		t.Fatalf("hub wakes %v", woken)
	}
	// A device whose member was removed is never pushed again; the other one is, 20 minutes on.
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), sleeper2.AccountID); err != nil {
		t.Fatal(err)
	}
	removedBefore := pusher.count(tokens[sleeper2.DeviceID])
	clock = clock.Add(wake.APNsInterval)
	write(t, h, tracker, 5000)
	pushTick(t, w)
	if got := pusher.count(tokens[sleeper2.DeviceID]); got != removedBefore {
		t.Fatal("a removed member's device was pushed")
	}
	if got := pusher.count(tokens[sleeper1.DeviceID]); got != 4 {
		t.Fatalf("the remaining backgrounded device: %d pushes, want 4", got)
	}
}

// TestWakeWorkerNeverStorms through both channels: 100 writes in 50 s with
// four lagging devices, two connected and two backgrounded with tokens. The
// hub is told once and wakes each connected device once; each backgrounded
// device is pushed once and never through the hub; a connected device is
// never pushed. A minute on, the connected devices are woken again; the
// backgrounded ones are not pushed again inside their 20 minutes.
func TestWakeWorkerNeverStormsThroughTheHubAndAPNs(t *testing.T) { // Review Focus 5
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher, sender := wake.NewTracker(0), newFakePusher(), &recordingSender{}
	clock := h.Now
	sleepers := []access.Principal{backgrounded(t, h, tracker, token(0x91), clock), backgrounded(t, h, tracker, token(0x92), clock)}
	registerToken(t, h, h.Owner, token(0x93), apns.Production)
	connected := map[string]bool{h.Owner.DeviceID: true, h.Caregiver.DeviceID: true}
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: sender, Pusher: pusher,
		Connected: func(device string) bool { return connected[device] }, Now: func() time.Time { return clock }}
	seq := h.FamilySequence(t)
	for _, p := range []access.Principal{h.Owner, h.Caregiver} {
		tracker.Synced(h.FamilyID, p.DeviceID, seq, seq, clock)
	}
	pushTick(t, w)
	for i := range 100 { // 100 writes over 50 s
		write(t, h, tracker, i)
		clock = clock.Add(500 * time.Millisecond)
		stats, err := w.Tick(t.Context())
		if err != nil || stats.Woken > 500 || stats.Pushed > wake.MaxPushesPerTick {
			t.Fatalf("tick %d: %+v %v", i, stats, err)
		}
	}
	woken := sender.woken()
	if woken[h.Owner.DeviceID] != 1 || woken[h.Caregiver.DeviceID] != 1 || len(woken) != 2 || sender.callCount() != 1 {
		t.Fatalf("hub wakes %v over %d calls, want each connected device once in one call", woken, sender.callCount())
	}
	if pusher.count(token(0x91)) != 1 || pusher.count(token(0x92)) != 1 || pusher.total() != 2 {
		t.Fatalf("pushes %v, want each backgrounded device once and no connected device", pusher.sent)
	}
	clock = clock.Add(time.Minute)
	write(t, h, tracker, 1000)
	pushTick(t, w)
	if woken := sender.woken(); woken[h.Owner.DeviceID] != 2 || woken[h.Caregiver.DeviceID] != 2 || woken[sleepers[0].DeviceID] != 0 {
		t.Fatalf("a minute on: hub wakes %v", woken)
	}
	if pusher.total() != 2 {
		t.Fatalf("a minute on: pushed again inside 20 minutes: %v", pusher.sent)
	}
}

// Review Focus 5 at the tick: 250 backgrounded devices behind at once get at
// most MaxPushesPerTick pushes a tick and at most 8 at a time; the devices
// over the budget are not marked pushed and are pushed on the next ticks
// without a further write, each once.
func TestPushesAreBoundedPerTickAndInFlight(t *testing.T) {
	if wake.MaxPushesPerTick != 100 || wake.APNsInterval != 20*time.Minute {
		t.Fatalf("push bounds %d per tick, one per %v", wake.MaxPushesPerTick, wake.APNsInterval)
	}
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	pusher.delay = 2 * time.Millisecond
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	const devices = 250
	tokens := make(map[string][]byte, devices)
	seq := h.FamilySequence(t)
	for i := range devices {
		p, err := h.NewDevice("caregiver")
		if err != nil {
			t.Fatal(err)
		}
		value := []byte(fmt.Sprintf("token-%04d-padding-to-32-bytes!!", i))
		registerToken(t, h, p, value, apns.Production)
		tokens[p.DeviceID] = value
		tracker.Synced(h.FamilyID, p.DeviceID, seq, seq, clock)
	}
	pushTick(t, w) // flushed at the high water
	write(t, h, tracker, 1)
	stats := pushTick(t, w)
	if stats.Pushed != wake.MaxPushesPerTick || stats.PushFailed != 0 {
		t.Fatalf("first tick %+v, want %d pushes", stats, wake.MaxPushesPerTick)
	}
	rows := pushRows(t, h, h.FamilyID)
	reserved := 0
	for device := range tokens {
		row := rows[device]
		switch {
		case row.PushedAt != nil:
			reserved++
		case row.Pushed != 0:
			t.Fatalf("an unpushed device was marked pushed: %+v", row)
		}
	}
	if reserved != wake.MaxPushesPerTick {
		t.Fatalf("%d devices marked pushed, want the %d sent", reserved, wake.MaxPushesPerTick)
	}
	// No further write: the held devices are pushed on the next ticks.
	for range 3 {
		clock = clock.Add(2 * time.Second)
		if stats := pushTick(t, w); stats.Pushed > wake.MaxPushesPerTick {
			t.Fatalf("tick %+v", stats)
		}
	}
	for device, value := range tokens {
		if got := pusher.count(value); got != 1 {
			t.Fatalf("device %s pushed %d times, want once", device, got)
		}
	}
	if peak := pusher.peak.Load(); peak > 8 || peak < 2 {
		t.Fatalf("%d pushes in flight at once, want at most 8 (and sent concurrently)", peak)
	}
	// Twenty minutes on with no write, nobody is pushed again.
	clock = clock.Add(wake.APNsInterval)
	tracker.Changed(h.FamilyID, h.FamilySequence(t))
	if stats := pushTick(t, w); stats.Pushed != 0 {
		t.Fatalf("pushed again for the same sequence: %+v", stats)
	}
}

// G1: a write that lands inside a backgrounded device's push window is
// pushed once that window ends, with no further write.
func TestTrailingPushAfterTheWindowWithoutAFurtherWrite(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	sleeper := backgrounded(t, h, tracker, token(0x71), clock)
	write(t, h, tracker, 1)
	if stats := pushTick(t, w); stats.Pushed != 1 {
		t.Fatalf("first write %+v, want a push", stats)
	}
	pushedAt := clock
	clock = clock.Add(5 * time.Minute)
	write(t, h, tracker, 2) // the family's last write, inside the window
	trailing := h.FamilySequence(t)
	if stats := pushTick(t, w); stats.Pushed != 0 {
		t.Fatalf("pushed inside the window: %+v", stats)
	}
	for _, at := range []time.Duration{6 * time.Minute, 19 * time.Minute, 20*time.Minute - time.Second} {
		clock = pushedAt.Add(at)
		if stats := pushTick(t, w); stats.Pushed != 0 {
			t.Fatalf("pushed %v after the first push: %+v", at, stats)
		}
	}
	clock = pushedAt.Add(wake.APNsInterval)
	if stats := pushTick(t, w); stats.Pushed != 1 {
		t.Fatalf("the trailing write was not pushed when the window ended: %+v", stats)
	}
	if row := pushRows(t, h, h.FamilyID)[sleeper.DeviceID]; row.Pushed != int64(trailing) || row.PushedAt == nil || !row.PushedAt.Equal(clock) {
		t.Fatalf("reservation %+v, want sequence %d at %v", row, trailing, clock)
	}
	// Once per sequence: nothing more without a write.
	clock = clock.Add(time.Hour)
	tracker.Changed(h.FamilyID, trailing)
	pushTick(t, w)
	if got := pusher.count(token(0x71)); got != 2 {
		t.Fatalf("pushed %d times, want 2", got)
	}
}

// After a restart no Changed names the family: the startup backstop pushes a
// backgrounded device behind and never pushed at once, and one pushed inside
// its window before the restart once that window ends, with no write.
func TestBackstopPushesAfterARestart(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	clock := h.Now
	now := func() time.Time { return clock }
	before := wake.NewTracker(0)
	first := &wake.Worker{Pool: h.Maintenance, Tracker: before, Sender: discardSender{}, Pusher: newFakePusher(),
		Connected: func(string) bool { return false }, Now: now}
	backgrounded(t, h, before, token(0xd1), clock)
	write(t, h, before, 1)
	if stats := pushTick(t, first); stats.Pushed != 1 {
		t.Fatalf("pre-restart push %+v", stats)
	}
	pushedAt := clock
	// A device registered later, and a write the old process never ticked for.
	backgrounded(t, h, before, token(0xd2), clock)
	pushTick(t, first)
	caretest.Seed(t, h, caretest.Feed(caretest.BabyA, 2))

	clock = pushedAt.Add(5 * time.Minute)
	pusher := newFakePusher()
	restarted := &wake.Worker{Pool: h.Maintenance, Tracker: wake.NewTracker(0), Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: now}
	for range 3 {
		pushTick(t, restarted)
	}
	if pusher.count(token(0xd2)) != 1 || pusher.count(token(0xd1)) != 0 {
		t.Fatalf("after the restart: %v, want only the never-pushed device", pusher.sent)
	}
	clock = pushedAt.Add(wake.APNsInterval - time.Second)
	pushTick(t, restarted)
	if pusher.count(token(0xd1)) != 0 {
		t.Fatalf("pushed inside its window: %v", pusher.sent)
	}
	clock = pushedAt.Add(wake.APNsInterval)
	pushTick(t, restarted)
	if pusher.count(token(0xd1)) != 1 || pusher.count(token(0xd2)) != 1 {
		t.Fatalf("when the window ended: %v, want the pre-restart device pushed once more", pusher.sent)
	}
}

func TestPushBacksOff(t *testing.T) { // Review Focus 5: a 429 stops every send until Retry-After
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	// Each call makes one new backgrounded device due, so only the pause can hold a send back.
	devices := 0
	makeDue := func(int) []byte {
		devices++
		value := token(byte(0x60 + devices))
		backgrounded(t, h, tracker, value, clock)
		write(t, h, tracker, 7000+devices)
		return value
	}
	retryUntil := clock.Add(10 * time.Minute)
	pusher.setAnswer(answering(apns.Result{Status: 429, Reason: "TooManyRequests", Class: apns.Retryable, RetryAfter: retryUntil}, nil))
	makeDue(1)
	if stats, err := w.Tick(t.Context()); err != nil || stats.Pushed != 1 || stats.PushFailed != 1 {
		t.Fatalf("the first push: %+v %v", stats, err)
	}
	pusher.setAnswer(nil)
	clock = clock.Add(time.Minute)
	makeDue(2)
	if stats, err := w.Tick(t.Context()); err != nil || stats.Pushed != 0 {
		t.Fatalf("a push went out during the 429 pause: %+v %v", stats, err)
	}
	clock = retryUntil.Add(-time.Second)
	if stats := pushTick(t, w); stats.Pushed != 0 {
		t.Fatalf("a push went out a second before Retry-After: %+v", stats)
	}
	// The held family is scanned again when the pause ends, without a write.
	clock = retryUntil.Add(time.Second)
	if stats, err := w.Tick(t.Context()); err != nil || stats.Pushed != 1 {
		t.Fatalf("sends did not resume after Retry-After: %+v %v", stats, err)
	}
	if pusher.count(token(0x62)) != 1 || pusher.count(token(0x61)) != 1 {
		t.Fatalf("pushes %v", pusher.sent)
	}
	// A Retry-After under a minute pauses a minute; one over an hour pauses
	// an hour; none pauses a minute.
	for _, test := range []struct {
		name  string
		after time.Duration
		pause time.Duration
	}{{"soon", 5 * time.Second, time.Minute}, {"far", 3 * time.Hour, time.Hour}, {"absent", 0, time.Minute}} {
		retry := time.Time{}
		if test.after > 0 {
			retry = clock.Add(test.after)
		}
		pusher.setAnswer(answering(apns.Result{Status: 429, Class: apns.Retryable, RetryAfter: retry}, nil))
		refused := makeDue(0)
		if stats := pushTick(t, w); stats.Pushed != 1 || pusher.count(refused) != 1 {
			t.Fatalf("%s: %+v", test.name, stats)
		}
		pusher.setAnswer(nil)
		pausedAt := clock
		clock = pausedAt.Add(test.pause - time.Second)
		held := makeDue(0)
		if stats := pushTick(t, w); stats.Pushed != 0 {
			t.Fatalf("%s: a push went out a second before the %v pause ended: %+v", test.name, test.pause, stats)
		}
		// When the pause ends the held device is pushed (and, after an hour,
		// every earlier device whose 20-minute window ended since).
		clock = pausedAt.Add(test.pause)
		if stats := pushTick(t, w); stats.Pushed == 0 || pusher.count(held) != 1 {
			t.Fatalf("%s: no push when the %v pause ended: %+v", test.name, test.pause, stats)
		}
	}
}

// countingHandler counts slog records by message and keeps their attributes.
type countingHandler struct {
	mu      sync.Mutex
	records map[string]int
	attrs   []string
}

func (c *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (c *countingHandler) Handle(_ context.Context, record slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.records == nil {
		c.records = map[string]int{}
	}
	c.records[record.Message]++
	record.Attrs(func(a slog.Attr) bool {
		c.attrs = append(c.attrs, a.Key+"="+a.Value.String())
		return true
	})
	return nil
}
func (c *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *countingHandler) WithGroup(string) slog.Handler      { return c }
func (c *countingHandler) count(message string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.records[message]
}

func TestPushServerErrorsBackOff(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher, logs := wake.NewTracker(0), newFakePusher(), &countingHandler{}
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher, Logger: slog.New(logs),
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	n := 0
	makeDue := func(count int) access.Principal {
		var p access.Principal
		for range count {
			n++
			p = backgrounded(t, h, tracker, token(byte(0x80+n)), clock)
		}
		write(t, h, tracker, 8000+n)
		return p
	}
	// expectPause: a send now sets a pause of want; a tick a second before its
	// end sends nothing; a tick at its end sends one (the held device). It
	// returns the device answered.
	expectPause := func(step string, answer func([]byte) (apns.Result, error), want time.Duration) access.Principal {
		t.Helper()
		pusher.setAnswer(answer)
		answered := makeDue(1)
		if stats := pushTick(t, w); stats.PushFailed != 1 {
			t.Fatalf("%s: %+v, want one failed push", step, stats)
		}
		start := clock
		if want > time.Second {
			clock = start.Add(want - time.Second)
			makeDue(1)
			if stats := pushTick(t, w); stats.Pushed != 0 {
				t.Fatalf("%s: a push went out a second before the %v pause ended: %+v", step, want, stats)
			}
		} else {
			makeDue(1)
		}
		pusher.setAnswer(nil)
		clock = start.Add(want)
		if stats := pushTick(t, w); stats.Pushed != 1 {
			t.Fatalf("%s: no push when the %v pause ended: %+v", step, want, stats)
		}
		return answered
	}
	unavailable := answering(apns.Result{Status: 503, Reason: "ServiceUnavailable", Class: apns.Retryable}, nil)
	// Three 503s in a row double the pause: 2 s, 4 s, 8 s. (Each step's last
	// tick answers 200 only after the next step set its answer, so the 503s
	// stay consecutive: expectPause's final tick is the next 503.)
	pusher.setAnswer(unavailable)
	makeDue(1)
	if stats := pushTick(t, w); stats.Pushed != 1 || stats.PushFailed != 1 {
		t.Fatalf("first 503: %+v", stats)
	}
	for _, pause := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} {
		start := clock
		clock = start.Add(pause - time.Second)
		makeDue(1)
		if stats := pushTick(t, w); stats.Pushed != 0 {
			t.Fatalf("a push went out a second inside the %v pause: %+v", pause, stats)
		}
		clock = start.Add(pause)
		if pause == 8*time.Second {
			pusher.setAnswer(nil) // a 200 ends the run
		}
		if stats := pushTick(t, w); stats.Pushed != 1 {
			t.Fatalf("no push when the %v pause ended: %+v", pause, stats)
		}
	}
	// The 200 reset the pause: the next 503 pauses 2 s again.
	expectPause("503 after a 200", unavailable, 2*time.Second)
	// A lost answer (Ambiguous) backs off like a 503; so does a send the
	// client refused before writing (ErrNotSent), which pauses neither five
	// minutes nor deletes the token.
	clock = clock.Add(time.Minute)
	expectPause("ambiguous", answering(apns.Result{Class: apns.Ambiguous}, fmt.Errorf("%w: %w", apns.ErrOutcomeUnknown, apns.ErrCauseTimeout)), 2*time.Second)
	clock = clock.Add(time.Minute)
	unsent := expectPause("not sent", answering(apns.Result{Class: apns.ConfigurationFailure}, errors.Join(apns.ErrNotSent, errors.New("signing failed"))), 2*time.Second)
	if storedToken(t, h, unsent.DeviceID) == nil || logs.count("apns configuration refused") != 0 || logs.count("apns wake not sent") != 1 {
		t.Fatal("a send that never left deleted its token, logged a configuration refusal, or was not logged as not sent")
	}
	// An expired provider token pauses a minute (the client re-signs).
	clock = clock.Add(time.Minute)
	expectPause("expired provider token", answering(apns.Result{Status: 403, Reason: "ExpiredProviderToken", Class: apns.ConfigurationFailure}, nil), time.Minute)
	if logs.count("apns provider token refused") != 1 {
		t.Fatalf("provider token refusal logged %d times, want once", logs.count("apns provider token refused"))
	}
	// Any other configuration refusal pauses five minutes and logs once,
	// however many devices it answered in the tick.
	clock = clock.Add(time.Minute)
	pusher.setAnswer(answering(apns.Result{Status: 400, Reason: "BadTopic", Class: apns.ConfigurationFailure}, nil))
	makeDue(3)
	if stats := pushTick(t, w); stats.Pushed == 0 || stats.PushFailed != stats.Pushed {
		t.Fatalf("bad topic: %+v", stats)
	}
	pusher.setAnswer(nil)
	start := clock
	clock = start.Add(5*time.Minute - time.Second)
	makeDue(1)
	if stats := pushTick(t, w); stats.Pushed != 0 {
		t.Fatalf("a push went out inside the configuration pause: %+v", stats)
	}
	clock = start.Add(5 * time.Minute)
	if stats := pushTick(t, w); stats.Pushed == 0 {
		t.Fatalf("no push after the configuration pause: %+v", stats)
	}
	if got := logs.count("apns configuration refused"); got != 1 {
		t.Fatalf("configuration refusal logged %d times, want once", got)
	}
	for _, attr := range logs.attrs {
		for fill := byte(0x80); fill < 0x80+byte(n)+1; fill++ {
			if bytes.Contains([]byte(attr), []byte(hex.EncodeToString(token(fill))[:16])) {
				t.Fatalf("a log attribute carries a token: %s", attr)
			}
		}
	}
}

// An answer that pauses sends stops the tick's other pushes before they
// leave: their reservations are given back, and they are pushed once the
// pause ends, without a further write.
func TestAPauseStopsTheTicksOtherPushesAndReleasesThem(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	devices := map[string][]byte{}
	for i := range 10 {
		value := token(byte(0xc0 + i))
		devices[backgrounded(t, h, tracker, value, clock).DeviceID] = value
	}
	write(t, h, tracker, 1)
	pusher.setAnswer(answering(apns.Result{Status: 503, Class: apns.Retryable}, nil))
	if stats := pushTick(t, w); stats.Pushed != 1 || stats.PushFailed != 1 {
		t.Fatalf("%+v, want the first push only (it paused the rest)", stats)
	}
	rows := pushRows(t, h, h.FamilyID)
	reserved := 0
	for device := range devices {
		if row := rows[device]; row.PushedAt != nil {
			reserved++
		} else if row.Pushed != 0 {
			t.Fatalf("a released reservation kept its sequence: %+v", row)
		}
	}
	if reserved != 1 {
		t.Fatalf("%d reservations kept, want the one sent", reserved)
	}
	pusher.setAnswer(nil)
	clock = clock.Add(time.Second)
	if stats := pushTick(t, w); stats.Pushed != 0 {
		t.Fatalf("inside the pause: %+v", stats)
	}
	clock = clock.Add(time.Second)
	if stats := pushTick(t, w); stats.Pushed != 9 {
		t.Fatalf("when the pause ended: %+v, want the 9 released pushes", stats)
	}
	for device, value := range devices {
		if pusher.count(value) != 1 {
			t.Fatalf("device %s pushed %d times", device, pusher.count(value))
		}
	}
}

func TestPushOnlyEligibleDevices(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	familyB := writtenFamily(t, h)
	outsider := h.Refresh("outsider")
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	pushTick(t, w) // the startup backstop finds nothing
	// A Pusher without Connected could push a device the hub holds: refused.
	if _, err := (&wake.Worker{Pool: h.Maintenance, Tracker: wake.NewTracker(0), Sender: discardSender{}, Pusher: pusher}).Tick(t.Context()); err == nil {
		t.Fatal("a worker with a Pusher and no Connected ticked")
	}
	seq := h.FamilySequence(t)
	// A token but no device_sync_cursors row (the device never synced here).
	unsynced := h.AddMember(t, "unsynced", "caregiver")
	registerToken(t, h, unsynced, token(0x01), apns.Production)
	// Two backgrounded devices in two environments.
	sandbox := h.AddMember(t, "sandboxed", "caregiver")
	registerToken(t, h, sandbox, token(0x02), apns.Sandbox)
	tracker.Synced(h.FamilyID, sandbox.DeviceID, seq, seq, clock)
	production := h.AddMember(t, "produced", "caregiver")
	registerToken(t, h, production, token(0x03), apns.Production)
	tracker.Synced(h.FamilyID, production.DeviceID, seq, seq, clock)
	// Another family's device, behind its own family and registered.
	registerToken(t, h, outsider, token(0x04), apns.Production)
	tracker.Synced(familyB, outsider.DeviceID, 0, 0, clock)
	// A device behind with no token.
	tokenless := h.AddMember(t, "tokenless", "caregiver")
	tracker.Synced(h.FamilyID, tokenless.DeviceID, seq, seq, clock)
	pushTick(t, w)
	write(t, h, tracker, 1)
	if stats := pushTick(t, w); stats.Pushed != 2 {
		t.Fatalf("pushed %+v, want the two registered, synced devices of the changed family", stats)
	}
	if pusher.count(token(0x01)) != 0 || pusher.count(token(0x04)) != 0 || pusher.total() != 2 {
		t.Fatalf("pushes %v: the unsynced device or the other family's device was pushed", pusher.sent)
	}
	if pusher.envs[hex.EncodeToString(token(0x02))] != apns.Sandbox || pusher.envs[hex.EncodeToString(token(0x03))] != apns.Production {
		t.Fatalf("environments %v, want each device's registered one", pusher.envs)
	}
	// The tokenless device is not reserved (a later registration is pushed).
	if row := pushRows(t, h, h.FamilyID)[tokenless.DeviceID]; row.PushedAt != nil || row.Pushed != 0 || row.WokenAt == nil {
		t.Fatalf("tokenless device row %+v, want woken only", row)
	}
}

func TestInvalidTokenIsDeletedOnlyIfUnchanged(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	gone := backgrounded(t, h, tracker, token(0x21), clock)
	replaced := backgrounded(t, h, tracker, token(0x22), clock)
	pusher.setAnswer(func(value []byte) (apns.Result, error) {
		if bytes.Equal(value, token(0x22)) {
			// The phone registers a new token (a PUT) while Apple answers.
			registerToken(t, h, replaced, token(0x23), apns.Production)
		}
		return apns.Result{Status: 410, Reason: "Unregistered", Class: apns.InvalidToken}, nil
	})
	write(t, h, tracker, 1)
	if stats := pushTick(t, w); stats.Pushed != 2 || stats.PushFailed != 2 {
		t.Fatalf("%+v", stats)
	}
	if got := storedToken(t, h, gone.DeviceID); got != nil {
		t.Fatalf("the refused token was kept: %x", got)
	}
	if got := storedToken(t, h, replaced.DeviceID); !bytes.Equal(got, token(0x23)) {
		t.Fatalf("the re-registered token was deleted: %x", got)
	}
	// An invalid token pauses nothing: the next due device is pushed at once.
	pusher.setAnswer(nil)
	backgrounded(t, h, tracker, token(0x24), clock)
	write(t, h, tracker, 2)
	if stats := pushTick(t, w); stats.Pushed != 1 {
		t.Fatalf("after an invalid token: %+v", stats)
	}
}

// blockingPusher never answers until its context ends; it records the
// first token it was handed.
type blockingPusher struct {
	started chan struct{}
	once    sync.Once
	first   []byte
}

func (p *blockingPusher) Wake(ctx context.Context, _ apns.Environment, value []byte) (apns.Result, error) {
	p.once.Do(func() {
		p.first = value
		close(p.started)
	})
	<-ctx.Done()
	return apns.Result{Class: apns.Ambiguous}, fmt.Errorf("%w: %w", apns.ErrOutcomeUnknown, apns.ErrCauseCanceled)
}

// The reservation commits before the send: a crash (here a cancelled tick)
// while Apple has the push leaves pushed_at set, and a new worker does not
// push that device again within 20 minutes. The tick's other push, never
// started (the first is sent alone), gives its reservation back.
func TestPushReservationSurvivesACrash(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	tracker := wake.NewTracker(0)
	clock := h.Now
	now := func() time.Time { return clock }
	devices := map[string]access.Principal{}
	for _, value := range [][]byte{token(0x31), token(0x32)} {
		devices[string(value)] = backgrounded(t, h, tracker, value, clock)
	}
	blocked := &blockingPusher{started: make(chan struct{})}
	crashed := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: blocked,
		Connected: func(string) bool { return false }, Now: now}
	write(t, h, tracker, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := crashed.Tick(ctx); done <- err }()
	select {
	case <-blocked.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the push never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the tick did not end with its context")
	}
	sent, unsent := blocked.first, token(0x31)
	if bytes.Equal(sent, unsent) {
		unsent = token(0x32)
	}
	rows := pushRows(t, h, h.FamilyID)
	if row := rows[devices[string(sent)].DeviceID]; row.PushedAt == nil || !row.PushedAt.Equal(clock) {
		t.Fatalf("reservation %+v, want pushed_at set before the send", row)
	}
	if row := rows[devices[string(unsent)].DeviceID]; row.PushedAt != nil || row.Pushed != 0 {
		t.Fatalf("the push never started kept its reservation: %+v", row)
	}
	// A new process: the released device is pushed at once; the reserved one
	// is not pushed by later writes inside its window, and is pushed once
	// when the window ends.
	pusher := newFakePusher()
	restartedTracker := wake.NewTracker(0)
	restarted := &wake.Worker{Pool: h.Maintenance, Tracker: restartedTracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(string) bool { return false }, Now: now}
	reservedAt := clock
	for i, at := range []time.Duration{0, time.Minute, 19 * time.Minute} {
		clock = reservedAt.Add(at)
		write(t, h, restartedTracker, 10+i)
		pushTick(t, restarted)
	}
	if pusher.count(sent) != 0 || pusher.count(unsent) != 1 {
		t.Fatalf("inside the window after a crash: %v", pusher.sent)
	}
	clock = reservedAt.Add(wake.APNsInterval)
	pushTick(t, restarted)
	if pusher.count(sent) != 1 {
		t.Fatalf("after the window: %v", pusher.sent)
	}
}

// A tick with push candidates costs at most five round trips (BEGIN, the
// flush and scans, app.push_targets, the mark, COMMIT); one without costs at
// most four.
func TestWakeTickWithPushIsAtMostFiveRoundTrips(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	pool, wire := testkit.CountedPool(t, h.Maintenance)
	tracker, pusher := wake.NewTracker(0), newFakePusher()
	clock := h.Now
	connected := map[string]bool{}
	w := &wake.Worker{Pool: pool, Tracker: tracker, Sender: discardSender{}, Pusher: pusher,
		Connected: func(device string) bool { return connected[device] }, Now: func() time.Time { return clock }}
	// Warm the statement cache: a push tick and a hub tick.
	backgrounded(t, h, tracker, token(0x40), clock)
	write(t, h, tracker, 1)
	if stats := pushTick(t, w); stats.Pushed != 1 {
		t.Fatalf("warm %+v", stats)
	}
	for i := range 3 {
		backgrounded(t, h, tracker, token(byte(0x41+i)), clock)
	}
	clock = clock.Add(time.Minute)
	write(t, h, tracker, 2)
	wire.Reset()
	if stats := pushTick(t, w); stats.Pushed != 3 {
		t.Fatalf("push tick %+v", stats)
	}
	if trips := wire.Trips(); trips > 5 {
		t.Fatalf("a tick with pushes took %d round trips, want at most 5: %q", trips, wire.Statements())
	}
	// A tick whose lagging devices are all connected looks up no token.
	clock = clock.Add(2 * time.Minute)
	write(t, h, tracker, 3)
	for device := range pushRows(t, h, h.FamilyID) {
		connected[device] = true
	}
	wire.Reset()
	if stats := pushTick(t, w); stats.Woken == 0 || stats.Pushed != 0 {
		t.Fatalf("hub tick %+v", stats)
	}
	if trips := wire.Trips(); trips > 4 {
		t.Fatalf("a tick without push candidates took %d round trips, want at most 4: %q", trips, wire.Statements())
	}
}

// The worker over the real APNs client (a local TLS HTTP/2 server stands in
// for Apple): 20 lagging backgrounded devices are 20 background pushes over
// one connection, each the content-free wake payload.
func TestWorkerPushesThroughTheRealClient(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	type request struct {
		path    string
		headers http.Header
		body    string
	}
	var mu sync.Mutex
	var requests []request
	var conns atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, request{path: r.URL.Path, headers: r.Header.Clone(), body: string(body)})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	path := filepath.Join(t.TempDir(), "apns.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := h.Now
	client, err := apns.NewWithTransport(config.APNSCredentials{TeamID: "TEAM123456", KeyID: "KEY1234567", KeyFile: path, BundleID: "com.example.babytracker"},
		server.Client().Transport, server.URL, server.URL, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	tracker := wake.NewTracker(0)
	w := &wake.Worker{Pool: h.Maintenance, Tracker: tracker, Sender: discardSender{}, Pusher: client,
		Connected: func(string) bool { return false }, Now: func() time.Time { return clock }}
	seq := h.FamilySequence(t)
	tokens := map[string]bool{}
	for i := range 20 {
		p, err := h.NewDevice("caregiver")
		if err != nil {
			t.Fatal(err)
		}
		value := token(byte(0xa0 + i))
		registerToken(t, h, p, value, apns.Sandbox)
		tokens["/3/device/"+hex.EncodeToString(value)] = true
		tracker.Synced(h.FamilyID, p.DeviceID, seq, seq, clock)
	}
	write(t, h, tracker, 1)
	if stats := pushTick(t, w); stats.Pushed != 20 || stats.PushFailed != 0 {
		t.Fatalf("%+v, want 20 accepted pushes", stats)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 20 || conns.Load() != 1 {
		t.Fatalf("%d requests over %d connections, want 20 over 1", len(requests), conns.Load())
	}
	for _, r := range requests {
		if !tokens[r.path] || r.body != apns.WakePayload || r.headers.Get("apns-push-type") != "background" ||
			r.headers.Get("apns-priority") != "5" || r.headers.Get("apns-topic") != "com.example.babytracker" {
			t.Fatalf("request %s %v %q", r.path, r.headers, r.body)
		}
		delete(tokens, r.path)
	}
}
