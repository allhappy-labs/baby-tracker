package account

import (
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/identity"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
)

func TestAppleRevocationRepositoryRoundTripsEveryJobPhase(t *testing.T) {
	fixture := testpg.New(t)
	repository, err := NewAppleRevocationRepository(fixture.Revoker)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	want := seedAppleRevocationJob(t, fixture, now)

	claim, ok, err := repository.Claim(t.Context(), "worker-1", now, now.Add(30*time.Second))
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.JobID != want.JobID || claim.Status != "pending" || claim.AccountID != want.AccountID ||
		claim.ServerInstanceID != want.ServerInstanceID || claim.Purpose != want.Purpose || claim.ObjectID != want.ObjectID ||
		claim.ReferenceID != want.ReferenceID || claim.ReferenceKeyID != want.ReferenceKeyID || claim.Attempts != 1 {
		t.Fatalf("claim=%+v want seed=%+v", claim, want)
	}
	if err := repository.ValidateClaim(t.Context(), claim, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	stale := claim
	stale.Attempts++
	if err := repository.ValidateClaim(t.Context(), stale, now.Add(time.Second)); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("stale err=%v", err)
	}
	referenced, err := repository.CredentialReferencedOutsideJob(t.Context(), claim, now.Add(time.Second))
	if err != nil || referenced {
		t.Fatalf("referenced=%t err=%v", referenced, err)
	}
	if err := repository.MarkProviderRevoked(t.Context(), claim, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	claim = claimAgain(t, repository, now.Add(2*time.Second), "revoke_succeeded")
	if err := repository.BeginCleanup(t.Context(), claim, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	claim = claimAgain(t, repository, now.Add(4*time.Second), "cleanup_pending")
	if err := repository.Complete(t.Context(), claim, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repository.Claim(t.Context(), "worker-1", now.Add(time.Minute), now.Add(2*time.Minute)); err != nil || ok {
		t.Fatalf("completed claim ok=%t err=%v", ok, err)
	}
}

func TestAppleRevocationRepositoryRetryAndOrphanCleanup(t *testing.T) {
	fixture := testpg.New(t)
	repository, err := NewAppleRevocationRepository(fixture.Revoker)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	seed := seedAppleRevocationJob(t, fixture, now)
	claim, ok, err := repository.Claim(t.Context(), "worker-1", now, now.Add(30*time.Second))
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if err := repository.Retry(t.Context(), claim, "apple_unavailable", now.Add(time.Minute), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repository.Claim(t.Context(), "worker-1", now.Add(30*time.Second), now.Add(time.Minute)); err != nil || ok {
		t.Fatalf("early retry claim ok=%t err=%v", ok, err)
	}
	retry := claimAgain(t, repository, now.Add(time.Minute), "retry")
	if retry.OperationID != claim.OperationID || retry.CommandID != claim.CommandID || retry.Attempts != claim.Attempts+1 {
		t.Fatalf("retry=%+v original=%+v", retry, claim)
	}

	// A distinct, unreferenced credential can be claimed through the orphan
	// cleanup path without access to any runtime or owner connection.
	binding, err := identity.NewAppleCredentialBinding(testUUID(301), seed.ServerInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	ref := identity.CredentialRef{ID: binding.ObjectID, KeyID: "credential.v1"}
	orphan, ok, err := repository.ClaimOrphanCleanup(t.Context(), binding, ref, "worker-1", now, now.Add(30*time.Second))
	if err != nil || !ok {
		t.Fatalf("orphan claim ok=%t err=%v", ok, err)
	}
	if err := repository.ValidateOrphanCleanup(t.Context(), orphan, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	referenced, err := repository.CredentialReferenced(t.Context(), orphan, now.Add(time.Second))
	if err != nil || referenced {
		t.Fatalf("orphan referenced=%t err=%v", referenced, err)
	}
	stale := orphan
	stale.OperationVersion = "999"
	if err := repository.FinishOrphanCleanup(t.Context(), stale, now.Add(time.Second)); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("stale orphan err=%v", err)
	}
	if err := repository.RetryOrphanCleanup(t.Context(), orphan, "service_unavailable", now.Add(time.Minute), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	resumed, ok, err := repository.ClaimUnfinishedOrphanCleanup(t.Context(), "worker-2", now.Add(time.Minute), now.Add(2*time.Minute))
	if err != nil || !ok {
		t.Fatalf("retry orphan claim ok=%t err=%v", ok, err)
	}
	if resumed.Binding != binding || resumed.Ref != ref || resumed.Claim.OperationID != orphan.OperationID || resumed.Claim.CommandID != orphan.CommandID {
		t.Fatalf("resumed=%+v binding=%+v ref=%+v original=%+v", resumed, binding, ref, orphan)
	}
	if err := repository.FinishOrphanCleanup(t.Context(), resumed.Claim, now.Add(time.Minute+time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestAppleRevocationWorkerResumesRealCleanupClaimAfterMainWasDeleted(t *testing.T) {
	database := testpg.New(t)
	repository, err := NewAppleRevocationRepository(database.Revoker)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newWorkerFixture(t)
	state := fixture.makeReady(t)
	claim, ok, err := repository.ClaimOrphanCleanup(t.Context(), fixture.binding, state.Ref, fixture.worker.WorkerID,
		fixture.now, fixture.now.Add(fixture.worker.Lease))
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	intent, err := fixture.vault.PrepareDeletionCAS(t.Context(), fixture.binding, state,
		identity.DeletionOperation{OperationID: claim.OperationID, CommandID: claim.CommandID})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := fixture.vault.MarkDeleting(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	capability, err = fixture.vault.EraseDeletingTokens(t.Context(), capability)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.vault.Delete(t.Context(), capability); err != nil {
		t.Fatal(err)
	}

	fixture.worker.Repository = repository
	fixture.advance(time.Minute)
	if err := fixture.worker.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := database.Owner.QueryRow(t.Context(), `SELECT status FROM credential_cleanup_jobs
		WHERE account_id=$1 AND credential_object_id=$2`, fixture.binding.AccountID, fixture.binding.ObjectID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "complete" {
		t.Fatalf("cleanup status=%s", status)
	}
}

func seedAppleRevocationJob(t *testing.T, fixture *testpg.Fixture, now time.Time) AppleRevocationJob {
	t.Helper()
	binding, err := identity.NewAppleCredentialBinding(testUUID(300), testUUID(302))
	if err != nil {
		t.Fatal(err)
	}
	job := AppleRevocationJob{JobID: testUUID(303), OperationID: testUUID(304), CommandID: testUUID(305), Status: "pending",
		AccountID: binding.AccountID, ServerInstanceID: binding.ServerInstanceID, Purpose: binding.Purpose, ObjectID: binding.ObjectID,
		ReferenceID: binding.ObjectID, ReferenceKeyID: "credential.v1"}
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetAccountLocal(t.Context(), tx, job.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO accounts(id,display_name) VALUES($1,'Deleted account')`, job.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO apple_revocation_jobs(job_id,account_id,server_instance_id,credential_purpose,credential_object_id,
		credential_reference_id,credential_reference_key_id,status,attempts,next_attempt_at,created_at,updated_at,
		deletion_operation_id,deletion_command_id,operation_version)
		VALUES($1,$2,$3,$4,$5,$6,$7,'pending',0,$8,$8,$8,$9,$10,1)`, job.JobID, job.AccountID, job.ServerInstanceID,
		job.Purpose, job.ObjectID, job.ReferenceID, job.ReferenceKeyID, now, job.OperationID, job.CommandID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return job
}

func claimAgain(t *testing.T, repository AppleRevocationRepository, now time.Time, status string) AppleRevocationJob {
	t.Helper()
	claim, ok, err := repository.Claim(t.Context(), "worker-1", now, now.Add(30*time.Second))
	if err != nil || !ok || claim.Status != status {
		t.Fatalf("claim status=%q want=%q ok=%t err=%v", claim.Status, status, ok, err)
	}
	return claim
}
