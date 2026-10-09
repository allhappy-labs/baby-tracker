package records_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var errCascadeSecondBatch = errors.New("injected child cascade second batch failure")

type cascadeBatchFaultTx struct {
	pgx.Tx
	batches *atomic.Int32
	failAt  int32
}

func (tx cascadeBatchFaultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "child-cascade-journal") && tx.batches.Add(1) == tx.failAt {
		return pgconn.CommandTag{}, errCascadeSecondBatch
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

type cascadeBatchFaultGuard struct {
	inner   records.Guard
	batches atomic.Int32
	failAt  int32
}

func (g *cascadeBatchFaultGuard) WithAuthorizedTransaction(ctx context.Context, principal access.Principal, familyID, action string, run func(pgx.Tx, access.Grant) error) error {
	return g.inner.WithAuthorizedTransaction(ctx, principal, familyID, action, func(tx pgx.Tx, grant access.Grant) error {
		return run(cascadeBatchFaultTx{Tx: tx, batches: &g.batches, failAt: g.failAt}, grant)
	})
}

func babyDelete(t *testing.T, h *testkit.Harness, mutation int) protocol.Outcome {
	t.Helper()
	m := testkit.MustMutation(t, "baby.json")
	m.MutationID, m.Operation, m.Payload = mutationID(mutation), "delete", protocol.DeletePayload{}
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil {
		t.Fatalf("delete baby: %v", err)
	}
	return outcome
}

func TestChildDeletionIsAtomicOrderedCascade(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	for index, fixture := range []string{"feed-bottle.json", "sleep.json", "diaper.json"} {
		m := testkit.MustMutation(t, fixture)
		m.MutationID = mutationID(201 + index)
		outcome, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m)
		if err != nil || outcome.Status != "applied" {
			t.Fatalf("seed %s outcome=%+v err=%v", fixture, outcome, err)
		}
	}
	secondFeed := testkit.MustMutation(t, "feed-bottle.json")
	secondFeed.MutationID, secondFeed.EntityID = mutationID(204), "20000000-0000-4000-8000-000000000000"
	if outcome, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, secondFeed); err != nil || outcome.Status != "applied" {
		t.Fatalf("seed second feed=%+v err=%v", outcome, err)
	}
	// The running timer holds a finished part, so the cascade has timer parts to drop.
	right, sideStart := "right", "2026-09-21T11:10:00.000000Z"
	startFeedWithParts(t, h, babyID, 205, &right, &sideStart, breastPart(editPartA, "left", 600))
	if feeds, timers := h.Count(t, "feed_components", h.FamilyID), h.Count(t, "timer_components", h.FamilyID); feeds != 2 || timers != 1 {
		t.Fatalf("seeded parts: feeds=%d timers=%d", feeds, timers)
	}
	h.RebuildSyncService(t)
	beforeCount, beforeBytes := h.FamilyUsage(t)
	if beforeCount != 4 {
		t.Fatalf("pre-delete canonical count=%d want=4", beforeCount)
	}
	before := h.FamilySequence(t)
	deleted := babyDelete(t, h, 206)
	if deleted.Status != "applied" || deleted.CanonicalEntity == nil || deleted.CanonicalEntity.EntityKind != "baby" || deleted.CanonicalEntity.Payload != nil || len(deleted.RelatedEntities) != 0 {
		t.Fatalf("delete=%+v", deleted)
	}
	if deleted.Cursor == nil || deleted.Cursor.Sequence != before+6 {
		t.Fatalf("cursor=%+v before=%d", deleted.Cursor, before)
	}
	changes, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(h.Cursor(before), nil))
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []string{"baby", "feed", "feed", "sleep", "diaper", "timer"}
	if len(changes.Changes) != len(wantKinds) {
		t.Fatalf("changes=%+v", changes.Changes)
	}
	for index, want := range wantKinds {
		change := changes.Changes[index]
		if change.EntityKind != want || change.Operation != "delete" || change.SnapshotOrTombstone.Payload != nil || change.Cursor.Sequence != before+uint64(index)+1 {
			t.Fatalf("change %d=%+v want kind=%s", index, change, want)
		}
	}
	if changes.Changes[1].EntityID != secondFeed.EntityID || changes.Changes[2].EntityID != "20000000-0000-4000-8000-000000000001" {
		t.Fatalf("feeds not UUID sorted: %+v", changes.Changes[1:3])
	}
	if h.Count(t, "feed_entries", h.FamilyID) != 2 || h.Count(t, "sleep_entries", h.FamilyID) != 1 || h.Count(t, "diaper_entries", h.FamilyID) != 1 || h.Count(t, "active_timers", h.FamilyID) != 1 {
		t.Fatal("cascade physically deleted canonical rows")
	}
	if feeds, timers := h.Count(t, "feed_components", h.FamilyID), h.Count(t, "timer_components", h.FamilyID); feeds != 0 || timers != 0 {
		t.Fatalf("cascade tombstones kept parts: feeds=%d timers=%d", feeds, timers)
	}
	if count, bytes := h.FamilyUsage(t); count != 0 || bytes >= beforeBytes {
		t.Fatalf("post-delete usage=(%d,%d), want count=0 and bytes below %d", count, bytes, beforeBytes)
	}
}

