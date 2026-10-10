package maintenance

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The family purge's cadence and bounds.
const (
	// PurgeInterval is the purge loop's cadence.
	PurgeInterval = 30 * time.Second
	// PurgeBatch is the rows one purge statement deletes at most (one
	// import session at a time, whatever the batch).
	PurgeBatch = 5000
	// PurgePassCap bounds the rows one pass (one tick) of the purge removes:
	// full batches re-run until a short one or the cap.
	PurgePassCap = 20000
)

// Purger is a Sweep that purges deleted families, the retention worker's
// side of the family lifecycle (ScheduleDeletion → StartFamilyPurge →
// CompleteFamilyPurge). Each tick drives one family, the first of
// Authority.FamilyPurgeWork (a started purge before a due deletion-pending
// family), by the step its persisted status calls for:
//
//  1. deletion_pending and due: StartFamilyPurge under the command ID
//     derived from the family (purgeCommandIDs); its exact retry after a
//     lost acknowledgement returns the stored result. From then on no
//     request writes the family, recovery is impossible and every member's
//     session is long revoked.
//  2. purge_started: one maintenance transaction per batch, inside a worker
//     slot: lock the family row FOR NO KEY UPDATE SKIP LOCKED (a family
//     another transaction holds is left for the next tick, never waited
//     on), re-check it is purge_started, then delete one bounded batch from
//     the first non-empty step in purgeSteps order: care receipts,
//     continuation bindings, snapshot sessions and device cursors (the
//     maintenance login's own bounded deletes), then through
//     app.purge_family_step one import session (cascading through its
//     staging and baby mappings, which reference care rows, so before any
//     baby row), care rows that are not babies, babies, and then the
//     family's member names and memberships: label history, display labels,
//     memberships and the (forfeited) recovery claim. Full batches re-run at
//     once (AdaptiveBatch, up to PurgePassCap a tick).
//  3. When a locked transaction finds every step empty it zeroes the four
//     care counters in it and commits; then CompleteFamilyPurge, under the
//     persisted purge_command_id (the Phase 1 carry: a purge resumed after a
//     crash completes the start that is in flight, whatever node began it).
//
// Counters: between steps the family's counters still hold the deleted
// rows' charges (the receipt sweep may release some of its receipts' too,
// by subtraction, under the same family lock); step 3 settles them, in the
// transaction that saw the last row gone, so after it they equal the rows
// (none). next_sequence is kept.
//
// Crash safety: every step is idempotent and keyed on the persisted status.
// A batch rolled back deletes nothing and the next tick repeats it; a crash
// after a commit leaves the next step for the next tick; a crash after the
// zeroing commit zeroes again (nothing to delete) and completes. Locks: the
// family row, then rows of that family only; never an account or session
// row (StartFamilyPurge and CompleteFamilyPurge take their own locks in the
// commands' order). Lock waits are bounded at 200ms (a cursor row the wake
// worker holds, a binding an expiry sweep is deleting), below
// deadlock_timeout.
//
// Quarantine: a family whose purge fails for a reason a retry will not
// change (an authority refusal such as scope_failed on a failed audit chain
// or invalid_command, a database error other than a lock, deadlock,
// serialization, connection or resource failure, or a statement that timed
// out maxPurgeTimeouts ticks in a row at a batch that cannot shrink: the
// minimum, or an authority command) is quarantined: logged at Error (family
// ID and cause class only), left out of the work list, and the tick goes on
// at once with the next family, so one bad family never stops the purge of
// the others. A timeout at a larger batch only halves it (AdaptiveBatch) and
// is retried. A quarantine expires after PurgeQuarantineTTL, so a repaired
// family (or a misjudged failure) is retried hourly without a restart, and
// holds at most PurgeQuarantineCap families (the oldest is evicted, and
// logged, to make room). It lives in memory: a restart forgets it, and the
// first tick after a restart meets (and logs) the family again.
//
// Authority must be the request authority, so the purge's invalidations
// reach the live hub. A Purger keeps its batch size across runs: use it by
// pointer.
type Purger struct {
	Authority *authority.Service
	Pool      *pgxpool.Pool // maintenance
	// Slots are the maintenance pool's worker slots: each purge transaction
	// holds one. nil takes none (tests).
	Slots  *database.WorkerSlots
	Batch  int          // rows per statement; 0 means PurgeBatch
	Logger *slog.Logger // nil discards

	once  sync.Once
	batch *AdaptiveBatch
	// mu guards quarantined (families this process no longer purges, with
	// their cause and when they were quarantined) and timeouts (consecutive
	// statement timeouts at the minimum batch, per family).
	mu          sync.Mutex
	quarantined map[string]quarantined
	timeouts    map[string]int
	// quarantineCap is PurgeQuarantineCap unless a test lowers it.
	quarantineCap int
	// hook is a test hook (export_test.go) at each step: for a delete or
	// the zeroing, after its statement and before its commit (an error
	// rolls the transaction back); for the authority commands, after they
	// committed ("start", "completed") or just before ("complete").
	hook func(step string, deleted int) error
}

