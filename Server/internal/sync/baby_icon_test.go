package sync_test

import (
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

func TestBabyUpdateWithoutIconKeepsStoredIcon(t *testing.T) {
	h := testkit.New(t)
	id := h.SeedBabies(t, 1)[0].CanonicalEntity.Meta.ID
	// Set an icon with an update that carries one.
	h.SetBabyIcon(t, id, "girl.2")
	// An older build renames without the key.
	outcome := h.RenameBaby(t, id, "Renamed")
	if got := outcome.CanonicalEntity.Payload.(protocol.BabyPayload); got.Icon == nil || *got.Icon != "girl.2" || got.Name != "Renamed" {
		t.Fatalf("outcome payload = %+v", got)
	}
	// What the journal hands other phones.
	page, err := h.Service.ApplyAndPull(t.Context(), h.Owner, h.SyncRequest(h.Cursor(0), nil))
	if err != nil || page.NextPageToken != nil {
		t.Fatalf("pull = %+v err=%v", page, err)
	}
	pulled := make([]protocol.CanonicalEntity, 0, len(page.Changes))
	for _, change := range page.Changes {
		pulled = append(pulled, change.SnapshotOrTombstone)
	}
	assertBabyIcon(t, "pull", pulled, id, "girl.2")
	// What a fresh snapshot shows.
	assertBabyIcon(t, "snapshot", fullSnapshot(t, h), id, "girl.2")
	// A present icon replaces the stored one.
	h.SetBabyIcon(t, id, "girl.4")
	assertBabyIcon(t, "snapshot after replace", fullSnapshot(t, h), id, "girl.4")
}

func TestBabyIconOutOfSetIsRejected(t *testing.T) {
	h := testkit.New(t)
	id := h.SeedBabies(t, 1)[0].CanonicalEntity.Meta.ID
	m := testkit.MustMutation(t, "baby.json")
	m.MutationID = h.CommandID()
	m.EntityID = id
	m.Operation = "update"
	m.ObservedEntityVersion = 1
	payload := m.Payload.(protocol.BabyPayload)
	icon := "boy.7"
	payload.Icon = &icon
	m.Payload = payload
	outcome, err := h.Service.Apply(t.Context(), h.Owner, h.FamilyID, m)
	if err != nil || outcome.Status != "rejected" || outcome.Code == nil || *outcome.Code != "invalid_mutation" {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if got := fullSnapshot(t, h); len(got) != 1 || got[0].Payload.(protocol.BabyPayload).Icon != nil {
		t.Fatalf("rejected icon reached the stored baby: %+v", got)
	}
}

func fullSnapshot(t *testing.T, h *testkit.Harness) []protocol.CanonicalEntity {
	t.Helper()
	snapshot, err := h.Service.FullSnapshot(t.Context(), h.Owner, protocol.SnapshotRequest{})
	if err != nil || snapshot.NextPageToken != nil {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	return snapshot.Entities
}

// assertBabyIcon checks the last entity for the baby: a pull page holds every
// version in order, a snapshot holds one.
func assertBabyIcon(t *testing.T, source string, entities []protocol.CanonicalEntity, id, want string) {
	t.Helper()
	var found *protocol.CanonicalEntity
	for index := range entities {
		if entities[index].EntityKind == "baby" && entities[index].Meta.ID == id {
			found = &entities[index]
		}
	}
	if found == nil {
		t.Fatalf("%s: baby %s missing from %+v", source, id, entities)
	}
	payload, ok := found.Payload.(protocol.BabyPayload)
	if !ok || payload.Icon == nil || *payload.Icon != want {
		t.Fatalf("%s: baby payload = %+v, want icon %s", source, found.Payload, want)
	}
}
