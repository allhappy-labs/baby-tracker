package records_test

import (
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

func TestFeedComponentsPersistInOrderAndPullBack(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	left, right := "left", "right"
	leftSeconds, rightSeconds := uint32(480), uint32(360)
	breastMilk, formula := "breast_milk", "formula"
	mutation := protocol.Mutation{
		MutationID: mutationID(801), EntityID: recordA, EntityKind: "feed", Operation: "create",
		PayloadVersion: 4, ClientMutationTime: "2026-09-24T10:20:00.000000Z",
		Payload: protocol.FeedPayload{
			BabyID: babyID, Kind: "breast", StartedAt: "2026-09-24T10:00:00.000000Z",
			EndedAt: stringPointer("2026-09-24T10:20:00.000000Z"), TimeZoneID: "Europe/Zurich",
			Components: []protocol.FeedComponent{
				{ID: "30000000-0000-4000-8000-000000000001", Kind: "breast", Side: &left, Seconds: &leftSeconds},
				{ID: "30000000-0000-4000-8000-000000000002", Kind: "breast", Side: &right, Seconds: &rightSeconds},
				{ID: "30000000-0000-4000-8000-000000000003", Kind: "bottle", VolumeML: protocol.MustParseDecimal("50"), Contents: &breastMilk},
				{ID: "30000000-0000-4000-8000-000000000004", Kind: "bottle", VolumeML: protocol.MustParseDecimal("30"), Contents: &formula},
			},
		},
	}
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, mutation)
	if err != nil || outcome.Status != "applied" || outcome.CanonicalEntity == nil {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	feed := outcome.CanonicalEntity.Payload.(protocol.FeedPayload)
	if len(feed.Components) != 4 || feed.Components[2].Contents == nil || *feed.Components[2].Contents != "breast_milk" || feed.Components[3].Contents == nil || *feed.Components[3].Contents != "formula" {
		t.Fatalf("canonical components=%+v", feed.Components)
	}
	if count := h.Count(t, "feed_components", h.FamilyID); count != 4 {
		t.Fatalf("component rows=%d", count)
	}
	pull := h.Pull(t, h.Caregiver, nil, nil)
	var pulled []protocol.FeedComponent
	for _, change := range pull.Changes {
		if change.EntityID == recordA {
			pulled = change.SnapshotOrTombstone.Payload.(protocol.FeedPayload).Components
		}
	}
	if len(pulled) != 4 || pulled[0].ID != "30000000-0000-4000-8000-000000000001" || pulled[3].ID != "30000000-0000-4000-8000-000000000004" {
		t.Fatalf("pulled components=%+v", pulled)
	}
}

// An older client's feed update is refused before it reaches the stored feed,
// so it can never drop the v4 fields.
func TestFeedV4ReadbackAndOlderUpdateRefusal(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	start, end, side := "2026-09-24T10:00:00.000000Z", "2026-09-24T10:02:00.000000Z", "left"
	seconds := uint32(120)
	link := "40000000-0000-4000-8000-000000000801"
	create := protocol.Mutation{
		MutationID: mutationID(901), EntityID: recordA, EntityKind: "feed", Operation: "create",
		PayloadVersion: 4, ClientMutationTime: end,
		Payload: protocol.FeedPayload{BabyID: babyID, Kind: "breast", StartedAt: start, EndedAt: &end,
			TimeZoneID: "UTC", FeedingSessionID: &link,
			Components: []protocol.FeedComponent{{ID: "30000000-0000-4000-8000-000000000901", Kind: "breast", Side: &side,
				Seconds: &seconds, StartedAt: &start, EndedAt: &end}}},
	}
	created, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, create)
	if err != nil || created.Status != "applied" {
		t.Fatalf("create=%+v err=%v", created, err)
	}
	stored := h.ReadFeed(t, recordA).Payload.(protocol.FeedPayload)
	if stored.FeedingSessionID == nil || *stored.FeedingSessionID != link ||
		len(stored.Components) != 1 || stored.Components[0].StartedAt == nil || *stored.Components[0].StartedAt != start ||
		stored.Components[0].EndedAt == nil || *stored.Components[0].EndedAt != end {
		t.Fatalf("v4 readback=%+v", stored)
	}
	legacy := create
	legacy.MutationID, legacy.Operation, legacy.PayloadVersion, legacy.ObservedEntityVersion = mutationID(902), "update", 3, 1
	formula := "formula"
	legacy.Payload = protocol.FeedPayload{BabyID: babyID, Kind: "breast", StartedAt: start, EndedAt: &end,
		TimeZoneID: "UTC",
		Components: []protocol.FeedComponent{
			{ID: "30000000-0000-4000-8000-000000000901", Kind: "breast", Side: &side, Seconds: &seconds},
			{ID: "30000000-0000-4000-8000-000000000902", Kind: "bottle", Seconds: &seconds, VolumeML: protocol.MustParseDecimal("30"), Contents: &formula},
		}}
	rejected, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, legacy)
	if err != nil || rejected.Status != "rejected" || rejected.Code == nil || *rejected.Code != "invalid_mutation" {
		code := "<nil>"
		if rejected.Code != nil {
			code = *rejected.Code
		}
		t.Fatalf("legacy update=%+v code=%s err=%v", rejected, code, err)
	}
	stored = h.ReadFeed(t, recordA).Payload.(protocol.FeedPayload)
	if stored.FeedingSessionID == nil || *stored.FeedingSessionID != link || stored.Components[0].StartedAt == nil {
		t.Fatalf("v4 fields changed after legacy update: %+v", stored)
	}
}

