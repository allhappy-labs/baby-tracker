package protocol

import (
	"regexp"
	"slices"
)

var refreshSecretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

const (
	CapabilityIdentityV1         = "identity-v1"
	CapabilityRefreshRecoveryV1  = "refresh-recovery-v1"
	CapabilityFamilyLifecycleV1  = "family-lifecycle-v1"
	CapabilityRecoveryV1         = "recovery-v1"
	CapabilityDeviceContinuityV1 = "device-continuity-v1"
	CapabilityRecordsV1          = "records-v1"
	CapabilitySnapshotV1         = "snapshot-v1"
	CapabilityTimerCommandsV1    = "timer-commands-v1"
	CapabilityImportResolverV1   = "import-resolver-v1"
	CapabilityLiveHintsV1        = "live-hints-v1"
	CapabilityExportV1           = "export-v1"
	// CapabilityPushTokenV1 is the device push-token route (PUT/DELETE
	// /v1/devices/push-token), the one additive wire surface of the
	// PostgreSQL authority redesign (owner decision D4). It is advertised
	// only by a server with APNs wakes configured.
	CapabilityPushTokenV1 = "push-token-v1"
)

var reservedCapabilities = []string{
	CapabilityIdentityV1,
	CapabilityFamilyLifecycleV1,
	CapabilityRecoveryV1,
	CapabilityDeviceContinuityV1,
}

var knownCapabilities = []string{
	CapabilityLiveActivityPushV1,
	CapabilityIdentityV1,
	CapabilityRefreshRecoveryV1,
	CapabilityFamilyLifecycleV1,
	CapabilityRecoveryV1,
	CapabilityDeviceContinuityV1,
	CapabilityRecordsV1,
	CapabilitySnapshotV1,
	CapabilityTimerCommandsV1,
	CapabilityImportResolverV1,
	CapabilityLiveHintsV1,
	CapabilityExportV1,
	CapabilityPushTokenV1,
}

func ReservedCapabilities() []string { return slices.Clone(reservedCapabilities) }

// KnownCapabilities is the protocol registry. Membership does not advertise
// support; ServerInfo.Capabilities remains the authoritative active set.
func KnownCapabilities() []string { return slices.Clone(knownCapabilities) }

func IdentityCapabilities() []string { return []string{CapabilityIdentityV1} }

func RefreshRecoveryCapabilities() []string {
	return []string{CapabilityIdentityV1, CapabilityRefreshRecoveryV1}
}

type RequestMeta struct {
	ProtocolVersion string   `json:"protocol_version"`
	Capabilities    []string `json:"capabilities"`
}

// requestMetadata makes protocol request registration explicit. Embedding
// RequestMeta promotes this method to each request DTO; a request without the
// embedded metadata fails closed in DecodeRequest.
func (meta RequestMeta) requestMetadata() RequestMeta { return meta }

type ChallengeRequest struct {
	RequestMeta
}

type ChallengeResponse struct {
	ProtocolVersion  string `json:"protocol_version"`
	Challenge        string `json:"challenge"`
	ServerInstanceID string `json:"server_instance_id"`
	CanonicalOrigin  string `json:"canonical_origin"`
	ExpiresAt        string `json:"expires_at"`
}

type ProfileInput struct {
	DisplayName string `json:"display_name"`
	Confirmed   bool   `json:"confirmed"`
}

type DeviceContinuityClaim struct {
	DeviceID   string `json:"device_id"`
	Credential string `json:"credential"`
}

type AppleExchangeRequest struct {
	RequestMeta
	CommandID         string                 `json:"command_id"`
	Challenge         string                 `json:"challenge"`
	IdentityToken     string                 `json:"identity_token"`
	AuthorizationCode string                 `json:"authorization_code"`
	InstallationID    string                 `json:"installation_id"`
	Profile           *ProfileInput          `json:"profile"`
	DeviceContinuity  *DeviceContinuityClaim `json:"device_continuity"`
}

type RefreshRequest struct {
	RequestMeta
	RefreshToken string `json:"refresh_token"`
}

type RecoverableRefreshRequest struct {
	RequestMeta
	RefreshToken     string                `json:"refresh_token"`
	RefreshAttemptID string                `json:"refresh_attempt_id"`
	DeviceContinuity DeviceContinuityClaim `json:"device_continuity"`
}

type RefreshAckRequest struct {
	RequestMeta
	RefreshAttemptID string                `json:"refresh_attempt_id"`
	DeviceContinuity DeviceContinuityClaim `json:"device_continuity"`
}

type RecoverableRefreshResponse struct {
	SessionResponse
	RefreshAttemptID string `json:"refresh_attempt_id"`
	ServerInstanceID string `json:"server_instance_id"`
}

func ValidateRefreshRecoveryRequest(input RecoverableRefreshRequest) error {
	if err := ValidateCapabilities(input.Capabilities, RefreshRecoveryCapabilities()); err != nil {
		return err
	}
	if !slices.Equal(input.Capabilities, RefreshRecoveryCapabilities()) || !refreshSecretPattern.MatchString(input.RefreshToken) || !ValidUUID(input.RefreshAttemptID) || !ValidUUID(input.DeviceContinuity.DeviceID) || !refreshSecretPattern.MatchString(input.DeviceContinuity.Credential) {
		return errInvalidRequest
	}
	return nil
}

func ValidateRefreshAckRequest(input RefreshAckRequest) error {
	if err := ValidateCapabilities(input.Capabilities, RefreshRecoveryCapabilities()); err != nil {
		return err
	}
	if !slices.Equal(input.Capabilities, RefreshRecoveryCapabilities()) || !ValidUUID(input.RefreshAttemptID) || !ValidUUID(input.DeviceContinuity.DeviceID) || !refreshSecretPattern.MatchString(input.DeviceContinuity.Credential) {
		return errInvalidRequest
	}
	return nil
}

type LogoutRequest struct {
	RequestMeta
	CommandID string `json:"command_id"`
}

type SessionAccount struct {
	ID              string `json:"id"`
	DisplayName     string `json:"display_name"`
	ProfileRequired bool   `json:"profile_required"`
}

type SessionFamilySlot struct {
	FamilyID string `json:"family_id"`
	State    string `json:"state"`
}

type SessionResponse struct {
	ProtocolVersion  string             `json:"protocol_version"`
	AccessToken      string             `json:"access_token"`
	RefreshToken     string             `json:"refresh_token"`
	ExpiresAt        string             `json:"expires_at"`
	ReauthenticateAt string             `json:"reauthenticate_at"`
	RecoveryEpoch    uint64             `json:"recovery_epoch"`
	DeviceID         string             `json:"device_id"`
	DeviceCredential string             `json:"device_credential,omitempty"`
	Account          SessionAccount     `json:"account"`
	FamilySlot       *SessionFamilySlot `json:"family_slot,omitempty"`
}

func validateRequestMeta(meta RequestMeta) error {
	if meta.ProtocolVersion != "v1" {
		return errUnsupportedProtocol
	}
	if meta.Capabilities == nil {
		return errUnsupportedCapability
	}
	return nil
}
