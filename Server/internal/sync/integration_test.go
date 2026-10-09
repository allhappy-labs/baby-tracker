package sync_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/records"
	"babytracker/server/internal/security"
	familysync "babytracker/server/internal/sync"
	"babytracker/server/internal/testkit"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMutationDiagnosticsRunAfterAuthorizedFenceRelease(t *testing.T) {
	h := testkit.NewHosted(t)
	var phases []diagnostics.Phase
	var blocked []diagnostics.Phase
	ctx := diagnostics.WithRecorder(t.Context(), func(_ context.Context, phase diagnostics.Phase, _ time.Duration) {
		// ExecutePrepared needs an exclusive authority fence. If this callback
		// runs inside the mutation's shared lease, it times out as security_busy.
		reentryCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := h.Security.ExecutePrepared(reentryCtx, func(context.Context, security.AuthoritySnapshot) (security.PreparedCommand, error) {
			return security.PreparedCommand{}, nil
		})
		if security.Code(err) == "security_busy" {
			blocked = append(blocked, phase)
		} else if security.Code(err) != "invalid_command" {
			t.Errorf("independent authority reentry for %s returned %v", phase, err)
		}
		phases = append(phases, phase)
	})
	mutation := testkit.MustMutation(t, "baby.json")
	outcome, err := h.Service.Apply(ctx, h.Owner, h.FamilyID, mutation)
	if err != nil || outcome.Status != "applied" {
		t.Fatalf("mutation outcome status=%q err=%v", outcome.Status, err)
	}
	var database, observer int
	for _, phase := range phases {
		if phase == diagnostics.Database {
			database++ // guarded transaction plus actual repository Apply
		}
		if phase == diagnostics.Observer {
			observer++ // actual hosted Schedule
		}
	}
	if database < 2 || observer < 1 || len(blocked) != 0 {
		t.Fatalf("diagnostic phases=%v, callbacks blocked under fence=%v", phases, blocked)
	}
}

func TestReceiptReplayPreservesFirstResult(t *testing.T) {
	h := testkit.New(t)
	m := testkit.MustMutation(t, "baby.json")
	first, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || first.Status != "applied" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	again, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || again.Status != "already_applied" || again.OriginalStatus == nil || *again.OriginalStatus != "applied" {
		t.Fatalf("again=%+v err=%v", again, err)
	}
	if !reflect.DeepEqual(first.CanonicalEntity, again.CanonicalEntity) || first.Cursor == nil || again.Cursor == nil || *first.Cursor != *again.Cursor {
		t.Fatal("replay changed accepted result")
	}
	journalCount, receiptCount := h.Count(t, "mutation_journal", h.FamilyID), h.Count(t, "mutation_receipts", h.FamilyID)
	if journalCount != 1 || receiptCount != 1 {
		t.Fatalf("durable counts journal=%d receipt=%d", journalCount, receiptCount)
	}
}

func TestOrdinaryUpdateReportsOverwrittenVersionForEveryCanonicalKind(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	for index, fixture := range []string{"feed-bottle.json", "sleep.json", "diaper.json", "supplement.json"} {
		m := testkit.MustMutation(t, fixture)
		created, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m)
		if err != nil || created.Status != "applied" || created.CanonicalEntity == nil || created.CanonicalEntity.Meta.CreatorAccountID != h.Caregiver.AccountID {
			t.Fatalf("%s create=%+v err=%v", fixture, created, err)
		}
		m.Operation = "update"
		m.MutationID = fixtureUUID(10 + index)
		m.ObservedEntityVersion = 0
		m.ClientMutationTime = "2026-09-20T13:00:00.000000Z"
		updated, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
		if err != nil || updated.Status != "applied" || updated.CanonicalEntity == nil || updated.CanonicalEntity.Meta.EntityVersion != 2 || !updated.OverwroteNewerVersion || updated.CanonicalEntity.Meta.CreatorAccountID != h.Caregiver.AccountID || updated.CanonicalEntity.Meta.LastEditorAccountID != h.Owner.AccountID {
			t.Fatalf("%s update=%+v err=%v", fixture, updated, err)
		}
	}
	if h.Count(t, "feed_entries", h.FamilyID) != 1 || h.Count(t, "sleep_entries", h.FamilyID) != 1 || h.Count(t, "diaper_entries", h.FamilyID) != 1 || h.Count(t, "supplement_entries", h.FamilyID) != 1 {
		t.Fatal("typed rows missing")
	}
}

func TestTombstoneCannotResurrect(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	m := testkit.MustMutation(t, "sleep.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil {
		t.Fatal(err)
	}
	babyID := m.Payload.(protocol.SleepPayload).BabyID
	del := protocol.Mutation{MutationID: fixtureUUID(30), EntityID: m.EntityID, EntityKind: "sleep", Operation: "delete", PayloadVersion: 1, ObservedEntityVersion: 1, ClientMutationTime: "2026-09-20T13:00:00.000000Z", Payload: protocol.DeletePayload{BabyID: &babyID}}
	deleted, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, del)
	if err != nil || deleted.Status != "applied" || deleted.CanonicalEntity == nil || deleted.CanonicalEntity.Payload != nil || deleted.CanonicalEntity.Meta.TombstonedAt == nil {
		t.Fatalf("delete=%+v err=%v", deleted, err)
	}
	m.Operation = "update"
	m.MutationID = fixtureUUID(31)
	m.ObservedEntityVersion = 1
	later, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || later.Status != "superseded" || later.Code == nil || *later.Code != "entity_deleted" || later.CanonicalEntity == nil || later.CanonicalEntity.Payload != nil {
		t.Fatalf("later=%+v err=%v", later, err)
	}
	if h.Count(t, "mutation_journal", h.FamilyID) != 3 || h.Count(t, "mutation_receipts", h.FamilyID) != 4 {
		t.Fatalf("journal=%d receipts=%d", h.Count(t, "mutation_journal", h.FamilyID), h.Count(t, "mutation_receipts", h.FamilyID))
	}
}

func TestPermanentRejectionReplaysAfterDomainChanges(t *testing.T) {
	h := testkit.New(t)
	m := testkit.MustMutation(t, "feed-bottle.json")
	first, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || first.Status != "rejected" || first.Code == nil || *first.Code != "baby_not_found" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err = h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	again, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || again.Status != "already_applied" || again.OriginalStatus == nil || *again.OriginalStatus != "rejected" {
		t.Fatalf("again=%+v err=%v", again, err)
	}
	if h.Count(t, "feed_entries", h.FamilyID) != 0 {
		t.Fatal("replayed rejection wrote record")
	}
}