func TestDelayedChildMutationGetsBabyDeleted(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	feed := testkit.MustMutation(t, "feed-bottle.json")
	feed.MutationID, feed.EntityID = mutationID(210), recordA
	if outcome, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, feed); err != nil || outcome.Status != "applied" {
		t.Fatalf("seed feed=%+v err=%v", outcome, err)
	}
	startFeed(t, h, timerA, 211)
	babyDelete(t, h, 212)

	create := testkit.MustMutation(t, "feed-bottle.json")
	create.MutationID, create.EntityID = mutationID(213), recordB
	feedResult, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, create)
	if err != nil || feedResult.Status != "rejected" || feedResult.Code == nil || *feedResult.Code != "baby_deleted" {
		t.Fatalf("feed create=%+v err=%v", feedResult, err)
	}
	update := feed
	update.MutationID, update.Operation = mutationID(216), "update"
	updated, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, update)
	if err != nil || updated.Status != "rejected" || updated.Code == nil || *updated.Code != "baby_deleted" {
		t.Fatalf("feed update=%+v err=%v", updated, err)
	}
	start := testkit.MustMutation(t, "timer-feed.json")
	start.MutationID, start.EntityID = mutationID(214), timerB
	startResult, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, start)
	if err != nil || startResult.Status != "rejected" || startResult.Code == nil || *startResult.Code != "baby_deleted" {
		t.Fatalf("start=%+v err=%v", startResult, err)
	}
	stop := stopFeedAs(t, h, false, timerA, recordB, 215)
	if stop.Status != "rejected" || stop.Code == nil || *stop.Code != "baby_deleted" {
		t.Fatalf("stop=%+v", stop)
	}
}

func TestCaregiverChildDeletionFailsBeforeReceipt(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	m := testkit.MustMutation(t, "baby.json")
	m.MutationID, m.Operation, m.Payload = mutationID(221), "delete", protocol.DeletePayload{}
	beforeReceipts, beforeSequence := h.Count(t, "mutation_receipts", h.FamilyID), h.FamilySequence(t)
	if _, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m); err == nil {
		t.Fatal("caregiver child deletion succeeded")
	}
	if h.Count(t, "mutation_receipts", h.FamilyID) != beforeReceipts || h.FamilySequence(t) != beforeSequence {
		t.Fatal("denied deletion changed receipt or cursor state")
	}
}

