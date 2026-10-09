package auth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"babytracker/server/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	createChallengeSQL  = `SELECT app.create_auth_challenge($1::bytea,$2::text,$3::uuid,$4::timestamptz,$5::timestamptz)`
	consumeChallengeSQL = `SELECT app.consume_auth_challenge($1::bytea,$2::text,$3::uuid,$4::timestamptz)`
	lookupIdentitySQL   = `SELECT account_id,authority_status,display_name,profile_required FROM app.lookup_auth_identity($1::text,$2::text,$3::bytea)`
	lookupDeviceSQL     = `SELECT continuity_key_id,continuity_digest,binding_revoked FROM app.lookup_device_binding($1::uuid,$2::uuid,$3::uuid,$4::text)`
	lookupAccessSQL     = `SELECT session_id,account_id,device_id,lineage_root_session_id,server_instance_id,recovery_epoch,security_revision,scope,family_id,
		access_expires_at,refresh_expires_at,reauthenticate_at,absolute_expires_at,session_status,consumed_at,revoked_at,revoke_reason
		FROM app.lookup_access_session($1::text,$2::bytea)`
	lookupRefreshSQL = `SELECT session_id,account_id,device_id,parent_session_id,lineage_root_session_id,server_instance_id,recovery_epoch,security_revision,scope,family_id,
		access_expires_at,refresh_expires_at,reauthenticate_at,absolute_expires_at,session_status,refresh_consumed_at,reuse_detected_at,revoked_at,revoke_reason
		FROM app.lookup_refresh_session($1::text,$2::bytea)`
	lineageStartedSQL      = `SELECT app.session_lineage_started_at($1::uuid,$2::uuid)`
	stageMaterialSQL       = `SELECT app.stage_auth_material($1::uuid,$2::bytea,$3::bytea,$4::bytea,$5::timestamptz)`
	reserveNotificationSQL = `SELECT command_id,newly_reserved FROM app.reserve_apple_notification($1::jsonb,$2::text,$3::bytea,$4::uuid,$5::timestamptz)`
)

var errRepositoryUnavailable = errors.New("auth repository is unavailable")

type Repository struct{ runtime *pgxpool.Pool }

type ChallengeRecord struct {
	Digest                            [32]byte
	CanonicalOrigin, ServerInstanceID string
	ExpiresAt, Now                    time.Time
}

type ChallengeConsumption struct {
	Digest                            [32]byte
	CanonicalOrigin, ServerInstanceID string
	Now                               time.Time
}

type AuthIdentityLookup struct {
	AccountID, AuthorityStatus, DisplayName string
	ProfileRequired                         bool
}

type DeviceBindingLookup struct {
	ContinuityKeyID  string
	ContinuityDigest [32]byte
	Revoked          bool
}

type AccessSessionLookup struct {
	SessionID, AccountID, DeviceID, LineageRootSessionID, ServerInstanceID string
	RecoveryEpoch, SecurityRevision                                        uint64
	Scope                                                                  string
	FamilyID                                                               *string
	AccessExpiresAt, RefreshExpiresAt, ReauthenticateAt, AbsoluteExpiresAt time.Time
	Status                                                                 string
	ConsumedAt, RevokedAt                                                  *time.Time
	RevokeReason                                                           *string
}

type RefreshSessionLookup struct {
	SessionID, AccountID, DeviceID, LineageRootSessionID, ServerInstanceID string
	ParentSessionID                                                        *string
	RecoveryEpoch, SecurityRevision                                        uint64
	Scope                                                                  string
	FamilyID                                                               *string
	AccessExpiresAt, RefreshExpiresAt, ReauthenticateAt, AbsoluteExpiresAt time.Time
	Status                                                                 string
	RefreshConsumedAt, ReuseDetectedAt, RevokedAt                          *time.Time
	RevokeReason                                                           *string
}

type AuthMaterialStage struct {
	CommandID                string
	CommandHash              [32]byte
	ProjectionMaterialDigest [32]byte
	Material                 []byte
	Now                      time.Time
}