// purgeSteps are the hook points, in the order a purge reaches them.
var purgeSteps = []string{"start", "care_receipts", "sync_page_bindings", "sync_snapshot_sessions", "device_sync_cursors",
	"import_sessions", "care_records", "babies", "label_history", "labels", "memberships", "recovery_claim", "zero", "complete", "completed"}

const (
	// maxPurgeTimeouts is how many ticks in a row a family's purge may end
	// in a statement timeout before it is quarantined (a step that never
	// fits statement_timeout, such as a huge import session's cascade).
	maxPurgeTimeouts = 3
	// maxPurgeQuarantinesPerRun bounds how many failing families one tick
	// steps past before it gives up until the next tick.
	maxPurgeQuarantinesPerRun = 16
)

const (
	// PurgeQuarantineTTL is how long a quarantined family is skipped before
	// the purge tries it again.
	PurgeQuarantineTTL = time.Hour
	// PurgeQuarantineCap bounds the quarantine (FamilyPurgeWork excludes at
	// most 1,000 families).
	PurgeQuarantineCap = 1000
)

// quarantined is one quarantined family: why, and since when (app clock).
type quarantined struct {
	cause string
	since time.Time
}

const (
	// purgeTimeouts: lock waits stay below deadlock_timeout (1s), so the
	// purge yields any cycle (55P03) instead of meeting the detector.
	purgeTimeouts = `SELECT set_config('lock_timeout', '200ms', true), set_config('statement_timeout', '10s', true)`
	// sqlPurgeLockFamily locks the family row (families_pkey), skipping it
	// while another transaction holds it.
	sqlPurgeLockFamily = `SELECT status::text FROM families WHERE id = $1 FOR NO KEY UPDATE SKIP LOCKED`
	// sqlPurgeStep is one bounded step of import staging and care rows; the
	// maintenance login cannot delete those rows otherwise.
	sqlPurgeStep = `SELECT step, deleted FROM app.purge_family_step($1, $2)`
	// sqlPurgeZero settles the counters once every row is gone.
	sqlPurgeZero = `UPDATE families SET care_record_count = 0, care_charged_bytes = 0, care_entity_count = 0, care_wire_bytes = 0 WHERE id = $1`
)

