package security

import (
	"context"
	"encoding/json"
	"time"

	"babytracker/server/internal/access"
)

type OperatorRecoveryInput struct {
	CommandID, OperatorID                          string
	FamilyID, OldOwnerAccountID, NewOwnerAccountID string
	PolicyID, EvidenceDigest, ReasonCode           string
	ExpectedRevision                               uint64
	ConfirmationExpiresAt                          time.Time
}

type OperatorEntitlementInput struct {
	CommandID, OperatorID                string
	FamilyID, Operation, Key, Source     string
	ExpiresAt                            time.Time
	PolicyID, EvidenceDigest, ReasonCode string
	ExpectedRevision                     uint64
	ConfirmationExpiresAt                time.Time
}

// OperatorAuthorityGuard revalidates the configured recovery identity, epoch,
// trusted manifest, and mirror while the security authority write fence is
// held. Implementations must not reacquire that fence.
type OperatorAuthorityGuard interface {
	ValidateOperatorAuthorityUnderFence(context.Context, Head) error
}

func (service *Service) PreflightOperatorRecovery(ctx context.Context, input OperatorRecoveryInput) error {
	return service.WithAuthoritySnapshot(ctx, func(snapshot AuthoritySnapshot) error {
		prepared, err := snapshot.newOperatorRecovery(input, service.now().UTC())
		if err != nil {
			return err
		}
		if replayed, err := snapshot.preparedReceipt(prepared); err != nil || replayed {
			return err
		}
		if input.ExpectedRevision != snapshot.Head.Revision {
			return errorCode("stale_security_revision")
		}
		_, err = preparedEffects(snapshot.token.owner.state, prepared)
		return err
	})
}

func (service *Service) PreflightOperatorEntitlement(ctx context.Context, input OperatorEntitlementInput) error {
	return service.WithAuthoritySnapshot(ctx, func(snapshot AuthoritySnapshot) error {
		prepared, err := snapshot.newOperatorEntitlement(input, service.now().UTC())
		if err != nil {
			return err
		}
		if replayed, err := snapshot.preparedReceipt(prepared); err != nil || replayed {
			return err
		}
		if input.ExpectedRevision != snapshot.Head.Revision {
			return errorCode("stale_security_revision")
		}
		_, err = preparedEffects(snapshot.token.owner.state, prepared)
		return err
	})
}

func (service *Service) ReplayOperatorRecovery(ctx context.Context, input OperatorRecoveryInput, guard OperatorAuthorityGuard) (CommandResult, bool, error) {
	return service.replayOperator(ctx, guard, func(snapshot AuthoritySnapshot) (PreparedCommand, error) {
		return snapshot.newOperatorRecovery(input, service.now().UTC())
	})
}

func (service *Service) ReplayOperatorEntitlement(ctx context.Context, input OperatorEntitlementInput, guard OperatorAuthorityGuard) (CommandResult, bool, error) {
	return service.replayOperator(ctx, guard, func(snapshot AuthoritySnapshot) (PreparedCommand, error) {
		return snapshot.newOperatorEntitlement(input, service.now().UTC())
	})
}