func TestDecodedSemanticValidationFailuresAreStableReceiptedRejections(t *testing.T) {
	h := testkit.New(t)
	badDate := testkit.MustMutation(t, "baby.json")
	pBaby := badDate.Payload.(protocol.BabyPayload)
	pBaby.BirthDate = "2026-02-30"
	badDate.Payload = pBaby
	badVolume := testkit.MustMutation(t, "feed-bottle.json")
	badVolume.MutationID = fixtureUUID(501)
	badVolume.EntityID = "20000000-0000-4000-8000-000000000501"
	pFeed := badVolume.Payload.(protocol.FeedPayload)
	pFeed.Components[0].VolumeML = protocol.MustParseDecimal("2001")
	badVolume.Payload = pFeed
	badRange := testkit.MustMutation(t, "sleep.json")
	badRange.MutationID = fixtureUUID(502)
	badRange.EntityID = "30000000-0000-4000-8000-000000000502"
	pSleep := badRange.Payload.(protocol.SleepPayload)
	ended := "2026-09-20T11:00:00.000000Z"
	pSleep.EndedAt = &ended
	badRange.Payload = pSleep
	missingParent := testkit.MustMutation(t, "diaper.json")
	missingParent.MutationID = fixtureUUID(503)
	missingParent.EntityID = "40000000-0000-4000-8000-000000000503"
	for _, test := range []struct {
		name, code string
		m          protocol.Mutation
	}{{"date", "invalid_timestamp", badDate}, {"volume", "invalid_decimal", badVolume}, {"range", "invalid_timestamp", badRange}, {"relationship", "baby_not_found", missingParent}} {
		t.Run(test.name, func(t *testing.T) {
			first, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, test.m)
			if err != nil || first.Status != "rejected" || first.Code == nil || *first.Code != test.code {
				t.Fatalf("first=%+v err=%v", first, err)
			}
			again, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, test.m)
			if err != nil || again.Status != "already_applied" || again.OriginalStatus == nil || *again.OriginalStatus != "rejected" || again.Code == nil || *again.Code != test.code {
				t.Fatalf("again=%+v err=%v", again, err)
			}
		})
	}
	if h.Count(t, "mutation_receipts", h.FamilyID) != 4 || h.Count(t, "mutation_journal", h.FamilyID) != 0 {
		t.Fatal("stable validation rejection durability mismatch")
	}
}

func TestMutationByteBoundaryIsStableAndMinimumPageSurvivesConfigLowering(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	large := testkit.MustMutation(t, "sleep.json")
	payload := large.Payload.(protocol.SleepPayload)
	note := strings.Repeat("n", 4096)
	payload.Note = &note
	large.Payload = payload
	canonical, err := protocol.CanonicalMutationBytes(large)
	if err != nil || len(canonical) <= 4096 {
		t.Fatalf("canonical bytes=%d err=%v", len(canonical), err)
	}
	h.Limits.SyncMutationBytes = len(canonical) - 1
	h.RebuildSyncService(t)
	rejected, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, large)
	if err != nil || rejected.Status != "rejected" || rejected.Code == nil || *rejected.Code != "mutation_too_large" {
		t.Fatalf("rejected=%+v err=%v", rejected, err)
	}
	replay, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, large)
	if err != nil || replay.Status != "already_applied" || replay.Code == nil || *replay.Code != "mutation_too_large" {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}

	accepted := large
	accepted.MutationID = fixtureUUID(710)
	accepted.EntityID = "30000000-0000-4000-8000-000000000710"
	h.Limits.SyncMutationBytes = len(canonical)
	h.Limits.SyncPageBytes = 4 << 20
	h.RebuildSyncService(t)
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, accepted)
	if err != nil || outcome.Status != "applied" {
		t.Fatalf("boundary outcome=%+v err=%v", outcome, err)
	}

	// Page admission is permanently based on the legal 64 KiB minimum, so a
	// later operator restart may lower the configured page bound safely.
	h.Limits.SyncPageBytes = protocol.MinimumSyncPageBytes
	h.RebuildSyncService(t)
	page, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(h.Cursor(1), nil))
	if err != nil || len(page.Changes) != 1 || page.Changes[0].EntityID != accepted.EntityID {
		t.Fatalf("lowered page=%+v err=%v", page, err)
	}
}

func fixtureUUID(value int) string { return fmt.Sprintf("01000000-0000-4000-8000-%012d", value) }

func TestReceiptDifferentHashRejected(t *testing.T) {
	h := testkit.New(t)
	m := testkit.MustMutation(t, "baby.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil {
		t.Fatal(err)
	}
	changed := testkit.MustMutation(t, "baby.json")
	// The same mutation identity with a different operation hash never discloses the prior result.
	changed.ClientMutationTime = "2026-09-20T12:34:57.000000Z"
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, changed); !errors.Is(err, familysync.ErrMutationIDReused) {
		t.Fatalf("err=%v", err)
	}
	if h.Count(t, "mutation_journal", h.FamilyID) != 1 || h.Count(t, "mutation_receipts", h.FamilyID) != 1 {
		t.Fatal("collision changed storage")
	}
}

func TestReceiptSurvivesAuthenticatedDeviceReauthentication(t *testing.T) {
	h := testkit.New(t)
	m := testkit.MustMutation(t, "baby.json")
	first, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := h.Reauthenticate("owner")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.DeviceID != h.Owner.DeviceID || fresh.SessionID == h.Owner.SessionID {
		t.Fatalf("fresh=%+v old=%+v", fresh, h.Owner)
	}
	again, err := h.Service.Apply(t.Context(), fresh, h.FamilyID, m)
	if err != nil || again.Status != "already_applied" || again.Cursor == nil || first.Cursor == nil || *again.Cursor != *first.Cursor {
		t.Fatalf("again=%+v err=%v", again, err)
	}
}

type harnessResolver struct{ h *testkit.Harness }

func (r harnessResolver) Current(ctx context.Context, p access.Principal) (string, error) {
	view, err := r.h.Family.Current(ctx, p)
	return view.ID, err
}

type captureObserver struct {
	schedules, publishes      int
	grant                     access.Grant
	device                    string
	failSchedule, failPublish bool
}

var errInjectedCommitBoundary = errors.New("injected commit boundary failure")

type errorRow struct{ err error }

func (r errorRow) Scan(...any) error { return r.err }

type faultTx struct {
	pgx.Tx
	mode string
}

