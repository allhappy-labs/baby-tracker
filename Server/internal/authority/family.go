package authority

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"babytracker/server/internal/access"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Family commands (security/state.go decideFamilyEffects and the projector's
// family cases). Every command is one transaction:
//
//  1. lock every involved account in ID order (the actor and every member
//     whose membership, slot or sessions the command changes);
//  2. look up the actor's receipt: an exact retry returns the stored result
//     before any other check, a different payload is command_id_reused;
//  3. re-check the caller's session (account active, session active and
//     current, device active, before its deadline);
//  4. read the actor's slot, then lock the family;
//  5. check, write state, append the family's and the affected accounts'
//     audit rows from their locked heads, write the receipt, commit.
//
// Sessions are touched only after the family lock (accounts → families →
// sessions). Because each affected member's account is locked first, a
// concurrent Refresh of that member (which locks the account, then reads the
// slot and membership) either commits before the command, whose UPDATE then
// covers its child, or runs after it and sees the new membership.

// CreateFamily creates a family owned by OwnerAccountID (family_created; on a
// node that requires the family_sync entitlement, family_bootstrapped with a
// hosted_beta grant expiring 366 days after the command's whole second).
type CreateFamily struct {
	CommandID, OwnerAccountID, FamilyID, DisplayName string
	// At is the command time; zero means the service clock after the locks.
	At        time.Time
	Principal access.Principal
}

// FamilyResult is a command result that names the family it created or joined.
type FamilyResult struct {
	Result
	FamilyID string
}

// RepairFamilyProfile names the owner's family when its profile is required.
type RepairFamilyProfile struct {
	CommandID, OwnerAccountID, DisplayName string
	At                                     time.Time
	Principal                              access.Principal
}

// CreateInvitation stores an invitation the caller minted. TokenDigest is the
// keyed digest of the invitation token (hmac-sha256:<key id>:<hex>).
type CreateInvitation struct {
	CommandID, CreatorAccountID, InvitationID, FamilyID, TokenDigest, Role string
	ExpiresAt, At                                                          time.Time
	Principal                                                              access.Principal
}

// InvitationResult is CreateInvitation's result. SecretUnavailable marks an
// exact retry: Invitation is the stored invitation, whose token only the first
// attempt's caller held (family/service.go Invite replay).
type InvitationResult struct {
	Result
	Invitation        InvitationView
	SecretUnavailable bool
}

// RedeemInvitation joins the invitation whose token digest is one of
// TokenDigestCandidates (one per configured invitation key).
type RedeemInvitation struct {
	CommandID, RedeemerAccountID string
	TokenDigestCandidates        []string
	At                           time.Time
	Principal                    access.Principal
}

// RevokeInvitation withdraws an active invitation of the actor's family.
type RevokeInvitation struct {
	CommandID, ActorAccountID, InvitationID string
	At                                      time.Time
	Principal                               access.Principal
}

// LeaveFamily removes the caller from their family (member_left).
type LeaveFamily struct {
	CommandID, AccountID string
	At                   time.Time
	Principal            access.Principal
}

// RemoveMember removes AccountID from the owner's family (member_removed).
type RemoveMember struct {
	CommandID, OwnerAccountID, AccountID string
	At                                   time.Time
	Principal                            access.Principal
}

// ChangeRole moves a member of the owner's family between caregiver and viewer.
type ChangeRole struct {
	CommandID, OwnerAccountID, AccountID, Role string
	At                                         time.Time
	Principal                                  access.Principal
}

// TransferOwner hands the old owner's family to an active caregiver.
type TransferOwner struct {
	CommandID, OldOwnerID, NewOwnerID string
	At                                time.Time
	Principal                         access.Principal
}

// InvitationView is an invitation row. LookupInvitation, an invitee's preview
// that crosses families, fills only FamilyDisplayName, Status and ExpiresAt.
type InvitationView struct {
	ID, FamilyID, CreatorAccountID, TokenDigest, Role, Status string
	ExpiresAt, CreatedAt                                      time.Time
	FamilyDisplayName                                         string
}

// formerCaregiverLabel is the label a member keeps after leaving.
const formerCaregiverLabel = "Former caregiver"

// hostedEntitlementLifetime is the hosted bootstrap grant (state.go:459).
const hostedEntitlementLifetime = 366 * 24 * time.Hour

const maxInvitationCandidates = 8

// CreateFamily creates the owner's family (state.go:427-462; projector
// family_created/family_bootstrapped). Preconditions: a live session; the
// owner has no family slot (family_slot_occupied, also enforced by the
// family_slots primary key); the family ID is unused (invalid_command).
// Locks: the owner's account. Audit: family_created (or family_bootstrapped)
// on the new family and on the owner's account.
func (s *Service) CreateFamily(ctx context.Context, in CreateFamily) (FamilyResult, error) {
	if !validID(in.CommandID) || !validID(in.OwnerAccountID) || !validID(in.FamilyID) || !validFamilyName(in.DisplayName) || !validCommandTime(in.At) {
		return FamilyResult{}, ErrInvalidCommand
	}
	if err := commandPrincipal(in.Principal, in.OwnerAccountID); err != nil {
		return FamilyResult{}, err
	}
	kind, bootstrap := KindFamilyCreated, s.requireFamilySyncEntitlement
	if bootstrap {
		kind = KindFamilyBootstrapped
	}
	result, _, err := s.familyCommand(ctx, commandSpec{principal: in.Principal, actor: in.OwnerAccountID, commandID: in.CommandID, kind: kind, at: in.At,
		accounts: []string{in.OwnerAccountID}, payload: map[string]any{"family_id": in.FamilyID, "display_name": in.DisplayName}, wholeSecond: bootstrap},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			if slot, err := readSlot(ctx, c.tx, in.OwnerAccountID); err != nil || slot != nil {
				return Result{}, "", errOr(err, ErrFamilySlotOccupied)
			}
			var exists bool
			if err := c.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM families WHERE id=$1)`, in.FamilyID).Scan(&exists); err != nil {
				return Result{}, "", unavailable("read family", err)
			}
			if exists {
				return Result{}, "", ErrInvalidCommand
			}
			steps := []batchStep{
				{`INSERT INTO families(id,display_name,status,owner_account_id,profile_required,entitlement_required,created_at,updated_at)
					VALUES ($1,$2,'active',$3,false,$4,$5,$5)`, []any{in.FamilyID, in.DisplayName, in.OwnerAccountID, bootstrap, c.now}, 1},
				{`INSERT INTO memberships(family_id,account_id,role,status,created_at,updated_at) VALUES ($1,$2,'owner','active',$3,$3)`,
					[]any{in.FamilyID, in.OwnerAccountID, c.now}, 1},
				{`INSERT INTO family_slots(account_id,family_id,state) VALUES ($1,$2,'active')`, []any{in.OwnerAccountID, in.FamilyID}, 1},
			}
			audit := map[string]any{"family_id": in.FamilyID, "owner_account_id": in.OwnerAccountID}
			if bootstrap {
				expires := c.now.Add(hostedEntitlementLifetime)
				steps = append(steps, batchStep{`INSERT INTO family_entitlements(family_id,entitlement_key,source,status,expires_at,created_at,updated_at)
					VALUES ($1,'family_sync','hosted_beta','active',$2,$3,$3)`, []any{in.FamilyID, expires, c.now}, 1})
				audit["entitlement_key"], audit["entitlement_source"], audit["entitlement_expires_at"] = "family_sync", "hosted_beta", timeText(expires)
			}
			steps = append(steps, actorLabelStep(in.FamilyID, in.OwnerAccountID, false, c.now))
			if err := familyBatch(ctx, c.tx, steps); err != nil {
				return Result{}, "", err
			}
			head, err := newChainHead(familyScope(in.FamilyID), 0, make([]byte, 32), false)
			if err != nil {
				return Result{}, "", err
			}
			return c.audit(ctx, in.FamilyID, []*chainHead{head, c.accounts[in.OwnerAccountID].Chain}, audit)
		})
	return FamilyResult{Result: result, FamilyID: in.FamilyID}, err
}

// RepairFamilyProfile names a family whose profile is required
// (family/service.go RepairProfile, state.go:463-476, projector
// family_profile_repaired). Preconditions: the owner's slot is active
// (family_unavailable); owner (owner_required); entitled
// (entitlement_required); the profile is required (profile_not_required).
// Locks: the owner's account, the family. Audit: family_profile_repaired on
// the family.
func (s *Service) RepairFamilyProfile(ctx context.Context, in RepairFamilyProfile) (Result, error) {
	if !validID(in.CommandID) || !validID(in.OwnerAccountID) || !validFamilyName(in.DisplayName) || !validCommandTime(in.At) {
		return Result{}, ErrInvalidCommand
	}
	if err := commandPrincipal(in.Principal, in.OwnerAccountID); err != nil {
		return Result{}, err
	}
	result, _, err := s.familyCommand(ctx, commandSpec{principal: in.Principal, actor: in.OwnerAccountID, commandID: in.CommandID, kind: KindFamilyProfileRepaired,
		at: in.At, accounts: []string{in.OwnerAccountID}, payload: map[string]any{"display_name": in.DisplayName}},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			family, err := c.ownedFamily(ctx, in.OwnerAccountID, true)
			if err != nil {
				return Result{}, "", err
			}
			if !family.ProfileRequired {
				return Result{}, "", ErrProfileNotRequired
			}
			if err := familyBatch(ctx, c.tx, []batchStep{{`UPDATE families SET display_name=$2, profile_required=false, updated_at=$3
				WHERE id=$1 AND status='active' AND profile_required`, []any{family.ID, in.DisplayName, c.now}, 1}}); err != nil {
				return Result{}, "", err
			}
			return c.audit(ctx, family.ID, []*chainHead{family.Chain}, map[string]any{"family_id": family.ID})
		})
	return result, err
}

// CreateInvitation stores an invitation of FamilyID (family/service.go
// Invite, state.go:477-505, projector invitation_created). Preconditions:
// role caregiver or viewer and an expiry after the command time
// (invalid_command); owner (owner_required); entitled (entitlement_required);
// fewer than MaxInvitations active unexpired invitations (invitation_limit);
// the invitation ID and token digest are unused (invalid_command). An exact
// retry returns the stored result and invitation with SecretUnavailable; the
// command hash covers the invitation ID, family and role, not the freshly
// minted digest or expiry. Locks: the creator's account, the family. Audit:
// invitation_created {invitation_id, role, expires_at} on the family.
func (s *Service) CreateInvitation(ctx context.Context, in CreateInvitation) (InvitationResult, error) {
	if !validID(in.CommandID) || !validID(in.CreatorAccountID) || !validID(in.InvitationID) || !validID(in.FamilyID) ||
		!identityKeyPattern.MatchString(in.TokenDigest) || (in.Role != "caregiver" && in.Role != "viewer") || !validTime(in.ExpiresAt) || !validCommandTime(in.At) {
		return InvitationResult{}, ErrInvalidCommand
	}
	if err := commandPrincipal(in.Principal, in.CreatorAccountID); err != nil {
		return InvitationResult{}, err
	}
	var view InvitationView
	result, replayed, err := s.familyCommand(ctx, commandSpec{principal: in.Principal, actor: in.CreatorAccountID, commandID: in.CommandID,
		kind: KindInvitationCreated, at: in.At, accounts: []string{in.CreatorAccountID},
		payload: map[string]any{"invitation_id": in.InvitationID, "family_id": in.FamilyID, "role": in.Role},
		replay: func(ctx context.Context, tx pgx.Tx) error {
			stored, found, err := readInvitation(ctx, tx, in.FamilyID, in.InvitationID)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("authority: receipt for invitation %s without its row: %w", in.InvitationID, ErrUnavailable)
			}
			view = stored
			return nil
		}},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			expires := in.ExpiresAt.UTC().Truncate(time.Microsecond)
			if !c.now.Before(expires) {
				return Result{}, "", ErrInvalidCommand
			}
			family, err := c.ownerFamily(ctx, in.FamilyID, in.CreatorAccountID, true)
			if err != nil {
				return Result{}, "", err
			}
			var active int
			var taken bool
			if err := c.tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM invitations WHERE family_id=$1 AND status='active' AND expires_at>$2),
					EXISTS (SELECT 1 FROM invitations WHERE id=$3 OR token_digest=$4)`, family.ID, c.now, in.InvitationID, in.TokenDigest).Scan(&active, &taken); err != nil {
				return Result{}, "", unavailable("count invitations", err)
			}
			if active >= c.s.maxInvitations {
				return Result{}, "", ErrInvitationLimit
			}
			if taken {
				return Result{}, "", ErrInvalidCommand
			}
			if err := familyBatch(ctx, c.tx, []batchStep{{`INSERT INTO invitations(id,family_id,creator_account_id,token_digest,role,status,expires_at,created_at,updated_at)
				VALUES ($1,$2,$3,$4,$5::text::app.member_role,'active',$6,$7,$7)`,
				[]any{in.InvitationID, family.ID, in.CreatorAccountID, in.TokenDigest, in.Role, expires, c.now}, 1}}); err != nil {
				return Result{}, "", err
			}
			view = InvitationView{ID: in.InvitationID, FamilyID: family.ID, CreatorAccountID: in.CreatorAccountID, TokenDigest: in.TokenDigest, Role: in.Role,
				Status: "active", ExpiresAt: expires, CreatedAt: c.now}
			return c.audit(ctx, family.ID, []*chainHead{family.Chain},
				map[string]any{"invitation_id": in.InvitationID, "role": in.Role, "expires_at": timeText(expires)})
		})
	if err != nil {
		return InvitationResult{}, err
	}
	return InvitationResult{Result: result, Invitation: view, SecretUnavailable: replayed}, nil
}

