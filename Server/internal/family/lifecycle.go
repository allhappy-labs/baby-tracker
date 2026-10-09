package family

import (
	"context"
	"encoding/json"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/security"
)

type RecoveryStatus struct {
	FamilyID   string    `json:"family_id"`
	State      string    `json:"state"`
	PurgeAfter time.Time `json:"purge_after"`
	CanRecover bool      `json:"can_recover"`
}

func (service *Service) ScheduleDeletion(ctx context.Context, principal access.Principal, commandID string, confirmed bool) (RecoveryStatus, error) {
	if !confirmed {
		return RecoveryStatus{}, ErrConfirmationRequired
	}
	if service == nil || service.security == nil || !validUUID(commandID) {
		return RecoveryStatus{}, ErrInvalidRequest
	}
	var receipt security.ReceiptView
	var replay bool
	var status RecoveryStatus
	var slot security.FamilySlotView
	var members []security.MembershipView
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		receipt, replay = view.Receipt(principal.AccountID, commandID)
		if replay {
			if receipt.Kind != "family_deletion_scheduled" || len(receipt.FamilyIDs) != 1 {
				return ErrCommandIDReused
			}
			var err error
			status, err = service.recoveryStatusFromState(view, principal.AccountID, receipt.FamilyIDs[0])
			return err
		}
		var ok bool
		slot, ok = view.FamilySlot(principal.AccountID)
		if !ok || slot.State != "active" {
			return ErrFamilyUnavailable
		}
		members = view.Memberships(slot.FamilyID)
		return nil
	})
	if err != nil {
		return RecoveryStatus{}, familyError(err)
	}
	if replay {
		return status, nil
	}
	if _, err := service.Authorize(ctx, principal, slot.FamilyID, "delete_family"); err != nil {
		return RecoveryStatus{}, err
	}
	memberIDs, roles := make([]string, 0, len(members)), make([]string, 0, len(members))
	for _, member := range members {
		if member.Status == "active" {
			memberIDs, roles = append(memberIDs, member.AccountID), append(roles, member.Role)
		}
	}
	payload := security.FamilyLifecyclePayload{Version: "v1", FamilyID: slot.FamilyID, ClaimOwnerAccountID: principal.AccountID,
		PreviousMemberIDs: memberIDs, PreviousRoles: roles}
	raw, _ := json.Marshal(payload)
	command := security.Command{ID: commandID, Kind: "family_deletion_scheduled", AffectedAccountIDs: memberIDs,
		AffectedFamilyIDs: []string{slot.FamilyID}, Payload: raw}
	if _, err := service.security.Execute(ctx, principal, command); err != nil {
		return RecoveryStatus{}, familyError(err)
	}
	err = service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		var err error
		status, err = service.recoveryStatusFromState(view, principal.AccountID, slot.FamilyID)
		return err
	})
	return status, familyErrorOrNil(err)
}

func (service *Service) RecoveryStatus(ctx context.Context, principal access.Principal) (RecoveryStatus, error) {
	if service == nil || service.security == nil {
		return RecoveryStatus{}, ErrUnavailable
	}
	var status RecoveryStatus
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		if principal.Scope != "family_recovery" || !liveLifecyclePrincipal(view, principal, service.now().UTC()) {
			return ErrRecoveryOnly
		}
		slot, ok := view.FamilySlot(principal.AccountID)
		if !ok || slot.State != "family_recovery" {
			return ErrRecoveryOnly
		}
		var err error
		status, err = service.recoveryStatusFromState(view, principal.AccountID, slot.FamilyID)
		return err
	})
	return status, familyErrorOrNil(err)
}

func (service *Service) Recover(ctx context.Context, principal access.Principal, commandID string, confirmed bool) (security.CommandResult, error) {
	return service.executeRecoveryLifecycle(ctx, principal, commandID, confirmed, "family_recovered")
}