func (tx faultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if tx.mode == "after_entity" && strings.Contains(sql, "INSERT INTO mutation_journal") {
		return pgconn.CommandTag{}, errInjectedCommitBoundary
	}
	if tx.mode == "after_journal" && strings.Contains(sql, "SET last_sequence=$3") {
		return pgconn.CommandTag{}, errInjectedCommitBoundary
	}
	// Child cascades journal bounded homogeneous batches. Let the root batch
	// complete, then fail the first dependent batch to retain the original
	// after-first-journal atomicity coverage.
	if tx.mode == "after_journal" && strings.Contains(sql, "child-cascade-journal") && len(args) > 6 && args[6] == "feed" {
		return pgconn.CommandTag{}, errInjectedCommitBoundary
	}
	return tx.Tx.Exec(ctx, sql, args...)
}
func (tx faultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx.mode == "before_receipt" && strings.Contains(sql, "app.save_mutation_receipt") {
		return errorRow{errInjectedCommitBoundary}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

type faultGuard struct {
	inner records.Guard
	mode  string
}

func (g faultGuard) WithAuthorizedTransaction(ctx context.Context, p access.Principal, familyID, action string, run func(pgx.Tx, access.Grant) error) error {
	return g.inner.WithAuthorizedTransaction(ctx, p, familyID, action, func(tx pgx.Tx, grant access.Grant) error { return run(faultTx{Tx: tx, mode: g.mode}, grant) })
}

func (o *captureObserver) Schedule(_ context.Context, _ pgx.Tx, grant access.Grant, device string, _ protocol.Cursor) error {
	o.schedules++
	o.grant, o.device = grant, device
	if o.failSchedule {
		return errors.New("schedule failed")
	}
	return nil
}
func (o *captureObserver) Publish(_ context.Context, _ string, _ protocol.Cursor) error {
	o.publishes++
	if o.failPublish {
		return errors.New("publish failed")
	}
	return nil
}

func TestCommitObserverSchedulesInTransactionAndPublishesOnlyAfterCommit(t *testing.T) {
	h := testkit.New(t)
	observer := &captureObserver{}
	service := familysync.NewServiceWithResolver(h.Security, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now }, harnessResolver{h}, observer)
	outcome, err := service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json"))
	if err != nil || outcome.Status != "applied" {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if observer.schedules != 1 || observer.publishes != 1 || observer.grant.AccountID != h.Owner.AccountID || observer.device != h.Owner.DeviceID {
		t.Fatalf("observer=%+v", observer)
	}
}

func TestCommitObserverScheduleFailureRollsBackAndPublishFailureDoesNotLieAboutCommit(t *testing.T) {
	t.Run("schedule", func(t *testing.T) {
		h := testkit.New(t)
		observer := &captureObserver{failSchedule: true}
		service := familysync.NewServiceWithResolver(h.Security, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now }, harnessResolver{h}, observer)
		if _, err := service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err == nil {
			t.Fatal("schedule failure accepted")
		}
		if h.Count(t, "babies", h.FamilyID) != 0 || h.Count(t, "mutation_journal", h.FamilyID) != 0 || h.Count(t, "mutation_receipts", h.FamilyID) != 0 || observer.publishes != 0 {
			t.Fatal("schedule failure left durable or published state")
		}
	})
	t.Run("publish", func(t *testing.T) {
		h := testkit.New(t)
		observer := &captureObserver{failPublish: true}
		service := familysync.NewServiceWithResolver(h.Security, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now }, harnessResolver{h}, observer)
		outcome, err := service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json"))
		if err != nil || outcome.Status != "applied" {
			t.Fatalf("outcome=%+v err=%v", outcome, err)
		}
		if h.Count(t, "mutation_receipts", h.FamilyID) != 1 || observer.publishes != 1 {
			t.Fatal("post-commit publish changed commit result")
		}
	})
}

func TestCommitObserverIgnoresCursorBearingNoopAndReceiptReplay(t *testing.T) {
	h := testkit.New(t)
	observer := &captureObserver{}
	service := familysync.NewServiceWithResolver(h.Security, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now }, harnessResolver{h}, observer)
	h.Service = service
	baby := testkit.MustMutation(t, "baby.json")
	if _, err := service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil {
		t.Fatal(err)
	}
	observer.schedules, observer.publishes = 0, 0
	if replay, err := service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil || replay.Status != "already_applied" {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if observer.schedules != 0 || observer.publishes != 0 {
		t.Fatalf("receipt replay observed schedules=%d publishes=%d", observer.schedules, observer.publishes)
	}
	started := h.StartFeed(t, h.Owner, testkit.TimerA)
	payload := started.CanonicalEntity.Payload.(protocol.TimerStartPayload)
	before := h.FamilySequence(t)
	observer.schedules, observer.publishes = 0, 0
	startedAt := payload.StartedAt
	edit := protocol.Mutation{MutationID: h.CommandID(), EntityID: testkit.TimerA, EntityKind: "timer", Operation: "update",
		PayloadVersion: 5, ClientMutationTime: startedAt,
		Payload: protocol.TimerEditPayload{BabyID: payload.BabyID, StartedAt: &startedAt, Components: []protocol.FeedComponent{}}}
	noop, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, edit)
	if err != nil {
		t.Fatal(err)
	}
	if noop.Status != "applied" || noop.Cursor == nil || noop.Cursor.Sequence != before || h.FamilySequence(t) != before {
		t.Fatalf("unchanged-edit noop=%+v before=%d after=%d", noop, before, h.FamilySequence(t))
	}
	if observer.schedules != 0 || observer.publishes != 0 {
		t.Fatalf("unchanged-edit no-op observed schedules=%d publishes=%d", observer.schedules, observer.publishes)
	}
}

func TestMutationFaultsRollbackAfterEntityAfterJournalAndBeforeReceipt(t *testing.T) {
	for _, mode := range []string{"after_entity", "after_journal", "before_receipt"} {
		t.Run(mode, func(t *testing.T) {
			h := testkit.New(t)
			service := familysync.NewServiceWithResolver(faultGuard{inner: h.Security, mode: mode}, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now }, harnessResolver{h}, nil)
			if _, err := service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); !errors.Is(err, errInjectedCommitBoundary) {
				t.Fatalf("err=%v", err)
			}
			if h.Count(t, "babies", h.FamilyID) != 0 || h.Count(t, "mutation_journal", h.FamilyID) != 0 || h.Count(t, "mutation_receipts", h.FamilyID) != 0 || h.FamilySequence(t) != 0 {
				t.Fatal("fault left partial transaction")
			}
		})
	}
}

func TestTransientQuotaRollbackHasNoReceiptOrPartialState(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	h.ExecFamily(t, `UPDATE family_record_usage SET charged_storage_bytes=$2 WHERE family_id=$1`, h.FamilyID, h.Limits.MaxChargedStorageBytesPerFamily-1)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "feed-bottle.json")); !errors.Is(err, records.ErrQuotaExceeded) {
		t.Fatalf("err=%v", err)
	}
	if h.Count(t, "feed_entries", h.FamilyID) != 0 || h.Count(t, "mutation_journal", h.FamilyID) != 1 || h.Count(t, "mutation_receipts", h.FamilyID) != 1 {
		t.Fatal("quota failure left partial durable state")
	}
}

func TestTimerStopFaultAfterFirstJournalRollsBackAtomically(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	start := testkit.MustMutation(t, "timer-feed.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, start); err != nil {
		t.Fatal(err)
	}
	stop := timerStopMutation(t, start, fixtureUUID(910), "20000000-0000-4000-8000-000000000910")
	service := familysync.NewServiceWithResolver(faultGuard{inner: h.Security, mode: "after_journal"}, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now }, harnessResolver{h}, nil)
	if _, err := service.Apply(t.Context(), h.Owner, h.FamilyID, stop); !errors.Is(err, errInjectedCommitBoundary) {
		t.Fatalf("err=%v", err)
	}
	if h.Count(t, "feed_entries", h.FamilyID) != 0 || h.Count(t, "mutation_journal", h.FamilyID) != 2 || h.Count(t, "mutation_receipts", h.FamilyID) != 2 || h.FamilySequence(t) != 2 {
		t.Fatal("fault left partial timer completion")
	}
	stop.MutationID = fixtureUUID(911)
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stop)
	if err != nil || outcome.Status != "applied" || h.Count(t, "feed_entries", h.FamilyID) != 1 {
		t.Fatalf("retry=%+v err=%v", outcome, err)
	}
}

