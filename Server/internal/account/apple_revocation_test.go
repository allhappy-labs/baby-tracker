package account

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"babytracker/server/internal/identity"
	"babytracker/server/internal/security"
)

type workerRepositoryFake struct {
	job               *AppleRevocationJob
	jobClaimed        bool
	staleValidation   bool
	orphanEnabled     bool
	unfinishedEnabled bool
	orphanClaim       security.CredentialCleanupClaim
	orphanBinding     identity.CredentialBinding
	orphanRef         identity.CredentialRef
	orphanClaimed     bool
	orphanLeaseExpiry time.Time
	referencedChecks  []bool
	validateCalls     int
	finishOrphanCalls int
	retryCodes        []string
	orphanRetryCodes  []string
	afterClaim        func()
	afterOrphanClaim  func()
	staleFinish       bool
}

func (repository *workerRepositoryFake) Claim(_ context.Context, workerID string, now, expires time.Time) (AppleRevocationJob, bool, error) {
	if repository.job == nil || repository.job.Status == "complete" || repository.jobClaimed {
		return AppleRevocationJob{}, false, nil
	}
	repository.jobClaimed = true
	version, _ := strconv.Atoi(repository.job.OperationVersion)
	repository.job.OperationVersion = strconv.Itoa(version + 1)
	repository.job.Attempts++
	repository.job.LeaseToken = workerID + ":lease"
	repository.job.LeaseExpiresAt = expires
	if repository.afterClaim != nil {
		repository.afterClaim()
	}
	return *repository.job, true, nil
}

func (repository *workerRepositoryFake) ValidateClaim(_ context.Context, claim AppleRevocationJob, now time.Time) error {
	repository.validateCalls++
	if repository.staleValidation || repository.job == nil || claim != *repository.job || !repository.jobClaimed || !now.Before(claim.LeaseExpiresAt) {
		return ErrStaleLease
	}
	return nil
}

func (repository *workerRepositoryFake) CredentialReferencedOutsideJob(ctx context.Context, claim AppleRevocationJob, now time.Time) (bool, error) {
	if err := repository.ValidateClaim(ctx, claim, now); err != nil {
		return false, err
	}
	return false, nil
}

func (repository *workerRepositoryFake) MarkProviderRevoked(ctx context.Context, claim AppleRevocationJob, now time.Time) error {
	if err := repository.ValidateClaim(ctx, claim, now); err != nil {
		return err
	}
	repository.job.Status, repository.jobClaimed = "revoke_succeeded", false
	return nil
}

func (repository *workerRepositoryFake) BeginCleanup(ctx context.Context, claim AppleRevocationJob, now time.Time) error {
	if err := repository.ValidateClaim(ctx, claim, now); err != nil {
		return err
	}
	repository.job.Status, repository.jobClaimed = "cleanup_pending", false
	return nil
}

func (repository *workerRepositoryFake) Complete(ctx context.Context, claim AppleRevocationJob, now time.Time) error {
	if err := repository.ValidateClaim(ctx, claim, now); err != nil {
		return err
	}
	repository.job.Status, repository.jobClaimed = "complete", false
	return nil
}

func (repository *workerRepositoryFake) Retry(ctx context.Context, claim AppleRevocationJob, code string, _ time.Time, now time.Time) error {
	if err := repository.ValidateClaim(ctx, claim, now); err != nil {
		return err
	}
	repository.retryCodes = append(repository.retryCodes, code)
	repository.job.Status, repository.jobClaimed = "retry", false
	return nil
}

func (repository *workerRepositoryFake) ClaimOrphanCleanup(_ context.Context, binding identity.CredentialBinding, ref identity.CredentialRef, workerID string, now, expires time.Time) (security.CredentialCleanupClaim, bool, error) {
	if !repository.orphanEnabled || repository.orphanClaimed {
		return security.CredentialCleanupClaim{}, false, nil
	}
	if repository.orphanClaim.OperationID == "" {
		repository.orphanClaim = security.CredentialCleanupClaim{OperationID: testUUID(80), CommandID: testUUID(81)}
	}
	version, _ := strconv.Atoi(repository.orphanClaim.OperationVersion)
	repository.orphanClaim.OperationVersion = strconv.Itoa(version + 1)
	if version == 0 {
		repository.orphanClaim.OperationVersion = "1"
	}
	repository.orphanClaim.LeaseToken = workerID + ":orphan"
	repository.orphanClaim.AccountID, repository.orphanClaim.ObjectID = binding.AccountID, binding.ObjectID
	repository.orphanClaim.ReferenceID, repository.orphanClaim.ReferenceKeyID = ref.ID, ref.KeyID
	repository.orphanBinding, repository.orphanRef = binding, ref
	repository.orphanClaimed = true
	repository.orphanLeaseExpiry = expires
	if repository.afterOrphanClaim != nil {
		repository.afterOrphanClaim()
	}
	return repository.orphanClaim, true, nil
}

