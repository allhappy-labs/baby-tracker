package family

import (
	"context"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"github.com/jackc/pgx/v5"
)

type RecoveryStatus struct {
	FamilyID   string    `json:"family_id"`
	State      string    `json:"state"`
	PurgeAfter time.Time `json:"purge_after"`
	CanRecover bool      `json:"can_recover"`
}

func recoveryStatusOf(status authority.RecoveryStatus) RecoveryStatus {
	return RecoveryStatus{FamilyID: status.FamilyID, State: status.State, PurgeAfter: status.PurgeAfter.UTC(), CanRecover: status.CanRecover}
}

// ScheduleDeletion schedules the owner's family for deletion and returns its
// recovery window. An exact retry returns the window of the family the
// original command scheduled, read now (legacy recoveryStatusFromState).
func (service *Service) ScheduleDeletion(ctx context.Context, principal access.Principal, commandID string, confirmed bool) (RecoveryStatus, error) {
	if !confirmed {
		return RecoveryStatus{}, ErrConfirmationRequired
	}
	if service == nil || service.authority == nil || !validUUID(commandID) {
		return RecoveryStatus{}, ErrInvalidRequest
	}
	result, err := service.authority.ScheduleDeletion(ctx, authority.ScheduleDeletion{CommandID: commandID, OwnerAccountID: principal.AccountID,
		Principal: principal})
	if err != nil {
		return RecoveryStatus{}, familyError(err)
	}
	status, err := service.authority.FamilyRecoveryStatus(ctx, principal.AccountID, result.FamilyID)
	if err != nil {
		return RecoveryStatus{}, familyError(err)
	}
	return recoveryStatusOf(status), nil
}

// RecoveryStatus is the deletion-pending family a live family_recovery
// session may recover. Every other caller, a dead or non-recovery session
// included, is recovery_only.
func (service *Service) RecoveryStatus(ctx context.Context, principal access.Principal) (RecoveryStatus, error) {
	if service == nil || service.authority == nil {
		return RecoveryStatus{}, ErrUnavailable
	}
	if err := service.requireRecoverySession(ctx, principal); err != nil {
		return RecoveryStatus{}, err
	}
	status, err := service.authority.RecoveryStatus(ctx, principal.AccountID)
	if err != nil {
		return RecoveryStatus{}, familyError(err)
	}
	return recoveryStatusOf(status), nil
}

// requireRecoverySession admits only a principal authenticated with the
// family_recovery scope whose session is still live and still recovery
// scoped (legacy liveLifecyclePrincipal). The authority's family read is the
// check: for a live recovery session it answers recovery_only, for a live
// session of any other scope it grants or refuses on family policy, and for
// a dead session it refuses before policy. Only an operational failure is
// reported as such; every other outcome is recovery_only, as legacy said.
func (service *Service) requireRecoverySession(ctx context.Context, principal access.Principal) error {
	if principal.Scope != "family_recovery" {
		return ErrRecoveryOnly
	}
	err := service.authority.WithFamilyReadTx(ctx, principal, "read", func(pgx.Tx, access.Grant) error { return nil })
	switch authority.CodeOf(err) {
	case "recovery_only":
		return nil
	case "service_unavailable", "scope_failed":
		return familyError(err)
	default:
		return ErrRecoveryOnly
	}
}

func (service *Service) Recover(ctx context.Context, principal access.Principal, commandID string, confirmed bool) (authority.Result, error) {
	return service.executeRecoveryLifecycle(ctx, principal, commandID, confirmed, authority.KindFamilyRecovered)
}

func (service *Service) Forfeit(ctx context.Context, principal access.Principal, commandID string, confirmed bool) (authority.Result, error) {
	return service.executeRecoveryLifecycle(ctx, principal, commandID, confirmed, authority.KindRecoveryForfeited)
}

// executeRecoveryLifecycle recovers or forfeits the family the caller's
// recovery session names. As in the legacy service, an exact retry is
// answered first (from any later session of the account), and only a first
// attempt must pass RecoveryStatus before the command runs.
func (service *Service) executeRecoveryLifecycle(ctx context.Context, principal access.Principal, commandID string, confirmed bool, kind authority.Kind) (authority.Result, error) {
	if !confirmed {
		return authority.Result{}, ErrConfirmationRequired
	}
	if service == nil || service.authority == nil || !validUUID(commandID) {
		return authority.Result{}, ErrInvalidRequest
	}
	execute := func() (authority.Result, error) {
		if kind == authority.KindFamilyRecovered {
			return commandResult(service.authority.RecoverFamily(ctx, authority.RecoverFamily{CommandID: commandID, AccountID: principal.AccountID,
				Principal: principal}))
		}
		return commandResult(service.authority.ForfeitRecovery(ctx, authority.ForfeitRecovery{CommandID: commandID, AccountID: principal.AccountID,
			Principal: principal}))
	}
	if validUUID(principal.AccountID) {
		_, found, err := service.authority.CommandReceipt(ctx, principal.AccountID, commandID)
		if err != nil {
			return authority.Result{}, familyError(err)
		}
		if found {
			return execute()
		}
	}
	if _, err := service.RecoveryStatus(ctx, principal); err != nil {
		return authority.Result{}, err
	}
	return execute()
}
