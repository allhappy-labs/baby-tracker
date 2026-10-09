package protocol

import (
	"encoding/json"
	"regexp"
	"unicode"
)

const CapabilityLiveActivityPushV1 = "live-activity-push-v1"
const LiveActivityPayloadVersion uint64 = 1

var liveActivityTokenPattern = regexp.MustCompile(`^[0-9a-f]+$`)

// LiveActivityScope is immutable card identity. Authentication remains the
// authority for account/device/family access; this target is only a claim.
type LiveActivityScope struct {
	ServerInstanceID string `json:"server_instance_id"`
	RecoveryEpoch    uint64 `json:"recovery_epoch"`
	FamilyID         string `json:"family_id"`
	BabyID           string `json:"baby_id"`
	KindRaw          string `json:"kind_raw"`
}

func (scope LiveActivityScope) Validate() error {
	if !ValidUUID(scope.ServerInstanceID) || !ValidRecoveryEpoch(scope.RecoveryEpoch) ||
		!ValidUUID(scope.FamilyID) || !ValidUUID(scope.BabyID) || (scope.KindRaw != "feed" && scope.KindRaw != "sleep") {
		return errInvalidRequest
	}
	return nil
}

func (scope LiveActivityScope) MatchesRealm(serverInstanceID string, recoveryEpoch uint64, familyID string) bool {
	return scope.ServerInstanceID == serverInstanceID && scope.RecoveryEpoch == recoveryEpoch && scope.FamilyID == familyID
}

// LiveActivityRevision is the registration/storage shape. APNs content uses
// liveactivity.Revision's separate camelCase mapping. Timestamp is Unix seconds.
type LiveActivityRevision struct {
	Scope                 LiveActivityScope `json:"scope"`
	GenerationID          string            `json:"generation_id"`
	TimerID               *string           `json:"timer_id,omitempty"`
	SessionID             *string           `json:"session_id,omitempty"`
	FamilySequence        uint64            `json:"family_sequence"`
	PresentationVersion   uint64            `json:"presentation_version"`
	PresentationTimestamp uint64            `json:"presentation_timestamp"`
	CanWrite              bool              `json:"can_write"`
}

func (revision LiveActivityRevision) Validate() error {
	if revision.Scope.Validate() != nil || !ValidUUID(revision.GenerationID) || !validOptionalLiveActivityUUID(revision.TimerID) ||
		!validOptionalLiveActivityUUID(revision.SessionID) || revision.FamilySequence > MaxSafeInteger ||
		!validLiveActivityVersion(revision.PresentationVersion) || revision.PresentationTimestamp > MaxSafeInteger {
		return errInvalidRequest
	}
	return nil
}

type LiveActivityDeviceRegistration struct {
	RequestMeta
	InstallationID      string                           `json:"installation_id"`
	RegistrationVersion uint64                           `json:"registration_version"`
	APNsEnvironment     string                           `json:"apns_environment"`
	PayloadVersion      uint64                           `json:"payload_version"`
	PushToStartToken    string                           `json:"push_to_start_token"`
	FeedBreakMinutes    uint64                           `json:"feed_break_minutes"`
	ForegroundInventory *LiveActivityForegroundInventory `json:"foreground_inventory,omitempty"`
}

type LiveActivityAbsentGeneration struct {
	GenerationID      string `json:"generation_id"`
	ObservedAttemptID string `json:"observed_attempt_id"`
}
type LiveActivityForegroundInventory struct {
	InventoryVersion  uint64                         `json:"inventory_version"`
	AbsentGenerations []LiveActivityAbsentGeneration `json:"absent_generations"`
}

func (i LiveActivityForegroundInventory) Validate() error {
	if !validLiveActivityVersion(i.InventoryVersion) || i.AbsentGenerations == nil || len(i.AbsentGenerations) > 16 {
		return errInvalidRequest
	}
	seen := map[string]bool{}
	for _, p := range i.AbsentGenerations {
		if !ValidUUID(p.GenerationID) || !ValidUUID(p.ObservedAttemptID) || seen[p.GenerationID] {
			return errInvalidRequest
		}
		seen[p.GenerationID] = true
	}
	return nil
}

type LiveActivityUnconfirmedStart struct {
	Scope        LiveActivityScope `json:"scope"`
	GenerationID string            `json:"generation_id"`
	AttemptID    string            `json:"attempt_id"`
}

func (input *LiveActivityDeviceRegistration) UnmarshalJSON(raw []byte) error {
	type wire LiveActivityDeviceRegistration
	var decoded wire
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	var preference struct {
		FeedBreakMinutes *uint64 `json:"feed_break_minutes"`
	}
	if err := json.Unmarshal(raw, &preference); err != nil {
		return err
	}
	if preference.FeedBreakMinutes == nil {
		decoded.FeedBreakMinutes = 30
	}
	*input = LiveActivityDeviceRegistration(decoded)
	return nil
}

