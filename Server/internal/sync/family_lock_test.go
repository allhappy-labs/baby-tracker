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

// The family row lock (records.LockFamilyForUpdate) serializes a family's
// writes against each other and against page reads. These tests hold that row
// from another transaction and require each operation to wait on its own
// family fence statement first — not on whatever later statement happens to
// touch the row (a journal insert's foreign key check, the next_sequence
// update), which is what a missing explicit lock would look like.

const familyFenceSQL = "SELECT id FROM families WHERE id=$1 FOR UPDATE"

func holdFamilyRow(t *testing.T, h *testkit.Harness) (pid uint32, release func()) {
	t.Helper()
	tx, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := tx.QueryRow(t.Context(), `SELECT id::text FROM families WHERE id=$1 FOR UPDATE`, h.FamilyID).Scan(&id); err != nil {
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

func TestSyncContinuationWaitsOnTheFamilyFenceFirst(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t)
	h.SeedBabies(t, 2)
	first, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	requireFirstWaitIsFamilyFence(t, h, func() error {
		_, err := h.Service.ApplyAndPull(context.Background(), h.Owner, h.SyncRequest(nil, first.NextPageToken))
		return err
	})
}

func TestSnapshotContinuationWaitsOnTheFamilyFenceFirst(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t)
	h.SeedBabies(t, 2)
	first, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{})
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	requireFirstWaitIsFamilyFence(t, h, func() error {
		_, err := h.Service.FullSnapshot(context.Background(), h.Owner, protocol.SnapshotRequest{PageToken: first.NextPageToken})
		return err
	})
}