// purgeDeletes are the family's metadata, deleted by the maintenance login
// itself, at most $2 rows a statement. Receipts, bindings and snapshot
// sessions are never updated, so they are deleted by row address (a TID
// scan; see tidDelete) after a selection through care_receipts_family_idx,
// sync_page_bindings_pkey and sync_snapshot_sessions_pkey. Device cursors
// are updated by the wake worker, so they are locked by key in the order its
// flush and mark take them (device_sync_cursors_pkey) and deleted by key.
var purgeDeletes = []struct{ step, sql string }{
	{"care_receipts", `DELETE FROM care_receipts WHERE ctid = ANY(ARRAY(
  SELECT ctid FROM care_receipts WHERE family_id = $1 LIMIT $2))`},
	{"sync_page_bindings", `DELETE FROM sync_page_bindings WHERE ctid = ANY(ARRAY(
  SELECT ctid FROM sync_page_bindings WHERE family_id = $1 LIMIT $2))`},
	{"sync_snapshot_sessions", `DELETE FROM sync_snapshot_sessions WHERE ctid = ANY(ARRAY(
  SELECT ctid FROM sync_snapshot_sessions WHERE family_id = $1 LIMIT $2))`},
	{"device_sync_cursors", `DELETE FROM device_sync_cursors WHERE family_id = $1 AND device_id = ANY(ARRAY(
  SELECT device_id FROM device_sync_cursors WHERE family_id = $1 ORDER BY device_id LIMIT $2 FOR UPDATE))`},
}

// Name is "family-purge".
func (*Purger) Name() string { return "family-purge" }

// Run drives one family's purge one step at the app clock now and returns
// the rows it deleted. A family whose purge fails for good is quarantined
// and the next family is driven at once.
func (p *Purger) Run(ctx context.Context, now time.Time) (int, error) {
	p.once.Do(func() {
		start := p.Batch
		if start <= 0 {
			start = PurgeBatch
		}
		p.batch = &AdaptiveBatch{Start: start, Min: min(MinBatch, start), PassCap: max(PurgePassCap, start)}
	})
	removed := 0
	for range maxPurgeQuarantinesPerRun {
		work, err := p.Authority.FamilyPurgeWork(ctx, now, 1, p.quarantine(now)...)
		if err != nil {
			return removed, fmt.Errorf("maintenance: purge: %w", err)
		}
		if len(work) == 0 {
			return removed, nil
		}
		n, shrinkable, err := p.drive(ctx, now, work[0])
		removed += n
		if err == nil || !p.contain(ctx, now, work[0].FamilyID, err, shrinkable) {
			return removed, err
		}
	}
	return removed, fmt.Errorf("maintenance: purge: %d families quarantined in one run", maxPurgeQuarantinesPerRun)
}

// drive runs one family's next purge step (start, a pass of deletes, or the
// completion). On failure, shrinkable reports whether the failing statement
// ran above the minimum batch, so a timeout there halves the batch and is
// retried rather than counted toward quarantine.
func (p *Purger) drive(ctx context.Context, now time.Time, family authority.PurgeWork) (removed int, shrinkable bool, err error) {
	start, complete, receipt := purgeCommandIDs(family.FamilyID)
	if family.StartCommandID == "" {
		if _, err := p.Authority.StartFamilyPurge(ctx, authority.StartFamilyPurge{CommandID: start, FamilyID: family.FamilyID, At: now}); err != nil {
			if errors.Is(err, authority.ErrFamilyUnavailable) {
				return 0, false, nil // recovered since the read: nothing to purge
			}
			return 0, false, fmt.Errorf("maintenance: purge: start: %w", err)
		}
		p.logger().Info("family purge started", slog.String("family_id", family.FamilyID))
		p.succeeded(family.FamilyID)
		return 0, false, p.at("start", 0)
	}
	settled, lastLimit := false, 0
	removed, capped, err := p.batch.Drain(ctx, func(ctx context.Context, limit int) (int, error) {
		lastLimit = limit
		n, done, err := p.step(ctx, family.FamilyID, limit)
		settled = done
		return n, err
	})
	if capped {
		p.logger().Info("family purge: the pass reached its cap; the next pass continues", slog.Int("removed", removed), slog.Int("batch", p.batch.Size()))
	}
	if err != nil || !settled {
		if err == nil {
			p.succeeded(family.FamilyID)
		}
		return removed, lastLimit > p.batch.Min, err
	}
	if err := p.at("complete", 0); err != nil {
		return removed, false, err
	}
	if _, err := p.Authority.CompleteFamilyPurge(ctx, authority.CompleteFamilyPurge{CommandID: complete, FamilyID: family.FamilyID,
		StartCommandID: family.StartCommandID, Receipt: receipt, At: now}); err != nil {
		return removed, false, fmt.Errorf("maintenance: purge: complete: %w", err)
	}
	p.logger().Info("family purge completed", slog.String("family_id", family.FamilyID))
	p.succeeded(family.FamilyID)
	return removed, false, p.at("completed", 0)
}

