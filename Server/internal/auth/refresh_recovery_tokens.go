package auth

import (
	"encoding/base64"
	"errors"
	"time"

	"babytracker/server/internal/identity"
)

func DeriveRecoverableRefreshRotation(input RecoveryDerivationInput) (RefreshDerivation, error) {
	fields, err := recoveryRotationFields(input)
	if err != nil {
		return RefreshDerivation{}, err
	}
	key := input.ParentKey.Secret[:]
	command := framedHMAC(key, "baby-tracker-refresh-command-v3", fields...)
	session := framedHMAC(key, "baby-tracker-refresh-session-v3", fields...)
	access := framedHMAC(key, "baby-tracker-refresh-access-v3", fields...)
	refresh := framedHMAC(key, "baby-tracker-refresh-token-v3", fields...)
	return RefreshDerivation{CommandID: uuidV8(command), SessionID: uuidV8(session),
		AccessToken:  base64.RawURLEncoding.EncodeToString(access[:]),
		RefreshToken: base64.RawURLEncoding.EncodeToString(refresh[:])}, nil
}

func recoveryRotationFields(input RecoveryDerivationInput) ([][]byte, error) {
	fields, err := refreshRotationFields(input.RefreshDerivationInput)
	if err != nil || !validUUID(input.AttemptID) || input.AttemptID == "00000000-0000-4000-8000-000000000000" ||
		!validCanonicalOrigin(input.CanonicalOrigin) || !digestKeyIDPattern.MatchString(input.ContinuityKeyID) || allZero(input.ContinuityDigest[:]) {
		return nil, errors.New("invalid recovery derivation input")
	}
	return append(fields, []byte(input.AttemptID), []byte(input.CanonicalOrigin), []byte(input.ContinuityKeyID), input.ContinuityDigest[:]), nil
}

// RestoreRecoveryInput must be given the named configured parent key, not the
// current key. The output key ID remains frozen for digest verification.
func RestoreRecoveryInput(frozen FrozenRecoveryInput, parentKey identity.DigestKey) (RecoveryDerivationInput, error) {
	if frozen.DerivationVersion != "v3" || parentKey.ID != frozen.ParentKeyID {
		return RecoveryDerivationInput{}, errors.New("invalid frozen recovery key or version")
	}
	input := recoveryInputFromFrozen(frozen, parentKey)
	if _, err := recoveryRotationFields(input); err != nil {
		return RecoveryDerivationInput{}, err
	}
	return input, nil
}

// EncodeRecoveryRefreshStage binds the frozen receipt material to the same
// canonical stage bytes whose SHA-256 digest the authority command commits.
func EncodeRecoveryRefreshStage(stage RefreshStage, material RecoveryMaterial) ([]byte, [32]byte, error) {
	if stage.Recovery != nil || material.Input.DerivationVersion != "v3" ||
		!validCanonicalDeadline(material.CreatedAt) || !validCanonicalDeadline(material.EffectiveDeadline) ||
		material.EffectiveDeadline.Before(material.CreatedAt) ||
		material.EffectiveDeadline.After(material.Input.AbsoluteExpiresAt) {
		return nil, [32]byte{}, errors.New("invalid recovery material")
	}
	input := material.Input
	if !validUUID(input.AttemptID) || input.AttemptID == "00000000-0000-4000-8000-000000000000" ||
		!validCanonicalOrigin(input.CanonicalOrigin) || !digestKeyIDPattern.MatchString(input.ContinuityKeyID) ||
		allZero(input.ContinuityDigest[:]) || !digestKeyIDPattern.MatchString(input.ParentKeyID) ||
		allZero(input.ParentDigest[:]) || allZero(input.HeadHash[:]) ||
		input.ServerInstanceID != stage.ServerInstanceID || input.AccountID != stage.AccountID ||
		input.DeviceID != stage.DeviceID || input.ParentSessionID != stage.ParentSessionID ||
		input.LineageRootSessionID != stage.LineageRootSessionID || input.RecoveryEpoch != stage.RecoveryEpoch ||
		input.OutputKeyID != stage.TokenKeyID || input.Scope != stage.Scope ||
		input.CanonicalOrigin != stage.CanonicalOrigin || input.AccessExpiresAt.Format(time.RFC3339Nano) != stage.AccessExpiresAt ||
		input.RefreshExpiresAt.Format(time.RFC3339Nano) != stage.RefreshExpiresAt ||
		input.ReauthenticateAt.Format(time.RFC3339Nano) != stage.ReauthenticateAt ||
		input.AbsoluteExpiresAt.Format(time.RFC3339Nano) != stage.AbsoluteExpiresAt ||
		!validDeadlines(input.AccessExpiresAt, input.RefreshExpiresAt, input.ReauthenticateAt, input.AbsoluteExpiresAt) ||
		input.HeadGeneration == 0 || input.HeadGeneration > maxSafeJSONInteger || input.HeadRevision >= maxSafeJSONInteger || input.TargetRevision != input.HeadRevision+1 ||
		!sameFamily(input.FamilyID, stage.FamilyID) {
		return nil, [32]byte{}, errors.New("recovery material does not match stage")
	}
	if err := validateStageCommon(stage.StageCommon, "refresh"); err != nil || !validUUID(stage.ParentSessionID) {
		return nil, [32]byte{}, errors.New("invalid refresh stage")
	}
	stage.Recovery = &material
	return marshalStage(stage)
}

func sameFamily(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