func TestTimerStopQuotaRollbackAndRecordCountAccounting(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	start := testkit.MustMutation(t, "timer-feed.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, start); err != nil {
		t.Fatal(err)
	}
	count, _ := h.FamilyUsage(t)
	if count != 0 {
		t.Fatalf("active timer counted as canonical record: %d", count)
	}
	h.ExecFamily(t, `UPDATE family_record_usage SET canonical_record_count=$2 WHERE family_id=$1`, h.FamilyID, h.Limits.MaxCanonicalRecordsPerFamily)
	stop := timerStopMutation(t, start, fixtureUUID(920), "20000000-0000-4000-8000-000000000920")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stop); !errors.Is(err, records.ErrQuotaExceeded) {
		t.Fatalf("err=%v", err)
	}
	if h.Count(t, "feed_entries", h.FamilyID) != 0 || h.Count(t, "mutation_journal", h.FamilyID) != 2 || h.Count(t, "mutation_receipts", h.FamilyID) != 2 {
		t.Fatal("quota failure left partial timer state")
	}
	h.ExecFamily(t, `UPDATE family_record_usage SET canonical_record_count=0 WHERE family_id=$1`, h.FamilyID)
	stop.MutationID = fixtureUUID(921)
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, stop); err != nil || outcome.Status != "applied" {
		t.Fatalf("stop=%+v err=%v", outcome, err)
	}
	count, _ = h.FamilyUsage(t)
	if count != 1 {
		t.Fatalf("completion count=%d want=1", count)
	}
}

func TestChildCascadeFaultAfterFirstJournalRollsBackAtomically(t *testing.T) {
	h := testkit.New(t)
	baby := testkit.MustMutation(t, "baby.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "feed-bottle.json")); err != nil {
		t.Fatal(err)
	}
	deleted := baby
	deleted.MutationID, deleted.Operation, deleted.Payload = fixtureUUID(930), "delete", protocol.DeletePayload{}
	service := familysync.NewServiceWithResolver(faultGuard{inner: h.Security, mode: "after_journal"}, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now }, harnessResolver{h}, nil)
	if _, err := service.Apply(t.Context(), h.Owner, h.FamilyID, deleted); !errors.Is(err, errInjectedCommitBoundary) {
		t.Fatalf("err=%v", err)
	}
	if h.Count(t, "mutation_journal", h.FamilyID) != 2 || h.Count(t, "mutation_receipts", h.FamilyID) != 2 || h.FamilySequence(t) != 2 {
		t.Fatal("cascade fault left partial durable state")
	}
	deleted.MutationID = fixtureUUID(931)
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, deleted); err != nil || outcome.Status != "applied" {
		t.Fatalf("retry=%+v err=%v", outcome, err)
	}
}

func timerStopMutation(t *testing.T, start protocol.Mutation, mutationID, completionID string) protocol.Mutation {
	t.Helper()
	payload := start.Payload.(protocol.TimerStartPayload)
	return protocol.Mutation{MutationID: mutationID, EntityID: start.EntityID, EntityKind: "timer", Operation: "timer_stop", PayloadVersion: 5,
		ObservedEntityVersion: 1, ClientMutationTime: "2026-09-20T15:10:00.000000Z",
		Payload: protocol.TimerStopPayload{BabyID: payload.BabyID, CompletedRecordID: completionID, StoppedAt: "2026-09-20T15:10:00.000000Z", ComponentID: "4" + completionID[1:]}}
}

func TestRecordDeleteReceiptRemainsQuotaExempt(t *testing.T) {
	for _, test := range []struct {
		name         string
		count, bytes int64
	}{{"lowered below current", 101, (16 << 20) + (1 << 20)}, {"exactly at limits", 100, 16 << 20}} {
		t.Run(test.name, func(t *testing.T) {
			h := testkit.New(t)
			if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
				t.Fatal(err)
			}
			feed := testkit.MustMutation(t, "feed-bottle.json")
			if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, feed); err != nil {
				t.Fatal(err)
			}
			h.ExecFamily(t, `UPDATE family_record_usage SET canonical_record_count=$2,charged_storage_bytes=$3 WHERE family_id=$1`, h.FamilyID, test.count, test.bytes)
			limits := h.Limits
			limits.MaxCanonicalRecordsPerFamily = 100
			limits.MaxChargedStorageBytesPerFamily = 16 << 20
			info := h.Info
			info.Limits = &limits
			service := familysync.NewServiceWithResolver(h.Security, records.NewRepository(), info, limits, func() time.Time { return h.Now }, harnessResolver{h}, nil)
			babyID := feed.Payload.(protocol.FeedPayload).BabyID
			del := protocol.Mutation{MutationID: fixtureUUID(550), EntityID: feed.EntityID, EntityKind: "feed", Operation: "delete", PayloadVersion: 1, ObservedEntityVersion: 1, ClientMutationTime: "2026-09-20T15:00:00.000000Z", Payload: protocol.DeletePayload{BabyID: &babyID}}
			outcome, err := service.Apply(t.Context(), h.Owner, h.FamilyID, del)
			if err != nil || outcome.Status != "applied" {
				t.Fatalf("delete=%+v err=%v", outcome, err)
			}
			count, _ := h.FamilyUsage(t)
			if count != test.count-1 {
				t.Fatalf("count=%d want=%d", count, test.count-1)
			}
			if h.Count(t, "mutation_receipts", h.FamilyID) != 3 {
				t.Fatal("delete receipt missing")
			}
		})
	}
}

func TestRejectedDeleteAboveQuotaDoesNotPersistReceipt(t *testing.T) {
	h := testkit.New(t)
	first := testkit.MustMutation(t, "baby.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, first); err != nil {
		t.Fatal(err)
	}
	second := testkit.MustMutation(t, "baby.json")
	second.MutationID = fixtureUUID(560)
	second.EntityID = "10000000-0000-4000-8000-000000000560"
	p := second.Payload.(protocol.BabyPayload)
	p.Name = "Second"
	second.Payload = p
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, second); err != nil {
		t.Fatal(err)
	}
	feed := testkit.MustMutation(t, "feed-bottle.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, feed); err != nil {
		t.Fatal(err)
	}
	h.ExecFamily(t, `UPDATE family_record_usage SET canonical_record_count=101,charged_storage_bytes=$2 WHERE family_id=$1`, h.FamilyID, (16<<20)+(1<<20))
	wrongParent := second.EntityID
	del := protocol.Mutation{MutationID: fixtureUUID(561), EntityID: feed.EntityID, EntityKind: "feed", Operation: "delete", PayloadVersion: 1, ObservedEntityVersion: 1, ClientMutationTime: "2026-09-20T15:00:00.000000Z", Payload: protocol.DeletePayload{BabyID: &wrongParent}}
	limits := h.Limits
	limits.MaxCanonicalRecordsPerFamily = 100
	limits.MaxChargedStorageBytesPerFamily = 16 << 20
	info := h.Info
	info.Limits = &limits
	service := familysync.NewServiceWithResolver(h.Security, records.NewRepository(), info, limits, func() time.Time { return h.Now }, harnessResolver{h}, nil)
	if _, err := service.Apply(t.Context(), h.Owner, h.FamilyID, del); !errors.Is(err, records.ErrQuotaExceeded) {
		t.Fatalf("err=%v", err)
	}
	if h.Count(t, "mutation_receipts", h.FamilyID) != 3 || h.Count(t, "mutation_journal", h.FamilyID) != 3 {
		t.Fatal("over-quota rejected delete persisted")
	}
}

