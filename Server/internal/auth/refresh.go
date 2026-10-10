package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/identity"
)

var (
	ErrAccessExpired            = codedError("access_expired")
	ErrSessionRevoked           = codedError("session_revoked")
	ErrRefreshReused            = codedError("refresh_reused")
	ErrReauthenticationRequired = codedError("reauthentication_required")
	ErrEpochChanged             = codedError("epoch_changed")
)

// Authenticate resolves a bearer access token to its principal: one
// authority lookup by the token's digest under every configured token key.
func (service *Service) Authenticate(ctx context.Context, rawToken string) (access.Principal, error) {
	if service == nil || service.authority == nil || !validBearerSecret(rawToken) {
		return access.Principal{}, ErrSessionRevoked
	}
	return service.authority.Authenticate(ctx, service.tokenCandidates("access", rawToken))
}

// AccessDigests is a bearer access token's digest under every configured
// token key, current first, with no database work; a malformed token is
// session_revoked, as Authenticate refuses it. The digests are the
// sessions.access_digest lookup keys: callers pass them to the authority
// and never log them. Every well-formed token costs the same HMACs (one per
// key), and a refusal carries no token material.
func (service *Service) AccessDigests(rawToken string) ([]authority.KeyedDigest, error) {
	if service == nil || !validBearerSecret(rawToken) {
		return nil, ErrSessionRevoked
	}
	return service.tokenCandidates("access", rawToken), nil
}

// Refresh rotates a refresh token (the legacy, non-recoverable path): the
// parent is consumed and its child issued, or, for a consumed parent, the
// whole lineage is revoked (refresh_reused).
func (service *Service) Refresh(ctx context.Context, rawToken string) (SessionBundle, error) {
	if service == nil || service.authority == nil || !validBearerSecret(rawToken) {
		return SessionBundle{}, ErrSessionRevoked
	}
	result, err := service.authority.Refresh(ctx, service.refreshCommand(rawToken, "", nil))
	if err != nil {
		return SessionBundle{}, err
	}
	return refreshBundle(result), nil
}

// ResolveRefreshSubject performs the same keyed, epoch-bound lookup Refresh
// uses without consuming or rotating the credential. HTTP uses the returned
// opaque actor IDs only to charge the account/device quotas before the
// rotation.
func (service *Service) ResolveRefreshSubject(ctx context.Context, rawToken string) (RefreshSubject, error) {
	if service == nil || service.authority == nil || !validBearerSecret(rawToken) {
		return RefreshSubject{}, ErrSessionRevoked
	}
	accountID, deviceID, err := service.authority.ResolveRefreshSubject(ctx, service.tokenCandidates("refresh", rawToken))
	if err != nil {
		return RefreshSubject{}, err
	}
	return RefreshSubject{AccountID: accountID, DeviceID: deviceID}, nil
}

// Logout revokes the principal's device binding and every live session on
// that device. An exact retry returns the stored result.
func (service *Service) Logout(ctx context.Context, principal access.Principal, commandID string) (authority.Result, error) {
	if service == nil || service.authority == nil || !validUUID(commandID) {
		return authority.Result{}, ErrInvalidRequest
	}
	return service.authority.Logout(ctx, principal, commandID, time.Time{})
}

// tokenCandidates digests a bearer token under every configured token key,
// current first.
func (service *Service) tokenCandidates(class, raw string) []authority.KeyedDigest {
	keys := append([]identity.DigestKey{service.tokenKeys.Current}, service.tokenKeys.Previous...)
	candidates := make([]authority.KeyedDigest, 0, len(keys))
	for _, key := range keys {
		candidates = append(candidates, tokenDigest(key, class, raw))
	}
	return candidates
}

// refreshCommand is the authority rotation of rawToken. attemptID "" is the
// legacy refresh (v2 derivation, refresh stage material, prove nil);
// otherwise the recoverable refresh (v3 derivation over the attempt and the
// device binding prove accepted, recovery stage material frozen in the
// refresh receipt).
func (service *Service) refreshCommand(rawToken, attemptID string, prove func(authority.DeviceBinding) bool) authority.Refresh {
	command := authority.Refresh{ParentRefreshDigestCandidates: service.tokenCandidates("refresh", rawToken), AttemptID: attemptID,
		ProveContinuity: prove, OutputKeyID: service.tokenKeys.Current.ID, AccessTTL: service.accessTTL, RefreshTTL: service.refreshTTL,
		Mode: authority.Mode(service.mode), Derive: service.deriveRotation, Material: service.rotationMaterial}
	if attemptID != "" {
		command.Derive, command.Material = service.deriveRecoverableRotation, service.recoverableRotationMaterial
	}
	return command
}

