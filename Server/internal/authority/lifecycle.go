package authority

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"time"

	"babytracker/server/internal/access"
	"github.com/jackc/pgx/v5"
)

// Lifecycle commands (security/state.go decideLifecycleEffects,
// event_handlers.go:741-888 and the 000007 projector's lifecycle branch).
// Each runs in familyCommand, so its exact retry returns the stored result
// before any other check, under the same lock order (accounts, then
// families, then sessions):
//
//   - ScheduleDeletion locks every active member's account (read before the
//     locks, re-checked under the family lock), then the family.
//   - RecoverFamily and ForfeitRecovery lock the account, then the family
//     its family_recovery session names.
//   - DeleteAccount locks the account, then every family it is still part of.
//   - StartFamilyPurge (retention worker) locks the owner's account, then the
//     family; CompleteFamilyPurge locks the family.
//
// Every command that revokes sessions or moves a slot holds that account's
// row lock, so a concurrent Refresh (which locks the account first) either
// commits before the command, whose UPDATE then covers its child, or runs
// after it and sees the new state.

// familyPurgeDelay is the recovery window: purge_after = scheduling time +
// 30 days (state.go:670).
const familyPurgeDelay = 30 * 24 * time.Hour

// ScheduleDeletion schedules the owner's family for deletion.
type ScheduleDeletion struct {
	CommandID, OwnerAccountID string
	At                        time.Time
	Principal                 access.Principal
}

// DeletionResult is ScheduleDeletion's result: the family and the end of its
// recovery window (on an exact retry, those of the original command).
type DeletionResult struct {
	Result
	FamilyID   string
	PurgeAfter time.Time
}

// RecoverFamily restores the family the caller's family_recovery session
// names.
type RecoverFamily struct {
	CommandID, AccountID string
	At                   time.Time
	Principal            access.Principal
}

// ForfeitRecovery gives up the claim on the family the caller's
// family_recovery session names.
type ForfeitRecovery struct {
	CommandID, AccountID string
	At                   time.Time
	Principal            access.Principal
}

// DeleteAccount deletes the caller's account. IdentityKey is the account's
// identity key (hmac-sha256:<key id>:<hex>), as the caller resolved it.
type DeleteAccount struct {
	CommandID, AccountID, IdentityKey string
	At                                time.Time
	Principal                         access.Principal
}

// StartFamilyPurge marks a due deletion-pending family as being purged. It
// is a retention-worker command: its actor is RetentionWorkerActorID of the
// node's instance.
type StartFamilyPurge struct {
	CommandID, FamilyID string
	At                  time.Time
}

// CompleteFamilyPurge records a started purge as finished. StartCommandID is
// the StartFamilyPurge command that began it; Receipt is the physical purge
// receipt (a UUID).
type CompleteFamilyPurge struct {
	CommandID, FamilyID, StartCommandID, Receipt string
	At                                           time.Time
}

// RecoveryStatus is a deletion-pending family as its former owner sees it
// (family/lifecycle.go RecoveryStatus).
type RecoveryStatus struct {
	FamilyID, State string
	PurgeAfter      time.Time
	CanRecover      bool
}