// purgeCause classifies a failed purge step: its cause class (safe to log:
// an authority code, a SQLSTATE, or a fixed word) and whether a retry can
// succeed. Transient: the run's own context, a lock or statement timeout, a
// deadlock or serialization failure, a lost or overloaded connection, or an
// unavailable authority without a database error. Anything else (an
// authority refusal, an integrity or privilege error) is permanent.
func purgeCause(ctx context.Context, err error) (cause string, transient bool) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled", true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "57014":
			return "statement_timeout", true
		case pgErr.Code == "55P03", pgErr.Code == "40P01", pgErr.Code == "40001",
			strings.HasPrefix(pgErr.Code, "08"), strings.HasPrefix(pgErr.Code, "53"), strings.HasPrefix(pgErr.Code, "57P"):
			return "sqlstate_" + pgErr.Code, true
		}
		return "sqlstate_" + pgErr.Code, false
	}
	if code := authority.CodeOf(err); code != authority.CodeOf(authority.ErrUnavailable) {
		return code, false
	}
	return "unavailable", true
}

// contain decides on a failed family: a permanent failure, or a statement
// timeout maxPurgeTimeouts ticks in a row where the batch could not shrink
// (shrinkable false: at the minimum, or an authority command), quarantines
// it (logged at Error) and reports true, so the run goes on with the next
// family. A timeout of a shrinkable statement is retried at the halved batch
// and not counted.
func (p *Purger) contain(ctx context.Context, now time.Time, familyID string, err error, shrinkable bool) bool {
	cause, transient := purgeCause(ctx, err)
	p.mu.Lock()
	defer p.mu.Unlock()
	if transient {
		if cause != "statement_timeout" || shrinkable {
			return false
		}
		if p.timeouts == nil {
			p.timeouts = map[string]int{}
		}
		p.timeouts[familyID]++
		if p.timeouts[familyID] < maxPurgeTimeouts {
			return false
		}
	}
	delete(p.timeouts, familyID)
	p.quarantineLocked(now, familyID, cause)
	return true
}

// quarantineLocked adds familyID, evicting (and logging) the oldest entry
// when the quarantine is full. p.mu is held.
func (p *Purger) quarantineLocked(now time.Time, familyID, cause string) {
	if p.quarantined == nil {
		p.quarantined = map[string]quarantined{}
	}
	if _, known := p.quarantined[familyID]; known {
		return
	}
	limit := p.quarantineCap
	if limit <= 0 {
		limit = PurgeQuarantineCap
	}
	for len(p.quarantined) >= limit {
		oldest, since := "", time.Time{}
		for family, entry := range p.quarantined {
			if oldest == "" || entry.since.Before(since) || (entry.since.Equal(since) && family < oldest) {
				oldest, since = family, entry.since
			}
		}
		delete(p.quarantined, oldest)
		p.logger().Warn("family purge: the quarantine is full; its oldest family is tried again", slog.String("family_id", oldest),
			slog.Int("cap", limit))
	}
	p.quarantined[familyID] = quarantined{cause: cause, since: now}
	p.logger().Error("family purge: the family's purge keeps failing; it is skipped for an hour (until the process restarts at the latest), its rows kept",
		slog.String("family_id", familyID), slog.String("cause", cause))
}

// succeeded clears a family's run of statement timeouts.
func (p *Purger) succeeded(familyID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.timeouts, familyID)
}

