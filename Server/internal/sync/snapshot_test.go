package sync_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
)

func TestSnapshotContainsAllTypedKindsAndTombstonesInDeterministicOrder(t *testing.T) {
	h := testkit.New(t)
	baby, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json"))
	if err != nil {
		t.Fatal(err)
	}
	expected := []protocol.CanonicalEntity{*baby.CanonicalEntity}
	for _, fixture := range []string{"feed-bottle.json", "sleep.json", "diaper.json", "supplement.json"} {
		mutation := testkit.MustMutation(t, fixture)
		outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, mutation)
		if err != nil || outcome.CanonicalEntity == nil {
			t.Fatalf("%s outcome=%+v err=%v", fixture, outcome, err)
		}
		if fixture == "sleep.json" {
			babyID := mutation.Payload.(protocol.SleepPayload).BabyID
			deleted, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, protocol.Mutation{MutationID: "01000000-0000-4000-8000-000000000811",
				EntityID: mutation.EntityID, EntityKind: "sleep", Operation: "delete", PayloadVersion: 1, ObservedEntityVersion: 1,
				ClientMutationTime: "2026-09-20T13:00:00.000000Z", Payload: protocol.DeletePayload{BabyID: &babyID}})
			if err != nil || deleted.CanonicalEntity == nil || deleted.CanonicalEntity.Meta.TombstonedAt == nil {
				t.Fatalf("delete=%+v err=%v", deleted, err)
			}
			outcome = deleted
		}
		expected = append(expected, *outcome.CanonicalEntity)
	}
	h.RebuildSyncService(t)
	snapshot, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{})
	if err != nil || snapshot.NextPageToken != nil || !reflect.DeepEqual(snapshot.Entities, expected) {
		t.Fatalf("snapshot=%+v expected=%+v err=%v", snapshot.Entities, expected, err)
	}
}

func TestSnapshotContainsLiveAndStoppedTimerCanonicalForms(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	start := testkit.MustMutation(t, "timer-feed.json")
	started, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, start)
	if err != nil || started.Status != "applied" {
		t.Fatalf("start=%+v err=%v", started, err)
	}
	h.RebuildSyncService(t)
	live, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotTimer(t, live.Entities, start.EntityID, false)

	stop := testkit.MustMutation(t, "timer-feed.json")
	startPayload := stop.Payload.(protocol.TimerStartPayload)
	stop.MutationID, stop.Operation = "01000000-0000-4000-8000-000000000812", "timer_stop"
	stop.Payload = protocol.TimerStopPayload{BabyID: startPayload.BabyID, CompletedRecordID: "20000000-0000-4000-8000-000000000812", StoppedAt: "2026-09-20T15:10:00.000000Z", ComponentID: "40000000-0000-4000-8000-000000000812"}
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stop); err != nil || outcome.Status != "applied" {
		t.Fatalf("stop=%+v err=%v", outcome, err)
	}
	stopped, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotTimer(t, stopped.Entities, start.EntityID, true)
}

func assertSnapshotTimer(t *testing.T, entities []protocol.CanonicalEntity, id string, tombstoned bool) {
	t.Helper()
	for _, entity := range entities {
		if entity.EntityKind != "timer" || entity.Meta.ID != id {
			continue
		}
		if (entity.Meta.TombstonedAt != nil) != tombstoned || tombstoned && entity.Payload != nil {
			t.Fatalf("timer=%+v tombstoned=%t", entity, tombstoned)
		}
		if !tombstoned {
			payload, ok := entity.Payload.(protocol.TimerStartPayload)
			if !ok || payload.TimerKind != "feed" || payload.CurrentSide == nil || *payload.CurrentSide != "left" {
				t.Fatalf("live timer payload=%+v", entity.Payload)
			}
		}
		return
	}
	t.Fatalf("timer %s missing from snapshot", id)
}

