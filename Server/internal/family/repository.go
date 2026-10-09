package family

import (
	"context"
	"errors"
	"strconv"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/security"
	"babytracker/server/internal/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct{ runtime *pgxpool.Pool }

type FamilyMaterialStage struct {
	CommandID                string
	CommandHash              [32]byte
	ProjectionMaterialDigest [32]byte
	Material                 []byte
	AccountID                string
	Now                      time.Time
}

type invitationLookup struct {
	InvitationView
	FamilyDisplayName, Role string
}

func NewRepository(runtime *pgxpool.Pool) (*Repository, error) {
	if runtime == nil {
		return nil, errors.New("family repository requires runtime database")
	}
	return &Repository{runtime: runtime}, nil
}

func (repository *Repository) StageFamilyMaterial(ctx context.Context, stage FamilyMaterialStage) error {
	if repository == nil || repository.runtime == nil {
		return ErrUnavailable
	}
	tx, err := repository.runtime.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tenant.SetAccountLocal(ctx, tx, stage.AccountID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT app.stage_family_material($1::uuid,$2::bytea,$3::bytea,$4::bytea,$5::timestamptz)`,
		stage.CommandID, stage.CommandHash[:], stage.ProjectionMaterialDigest[:], stage.Material, stage.Now); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "BT001" {
			return ErrProfileNotRequired
		}
		return err
	}
	return tx.Commit(ctx)
}

func (repository *Repository) account(ctx context.Context, authority *security.Service, principal access.Principal) (AccountView, error) {
	var result AccountView
	err := authority.WithAccountTransaction(ctx, principal, func(tx pgx.Tx) error {
		var familyID, slotState *string
		if err := tx.QueryRow(ctx, `SELECT account_id::text,display_name,authority_status,profile_required,family_id::text,slot_state
			FROM app.current_account_profile($1::uuid)`, principal.AccountID).Scan(&result.ID, &result.DisplayName, &result.Status,
			&result.ProfileRequired, &familyID, &slotState); err != nil {
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
func (repository *Repository) updateProfile(ctx context.Context, authority *security.Service, principal access.Principal, commandID, displayName string, now time.Time) error {
	return authority.WithAccountTransaction(ctx, principal, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT app.update_own_profile($1::uuid,$2::uuid,$3::text,$4::timestamptz)`, principal.AccountID, commandID, displayName, now)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "BT002" {
			return ErrCommandIDReused
		}
		return err
	})
}