func (repository *workerRepositoryFake) ClaimUnfinishedOrphanCleanup(_ context.Context, workerID string, now, expires time.Time) (OrphanCleanupJob, bool, error) {
	if !repository.unfinishedEnabled || (repository.orphanClaimed && now.Before(repository.orphanLeaseExpiry)) || repository.orphanClaim.OperationID == "" {
		return OrphanCleanupJob{}, false, nil
	}
	version, _ := strconv.Atoi(repository.orphanClaim.OperationVersion)
	repository.orphanClaim.OperationVersion = strconv.Itoa(version + 1)
	repository.orphanClaim.LeaseToken = workerID + ":resume"
	repository.orphanClaimed = true
	repository.orphanLeaseExpiry = expires
	return OrphanCleanupJob{Claim: repository.orphanClaim, Binding: repository.orphanBinding, Ref: repository.orphanRef}, true, nil
}

func (repository *workerRepositoryFake) ValidateOrphanCleanup(_ context.Context, claim security.CredentialCleanupClaim, now time.Time) error {
	if !repository.orphanClaimed || claim != repository.orphanClaim || !now.Before(repository.orphanLeaseExpiry) {
		return ErrStaleLease
	}
	return nil
}

func (repository *workerRepositoryFake) CredentialReferenced(ctx context.Context, claim security.CredentialCleanupClaim, now time.Time) (bool, error) {
	if err := repository.ValidateOrphanCleanup(ctx, claim, now); err != nil {
		return false, err
	}
	if len(repository.referencedChecks) == 0 {
		return false, nil
	}
	result := repository.referencedChecks[0]
	repository.referencedChecks = repository.referencedChecks[1:]
	return result, nil
}

func (repository *workerRepositoryFake) FinishOrphanCleanup(ctx context.Context, claim security.CredentialCleanupClaim, now time.Time) error {
	if err := repository.ValidateOrphanCleanup(ctx, claim, now); err != nil {
		return err
	}
	if repository.staleFinish {
		return ErrStaleLease
	}
	repository.finishOrphanCalls++
	repository.orphanClaimed, repository.orphanEnabled = false, false
	return nil
}

func (repository *workerRepositoryFake) RetryOrphanCleanup(ctx context.Context, claim security.CredentialCleanupClaim, code string, _, now time.Time) error {
	if err := repository.ValidateOrphanCleanup(ctx, claim, now); err != nil {
		return err
	}
	repository.orphanRetryCodes = append(repository.orphanRetryCodes, code)
	repository.orphanClaimed = false
	return nil
}

type workerAuthorityFake struct {
	receipts       map[string][32]byte
	receiptErr     error
	failAfterMark  bool
	beforeDelete   func()
	markCalls      int
	referenceCalls int
}

type ambiguousMarkVault struct {
	identity.CredentialVault
	afterMark func()
}

func (vault ambiguousMarkVault) MarkDeleting(ctx context.Context, intent identity.DeletionCASIntent) (identity.DeletionCapability, error) {
	if _, err := vault.CredentialVault.MarkDeleting(ctx, intent); err != nil {
		return identity.DeletionCapability{}, err
	}
	if vault.afterMark != nil {
		vault.afterMark()
	}
	return identity.DeletionCapability{}, identity.ErrUnavailable
}

type failingInventory struct{ err error }

func (inventory failingInventory) List(context.Context, time.Time, string, int) ([]identity.CredentialMetadata, string, error) {
	return nil, "", inventory.err
}