// ScheduleDeletion schedules the owner's family for deletion
// (family/lifecycle.go ScheduleDeletion, state.go:648-684,
// event_handlers.go:741-780, 000007 projector). Preconditions: a live
// session; the owner's slot is active (family_unavailable); owner
// (owner_required); the family's active members are exactly the ones read
// before the locks (membership_revoked; legacy's PreviousMemberIDs
// snapshot). Effects: the family is deletion_pending with purge_after 30
// days after the command; every membership is former; non-owner slots are
// deleted and the owner's becomes family_recovery; the owner's recovery claim
// is active; every active session of every member is revoked
// (family_deleted); pending invitations are revoked; every member's label is
// 'Former caregiver'. No entitlement is required (legacy entitlementExempt).
// Locks: every member's account, the family, then the members' sessions.
// Audit: family_deletion_scheduled on the family and on every member's
// account.
func (s *Service) ScheduleDeletion(ctx context.Context, in ScheduleDeletion) (DeletionResult, error) {
	if !validID(in.CommandID) || !validID(in.OwnerAccountID) || !validCommandTime(in.At) {
		return DeletionResult{}, ErrInvalidCommand
	}
	if err := commandPrincipal(in.Principal, in.OwnerAccountID); err != nil {
		return DeletionResult{}, err
	}
	var snapshotFamily, familyID string
	var snapshot []string
	var purgeAfter time.Time
	result, _, err := s.familyCommand(ctx, commandSpec{principal: in.Principal, actor: in.OwnerAccountID, commandID: in.CommandID,
		kind: KindFamilyDeletionScheduled, at: in.At, accounts: []string{in.OwnerAccountID}, payload: map[string]any{},
		lockSet: func(ctx context.Context, tx pgx.Tx) ([]string, error) {
			slot, err := readSlot(ctx, tx, in.OwnerAccountID)
			if err != nil || slot == nil {
				return nil, err
			}
			snapshotFamily = slot.FamilyID
			snapshot, err = activeMembers(ctx, tx, slot.FamilyID)
			return snapshot, err
		},
		replay: func(ctx context.Context, tx pgx.Tx) error {
			var err error
			familyID, purgeAfter, err = deletionReceipt(ctx, tx, in.OwnerAccountID, in.CommandID)
			return err
		}},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			slot, err := activeSlot(ctx, c.tx, in.OwnerAccountID)
			if err != nil {
				return Result{}, "", err
			}
			family, err := c.ownerFamily(ctx, slot.FamilyID, in.OwnerAccountID, false)
			if err != nil {
				return Result{}, "", err
			}
			// Membership changes lock the family, so the set is stable now; it
			// must be the set whose accounts were locked.
			members, err := activeMembers(ctx, c.tx, family.ID)
			if err != nil {
				return Result{}, "", err
			}
			if family.ID != snapshotFamily || !slices.Equal(members, snapshot) {
				return Result{}, "", ErrMembershipRevoked
			}
			heads := []*chainHead{family.Chain}
			others := make([]string, 0, len(members))
			for _, member := range members {
				account, ok := c.accounts[member]
				if !ok {
					return Result{}, "", ErrMembershipRevoked
				}
				heads = append(heads, account.Chain)
				if member != in.OwnerAccountID {
					others = append(others, member)
				}
			}
			familyID, purgeAfter = family.ID, c.now.Add(familyPurgeDelay)
			steps := []batchStep{
				{`UPDATE families SET status='deletion_pending', purge_after=$2, updated_at=$3 WHERE id=$1 AND status='active' AND owner_account_id=$4`,
					[]any{family.ID, purgeAfter, c.now, in.OwnerAccountID}, 1},
				{`UPDATE memberships SET status='former', updated_at=$2 WHERE family_id=$1 AND status='active'`, []any{family.ID, c.now}, int64(len(members))},
				{`DELETE FROM family_slots WHERE family_id=$1 AND account_id = ANY($2::uuid[])`, []any{family.ID, others}, -1},
				{`UPDATE family_slots SET state='family_recovery' WHERE account_id=$1 AND family_id=$2 AND state='active'`, []any{in.OwnerAccountID, family.ID}, 1},
				// One claim row per family (000007 PK): a family recovered and
				// deleted again re-arms it for its current owner.
				{`INSERT INTO family_recovery_claims(family_id,account_id,status) VALUES ($1,$2,'active')
					ON CONFLICT (family_id) DO UPDATE SET account_id=EXCLUDED.account_id, status='active' WHERE family_recovery_claims.status<>'active'`,
					[]any{family.ID, in.OwnerAccountID}, 1},
				{`UPDATE sessions SET status='revoked', revoked_at=$2, revoke_reason='family_deleted' WHERE account_id = ANY($1::uuid[]) AND status='active'`,
					[]any{members, c.now}, -1},
				{`DELETE FROM refresh_receipts r USING sessions s WHERE r.child_id=s.id AND r.account_id = ANY($1::uuid[]) AND s.status<>'active'`, []any{members}, -1},
				{`UPDATE invitations SET status='revoked', revoked_at=$2, updated_at=$2 WHERE family_id=$1 AND status='active'`, []any{family.ID, c.now}, -1},
			}
			for _, member := range members {
				steps = append(steps, actorLabelStep(family.ID, member, true, c.now))
			}
			if err := familyBatch(ctx, c.tx, steps); err != nil {
				return Result{}, "", err
			}
			for _, member := range members {
				c.invalidate(member, family.ID)
			}
			return c.audit(ctx, family.ID, heads, map[string]any{"family_id": family.ID, "purge_after": timeText(purgeAfter), "account_ids": members})
		})
	if err != nil {
		return DeletionResult{}, err
	}
	return DeletionResult{Result: result, FamilyID: familyID, PurgeAfter: purgeAfter}, nil
}

