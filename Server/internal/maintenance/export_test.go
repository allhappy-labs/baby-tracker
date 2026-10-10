package maintenance

import (
	"context"
	"time"
)

// AfterDelete sets a test hook run between a batch's delete and its
// release (nil clears it).
func (s *ReceiptSweep) AfterDelete(hook func()) { s.afterDelete = hook }

// AfterCommit sets a test hook run after each batch commits (nil clears
// it).
func (s *ReceiptSweep) AfterCommit(hook func()) { s.afterCommit = hook }

// Once runs one pass (Pass) and returns its counts, whatever failed.
func (l Loop) Once(ctx context.Context) map[string]int {
	counts, _ := l.Pass(ctx)
	return counts
}

// SetQuarantineCap lowers the receipt quarantine's cap.
func (s *ReceiptSweep) SetQuarantineCap(limit int) { s.quarantineCap = limit }

// Quarantine quarantines familyID at now with cause, as a failing batch
// would.
func (s *ReceiptSweep) Quarantine(now time.Time, familyID, cause string) {
	s.quarantineFamily(now, &familyFailure{family: familyID, cause: cause})
}

// Quarantined is the receipt quarantine at now (expired entries dropped).
func (s *ReceiptSweep) Quarantined(now time.Time) []string { return s.quarantine(now) }

// AfterDelete sets a test hook run between a batch's delete and its commit
// (nil clears it).
func (s *ExpirySweep) AfterDelete(hook func()) { s.afterDelete = hook }

// ReceiptSweepSQL are the receipt sweep's family selection, delete and
// release statements.
var ReceiptSweepSQL = map[string]string{"families": sqlReceiptFamilies, "delete": sqlDeleteReceipts, "release": sqlReleaseReceipts}

// ReceiptTimeouts is the receipt sweep's timeouts statement.
const ReceiptTimeouts = receiptTimeouts

// ExpirySQL is each ExpirySweep table's delete.
func ExpirySQL(table string) string { return expiries[table].sql }

// FailAt sets a test hook run at each purge step (nil clears it): for a
// delete or the zeroing after its statement and before its commit, for the
// authority commands after "start" and "completed" committed and just
// before "complete". An error from it ends the run (a crash).
func (p *Purger) FailAt(hook func(step string, deleted int) error) { p.hook = hook }

// PurgeCause is purgeCause: a failure's cause class and whether it is
// transient.
func PurgeCause(ctx context.Context, err error) (string, bool) { return purgeCause(ctx, err) }

// SetQuarantineCap lowers the purge quarantine's cap.
func (p *Purger) SetQuarantineCap(limit int) { p.quarantineCap = limit }

// Quarantine quarantines familyID at now with cause, as a failure would.
func (p *Purger) Quarantine(now time.Time, familyID, cause string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.quarantineLocked(now, familyID, cause)
}

// Quarantined is the families quarantined at now (expired entries dropped).
func (p *Purger) Quarantined(now time.Time) []string { return p.quarantine(now) }

// PurgeSteps are the purge's hook points in order.
var PurgeSteps = purgeSteps

// PurgeCommandIDs are a family's derived purge command IDs and receipt.
func PurgeCommandIDs(familyID string) (start, complete, receipt string) {
	return purgeCommandIDs(familyID)
}

// PurgeTimeouts is the purge transactions' timeouts statement.
const PurgeTimeouts = purgeTimeouts

// PurgeSQL are the purge's metadata deletes by step, and its family lock,
// care step and zeroing.
func PurgeSQL() map[string]string {
	out := map[string]string{"lock": sqlPurgeLockFamily, "step": sqlPurgeStep, "zero": sqlPurgeZero}
	for _, d := range purgeDeletes {
		out[d.step] = d.sql
	}
	return out
}