// RedeemInvitation joins the redeemer to the invitation's family
// (family/service.go Redeem, state.go:506-543, projector
// invitation_redeemed). The invitation is resolved from the candidates
// first (more than one match is authorization_quarantine); the command hash
// covers the resolved invitation ID. Preconditions, in legacy order: the
// redeemer has no slot (family_slot_occupied); the invitation exists
// (invitation_invalid), is active (invitation_consumed / invitation_revoked)
// and unexpired (invitation_expired); the family is active
// (family_unavailable); it has fewer than MaxCaregivers active members
// (caregiver_limit); it is entitled (entitlement_required). The redeemer's
// sessions are not touched: their scope upgrades at the next refresh. Locks:
// the redeemer's account, the family. Audit: invitation_redeemed on the
// family and the redeemer's account.
func (s *Service) RedeemInvitation(ctx context.Context, in RedeemInvitation) (FamilyResult, error) {
	if !validID(in.CommandID) || !validID(in.RedeemerAccountID) || !validInvitationCandidates(in.TokenDigestCandidates) || !validCommandTime(in.At) {
		return FamilyResult{}, ErrInvalidCommand
	}
	if err := commandPrincipal(in.Principal, in.RedeemerAccountID); err != nil {
		return FamilyResult{}, err
	}
	var invitationID, familyID string
	result, _, err := s.familyCommand(ctx, commandSpec{principal: in.Principal, actor: in.RedeemerAccountID, commandID: in.CommandID,
		kind: KindInvitationRedeemed, at: in.At, accounts: []string{in.RedeemerAccountID},
		resolve: func(ctx context.Context, tx pgx.Tx) (any, error) {
			rows, err := tx.Query(ctx, `SELECT id::text, family_id::text FROM invitations WHERE token_digest = ANY($1::text[])`, in.TokenDigestCandidates)
			if err != nil {
				return nil, unavailable("resolve invitation", err)
			}
			matches, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
				var out [2]string
				err := row.Scan(&out[0], &out[1])
				return out, err
			})
			if err != nil {
				return nil, unavailable("resolve invitation", err)
			}
			if len(matches) > 1 {
				return nil, ErrAuthorizationQuarantine
			}
			if len(matches) == 1 {
				invitationID, familyID = matches[0][0], matches[0][1]
			}
			return map[string]any{"invitation_id": invitationID}, nil
		}},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			if slot, err := readSlot(ctx, c.tx, in.RedeemerAccountID); err != nil || slot != nil {
				return Result{}, "", errOr(err, ErrFamilySlotOccupied)
			}
			if invitationID == "" {
				return Result{}, "", ErrInvitationInvalid
			}
			families, err := lockFamilies(ctx, c.tx, familyID)
			if err != nil {
				return Result{}, "", err
			}
			family, familyOK := families[familyID]
			var status, role string
			var expires time.Time
			if err := c.tx.QueryRow(ctx, `SELECT status::text, role::text, expires_at FROM invitations WHERE id=$1`, invitationID).Scan(&status, &role, &expires); err != nil {
				return Result{}, "", unavailable("read invitation", err)
			}
			if err := invitationStatusError(status); err != nil {
				return Result{}, "", err
			}
			if !c.now.Before(expires) {
				return Result{}, "", ErrInvitationExpired
			}
			if !familyOK || family.Status != "active" {
				return Result{}, "", ErrFamilyUnavailable
			}
			var members int
			if err := c.tx.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE family_id=$1 AND status='active'`, familyID).Scan(&members); err != nil {
				return Result{}, "", unavailable("count members", err)
			}
			if members >= c.s.maxCaregivers {
				return Result{}, "", ErrCaregiverLimit
			}
			if err := c.requireEntitlement(ctx, family); err != nil {
				return Result{}, "", err
			}
			// A removed member rejoins on their old row (000016 membership upsert).
			if err := familyBatch(ctx, c.tx, []batchStep{
				{`UPDATE invitations SET status='redeemed', redeemed_at=$2, updated_at=$2 WHERE id=$1 AND status='active'`, []any{invitationID, c.now}, 1},
				{`INSERT INTO memberships(family_id,account_id,role,status,created_at,updated_at) VALUES ($1,$2,$3::text::app.member_role,'active',$4,$4)
					ON CONFLICT (family_id,account_id) DO UPDATE SET role=EXCLUDED.role, status='active', updated_at=EXCLUDED.updated_at
					WHERE memberships.status<>'active' AND memberships.role<>'owner'`, []any{familyID, in.RedeemerAccountID, role, c.now}, 1},
				{`INSERT INTO family_slots(account_id,family_id,state) VALUES ($1,$2,'active')`, []any{in.RedeemerAccountID, familyID}, 1},
				actorLabelStep(familyID, in.RedeemerAccountID, false, c.now),
			}); err != nil {
				return Result{}, "", err
			}
			return c.audit(ctx, familyID, []*chainHead{family.Chain, c.accounts[in.RedeemerAccountID].Chain},
				map[string]any{"invitation_id": invitationID, "account_id": in.RedeemerAccountID, "role": role})
		})
	if err != nil {
		return FamilyResult{}, err
	}
	return FamilyResult{Result: result, FamilyID: familyID}, nil
}

// RevokeInvitation revokes an invitation of the actor's family
// (family/service.go RevokeInvitation, state.go:544-571, projector
// invitation_revoked). Preconditions: the actor's slot is active
// (family_unavailable); the invitation belongs to that family
// (invitation_invalid); owner (owner_required); entitled
// (entitlement_required); the invitation is active (invitation_consumed /
// invitation_revoked). Locks: the actor's account, the family. Audit:
// invitation_revoked on the family.
func (s *Service) RevokeInvitation(ctx context.Context, in RevokeInvitation) (Result, error) {
	if !validID(in.CommandID) || !validID(in.ActorAccountID) || !validID(in.InvitationID) || !validCommandTime(in.At) {
		return Result{}, ErrInvalidCommand
	}
	if err := commandPrincipal(in.Principal, in.ActorAccountID); err != nil {
		return Result{}, err
	}
	result, _, err := s.familyCommand(ctx, commandSpec{principal: in.Principal, actor: in.ActorAccountID, commandID: in.CommandID, kind: KindInvitationRevoked,
		at: in.At, accounts: []string{in.ActorAccountID}, payload: map[string]any{"invitation_id": in.InvitationID}},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			slot, err := activeSlot(ctx, c.tx, in.ActorAccountID)
			if err != nil {
				return Result{}, "", err
			}
			var invitationFamily string
			err = c.tx.QueryRow(ctx, `SELECT family_id::text FROM invitations WHERE id=$1`, in.InvitationID).Scan(&invitationFamily)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && invitationFamily != slot.FamilyID) {
				return Result{}, "", ErrInvitationInvalid
			}
			if err != nil {
				return Result{}, "", unavailable("read invitation", err)
			}
			family, err := c.ownerFamily(ctx, slot.FamilyID, in.ActorAccountID, true)
			if err != nil {
				return Result{}, "", err
			}
			var status string
			if err := c.tx.QueryRow(ctx, `SELECT status::text FROM invitations WHERE id=$1`, in.InvitationID).Scan(&status); err != nil {
				return Result{}, "", unavailable("read invitation", err)
			}
			if err := invitationStatusError(status); err != nil {
				return Result{}, "", err
			}
			if err := familyBatch(ctx, c.tx, []batchStep{{`UPDATE invitations SET status='revoked', revoked_at=$2, updated_at=$2 WHERE id=$1 AND status='active'`,
				[]any{in.InvitationID, c.now}, 1}}); err != nil {
				return Result{}, "", err
			}
			return c.audit(ctx, family.ID, []*chainHead{family.Chain}, map[string]any{"invitation_id": in.InvitationID})
		})
	return result, err
}

// LeaveFamily removes the caller from their family (family/service.go Leave,
// state.go:572-593 member_left). Preconditions: an active slot
// (family_unavailable); an active membership (membership_revoked); not the
// owner (owner_required). Effects as RemoveMember. Locks: the account, the
// family. Audit: member_left on the family and the account.
func (s *Service) LeaveFamily(ctx context.Context, in LeaveFamily) (Result, error) {
	if !validID(in.CommandID) || !validID(in.AccountID) || !validCommandTime(in.At) {
		return Result{}, ErrInvalidCommand
	}
	if err := commandPrincipal(in.Principal, in.AccountID); err != nil {
		return Result{}, err
	}
	result, _, err := s.familyCommand(ctx, commandSpec{principal: in.Principal, actor: in.AccountID, commandID: in.CommandID, kind: KindMemberLeft, at: in.At,
		accounts: []string{in.AccountID}, payload: map[string]any{"account_id": in.AccountID}},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			slot, err := activeSlot(ctx, c.tx, in.AccountID)
			if err != nil {
				return Result{}, "", err
			}
			families, err := lockFamilies(ctx, c.tx, slot.FamilyID)
			if err != nil {
				return Result{}, "", err
			}
			family, ok := families[slot.FamilyID]
			if !ok {
				return Result{}, "", ErrMembershipRevoked
			}
			return c.removeMember(ctx, family, in.AccountID)
		})
	return result, err
}

// RemoveMember removes a member from the owner's family (family/service.go
// Remove, state.go:572-593 member_removed, event_handlers.go:627-638).
// Preconditions: the owner's slot is active (family_unavailable); owner
// (owner_required); entitled (entitlement_required); the target's membership
// is active (membership_revoked) and not the owner's (owner_required).
// Effects: the membership is removed, the target's slot deleted, every active
// session of the target revoked (member_removed), the target's label turned
// into 'Former caregiver'. Locks: the owner's and the target's accounts, the
// family, then the target's sessions. Audit: member_removed on the family and
// the target's account.
func (s *Service) RemoveMember(ctx context.Context, in RemoveMember) (Result, error) {
	if !validID(in.CommandID) || !validID(in.OwnerAccountID) || !validID(in.AccountID) || !validCommandTime(in.At) {
		return Result{}, ErrInvalidCommand
	}
	if err := commandPrincipal(in.Principal, in.OwnerAccountID); err != nil {
		return Result{}, err
	}
	result, _, err := s.familyCommand(ctx, commandSpec{principal: in.Principal, actor: in.OwnerAccountID, commandID: in.CommandID, kind: KindMemberRemoved,
		at: in.At, accounts: []string{in.OwnerAccountID, in.AccountID}, payload: map[string]any{"account_id": in.AccountID}},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			family, err := c.ownedFamily(ctx, in.OwnerAccountID, true)
			if err != nil {
				return Result{}, "", err
			}
			return c.removeMember(ctx, family, in.AccountID)
		})
	return result, err
}

// ChangeRole moves a member between caregiver and viewer (family/service.go
// ChangeRole, state.go:594-619, 000015 projector). Preconditions: role
// caregiver or viewer (invalid_command); the owner's slot is active
// (family_unavailable); owner (owner_required); not the owner's own row
// (owner_required); entitled (entitlement_required); the target is an active
// non-owner member (membership_revoked). Effects: the membership role and the
// role of the target's active sessions of the family. Locks: the owner's and
// the target's accounts, the family, then the target's sessions. Audit:
// member_role_changed on the family and the target's account.
func (s *Service) ChangeRole(ctx context.Context, in ChangeRole) (Result, error) {
	if !validID(in.CommandID) || !validID(in.OwnerAccountID) || !validID(in.AccountID) || (in.Role != "caregiver" && in.Role != "viewer") ||
		!validCommandTime(in.At) {
		return Result{}, ErrInvalidCommand
	}
	if err := commandPrincipal(in.Principal, in.OwnerAccountID); err != nil {
		return Result{}, err
	}
	result, _, err := s.familyCommand(ctx, commandSpec{principal: in.Principal, actor: in.OwnerAccountID, commandID: in.CommandID, kind: KindMemberRoleChanged,
		at: in.At, accounts: []string{in.OwnerAccountID, in.AccountID}, payload: map[string]any{"account_id": in.AccountID, "role": in.Role}},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			family, err := c.ownedFamily(ctx, in.OwnerAccountID, false)
			if err != nil {
				return Result{}, "", err
			}
			// An owner cannot demote themselves: stepping down is a transfer.
			if in.AccountID == in.OwnerAccountID {
				return Result{}, "", ErrOwnerRequired
			}
			if err := c.requireEntitlement(ctx, family); err != nil {
				return Result{}, "", err
			}
			role, status, err := readMembership(ctx, c.tx, family.ID, in.AccountID)
			if err != nil {
				return Result{}, "", err
			}
			if status != "active" || role == "owner" {
				return Result{}, "", ErrMembershipRevoked
			}
			if err := familyBatch(ctx, c.tx, []batchStep{
				{`UPDATE memberships SET role=$3::text::app.member_role, updated_at=$4 WHERE family_id=$1 AND account_id=$2 AND status='active' AND role<>'owner'`,
					[]any{family.ID, in.AccountID, in.Role, c.now}, 1},
				{`UPDATE sessions SET role=$3::text::app.member_role WHERE account_id=$2 AND family_id=$1 AND status='active'`,
					[]any{family.ID, in.AccountID, in.Role}, -1},
			}); err != nil {
				return Result{}, "", err
			}
			c.invalidate(in.AccountID, family.ID)
			return c.audit(ctx, family.ID, []*chainHead{family.Chain, c.accounts[in.AccountID].Chain},
				map[string]any{"family_id": family.ID, "account_id": in.AccountID, "role": in.Role})
		})
	return result, err
}

// TransferOwner makes an active caregiver the owner and the old owner a
// caregiver (family/service.go Transfer, state.go:620-640, projector
// owner_transferred). Preconditions: a different new owner
// (invalid_command); the old owner's slot is active (family_unavailable);
// owner (owner_required); entitled (entitlement_required); the new owner is
// an active caregiver with an active account (membership_revoked). Effects:
// the old owner is demoted before the new one is promoted (one active owner
// per family is a non-deferred unique index), families.owner_account_id, and
// the role of both accounts' active sessions of the family. Locks: both
// accounts, the family, then their sessions. Audit: owner_transferred on the
// family and both accounts.
func (s *Service) TransferOwner(ctx context.Context, in TransferOwner) (Result, error) {
	if !validID(in.CommandID) || !validID(in.OldOwnerID) || !validID(in.NewOwnerID) || in.OldOwnerID == in.NewOwnerID || !validCommandTime(in.At) {
		return Result{}, ErrInvalidCommand
	}
	if err := commandPrincipal(in.Principal, in.OldOwnerID); err != nil {
		return Result{}, err
	}
	result, _, err := s.familyCommand(ctx, commandSpec{principal: in.Principal, actor: in.OldOwnerID, commandID: in.CommandID, kind: KindOwnerTransferred,
		at: in.At, accounts: []string{in.OldOwnerID, in.NewOwnerID}, payload: map[string]any{"new_owner_account_id": in.NewOwnerID}},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			family, err := c.ownedFamily(ctx, in.OldOwnerID, true)
			if err != nil {
				return Result{}, "", err
			}
			role, status, err := readMembership(ctx, c.tx, family.ID, in.NewOwnerID)
			if err != nil {
				return Result{}, "", err
			}
			if next, ok := c.accounts[in.NewOwnerID]; status != "active" || role != "caregiver" || !ok || next.Status != "active" {
				return Result{}, "", ErrMembershipRevoked
			}
			if err := familyBatch(ctx, c.tx, []batchStep{
				{`UPDATE memberships SET role='caregiver', updated_at=$3 WHERE family_id=$1 AND account_id=$2 AND role='owner' AND status='active'`,
					[]any{family.ID, in.OldOwnerID, c.now}, 1},
				{`UPDATE memberships SET role='owner', updated_at=$3 WHERE family_id=$1 AND account_id=$2 AND role='caregiver' AND status='active'`,
					[]any{family.ID, in.NewOwnerID, c.now}, 1},
				{`UPDATE families SET owner_account_id=$3, updated_at=$4 WHERE id=$1 AND owner_account_id=$2 AND status='active'`,
					[]any{family.ID, in.OldOwnerID, in.NewOwnerID, c.now}, 1},
				{`UPDATE sessions SET role='caregiver' WHERE account_id=$2 AND family_id=$1 AND status='active'`, []any{family.ID, in.OldOwnerID}, -1},
				{`UPDATE sessions SET role='owner' WHERE account_id=$2 AND family_id=$1 AND status='active'`, []any{family.ID, in.NewOwnerID}, -1},
			}); err != nil {
				return Result{}, "", err
			}
			c.invalidate(in.OldOwnerID, family.ID)
			c.invalidate(in.NewOwnerID, family.ID)
			return c.audit(ctx, family.ID, []*chainHead{family.Chain, c.accounts[in.OldOwnerID].Chain, c.accounts[in.NewOwnerID].Chain},
				map[string]any{"family_id": family.ID, "old_owner_account_id": in.OldOwnerID, "new_owner_account_id": in.NewOwnerID})
		})
	return result, err
}

// FamilySlot returns accountID's family slot, or nil when it has none.
func (s *Service) FamilySlot(ctx context.Context, accountID string) (*FamilySlot, error) {
	if !validID(accountID) {
		return nil, ErrInvalidCommand
	}
	var slot *FamilySlot
	err := s.read(ctx, func(tx pgx.Tx) error {
		var err error
		slot, err = readSlot(ctx, tx, accountID)
		return err
	})
	return slot, err
}

// Invitation returns invitationID if it belongs to familyID.
func (s *Service) Invitation(ctx context.Context, familyID, invitationID string) (InvitationView, bool, error) {
	if !validID(familyID) || !validID(invitationID) {
		return InvitationView{}, false, ErrInvalidCommand
	}
	var view InvitationView
	var found bool
	err := s.read(ctx, func(tx pgx.Tx) error {
		var err error
		view, found, err = readInvitation(ctx, tx, familyID, invitationID)
		return err
	})
	return view, found, err
}

// LookupInvitation is an invitee's preview by token digest candidates
// (000006 app.lookup_family_invitation): only an invitation of an active
// family is found, and only the family's display name, the invitation's
// status and its expiry are returned. More than one match is
// authorization_quarantine (family/service.go lookupInvitation).
func (s *Service) LookupInvitation(ctx context.Context, digests []string) (InvitationView, bool, error) {
	if !validInvitationCandidates(digests) {
		return InvitationView{}, false, ErrInvitationInvalid
	}
	var views []InvitationView
	err := s.read(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT f.display_name, i.status::text, i.expires_at FROM invitations i JOIN families f ON f.id=i.family_id
			WHERE i.token_digest = ANY($1::text[]) AND f.status='active'`, digests)
		if err != nil {
			return unavailable("look up invitation", err)
		}
		views, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (InvitationView, error) {
			var v InvitationView
			err := row.Scan(&v.FamilyDisplayName, &v.Status, &v.ExpiresAt)
			v.ExpiresAt = v.ExpiresAt.UTC()
			return v, err
		})
		if err != nil {
			return unavailable("look up invitation", err)
		}
		return nil
	})
	switch {
	case err != nil:
		return InvitationView{}, false, err
	case len(views) > 1:
		return InvitationView{}, false, ErrAuthorizationQuarantine
	case len(views) == 0:
		return InvitationView{}, false, nil
	}
	return views[0], true, nil
}