// RecoverFamily restores a deletion-pending family to its former owner
// (family/lifecycle.go Recover, state.go:685-713, event_handlers.go:782-797,
// 000007 projector). The family is the one the caller's session names.
// Preconditions: a live session; the session is family_recovery scoped
// (recovery_only); the account's claim on the family is active, its slot is
// the family's family_recovery slot and the family is deletion_pending
// (recovery_forfeited for a forfeited claim, family_unavailable otherwise);
// the command time is before purge_after (recovery_expired). Effects: the
// family is active, owned by the account, without purge_after; the owner's
// membership and slot are active; the claim is recovered; the owner's label
// is restored; the account's active family_recovery sessions become family
// scope with the owner role. Former members stay former. Locks: the account, the family.
// Audit: family_recovered on the family and the account.
func (s *Service) RecoverFamily(ctx context.Context, in RecoverFamily) (Result, error) {
	return s.recoveryCommand(ctx, KindFamilyRecovered, in.CommandID, in.AccountID, in.At, in.Principal)
}

// ForfeitRecovery gives up a recovery claim (family/lifecycle.go Forfeit,
// state.go:685-714, event_handlers.go:799-811, 000007 projector).
// Preconditions as RecoverFamily, without the deadline. Effects: the claim is
// forfeited and the account's family_recovery slot deleted; the account's
// active family_recovery sessions become account scope (not revoked). Locks: the
// account, the family. Audit: recovery_forfeited on the family and the
// account.
func (s *Service) ForfeitRecovery(ctx context.Context, in ForfeitRecovery) (Result, error) {
	return s.recoveryCommand(ctx, KindRecoveryForfeited, in.CommandID, in.AccountID, in.At, in.Principal)
}

