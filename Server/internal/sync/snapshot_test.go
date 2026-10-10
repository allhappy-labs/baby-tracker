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
	"babytracker/server/internal/care/caretest"
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
	h.RebuildSyncService(t, h.Limits)
	snapshot, err := fullSnapshot(t, h.Service, h.Owner, protocol.SnapshotRequest{})
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
	h.RebuildSyncService(t, h.Limits)
	live, err := fullSnapshot(t, h.Service, h.Owner, protocol.SnapshotRequest{})
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
	stopped, err := fullSnapshot(t, h.Service, h.Owner, protocol.SnapshotRequest{})
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
	h.RebuildSyncService(t, h.Limits)
	blocker, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	if _, err := blocker.Exec(t.Context(), `LOCK TABLE care_entities IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	// Expire the deadline only after PostgreSQL proves that the snapshot has
	// reached the blocked page read. A wall-clock deadline started before
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
			AND relation = 'care_entities'::regclass AND mode = 'AccessShareLock' AND NOT granted
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
			t.Fatal("snapshot did not reach blocked page read")
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
	if got := h.Count(t, "sync_snapshot_sessions", h.FamilyID); got != 0 {
		t.Fatalf("partial snapshots=%d", got)
	}
	if got := h.Count(t, "sync_page_bindings", h.FamilyID); got != 0 {
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

// A snapshot is not frozen (Phase 2 snapshot ruling): its pages carry the
// one high water recorded at capture, a row changed after the capture is
// served at its current version, and the pull from that high water
// re-delivers it, so the device converges.
func TestSnapshotPagesKeepOneHighWaterAndTheNextPullCatchesUp(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 2
	h.RebuildSyncService(t, h.Limits)
	seeded := h.SeedBabies(t, 3)
	first, err := fullSnapshot(t, h.Service, h.Owner, protocol.SnapshotRequest{})
	if err != nil || first.NextPageToken == nil || len(first.Entities) != 2 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	notYetDelivered := seeded[2].CanonicalEntity
	original := notYetDelivered.Payload.(protocol.BabyPayload).Name
	h.RenameBaby(t, notYetDelivered.Meta.ID, "Changed after capture")
	last, err := fullSnapshot(t, h.Service, h.Owner, protocol.SnapshotRequest{PageToken: first.NextPageToken})
	if err != nil || last.NextPageToken != nil || last.HighWater != first.HighWater || last.SnapshotID != first.SnapshotID {
		t.Fatalf("last=%+v err=%v", last, err)
	}
	if len(last.Entities) != 1 || last.Entities[0].Meta.ID != notYetDelivered.Meta.ID ||
		last.Entities[0].Meta.EntityVersion != 2 || last.Entities[0].Payload.(protocol.BabyPayload).Name == original {
		t.Fatalf("late page did not serve the current row: %+v", last.Entities)
	}
	pull := h.Pull(t, h.Owner, &first.HighWater, nil)
	if len(pull.Changes) != 1 || pull.Changes[0].EntityID != notYetDelivered.Meta.ID || pull.Changes[0].EntityVersion != 2 {
		t.Fatalf("pull after the snapshot=%+v", pull.Changes)
	}
}

func TestSnapshotAccountAdmissionSerializesConcurrentSecondAndThirdStarts(t *testing.T) {
	h := testkit.New(t)
	h.SeedBabies(t, 1)
	h.RebuildSyncService(t, h.Limits)
	if _, err := fullSnapshot(t, h.Service, h.Owner, protocol.SnapshotRequest{}); err != nil {
		t.Fatal(err)
	}
	errorsByAttempt := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			_, err := fullSnapshot(t, h.Service, h.Owner, protocol.SnapshotRequest{})
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
	if allowed != 1 || denied != 1 || h.Count(t, "sync_snapshot_sessions", h.FamilyID) != 2 {
		t.Fatalf("allowed=%d denied=%d snapshots=%d", allowed, denied, h.Count(t, "sync_snapshot_sessions", h.FamilyID))
	}
}

func TestSnapshotFamilyAdmissionSerializesConcurrentEighthAndNinthStarts(t *testing.T) {
	h := testkit.New(t)
	h.SeedBabies(t, 1)
	h.RebuildSyncService(t, h.Limits)
	principals := []access.Principal{h.Owner, h.Caregiver}
	for index := 0; index < 7; index++ {
		principals = append(principals, h.AddCaregiver(t, fmt.Sprintf("snapshot-%d", index)))
	}
	for _, principal := range principals[:7] {
		if _, err := fullSnapshot(t, h.Service, principal, protocol.SnapshotRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	errorsByAttempt := make(chan error, 2)
	start := make(chan struct{})
	for _, principal := range principals[7:9] {
		principal := principal
		go func() {
			<-start
			_, err := fullSnapshot(t, h.Service, principal, protocol.SnapshotRequest{})
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
	if allowed != 1 || denied != 1 || h.Count(t, "sync_snapshot_sessions", h.FamilyID) != 8 {
		t.Fatalf("allowed=%d denied=%d admissions=%d", allowed, denied, h.Count(t, "sync_snapshot_sessions", h.FamilyID))
	}
}

func TestSnapshotContinuationReauthorizes(t *testing.T) {
	h := testkit.New(t)
	h.Limits.SyncPageSize = 1
	h.RebuildSyncService(t, h.Limits)
	h.SeedBabies(t, 2)
	first, err := fullSnapshot(t, h.Service, h.Caregiver, protocol.SnapshotRequest{})
	if err != nil || first.NextPageToken == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err = h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	_, err = fullSnapshot(t, h.Service, h.Caregiver, protocol.SnapshotRequest{PageToken: first.NextPageToken})
	if err == nil || errors.Is(err, familysync.ErrInvalidPageToken) {
		t.Fatalf("continuation did not reauthorize: %v", err)
	}
}

func TestSnapshotEntityCapRollsBackWholeCapture(t *testing.T) {
	h := testkit.New(t)
	baby := h.SeedBabies(t, 1)[0].CanonicalEntity
	sleeps := make([]protocol.Mutation, 0, 1000)
	for n := range 1000 {
		sleeps = append(sleeps, caretest.Record("sleep", baby.Meta.ID, n+1))
	}
	caretest.Seed(t, h, sleeps...)
	h.Limits.SnapshotMaxEntities = 1000
	h.RebuildSyncService(t, h.Limits)
	_, err := fullSnapshot(t, h.Service, h.Owner, protocol.SnapshotRequest{})
	if !errors.Is(err, familysync.ErrSnapshotLimit) {
		t.Fatalf("err=%v", err)
	}
	if got := h.Count(t, "sync_snapshot_sessions", h.FamilyID); got != 0 {
		t.Fatalf("partial snapshots=%d", got)
	}
	if got := h.Count(t, "sync_page_bindings", h.FamilyID); got != 0 {
		t.Fatalf("partial pages=%d", got)
	}

	// Reconstruct the service over the same canonical rows with a raised legal
	// cap. No source mutation occurs between the failed and successful capture.
	h.Limits.SnapshotMaxEntities = 1001
	h.RebuildSyncService(t, h.Limits)
	first, err := fullSnapshot(t, h.Service, h.Owner, protocol.SnapshotRequest{})
	if err != nil || len(first.Entities) != h.Limits.SyncPageSize || first.NextPageToken == nil {
		t.Fatalf("restart snapshot=%d entities, token %v, err=%v", len(first.Entities), first.NextPageToken, err)
	}
	if got := h.Count(t, "sync_snapshot_sessions", h.FamilyID); got != 1 {
		t.Fatalf("snapshots=%d", got)
	}
}

// Snapshot admission is serialized: a capture waits for an uncommitted
// capture of the same account, then counts its session, so the account cap
// holds even when both start together.
func TestSnapshotAdmissionWaitsForAnUncommittedCapture(t *testing.T) {
	h := testkit.New(t)
	h.SeedBabies(t, 1)
	h.RebuildSyncService(t, h.Limits)
	if _, err := fullSnapshot(t, h.Service, h.Owner, protocol.SnapshotRequest{}); err != nil {
		t.Fatal(err)
	}
	guard := &pausingGuard{RequestGuard: h.Authority, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(guard.release) }) }
	t.Cleanup(release) // a failing check still lets the paused capture end
	paused := serviceGuarded(h, guard)
	first := make(chan error, 1)
	go func() {
		_, err := paused.FullSnapshot(context.Background(), h.Owner, protocol.SnapshotRequest{})
		first <- err
	}()
	<-guard.entered // the second session is written, not committed
	second := make(chan error, 1)
	go func() {
		_, err := h.Service.FullSnapshot(context.Background(), h.Owner, protocol.SnapshotRequest{})
		second <- err
	}()
	select {
	case err := <-second:
		t.Fatalf("a capture was admitted beside an uncommitted one: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	release()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; !errors.Is(err, familysync.ErrSnapshotLimit) {
		t.Fatalf("third capture: %v", err)
	}
	if got := h.Count(t, "sync_snapshot_sessions", h.FamilyID); got != 2 {
		t.Fatalf("sessions=%d", got)
	}
}