func TestTransientRollbackHasNoReceipt(t *testing.T) {
	h := testkit.New(t)
	h.ExecFamily(t, `UPDATE families SET next_sequence=9007199254740991 WHERE id=$1`, h.FamilyID)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err == nil {
		t.Fatal("exhausted cursor accepted")
	}
	if h.Count(t, "babies", h.FamilyID) != 0 || h.Count(t, "mutation_journal", h.FamilyID) != 0 || h.Count(t, "mutation_receipts", h.FamilyID) != 0 || h.FamilySequence(t) != protocol.MaxSafeInteger {
		t.Fatal("cursor exhaustion left partial state")
	}
}

func TestConfiguredCanonicalRecordAndBabyCapsRollbackWithoutReceipt(t *testing.T) {
	t.Run("record count", func(t *testing.T) {
		h := testkit.New(t)
		if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
			t.Fatal(err)
		}
		h.ExecFamily(t, `UPDATE family_record_usage SET canonical_record_count=$2 WHERE family_id=$1`, h.FamilyID, h.Limits.MaxCanonicalRecordsPerFamily)
		if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "sleep.json")); !errors.Is(err, records.ErrQuotaExceeded) {
			t.Fatalf("err=%v", err)
		}
		if h.Count(t, "sleep_entries", h.FamilyID) != 0 || h.Count(t, "mutation_receipts", h.FamilyID) != 1 {
			t.Fatal("record cap left state")
		}
	})
	t.Run("live babies", func(t *testing.T) {
		h := testkit.New(t)
		limits := h.Limits
		limits.MaxLiveBabies = 1
		info := h.Info
		info.Limits = &limits
		service := familysync.NewServiceWithResolver(h.Security, records.NewRepository(), info, limits, func() time.Time { return h.Now }, harnessResolver{h}, nil)
		if _, err := service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
			t.Fatal(err)
		}
		second := testkit.MustMutation(t, "baby.json")
		second.MutationID = fixtureUUID(400)
		second.EntityID = "10000000-0000-4000-8000-000000000400"
		payload := second.Payload.(protocol.BabyPayload)
		payload.Name = "Over cap"
		second.Payload = payload
		if _, err := service.Apply(t.Context(), h.Owner, h.FamilyID, second); !errors.Is(err, records.ErrQuotaExceeded) {
			t.Fatalf("err=%v", err)
		}
		if h.Count(t, "babies", h.FamilyID) != 1 || h.Count(t, "mutation_receipts", h.FamilyID) != 1 {
			t.Fatal("baby cap left state")
		}
	})
}

func TestEntityKindAndParentCollisionsAreStableReceiptedRejections(t *testing.T) {
	h := testkit.New(t)
	baby := testkit.MustMutation(t, "baby.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil {
		t.Fatal(err)
	}
	feed := testkit.MustMutation(t, "feed-bottle.json")
	feed.EntityID = baby.EntityID
	feed.MutationID = fixtureUUID(50)
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, feed)
	if err != nil || outcome.Status != "rejected" || outcome.Code == nil || *outcome.Code != "entity_kind_collision" {
		t.Fatalf("collision=%+v err=%v", outcome, err)
	}
	valid := testkit.MustMutation(t, "feed-bottle.json")
	if _, err = h.Service.Apply(t.Context(), h.Owner, h.FamilyID, valid); err != nil {
		t.Fatal(err)
	}
	update := valid
	update.Operation = "update"
	update.MutationID = fixtureUUID(52)
	payload := update.Payload.(protocol.FeedPayload)
	payload.BabyID = fixtureUUID(51)
	update.Payload = payload
	outcome, err = h.Service.Apply(t.Context(), h.Owner, h.FamilyID, update)
	if err != nil || outcome.Status != "rejected" || outcome.Code == nil || *outcome.Code != "record_parent_mismatch" {
		t.Fatalf("parent=%+v err=%v", outcome, err)
	}
}

func TestReceiptReplayPrecedesChangedQuotaAdmission(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	m := testkit.MustMutation(t, "feed-bottle.json")
	first, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil {
		t.Fatal(err)
	}
	h.ExecFamily(t, `UPDATE family_record_usage SET charged_storage_bytes=$2 WHERE family_id=$1`, h.FamilyID, h.Limits.MaxChargedStorageBytesPerFamily)
	again, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || again.Status != "already_applied" || again.Cursor == nil || first.Cursor == nil || *again.Cursor != *first.Cursor {
		t.Fatalf("again=%+v err=%v", again, err)
	}
}

func TestReceiptCollisionInOldFamilyRollsBackTentativeNewFamilyWrite(t *testing.T) {
	h := testkit.New(t)
	m := testkit.MustMutation(t, "baby.json")
	if _, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Family.Leave(t.Context(), h.Caregiver, h.CommandID()); err != nil {
		t.Fatal(err)
	}
	fresh, err := h.Reauthenticate("caregiver")
	if err != nil {
		t.Fatal(err)
	}
	created, err := h.Family.Create(t.Context(), fresh, h.CommandID(), "Second family")
	if err != nil {
		t.Fatal(err)
	}
	fresh = h.Refresh("caregiver")
	if _, err = h.Service.Apply(t.Context(), fresh, created.ID, m); !errors.Is(err, familysync.ErrMutationIDReused) {
		t.Fatalf("err=%v", err)
	}
	if h.Count(t, "babies", created.ID) != 0 || h.Count(t, "mutation_journal", created.ID) != 0 || h.Count(t, "mutation_receipts", created.ID) != 0 {
		t.Fatal("collision left tentative state in new family")
	}
	if h.Count(t, "babies", h.FamilyID) != 1 || h.Count(t, "mutation_receipts", h.FamilyID) != 1 {
		t.Fatal("collision changed original family")
	}
}

func TestCrossAccountDeviceContinuityCannotReadReceipt(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	if err := h.CrossAccountContinuity("outsider", "owner"); auth.Code(err) != "device_continuity_invalid" {
		t.Fatalf("code=%q err=%v", auth.Code(err), err)
	}
	if h.Count(t, "mutation_receipts", h.FamilyID) != 1 {
		t.Fatal("cross-account continuity changed receipt")
	}
}

