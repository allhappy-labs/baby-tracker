package maintenance

import "time"

// Cadences, windows and batch bounds (Phase 3 retention and worker rulings).
const (
	// RetentionInterval is the retention loop's cadence.
	RetentionInterval = 10 * time.Second
	// AuditPartitionInterval is the audit partition roll's cadence.
	AuditPartitionInterval = time.Hour
	// AuditPartitionHorizon is how far ahead the monthly audit partitions
	// reach: three months, so no audit row lands in the default partition.
	AuditPartitionHorizon = 92 * 24 * time.Hour
	// AuditRetentionInterval is the audit retention loop's cadence: a month
	// expires once a month, and a run that finds nothing to drop reads only
	// the catalog.
	AuditRetentionInterval = time.Hour
	// AuditRetentionMonths keeps security audit history 13 months (owner
	// decision D1, 2026-10-16): a partition is dropped once its whole range
	// ended that long ago. app.drop_expired_audit_partition enforces the same
	// window against the database's clock.
	AuditRetentionMonths = 13
	// ConsumedSessionRetention keeps consumed, revoked and refresh-expired
	// sessions for 24 hours. Owner decision D2: after it, a consumed token's
	// replay is session_revoked, not lineage revocation.
	ConsumedSessionRetention = 24 * time.Hour
	// ReceiptRetention keeps a care mutation receipt 30 days (spec) after
	// its acceptance, by the app clock that stamped accepted_at: a retry
	// inside it replays the receipt, and its deletion releases its charge.
	ReceiptRetention = 30 * 24 * time.Hour
	// PageGrace keeps a continuation binding and a snapshot session an hour
	// past its expires_at, so a just-expired token still answers
	// page_expired; once swept it is invalid_page_token (both mean "take a
	// snapshot" to the client).
	PageGrace = time.Hour
	// DeviceCursorRetention keeps a device's sync cursor 35 days after the
	// wake worker last saw it (seen_at advances at most once a day).
	DeviceCursorRetention = 35 * 24 * time.Hour
)

// Batch bounds (AdaptiveBatch). Each statement starts at 1,000 rows and a
// pass re-runs full batches up to its cap, so a pass drains as much as the
// former single 5,000/5,000/2,000-row batches did, in shorter transactions.
const (
	// SessionBatch is the rows per session kind per statement (consumed,
	// revoked and refresh-expired, one statement each per batch).
	SessionBatch = 1000
	// ChallengeBatch is the challenges per statement.
	ChallengeBatch = 1000
	// RefreshReceiptBatch is the recovery receipts per statement.
	RefreshReceiptBatch = 1000
	// MinBatch is the floor a statement timeout halves a batch down to.
	MinBatch = 100
	// SessionPassCap bounds the sessions one pass removes: 1,500/s of
	// capacity at the 10 s cadence against ~70/s consumed at the load model.
	SessionPassCap = 15000
	// ChallengePassCap bounds the challenges one pass removes.
	ChallengePassCap = 5000
	// RefreshReceiptPassCap bounds the recovery receipts one pass removes.
	RefreshReceiptPassCap = 2000
	// ReceiptBatch is the care receipts per batch: each batch locks the
	// families of the oldest due receipts, so a request waiting on one of
	// them waits for one small batch.
	ReceiptBatch = 1000
	// ReceiptPassCap bounds the care receipts one pass removes: 200/s of
	// capacity at the 10 s cadence against under 10 care writes per second
	// at the load model.
	ReceiptPassCap = 2000
	// ExpiryBatch is the continuation bindings, snapshot sessions or device
	// cursors per statement.
	ExpiryBatch = 1000
	// ExpiryPassCap bounds the rows of one table one pass removes: 500/s of
	// capacity per table. Only a pull that pages issues a binding (most of
	// the 500–700 pulls per second fit one page), a session is one snapshot,
	// and a device has one cursor per family.
	ExpiryPassCap = 5000
)
