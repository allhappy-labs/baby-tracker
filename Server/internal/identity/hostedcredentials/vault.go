package hostedcredentials

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sync"
	"time"

	"babytracker/server/internal/identity"
	"babytracker/server/internal/security/hosted"
)

const (
	mainKind           = "credential-main-v1"
	markerKind         = "credential-replay-marker-v1"
	capabilityAAD      = "baby-tracker-credential-capability-aad-v1"
	maxPlaintext       = 64 << 10
	maxCiphertext      = 96 << 10
	maxTokenBytes      = 16 << 10
	rotationTTL        = 10 * time.Minute
	intentTTL          = 5 * time.Second
	reconcileTimeout   = 5 * time.Second
	markerCASAttempts  = 4
	maxPreparedIntents = 256
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type vault struct {
	store    SecretStore
	cipher   hosted.EnvelopeCipher
	keyID    string
	now      func() time.Time
	mu       sync.Mutex
	prepared map[[32]byte]preparedIntent
}

type mainRecord struct {
	version    string
	sequence   uint64
	cipherHash [32]byte
	body       mainBody
}

type mainBody struct {
	Kind                 string                     `json:"kind"`
	Binding              identity.CredentialBinding `json:"binding"`
	Ref                  identity.CredentialRef     `json:"ref"`
	State                stateBody                  `json:"state"`
	Current              []byte                     `json:"current,omitempty"`
	Pending              []byte                     `json:"pending,omitempty"`
	PendingDigest        string                     `json:"pending_digest"`
	CreatedAt            string                     `json:"created_at"`
	LastOperationID      string                     `json:"last_operation_id"`
	LastCommandID        string                     `json:"last_command_id"`
	HistorySequence      uint64                     `json:"history_sequence"`
	AbortPreserveCurrent bool                       `json:"abort_preserve_current"`
	MaintenanceOwner     string                     `json:"maintenance_owner"`
	MaintenanceUntil     string                     `json:"maintenance_until"`
	LastTransition       *transitionBody            `json:"last_transition,omitempty"`
}

type stateBody struct {
	OperationID         string `json:"operation_id"`
	CommandID           string `json:"command_id"`
	Phase               string `json:"phase"`
	ReconcileAfter      string `json:"reconcile_after"`
	ExpectedCommandHash string `json:"expected_command_hash"`
	MarkerDigest        string `json:"marker_digest"`
	MarkerSequence      uint64 `json:"marker_sequence"`
	HasCurrent          bool   `json:"has_current"`
	HasPending          bool   `json:"has_pending"`
}

type transitionBody struct {
	Name  string      `json:"name"`
	Input publicState `json:"input"`
}

type publicState struct {
	Ref                 identity.CredentialRef `json:"ref"`
	Version             string                 `json:"version"`
	OperationID         string                 `json:"operation_id"`
	CommandID           string                 `json:"command_id"`
	Phase               string                 `json:"phase"`
	ReconcileAfter      string                 `json:"reconcile_after"`
	ExpectedCommandHash string                 `json:"expected_command_hash"`
	MarkerDigest        string                 `json:"marker_digest"`
	MarkerSequence      uint64                 `json:"marker_sequence"`
	HasCurrent          bool                   `json:"has_current"`
	HasPending          bool                   `json:"has_pending"`
}

type markerRecord struct {
	version  string
	sequence uint64
	body     markerBody
}

type markerBody struct {
	Kind             string                     `json:"kind"`
	Binding          identity.CredentialBinding `json:"binding"`
	OperationID      string                     `json:"operation_id"`
	CommandID        string                     `json:"command_id"`
	EnvelopeSequence uint64                     `json:"envelope_sequence"`
}

type capabilityBody struct {
	Kind                string                     `json:"kind"`
	Binding             identity.CredentialBinding `json:"binding"`
	Expected            publicState                `json:"expected"`
	Ref                 identity.CredentialRef     `json:"ref"`
	Version             string                     `json:"version"`
	OperationID         string                     `json:"operation_id"`
	CommandID           string                     `json:"command_id"`
	ExpectedCommandHash string                     `json:"expected_command_hash"`
	MarkerSequence      uint64                     `json:"marker_sequence"`
	MarkerDigest        string                     `json:"marker_digest"`
	LastOperationID     string                     `json:"last_operation_id"`
	LastCommandID       string                     `json:"last_command_id"`
	ExpiresAt           string                     `json:"expires_at"`
	TargetSequence      uint64                     `json:"target_sequence"`
	TargetCipherHash    string                     `json:"target_cipher_hash"`
}

type preparedIntent struct {
	kind       string
	opaque     []byte
	expiresAt  time.Time
	expected   string
	target     SecretObject
	capability []byte
}

func newCredentialAdapters(store SecretStore, cipher hosted.EnvelopeCipher, keyID string, now func() time.Time) (CredentialAdapters, error) {
	if store == nil || cipher == nil || !credentialReference.MatchString(keyID) || now == nil {
		return CredentialAdapters{}, errors.New("invalid hosted credential configuration")
	}
	adapter := &vault{store: store, cipher: cipher, keyID: keyID, now: now, prepared: make(map[[32]byte]preparedIntent)}
	return CredentialAdapters{Vault: adapter, Inventory: adapter, ReplayMarkers: adapter}, nil
}

func (vault *vault) Resolve(ctx context.Context, binding identity.CredentialBinding) (identity.CredentialRotationState, bool, error) {
	record, exists, err := vault.loadMain(ctx, binding)
	if err != nil {
		return identity.CredentialRotationState{}, false, err
	}
	if !exists {
		return identity.CredentialRotationState{Phase: "absent"}, false, nil
	}
	return record.state(), true, nil
}

func (vault *vault) ReserveRotation(ctx context.Context, binding identity.CredentialBinding, operationID, commandID string) (identity.CredentialRotationState, error) {
	if !validBinding(binding) || !validUUID(operationID) || !validUUID(commandID) {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	marker, markerExists, err := vault.loadMarker(ctx, binding)
	if err != nil {
		return identity.CredentialRotationState{}, err
	}
	if markerExists && (marker.body.OperationID == operationID || marker.body.CommandID == commandID) {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	record, exists, err := vault.loadMain(ctx, binding)
	if err != nil {
		return identity.CredentialRotationState{}, err
	}
	if !exists {
		sequence := uint64(1)
		lastOperationID, lastCommandID := "", ""
		historySequence := uint64(0)
		if markerExists {
			if marker.sequence >= maxJSONSafeInteger {
				return identity.CredentialRotationState{}, identity.ErrCredentialConflict
			}
			sequence = marker.sequence + 1
			lastOperationID, lastCommandID = marker.body.OperationID, marker.body.CommandID
			historySequence = marker.sequence
		}
		now := vault.now().UTC()
		desired := mainRecord{sequence: sequence, body: mainBody{
			Kind: mainKind, Binding: binding, Ref: identity.CredentialRef{ID: binding.ObjectID, KeyID: vault.keyID},
			State:     stateBody{OperationID: operationID, CommandID: commandID, Phase: "reserved", ReconcileAfter: canonicalTime(now.Add(rotationTTL))},
			CreatedAt: canonicalTime(now), LastOperationID: lastOperationID, LastCommandID: lastCommandID, HistorySequence: historySequence,
		}}
		created, err := vault.createMain(ctx, desired)
		if err != nil {
			return identity.CredentialRotationState{}, err
		}
		currentMarker, currentMarkerExists, markerErr := vault.loadMarker(ctx, binding)
		if markerErr == nil && sameMarkerSnapshot(marker, markerExists, currentMarker, currentMarkerExists) {
			return created, nil
		}
		rollbackErr := vault.rollbackUnacceptedMain(ctx, binding, created.Version)
		if rollbackErr != nil {
			return identity.CredentialRotationState{}, rollbackErr
		}
		if markerErr != nil {
			return identity.CredentialRotationState{}, markerErr
		}
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	state := record.state()
	freshMarker, freshMarkerExists, err := vault.loadMarker(ctx, binding)
	if err != nil {
		return identity.CredentialRotationState{}, err
	}
	if !mainMatchesMarkerSnapshot(record, freshMarker, freshMarkerExists) {
		if state.Phase == "reserved" && !state.HasCurrent && !state.HasPending && freshMarkerExists && record.body.HistorySequence < freshMarker.sequence {
			if rollbackErr := vault.rollbackUnacceptedMain(ctx, binding, record.version); rollbackErr != nil {
				return identity.CredentialRotationState{}, rollbackErr
			}
		}
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	if freshMarkerExists && (freshMarker.body.OperationID == operationID || freshMarker.body.CommandID == commandID) {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	if operationID == record.body.LastOperationID || commandID == record.body.LastCommandID {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	if state.OperationID == operationID && state.CommandID == commandID && state.Phase != "deleting" && state.Phase != "aborting" {
		return state, nil
	}
	if state.Phase != "ready" {
		return identity.CredentialRotationState{}, identity.ErrCredentialRotationBusy
	}
	return vault.transition(ctx, record, state, "reserve_rotation", nil, func(body *mainBody) error {
		body.State = stateBody{OperationID: operationID, CommandID: commandID, Phase: "reserved", ReconcileAfter: canonicalTime(vault.now().UTC().Add(rotationTTL)), HasCurrent: true}
		return nil
	})
}

func (vault *vault) StoreCandidate(ctx context.Context, binding identity.CredentialBinding, expected identity.CredentialRotationState, token []byte) (identity.CredentialRotationState, error) {
	if len(token) == 0 || len(token) > maxTokenBytes {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	digest := sha256.Sum256(token)
	return vault.transitionBinding(ctx, binding, expected, "store_candidate", func(record mainRecord) bool {
		return record.body.PendingDigest == hex.EncodeToString(digest[:])
	}, func(body *mainBody) error {
		if body.State.Phase != "reserved" || len(body.Pending) != 0 {
			return identity.ErrCredentialConflict
		}
		body.Pending = append([]byte(nil), token...)
		body.PendingDigest = hex.EncodeToString(digest[:])
		body.State.Phase, body.State.HasPending = "candidate", true
		return nil
	})
}

func (vault *vault) OpenPrior(ctx context.Context, binding identity.CredentialBinding, expected identity.CredentialRotationState) ([]byte, error) {
	record, err := vault.requireExpected(ctx, binding, expected)
	if err != nil {
		return nil, err
	}
	if record.body.State.Phase != "candidate" || len(record.body.Current) == 0 {
		return nil, identity.ErrCredentialConflict
	}
	return append([]byte(nil), record.body.Current...), nil
}

func (vault *vault) MarkPriorRevoked(ctx context.Context, binding identity.CredentialBinding, expected identity.CredentialRotationState) (identity.CredentialRotationState, error) {
	return vault.transitionBinding(ctx, binding, expected, "mark_prior_revoked", nil, func(body *mainBody) error {
		if body.State.Phase != "candidate" || len(body.Current) == 0 || len(body.Pending) == 0 {
			return identity.ErrCredentialConflict
		}
		body.Current = nil
		body.State.HasCurrent, body.State.Phase = false, "retiring"
		return nil
	})
}

func (vault *vault) PromotePending(ctx context.Context, binding identity.CredentialBinding, expected identity.CredentialRotationState) (identity.CredentialRotationState, error) {
	return vault.transitionBinding(ctx, binding, expected, "promote_pending", nil, func(body *mainBody) error {
		if ((body.State.Phase != "candidate" || body.State.HasCurrent) && body.State.Phase != "retiring") || len(body.Pending) == 0 {
			return identity.ErrCredentialConflict
		}
		body.Current, body.Pending, body.PendingDigest = append([]byte(nil), body.Pending...), nil, ""
		body.State.HasCurrent, body.State.HasPending, body.State.Phase = true, false, "promoted"
		return nil
	})
}

func (vault *vault) PreparePromotionCAS(ctx context.Context, binding identity.CredentialBinding, expected identity.CredentialRotationState, hash [32]byte) (identity.PromotionCASIntent, error) {
	record, err := vault.requireExpected(ctx, binding, expected)
	if err != nil {
		return identity.PromotionCASIntent{}, err
	}
	if record.body.State.Phase != "promoted" || hash == ([32]byte{}) {
		return identity.PromotionCASIntent{}, identity.ErrCredentialConflict
	}
	expiresAt := vault.now().UTC().Add(intentTTL)
	target, targetState, err := vault.prepareMainTarget(ctx, record, expected, "confirm_promotion", func(body *mainBody) error {
		body.State.Phase, body.State.ExpectedCommandHash, body.State.ReconcileAfter = "adopting", hex.EncodeToString(hash[:]), canonicalTime(expiresAt)
		return nil
	})
	if err != nil {
		return identity.PromotionCASIntent{}, err
	}
	capability, err := vault.sealTargetCapability(ctx, "adoption-capability-v1", binding, targetState, target, "", "")
	if err != nil {
		return identity.PromotionCASIntent{}, err
	}
	opaque, err := vault.sealCapability(ctx, capabilityBody{Kind: "promotion-intent-v1", Binding: binding, Expected: toPublicState(expected), Ref: expected.Ref, Version: expected.Version,
		OperationID: expected.OperationID, CommandID: expected.CommandID, ExpectedCommandHash: hex.EncodeToString(hash[:]), ExpiresAt: canonicalTime(expiresAt)})
	if err != nil {
		return identity.PromotionCASIntent{}, err
	}
	if err := vault.cachePrepared(preparedIntent{kind: "promotion-intent-v1", opaque: opaque, expiresAt: expiresAt, expected: record.version, target: target, capability: capability}); err != nil {
		return identity.PromotionCASIntent{}, err
	}
	return identity.NewPromotionCASIntent(opaque, expiresAt, vault.now().UTC())
}

func (vault *vault) ConfirmPromotion(ctx context.Context, intent identity.PromotionCASIntent) (identity.AdoptionCapability, error) {
	prepared, err := vault.consumePrepared(intent.Opaque, "promotion-intent-v1", intent.ExpiresAt())
	if err != nil {
		return identity.AdoptionCapability{}, err
	}
	swapped, err := vault.store.CompareAndSwap(ctx, prepared.target.ID, prepared.expected, prepared.target)
	if err != nil {
		return identity.AdoptionCapability{}, providerError(ctx)
	}
	if !swapped {
		return identity.AdoptionCapability{}, identity.ErrCredentialConflict
	}
	return identity.AdoptionCapability{Opaque: append([]byte(nil), prepared.capability...)}, nil
}

func (vault *vault) RecoverAdoptionCapability(ctx context.Context, recovery identity.AdoptionRecovery) (identity.AdoptionCapability, error) {
	record, exists, err := vault.loadMain(ctx, recovery.Binding)
	if err != nil {
		return identity.AdoptionCapability{}, err
	}
	if !exists {
		return identity.AdoptionCapability{}, identity.ErrCredentialConflict
	}
	state := record.state()
	if state.Phase != "adopting" || state.Ref != recovery.Ref || state.OperationID != recovery.OperationID || state.CommandID != recovery.CommandID ||
		recovery.ExpectedCommandHash == ([32]byte{}) || state.ExpectedCommandHash != recovery.ExpectedCommandHash {
		return identity.AdoptionCapability{}, identity.ErrCredentialConflict
	}
	return vault.adoptionCapability(ctx, recovery.Binding, state)
}

func (vault *vault) FinishAdoption(ctx context.Context, binding identity.CredentialBinding, capability identity.AdoptionCapability) error {
	return vault.finishAdoption(ctx, binding, capability, true)
}

func (vault *vault) AbandonAdoption(ctx context.Context, binding identity.CredentialBinding, capability identity.AdoptionCapability) error {
	return vault.finishAdoption(ctx, binding, capability, false)
}

func (vault *vault) finishAdoption(ctx context.Context, binding identity.CredentialBinding, capability identity.AdoptionCapability, finish bool) error {
	payload, err := vault.openCapability(ctx, capability.Opaque, "adoption-capability-v1")
	if err != nil {
		return err
	}
	if payload.Binding != binding {
		return identity.ErrCredentialConflict
	}
	name := "abandon_adoption"
	if finish {
		name = "finish_adoption"
	}
	record, exists, err := vault.loadMain(ctx, binding)
	if err != nil {
		return err
	}
	if !exists {
		return identity.ErrCredentialConflict
	}
	if !capabilityMatchesRecord(payload, record) {
		if record.body.LastOperationID == payload.OperationID && record.body.LastCommandID == payload.CommandID &&
			((finish && record.body.State.Phase == "ready") || (!finish && record.body.State.Phase == "aborting")) {
			return nil
		}
		return identity.ErrCredentialConflict
	}
	expected := record.state()
	_, err = vault.transition(ctx, record, expected, name, nil, func(body *mainBody) error {
		if body.State.Phase != "adopting" || body.State.ExpectedCommandHash != payload.ExpectedCommandHash {
			return identity.ErrCredentialConflict
		}
		body.LastOperationID, body.LastCommandID = body.State.OperationID, body.State.CommandID
		body.State.ExpectedCommandHash = ""
		if finish {
			body.State.Phase, body.State.OperationID, body.State.CommandID, body.State.ReconcileAfter = "ready", "", "", ""
		} else {
			body.State.Phase = "aborting"
			body.AbortPreserveCurrent = false
		}
		return nil
	})
	return err
}

func (vault *vault) MarkCommitted(ctx context.Context, binding identity.CredentialBinding, expected identity.CredentialRotationState) (identity.CredentialRotationState, error) {
	return vault.transitionBinding(ctx, binding, expected, "mark_committed", nil, func(body *mainBody) error {
		if body.State.Phase != "adopting" {
			return identity.ErrCredentialConflict
		}
		body.LastOperationID, body.LastCommandID = body.State.OperationID, body.State.CommandID
		body.State.Phase, body.State.OperationID, body.State.CommandID = "ready", "", ""
		body.State.ExpectedCommandHash, body.State.ReconcileAfter = "", ""
		return nil
	})
}

func (vault *vault) MarkAbandoned(ctx context.Context, binding identity.CredentialBinding, expected identity.CredentialRotationState) (identity.CredentialRotationState, error) {
	return vault.transitionBinding(ctx, binding, expected, "mark_abandoned", nil, func(body *mainBody) error {
		switch body.State.Phase {
		case "reserved":
			body.LastOperationID, body.LastCommandID = body.State.OperationID, body.State.CommandID
			if body.State.HasCurrent {
				body.State.Phase, body.State.OperationID, body.State.CommandID, body.State.ReconcileAfter = "ready", "", "", ""
				return nil
			}
			body.State.Phase, body.AbortPreserveCurrent = "aborting", false
		case "candidate":
			body.LastOperationID, body.LastCommandID = body.State.OperationID, body.State.CommandID
			body.State.Phase, body.AbortPreserveCurrent = "aborting", body.State.HasCurrent
		case "retiring", "promoted", "adopting":
			body.LastOperationID, body.LastCommandID = body.State.OperationID, body.State.CommandID
			body.State.Phase, body.AbortPreserveCurrent = "aborting", false
		default:
			return identity.ErrCredentialConflict
		}
		body.State.ExpectedCommandHash = ""
		return nil
	})
}

func (vault *vault) PrepareDeletionCAS(ctx context.Context, binding identity.CredentialBinding, expected identity.CredentialRotationState, operation identity.DeletionOperation) (identity.DeletionCASIntent, error) {
	record, err := vault.requireExpected(ctx, binding, expected)
	if err != nil {
		return identity.DeletionCASIntent{}, err
	}
	if !validUUID(operation.OperationID) || !validUUID(operation.CommandID) || operation.OperationID == record.body.LastOperationID ||
		operation.CommandID == record.body.LastCommandID || operation.OperationID == expected.OperationID || operation.CommandID == expected.CommandID ||
		expected.Phase == "adopting" || expected.Phase == "deleting" {
		return identity.DeletionCASIntent{}, identity.ErrCredentialConflict
	}
	if expected.MarkerSequence >= maxJSONSafeInteger {
		return identity.DeletionCASIntent{}, identity.ErrCredentialConflict
	}
	sequence := expected.MarkerSequence + 1
	digest := deletionMarker(binding, expected.Ref, operation, sequence, expected.Version)
	expiresAt := vault.now().UTC().Add(intentTTL)
	target, targetState, err := vault.prepareMainTarget(ctx, record, expected, "mark_deleting", func(body *mainBody) error {
		if validUUID(body.State.OperationID) && validUUID(body.State.CommandID) {
			body.LastOperationID, body.LastCommandID = body.State.OperationID, body.State.CommandID
		}
		body.State.Phase, body.State.OperationID, body.State.CommandID = "deleting", operation.OperationID, operation.CommandID
		body.State.ExpectedCommandHash, body.State.ReconcileAfter = "", canonicalTime(expiresAt)
		body.State.MarkerSequence, body.State.MarkerDigest = sequence, hex.EncodeToString(digest[:])
		body.AbortPreserveCurrent = false
		return nil
	})
	if err != nil {
		return identity.DeletionCASIntent{}, err
	}
	lastOperationID, lastCommandID := record.body.LastOperationID, record.body.LastCommandID
	if validUUID(expected.OperationID) && validUUID(expected.CommandID) {
		lastOperationID, lastCommandID = expected.OperationID, expected.CommandID
	}
	capability, err := vault.sealTargetCapability(ctx, "deletion-capability-v1", binding, targetState, target, lastOperationID, lastCommandID)
	if err != nil {
		return identity.DeletionCASIntent{}, err
	}
	opaque, err := vault.sealCapability(ctx, capabilityBody{Kind: "deletion-intent-v1", Binding: binding, Expected: toPublicState(expected), Ref: expected.Ref, Version: expected.Version,
		OperationID: operation.OperationID, CommandID: operation.CommandID, MarkerSequence: sequence, MarkerDigest: hex.EncodeToString(digest[:]), ExpiresAt: canonicalTime(expiresAt)})
	if err != nil {
		return identity.DeletionCASIntent{}, err
	}
	if err := vault.cachePrepared(preparedIntent{kind: "deletion-intent-v1", opaque: opaque, expiresAt: expiresAt, expected: record.version, target: target, capability: capability}); err != nil {
		return identity.DeletionCASIntent{}, err
	}
	return identity.NewDeletionCASIntent(opaque, expiresAt, vault.now().UTC())
}

func (vault *vault) MarkDeleting(ctx context.Context, intent identity.DeletionCASIntent) (identity.DeletionCapability, error) {
	prepared, err := vault.consumePrepared(intent.Opaque, "deletion-intent-v1", intent.ExpiresAt())
	if err != nil {
		return identity.DeletionCapability{}, err
	}
	swapped, err := vault.store.CompareAndSwap(ctx, prepared.target.ID, prepared.expected, prepared.target)
	if err != nil {
		return identity.DeletionCapability{}, providerError(ctx)
	}
	if !swapped {
		return identity.DeletionCapability{}, identity.ErrCredentialConflict
	}
	return identity.DeletionCapability{Opaque: append([]byte(nil), prepared.capability...)}, nil
}

func (vault *vault) RecoverDeletionCapability(ctx context.Context, recovery identity.DeletionRecovery) (identity.DeletionCapability, error) {
	record, exists, err := vault.loadMain(ctx, recovery.Binding)
	if err != nil {
		return identity.DeletionCapability{}, err
	}
	if !exists {
		return identity.DeletionCapability{}, identity.ErrCredentialConflict
	}
	state := record.state()
	if state.Phase != "deleting" || state.Ref != recovery.Ref || state.OperationID != recovery.OperationID || state.CommandID != recovery.CommandID ||
		recovery.MarkerSequence == 0 || state.MarkerSequence != recovery.MarkerSequence || recovery.MarkerDigest == ([32]byte{}) || state.MarkerDigest != recovery.MarkerDigest {
		return identity.DeletionCapability{}, identity.ErrCredentialConflict
	}
	return vault.deletionCapability(ctx, record, state)
}

func (vault *vault) ClaimMaintenance(ctx context.Context, binding identity.CredentialBinding, expected identity.CredentialRotationState, worker string, until time.Time) (identity.CredentialRotationState, error) {
	name := "claim_maintenance:" + worker + ":" + canonicalTime(until)
	return vault.transitionBinding(ctx, binding, expected, name, nil, func(body *mainBody) error {
		if !credentialReference.MatchString(worker) || !validTime(until) || !until.After(vault.now().UTC()) || (body.State.Phase != "aborting" && body.State.Phase != "candidate" && body.State.Phase != "retiring") {
			return identity.ErrCredentialConflict
		}
		maintenanceUntil, _ := parseTime(body.MaintenanceUntil)
		if body.MaintenanceOwner != "" && maintenanceUntil.After(vault.now().UTC()) && body.MaintenanceOwner != worker {
			return identity.ErrCredentialRotationBusy
		}
		body.MaintenanceOwner, body.MaintenanceUntil = worker, canonicalTime(until)
		return nil
	})
}

func (vault *vault) OpenMaintenanceTokens(ctx context.Context, binding identity.CredentialBinding, expected identity.CredentialRotationState) ([][]byte, error) {
	record, err := vault.requireExpected(ctx, binding, expected)
	if err != nil {
		return nil, err
	}
	until, _ := parseTime(record.body.MaintenanceUntil)
	if record.body.MaintenanceOwner == "" || !until.After(vault.now().UTC()) {
		return nil, identity.ErrCredentialConflict
	}
	var tokens [][]byte
	if record.body.State.Phase == "aborting" && record.body.AbortPreserveCurrent {
		if len(record.body.Pending) > 0 {
			tokens = append(tokens, append([]byte(nil), record.body.Pending...))
		}
	} else {
		for _, token := range [][]byte{record.body.Current, record.body.Pending} {
			if len(token) > 0 {
				tokens = append(tokens, append([]byte(nil), token...))
			}
		}
	}
	return tokens, nil
}

func (vault *vault) OpenDeletingTokens(ctx context.Context, capability identity.DeletionCapability) ([][]byte, error) {
	record, _, err := vault.deletionRecord(ctx, capability)
	if err != nil {
		return nil, err
	}
	var result [][]byte
	for _, token := range [][]byte{record.body.Current, record.body.Pending} {
		if len(token) > 0 {
			result = append(result, append([]byte(nil), token...))
		}
	}
	return result, nil
}

func (vault *vault) EraseDeletingTokens(ctx context.Context, capability identity.DeletionCapability) (identity.DeletionCapability, error) {
	record, payload, err := vault.deletionRecord(ctx, capability)
	if err != nil {
		return identity.DeletionCapability{}, err
	}
	expected := record.state()
	if len(record.body.Current) == 0 && len(record.body.Pending) == 0 {
		return vault.deletionCapability(ctx, record, expected)
	}
	result, err := vault.transition(ctx, record, expected, "erase_deleting", nil, func(body *mainBody) error {
		body.Current, body.Pending, body.PendingDigest = nil, nil, ""
		body.State.HasCurrent, body.State.HasPending = false, false
		return nil
	})
	if err != nil {
		return identity.DeletionCapability{}, err
	}
	updated, exists, err := vault.loadMain(ctx, payload.Binding)
	if err != nil {
		return identity.DeletionCapability{}, err
	}
	if !exists {
		return identity.DeletionCapability{}, identity.ErrCredentialConflict
	}
	return vault.deletionCapability(ctx, updated, result)
}

func (vault *vault) CompleteMaintenance(ctx context.Context, binding identity.CredentialBinding, expected identity.CredentialRotationState) (identity.CredentialRotationState, error) {
	record, exists, err := vault.loadMain(ctx, binding)
	if err != nil {
		return identity.CredentialRotationState{}, err
	}
	if !exists {
		marker, markerExists, markerErr := vault.loadMarker(ctx, binding)
		if markerErr == nil && markerExists && marker.body.OperationID == expected.OperationID && marker.body.CommandID == expected.CommandID {
			return identity.CredentialRotationState{Phase: "absent"}, nil
		}
		if markerErr == nil && markerExists && expected.Phase == "aborting" && !expected.HasCurrent && !expected.HasPending && validateState(binding, expected) == nil {
			// An unaccepted reservation may have been conditionally removed after
			// a newer replay marker won the delete/create barrier. Its coordinates
			// must not replace that retained history, but absence is terminal.
			return identity.CredentialRotationState{Phase: "absent"}, nil
		}
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	if record.state() != expected || record.body.State.Phase != "aborting" {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	until, _ := parseTime(record.body.MaintenanceUntil)
	if record.body.MaintenanceOwner == "" || !until.After(vault.now().UTC()) {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	if len(record.body.Current) == 0 && len(record.body.Pending) == 0 {
		marker, markerExists, markerErr := vault.loadMarker(ctx, binding)
		if markerErr != nil {
			return identity.CredentialRotationState{}, markerErr
		}
		if markerExists && marker.sequence > record.body.HistorySequence &&
			(marker.body.OperationID != record.body.LastOperationID || marker.body.CommandID != record.body.LastCommandID) {
			if err := vault.rollbackUnacceptedMain(ctx, binding, record.version); err != nil {
				return identity.CredentialRotationState{}, err
			}
			return identity.CredentialRotationState{Phase: "absent"}, nil
		}
	}
	if record.body.AbortPreserveCurrent {
		return vault.transition(ctx, record, expected, "complete_maintenance", nil, func(body *mainBody) error {
			body.Pending, body.PendingDigest = nil, ""
			body.State.HasPending, body.State.Phase = false, "ready"
			body.State.OperationID, body.State.CommandID, body.State.ExpectedCommandHash, body.State.ReconcileAfter = "", "", "", ""
			body.MaintenanceOwner, body.MaintenanceUntil = "", ""
			body.AbortPreserveCurrent = false
			return nil
		})
	}
	if err := vault.ensureReplayMarker(ctx, record); err != nil {
		return identity.CredentialRotationState{}, err
	}
	if err := vault.deleteMain(ctx, record); err != nil {
		return identity.CredentialRotationState{}, err
	}
	return identity.CredentialRotationState{Phase: "absent"}, nil
}

func (vault *vault) Delete(ctx context.Context, capability identity.DeletionCapability) error {
	record, payload, err := vault.deletionRecord(ctx, capability)
	if err != nil {
		if !errors.Is(err, identity.ErrCredentialConflict) || payload.Binding.ObjectID == "" {
			return err
		}
		_, mainExists, resolveErr := vault.loadMain(ctx, payload.Binding)
		if resolveErr != nil {
			return resolveErr
		}
		if mainExists {
			return identity.ErrCredentialConflict
		}
		marker, exists, markerErr := vault.loadMarker(ctx, payload.Binding)
		if markerErr == nil && exists && marker.body.OperationID == payload.LastOperationID && marker.body.CommandID == payload.LastCommandID {
			return nil
		}
		return err
	}
	if len(record.body.Current) != 0 || len(record.body.Pending) != 0 {
		return identity.ErrCredentialConflict
	}
	if err := vault.ensureReplayMarker(ctx, record); err != nil {
		return err
	}
	return vault.deleteMain(ctx, record)
}

func (vault *vault) ConfirmAbsent(ctx context.Context, binding identity.CredentialBinding, ref identity.CredentialRef) (bool, error) {
	if !validBinding(binding) || ref.ID != binding.ObjectID || !credentialReference.MatchString(ref.KeyID) {
		return false, identity.ErrCredentialConflict
	}
	_, exists, err := vault.loadMain(ctx, binding)
	return !exists, err
}

func (vault *vault) List(ctx context.Context, before time.Time, cursor string, limit int) ([]identity.CredentialMetadata, string, error) {
	if !validTime(before) || limit < 1 || limit > 100 {
		return nil, "", identity.ErrCredentialConflict
	}
	metadata, next, err := vault.store.ListMetadata(ctx, before, cursor, limit)
	if err != nil {
		return nil, "", providerError(ctx)
	}
	result := make([]identity.CredentialMetadata, 0, len(metadata))
	seen := map[string]bool{}
	for _, item := range metadata {
		if seen[item.ObjectID] || item.Version == "" || !credentialReference.MatchString(item.KeyID) || !validTime(item.CreatedAt) {
			return nil, "", identity.ErrCredentialConflict
		}
		seen[item.ObjectID] = true
		object, exists, err := vault.store.Resolve(ctx, item.ObjectID)
		if err != nil {
			return nil, "", providerError(ctx)
		}
		if !exists {
			return nil, "", identity.ErrCredentialConflict
		}
		kind, main, _, err := vault.openAny(ctx, object)
		if err != nil {
			return nil, "", err
		}
		if object.ID != item.ObjectID || object.Version != item.Version || object.KeyID != item.KeyID {
			return nil, "", identity.ErrCredentialConflict
		}
		if kind == markerKind {
			continue
		}
		state := main.state()
		result = append(result, identity.CredentialMetadata{Binding: main.body.Binding, Ref: state.Ref, Phase: state.Phase, CreatedAt: item.CreatedAt})
	}
	return result, next, nil
}

func (vault *vault) PurgeReplayMarker(ctx context.Context, binding identity.CredentialBinding) error {
	marker, exists, err := vault.loadMarker(ctx, binding)
	if err != nil || !exists {
		return err
	}
	id, _ := replayObjectID(binding)
	deleteErr := vault.store.Delete(ctx, id, marker.version)
	if deleteErr == nil {
		return nil
	}
	reconcileCtx, cancel := reconcileContext(ctx)
	defer cancel()
	current, stillExists, resolveErr := vault.loadMarker(reconcileCtx, binding)
	if resolveErr == nil && !stillExists {
		return nil
	}
	if resolveErr == nil && current.version != marker.version {
		return identity.ErrCredentialConflict
	}
	if resolveErr != nil {
		return resolveErr
	}
	return providerError(ctx)
}

func (vault *vault) prepareMainTarget(ctx context.Context, record mainRecord, expected identity.CredentialRotationState, name string, mutate func(*mainBody) error) (SecretObject, identity.CredentialRotationState, error) {
	if record.state() != expected || record.sequence >= maxJSONSafeInteger {
		return SecretObject{}, identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	desired := cloneMainRecord(record)
	if err := mutate(&desired.body); err != nil {
		return SecretObject{}, identity.CredentialRotationState{}, err
	}
	desired.sequence = record.sequence + 1
	desired.body.State.HasCurrent, desired.body.State.HasPending = len(desired.body.Current) > 0, len(desired.body.Pending) > 0
	desired.body.LastTransition = &transitionBody{Name: name, Input: toPublicState(expected)}
	target, err := vault.sealMain(ctx, desired)
	if err != nil {
		return SecretObject{}, identity.CredentialRotationState{}, err
	}
	state, err := desired.body.State.toState(desired.body.Ref, record.version)
	if err != nil || validateState(desired.body.Binding, state) != nil {
		return SecretObject{}, identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	return target, state, nil
}

func (vault *vault) sealTargetCapability(ctx context.Context, kind string, binding identity.CredentialBinding, state identity.CredentialRotationState, target SecretObject, lastOperationID, lastCommandID string) ([]byte, error) {
	payload := capabilityBody{
		Kind: kind, Binding: binding, Expected: toPublicState(state), Ref: state.Ref, Version: state.Version,
		OperationID: state.OperationID, CommandID: state.CommandID,
		ExpectedCommandHash: encodeOptionalDigest(state.ExpectedCommandHash), MarkerSequence: state.MarkerSequence,
		MarkerDigest: encodeOptionalDigest(state.MarkerDigest), LastOperationID: lastOperationID, LastCommandID: lastCommandID,
		TargetSequence: target.Header.EnvelopeSequence, TargetCipherHash: hex.EncodeToString(target.SHA256[:]),
	}
	return vault.sealCapability(ctx, payload)
}

func (vault *vault) cachePrepared(intent preparedIntent) error {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	now := vault.now().UTC()
	for digest, current := range vault.prepared {
		if !now.Before(current.expiresAt) {
			delete(vault.prepared, digest)
		}
	}
	if len(vault.prepared) >= maxPreparedIntents {
		return identity.ErrUnavailable
	}
	digest := sha256.Sum256(intent.opaque)
	intent.opaque = append([]byte(nil), intent.opaque...)
	intent.target = cloneProviderObject(intent.target)
	intent.capability = append([]byte(nil), intent.capability...)
	vault.prepared[digest] = intent
	return nil
}

func (vault *vault) consumePrepared(opaque []byte, kind string, expiresAt time.Time) (preparedIntent, error) {
	digest := sha256.Sum256(opaque)
	vault.mu.Lock()
	defer vault.mu.Unlock()
	prepared, exists := vault.prepared[digest]
	if !exists || prepared.kind != kind || !bytes.Equal(prepared.opaque, opaque) || prepared.expiresAt != expiresAt ||
		!vault.now().UTC().Before(prepared.expiresAt) {
		return preparedIntent{}, identity.ErrCredentialConflict
	}
	delete(vault.prepared, digest)
	prepared.target = cloneProviderObject(prepared.target)
	prepared.capability = append([]byte(nil), prepared.capability...)
	return prepared, nil
}

func (vault *vault) transitionBinding(ctx context.Context, binding identity.CredentialBinding, expected identity.CredentialRotationState, name string, replayExtra func(mainRecord) bool, mutate func(*mainBody) error) (identity.CredentialRotationState, error) {
	record, exists, err := vault.loadMain(ctx, binding)
	if err != nil || !exists {
		if err != nil {
			return identity.CredentialRotationState{}, err
		}
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	return vault.transition(ctx, record, expected, name, replayExtra, mutate)
}

func (vault *vault) transition(ctx context.Context, record mainRecord, expected identity.CredentialRotationState, name string, replayExtra func(mainRecord) bool, mutate func(*mainBody) error) (identity.CredentialRotationState, error) {
	if record.state() != expected {
		if record.body.LastTransition != nil && record.body.LastTransition.Name == name && record.body.LastTransition.Input == toPublicState(expected) && (replayExtra == nil || replayExtra(record)) {
			return record.state(), nil
		}
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	desired := cloneMainRecord(record)
	if err := mutate(&desired.body); err != nil {
		return identity.CredentialRotationState{}, err
	}
	desired.body.State.HasCurrent, desired.body.State.HasPending = len(desired.body.Current) > 0, len(desired.body.Pending) > 0
	desired.body.LastTransition = &transitionBody{Name: name, Input: toPublicState(expected)}
	return vault.casMain(ctx, record, desired)
}

func (vault *vault) requireExpected(ctx context.Context, binding identity.CredentialBinding, expected identity.CredentialRotationState) (mainRecord, error) {
	record, exists, err := vault.loadMain(ctx, binding)
	if err != nil || !exists || record.state() != expected {
		if err != nil {
			return mainRecord{}, err
		}
		return mainRecord{}, identity.ErrCredentialConflict
	}
	return record, nil
}

func (vault *vault) createMain(ctx context.Context, desired mainRecord) (identity.CredentialRotationState, error) {
	object, err := vault.sealMain(ctx, desired)
	if err != nil {
		return identity.CredentialRotationState{}, err
	}
	createErr := vault.store.Create(ctx, object)
	reconcileCtx, cancel := reconcileContext(ctx)
	defer cancel()
	current, exists, loadErr := vault.loadMain(reconcileCtx, desired.body.Binding)
	if loadErr == nil && exists && mainBodiesEqual(current.body, desired.body) {
		return current.state(), nil
	}
	if loadErr != nil {
		return identity.CredentialRotationState{}, loadErr
	}
	if createErr != nil {
		return identity.CredentialRotationState{}, providerError(ctx)
	}
	return identity.CredentialRotationState{}, identity.ErrCredentialConflict
}

func (vault *vault) casMain(ctx context.Context, current, desired mainRecord) (identity.CredentialRotationState, error) {
	if current.sequence >= maxJSONSafeInteger {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	desired.sequence = current.sequence + 1
	object, err := vault.sealMain(ctx, desired)
	if err != nil {
		return identity.CredentialRotationState{}, err
	}
	swapped, casErr := vault.store.CompareAndSwap(ctx, current.body.Binding.ObjectID, current.version, object)
	if !swapped && casErr == nil {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	reconcileCtx, cancel := reconcileContext(ctx)
	defer cancel()
	resolved, exists, loadErr := vault.loadMain(reconcileCtx, desired.body.Binding)
	if loadErr == nil && exists && mainBodiesEqual(resolved.body, desired.body) {
		return resolved.state(), nil
	}
	if loadErr != nil {
		return identity.CredentialRotationState{}, loadErr
	}
	if casErr != nil {
		return identity.CredentialRotationState{}, providerError(ctx)
	}
	return identity.CredentialRotationState{}, identity.ErrCredentialConflict
}

func (vault *vault) loadMain(ctx context.Context, binding identity.CredentialBinding) (mainRecord, bool, error) {
	if !validBinding(binding) {
		return mainRecord{}, false, identity.ErrCredentialConflict
	}
	object, exists, err := vault.store.Resolve(ctx, binding.ObjectID)
	if err != nil {
		return mainRecord{}, false, providerError(ctx)
	}
	if !exists {
		return mainRecord{}, false, nil
	}
	kind, main, _, err := vault.openAny(ctx, object)
	if err != nil {
		return mainRecord{}, false, err
	}
	if kind != mainKind || main.body.Binding != binding {
		return mainRecord{}, false, identity.ErrCredentialConflict
	}
	return main, true, nil
}

func (vault *vault) loadMarker(ctx context.Context, binding identity.CredentialBinding) (markerRecord, bool, error) {
	if !validBinding(binding) {
		return markerRecord{}, false, identity.ErrCredentialConflict
	}
	id, _ := replayObjectID(binding)
	object, exists, err := vault.store.Resolve(ctx, id)
	if err != nil {
		return markerRecord{}, false, providerError(ctx)
	}
	if !exists {
		return markerRecord{}, false, nil
	}
	kind, _, marker, err := vault.openAny(ctx, object)
	if err != nil {
		return markerRecord{}, false, err
	}
	if kind != markerKind || marker.body.Binding != binding {
		return markerRecord{}, false, identity.ErrCredentialConflict
	}
	return marker, true, nil
}

func (vault *vault) sealMain(ctx context.Context, record mainRecord) (SecretObject, error) {
	return vault.sealObject(ctx, record.body.Binding.ObjectID, record.sequence, record.body)
}

func (vault *vault) sealObject(ctx context.Context, objectID string, sequence uint64, payload any) (SecretObject, error) {
	header := CredentialEnvelopeHeader{Schema: credentialEnvelopeSchema, EnvelopeSequence: sequence}
	aad, err := credentialAAD(objectID, vault.keyID, header)
	if err != nil {
		return SecretObject{}, err
	}
	plaintext, err := json.Marshal(payload)
	if err != nil || len(plaintext) == 0 || len(plaintext) > maxPlaintext {
		return SecretObject{}, identity.ErrCredentialConflict
	}
	defer zero(plaintext)
	sealed, err := vault.cipher.Seal(ctx, vault.keyID, plaintext, aad)
	ciphertext := append([]byte(nil), sealed...)
	if err != nil || len(ciphertext) == 0 || len(ciphertext) > maxCiphertext {
		return SecretObject{}, providerError(ctx)
	}
	return SecretObject{ID: objectID, KeyID: vault.keyID, Header: header, Ciphertext: ciphertext, SHA256: sha256.Sum256(ciphertext)}, nil
}

func (vault *vault) openAny(ctx context.Context, object SecretObject) (string, mainRecord, markerRecord, error) {
	if !credentialReference.MatchString(object.ID) || object.Version == "" || !credentialReference.MatchString(object.KeyID) || object.Header.Schema != credentialEnvelopeSchema ||
		object.Header.EnvelopeSequence == 0 || object.Header.EnvelopeSequence > maxJSONSafeInteger || len(object.Ciphertext) == 0 ||
		len(object.Ciphertext) > maxCiphertext || sha256.Sum256(object.Ciphertext) != object.SHA256 {
		return "", mainRecord{}, markerRecord{}, identity.ErrCredentialConflict
	}
	aad, err := credentialAAD(object.ID, object.KeyID, object.Header)
	if err != nil {
		return "", mainRecord{}, markerRecord{}, err
	}
	plaintext, err := vault.cipher.Open(ctx, object.KeyID, object.Ciphertext, aad)
	defer zero(plaintext)
	if err != nil || len(plaintext) == 0 || len(plaintext) > maxPlaintext {
		if ctx != nil && ctx.Err() != nil {
			return "", mainRecord{}, markerRecord{}, ctx.Err()
		}
		return "", mainRecord{}, markerRecord{}, identity.ErrCredentialConflict
	}
	var shape struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(plaintext, &shape); err != nil || shape.Kind == "" {
		return "", mainRecord{}, markerRecord{}, identity.ErrCredentialConflict
	}
	switch shape.Kind {
	case mainKind:
		var body mainBody
		if err := strictJSON(plaintext, &body, true); err != nil || validateMainBody(body, object) != nil {
			return "", mainRecord{}, markerRecord{}, identity.ErrCredentialConflict
		}
		return mainKind, mainRecord{version: object.Version, sequence: object.Header.EnvelopeSequence, cipherHash: object.SHA256, body: body}, markerRecord{}, nil
	case markerKind:
		var body markerBody
		if err := strictJSON(plaintext, &body, true); err != nil || validateMarkerBody(body, object) != nil {
			return "", mainRecord{}, markerRecord{}, identity.ErrCredentialConflict
		}
		return markerKind, mainRecord{}, markerRecord{version: object.Version, sequence: object.Header.EnvelopeSequence, body: body}, nil
	default:
		return "", mainRecord{}, markerRecord{}, identity.ErrCredentialConflict
	}
}

func (vault *vault) ensureReplayMarker(ctx context.Context, main mainRecord) error {
	if !validUUID(main.body.LastOperationID) || !validUUID(main.body.LastCommandID) {
		return identity.ErrCredentialConflict
	}
	id, _ := replayObjectID(main.body.Binding)
	for attempt := 0; attempt < markerCASAttempts; attempt++ {
		marker, exists, err := vault.loadMarker(ctx, main.body.Binding)
		if err != nil {
			return err
		}
		if main.sequence >= maxJSONSafeInteger {
			return identity.ErrCredentialConflict
		}
		sequence := main.sequence + 1
		if exists && marker.body.OperationID == main.body.LastOperationID && marker.body.CommandID == main.body.LastCommandID {
			if marker.sequence >= sequence {
				return nil
			}
			if marker.sequence < main.body.HistorySequence {
				return identity.ErrCredentialConflict
			}
		}
		exactHistory := exists && marker.body.OperationID == main.body.LastOperationID && marker.body.CommandID == main.body.LastCommandID
		if exists && !exactHistory && marker.sequence != main.body.HistorySequence {
			// The companion is newer than the history authenticated in this main
			// object. Never roll it back from a stale deletion attempt.
			return identity.ErrCredentialConflict
		}
		desired := markerBody{Kind: markerKind, Binding: main.body.Binding, OperationID: main.body.LastOperationID, CommandID: main.body.LastCommandID, EnvelopeSequence: sequence}
		object, err := vault.sealObject(ctx, id, sequence, desired)
		if err != nil {
			return err
		}
		var swapped = true
		if !exists {
			err = vault.store.Create(ctx, object)
		} else {
			swapped, err = vault.store.CompareAndSwap(ctx, id, marker.version, object)
		}
		reconcileCtx, cancel := reconcileContext(ctx)
		current, present, loadErr := vault.loadMarker(reconcileCtx, main.body.Binding)
		cancel()
		if loadErr == nil && present && current.sequence == desired.EnvelopeSequence &&
			current.body.OperationID == desired.OperationID && current.body.CommandID == desired.CommandID {
			return nil
		}
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if loadErr != nil {
			return loadErr
		}
		if err == nil && swapped {
			return identity.ErrCredentialConflict
		}
		// A conditional conflict, or a provider error whose outcome did not
		// reconcile to this attempt, may have exposed a newer authenticated
		// marker. Retry from that exact provider version and bounded sequence.
	}
	return identity.ErrCredentialConflict
}

func (vault *vault) deleteMain(ctx context.Context, main mainRecord) error {
	err := vault.store.Delete(ctx, main.body.Binding.ObjectID, main.version)
	if err == nil {
		return nil
	}
	reconcileCtx, cancel := reconcileContext(ctx)
	defer cancel()
	current, exists, loadErr := vault.loadMain(reconcileCtx, main.body.Binding)
	if loadErr == nil && !exists {
		return nil
	}
	if loadErr == nil && current.version != main.version {
		return identity.ErrCredentialConflict
	}
	if loadErr != nil {
		return loadErr
	}
	return providerError(ctx)
}

func sameMarkerSnapshot(left markerRecord, leftExists bool, right markerRecord, rightExists bool) bool {
	return leftExists == rightExists && (!leftExists || (left.version == right.version && left.sequence == right.sequence && left.body == right.body))
}

func mainMatchesMarkerSnapshot(main mainRecord, marker markerRecord, markerExists bool) bool {
	if !markerExists {
		return main.body.HistorySequence == 0
	}
	return main.body.HistorySequence == marker.sequence
}

func (vault *vault) rollbackUnacceptedMain(ctx context.Context, binding identity.CredentialBinding, version string) error {
	if version == "" {
		return identity.ErrCredentialConflict
	}
	err := vault.store.Delete(ctx, binding.ObjectID, version)
	if err == nil {
		return nil
	}
	reconcileCtx, cancel := reconcileContext(ctx)
	defer cancel()
	current, exists, loadErr := vault.loadMain(reconcileCtx, binding)
	if loadErr != nil {
		return loadErr
	}
	if !exists {
		return nil
	}
	if current.version != version {
		return identity.ErrCredentialConflict
	}
	return providerError(ctx)
}

func (vault *vault) sealCapability(ctx context.Context, payload capabilityBody) ([]byte, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil || len(plaintext) == 0 || len(plaintext) > maxPlaintext {
		return nil, identity.ErrCredentialConflict
	}
	defer zero(plaintext)
	aad := frame([]byte(capabilityAAD), []byte(vault.keyID), []byte(payload.Kind))
	sealed, err := vault.cipher.Seal(ctx, vault.keyID, plaintext, aad)
	if err != nil || len(sealed) == 0 || len(sealed) > maxPlaintext {
		return nil, providerError(ctx)
	}
	return append([]byte(nil), sealed...), nil
}

func (vault *vault) openCapability(ctx context.Context, opaque []byte, kind string) (capabilityBody, error) {
	if len(opaque) == 0 || len(opaque) > maxPlaintext {
		return capabilityBody{}, identity.ErrCredentialConflict
	}
	plaintext, err := vault.cipher.Open(ctx, vault.keyID, opaque, frame([]byte(capabilityAAD), []byte(vault.keyID), []byte(kind)))
	defer zero(plaintext)
	if err != nil || len(plaintext) == 0 || len(plaintext) > maxPlaintext {
		if ctx != nil && ctx.Err() != nil {
			return capabilityBody{}, ctx.Err()
		}
		return capabilityBody{}, identity.ErrCredentialConflict
	}
	var payload capabilityBody
	if err := strictJSON(plaintext, &payload, true); err != nil || validateCapability(payload, kind) != nil {
		return capabilityBody{}, identity.ErrCredentialConflict
	}
	return payload, nil
}

func (vault *vault) adoptionCapability(ctx context.Context, binding identity.CredentialBinding, state identity.CredentialRotationState) (identity.AdoptionCapability, error) {
	record, exists, err := vault.loadMain(ctx, binding)
	if err != nil || !exists || record.state() != state {
		if err != nil {
			return identity.AdoptionCapability{}, err
		}
		return identity.AdoptionCapability{}, identity.ErrCredentialConflict
	}
	opaque, err := vault.sealTargetCapability(ctx, "adoption-capability-v1", binding, state,
		SecretObject{Header: CredentialEnvelopeHeader{EnvelopeSequence: record.sequence}, SHA256: record.cipherHash}, "", "")
	return identity.AdoptionCapability{Opaque: opaque}, err
}

func (vault *vault) deletionCapability(ctx context.Context, record mainRecord, state identity.CredentialRotationState) (identity.DeletionCapability, error) {
	opaque, err := vault.sealTargetCapability(ctx, "deletion-capability-v1", record.body.Binding, state,
		SecretObject{Header: CredentialEnvelopeHeader{EnvelopeSequence: record.sequence}, SHA256: record.cipherHash}, record.body.LastOperationID, record.body.LastCommandID)
	return identity.DeletionCapability{Opaque: opaque}, err
}

func (vault *vault) deletionRecord(ctx context.Context, capability identity.DeletionCapability) (mainRecord, capabilityBody, error) {
	payload, err := vault.openCapability(ctx, capability.Opaque, "deletion-capability-v1")
	if err != nil {
		return mainRecord{}, payload, err
	}
	record, exists, err := vault.loadMain(ctx, payload.Binding)
	if err != nil || !exists {
		if err != nil {
			return mainRecord{}, payload, err
		}
		return mainRecord{}, payload, identity.ErrCredentialConflict
	}
	state := record.state()
	markerDigest, ok := decodeDigest(payload.MarkerDigest)
	if !ok || !capabilityMatchesRecord(payload, record) || state.Phase != "deleting" || state.Ref != payload.Ref || state.OperationID != payload.OperationID || state.CommandID != payload.CommandID || state.MarkerSequence != payload.MarkerSequence || state.MarkerDigest != markerDigest {
		return mainRecord{}, payload, identity.ErrCredentialConflict
	}
	return record, payload, nil
}

func capabilityMatchesRecord(payload capabilityBody, record mainRecord) bool {
	targetHash, ok := decodeDigest(payload.TargetCipherHash)
	if !ok || payload.TargetSequence == 0 || record.sequence != payload.TargetSequence || record.cipherHash != targetHash {
		return false
	}
	priorVersion := ""
	if record.body.LastTransition != nil {
		priorVersion = record.body.LastTransition.Input.Version
	}
	if payload.Version != record.version && payload.Version != priorVersion {
		return false
	}
	expected, err := payload.state()
	if err != nil {
		return false
	}
	actual := record.state()
	actual.Version = expected.Version
	return actual == expected
}

func (record mainRecord) state() identity.CredentialRotationState {
	state, _ := record.body.State.toState(record.body.Ref, record.version)
	return state
}

func (state stateBody) toState(ref identity.CredentialRef, version string) (identity.CredentialRotationState, error) {
	reconcileAfter, err := parseOptionalTime(state.ReconcileAfter)
	if err != nil {
		return identity.CredentialRotationState{}, err
	}
	expectedHash, ok := decodeOptionalDigest(state.ExpectedCommandHash)
	if !ok {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	markerDigest, ok := decodeOptionalDigest(state.MarkerDigest)
	if !ok {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	return identity.CredentialRotationState{Ref: ref, Version: version, OperationID: state.OperationID, CommandID: state.CommandID,
		Phase: state.Phase, ReconcileAfter: reconcileAfter, ExpectedCommandHash: expectedHash, MarkerDigest: markerDigest,
		MarkerSequence: state.MarkerSequence, HasCurrent: state.HasCurrent, HasPending: state.HasPending}, nil
}

func (payload capabilityBody) state() (identity.CredentialRotationState, error) {
	state, err := payload.Expected.toState()
	if err != nil || state.Ref != payload.Ref || state.Version != payload.Version {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	return state, nil
}

func toPublicState(state identity.CredentialRotationState) publicState {
	return publicState{Ref: state.Ref, Version: state.Version, OperationID: state.OperationID, CommandID: state.CommandID, Phase: state.Phase,
		ReconcileAfter: canonicalOptionalTime(state.ReconcileAfter), ExpectedCommandHash: encodeOptionalDigest(state.ExpectedCommandHash),
		MarkerDigest: encodeOptionalDigest(state.MarkerDigest), MarkerSequence: state.MarkerSequence, HasCurrent: state.HasCurrent, HasPending: state.HasPending}
}

func (state publicState) toState() (identity.CredentialRotationState, error) {
	reconcileAfter, err := parseOptionalTime(state.ReconcileAfter)
	if err != nil {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	expectedHash, ok := decodeOptionalDigest(state.ExpectedCommandHash)
	if !ok {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	markerDigest, ok := decodeOptionalDigest(state.MarkerDigest)
	if !ok {
		return identity.CredentialRotationState{}, identity.ErrCredentialConflict
	}
	return identity.CredentialRotationState{
		Ref: state.Ref, Version: state.Version, OperationID: state.OperationID, CommandID: state.CommandID,
		Phase: state.Phase, ReconcileAfter: reconcileAfter, ExpectedCommandHash: expectedHash,
		MarkerDigest: markerDigest, MarkerSequence: state.MarkerSequence, HasCurrent: state.HasCurrent, HasPending: state.HasPending,
	}, nil
}

func validateCapability(payload capabilityBody, kind string) error {
	if payload.Kind != kind || !validBinding(payload.Binding) || payload.Ref.ID != payload.Binding.ObjectID ||
		!credentialReference.MatchString(payload.Ref.KeyID) || payload.Version == "" || payload.MarkerSequence > maxJSONSafeInteger ||
		payload.TargetSequence > maxJSONSafeInteger {
		return identity.ErrCredentialConflict
	}
	state, err := payload.state()
	if err != nil || validateState(payload.Binding, state) != nil {
		return identity.ErrCredentialConflict
	}
	expiresAt, expiresErr := parseOptionalTime(payload.ExpiresAt)
	expectedHash, expectedHashOK := decodeOptionalDigest(payload.ExpectedCommandHash)
	markerDigest, markerDigestOK := decodeOptionalDigest(payload.MarkerDigest)
	targetHash, targetHashOK := decodeOptionalDigest(payload.TargetCipherHash)
	validCoordinates := validUUID(payload.OperationID) && validUUID(payload.CommandID)
	lastCoordinates := (payload.LastOperationID == "" && payload.LastCommandID == "") ||
		(validUUID(payload.LastOperationID) && validUUID(payload.LastCommandID))
	if expiresErr != nil || !expectedHashOK || !markerDigestOK || !targetHashOK || !lastCoordinates {
		return identity.ErrCredentialConflict
	}
	switch kind {
	case "promotion-intent-v1":
		if state.Phase != "promoted" || !validCoordinates || payload.OperationID != state.OperationID || payload.CommandID != state.CommandID ||
			expectedHash == ([32]byte{}) || markerDigest != ([32]byte{}) || payload.MarkerSequence != 0 ||
			payload.LastOperationID != "" || payload.LastCommandID != "" || !validTime(expiresAt) || payload.TargetSequence != 0 || targetHash != ([32]byte{}) {
			return identity.ErrCredentialConflict
		}
	case "adoption-capability-v1":
		if state.Phase != "adopting" || !validCoordinates || payload.OperationID != state.OperationID || payload.CommandID != state.CommandID ||
			expectedHash == ([32]byte{}) || expectedHash != state.ExpectedCommandHash || markerDigest != ([32]byte{}) || payload.MarkerSequence != 0 ||
			payload.LastOperationID != "" || payload.LastCommandID != "" || !expiresAt.IsZero() || payload.TargetSequence == 0 || targetHash == ([32]byte{}) {
			return identity.ErrCredentialConflict
		}
	case "deletion-intent-v1":
		if state.Phase == "adopting" || state.Phase == "deleting" || !validCoordinates || expectedHash != ([32]byte{}) ||
			markerDigest == ([32]byte{}) || payload.MarkerSequence == 0 || payload.LastOperationID != "" || payload.LastCommandID != "" || !validTime(expiresAt) ||
			payload.TargetSequence != 0 || targetHash != ([32]byte{}) {
			return identity.ErrCredentialConflict
		}
		want := deletionMarker(payload.Binding, payload.Ref, identity.DeletionOperation{OperationID: payload.OperationID, CommandID: payload.CommandID}, payload.MarkerSequence, payload.Version)
		if markerDigest != want {
			return identity.ErrCredentialConflict
		}
	case "deletion-capability-v1":
		if state.Phase != "deleting" || !validCoordinates || payload.OperationID != state.OperationID || payload.CommandID != state.CommandID ||
			expectedHash != ([32]byte{}) || markerDigest == ([32]byte{}) || markerDigest != state.MarkerDigest ||
			payload.MarkerSequence != state.MarkerSequence || !validUUID(payload.LastOperationID) || !validUUID(payload.LastCommandID) || !expiresAt.IsZero() ||
			payload.TargetSequence == 0 || targetHash == ([32]byte{}) {
			return identity.ErrCredentialConflict
		}
	default:
		return identity.ErrCredentialConflict
	}
	return nil
}

func validateMainBody(body mainBody, object SecretObject) error {
	if body.Kind != mainKind || !validBinding(body.Binding) || body.Binding.ObjectID != object.ID || body.Ref.ID != object.ID ||
		!credentialReference.MatchString(body.Ref.KeyID) ||
		body.HistorySequence >= object.Header.EnvelopeSequence || body.HistorySequence > maxJSONSafeInteger || len(body.Current) > maxTokenBytes || len(body.Pending) > maxTokenBytes ||
		body.State.HasCurrent != (len(body.Current) > 0) || body.State.HasPending != (len(body.Pending) > 0) {
		return identity.ErrCredentialConflict
	}
	created, err := parseTime(body.CreatedAt)
	if err != nil || !validTime(created) {
		return identity.ErrCredentialConflict
	}
	if body.PendingDigest != "" {
		digest, ok := decodeDigest(body.PendingDigest)
		if !ok || digest != sha256.Sum256(body.Pending) {
			return identity.ErrCredentialConflict
		}
	} else if len(body.Pending) != 0 {
		return identity.ErrCredentialConflict
	}
	state, err := body.State.toState(body.Ref, object.Version)
	if err != nil || validateState(body.Binding, state) != nil {
		return identity.ErrCredentialConflict
	}
	if (body.LastOperationID == "") != (body.LastCommandID == "") || (body.LastOperationID != "" && (!validUUID(body.LastOperationID) || !validUUID(body.LastCommandID))) {
		return identity.ErrCredentialConflict
	}
	if body.HistorySequence > 0 && body.LastOperationID == "" {
		return identity.ErrCredentialConflict
	}
	if (body.MaintenanceOwner == "") != (body.MaintenanceUntil == "") {
		return identity.ErrCredentialConflict
	}
	if body.MaintenanceUntil != "" {
		if !credentialReference.MatchString(body.MaintenanceOwner) {
			return identity.ErrCredentialConflict
		}
		if _, err := parseTime(body.MaintenanceUntil); err != nil {
			return identity.ErrCredentialConflict
		}
	}
	if body.AbortPreserveCurrent && body.State.Phase != "aborting" {
		return identity.ErrCredentialConflict
	}
	if body.LastTransition != nil {
		if body.LastTransition.Name == "" || len(body.LastTransition.Name) > 512 {
			return identity.ErrCredentialConflict
		}
		prior, err := body.LastTransition.Input.toState()
		if err != nil || validateState(body.Binding, prior) != nil {
			return identity.ErrCredentialConflict
		}
	}
	return nil
}

func validateMarkerBody(body markerBody, object SecretObject) error {
	id, err := replayObjectID(body.Binding)
	if err != nil || body.Kind != markerKind || object.ID != id || !validUUID(body.OperationID) || !validUUID(body.CommandID) ||
		body.EnvelopeSequence == 0 || body.EnvelopeSequence > maxJSONSafeInteger || body.EnvelopeSequence != object.Header.EnvelopeSequence {
		return identity.ErrCredentialConflict
	}
	return nil
}

func validateState(binding identity.CredentialBinding, state identity.CredentialRotationState) error {
	if state.Ref.ID != binding.ObjectID || !credentialReference.MatchString(state.Ref.KeyID) || state.Version == "" || state.MarkerSequence > maxJSONSafeInteger {
		return identity.ErrCredentialConflict
	}
	zeroExpected, zeroMarker := state.ExpectedCommandHash == ([32]byte{}), state.MarkerDigest == ([32]byte{})
	nonterminal := validUUID(state.OperationID) && validUUID(state.CommandID) && validTime(state.ReconcileAfter)
	switch state.Phase {
	case "ready":
		if !state.HasCurrent || state.HasPending || state.OperationID != "" || state.CommandID != "" || !state.ReconcileAfter.IsZero() || !zeroExpected || state.MarkerSequence != 0 || !zeroMarker {
			return identity.ErrCredentialConflict
		}
	case "reserved":
		if state.HasPending || !nonterminal || !zeroExpected || state.MarkerSequence != 0 || !zeroMarker {
			return identity.ErrCredentialConflict
		}
	case "candidate":
		if !state.HasPending || !nonterminal || !zeroExpected || state.MarkerSequence != 0 || !zeroMarker {
			return identity.ErrCredentialConflict
		}
	case "retiring":
		if state.HasCurrent || !state.HasPending || !nonterminal || !zeroExpected || state.MarkerSequence != 0 || !zeroMarker {
			return identity.ErrCredentialConflict
		}
	case "promoted":
		if !state.HasCurrent || state.HasPending || !nonterminal || !zeroExpected || state.MarkerSequence != 0 || !zeroMarker {
			return identity.ErrCredentialConflict
		}
	case "adopting":
		if !state.HasCurrent || state.HasPending || !nonterminal || zeroExpected || state.MarkerSequence != 0 || !zeroMarker {
			return identity.ErrCredentialConflict
		}
	case "aborting":
		if !nonterminal || !zeroExpected || state.MarkerSequence != 0 || !zeroMarker {
			return identity.ErrCredentialConflict
		}
	case "deleting":
		if !nonterminal || !zeroExpected || state.MarkerSequence == 0 || zeroMarker {
			return identity.ErrCredentialConflict
		}
	default:
		return identity.ErrCredentialConflict
	}
	return nil
}

func validBinding(binding identity.CredentialBinding) bool {
	expected, err := identity.NewAppleCredentialBinding(binding.AccountID, binding.ServerInstanceID)
	return err == nil && expected == binding
}
func validUUID(value string) bool {
	return uuidPattern.MatchString(value) && value != "00000000-0000-0000-0000-000000000000"
}
func validTime(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC && value.Year() >= 1 && value.Year() <= 9999
}
func canonicalTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
func canonicalOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return canonicalTime(value)
}
func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != value {
		return time.Time{}, identity.ErrCredentialConflict
	}
	return parsed, nil
}
func parseOptionalTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return parseTime(value)
}
func encodeOptionalDigest(value [32]byte) string {
	if value == ([32]byte{}) {
		return ""
	}
	return hex.EncodeToString(value[:])
}
func decodeOptionalDigest(value string) ([32]byte, bool) {
	if value == "" {
		return [32]byte{}, true
	}
	return decodeDigest(value)
}
func decodeDigest(value string) ([32]byte, bool) {
	var result [32]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != value {
		return result, false
	}
	copy(result[:], decoded)
	return result, true
}

func strictJSON(data []byte, target any, canonical bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	if canonical {
		encoded, err := json.Marshal(target)
		if err != nil || !bytes.Equal(encoded, data) {
			return errors.New("noncanonical JSON")
		}
	}
	return nil
}

func cloneMainRecord(record mainRecord) mainRecord {
	encoded, _ := json.Marshal(record.body)
	var body mainBody
	_ = json.Unmarshal(encoded, &body)
	return mainRecord{version: record.version, sequence: record.sequence, cipherHash: record.cipherHash, body: body}
}
func cloneProviderObject(object SecretObject) SecretObject {
	object.Ciphertext = append([]byte(nil), object.Ciphertext...)
	return object
}
func mainBodiesEqual(left, right mainBody) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return bytes.Equal(a, b)
}
func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
func reconcileContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.WithTimeout(context.Background(), reconcileTimeout)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), reconcileTimeout)
}
func providerError(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return identity.ErrUnavailable
}

func deletionMarker(binding identity.CredentialBinding, ref identity.CredentialRef, operation identity.DeletionOperation, sequence uint64, version string) [32]byte {
	digest := sha256.New()
	for _, value := range []string{"baby-tracker-credential-deletion-marker-v1", binding.AccountID, binding.ServerInstanceID, binding.Purpose, binding.ObjectID, ref.ID, ref.KeyID, operation.OperationID, operation.CommandID, version} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		digest.Write(length[:])
		digest.Write([]byte(value))
	}
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], sequence)
	digest.Write(encoded[:])
	var result [32]byte
	copy(result[:], digest.Sum(nil))
	return result
}

var _ identity.CredentialVault = (*vault)(nil)
var _ identity.CredentialInventory = (*vault)(nil)
var _ CredentialReplayMarkerPurger = (*vault)(nil)
