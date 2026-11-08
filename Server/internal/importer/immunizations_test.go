package importer_test

import (
	"encoding/hex"
	"testing"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

func TestImportImmunizationAndReuseCanonicalRecord(t *testing.T) {
	h := testkit.New(t)
	baby := "10000000-0000-4000-8000-000000000010"
	seedBaby(t, h, baby, "Élie")
	m := testkit.MustMutation(t, "immunization.json")
	p := m.Payload.(protocol.ImmunizationPayload)
	p.BabyID = baby
	m.Payload = p
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	hash, err := protocol.CanonicalContentHash(m.EntityKind, m.Payload)
	if err != nil {
		t.Fatal(err)
	}
	r.Records = []protocol.RecordManifest{{RecordID: m.EntityID, EntityKind: m.EntityKind, LocalBabyID: r.Babies[0].LocalBabyID, ContentSHA256: hex.EncodeToString(hash[:])}}
	r.TotalRecordCount = 1
	resolved, err := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if err != nil || !resolved.MappingComplete {
		t.Fatalf("resolve %+v %v", resolved, err)
	}
	m.ImportID = &r.ImportID
	m.MutationID = h.CommandID()
	out := apply(t, h, h.Caregiver, m)
	if out.CanonicalEntity == nil {
		t.Fatalf("canonical %+v", out)
	}
	got := out.CanonicalEntity.Payload.(protocol.ImmunizationPayload)
	if got.Vaccine != "Prevenar 13" || got.Dose == nil || *got.Dose != 2 || got.BabyID != baby {
		t.Fatalf("canonical %+v", got)
	}
	m.MutationID = h.CommandID()
	again := apply(t, h, h.Caregiver, m)
	if again.CanonicalEntity == nil || again.CanonicalEntity.Meta.EntityVersion != 1 {
		t.Fatalf("import reuse %+v", again)
	}
}