// derivationInput maps the authority's derivation input onto the frozen v2
// domain. The legacy ledger head fields keep their names but are fed values
// fixed by the parent session row: HeadGeneration 1, HeadRevision the
// parent's issued_seq, HeadHash sha256 of the parent session ID and
// TargetRevision issued_seq+1. ReauthenticateAt and AbsoluteExpiresAt are the
// lineage's deadline_at. Every value is read from the parent and child rows,
// so a lost-acknowledgement retry re-derives the committed child exactly.
func (service *Service) derivationInput(in authority.RefreshDerivationInput) RefreshDerivationInput {
	parent := in.Parent
	var familyID *string
	if in.FamilyID != "" {
		familyID = stringPointer(in.FamilyID)
	}
	return RefreshDerivationInput{ServerInstanceID: service.instanceID, AccountID: parent.AccountID, DeviceID: parent.DeviceID,
		ParentSessionID: parent.ID, LineageRootSessionID: parent.LineageRootID, RecoveryEpoch: parent.RecoveryEpoch, ParentKey: in.ParentKey,
		ParentDigest: in.ParentDigest, OutputKeyID: in.OutputKeyID, Scope: in.Scope, FamilyID: familyID,
		AccessExpiresAt: in.AccessExpiresAt.UTC(), RefreshExpiresAt: in.RefreshExpiresAt.UTC(), ReauthenticateAt: parent.DeadlineAt.UTC(),
		AbsoluteExpiresAt: parent.DeadlineAt.UTC(), HeadGeneration: 1, HeadRevision: parent.IssuedSeq, HeadHash: sha256.Sum256([]byte(parent.ID)),
		TargetRevision: parent.IssuedSeq + 1}
}

// recoveryDerivationInput adds the attempt, origin and device proof (v3).
func (service *Service) recoveryDerivationInput(in authority.RefreshDerivationInput) RecoveryDerivationInput {
	input := RecoveryDerivationInput{RefreshDerivationInput: service.derivationInput(in), AttemptID: in.AttemptID, CanonicalOrigin: service.origin}
	if in.Continuity != nil {
		input.ContinuityKeyID, input.ContinuityDigest = in.Continuity.CredentialKeyID, in.Continuity.CredentialDigest
	}
	return input
}

func (service *Service) deriveRotation(in authority.RefreshDerivationInput) (authority.RefreshDerivation, error) {
	derived, err := DeriveRefreshRotation(service.derivationInput(in))
	return authority.RefreshDerivation(derived), err
}

func (service *Service) deriveRecoverableRotation(in authority.RefreshDerivationInput) (authority.RefreshDerivation, error) {
	derived, err := DeriveRecoverableRefreshRotation(service.recoveryDerivationInput(in))
	return authority.RefreshDerivation(derived), err
}

// rotationMaterial is the legacy refresh stage of a rotation: its digest is
// the session_rotated audit row's material_digest.
func (service *Service) rotationMaterial(stage authority.RefreshStage) ([]byte, [32]byte, error) {
	return EncodeRefreshStage(service.refreshStage(stage))
}

// recoverableRotationMaterial is the recovery stage frozen in the refresh
// receipt: the refresh stage plus the v3 derivation input without the key
// secret, the child's creation time and the effective deadline.
func (service *Service) recoverableRotationMaterial(stage authority.RefreshStage) ([]byte, [32]byte, error) {
	material := RecoveryMaterial{Input: FreezeRecoveryInput(service.recoveryDerivationInput(stage.Input)), CreatedAt: stage.Child.CreatedAt.UTC(),
		EffectiveDeadline: stage.EffectiveDeadline.UTC()}
	return EncodeRecoveryRefreshStage(service.refreshStage(stage), material)
}

// refreshStage is the canonical stage of a rotation's child row.
func (service *Service) refreshStage(stage authority.RefreshStage) RefreshStage {
	child := stage.Child
	var familyID *string
	if child.FamilyID != "" {
		familyID = stringPointer(child.FamilyID)
	}
	return RefreshStage{StageCommon: StageCommon{Version: "v1", Kind: "refresh", AccountID: child.AccountID, DeviceID: child.DeviceID,
		SessionID: child.ID, LineageRootSessionID: child.LineageRootID, ServerInstanceID: service.instanceID, CanonicalOrigin: service.origin,
		TokenKeyID: child.TokenKeyID, AccessDigest: hex.EncodeToString(child.AccessDigest[:]), RefreshDigest: hex.EncodeToString(child.RefreshDigest[:]),
		RecoveryEpoch: child.RecoveryEpoch, SecurityRevision: child.IssuedSeq, Scope: child.Scope, FamilyID: familyID,
		AccessExpiresAt: child.AccessExpiresAt.UTC().Format(time.RFC3339Nano), RefreshExpiresAt: child.RefreshExpiresAt.UTC().Format(time.RFC3339Nano),
		ReauthenticateAt: child.DeadlineAt.UTC().Format(time.RFC3339Nano), AbsoluteExpiresAt: child.DeadlineAt.UTC().Format(time.RFC3339Nano)},
		ParentSessionID: stage.Input.Parent.ID}
}

// refreshBundle is the session a rotation (or its recovery) returns: the
// child's tokens, its access expiry capped by the effective reauthentication
// deadline, and the account's current presentation.
func refreshBundle(result authority.RefreshResult) SessionBundle {
	return SessionBundle{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, DeviceID: result.Child.DeviceID,
		RecoveryEpoch: result.Child.RecoveryEpoch, ExpiresAt: minDeadline(result.Child.AccessExpiresAt, result.EffectiveDeadline),
		ReauthenticateAt: result.EffectiveDeadline, Account: AccountView(result.Account), FamilySlot: familySlot(result.FamilySlot)}
}

func minDeadline(first, second time.Time) time.Time {
	if second.Before(first) {
		return second
	}
	return first
}

func validBearerSecret(raw string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == raw
}
