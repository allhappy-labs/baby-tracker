package security

import (
	"context"
	"time"
)

const maxCredentialIntentBytes = 64 << 10

type CredentialDeletionCASIntent struct{ Opaque []byte }
type CredentialDeletionCapability struct{ Opaque []byte }

type CredentialCleanupClaim struct {
	LeaseToken, OperationVersion, OperationID, CommandID string
	AccountID, ObjectID, ReferenceID, ReferenceKeyID     string
}

type CredentialCleanupAttempt struct {
	Claim         CredentialCleanupClaim
	Intent        CredentialDeletionCASIntent
	ValidateClaim func(context.Context, CredentialCleanupClaim) error
	Referenced    func(context.Context, CredentialCleanupClaim) (bool, error)
	MarkDeleting  func(context.Context, CredentialCleanupClaim, CredentialDeletionCASIntent) (CredentialDeletionCapability, error)
	Delete        func(context.Context, CredentialDeletionCapability) error
	ConfirmAbsent func(context.Context, CredentialDeletionCapability) (bool, error)
	Finish        func(context.Context, CredentialCleanupClaim) error
}

type AccountDeletionJobClaim struct {
	JobID, LeaseToken, OperationID, CommandID, OperationVersion string
	Status, AccountID, ServerInstanceID, Purpose, ObjectID      string
	ReferenceID, ReferenceKeyID                                 string
	Attempts                                                    int
	LeaseExpiresAt                                              time.Time
}

type AccountDeletionCredentialAttempt struct {
	Claim                AccountDeletionJobClaim
	Intent               CredentialDeletionCASIntent
	ValidateClaimedJob   func(context.Context, AccountDeletionJobClaim) error
	ReferencedOutsideJob func(context.Context, AccountDeletionJobClaim) (bool, error)
	MarkDeleting         func(context.Context, AccountDeletionJobClaim, CredentialDeletionCASIntent) (CredentialDeletionCapability, error)
}

func (service *Service) DeleteCredentialIfUnreferenced(ctx context.Context, attempt CredentialCleanupAttempt) (completed bool, err error) {
	if err := service.rejectCallbackEntry(ctx); err != nil {
		return false, err
	}
	if service == nil || service.fence == nil || !validCredentialCleanupAttempt(attempt) {
		return false, errorCode("invalid_command")
	}
	outsideFence := ctx
	callbackCtx := service.withCallbackDenial(ctx)
	release, err := service.fence.Write(ctx)
	if err != nil {
		return false, errorCode("security_busy")
	}
	released := false
	var lease *fenceLease
	defer func() {
		if !released {
			lease.invalidate()
			release()
		}
	}()
	ctx, lease = service.withFenceLease(ctx)
	if err := service.prepareCleanupFence(ctx); err != nil {
		return false, err
	}
	claim := attempt.Claim
	if err := attempt.ValidateClaim(callbackCtx, claim); err != nil {
		return false, err
	}
	for check := 0; check < 2; check++ {
		referenced, err := attempt.Referenced(callbackCtx, claim)
		if err != nil {
			return false, err
		}
		if referenced {
			return false, nil
		}
	}
	capability, err := attempt.MarkDeleting(callbackCtx, claim, cloneCredentialIntent(attempt.Intent))
	if err != nil {
		return false, err
	}
	if !validOpaqueCapability(capability) {
		return false, errorCode("service_unavailable")
	}
	lease.invalidate()
	release()
	released = true
	capability = cloneCredentialCapability(capability)
	if err := attempt.Delete(outsideFence, cloneCredentialCapability(capability)); err != nil {
		return false, err
	}
	absent, err := attempt.ConfirmAbsent(outsideFence, cloneCredentialCapability(capability))
	if err != nil {
		return false, err
	}
	if !absent {
		return false, errorCode("service_unavailable")
	}
	if err := attempt.Finish(outsideFence, claim); err != nil {
		return false, err
	}
	return true, nil
}

