package sync_test

import (
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// A shot is created, updated and deleted; each is applied, the canonical
// entity carries its fields, and a pull serves the tombstone.
func TestImmunizationCreateUpdateDelete(t *testing.T) {
	h := testkit.New(t)
	baby := testkit.MustMutation(t, "baby.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil {
		t.Fatal(err)
	}
	m := testkit.MustMutation(t, "immunization.json")
	created, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m)
	if err != nil || created.Status != "applied" {
		t.Fatalf("create %+v %v", created, err)
	}
	got := created.CanonicalEntity.Payload.(protocol.ImmunizationPayload)
	if got.Vaccine != "Prevenar 13" || got.Dose == nil || *got.Dose != 2 || got.Note != nil ||
		got.GivenAt != "2026-09-20T09:30:00.000000Z" || got.TimeZoneID != "Europe/Zurich" || got.BabyID != baby.EntityID {
		t.Fatalf("created %+v", got)
	}
	if count, _ := h.FamilyUsage(t); count != 1 {
		t.Fatalf("quota count after create %d", count)
	}

	p := m.Payload.(protocol.ImmunizationPayload)
	three, note := 3, "left thigh"
	p.Dose, p.Note, p.GivenAt, p.Vaccine = &three, &note, "2026-09-19T08:00:00.000000Z", "Prevenar 20"
	m.Payload, m.Operation, m.MutationID, m.ObservedEntityVersion = p, "update", h.CommandID(), 1
	edited, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || edited.Status != "applied" {
		t.Fatalf("update %+v %v", edited, err)
	}
	got = edited.CanonicalEntity.Payload.(protocol.ImmunizationPayload)
	if got.Vaccine != "Prevenar 20" || *got.Dose != 3 || got.Note == nil || *got.Note != "left thigh" || got.GivenAt != p.GivenAt {
		t.Fatalf("edited %+v", got)
	}
	p.Dose = nil
	m.Payload, m.MutationID, m.ObservedEntityVersion = p, h.CommandID(), 2
	cleared, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || cleared.Status != "applied" || cleared.CanonicalEntity.Payload.(protocol.ImmunizationPayload).Dose != nil {
		t.Fatalf("clearing the dose %+v %v", cleared, err)
	}

	before := h.FamilySequence(t)
	babyID := baby.EntityID
	m.MutationID, m.Operation, m.Payload, m.ObservedEntityVersion = h.CommandID(), "delete", protocol.DeletePayload{BabyID: &babyID}, 3
	deleted, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m)
	if err != nil || deleted.Status != "applied" {
		t.Fatalf("delete %+v %v", deleted, err)
	}
	pull, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(h.Cursor(before), nil))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range pull.Changes {
		if change.EntityKind == "immunization" {
			found = true
			if change.Optional || change.SnapshotOrTombstone.Payload != nil || change.Operation != "delete" {
				t.Fatalf("delete change %+v", change)
			}
		}
	}
	if !found {
		t.Fatal("immunization tombstone missing")
	}
	if count, _ := h.FamilyUsage(t); count != 0 {
		t.Fatalf("quota count after delete %d", count)
	}
}

// Deleting the baby tombstones its shot in the same cascade, and a later
// update of the shot is refused. A shot for a baby the family does not have
// is refused and nothing is written.
func TestImmunizationFollowsItsBaby(t *testing.T) {
	h := testkit.New(t)
	baby := testkit.MustMutation(t, "baby.json")
	m := testkit.MustMutation(t, "immunization.json")

	before := h.FamilySequence(t)
	orphan, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || orphan.Status != "rejected" || orphan.Code == nil || *orphan.Code != "baby_not_found" {
		t.Fatalf("a shot with no such baby %+v %v", orphan, err)
	}
	if h.FamilySequence(t) != before {
		t.Fatal("a refused shot advanced the family sequence")
	}

	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil {
		t.Fatal(err)
	}
	m.MutationID = h.CommandID()
	if out, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m); err != nil || out.Status != "applied" {
		t.Fatalf("create %+v %v", out, err)
	}
	before = h.FamilySequence(t)
	gone := baby
	gone.MutationID, gone.Operation, gone.Payload, gone.ObservedEntityVersion = h.CommandID(), "delete", protocol.DeletePayload{}, 1
	if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, gone); err != nil || out.Status != "applied" {
		t.Fatalf("baby delete %+v %v", out, err)
	}
	pull, err := applyAndPull(t, h.Service, h.Owner, h.SyncRequest(h.Cursor(before), nil))
	if err != nil {
		t.Fatal(err)
	}
	tombstoned := false
	for _, change := range pull.Changes {
		if change.EntityKind == "immunization" && change.Operation == "delete" && change.SnapshotOrTombstone.Payload == nil {
			tombstoned = true
		}
	}
	if !tombstoned {
		t.Fatal("the baby's shot was not tombstoned with it")
	}
	if count, _ := h.FamilyUsage(t); count != 0 {
		t.Fatalf("quota count %d", count)
	}
	m.MutationID, m.Operation, m.ObservedEntityVersion = h.CommandID(), "update", 2
	late, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m)
	if err != nil || late.Status != "rejected" || late.Code == nil || (*late.Code != "baby_deleted" && *late.Code != "entity_deleted") {
		t.Fatalf("an update after the baby is gone %+v %v", late, err)
	}
}

// A fresh snapshot serves the live shot, after every measurement (kind order).
func TestImmunizationSurvivesSnapshot(t *testing.T) {
	h := testkit.New(t)
	for _, fixture := range []string{"baby.json", "immunization.json", "measurement.json", "supplement.json"} {
		if out, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, testkit.MustMutation(t, fixture)); err != nil || out.Status != "applied" {
			t.Fatalf("%s %+v %v", fixture, out, err)
		}
	}
	h.RebuildSyncService(t, h.Limits)
	snapshot, err := fullSnapshot(t, h.Service, h.Caregiver, protocol.SnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range snapshot.Entities {
		kinds = append(kinds, e.EntityKind)
		if e.EntityKind == "immunization" {
			got := e.Payload.(protocol.ImmunizationPayload)
			if got.Vaccine != "Prevenar 13" || got.Dose == nil || *got.Dose != 2 {
				t.Fatalf("snapshot %+v", got)
			}
		}
	}
	if len(kinds) != 4 || kinds[len(kinds)-1] != "immunization" || kinds[len(kinds)-2] != "measurement" {
		t.Fatalf("snapshot order %v", kinds)
	}
}
