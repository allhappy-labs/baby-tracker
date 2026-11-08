package authority

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Retention of the authority's own rows (Phase 3 retention ruling): consumed,
// revoked and refresh-expired sessions, expired sign-in challenges and
// recovery receipts that can no longer recover. The maintenance loop
// (maintenance.SessionSweep) calls these on the background pool.
//
// Each prune is one authority transaction of three round trips: BEGIN with
// the authority flag and the timeouts (lock_timeout 200ms, statement_timeout
// 10s), the prune's statements pipelined in one batch, and COMMIT. A
// rolled-back prune changes nothing; the next one repeats it.
//
// Locks: rows of the swept table only. Sessions and challenges are chosen
// FOR UPDATE SKIP LOCKED, so a prune never waits on a row a request holds
// and two prunes never take the same row. Recovery receipts cannot be locked
// (the runtime holds no UPDATE on refresh_receipts): the receipt prune is a
// plain bounded DELETE, and a receipt another transaction is deleting is
// waited for and then found gone. That wait can close a cycle with a
// request deleting due receipts in the opposite order (a reuse revocation
// deletes the lineage's receipts, then the account's dead ones), and no
// ORDER BY removes it: the request side cannot lock its receipts in order.
// So a prune's lock_timeout (200ms) is below the server's deadlock_timeout
// (1s): the prune gives up (55P03) before the deadlock detector runs, the
// request never sees 40P01, and the next pass retries. No prune locks an
// account, family or device row, so none can take part in the
// accounts → families → sessions order the commands follow.
//
// Clocks: every column compared here is stamped by the app clock (the
// service clock refresh, revocation and sign-in use), so the cutoffs come
// from the caller's app clock, never the database's now().

// pruneBegin opens a prune's authority transaction: the authority flag and
// the timeouts in the same simple-protocol round trip as BEGIN. The lock
// timeout stays below deadlock_timeout (1s, the server default; only a
// superuser may change it), so a prune yields any lock cycle to the request.
const pruneBegin = `BEGIN ISOLATION LEVEL READ COMMITTED READ WRITE; ` +
	`SELECT set_config('app.authority','1',true), set_config('lock_timeout','200ms',true), set_config('statement_timeout','10s',true)`

// The prune statements; $1 is the cutoff, $2 the batch bound.
//
// A session is never pruned while a refresh receipt names it, as parent or
// child: the receipt is what a lost-acknowledgement retry recovers from (the
// consumed parent resolves the presented token, the child is returned), and
// deleting either session would cascade the receipt away. No transaction
// can add a receipt naming a candidate: a rotation names only the active
// parent it consumes (locked, so skipped here, and refused past
// refresh_expires_at, which a refresh-expired candidate is a day past) and
// the child it inserts.
//
// Consumed and revoked sessions are deleted by row address: the locked
// selection hands its rows' ctids to the DELETE, a TID scan, so a batch
// reads each row's heap page once instead of probing sessions_id_key (a
// random leaf of an index over every session) once per row. That is the
// same rows only because no statement updates a consumed or revoked
// session (every UPDATE of sessions names status = 'active'): the version
// the selection locks is the version the statement's snapshot sees, so the
// TID scan finds and deletes each locked row. Active sessions are updated
// (role, scope, consumption, revocation), and a TID scan cannot see a
// version committed after the statement's snapshot that the lock followed
// to, so refresh-expired sessions stay deleted by key: a concurrently
// updated one still eligible is deleted now, not left locked and undeleted.
const (
	pruneConsumedSQL = `DELETE FROM sessions WHERE ctid = ANY(ARRAY(SELECT s.ctid FROM sessions s WHERE s.status = 'consumed' AND s.consumed_at < $1
  AND NOT EXISTS (SELECT 1 FROM refresh_receipts r WHERE r.parent_id = s.id)
  AND NOT EXISTS (SELECT 1 FROM refresh_receipts r WHERE r.child_id = s.id)
  ORDER BY s.consumed_at LIMIT $2 FOR UPDATE SKIP LOCKED))`
	pruneRevokedSQL = `DELETE FROM sessions WHERE ctid = ANY(ARRAY(SELECT s.ctid FROM sessions s WHERE s.status = 'revoked' AND s.revoked_at < $1
  AND NOT EXISTS (SELECT 1 FROM refresh_receipts r WHERE r.parent_id = s.id)
  AND NOT EXISTS (SELECT 1 FROM refresh_receipts r WHERE r.child_id = s.id)
  ORDER BY s.revoked_at LIMIT $2 FOR UPDATE SKIP LOCKED))`
	pruneExpiredSQL = `DELETE FROM sessions WHERE id IN (SELECT s.id FROM sessions s WHERE s.status = 'active' AND s.refresh_expires_at < $1
  AND NOT EXISTS (SELECT 1 FROM refresh_receipts r WHERE r.parent_id = s.id)
  AND NOT EXISTS (SELECT 1 FROM refresh_receipts r WHERE r.child_id = s.id)
  ORDER BY s.refresh_expires_at LIMIT $2 FOR UPDATE SKIP LOCKED)`
	// A challenge is consumable only while expires_at > now
	// (ConsumeChallenge), so one at or past its expiry is dead. The batch is
	// an array the delete probes auth_challenges_pkey with: as a semi-join,
	// the planner may hash it against a scan of the whole table, which the
	// inlined authority_context() filter (000037) makes look cheap.
	pruneChallengesSQL = `DELETE FROM auth_challenges WHERE token_digest = ANY(ARRAY(SELECT token_digest FROM auth_challenges WHERE expires_at <= $1
  ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED))`
	// A receipt recovers only while now is before its child's refresh
	// expiry and before its effective deadline (restore, validateChild):
	// at LEAST of the two it is dead.
	pruneRefreshReceiptsSQL = `DELETE FROM refresh_receipts WHERE parent_id IN (SELECT parent_id FROM refresh_receipts
  WHERE LEAST(refresh_expires_at, effective_deadline) <= $1 ORDER BY LEAST(refresh_expires_at, effective_deadline) LIMIT $2)`
)