func TestFeedV4RejectsSessionLinkBelongingToAnotherBaby(t *testing.T) {
	h := testkit.New(t)
	firstBaby := seedBaby(t, h)
	second := testkit.MustMutation(t, "baby.json")
	second.MutationID, second.EntityID = mutationID(920), "10000000-0000-4000-8000-000000000920"
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, second); err != nil || outcome.Status != "applied" {
		t.Fatalf("second baby=%+v err=%v", outcome, err)
	}
	start, end, side, seconds := "2026-09-24T10:00:00.000000Z", "2026-09-24T10:02:00.000000Z", "left", uint32(120)
	feed := protocol.Mutation{MutationID: mutationID(921), EntityID: recordA, EntityKind: "feed", Operation: "create", PayloadVersion: 4, ClientMutationTime: end,
		Payload: protocol.FeedPayload{BabyID: firstBaby, Kind: "breast", StartedAt: start, EndedAt: &end, TimeZoneID: "UTC",
			Components: []protocol.FeedComponent{{ID: "30000000-0000-4000-8000-000000000921", Kind: "breast", Side: &side, Seconds: &seconds, StartedAt: &start, EndedAt: &end}}}}
	if outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, feed); err != nil || outcome.Status != "applied" {
		t.Fatalf("first feed=%+v err=%v", outcome, err)
	}
	payload := feed.Payload.(protocol.FeedPayload)
	firstFeedID := feed.EntityID
	payload.BabyID, payload.FeedingSessionID = second.EntityID, &firstFeedID
	feed.MutationID, feed.EntityID, feed.Payload = mutationID(922), recordB, payload
	rejected, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, feed)
	if err != nil || rejected.Status != "rejected" || rejected.Code == nil || *rejected.Code != "record_parent_mismatch" {
		code := "<nil>"
		if rejected.Code != nil {
			code = *rejected.Code
		}
		t.Fatalf("cross-baby link=%+v code=%s err=%v", rejected, code, err)
	}
}

func TestFoldedTimerStartAllowsCompletedComponentsBetweenBreasts(t *testing.T) {
	h := testkit.New(t)
	babyID := seedBaby(t, h)
	milk := "breast_milk"
	start := protocol.Mutation{MutationID: mutationID(841), EntityID: timerA, EntityKind: "timer", Operation: "timer_start",
		PayloadVersion: 5, ClientMutationTime: "2026-09-24T10:05:00.000000Z",
		Payload: protocol.TimerStartPayload{BabyID: babyID, TimerKind: "feed", StartedAt: "2026-09-24T10:00:00.000000Z",
			TimeZoneID: "Europe/Zurich", CompletedComponents: []protocol.FeedComponent{
				{ID: "34000000-0000-4000-8000-000000000001", Kind: "bottle", VolumeML: protocol.MustParseDecimal("50"), Contents: &milk},
			}}}
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, start)
	if err != nil || outcome.Status != "applied" {
		t.Fatalf("folded start=%+v err=%v", outcome, err)
	}
	payload := outcome.CanonicalEntity.Payload.(protocol.TimerStartPayload)
	if payload.CurrentSide != nil || len(payload.CompletedComponents) != 1 {
		t.Fatalf("canonical folded timer=%+v", payload)
	}
}

func stringPointer(value string) *string { return &value }
