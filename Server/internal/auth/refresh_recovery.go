package auth

import (
	"context"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/identity"
)

type RecoverableRefreshInput struct {
	RefreshToken, AttemptID string
	DeviceContinuity        identity.DeviceContinuityClaim
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
// entitlement, receipt) is decided by RefreshRecoverable.
func (service *Service) ResolveRecoverableRefreshSubject(ctx context.Context, input RecoverableRefreshInput) (RefreshSubject, error) {
	if !service.validRecoveryRequest(input) {
		return RefreshSubject{}, ErrInvalidRequest
	}
	device, err := service.recoveryContinuity(ctx, input)
	if err != nil {
		return RefreshSubject{}, err
	}
	return RefreshSubject{AccountID: device.AccountID, DeviceID: device.DeviceID}, nil
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
	result, err := service.authority.Refresh(ctx, service.refreshCommand(input.RefreshToken, input.AttemptID,
		func(stored authority.DeviceBinding) bool { return service.continuityMatches(stored, &claim) }))
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
func (service *Service) recoveryContinuity(ctx context.Context, input RecoverableRefreshInput) (authority.DeviceBinding, error) {
	device, status, err := service.authority.RefreshDevice(ctx, service.tokenCandidates("refresh", input.RefreshToken))
	if err != nil {
		return authority.DeviceBinding{}, err
	}
	if status != "active" || !service.continuityMatches(device, &input.DeviceContinuity) {
		return authority.DeviceBinding{}, ErrDeviceContinuityInvalid
	}
	return device, nil
}
