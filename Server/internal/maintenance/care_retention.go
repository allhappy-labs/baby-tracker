package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"babytracker/server/internal/care"
	"babytracker/server/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReceiptSweep deletes care mutation receipts ReceiptRetention after their
// acceptance and releases their charge, on the maintenance pool inside a
// worker slot per batch.
//
// One batch is one maintenance transaction of at most 5 round trips:
//
//  1. BEGIN;
//  2. the worker timeouts and the families of the oldest due receipts,
//     locked FOR NO KEY UPDATE SKIP LOCKED in ID order (a family a request
//     holds is skipped, never waited on);
//  3. the due receipts of the locked families, oldest first, at most the
//     batch size, deleted, each returning its family and stored outcome;
//  4. the release: each family's care_charged_bytes less the sum of
//     care.StoredReceiptCharge over its deleted receipts, the charge each
//     was written with (one UPDATE over every family of the batch);
//  5. COMMIT.
//
// Retry window: a receipt is deleted only once accepted_at <= now −
// ReceiptRetention by the app clock that stamped it, so a retry up to the
// last second of its 30 days replays it (already_applied). A request
// locks its family row before it reads receipts or counters, and the batch
// holds the families it deletes from until it commits, so a request sees
// either the receipts with their charge or neither.
//
// Exactness: the delete and the release commit together or not at all.
// Nothing else writes the counters of a family this batch holds.
//
// Quarantine: a family whose release would make care_charged_bytes negative
// (only after drift; the family's counter is read under the batch's lock and
// checked before the release, and the column's CHECK backs it) or one of
// whose stored outcomes does not decode (its charge is unknown) fails the
// batch, which rolls back whole. The sweep then quarantines that family:
// it logs it once at Error (family ID and cause class, never outcome
// contents), keeps it out of every later selection, and retries the batch
// at once without it, so one bad family never stops retention for the
// others. The family keeps its receipts and their charge until an operator
// repairs it. A quarantine expires after ReceiptQuarantineTTL, so a repaired
// family is swept again within the hour without a restart (one still
// failing is quarantined, and logged, again), and the quarantine holds at
// most ReceiptQuarantineCap families (the oldest is evicted, logged at Warn,
// to make room), as the purge's does. It lives in memory: a restart forgets
// it, and the first pass after a restart detects (and logs) the family
// again.
//
// Locks: family rows in ID order, then receipt rows of those families only,
// then the same family rows' counters; never an account or session row.
// The sweep waits on no lock a request holds (lock_timeout 200ms bounds any
// wait all the same). Two sweeps never share a family.
//
// Each pass drains through an AdaptiveBatch (Batch, or ReceiptBatch, rows
// per batch, re-run while full up to ReceiptPassCap). A ReceiptSweep keeps
// its batch size across runs: use it by pointer.
type ReceiptSweep struct {
	Pool *pgxpool.Pool // maintenance
	// Slots are the maintenance pool's worker slots: each batch holds one
	// for its transaction. nil takes none (tests).
	Slots  *database.WorkerSlots
	Batch  int          // receipts per batch; 0 means ReceiptBatch
	Logger *slog.Logger // nil discards

	once  sync.Once
	batch *AdaptiveBatch
	// quarantined are the families this process does not sweep for now,
	// each with when it was quarantined (app clock; see Quarantine above).
	mu          sync.Mutex
	quarantined map[string]time.Time
	// quarantineCap is ReceiptQuarantineCap unless a test lowers it.
	quarantineCap int
	// Test hooks (export_test.go): between the delete and the release, and
	// after each commit.
	afterDelete, afterCommit func()
}

// maxQuarantinesPerBatch bounds how many failing families one batch steps
// past before it gives up for the pass.
const maxQuarantinesPerBatch = 16

const (
	// ReceiptQuarantineTTL is how long a quarantined family is skipped
	// before the receipt sweep tries it again.
	ReceiptQuarantineTTL = time.Hour
	// ReceiptQuarantineCap bounds the receipt quarantine (the selection
	// excludes every quarantined family).
	ReceiptQuarantineCap = 1000
)

