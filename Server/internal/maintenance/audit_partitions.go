package maintenance

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"babytracker/server/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditPartitionSweep keeps the monthly security audit partitions three
// months ahead of the app clock (the clock that stamps audit rows' `at`)
// through app.ensure_audit_partitions, on the maintenance pool inside a
// worker slot.
//
// The roll is one implicit transaction in one round trip: the worker
// timeouts and the call, pipelined before a single Sync, so the transaction
// that attaches a month is never left open across a network wait (an open
// roller transaction would hold the default partition's lock while audit
// readers queue behind it). The function attaches each month (SHARE UPDATE
// EXCLUSIVE on the audit parent, which no audit insert conflicts with) and
// bounds its own lock waits to 1s; behind a long reader it fails with 55P03
// and the next run retries. Concurrent rolls serialize on the function's
// advisory lock. A month the default partition already holds rows of is
// skipped and logged at error level: it needs an operator.
type AuditPartitionSweep struct {
	Pool *pgxpool.Pool // maintenance
	// Slots are the maintenance pool's worker slots (nil takes none: tests).
	Slots  *database.WorkerSlots
	Logger *slog.Logger // nil discards
}

const (
	sweepTimeouts       = `SELECT set_config('lock_timeout', '2s', true), set_config('statement_timeout', '10s', true)`
	sqlEnsurePartitions = `SELECT created, skipped FROM app.ensure_audit_partitions($1)`
)

// Name is "audit-partitions".
func (AuditPartitionSweep) Name() string { return "audit-partitions" }

// Run creates the missing monthly partitions through now +
// AuditPartitionHorizon and returns how many it created.
func (s AuditPartitionSweep) Run(ctx context.Context, now time.Time) (int, error) {
	if s.Slots != nil {
		release, err := s.Slots.Acquire(ctx)
		if err != nil {
			return 0, err
		}
		defer release()
	}
	var created int
	var skipped []string
	batch := &pgx.Batch{}
	batch.Queue(sweepTimeouts)
	batch.Queue(sqlEnsurePartitions, now.Add(AuditPartitionHorizon)).QueryRow(func(row pgx.Row) error { return row.Scan(&created, &skipped) })
	if err := s.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return 0, fmt.Errorf("maintenance: roll audit partitions: %w", err)
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	for _, partition := range skipped {
		logger.Error("audit partition not created: the default partition holds rows of its month; move them before the month can be attached",
			slog.String("partition", partition))
	}
	return created, nil
}
