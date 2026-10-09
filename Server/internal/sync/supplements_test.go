package sync_test

import (
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"testing"
)

func TestSupplementDeleteAndChildCascade(t *testing.T) {
	for _, child := range []bool{false, true} {
		h := testkit.New(t)
		baby := testkit.MustMutation(t, "baby.json")
		if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil {
			t.Fatal(err)
		}
		m := testkit.MustMutation(t, "supplement.json")
		out, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m)
		if err != nil || out.Status != "applied" {
			t.Fatalf("create %+v %v", out, err)
		}
		before := h.FamilySequence(t)
		if child {
			m = baby
		}
		babyID := baby.EntityID
		m.MutationID = h.CommandID()
		m.Operation = "delete"
		m.Payload = protocol.DeletePayload{BabyID: &babyID}
		if child {
			m.Payload = protocol.DeletePayload{}
		}
		out, err = h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
		if err != nil || out.Status != "applied" {
			t.Fatalf("delete %+v %v", out, err)
		}
		pull, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(h.Cursor(before), nil))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, change := range pull.Changes {
			if change.EntityKind == "supplement" {
				found = true
				if change.Optional || change.SnapshotOrTombstone.Payload != nil || change.Operation != "delete" {
					t.Fatalf("delete change %+v", change)
				}
			}
		}
		if !found {
			t.Fatal("supplement tombstone missing")
		}
		if count, _ := h.FamilyUsage(t); count != 0 {
			t.Fatalf("quota count %d", count)
		}
	}
}

func TestSupplementLoggedAtSurvivesEditAndFreshSnapshot(t *testing.T) {
	h := testkit.New(t)
	baby := testkit.MustMutation(t, "baby.json")
	if _, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, baby); err != nil {
		t.Fatal(err)
	}
	m := testkit.MustMutation(t, "supplement.json")
	created, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || created.Status != "applied" {
		t.Fatalf("create %+v %v", created, err)
	}
	p := m.Payload.(protocol.SupplementPayload)
	original := p.LoggedAt
	p.StartedAt = "2026-09-19T09:00:00.000000Z"
	p.Name = "Iron"
	m.Payload = p
	m.Operation = "update"
	m.MutationID = h.CommandID()
	m.ObservedEntityVersion = 1
	edited, err := h.Service.Apply(t.Context(), h.Caregiver, h.FamilyID, m)
	if err != nil || edited.Status != "applied" {
		t.Fatalf("edit %+v %v", edited, err)
	}
	if edited.CanonicalEntity.Payload.(protocol.SupplementPayload).LoggedAt != original {
		t.Fatal("edit changed logging order")
	}
	h.RebuildSyncService(t)
	snapshot, err := h.Service.FullSnapshot(t.Context(), h.Caregiver, protocol.SnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range snapshot.Entities {
		if e.EntityKind == "supplement" {
			found = true
			got := e.Payload.(protocol.SupplementPayload)
			if got.LoggedAt != original || got.Name != "Iron" || got.StartedAt != p.StartedAt {
				t.Fatalf("fresh snapshot %+v", got)
			}
		}
	}
	if !found {
		t.Fatal("missing supplement")
	}
	before := h.FamilySequence(t)
	p.LoggedAt = "2026-09-21T09:00:00.000000Z"
	m.Payload = p
	m.MutationID = h.CommandID()
	m.ObservedEntityVersion = 2
	rejected, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || rejected.Status != "rejected" || rejected.Code == nil || *rejected.Code != "record_logged_at_mismatch" {
		t.Fatalf("immutable field %+v %v", rejected, err)
	}
	if h.FamilySequence(t) != before {
		t.Fatal("rejected edit advanced journal")
	}
}
