package security

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"babytracker/server/internal/access"
)

const (
	maxEventBytes  = 64 << 10
	maxWireInteger = uint64(9007199254740991)
)

type Event struct {
	Version        string    `json:"version"`
	Generation     uint64    `json:"generation"`
	Revision       uint64    `json:"revision"`
	PreviousHash   string    `json:"previous_hash"`
	CommandID      string    `json:"command_id"`
	CommandHash    string    `json:"command_hash"`
	ActorAccountID string    `json:"actor_account_id"`
	ActorDeviceID  string    `json:"actor_device_id"`
	Kind           EventKind `json:"kind"`
	At             string    `json:"at"`
	Effects        []Effect  `json:"effects"`
}

type Effect struct {
	Kind                     EventKind `json:"kind"`
	AccountID                string    `json:"account_id,omitempty"`
	FamilyID                 string    `json:"family_id,omitempty"`
	DeviceID                 string    `json:"device_id,omitempty"`
	SessionID                string    `json:"session_id,omitempty"`
	ParentSessionID          string    `json:"parent_session_id,omitempty"`
	LineageRootSessionID     string    `json:"lineage_root_session_id,omitempty"`
	ServerInstanceID         string    `json:"server_instance_id,omitempty"`
	CanonicalOrigin          string    `json:"canonical_origin,omitempty"`
	InvitationID             string    `json:"invitation_id,omitempty"`
	IdentityKey              string    `json:"identity_key,omitempty"`
	TokenDigest              string    `json:"token_digest,omitempty"`
	DeviceCredentialDigest   string    `json:"device_credential_digest,omitempty"`
	ProjectionMaterialDigest string    `json:"projection_material_digest,omitempty"`
	RefreshAttemptID         string    `json:"refresh_attempt_id,omitempty"`
	Role                     string    `json:"role,omitempty"`
	Status                   string    `json:"status,omitempty"`
	EntitlementKey           string    `json:"entitlement_key,omitempty"`
	Source                   string    `json:"source,omitempty"`
	ExpiresAt                string    `json:"expires_at,omitempty"`
	PurgeAfter               string    `json:"purge_after,omitempty"`
	RecoveryEpoch            uint64    `json:"recovery_epoch,omitempty"`
	OperatorPolicyID         string    `json:"operator_policy_id,omitempty"`
	OperatorEvidenceDigest   string    `json:"operator_evidence_digest,omitempty"`
	OperatorReasonCode       string    `json:"operator_reason_code,omitempty"`
	ExpectedRevision         *uint64   `json:"expected_revision,omitempty"`
	PurgeStartCommandID      string    `json:"purge_start_command_id,omitempty"`
	PurgeStartRevision       *uint64   `json:"purge_start_revision,omitempty"`
	PhysicalPurgeReceipt     string    `json:"physical_purge_receipt,omitempty"`
	PurgedAt                 string    `json:"purged_at,omitempty"`
	ExpectedStartRevision    *uint64   `json:"expected_start_revision,omitempty"`
}

