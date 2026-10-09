package security

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"time"

	"babytracker/server/internal/access"
)

type AuthoritySnapshot struct {
	View          AuthorityView
	Head          Head
	RecoveryEpoch uint64
	token         *snapshotCapability
}

type snapshotCapability struct {
	id                      uint64
	owner                   *validatedAuthority
	lease                   *fenceLease
	stateHead, exportedHead Head
	recoveryEpoch           uint64
}

type preparedKind uint8

const (
	preparedIdentityIssued preparedKind = iota + 1
	preparedIdentityRestored
	preparedSessionRotation
	preparedRefreshReuse
	preparedDeviceRevocation
	preparedIdentityUnavailable
	preparedOperatorRecovery
	preparedOperatorEntitlementGrant
	preparedOperatorEntitlementSuspend
)

type PreparedCommand struct {
	token      *snapshotCapability
	kind       preparedKind
	principal  access.Principal
	command    Command
	at         time.Time
	provenance *preparedProvenance
}

type preparedProvenance struct {
	token     *snapshotCapability
	kind      preparedKind
	commandID string
}

type IdentityActor struct {
	token      *snapshotCapability
	resolution IdentityAuthorityResolution
	candidates []string
	principal  access.Principal
}

type SessionActor struct {
	token     *snapshotCapability
	principal access.Principal
	device    DeviceView
	session   SessionView
}

type CommandMeta struct {
	CommandID string
	At        time.Time
}

type ExistingSessionActorInput struct {
	AccountID, DeviceID, SessionID  string
	RecoveryEpoch, SecurityRevision uint64
}

type IdentitySessionCommandInput struct {
	Meta    CommandMeta
	Payload IdentitySessionIssuedPayload
}

type SessionRotationCommandInput struct {
	Meta    CommandMeta
	Payload SessionPayload
}

type RefreshReuseCommandInput struct {
	Meta    CommandMeta
	Payload SessionPayload
}

type DeviceRevocationCommandInput struct {
	Meta    CommandMeta
	Payload DevicePayload
}

type IdentityUnavailableCommandInput struct {
	Meta    CommandMeta
	Payload IdentityAuthorityPayload
}

type PrepareCommand func(context.Context, AuthoritySnapshot) (PreparedCommand, error)

func (snapshot AuthoritySnapshot) ResolveIdentityActor(candidates []string, proposedAccountID string, proposedGeneration uint64) (IdentityActor, error) {
	if !snapshot.valid() {
		return IdentityActor{}, errorCode("invalid_command")
	}
	resolution, err := snapshot.token.owner.state.ResolveIdentityAuthority(candidates)
	if err != nil {
		return IdentityActor{}, err
	}
	accountID := resolution.AccountID
	if resolution.Status == "absent" || resolution.Status == "deleted" {
		if !validUUID(proposedAccountID) || proposedGeneration != resolution.NextActorGeneration {
			return IdentityActor{}, errorCode("invalid_command")
		}
		accountID = proposedAccountID
	} else if proposedAccountID != resolution.AccountID || proposedGeneration != resolution.LifecycleGeneration {
		return IdentityActor{}, errorCode("invalid_command")
	}
	return IdentityActor{token: snapshot.token, resolution: resolution, candidates: append([]string(nil), candidates...),
		principal: access.Principal{AccountID: accountID, RecoveryEpoch: snapshot.RecoveryEpoch, SecurityRevision: snapshot.Head.Revision, Scope: "account"}}, nil
}