// commandSpec describes one family command for familyCommand.
type commandSpec struct {
	principal        access.Principal
	actor, commandID string
	kind             Kind
	at               time.Time
	// accounts are locked in ID order before anything else, together with
	// the accounts lockSet reads (unlocked, in the same transaction) when it
	// is set: a command whose affected members are only known from the
	// database reads them first and re-checks them under its family lock.
	accounts []string
	lockSet  func(context.Context, pgx.Tx) ([]string, error)
	// families are locked in ID order after the accounts and before the
	// receipt lookup (system commands, whose actor has no account row to
	// serialize their retries on).
	families []string
	// system marks a command of a server worker: no principal and no session
	// re-check.
	system bool
	// payload is the command hash payload; resolve, if set, computes it
	// under the account locks instead.
	payload any
	resolve func(context.Context, pgx.Tx) (any, error)
	// replay, if set, runs on an exact retry inside the transaction.
	replay func(context.Context, pgx.Tx) error
	// wholeSecond truncates the command time to the second (hosted bootstrap).
	wholeSecond bool
}

// familyTx is one family command's transaction after its account locks.
type familyTx struct {
	s        *Service
	tx       pgx.Tx
	accounts map[string]accountRow
	// families are spec.families as locked.
	families map[string]familyRow
	// session is the caller's session as re-checked (zero for system commands).
	session SessionRow
	now     time.Time
	spec    commandSpec
	// receiptFamilies are recorded on the receipt besides the family body
	// returns.
	receiptFamilies []string
	// invalidations are published after the commit, at the result's revision.
	invalidations []SecurityInvalidation
}