func (service *Service) replayOperator(ctx context.Context, guard OperatorAuthorityGuard, build func(AuthoritySnapshot) (PreparedCommand, error)) (CommandResult, bool, error) {
	if err := service.rejectCallbackEntry(ctx); err != nil {
		return CommandResult{}, false, err
	}
	if service == nil || service.ledger == nil || service.heads == nil || service.projection == nil || service.fence == nil || service.epoch == nil || guard == nil || build == nil {
		return CommandResult{}, false, errorCode("service_unavailable")
	}
	release, err := service.fence.Write(ctx)
	if err != nil {
		return CommandResult{}, false, errorCode("security_busy")
	}
	defer release()
	ctx, lease := service.withFenceLease(ctx)
	defer lease.invalidate()
	head, state, err := service.loadState(ctx)
	if err != nil {
		return CommandResult{}, false, err
	}
	if err := guard.ValidateOperatorAuthorityUnderFence(ctx, head); err != nil {
		service.setQuarantine(true)
		return CommandResult{}, false, errorCode("service_unavailable")
	}
	epoch, err := service.epoch(ctx)
	if err != nil || epoch == 0 || epoch > maxWireInteger {
		return CommandResult{}, false, errorCode("service_unavailable")
	}
	owner := lease.material
	if owner == nil {
		owner = &validatedAuthority{state: state, head: head, binding: AuthorityBinding{Epoch: epoch, TrustedHead: Head{Generation: 1}}, keys: state.ReferencedCredentialKeyIDs()}
	}
	if owner.binding.Epoch != epoch {
		return CommandResult{}, false, ErrUnavailable
	}
	snapshot := newLeasedAuthoritySnapshot(owner, lease, service.snapshotID.Add(1))
	prepared, err := build(snapshot)
	if err != nil {
		return CommandResult{}, false, err
	}
	replayed, err := snapshot.preparedReceipt(prepared)
	if err != nil || !replayed {
		return CommandResult{}, replayed, err
	}
	receipt, ok := state.Receipt(prepared.principal.AccountID, prepared.command.ID)
	if !ok {
		return CommandResult{}, false, errorCode("service_unavailable")
	}
	if err := service.applyProjection(ctx, head); err != nil {
		service.setQuarantine(true)
		return CommandResult{}, false, errorCode("service_unavailable")
	}
	service.setQuarantine(false)
	return receipt.Result, true, nil
}

func (service *Service) ExecuteOperatorRecovery(ctx context.Context, input OperatorRecoveryInput, guard OperatorAuthorityGuard) (CommandResult, error) {
	if guard == nil {
		return CommandResult{}, errorCode("service_unavailable")
	}
	return service.executePrepared(ctx, guard, func(_ context.Context, snapshot AuthoritySnapshot) (PreparedCommand, error) {
		return snapshot.prepareOperatorRecovery(input, service.now().UTC())
	})
}

func (service *Service) ExecuteOperatorEntitlement(ctx context.Context, input OperatorEntitlementInput, guard OperatorAuthorityGuard) (CommandResult, error) {
	if guard == nil {
		return CommandResult{}, errorCode("service_unavailable")
	}
	return service.executePrepared(ctx, guard, func(_ context.Context, snapshot AuthoritySnapshot) (PreparedCommand, error) {
		return snapshot.prepareOperatorEntitlement(input, service.now().UTC())
	})
}

func (snapshot AuthoritySnapshot) prepareOperatorRecovery(input OperatorRecoveryInput, now time.Time) (PreparedCommand, error) {
	prepared, err := snapshot.newOperatorRecovery(input, now)
	if err != nil {
		return PreparedCommand{}, err
	}
	if replay, err := snapshot.preparedReceipt(prepared); err != nil || replay {
		return prepared, err
	}
	if !validOperatorConfirmation(input.ConfirmationExpiresAt, now) {
		return PreparedCommand{}, errorCode("invalid_confirmation")
	}
	if input.ExpectedRevision != snapshot.Head.Revision {
		return PreparedCommand{}, errorCode("stale_security_revision")
	}
	return snapshot.finishPrepared(prepared.principal, prepared.command, now, preparedOperatorRecovery)
}

func (snapshot AuthoritySnapshot) prepareOperatorEntitlement(input OperatorEntitlementInput, now time.Time) (PreparedCommand, error) {
	prepared, err := snapshot.newOperatorEntitlement(input, now)
	if err != nil {
		return PreparedCommand{}, err
	}
	if replay, err := snapshot.preparedReceipt(prepared); err != nil || replay {
		return prepared, err
	}
	if !validOperatorConfirmation(input.ConfirmationExpiresAt, now) {
		return PreparedCommand{}, errorCode("invalid_confirmation")
	}
	if input.ExpectedRevision != snapshot.Head.Revision {
		return PreparedCommand{}, errorCode("stale_security_revision")
	}
	return snapshot.finishPrepared(prepared.principal, prepared.command, now, prepared.kind)
}