func (service *Service) MarkCredentialDeletingForAccountDeletion(ctx context.Context, attempt AccountDeletionCredentialAttempt) (CredentialDeletionCapability, error) {
	if err := service.rejectCallbackEntry(ctx); err != nil {
		return CredentialDeletionCapability{}, err
	}
	if service == nil || service.fence == nil || !validAccountDeletionAttempt(attempt) {
		return CredentialDeletionCapability{}, errorCode("invalid_command")
	}
	callbackCtx := service.withCallbackDenial(ctx)
	release, err := service.fence.Write(ctx)
	if err != nil {
		return CredentialDeletionCapability{}, errorCode("security_busy")
	}
	defer release()
	ctx, lease := service.withFenceLease(ctx)
	defer lease.invalidate()
	if err := service.prepareCleanupFence(ctx); err != nil {
		return CredentialDeletionCapability{}, err
	}
	claim := attempt.Claim
	if err := attempt.ValidateClaimedJob(callbackCtx, claim); err != nil {
		return CredentialDeletionCapability{}, err
	}
	for check := 0; check < 2; check++ {
		referenced, err := attempt.ReferencedOutsideJob(callbackCtx, claim)
		if err != nil {
			return CredentialDeletionCapability{}, err
		}
		if referenced {
			return CredentialDeletionCapability{}, errorCode("credential_conflict")
		}
	}
	capability, err := attempt.MarkDeleting(callbackCtx, claim, cloneCredentialIntent(attempt.Intent))
	if err != nil {
		return CredentialDeletionCapability{}, err
	}
	if !validOpaqueCapability(capability) {
		return CredentialDeletionCapability{}, errorCode("service_unavailable")
	}
	return cloneCredentialCapability(capability), nil
}

func (service *Service) prepareCleanupFence(ctx context.Context) error {
	if service.ledger == nil || service.heads == nil || service.projection == nil || service.fence == nil || service.epoch == nil {
		return errorCode("service_unavailable")
	}
	head, _, err := service.loadState(ctx)
	if err != nil {
		return err
	}
	if err := service.applyProjection(ctx, head); err != nil {
		service.setQuarantine(true)
		return errorCode("service_unavailable")
	}
	service.setQuarantine(false)
	epoch, err := service.epoch(ctx)
	if err != nil || epoch == 0 || epoch > maxWireInteger {
		return errorCode("service_unavailable")
	}
	return nil
}

func validCredentialCleanupAttempt(attempt CredentialCleanupAttempt) bool {
	return validCredentialCleanupClaim(attempt.Claim) && validOpaqueIntent(attempt.Intent) && attempt.ValidateClaim != nil && attempt.Referenced != nil &&
		attempt.MarkDeleting != nil && attempt.Delete != nil && attempt.ConfirmAbsent != nil && attempt.Finish != nil
}

func validCredentialCleanupClaim(claim CredentialCleanupClaim) bool {
	return validOpaque(claim.LeaseToken) && validOpaque(claim.OperationVersion) && validUUID(claim.OperationID) && validUUID(claim.CommandID) &&
		validUUID(claim.AccountID) && validUUID(claim.ObjectID) && validCredentialReference(claim.ReferenceID) && validCredentialReference(claim.ReferenceKeyID)
}

func validAccountDeletionAttempt(attempt AccountDeletionCredentialAttempt) bool {
	claim := attempt.Claim
	return validUUID(claim.JobID) && validOpaque(claim.LeaseToken) && validOpaque(claim.OperationVersion) && validUUID(claim.OperationID) &&
		validUUID(claim.CommandID) && (claim.Status == "pending" || claim.Status == "retry") && validUUID(claim.AccountID) && validUUID(claim.ServerInstanceID) &&
		claim.Purpose == "apple-refresh-revocation-v1" && validUUID(claim.ObjectID) && validCredentialReference(claim.ReferenceID) && validCredentialReference(claim.ReferenceKeyID) &&
		claim.Attempts > 0 && validCredentialClaimTime(claim.LeaseExpiresAt) &&
		validOpaqueIntent(attempt.Intent) && attempt.ValidateClaimedJob != nil && attempt.ReferencedOutsideJob != nil && attempt.MarkDeleting != nil
}

func validCredentialClaimTime(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC && value.Year() >= 1 && value.Year() <= 9999
}

func validOpaqueIntent(intent CredentialDeletionCASIntent) bool {
	return len(intent.Opaque) > 0 && len(intent.Opaque) <= maxCredentialIntentBytes
}

func validOpaqueCapability(capability CredentialDeletionCapability) bool {
	return len(capability.Opaque) > 0 && len(capability.Opaque) <= maxCredentialIntentBytes
}

func validCredentialReference(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '.' && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func cloneCredentialIntent(intent CredentialDeletionCASIntent) CredentialDeletionCASIntent {
	return CredentialDeletionCASIntent{Opaque: append([]byte(nil), intent.Opaque...)}
}

func cloneCredentialCapability(capability CredentialDeletionCapability) CredentialDeletionCapability {
	return CredentialDeletionCapability{Opaque: append([]byte(nil), capability.Opaque...)}
}
