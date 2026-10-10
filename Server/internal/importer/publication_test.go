package importer_test

import (
	"testing"

	"babytracker/server/internal/care/caretest"
	"babytracker/server/internal/importer"
	"babytracker/server/internal/testkit"
)

// mappingsMatchCare requires every import mapping row's
// (shared_baby_entity_version, shared_baby_cursor) to be its care baby's
// (entity_version, changed_seq), and returns them by local baby.
func mappingsMatchCare(t *testing.T, h *testkit.Harness, importID string) map[string][2]uint64 {
	t.Helper()
	tx := caretest.TenantTx(t, h.Runtime, h.Owner.AccountID, h.FamilyID)
	defer func() { _ = tx.Rollback(t.Context()) }()
	rows, err := tx.Query(t.Context(), `SELECT m.local_baby_id::text, m.shared_baby_entity_version, m.shared_baby_cursor, c.entity_version, c.changed_seq
		FROM import_baby_mappings m JOIN care_entities c ON c.family_id = m.family_id AND c.id = m.shared_baby_id
		WHERE m.family_id = $1 AND m.import_id = $2 AND c.kind = 1`, h.FamilyID, importID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][2]uint64{}
	for rows.Next() {
		var local string
		var version, cursor, rowVersion, rowSeq uint64
		if err := rows.Scan(&local, &version, &cursor, &rowVersion, &rowSeq); err != nil {
			t.Fatal(err)
		}
		if version != rowVersion || cursor != rowSeq {
			t.Fatalf("mapping of %s records version %d at %d, its baby is version %d at %d", local, version, cursor, rowVersion, rowSeq)
		}
		out[local] = [2]uint64{version, cursor}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Each mapping records its baby's version and changed_seq, both for a
// matched baby already stored (here at version 2, sequence 3) and for each
// baby the publication creates (one batch, consecutive sequences).
func TestPublicationMappingsRecordTheirBabysVersionAndCursor(t *testing.T) {
	h := testkit.New(t)
	seedBaby(t, h, "10000000-0000-4000-8000-000000000009", "Other")
	shared := "10000000-0000-4000-8000-000000000010"
	seedBaby(t, h, shared, "Élie")
	h.RenameBaby(t, shared, "Élie") // version 2 at sequence 3
	r := testkit.ImportRequest(0, "Élie", "2026-01-12")
	for i, name := range []string{"Noa", "Mila"} {
		b := r.Babies[0]
		b.LocalBabyID, b.Name, b.SortOrder = []string{"20000000-0000-4000-8000-000000000091", "20000000-0000-4000-8000-000000000092"}[i], name, uint32(i+1)
		r.Babies = append(r.Babies, b)
	}
	out, e := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if e != nil || !out.MappingComplete || len(out.Mappings) != 3 {
		t.Fatalf("resolve %+v %v", out, e)
	}
	got := mappingsMatchCare(t, h, r.ImportID)
	if len(got) != 3 || got[r.Babies[0].LocalBabyID] != [2]uint64{2, 3} || got[r.Babies[1].LocalBabyID] != [2]uint64{1, 4} ||
		got[r.Babies[2].LocalBabyID] != [2]uint64{1, 5} || h.FamilySequence(t) != 5 {
		t.Fatalf("mappings %v at sequence %d", got, h.FamilySequence(t))
	}
}

// Publication reads and writes no receipt: a device that once sent a
// mutation under a publication's derivable mutation id neither replays its
// outcome into the import nor refuses it.
func TestPublicationIgnoresAReceiptUnderItsMutationID(t *testing.T) {
	h := testkit.New(t)
	r := testkit.ImportRequest(0, "Noa", "2026-01-12")
	local := r.Babies[0].LocalBabyID
	reused := testkit.MustMutation(t, "baby.json")
	reused.MutationID = importer.PublicationMutationID(r.ImportID, local)
	reused.EntityID = "10000000-0000-4000-8000-0000000000aa"
	apply(t, h, h.Caregiver, reused) // the initiating device's receipt under that id
	receipts := h.Count(t, "care_receipts", h.FamilyID)
	out, e := h.Importer.Resolve(t.Context(), h.Caregiver, r)
	if e != nil || !out.MappingComplete || len(out.Mappings) != 1 || out.Mappings[0].SharedBabyID != local {
		t.Fatalf("resolve %+v %v", out, e)
	}
	if h.CareCount(t, "baby", h.FamilyID) != 2 || h.Count(t, "care_receipts", h.FamilyID) != receipts {
		t.Fatalf("publication: %d babies, %d receipts (was %d)", h.CareCount(t, "baby", h.FamilyID), h.Count(t, "care_receipts", h.FamilyID), receipts)
	}
	mappingsMatchCare(t, h, r.ImportID)
}