func (snapshot AuthoritySnapshot) newOperatorRecovery(input OperatorRecoveryInput, now time.Time) (PreparedCommand, error) {
	if !validOperatorMeta(input.CommandID, input.OperatorID, input.PolicyID, input.EvidenceDigest, input.ReasonCode, input.ExpectedRevision, "owner_identity_unavailable") ||
		!validUUID(input.FamilyID) || !validUUID(input.OldOwnerAccountID) || !validUUID(input.NewOwnerAccountID) || input.OldOwnerAccountID == input.NewOwnerAccountID {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	payload := OwnerChangePayload{Version: "v1", FamilyID: input.FamilyID, OldOwnerAccountID: input.OldOwnerAccountID, NewOwnerAccountID: input.NewOwnerAccountID,
		OperatorPolicyID: input.PolicyID, OperatorEvidenceDigest: input.EvidenceDigest, OperatorReasonCode: input.ReasonCode, ExpectedRevision: input.ExpectedRevision}
	raw, _ := json.Marshal(payload)
	principal := operatorPrincipal(snapshot, input.OperatorID)
	command := Command{ID: input.CommandID, Kind: "owner_recovered", AffectedAccountIDs: []string{input.OldOwnerAccountID, input.NewOwnerAccountID},
		AffectedFamilyIDs: []string{input.FamilyID}, Payload: raw}
	return snapshot.newPrepared(principal, command, now, preparedOperatorRecovery), nil
}

func (snapshot AuthoritySnapshot) newOperatorEntitlement(input OperatorEntitlementInput, now time.Time) (PreparedCommand, error) {
	wantReason, status, kind, preparedKind := "beta_extension", "active", EventKind("operator_entitlement_granted"), preparedOperatorEntitlementGrant
	if input.Operation == "suspend" {
		wantReason, status, kind, preparedKind = "abuse_suspension", "suspended", "operator_entitlement_suspended", preparedOperatorEntitlementSuspend
	} else if input.Operation != "grant" {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	if !validOperatorMeta(input.CommandID, input.OperatorID, input.PolicyID, input.EvidenceDigest, input.ReasonCode, input.ExpectedRevision, wantReason) ||
		!validUUID(input.FamilyID) || input.Key != "family_sync" || input.Source != "hosted_beta" || !canonicalUTCSecond(input.ExpiresAt) {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	payload := OperatorEntitlementPayload{Version: "v1", FamilyID: input.FamilyID, EntitlementKey: input.Key, Source: input.Source, Status: status,
		ExpiresAt: input.ExpiresAt.Format(time.RFC3339), OperatorPolicyID: input.PolicyID, OperatorEvidenceDigest: input.EvidenceDigest,
		OperatorReasonCode: input.ReasonCode, ExpectedRevision: input.ExpectedRevision}
	raw, _ := json.Marshal(payload)
	principal := operatorPrincipal(snapshot, input.OperatorID)
	command := Command{ID: input.CommandID, Kind: kind, AffectedFamilyIDs: []string{input.FamilyID}, Payload: raw}
	return snapshot.newPrepared(principal, command, now, preparedKind), nil
}

func validOperatorMeta(commandID, operatorID, policyID, evidenceDigest, reason string, expectedRevision uint64, wantReason string) bool {
	return validUUID(commandID) && validUUID(operatorID) && operatorPolicyID.MatchString(policyID) && canonicalHash.MatchString(evidenceDigest) &&
		reason == wantReason && expectedRevision > 0 && expectedRevision <= maxWireInteger
}

func operatorPrincipal(snapshot AuthoritySnapshot, operatorID string) access.Principal {
	return access.Principal{AccountID: operatorID, RecoveryEpoch: snapshot.RecoveryEpoch, SecurityRevision: snapshot.Head.Revision, Scope: "operator"}
}

func validOperatorConfirmation(expiresAt, now time.Time) bool {
	return canonicalUTCSecond(expiresAt) && now.Before(expiresAt)
}

func canonicalUTCSecond(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC && value.Nanosecond() == 0 && value.Year() >= 1 && value.Year() <= 9999
}

func operatorRecoveryEffects(state State, payload OwnerChangePayload) ([]Effect, error) {
	family, familyOK := state.families[payload.FamilyID]
	oldAccount, oldAccountOK := state.accounts[payload.OldOwnerAccountID]
	newAccount, newAccountOK := state.accounts[payload.NewOwnerAccountID]
	oldMembership, oldMemberOK := state.memberships[payload.FamilyID+"/"+payload.OldOwnerAccountID]
	newMembership, newMemberOK := state.memberships[payload.FamilyID+"/"+payload.NewOwnerAccountID]
	if !familyOK || family.Status != "active" || family.OwnerAccountID != payload.OldOwnerAccountID || !oldAccountOK || oldAccount.Status != "identity_unavailable" ||
		!newAccountOK || newAccount.Status != "active" || !oldMemberOK || oldMembership.Status != "active" || oldMembership.Role != "owner" ||
		!newMemberOK || newMembership.Status != "active" || newMembership.Role != "caregiver" {
		return nil, errorCode("owner_identity_unavailable")
	}
	revision := payload.ExpectedRevision
	audit := func(accountID, role string) Effect {
		return Effect{Kind: "owner_recovered", AccountID: accountID, FamilyID: payload.FamilyID, Role: role, Status: "active",
			OperatorPolicyID: payload.OperatorPolicyID, OperatorEvidenceDigest: payload.OperatorEvidenceDigest,
			OperatorReasonCode: payload.OperatorReasonCode, ExpectedRevision: &revision}
	}
	return []Effect{audit(payload.OldOwnerAccountID, "caregiver"), audit(payload.NewOwnerAccountID, "owner")}, nil
}

func operatorEntitlementEffects(state State, payload OperatorEntitlementPayload, at time.Time) ([]Effect, error) {
	family, familyOK := state.families[payload.FamilyID]
	if !familyOK || family.Status != "active" {
		return nil, errorCode("family_unavailable")
	}
	nextExpiry, err := time.Parse(time.RFC3339Nano, payload.ExpiresAt)
	if err != nil || nextExpiry.Location() != time.UTC || nextExpiry.Format(time.RFC3339Nano) != payload.ExpiresAt {
		return nil, errorCode("invalid_command")
	}
	current, exists := state.entitlements[payload.FamilyID+"/"+payload.EntitlementKey]
	switch payload.Status {
	case "active":
		currentExpiry, currentErr := time.Parse(time.RFC3339Nano, current.ExpiresAt)
		if !nextExpiry.After(at) || (exists && (current.Status != "active" || current.Source != "hosted_beta" || currentErr != nil || !nextExpiry.After(currentExpiry))) {
			return nil, errorCode("invalid_command")
		}
	case "suspended":
		if !exists || current.Status != "active" || current.Source != "hosted_beta" || current.ExpiresAt != payload.ExpiresAt {
			return nil, errorCode("invalid_command")
		}
	default:
		return nil, errorCode("invalid_command")
	}
	revision := payload.ExpectedRevision
	return []Effect{{Kind: EventKind(payloadStatusKind(payload.Status)), FamilyID: payload.FamilyID, EntitlementKey: payload.EntitlementKey,
		Source: payload.Source, Status: payload.Status, ExpiresAt: payload.ExpiresAt, OperatorPolicyID: payload.OperatorPolicyID,
		OperatorEvidenceDigest: payload.OperatorEvidenceDigest, OperatorReasonCode: payload.OperatorReasonCode, ExpectedRevision: &revision}}, nil
}

func payloadStatusKind(status string) string {
	if status == "active" {
		return "operator_entitlement_granted"
	}
	return "operator_entitlement_suspended"
}