func (authority *workerAuthorityFake) MarkAccountDeleting(ctx context.Context, attempt security.AccountDeletionCredentialAttempt) (security.CredentialDeletionCapability, error) {
	if err := attempt.ValidateClaimedJob(ctx, attempt.Claim); err != nil {
		return security.CredentialDeletionCapability{}, err
	}
	for index := 0; index < 2; index++ {
		authority.referenceCalls++
		referenced, err := attempt.ReferencedOutsideJob(ctx, attempt.Claim)
		if err != nil || referenced {
			if err != nil {
				return security.CredentialDeletionCapability{}, err
			}
			return security.CredentialDeletionCapability{}, identity.ErrCredentialConflict
		}
	}
	authority.markCalls++
	capability, err := attempt.MarkDeleting(ctx, attempt.Claim, attempt.Intent)
	if err != nil {
		return security.CredentialDeletionCapability{}, err
	}
	if authority.failAfterMark {
		authority.failAfterMark = false
		return security.CredentialDeletionCapability{}, ErrUnavailable
	}
	return capability, nil
}

func (authority *workerAuthorityFake) DeleteOrphan(ctx context.Context, attempt security.CredentialCleanupAttempt) (bool, error) {
	if err := attempt.ValidateClaim(ctx, attempt.Claim); err != nil {
		return false, err
	}
	for index := 0; index < 2; index++ {
		authority.referenceCalls++
		referenced, err := attempt.Referenced(ctx, attempt.Claim)
		if err != nil || referenced {
			return false, err
		}
	}
	authority.markCalls++
	capability, err := attempt.MarkDeleting(ctx, attempt.Claim, attempt.Intent)
	if err != nil {
		return false, err
	}
	if authority.failAfterMark {
		authority.failAfterMark = false
		return false, ErrUnavailable
	}
	if authority.beforeDelete != nil {
		authority.beforeDelete()
	}
	if err := attempt.Delete(ctx, capability); err != nil {
		return false, err
	}
	absent, err := attempt.ConfirmAbsent(ctx, capability)
	if err != nil || !absent {
		return false, err
	}
	if err := attempt.Finish(ctx, attempt.Claim); err != nil {
		return false, err
	}
	return true, nil
}

func (authority *workerAuthorityFake) Receipt(_ context.Context, accountID, commandID string) ([32]byte, bool, error) {
	if authority.receiptErr != nil {
		return [32]byte{}, false, authority.receiptErr
	}
	result, ok := authority.receipts[accountID+"/"+commandID]
	return result, ok, nil
}

type workerAppleFake struct {
	requests []identity.AppleRevocationRequest
	err      error
	before   func(identity.AppleRevocationRequest)
}

func (apple *workerAppleFake) Revoke(_ context.Context, request identity.AppleRevocationRequest) error {
	if apple.before != nil {
		apple.before(request)
	}
	apple.requests = append(apple.requests, identity.AppleRevocationRequest{Binding: request.Binding, RefreshToken: append([]byte(nil), request.RefreshToken...)})
	return apple.err
}

func TestAppleRevocationWorkerMarksBeforeTokenOpenAndCompletesInPhases(t *testing.T) {
	fixture := newWorkerFixture(t)
	state := fixture.makeReady(t)
	fixture.repository.job = fixture.job(state)
	fixture.apple.before = func(identity.AppleRevocationRequest) {
		resolved, ok, err := fixture.vault.Resolve(t.Context(), fixture.binding)
		if err != nil || !ok || resolved.Phase != identity.CredentialPhaseDeleting {
			t.Fatalf("provider called before deleting marker: state=%+v present=%t err=%v", resolved, ok, err)
		}
	}
	if err := fixture.worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fixture.repository.job.Status != "revoke_succeeded" || len(fixture.apple.requests) != 1 || fixture.authority.markCalls != 1 {
		t.Fatalf("after revoke job=%+v apple=%d marks=%d", fixture.repository.job, len(fixture.apple.requests), fixture.authority.markCalls)
	}
	if err := fixture.worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fixture.repository.job.Status != "cleanup_pending" || len(fixture.apple.requests) != 1 {
		t.Fatalf("after cleanup marker status=%s apple=%d", fixture.repository.job.Status, len(fixture.apple.requests))
	}
	if err := fixture.worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fixture.repository.job.Status != "complete" || len(fixture.apple.requests) != 1 {
		t.Fatalf("complete status=%s apple=%d", fixture.repository.job.Status, len(fixture.apple.requests))
	}
	if _, present, err := fixture.vault.Resolve(t.Context(), fixture.binding); err != nil || present {
		t.Fatalf("credential present=%t err=%v", present, err)
	}
}

