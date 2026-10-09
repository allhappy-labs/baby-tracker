//go:build identitytest

package protocol_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

func TestDroppedCommittedTimerResponseRecoversReceiptAcrossContinuityThenStops(t *testing.T) {
	fixture := newProtocolFixture(t)
	owner := fixture.Owner()
	if _, err := owner.Sync([]protocol.Mutation{mutation(t, "baby.json", fixture.h.CommandID())}); err != nil {
		t.Fatal(err)
	}
	originalDevice := owner.principal.DeviceID
	frozen := owner.FrozenTimerStart(testkit.TimerA)
	fixture.DropNextSyncResponseAfterCommit()
	if _, err := owner.SyncExact(frozen); err == nil {
		t.Fatal("expected committed response to be dropped")
	}
	original := fixture.DroppedSyncResponse()
	owner = fixture.RestartClient(owner)
	oldSession := owner.principal.SessionID
	fixture.RequireReauthByRefreshReuse(owner)
	if err := owner.ReauthenticateWithContinuity(); err != nil {
		t.Fatal(err)
	}
	if owner.principal.DeviceID != originalDevice || owner.principal.SessionID == oldSession {
		t.Fatal("client restart and reauthentication lost device continuity")
	}
	replay, err := owner.SyncExact(frozen)
	if err != nil || replay.Outcomes[0].Status != "already_applied" || replay.Outcomes[0].OriginalStatus == nil || *replay.Outcomes[0].OriginalStatus != "applied" {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if replay.Outcomes[0].CanonicalEntity == nil || replay.Outcomes[0].CanonicalEntity.Meta.ID != testkit.TimerA {
		t.Fatal("recovered receipt does not prove the accepted timer identity")
	}
	if original.Outcomes[0].Cursor == nil || replay.Outcomes[0].Cursor == nil || *original.Outcomes[0].Cursor != *replay.Outcomes[0].Cursor ||
		original.Outcomes[0].CanonicalEntity == nil || original.Outcomes[0].CanonicalEntity.Meta.AcceptedAt != replay.Outcomes[0].CanonicalEntity.Meta.AcceptedAt {
		t.Fatalf("receipt cursor/timestamp changed original=%+v replay=%+v", original.Outcomes[0], replay.Outcomes[0])
	}
	stopped := owner.StopTimer(testkit.TimerA, testkit.RecordA)
	started := "2026-09-20T15:00:00.000000Z"
	ended := "2026-09-20T15:10:00.000000Z"
	side, seconds := "left", uint32(600)
	// timer-feed.json has no session, so the completion is a feed with the
	// client-named component id and its step bounds.
	expectedFeed := protocol.FeedPayload{BabyID: "10000000-0000-4000-8000-000000000001", Kind: "breast", StartedAt: started,
		EndedAt: &ended, TimeZoneID: "Europe/Zurich",
		Components: []protocol.FeedComponent{{
			ID: "4" + testkit.RecordA[1:], Kind: "breast", Side: &side, Seconds: &seconds, StartedAt: &started, EndedAt: &ended,
		}},
	}
	if stopped.Status != "applied" || len(stopped.RelatedEntities) != 1 || stopped.RelatedEntities[0].EntityKind != "feed" ||
		stopped.RelatedEntities[0].Meta.ID != testkit.RecordA || !reflect.DeepEqual(stopped.RelatedEntities[0].Payload, expectedFeed) ||
		fixture.CountCompletedRecords(testkit.RecordA) != 1 {
		t.Fatalf("stop=%+v", stopped)
	}
	caregiver, err := fixture.Caregiver().Sync(nil)
	if err != nil {
		t.Fatal(err)
	}
	stopChanges, feedChanges := 0, 0
	for _, change := range caregiver.Changes {
		if change.EntityID == testkit.TimerA && change.Operation == "timer_stop" {
			stopChanges++
		}
		if change.EntityID == testkit.RecordA && change.EntityKind == "feed" && change.Operation == "timer_stop" {
			feedChanges++
			if !reflect.DeepEqual(change.SnapshotOrTombstone, stopped.RelatedEntities[0]) {
				t.Fatalf("caregiver completed feed=%+v outcome=%+v", change.SnapshotOrTombstone, stopped.RelatedEntities[0])
			}
		}
	}
	if stopChanges != 1 || feedChanges != 1 {
		t.Fatalf("caregiver stop materializations=%d completed feeds=%d changes=%+v", stopChanges, feedChanges, caregiver.Changes)
	}
}

func TestAbsoluteAgeReauthRecoversCommittedTimerReceiptThenStops(t *testing.T) {
	fixture := newProtocolFixture(t)
	owner := fixture.Owner()
	if _, err := owner.Sync([]protocol.Mutation{mutation(t, "baby.json", fixture.h.CommandID())}); err != nil {
		t.Fatal(err)
	}
	originalDevice := owner.principal.DeviceID
	frozen := owner.FrozenTimerStart(testkit.TimerA)
	fixture.DropNextSyncResponseAfterCommit()
	if _, err := owner.SyncExact(frozen); err == nil {
		t.Fatal("expected committed response to be dropped")
	}
	owner = fixture.RestartClient(owner)
	fixture.h.Now = fixture.h.Now.Add(365*24*time.Hour + time.Hour)
	if err := owner.refreshHTTP(owner.refreshToken); err == nil || !strings.Contains(err.Error(), "reauthentication_required") {
		t.Fatalf("refresh err=%v", err)
	}
	if err := owner.ReauthenticateWithContinuity(); err != nil || owner.principal.DeviceID != originalDevice {
		t.Fatalf("device=%q err=%v", owner.principal.DeviceID, err)
	}
	replay, err := owner.SyncExact(frozen)
	if err != nil || len(replay.Outcomes) != 1 || replay.Outcomes[0].OriginalStatus == nil || *replay.Outcomes[0].OriginalStatus != "applied" {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if stopped := owner.StopTimer(testkit.TimerA, testkit.RecordA); stopped.Status != "applied" || fixture.CountCompletedRecords(testkit.RecordA) != 1 {
		t.Fatalf("stop=%+v", stopped)
	}
}

func TestSupersededTimerStartBlocksCommandsDependingOnLosingIdentity(t *testing.T) {
	fixture := newProtocolFixture(t)
	if _, err := fixture.Owner().Sync([]protocol.Mutation{mutation(t, "baby.json", fixture.h.CommandID())}); err != nil {
		t.Fatal(err)
	}
	start := testkit.MustMutation(t, "timer-feed.json")
	start.MutationID, start.EntityID = fixture.h.CommandID(), testkit.TimerA
	first, err := fixture.Owner().Sync([]protocol.Mutation{start})
	if err != nil || first.Outcomes[0].Status != "applied" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	loser := start
	loser.MutationID, loser.EntityID = fixture.h.CommandID(), testkit.TimerB
	superseded, err := fixture.Caregiver().Sync([]protocol.Mutation{loser})
	if err != nil || superseded.Outcomes[0].Status != "superseded" || superseded.Outcomes[0].CanonicalEntity == nil || superseded.Outcomes[0].CanonicalEntity.Meta.ID != testkit.TimerA {
		t.Fatalf("superseded=%+v err=%v", superseded, err)
	}
	if err = fixture.Caregiver().ReauthenticateWithContinuity(); err != nil {
		t.Fatal(err)
	}
	replay, err := fixture.Caregiver().Sync([]protocol.Mutation{loser})
	if err != nil || replay.Outcomes[0].Status != "already_applied" || replay.Outcomes[0].OriginalStatus == nil || *replay.Outcomes[0].OriginalStatus != "superseded" {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	payload := loser.Payload.(protocol.TimerStartPayload)
	dependent := loser
	dependent.MutationID, dependent.Operation = fixture.h.CommandID(), "timer_stop"
	dependent.Payload = protocol.TimerStopPayload{BabyID: payload.BabyID, CompletedRecordID: testkit.RecordA, StoppedAt: "2026-09-20T15:05:00.000000Z"}
	blocked, err := fixture.Caregiver().Sync([]protocol.Mutation{dependent})
	if err != nil || blocked.Outcomes[0].Status != "rejected" || blocked.Outcomes[0].Code == nil || *blocked.Outcomes[0].Code != "entity_not_found" {
		t.Fatalf("blocked=%+v err=%v", blocked, err)
	}
}

// An edit whose response is lost is resent byte for byte; the receipt answers
// it, nothing is applied twice, and the stop that follows builds the feed from
// the edited timer.
func TestDroppedTimerEditResponseRecoversReceiptThenStopSeesTheEdit(t *testing.T) {
	fixture := newProtocolFixture(t)
	owner := fixture.Owner()
	if _, err := owner.Sync([]protocol.Mutation{mutation(t, "baby.json", fixture.h.CommandID())}); err != nil {
		t.Fatal(err)
	}
	at := func(offset time.Duration) string {
		return fixture.h.Now.Add(offset).UTC().Format("2006-01-02T15:04:05.000000Z")
	}
	babyID := "10000000-0000-4000-8000-000000000001"
	leftPart, rightPart := "37000000-0000-4000-8000-000000000001", "37000000-0000-4000-8000-000000000002"
	command := func(operation, clientTime string, payload protocol.Payload) protocol.Mutation {
		return protocol.Mutation{MutationID: fixture.h.CommandID(), EntityID: testkit.TimerA, EntityKind: "timer",
			Operation: operation, PayloadVersion: 5, ClientMutationTime: clientTime, Payload: payload}
	}
	left, right, started, sideStart := "left", "right", at(-time.Hour), at(-50*time.Minute)
	leftSeconds := uint32(600)
	start := command("timer_start", started, protocol.TimerStartPayload{BabyID: babyID, TimerKind: "feed", StartedAt: started,
		TimeZoneID: "Europe/Zurich", CurrentSide: &right, CurrentSideStartedAt: &sideStart,
		CompletedComponents: []protocol.FeedComponent{{ID: leftPart, Kind: "breast", Side: &left, Seconds: &leftSeconds}}})
	if response, err := owner.Sync([]protocol.Mutation{start}); err != nil || response.Outcomes[0].Status != "applied" {
		t.Fatalf("%s=%+v err=%v", start.Operation, response, err)
	}
	editedStart, editedSeconds := at(-65*time.Minute), uint32(540)
	edit := command("update", at(-45*time.Minute), protocol.TimerEditPayload{BabyID: babyID, StartedAt: &editedStart,
		Components: []protocol.FeedComponent{{ID: leftPart, Kind: "breast", Side: &left, Seconds: &editedSeconds}}})
	raw, err := owner.syncRequestBytes([]protocol.Mutation{edit})
	if err != nil {
		t.Fatal(err)
	}
	fixture.DropNextSyncResponseAfterCommit()
	if _, err := owner.SyncExact(raw); err == nil {
		t.Fatal("expected committed edit response to be dropped")
	}
	original := fixture.DroppedSyncResponse().Outcomes[0]
	replay, err := owner.SyncExact(raw)
	if err != nil || len(replay.Outcomes) != 1 || replay.Outcomes[0].Status != "already_applied" ||
		replay.Outcomes[0].OriginalStatus == nil || *replay.Outcomes[0].OriginalStatus != "applied" {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if original.Cursor == nil || replay.Outcomes[0].Cursor == nil || *original.Cursor != *replay.Outcomes[0].Cursor ||
		!reflect.DeepEqual(original.CanonicalEntity, replay.Outcomes[0].CanonicalEntity) {
		t.Fatalf("receipt changed original=%+v replay=%+v", original, replay.Outcomes[0])
	}
	if timer := replay.Outcomes[0].CanonicalEntity.Payload.(protocol.TimerStartPayload); timer.StartedAt != editedStart ||
		*timer.CompletedComponents[0].Seconds != editedSeconds || *timer.CurrentSideStartedAt != at(-50*time.Minute) {
		t.Fatalf("edited timer=%+v", timer)
	}
	stop := command("timer_stop", at(-40*time.Minute), protocol.TimerStopPayload{BabyID: babyID, CompletedRecordID: testkit.RecordA,
		StoppedAt: at(-40 * time.Minute), ComponentID: rightPart})
	stopped, err := owner.Sync([]protocol.Mutation{stop})
	if err != nil || stopped.Outcomes[0].Status != "applied" || len(stopped.Outcomes[0].RelatedEntities) != 1 {
		t.Fatalf("stop=%+v err=%v", stopped, err)
	}
	feed := stopped.Outcomes[0].RelatedEntities[0].Payload.(protocol.FeedPayload)
	if feed.StartedAt != editedStart || len(feed.Components) != 2 || feed.Components[0].ID != leftPart ||
		*feed.Components[0].Seconds != editedSeconds || feed.Components[1].ID != rightPart || *feed.Components[1].Seconds != 600 {
		t.Fatalf("completed feed=%+v", feed)
	}
	pulled, err := fixture.Caregiver().Sync(nil)
	if err != nil {
		t.Fatal(err)
	}
	edits := 0
	for _, change := range pulled.Changes {
		if change.EntityID == testkit.TimerA && change.Operation == "update" {
			edits++
			if change.SnapshotOrTombstone.Payload.(protocol.TimerStartPayload).StartedAt != editedStart {
				t.Fatalf("caregiver pulled edit=%+v", change.SnapshotOrTombstone)
			}
		}
	}
	if edits != 1 || fixture.CountCompletedRecords(testkit.RecordA) != 1 {
		t.Fatalf("caregiver edits=%d completed=%d", edits, fixture.CountCompletedRecords(testkit.RecordA))
	}
}