func (s *Service) recoveryCommand(ctx context.Context, kind Kind, commandID, accountID string, at time.Time, p access.Principal) (Result, error) {
	if !validID(commandID) || !validID(accountID) || !validCommandTime(at) {
		return Result{}, ErrInvalidCommand
	}
	if err := commandPrincipal(p, accountID); err != nil {
		return Result{}, err
	}
	result, _, err := s.familyCommand(ctx, commandSpec{principal: p, actor: accountID, commandID: commandID, kind: kind, at: at,
		accounts: []string{accountID}, payload: map[string]any{}},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			// The scope is the principal's as authenticated, as legacy
			// checked it (state.go:688): RecoverFamily and ForfeitRecovery
			// move the session row's scope, so a racing loser holding the
			// same principal meets the claim and slot checks below
			// (family_unavailable / recovery_forfeited), while a later
			// request authenticates as account scope (recovery_only).
			if p.SessionScope != "family_recovery" || !validID(p.SessionFamilyID) {
				return Result{}, "", ErrRecoveryOnly
			}
			familyID := p.SessionFamilyID
			families, err := lockFamilies(ctx, c.tx, familyID)
			if err != nil {
				return Result{}, "", err
			}
			family, familyOK := families[familyID]
			claim, err := readClaim(ctx, c.tx, familyID, accountID)
			if err != nil {
				return Result{}, "", err
			}
			slot, err := readSlot(ctx, c.tx, accountID)
			if err != nil {
				return Result{}, "", err
			}
			if claim != "active" || slot == nil || slot.FamilyID != familyID || slot.State != "family_recovery" || !familyOK || family.Status != "deletion_pending" {
				if claim == "forfeited" {
					return Result{}, "", ErrRecoveryForfeited
				}
				return Result{}, "", ErrFamilyUnavailable
			}
			var steps []batchStep
			if kind == KindFamilyRecovered {
				if family.PurgeAfter == nil || !c.now.Before(*family.PurgeAfter) {
					return Result{}, "", ErrRecoveryExpired
				}
				steps = []batchStep{
					{`UPDATE families SET status='active', owner_account_id=$2, purge_after=NULL, updated_at=$3 WHERE id=$1 AND status='deletion_pending'`,
						[]any{familyID, accountID, c.now}, 1},
					{`UPDATE memberships SET status='active', updated_at=$3 WHERE family_id=$1 AND account_id=$2 AND role='owner' AND status='former'`,
						[]any{familyID, accountID, c.now}, 1},
					{`UPDATE family_slots SET state='active' WHERE account_id=$2 AND family_id=$1 AND state='family_recovery'`, []any{familyID, accountID}, 1},
					{`UPDATE family_recovery_claims SET status='recovered' WHERE family_id=$1 AND account_id=$2 AND status='active'`, []any{familyID, accountID}, 1},
					actorLabelStep(familyID, accountID, false, c.now),
					// The recovering sessions now act for the active family
					// (legacy derived the scope from the live slot).
					{`UPDATE sessions SET scope='family', role='owner' WHERE account_id=$2 AND family_id=$1 AND scope='family_recovery' AND status='active'`,
						[]any{familyID, accountID}, -1},
				}
			} else {
				steps = []batchStep{
					{`UPDATE family_recovery_claims SET status='forfeited' WHERE family_id=$1 AND account_id=$2 AND status='active'`, []any{familyID, accountID}, 1},
					{`DELETE FROM family_slots WHERE account_id=$2 AND family_id=$1 AND state='family_recovery'`, []any{familyID, accountID}, 1},
					// Without a slot the sessions are account scoped, as
					// legacy derived them; they are not revoked.
					{`UPDATE sessions SET scope='account', family_id=NULL, role=NULL WHERE account_id=$2 AND family_id=$1 AND scope='family_recovery' AND status='active'`,
						[]any{familyID, accountID}, -1},
				}
			}
			if err := familyBatch(ctx, c.tx, steps); err != nil {
				return Result{}, "", err
			}
			return c.audit(ctx, familyID, []*chainHead{family.Chain, c.accounts[accountID].Chain}, map[string]any{"family_id": familyID})
		})
	return result, err
}

