package sync

import (
	"context"

	"babytracker/server/internal/access"
	"babytracker/server/internal/protocol"
	"github.com/jackc/pgx/v5"
)

// SetPageLookup makes s fill first pages' token lookups with read instead
// of crypto/rand.
func SetPageLookup(s *Service, read func([]byte) (int, error)) { s.pageLookup = read }

// Statements are the request-path statements this package sends itself,
// by name, for the plan test.
func Statements() map[string]string {
	return map[string]string{
		"sync_continuation":     sqlSyncContinuation,
		"snapshot_continuation": sqlSnapshotContinuation,
		"snapshot_admission":    sqlSnapshotAdmission,
	}
}

// ExchangeAtCommit is the service's own exchange (ServeSync,
// ApplyAndPull): in a request transaction a pull goes with the COMMIT, and
// the result must be settled (Settle) before it is used.
func ExchangeAtCommit(ctx context.Context, s *Service, tx pgx.Tx, p access.Principal, g access.Grant, request protocol.SyncRequest) (Exchanged, error) {
	return s.exchangeRequest(ctx, tx, p, g, request, true)
}

// Settle is the service's settle as ApplyAndPull runs it: a pull read with
// the commit gets its page, and a page with more stores its binding in a
// second transaction for p's session family.
func Settle(ctx context.Context, s *Service, p access.Principal, e *Exchanged) error {
	return s.settle(ctx, e, s.principalBindingTx(ctx, p, p.SessionFamilyID))
}

// Unsettled reports whether e is a pull read with the commit that Settle
// has not completed.
func Unsettled(e Exchanged) bool { return e.page != nil }

// SetPendingFamily makes the binding e's page will store name familyID
// (a page read for another family than the one its binding transaction
// grants); it reports whether e had a pending binding.
func SetPendingFamily(e *Exchanged, familyID string) bool {
	if e.page == nil || e.page.binding == nil {
		return false
	}
	e.page.binding.binding.familyID = familyID
	return true
}