type notificationCandidate struct {
	KeyID  string `json:"key_id"`
	Digest string `json:"digest"`
}

func NewRepository(runtime *pgxpool.Pool) (*Repository, error) {
	if runtime == nil {
		return nil, errors.New("auth repository requires runtime database")
	}
	return &Repository{runtime: runtime}, nil
}

func (repository *Repository) CreateChallenge(ctx context.Context, record ChallengeRecord) (string, error) {
	if repository == nil || repository.runtime == nil {
		return "", errRepositoryUnavailable
	}
	var id string
	err := repository.runtime.QueryRow(ctx, createChallengeSQL, record.Digest[:], record.CanonicalOrigin, record.ServerInstanceID, record.ExpiresAt, record.Now).Scan(&id)
	return id, err
}

func (repository *Repository) ConsumeChallenge(ctx context.Context, consumption ChallengeConsumption) (string, error) {
	if repository == nil || repository.runtime == nil {
		return "", errRepositoryUnavailable
	}
	var id string
	err := repository.runtime.QueryRow(ctx, consumeChallengeSQL, consumption.Digest[:], consumption.CanonicalOrigin, consumption.ServerInstanceID, consumption.Now).Scan(&id)
	return id, err
}

func (repository *Repository) LookupAuthIdentity(ctx context.Context, provider, keyID string, digest [32]byte) (AuthIdentityLookup, bool, error) {
	if repository == nil || repository.runtime == nil {
		return AuthIdentityLookup{}, false, errRepositoryUnavailable
	}
	var result AuthIdentityLookup
	err := repository.runtime.QueryRow(ctx, lookupIdentitySQL, provider, keyID, digest[:]).Scan(
		&result.AccountID, &result.AuthorityStatus, &result.DisplayName, &result.ProfileRequired)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthIdentityLookup{}, false, nil
	}
	return result, err == nil, err
}

func (repository *Repository) LookupDeviceBinding(ctx context.Context, accountID, deviceID, instanceID, origin string) (DeviceBindingLookup, bool, error) {
	if repository == nil || repository.runtime == nil {
		return DeviceBindingLookup{}, false, errRepositoryUnavailable
	}
	var result DeviceBindingLookup
	var digest []byte
	err := repository.runtime.QueryRow(ctx, lookupDeviceSQL, accountID, deviceID, instanceID, origin).Scan(
		&result.ContinuityKeyID, &digest, &result.Revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceBindingLookup{}, false, nil
	}
	if err != nil {
		return DeviceBindingLookup{}, false, err
	}
	if err := copyDigest(&result.ContinuityDigest, digest); err != nil {
		return DeviceBindingLookup{}, false, err
	}
	return result, true, nil
}

func (repository *Repository) LookupAccessSession(ctx context.Context, keyID string, digest [32]byte) (AccessSessionLookup, bool, error) {
	if repository == nil || repository.runtime == nil {
		return AccessSessionLookup{}, false, errRepositoryUnavailable
	}
	var result AccessSessionLookup
	var recoveryEpoch, securityRevision int64
	err := repository.runtime.QueryRow(ctx, lookupAccessSQL, keyID, digest[:]).Scan(
		&result.SessionID, &result.AccountID, &result.DeviceID, &result.LineageRootSessionID, &result.ServerInstanceID,
		&recoveryEpoch, &securityRevision, &result.Scope, &result.FamilyID, &result.AccessExpiresAt, &result.RefreshExpiresAt,
		&result.ReauthenticateAt, &result.AbsoluteExpiresAt, &result.Status, &result.ConsumedAt, &result.RevokedAt, &result.RevokeReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessSessionLookup{}, false, nil
	}
	if err != nil {
		return AccessSessionLookup{}, false, err
	}
	if recoveryEpoch <= 0 || securityRevision <= 0 {
		return AccessSessionLookup{}, false, errors.New("invalid access session revision")
	}
	result.AccessExpiresAt = result.AccessExpiresAt.UTC()
	result.RefreshExpiresAt = result.RefreshExpiresAt.UTC()
	result.ReauthenticateAt = result.ReauthenticateAt.UTC()
	result.AbsoluteExpiresAt = result.AbsoluteExpiresAt.UTC()
	result.RecoveryEpoch, result.SecurityRevision = uint64(recoveryEpoch), uint64(securityRevision)
	return result, true, nil
}