// DeleteAccount deletes the caller's account (account/lifecycle.go Delete,
// state.go:715-730, event_handlers.go:813-847, 000007 projector).
// Preconditions: a live session; the account is active and IdentityKey is
// its live identity key (session_revoked); it owns no active family
// (ownership_transfer_required). Effects: the account is deleted (its
// display name cleared, as the projector did); its identity is no longer
// live (a later sign-in is a new actor generation); its slot is deleted;
// every active session is revoked (account_deleted) and every device
// revoked; every membership is removed (an active one's label becomes
// 'Former caregiver'); its active recovery claims are forfeited. Locks: the
// account, then every family where it has a membership that is not removed,
// a slot or an active claim. Audit: account_deleted on the account and on
// each of those families. Revision is the account's audit seq.
func (s *Service) DeleteAccount(ctx context.Context, in DeleteAccount) (Result, error) {
	if !validID(in.CommandID) || !validID(in.AccountID) || !identityKeyPattern.MatchString(in.IdentityKey) || !validCommandTime(in.At) {
		return Result{}, ErrInvalidCommand
	}
	if err := commandPrincipal(in.Principal, in.AccountID); err != nil {
		return Result{}, err
	}
	result, _, err := s.familyCommand(ctx, commandSpec{principal: in.Principal, actor: in.AccountID, commandID: in.CommandID, kind: KindAccountDeleted,
		at: in.At, accounts: []string{in.AccountID}, payload: map[string]any{"identity_key": in.IdentityKey}},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			account, ok := c.accounts[in.AccountID]
			var key string
			var live bool
			err := c.tx.QueryRow(ctx, `SELECT 'hmac-sha256:'||identity_key_id||':'||encode(identity_digest,'hex'), live FROM account_identities
				WHERE account_id=$1`, in.AccountID).Scan(&key, &live)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return Result{}, "", unavailable("read identity", err)
			}
			if err != nil || !ok || account.Status != "active" || !live || key != in.IdentityKey {
				return Result{}, "", ErrSessionRevoked
			}
			// Memberships, slots and claims of the account change only under
			// its account lock, which is held: this set is stable.
			rows, err := c.tx.Query(ctx, `SELECT family_id::text FROM memberships WHERE account_id=$1 AND status<>'removed'
				UNION SELECT family_id::text FROM family_slots WHERE account_id=$1
				UNION SELECT family_id::text FROM family_recovery_claims WHERE account_id=$1 AND status='active'
				UNION SELECT id::text FROM families WHERE owner_account_id=$1 AND status='active'`, in.AccountID)
			if err != nil {
				return Result{}, "", unavailable("read account families", err)
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return Result{}, "", unavailable("read account families", err)
			}
			families, err := lockFamilies(ctx, c.tx, ids...)
			if err != nil {
				return Result{}, "", err
			}
			sorted, _ := sortedIDs(ids)
			heads := []*chainHead{account.Chain}
			var steps []batchStep
			for _, id := range sorted {
				family, ok := families[id]
				if !ok {
					return Result{}, "", fmt.Errorf("authority: family %s vanished: %w", id, ErrUnavailable)
				}
				if family.Status == "active" && family.OwnerAccountID == in.AccountID {
					return Result{}, "", ErrOwnershipTransferRequired
				}
				heads = append(heads, family.Chain)
				if _, status, err := readMembership(ctx, c.tx, id, in.AccountID); err != nil {
					return Result{}, "", err
				} else if status == "active" {
					steps = append(steps, actorLabelStep(id, in.AccountID, true, c.now))
				}
			}
			steps = append([]batchStep{
				{`UPDATE accounts SET status='deleted', display_name='', profile_required=false, updated_at=$2 WHERE id=$1 AND status='active'`,
					[]any{in.AccountID, c.now}, 1},
				{`UPDATE account_identities SET live=false WHERE account_id=$1 AND live`, []any{in.AccountID}, 1},
				{`DELETE FROM family_slots WHERE account_id=$1`, []any{in.AccountID}, -1},
				{`UPDATE sessions SET status='revoked', revoked_at=$2, revoke_reason='account_deleted' WHERE account_id=$1 AND status='active'`,
					[]any{in.AccountID, c.now}, -1},
				deadReceiptsStep(in.AccountID),
				{`UPDATE devices SET status='revoked', revoked_at=$2, updated_at=$2 WHERE account_id=$1 AND status='active'`, []any{in.AccountID, c.now}, -1},
				{`UPDATE memberships SET status='removed', updated_at=$2 WHERE account_id=$1 AND status<>'removed'`, []any{in.AccountID, c.now}, -1},
				{`UPDATE family_recovery_claims SET status='forfeited' WHERE account_id=$1 AND status='active'`, []any{in.AccountID}, -1},
			}, steps...)
			if err := familyBatch(ctx, c.tx, steps); err != nil {
				return Result{}, "", err
			}
			c.receiptFamilies = sorted
			c.invalidate(in.AccountID, "")
			return c.audit(ctx, "", heads, map[string]any{"account_id": in.AccountID})
		})
	return result, err
}