// invalidate records that the command reduces accountID's authority in
// familyID (either may be empty: all accounts of the family, or the account
// everywhere). familyCommand publishes it once the transaction committed.
func (c *familyTx) invalidate(accountID, familyID string) {
	c.invalidations = append(c.invalidations, SecurityInvalidation{AccountID: accountID, FamilyID: familyID})
}

// familyCommand runs body as spec's command and reports whether the result
// is a stored receipt. body returns the result and the family it changed.
func (s *Service) familyCommand(ctx context.Context, spec commandSpec, body func(context.Context, *familyTx) (Result, string, error)) (Result, bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Result{}, false, errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	lockIDs := spec.accounts
	if spec.lockSet != nil {
		extra, err := spec.lockSet(ctx, tx)
		if err != nil {
			return Result{}, false, err
		}
		lockIDs = append(append([]string(nil), lockIDs...), extra...)
	}
	accounts, err := lockAccounts(ctx, tx, lockIDs...)
	if err != nil {
		return Result{}, false, err
	}
	families, err := lockFamilies(ctx, tx, spec.families...)
	if err != nil {
		return Result{}, false, err
	}
	payload := spec.payload
	if spec.resolve != nil {
		if payload, err = spec.resolve(ctx, tx); err != nil {
			return Result{}, false, err
		}
	}
	hash, err := CanonicalCommandHash(spec.actor, spec.kind, payload)
	if err != nil {
		return Result{}, false, err
	}
	stored, found, err := lookupReceipt(ctx, tx, spec.actor, spec.commandID, hash)
	if err != nil {
		return Result{}, false, err
	}
	if found {
		if spec.replay != nil {
			if err := spec.replay(ctx, tx); err != nil {
				return Result{}, false, err
			}
		}
		return stored, true, nil
	}
	now := s.at(spec.at)
	if spec.wholeSecond {
		now = now.Truncate(time.Second)
	}
	c := &familyTx{s: s, tx: tx, accounts: accounts, families: families, now: now, spec: spec}
	if !spec.system {
		if c.session, err = s.checkCommandSession(ctx, tx, spec.principal, accounts, now); err != nil {
			return Result{}, false, err
		}
	}
	result, familyID, err := body(ctx, c)
	if err != nil {
		return Result{}, false, err
	}
	receiptFamilies := c.receiptFamilies
	if familyID != "" {
		receiptFamilies = append(receiptFamilies, familyID)
	}
	if err := writeReceipt(ctx, tx, spec.actor, spec.commandID, hash, spec.kind, result, receiptFamilies, now); err != nil {
		return Result{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, false, fmt.Errorf("authority: commit %s: %w", spec.kind, errors.Join(err, ErrUnavailable))
	}
	for i := range c.invalidations {
		c.invalidations[i].Revision = result.Revision
	}
	s.publish(c.invalidations...)
	return result, false, nil
}

// commandPrincipal checks a family command's principal before its
// transaction: it must be well formed (session_revoked) and be the actor
// (invalid_command).
func commandPrincipal(p access.Principal, actor string) error {
	if !validPrincipal(p) {
		return ErrSessionRevoked
	}
	if p.AccountID != actor {
		return ErrInvalidCommand
	}
	return nil
}

// checkCommandSession re-checks the caller's session inside the command's
// transaction (security/state.go validFamilyPrincipal) and returns it: the
// epoch is the node's (epoch_changed); the account is active; the session is
// active, belongs to the principal's device and was issued at its revision;
// the device is active; the command time is before the session's deadline
// (session_revoked otherwise).
func (s *Service) checkCommandSession(ctx context.Context, tx pgx.Tx, p access.Principal, accounts map[string]accountRow, now time.Time) (SessionRow, error) {
	if p.RecoveryEpoch != s.epoch {
		return SessionRow{}, ErrEpochChanged
	}
	account, ok := accounts[p.AccountID]
	session, deviceStatus, err := principalSession(ctx, tx, p, account, ok, "")
	if err != nil {
		return SessionRow{}, err
	}
	if session.Status != "active" || deviceStatus != "active" || !now.Before(session.DeadlineAt) {
		return SessionRow{}, ErrSessionRevoked
	}
	return session, nil
}

// ownedFamily locks the family of actor's active slot (family_unavailable
// without one) and requires actor to own it (owner_required) and, if
// entitled, the family to be entitled (entitlement_required).
func (c *familyTx) ownedFamily(ctx context.Context, actor string, entitled bool) (familyRow, error) {
	slot, err := activeSlot(ctx, c.tx, actor)
	if err != nil {
		return familyRow{}, err
	}
	return c.ownerFamily(ctx, slot.FamilyID, actor, entitled)
}

// ownerFamily locks familyID and requires actor to own it (state.go:384-388:
// an active family whose owner is actor, with actor's active owner
// membership) and, if entitled, the family to be entitled.
func (c *familyTx) ownerFamily(ctx context.Context, familyID, actor string, entitled bool) (familyRow, error) {
	families, err := lockFamilies(ctx, c.tx, familyID)
	if err != nil {
		return familyRow{}, err
	}
	family, ok := families[familyID]
	if !ok || family.Status != "active" || family.OwnerAccountID != actor {
		return familyRow{}, ErrOwnerRequired
	}
	role, status, err := readMembership(ctx, c.tx, familyID, actor)
	if err != nil {
		return familyRow{}, err
	}
	if role != "owner" || status != "active" {
		return familyRow{}, ErrOwnerRequired
	}
	if entitled {
		if err := c.requireEntitlement(ctx, family); err != nil {
			return familyRow{}, err
		}
	}
	return family, nil
}

// requireEntitlement is state.go familyEntitled: when the node or the family
// requires it, the family's family_sync entitlement must be active and
// unexpired at the command time (entitlement_required).
func (c *familyTx) requireEntitlement(ctx context.Context, family familyRow) error {
	if !c.s.requireFamilySyncEntitlement && !family.EntitlementRequired {
		return nil
	}
	var status string
	var expires *time.Time
	err := c.tx.QueryRow(ctx, `SELECT status::text, expires_at FROM family_entitlements WHERE family_id=$1 AND entitlement_key='family_sync'`, family.ID).
		Scan(&status, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrEntitlementRequired
	}
	if err != nil {
		return unavailable("read entitlement", err)
	}
	if status != "active" || (expires != nil && !c.now.Before(*expires)) {
		return ErrEntitlementRequired
	}
	return nil
}

// removeMember is member_left/member_removed's shared check and effect
// (state.go:582-592, projector member_left/member_removed,
// event_handlers.go:627-638): the target's membership must be active
// (membership_revoked) and not the owner's (owner_required). The target's
// account is locked by the caller.
func (c *familyTx) removeMember(ctx context.Context, family familyRow, target string) (Result, string, error) {
	role, status, err := readMembership(ctx, c.tx, family.ID, target)
	if err != nil {
		return Result{}, "", err
	}
	if status != "active" {
		return Result{}, "", ErrMembershipRevoked
	}
	if role == "owner" {
		return Result{}, "", ErrOwnerRequired
	}
	if err := familyBatch(ctx, c.tx, []batchStep{
		{`UPDATE memberships SET status='removed', updated_at=$3 WHERE family_id=$1 AND account_id=$2 AND status='active' AND role<>'owner'`,
			[]any{family.ID, target, c.now}, 1},
		{`DELETE FROM family_slots WHERE account_id=$2 AND family_id=$1 AND state='active'`, []any{family.ID, target}, -1},
		{`UPDATE sessions SET status='revoked', revoked_at=$2, revoke_reason='member_removed' WHERE account_id=$1 AND status='active'`, []any{target, c.now}, -1},
		deadReceiptsStep(target),
		actorLabelStep(family.ID, target, true, c.now),
	}); err != nil {
		return Result{}, "", err
	}
	c.invalidate(target, family.ID)
	return c.audit(ctx, family.ID, []*chainHead{family.Chain, c.accounts[target].Chain}, map[string]any{"family_id": family.ID, "account_id": target})
}

// audit appends the command's row to each head (the family's first) and
// returns the result, whose revision is the family's new audit seq.
func (c *familyTx) audit(ctx context.Context, familyID string, heads []*chainHead, payload map[string]any) (Result, string, error) {
	var revision uint64
	for i, head := range heads {
		seq, _, err := appendAudit(ctx, c.tx, head, c.spec.kind, c.spec.commandID, c.spec.actor, c.now, payload)
		if err != nil {
			return Result{}, "", err
		}
		if i == 0 {
			revision = seq
		}
	}
	return Result{CommandID: c.spec.commandID, Outcome: "committed", Code: "ok", Revision: revision}, familyID, nil
}

// actorLabelStep records accountID's label in familyID through
// app.authority_actor_label: its current display name, or 'Former caregiver'.
func actorLabelStep(familyID, accountID string, former bool, at time.Time) batchStep {
	return batchStep{`SELECT app.authority_actor_label($1,$2,$3,$4)`, []any{familyID, accountID, former, at}, -1}
}

// familyBatch runs steps and maps the unique violations family commands can
// meet to their legacy codes.
func familyBatch(ctx context.Context, tx pgx.Tx, steps []batchStep) error {
	return mapFamilyWriteError(runBatch(ctx, tx, steps))
}

// mapFamilyWriteError maps a unique violation on the family_slots primary
// key (one family per account) to family_slot_occupied, and one on the
// one-active-owner index or a family/invitation key to invalid_command.
// Anything else is returned unchanged.
func mapFamilyWriteError(err error) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return err
	}
	switch pgErr.ConstraintName {
	case "family_slots_pkey":
		return fmt.Errorf("authority: %w", errors.Join(pgErr, ErrFamilySlotOccupied))
	case "memberships_one_active_owner", "families_pkey", "invitations_pkey", "invitations_token_digest_key":
		return fmt.Errorf("authority: %w", errors.Join(pgErr, ErrInvalidCommand))
	}
	return err
}