func TestSameMutationIDOnTwoAuthenticatedDevicesIsIndependent(t *testing.T) {
	h := testkit.New(t)
	second, err := h.NewDevice("owner")
	if err != nil {
		t.Fatal(err)
	}
	if second.AccountID != h.Owner.AccountID || second.DeviceID == h.Owner.DeviceID || second.Scope != "family" {
		t.Fatalf("second=%+v owner=%+v", second, h.Owner)
	}
	m := testkit.MustMutation(t, "baby.json")
	first, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || first.Status != "applied" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	secondOutcome, err := h.Service.Apply(t.Context(), second, h.FamilyID, m)
	if err != nil || secondOutcome.Status != "rejected" || secondOutcome.Code == nil || *secondOutcome.Code != "entity_exists" {
		t.Fatalf("second=%+v err=%v", secondOutcome, err)
	}
	if h.Count(t, "mutation_receipts", h.FamilyID) != 2 || h.Count(t, "mutation_journal", h.FamilyID) != 1 {
		t.Fatal("device-scoped receipts were not independent")
	}
}

type pausingGuard struct {
	inner   records.Guard
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

type preSQLGuard struct {
	inner   records.Guard
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *preSQLGuard) WithAuthorizedTransaction(ctx context.Context, p access.Principal, familyID, action string, run func(pgx.Tx, access.Grant) error) error {
	g.once.Do(func() { close(g.entered); <-g.release })
	return g.inner.WithAuthorizedTransaction(ctx, p, familyID, action, run)
}

func (g *pausingGuard) WithAuthorizedTransaction(ctx context.Context, p access.Principal, familyID, action string, run func(pgx.Tx, access.Grant) error) error {
	return g.inner.WithAuthorizedTransaction(ctx, p, familyID, action, func(tx pgx.Tx, grant access.Grant) error {
		err := run(tx, grant)
		g.once.Do(func() { close(g.entered); <-g.release })
		return err
	})
}

func TestCommitOrderFollowsFamilyLock(t *testing.T) {
	h := testkit.New(t)
	guard := &pausingGuard{inner: h.Security, entered: make(chan struct{}), release: make(chan struct{})}
	var ticks atomic.Int64
	clock := func() time.Time { return h.Now.Add(time.Duration(ticks.Add(1)) * time.Second) }
	firstService := familysync.NewServiceWithResolver(guard, records.NewRepository(), h.Info, h.Limits, clock, harnessResolver{h}, nil)
	secondService := familysync.NewServiceWithResolver(h.Security, records.NewRepository(), h.Info, h.Limits, clock, harnessResolver{h}, nil)
	firstMutation := testkit.MustMutation(t, "baby.json")
	secondMutation := testkit.MustMutation(t, "baby.json")
	secondMutation.MutationID = fixtureUUID(70)
	secondMutation.EntityID = "10000000-0000-4000-8000-000000000070"
	payload := secondMutation.Payload.(protocol.BabyPayload)
	payload.Name = "Second"
	secondMutation.Payload = payload
	type result struct {
		outcome protocol.Outcome
		err     error
	}
	firstDone, secondDone := make(chan result, 1), make(chan result, 1)
	go func() {
		o, e := firstService.Apply(context.Background(), h.Owner, h.FamilyID, firstMutation)
		firstDone <- result{o, e}
	}()
	<-guard.entered
	started := make(chan struct{})
	go func() {
		close(started)
		o, e := secondService.Apply(context.Background(), h.Caregiver, h.FamilyID, secondMutation)
		secondDone <- result{o, e}
	}()
	<-started
	if got := h.FamilySequence(t); got != 0 {
		t.Fatalf("uncommitted high water=%d", got)
	}
	select {
	case got := <-secondDone:
		t.Fatalf("second completed before first commit: %+v", got)
	default:
	}
	close(guard.release)
	first := <-firstDone
	second := <-secondDone
	if first.err != nil || second.err != nil || first.outcome.Cursor == nil || second.outcome.Cursor == nil || first.outcome.Cursor.Sequence != 1 || second.outcome.Cursor.Sequence != 2 {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	if first.outcome.CanonicalEntity.Meta.AcceptedAt >= second.outcome.CanonicalEntity.Meta.AcceptedAt {
		t.Fatalf("acceptance order first=%s second=%s", first.outcome.CanonicalEntity.Meta.AcceptedAt, second.outcome.CanonicalEntity.Meta.AcceptedAt)
	}
}

func TestServerAcceptanceTimeUsesCanonicalTiesToEvenMicroseconds(t *testing.T) {
	h := testkit.New(t)
	accepted := time.Date(2026, 9, 21, 13, 0, 0, 123456500, time.UTC)
	service := familysync.NewServiceWithResolver(h.Security, records.NewRepository(), h.Info, h.Limits, func() time.Time { return accepted }, harnessResolver{h}, nil)
	outcome, err := service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := outcome.CanonicalEntity.Meta.AcceptedAt; got != "2026-09-21T13:00:00.123456Z" {
		t.Fatalf("accepted_at=%q", got)
	}
}

func TestRevocationFencePreventsLaterWrite(t *testing.T) {
	h := testkit.New(t)
	guard := &pausingGuard{inner: h.Security, entered: make(chan struct{}), release: make(chan struct{})}
	service := familysync.NewServiceWithResolver(guard, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now }, harnessResolver{h}, nil)
	writeDone := make(chan error, 1)
	go func() {
		_, err := service.Apply(context.Background(), h.Caregiver, h.FamilyID, testkit.MustMutation(t, "baby.json"))
		writeDone <- err
	}()
	<-guard.entered
	revokeStarted := make(chan struct{})
	revokeDone := make(chan error, 1)
	go func() {
		close(revokeStarted)
		_, err := h.Family.Remove(context.Background(), h.Owner, h.CommandID(), h.Caregiver.AccountID)
		revokeDone <- err
	}()
	<-revokeStarted
	select {
	case err := <-revokeDone:
		t.Fatalf("revocation crossed active SQL fence: %v", err)
	default:
	}
	close(guard.release)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-revokeDone; err != nil {
		t.Fatal(err)
	}
	later := testkit.MustMutation(t, "baby.json")
	later.MutationID = fixtureUUID(80)
	later.EntityID = "10000000-0000-4000-8000-000000000080"
	payload := later.Payload.(protocol.BabyPayload)
	payload.Name = "Denied"
	later.Payload = payload
	if _, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, later); err == nil {
		t.Fatal("revoked principal wrote after removal")
	}
	if h.Count(t, "babies", h.FamilyID) != 1 {
		t.Fatal("revoked write changed rows")
	}
}

func TestRevocationCommittedBeforeSQLAuthorizationDeniesStaleRequest(t *testing.T) {
	h := testkit.New(t)
	guard := &preSQLGuard{inner: h.Security, entered: make(chan struct{}), release: make(chan struct{})}
	service := familysync.NewServiceWithResolver(guard, records.NewRepository(), h.Info, h.Limits, func() time.Time { return h.Now }, harnessResolver{h}, nil)
	done := make(chan error, 1)
	go func() {
		_, err := service.Apply(context.Background(), h.Caregiver, h.FamilyID, testkit.MustMutation(t, "baby.json"))
		done <- err
	}()
	<-guard.entered
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	close(guard.release)
	if err := <-done; err == nil {
		t.Fatal("stale request crossed committed revocation")
	}
	if h.Count(t, "babies", h.FamilyID) != 0 || h.Count(t, "mutation_receipts", h.FamilyID) != 0 {
		t.Fatal("stale request left durable state")
	}
}