func TestChildDeletionSecondBatchFailureRollsBackEntireCascade(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h)
	mutation := testkit.MustMutation(t, "feed-bottle.json")
	mutation.MutationID, mutation.EntityID = mutationID(223), "20000000-0000-4000-8001-000000000001"
	if outcome, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, mutation); err != nil || outcome.Status != "applied" {
		t.Fatalf("seed feed: outcome=%+v err=%v", outcome, err)
	}
	// Seed enough rows in one statement to cross the production metadata batch
	// boundary. The injected failure occurs only after one complete dependent
	// batch has been updated and journaled.
	const dependentCount = 4097
	h.ExecFamily(t, `WITH seed AS (
		SELECT feed.*,journal.actor_account_id,journal.device_id
		FROM feed_entries feed JOIN mutation_journal journal
			ON journal.family_id=feed.family_id AND journal.sequence=feed.last_sequence
		WHERE feed.family_id=$1 AND feed.id=$2
	), advance AS (
		UPDATE families SET next_sequence=next_sequence+$3 WHERE id=$1 RETURNING next_sequence
	), generated AS (
		SELECT seed.*,series,('20000000-0000-4000-8002-' || lpad(to_hex(series),12,'0'))::uuid generated_id,
			advance.next_sequence-($3-series) generated_sequence
		FROM seed CROSS JOIN advance CROSS JOIN generate_series(1,$3) series
	), inserted_journal AS (
		INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,
			actor_account_id,device_id,operation,canonical_entity,accepted_at)
		SELECT family_id,generated_sequence,'feed',generated_id,entity_version,actor_account_id,
			device_id,'create',jsonb_build_object('entity_kind','feed'),accepted_at FROM generated
	)
	INSERT INTO feed_entries(family_id,id,baby_id,entity_version,creator_account_id,
		last_editor_account_id,client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,
		kind,started_at,ended_at,time_zone_id,note)
	SELECT family_id,generated_id,baby_id,entity_version,creator_account_id,last_editor_account_id,
		client_mutation_time,accepted_at,tombstoned_at,generated_sequence,storage_bytes,kind,started_at,
		ended_at,time_zone_id,note FROM generated`,
		h.FamilyID, mutation.EntityID, dependentCount-1)

	beforeSequence := h.FamilySequence(t)
	beforeCount, beforeBytes := h.FamilyUsage(t)
	beforeReceipts := h.Count(t, "mutation_receipts", h.FamilyID)
	beforeJournal := h.Count(t, "mutation_journal", h.FamilyID)
	guard := &cascadeBatchFaultGuard{inner: h.Security, failAt: 3}
	service := familysync.NewService(guard, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now })
	deletion := testkit.MustMutation(t, "baby.json")
	deletion.MutationID, deletion.Operation, deletion.Payload = mutationID(222), "delete", protocol.DeletePayload{}
	if _, err := service.Apply(t.Context(), h.Owner, h.FamilyID, deletion); !errors.Is(err, errCascadeSecondBatch) {
		t.Fatalf("delete error=%v, want injected second-batch failure", err)
	}
	if guard.batches.Load() != 3 {
		t.Fatalf("journal batches=%d, want 3", guard.batches.Load())
	}
	if h.FamilySequence(t) != beforeSequence {
		t.Fatalf("sequence=%d, want %d", h.FamilySequence(t), beforeSequence)
	}
	if count, bytes := h.FamilyUsage(t); count != beforeCount || bytes != beforeBytes {
		t.Fatalf("usage=(%d,%d), want (%d,%d)", count, bytes, beforeCount, beforeBytes)
	}
	if h.Count(t, "mutation_receipts", h.FamilyID) != beforeReceipts {
		t.Fatal("failed cascade stored a receipt")
	}
	if h.Count(t, "mutation_journal", h.FamilyID) != beforeJournal {
		t.Fatal("failed cascade stored journal changes")
	}
	assertLiveCascadeRows(t, h, dependentCount)
}

func assertLiveCascadeRows(t *testing.T, h *testkit.Harness, wantFeeds int) {
	t.Helper()
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		var babies, feeds int
		if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM babies WHERE family_id=$1 AND tombstoned_at IS NULL`, h.FamilyID).Scan(&babies); err != nil {
			return err
		}
		if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM feed_entries WHERE family_id=$1 AND tombstoned_at IS NULL`, h.FamilyID).Scan(&feeds); err != nil {
			return err
		}
		if babies != 1 || feeds != wantFeeds {
			t.Fatalf("live rows babies=%d feeds=%d, want 1/%d", babies, feeds, wantFeeds)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
