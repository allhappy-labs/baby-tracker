package authority

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Test hooks for the external authority_test package, which cannot be the
// authority package itself because it imports authoritytest.

type AccountRow = accountRow
type FamilyRow = familyRow
type LockedChainHead = chainHead

var (
	AppendAudit   = appendAudit
	LockAccounts  = lockAccounts
	LockFamilies  = lockFamilies
	LookupReceipt = lookupReceipt
	WriteReceipt  = writeReceipt
	AuditRowHash  = auditRowHash
	CanonicalJSON = canonicalJSON
)

func (s *Service) Begin(ctx context.Context) (pgx.Tx, error) { return s.begin(ctx) }

// NewChainHead builds a head that was not read under a lock (stale or
// malformed heads in tests).
func NewChainHead(scope Scope, scopeID string, seq uint64, hash [32]byte, failed bool) *LockedChainHead {
	return &chainHead{Scope: scopeRef{kind: scope, id: scopeID}, Seq: seq, Hash: hash, Failed: failed}
}

// SetVerifyPageRows shrinks the verifier's keyset page for one test.
func SetVerifyPageRows(t interface{ Cleanup(func()) }, n int) {
	old := verifyPageRows
	verifyPageRows = n
	t.Cleanup(func() { verifyPageRows = old })
}

var MapFamilyWriteError = mapFamilyWriteError

// InvalidationSubscriberCount is the number of live invalidation subscriptions.
func (s *Service) InvalidationSubscriberCount() int { return s.invalidationSubscriberCount() }

// ReadOnlyForTest runs fn in the read helpers' transaction (readOnly).
func ReadOnlyForTest(s *Service, ctx context.Context, fn func(pgx.Tx) error) error {
	return s.readOnly(ctx, fn)
}

// PublishForTest publishes items as a committed command would.
func (s *Service) PublishForTest(items ...SecurityInvalidation) { s.publish(items...) }

// SetAfterPruneBatch makes hook run after each prune's statements and before
// its COMMIT (nil: none). A hook that cancels the prune's context leaves it
// rolled back.
func SetAfterPruneBatch(s *Service, hook func()) { s.afterPruneBatch = hook }

// PruneSQL are the prune statements, for plan tests.
var PruneSQL = map[string]string{
	"consumed":   pruneConsumedSQL,
	"revoked":    pruneRevokedSQL,
	"expired":    pruneExpiredSQL,
	"challenges": pruneChallengesSQL,
	"receipts":   pruneRefreshReceiptsSQL,
}

// PruneBegin is the prune transaction's BEGIN query.
const PruneBegin = pruneBegin
