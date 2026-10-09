package records

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// sessionOwnerMismatch reports whether sessionID already groups records of
// another baby or of the other kind in this family. A session ID is opaque but
// must never stitch a sleep to a feed or one baby to another.
func sessionOwnerMismatch(ctx context.Context, tx pgx.Tx, familyID, babyID, kind, sessionID string) (bool, error) {
	var mismatch bool
	err := tx.QueryRow(ctx, `SELECT
	  EXISTS(SELECT 1 FROM feed_entries WHERE family_id=$1 AND tombstoned_at IS NULL
	         AND (id=$4 OR feeding_session_id=$4) AND ($3<>'feed' OR baby_id<>$2))
	  OR EXISTS(SELECT 1 FROM sleep_entries WHERE family_id=$1 AND tombstoned_at IS NULL
	         AND (id=$4 OR sleep_session_id=$4) AND ($3<>'sleep' OR baby_id<>$2))
	  OR EXISTS(SELECT 1 FROM active_timers WHERE family_id=$1 AND tombstoned_at IS NULL
	         AND session_id=$4 AND (timer_kind<>$3 OR baby_id<>$2))`,
		familyID, babyID, kind, sessionID).Scan(&mismatch)
	return mismatch, err
}