func (repository *Repository) LookupRefreshSession(ctx context.Context, keyID string, digest [32]byte) (RefreshSessionLookup, bool, error) {
	if repository == nil || repository.runtime == nil {
		return RefreshSessionLookup{}, false, errRepositoryUnavailable
	}
	var result RefreshSessionLookup
	var recoveryEpoch, securityRevision int64
	err := repository.runtime.QueryRow(ctx, lookupRefreshSQL, keyID, digest[:]).Scan(
		&result.SessionID, &result.AccountID, &result.DeviceID, &result.ParentSessionID, &result.LineageRootSessionID, &result.ServerInstanceID,
		&recoveryEpoch, &securityRevision, &result.Scope, &result.FamilyID, &result.AccessExpiresAt, &result.RefreshExpiresAt,
		&result.ReauthenticateAt, &result.AbsoluteExpiresAt, &result.Status, &result.RefreshConsumedAt, &result.ReuseDetectedAt,
		&result.RevokedAt, &result.RevokeReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefreshSessionLookup{}, false, nil
	}
	if err != nil {
		return RefreshSessionLookup{}, false, err
	}
	if recoveryEpoch <= 0 || securityRevision <= 0 {
		return RefreshSessionLookup{}, false, errors.New("invalid refresh session revision")
	}
	result.AccessExpiresAt = result.AccessExpiresAt.UTC()
	result.RefreshExpiresAt = result.RefreshExpiresAt.UTC()
	result.ReauthenticateAt = result.ReauthenticateAt.UTC()
	result.AbsoluteExpiresAt = result.AbsoluteExpiresAt.UTC()
	result.RecoveryEpoch, result.SecurityRevision = uint64(recoveryEpoch), uint64(securityRevision)
	return result, true, nil
}

func (repository *Repository) LineageStartedAt(ctx context.Context, accountID, rootSessionID string) (time.Time, error) {
	if repository == nil || repository.runtime == nil {
		return time.Time{}, errRepositoryUnavailable
	}
	var startedAt *time.Time
	if err := repository.runtime.QueryRow(ctx, lineageStartedSQL, accountID, rootSessionID).Scan(&startedAt); err != nil {
		return time.Time{}, err
	}
	if startedAt == nil || startedAt.IsZero() {
		return time.Time{}, errors.New("missing session lineage root")
	}
	return startedAt.UTC(), nil
}

func (repository *Repository) StageAuthMaterial(ctx context.Context, stage AuthMaterialStage) error {
	if repository == nil || repository.runtime == nil {
		return errRepositoryUnavailable
	}
	_, err := repository.runtime.Exec(ctx, stageMaterialSQL, stage.CommandID, stage.CommandHash[:], stage.ProjectionMaterialDigest[:], stage.Material, stage.Now)
	return err
}

func (repository *Repository) ReserveAppleNotification(ctx context.Context, candidates []identity.KeyedDigest, commandID string, receivedAt time.Time) (string, bool, error) {
	if repository == nil || repository.runtime == nil {
		return "", false, errRepositoryUnavailable
	}
	if len(candidates) == 0 {
		return "", false, errors.New("notification candidates are required")
	}
	payload := make([]notificationCandidate, len(candidates))
	for index, candidate := range candidates {
		payload[index] = notificationCandidate{KeyID: candidate.KeyID, Digest: hex.EncodeToString(candidate.Sum[:])}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", false, err
	}
	var storedCommand string
	var newlyReserved bool
	err = repository.runtime.QueryRow(ctx, reserveNotificationSQL, encoded, candidates[0].KeyID, candidates[0].Sum[:], commandID, receivedAt).
		Scan(&storedCommand, &newlyReserved)
	return storedCommand, newlyReserved, err
}

