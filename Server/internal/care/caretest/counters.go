package caretest

import (
	"encoding/json"
	"testing"

	"babytracker/server/internal/care"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/testkit"
)

// LegacyReceiptCharge is the legacy storage's charge of a mutation receipt
// (records.ReceiptStorageBytes at 342973dc: the encoded outcome plus 256
// bytes), copied as the independent oracle of the receipt charge.
func LegacyReceiptCharge(outcome protocol.Outcome) (int64, error) {
	raw, err := json.Marshal(outcome)
	if err != nil {
		return 0, err
	}
	return int64(len(raw)) + 256, nil
}

// AssertCounters fails t unless the family's care counters equal the
// charge recomputed from what remains: rows, retained import staging and
// live receipts. The receipt worker releases each deleted receipt's charge
// by subtraction, and a completed purge has zeroed the counters with no row
// left, so the recomputation over existing rows holds after both. (It no
// longer assumes import or receipt charges are never released.)
//
// The record count, entity count and wire bytes are the aggregates of the
// family's care rows. Charged bytes are each row's wire plus 256 bytes, plus
// 4,096 bytes of deletion reserve while it is live, plus the storage_bytes
// of every import staging row the family retains (each care.ChargeImport
// charges exactly the storage_bytes of the row it writes or resizes; only
// the purge removes staging rows, and it zeroes the counters with the last
// of them), plus each receipt the family still holds,
// charged as the legacy receipt charge of its stored outcome (decoded as
// written). The family's next_sequence is not a counter and is not checked.
//
// The read is a runtime tenant transaction in familyID's context, so any
// family can be checked, a purged one included.
func AssertCounters(t *testing.T, h *testkit.Harness, familyID string) {
	t.Helper()
	tx := TenantTx(t, h.Runtime, h.Owner.AccountID, familyID)
	defer func() { _ = tx.Rollback(t.Context()) }()
	state, err := care.ReadFamilyState(t.Context(), tx, familyID)
	if err != nil {
		t.Fatal(err)
	}
	want := care.FamilyState{Sequence: state.Sequence}
	if err := tx.QueryRow(t.Context(), `SELECT
		count(*) FILTER (WHERE tombstoned_at IS NULL AND kind NOT IN (1, 5)),
		coalesce(sum(octet_length(wire) + 256), 0) + 4096 * count(*) FILTER (WHERE tombstoned_at IS NULL),
		count(*), coalesce(sum(octet_length(wire)), 0) FROM care_entities WHERE family_id = $1`, familyID).
		Scan(&want.RecordCount, &want.ChargedBytes, &want.EntityCount, &want.WireBytes); err != nil {
		t.Fatal(err)
	}
	var imports uint64
	if err := tx.QueryRow(t.Context(), `SELECT
		(SELECT coalesce(sum(storage_bytes), 0) FROM import_sessions WHERE family_id = $1) +
		(SELECT coalesce(sum(storage_bytes), 0) FROM import_chunks WHERE family_id = $1) +
		(SELECT coalesce(sum(storage_bytes), 0) FROM import_baby_candidates WHERE family_id = $1) +
		(SELECT coalesce(sum(storage_bytes), 0) FROM import_record_manifests WHERE family_id = $1) +
		(SELECT coalesce(sum(storage_bytes), 0) FROM import_conflicts WHERE family_id = $1) +
		(SELECT coalesce(sum(storage_bytes), 0) FROM import_conflict_receipts WHERE family_id = $1) +
		(SELECT coalesce(sum(storage_bytes), 0) FROM import_baby_mappings WHERE family_id = $1)`, familyID).Scan(&imports); err != nil {
		t.Fatal(err)
	}
	want.ChargedBytes += imports
	receipts, err := tx.Query(t.Context(), `SELECT outcome::text FROM care_receipts WHERE family_id = $1`, familyID)
	if err != nil {
		t.Fatal(err)
	}
	for receipts.Next() {
		var raw string
		var outcome protocol.Outcome
		if err := receipts.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &outcome); err != nil {
			t.Fatal(err)
		}
		charge, err := LegacyReceiptCharge(outcome)
		if err != nil {
			t.Fatal(err)
		}
		want.ChargedBytes += uint64(charge)
	}
	if err := receipts.Err(); err != nil {
		t.Fatal(err)
	}
	if state != want {
		t.Fatalf("family %s counters %+v, recomputed from its rows, import staging and receipts %+v", familyID, state, want)
	}
	// The production recomputation (care.RecomputeCounters) is this
	// oracle's.
	production, err := care.RecomputeCounters(t.Context(), tx, familyID)
	if err != nil {
		t.Fatal(err)
	}
	production.Sequence = state.Sequence
	if production != want {
		t.Fatalf("family %s: care.RecomputeCounters %+v, the oracle %+v", familyID, production, want)
	}
}
