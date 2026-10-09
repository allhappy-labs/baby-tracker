package auth

import (
	"time"

	"babytracker/server/internal/identity"
)

// RecoveryDerivationInput adds the exact attempt and device proof to a v2 input.
type RecoveryDerivationInput struct {
	RefreshDerivationInput
	AttemptID        string
	CanonicalOrigin  string
	ContinuityKeyID  string
	ContinuityDigest [32]byte
}

// FrozenRecoveryInput holds every v3 derivation input except the parent key secret.
// Its named key must be restored from the configured keyring for replay.
type FrozenRecoveryInput struct {
	DerivationVersion    string    `json:"derivation_version"`
	ServerInstanceID     string    `json:"server_instance_id"`
	AccountID            string    `json:"account_id"`
	DeviceID             string    `json:"device_id"`
	ParentSessionID      string    `json:"parent_session_id"`
	LineageRootSessionID string    `json:"lineage_root_session_id"`
	RecoveryEpoch        uint64    `json:"recovery_epoch"`
	ParentKeyID          string    `json:"parent_key_id"`
	ParentDigest         [32]byte  `json:"parent_digest"`
	OutputKeyID          string    `json:"output_key_id"`
	Scope                string    `json:"scope"`
	FamilyID             *string   `json:"family_id"`
	AccessExpiresAt      time.Time `json:"access_expires_at"`
	RefreshExpiresAt     time.Time `json:"refresh_expires_at"`
	ReauthenticateAt     time.Time `json:"reauthenticate_at"`
	AbsoluteExpiresAt    time.Time `json:"absolute_expires_at"`
	HeadGeneration       uint64    `json:"head_generation"`
	HeadRevision         uint64    `json:"head_revision"`
	HeadHash             [32]byte  `json:"head_hash"`
	TargetRevision       uint64    `json:"target_revision"`
	AttemptID            string    `json:"attempt_id"`
	CanonicalOrigin      string    `json:"canonical_origin"`
	ContinuityKeyID      string    `json:"continuity_key_id"`
	ContinuityDigest     [32]byte  `json:"continuity_digest"`
}

type RecoveryMaterial struct {
	Input             FrozenRecoveryInput `json:"input"`
	CreatedAt         time.Time           `json:"created_at"`
	EffectiveDeadline time.Time           `json:"effective_deadline"`
}

func FreezeRecoveryInput(input RecoveryDerivationInput) FrozenRecoveryInput {
	base := input.RefreshDerivationInput
	var familyID *string
	if base.FamilyID != nil {
		family := *base.FamilyID
		familyID = &family
	}
	return FrozenRecoveryInput{DerivationVersion: "v3", ServerInstanceID: base.ServerInstanceID,
		AccountID: base.AccountID, DeviceID: base.DeviceID, ParentSessionID: base.ParentSessionID,
		LineageRootSessionID: base.LineageRootSessionID, RecoveryEpoch: base.RecoveryEpoch,
		ParentKeyID: base.ParentKey.ID, ParentDigest: base.ParentDigest, OutputKeyID: base.OutputKeyID,
		Scope: base.Scope, FamilyID: familyID, AccessExpiresAt: base.AccessExpiresAt,
		RefreshExpiresAt: base.RefreshExpiresAt, ReauthenticateAt: base.ReauthenticateAt,
		AbsoluteExpiresAt: base.AbsoluteExpiresAt, HeadGeneration: base.HeadGeneration,
		HeadRevision: base.HeadRevision, HeadHash: base.HeadHash, TargetRevision: base.TargetRevision,
		AttemptID: input.AttemptID, CanonicalOrigin: input.CanonicalOrigin,
		ContinuityKeyID: input.ContinuityKeyID, ContinuityDigest: input.ContinuityDigest}
}

func recoveryInputFromFrozen(frozen FrozenRecoveryInput, key identity.DigestKey) RecoveryDerivationInput {
	var familyID *string
	if frozen.FamilyID != nil {
		family := *frozen.FamilyID
		familyID = &family
	}
	return RecoveryDerivationInput{RefreshDerivationInput: RefreshDerivationInput{
		ServerInstanceID: frozen.ServerInstanceID, AccountID: frozen.AccountID, DeviceID: frozen.DeviceID,
		ParentSessionID: frozen.ParentSessionID, LineageRootSessionID: frozen.LineageRootSessionID,
		RecoveryEpoch: frozen.RecoveryEpoch, ParentKey: key, ParentDigest: frozen.ParentDigest,
		OutputKeyID: frozen.OutputKeyID, Scope: frozen.Scope, FamilyID: familyID,
		AccessExpiresAt: frozen.AccessExpiresAt, RefreshExpiresAt: frozen.RefreshExpiresAt,
		ReauthenticateAt: frozen.ReauthenticateAt, AbsoluteExpiresAt: frozen.AbsoluteExpiresAt,
		HeadGeneration: frozen.HeadGeneration, HeadRevision: frozen.HeadRevision,
		HeadHash: frozen.HeadHash, TargetRevision: frozen.TargetRevision,
	}, AttemptID: frozen.AttemptID, CanonicalOrigin: frozen.CanonicalOrigin,
		ContinuityKeyID: frozen.ContinuityKeyID, ContinuityDigest: frozen.ContinuityDigest}
}