// quarantine is the families quarantined at now, for the work list's
// exclusion; entries PurgeQuarantineTTL old expire.
func (p *Purger) quarantine(now time.Time) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	families := make([]string, 0, len(p.quarantined))
	for family, entry := range p.quarantined {
		if !now.Before(entry.since.Add(PurgeQuarantineTTL)) {
			delete(p.quarantined, family)
			continue
		}
		families = append(families, family)
	}
	return families
}

// step runs one purge transaction over familyID and returns the rows it
// deleted, and settled when it found every step empty and zeroed the
// counters. A family another transaction holds, or one no longer being
// purged, is a step of nothing.
func (p *Purger) step(ctx context.Context, familyID string, limit int) (n int, settled bool, err error) {
	if p.Slots != nil {
		release, err := p.Slots.Acquire(ctx)
		if err != nil {
			return 0, false, err
		}
		defer release()
	}
	tx, err := p.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, false, fmt.Errorf("maintenance: purge: %w", err)
	}
	defer rollback(ctx, tx)
	status := ""
	batch := &pgx.Batch{}
	batch.Queue(purgeTimeouts)
	batch.Queue(sqlPurgeLockFamily, familyID).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			if err := rows.Scan(&status); err != nil {
				return err
			}
		}
		return rows.Err()
	})
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return 0, false, fmt.Errorf("maintenance: purge: lock family: %w", err)
	}
	if status != "purge_started" {
		return 0, false, nil
	}
	for _, d := range purgeDeletes {
		tag, err := tx.Exec(ctx, d.sql, familyID, limit)
		if err != nil {
			return 0, false, fmt.Errorf("maintenance: purge: %s: %w", d.step, err)
		}
		if n := int(tag.RowsAffected()); n > 0 {
			return n, false, p.commit(ctx, tx, d.step, n)
		}
	}
	var step string
	var deleted int
	if err := tx.QueryRow(ctx, sqlPurgeStep, familyID, limit).Scan(&step, &deleted); err != nil {
		return 0, false, fmt.Errorf("maintenance: purge: care step: %w", err)
	}
	if deleted > 0 {
		return deleted, false, p.commit(ctx, tx, step, deleted)
	}
	tag, err := tx.Exec(ctx, sqlPurgeZero, familyID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return 0, false, fmt.Errorf("maintenance: purge: zero counters (%s): %w", pgErr.Code, err)
		}
		return 0, false, fmt.Errorf("maintenance: purge: zero counters: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return 0, false, fmt.Errorf("maintenance: purge: zeroed %d family rows", tag.RowsAffected())
	}
	if err := p.commit(ctx, tx, "zero", 0); err != nil {
		return 0, false, err
	}
	return 0, true, nil
}

// commit runs the hook for step, then commits.
func (p *Purger) commit(ctx context.Context, tx pgx.Tx, step string, deleted int) error {
	if err := p.at(step, deleted); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("maintenance: purge: %s: commit: %w", step, err)
	}
	return nil
}

func (p *Purger) at(step string, deleted int) error {
	if p.hook == nil {
		return nil
	}
	return p.hook(step, deleted)
}

func (p *Purger) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// purgeCommandIDs are a family's purge command IDs and physical purge
// receipt: version-4-shaped UUIDs from sha256(domain 0x00 family ID), the
// same for the same family, so a retry after a lost acknowledgement is the
// command's exact retry.
func purgeCommandIDs(familyID string) (start, complete, receipt string) {
	return purgeUUID("baby-tracker/family-purge-start/v1", familyID), purgeUUID("baby-tracker/family-purge-complete/v1", familyID),
		purgeUUID("baby-tracker/family-purge-receipt/v1", familyID)
}

func purgeUUID(domain, value string) string {
	sum := sha256.Sum256([]byte(domain + "\x00" + value))
	sum[6] = sum[6]&0x0f | 0x40
	sum[8] = sum[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}