func readSlot(ctx context.Context, tx pgx.Tx, accountID string) (*FamilySlot, error) {
	var slot FamilySlot
	err := tx.QueryRow(ctx, `SELECT family_id::text, state::text FROM family_slots WHERE account_id=$1`, accountID).Scan(&slot.FamilyID, &slot.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable("read family slot", err)
	}
	return &slot, nil
}

// activeSlot is the account's active slot, or family_unavailable
// (family/service.go: every member command starts from the caller's slot).
func activeSlot(ctx context.Context, tx pgx.Tx, accountID string) (FamilySlot, error) {
	slot, err := readSlot(ctx, tx, accountID)
	if err != nil {
		return FamilySlot{}, err
	}
	if slot == nil || slot.State != "active" {
		return FamilySlot{}, ErrFamilyUnavailable
	}
	return *slot, nil
}

// readMembership returns the membership's role and status, or "" for both.
func readMembership(ctx context.Context, tx pgx.Tx, familyID, accountID string) (role, status string, err error) {
	err = tx.QueryRow(ctx, `SELECT role::text, status::text FROM memberships WHERE family_id=$1 AND account_id=$2`, familyID, accountID).Scan(&role, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", unavailable("read membership", err)
	}
	return role, status, nil
}

func readInvitation(ctx context.Context, tx pgx.Tx, familyID, invitationID string) (InvitationView, bool, error) {
	var v InvitationView
	err := tx.QueryRow(ctx, `SELECT id::text, family_id::text, creator_account_id::text, token_digest, role::text, status::text, expires_at, created_at
		FROM invitations WHERE id=$1 AND family_id=$2`, invitationID, familyID).
		Scan(&v.ID, &v.FamilyID, &v.CreatorAccountID, &v.TokenDigest, &v.Role, &v.Status, &v.ExpiresAt, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return InvitationView{}, false, nil
	}
	if err != nil {
		return InvitationView{}, false, unavailable("read invitation", err)
	}
	v.ExpiresAt, v.CreatedAt = v.ExpiresAt.UTC(), v.CreatedAt.UTC()
	return v, true, nil
}

