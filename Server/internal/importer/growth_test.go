package importer_test

import (
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
	"encoding/hex"
	"testing"
)

func TestGrowthProfileImportRemapsIdentityAndConflicts(t *testing.T) {
	h := testkit.New(t)
	baby := "10000000-0000-4000-8000-000000000010"
	seedBaby(t, h, baby, "Élie")
	m := testkit.MustMutation(t, "growth-profile.json")
	p := m.Payload.(protocol.GrowthProfilePayload)
	p.BabyID = baby
	m.Payload = p
	m.EntityID = protocol.GrowthProfileID(baby)
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	hash, err := protocol.CanonicalContentHash(m.EntityKind, m.Payload)
	if err != nil {
		t.Fatal(err)
	}
	r.Records = []protocol.RecordManifest{{RecordID: protocol.GrowthProfileID(r.Babies[0].LocalBabyID), EntityKind: m.EntityKind, LocalBabyID: r.Babies[0].LocalBabyID, ContentSHA256: hex.EncodeToString(hash[:])}}
	r.TotalRecordCount = 1
	resolved, err := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if err != nil || !resolved.MappingComplete {
		t.Fatalf("resolve %+v %v", resolved, err)
	}
	m.ImportID = &r.ImportID
	m.MutationID = h.CommandID()
	out := apply(t, h, h.Caregiver, m)
	if out.CanonicalEntity == nil || out.CanonicalEntity.Meta.ID != protocol.GrowthProfileID(baby) {
		t.Fatalf("remapped profile %+v", out)
	}
	m.MutationID = h.CommandID()
	again := apply(t, h, h.Caregiver, m)
	if again.CanonicalEntity == nil || again.CanonicalEntity.Meta.EntityVersion != 1 {
		t.Fatalf("reuse %+v", again)
	}
	// A different local profile for the same mapped baby must become a resolver conflict.
	r.ImportID = h.CommandID()
	p.ReferenceSex = "boys"
	hash, err = protocol.CanonicalContentHash(m.EntityKind, p)
	if err != nil {
		t.Fatal(err)
	}
	r.Records[0].ContentSHA256 = hex.EncodeToString(hash[:])
	resolved, err = h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if err != nil || len(resolved.Conflicts) == 0 {
		t.Fatalf("differing shared profile did not conflict: %+v %v", resolved, err)
	}
}