// StartFamilyPurge begins a due family's purge (event_handlers.go:849-866;
// a reserved event in the legacy ledger, now a retention-worker command whose
// actor is RetentionWorkerActorID of this node). Preconditions: the family is
// deletion_pending (family_unavailable) and the command time is at or after
// purge_after (invalid_command). Effects: the family is purge_started with
// purge_started_at and purge_command_id; the owner's family_recovery slot is
// deleted, the owner's active family_recovery sessions of the family become
// account scope (not revoked, as ForfeitRecovery leaves them), and the
// family's active recovery claims are forfeited (recovery is no longer
// possible, and the owner's one active claim is free for a later deletion of
// another family). Locks: the owner's account, the family.
// Audit: family_purge_started on the family.
func (s *Service) StartFamilyPurge(ctx context.Context, in StartFamilyPurge) (Result, error) {
	if !validID(in.CommandID) || !validID(in.FamilyID) || !validCommandTime(in.At) {
		return Result{}, ErrInvalidCommand
	}
	actor, err := RetentionWorkerActorID(s.instanceID)
	if err != nil {
		return Result{}, err
	}
	result, _, err := s.familyCommand(ctx, commandSpec{system: true, actor: actor, commandID: in.CommandID, kind: KindFamilyPurgeStarted, at: in.At,
		families: []string{in.FamilyID}, payload: map[string]any{"family_id": in.FamilyID},
		lockSet: func(ctx context.Context, tx pgx.Tx) ([]string, error) {
			var owner string
			err := tx.QueryRow(ctx, `SELECT coalesce(owner_account_id::text,'') FROM families WHERE id=$1`, in.FamilyID).Scan(&owner)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner == "") {
				return nil, nil
			}
			if err != nil {
				return nil, unavailable("read family owner", err)
			}
			return []string{owner}, nil
		}},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			family, ok := c.families[in.FamilyID]
			if !ok || family.Status != "deletion_pending" || family.PurgeAfter == nil {
				return Result{}, "", ErrFamilyUnavailable
			}
			if c.now.Before(*family.PurgeAfter) {
				return Result{}, "", ErrInvalidCommand
			}
			// A deletion-pending family's owner cannot change (only its
			// recovery, by that owner, ends the state), so the account read
			// before the locks is the one locked.
			if _, locked := c.accounts[family.OwnerAccountID]; !locked {
				return Result{}, "", fmt.Errorf("authority: purge of %s: owner not locked: %w", family.ID, ErrUnavailable)
			}
			if err := familyBatch(ctx, c.tx, []batchStep{
				{`UPDATE families SET status='purge_started', purge_started_at=$2, purge_command_id=$3, updated_at=$2 WHERE id=$1 AND status='deletion_pending'`,
					[]any{family.ID, c.now, in.CommandID}, 1},
				{`DELETE FROM family_slots WHERE account_id=$2 AND family_id=$1 AND state='family_recovery'`, []any{family.ID, family.OwnerAccountID}, -1},
				// Without the slot the owner's recovery sessions are account
				// scoped, as ForfeitRecovery leaves them (legacy derived the
				// scope from the slot); they are not revoked.
				{`UPDATE sessions SET scope='account', family_id=NULL, role=NULL WHERE account_id=$2 AND family_id=$1 AND scope='family_recovery' AND status='active'`,
					[]any{family.ID, family.OwnerAccountID}, -1},
				{`UPDATE family_recovery_claims SET status='forfeited' WHERE family_id=$1 AND status='active'`, []any{family.ID}, -1},
			}); err != nil {
				return Result{}, "", err
			}
			c.invalidate("", family.ID)
			return c.audit(ctx, family.ID, []*chainHead{family.Chain},
				map[string]any{"family_id": family.ID, "purge_after": timeText(*family.PurgeAfter), "server_instance_id": c.s.instanceID})
		})
	return result, err
}