// familyFailure is a batch rolled back because of one family.
type familyFailure struct {
	family, cause string // cause: "release_below_zero" or "undecodable_outcome"
	attrs         []any  // the receipt that failed to decode (its key only)
}

func (f *familyFailure) Error() string {
	return fmt.Sprintf("maintenance: receipt sweep: family %s: %s", f.family, f.cause)
}

const (
	// receiptTimeouts: the batch holds family rows a request may wait on;
	// its own waits are bounded below deadlock_timeout, as the authority
	// prunes are (Task 9).
	receiptTimeouts = `SELECT set_config('lock_timeout', '200ms', true), set_config('statement_timeout', '10s', true)`
	// sqlReceiptFamilies: the families of the oldest due receipts
	// (care_receipts_retention_idx) but the quarantined ($3), locked in ID
	// order, skipping any a writer holds, with their charged bytes as the
	// lock reads them.
	sqlReceiptFamilies = `SELECT f.id::text, f.care_charged_bytes FROM families f
WHERE f.id IN (SELECT DISTINCT r.family_id FROM (SELECT family_id FROM care_receipts
  WHERE accepted_at <= $1 AND family_id <> ALL($3::uuid[]) ORDER BY accepted_at LIMIT $2) r)
ORDER BY f.id FOR NO KEY UPDATE OF f SKIP LOCKED`
	// sqlDeleteReceipts: the locked families' due receipts, oldest first, at
	// most $2 (care_receipts_retention_idx), deleted by their row addresses
	// (a TID scan; see tidDelete).
	sqlDeleteReceipts = `DELETE FROM care_receipts r WHERE r.ctid = ANY(ARRAY(
  SELECT ctid FROM care_receipts WHERE family_id = ANY($3::uuid[]) AND accepted_at <= $1 ORDER BY accepted_at LIMIT $2))
  AND r.family_id = ANY($3::uuid[]) AND r.accepted_at <= $1
RETURNING r.family_id::text, r.device_id::text, r.mutation_id::text, r.outcome::text`
	// sqlReleaseReceipts subtracts each family's released charge.
	sqlReleaseReceipts = `UPDATE families f SET care_charged_bytes = f.care_charged_bytes - v.bytes
FROM unnest($1::uuid[], $2::bigint[]) AS v(id, bytes) WHERE f.id = v.id`
)

// Name is "care-receipts".
func (*ReceiptSweep) Name() string { return "care-receipts" }

// Run deletes the receipts due at the app clock now (accepted at least
// ReceiptRetention before it), releasing their charges, in batches, and
// returns how many it deleted.
func (s *ReceiptSweep) Run(ctx context.Context, now time.Time) (int, error) {
	s.once.Do(func() {
		start := s.Batch
		if start <= 0 {
			start = ReceiptBatch
		}
		s.batch = &AdaptiveBatch{Start: start, Min: min(MinBatch, start), PassCap: max(ReceiptPassCap, start)}
	})
	cutoff := now.Add(-ReceiptRetention)
	removed, capped, err := s.batch.Drain(ctx, func(ctx context.Context, limit int) (int, error) {
		return s.containedBatch(ctx, now, cutoff, limit)
	})
	if capped {
		s.logger().Info("retention backlog: the pass reached its cap; the next pass continues", slog.String("kind", "care_receipts"),
			slog.Int("removed", removed), slog.Int("batch", s.batch.Size()))
	}
	return removed, err
}

// containedBatch runs one batch; a batch that rolls back because of one
// family quarantines that family and runs again at once without it, at most
// maxQuarantinesPerBatch times.
func (s *ReceiptSweep) containedBatch(ctx context.Context, now, cutoff time.Time, limit int) (int, error) {
	for range maxQuarantinesPerBatch {
		n, err := s.sweepBatch(ctx, cutoff, limit, s.quarantine(now))
		var failure *familyFailure
		if !errors.As(err, &failure) {
			return n, err
		}
		s.quarantineFamily(now, failure)
	}
	return 0, fmt.Errorf("maintenance: receipt sweep: %d families failed in one batch", maxQuarantinesPerBatch)
}