func (service *Service) Forfeit(ctx context.Context, principal access.Principal, commandID string, confirmed bool) (security.CommandResult, error) {
	return service.executeRecoveryLifecycle(ctx, principal, commandID, confirmed, "recovery_forfeited")
}

func (service *Service) executeRecoveryLifecycle(ctx context.Context, principal access.Principal, commandID string, confirmed bool, kind security.EventKind) (security.CommandResult, error) {
	if !confirmed {
		return security.CommandResult{}, ErrConfirmationRequired
	}
	if service == nil || service.security == nil || !validUUID(commandID) {
		return security.CommandResult{}, ErrInvalidRequest
	}
	var receipt security.ReceiptView
	var exists bool
	err := service.security.WithAuthorityView(ctx, func(view security.AuthorityView) error {
		receipt, exists = view.Receipt(principal.AccountID, commandID)
		return nil
	})
	if err != nil {
		return security.CommandResult{}, familyError(err)
	}
	if exists {
		if receipt.Kind != kind || len(receipt.FamilyIDs) != 1 {
			return security.CommandResult{}, ErrCommandIDReused
		}
		command := recoveryLifecycleCommand(commandID, kind, principal.AccountID, receipt.FamilyIDs[0])
		hash, hashErr := security.CanonicalCommandHash(principal, command)
		if hashErr != nil || hash != receipt.CommandHash {
			return security.CommandResult{}, ErrCommandIDReused
		}
		return receipt.Result, nil
	}
	status, err := service.RecoveryStatus(ctx, principal)
	if err != nil {
		return security.CommandResult{}, err
	}
	command := recoveryLifecycleCommand(commandID, kind, principal.AccountID, status.FamilyID)
	result, err := service.security.Execute(ctx, principal, command)
	return result, familyErrorOrNil(err)
}

func recoveryLifecycleCommand(commandID string, kind security.EventKind, accountID, familyID string) security.Command {
	payload := security.FamilyLifecyclePayload{Version: "v1", FamilyID: familyID, ClaimOwnerAccountID: accountID}
	raw, _ := json.Marshal(payload)
	return security.Command{ID: commandID, Kind: kind, AffectedAccountIDs: []string{accountID}, AffectedFamilyIDs: []string{familyID}, Payload: raw}
}

func (service *Service) recoveryStatusFromState(state security.AuthorityView, accountID, familyID string) (RecoveryStatus, error) {
	family, familyOK := state.Family(familyID)
	claim, claimOK := state.RecoveryClaim(familyID, accountID)
	if !familyOK || !claimOK || family.Status != "deletion_pending" {
		if claimOK && claim.Status == "forfeited" {
			return RecoveryStatus{}, ErrRecoveryForfeited
		}
		return RecoveryStatus{}, ErrFamilyUnavailable
	}
	purgeAfter, err := time.Parse(time.RFC3339Nano, family.PurgeAfter)
	if err != nil {
		return RecoveryStatus{}, ErrUnavailable
	}
	canRecover := claim.Status == "active" && service.now().UTC().Before(purgeAfter)
	return RecoveryStatus{FamilyID: familyID, State: family.Status, PurgeAfter: purgeAfter, CanRecover: canRecover}, nil
}

func liveLifecyclePrincipal(state security.AuthorityView, principal access.Principal, now time.Time) bool {
	account, accountOK := state.Account(principal.AccountID)
	device, deviceOK := state.Device(principal.DeviceID)
	session, sessionOK := state.Session(principal.SessionID)
	expiresAt, err := time.Parse(time.RFC3339Nano, session.ExpiresAt)
	return accountOK && account.Status == "active" && deviceOK && device.Status == "active" && device.AccountID == principal.AccountID &&
		sessionOK && session.Status == "active" && session.AccountID == principal.AccountID && session.DeviceID == principal.DeviceID &&
		session.RecoveryEpoch == principal.RecoveryEpoch && session.SecurityRevision == principal.SecurityRevision && err == nil && now.Before(expiresAt)
}