func TestSnapshotDeadlineRollsBackAllDurableRows(t *testing.T) {
	h := testkit.New(t)
	h.SeedBabies(t, 1)
	h.RebuildSyncService(t)
	blocker, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	if _, err := blocker.Exec(t.Context(), `LOCK TABLE babies IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	// Expire the deadline only after PostgreSQL proves that the snapshot has
	// reached the blocked babies read. A wall-clock deadline started before
	// family resolution can instead expire in strict authority validation.
	ctx := &snapshotDeadlineContext{Context: context.WithoutCancel(t.Context()), done: make(chan struct{})}
	finished := make(chan struct{})
	var snapshotErr error
	go func() {
		defer close(finished)
		_, snapshotErr = h.Service.FullSnapshot(ctx, h.Owner, protocol.SnapshotRequest{})
	}()
	defer func() {
		ctx.expire()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("snapshot goroutine did not stop after deadline")
		}
	}()
	waitCtx, stopWaiting := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopWaiting()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		var blocked bool
		err := h.Migrator.QueryRow(waitCtx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE database = (SELECT oid FROM pg_database WHERE datname = current_database())
			AND relation = 'babies'::regclass AND mode = 'AccessShareLock' AND NOT granted
			AND $1::int = ANY(pg_blocking_pids(pid)))`, blocker.Conn().PgConn().PID()).Scan(&blocked)
		if err != nil {
			t.Fatalf("observe blocked snapshot read: %v", err)
		}
		if blocked {
			break
		}
		select {
		case <-finished:
			t.Fatalf("snapshot returned before blocked read: %v", snapshotErr)
		case <-waitCtx.Done():
			t.Fatal("snapshot did not reach blocked babies read")
		case <-poll.C:
		}
	}
	ctx.expire()
	select {
	case <-finished:
		if !errors.Is(snapshotErr, context.DeadlineExceeded) {
			t.Fatalf("deadline err=%v", snapshotErr)
		}
	case <-waitCtx.Done():
		t.Fatal("snapshot did not return after deadline")
	}
	if got := h.Count(t, "sync_snapshot_admissions", h.FamilyID); got != 0 {
		t.Fatalf("partial admissions=%d", got)
	}
	if got := h.Count(t, "sync_snapshots", h.FamilyID); got != 0 {
		t.Fatalf("partial snapshots=%d", got)
	}
	if got := h.Count(t, "sync_snapshot_entities", h.FamilyID); got != 0 {
		t.Fatalf("partial entities=%d", got)
	}
	if got := h.Count(t, "sync_page_sessions", h.FamilyID); got != 0 {
		t.Fatalf("partial pages=%d", got)
	}
}

// snapshotDeadlineContext models timer expiry at an observed database boundary.
// It deliberately advertises no wall-clock deadline: only expire closes Done,
// exactly once, after which Err consistently reports DeadlineExceeded.
// The surrounding test independently bounds observation and goroutine cleanup.
type snapshotDeadlineContext struct {
	context.Context
	done       chan struct{}
	expireOnce sync.Once
}

func (c *snapshotDeadlineContext) Done() <-chan struct{} { return c.done }
func (c *snapshotDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func (c *snapshotDeadlineContext) expire() { c.expireOnce.Do(func() { close(c.done) }) }

func TestSnapshotPagesStayAtOneHighWater(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 2
	h.RebuildSyncService(t)
	seeded := h.SeedBabies(t, 3)
	first, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{})
	if err != nil || first.NextPageToken == nil || len(first.Entities) != 2 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	notYetDelivered := seeded[2].CanonicalEntity
	original := notYetDelivered.Payload.(protocol.BabyPayload).Name
	h.RenameBaby(t, notYetDelivered.Meta.ID, "Changed after capture")
	last, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{PageToken: first.NextPageToken})
	if err != nil || last.NextPageToken != nil || last.HighWater != first.HighWater || last.SnapshotID != first.SnapshotID {
		t.Fatalf("last=%+v err=%v", last, err)
	}
	if len(last.Entities) != 1 || last.Entities[0].Meta.ID != notYetDelivered.Meta.ID ||
		last.Entities[0].Meta.EntityVersion != 1 || last.Entities[0].Payload.(protocol.BabyPayload).Name != original {
		t.Fatalf("late page crossed capture boundary: %+v", last.Entities)
	}
}

func TestSnapshotAccountAdmissionSerializesConcurrentSecondAndThirdStarts(t *testing.T) {
	h := testkit.New(t)
	h.SeedBabies(t, 1)
	h.RebuildSyncService(t)
	if _, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{}); err != nil {
		t.Fatal(err)
	}
	errorsByAttempt := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			_, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{})
			errorsByAttempt <- err
		}()
	}
	close(start)
	allowed, denied := 0, 0
	for range 2 {
		if err := <-errorsByAttempt; err == nil {
			allowed++
		} else if errors.Is(err, familysync.ErrSnapshotLimit) {
			denied++
		} else {
			t.Fatalf("unexpected admission error: %v", err)
		}
	}
	if allowed != 1 || denied != 1 || h.Count(t, "sync_snapshots", h.FamilyID) != 2 {
		t.Fatalf("allowed=%d denied=%d snapshots=%d", allowed, denied, h.Count(t, "sync_snapshots", h.FamilyID))
	}
}

