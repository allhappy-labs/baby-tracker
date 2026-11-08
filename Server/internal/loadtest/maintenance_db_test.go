package loadtest

import (
	"testing"

	"babytracker/server/internal/testkit"
)

// The hot purge's database side on a real schema: after the product's
// ScheduleDeletion the family is deletion_pending 30 days out; purgeDueNow
// (the disposable stack's clock jump, under the authority context the
// families trigger requires) makes it due now, which is what the purge
// worker's work query selects (retention.go: deletion_pending and
// purge_after <= now). It refuses a family that is not deletion_pending,
// and the status and row counts read through the superuser.
func TestHotPurgeMakesAScheduledFamilyDueNow(t *testing.T) {
	h := testkit.New(t)
	db := superuserDB{pool: superuser(t, h)}
	applyAndLedger(t, h, 3)
	if err := db.purgeDueNow(t.Context(), h.FamilyID); err == nil {
		t.Fatal("an active family was made due")
	}
	if entities, receipts, err := db.careRows(t.Context(), h.FamilyID); err != nil || entities != 4 || receipts < 3 {
		t.Fatalf("care rows %d, receipts %d: %v", entities, receipts, err)
	}
	if _, err := h.Family.ScheduleDeletion(t.Context(), h.Owner, h.CommandID(), true); err != nil {
		t.Fatal(err)
	}
	var due bool
	if err := db.pool.QueryRow(t.Context(), `SELECT purge_after <= now() FROM families WHERE id = $1`, h.FamilyID).Scan(&due); err != nil || due {
		t.Fatalf("scheduled family due before the jump: %v %v", due, err)
	}
	if err := db.purgeDueNow(t.Context(), h.FamilyID); err != nil {
		t.Fatal(err)
	}
	if err := db.pool.QueryRow(t.Context(), `SELECT status = 'deletion_pending' AND purge_after <= now() FROM families WHERE id = $1`,
		h.FamilyID).Scan(&due); err != nil || !due {
		t.Fatalf("the family is not due for the purge worker: %v %v", due, err)
	}
	if status, err := db.familyStatus(t.Context(), h.FamilyID); err != nil || status != "deletion_pending" {
		t.Fatalf("status %q, %v", status, err)
	}
	if status, err := db.familyStatus(t.Context(), "00000000-0000-4000-8000-000000000000"); err != nil || status != "" {
		t.Fatalf("a missing family: %q, %v", status, err)
	}
}
