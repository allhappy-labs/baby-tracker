package account

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strconv"
	"sync"
	"time"

	"babytracker/server/internal/identity"
	"babytracker/server/internal/security"
)

const credentialInventoryPageSize = 100

var (
	workerIDPattern         = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	operationVersionPattern = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)
)

type AppleRevocationJob struct {
	JobID, LeaseToken, OperationVersion, OperationID, CommandID, Status string
	AccountID, ServerInstanceID, Purpose, ObjectID                      string
	ReferenceID, ReferenceKeyID                                         string
	Attempts                                                            int
	LeaseExpiresAt                                                      time.Time
}

type OrphanCleanupJob struct {
	Claim   security.CredentialCleanupClaim
	Binding identity.CredentialBinding
	Ref     identity.CredentialRef
}

type AppleRevocationRepository interface {
	Claim(context.Context, string, time.Time, time.Time) (AppleRevocationJob, bool, error)
	ValidateClaim(context.Context, AppleRevocationJob, time.Time) error
	CredentialReferencedOutsideJob(context.Context, AppleRevocationJob, time.Time) (bool, error)
	MarkProviderRevoked(context.Context, AppleRevocationJob, time.Time) error
	BeginCleanup(context.Context, AppleRevocationJob, time.Time) error
	Complete(context.Context, AppleRevocationJob, time.Time) error
	Retry(context.Context, AppleRevocationJob, string, time.Time, time.Time) error
	ClaimUnfinishedOrphanCleanup(context.Context, string, time.Time, time.Time) (OrphanCleanupJob, bool, error)
	ClaimOrphanCleanup(context.Context, identity.CredentialBinding, identity.CredentialRef, string, time.Time, time.Time) (security.CredentialCleanupClaim, bool, error)
	ValidateOrphanCleanup(context.Context, security.CredentialCleanupClaim, time.Time) error
	CredentialReferenced(context.Context, security.CredentialCleanupClaim, time.Time) (bool, error)
	FinishOrphanCleanup(context.Context, security.CredentialCleanupClaim, time.Time) error
	RetryOrphanCleanup(context.Context, security.CredentialCleanupClaim, string, time.Time, time.Time) error
}

type credentialAuthority interface {
	MarkAccountDeleting(context.Context, security.AccountDeletionCredentialAttempt) (security.CredentialDeletionCapability, error)
	DeleteOrphan(context.Context, security.CredentialCleanupAttempt) (bool, error)
	Receipt(context.Context, string, string) ([32]byte, bool, error)
}

type AppleRevocationWorker struct {
	WorkerID           string
	Repository         AppleRevocationRepository
	Vault              identity.CredentialVault
	Rotations          identity.CredentialRotator
	Inventory          identity.CredentialInventory
	Apple              identity.AppleRevoker
	Security           *security.Service
	Now                func() time.Time
	Lease, OrphanGrace time.Duration

	mu              sync.Mutex
	inventoryCursor string
	authority       credentialAuthority
}

