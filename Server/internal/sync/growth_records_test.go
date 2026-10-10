package sync_test

import (
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"testing"
)

func TestGrowthAndMeasurementStorage(t *testing.T) {
	h := testkit.New(t)
	baby := testkit.MustMutation(t, "baby.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil {
		t.Fatal(err)
	}
	profile := testkit.MustMutation(t, "growth-profile.json")
	m := testkit.MustMutation(t, "measurement.json")
	for _, record := range []protocol.Mutation{profile, m} {
		out, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, record)
		if err != nil || out.Status != "applied" {
			t.Fatalf("create %s: %+v %v", record.EntityKind, out, err)
		}
	}
	profile.MutationID = h.CommandID()
	duplicate, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, profile)
	if err != nil || duplicate.Status != "rejected" {
		t.Fatalf("two clients duplicate %+v %v", duplicate, err)
	}
	if h.CareCount(t, "growth_profile", h.FamilyID) != 1 || h.CareCount(t, "measurement", h.FamilyID) != 1 {
		t.Fatal("unexpected raw growth row count")
	}
	profile.Operation = "update"
	profile.ObservedEntityVersion = 1
	profile.MutationID = h.CommandID()
	growth := profile.Payload.(protocol.GrowthProfilePayload)
	due := "2026-01-20"
	growth.ExpectedDueDate = &due
	profile.Payload = growth
	updated, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, profile)
	if err != nil || updated.Status != "applied" {
		t.Fatalf("due date set %+v %v", updated, err)
	}
	growth.ExpectedDueDate = nil
	profile.Payload = growth
	profile.MutationID = h.CommandID()
	profile.ObservedEntityVersion = 2
	updated, err = h.Service.Apply(t.Context(), h.Owner, h.FamilyID, profile)
	if err != nil || updated.Status != "applied" || updated.CanonicalEntity.Payload.(protocol.GrowthProfilePayload).ExpectedDueDate != nil {
		t.Fatalf("due date clearing %+v %v", updated, err)
	}
	p := m.Payload.(protocol.MeasurementPayload)
	p.LengthCM = nil
	p.LengthPosition = nil
	p.WeightKG = nil
	p.HeadCircumferenceCM = protocol.MustParseDecimal("42")
	m.Payload = p
	m.Operation = "update"
	m.MutationID = h.CommandID()
	m.ObservedEntityVersion = 1
	out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || out.Status != "applied" {
		t.Fatalf("update %+v %v", out, err)
	}
	got := out.CanonicalEntity.Payload.(protocol.MeasurementPayload)
	if got.LengthCM != nil || got.WeightKG != nil || got.LengthPosition != nil {
		t.Fatal("null clearing failed")
	}
	before := h.FamilySequence(t)
	p.LoggedAt = "2026-09-21T00:00:00.000000Z"
	m.Payload = p
	m.MutationID = h.CommandID()
	out, err = h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || out.Code == nil || *out.Code != "record_logged_at_mismatch" || h.FamilySequence(t) != before {
		t.Fatalf("immutable logged_at %+v %v", out, err)
	}
	// Create the referenced baby in a real second family, not a missing UUID.
	other, err := h.Family.Create(t.Context(), h.Outsider, h.CommandID(), "Other family")
	if err != nil {
		t.Fatal(err)
	}
	foreignBaby := testkit.MustMutation(t, "baby.json")
	foreignBaby.EntityID = "10000000-0000-4000-8000-000000000009"
	foreignBaby.MutationID = h.CommandID()
	if created, err := h.Service.Apply(t.Context(), h.Refresh("outsider"), other.ID, foreignBaby); err != nil || created.Status != "applied" {
		t.Fatalf("foreign baby setup %+v %v", created, err)
	}
	p.LoggedAt = got.LoggedAt
	p.BabyID = "10000000-0000-4000-8000-000000000009"
	m.Payload = p
	m.Operation = "create"
	m.EntityID = h.CommandID()
	m.MutationID = h.CommandID()
	out, err = h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || out.Code == nil || *out.Code != "baby_not_found" || h.FamilySequence(t) != before {
		t.Fatalf("wrong family parent %+v %v", out, err)
	}
	h.RebuildSyncService(t, h.Limits)
	snapshot, err := fullSnapshot(t, h.Service, h.Owner, protocol.SnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, e := range snapshot.Entities {
		found[e.EntityKind] = true
		if e.EntityKind == "measurement" && e.Payload.(protocol.MeasurementPayload).HeadCircumferenceCM.String() != "42" {
			t.Fatal("snapshot mismatch")
		}
	}
	if !found["growth_profile"] || !found["measurement"] {
		t.Fatalf("missing snapshot growth %+v", found)
	}
}
