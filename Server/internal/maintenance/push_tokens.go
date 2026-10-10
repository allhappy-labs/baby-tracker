package maintenance

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"babytracker/server/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// PushTokenInterval is the push-token sweep's cadence.
	PushTokenInterval = time.Hour
	// PushTokenBatch is the tokens per statement.
	PushTokenBatch = 1000
	// PushTokenPassCap bounds the tokens one pass removes. Tokens go stale
	// only as devices sign out, are removed, or their family is deleted, a
	// few a minute at most; an hourly pass of 5,000 drains any backlog.
	PushTokenPassCap = 5000
)

// PushTokenSweep deletes APNs device tokens whose device no longer holds an
// active, unexpired family session (signed out, removed from its family,
// revoked, or its family deleted or purged), through app.prune_push_tokens
// (maintenance holds no privilege on device_push_tokens). It runs hourly in
// its own loop, on the maintenance pool: each batch is one implicit
// transaction in one round trip inside a worker slot. The wake worker never
// pushes such a device anyway (app.push_targets checks the session at send
// time); the sweep only keeps the table to live devices. A device that signs
// in again uploads its token again.
//
// Each pass drains through an AdaptiveBatch (Batch, or PushTokenBatch, tokens
// per statement, re-run while full up to PushTokenPassCap). A PushTokenSweep
// keeps its batch size across runs: use it by pointer.
type PushTokenSweep struct {
	Pool *pgxpool.Pool // maintenance
	// Slots are the maintenance pool's worker slots: each batch holds one.
	// nil takes none (tests).
	Slots  *database.WorkerSlots
	Batch  int          // tokens per statement; 0 means PushTokenBatch
	Logger *slog.Logger // nil discards

	once  sync.Once
	batch *AdaptiveBatch
}

const sqlPrunePushTokens = `SELECT app.prune_push_tokens($1)`

// Name is "push-tokens".
func (*PushTokenSweep) Name() string { return "push-tokens" }

// Run deletes tokens without a family session, in batches, and returns how
// many it deleted.
func (s *PushTokenSweep) Run(ctx context.Context, _ time.Time) (int, error) {
	s.once.Do(func() {
		start := s.Batch
		if start <= 0 {
			start = PushTokenBatch
		}
		s.batch = &AdaptiveBatch{Start: start, Min: min(MinBatch, start), PassCap: max(PushTokenPassCap, start)}
	})
	removed, capped, err := s.batch.Drain(ctx, s.pruneBatch)
	if capped && s.Logger != nil {
		s.Logger.Info("retention backlog: the pass reached its cap; the next pass continues", slog.String("kind", "device_push_tokens"),
			slog.Int("removed", removed), slog.Int("batch", s.batch.Size()))
	}
	return removed, err
}

// pruneBatch deletes at most limit tokens in one statement.
func (s *PushTokenSweep) pruneBatch(ctx context.Context, limit int) (int, error) {
	if s.Slots != nil {
		release, err := s.Slots.Acquire(ctx)
		if err != nil {
			return 0, err
		}
		defer release()
	}
	var deleted int
	batch := &pgx.Batch{}
	batch.Queue(sweepTimeouts)
	batch.Queue(sqlPrunePushTokens, limit).QueryRow(func(row pgx.Row) error { return row.Scan(&deleted) })
	if err := s.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return 0, fmt.Errorf("maintenance: prune push tokens: %w", err)
	}
	return deleted, nil
}
