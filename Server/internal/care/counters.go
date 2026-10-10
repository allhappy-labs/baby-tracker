package care

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// sqlRecomputeCounters recomputes a family's ($1) counters from its care
// rows (Row.CountsAsRecord: live and not $2's kinds; Row.ChargedBytes: the
// wire and $3 per row, plus $4 while live) and adds the import staging it
// retains (ChargeImport charges each staging row's storage_bytes until the
// purge releases them).
const sqlRecomputeCounters = `SELECT
  count(*) FILTER (WHERE c.tombstoned_at IS NULL AND c.kind <> ALL ($2::smallint[])),
  (coalesce(sum(octet_length(c.wire)), 0) + $3::bigint * count(*) + $4::bigint * count(*) FILTER (WHERE c.tombstoned_at IS NULL)
    + (SELECT coalesce(sum(storage_bytes), 0) FROM import_sessions WHERE family_id = $1)
    + (SELECT coalesce(sum(storage_bytes), 0) FROM import_chunks WHERE family_id = $1)
    + (SELECT coalesce(sum(storage_bytes), 0) FROM import_baby_candidates WHERE family_id = $1)
    + (SELECT coalesce(sum(storage_bytes), 0) FROM import_record_manifests WHERE family_id = $1)
    + (SELECT coalesce(sum(storage_bytes), 0) FROM import_conflicts WHERE family_id = $1)
    + (SELECT coalesce(sum(storage_bytes), 0) FROM import_conflict_receipts WHERE family_id = $1)
    + (SELECT coalesce(sum(storage_bytes), 0) FROM import_baby_mappings WHERE family_id = $1))::bigint,
  count(*), coalesce(sum(octet_length(c.wire)), 0)::bigint
FROM care_entities c WHERE c.family_id = $1::uuid`

// sqlStoredReceipts is every receipt the family holds, its outcome as stored.
const sqlStoredReceipts = `SELECT outcome::text FROM care_receipts WHERE family_id = $1::uuid`

// queueRecomputeCounters queues onto b the reads RecomputeCounters makes;
// read returns the counters once everything queued before them was read. The
// reads see what the transaction's row security admits: the caller is in the
// family's tenant context (care_entities, care_receipts and the import
// staging tables are family-scoped).
func queueRecomputeCounters(b *pgx.Batch, familyID string) (read func(pgx.BatchResults) (FamilyState, error)) {
	b.Queue(sqlRecomputeCounters, familyID, []int16{KindBaby, KindTimer}, int64(RowOverheadBytes), int64(LiveReserveBytes))
	b.Queue(sqlStoredReceipts, familyID)
	return func(results pgx.BatchResults) (FamilyState, error) {
		var s FamilyState
		var records, charged, entities, wire int64
		if err := results.QueryRow().Scan(&records, &charged, &entities, &wire); err != nil {
			return FamilyState{}, fmt.Errorf("care: recompute the counters of family %s: %w", familyID, err)
		}
		rows, err := results.Query()
		if err != nil {
			return FamilyState{}, fmt.Errorf("care: recompute the receipt charge of family %s: %w", familyID, err)
		}
		var outcome string
		if _, err := pgx.ForEachRow(rows, []any{&outcome}, func() error {
			charge, err := StoredReceiptCharge([]byte(outcome))
			charged += charge
			return err
		}); err != nil {
			return FamilyState{}, fmt.Errorf("care: recompute the receipt charge of family %s: %w", familyID, err)
		}
		s.RecordCount, s.ChargedBytes, s.EntityCount, s.WireBytes = uint64(records), uint64(charged), uint64(entities), uint64(wire)
		return s, nil
	}
}

// RecomputeCounters returns the family's four care counters as the write
// paths keep them, recomputed from what the family holds (Sequence is left
// zero):
//   - RecordCount: its live care rows that count as records (Row.CountsAsRecord);
//   - ChargedBytes: every care row's charge (Row.ChargedBytes), every
//     receipt it holds (StoredReceiptCharge: charged when written, released
//     by the receipt sweep with this same function) and the import staging
//     it retains (ChargeImport: each staging row's storage_bytes);
//   - EntityCount and WireBytes: its care rows and their wire bytes.
//
// It is the definition families' counters are checked against
// (caretest.AssertCounters asserts it equals its own independent oracle).
// The reads run in tx under its row security: the caller is in the family's
// tenant context.
func RecomputeCounters(ctx context.Context, tx pgx.Tx, familyID string) (FamilyState, error) {
	b := &pgx.Batch{}
	read := queueRecomputeCounters(b, familyID)
	results := tx.SendBatch(ctx, b)
	state, err := read(results)
	if closeErr := results.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("care: recompute the counters of family %s: %w", familyID, closeErr)
	}
	return state, err
}