func (repository *Repository) family(ctx context.Context, authority *security.Service, principal access.Principal, familyID, action string) (FamilyView, error) {
	result := FamilyView{Members: []MemberView{}}
	err := authority.WithAuthorizedTransaction(ctx, principal, familyID, action, func(tx pgx.Tx, _ access.Grant) error {
		if err := tx.QueryRow(ctx, `SELECT id::text,display_name,status,profile_required FROM families WHERE id=$1::uuid`, familyID).
			Scan(&result.ID, &result.DisplayName, &result.Status, &result.ProfileRequired); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT m.account_id::text,l.display_name,m.role,m.status
			FROM memberships m JOIN family_actor_labels l ON l.family_id=m.family_id AND l.account_id=m.account_id
			WHERE m.family_id=$1::uuid AND m.status='active' ORDER BY m.account_id`, familyID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var member MemberView
			if err := rows.Scan(&member.AccountID, &member.DisplayName, &member.Role, &member.Status); err != nil {
				return err
			}
			result.Members = append(result.Members, member)
		}
		return rows.Err()
	})
	return result, err
}

func (repository *Repository) actorLabels(ctx context.Context, authority *security.Service, principal access.Principal, familyID, after, highWater string, limit int) (ActorLabelPage, error) {
	result := ActorLabelPage{Items: []ActorLabelView{}, HighWater: highWater}
	err := authority.WithAuthorizedTransaction(ctx, principal, familyID, "read", func(tx pgx.Tx, _ access.Grant) error {
		if result.HighWater == "" {
			var highWater uint64
			if err := tx.QueryRow(ctx, `SELECT app.family_actor_labels_high_water()`).Scan(&highWater); err != nil {
				return err
			}
			result.HighWater = strconv.FormatUint(highWater, 10)
		}
		highWaterValue, err := strconv.ParseUint(result.HighWater, 10, 64)
		if err != nil {
			return err
		}
		if highWaterValue == 0 {
			return nil
		}
		var afterValue any
		if after != "" {
			afterValue = after
		}
		rows, err := tx.Query(ctx, `SELECT account_id::text,display_name,former,high_water,has_more
			FROM app.list_family_actor_labels($1::uuid,$2::bigint,$3::uuid,$4::integer)`, familyID, highWaterValue, afterValue, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		hasMore := false
		for rows.Next() {
			var item ActorLabelView
			var returnedHighWater uint64
			if err := rows.Scan(&item.AccountID, &item.DisplayName, &item.Former, &returnedHighWater, &hasMore); err != nil {
				return err
			}
			if returnedHighWater != highWaterValue {
				return errors.New("actor label high-water mismatch")
			}
			result.Items = append(result.Items, item)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if hasMore && len(result.Items) > 0 {
			result.NextCursor = result.Items[len(result.Items)-1].AccountID
		}
		return nil
	})
	return result, err
}

func (repository *Repository) lookupInvitation(ctx context.Context, authority *security.Service, principal access.Principal, digest string) (invitationLookup, bool, error) {
	var result invitationLookup
	err := authority.WithAccountTransaction(ctx, principal, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT invitation_id::text,family_id::text,family_display_name,status,role,expires_at
			FROM app.lookup_family_invitation($1::text)`, digest).Scan(&result.ID, &result.FamilyID, &result.FamilyDisplayName,
			&result.Status, &result.Role, &result.ExpiresAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return invitationLookup{}, false, nil
	}
	result.ExpiresAt = result.ExpiresAt.UTC()
	return result, err == nil, err
}

func (repository *Repository) invitation(ctx context.Context, authority *security.Service, principal access.Principal, familyID, invitationID string) (InvitationView, bool, error) {
	var result InvitationView
	err := authority.WithAuthorizedTransaction(ctx, principal, familyID, "manage_members", func(tx pgx.Tx, _ access.Grant) error {
		return tx.QueryRow(ctx, `SELECT id::text,family_id::text,role,status,expires_at,created_at FROM invitations WHERE id=$1::uuid`, invitationID).
			Scan(&result.ID, &result.FamilyID, &result.Role, &result.Status, &result.ExpiresAt, &result.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return InvitationView{}, false, nil
	}
	result.ExpiresAt = result.ExpiresAt.UTC()
	result.CreatedAt = result.CreatedAt.UTC()
	return result, err == nil, err
}

// pendingInvitations lists the family's active, unexpired invitations, newest
// first. "Unexpired" is judged against the caller-supplied clock rather than
// the database's own now(), matching every other expiry decision in this
// package so tests can move time deterministically instead of racing the
// wall clock. The family filter is explicit rather than left to row-level
// security alone, so a policy change can never widen what an owner sees.
func (repository *Repository) pendingInvitations(ctx context.Context, authority *security.Service, principal access.Principal, familyID string, now time.Time) ([]InvitationView, error) {
	var result []InvitationView
	err := authority.WithAuthorizedTransaction(ctx, principal, familyID, "manage_members", func(tx pgx.Tx, _ access.Grant) error {
		rows, err := tx.Query(ctx, `SELECT id::text,family_id::text,role,status,expires_at,created_at
			FROM invitations WHERE family_id=$1::uuid AND status='active' AND expires_at>$2::timestamptz
			ORDER BY created_at DESC LIMIT 50`, familyID, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item InvitationView
			if err := rows.Scan(&item.ID, &item.FamilyID, &item.Role, &item.Status, &item.ExpiresAt, &item.CreatedAt); err != nil {
				return err
			}
			item.ExpiresAt = item.ExpiresAt.UTC()
			item.CreatedAt = item.CreatedAt.UTC()
			result = append(result, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
