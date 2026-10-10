package maintenance

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"babytracker/server/internal/authority"
)

// SessionSweep prunes the authority's rows, each batch in its own authority
// transaction on the authority's pool (the background pool in production):
// recovery receipts that can no longer recover (at now), then consumed,
// revoked and refresh-expired sessions older than ConsumedSessionRetention
// that no receipt names, then expired sign-in challenges (at now).
// Receipts go first, so a session whose last receipt falls due is pruned in
// the same pass. Each kind drains through its AdaptiveBatch: batches of
// 1,000 rows re-run while full, up to the kind's pass cap (logged at Info:
// a backlog), halved after a statement timeout.
//
// Retry safety: a lost-acknowledgement retry needs its receipt, the consumed
// parent and the active child. The receipt lives until it could no longer
// recover anyway (its child's refresh expiry or its effective deadline), and
// no session it names is pruned while it lives. A plain (non-recoverable)
// refresh replay needs only the consumed parent, kept 24 hours (decision
// D2); afterwards the replay is session_revoked.
//
// A SessionSweep keeps its batch sizes across runs: use it by pointer.
type SessionSweep struct {
	Authority *authority.Service
	Logger    *slog.Logger // nil discards

	once                           sync.Once
	receipts, sessions, challenges *AdaptiveBatch
}

// Name is "sessions".
func (*SessionSweep) Name() string { return "sessions" }

// Run drains each kind at the app clock now and returns the rows removed.
// A failing kind does not stop the others.
func (s *SessionSweep) Run(ctx context.Context, now time.Time) (int, error) {
	s.once.Do(func() {
		s.receipts = &AdaptiveBatch{Start: RefreshReceiptBatch, Min: MinBatch, PassCap: RefreshReceiptPassCap}
		s.sessions = &AdaptiveBatch{Start: SessionBatch, Min: MinBatch, PassCap: SessionPassCap}
		s.challenges = &AdaptiveBatch{Start: ChallengeBatch, Min: MinBatch, PassCap: ChallengePassCap}
	})
	cutoff := now.Add(-ConsumedSessionRetention)
	receipts, receiptsErr := s.drain(ctx, "refresh_receipts", s.receipts, func(ctx context.Context, limit int) (int, error) {
		return s.Authority.PruneRefreshReceipts(ctx, now, limit)
	})
	sessions, sessionsErr := s.drain(ctx, "sessions", s.sessions, func(ctx context.Context, limit int) (int, error) {
		return s.Authority.PruneSessions(ctx, cutoff, limit)
	})
	challenges, challengesErr := s.drain(ctx, "challenges", s.challenges, func(ctx context.Context, limit int) (int, error) {
		return s.Authority.PruneChallenges(ctx, now, limit)
	})
	return receipts + sessions + challenges, errors.Join(receiptsErr, sessionsErr, challengesErr)
}

// drain runs one kind's batches and logs a pass that reached its cap.
func (s *SessionSweep) drain(ctx context.Context, kind string, batch *AdaptiveBatch, prune func(context.Context, int) (int, error)) (int, error) {
	removed, capped, err := batch.Drain(ctx, prune)
	if capped && s.Logger != nil {
		s.Logger.Info("retention backlog: the pass reached its cap; the next pass continues", slog.String("kind", kind),
			slog.Int("removed", removed), slog.Int("batch", batch.Size()))
	}
	return removed, err
}