// quarantine is the families quarantined at now, for the selection's
// exclusion; entries ReceiptQuarantineTTL old expire.
func (s *ReceiptSweep) quarantine(now time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	families := make([]string, 0, len(s.quarantined))
	for family, since := range s.quarantined {
		if !now.Before(since.Add(ReceiptQuarantineTTL)) {
			delete(s.quarantined, family)
			continue
		}
		families = append(families, family)
	}
	return families
}

// quarantineFamily adds the failing family at now and logs it, once,
// evicting (and logging) the oldest entry when the quarantine is full.
func (s *ReceiptSweep) quarantineFamily(now time.Time, failure *familyFailure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, known := s.quarantined[failure.family]; known {
		return
	}
	if s.quarantined == nil {
		s.quarantined = map[string]time.Time{}
	}
	limit := s.quarantineCap
	if limit <= 0 {
		limit = ReceiptQuarantineCap
	}
	for len(s.quarantined) >= limit {
		oldest, since := "", time.Time{}
		for family, at := range s.quarantined {
			if oldest == "" || at.Before(since) || (at.Equal(since) && family < oldest) {
				oldest, since = family, at
			}
		}
		delete(s.quarantined, oldest)
		s.logger().Warn("care receipt retention: the quarantine is full; its oldest family is swept again", slog.String("family_id", oldest),
			slog.Int("cap", limit))
	}
	s.quarantined[failure.family] = now
	s.logger().Error("care receipt retention: a family's receipts cannot be released; the family is skipped for an hour (until the process restarts at the latest), its receipts kept and charged",
		append([]any{slog.String("family_id", failure.family), slog.String("cause", failure.cause)}, failure.attrs...)...)
}

// sweepBatch runs one batch, excluding the quarantined families, and returns
// the receipts it deleted.
func (s *ReceiptSweep) sweepBatch(ctx context.Context, cutoff time.Time, limit int, quarantined []string) (int, error) {
	if s.Slots != nil {
		release, err := s.Slots.Acquire(ctx)
		if err != nil {
			return 0, err
		}
		defer release()
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("maintenance: receipt sweep: %w", err)
	}
	defer rollback(ctx, tx)
	var families []string
	charged := map[string]int64{}
	batch := &pgx.Batch{}
	batch.Queue(receiptTimeouts)
	batch.Queue(sqlReceiptFamilies, cutoff, limit, quarantined).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			var family string
			var bytes int64
			if err := rows.Scan(&family, &bytes); err != nil {
				return err
			}
			families, charged[family] = append(families, family), bytes
		}
		return rows.Err()
	})
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return 0, fmt.Errorf("maintenance: receipt sweep: lock families: %w", err)
	}
	if len(families) == 0 {
		return 0, nil
	}
	released, deleted, err := s.deleteDue(ctx, tx, cutoff, limit, families)
	if err != nil {
		return 0, err
	}
	if s.afterDelete != nil {
		s.afterDelete()
	}
	if len(released) > 0 {
		ids, bytes := make([]string, 0, len(released)), make([]int64, 0, len(released))
		for _, family := range families { // ID order, the order they are locked in
			if charge, ok := released[family]; ok {
				if charge > charged[family] {
					return 0, &familyFailure{family: family, cause: "release_below_zero"}
				}
				ids, bytes = append(ids, family), append(bytes, charge)
			}
		}
		tag, err := tx.Exec(ctx, sqlReleaseReceipts, ids, bytes)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23514" { // a backstop: the check above runs first
				s.logger().Error("care receipt retention: releasing the deleted receipts' charge would make a family's charged bytes negative; the batch rolled back",
					slog.Int("families", len(ids)))
			}
			return 0, fmt.Errorf("maintenance: receipt sweep: release: %w", err)
		}
		if tag.RowsAffected() != int64(len(ids)) {
			return 0, fmt.Errorf("maintenance: receipt sweep: released %d of %d families", tag.RowsAffected(), len(ids))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("maintenance: receipt sweep: commit: %w", err)
	}
	if s.afterCommit != nil {
		s.afterCommit()
	}
	return deleted, nil
}