func (input LiveActivityDeviceRegistration) Validate() error {
	if err := validateLiveActivityMetadata(input.RequestMeta); err != nil {
		return err
	}
	if !ValidUUID(input.InstallationID) || !validLiveActivityVersion(input.RegistrationVersion) ||
		(input.APNsEnvironment != "sandbox" && input.APNsEnvironment != "production") || input.PayloadVersion != LiveActivityPayloadVersion ||
		!validLiveActivityToken(input.PushToStartToken) || input.FeedBreakMinutes < 5 || input.FeedBreakMinutes > 120 || input.FeedBreakMinutes%5 != 0 {
		return errInvalidRequest
	}
	if input.ForegroundInventory != nil {
		return input.ForegroundInventory.Validate()
	}
	return nil
}

type LiveActivityTokenRegistration struct {
	RequestMeta
	InstallationID    string            `json:"installation_id"`
	ActivityID        string            `json:"activity_id"`
	Token             string            `json:"token"`
	TokenVersion      uint64            `json:"token_version"`
	Scope             LiveActivityScope `json:"scope"`
	KnownGenerationID *string           `json:"known_generation_id,omitempty"`
	TimerID           *string           `json:"timer_id,omitempty"`
	SessionID         *string           `json:"session_id,omitempty"`
	State             string            `json:"state"`
}

func (input LiveActivityTokenRegistration) Validate() error {
	if err := validateLiveActivityMetadata(input.RequestMeta); err != nil {
		return err
	}
	if !ValidUUID(input.InstallationID) || !validLiveActivityID(input.ActivityID) || !validLiveActivityToken(input.Token) ||
		!validLiveActivityVersion(input.TokenVersion) || input.Scope.Validate() != nil || !validOptionalLiveActivityUUID(input.KnownGenerationID) ||
		!validOptionalLiveActivityUUID(input.TimerID) || !validOptionalLiveActivityUUID(input.SessionID) ||
		(input.State != "active" && input.State != "ended" && input.State != "dismissed") {
		return errInvalidRequest
	}
	return nil
}

type LiveActivityGenerationMapping struct {
	ActivityID   string            `json:"activity_id"`
	Scope        LiveActivityScope `json:"scope"`
	GenerationID string            `json:"generation_id"`
}

func (mapping LiveActivityGenerationMapping) Validate() error {
	if !validLiveActivityID(mapping.ActivityID) || mapping.Scope.Validate() != nil || !ValidUUID(mapping.GenerationID) {
		return errInvalidRequest
	}
	return nil
}

type LiveActivityRegistrationReceipt struct {
	ProtocolVersion             string                          `json:"protocol_version"`
	ServerInstanceID            string                          `json:"server_instance_id"`
	RecoveryEpoch               uint64                          `json:"recovery_epoch"`
	AcceptedRegistrationVersion uint64                          `json:"accepted_registration_version"`
	AcceptedTokenVersion        *uint64                         `json:"accepted_token_version,omitempty"`
	Generations                 []LiveActivityGenerationMapping `json:"generations"`
	UnconfirmedStarts           []LiveActivityUnconfirmedStart  `json:"unconfirmed_starts,omitempty"`
}

func (receipt LiveActivityRegistrationReceipt) Validate() error {
	if receipt.ProtocolVersion != "v1" {
		return errUnsupportedProtocol
	}
	if !ValidUUID(receipt.ServerInstanceID) || !ValidRecoveryEpoch(receipt.RecoveryEpoch) || !validLiveActivityVersion(receipt.AcceptedRegistrationVersion) ||
		(receipt.AcceptedTokenVersion != nil && !validLiveActivityVersion(*receipt.AcceptedTokenVersion)) || receipt.Generations == nil {
		return errInvalidRequest
	}
	seen := make(map[string]bool, len(receipt.Generations))
	for _, start := range receipt.UnconfirmedStarts {
		if start.Scope.Validate() != nil || !ValidUUID(start.GenerationID) || !ValidUUID(start.AttemptID) || start.Scope.ServerInstanceID != receipt.ServerInstanceID || start.Scope.RecoveryEpoch != receipt.RecoveryEpoch {
			return errInvalidRequest
		}
	}
	for _, mapping := range receipt.Generations {
		if mapping.Validate() != nil || mapping.Scope.ServerInstanceID != receipt.ServerInstanceID || mapping.Scope.RecoveryEpoch != receipt.RecoveryEpoch || seen[mapping.ActivityID] {
			return errInvalidRequest
		}
		seen[mapping.ActivityID] = true
	}
	return nil
}

func validateLiveActivityMetadata(meta RequestMeta) error {
	if err := validateRequestMeta(meta); err != nil {
		return err
	}
	if len(meta.Capabilities) != 1 || meta.Capabilities[0] != CapabilityLiveActivityPushV1 {
		return errUnsupportedCapability
	}
	return nil
}

func validLiveActivityToken(token string) bool {
	return len(token) > 0 && len(token) <= 2048 && len(token)%2 == 0 && liveActivityTokenPattern.MatchString(token)
}

func validLiveActivityVersion(value uint64) bool       { return value > 0 && value <= MaxSafeInteger }
func validOptionalLiveActivityUUID(value *string) bool { return value == nil || ValidUUID(*value) }
func validLiveActivityID(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
			return false
		}
	}
	return true
}