func (worker *AppleRevocationWorker) RunOnce(ctx context.Context) error {
	if err := worker.validate(); err != nil {
		return err
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	var maintenanceErr error
	if err := worker.resumeUnfinishedOrphanCleanup(ctx); err != nil {
		maintenanceErr = err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := worker.reconcileInventory(ctx); err != nil && maintenanceErr == nil {
		maintenanceErr = err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	now := worker.Now().UTC()
	job, ok, err := worker.Repository.Claim(ctx, worker.WorkerID, now, now.Add(worker.Lease))
	if err != nil {
		return workerPublicError(err)
	}
	if !ok {
		return workerPublicError(maintenanceErr)
	}
	if !validAppleRevocationJob(job, now) {
		return ErrUnavailable
	}
	if err := worker.runClaimedJob(ctx, job); err != nil {
		if errors.Is(err, ErrStaleLease) || ctx.Err() != nil {
			return workerPublicError(err)
		}
		retryNow, timeErr := worker.currentTime()
		if timeErr != nil {
			return workerPublicError(timeErr)
		}
		if retryErr := worker.Repository.Retry(ctx, job, safeWorkerErrorCode(err), nextWorkerAttempt(job.Attempts, retryNow), retryNow); retryErr != nil {
			return workerPublicError(retryErr)
		}
	}
	return workerPublicError(maintenanceErr)
}

func (worker *AppleRevocationWorker) validate() error {
	if worker == nil || !workerIDPattern.MatchString(worker.WorkerID) || worker.Repository == nil || worker.Vault == nil ||
		worker.Rotations == nil || worker.Inventory == nil || worker.Apple == nil || worker.Now == nil ||
		worker.Lease < 10*time.Second || worker.Lease > 5*time.Minute || worker.OrphanGrace < 11*time.Minute {
		return ErrInvalidRequest
	}
	if worker.authority == nil && worker.Security == nil {
		return ErrInvalidRequest
	}
	now := worker.Now().UTC()
	if !validWorkerTime(now) {
		return ErrInvalidRequest
	}
	return nil
}

func (worker *AppleRevocationWorker) credentialAuthority() credentialAuthority {
	if worker.authority != nil {
		return worker.authority
	}
	return securityCredentialAuthority{service: worker.Security}
}

func (worker *AppleRevocationWorker) reconcileInventory(ctx context.Context) error {
	now := worker.Now().UTC()
	items, next, err := worker.Inventory.List(ctx, now, worker.inventoryCursor, credentialInventoryPageSize)
	if err != nil {
		return err
	}
	if next != "" && next == worker.inventoryCursor {
		return ErrUnavailable
	}
	worker.inventoryCursor = next
	for _, metadata := range items {
		if err := worker.reconcileCredential(ctx, metadata, now); err != nil {
			return err
		}
	}
	return nil
}

func (worker *AppleRevocationWorker) reconcileCredential(ctx context.Context, metadata identity.CredentialMetadata, now time.Time) error {
	resume, err := worker.Rotations.Resume(ctx, metadata.Binding)
	if err != nil {
		return err
	}
	state := resume.State
	if state.Phase == identity.CredentialPhaseAbsent {
		return nil
	}
	if state.Ref != metadata.Ref {
		return identity.ErrCredentialConflict
	}
	switch state.Phase {
	case identity.CredentialPhaseReady:
		if metadata.CreatedAt.After(now.Add(-worker.OrphanGrace)) {
			return nil
		}
		return worker.cleanupOrphan(ctx, metadata.Binding, state, now)
	case identity.CredentialPhaseReserved:
		if now.Before(state.ReconcileAfter) {
			return nil
		}
		if err := worker.Rotations.Abandon(ctx, metadata.Binding, state.OperationID); err != nil {
			return err
		}
		return worker.reconcileCredential(ctx, metadata, now)
	case identity.CredentialPhaseCandidate, identity.CredentialPhaseRetiring:
		_, err := worker.Rotations.RotateForSession(ctx, metadata.Binding, state.OperationID, state.CommandID,
			func(context.Context) (identity.AppleTokens, error) {
				return identity.AppleTokens{}, identity.ErrCredentialConflict
			})
		return err
	case identity.CredentialPhasePromoted:
		if now.Before(state.ReconcileAfter) {
			return nil
		}
		if err := worker.Rotations.Abandon(ctx, metadata.Binding, state.OperationID); err != nil {
			return err
		}
		return worker.reconcileCredential(ctx, metadata, now)
	case identity.CredentialPhaseAdopting:
		if err := worker.reconcileAdoption(ctx, metadata, state, now); err != nil {
			if errors.Is(err, identity.ErrCredentialRotationBusy) {
				return nil
			}
			return err
		}
		resolved, exists, err := worker.Vault.Resolve(ctx, metadata.Binding)
		if err != nil || !exists || resolved.Phase == identity.CredentialPhaseReady {
			return err
		}
		return worker.reconcileCredential(ctx, metadata, now)
	case identity.CredentialPhaseAborting:
		claimed, err := worker.Vault.ClaimMaintenance(ctx, metadata.Binding, state, worker.WorkerID, now.Add(worker.Lease))
		if err != nil {
			return err
		}
		tokens, err := worker.Vault.OpenMaintenanceTokens(ctx, metadata.Binding, claimed)
		if err != nil {
			return err
		}
		if err := worker.revokeTokens(ctx, metadata.Binding, tokens); err != nil {
			return err
		}
		_, err = worker.Vault.CompleteMaintenance(ctx, metadata.Binding, claimed)
		return err
	case identity.CredentialPhaseDeleting:
		return worker.cleanupOrphan(ctx, metadata.Binding, state, now)
	default:
		return identity.ErrCredentialConflict
	}
}

func (worker *AppleRevocationWorker) runClaimedJob(ctx context.Context, job AppleRevocationJob) error {
	binding := identity.CredentialBinding{AccountID: job.AccountID, ServerInstanceID: job.ServerInstanceID, Purpose: job.Purpose, ObjectID: job.ObjectID}
	ref := identity.CredentialRef{ID: job.ReferenceID, KeyID: job.ReferenceKeyID}
	switch job.Status {
	case "pending", "retry":
		capability, err := worker.markAccountCredentialDeleting(ctx, job, binding, ref)
		if err != nil {
			return err
		}
		tokens, err := worker.Vault.OpenDeletingTokens(ctx, capability)
		if err != nil {
			return err
		}
		if err := worker.revokeTokens(ctx, binding, tokens); err != nil {
			return err
		}
		transitionNow, err := worker.currentTime()
		if err != nil {
			return err
		}
		return worker.Repository.MarkProviderRevoked(ctx, job, transitionNow)
	case "revoke_succeeded":
		validationNow, err := worker.currentTime()
		if err != nil {
			return err
		}
		if err := worker.Repository.ValidateClaim(ctx, job, validationNow); err != nil {
			return err
		}
		capability, err := worker.recoverDeletionCapability(ctx, binding, ref, job.OperationID, job.CommandID)
		if err != nil {
			return err
		}
		if _, err := worker.Vault.EraseDeletingTokens(ctx, capability); err != nil {
			return err
		}
		transitionNow, err := worker.currentTime()
		if err != nil {
			return err
		}
		return worker.Repository.BeginCleanup(ctx, job, transitionNow)
	case "cleanup_pending":
		validationNow, err := worker.currentTime()
		if err != nil {
			return err
		}
		if err := worker.Repository.ValidateClaim(ctx, job, validationNow); err != nil {
			return err
		}
		state, exists, err := worker.Vault.Resolve(ctx, binding)
		if err != nil {
			return err
		}
		if exists {
			capability, err := recoverDeletionCapability(worker.Vault, ctx, binding, ref, state, job.OperationID, job.CommandID)
			if err != nil {
				return err
			}
			if err := worker.Vault.Delete(ctx, capability); err != nil {
				return err
			}
		}
		absent, err := worker.Vault.ConfirmAbsent(ctx, binding, ref)
		if err != nil {
			return err
		}
		if !absent {
			return identity.ErrCredentialConflict
		}
		transitionNow, err := worker.currentTime()
		if err != nil {
			return err
		}
		return worker.Repository.Complete(ctx, job, transitionNow)
	default:
		return ErrUnavailable
	}
}

func (worker *AppleRevocationWorker) markAccountCredentialDeleting(ctx context.Context, job AppleRevocationJob, binding identity.CredentialBinding, ref identity.CredentialRef) (identity.DeletionCapability, error) {
	now, err := worker.currentTime()
	if err != nil {
		return identity.DeletionCapability{}, err
	}
	state, exists, err := worker.Vault.Resolve(ctx, binding)
	if err != nil || !exists {
		if err != nil {
			return identity.DeletionCapability{}, err
		}
		return identity.DeletionCapability{}, identity.ErrCredentialConflict
	}
	if state.Ref != ref {
		return identity.DeletionCapability{}, identity.ErrCredentialConflict
	}
	if state.Phase == identity.CredentialPhaseAdopting {
		metadata := identity.CredentialMetadata{Binding: binding, Ref: ref, Phase: state.Phase, CreatedAt: now.Add(-worker.OrphanGrace)}
		if err := worker.reconcileAdoption(ctx, metadata, state, now); err != nil {
			return identity.DeletionCapability{}, err
		}
		state, exists, err = worker.Vault.Resolve(ctx, binding)
		if err != nil || !exists {
			if err != nil {
				return identity.DeletionCapability{}, err
			}
			return identity.DeletionCapability{}, identity.ErrCredentialConflict
		}
	}
	var intent identity.DeletionCASIntent
	var recovered *identity.DeletionCapability
	if state.Phase == identity.CredentialPhaseDeleting {
		capability, recoverErr := recoverDeletionCapability(worker.Vault, ctx, binding, ref, state, job.OperationID, job.CommandID)
		if recoverErr != nil {
			return identity.DeletionCapability{}, recoverErr
		}
		recovered = &capability
		intent, err = identity.NewDeletionCASIntent([]byte("account-deletion-recovery"), now.Add(time.Second), now)
	} else {
		intent, err = worker.Vault.PrepareDeletionCAS(ctx, binding, state, identity.DeletionOperation{OperationID: job.OperationID, CommandID: job.CommandID})
	}
	if err != nil {
		return identity.DeletionCapability{}, err
	}
	securityClaim := accountDeletionSecurityClaim(job)
	securityIntent := security.CredentialDeletionCASIntent{Opaque: append([]byte(nil), intent.Opaque...)}
	markInvoked, markAmbiguous := false, false
	capability, err := worker.credentialAuthority().MarkAccountDeleting(ctx, security.AccountDeletionCredentialAttempt{
		Claim: securityClaim, Intent: securityIntent,
		ValidateClaimedJob: func(callback context.Context, received security.AccountDeletionJobClaim) error {
			if received != securityClaim {
				return ErrStaleLease
			}
			callbackNow, err := worker.currentTime()
			if err != nil {
				return err
			}
			return worker.Repository.ValidateClaim(callback, job, callbackNow)
		},
		ReferencedOutsideJob: func(callback context.Context, received security.AccountDeletionJobClaim) (bool, error) {
			if received != securityClaim {
				return false, ErrStaleLease
			}
			callbackNow, err := worker.currentTime()
			if err != nil {
				return false, err
			}
			return worker.Repository.CredentialReferencedOutsideJob(callback, job, callbackNow)
		},
		MarkDeleting: func(callback context.Context, received security.AccountDeletionJobClaim, receivedIntent security.CredentialDeletionCASIntent) (security.CredentialDeletionCapability, error) {
			if received != securityClaim || !bytes.Equal(receivedIntent.Opaque, intent.Opaque) {
				return security.CredentialDeletionCapability{}, identity.ErrCredentialConflict
			}
			if recovered != nil {
				return security.CredentialDeletionCapability{Opaque: append([]byte(nil), recovered.Opaque...)}, nil
			}
			markInvoked = true
			result, err := worker.Vault.MarkDeleting(callback, intent)
			markAmbiguous = errors.Is(err, identity.ErrUnavailable)
			return security.CredentialDeletionCapability{Opaque: append([]byte(nil), result.Opaque...)}, err
		},
	})
	if err == nil {
		return identity.DeletionCapability{Opaque: append([]byte(nil), capability.Opaque...)}, nil
	}
	if errors.Is(err, ErrStaleLease) {
		return identity.DeletionCapability{}, err
	}
	if !markInvoked || !markAmbiguous {
		return identity.DeletionCapability{}, err
	}
	// A lost CAS response is recoverable only from the exact durable deleting
	// coordinates. Any other state remains ambiguous and fails closed.
	state, exists, resolveErr := worker.Vault.Resolve(ctx, binding)
	if resolveErr != nil || !exists {
		return identity.DeletionCapability{}, err
	}
	result, recoverErr := recoverDeletionCapability(worker.Vault, ctx, binding, ref, state, job.OperationID, job.CommandID)
	if recoverErr != nil {
		return identity.DeletionCapability{}, err
	}
	validationNow, timeErr := worker.currentTime()
	if timeErr != nil {
		return identity.DeletionCapability{}, timeErr
	}
	if validateErr := worker.Repository.ValidateClaim(ctx, job, validationNow); validateErr != nil {
		return identity.DeletionCapability{}, validateErr
	}
	return result, nil
}

func (worker *AppleRevocationWorker) reconcileAdoption(ctx context.Context, metadata identity.CredentialMetadata, state identity.CredentialRotationState, now time.Time) error {
	resume, err := worker.Rotations.Resume(ctx, metadata.Binding)
	if err != nil {
		return err
	}
	if resume.State != state || resume.Adoption == nil {
		return identity.ErrCredentialConflict
	}
	hash, exists, err := worker.credentialAuthority().Receipt(ctx, metadata.Binding.AccountID, state.CommandID)
	if err != nil {
		return err
	}
	if exists {
		if hash != resume.ExpectedCommandHash {
			return ErrUnavailable
		}
		return worker.Rotations.FinishAdoption(ctx, metadata.Binding, *resume.Adoption)
	}
	if now.Before(state.ReconcileAfter) {
		return identity.ErrCredentialRotationBusy
	}
	return worker.Rotations.AbandonAdoption(ctx, metadata.Binding, *resume.Adoption)
}

func (worker *AppleRevocationWorker) cleanupOrphan(ctx context.Context, binding identity.CredentialBinding, state identity.CredentialRotationState, now time.Time) error {
	claimNow, err := worker.currentTime()
	if err != nil {
		return err
	}
	claim, ok, err := worker.Repository.ClaimOrphanCleanup(ctx, binding, state.Ref, worker.WorkerID, claimNow, claimNow.Add(worker.Lease))
	if err != nil || !ok {
		return err
	}
	return worker.runClaimedOrphanCleanup(ctx, OrphanCleanupJob{Claim: claim, Binding: binding, Ref: state.Ref}, state, now)
}

func (worker *AppleRevocationWorker) resumeUnfinishedOrphanCleanup(ctx context.Context) error {
	now, err := worker.currentTime()
	if err != nil {
		return err
	}
	job, ok, err := worker.Repository.ClaimUnfinishedOrphanCleanup(ctx, worker.WorkerID, now, now.Add(worker.Lease))
	if err != nil || !ok {
		return err
	}
	state, exists, err := worker.Vault.Resolve(ctx, job.Binding)
	if err != nil {
		return worker.retryOrphan(ctx, job.Claim, err)
	}
	if !exists {
		validationNow, err := worker.currentTime()
		if err != nil {
			return err
		}
		if err := worker.Repository.ValidateOrphanCleanup(ctx, job.Claim, validationNow); err != nil {
			return err
		}
		absent, err := worker.Vault.ConfirmAbsent(ctx, job.Binding, job.Ref)
		if err != nil || !absent {
			if err == nil {
				err = identity.ErrCredentialConflict
			}
			return worker.retryOrphan(ctx, job.Claim, err)
		}
		finishNow, err := worker.currentTime()
		if err != nil {
			return err
		}
		return worker.Repository.FinishOrphanCleanup(ctx, job.Claim, finishNow)
	}
	if state.Ref != job.Ref || (state.Phase != identity.CredentialPhaseReady && state.Phase != identity.CredentialPhaseDeleting) {
		return worker.retryOrphan(ctx, job.Claim, identity.ErrCredentialConflict)
	}
	return worker.runClaimedOrphanCleanup(ctx, job, state, now)
}

func (worker *AppleRevocationWorker) runClaimedOrphanCleanup(ctx context.Context, job OrphanCleanupJob, state identity.CredentialRotationState, now time.Time) error {
	claim, binding := job.Claim, job.Binding
	if claim.AccountID != binding.AccountID || claim.ObjectID != binding.ObjectID || claim.ReferenceID != job.Ref.ID || claim.ReferenceKeyID != job.Ref.KeyID {
		return ErrUnavailable
	}
	var intent identity.DeletionCASIntent
	var recovered *identity.DeletionCapability
	var err error
	if state.Phase == identity.CredentialPhaseReady {
		intent, err = worker.Vault.PrepareDeletionCAS(ctx, binding, state, identity.DeletionOperation{OperationID: claim.OperationID, CommandID: claim.CommandID})
	} else if state.Phase == identity.CredentialPhaseDeleting {
		capability, recoverErr := recoverDeletionCapability(worker.Vault, ctx, binding, state.Ref, state, claim.OperationID, claim.CommandID)
		if recoverErr != nil {
			err = recoverErr
		} else {
			recovered = &capability
			intent, err = identity.NewDeletionCASIntent([]byte("orphan-deletion-recovery"), now.Add(time.Second), now)
		}
	} else {
		err = identity.ErrCredentialConflict
	}
	if err != nil {
		return worker.retryOrphan(ctx, claim, err)
	}
	securityIntent := security.CredentialDeletionCASIntent{Opaque: append([]byte(nil), intent.Opaque...)}
	completed, err := worker.credentialAuthority().DeleteOrphan(ctx, security.CredentialCleanupAttempt{
		Claim: claim, Intent: securityIntent,
		ValidateClaim: func(callback context.Context, received security.CredentialCleanupClaim) error {
			if received != claim {
				return ErrStaleLease
			}
			callbackNow, err := worker.currentTime()
			if err != nil {
				return err
			}
			return worker.Repository.ValidateOrphanCleanup(callback, claim, callbackNow)
		},
		Referenced: func(callback context.Context, received security.CredentialCleanupClaim) (bool, error) {
			if received != claim {
				return false, ErrStaleLease
			}
			callbackNow, err := worker.currentTime()
			if err != nil {
				return false, err
			}
			return worker.Repository.CredentialReferenced(callback, claim, callbackNow)
		},
		MarkDeleting: func(callback context.Context, received security.CredentialCleanupClaim, receivedIntent security.CredentialDeletionCASIntent) (security.CredentialDeletionCapability, error) {
			if received != claim || !bytes.Equal(receivedIntent.Opaque, intent.Opaque) {
				return security.CredentialDeletionCapability{}, identity.ErrCredentialConflict
			}
			if recovered != nil {
				return security.CredentialDeletionCapability{Opaque: append([]byte(nil), recovered.Opaque...)}, nil
			}
			result, err := worker.Vault.MarkDeleting(callback, intent)
			return security.CredentialDeletionCapability{Opaque: append([]byte(nil), result.Opaque...)}, err
		},
		Delete: func(callback context.Context, capability security.CredentialDeletionCapability) error {
			deleteNow, err := worker.currentTime()
			if err != nil {
				return err
			}
			if err := worker.Repository.ValidateOrphanCleanup(callback, claim, deleteNow); err != nil {
				return err
			}
			erased, err := worker.Vault.EraseDeletingTokens(callback, identity.DeletionCapability{Opaque: append([]byte(nil), capability.Opaque...)})
			if err != nil {
				return err
			}
			return worker.Vault.Delete(callback, erased)
		},
		ConfirmAbsent: func(callback context.Context, _ security.CredentialDeletionCapability) (bool, error) {
			return worker.Vault.ConfirmAbsent(callback, binding, job.Ref)
		},
		Finish: func(callback context.Context, received security.CredentialCleanupClaim) error {
			if received != claim {
				return ErrStaleLease
			}
			callbackNow, err := worker.currentTime()
			if err != nil {
				return err
			}
			return worker.Repository.FinishOrphanCleanup(callback, claim, callbackNow)
		},
	})
	if err != nil {
		if errors.Is(err, ErrStaleLease) {
			return err
		}
		return worker.retryOrphan(ctx, claim, err)
	}
	if !completed {
		return worker.retryOrphan(ctx, claim, codedError("credential_referenced"))
	}
	return nil
}

func (worker *AppleRevocationWorker) retryOrphan(ctx context.Context, claim security.CredentialCleanupClaim, cause error) error {
	retryNow, timeErr := worker.currentTime()
	if timeErr != nil {
		return workerPublicError(timeErr)
	}
	err := worker.Repository.RetryOrphanCleanup(ctx, claim, safeWorkerErrorCode(cause), retryNow.Add(time.Minute), retryNow)
	return workerPublicError(err)
}

func (worker *AppleRevocationWorker) currentTime() (time.Time, error) {
	now := worker.Now().UTC()
	if !validWorkerTime(now) {
		return time.Time{}, ErrUnavailable
	}
	return now, nil
}

func (worker *AppleRevocationWorker) recoverDeletionCapability(ctx context.Context, binding identity.CredentialBinding, ref identity.CredentialRef, operationID, commandID string) (identity.DeletionCapability, error) {
	state, exists, err := worker.Vault.Resolve(ctx, binding)
	if err != nil || !exists {
		if err != nil {
			return identity.DeletionCapability{}, err
		}
		return identity.DeletionCapability{}, identity.ErrCredentialConflict
	}
	return recoverDeletionCapability(worker.Vault, ctx, binding, ref, state, operationID, commandID)
}

func recoverDeletionCapability(vault identity.CredentialVault, ctx context.Context, binding identity.CredentialBinding, ref identity.CredentialRef, state identity.CredentialRotationState, operationID, commandID string) (identity.DeletionCapability, error) {
	if state.Phase != identity.CredentialPhaseDeleting || state.Ref != ref || state.OperationID != operationID || state.CommandID != commandID || state.MarkerSequence == 0 || state.MarkerDigest == ([32]byte{}) {
		return identity.DeletionCapability{}, identity.ErrCredentialConflict
	}
	return vault.RecoverDeletionCapability(ctx, identity.DeletionRecovery{Binding: binding, Ref: ref, OperationID: operationID,
		CommandID: commandID, MarkerSequence: state.MarkerSequence, MarkerDigest: state.MarkerDigest})
}

func (worker *AppleRevocationWorker) revokeTokens(ctx context.Context, binding identity.CredentialBinding, tokens [][]byte) error {
	if len(tokens) > 2 {
		return identity.ErrCredentialConflict
	}
	for _, token := range tokens {
		if len(token) == 0 {
			return identity.ErrCredentialConflict
		}
		if err := worker.Apple.Revoke(ctx, identity.AppleRevocationRequest{Binding: binding, RefreshToken: append([]byte(nil), token...)}); err != nil {
			return codedError("apple_unavailable")
		}
	}
	return nil
}

type securityCredentialAuthority struct{ service *security.Service }

func (authority securityCredentialAuthority) MarkAccountDeleting(ctx context.Context, attempt security.AccountDeletionCredentialAttempt) (security.CredentialDeletionCapability, error) {
	return authority.service.MarkCredentialDeletingForAccountDeletion(ctx, attempt)
}

func (authority securityCredentialAuthority) DeleteOrphan(ctx context.Context, attempt security.CredentialCleanupAttempt) (bool, error) {
	return authority.service.DeleteCredentialIfUnreferenced(ctx, attempt)
}

func (authority securityCredentialAuthority) Receipt(ctx context.Context, accountID, commandID string) (result [32]byte, found bool, err error) {
	err = authority.service.WithAuthoritySnapshot(ctx, func(snapshot security.AuthoritySnapshot) error {
		receipt, exists := snapshot.View.Receipt(accountID, commandID)
		if exists {
			result, found = receipt.CommandHash, true
		}
		return nil
	})
	return result, found, err
}

func accountDeletionSecurityClaim(job AppleRevocationJob) security.AccountDeletionJobClaim {
	return security.AccountDeletionJobClaim{JobID: job.JobID, LeaseToken: job.LeaseToken, OperationID: job.OperationID,
		CommandID: job.CommandID, OperationVersion: job.OperationVersion, Status: job.Status, AccountID: job.AccountID,
		ServerInstanceID: job.ServerInstanceID, Purpose: job.Purpose, ObjectID: job.ObjectID, ReferenceID: job.ReferenceID,
		ReferenceKeyID: job.ReferenceKeyID, Attempts: job.Attempts, LeaseExpiresAt: job.LeaseExpiresAt}
}

func validAppleRevocationJob(job AppleRevocationJob, now time.Time) bool {
	if !validUUID(job.JobID) || !workerIDPattern.MatchString(job.LeaseToken) || !canonicalPositiveDecimal(job.OperationVersion) ||
		!validUUID(job.OperationID) || !validUUID(job.CommandID) || !validUUID(job.AccountID) || !validUUID(job.ServerInstanceID) ||
		job.Purpose != identity.AppleCredentialPurpose || !validUUID(job.ObjectID) || job.Attempts < 1 || !validWorkerTime(job.LeaseExpiresAt) || !now.Before(job.LeaseExpiresAt) {
		return false
	}
	if job.Status != "pending" && job.Status != "retry" && job.Status != "revoke_succeeded" && job.Status != "cleanup_pending" {
		return false
	}
	binding, err := identity.NewAppleCredentialBinding(job.AccountID, job.ServerInstanceID)
	return err == nil && binding.ObjectID == job.ObjectID && job.ReferenceID == job.ObjectID && validCredentialReferenceText(job.ReferenceKeyID)
}

func validCredentialReferenceText(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func validWorkerTime(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC && value.Year() >= 1 && value.Year() <= 9999
}

func safeWorkerErrorCode(err error) string {
	if errors.Is(err, ErrStaleLease) {
		return "stale_lease"
	}
	if Code(err) == "apple_unavailable" || identity.Code(err) == "apple_unavailable" {
		return "apple_unavailable"
	}
	if identity.Code(err) == "credential_conflict" {
		return "credential_conflict"
	}
	if Code(err) == "credential_referenced" {
		return "credential_referenced"
	}
	return "service_unavailable"
}

func nextWorkerAttempt(attempts int, now time.Time) time.Time {
	shift := attempts - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 10 {
		shift = 10
	}
	delay := 5 * time.Second * time.Duration(1<<shift)
	if delay > time.Hour {
		delay = time.Hour
	}
	return now.Add(delay)
}

func workerPublicError(err error) error {
	if err == nil || errors.Is(err, ErrStaleLease) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return ErrUnavailable
}

func canonicalPositiveDecimal(value string) bool {
	if !operationVersionPattern.MatchString(value) {
		return false
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	return err == nil && parsed > 0 && parsed <= 9007199254740991
}
