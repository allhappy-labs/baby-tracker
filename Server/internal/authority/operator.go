package authority

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// Operator commands (security/operator_prepared.go, integration_prepared.go).
// An operator is not an account: its ID is the receipt and audit actor, and
// it has no session. Every operator command names one family; it runs as one
// transaction in familyCommand:
//
//  1. lock the accounts whose state it checks or changes, then the family;
//  2. look up the operator's receipt: an exact retry returns the stored
//     result before any other check (confirmation and time included), a
//     different payload under the same command ID is command_id_reused;
//  3. read the clock (after the locks): the command time;
//  4. the confirmation must be a whole UTC second after the command time
//     (invalid_confirmation);
//  5. ExpectedRevision must equal the family's audit seq
//     (stale_security_revision); a missing family skips this check and fails
//     the command's own family precondition instead;
//  6. the command's preconditions, its writes, its audit rows from the
//     locked heads, the receipt, commit; invalidations after the commit.
//
// Preflight runs steps 2, 5 and 6's checks in a read-only snapshot without
// locks or writes, and reports an already committed command as nil (the
// legacy prepare path); it checks no confirmation. Replay is step 2 alone.

// operatorPolicyID is a legacy operator policy identifier.
var operatorPolicyID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,127}$`)

// OperatorRecoverOwner is security.OperatorRecoveryInput: the operator moves
// a family whose owner's identity became unavailable to an active caregiver.
// ExpectedRevision is the family's audit seq.
type OperatorRecoverOwner struct {
	CommandID, OperatorID                          string
	FamilyID, OldOwnerAccountID, NewOwnerAccountID string
	PolicyID, EvidenceDigest, ReasonCode           string
	ExpectedRevision                               uint64
	ConfirmationExpiresAt                          time.Time
}

// OperatorEntitlement is security.OperatorEntitlementInput: Operation
// "grant" extends (or creates) the family's hosted_beta family_sync
// entitlement to ExpiresAt; "suspend" suspends it, naming its exact current
// expiry. ExpectedRevision is the family's audit seq.
type OperatorEntitlement struct {
	CommandID, OperatorID                string
	FamilyID, Operation, Key, Source     string
	ExpiresAt                            time.Time
	PolicyID, EvidenceDigest, ReasonCode string
	ExpectedRevision                     uint64
	ConfirmationExpiresAt                time.Time
}

// PreflightOperatorRecoverOwner reports whether the recovery would commit
// now; a committed one is nil. It writes nothing.
func (s *Service) PreflightOperatorRecoverOwner(ctx context.Context, in OperatorRecoverOwner) error {
	spec, err := s.recoverOwnerSpec(in)
	if err != nil {
		return err
	}
	return s.preflightOperator(ctx, spec)
}

// ReplayOperatorRecoverOwner returns the stored result of an already
// committed recovery (found=false when there is none).
func (s *Service) ReplayOperatorRecoverOwner(ctx context.Context, in OperatorRecoverOwner) (Result, bool, error) {
	spec, err := s.recoverOwnerSpec(in)
	if err != nil {
		return Result{}, false, err
	}
	return s.replayOperator(ctx, spec)
}

// ExecuteOperatorRecoverOwner recovers ownership (operator_prepared.go:
// 238-256, event_handlers.go applyOwnerChange, 000007 projector
// owner_recovered). Preconditions: the family is active and owned by
// OldOwnerAccountID, whose account is identity_unavailable and membership
// the active owner's; NewOwnerAccountID's account is active and its
// membership an active caregiver's (owner_identity_unavailable otherwise).
// Effects: the old owner becomes a caregiver, the new owner the owner (in
// that order: one active owner per family), families.owner_account_id, and
// the role of both accounts' active sessions of the family. Locks: both
// accounts, the family, then their sessions. Audit: owner_recovered on the
// family and both accounts. Invalidations: both accounts in the family.
func (s *Service) ExecuteOperatorRecoverOwner(ctx context.Context, in OperatorRecoverOwner) (Result, error) {
	spec, err := s.recoverOwnerSpec(in)
	if err != nil {
		return Result{}, err
	}
	return s.executeOperator(ctx, spec)
}

func (s *Service) recoverOwnerSpec(in OperatorRecoverOwner) (operatorSpec, error) {
	if !validOperatorMeta(in.CommandID, in.OperatorID, in.PolicyID, in.EvidenceDigest, in.ReasonCode, in.ExpectedRevision, "owner_identity_unavailable") ||
		!validID(in.FamilyID) || !validID(in.OldOwnerAccountID) || !validID(in.NewOwnerAccountID) || in.OldOwnerAccountID == in.NewOwnerAccountID {
		return operatorSpec{}, ErrInvalidCommand
	}
	audit := map[string]any{"family_id": in.FamilyID, "old_owner_account_id": in.OldOwnerAccountID, "new_owner_account_id": in.NewOwnerAccountID,
		"operator_policy_id": in.PolicyID, "operator_evidence_digest": in.EvidenceDigest, "operator_reason_code": in.ReasonCode,
		"expected_revision": in.ExpectedRevision}
	return operatorSpec{operatorID: in.OperatorID, commandID: in.CommandID, kind: KindOwnerRecovered, payload: audit,
		accounts: []string{in.OldOwnerAccountID, in.NewOwnerAccountID}, familyID: in.FamilyID, expectedRevision: in.ExpectedRevision,
		confirmation: in.ConfirmationExpiresAt,
		check: func(ctx context.Context, o *operatorTx) error {
			old, oldOK := o.accounts[in.OldOwnerAccountID]
			next, nextOK := o.accounts[in.NewOwnerAccountID]
			if !o.familyOK || o.family.Status != "active" || o.family.OwnerAccountID != in.OldOwnerAccountID ||
				!oldOK || old.Status != "identity_unavailable" || !nextOK || next.Status != "active" {
				return ErrOwnerIdentityUnavailable
			}
			oldRole, oldStatus, err := readMembership(ctx, o.tx, in.FamilyID, in.OldOwnerAccountID)
			if err != nil {
				return err
			}
			newRole, newStatus, err := readMembership(ctx, o.tx, in.FamilyID, in.NewOwnerAccountID)
			if err != nil {
				return err
			}
			if oldRole != "owner" || oldStatus != "active" || newRole != "caregiver" || newStatus != "active" {
				return ErrOwnerIdentityUnavailable
			}
			return nil
		},
		apply: func(ctx context.Context, o *operatorTx) (Result, error) {
			c := o.command
			if err := familyBatch(ctx, c.tx, []batchStep{
				{`UPDATE memberships SET role='caregiver', updated_at=$3 WHERE family_id=$1 AND account_id=$2 AND role='owner' AND status='active'`,
					[]any{in.FamilyID, in.OldOwnerAccountID, c.now}, 1},
				{`UPDATE memberships SET role='owner', updated_at=$3 WHERE family_id=$1 AND account_id=$2 AND role='caregiver' AND status='active'`,
					[]any{in.FamilyID, in.NewOwnerAccountID, c.now}, 1},
				{`UPDATE families SET owner_account_id=$3, updated_at=$4 WHERE id=$1 AND owner_account_id=$2 AND status='active'`,
					[]any{in.FamilyID, in.OldOwnerAccountID, in.NewOwnerAccountID, c.now}, 1},
				{`UPDATE sessions SET role='caregiver' WHERE account_id=$2 AND family_id=$1 AND status='active'`, []any{in.FamilyID, in.OldOwnerAccountID}, -1},
				{`UPDATE sessions SET role='owner' WHERE account_id=$2 AND family_id=$1 AND status='active'`, []any{in.FamilyID, in.NewOwnerAccountID}, -1},
			}); err != nil {
				return Result{}, err
			}
			c.invalidate(in.OldOwnerAccountID, in.FamilyID)
			c.invalidate(in.NewOwnerAccountID, in.FamilyID)
			result, _, err := c.audit(ctx, in.FamilyID, []*chainHead{o.family.Chain, o.accounts[in.OldOwnerAccountID].Chain,
				o.accounts[in.NewOwnerAccountID].Chain}, audit)
			return result, err
		}}, nil
}

// PreflightOperatorEntitlement reports whether the entitlement command would
// commit now; a committed one is nil. It writes nothing.
func (s *Service) PreflightOperatorEntitlement(ctx context.Context, in OperatorEntitlement) error {
	spec, err := s.entitlementSpec(in)
	if err != nil {
		return err
	}
	return s.preflightOperator(ctx, spec)
}

// ReplayOperatorEntitlement returns the stored result of an already
// committed entitlement command (found=false when there is none).
func (s *Service) ReplayOperatorEntitlement(ctx context.Context, in OperatorEntitlement) (Result, bool, error) {
	spec, err := s.entitlementSpec(in)
	if err != nil {
		return Result{}, false, err
	}
	return s.replayOperator(ctx, spec)
}

// ExecuteOperatorEntitlement grants or suspends the family's hosted_beta
// family_sync entitlement (operator_prepared.go:258-285, 000007 projector).
// Preconditions: the family is active (family_unavailable). A grant's expiry
// is after the command time and, when the family has an entitlement, that
// entitlement is an active hosted_beta one expiring earlier
// (invalid_command). A suspension names the active hosted_beta
// entitlement's exact current expiry (invalid_command). Effects: the
// family_entitlements row (inserted, extended, or suspended keeping its
// expiry). Locks: the family. Audit: operator_entitlement_granted or
// _suspended on the family. Invalidations: the family, on suspension.
func (s *Service) ExecuteOperatorEntitlement(ctx context.Context, in OperatorEntitlement) (Result, error) {
	spec, err := s.entitlementSpec(in)
	if err != nil {
		return Result{}, err
	}
	return s.executeOperator(ctx, spec)
}

func (s *Service) entitlementSpec(in OperatorEntitlement) (operatorSpec, error) {
	kind, reason, status := KindOperatorEntitlementGranted, "beta_extension", "active"
	switch in.Operation {
	case "grant":
	case "suspend":
		kind, reason, status = KindOperatorEntitlementSuspended, "abuse_suspension", "suspended"
	default:
		return operatorSpec{}, ErrInvalidCommand
	}
	if !validOperatorMeta(in.CommandID, in.OperatorID, in.PolicyID, in.EvidenceDigest, in.ReasonCode, in.ExpectedRevision, reason) ||
		!validID(in.FamilyID) || in.Key != "family_sync" || in.Source != "hosted_beta" || !canonicalUTCSecond(in.ExpiresAt) {
		return operatorSpec{}, ErrInvalidCommand
	}
	expires := in.ExpiresAt.Format(time.RFC3339)
	audit := map[string]any{"family_id": in.FamilyID, "entitlement_key": in.Key, "source": in.Source, "status": status, "expires_at": expires,
		"operator_policy_id": in.PolicyID, "operator_evidence_digest": in.EvidenceDigest, "operator_reason_code": in.ReasonCode,
		"expected_revision": in.ExpectedRevision}
	var exists bool
	return operatorSpec{operatorID: in.OperatorID, commandID: in.CommandID, kind: kind, payload: audit, familyID: in.FamilyID,
		expectedRevision: in.ExpectedRevision, confirmation: in.ConfirmationExpiresAt,
		check: func(ctx context.Context, o *operatorTx) error {
			if !o.familyOK || o.family.Status != "active" {
				return ErrFamilyUnavailable
			}
			var currentStatus, currentSource string
			var currentExpiry *time.Time
			err := o.tx.QueryRow(ctx, `SELECT status::text, source, expires_at FROM family_entitlements WHERE family_id=$1 AND entitlement_key=$2`,
				in.FamilyID, in.Key).Scan(&currentStatus, &currentSource, &currentExpiry)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return unavailable("read entitlement", err)
			}
			exists = err == nil
			current := exists && currentStatus == "active" && currentSource == in.Source && currentExpiry != nil
			if in.Operation == "grant" {
				if !in.ExpiresAt.After(o.now) || (exists && (!current || !in.ExpiresAt.After(*currentExpiry))) {
					return ErrInvalidCommand
				}
				return nil
			}
			if !current || !currentExpiry.Equal(in.ExpiresAt) {
				return ErrInvalidCommand
			}
			return nil
		},
		apply: func(ctx context.Context, o *operatorTx) (Result, error) {
			c := o.command
			var step batchStep
			switch {
			case in.Operation == "suspend":
				step = batchStep{`UPDATE family_entitlements SET status='suspended', updated_at=$3
					WHERE family_id=$1 AND entitlement_key='family_sync' AND source='hosted_beta' AND status='active' AND expires_at=$2`,
					[]any{in.FamilyID, in.ExpiresAt, c.now}, 1}
				c.invalidate("", in.FamilyID)
			case exists:
				step = batchStep{`UPDATE family_entitlements SET expires_at=$2, updated_at=$3
					WHERE family_id=$1 AND entitlement_key='family_sync' AND source='hosted_beta' AND status='active' AND expires_at<$2`,
					[]any{in.FamilyID, in.ExpiresAt, c.now}, 1}
			default:
				step = batchStep{`INSERT INTO family_entitlements(family_id,entitlement_key,source,status,expires_at,created_at,updated_at)
					VALUES ($1,'family_sync','hosted_beta','active',$2,$3,$3)`, []any{in.FamilyID, in.ExpiresAt, c.now}, 1}
			}
			if err := familyBatch(ctx, c.tx, []batchStep{step}); err != nil {
				return Result{}, err
			}
			result, _, err := c.audit(ctx, in.FamilyID, []*chainHead{o.family.Chain}, audit)
			return result, err
		}}, nil
}

// operatorSpec is one operator command for preflightOperator,
// replayOperator and executeOperator.
type operatorSpec struct {
	operatorID, commandID string
	kind                  Kind
	// payload is the command hash payload: every input but the confirmation.
	payload any
	// accounts are locked (or, in a preflight, read) before the family.
	accounts         []string
	familyID         string
	expectedRevision uint64
	confirmation     time.Time
	// wholeSecond truncates the command time to the second (integrations).
	wholeSecond bool
	// check is the command's preconditions against the rows read; it
	// writes nothing.
	check func(context.Context, *operatorTx) error
	// apply writes the command and its audit rows after check passed.
	apply func(context.Context, *operatorTx) (Result, error)
}

// operatorTx is an operator command's rows as locked (or read) at now.
type operatorTx struct {
	tx       pgx.Tx
	accounts map[string]accountRow
	family   familyRow
	familyOK bool
	now      time.Time
	// command is the committing transaction (nil in a preflight).
	command *familyTx
}

// precheck is steps 5 and 6's checks: the expected family revision, then
// the command's preconditions.
func (spec operatorSpec) precheck(ctx context.Context, o *operatorTx) error {
	if o.familyOK && spec.expectedRevision != o.family.Chain.Seq {
		return ErrStaleSecurityRevision
	}
	return spec.check(ctx, o)
}

func (s *Service) executeOperator(ctx context.Context, spec operatorSpec) (Result, error) {
	result, _, err := s.familyCommand(ctx, commandSpec{system: true, actor: spec.operatorID, commandID: spec.commandID, kind: spec.kind,
		accounts: spec.accounts, families: []string{spec.familyID}, payload: spec.payload, wholeSecond: spec.wholeSecond},
		func(ctx context.Context, c *familyTx) (Result, string, error) {
			// The confirmation is a whole second, so comparing it with the
			// command time truncated to the microsecond or the second is the
			// same as comparing it with the clock reading.
			if !validOperatorConfirmation(spec.confirmation, c.now) {
				return Result{}, "", ErrInvalidConfirmation
			}
			o := &operatorTx{tx: c.tx, accounts: c.accounts, now: c.now, command: c}
			o.family, o.familyOK = c.families[spec.familyID]
			if err := spec.precheck(ctx, o); err != nil {
				return Result{}, "", err
			}
			result, err := spec.apply(ctx, o)
			return result, spec.familyID, err
		})
	return result, err
}

func (s *Service) preflightOperator(ctx context.Context, spec operatorSpec) error {
	hash, err := CanonicalCommandHash(spec.operatorID, spec.kind, spec.payload)
	if err != nil {
		return err
	}
	tx, err := s.beginReadOnly(ctx)
	if err != nil {
		return errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, found, err := lookupReceipt(ctx, tx, spec.operatorID, spec.commandID, hash); err != nil || found {
		return err
	}
	accounts, err := readAccounts(ctx, tx, spec.accounts...)
	if err != nil {
		return err
	}
	families, err := readFamilies(ctx, tx, spec.familyID)
	if err != nil {
		return err
	}
	now := s.at(time.Time{})
	if spec.wholeSecond {
		now = now.Truncate(time.Second)
	}
	o := &operatorTx{tx: tx, accounts: accounts, now: now}
	o.family, o.familyOK = families[spec.familyID]
	return spec.precheck(ctx, o)
}

func (s *Service) replayOperator(ctx context.Context, spec operatorSpec) (Result, bool, error) {
	hash, err := CanonicalCommandHash(spec.operatorID, spec.kind, spec.payload)
	if err != nil {
		return Result{}, false, err
	}
	tx, err := s.beginReadOnly(ctx)
	if err != nil {
		return Result{}, false, errors.Join(err, ErrUnavailable)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	return lookupReceipt(ctx, tx, spec.operatorID, spec.commandID, hash)
}

// validOperatorMeta is operator_prepared.go validOperatorMeta.
func validOperatorMeta(commandID, operatorID, policyID, evidenceDigest, reason string, expectedRevision uint64, wantReason string) bool {
	return validID(commandID) && validID(operatorID) && operatorPolicyID.MatchString(policyID) && hexDigest.MatchString(evidenceDigest) &&
		reason == wantReason && expectedRevision > 0 && expectedRevision <= maxWireInteger
}

// validOperatorConfirmation is a whole-second UTC confirmation after now.
func validOperatorConfirmation(expiresAt, now time.Time) bool {
	return canonicalUTCSecond(expiresAt) && now.Before(expiresAt)
}

// canonicalUTCSecond is a non-zero UTC time on a whole second, years 1..9999.
func canonicalUTCSecond(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC && value.Nanosecond() == 0 && value.Year() >= 1 && value.Year() <= 9999
}
