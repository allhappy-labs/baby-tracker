package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"babytracker/server/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditRetentionSweep drops the security audit partitions whose whole range
// is older than AuditRetentionMonths (owner decision D1, 2026-10-16) through
// app.drop_expired_audit_partition, on the maintenance pool inside a worker
// slot, oldest first and one partition per call.
//
// Each call is one implicit transaction in one round trip, as the partition
// roll's is: the timeouts and the call pipelined before a single Sync. The
// function takes the roll's advisory lock, records each chain's retention
// anchor (the seq and hash of its last dropped row) and detaches and drops
// the partition under a 1s lock timeout: behind a long reader of the audit
// history it fails with 55P03 and the next run (an hour later: the loop backs
// off a whole interval, so a long reader does not stall audit inserts for a
// second every minute) retries. It refuses, changing nothing, to anchor over
// a chain that is failed or whose rows up to the new anchor are not one per
// seq linked by prev_hash (XX001, logged at error level). It never drops the
// default partition, nor anything holding rows from the last
// AuditRetentionMonths by the database's clock, so never the current or the
// next partition whatever the app clock says.
type AuditRetentionSweep struct {
	Pool *pgxpool.Pool // maintenance
	// Slots are the maintenance pool's worker slots (nil takes none: tests).
	Slots  *database.WorkerSlots
	Logger *slog.Logger // nil discards
}

const (
	// retentionTimeouts leaves the function its own 1s lock timeout and gives
	// the chain check and anchors over a whole month room: measured at 7–10 s
	// for 500,000 rows (10,000 chains of 50) on the test container, about
	// 15–20 µs a row, so 120 s covers several million rows a month. The check
	// holds only SHARE on the expiring partition (nothing writes there) and
	// the roll's advisory lock; audit inserts never wait on it.
	retentionTimeouts = `SELECT set_config('statement_timeout', '120s', true)`
	// AuditRetentionSweepTimeout bounds one run (Loop.Timeout): a drop's
	// statement plus the next partition's, should two be due.
	AuditRetentionSweepTimeout = 5 * time.Minute
	sqlDropExpiredPartition    = `SELECT dropped, anchored FROM app.drop_expired_audit_partition($1)`
	// retentionRefusedCode and retentionRefusedMessage are the definer's
	// refusal to anchor over a chain that does not hold together (XX001,
	// data_corrupted).
	retentionRefusedCode    = "XX001"
	retentionRefusedMessage = "audit retention refused"
	// retentionPassPartitions bounds the partitions one run drops: a month
	// expires a month apart, so more than one is due only after a long
	// outage or when the window first applies.
	retentionPassPartitions = 12
)

// Name is "audit-retention".
func (AuditRetentionSweep) Name() string { return "audit-retention" }

// Run drops every expired partition (at most retentionPassPartitions) and
// returns how many it dropped.
func (s AuditRetentionSweep) Run(ctx context.Context, now time.Time) (int, error) {
	logger := s.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	before := now.UTC().AddDate(0, -AuditRetentionMonths, 0)
	dropped := 0
	for dropped < retentionPassPartitions {
		partition, anchored, err := s.dropOne(ctx, before)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == retentionRefusedCode && pgErr.Message == retentionRefusedMessage {
				// A chain in the partition does not hold together (or is
				// failed): no anchor may be taken over it. The detail is the
				// definer's own text (partition, chain, seq, reason).
				logger.Error("audit partition not dropped: a chain in it does not verify; it needs an operator",
					slog.String("detail", pgErr.Detail))
			}
			return dropped, fmt.Errorf("maintenance: drop expired audit partition: %w", err)
		}
		if partition == nil {
			break
		}
		dropped++
		logger.Info("expired audit partition dropped", slog.String("partition", *partition), slog.Int("anchored_chains", anchored))
	}
	return dropped, nil
}

// dropOne drops the oldest expired partition, if any, inside a worker slot.
func (s AuditRetentionSweep) dropOne(ctx context.Context, before time.Time) (*string, int, error) {
	if s.Slots != nil {
		release, err := s.Slots.Acquire(ctx)
		if err != nil {
			return nil, 0, err
		}
		defer release()
	}
	var partition *string
	var anchored int
	batch := &pgx.Batch{}
	batch.Queue(retentionTimeouts)
	batch.Queue(sqlDropExpiredPartition, before).QueryRow(func(row pgx.Row) error { return row.Scan(&partition, &anchored) })
	if err := s.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return nil, 0, err
	}
	return partition, anchored, nil
}