func TestAppleRevocationWorkerStaleLeaseCannotOpenOrRevoke(t *testing.T) {
	fixture := newWorkerFixture(t)
	state := fixture.makeReady(t)
	fixture.repository.job = fixture.job(state)
	fixture.repository.staleValidation = true
	err := fixture.worker.RunOnce(t.Context())
	if !errors.Is(err, ErrStaleLease) || len(fixture.apple.requests) != 0 {
		t.Fatalf("err=%v apple=%d", err, len(fixture.apple.requests))
	}
	resolved, present, resolveErr := fixture.vault.Resolve(t.Context(), fixture.binding)
	if resolveErr != nil || !present || resolved.Phase != identity.CredentialPhaseReady {
		t.Fatalf("state=%+v present=%t err=%v", resolved, present, resolveErr)
	}
}

func TestAppleRevocationWorkerRecoversOnlyAmbiguousVaultCASAfterFreshValidation(t *testing.T) {
	t.Run("authority failure after successful callback does not bypass validation", func(t *testing.T) {
		fixture := newWorkerFixture(t)
		state := fixture.makeReady(t)
		fixture.repository.job = fixture.job(state)
		fixture.authority.failAfterMark = true
		if err := fixture.worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(fixture.apple.requests) != 0 || !reflect.DeepEqual(fixture.repository.retryCodes, []string{"service_unavailable"}) {
			t.Fatalf("provider=%d retries=%v", len(fixture.apple.requests), fixture.repository.retryCodes)
		}
		resolved, present, err := fixture.vault.Resolve(t.Context(), fixture.binding)
		if err != nil || !present || resolved.Phase != identity.CredentialPhaseDeleting {
			t.Fatalf("state=%+v present=%t err=%v", resolved, present, err)
		}
	})

	t.Run("ambiguous vault CAS recovers exact live claim", func(t *testing.T) {
		fixture := newWorkerFixture(t)
		state := fixture.makeReady(t)
		fixture.repository.job = fixture.job(state)
		fixture.worker.Vault = ambiguousMarkVault{CredentialVault: fixture.vault}
		if err := fixture.worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if fixture.repository.job.Status != "revoke_succeeded" || len(fixture.apple.requests) != 1 {
			t.Fatalf("status=%s provider=%d", fixture.repository.job.Status, len(fixture.apple.requests))
		}
	})

	t.Run("ambiguous vault CAS with expired claim cannot open tokens", func(t *testing.T) {
		fixture := newWorkerFixture(t)
		state := fixture.makeReady(t)
		fixture.repository.job = fixture.job(state)
		fixture.worker.Vault = ambiguousMarkVault{CredentialVault: fixture.vault, afterMark: func() { fixture.advance(time.Minute) }}
		err := fixture.worker.RunOnce(t.Context())
		if !errors.Is(err, ErrStaleLease) || len(fixture.apple.requests) != 0 {
			t.Fatalf("err=%v provider=%d", err, len(fixture.apple.requests))
		}
	})
}