func TestSnapshotFamilyAdmissionSerializesConcurrentEighthAndNinthStarts(t *testing.T) {
	h := testkit.New(t)
	h.SeedBabies(t, 1)
	h.RebuildSyncService(t)
	principals := []access.Principal{h.Owner, h.Caregiver}
	for index := 0; index < 7; index++ {
		principals = append(principals, h.AddCaregiver(t, fmt.Sprintf("snapshot-%d", index)))
	}
	for _, principal := range principals[:7] {
		if _, err := h.Service.FullSnapshot(t.Context(), principal, protocol.SnapshotRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	errorsByAttempt := make(chan error, 2)
	start := make(chan struct{})
	for _, principal := range principals[7:9] {
		principal := principal
		go func() {
			<-start
			_, err := h.Service.FullSnapshot(t.Context(), principal, protocol.SnapshotRequest{})
			errorsByAttempt <- err
		}()
	}
	close(start)
	allowed, denied := 0, 0
	for range 2 {
		if err := <-errorsByAttempt; err == nil {
			allowed++
		} else if errors.Is(err, familysync.ErrSnapshotLimit) {
			denied++
		} else {
			t.Fatalf("unexpected admission error: %v", err)
		}
	}
	if allowed != 1 || denied != 1 || h.Count(t, "sync_snapshot_admissions", h.FamilyID) != 8 {
		t.Fatalf("allowed=%d denied=%d admissions=%d", allowed, denied, h.Count(t, "sync_snapshot_admissions", h.FamilyID))
	}
}

func TestSnapshotContinuationReauthorizes(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t)
	h.SeedBabies(t, 2)
	first, err := h.Service.FullSnapshot(t.Context(), h.Caregiver, protocol.SnapshotRequest{})
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err = h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	_, err = h.Service.FullSnapshot(t.Context(), h.Caregiver, protocol.SnapshotRequest{PageToken: first.NextPageToken})
	if err == nil || errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("continuation did not reauthorize: %v", err)
	}
}

func TestSnapshotEntityCapRollsBackWholeCapture(t *testing.T) {
	h := testkit.New(t)
	baby := h.SeedBabies(t, 1)[0].CanonicalEntity
	h.ExecFamily(t, `UPDATE families SET next_sequence=1001 WHERE id=$1`, h.FamilyID)
	h.ExecFamily(t, `WITH source AS (
		SELECT n,('60000000-0000-4000-8000-' || lpad(to_hex(n),12,'0'))::uuid id FROM generate_series(1,1000) n
	), journal AS (
		INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,actor_account_id,device_id,operation,canonical_entity,accepted_at)
		SELECT $1::uuid,n+1,'sleep',id,2,$3::uuid,$4::uuid,'delete',jsonb_build_object('entity_kind','sleep','meta',jsonb_build_object(
		'id',id::text,'family_id',$1::text,'entity_version',2,'creator_account_id',$3::text,'last_editor_account_id',$3::text,
		'client_mutation_time','2026-09-20T12:00:00.000000Z','accepted_at','2026-09-20T12:00:01.000000Z',
		'tombstoned_at','2026-09-20T12:00:02.000000Z'),'payload',NULL), '2026-09-20T12:00:01Z' FROM source
		RETURNING sequence
	)
	INSERT INTO sleep_entries(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,
		client_mutation_time,accepted_at,tombstoned_at,last_sequence,storage_bytes,started_at,ended_at,time_zone_id,note,kind_override)
		SELECT $1::uuid,id,$2::uuid,2,$3::uuid,$3::uuid,'2026-09-20T12:00:00Z','2026-09-20T12:00:01Z','2026-09-20T12:00:02Z',n+1,512,
		'2026-09-20T11:00:00Z','2026-09-20T12:00:00Z','UTC',NULL,NULL FROM source`,
		h.FamilyID, baby.Meta.ID, h.Owner.AccountID, h.Owner.DeviceID)
	h.Limits.SnapshotMaxEntities = 1000
	h.RebuildSyncService(t)
	_, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{})
	if !errors.Is(err, familysync.ErrSnapshotLimit) {
		t.Fatalf("err=%v", err)
	}
	if got := h.Count(t, "sync_snapshots", h.FamilyID); got != 0 {
		t.Fatalf("partial snapshots=%d", got)
	}
	if got := h.Count(t, "sync_snapshot_entities", h.FamilyID); got != 0 {
		t.Fatalf("partial snapshot entities=%d", got)
	}

	// Reconstruct the service over the same canonical rows with a raised legal
	// cap. No source mutation occurs between the failed and successful capture.
	h.Limits.SnapshotMaxEntities = 1001
	h.RebuildSyncService(t)
	first, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{})
	if err != nil || len(first.Entities) == 0 {
		t.Fatalf("restart snapshot=%+v err=%v", first, err)
	}
	if got := h.Count(t, "sync_snapshots", h.FamilyID); got != 1 {
		t.Fatalf("snapshots=%d", got)
	}
	if got := h.Count(t, "sync_snapshot_entities", h.FamilyID); got != 1001 {
		t.Fatalf("snapshot entities=%d", got)
	}
}
