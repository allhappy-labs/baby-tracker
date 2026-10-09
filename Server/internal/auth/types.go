package auth

import (
	"errors"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/security"
)

type AccountView struct {
	ID, DisplayName string
	ProfileRequired bool
}

type FamilySlot struct{ FamilyID, State string }

type RefreshSubject struct{ AccountID, DeviceID string }

type SessionBundle struct {
	AccessToken, RefreshToken, DeviceID string
	DeviceCredential                    string
	RecoveryEpoch                       uint64
	ExpiresAt, ReauthenticateAt         time.Time
	Account                             AccountView
	FamilySlot                          *FamilySlot
}

type DeviceBinding struct {
	DeviceID, AccountID, ServerInstanceID, CanonicalOrigin, CredentialKeyID string
	CredentialDigest                                                        [32]byte
	Revoked                                                                 bool
}

type Options struct {
	Identity         *identity.Service
	Verifier         identity.IdentityVerifier
	Security         *security.Service
	Repository       *Repository
	Apple            identity.AppleCodeExchanger
	Credentials      identity.CredentialRotator
	Mode             config.Mode
	Origin           string
	InstanceID       string
	TokenKeys        identity.DigestKeyring
	DeviceKeys       identity.DigestKeyring
	NotificationKeys identity.DigestKeyring
	ActorIDKey       [32]byte
	Now              func() time.Time
	AccessTTL        time.Duration
	RefreshTTL       time.Duration
	AbsoluteAge      time.Duration
}

type Service struct {
	identity              *identity.Service
	verifier              identity.IdentityVerifier
	security              *security.Service
	repository            *Repository
	apple                 identity.AppleCodeExchanger
	credentials           identity.CredentialRotator
	mode                  config.Mode
	origin, instanceID    string
	tokenKeys, deviceKeys identity.DigestKeyring
	notificationKeys      identity.DigestKeyring
	actorIDKey            [32]byte
	now                   func() time.Time
	accessTTL, refreshTTL time.Duration
	absoluteAge           time.Duration
}

var (
	ErrDeviceContinuityInvalid  = codedError("device_continuity_invalid")
	ErrInvalidRequest           = codedError("invalid_request")
	ErrUnavailable              = codedError("service_unavailable")
	ErrHostedAdapterUnavailable = codedError("hosted_adapter_unavailable")
)

type codedError string

func (err codedError) Error() string { return string(err) }

func Code(err error) string {
	if err == nil {
		return ""
	}
	var coded codedError
	if errors.As(err, &coded) {
		return string(coded)
	}
	if code := identity.Code(err); code != "service_unavailable" {
		return code
	}
	if code := security.Code(err); code != "service_unavailable" {
		return code
	}
	return "service_unavailable"
}

type RefreshDerivationInput struct {
	ServerInstanceID     string
	AccountID            string
	DeviceID             string
	ParentSessionID      string
	LineageRootSessionID string
	RecoveryEpoch        uint64
	ParentKey            identity.DigestKey
	ParentDigest         [32]byte
	OutputKeyID          string
	Scope                string
	FamilyID             *string
	AccessExpiresAt      time.Time
	RefreshExpiresAt     time.Time
	ReauthenticateAt     time.Time
	AbsoluteExpiresAt    time.Time
	HeadGeneration       uint64
	HeadRevision         uint64
	HeadHash             [32]byte
	TargetRevision       uint64
}

type StageCommon struct {
	Version              string  `json:"version"`
	Kind                 string  `json:"kind"`
	AccountID            string  `json:"account_id"`
	DeviceID             string  `json:"device_id"`
	SessionID            string  `json:"session_id"`
	LineageRootSessionID string  `json:"lineage_root_session_id"`
	ServerInstanceID     string  `json:"server_instance_id"`
	CanonicalOrigin      string  `json:"canonical_origin"`
	TokenKeyID           string  `json:"token_key_id"`
	AccessDigest         string  `json:"access_digest"`
	RefreshDigest        string  `json:"refresh_digest"`
	RecoveryEpoch        uint64  `json:"recovery_epoch"`
	SecurityRevision     uint64  `json:"security_revision"`
	Scope                string  `json:"scope"`
	FamilyID             *string `json:"family_id"`
	AccessExpiresAt      string  `json:"access_expires_at"`
	RefreshExpiresAt     string  `json:"refresh_expires_at"`
	ReauthenticateAt     string  `json:"reauthenticate_at"`
	AbsoluteExpiresAt    string  `json:"absolute_expires_at"`
}

type ExchangeStage struct {
	StageCommon
	IdentityProvider        string  `json:"identity_provider"`
	IdentityKeyID           string  `json:"identity_key_id"`
	IdentityDigest          string  `json:"identity_digest"`
	IdentityAudience        string  `json:"identity_audience"`
	IdentitySubject         string  `json:"identity_subject"`
	DisplayName             *string `json:"display_name"`
	ProfileConfirmed        bool    `json:"profile_confirmed"`
	RegisterAccount         bool    `json:"register_account"`
	RegisterDevice          bool    `json:"register_device"`
	InstallationKeyID       *string `json:"installation_key_id"`
	InstallationDigest      *string `json:"installation_digest"`
	ContinuityKeyID         *string `json:"continuity_key_id"`
	ContinuityDigest        *string `json:"continuity_digest"`
	AppleCredentialID       *string `json:"apple_credential_id"`
	AppleCredentialKeyID    *string `json:"apple_credential_key_id"`
	AppleCredentialPurpose  *string `json:"apple_credential_purpose"`
	AppleCredentialObjectID *string `json:"apple_credential_object_id"`
}

type RefreshStage struct {
	StageCommon
	ParentSessionID string            `json:"parent_session_id"`
	Recovery        *RecoveryMaterial `json:"recovery,omitempty"`
}

type RefreshDerivation struct {
	CommandID    string
	SessionID    string
	AccessToken  string
	RefreshToken string
}