func TestAppleRevocationWorkerUsesFreshTimeForEveryPostClaimPhase(t *testing.T) {
	tests := []struct {
		name   string
		status string
		setup  func(*workerFixture, *testing.T) identity.CredentialRotationState
	}{
		{"pending before deletion marker", "pending", func(f *workerFixture, t *testing.T) identity.CredentialRotationState { return f.makeReady(t) }},
		{"provider revoked before token erase", "revoke_succeeded", func(f *workerFixture, t *testing.T) identity.CredentialRotationState {
			return f.makeDeleting(t, false)
		}},
		{"cleanup pending before credential delete", "cleanup_pending", func(f *workerFixture, t *testing.T) identity.CredentialRotationState {
			return f.makeDeleting(t, true)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWorkerFixture(t)
			state := test.setup(fixture, t)
			fixture.repository.job = fixture.job(state)
			fixture.repository.job.Status = test.status
			fixture.repository.afterClaim = func() { fixture.advance(time.Minute) }
			err := fixture.worker.RunOnce(t.Context())
			if !errors.Is(err, ErrStaleLease) || len(fixture.apple.requests) != 0 {
				t.Fatalf("err=%v apple=%d", err, len(fixture.apple.requests))
			}
			resolved, present, resolveErr := fixture.vault.Resolve(t.Context(), fixture.binding)
			if resolveErr != nil || !present || resolved != state {
				t.Fatalf("state mutated: got=%+v want=%+v present=%t err=%v", resolved, state, present, resolveErr)
			}
		})
	}

	t.Run("orphan cleanup before deletion marker", func(t *testing.T) {
		fixture := newWorkerFixture(t)
		state := fixture.makeReady(t)
		fixture.advance(2 * time.Hour)
		fixture.repository.orphanEnabled = true
		fixture.repository.afterOrphanClaim = func() { fixture.advance(time.Minute) }
		err := fixture.worker.RunOnce(t.Context())
		if !errors.Is(err, ErrStaleLease) || len(fixture.apple.requests) != 0 {
			t.Fatalf("err=%v apple=%d", err, len(fixture.apple.requests))
		}
		resolved, present, resolveErr := fixture.vault.Resolve(t.Context(), fixture.binding)
		if resolveErr != nil || !present || resolved != state {
			t.Fatalf("state mutated: got=%+v want=%+v present=%t err=%v", resolved, state, present, resolveErr)
		}
	})

	t.Run("orphan cleanup before token erase", func(t *testing.T) {
		fixture := newWorkerFixture(t)
		fixture.makeReady(t)
		fixture.advance(2 * time.Hour)
		fixture.repository.orphanEnabled = true
		fixture.authority.beforeDelete = func() { fixture.advance(time.Minute) }
		err := fixture.worker.RunOnce(t.Context())
		if !errors.Is(err, ErrStaleLease) || len(fixture.apple.requests) != 0 {
			t.Fatalf("err=%v apple=%d", err, len(fixture.apple.requests))
		}
		resolved, present, resolveErr := fixture.vault.Resolve(t.Context(), fixture.binding)
		if resolveErr != nil || !present || resolved.Phase != identity.CredentialPhaseDeleting || !resolved.HasCurrent {
			t.Fatalf("state=%+v present=%t err=%v", resolved, present, resolveErr)
		}
	})
}

func TestAppleRevocationWorkerRetriesProviderWithPrivacySafeCode(t *testing.T) {
	fixture := newWorkerFixture(t)
	state := fixture.makeReady(t)
	fixture.repository.job = fixture.job(state)
	fixture.apple.err = errors.New("provider leaked secret detail")
	if err := fixture.worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.repository.retryCodes, []string{"apple_unavailable"}) || fixture.repository.job.Status != "retry" {
		t.Fatalf("retry=%v status=%s", fixture.repository.retryCodes, fixture.repository.job.Status)
	}
}