// CompleteFamilyPurge records a finished purge (event_handlers.go:868-888).
// Preconditions: the family is purge_started (family_unavailable) by
// StartCommandID (invalid_command; legacy compared the start revision and
// instance). Effects: the family is purge_completed with purge_receipt and
// purged_at, and its display name is cleared (the empty string); any active
// recovery claim is forfeited (StartFamilyPurge already forfeited them, so
// this is a no-op kept for safety). An exact retry returns the stored result
// and writes nothing. Locks: the family. Audit: family_purge_completed on
// the family (display_name_cleared: true, never the name). Invitation rows are kept, so
// a lost-acknowledgement retry of a redemption into the family still finds
// its receipt (RedeemInvitation hashes the resolved invitation ID).
func (s *Service) CompleteFamilyPurge(ctx context.Context, in CompleteFamilyPurge) (Result, error) {
	if !validID(in.CommandID) || !validID(in.FamilyID) || !validID(in.StartCommandID) || !validID(in.Receipt) || !validCommandTime(in.At) {
		return Result{}, ErrInvalidCommand
	}
	actor, err := RetentionWorkerActorID(s.instanceID)
	if err != nil {
		return Result{}, err
	}
	result, _, err := s.familyCommand(ctx, commandSpec{system: true, actor: actor, commandID: in.CommandID, kind: KindFamilyPurgeCompleted, at: in.At,
		families: []string{in.FamilyID}, payload: map[string]any{"family_id": in.FamilyID, "start_command_id": in.StartCommandID, "purge_receipt": in.Receipt}},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			family, ok := c.families[in.FamilyID]
			if !ok || family.Status != "purge_started" {
				return Result{}, "", ErrFamilyUnavailable
			}
			if family.PurgeCommandID != in.StartCommandID {
				return Result{}, "", ErrInvalidCommand
			}
			if err := familyBatch(ctx, c.tx, []batchStep{
				// The family's own display name goes with the purge (owner
				// decision 2026-10-08: no personal name outlives a purge):
				// the empty string, which the column admits and which names
				// nothing, rather than a placeholder a reader could show as
				// a name. The audit row records only that it was cleared.
				{`UPDATE families SET status='purge_completed', purge_receipt=$2, purged_at=$3, updated_at=$3, display_name='' WHERE id=$1 AND status='purge_started' AND purge_command_id=$4`,
					[]any{family.ID, in.Receipt, c.now, in.StartCommandID}, 1},
				{`UPDATE family_recovery_claims SET status='forfeited' WHERE family_id=$1 AND status='active'`, []any{family.ID}, -1},
			}); err != nil {
				return Result{}, "", err
			}
			c.invalidate("", family.ID)
			return c.audit(ctx, family.ID, []*chainHead{family.Chain},
				map[string]any{"family_id": family.ID, "purge_receipt": in.Receipt, "display_name_cleared": true})
		})
	return result, err
}

