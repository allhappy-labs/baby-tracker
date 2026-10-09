package main

import (
	"babytracker/server/internal/httpapi"
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"log/slog"
	"time"
)

const recoveryCleanupInterval = 5 * time.Minute

type recoveryCleanup struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func (worker recoveryCleanup) RunOnce(ctx context.Context) error {
	_, err := worker.pool.Exec(ctx, `SELECT app.cleanup_refresh_recovery_receipts($1::timestamptz)`, worker.now().UTC())
	return err
}

type recoveryCleanupRunner interface{ RunOnce(context.Context) error }

// Each independent iteration has a deadline and a bounded SQL batch. A
// transient error waits the normal cadence; shutdown joins the caller's loop.
func runRecoveryCleanup(ctx context.Context, readiness httpapi.Readiness, worker recoveryCleanupRunner, logger *slog.Logger, interval time.Duration) {
	if readiness == nil || worker == nil || interval <= 0 {
		return
	}
	for ctx.Err() == nil {
		iteration, cancel := context.WithTimeout(ctx, 30*time.Second)
		if readiness.Check(iteration) == nil {
			if err := worker.RunOnce(iteration); err != nil {
				if ctx.Err() == nil && logger != nil {
					logger.Warn("recovery cleanup iteration failed")
				}
			} else {
				recoveryCleanupObserved(logger)
			}
		}
		cancel()
		if !waitLifecyclePoll(ctx, interval) {
			return
		}
	}
}
