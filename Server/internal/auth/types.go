package auth

import (
	"errors"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
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

// Options configures a Service. Authority is the PostgreSQL authority every
// session command runs in; its instance ID must be InstanceID and its token
// keyring must be TokenKeys. Hosted mode requires Apple: the authorization
// code is exchanged and verified before the sign-in transaction, and the
// returned Apple refresh token is not kept.
type Options struct {
	Identity    *identity.Service
	Verifier    identity.IdentityVerifier
	Authority   *authority.Service
	Apple       identity.AppleCodeExchanger
	Mode        config.Mode
	Origin      string
	InstanceID  string
	TokenKeys   identity.DigestKeyring
	DeviceKeys  identity.DigestKeyring
	ActorIDKey  [32]byte
	Now         func() time.Time
	AccessTTL   time.Duration
	RefreshTTL  time.Duration
	AbsoluteAge time.Duration
}

type Service struct {
	identity              *identity.Service
	verifier              identity.IdentityVerifier
	authority             *authority.Service
	apple                 identity.AppleCodeExchanger
	mode                  config.Mode
	origin, instanceID    string
	tokenKeys, deviceKeys identity.DigestKeyring
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

// Code is err's stable code: an auth code, else an explicit identity code
// (which wins over any authority cause it wraps), else the authority's code.
func Code(err error) string {
	if err == nil {
		return ""
	}
	var coded codedError
	if errors.As(err, &coded) {
		return string(coded)
	}
	if code, ok := identity.ExplicitCode(err); ok {
		return code
	}
	return authority.CodeOf(err)
}

// RefreshDerivationInput is the frozen v2 refresh derivation domain. The
// authority feeds HeadGeneration=1, HeadRevision=parent issued_seq,
// HeadHash=sha256(parent session ID) and TargetRevision=issued_seq+1, and
// ReauthenticateAt=AbsoluteExpiresAt=the lineage deadline_at, so the frozen
// v2/v3 known answers are unchanged (see derivationInput).
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
