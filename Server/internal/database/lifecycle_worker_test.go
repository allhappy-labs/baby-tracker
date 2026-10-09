package database_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type appleClaim struct {
	jobID, leaseToken, operationVersion, operationID, commandID, status string
	accountID, serverInstanceID, purpose, objectID                      string
	referenceID, referenceKeyID                                         string
	attempts                                                            int
	leaseExpiresAt                                                      time.Time
}

type claimCoordinateMutation struct {
	name  string
	index int
	value any
}

func appleClaimCoordinateMutations(claim appleClaim) []claimCoordinateMutation {
	return []claimCoordinateMutation{
		{"job", 0, "70000000-0000-4000-8000-000000000009"}, {"lease", 1, "wrong"}, {"version", 2, "999"},
		{"operation", 3, "71000000-0000-4000-8000-000000000009"}, {"command", 4, "72000000-0000-4000-8000-000000000009"},
		{"status", 5, "retry"}, {"account", 6, "10000000-0000-4000-8000-000000000009"},
		{"instance", 7, "40000000-0000-4000-8000-000000000009"}, {"purpose", 8, "wrong"},
		{"object", 9, "50000000-0000-4000-8000-000000000009"}, {"reference", 10, "wrong"},
		{"reference key", 11, "wrong-key"}, {"attempts", 12, claim.attempts + 1},
		{"lease expiry", 13, claim.leaseExpiresAt.Add(time.Second)},
	}
}

func TestAppleRevocationRepositoryRequiresFullLiveClaim(t *testing.T) {
	fixture := testpg.New(t)
	claim, now := seedAndClaimAppleJob(t, fixture)
	args := appleClaimArgs(claim, now.Add(time.Second))
	if _, err := fixture.Revoker.Exec(t.Context(), `SELECT app.validate_apple_revocation_claim(
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, args...); err != nil {
		t.Fatalf("valid full claim rejected: %v", err)
	}
	for _, mutation := range appleClaimCoordinateMutations(claim) {
		t.Run(mutation.name, func(t *testing.T) {
			changed := append([]any(nil), args...)
			changed[mutation.index] = mutation.value
			_, err := fixture.Revoker.Exec(t.Context(), `SELECT app.mark_apple_provider_revoked(
				$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, changed...)
			if sqlState(err) != "BT003" {
				t.Fatalf("mutated claim err=%v state=%s", err, sqlState(err))
			}
			assertAppleJobState(t, fixture, claim.jobID, "pending", claim.attempts, claim.operationVersion)
		})
	}
}

func TestEveryAppleMutationRejectsAStaleFullClaim(t *testing.T) {
	tests := []struct {
		name, status, query string
	}{
		{"mark provider", "pending", `SELECT app.mark_apple_provider_revoked(
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`},
		{"begin cleanup", "revoke_succeeded", `SELECT app.begin_apple_credential_cleanup(
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`},
		{"complete", "cleanup_pending", `SELECT app.complete_apple_revocation_job(
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`},
		{"retry", "pending", `SELECT app.retry_apple_revocation_job(
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'provider_unavailable',$15,$16)`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := testpg.New(t)
			claim, now := seedAndClaimAppleJob(t, fixture)
			if claim.status != test.status {
				if _, err := fixture.Owner.Exec(t.Context(), `UPDATE apple_revocation_jobs SET status=$2 WHERE job_id=$1`, claim.jobID, test.status); err != nil {
					t.Fatal(err)
				}
				claim.status = test.status
			}
			base := appleClaimArgs(claim, now.Add(time.Second))
			for _, mutation := range appleClaimCoordinateMutations(claim) {
				t.Run(mutation.name, func(t *testing.T) {
					args := append([]any(nil), base...)
					args[mutation.index] = mutation.value
					if test.name == "retry" {
						args = append(args[:14], now.Add(time.Minute), now.Add(time.Second))
					}
					_, err := fixture.Revoker.Exec(t.Context(), test.query, args...)
					if sqlState(err) != "BT003" {
						t.Fatalf("stale coordinate err=%v state=%s", err, sqlState(err))
					}
					assertAppleJobState(t, fixture, claim.jobID, test.status, claim.attempts, claim.operationVersion)
				})
			}
		})
	}
}

