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
// a backlog), halved after a statement timeout and grown back after full
// batches. Each pass logs one line (rows per kind, duration, batch sizes).
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
	started := time.Now()
	receipts, receiptsErr := s.drain(ctx, "refresh_receipts", s.receipts, func(ctx context.Context, limit int) (int, error) {
		return s.Authority.PruneRefreshReceipts(ctx, now, limit)
	})
	sessions, sessionsErr := s.drain(ctx, "sessions", s.sessions, func(ctx context.Context, limit int) (int, error) {
		return s.Authority.PruneSessions(ctx, cutoff, limit)
	})
	challenges, challengesErr := s.drain(ctx, "challenges", s.challenges, func(ctx context.Context, limit int) (int, error) {
		return s.Authority.PruneChallenges(ctx, now, limit)
	})
	err := errors.Join(receiptsErr, sessionsErr, challengesErr)
	s.logPass(receipts, sessions, challenges, time.Since(started), err != nil)
	return receipts + sessions + challenges, err
}

// logPass logs one line per pass, numbers only: the rows each kind removed,
// the pass duration and each kind's batch size after the pass (Task 17's
// retention throughput). At the 10 s cadence it is at Info only when the
// pass did something worth reading (it removed rows, failed, or ran with a
// batch below its start), and at Debug otherwise, so an idle node logs
// nothing at Info.
func (s *SessionSweep) logPass(receipts, sessions, challenges int, took time.Duration, failed bool) {
	if s.Logger == nil {
		return
	}
	level := slog.LevelDebug
	if receipts+sessions+challenges > 0 || failed || s.receipts.Size() < s.receipts.Start || s.sessions.Size() < s.sessions.Start ||
		s.challenges.Size() < s.challenges.Start {
		level = slog.LevelInfo
	}
	s.Logger.Log(context.Background(), level, "session sweep pass", slog.Int("refresh_receipts", receipts), slog.Int("sessions", sessions),
		slog.Int("challenges", challenges), slog.Int64("duration_ms", took.Milliseconds()), slog.Bool("failed", failed),
		slog.Int("refresh_receipts_batch", s.receipts.Size()), slog.Int("sessions_batch", s.sessions.Size()),
		slog.Int("challenges_batch", s.challenges.Size()))
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