func TestBabyDeleteAndTimerCreateReceipts(t *testing.T) {
	h := testkit.New(t)
	baby := testkit.MustMutation(t, "baby.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil {
		t.Fatal(err)
	}
	baby.Operation = "delete"
	baby.MutationID = fixtureUUID(90)
	baby.Payload = protocol.DeletePayload{}
	deleted, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby)
	if err != nil || deleted.Status != "applied" {
		t.Fatalf("baby delete=%+v err=%v", deleted, err)
	}
	timer := testkit.MustMutation(t, "timer-sleep.json")
	started, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, timer)
	if err != nil || started.Status != "rejected" || started.Code == nil || *started.Code != "baby_deleted" {
		t.Fatalf("timer=%+v err=%v", started, err)
	}
	if h.Count(t, "mutation_receipts", h.FamilyID) != 3 || h.Count(t, "mutation_journal", h.FamilyID) != 2 {
		t.Fatal("feature outcomes were not persisted exactly once")
	}
}

func TestFeedTombstoneRemovesCanonicalChildRows(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	for index, fixture := range []string{"feed-breast.json", "feed-solids.json"} {
		m := testkit.MustMutation(t, fixture)
		m.MutationID = fixtureUUID(300 + index)
		m.EntityID = fmt.Sprintf("20000000-0000-4000-8000-%012d", 300+index)
		if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil {
			t.Fatal(err)
		}
		babyID := m.Payload.(protocol.FeedPayload).BabyID
		del := protocol.Mutation{MutationID: fixtureUUID(310 + index), EntityID: m.EntityID, EntityKind: "feed", Operation: "delete", PayloadVersion: 1, ObservedEntityVersion: 1, ClientMutationTime: "2026-09-20T15:00:00.000000Z", Payload: protocol.DeletePayload{BabyID: &babyID}}
		if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, del); err != nil {
			t.Fatal(err)
		}
	}
	if h.Count(t, "feed_components", h.FamilyID) != 0 {
		t.Fatal("feed tombstones retained their parts")
	}
}

func TestDurableUsageAccountingMatchesCanonicalJournalReceiptAndDeleteReserve(t *testing.T) {
	h := testkit.New(t)
	babyMutation := testkit.MustMutation(t, "baby.json")
	baby, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, babyMutation)
	if err != nil {
		t.Fatal(err)
	}
	want := durableMutationBytes(t, babyMutation, baby, h.Owner)
	count, bytes := h.FamilyUsage(t)
	if count != 0 || bytes != want {
		t.Fatalf("baby usage count=%d bytes=%d want=%d", count, bytes, want)
	}
	feedMutation := testkit.MustMutation(t, "feed-bottle.json")
	feed, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, feedMutation)
	if err != nil {
		t.Fatal(err)
	}
	want += durableMutationBytes(t, feedMutation, feed, h.Caregiver)
	count, bytes = h.FamilyUsage(t)
	if count != 1 || bytes != want {
		t.Fatalf("feed usage count=%d bytes=%d want=%d", count, bytes, want)
	}
	if _, err = h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, feedMutation); err != nil {
		t.Fatal(err)
	}
	replayCount, replayBytes := h.FamilyUsage(t)
	if replayCount != count || replayBytes != bytes {
		t.Fatalf("replay usage=(%d,%d) want=(%d,%d)", replayCount, replayBytes, count, bytes)
	}
}

