package maintenance_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/database"
	"babytracker/server/internal/maintenance"
	"babytracker/server/internal/testkit"
)

// capLogs counts the records logged.
type capLogs struct{ n atomic.Int32 }

func (c *capLogs) Enabled(context.Context, slog.Level) bool  { return true }
func (c *capLogs) Handle(context.Context, slog.Record) error { c.n.Add(1); return nil }
func (c *capLogs) WithAttrs([]slog.Attr) slog.Handler        { return c }
func (c *capLogs) WithGroup(string) slog.Handler             { return c }

// registerPushToken stores p's device token as the push-token route does,
// through the migrator under the authority flag.
func registerPushToken(t *testing.T, h *testkit.Harness, p access.Principal, fill byte) {
	t.Helper()
	h.AuthorityExec(t, `INSERT INTO device_push_tokens (device_id, account_id, environment, token, registered_at)
		VALUES ($1, $2, 'production', $3, now())`, p.DeviceID, p.AccountID, bytes.Repeat([]byte{fill}, 32))
}

func pushTokenCount(t *testing.T, h *testkit.Harness, where string, args ...any) int {
	t.Helper()
	tx, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority', '1', true)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM device_push_tokens WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The push-token sweep's bounds: hourly, 1,000 tokens a statement (halved
// down to 100 after a statement timeout), at most 5,000 a pass.
func TestPushTokenBoundsArePinned(t *testing.T) {
	if maintenance.PushTokenInterval != time.Hour {
		t.Errorf("PushTokenInterval = %v, want 1h", maintenance.PushTokenInterval)
	}
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"PushTokenBatch", maintenance.PushTokenBatch, 1000},
		{"PushTokenPassCap", maintenance.PushTokenPassCap, 5000},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestPushTokenSweepDeletesTokensWithoutAFamilySession(t *testing.T) {
	h := testkit.New(t)
	caretest.Seed(t, h, caretest.Baby(caretest.BabyA))
	// A live member's token survives.
	registerPushToken(t, h, h.Caregiver, 0x01)
	// A logged-out device.
	loggedOut, err := h.NewDevice("caregiver")
	if err != nil {
		t.Fatal(err)
	}
	registerPushToken(t, h, loggedOut, 0x02)
	if _, err := h.Auth.Logout(t.Context(), loggedOut, h.CommandID()); err != nil {
		t.Fatal(err)
	}
	// A removed member's device.
	removed := h.AddMember(t, "removed", "caregiver")
	registerPushToken(t, h, removed, 0x03)
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), removed.AccountID); err != nil {
		t.Fatal(err)
	}
	// A purged family's device (its owner's, the outsider's own family).
	created, err := h.Family.Create(t.Context(), h.Outsider, h.CommandID(), "B")
	if err != nil {
		t.Fatal(err)
	}
	outsider := h.Refresh("outsider")
	registerPushToken(t, h, outsider, 0x04)
	if _, err := h.Family.ScheduleDeletion(t.Context(), outsider, h.CommandID(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Authority.StartFamilyPurge(t.Context(), authority.StartFamilyPurge{CommandID: h.CommandID(), FamilyID: created.ID,
		At: h.Now.Add(31 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// A pass bounded by PushTokenPassCap: 5,100 more devices of the removed
	// member, each with a token and no session, drained in batches.
	h.AuthorityExec(t, `WITH clones AS (
		  SELECT (jsonb_populate_record(d, jsonb_build_object('id', gen_random_uuid()))).* FROM devices d, generate_series(1, 5100) WHERE d.id = $1)
		, inserted AS (INSERT INTO devices SELECT * FROM clones RETURNING id, account_id)
		INSERT INTO device_push_tokens (device_id, account_id, environment, token, registered_at)
		SELECT id, account_id, 'sandbox', decode(md5(id::text) || md5(id::text), 'hex'), now() FROM inserted`, removed.DeviceID)
	if n := pushTokenCount(t, h, "true"); n != 5104 {
		t.Fatalf("%d tokens seeded", n)
	}
	logs := &capLogs{}
	sweep := &maintenance.PushTokenSweep{Pool: h.Maintenance, Logger: slog.New(logs)}
	if sweep.Name() != "push-tokens" {
		t.Fatalf("name %q", sweep.Name())
	}
	if n, err := sweep.Run(t.Context(), h.Now); err != nil || n != maintenance.PushTokenPassCap {
		t.Fatalf("first pass deleted %d (%v), want the pass cap %d", n, err, maintenance.PushTokenPassCap)
	}
	if logs.n.Load() != 1 {
		t.Fatalf("a capped pass logged %d times, want once", logs.n.Load())
	}
	if n, err := sweep.Run(t.Context(), h.Now); err != nil || n != 103 {
		t.Fatalf("second pass deleted %d (%v), want the other 103", n, err)
	}
	if n, err := sweep.Run(t.Context(), h.Now); err != nil || n != 0 {
		t.Fatalf("third pass deleted %d (%v)", n, err)
	}
	if n := pushTokenCount(t, h, "true"); n != 1 {
		t.Fatalf("%d tokens left, want the live member's", n)
	}
	if n := pushTokenCount(t, h, "device_id = $1", h.Caregiver.DeviceID); n != 1 {
		t.Fatal("the live member's token was deleted")
	}
}

// Each statement removes at most the batch size, one round trip each: a
// backlog of 250 at a batch of 100 takes three statements (100, 100, 50).
func TestPushTokenSweepBatchesAtItsSize(t *testing.T) {
	h := testkit.New(t)
	pool, wire := testkit.CountedPool(t, h.Maintenance)
	sweep := &maintenance.PushTokenSweep{Pool: pool, Batch: 100}
	// Warm the connection's statement cache on an empty backlog.
	if n, err := sweep.Run(t.Context(), h.Now); err != nil || n != 0 {
		t.Fatalf("empty backlog: %d (%v)", n, err)
	}
	removed := h.AddMember(t, "removed", "caregiver")
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), removed.AccountID); err != nil {
		t.Fatal(err)
	}
	h.AuthorityExec(t, `WITH clones AS (
		  SELECT (jsonb_populate_record(d, jsonb_build_object('id', gen_random_uuid()))).* FROM devices d, generate_series(1, 250) WHERE d.id = $1)
		, inserted AS (INSERT INTO devices SELECT * FROM clones RETURNING id, account_id)
		INSERT INTO device_push_tokens (device_id, account_id, environment, token, registered_at)
		SELECT id, account_id, 'sandbox', decode(md5(id::text) || md5(id::text), 'hex'), now() FROM inserted`, removed.DeviceID)
	wire.Reset()
	if n, err := sweep.Run(t.Context(), h.Now); err != nil || n != 250 {
		t.Fatalf("deleted %d (%v), want 250", n, err)
	}
	prunes := 0
	for _, statement := range wire.Statements() {
		if strings.Contains(statement, "app.prune_push_tokens") {
			prunes++
		}
	}
	if prunes != 3 || wire.Trips() != 3 {
		t.Fatalf("%d prune statements in %d round trips, want 3 in 3 (100, 100, 50)", prunes, wire.Trips())
	}
}

// The sweep runs inside a maintenance worker slot: with every slot held it
// waits (and gives up with its context) without touching the pool.
func TestPushTokenSweepRunsInsideAWorkerSlot(t *testing.T) {
	h := testkit.New(t)
	pool, err := database.OpenWithMax(t.Context(), h.Maintenance.Config().ConnString(), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	slots, err := database.NewWorkerSlots(pool)
	if err != nil {
		t.Fatal(err)
	}
	release, err := slots.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sweep := &maintenance.PushTokenSweep{Pool: pool, Slots: slots}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := sweep.Run(ctx, h.Now); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("with every slot held: %v, want to wait for one", err)
	}
	if acquired := pool.Stat().AcquireCount(); acquired != 0 {
		t.Fatalf("the sweep took %d pool connections outside a slot", acquired)
	}
	release()
	if _, err := sweep.Run(t.Context(), h.Now); err != nil {
		t.Fatalf("with a free slot: %v", err)
	}
}