// invitationStatusError is state.go's invitation status switch.
func invitationStatusError(status string) error {
	switch status {
	case "active":
		return nil
	case "redeemed":
		return ErrInvitationConsumed
	case "revoked":
		return ErrInvitationRevoked
	default:
		return ErrInvitationInvalid
	}
}

// read runs fn in a read-only use of an authority transaction.
func (s *Service) read(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	return fn(tx)
}

func validInvitationCandidates(digests []string) bool {
	if len(digests) < 1 || len(digests) > maxInvitationCandidates {
		return false
	}
	seen := make(map[string]struct{}, len(digests))
	for _, digest := range digests {
		if !identityKeyPattern.MatchString(digest) {
			return false
		}
		if _, dup := seen[digest]; dup {
			return false
		}
		seen[digest] = struct{}{}
	}
	return true
}

// validFamilyName is family/service.go validDisplayName: trimmed, 1..80
// characters, at most 320 bytes, no control characters.
func validFamilyName(name string) bool {
	if name != strings.TrimSpace(name) || !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 80 || len(name) > 320 {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validCommandTime accepts a zero time (the service clock) or a valid one.
func validCommandTime(at time.Time) bool { return at.IsZero() || validTime(at) }

func unavailable(what string, err error) error {
	return fmt.Errorf("authority: %s: %w", what, errors.Join(err, ErrUnavailable))
}

// errOr returns err when it is set and otherwise code.
func errOr(err, code error) error {
	if err != nil {
		return err
	}
	return code
}