// deleteDue deletes the locked families' due receipts and returns each
// family's released charge and the receipts deleted.
func (s *ReceiptSweep) deleteDue(ctx context.Context, tx pgx.Tx, cutoff time.Time, limit int, families []string) (map[string]int64, int, error) {
	rows, err := tx.Query(ctx, sqlDeleteReceipts, cutoff, limit, families)
	if err != nil {
		return nil, 0, fmt.Errorf("maintenance: receipt sweep: delete: %w", err)
	}
	defer rows.Close()
	released := make(map[string]int64, len(families))
	deleted := 0
	for rows.Next() {
		var family, device, mutation, outcome string
		if err := rows.Scan(&family, &device, &mutation, &outcome); err != nil {
			return nil, 0, fmt.Errorf("maintenance: receipt sweep: delete: %w", err)
		}
		charge, err := care.StoredReceiptCharge([]byte(outcome))
		if err != nil { // its charge is unknown: the family is quarantined (the decoder's text is not logged: it may quote the outcome)
			return nil, 0, &familyFailure{family: family, cause: "undecodable_outcome",
				attrs: []any{slog.String("device_id", device), slog.String("mutation_id", mutation)}}
		}
		released[family] += charge
		deleted++
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("maintenance: receipt sweep: delete: %w", err)
	}
	return released, deleted, nil
}

func (s *ReceiptSweep) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// ExpirySweep deletes the rows of one table past their retention, at most
// one bounded statement per batch, on the maintenance pool inside a worker
// slot per batch:
//
//   - sync_page_bindings: expires_at <= now − PageGrace
//     (sync_page_bindings_expiry_idx);
//   - sync_snapshot_sessions: expires_at <= now − PageGrace, a sequential
//     scan by design (000031 has no expiry index there: it would capture
//     the request path's snapshot admission count; the table holds at most
//     a few sessions per account);
//   - device_sync_cursors: seen_at <= now − DeviceCursorRetention
//     (device_sync_cursors_seen_idx).
//
// Every cutoff is by the app clock that stamped the column. The bindings and
// sessions are deleted by a plain bounded DELETE of the rows a SELECT … LIMIT
// n chose (tidDelete), with no lock taken ahead of it: the maintenance
// credential holds no UPDATE there, so it cannot lock rows ahead of the
// delete, and nothing else updates or deletes them (requests only
// insert, and never a row this old), so the delete waits on nothing but a
// concurrent sweep, after which it skips the rows already gone. The device
// cursors are chosen FOR UPDATE SKIP LOCKED: the wake worker's flush and
// mark lock them in key order, the sweep in seen_at order, so it skips
// rather than waits.
//
// One batch is one transaction of 3 round trips (BEGIN; the worker timeouts
// and the delete; COMMIT). Each pass drains through an AdaptiveBatch
// (ExpiryBatch rows per statement, re-run while full up to ExpiryPassCap).
// An ExpirySweep keeps its batch size across runs: use it by pointer.
type ExpirySweep struct {
	Pool *pgxpool.Pool // maintenance
	// Table is "sync_page_bindings", "sync_snapshot_sessions" or
	// "device_sync_cursors".
	Table string
	// Slots are the maintenance pool's worker slots: each batch holds one
	// for its transaction. nil takes none (tests).
	Slots  *database.WorkerSlots
	Logger *slog.Logger // nil discards

	once  sync.Once
	batch *AdaptiveBatch
	// afterDelete is a test hook (export_test.go) between the delete and
	// the commit.
	afterDelete func()
}

