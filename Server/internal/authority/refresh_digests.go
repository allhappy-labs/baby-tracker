package authority

import (
	"context"
	"fmt"
	"time"
)

// liveRefreshDigestsSQL lists the refresh digests a refresh can still
// present: an active session (not consumed, not revoked) of this recovery
// epoch whose refresh token has not expired (refresh_expires_at is at most
// deadline_at, so its deadline has not passed either), the latest issued
// first. It reads only sessions_expired_idx, the partial index on
// refresh_expires_at of the active rows, backwards from the newest: the
// consumed and revoked rows (almost all of the table) are not in it, the
// expired active rows are outside the range, and the scan stops at $3.
const liveRefreshDigestsSQL = `SELECT token_key_id, refresh_digest FROM public.sessions
WHERE status = 'active' AND refresh_expires_at > $1 AND recovery_epoch = $2
ORDER BY refresh_expires_at DESC LIMIT $3`

// LiveRefreshDigests is up to limit (token key, refresh digest) pairs of the
// sessions whose refresh token can still be used, the latest issued first,
// read in one read-only authority transaction on the runtime pool. The
// digests are keyed (sessions.refresh_digest): they reveal no token, and
// knowing one authorizes nothing. HTTP admission warms its known refresh
// set with them at startup, so a refresh of a session another process
// issued takes a Sync slot rather than the auth lane.
func (s *Service) LiveRefreshDigests(ctx context.Context, limit int) ([]KeyedDigest, error) {
	if limit <= 0 {
		return nil, nil
	}
	tx, err := s.beginReadOnly(ctx)
	if err != nil {
		return nil, unavailable("live refresh digests", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, liveRefreshDigestsSQL, s.at(time.Time{}), int64(s.epoch), limit)
	if err != nil {
		return nil, unavailable("live refresh digests", err)
	}
	defer rows.Close()
	digests := make([]KeyedDigest, 0, min(limit, 4096))
	for rows.Next() {
		var keyID string
		var sum []byte
		if err := rows.Scan(&keyID, &sum); err != nil {
			return nil, unavailable("live refresh digests", err)
		}
		if len(sum) != 32 {
			return nil, unavailable("live refresh digests", fmt.Errorf("refresh digest of %d bytes", len(sum)))
		}
		digest := KeyedDigest{KeyID: keyID}
		copy(digest.Sum[:], sum)
		digests = append(digests, digest)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable("live refresh digests", err)
	}
	return digests, nil
}