func (snapshot AuthoritySnapshot) ResolveSessionActor(input ExistingSessionActorInput) (SessionActor, error) {
	if !snapshot.valid() || !validUUID(input.AccountID) || !validUUID(input.DeviceID) || !validUUID(input.SessionID) ||
		input.RecoveryEpoch == 0 || input.RecoveryEpoch != snapshot.RecoveryEpoch || input.SecurityRevision == 0 || input.SecurityRevision > snapshot.Head.Revision {
		return SessionActor{}, errorCode("session_revoked")
	}
	account, accountOK := snapshot.token.owner.state.Account(input.AccountID)
	device, deviceOK := snapshot.token.owner.state.Device(input.DeviceID)
	session, sessionOK := snapshot.token.owner.state.Session(input.SessionID)
	if !accountOK || account.Status != "active" || !deviceOK || (device.Status != "active" && device.Status != "revoked") || device.AccountID != input.AccountID ||
		!sessionOK || (session.Status != "active" && session.Status != "rotated" && session.Status != "revoked") || session.AccountID != input.AccountID || session.DeviceID != input.DeviceID ||
		session.RecoveryEpoch != input.RecoveryEpoch || session.SecurityRevision != input.SecurityRevision {
		return SessionActor{}, errorCode("session_revoked")
	}
	principal := access.Principal{AccountID: input.AccountID, DeviceID: input.DeviceID, SessionID: input.SessionID,
		RecoveryEpoch: input.RecoveryEpoch, SecurityRevision: input.SecurityRevision, Scope: "account"}
	return SessionActor{token: snapshot.token, principal: principal, device: device, session: session}, nil
}

func (snapshot AuthoritySnapshot) PrepareIdentitySessionIssued(actor IdentityActor, input IdentitySessionCommandInput) (PreparedCommand, error) {
	return snapshot.prepareIdentitySession(actor, input, preparedIdentityIssued)
}

func (snapshot AuthoritySnapshot) PrepareIdentityRestored(actor IdentityActor, input IdentitySessionCommandInput) (PreparedCommand, error) {
	return snapshot.prepareIdentitySession(actor, input, preparedIdentityRestored)
}

