package account

import (
	"context"
	"errors"
	"time"

	"babytracker/server/internal/identity"
	"babytracker/server/internal/security"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type appleRevocationRepository struct{ worker *pgxpool.Pool }

func NewAppleRevocationRepository(worker *pgxpool.Pool) (AppleRevocationRepository, error) {
	if worker == nil {
		return nil, errors.New("apple revocation repository requires worker database")
	}
	return &appleRevocationRepository{worker: worker}, nil
}

func (repository *appleRevocationRepository) Claim(ctx context.Context, workerID string, now, leaseExpiresAt time.Time) (AppleRevocationJob, bool, error) {
	if repository == nil || repository.worker == nil {
		return AppleRevocationJob{}, false, ErrUnavailable
	}
	var result AppleRevocationJob
	err := repository.worker.QueryRow(ctx, `SELECT * FROM app.claim_apple_revocation_job($1::text,$2::timestamptz,$3::timestamptz)`,
		workerID, now, leaseExpiresAt).Scan(&result.JobID, &result.LeaseToken, &result.OperationVersion, &result.OperationID,
		&result.CommandID, &result.Status, &result.AccountID, &result.ServerInstanceID, &result.Purpose, &result.ObjectID,
		&result.ReferenceID, &result.ReferenceKeyID, &result.Attempts, &result.LeaseExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AppleRevocationJob{}, false, nil
	}
	if err != nil {
		return AppleRevocationJob{}, false, mapWorkerRepositoryError(err)
	}
	result.LeaseExpiresAt = result.LeaseExpiresAt.UTC()
	return result, true, nil
}

func (repository *appleRevocationRepository) ValidateClaim(ctx context.Context, claim AppleRevocationJob, now time.Time) error {
	return repository.execAppleClaim(ctx, `SELECT app.validate_apple_revocation_claim(
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, claim, now)
}

func (repository *appleRevocationRepository) CredentialReferencedOutsideJob(ctx context.Context, claim AppleRevocationJob, now time.Time) (bool, error) {
	if repository == nil || repository.worker == nil {
		return false, ErrUnavailable
	}
	var referenced bool
	err := repository.worker.QueryRow(ctx, `SELECT app.apple_credential_referenced_outside_job(
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, appleClaimArguments(claim, now)...).Scan(&referenced)
	return referenced, mapWorkerRepositoryError(err)
}

func (repository *appleRevocationRepository) MarkProviderRevoked(ctx context.Context, claim AppleRevocationJob, now time.Time) error {
	return repository.execAppleClaim(ctx, `SELECT app.mark_apple_provider_revoked(
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, claim, now)
}

func (repository *appleRevocationRepository) BeginCleanup(ctx context.Context, claim AppleRevocationJob, now time.Time) error {
	return repository.execAppleClaim(ctx, `SELECT app.begin_apple_credential_cleanup(
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, claim, now)
}

func (repository *appleRevocationRepository) Complete(ctx context.Context, claim AppleRevocationJob, now time.Time) error {
	return repository.execAppleClaim(ctx, `SELECT app.complete_apple_revocation_job(
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, claim, now)
}

func (repository *appleRevocationRepository) Retry(ctx context.Context, claim AppleRevocationJob, errorCode string, nextAttemptAt, now time.Time) error {
	if repository == nil || repository.worker == nil {
		return ErrUnavailable
	}
	arguments := appleClaimArguments(claim, now)
	arguments = append(arguments[:14:14], errorCode, nextAttemptAt, now)
	_, err := repository.worker.Exec(ctx, `SELECT app.retry_apple_revocation_job(
		$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, arguments...)
	return mapWorkerRepositoryError(err)
}

func (repository *appleRevocationRepository) ClaimOrphanCleanup(ctx context.Context, binding identity.CredentialBinding, ref identity.CredentialRef, workerID string, now, leaseExpiresAt time.Time) (security.CredentialCleanupClaim, bool, error) {
	if repository == nil || repository.worker == nil {
		return security.CredentialCleanupClaim{}, false, ErrUnavailable
	}
	var result security.CredentialCleanupClaim
	err := repository.worker.QueryRow(ctx, `SELECT * FROM app.claim_credential_cleanup(
		$1::uuid,$2::uuid,$3::text,$4::uuid,$5::text,$6::text,$7::text,$8::timestamptz,$9::timestamptz)`,
		binding.AccountID, binding.ServerInstanceID, binding.Purpose, binding.ObjectID, ref.ID, ref.KeyID, workerID, now, leaseExpiresAt).
		Scan(&result.LeaseToken, &result.OperationVersion, &result.OperationID, &result.CommandID, &result.AccountID,
			&result.ObjectID, &result.ReferenceID, &result.ReferenceKeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return security.CredentialCleanupClaim{}, false, nil
	}
	if err != nil {
		return security.CredentialCleanupClaim{}, false, mapWorkerRepositoryError(err)
	}
	return result, true, nil
}

func (repository *appleRevocationRepository) ClaimUnfinishedOrphanCleanup(ctx context.Context, workerID string, now, leaseExpiresAt time.Time) (OrphanCleanupJob, bool, error) {
	if repository == nil || repository.worker == nil {
		return OrphanCleanupJob{}, false, ErrUnavailable
	}
	var result OrphanCleanupJob
	err := repository.worker.QueryRow(ctx, `SELECT * FROM app.claim_unfinished_credential_cleanup(
		$1::text,$2::timestamptz,$3::timestamptz)`, workerID, now, leaseExpiresAt).
		Scan(&result.Claim.LeaseToken, &result.Claim.OperationVersion, &result.Claim.OperationID, &result.Claim.CommandID,
			&result.Binding.AccountID, &result.Binding.ServerInstanceID, &result.Binding.Purpose, &result.Binding.ObjectID,
			&result.Ref.ID, &result.Ref.KeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrphanCleanupJob{}, false, nil
	}
	if err != nil {
		return OrphanCleanupJob{}, false, mapWorkerRepositoryError(err)
	}
	result.Claim.AccountID, result.Claim.ObjectID = result.Binding.AccountID, result.Binding.ObjectID
	result.Claim.ReferenceID, result.Claim.ReferenceKeyID = result.Ref.ID, result.Ref.KeyID
	return result, true, nil
}

func (repository *appleRevocationRepository) ValidateOrphanCleanup(ctx context.Context, claim security.CredentialCleanupClaim, now time.Time) error {
	return repository.execCleanupClaim(ctx, `SELECT app.validate_credential_cleanup_claim($1,$2,$3,$4,$5,$6,$7,$8,$9)`, claim, now)
}

func (repository *appleRevocationRepository) CredentialReferenced(ctx context.Context, claim security.CredentialCleanupClaim, now time.Time) (bool, error) {
	if repository == nil || repository.worker == nil {
		return false, ErrUnavailable
	}
	var referenced bool
	err := repository.worker.QueryRow(ctx, `SELECT app.credential_cleanup_referenced($1,$2,$3,$4,$5,$6,$7,$8,$9)`, cleanupClaimArguments(claim, now)...).Scan(&referenced)
	return referenced, mapWorkerRepositoryError(err)
}

func (repository *appleRevocationRepository) FinishOrphanCleanup(ctx context.Context, claim security.CredentialCleanupClaim, now time.Time) error {
	return repository.execCleanupClaim(ctx, `SELECT app.finish_credential_cleanup($1,$2,$3,$4,$5,$6,$7,$8,$9)`, claim, now)
}

func (repository *appleRevocationRepository) RetryOrphanCleanup(ctx context.Context, claim security.CredentialCleanupClaim, errorCode string, nextAttemptAt, now time.Time) error {
	if repository == nil || repository.worker == nil {
		return ErrUnavailable
	}
	arguments := cleanupClaimArguments(claim, now)
	claimNow := arguments[8]
	arguments = append(arguments[:8:8], errorCode, nextAttemptAt, claimNow)
	_, err := repository.worker.Exec(ctx, `SELECT app.retry_credential_cleanup($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, arguments...)
	return mapWorkerRepositoryError(err)
}

func (repository *appleRevocationRepository) execAppleClaim(ctx context.Context, query string, claim AppleRevocationJob, now time.Time) error {
	if repository == nil || repository.worker == nil {
		return ErrUnavailable
	}
	_, err := repository.worker.Exec(ctx, query, appleClaimArguments(claim, now)...)
	return mapWorkerRepositoryError(err)
}

func (repository *appleRevocationRepository) execCleanupClaim(ctx context.Context, query string, claim security.CredentialCleanupClaim, now time.Time) error {
	if repository == nil || repository.worker == nil {
		return ErrUnavailable
	}
	_, err := repository.worker.Exec(ctx, query, cleanupClaimArguments(claim, now)...)
	return mapWorkerRepositoryError(err)
}

func appleClaimArguments(claim AppleRevocationJob, now time.Time) []any {
	return []any{claim.JobID, claim.LeaseToken, claim.OperationVersion, claim.OperationID, claim.CommandID, claim.Status,
		claim.AccountID, claim.ServerInstanceID, claim.Purpose, claim.ObjectID, claim.ReferenceID, claim.ReferenceKeyID,
		claim.Attempts, claim.LeaseExpiresAt, now}
}

func cleanupClaimArguments(claim security.CredentialCleanupClaim, now time.Time) []any {
	return []any{claim.LeaseToken, claim.OperationVersion, claim.OperationID, claim.CommandID, claim.AccountID,
		claim.ObjectID, claim.ReferenceID, claim.ReferenceKeyID, now}
}

func mapWorkerRepositoryError(err error) error {
	if err == nil {
		return nil
	}
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) && databaseError.Code == "BT003" {
		return ErrStaleLease
	}
	return err
}