func TestTypedPostgresRoundTripPreservesExactCanonicalPayloadAndMetadata(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"mutation_id":"01000000-0000-4000-8000-000000000601","entity_id":"20000000-0000-4000-8000-000000000601","entity_kind":"feed","operation":"create","payload_version":4,"observed_entity_version":0,"client_mutation_time":"2026-09-20T12:34:56.1234565Z","import_id":null,"payload":{"baby_id":"10000000-0000-4000-8000-000000000001","kind":"breast","started_at":"2026-09-20T12:00:00.1234575Z","ended_at":"2026-09-20T12:10:00.1234565Z","time_zone_id":"Europe/Zurich","note":"exact","feeding_session_id":null,"components":[{"id":"31000000-0000-4000-8000-000000000601","kind":"breast","side":"left","seconds":1},{"id":"31000000-0000-4000-8000-000000000602","kind":"breast","side":"right","seconds":2},{"id":"31000000-0000-4000-8000-000000000603","kind":"breast","side":"left","seconds":3},{"id":"31000000-0000-4000-8000-000000000604","kind":"bottle","volume_ml":1.2345675,"contents":"mixed"},{"id":"31000000-0000-4000-8000-000000000605","kind":"solids","foods":["banana","banana","yogurt"],"amount":"some"}]}}`)
	m, err := protocol.DecodeMutation(raw)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil {
		t.Fatal(err)
	}
	want := m.Payload.(protocol.FeedPayload)
	repo := records.NewRepository()
	err = h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		got, meta, err := repo.GetFeed(t.Context(), tx, h.FamilyID, m.EntityID)
		if err != nil {
			return err
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if !reflect.DeepEqual(gotJSON, wantJSON) {
			t.Fatalf("payload=%s want=%s", gotJSON, wantJSON)
		}
		if meta != outcome.CanonicalEntity.Meta || meta.CreatorAccountID != h.Owner.AccountID || meta.LastEditorAccountID != h.Owner.AccountID || meta.ClientMutationTime != "2026-09-20T12:34:56.123456Z" {
			t.Fatalf("meta=%+v outcome=%+v", meta, outcome.CanonicalEntity.Meta)
		}
		if got.StartedAt != "2026-09-20T12:00:00.123458Z" || got.EndedAt == nil || *got.EndedAt != "2026-09-20T12:10:00.123456Z" || len(got.Components) != 5 ||
			got.Components[3].VolumeML == nil || got.Components[3].VolumeML.String() != "1.234568" || !reflect.DeepEqual(got.Components[4].Foods, []string{"banana", "banana", "yogurt"}) {
			t.Fatalf("canonical payload=%+v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSharedCorpusPostgresRoundTripsEveryCanonicalKindExactly(t *testing.T) {
	h := testkit.New(t)
	repo := records.NewRepository()
	fixtures := []string{
		"baby.json",
		"feed-bottle.json",
		"feed-breast.json",
		"feed-solids.json",
		"sleep.json",
		"diaper.json",
	}
	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture, func(t *testing.T) {
			mutation := testkit.MustMutation(t, fixture)
			outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, mutation)
			if err != nil || outcome.Status != "applied" || outcome.CanonicalEntity == nil {
				t.Fatalf("outcome=%+v err=%v", outcome, err)
			}
			wantMeta := protocol.EntityMeta{
				ID: mutation.EntityID, FamilyID: h.FamilyID, EntityVersion: 1,
				CreatorAccountID: h.Owner.AccountID, LastEditorAccountID: h.Owner.AccountID,
				ClientMutationTime: mutation.ClientMutationTime,
				AcceptedAt:         "2026-09-21T12:00:00.000000Z",
			}
			var gotPayload protocol.Payload
			var gotMeta protocol.EntityMeta
			err = h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
				switch mutation.EntityKind {
				case "baby":
					gotPayload, gotMeta, err = repo.GetBaby(t.Context(), tx, h.FamilyID, mutation.EntityID)
				case "feed":
					gotPayload, gotMeta, err = repo.GetFeed(t.Context(), tx, h.FamilyID, mutation.EntityID)
				case "sleep":
					gotPayload, gotMeta, err = repo.GetSleep(t.Context(), tx, h.FamilyID, mutation.EntityID)
				case "diaper":
					gotPayload, gotMeta, err = repo.GetDiaper(t.Context(), tx, h.FamilyID, mutation.EntityID)
				default:
					return fmt.Errorf("unsupported fixture entity kind %q", mutation.EntityKind)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotPayload, mutation.Payload) {
				gotJSON, _ := json.Marshal(gotPayload)
				wantJSON, _ := json.Marshal(mutation.Payload)
				t.Fatalf("payload=%s want=%s", gotJSON, wantJSON)
			}
			if gotMeta != wantMeta {
				t.Fatalf("meta=%+v want=%+v", gotMeta, wantMeta)
			}
			wantEntity := protocol.CanonicalEntity{EntityKind: mutation.EntityKind, Meta: wantMeta, Payload: mutation.Payload}
			if !reflect.DeepEqual(*outcome.CanonicalEntity, wantEntity) {
				gotJSON, _ := json.Marshal(outcome.CanonicalEntity)
				wantJSON, _ := json.Marshal(wantEntity)
				t.Fatalf("canonical entity=%s want=%s", gotJSON, wantJSON)
			}
			wantHash, err := protocol.CanonicalContentHash(mutation.EntityKind, mutation.Payload)
			if err != nil {
				t.Fatal(err)
			}
			gotHash, err := protocol.CanonicalContentHash(mutation.EntityKind, gotPayload)
			if err != nil {
				t.Fatal(err)
			}
			if gotHash != wantHash {
				t.Fatalf("content hash=%x want=%x", gotHash, wantHash)
			}
		})
	}
}

func TestReadJournalReturnsFullyScopedCursor(t *testing.T) {
	h := testkit.New(t)
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, "baby.json")); err != nil {
		t.Fatal(err)
	}
	err := h.Security.WithAuthorizedTransaction(t.Context(), h.Owner, h.FamilyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		changes, err := familysync.ReadJournal(t.Context(), tx, h.FamilyID, h.Info.InstanceID, h.Info.RecoveryEpoch, 0, protocol.MaxSafeInteger, 10)
		if err != nil {
			return err
		}
		if len(changes) != 1 || changes[0].Cursor.ServerInstanceID != h.Info.InstanceID || changes[0].Cursor.RecoveryEpoch != h.Info.RecoveryEpoch || changes[0].Cursor.FamilyID != h.FamilyID || changes[0].Cursor.Sequence != 1 || changes[0].Cursor.Validate() != nil {
			t.Fatalf("changes=%+v", changes)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func durableMutationBytes(t *testing.T, m protocol.Mutation, outcome protocol.Outcome, p access.Principal) int64 {
	t.Helper()
	if outcome.CanonicalEntity == nil || outcome.Cursor == nil {
		t.Fatalf("noncanonical outcome %+v", outcome)
	}
	entityBytes, err := records.EntityStorageBytes(*outcome.CanonicalEntity)
	if err != nil {
		t.Fatal(err)
	}
	change := protocol.JournalChange{Cursor: *outcome.Cursor, EntityKind: m.EntityKind, EntityID: m.EntityID, EntityVersion: outcome.CanonicalEntity.Meta.EntityVersion, ActorAccountID: p.AccountID, DeviceID: p.DeviceID, Operation: m.Operation, AcceptedAt: outcome.CanonicalEntity.Meta.AcceptedAt, Optional: false, SnapshotOrTombstone: *outcome.CanonicalEntity}
	journalBytes, err := records.JournalStorageBytes(change)
	if err != nil {
		t.Fatal(err)
	}
	receiptBytes, err := records.ReceiptStorageBytes(outcome)
	if err != nil {
		t.Fatal(err)
	}
	return entityBytes + journalBytes + receiptBytes + records.DeletionReserveStorageBytes()
}

// A solids part whose foods list is empty — build 2961ca1 sends one after an
// edit clears the foods, or for an amount-only legacy feed — is stored with
// `"foods":[]`, so the journal row the family pages through and the receipt a
// resend replays both read back.
func TestSolidsPartWithNoFoodsReadsBackFromJournalAndReceipt(t *testing.T) {
	h := testkit.New(t)
	baby := testkit.MustMutation(t, "baby.json")
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil || out.Status != "applied" {
		t.Fatalf("seed baby %+v %v", out, err)
	}
	const feedID, partID = "5f000000-0000-4000-8000-000000000001", "5f000000-0000-4000-8000-000000000002"
	feed, err := protocol.DecodeMutation([]byte(`{"mutation_id":"5f000000-0000-4000-8000-000000000003","entity_id":"` + feedID + `",` +
		`"entity_kind":"feed","operation":"create","payload_version":4,"observed_entity_version":0,"client_mutation_time":"2026-09-21T12:00:00.000Z",` +
		`"payload":{"baby_id":"` + baby.EntityID + `","kind":"solids","started_at":"2026-09-21T12:00:00.000Z","ended_at":null,` +
		`"time_zone_id":"Europe\/Zurich","note":null,"feeding_session_id":null,"components":[{"id":"` + partID + `","kind":"solids","foods":[],"amount":"taste"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	requireNoFoods := func(label string, entity *protocol.CanonicalEntity) {
		t.Helper()
		if entity == nil {
			t.Fatalf("%s: no entity", label)
		}
		payload, ok := entity.Payload.(protocol.FeedPayload)
		if !ok || len(payload.Components) != 1 || payload.Components[0].Foods == nil || len(payload.Components[0].Foods) != 0 {
			t.Fatalf("%s: payload=%+v", label, entity.Payload)
		}
	}
	created, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, feed)
	if err != nil || created.Status != "applied" {
		t.Fatalf("create %+v %v", created, err)
	}
	replay, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, feed)
	if err != nil || replay.Status != "already_applied" {
		t.Fatalf("receipt replay %+v %v", replay, err)
	}
	requireNoFoods("receipt replay", replay.CanonicalEntity)
	page, err := h.Service.ApplyAndPull(t.Context(), h.Caregiver, h.SyncRequest(nil, nil))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	pulled := false
	for _, change := range page.Changes {
		if change.EntityID == feedID {
			requireNoFoods("pulled change", &change.SnapshotOrTombstone)
			pulled = true
		}
	}
	if !pulled {
		t.Fatalf("feed not pulled: %+v", page.Changes)
	}
}