func (snapshot AuthoritySnapshot) prepareIdentitySession(actor IdentityActor, input IdentitySessionCommandInput, kind preparedKind) (PreparedCommand, error) {
	if !snapshot.validIdentityActor(actor) || !validCommandMeta(input.Meta) {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	payload := input.Payload
	if payload.Version != "v1" || payload.AccountID != actor.principal.AccountID || payload.RecoveryEpoch != snapshot.RecoveryEpoch ||
		!validUUID(payload.AccountID) || !validUUID(payload.DeviceID) || !validUUID(payload.ServerInstanceID) || !validUUID(payload.SessionID) ||
		payload.LineageRootSessionID != payload.SessionID || !keyedDigest.MatchString(payload.IdentityKey) ||
		!keyedDigest.MatchString(payload.DeviceCredentialDigest) || !canonicalHash.MatchString(payload.ProjectionMaterialDigest) ||
		!validOpaque(payload.CanonicalOrigin) || !validFutureTime(payload.ExpiresAt, input.Meta.At) {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	raw, _ := json.Marshal(payload)
	command := Command{ID: input.Meta.CommandID, Kind: "identity_session_issued", AffectedAccountIDs: []string{payload.AccountID}, Payload: raw}
	prepared := snapshot.newPrepared(actor.principal, command, input.Meta.At, kind)
	if replay, err := snapshot.preparedReceipt(prepared); err != nil {
		return PreparedCommand{}, err
	} else if replay {
		return prepared, nil
	}
	switch kind {
	case preparedIdentityIssued:
		if actor.resolution.Status == "absent" || actor.resolution.Status == "deleted" {
			if !payload.RegisterAccount || len(actor.candidates) == 0 || payload.IdentityKey != actor.candidates[0] {
				return PreparedCommand{}, errorCode("invalid_command")
			}
		} else if actor.resolution.Status == "active" {
			if payload.RegisterAccount || payload.IdentityKey != actor.resolution.IdentityKey {
				return PreparedCommand{}, errorCode("invalid_command")
			}
		} else {
			return PreparedCommand{}, errorCode("invalid_command")
		}
	case preparedIdentityRestored:
		if actor.resolution.Status != "identity_unavailable" || payload.RegisterAccount || payload.IdentityKey != actor.resolution.IdentityKey {
			return PreparedCommand{}, errorCode("invalid_command")
		}
	default:
		return PreparedCommand{}, errorCode("invalid_command")
	}
	if err := snapshot.validateDeviceForIssue(payload); err != nil {
		return PreparedCommand{}, err
	}
	if _, exists := snapshot.token.owner.state.Session(payload.SessionID); exists {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	return snapshot.finishPrepared(actor.principal, command, input.Meta.At, kind)
}

func (snapshot AuthoritySnapshot) validateDeviceForIssue(payload IdentitySessionIssuedPayload) error {
	device, exists := snapshot.token.owner.state.Device(payload.DeviceID)
	if payload.RegisterDevice {
		if exists {
			return errorCode("invalid_command")
		}
		return nil
	}
	if !exists || device.Status != "active" || device.AccountID != payload.AccountID || device.ServerInstanceID != payload.ServerInstanceID ||
		device.CanonicalOrigin != payload.CanonicalOrigin || device.CredentialDigest != payload.DeviceCredentialDigest {
		return errorCode("invalid_command")
	}
	return nil
}

func (snapshot AuthoritySnapshot) PrepareSessionRotation(actor SessionActor, input SessionRotationCommandInput) (PreparedCommand, error) {
	if !snapshot.validSessionActor(actor) || !validCommandMeta(input.Meta) {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	payload := input.Payload
	if payload.Version != "v1" || payload.Status != "active" || payload.AccountID != actor.principal.AccountID || payload.DeviceID != actor.principal.DeviceID ||
		payload.ParentSessionID != actor.principal.SessionID || payload.LineageRootSessionID != actor.session.LineageRootSessionID ||
		(payload.RefreshAttemptID != "" && !validUUID(payload.RefreshAttemptID)) || payload.RecoveryEpoch != snapshot.RecoveryEpoch || !validUUID(payload.SessionID) || payload.SessionID == payload.ParentSessionID ||
		!canonicalHash.MatchString(payload.ProjectionMaterialDigest) || payload.ExpiresAt != actor.session.ExpiresAt || !validFutureTime(payload.ExpiresAt, input.Meta.At) {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	raw, _ := json.Marshal(payload)
	prepared := snapshot.newPrepared(actor.principal, Command{ID: input.Meta.CommandID, Kind: "session_rotated", AffectedAccountIDs: []string{payload.AccountID}, Payload: raw}, input.Meta.At, preparedSessionRotation)
	if replay, err := snapshot.preparedReceipt(prepared); err != nil {
		return PreparedCommand{}, err
	} else if replay {
		return prepared, nil
	}
	if actor.device.Status != "active" || actor.session.Status != "active" {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	if _, exists := snapshot.token.owner.state.Session(payload.SessionID); exists {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	return snapshot.finishPrepared(actor.principal, prepared.command, input.Meta.At, preparedSessionRotation)
}

func (snapshot AuthoritySnapshot) PrepareRefreshReuseRevocation(actor SessionActor, input RefreshReuseCommandInput) (PreparedCommand, error) {
	if !snapshot.validSessionActor(actor) || !validCommandMeta(input.Meta) {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	payload := input.Payload
	if payload.Version != "v1" || payload.Status != "revoked" || payload.AccountID != actor.principal.AccountID || payload.DeviceID != actor.principal.DeviceID ||
		payload.SessionID != actor.principal.SessionID || payload.ParentSessionID != "" || payload.LineageRootSessionID != actor.session.LineageRootSessionID ||
		payload.RecoveryEpoch != snapshot.RecoveryEpoch || payload.ExpiresAt != actor.session.ExpiresAt || !validTime(payload.ExpiresAt) || payload.ProjectionMaterialDigest != "" || payload.RefreshAttemptID != "" {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	raw, _ := json.Marshal(payload)
	prepared := snapshot.newPrepared(actor.principal, Command{ID: input.Meta.CommandID, Kind: "session_revoked", AffectedAccountIDs: []string{payload.AccountID}, Payload: raw}, input.Meta.At, preparedRefreshReuse)
	if replay, err := snapshot.preparedReceipt(prepared); err != nil {
		return PreparedCommand{}, err
	} else if replay {
		return prepared, nil
	}
	if actor.device.Status != "active" || (actor.session.Status != "active" && actor.session.Status != "rotated") {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	return snapshot.finishPrepared(actor.principal, prepared.command, input.Meta.At, preparedRefreshReuse)
}

func (snapshot AuthoritySnapshot) PrepareDeviceRevocation(actor SessionActor, input DeviceRevocationCommandInput) (PreparedCommand, error) {
	if !snapshot.validSessionActor(actor) || !validCommandMeta(input.Meta) {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	payload := input.Payload
	if payload.Version != "v1" || payload.AccountID != actor.principal.AccountID || payload.DeviceID != actor.principal.DeviceID ||
		payload.ServerInstanceID != actor.device.ServerInstanceID || payload.CanonicalOrigin != actor.device.CanonicalOrigin ||
		payload.DeviceCredentialDigest != actor.device.CredentialDigest || !validFutureTime(actor.session.ExpiresAt, input.Meta.At) {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	raw, _ := json.Marshal(payload)
	prepared := snapshot.newPrepared(actor.principal, Command{ID: input.Meta.CommandID, Kind: "device_binding_revoked", AffectedAccountIDs: []string{payload.AccountID}, Payload: raw}, input.Meta.At, preparedDeviceRevocation)
	if replay, err := snapshot.preparedReceipt(prepared); err != nil {
		return PreparedCommand{}, err
	} else if replay {
		return prepared, nil
	}
	if actor.device.Status != "active" || actor.session.Status != "active" {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	return snapshot.finishPrepared(actor.principal, prepared.command, input.Meta.At, preparedDeviceRevocation)
}

func (snapshot AuthoritySnapshot) PrepareIdentityUnavailable(actor IdentityActor, input IdentityUnavailableCommandInput) (PreparedCommand, error) {
	if !snapshot.validIdentityActor(actor) || !validCommandMeta(input.Meta) {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	payload := input.Payload
	if payload.Version != "v1" || payload.AccountID != actor.resolution.AccountID || payload.IdentityKey != actor.resolution.IdentityKey ||
		(payload.NotificationDigest != "" && !canonicalHash.MatchString(payload.NotificationDigest)) {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	raw, _ := json.Marshal(payload)
	prepared := snapshot.newPrepared(actor.principal, Command{ID: input.Meta.CommandID, Kind: "identity_unavailable", AffectedAccountIDs: []string{payload.AccountID}, Payload: raw}, input.Meta.At, preparedIdentityUnavailable)
	if replay, err := snapshot.preparedReceipt(prepared); err != nil {
		return PreparedCommand{}, err
	} else if replay {
		return prepared, nil
	}
	if actor.resolution.Status != "active" {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	return snapshot.finishPrepared(actor.principal, prepared.command, input.Meta.At, preparedIdentityUnavailable)
}

func (snapshot AuthoritySnapshot) CommandHash(prepared PreparedCommand) ([32]byte, error) {
	if !snapshot.validPrepared(prepared) {
		return [32]byte{}, errorCode("invalid_command")
	}
	return CanonicalCommandHash(prepared.principal, prepared.command)
}

func (snapshot AuthoritySnapshot) finishPrepared(principal access.Principal, command Command, at time.Time, kind preparedKind) (PreparedCommand, error) {
	prepared := snapshot.newPrepared(principal, command, at, kind)
	if !snapshot.validPrepared(prepared) {
		return PreparedCommand{}, errorCode("invalid_command")
	}
	if replay, err := snapshot.preparedReceipt(prepared); err != nil {
		return PreparedCommand{}, err
	} else if replay {
		return prepared, nil
	}
	if _, err := preparedEffects(snapshot.token.owner.state, prepared); err != nil {
		return PreparedCommand{}, err
	}
	return prepared, nil
}

func (snapshot AuthoritySnapshot) newPrepared(principal access.Principal, command Command, at time.Time, kind preparedKind) PreparedCommand {
	return PreparedCommand{token: snapshot.token, kind: kind, principal: principal, command: command, at: at,
		provenance: &preparedProvenance{token: snapshot.token, kind: kind, commandID: command.ID}}
}

func (snapshot AuthoritySnapshot) preparedReceipt(prepared PreparedCommand) (bool, error) {
	if !snapshot.validPrepared(prepared) {
		return false, errorCode("invalid_command")
	}
	hash, err := CanonicalCommandHash(prepared.principal, prepared.command)
	if err != nil {
		return false, err
	}
	receipt, exists := snapshot.token.owner.state.commandReceipts[prepared.principal.AccountID+"/"+prepared.command.ID]
	if !exists {
		return false, nil
	}
	if receipt.CommandHash != hash {
		return false, errorCode("command_id_reused")
	}
	if receipt.preparedKind != prepared.kind {
		return false, errorCode("invalid_command")
	}
	return true, nil
}

func (snapshot AuthoritySnapshot) validIdentityActor(actor IdentityActor) bool {
	return snapshot.valid() && actor.token == snapshot.token && validUUID(actor.principal.AccountID) && actor.principal.DeviceID == "" && actor.principal.SessionID == "" &&
		actor.principal.RecoveryEpoch == snapshot.RecoveryEpoch && actor.principal.SecurityRevision == snapshot.Head.Revision
}

func (snapshot AuthoritySnapshot) validSessionActor(actor SessionActor) bool {
	return snapshot.valid() && actor.token == snapshot.token && validPrincipal(actor.principal) && actor.principal.DeviceID != "" && actor.principal.SessionID != "" &&
		actor.principal.RecoveryEpoch == snapshot.RecoveryEpoch
}

func (snapshot AuthoritySnapshot) validPrepared(prepared PreparedCommand) bool {
	return snapshot.valid() && prepared.token == snapshot.token && prepared.kind >= preparedIdentityIssued && prepared.kind <= preparedOperatorEntitlementSuspend &&
		prepared.provenance != nil && prepared.provenance.token == snapshot.token && prepared.provenance.kind == prepared.kind && prepared.provenance.commandID == prepared.command.ID &&
		preparedKindMatchesCommand(prepared.kind, prepared.command.Kind) && validCommandMeta(CommandMeta{CommandID: prepared.command.ID, At: prepared.at}) &&
		validPrincipal(prepared.principal) && prepared.principal.RecoveryEpoch == snapshot.RecoveryEpoch
}

func (snapshot AuthoritySnapshot) valid() bool {
	token := snapshot.token
	return token != nil && token.id != 0 && token.owner != nil && token.lease != nil && token.lease.active.Load() &&
		snapshot.Head == token.stateHead && snapshot.Head == token.exportedHead &&
		snapshot.RecoveryEpoch == token.recoveryEpoch && snapshot.Head == token.owner.head && snapshot.RecoveryEpoch != 0 && snapshot.RecoveryEpoch <= maxWireInteger &&
		snapshot.View.owner == token.owner && snapshot.View.lease == token.lease && snapshot.View.snapshotToken == token
}

func newLeasedAuthoritySnapshot(owner *validatedAuthority, lease *fenceLease, id uint64) AuthoritySnapshot {
	token := &snapshotCapability{id: id, stateHead: owner.head, exportedHead: owner.head, recoveryEpoch: owner.binding.Epoch, owner: owner, lease: lease}
	return AuthoritySnapshot{Head: owner.head, RecoveryEpoch: owner.binding.Epoch, token: token, View: AuthorityView{owner: owner, lease: lease, snapshotToken: token}}
}

func preparedKindMatchesCommand(kind preparedKind, eventKind EventKind) bool {
	switch kind {
	case preparedIdentityIssued, preparedIdentityRestored:
		return eventKind == "identity_session_issued"
	case preparedSessionRotation:
		return eventKind == "session_rotated"
	case preparedRefreshReuse:
		return eventKind == "session_revoked"
	case preparedDeviceRevocation:
		return eventKind == "device_binding_revoked"
	case preparedIdentityUnavailable:
		return eventKind == "identity_unavailable"
	case preparedOperatorRecovery:
		return eventKind == "owner_recovered"
	case preparedOperatorEntitlementGrant:
		return eventKind == "operator_entitlement_granted"
	case preparedOperatorEntitlementSuspend:
		return eventKind == "operator_entitlement_suspended"
	default:
		return false
	}
}

func validCommandMeta(meta CommandMeta) bool {
	return validUUID(meta.CommandID) && !meta.At.IsZero() && meta.At.Location() == time.UTC && meta.At.Year() >= 1 && meta.At.Year() <= 9999
}

func validFutureTime(value string, at time.Time) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && parsed.Location() == time.UTC && parsed.Format(time.RFC3339Nano) == value && parsed.After(at)
}

func preparedEffects(state State, prepared PreparedCommand) ([]Effect, error) {
	var effects []Effect
	var err error
	switch prepared.kind {
	case preparedIdentityIssued, preparedIdentityRestored:
		var payload IdentitySessionIssuedPayload
		if err := decodeStrict(prepared.command.Payload, &payload, maxEventBytes); err != nil {
			return nil, errorCode("invalid_command")
		}
		account, accountExists := state.Account(payload.AccountID)
		if prepared.kind == preparedIdentityIssued && payload.RegisterAccount {
			if accountExists {
				return nil, errorCode("invalid_command")
			}
			effects = append(effects, Effect{Kind: "account_registered", AccountID: payload.AccountID, IdentityKey: payload.IdentityKey, Status: "active"})
		} else {
			wantStatus := "active"
			if prepared.kind == preparedIdentityRestored {
				wantStatus = "identity_unavailable"
			}
			if !accountExists || account.IdentityKey != payload.IdentityKey || account.Status != wantStatus {
				return nil, errorCode("invalid_command")
			}
		}
		if payload.RegisterDevice {
			if _, exists := state.Device(payload.DeviceID); exists {
				return nil, errorCode("invalid_command")
			}
			effects = append(effects, Effect{Kind: "device_registered", AccountID: payload.AccountID, DeviceID: payload.DeviceID, ServerInstanceID: payload.ServerInstanceID, CanonicalOrigin: payload.CanonicalOrigin, DeviceCredentialDigest: payload.DeviceCredentialDigest, Status: "active"})
		}
		if _, exists := state.Session(payload.SessionID); exists {
			return nil, errorCode("invalid_command")
		}
		effects = append(effects, Effect{Kind: "session_granted", AccountID: payload.AccountID, IdentityKey: payload.IdentityKey, DeviceID: payload.DeviceID, SessionID: payload.SessionID, LineageRootSessionID: payload.LineageRootSessionID, ServerInstanceID: payload.ServerInstanceID, CanonicalOrigin: payload.CanonicalOrigin, DeviceCredentialDigest: payload.DeviceCredentialDigest, ProjectionMaterialDigest: payload.ProjectionMaterialDigest, RecoveryEpoch: payload.RecoveryEpoch, ExpiresAt: payload.ExpiresAt, Status: "active"})
	case preparedSessionRotation:
		var payload SessionPayload
		if err := decodeStrict(prepared.command.Payload, &payload, maxEventBytes); err != nil {
			return nil, errorCode("invalid_command")
		}
		effects = []Effect{{Kind: "session_rotated", AccountID: payload.AccountID, DeviceID: payload.DeviceID, SessionID: payload.SessionID, ParentSessionID: payload.ParentSessionID, LineageRootSessionID: payload.LineageRootSessionID, RefreshAttemptID: payload.RefreshAttemptID, ProjectionMaterialDigest: payload.ProjectionMaterialDigest, RecoveryEpoch: payload.RecoveryEpoch, ExpiresAt: payload.ExpiresAt, Status: "active"}}
	case preparedRefreshReuse:
		var payload SessionPayload
		if err := decodeStrict(prepared.command.Payload, &payload, maxEventBytes); err != nil {
			return nil, errorCode("invalid_command")
		}
		effects = []Effect{{Kind: "session_revoked", AccountID: payload.AccountID, DeviceID: payload.DeviceID, SessionID: payload.SessionID, LineageRootSessionID: payload.LineageRootSessionID, RecoveryEpoch: payload.RecoveryEpoch, ExpiresAt: payload.ExpiresAt, Status: "revoked"}}
	case preparedDeviceRevocation:
		var payload DevicePayload
		if err := decodeStrict(prepared.command.Payload, &payload, maxEventBytes); err != nil {
			return nil, errorCode("invalid_command")
		}
		effects = []Effect{{Kind: "device_binding_revoked", AccountID: payload.AccountID, DeviceID: payload.DeviceID, ServerInstanceID: payload.ServerInstanceID, CanonicalOrigin: payload.CanonicalOrigin, DeviceCredentialDigest: payload.DeviceCredentialDigest, Status: "revoked"}}
	case preparedIdentityUnavailable:
		var payload IdentityAuthorityPayload
		if err := decodeStrict(prepared.command.Payload, &payload, maxEventBytes); err != nil {
			return nil, errorCode("invalid_command")
		}
		effects = []Effect{{Kind: "identity_unavailable", AccountID: payload.AccountID, IdentityKey: payload.IdentityKey, Status: "identity_unavailable"}}
	case preparedOperatorRecovery:
		var payload OwnerChangePayload
		if err := decodeStrict(prepared.command.Payload, &payload, maxEventBytes); err != nil {
			return nil, errorCode("invalid_command")
		}
		effects, err = operatorRecoveryEffects(state, payload)
	case preparedOperatorEntitlementGrant, preparedOperatorEntitlementSuspend:
		var payload OperatorEntitlementPayload
		if err := decodeStrict(prepared.command.Payload, &payload, maxEventBytes); err != nil {
			return nil, errorCode("invalid_command")
		}
		effects, err = operatorEntitlementEffects(state, payload, prepared.at)
	default:
		return nil, errorCode("invalid_command")
	}
	if err != nil {
		return nil, err
	}
	sortedEffects(effects)
	return effects, nil
}

func (state State) decidePrepared(prepared PreparedCommand) (Event, CommandResult, error) {
	commandHash, err := CanonicalCommandHash(prepared.principal, prepared.command)
	if err != nil {
		return Event{}, CommandResult{}, err
	}
	if existing, ok := state.commandReceipts[prepared.principal.AccountID+"/"+prepared.command.ID]; ok {
		if existing.CommandHash != commandHash {
			return Event{}, CommandResult{}, errorCode("command_id_reused")
		}
		if existing.preparedKind != prepared.kind {
			return Event{}, CommandResult{}, errorCode("invalid_command")
		}
		return Event{}, existing.Result, nil
	}
	effects, err := preparedEffects(state, prepared)
	if err != nil || !affectedIDsMatch(prepared.command, effects) {
		return Event{}, CommandResult{}, errorCode("invalid_command")
	}
	result := CommandResult{CommandID: prepared.command.ID, Outcome: "committed", Code: "ok", Revision: state.head.Revision + 1}
	event := Event{Version: "v1", Generation: state.head.Generation, Revision: result.Revision, PreviousHash: hex.EncodeToString(state.head.EventHash[:]),
		CommandID: prepared.command.ID, CommandHash: hex.EncodeToString(commandHash[:]), ActorAccountID: prepared.principal.AccountID,
		ActorDeviceID: prepared.principal.DeviceID, Kind: prepared.command.Kind, At: prepared.at.Format(time.RFC3339Nano), Effects: effects}
	return event, result, nil
}
