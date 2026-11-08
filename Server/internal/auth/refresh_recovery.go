package auth

import (
	"context"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/identity"
)

// RecoverableRefreshInput is a recoverable refresh. Subject is optional: the
// subject ResolveRecoverableRefreshSubject returned for this input, whose
// session RefreshRecoverable then rotates without looking it up again (the
// rotation still locks and re-reads it). A subject resolved for another
// refresh token is refused.
type RecoverableRefreshInput struct {
	RefreshToken, AttemptID string
	DeviceContinuity        identity.DeviceContinuityClaim
	Subject                 RefreshSubject
}
type RecoveredSessionBundle struct {
	SessionBundle
	RefreshAttemptID string
}

var ErrRefreshAttemptConflict = codedError("refresh_attempt_conflict")
var ErrRefreshReceiptUnavailable = codedError("refresh_receipt_unavailable")

// ResolveRecoverableRefreshSubject returns the quota subject of a
// recoverable refresh without consuming it: the refresh token's account and
// device (ResolveRefreshSubject's refusals) once the device proof matches
// that device. Whether the rotation itself is still allowed (scope,
// entitlement, receipt) is decided by RefreshRecoverable, which takes the
// returned subject (RecoverableRefreshInput.Subject) to skip the lookup.
func (service *Service) ResolveRecoverableRefreshSubject(ctx context.Context, input RecoverableRefreshInput) (RefreshSubject, error) {
	if !service.validRecoveryRequest(input) {
		return RefreshSubject{}, ErrInvalidRequest
	}
	parent, err := service.recoveryContinuity(ctx, input)
	if err != nil {
		return RefreshSubject{}, err
	}
	device, _ := parent.Device()
	return RefreshSubject{AccountID: device.AccountID, DeviceID: device.DeviceID, parent: parent}, nil
}

// RefreshRecoverable rotates a refresh token under a client attempt ID and
// device proof, in one authority transaction: the proof is recomputed under
// the device row's stored continuity key inside it. A lost-acknowledgement
// retry of the same attempt returns the committed child unchanged; another
// attempt on a consumed parent revokes the lineage
// (refresh_attempt_conflict). A wrong device proof is
// device_continuity_invalid and never revokes.
func (service *Service) RefreshRecoverable(ctx context.Context, input RecoverableRefreshInput) (RecoveredSessionBundle, error) {
	if !service.validRecoveryRequest(input) {
		return RecoveredSessionBundle{}, ErrInvalidRequest
	}
	claim := input.DeviceContinuity
	command := service.refreshCommand(input.RefreshToken, input.AttemptID,
		func(stored authority.DeviceBinding) bool { return service.continuityMatches(stored, &claim) })
	command.Parent = input.Subject.parent
	result, err := service.authority.Refresh(ctx, command)
	if err != nil {
		return RecoveredSessionBundle{}, err
	}
	return RecoveredSessionBundle{SessionBundle: refreshBundle(result), RefreshAttemptID: input.AttemptID}, nil
}

func (service *Service) validRecoveryRequest(input RecoverableRefreshInput) bool {
	return service != nil && service.authority != nil && validBearerSecret(input.RefreshToken) && validUUID(input.AttemptID) &&
		input.AttemptID != "00000000-0000-4000-8000-000000000000"
}

// recoveryContinuity proves the request's device claim against the device
// the refresh token's session is bound to, for quota charging only.
func (service *Service) recoveryContinuity(ctx context.Context, input RecoverableRefreshInput) (*authority.RefreshParent, error) {
	parent, err := service.authority.ResolveRefreshParent(ctx, service.tokenCandidates("refresh", input.RefreshToken))
	if err != nil {
		return nil, err
	}
	device, status := parent.Device()
	if status != "active" || !service.continuityMatches(device, &input.DeviceContinuity) {
		return nil, ErrDeviceContinuityInvalid
	}
	return parent, nil
}