func TestAppleRevocationWorkerDeletionMarkerFreezesAndRevokesBothSlots(t *testing.T) {
	fixture := newWorkerFixture(t)
	fixture.makeReady(t)
	state, err := fixture.vault.ReserveRotation(t.Context(), fixture.binding, testUUID(20), testUUID(21))
	if err != nil {
		t.Fatal(err)
	}
	state, err = fixture.vault.StoreCandidate(t.Context(), fixture.binding, state, []byte("candidate-refresh"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.repository.job = fixture.job(state)
	claim, ok, err := fixture.repository.Claim(t.Context(), fixture.worker.WorkerID, fixture.now, fixture.now.Add(fixture.worker.Lease))
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	fixture.apple.before = func(identity.AppleRevocationRequest) {
		resolved, present, resolveErr := fixture.vault.Resolve(t.Context(), fixture.binding)
		if resolveErr != nil || !present || resolved.Phase != identity.CredentialPhaseDeleting {
			t.Fatalf("provider called before marker: state=%+v present=%t err=%v", resolved, present, resolveErr)
		}
	}
	if err := fixture.worker.runClaimedJob(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	if fixture.repository.job.Status != "revoke_succeeded" || len(fixture.apple.requests) != 2 {
		t.Fatalf("status=%s revocations=%d", fixture.repository.job.Status, len(fixture.apple.requests))
	}
	got := map[string]bool{}
	for _, request := range fixture.apple.requests {
		got[string(request.RefreshToken)] = true
	}
	if !got["refresh"] || !got["candidate-refresh"] {
		t.Fatalf("revoked tokens=%v", got)
	}
}

func TestRotationReconcilerUsesExactAuthorityReceipt(t *testing.T) {
	t.Run("matching receipt finishes", func(t *testing.T) {
		fixture := newWorkerFixture(t)
		state := fixture.makeAdopting(t)
		fixture.authority.receipts[fixture.binding.AccountID+"/"+state.CommandID] = state.ExpectedCommandHash
		fixture.advance(time.Minute)
		if err := fixture.worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		resolved, ok, err := fixture.vault.Resolve(t.Context(), fixture.binding)
		if err != nil || !ok || resolved.Phase != identity.CredentialPhaseReady {
			t.Fatalf("state=%+v present=%t err=%v", resolved, ok, err)
		}
	})
	t.Run("definite absence waits then abandons", func(t *testing.T) {
		fixture := newWorkerFixture(t)
		state := fixture.makeAdopting(t)
		fixture.advance(time.Second)
		if err := fixture.worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		resolved, _, _ := fixture.vault.Resolve(t.Context(), fixture.binding)
		if resolved.Phase != identity.CredentialPhaseAdopting {
			t.Fatalf("before deadline phase=%s", resolved.Phase)
		}
		fixture.now = state.ReconcileAfter
		if err := fixture.worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, present, err := fixture.vault.Resolve(t.Context(), fixture.binding); err != nil || present {
			t.Fatalf("present=%t err=%v", present, err)
		}
	})
	t.Run("ambiguous authority never mutates", func(t *testing.T) {
		fixture := newWorkerFixture(t)
		state := fixture.makeAdopting(t)
		fixture.now = state.ReconcileAfter.Add(time.Minute)
		fixture.authority.receiptErr = errors.New("authority unavailable")
		if Code(fixture.worker.RunOnce(t.Context())) != "service_unavailable" {
			t.Fatal("ambiguous authority did not fail closed")
		}
		resolved, _, _ := fixture.vault.Resolve(t.Context(), fixture.binding)
		if resolved.Phase != identity.CredentialPhaseAdopting {
			t.Fatalf("phase=%s", resolved.Phase)
		}
	})
}

func TestRotationReconcilerResumesEveryNonterminalPhase(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*workerFixture, *testing.T) identity.CredentialRotationState
		want    string
	}{
		{"expired reserved", func(f *workerFixture, t *testing.T) identity.CredentialRotationState { return f.reserve(t) }, identity.CredentialPhaseAbsent},
		{"candidate", func(f *workerFixture, t *testing.T) identity.CredentialRotationState { return f.candidate(t, false) }, identity.CredentialPhasePromoted},
		{"retiring", func(f *workerFixture, t *testing.T) identity.CredentialRotationState { return f.candidate(t, true) }, identity.CredentialPhasePromoted},
		{"expired promoted", func(f *workerFixture, t *testing.T) identity.CredentialRotationState { return f.promoted(t) }, identity.CredentialPhaseAbsent},
		{"aborting", func(f *workerFixture, t *testing.T) identity.CredentialRotationState {
			state := f.candidate(t, false)
			if _, err := f.vault.MarkAbandoned(t.Context(), f.binding, state); err != nil {
				t.Fatal(err)
			}
			resolved, _, _ := f.vault.Resolve(t.Context(), f.binding)
			return resolved
		}, identity.CredentialPhaseAbsent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWorkerFixture(t)
			state := test.prepare(fixture, t)
			if test.name == "candidate" || test.name == "retiring" {
				fixture.advance(time.Minute)
			} else {
				fixture.now = state.ReconcileAfter
			}
			if err := fixture.worker.RunOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			resolved, present, err := fixture.vault.Resolve(t.Context(), fixture.binding)
			if err != nil {
				t.Fatal(err)
			}
			phase := identity.CredentialPhaseAbsent
			if present {
				phase = resolved.Phase
			}
			if phase != test.want {
				t.Fatalf("phase=%s want=%s", phase, test.want)
			}
		})
	}
}