func copyDigest(destination *[32]byte, source []byte) error {
	if len(source) != len(destination) {
		return fmt.Errorf("invalid database digest length: %d", len(source))
	}
	copy(destination[:], source)
	return nil
}

// RecoveryReceipt contains the exact stage committed by a rotation. Its digest
// must also match the immutable authority session before issuing credentials.
type RecoveryReceipt struct {
	Material                                   []byte
	ProjectionMaterialDigest                   [32]byte
	ChildSessionID, ParentSessionID, AttemptID string
}

func (repository *Repository) LookupRecoveryReceipt(ctx context.Context, parentSessionID, attemptID string) (RecoveryReceipt, bool, error) {
	if repository == nil || repository.runtime == nil {
		return RecoveryReceipt{}, false, errRepositoryUnavailable
	}
	var receipt RecoveryReceipt
	var digest []byte
	err := repository.runtime.QueryRow(ctx, `SELECT material,projection_material_digest,child_session_id,parent_session_id,attempt_id FROM app.lookup_refresh_recovery_receipt($1::uuid,$2::uuid)`, parentSessionID, attemptID).Scan(&receipt.Material, &digest, &receipt.ChildSessionID, &receipt.ParentSessionID, &receipt.AttemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryReceipt{}, false, nil
	}
	if err != nil {
		return RecoveryReceipt{}, false, err
	}
	if len(digest) != 32 {
		return RecoveryReceipt{}, false, errors.New("invalid recovery receipt commitment")
	}
	copy(receipt.ProjectionMaterialDigest[:], digest)
	return receipt, true, nil
}

func (repository *Repository) LookupRecoveryChild(ctx context.Context, sessionID string) (RefreshSessionLookup, bool, error) {
	if repository == nil || repository.runtime == nil {
		return RefreshSessionLookup{}, false, errRepositoryUnavailable
	}
	var keyID string
	var digest []byte
	err := repository.runtime.QueryRow(ctx, `SELECT token_key_id,refresh_digest FROM app.lookup_refresh_recovery_child($1::uuid)`, sessionID).Scan(&keyID, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefreshSessionLookup{}, false, nil
	}
	if err != nil {
		return RefreshSessionLookup{}, false, err
	}
	var sum [32]byte
	if err = copyDigest(&sum, digest); err != nil {
		return RefreshSessionLookup{}, false, err
	}
	return repository.LookupRefreshSession(ctx, keyID, sum)
}

func (repository *Repository) LookupRecoveryAcknowledgements(ctx context.Context, accountID, deviceID, instanceID string, epoch uint64, attemptID string) ([]RecoveryReceipt, error) {
	if repository == nil || repository.runtime == nil {
		return nil, errRepositoryUnavailable
	}
	rows, err := repository.runtime.Query(ctx, `SELECT material,projection_material_digest,child_session_id,parent_session_id,attempt_id FROM app.lookup_refresh_recovery_ack($1::uuid,$2::uuid,$3::uuid,$4::bigint,$5::uuid)`, accountID, deviceID, instanceID, epoch, attemptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []RecoveryReceipt
	for rows.Next() {
		var r RecoveryReceipt
		var digest []byte
		if err = rows.Scan(&r.Material, &digest, &r.ChildSessionID, &r.ParentSessionID, &r.AttemptID); err != nil {
			return nil, err
		}
		if err = copyDigest(&r.ProjectionMaterialDigest, digest); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (repository *Repository) AcknowledgeRecovery(ctx context.Context, accountID, deviceID, instanceID, attemptID string, epoch uint64, lineageID string) error {
	if repository == nil || repository.runtime == nil {
		return errRepositoryUnavailable
	}
	_, err := repository.runtime.Exec(ctx, `SELECT app.ack_refresh_recovery_receipt($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::bigint,$6::uuid)`, accountID, deviceID, instanceID, attemptID, epoch, lineageID)
	return err
}