// PruneSessions deletes, before cutoff, consumed sessions (by consumed_at),
// revoked sessions (by revoked_at) and active sessions whose refresh expired
// (by refresh_expires_at): at most limit of each kind, oldest first, rows a
// concurrent transaction holds skipped, never a session a refresh receipt
// names. The caller passes its app clock minus the retention window
// (maintenance.ConsumedSessionRetention, decision D2). It returns the number
// of sessions deleted.
func (s *Service) PruneSessions(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	return s.prune(ctx, "sessions", cutoff, limit, pruneConsumedSQL, pruneRevokedSQL, pruneExpiredSQL)
}

// PruneChallenges deletes at most limit sign-in challenges whose expires_at
// is at or before now, oldest first, skipping rows a concurrent transaction
// holds.
func (s *Service) PruneChallenges(ctx context.Context, now time.Time, limit int) (int, error) {
	return s.prune(ctx, "challenges", now, limit, pruneChallengesSQL)
}

// PruneRefreshReceipts deletes at most limit recovery receipts that can no
// longer recover: LEAST(refresh_expires_at, effective_deadline) at or before
// now, oldest first.
func (s *Service) PruneRefreshReceipts(ctx context.Context, now time.Time, limit int) (int, error) {
	return s.prune(ctx, "refresh receipts", now, limit, pruneRefreshReceiptsSQL)
}

// PurgeWork is one family purge to drive (maintenance.Purger).
type PurgeWork struct {
	FamilyID string
	// StartCommandID is the StartFamilyPurge command that began the purge,
	// as the family row persists it (purge_command_id): a purge resumed
	// after a crash completes under it. "" for a family whose purge has
	// not started.
	StartCommandID string
}

// sqlFamilyPurgeWork: started purges first (they resume whatever the
// clock), then deletion-pending families due at $1, each group by ID. A
// sequential scan of families by design: the purge runs every 30 s, off the
// request path, and families is a small table (one row per family).
const sqlFamilyPurgeWork = `SELECT id::text, coalesce(purge_command_id::text, '') FROM families
WHERE (status = 'purge_started' OR (status = 'deletion_pending' AND purge_after <= $1)) AND id <> ALL($3::uuid[])
ORDER BY (status <> 'purge_started'), id LIMIT $2`

// FamilyPurgeWork lists purges to drive, started ones first (resumed after a
// crash by their purge_command_id), then deletion-pending families due at
// now (purge_after at or before it, by the app clock that stamped it), by
// family ID, at most limit (1..1000), never one of exclude (the purge's
// quarantined families, at most 1000). One read-only authority transaction;
// it locks nothing (StartFamilyPurge and CompleteFamilyPurge re-check the
// family under its lock).
func (s *Service) FamilyPurgeWork(ctx context.Context, now time.Time, limit int, exclude ...string) ([]PurgeWork, error) {
	if limit < 1 || limit > 1000 || !validTime(now) || len(exclude) > 1000 {
		return nil, ErrInvalidCommand
	}
	for _, id := range exclude {
		if !validID(id) {
			return nil, ErrInvalidCommand
		}
	}
	if exclude == nil {
		exclude = []string{} // id <> ALL(NULL) would match nothing
	}
	var work []PurgeWork
	err := s.readOnly(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sqlFamilyPurgeWork, now.UTC(), limit, exclude)
		if err != nil {
			return unavailable("read purge work", err)
		}
		work, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (PurgeWork, error) {
			var w PurgeWork
			return w, row.Scan(&w.FamilyID, &w.StartCommandID)
		})
		if err != nil {
			return unavailable("read purge work", err)
		}
		return nil
	})
	return work, err
}

// prune runs statements in one authority transaction and returns the rows
// they deleted.
func (s *Service) prune(ctx context.Context, what string, cutoff time.Time, limit int, statements ...string) (int, error) {
	if limit < 1 || !validTime(cutoff) {
		return 0, ErrInvalidCommand
	}
	cutoff = cutoff.UTC().Truncate(time.Microsecond)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{BeginQuery: pruneBegin})
	if err != nil {
		return 0, fmt.Errorf("authority: prune %s: begin: %w", what, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	batch := &pgx.Batch{}
	for _, statement := range statements {
		batch.Queue(statement, cutoff, limit)
	}
	results := tx.SendBatch(ctx, batch)
	deleted := 0
	for range statements {
		tag, err := results.Exec()
		if err != nil {
			_ = results.Close()
			return 0, fmt.Errorf("authority: prune %s: %w", what, err)
		}
		deleted += int(tag.RowsAffected())
	}
	if err := results.Close(); err != nil {
		return 0, fmt.Errorf("authority: prune %s: %w", what, err)
	}
	if s.afterPruneBatch != nil {
		s.afterPruneBatch()
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("authority: prune %s: commit: %w", what, errors.Join(err, ctx.Err()))
	}
	return deleted, nil
}