func TestOrphanGraceDoubleCheckAndCrashRecovery(t *testing.T) {
	t.Run("double check finds reference", func(t *testing.T) {
		fixture := newWorkerFixture(t)
		fixture.makeReady(t)
		fixture.advance(2 * time.Hour)
		fixture.repository.orphanEnabled = true
		fixture.repository.referencedChecks = []bool{false, true}
		if err := fixture.worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if fixture.authority.referenceCalls != 2 || !reflect.DeepEqual(fixture.repository.orphanRetryCodes, []string{"credential_referenced"}) {
			t.Fatalf("checks=%d retries=%v", fixture.authority.referenceCalls, fixture.repository.orphanRetryCodes)
		}
		if _, present, _ := fixture.vault.Resolve(t.Context(), fixture.binding); !present {
			t.Fatal("referenced credential deleted")
		}
	})
	t.Run("lost mark response recovers exact deletion", func(t *testing.T) {
		fixture := newWorkerFixture(t)
		fixture.makeReady(t)
		fixture.advance(2 * time.Hour)
		fixture.repository.orphanEnabled = true
		fixture.authority.failAfterMark = true
		if err := fixture.worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		state, present, _ := fixture.vault.Resolve(t.Context(), fixture.binding)
		if !present || state.Phase != identity.CredentialPhaseDeleting {
			t.Fatalf("state=%+v present=%t", state, present)
		}
		if err := fixture.worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, present, err := fixture.vault.Resolve(t.Context(), fixture.binding); err != nil || present {
			t.Fatalf("present=%t err=%v", present, err)
		}
		if fixture.repository.finishOrphanCalls != 1 {
			t.Fatalf("finish calls=%d", fixture.repository.finishOrphanCalls)
		}
	})
	t.Run("crash after main delete resumes durable cleanup claim", func(t *testing.T) {
		fixture := newWorkerFixture(t)
		fixture.makeReady(t)
		fixture.advance(2 * time.Hour)
		fixture.repository.orphanEnabled = true
		fixture.repository.staleFinish = true
		err := fixture.worker.RunOnce(t.Context())
		if !errors.Is(err, ErrStaleLease) {
			t.Fatalf("first run err=%v", err)
		}
		if _, present, resolveErr := fixture.vault.Resolve(t.Context(), fixture.binding); resolveErr != nil || present {
			t.Fatalf("deleted main present=%t err=%v", present, resolveErr)
		}
		fixture.repository.staleFinish = false
		fixture.repository.unfinishedEnabled = true
		fixture.advance(time.Minute)
		if err := fixture.worker.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if fixture.repository.finishOrphanCalls != 1 || fixture.repository.orphanEnabled {
			t.Fatalf("finish calls=%d orphan enabled=%t", fixture.repository.finishOrphanCalls, fixture.repository.orphanEnabled)
		}
	})
}

func TestInventoryMaintenanceFailureDoesNotStarveAccountDeletionJob(t *testing.T) {
	fixture := newWorkerFixture(t)
	state := fixture.makeReady(t)
	fixture.repository.job = fixture.job(state)
	fixture.worker.Inventory = failingInventory{err: errors.New("inventory unavailable")}
	err := fixture.worker.RunOnce(t.Context())
	if Code(err) != "service_unavailable" || fixture.repository.job.Status != "revoke_succeeded" || len(fixture.apple.requests) != 1 {
		t.Fatalf("err=%v status=%s provider=%d", err, fixture.repository.job.Status, len(fixture.apple.requests))
	}
}

type workerFixture struct {
	now        time.Time
	binding    identity.CredentialBinding
	vault      *identity.MemoryCredentialVault
	rotations  identity.CredentialRotator
	repository *workerRepositoryFake
	authority  *workerAuthorityFake
	apple      *workerAppleFake
	worker     *AppleRevocationWorker
}

func newWorkerFixture(t *testing.T) *workerFixture {
	t.Helper()
	fixture := &workerFixture{now: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC), repository: &workerRepositoryFake{},
		authority: &workerAuthorityFake{receipts: map[string][32]byte{}}, apple: &workerAppleFake{}}
	binding, err := identity.NewAppleCredentialBinding(testUUID(1), testUUID(2))
	if err != nil {
		t.Fatal(err)
	}
	fixture.binding = binding
	fixture.vault = identity.NewMemoryCredentialVault(func() time.Time { return fixture.now })
	fixture.rotations, err = identity.NewCredentialRotator(fixture.vault, fixture.apple, func() string { return testUUID(99) })
	if err != nil {
		t.Fatal(err)
	}
	fixture.worker = &AppleRevocationWorker{WorkerID: "worker-1", Repository: fixture.repository, Vault: fixture.vault,
		Rotations: fixture.rotations, Inventory: fixture.vault, Apple: fixture.apple, Now: func() time.Time { return fixture.now },
		Lease: 30 * time.Second, OrphanGrace: time.Hour, authority: fixture.authority}
	return fixture
}

func (fixture *workerFixture) advance(duration time.Duration) {
	fixture.now = fixture.now.Add(duration)
}