// tidDelete: every bounded delete here selects its rows through the
// table's retention index and deletes them by their row addresses, `ctid =
// ANY(ARRAY(SELECT ctid … LIMIT n))`, a TID scan. Joined back on the key
// instead (`(key) IN (SELECT key … LIMIT n)`), a delete of a few hundred
// rows from a table of tens of thousands is planned as a hash semi join over
// a sequential scan of the whole table. A selected address stays the row's
// for the statement: none of these rows is ever updated (receipts,
// bindings, snapshot sessions), or the selection locks it first (device
// cursors), and a row a concurrent sweep deleted cannot be vacuumed away
// while this statement's snapshot still sees it, so the delete finds it gone
// and skips it. The outer DELETE repeats the selection's predicates: a TID
// scan's recheck of a row updated meanwhile tests only the outer quals, so a
// row that no longer qualifies is skipped rather than deleted in its updated
// version. Every table deleted this way is a plain table (relkind 'r',
// pinned by TestCtidDeletesTargetPlainTables): a ctid names a row only
// within one relation, so on a partitioned table the delete would need
// (tableoid, ctid) pairs.
//
// expiry is one table's statement and the retention its cutoff subtracts
// from the app clock.
type expiry struct {
	sql       string
	retention time.Duration
}

var expiries = map[string]expiry{
	"sync_page_bindings": {`DELETE FROM sync_page_bindings WHERE ctid = ANY(ARRAY(
  SELECT ctid FROM sync_page_bindings WHERE expires_at <= $1 ORDER BY expires_at LIMIT $2)) AND expires_at <= $1`, PageGrace},
	"sync_snapshot_sessions": {`DELETE FROM sync_snapshot_sessions WHERE ctid = ANY(ARRAY(
  SELECT ctid FROM sync_snapshot_sessions WHERE expires_at <= $1 ORDER BY expires_at LIMIT $2)) AND expires_at <= $1`, PageGrace},
	"device_sync_cursors": {`DELETE FROM device_sync_cursors WHERE ctid = ANY(ARRAY(
  SELECT ctid FROM device_sync_cursors WHERE seen_at <= $1 ORDER BY seen_at LIMIT $2 FOR UPDATE SKIP LOCKED)) AND seen_at <= $1`, DeviceCursorRetention},
}

// Name is the table.
func (s *ExpirySweep) Name() string { return s.Table }

// Run deletes the table's rows past their retention at the app clock now
// and returns how many it deleted.
func (s *ExpirySweep) Run(ctx context.Context, now time.Time) (int, error) {
	expiry, ok := expiries[s.Table]
	if !ok {
		return 0, fmt.Errorf("maintenance: no expiry sweep for table %q", s.Table)
	}
	s.once.Do(func() { s.batch = &AdaptiveBatch{Start: ExpiryBatch, Min: MinBatch, PassCap: ExpiryPassCap} })
	cutoff := now.Add(-expiry.retention)
	removed, capped, err := s.batch.Drain(ctx, func(ctx context.Context, limit int) (int, error) {
		return s.deleteBatch(ctx, expiry.sql, cutoff, limit)
	})
	if capped && s.Logger != nil {
		s.Logger.Info("retention backlog: the pass reached its cap; the next pass continues", slog.String("kind", s.Table),
			slog.Int("removed", removed), slog.Int("batch", s.batch.Size()))
	}
	return removed, err
}

// deleteBatch runs one bounded delete in its own transaction.
func (s *ExpirySweep) deleteBatch(ctx context.Context, statement string, cutoff time.Time, limit int) (int, error) {
	if s.Slots != nil {
		release, err := s.Slots.Acquire(ctx)
		if err != nil {
			return 0, err
		}
		defer release()
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("maintenance: %s sweep: %w", s.Table, err)
	}
	defer rollback(ctx, tx)
	var deleted int64
	batch := &pgx.Batch{}
	batch.Queue(sweepTimeouts)
	batch.Queue(statement, cutoff, limit).Exec(func(tag pgconn.CommandTag) error {
		deleted = tag.RowsAffected()
		return nil
	})
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return 0, fmt.Errorf("maintenance: %s sweep: %w", s.Table, err)
	}
	if s.afterDelete != nil {
		s.afterDelete()
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("maintenance: %s sweep: commit: %w", s.Table, err)
	}
	return int(deleted), nil
}

// rollback ends tx if it is still open, even after ctx ended (a cancelled
// batch must not leave its connection mid-transaction).
func rollback(ctx context.Context, tx pgx.Tx) {
	rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(rollbackContext)
}
