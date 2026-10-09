package records_test

import (
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

func TestStandaloneTimedBottleCannotStopBeforeFinish(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	at := "2026-09-28T13:00:00.000000Z"
	m := protocol.Mutation{MutationID: mutationID(960), EntityID: timerA, EntityKind: "timer", Operation: "timer_start", PayloadVersion: 5, ClientMutationTime: at,
		Payload: protocol.TimerStartPayload{BabyID: babyID, TimerKind: "feed", StartedAt: at, TimeZoneID: "UTC", CurrentBottleStartedAt: &at, CompletedComponents: []protocol.FeedComponent{}}}
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil || outcome.Status != "applied" {
		t.Fatalf("start outcome=%+v err=%v", outcome, err)
	}
	m.MutationID, m.Operation, m.ClientMutationTime = mutationID(961), "timer_stop", "2026-09-28T13:03:00.000000Z"
	m.Payload = protocol.TimerStopPayload{BabyID: babyID, CompletedRecordID: recordB, StoppedAt: m.ClientMutationTime}
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil || outcome.Code == nil || *outcome.Code != "active_bottle_unfinished" {
		t.Fatalf("stop outcome=%+v err=%v", outcome, err)
	}
	formula := "formula"
	finishAt := "2026-09-28T13:04:00.000000Z"
	m.MutationID, m.Operation, m.ClientMutationTime = mutationID(962), "timer_finish_bottle", finishAt
	m.Payload = protocol.TimerBottleFinishPayload{BabyID: babyID, FinishedAt: finishAt, ComponentID: "30000000-0000-4000-8000-000000000960", VolumeML: protocol.MustParseDecimal("80"), Contents: &formula}
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil || outcome.Status != "applied" {
		t.Fatalf("finish outcome=%+v err=%v", outcome, err)
	}
	m.MutationID, m.Operation, m.ClientMutationTime = mutationID(963), "timer_stop", finishAt
	m.Payload = protocol.TimerStopPayload{BabyID: babyID, CompletedRecordID: recordB, StoppedAt: finishAt}
	stop, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || stop.Status != "applied" || len(stop.RelatedEntities) != 1 {
		t.Fatalf("final stop=%+v err=%v", stop, err)
	}
	feed := stop.RelatedEntities[0].Payload.(protocol.FeedPayload)
	if feed.Kind != "bottle" || len(feed.Components) != 1 || feed.Components[0].Seconds == nil || *feed.Components[0].Seconds != 240 {
		t.Fatalf("standalone feed=%+v", feed)
	}
}

func TestBreastTimerCompletionRetainsStepBounds(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	left, startAt, stopAt := "left", "2026-09-28T14:00:00.000000Z", "2026-09-28T14:04:00.000000Z"
	m := protocol.Mutation{MutationID: mutationID(972), EntityID: timerA, EntityKind: "timer", Operation: "timer_start", PayloadVersion: 5, ClientMutationTime: startAt,
		Payload: protocol.TimerStartPayload{BabyID: babyID, TimerKind: "feed", StartedAt: startAt, TimeZoneID: "UTC", CurrentSide: &left, CurrentSideStartedAt: &startAt, CompletedComponents: []protocol.FeedComponent{}}}
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil || outcome.Status != "applied" {
		t.Fatalf("start=%+v err=%v", outcome, err)
	}
	m.MutationID, m.Operation, m.ClientMutationTime = mutationID(973), "timer_stop", stopAt
	m.Payload = protocol.TimerStopPayload{BabyID: babyID, CompletedRecordID: recordB, StoppedAt: stopAt, ComponentID: "30000000-0000-4000-8000-000000000972"}
	result, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || result.Status != "applied" || len(result.RelatedEntities) != 1 {
		t.Fatalf("stop=%+v err=%v", result, err)
	}
	feed := h.ReadFeed(t, recordB).Payload.(protocol.FeedPayload)
	if len(feed.Components) != 1 || feed.Components[0].StartedAt == nil || *feed.Components[0].StartedAt != startAt ||
		feed.Components[0].EndedAt == nil || *feed.Components[0].EndedAt != stopAt {
		t.Fatalf("completion=%+v", feed)
	}
}

func TestTimedBottleStartEditCannotConflictWithMeasuredDuration(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	startAt, finishAt, formula := "2026-09-21T11:00:00.000000Z", "2026-09-21T11:05:00.000000Z", "formula"
	m := protocol.Mutation{MutationID: mutationID(975), EntityID: timerA, EntityKind: "timer", Operation: "timer_start", PayloadVersion: 5, ClientMutationTime: startAt,
		Payload: protocol.TimerStartPayload{BabyID: babyID, TimerKind: "feed", StartedAt: startAt, TimeZoneID: "UTC", CurrentBottleStartedAt: &startAt, CompletedComponents: []protocol.FeedComponent{}}}
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil || outcome.Status != "applied" {
		t.Fatalf("start=%+v %v", outcome, err)
	}
	bottleID := "30000000-0000-4000-8000-000000000975"
	m.MutationID, m.Operation, m.ClientMutationTime = mutationID(976), "timer_finish_bottle", finishAt
	m.Payload = protocol.TimerBottleFinishPayload{BabyID: babyID, FinishedAt: finishAt, ComponentID: bottleID, VolumeML: protocol.MustParseDecimal("60"), Contents: &formula}
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil || outcome.Status != "applied" {
		t.Fatalf("finish=%+v %v", outcome, err)
	}
	tooLate := "2026-09-21T11:58:00.000000Z"
	m.MutationID, m.Operation, m.ClientMutationTime = mutationID(977), "update", tooLate
	m.Payload = protocol.TimerEditPayload{BabyID: babyID, StartedAt: &tooLate, Components: []protocol.FeedComponent{}}
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil || outcome.Code == nil || *outcome.Code != "invalid_timer_time" {
		t.Fatalf("invalid start edit=%+v %v", outcome, err)
	}
	longer := uint32(301)
	m.MutationID = mutationID(978)
	m.Payload = protocol.TimerEditPayload{BabyID: babyID, Components: []protocol.FeedComponent{{ID: bottleID, Kind: "bottle", Seconds: &longer, VolumeML: protocol.MustParseDecimal("60"), Contents: &formula}}}
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m); err != nil || outcome.Code == nil || *outcome.Code != "invalid_timer_time" {
		t.Fatalf("duration edit=%+v %v", outcome, err)
	}
}