func (fixture *workerFixture) reserve(t *testing.T) identity.CredentialRotationState {
	t.Helper()
	state, err := fixture.vault.ReserveRotation(t.Context(), fixture.binding, testUUID(10), testUUID(11))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func (fixture *workerFixture) candidate(t *testing.T, withCurrent bool) identity.CredentialRotationState {
	t.Helper()
	if withCurrent {
		fixture.makeReady(t)
	}
	state, err := fixture.vault.ReserveRotation(t.Context(), fixture.binding, testUUID(20), testUUID(21))
	if err != nil {
		t.Fatal(err)
	}
	state, err = fixture.vault.StoreCandidate(t.Context(), fixture.binding, state, []byte("candidate-refresh"))
	if err != nil {
		t.Fatal(err)
	}
	if withCurrent {
		if _, err := fixture.vault.OpenPrior(t.Context(), fixture.binding, state); err != nil {
			t.Fatal(err)
		}
		state, err = fixture.vault.MarkPriorRevoked(t.Context(), fixture.binding, state)
		if err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func (fixture *workerFixture) promoted(t *testing.T) identity.CredentialRotationState {
	t.Helper()
	state := fixture.candidate(t, false)
	state, err := fixture.vault.PromotePending(t.Context(), fixture.binding, state)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func (fixture *workerFixture) makeReady(t *testing.T) identity.CredentialRotationState {
	t.Helper()
	promotion, err := fixture.rotations.RotateForSession(t.Context(), fixture.binding, testUUID(30), testUUID(31), func(context.Context) (identity.AppleTokens, error) {
		return identity.AppleTokens{IdentityToken: "identity", RefreshToken: "refresh"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var hash [32]byte
	hash[0] = 1
	promotion, err = fixture.rotations.PreparePromotionCAS(t.Context(), fixture.binding, promotion, hash)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := fixture.rotations.ConfirmPromotion(t.Context(), promotion)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.rotations.FinishAdoption(t.Context(), fixture.binding, capability); err != nil {
		t.Fatal(err)
	}
	state, present, err := fixture.vault.Resolve(t.Context(), fixture.binding)
	if err != nil || !present {
		t.Fatalf("ready present=%t err=%v", present, err)
	}
	return state
}

func (fixture *workerFixture) makeAdopting(t *testing.T) identity.CredentialRotationState {
	t.Helper()
	promotion, err := fixture.rotations.RotateForSession(t.Context(), fixture.binding, testUUID(40), testUUID(41), func(context.Context) (identity.AppleTokens, error) {
		return identity.AppleTokens{IdentityToken: "identity", RefreshToken: "refresh"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var hash [32]byte
	hash[0] = 2
	promotion, err = fixture.rotations.PreparePromotionCAS(t.Context(), fixture.binding, promotion, hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.rotations.ConfirmPromotion(t.Context(), promotion); err != nil {
		t.Fatal(err)
	}
	state, _, err := fixture.vault.Resolve(t.Context(), fixture.binding)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func (fixture *workerFixture) makeDeleting(t *testing.T, erase bool) identity.CredentialRotationState {
	t.Helper()
	state := fixture.makeReady(t)
	intent, err := fixture.vault.PrepareDeletionCAS(t.Context(), fixture.binding, state,
		identity.DeletionOperation{OperationID: testUUID(51), CommandID: testUUID(52)})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := fixture.vault.MarkDeleting(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if erase {
		if _, err := fixture.vault.EraseDeletingTokens(t.Context(), capability); err != nil {
			t.Fatal(err)
		}
	}
	state, present, err := fixture.vault.Resolve(t.Context(), fixture.binding)
	if err != nil || !present {
		t.Fatalf("deleting present=%t err=%v", present, err)
	}
	return state
}

func (fixture *workerFixture) job(state identity.CredentialRotationState) *AppleRevocationJob {
	return &AppleRevocationJob{JobID: testUUID(50), OperationVersion: "1", OperationID: testUUID(51), CommandID: testUUID(52),
		Status: "pending", AccountID: fixture.binding.AccountID, ServerInstanceID: fixture.binding.ServerInstanceID, Purpose: fixture.binding.Purpose,
		ObjectID: fixture.binding.ObjectID, ReferenceID: state.Ref.ID, ReferenceKeyID: state.Ref.KeyID}
}

func testUUID(value int) string {
	return "70000000-0000-4000-8000-" + strconv.FormatInt(int64(100000000000+value), 10)
}