var (
	canonicalUUID    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	canonicalHash    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	operatorPolicyID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,127}$`)
	operatorReason   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	keyedDigest      = regexp.MustCompile(`^hmac-sha256:[a-z][a-z0-9._-]{0,63}:[0-9a-f]{64}$`)
)

type EventKind string

type eventDescriptor struct {
	newPayload     func() any
	allowedEffects []EventKind
	validate       func(Event) error
	apply          func(*State, Event) error
}

func describeEvent(newPayload func() any, validate func(Event) error, apply func(*State, Event) error, allowedEffects ...EventKind) eventDescriptor {
	if newPayload == nil || validate == nil || apply == nil || len(allowedEffects) == 0 {
		panic("incomplete event descriptor")
	}
	return eventDescriptor{newPayload: newPayload, allowedEffects: allowedEffects, validate: validate, apply: apply}
}

var eventDescriptors = map[EventKind]eventDescriptor{
	"account_registered":             describeEvent(func() any { return &AccountRegisteredPayload{} }, validateAccountRegistered, applyAccountRegistered, "account_registered"),
	"identity_unavailable":           describeEvent(func() any { return &IdentityAuthorityPayload{} }, validateIdentityUnavailable, applyIdentityUnavailable, "identity_unavailable"),
	"identity_restored":              describeEvent(func() any { return &IdentityAuthorityPayload{} }, validateIdentityRestored, applyIdentityRestored, "identity_restored"),
	"device_registered":              describeEvent(func() any { return &DevicePayload{} }, validateDeviceRegistered, applyDeviceRegistered, "device_registered"),
	"device_binding_revoked":         describeEvent(func() any { return &DevicePayload{} }, validateDeviceBindingRevoked, applyDeviceBindingRevoked, "device_binding_revoked"),
	"session_granted":                describeEvent(func() any { return &SessionPayload{} }, validateSessionGranted, applySessionGranted, "session_granted"),
	"session_rotated":                describeEvent(func() any { return &SessionPayload{} }, validateSessionRotated, applySessionRotated, "session_rotated"),
	"session_revoked":                describeEvent(func() any { return &SessionPayload{} }, validateSessionRevoked, applySessionRevoked, "session_revoked"),
	"identity_session_issued":        describeEvent(func() any { return &IdentitySessionIssuedPayload{} }, validateIdentitySessionIssued, applyIdentitySessionIssued, "account_registered", "device_registered", "session_granted"),
	"family_created":                 describeEvent(func() any { return &FamilyCreatedPayload{} }, validateFamilyCreated, applyFamilyCreated, "family_created"),
	"family_bootstrapped":            describeEvent(func() any { return &FamilyBootstrappedPayload{} }, validateFamilyBootstrapped, applyFamilyBootstrapped, "family_created", "entitlement_granted"),
	"family_profile_repaired":        describeEvent(func() any { return &FamilyProfileRepairedPayload{} }, validateFamilyProfileRepaired, applyFamilyProfileRepaired, "family_profile_repaired"),
	"invitation_created":             describeEvent(func() any { return &InvitationPayload{} }, validateInvitationCreated, applyInvitationCreated, "invitation_created"),
	"invitation_revoked":             describeEvent(func() any { return &InvitationPayload{} }, validateInvitationRevoked, applyInvitationRevoked, "invitation_revoked"),
	"invitation_redeemed":            describeEvent(func() any { return &InvitationPayload{} }, validateInvitationRedeemed, applyInvitationRedeemed, "invitation_redeemed"),
	"member_left":                    describeEvent(func() any { return &MembershipPayload{} }, validateMemberRemoved, applyMemberRemoved, "member_left"),
	"member_removed":                 describeEvent(func() any { return &MembershipPayload{} }, validateMemberRemoved, applyMemberRemoved, "member_removed"),
	"member_role_changed":            describeEvent(func() any { return &MemberRolePayload{} }, validateMemberRoleChanged, applyMemberRoleChanged, "member_role_changed"),
	"owner_transferred":              describeEvent(func() any { return &OwnerChangePayload{} }, validateOwnerTransferred, applyOwnerTransferred, "owner_transferred"),
	"owner_recovered":                describeEvent(func() any { return &OwnerChangePayload{} }, validateOwnerRecovered, applyOwnerRecovered, "owner_recovered"),
	"entitlement_granted":            describeEvent(func() any { return &EntitlementPayload{} }, validateEntitlementGranted, applyEntitlementGranted, "entitlement_granted"),
	"entitlement_suspended":          describeEvent(func() any { return &EntitlementPayload{} }, validateEntitlementSuspended, applyEntitlementSuspended, "entitlement_suspended"),
	"operator_entitlement_granted":   describeEvent(func() any { return &OperatorEntitlementPayload{} }, validateOperatorEntitlementGranted, applyOperatorEntitlementGranted, "operator_entitlement_granted"),
	"operator_entitlement_suspended": describeEvent(func() any { return &OperatorEntitlementPayload{} }, validateOperatorEntitlementSuspended, applyOperatorEntitlementSuspended, "operator_entitlement_suspended"),
	"family_deletion_scheduled":      describeEvent(func() any { return &FamilyLifecyclePayload{} }, validateFamilyDeletionScheduled, applyFamilyDeletionScheduled, "family_deletion_scheduled"),
	"family_recovered":               describeEvent(func() any { return &FamilyLifecyclePayload{} }, validateFamilyRecovered, applyFamilyRecovered, "family_recovered"),
	"recovery_forfeited":             describeEvent(func() any { return &FamilyLifecyclePayload{} }, validateRecoveryForfeited, applyRecoveryForfeited, "recovery_forfeited"),
	"account_deleted":                describeEvent(func() any { return &AccountDeletedPayload{} }, validateAccountDeleted, applyAccountDeleted, "account_deleted"),
	"family_purge_started":           describeEvent(func() any { return &FamilyPurgeStartedPayload{} }, validateFamilyPurgeStarted, applyFamilyPurgeStarted, "family_purge_started"),
	"family_purge_completed":         describeEvent(func() any { return &FamilyPurgeCompletedPayload{} }, validateFamilyPurgeCompleted, applyFamilyPurgeCompleted, "family_purge_completed"),
}

func CanonicalCommandHash(principal access.Principal, command Command) ([32]byte, error) {
	var zero [32]byte
	if _, supported := eventDescriptors[command.Kind]; !validUUID(command.ID) || !supported || !validPrincipal(principal) ||
		principal.RecoveryEpoch > maxWireInteger || principal.SecurityRevision > maxWireInteger {
		return zero, errorCode("invalid_command")
	}
	payload, err := canonicalPayload(command.Kind, command.Payload)
	if err != nil {
		return zero, err
	}
	accounts, ok := sortedCanonicalIDs(command.AffectedAccountIDs)
	if !ok {
		return zero, errorCode("invalid_command")
	}
	families, ok := sortedCanonicalIDs(command.AffectedFamilyIDs)
	if !ok {
		return zero, errorCode("invalid_command")
	}
	if familySemanticCommand(command.Kind) {
		preimage := struct {
			Version            string          `json:"version"`
			CommandID          string          `json:"command_id"`
			Kind               EventKind       `json:"kind"`
			ActorAccountID     string          `json:"actor_account_id"`
			AffectedAccountIDs []string        `json:"affected_account_ids"`
			AffectedFamilyIDs  []string        `json:"affected_family_ids"`
			Payload            json.RawMessage `json:"payload"`
		}{"family-v1", command.ID, command.Kind, principal.AccountID, accounts, families, payload}
		b, err := json.Marshal(preimage)
		if err != nil {
			return zero, errorCode("invalid_command")
		}
		return sha256.Sum256(b), nil
	}
	preimage := struct {
		Version            string          `json:"version"`
		CommandID          string          `json:"command_id"`
		Kind               EventKind       `json:"kind"`
		ActorAccountID     string          `json:"actor_account_id"`
		ActorDeviceID      string          `json:"actor_device_id,omitempty"`
		ActorSessionID     string          `json:"actor_session_id,omitempty"`
		RecoveryEpoch      uint64          `json:"recovery_epoch"`
		AffectedAccountIDs []string        `json:"affected_account_ids"`
		AffectedFamilyIDs  []string        `json:"affected_family_ids"`
		Payload            json.RawMessage `json:"payload"`
	}{"v1", command.ID, command.Kind, principal.AccountID, principal.DeviceID, principal.SessionID, principal.RecoveryEpoch, accounts, families, payload}
	b, err := json.Marshal(preimage)
	if err != nil {
		return zero, errorCode("invalid_command")
	}
	return sha256.Sum256(b), nil
}

func familySemanticCommand(kind EventKind) bool {
	switch kind {
	case "family_created", "family_bootstrapped", "family_profile_repaired", "invitation_created", "invitation_revoked", "invitation_redeemed",
		"member_left", "member_removed", "member_role_changed", "owner_transferred", "owner_recovered", "entitlement_granted", "entitlement_suspended",
		"operator_entitlement_granted", "operator_entitlement_suspended", "family_deletion_scheduled", "family_recovered",
		"recovery_forfeited", "family_purge_started", "family_purge_completed":
		return true
	default:
		return false
	}
}

func canonicalPayload(kind EventKind, raw json.RawMessage) ([]byte, error) {
	descriptor, ok := eventDescriptors[kind]
	if !ok || descriptor.newPayload == nil {
		return nil, ErrUnsupportedEvent
	}
	target := descriptor.newPayload()
	if err := decodeStrict(raw, target, maxEventBytes); err != nil {
		return nil, errorCode("invalid_command")
	}
	b, err := json.Marshal(target)
	if err != nil || !bytes.Contains(b, []byte(`"version":"v1"`)) {
		return nil, errorCode("invalid_command")
	}
	return b, nil
}

func EncodeEvent(event Event) ([]byte, error) {
	if err := validateEvent(event); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(event)
	if err != nil || len(encoded) > maxEventBytes {
		return nil, ErrLedgerCorrupt
	}
	return encoded, nil
}

func decodeEventStrict(data []byte, event *Event) error {
	return decodeStrict(data, event, maxEventBytes)
}

func decodeStrict(data []byte, target any, limit int) error {
	if len(data) == 0 || len(data) > limit || !utf8.Valid(data) {
		return errors.New("invalid JSON size")
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || seen[key] {
					return errors.New("duplicate JSON key")
				}
				seen[key] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		default:
			return errors.New("invalid JSON delimiter")
		}
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func validateEvent(event Event) error {
	descriptor, supported := eventDescriptors[event.Kind]
	if event.Version != "v1" || !supported {
		return ErrUnsupportedEvent
	}
	if event.Generation == 0 || event.Generation > maxWireInteger || event.Revision == 0 || event.Revision > maxWireInteger || !canonicalHash.MatchString(event.PreviousHash) ||
		!canonicalHash.MatchString(event.CommandHash) || !validUUID(event.CommandID) || !validUUID(event.ActorAccountID) {
		return ErrLedgerCorrupt
	}
	if event.ActorDeviceID != "" && !validUUID(event.ActorDeviceID) {
		return ErrLedgerCorrupt
	}
	at, err := time.Parse(time.RFC3339Nano, event.At)
	if err != nil || at.Location() != time.UTC || at.Format(time.RFC3339Nano) != event.At {
		return ErrLedgerCorrupt
	}
	if len(event.Effects) == 0 {
		return ErrLedgerCorrupt
	}
	for _, effect := range event.Effects {
		if _, supported := eventDescriptors[effect.Kind]; !supported || !slices.Contains(descriptor.allowedEffects, effect.Kind) {
			return ErrUnsupportedEvent
		}
		for _, id := range []string{effect.AccountID, effect.FamilyID, effect.DeviceID, effect.SessionID, effect.ParentSessionID, effect.LineageRootSessionID, effect.ServerInstanceID, effect.InvitationID, effect.PurgeStartCommandID, effect.PhysicalPurgeReceipt, effect.RefreshAttemptID} {
			if id != "" && !validUUID(id) {
				return ErrLedgerCorrupt
			}
		}
		for _, timestamp := range []string{effect.ExpiresAt, effect.PurgeAfter, effect.PurgedAt} {
			if timestamp != "" {
				parsed, err := time.Parse(time.RFC3339Nano, timestamp)
				if err != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != timestamp {
					return ErrLedgerCorrupt
				}
			}
		}
		if effect.RecoveryEpoch > maxWireInteger {
			return ErrLedgerCorrupt
		}
		if effect.ExpectedRevision != nil && *effect.ExpectedRevision > maxWireInteger {
			return ErrLedgerCorrupt
		}
		for _, revision := range []*uint64{effect.PurgeStartRevision, effect.ExpectedStartRevision} {
			if revision != nil && (*revision == 0 || *revision > maxWireInteger) {
				return ErrLedgerCorrupt
			}
		}
	}
	return validateEventEffects(event)
}

func validateEventEffects(event Event) error {
	if len(event.Effects) == 0 {
		return ErrLedgerCorrupt
	}
	ordered := append([]Effect(nil), event.Effects...)
	sortedEffects(ordered)
	if !slices.Equal(ordered, event.Effects) {
		return ErrLedgerCorrupt
	}
	seen := map[string]bool{}
	for _, effect := range event.Effects {
		encoded, _ := json.Marshal(effect)
		if seen[string(encoded)] {
			return ErrLedgerCorrupt
		}
		seen[string(encoded)] = true
		for _, opaque := range []string{effect.CanonicalOrigin, effect.IdentityKey, effect.TokenDigest, effect.DeviceCredentialDigest, effect.Role, effect.Status, effect.EntitlementKey, effect.Source, effect.OperatorPolicyID, effect.OperatorEvidenceDigest, effect.OperatorReasonCode} {
			if opaque != "" && !validOpaque(opaque) {
				return ErrLedgerCorrupt
			}
		}
	}
	return dispatchEventValidation(eventDescriptors, event)
}

func validOpaque(value string) bool {
	return len(value) >= 1 && len(value) <= 4096 && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validMemberRole(role string) bool { return role == "owner" || role == "caregiver" }

// validInvitationRole is deliberately NOT `validMemberRole`. An owner transfer
// asserts an owner/caregiver pair through that one, and widening it to admit a
// viewer would let a transfer hand the family to somebody who cannot write in
// it. An invitation is the opposite case: never an owner, either role below.
func validInvitationRole(role string) bool { return role == "caregiver" || role == "viewer" }

func exactEffectKeys(effect Effect, required, allowed []string) bool {
	keys := map[string]bool{"kind": effect.Kind != "", "account_id": effect.AccountID != "", "family_id": effect.FamilyID != "", "device_id": effect.DeviceID != "", "session_id": effect.SessionID != "", "parent_session_id": effect.ParentSessionID != "", "lineage_root_session_id": effect.LineageRootSessionID != "", "server_instance_id": effect.ServerInstanceID != "", "canonical_origin": effect.CanonicalOrigin != "", "invitation_id": effect.InvitationID != "", "identity_key": effect.IdentityKey != "", "token_digest": effect.TokenDigest != "", "device_credential_digest": effect.DeviceCredentialDigest != "", "projection_material_digest": effect.ProjectionMaterialDigest != "", "refresh_attempt_id": effect.RefreshAttemptID != "", "role": effect.Role != "", "status": effect.Status != "", "entitlement_key": effect.EntitlementKey != "", "source": effect.Source != "", "expires_at": effect.ExpiresAt != "", "purge_after": effect.PurgeAfter != "", "recovery_epoch": effect.RecoveryEpoch != 0, "operator_policy_id": effect.OperatorPolicyID != "", "operator_evidence_digest": effect.OperatorEvidenceDigest != "", "operator_reason_code": effect.OperatorReasonCode != "", "expected_revision": effect.ExpectedRevision != nil, "purge_start_command_id": effect.PurgeStartCommandID != "", "purge_start_revision": effect.PurgeStartRevision != nil, "physical_purge_receipt": effect.PhysicalPurgeReceipt != "", "purged_at": effect.PurgedAt != "", "expected_start_revision": effect.ExpectedStartRevision != nil}
	allowedSet := map[string]bool{}
	for _, key := range allowed {
		allowedSet[key] = true
	}
	for key, present := range keys {
		if present && !allowedSet[key] {
			return false
		}
	}
	for _, key := range required {
		if !keys[key] {
			return false
		}
	}
	return true
}

func RetentionWorkerActorID(serverInstanceID string) (string, error) {
	if !validUUID(serverInstanceID) {
		return "", ErrLedgerCorrupt
	}
	digest := sha256.Sum256([]byte("baby-tracker/retention-worker/v1/" + serverInstanceID))
	digest[6] = (digest[6] & 0x0f) | 0x80
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16]), nil
}

func retentionActorMatches(actorAccountID, serverInstanceID string) bool {
	expected, err := RetentionWorkerActorID(serverInstanceID)
	return err == nil && actorAccountID == expected
}

func validateCandidate(prior Head, candidate Candidate) error {
	if candidate.Prior != prior || sha256.Sum256(candidate.EventJSON) != candidate.EventHash {
		return ErrLedgerCorrupt
	}
	var event Event
	if err := decodeEventStrict(candidate.EventJSON, &event); err != nil {
		return ErrLedgerCorrupt
	}
	if event.Generation != prior.Generation || event.Revision != prior.Revision+1 || event.CommandID != candidate.CommandID ||
		event.PreviousHash != hex.EncodeToString(prior.EventHash[:]) {
		return ErrLedgerCorrupt
	}
	canonical, err := EncodeEvent(event)
	if err != nil || !bytes.Equal(canonical, candidate.EventJSON) {
		return ErrLedgerCorrupt
	}
	return nil
}

func Replay(candidates []Candidate) (State, error) {
	state := NewState()
	prior := Head{Generation: 1}
	for _, candidate := range candidates {
		if err := validateCandidate(prior, candidate); err != nil {
			return State{}, err
		}
		var event Event
		if err := decodeEventStrict(candidate.EventJSON, &event); err != nil {
			return State{}, ErrLedgerCorrupt
		}
		if err := state.apply(event, candidate.EventHash); err != nil {
			return State{}, err
		}
		prior = Head{Generation: event.Generation, Revision: event.Revision, EventHash: candidate.EventHash}
	}
	state.head = prior
	return state, nil
}

func validUUID(value string) bool {
	return canonicalUUID.MatchString(value) && value != "00000000-0000-0000-0000-000000000000"
}

func validPrincipal(principal access.Principal) bool {
	return validUUID(principal.AccountID) && (principal.DeviceID == "" || validUUID(principal.DeviceID)) &&
		(principal.SessionID == "" || validUUID(principal.SessionID))
}

func sortedCanonicalIDs(input []string) ([]string, bool) {
	result := append([]string(nil), input...)
	for _, id := range result {
		if !validUUID(id) {
			return nil, false
		}
	}
	sort.Strings(result)
	for i := 1; i < len(result); i++ {
		if result[i] == result[i-1] {
			return nil, false
		}
	}
	return result, true
}

func decodeHash(value string) ([32]byte, error) {
	var result [32]byte
	b, err := hex.DecodeString(value)
	if err != nil || len(b) != len(result) {
		return result, fmt.Errorf("invalid hash")
	}
	copy(result[:], b)
	return result, nil
}
