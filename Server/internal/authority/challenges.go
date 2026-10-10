package authority

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// challengeLifetime is an Apple sign-in challenge's lifetime
// (000005 create_auth_challenge: expires_at = created_at + 5 minutes).
const challengeLifetime = 5 * time.Minute

// CreateChallenge stores a sign-in challenge's digest
// (app.create_auth_challenge): expiresAt must be exactly now + 5 minutes.
// Every failure, a duplicate digest included, is service_unavailable.
func (s *Service) CreateChallenge(ctx context.Context, digest [32]byte, expiresAt, now time.Time) error {
	now, expiresAt = now.UTC().Truncate(time.Microsecond), expiresAt.UTC().Truncate(time.Microsecond)
	if !validTime(now) || !validTime(expiresAt) || !expiresAt.Equal(now.Add(challengeLifetime)) {
		return ErrUnavailable
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO auth_challenges(token_digest,expires_at,created_at) VALUES ($1,$2,$3)`, digest[:], expiresAt, now)
		return err
	})
}

// ConsumeChallenge consumes an unconsumed, unexpired challenge once
// (app.consume_auth_challenge); anything else is invalid_or_expired_challenge.
// Locks: the challenge row.
func (s *Service) ConsumeChallenge(ctx context.Context, digest [32]byte, now time.Time) error {
	now = now.UTC().Truncate(time.Microsecond)
	if !validTime(now) {
		return ErrUnavailable
	}
	consumed := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE auth_challenges SET consumed_at=$2 WHERE token_digest=$1 AND consumed_at IS NULL AND expires_at > $2`, digest[:], now)
		consumed = err == nil && tag.RowsAffected() == 1
		return err
	})
	if err != nil {
		return err
	}
	if !consumed {
		return ErrInvalidOrExpiredChallenge
	}
	return nil
}

// inTx runs fn in an authority transaction and commits it; any failure is a
// wrapped ErrUnavailable.
func (s *Service) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := fn(tx); err != nil {
		return fmt.Errorf("authority: %w", errors.Join(err, ErrUnavailable))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("authority: commit: %w", errors.Join(err, ErrUnavailable))
	}
	return nil
}
