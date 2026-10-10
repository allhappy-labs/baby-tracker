package family

import (
	"context"
	"errors"
	"strconv"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Tenant reads and the own-profile write. Each runs inside one authority
// guarded transaction: WithAccountTx re-checks the caller's session and sets
// the account's tenant context; WithFamilyReadTx also evaluates family policy
// and names the family through its grant, never through the caller's claimed
// scope.

func readAccount(ctx context.Context, a *authority.Service, principal access.Principal) (AccountView, error) {
	var result AccountView
	err := a.WithAccountTx(ctx, principal, func(tx pgx.Tx) error {
		var familyID, slotState *string
		if err := tx.QueryRow(ctx, `SELECT a.id::text, a.display_name, a.status::text, a.profile_required, s.family_id::text, s.state::text
			FROM accounts a LEFT JOIN family_slots s ON s.account_id = a.id WHERE a.id = $1`, principal.AccountID).
			Scan(&result.ID, &result.DisplayName, &result.Status, &result.ProfileRequired, &familyID, &slotState); err != nil {
			return err
		}
		if familyID != nil && slotState != nil {
			result.FamilySlot = &FamilySlotView{FamilyID: *familyID, State: *slotState}
		}
		return nil
	})
	return result, err
}

// Profile command receipts are endpoint-local presentation receipts. Authority
// receipts are checked by Service before this function; this table prevents an
// accepted profile command from mutating presentation again with changed input.
func updateProfile(ctx context.Context, a *authority.Service, principal access.Principal, commandID, displayName string, now time.Time) error {
	return a.WithAccountTx(ctx, principal, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT app.update_own_profile($1::uuid,$2::uuid,$3::text,$4::timestamptz)`, principal.AccountID, commandID, displayName, now)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "BT002" {
			return ErrCommandIDReused
		}
		return err
	})
}

// readFamily reads the granted family and its active members.
func readFamily(ctx context.Context, tx pgx.Tx, familyID string) (FamilyView, error) {
	result := FamilyView{Members: []MemberView{}}
	if err := tx.QueryRow(ctx, `SELECT id::text,display_name,status::text,profile_required FROM families WHERE id=$1::uuid`, familyID).
		Scan(&result.ID, &result.DisplayName, &result.Status, &result.ProfileRequired); err != nil {
		return FamilyView{}, err
	}
	rows, err := tx.Query(ctx, `SELECT m.account_id::text,l.display_name,m.role::text,m.status::text
		FROM memberships m JOIN family_actor_labels l ON l.family_id=m.family_id AND l.account_id=m.account_id
		WHERE m.family_id=$1::uuid AND m.status='active' ORDER BY m.account_id`, familyID)
	if err != nil {
		return FamilyView{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var member MemberView
		if err := rows.Scan(&member.AccountID, &member.DisplayName, &member.Role, &member.Status); err != nil {
			return FamilyView{}, err
		}
		result.Members = append(result.Members, member)
	}
	return result, rows.Err()
}

func readActorLabels(ctx context.Context, tx pgx.Tx, familyID, after, highWater string, limit int) (ActorLabelPage, error) {
	result := ActorLabelPage{Items: []ActorLabelView{}, HighWater: highWater}
	if result.HighWater == "" {
		var highWater uint64
		if err := tx.QueryRow(ctx, `SELECT app.family_actor_labels_high_water()`).Scan(&highWater); err != nil {
			return ActorLabelPage{}, err
		}
		result.HighWater = strconv.FormatUint(highWater, 10)
	}
	highWaterValue, err := strconv.ParseUint(result.HighWater, 10, 64)
	if err != nil {
		return ActorLabelPage{}, err
	}
	if highWaterValue == 0 {
		return result, nil
	}
	var afterValue any
	if after != "" {
		afterValue = after
	}
	rows, err := tx.Query(ctx, `SELECT account_id::text,display_name,former,high_water,has_more
		FROM app.list_family_actor_labels($1::uuid,$2::bigint,$3::uuid,$4::integer)`, familyID, highWaterValue, afterValue, limit)
	if err != nil {
		return ActorLabelPage{}, err
	}
	defer rows.Close()
	hasMore := false
	for rows.Next() {
		var item ActorLabelView
		var returnedHighWater uint64
		if err := rows.Scan(&item.AccountID, &item.DisplayName, &item.Former, &returnedHighWater, &hasMore); err != nil {
			return ActorLabelPage{}, err
		}
		if returnedHighWater != highWaterValue {
			return ActorLabelPage{}, errors.New("actor label high-water mismatch")
		}
		result.Items = append(result.Items, item)
	}
	if err := rows.Err(); err != nil {
		return ActorLabelPage{}, err
	}
	if hasMore && len(result.Items) > 0 {
		result.NextCursor = result.Items[len(result.Items)-1].AccountID
	}
	return result, nil
}

// readInvitation reads one invitation of the granted family; another
// family's invitation is not found.
func readInvitation(ctx context.Context, tx pgx.Tx, familyID, invitationID string) (InvitationView, bool, error) {
	var result InvitationView
	err := tx.QueryRow(ctx, `SELECT id::text,family_id::text,role::text,status::text,expires_at,created_at FROM invitations
		WHERE id=$1::uuid AND family_id=$2::uuid`, invitationID, familyID).
		Scan(&result.ID, &result.FamilyID, &result.Role, &result.Status, &result.ExpiresAt, &result.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return InvitationView{}, false, nil
	}
	if err != nil {
		return InvitationView{}, false, err
	}
	result.ExpiresAt = result.ExpiresAt.UTC()
	result.CreatedAt = result.CreatedAt.UTC()
	return result, true, nil
}

// readPendingInvitations lists the family's active, unexpired invitations,
// newest first. "Unexpired" is judged against the caller-supplied clock rather
// than the database's own now(), matching every other expiry decision in this
// package so tests can move time deterministically instead of racing the
// wall clock. The family filter is explicit rather than left to row-level
// security alone, so a policy change can never widen what an owner sees.
func readPendingInvitations(ctx context.Context, tx pgx.Tx, familyID string, now time.Time) ([]InvitationView, error) {
	rows, err := tx.Query(ctx, `SELECT id::text,family_id::text,role::text,status::text,expires_at,created_at
		FROM invitations WHERE family_id=$1::uuid AND status='active' AND expires_at>$2::timestamptz
		ORDER BY created_at DESC LIMIT 50`, familyID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []InvitationView
	for rows.Next() {
		var item InvitationView
		if err := rows.Scan(&item.ID, &item.FamilyID, &item.Role, &item.Status, &item.ExpiresAt, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.ExpiresAt = item.ExpiresAt.UTC()
		item.CreatedAt = item.CreatedAt.UTC()
		result = append(result, item)
	}
	return result, rows.Err()
}
