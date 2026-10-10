package authority

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// KeyInventory is the set of key IDs live credentials still reference, per
// keyring, each sorted bytewise. Readiness refuses to serve while a
// referenced key is not configured, so an operator cannot retire a key that
// still verifies a credential.
type KeyInventory struct{ SessionToken, DeviceContinuity, Identity, Invitation []string }

// ReferencedKeyIDs reads the key inventory in one snapshot, at the service
// clock and under mode's session lifetime:
//   - SessionToken: token_key_id of every live session (active, of this
//     recovery epoch, and before its lifetime ends: deadline_at,
//     refresh_expires_at and, self-hosted, one year from the lineage's
//     sign-in, as effectiveDeadline), and of every consumed parent whose
//     refresh receipt still recovers a live child before the receipt's
//     effective deadline (a lost-acknowledgement retry presents the parent's
//     refresh token, which findRefreshSession resolves only under a
//     configured key; legacy pinned it as the recovery candidate's parent
//     key). A session past its lifetime can neither authenticate nor refresh
//     even while its status stays active, so it pins nothing (legacy
//     referencedRecoveryKeyEligibility released it too); other consumed and
//     revoked sessions can never authenticate or refresh;
//   - DeviceContinuity: continuity_key_id of active devices;
//   - Identity: identity_key_id of every identity binding, live or not
//     (sign-in resolves non-live bindings to pick the next lifecycle
//     generation; legacy listed every account's identity key);
//   - Invitation: the key ID inside token_digest
//     (hmac-sha256:<key>:<hex>) of every invitation that is not revoked
//     (an exact retry of a redemption resolves the redeemed invitation's
//     token again; a revoked one only changes invitation_revoked into
//     invitation_invalid).
func (s *Service) ReferencedKeyIDs(ctx context.Context, mode Mode) (KeyInventory, error) {
	if mode != ModeSelfHosted && mode != ModeHosted {
		return KeyInventory{}, ErrInvalidCommand
	}
	now := s.at(time.Time{})
	tx, err := s.beginReadOnly(ctx)
	if err != nil {
		return KeyInventory{}, errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	// $1 now, $2 the self-hosted lineage cap applies, $3 this node's epoch.
	// 8760 hours is selfHostedLineageAge exactly (a '365 days' interval
	// follows the session time zone across DST).
	rows, err := tx.Query(ctx, `SELECT 0, k FROM (SELECT DISTINCT token_key_id AS k FROM sessions
			WHERE status='active' AND recovery_epoch=$3 AND $1 < least(deadline_at, refresh_expires_at,
				CASE WHEN $2 THEN lineage_started_at + interval '8760 hours' ELSE 'infinity'::timestamptz END)) s
		UNION ALL SELECT 0, k FROM (SELECT DISTINCT p.token_key_id AS k FROM refresh_receipts r
			JOIN sessions p ON p.id=r.parent_id JOIN sessions c ON c.id=r.child_id
			WHERE c.status='active' AND c.recovery_epoch=$3 AND r.effective_deadline > $1 AND $1 < least(c.deadline_at, c.refresh_expires_at,
				CASE WHEN $2 THEN c.lineage_started_at + interval '8760 hours' ELSE 'infinity'::timestamptz END)) rp
		UNION ALL SELECT 1, k FROM (SELECT DISTINCT continuity_key_id AS k FROM devices WHERE status='active') d
		UNION ALL SELECT 2, k FROM (SELECT DISTINCT identity_key_id AS k FROM account_identities) i
		UNION ALL SELECT 3, k FROM (SELECT DISTINCT split_part(token_digest, ':', 2) AS k FROM invitations WHERE status<>'revoked') v`,
		now, mode == ModeSelfHosted, int64(s.epoch))
	if err != nil {
		return KeyInventory{}, fmt.Errorf("authority: key inventory: %w", errors.Join(err, ErrUnavailable))
	}
	lists := [4][]string{{}, {}, {}, {}}
	var class int
	var keyID string
	if _, err := pgx.ForEachRow(rows, []any{&class, &keyID}, func() error {
		if class < 0 || class >= len(lists) || keyID == "" {
			return fmt.Errorf("malformed key inventory row (%d, %q)", class, keyID)
		}
		lists[class] = append(lists[class], keyID)
		return nil
	}); err != nil {
		return KeyInventory{}, fmt.Errorf("authority: key inventory: %w", errors.Join(err, ErrUnavailable))
	}
	if err := tx.Commit(ctx); err != nil {
		return KeyInventory{}, fmt.Errorf("authority: key inventory: %w", errors.Join(err, ErrUnavailable))
	}
	for i := range lists {
		slices.Sort(lists[i]) // bytewise, whatever the database collation
		lists[i] = slices.Compact(lists[i])
	}
	return KeyInventory{SessionToken: lists[0], DeviceContinuity: lists[1], Identity: lists[2], Invitation: lists[3]}, nil
}