func TestAppleRevocationRepositoryRejectsExpiredLeaseAndStaleOperationVersion(t *testing.T) {
	fixture := testpg.New(t)
	claim, now := seedAndClaimAppleJob(t, fixture)
	args := appleClaimArgs(claim, claim.leaseExpiresAt)
	_, err := fixture.Revoker.Exec(t.Context(), `SELECT app.validate_apple_revocation_claim(
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, args...)
	if sqlState(err) != "BT003" {
		t.Fatalf("deadline-equal lease err=%v", err)
	}
	if _, err := fixture.Owner.Exec(t.Context(), `UPDATE apple_revocation_jobs
		SET lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,next_attempt_at=$2 WHERE job_id=$1`, claim.jobID, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertAppleJobState(t, fixture, claim.jobID, "pending", claim.attempts, claim.operationVersion)
}

func TestAppleRevocationFullClaimLockExcludesConcurrentReclaim(t *testing.T) {
	fixture := testpg.New(t)
	first, now := seedAndClaimAppleJob(t, fixture)
	reclaimAt := first.leaseExpiresAt
	locking, err := fixture.Revoker.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer locking.Rollback(t.Context())
	if _, err := locking.Exec(t.Context(), `SELECT app.validate_apple_revocation_claim(
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, appleClaimArgs(first, now.Add(time.Second))...); err != nil {
		t.Fatalf("acquire full-claim lock: %v", err)
	}
	type claimResult struct {
		claim appleClaim
		err   error
	}
	started := make(chan struct{})
	result := make(chan claimResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var second appleClaim
		close(started)
		err := fixture.Revoker.QueryRow(ctx, `SELECT * FROM app.claim_apple_revocation_job($1,$2,$3)`,
			"worker-2", reclaimAt, reclaimAt.Add(30*time.Second)).Scan(&second.jobID, &second.leaseToken, &second.operationVersion,
			&second.operationID, &second.commandID, &second.status, &second.accountID, &second.serverInstanceID, &second.purpose,
			&second.objectID, &second.referenceID, &second.referenceKeyID, &second.attempts, &second.leaseExpiresAt)
		result <- claimResult{claim: second, err: err}
	}()
	<-started
	concurrent := <-result
	if !errors.Is(concurrent.err, pgx.ErrNoRows) {
		t.Fatalf("concurrent reclaim crossed held full-claim lock: claim=%+v err=%v", concurrent.claim, concurrent.err)
	}
	if err := locking.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	var second appleClaim
	if err := fixture.Revoker.QueryRow(t.Context(), `SELECT * FROM app.claim_apple_revocation_job($1,$2,$3)`,
		"worker-2", reclaimAt, reclaimAt.Add(30*time.Second)).Scan(&second.jobID, &second.leaseToken, &second.operationVersion,
		&second.operationID, &second.commandID, &second.status, &second.accountID, &second.serverInstanceID, &second.purpose,
		&second.objectID, &second.referenceID, &second.referenceKeyID, &second.attempts, &second.leaseExpiresAt); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.Revoker.Exec(t.Context(), `SELECT app.mark_apple_provider_revoked(
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, appleClaimArgs(first, reclaimAt.Add(time.Second))...)
	if sqlState(err) != "BT003" {
		t.Fatalf("stale worker err=%v", err)
	}
	assertAppleJobState(t, fixture, second.jobID, "pending", second.attempts, second.operationVersion)
}

func TestOrphanClaimRoundTripsSecurityCredentialCleanupClaim(t *testing.T) {
	fixture := testpg.New(t)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	row := fixture.Revoker.QueryRow(t.Context(), `SELECT * FROM app.claim_credential_cleanup(
		$1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		"10000000-0000-4000-8000-000000000001", "40000000-0000-4000-8000-000000000001",
		"apple-refresh-revocation-v1", "50000000-0000-4000-8000-000000000001", "credential.ref", "credential.key",
		"worker-1", now, now.Add(30*time.Second))
	var lease, version, operation, command, account, object, reference, referenceKey string
	if err := row.Scan(&lease, &version, &operation, &command, &account, &object, &reference, &referenceKey); err != nil {
		t.Fatal(err)
	}
	args := []any{lease, version, operation, command, account, object, reference, referenceKey, now.Add(time.Second)}
	if _, err := fixture.Revoker.Exec(t.Context(), `SELECT app.validate_credential_cleanup_claim($1,$2,$3,$4,$5,$6,$7,$8,$9)`, args...); err != nil {
		t.Fatalf("round-tripped cleanup claim rejected: %v", err)
	}
	var referenced bool
	if err := fixture.Revoker.QueryRow(t.Context(), `SELECT app.credential_cleanup_referenced($1,$2,$3,$4,$5,$6,$7,$8,$9)`, args...).Scan(&referenced); err != nil {
		t.Fatal(err)
	}
	if referenced {
		t.Fatal("unreferenced orphan reported referenced")
	}
	for _, mutation := range []struct {
		name, query string
		extra       []any
	}{
		{"finish", `SELECT app.finish_credential_cleanup($1,$2,$3,$4,$5,$6,$7,$8,$9)`, nil},
		{"retry", `SELECT app.retry_credential_cleanup($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, []any{"vault_unavailable", now.Add(time.Minute)}},
	} {
		t.Run("stale "+mutation.name, func(t *testing.T) {
			for _, coordinate := range []claimCoordinateMutation{
				{"lease", 0, "wrong"}, {"version", 1, "999"},
				{"operation", 2, "71000000-0000-4000-8000-000000000009"},
				{"command", 3, "72000000-0000-4000-8000-000000000009"},
				{"account", 4, "10000000-0000-4000-8000-000000000009"},
				{"object", 5, "50000000-0000-4000-8000-000000000009"},
				{"reference", 6, "wrong"}, {"reference key", 7, "wrong-key"},
			} {
				t.Run(coordinate.name, func(t *testing.T) {
					stale := append([]any(nil), args...)
					stale[coordinate.index] = coordinate.value
					callArgs := stale
					if mutation.name == "retry" {
						claimNow := stale[8]
						callArgs = append(callArgs[:8:8], mutation.extra[0], mutation.extra[1], claimNow)
					}
					_, err := fixture.Revoker.Exec(t.Context(), mutation.query, callArgs...)
					if sqlState(err) != "BT003" {
						t.Fatalf("stale orphan claim err=%v state=%s", err, sqlState(err))
					}
					var got string
					if err := fixture.Owner.QueryRow(t.Context(), `SELECT status FROM credential_cleanup_jobs WHERE account_id=$1`, account).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if got != "pending" {
						t.Fatalf("orphan cleanup status=%s", got)
					}
				})
			}
		})
	}
	if _, err := fixture.Revoker.Exec(t.Context(), `SELECT app.finish_credential_cleanup($1,$2,$3,$4,$5,$6,$7,$8,$9)`, args...); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT status FROM credential_cleanup_jobs WHERE account_id=$1`, account).Scan(&status); err != nil || status != "complete" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	firstVersion, err := strconv.ParseInt(version, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	var lease2, version2, operation2, command2, account2, object2, reference2, referenceKey2 string
	if err := fixture.Revoker.QueryRow(t.Context(), `SELECT * FROM app.claim_credential_cleanup(
		$1,$2,$3,$4,$5,$6,$7,$8,$9)`, account, "40000000-0000-4000-8000-000000000001",
		"apple-refresh-revocation-v1", object, reference, referenceKey, "worker-2", now.Add(time.Minute), now.Add(time.Minute+30*time.Second)).
		Scan(&lease2, &version2, &operation2, &command2, &account2, &object2, &reference2, &referenceKey2); err != nil {
		t.Fatalf("rearm completed cleanup: %v", err)
	}
	secondVersion, err := strconv.ParseInt(version2, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if operation2 == operation || command2 == command || secondVersion <= firstVersion || account2 != account || object2 != object || reference2 != reference || referenceKey2 != referenceKey {
		t.Fatalf("rearmed cleanup operation=%s command=%s version=%s", operation2, command2, version2)
	}
	_, err = fixture.Revoker.Exec(t.Context(), `SELECT app.finish_credential_cleanup($1,$2,$3,$4,$5,$6,$7,$8,$9)`, args...)
	if sqlState(err) != "BT003" {
		t.Fatalf("old cleanup claim survived rearm: %v", err)
	}
	_ = lease2
}

func TestCompletedDeletionJobStillBlocksOrphanCleanup(t *testing.T) {
	fixture := testpg.New(t)
	claim, now := seedAndClaimAppleJob(t, fixture)
	if _, err := fixture.Owner.Exec(t.Context(), `UPDATE apple_revocation_jobs SET status='complete',completed_at=$2,
		lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL WHERE job_id=$1`, claim.jobID, now); err != nil {
		t.Fatal(err)
	}
	var lease string
	err := fixture.Revoker.QueryRow(t.Context(), `SELECT lease_token FROM app.claim_credential_cleanup(
		$1,$2,$3,$4,$5,$6,$7,$8,$9)`, claim.accountID, claim.serverInstanceID, claim.purpose, claim.objectID,
		claim.referenceID, claim.referenceKeyID, "worker-1", now.Add(time.Minute), now.Add(time.Minute+30*time.Second)).Scan(&lease)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("completed deletion reference orphan claim err=%v lease=%q", err, lease)
	}
}

func seedAndClaimAppleJob(t *testing.T, fixture *testpg.Fixture) (appleClaim, time.Time) {
	t.Helper()
	const account = "10000000-0000-4000-8000-000000000001"
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetAccountLocal(t.Context(), tx, account); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO accounts(id,display_name) VALUES($1,'Owner')`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO apple_revocation_jobs(job_id,account_id,server_instance_id,credential_purpose,credential_object_id,
		credential_reference_id,credential_reference_key_id,status,attempts,next_attempt_at,created_at,updated_at,
		deletion_operation_id,deletion_command_id,operation_version)
		VALUES('70000000-0000-4000-8000-000000000001',$1,'40000000-0000-4000-8000-000000000001',
		'apple-refresh-revocation-v1','50000000-0000-4000-8000-000000000001','credential.ref','credential.key',
		'pending',0,$2,$2,$2,'71000000-0000-4000-8000-000000000001','72000000-0000-4000-8000-000000000001',1)`, account, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var claim appleClaim
	err = fixture.Revoker.QueryRow(t.Context(), `SELECT * FROM app.claim_apple_revocation_job($1,$2,$3)`,
		"worker-1", now, now.Add(30*time.Second)).Scan(&claim.jobID, &claim.leaseToken, &claim.operationVersion,
		&claim.operationID, &claim.commandID, &claim.status, &claim.accountID, &claim.serverInstanceID, &claim.purpose,
		&claim.objectID, &claim.referenceID, &claim.referenceKeyID, &claim.attempts, &claim.leaseExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	return claim, now
}

func appleClaimArgs(claim appleClaim, now time.Time) []any {
	return []any{claim.jobID, claim.leaseToken, claim.operationVersion, claim.operationID, claim.commandID, claim.status,
		claim.accountID, claim.serverInstanceID, claim.purpose, claim.objectID, claim.referenceID, claim.referenceKeyID,
		claim.attempts, claim.leaseExpiresAt, now}
}

func assertAppleJobState(t *testing.T, fixture *testpg.Fixture, jobID, status string, attempts int, operationVersion string) {
	t.Helper()
	var gotStatus string
	var gotAttempts int
	var gotVersion string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT status,attempts,operation_version::text FROM apple_revocation_jobs WHERE job_id=$1`, jobID).
		Scan(&gotStatus, &gotAttempts, &gotVersion); err != nil {
		t.Fatal(err)
	}
	if gotStatus != status || gotAttempts != attempts || gotVersion != operationVersion {
		t.Fatalf("job status=%s attempts=%d version=%s", gotStatus, gotAttempts, gotVersion)
	}
}

func requirePermissionDenied(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("error=%v, want permission denied", err)
	}
}
