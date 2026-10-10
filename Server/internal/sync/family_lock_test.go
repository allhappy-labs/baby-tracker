package sync_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
)

// The family row lock (care.LockFamily, the first statement of the batch's
// Fetch) serializes a family's writes. These tests hold that row from
// another transaction and require a mutation to wait on its own family
// fence statement first — not on whatever later statement happens to touch
// the row (the family update of the write batch), which is what a missing
// explicit lock would look like. Pull pages and continuations take no lock:
// they read under their statement's snapshot while a writer holds the row.

const familyFenceSQL = "SELECT next_sequence, care_record_count, care_charged_bytes, care_entity_count, care_wire_bytes FROM families WHERE id=$1 FOR NO KEY UPDATE"

func holdFamilyRow(t *testing.T, h *testkit.Harness) (pid uint32, release func()) {
	t.Helper()
	return holdFamilyRowAs(t, h, "FOR UPDATE")
}

// holdFamilyRowAs holds the family row with lock (FOR UPDATE, or FOR NO KEY
// UPDATE as a concurrent writer's batch holds it).
func holdFamilyRowAs(t *testing.T, h *testkit.Harness, lock string) (pid uint32, release func()) {
	t.Helper()
	tx, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := tx.QueryRow(t.Context(), `SELECT id::text FROM families WHERE id=$1 `+lock, h.FamilyID).Scan(&id); err != nil {
		t.Fatalf("hold family row: %v", err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = tx.Rollback(context.WithoutCancel(t.Context()))
		}
	}
	t.Cleanup(release)
	return tx.Conn().PgConn().PID(), release
}

// requireFirstWaitIsFamilyFence runs op, waits until it blocks on the held
// family row, and requires the blocked statement to be the family fence.
func requireFirstWaitIsFamilyFence(t *testing.T, h *testkit.Harness, op func() error) {
	t.Helper()
	blocker, release := holdFamilyRow(t, h)
	done := make(chan error, 1)
	go func() { done <- op() }()
	waitCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		var queries []string
		rows, err := h.Admin.Query(waitCtx, `SELECT query FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, int(blocker))
		if err != nil {
			t.Fatalf("observe blocked statement: %v", err)
		}
		for rows.Next() {
			var query string
			if err := rows.Scan(&query); err != nil {
				t.Fatal(err)
			}
			queries = append(queries, query)
		}
		rows.Close()
		if len(queries) > 0 {
			if len(queries) != 1 || strings.TrimSpace(queries[0]) != familyFenceSQL {
				t.Fatalf("first wait on the family row was %q, want the family fence", queries)
			}
			break
		}
		select {
		case err := <-done:
			t.Fatalf("operation finished without waiting on the family row: %v", err)
		case <-waitCtx.Done():
			t.Fatal("operation never waited on the family row")
		case <-poll.C:
		}
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("operation after lock release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("operation did not finish after the family row was released")
	}
}

func TestMutationWaitsOnTheFamilyFenceFirst(t *testing.T) {
	h := testkit.New(t)
	baby := testkit.MustMutation(t, "baby.json")
	requireFirstWaitIsFamilyFence(t, h, func() error {
		out, err := h.Service.Apply(context.Background(), h.Owner, h.FamilyID, baby)
		if err == nil && out.Status != "applied" {
			t.Errorf("outcome %+v", out)
		}
		return err
	})
}

// requireNoWaitOnTheFamilyRow runs op while a writer holds the family row
// and requires it to finish without waiting.
func requireNoWaitOnTheFamilyRow(t *testing.T, h *testkit.Harness, op func() error) {
	t.Helper()
	_, release := holdFamilyRowAs(t, h, "FOR NO KEY UPDATE")
	defer release()
	done := make(chan error, 1)
	go func() { done <- op() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("operation under a held family row: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a pull waited on the family row a writer holds")
	}
}

func TestPullsAndContinuationsTakeNoFamilyLock(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
	h.SeedBabies(t, 3)
	var first protocol.SyncResponse
	requireNoWaitOnTheFamilyRow(t, h, func() error {
		var err error
		first, err = applyAndPull(t, h.Service, h.Owner, h.SyncRequest(h.Cursor(0), nil))
		return err
	})
	if first.NextPageToken == nil {
		t.Fatalf("first=%+v", first)
	}
	requireNoWaitOnTheFamilyRow(t, h, func() error {
		_, err := h.Service.ApplyAndPull(context.Background(), h.Owner, h.SyncRequest(nil, first.NextPageToken))
		return err
	})
	var snapshot protocol.SnapshotResponse
	requireNoWaitOnTheFamilyRow(t, h, func() error {
		var err error
		snapshot, err = h.Service.FullSnapshot(context.Background(), h.Owner, protocol.SnapshotRequest{})
		return err
	})
	if snapshot.NextPageToken == nil {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	requireNoWaitOnTheFamilyRow(t, h, func() error {
		_, err := h.Service.FullSnapshot(context.Background(), h.Owner, protocol.SnapshotRequest{PageToken: snapshot.NextPageToken})
		return err
	})
}