// RecoveryStatus is the deletion-pending family accountID may recover
// (family/lifecycle.go RecoveryStatus and recoveryStatusFromState): the
// account's slot must be a family_recovery slot (recovery_only); the family
// must be deletion_pending with a claim of the account (recovery_forfeited
// for a forfeited claim, family_unavailable otherwise). CanRecover is an
// active claim before purge_after on the service clock. The caller checks
// the principal (a live family_recovery session) first, as the legacy
// service did.
func (s *Service) RecoveryStatus(ctx context.Context, accountID string) (RecoveryStatus, error) {
	if !validID(accountID) {
		return RecoveryStatus{}, ErrInvalidCommand
	}
	var status RecoveryStatus
	err := s.read(ctx, func(tx pgx.Tx) error {
		slot, err := readSlot(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if slot == nil || slot.State != "family_recovery" {
			return ErrRecoveryOnly
		}
		status, err = s.recoveryStatus(ctx, tx, accountID, slot.FamilyID)
		return err
	})
	return status, err
}

// FamilyRecoveryStatus is RecoveryStatus for a named family, without the
// slot (family/lifecycle.go recoveryStatusFromState): the family must be
// deletion_pending with a claim of accountID (recovery_forfeited for a
// forfeited claim, family_unavailable otherwise). ScheduleDeletion's caller
// reads it for the family the command (or its exact retry) names. It runs
// read-only (readOnly); accountID must be the authenticated caller's own
// account and familyID the one its command's result names.
func (s *Service) FamilyRecoveryStatus(ctx context.Context, accountID, familyID string) (RecoveryStatus, error) {
	if !validID(accountID) || !validID(familyID) {
		return RecoveryStatus{}, ErrInvalidCommand
	}
	var status RecoveryStatus
	err := s.readOnly(ctx, func(tx pgx.Tx) (err error) {
		status, err = s.recoveryStatus(ctx, tx, accountID, familyID)
		return err
	})
	return status, err
}

// recoveryStatus is legacy recoveryStatusFromState over one family's row and
// accountID's claim on it.
func (s *Service) recoveryStatus(ctx context.Context, tx pgx.Tx, accountID, familyID string) (RecoveryStatus, error) {
	var familyStatus, claim string
	var purgeAfter *time.Time
	err := tx.QueryRow(ctx, `SELECT f.status::text, f.purge_after, coalesce(c.status::text,'') FROM families f
		LEFT JOIN family_recovery_claims c ON c.family_id=f.id AND c.account_id=$2 WHERE f.id=$1`, familyID, accountID).Scan(&familyStatus, &purgeAfter, &claim)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return RecoveryStatus{}, unavailable("read recovery status", err)
	}
	if err != nil || claim == "" || familyStatus != "deletion_pending" {
		if claim == "forfeited" {
			return RecoveryStatus{}, ErrRecoveryForfeited
		}
		return RecoveryStatus{}, ErrFamilyUnavailable
	}
	if purgeAfter == nil {
		return RecoveryStatus{}, fmt.Errorf("authority: deletion-pending family %s without purge_after: %w", familyID, ErrUnavailable)
	}
	after := purgeAfter.UTC()
	return RecoveryStatus{FamilyID: familyID, State: familyStatus, PurgeAfter: after, CanRecover: claim == "active" && s.now().UTC().Before(after)}, nil
}

// RetentionWorkerActorID is the retention worker's actor ID on the node
// instanceID (security/event.go:411-419): sha256("baby-tracker/retention-
// worker/v1/" + instanceID) shaped as a UUID with version nibble 8 and the
// RFC 4122 variant. It is not an account; purge receipts and audit rows carry
// it as their actor.
func RetentionWorkerActorID(instanceID string) (string, error) {
	if !validID(instanceID) {
		return "", ErrInvalidCommand
	}
	digest := sha256.Sum256([]byte("baby-tracker/retention-worker/v1/" + instanceID))
	digest[6] = digest[6]&0x0f | 0x80
	digest[8] = digest[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16]), nil
}

// activeMembers returns the family's active members in ID order.
func activeMembers(ctx context.Context, tx pgx.Tx, familyID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT account_id::text FROM memberships WHERE family_id=$1 AND status='active' ORDER BY memberships.account_id`, familyID)
	if err != nil {
		return nil, unavailable("read members", err)
	}
	members, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, unavailable("read members", err)
	}
	return members, nil
}

// readClaim returns the status of account's recovery claim on family, or "".
func readClaim(ctx context.Context, tx pgx.Tx, familyID, accountID string) (string, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status::text FROM family_recovery_claims WHERE family_id=$1 AND account_id=$2`, familyID, accountID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", unavailable("read recovery claim", err)
	}
	return status, nil
}

// deletionReceipt rebuilds an exact ScheduleDeletion retry's family and
// deadline from its receipt: the receipt names the family, and purge_after
// was the receipt's time plus the recovery window.
func deletionReceipt(ctx context.Context, tx pgx.Tx, actorID, commandID string) (string, time.Time, error) {
	var families []string
	var at time.Time
	if err := tx.QueryRow(ctx, `SELECT family_ids::text[], created_at FROM command_receipts WHERE actor_account_id=$1 AND command_id=$2`, actorID, commandID).
		Scan(&families, &at); err != nil {
		return "", time.Time{}, unavailable("read deletion receipt", err)
	}
	if len(families) != 1 {
		return "", time.Time{}, fmt.Errorf("authority: deletion receipt %s names %d families: %w", commandID, len(families), ErrUnavailable)
	}
	return families[0], at.UTC().Add(familyPurgeDelay), nil
}
