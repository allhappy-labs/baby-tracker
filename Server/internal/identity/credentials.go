package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	AppleCredentialPurpose     = "apple-refresh-revocation-v1"
	maxCredentialOpaque        = 64 << 10
	maxAppleToken              = 16 << 10
	credentialIntentTTL        = 5 * time.Second
	credentialReconcileTTL     = 10 * time.Minute
	credentialBindingAADDomain = "baby-tracker-credential-binding-v1"

	credentialPhaseAbsent    = "absent"
	credentialPhaseReady     = "ready"
	credentialPhaseReserved  = "reserved"
	credentialPhaseCandidate = "candidate"
	credentialPhaseRetiring  = "retiring"
	credentialPhasePromoted  = "promoted"
	credentialPhaseAdopting  = "adopting"
	credentialPhaseAborting  = "aborting"
	credentialPhaseDeleting  = "deleting"

	memoryOperationReserveRotation   = "reserve_rotation"
	memoryOperationStoreCandidate    = "store_candidate"
	memoryOperationMarkPriorRevoked  = "mark_prior_revoked"
	memoryOperationPromotePending    = "promote_pending"
	memoryOperationConfirmPromotion  = "confirm_promotion"
	memoryOperationMarkDeleting      = "mark_deleting"
	memoryOperationEraseDeleting     = "erase_deleting"
	memoryOperationStoreReplayMarker = "store_replay_marker"
	memoryOperationDelete            = "delete"
)

const (
	CredentialPhaseAbsent    = credentialPhaseAbsent
	CredentialPhaseReady     = credentialPhaseReady
	CredentialPhaseReserved  = credentialPhaseReserved
	CredentialPhaseCandidate = credentialPhaseCandidate
	CredentialPhaseRetiring  = credentialPhaseRetiring
	CredentialPhasePromoted  = credentialPhasePromoted
	CredentialPhaseAdopting  = credentialPhaseAdopting
	CredentialPhaseAborting  = credentialPhaseAborting
	CredentialPhaseDeleting  = credentialPhaseDeleting
)

var (
	ErrCredentialRotationBusy  = codedError("credential_rotation_busy")
	ErrCredentialConflict      = codedError("credential_conflict")
	credentialReferencePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

type CredentialBinding struct {
	AccountID, ServerInstanceID, Purpose, ObjectID string
}

type CredentialRef struct{ ID, KeyID string }

type CredentialRotationState struct {
	Ref                                    CredentialRef
	Version, OperationID, CommandID, Phase string
	ReconcileAfter                         time.Time
	ExpectedCommandHash, MarkerDigest      [32]byte
	MarkerSequence                         uint64
	HasCurrent, HasPending                 bool
}

type DeletionCapability struct{ Opaque []byte }
type AdoptionCapability struct{ Opaque []byte }

type PromotionCASIntent struct {
	Opaque    []byte
	expiresAt time.Time
}

func NewPromotionCASIntent(opaque []byte, expiresAt, now time.Time) (PromotionCASIntent, error) {
	if !validOpaqueCredentialValue(opaque) || !validIntentExpiry(expiresAt, now) {
		return PromotionCASIntent{}, ErrCredentialConflict
	}
	return PromotionCASIntent{Opaque: append([]byte(nil), opaque...), expiresAt: expiresAt}, nil
}

func (intent PromotionCASIntent) ExpiresAt() time.Time { return intent.expiresAt }

type DeletionCASIntent struct {
	Opaque    []byte
	expiresAt time.Time
}

func NewDeletionCASIntent(opaque []byte, expiresAt, now time.Time) (DeletionCASIntent, error) {
	if !validOpaqueCredentialValue(opaque) || !validIntentExpiry(expiresAt, now) {
		return DeletionCASIntent{}, ErrCredentialConflict
	}
	return DeletionCASIntent{Opaque: append([]byte(nil), opaque...), expiresAt: expiresAt}, nil
}

func (intent DeletionCASIntent) ExpiresAt() time.Time { return intent.expiresAt }

type DeletionOperation struct{ OperationID, CommandID string }

type AdoptionRecovery struct {
	Binding                CredentialBinding
	Ref                    CredentialRef
	OperationID, CommandID string
	ExpectedCommandHash    [32]byte
}

type DeletionRecovery struct {
	Binding                CredentialBinding
	Ref                    CredentialRef
	OperationID, CommandID string
	MarkerSequence         uint64
	MarkerDigest           [32]byte
}

// CredentialVault durably keeps the last completed or abandoned rotation
// coordinates as bounded replay metadata for each stable binding, including
// across adapter reopen or process restart. That metadata is not a
// CredentialRotationState and must not make Resolve or inventory report an
// object after no-current maintenance deletes it. ReserveRotation rejects reuse
// of either coordinate before creating a new credential object and carries the
// marker into the reclaimed object's stale-coordinate checks. Delete must prove
// the marker write before removing the main object. Only a future, explicit
// account-purge operation may remove the replay metadata.
type CredentialVault interface {
	Resolve(context.Context, CredentialBinding) (CredentialRotationState, bool, error)
	ReserveRotation(context.Context, CredentialBinding, string, string) (CredentialRotationState, error)
	StoreCandidate(context.Context, CredentialBinding, CredentialRotationState, []byte) (CredentialRotationState, error)
	OpenPrior(context.Context, CredentialBinding, CredentialRotationState) ([]byte, error)
	MarkPriorRevoked(context.Context, CredentialBinding, CredentialRotationState) (CredentialRotationState, error)
	PromotePending(context.Context, CredentialBinding, CredentialRotationState) (CredentialRotationState, error)
	PreparePromotionCAS(context.Context, CredentialBinding, CredentialRotationState, [32]byte) (PromotionCASIntent, error)
	ConfirmPromotion(context.Context, PromotionCASIntent) (AdoptionCapability, error)
	RecoverAdoptionCapability(context.Context, AdoptionRecovery) (AdoptionCapability, error)
	FinishAdoption(context.Context, CredentialBinding, AdoptionCapability) error
	AbandonAdoption(context.Context, CredentialBinding, AdoptionCapability) error
	MarkCommitted(context.Context, CredentialBinding, CredentialRotationState) (CredentialRotationState, error)
	MarkAbandoned(context.Context, CredentialBinding, CredentialRotationState) (CredentialRotationState, error)
	PrepareDeletionCAS(context.Context, CredentialBinding, CredentialRotationState, DeletionOperation) (DeletionCASIntent, error)
	MarkDeleting(context.Context, DeletionCASIntent) (DeletionCapability, error)
	RecoverDeletionCapability(context.Context, DeletionRecovery) (DeletionCapability, error)
	ClaimMaintenance(context.Context, CredentialBinding, CredentialRotationState, string, time.Time) (CredentialRotationState, error)
	OpenMaintenanceTokens(context.Context, CredentialBinding, CredentialRotationState) ([][]byte, error)
	OpenDeletingTokens(context.Context, DeletionCapability) ([][]byte, error)
	EraseDeletingTokens(context.Context, DeletionCapability) (DeletionCapability, error)
	CompleteMaintenance(context.Context, CredentialBinding, CredentialRotationState) (CredentialRotationState, error)
	Delete(context.Context, DeletionCapability) error
	ConfirmAbsent(context.Context, CredentialBinding, CredentialRef) (bool, error)
}

type CredentialMetadata struct {
	Binding   CredentialBinding
	Ref       CredentialRef
	Phase     string
	CreatedAt time.Time
}

type CredentialInventory interface {
	List(context.Context, time.Time, string, int) ([]CredentialMetadata, string, error)
}

type CredentialRotator interface {
	RotateForSession(context.Context, CredentialBinding, string, string, func(context.Context) (AppleTokens, error)) (CredentialPromotion, error)
	PreparePromotionCAS(context.Context, CredentialBinding, CredentialPromotion, [32]byte) (CredentialPromotion, error)
	ConfirmPromotion(context.Context, CredentialPromotion) (AdoptionCapability, error)
	RecoverAdoptionCapability(context.Context, CredentialPromotion) (AdoptionCapability, error)
	FinishAdoption(context.Context, CredentialBinding, AdoptionCapability) error
	AbandonAdoption(context.Context, CredentialBinding, AdoptionCapability) error
	Resume(context.Context, CredentialBinding) (CredentialResume, error)
	Commit(context.Context, CredentialBinding, string) error
	Abandon(context.Context, CredentialBinding, string) error
}

type CredentialResume struct {
	State               CredentialRotationState
	ExpectedCommandHash [32]byte
	Adoption            *AdoptionCapability
	Deletion            *DeletionCapability
}

type CredentialPromotion struct {
	Ref                             CredentialRef
	OperationID, CommandID, Version string
	ExpectedCommandHash             [32]byte
	ExpiresAt                       time.Time
	ConfirmIntent                   PromotionCASIntent
	binding                         CredentialBinding
}

type credentialRotator struct {
	vault   CredentialVault
	revoker AppleRevoker
	newID   func() string
	mu      sync.Mutex
	locks   map[string]*credentialBindingLock
}

type credentialBindingLock struct {
	mu   sync.Mutex
	refs int
}

func NewCredentialRotator(vault CredentialVault, revoker AppleRevoker, newID func() string) (CredentialRotator, error) {
	if vault == nil || revoker == nil || newID == nil {
		return nil, errors.New("credential rotator requires vault, revoker, and ID source")
	}
	return &credentialRotator{vault: vault, revoker: revoker, newID: newID, locks: make(map[string]*credentialBindingLock)}, nil
}

func (rotator *credentialRotator) RotateForSession(ctx context.Context, binding CredentialBinding, operationID, commandID string, exchange func(context.Context) (AppleTokens, error)) (CredentialPromotion, error) {
	if rotator == nil || !validCredentialBinding(binding) || !validNonzeroUUID(operationID) || !validNonzeroUUID(commandID) || exchange == nil {
		return CredentialPromotion{}, ErrInvalidRequest
	}
	unlock := rotator.lock(binding.ObjectID)
	defer unlock()
	state, err := rotator.vault.ReserveRotation(ctx, binding, operationID, commandID)
	if err != nil {
		return CredentialPromotion{}, err
	}
	if state.OperationID != operationID || state.CommandID != commandID {
		return CredentialPromotion{}, ErrCredentialRotationBusy
	}
	for {
		switch state.Phase {
		case credentialPhaseReserved:
			tokens, exchangeErr := exchange(ctx)
			if exchangeErr != nil {
				_, _ = rotator.vault.MarkAbandoned(ctx, binding, state)
				return CredentialPromotion{}, exchangeErr
			}
			if !validAppleToken(tokens.IdentityToken) || !validAppleToken(tokens.RefreshToken) {
				_, _ = rotator.vault.MarkAbandoned(ctx, binding, state)
				return CredentialPromotion{}, ErrInvalidAppleProof
			}
			state, err = rotator.vault.StoreCandidate(ctx, binding, state, []byte(tokens.RefreshToken))
			if err != nil {
				state, err = rotator.reconcile(ctx, binding, operationID, commandID, credentialPhaseCandidate, err)
			}
		case credentialPhaseCandidate:
			if state.HasCurrent {
				prior, openErr := rotator.vault.OpenPrior(ctx, binding, state)
				if openErr != nil {
					return CredentialPromotion{}, openErr
				}
				if revokeErr := rotator.revoker.Revoke(ctx, AppleRevocationRequest{Binding: binding, RefreshToken: append([]byte(nil), prior...)}); revokeErr != nil {
					return CredentialPromotion{}, revokeErr
				}
				state, err = rotator.vault.MarkPriorRevoked(ctx, binding, state)
				if err != nil {
					state, err = rotator.reconcile(ctx, binding, operationID, commandID, credentialPhaseRetiring, err)
				}
			} else {
				state, err = rotator.vault.PromotePending(ctx, binding, state)
				if err != nil {
					state, err = rotator.reconcile(ctx, binding, operationID, commandID, credentialPhasePromoted, err)
				}
			}
		case credentialPhaseRetiring:
			state, err = rotator.vault.PromotePending(ctx, binding, state)
			if err != nil {
				state, err = rotator.reconcile(ctx, binding, operationID, commandID, credentialPhasePromoted, err)
			}
		case credentialPhasePromoted:
			if err := validateCredentialState(binding, state); err != nil {
				return CredentialPromotion{}, err
			}
			return promotionFromState(binding, state), nil
		default:
			return CredentialPromotion{}, ErrCredentialRotationBusy
		}
		if err != nil {
			return CredentialPromotion{}, err
		}
	}
}

func (rotator *credentialRotator) reconcile(ctx context.Context, binding CredentialBinding, operationID, commandID, phase string, original error) (CredentialRotationState, error) {
	state, exists, err := rotator.vault.Resolve(ctx, binding)
	if err != nil || !exists || state.OperationID != operationID || state.CommandID != commandID || state.Phase != phase {
		return CredentialRotationState{}, original
	}
	return state, nil
}

func (rotator *credentialRotator) PreparePromotionCAS(ctx context.Context, binding CredentialBinding, promotion CredentialPromotion, hash [32]byte) (CredentialPromotion, error) {
	if rotator == nil || !validCredentialBinding(binding) || zeroDigest(hash) || !validPromotionShape(binding, promotion, false) {
		return CredentialPromotion{}, ErrCredentialConflict
	}
	state, exists, err := rotator.vault.Resolve(ctx, binding)
	if err != nil {
		return CredentialPromotion{}, err
	}
	if !exists || state.Phase != credentialPhasePromoted || state.Ref != promotion.Ref || state.Version != promotion.Version ||
		state.OperationID != promotion.OperationID || state.CommandID != promotion.CommandID {
		return CredentialPromotion{}, ErrCredentialConflict
	}
	intent, err := rotator.vault.PreparePromotionCAS(ctx, binding, state, hash)
	if err != nil {
		return CredentialPromotion{}, err
	}
	if !validPromotionIntent(intent) {
		return CredentialPromotion{}, ErrCredentialConflict
	}
	promotion.ExpectedCommandHash = hash
	promotion.ConfirmIntent = clonePromotionIntent(intent)
	promotion.ExpiresAt = intent.ExpiresAt()
	return promotion, nil
}

func (rotator *credentialRotator) ConfirmPromotion(ctx context.Context, promotion CredentialPromotion) (AdoptionCapability, error) {
	if rotator == nil || !validCredentialBinding(promotion.binding) || !validPromotionShape(promotion.binding, promotion, true) || promotion.ExpiresAt != promotion.ConfirmIntent.ExpiresAt() {
		return AdoptionCapability{}, ErrCredentialConflict
	}
	return rotator.vault.ConfirmPromotion(ctx, clonePromotionIntent(promotion.ConfirmIntent))
}

func (rotator *credentialRotator) RecoverAdoptionCapability(ctx context.Context, promotion CredentialPromotion) (AdoptionCapability, error) {
	if rotator == nil || !validCredentialBinding(promotion.binding) || zeroDigest(promotion.ExpectedCommandHash) {
		return AdoptionCapability{}, ErrCredentialConflict
	}
	state, exists, err := rotator.vault.Resolve(ctx, promotion.binding)
	if err != nil || !exists {
		if err != nil {
			return AdoptionCapability{}, err
		}
		return AdoptionCapability{}, ErrCredentialConflict
	}
	if state.Phase != credentialPhaseAdopting || state.Ref != promotion.Ref || state.OperationID != promotion.OperationID ||
		state.CommandID != promotion.CommandID || state.ExpectedCommandHash != promotion.ExpectedCommandHash {
		return AdoptionCapability{}, ErrCredentialConflict
	}
	return rotator.vault.RecoverAdoptionCapability(ctx, AdoptionRecovery{Ref: promotion.Ref, OperationID: promotion.OperationID,
		CommandID: promotion.CommandID, ExpectedCommandHash: promotion.ExpectedCommandHash, Binding: promotion.binding})
}

func (rotator *credentialRotator) FinishAdoption(ctx context.Context, binding CredentialBinding, capability AdoptionCapability) error {
	if rotator == nil || !validCredentialBinding(binding) || !validOpaqueCredentialValue(capability.Opaque) {
		return ErrCredentialConflict
	}
	return rotator.vault.FinishAdoption(ctx, binding, cloneAdoptionCapability(capability))
}

func (rotator *credentialRotator) AbandonAdoption(ctx context.Context, binding CredentialBinding, capability AdoptionCapability) error {
	if rotator == nil || !validCredentialBinding(binding) || !validOpaqueCredentialValue(capability.Opaque) {
		return ErrCredentialConflict
	}
	return rotator.vault.AbandonAdoption(ctx, binding, cloneAdoptionCapability(capability))
}

func (rotator *credentialRotator) Resume(ctx context.Context, binding CredentialBinding) (CredentialResume, error) {
	if rotator == nil || !validCredentialBinding(binding) {
		return CredentialResume{}, ErrCredentialConflict
	}
	state, exists, err := rotator.vault.Resolve(ctx, binding)
	if err != nil {
		return CredentialResume{}, err
	}
	if !exists {
		return CredentialResume{State: CredentialRotationState{Phase: credentialPhaseAbsent}}, nil
	}
	result := CredentialResume{State: state, ExpectedCommandHash: state.ExpectedCommandHash}
	switch state.Phase {
	case credentialPhaseAdopting:
		capability, recoverErr := rotator.vault.RecoverAdoptionCapability(ctx, AdoptionRecovery{Binding: binding, Ref: state.Ref,
			OperationID: state.OperationID, CommandID: state.CommandID, ExpectedCommandHash: state.ExpectedCommandHash})
		if recoverErr != nil {
			return CredentialResume{}, recoverErr
		}
		result.Adoption = &capability
	case credentialPhaseDeleting:
		capability, recoverErr := rotator.vault.RecoverDeletionCapability(ctx, DeletionRecovery{Binding: binding, Ref: state.Ref,
			OperationID: state.OperationID, CommandID: state.CommandID, MarkerSequence: state.MarkerSequence, MarkerDigest: state.MarkerDigest})
		if recoverErr != nil {
			return CredentialResume{}, recoverErr
		}
		result.Deletion = &capability
	}
	return result, nil
}

func (rotator *credentialRotator) Commit(ctx context.Context, binding CredentialBinding, operationID string) error {
	resume, err := rotator.Resume(ctx, binding)
	if err != nil {
		return err
	}
	if resume.State.OperationID != operationID || resume.Adoption == nil {
		return ErrCredentialConflict
	}
	return rotator.FinishAdoption(ctx, binding, *resume.Adoption)
}

func (rotator *credentialRotator) Abandon(ctx context.Context, binding CredentialBinding, operationID string) error {
	state, exists, err := rotator.vault.Resolve(ctx, binding)
	if err != nil {
		return err
	}
	if !exists || state.OperationID != operationID {
		return ErrCredentialConflict
	}
	_, err = rotator.vault.MarkAbandoned(ctx, binding, state)
	return err
}

func (rotator *credentialRotator) lock(key string) func() {
	rotator.mu.Lock()
	entry := rotator.locks[key]
	if entry == nil {
		entry = &credentialBindingLock{}
		rotator.locks[key] = entry
	}
	entry.refs++
	rotator.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		rotator.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(rotator.locks, key)
		}
		rotator.mu.Unlock()
	}
}

func promotionFromState(binding CredentialBinding, state CredentialRotationState) CredentialPromotion {
	return CredentialPromotion{Ref: state.Ref, OperationID: state.OperationID, CommandID: state.CommandID, Version: state.Version, binding: binding}
}

// MemoryCredentialVault is a deterministic, concurrency-safe conformance fake.
// It keeps plaintext only in private slots and exposes only opaque handles.
type MemoryCredentialVault struct {
	*MemoryCredentialBacking
	now func() time.Time
}

// MemoryCredentialBacking is the fake's durable store. Reopened adapter
// instances share this backing but have independent adapter identity.
type MemoryCredentialBacking struct {
	mu            sync.Mutex
	objects       map[string]*memoryCredentialObject
	intents       map[string]memoryCredentialIntent
	adoptions     map[string]*memoryAdoptionCapability
	deletions     map[string]*memoryDeletionCapability
	failures      map[string]error
	replays       map[string]memoryTransitionReplay
	replayMarkers map[string]memoryCredentialReplayMarker
	counter       uint64
	capabilityKey [32]byte
}

type memoryCredentialObject struct {
	binding              CredentialBinding
	state                CredentialRotationState
	current, pending     []byte
	pendingDigest        [32]byte
	createdAt            time.Time
	revision             uint64
	lastOperationID      string
	lastCommandID        string
	abortPreserveCurrent bool
	maintenanceOwner     string
	maintenanceUntil     time.Time
}

type memoryTransitionReplay struct {
	binding       CredentialBinding
	input, output CredentialRotationState
	present       bool
}

type memoryCredentialReplayMarker struct {
	binding                CredentialBinding
	operationID, commandID string
}

type memoryCredentialIntent struct {
	kind                   string
	binding                CredentialBinding
	ref                    CredentialRef
	expectedVersion        string
	operationID, commandID string
	hash                   [32]byte
	markerSequence         uint64
	markerDigest           [32]byte
	expiresAt              time.Time
}

type memoryAdoptionCapability struct {
	binding                CredentialBinding
	ref                    CredentialRef
	version                string
	operationID, commandID string
	hash                   [32]byte
	completed              string
}

type memoryDeletionCapability struct {
	binding                CredentialBinding
	ref                    CredentialRef
	version                string
	operationID, commandID string
	markerSequence         uint64
	markerDigest           [32]byte
	completed              string
	next                   []byte
}

func NewMemoryCredentialVault(now func() time.Time) *MemoryCredentialVault {
	if now == nil {
		now = time.Now
	}
	backing := &MemoryCredentialBacking{objects: make(map[string]*memoryCredentialObject), intents: make(map[string]memoryCredentialIntent),
		adoptions: make(map[string]*memoryAdoptionCapability), deletions: make(map[string]*memoryDeletionCapability), failures: make(map[string]error),
		replays: make(map[string]memoryTransitionReplay), replayMarkers: make(map[string]memoryCredentialReplayMarker),
		capabilityKey: sha256.Sum256([]byte("baby-tracker-memory-credential-vault-v1"))}
	return &MemoryCredentialVault{MemoryCredentialBacking: backing, now: now}
}

// Reopen constructs a new adapter instance over the same durable fake store.
func (vault *MemoryCredentialVault) Reopen() *MemoryCredentialVault {
	if vault == nil || vault.MemoryCredentialBacking == nil {
		return nil
	}
	return &MemoryCredentialVault{MemoryCredentialBacking: vault.MemoryCredentialBacking, now: vault.now}
}

func (vault *MemoryCredentialVault) Resolve(ctx context.Context, binding CredentialBinding) (CredentialRotationState, bool, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return CredentialRotationState{}, false, err
	}
	object, err := vault.object(binding)
	if err != nil {
		return CredentialRotationState{}, false, err
	}
	if object == nil {
		return CredentialRotationState{Phase: credentialPhaseAbsent}, false, nil
	}
	return object.state, true, nil
}

func (vault *MemoryCredentialVault) ReserveRotation(ctx context.Context, binding CredentialBinding, operationID, commandID string) (CredentialRotationState, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil || !validCredentialBinding(binding) || !validNonzeroUUID(operationID) || !validNonzeroUUID(commandID) {
		if err != nil {
			return CredentialRotationState{}, err
		}
		return CredentialRotationState{}, ErrCredentialConflict
	}
	object, err := vault.object(binding)
	if err != nil {
		return CredentialRotationState{}, err
	}
	marker, markerExists := vault.replayMarkers[binding.ObjectID]
	if markerExists {
		if marker.binding != binding {
			return CredentialRotationState{}, ErrCredentialConflict
		}
		if operationID == marker.operationID || commandID == marker.commandID {
			return CredentialRotationState{}, ErrCredentialConflict
		}
	}
	if object == nil {
		object = &memoryCredentialObject{binding: binding, createdAt: vault.now().UTC(), revision: 1}
		if markerExists {
			object.lastOperationID, object.lastCommandID = marker.operationID, marker.commandID
		}
		object.state = CredentialRotationState{Ref: CredentialRef{ID: binding.ObjectID, KeyID: "memory.v1"}, Version: "1", OperationID: operationID,
			CommandID: commandID, Phase: credentialPhaseReserved, ReconcileAfter: vault.now().UTC().Add(credentialReconcileTTL)}
		vault.objects[binding.ObjectID] = object
		return object.state, vault.takeFailure(memoryOperationReserveRotation)
	}
	if object.state.Phase == credentialPhaseReady {
		if operationID == object.lastOperationID || commandID == object.lastCommandID {
			return CredentialRotationState{}, ErrCredentialConflict
		}
		object.state.OperationID, object.state.CommandID, object.state.Phase = operationID, commandID, credentialPhaseReserved
		object.state.ReconcileAfter = vault.now().UTC().Add(credentialReconcileTTL)
		object.state.ExpectedCommandHash, object.state.MarkerDigest, object.state.MarkerSequence = [32]byte{}, [32]byte{}, 0
		vault.bump(object)
		return object.state, vault.takeFailure(memoryOperationReserveRotation)
	}
	if object.state.OperationID == operationID && object.state.CommandID == commandID && object.state.Phase != credentialPhaseDeleting && object.state.Phase != credentialPhaseAborting {
		return object.state, nil
	}
	return CredentialRotationState{}, ErrCredentialRotationBusy
}

func (vault *MemoryCredentialVault) StoreCandidate(ctx context.Context, binding CredentialBinding, expected CredentialRotationState, token []byte) (CredentialRotationState, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return CredentialRotationState{}, err
	}
	if len(token) == 0 || len(token) > maxAppleToken {
		return CredentialRotationState{}, ErrCredentialConflict
	}
	object, err := vault.expected(binding, expected)
	if err != nil {
		if replay, ok := vault.replayTransition(memoryOperationStoreCandidate, binding, expected); ok {
			existing, objectErr := vault.object(binding)
			if objectErr == nil && existing != nil && existing.pendingDigest == sha256.Sum256(token) {
				return replay, nil
			}
		}
		return CredentialRotationState{}, err
	}
	if object.state.Phase != credentialPhaseReserved || object.pending != nil {
		return CredentialRotationState{}, ErrCredentialConflict
	}
	object.pending = append([]byte(nil), token...)
	object.pendingDigest = sha256.Sum256(token)
	object.state.Phase, object.state.HasPending = credentialPhaseCandidate, true
	vault.bump(object)
	state := object.state
	vault.recordTransition(memoryOperationStoreCandidate, binding, expected, state, true)
	return state, vault.takeFailure(memoryOperationStoreCandidate)
}

func (vault *MemoryCredentialVault) OpenPrior(ctx context.Context, binding CredentialBinding, expected CredentialRotationState) ([]byte, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	object, err := vault.expected(binding, expected)
	if err != nil || object.state.Phase != credentialPhaseCandidate || len(object.current) == 0 {
		if err != nil {
			return nil, err
		}
		return nil, ErrCredentialConflict
	}
	return append([]byte(nil), object.current...), nil
}

func (vault *MemoryCredentialVault) MarkPriorRevoked(ctx context.Context, binding CredentialBinding, expected CredentialRotationState) (CredentialRotationState, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return CredentialRotationState{}, err
	}
	object, err := vault.expected(binding, expected)
	if err != nil {
		if replay, ok := vault.replayTransition(memoryOperationMarkPriorRevoked, binding, expected); ok {
			return replay, nil
		}
		return CredentialRotationState{}, err
	}
	if object.state.Phase != credentialPhaseCandidate || len(object.current) == 0 || len(object.pending) == 0 {
		return CredentialRotationState{}, ErrCredentialConflict
	}
	object.current = nil
	object.state.HasCurrent, object.state.Phase = false, credentialPhaseRetiring
	vault.bump(object)
	state := object.state
	vault.recordTransition(memoryOperationMarkPriorRevoked, binding, expected, state, true)
	return state, vault.takeFailure(memoryOperationMarkPriorRevoked)
}

func (vault *MemoryCredentialVault) PromotePending(ctx context.Context, binding CredentialBinding, expected CredentialRotationState) (CredentialRotationState, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return CredentialRotationState{}, err
	}
	object, err := vault.expected(binding, expected)
	if err != nil {
		if replay, ok := vault.replayTransition(memoryOperationPromotePending, binding, expected); ok {
			return replay, nil
		}
		return CredentialRotationState{}, err
	}
	if (object.state.Phase != credentialPhaseCandidate || object.state.HasCurrent) && object.state.Phase != credentialPhaseRetiring {
		return CredentialRotationState{}, ErrCredentialConflict
	}
	if len(object.pending) == 0 {
		return CredentialRotationState{}, ErrCredentialConflict
	}
	object.current, object.pending = object.pending, nil
	object.pendingDigest = [32]byte{}
	object.state.HasCurrent, object.state.HasPending, object.state.Phase = true, false, credentialPhasePromoted
	vault.bump(object)
	state := object.state
	vault.recordTransition(memoryOperationPromotePending, binding, expected, state, true)
	return state, vault.takeFailure(memoryOperationPromotePending)
}

func (vault *MemoryCredentialVault) PreparePromotionCAS(ctx context.Context, binding CredentialBinding, expected CredentialRotationState, hash [32]byte) (PromotionCASIntent, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return PromotionCASIntent{}, err
	}
	object, err := vault.expected(binding, expected)
	if err != nil || object.state.Phase != credentialPhasePromoted || zeroDigest(hash) {
		if err != nil {
			return PromotionCASIntent{}, err
		}
		return PromotionCASIntent{}, ErrCredentialConflict
	}
	expiresAt := vault.now().UTC().Add(credentialIntentTTL)
	handle := vault.newHandle("promotion-intent")
	vault.intents[string(handle)] = memoryCredentialIntent{kind: "promotion", binding: binding, ref: object.state.Ref, expectedVersion: object.state.Version,
		operationID: object.state.OperationID, commandID: object.state.CommandID, hash: hash, expiresAt: expiresAt}
	return NewPromotionCASIntent(handle, expiresAt, vault.now().UTC())
}

func (vault *MemoryCredentialVault) ConfirmPromotion(ctx context.Context, intent PromotionCASIntent) (AdoptionCapability, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return AdoptionCapability{}, err
	}
	if !validPromotionIntent(intent) || !vault.now().UTC().Before(intent.ExpiresAt()) {
		return AdoptionCapability{}, ErrCredentialConflict
	}
	stored, ok := vault.intents[string(intent.Opaque)]
	if !ok || stored.kind != "promotion" || stored.expiresAt != intent.ExpiresAt() {
		return AdoptionCapability{}, ErrCredentialConflict
	}
	delete(vault.intents, string(intent.Opaque))
	object, err := vault.object(stored.binding)
	if err != nil || object == nil || object.state.Phase != credentialPhasePromoted || object.state.Version != stored.expectedVersion ||
		object.state.Ref != stored.ref || object.state.OperationID != stored.operationID || object.state.CommandID != stored.commandID {
		return AdoptionCapability{}, ErrCredentialConflict
	}
	object.state.Phase, object.state.ExpectedCommandHash, object.state.ReconcileAfter = credentialPhaseAdopting, stored.hash, stored.expiresAt
	vault.bump(object)
	handle := vault.newHandle("adoption-capability")
	vault.adoptions[string(handle)] = &memoryAdoptionCapability{binding: stored.binding, ref: stored.ref, version: object.state.Version,
		operationID: stored.operationID, commandID: stored.commandID, hash: stored.hash}
	capability := AdoptionCapability{Opaque: handle}
	if err := vault.takeFailure(memoryOperationConfirmPromotion); err != nil {
		return AdoptionCapability{}, err
	}
	return capability, nil
}

func (vault *MemoryCredentialVault) RecoverAdoptionCapability(ctx context.Context, recovery AdoptionRecovery) (AdoptionCapability, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return AdoptionCapability{}, err
	}
	object, err := vault.object(recovery.Binding)
	if err != nil || object == nil || object.state.Phase != credentialPhaseAdopting || object.state.Ref != recovery.Ref ||
		object.state.OperationID != recovery.OperationID || object.state.CommandID != recovery.CommandID ||
		zeroDigest(recovery.ExpectedCommandHash) || object.state.ExpectedCommandHash != recovery.ExpectedCommandHash {
		return AdoptionCapability{}, ErrCredentialConflict
	}
	handle := vault.newHandle("recovered-adoption-capability")
	vault.adoptions[string(handle)] = &memoryAdoptionCapability{binding: recovery.Binding, ref: recovery.Ref, version: object.state.Version,
		operationID: recovery.OperationID, commandID: recovery.CommandID, hash: recovery.ExpectedCommandHash}
	return AdoptionCapability{Opaque: handle}, nil
}

func (vault *MemoryCredentialVault) FinishAdoption(ctx context.Context, binding CredentialBinding, capability AdoptionCapability) error {
	return vault.finishAdoption(ctx, binding, capability, "finished")
}

func (vault *MemoryCredentialVault) AbandonAdoption(ctx context.Context, binding CredentialBinding, capability AdoptionCapability) error {
	return vault.finishAdoption(ctx, binding, capability, "abandoned")
}

func (vault *MemoryCredentialVault) finishAdoption(ctx context.Context, binding CredentialBinding, capability AdoptionCapability, outcome string) error {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return err
	}
	record := vault.adoptions[string(capability.Opaque)]
	if record == nil || record.binding != binding {
		return ErrCredentialConflict
	}
	if record.completed != "" {
		if record.completed == outcome {
			return nil
		}
		return ErrCredentialConflict
	}
	object, err := vault.object(binding)
	if err != nil || object == nil || object.state.Phase != credentialPhaseAdopting || object.state.Version != record.version ||
		object.state.Ref != record.ref || object.state.OperationID != record.operationID || object.state.CommandID != record.commandID || object.state.ExpectedCommandHash != record.hash {
		return ErrCredentialConflict
	}
	object.lastOperationID, object.lastCommandID = object.state.OperationID, object.state.CommandID
	vault.markReplay(binding, object.state.OperationID, object.state.CommandID)
	if outcome == "finished" {
		object.state.Phase, object.state.OperationID, object.state.CommandID = credentialPhaseReady, "", ""
		object.state.ExpectedCommandHash = [32]byte{}
		object.state.ReconcileAfter = time.Time{}
	} else {
		object.state.Phase = credentialPhaseAborting
		object.state.ExpectedCommandHash = [32]byte{}
		object.abortPreserveCurrent = false
	}
	vault.bump(object)
	record.completed = outcome
	return nil
}

func (vault *MemoryCredentialVault) MarkCommitted(ctx context.Context, binding CredentialBinding, expected CredentialRotationState) (CredentialRotationState, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return CredentialRotationState{}, err
	}
	object, err := vault.expected(binding, expected)
	if err != nil {
		if replay, ok := vault.replayTransition("mark_committed", binding, expected); ok {
			return replay, nil
		}
		return CredentialRotationState{}, err
	}
	if object.state.Phase != credentialPhaseAdopting {
		return CredentialRotationState{}, ErrCredentialConflict
	}
	object.lastOperationID, object.lastCommandID = object.state.OperationID, object.state.CommandID
	vault.markReplay(binding, object.state.OperationID, object.state.CommandID)
	object.state.Phase, object.state.OperationID, object.state.CommandID = credentialPhaseReady, "", ""
	object.state.ExpectedCommandHash = [32]byte{}
	object.state.ReconcileAfter = time.Time{}
	vault.bump(object)
	state := object.state
	vault.recordTransition("mark_committed", binding, expected, state, true)
	return state, nil
}

func (vault *MemoryCredentialVault) MarkAbandoned(ctx context.Context, binding CredentialBinding, expected CredentialRotationState) (CredentialRotationState, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return CredentialRotationState{}, err
	}
	object, err := vault.expected(binding, expected)
	if err != nil {
		if replay, ok := vault.replayTransition("mark_abandoned", binding, expected); ok {
			return replay, nil
		}
		return CredentialRotationState{}, err
	}
	switch object.state.Phase {
	case credentialPhaseReserved:
		object.lastOperationID, object.lastCommandID = object.state.OperationID, object.state.CommandID
		vault.markReplay(binding, object.state.OperationID, object.state.CommandID)
		if object.state.HasCurrent {
			object.state.Phase, object.state.OperationID, object.state.CommandID = credentialPhaseReady, "", ""
			object.state.ExpectedCommandHash = [32]byte{}
			object.state.ReconcileAfter = time.Time{}
			vault.bump(object)
			state := object.state
			vault.recordTransition("mark_abandoned", binding, expected, state, true)
			return state, nil
		}
		object.state.Phase = credentialPhaseAborting
		object.abortPreserveCurrent = false
	case credentialPhaseCandidate:
		object.lastOperationID, object.lastCommandID = object.state.OperationID, object.state.CommandID
		vault.markReplay(binding, object.state.OperationID, object.state.CommandID)
		object.abortPreserveCurrent = object.state.HasCurrent
		object.state.Phase = credentialPhaseAborting
	case credentialPhaseRetiring, credentialPhasePromoted, credentialPhaseAdopting:
		object.lastOperationID, object.lastCommandID = object.state.OperationID, object.state.CommandID
		vault.markReplay(binding, object.state.OperationID, object.state.CommandID)
		object.abortPreserveCurrent = false
		object.state.Phase = credentialPhaseAborting
	default:
		return CredentialRotationState{}, ErrCredentialConflict
	}
	object.state.ExpectedCommandHash = [32]byte{}
	vault.bump(object)
	state := object.state
	vault.recordTransition("mark_abandoned", binding, expected, state, true)
	return state, nil
}

func (vault *MemoryCredentialVault) PrepareDeletionCAS(ctx context.Context, binding CredentialBinding, expected CredentialRotationState, operation DeletionOperation) (DeletionCASIntent, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return DeletionCASIntent{}, err
	}
	object, err := vault.expected(binding, expected)
	if err != nil || !validNonzeroUUID(operation.OperationID) || !validNonzeroUUID(operation.CommandID) ||
		operation.OperationID == object.lastOperationID || operation.CommandID == object.lastCommandID ||
		operation.OperationID == object.state.OperationID || operation.CommandID == object.state.CommandID ||
		object.state.Phase == credentialPhaseAdopting || object.state.Phase == credentialPhaseDeleting {
		if err != nil {
			return DeletionCASIntent{}, err
		}
		return DeletionCASIntent{}, ErrCredentialConflict
	}
	sequence := object.state.MarkerSequence + 1
	marker := deletionMarker(binding, object.state.Ref, operation, sequence, object.state.Version)
	expiresAt := vault.now().UTC().Add(credentialIntentTTL)
	handle := vault.newHandle("deletion-intent")
	vault.intents[string(handle)] = memoryCredentialIntent{kind: "deletion", binding: binding, ref: object.state.Ref, expectedVersion: object.state.Version,
		operationID: operation.OperationID, commandID: operation.CommandID, markerSequence: sequence, markerDigest: marker, expiresAt: expiresAt}
	return NewDeletionCASIntent(handle, expiresAt, vault.now().UTC())
}

func (vault *MemoryCredentialVault) MarkDeleting(ctx context.Context, intent DeletionCASIntent) (DeletionCapability, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return DeletionCapability{}, err
	}
	if !validDeletionIntent(intent) || !vault.now().UTC().Before(intent.ExpiresAt()) {
		return DeletionCapability{}, ErrCredentialConflict
	}
	stored, ok := vault.intents[string(intent.Opaque)]
	if !ok || stored.kind != "deletion" || stored.expiresAt != intent.ExpiresAt() {
		return DeletionCapability{}, ErrCredentialConflict
	}
	delete(vault.intents, string(intent.Opaque))
	object, err := vault.object(stored.binding)
	if err != nil || object == nil || object.state.Version != stored.expectedVersion || object.state.Ref != stored.ref ||
		object.state.Phase == credentialPhaseAdopting || object.state.Phase == credentialPhaseDeleting {
		return DeletionCapability{}, ErrCredentialConflict
	}
	if validNonzeroUUID(object.state.OperationID) && validNonzeroUUID(object.state.CommandID) {
		// A deletion that wins over an incomplete rotation abandons that
		// rotation. Preserve its replay coordinates before deletion operation
		// coordinates replace them in the public state.
		object.lastOperationID, object.lastCommandID = object.state.OperationID, object.state.CommandID
	}
	object.state.Phase, object.state.OperationID, object.state.CommandID = credentialPhaseDeleting, stored.operationID, stored.commandID
	object.state.ExpectedCommandHash = [32]byte{}
	object.state.ReconcileAfter = stored.expiresAt
	object.state.MarkerSequence, object.state.MarkerDigest = stored.markerSequence, stored.markerDigest
	vault.bump(object)
	capability := vault.newDeletionCapability(object)
	if err := vault.takeFailure(memoryOperationMarkDeleting); err != nil {
		return DeletionCapability{}, err
	}
	return capability, nil
}

func (vault *MemoryCredentialVault) RecoverDeletionCapability(ctx context.Context, recovery DeletionRecovery) (DeletionCapability, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return DeletionCapability{}, err
	}
	object, err := vault.object(recovery.Binding)
	if err != nil || object == nil || object.state.Phase != credentialPhaseDeleting || object.state.Ref != recovery.Ref ||
		object.state.OperationID != recovery.OperationID || object.state.CommandID != recovery.CommandID || recovery.MarkerSequence == 0 ||
		object.state.MarkerSequence != recovery.MarkerSequence || zeroDigest(recovery.MarkerDigest) || object.state.MarkerDigest != recovery.MarkerDigest {
		return DeletionCapability{}, ErrCredentialConflict
	}
	return vault.newDeletionCapability(object), nil
}

func (vault *MemoryCredentialVault) ClaimMaintenance(ctx context.Context, binding CredentialBinding, expected CredentialRotationState, worker string, until time.Time) (CredentialRotationState, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return CredentialRotationState{}, err
	}
	object, err := vault.expected(binding, expected)
	replayName := "claim_maintenance:" + worker + ":" + until.Format(time.RFC3339Nano)
	if err != nil {
		if replay, ok := vault.replayTransition(replayName, binding, expected); ok {
			return replay, nil
		}
		return CredentialRotationState{}, err
	}
	if worker == "" || !validCredentialTime(until) || !until.After(vault.now().UTC()) ||
		(object.state.Phase != credentialPhaseAborting && object.state.Phase != credentialPhaseCandidate && object.state.Phase != credentialPhaseRetiring) {
		return CredentialRotationState{}, ErrCredentialConflict
	}
	if object.maintenanceOwner != "" && object.maintenanceUntil.After(vault.now().UTC()) && object.maintenanceOwner != worker {
		return CredentialRotationState{}, ErrCredentialRotationBusy
	}
	object.maintenanceOwner, object.maintenanceUntil = worker, until
	vault.bump(object)
	state := object.state
	vault.recordTransition(replayName, binding, expected, state, true)
	return state, nil
}

func (vault *MemoryCredentialVault) OpenMaintenanceTokens(ctx context.Context, binding CredentialBinding, expected CredentialRotationState) ([][]byte, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	object, err := vault.expected(binding, expected)
	if err != nil || object.maintenanceOwner == "" || !object.maintenanceUntil.After(vault.now().UTC()) {
		if err != nil {
			return nil, err
		}
		return nil, ErrCredentialConflict
	}
	var tokens [][]byte
	if object.state.Phase == credentialPhaseAborting && object.abortPreserveCurrent {
		if len(object.pending) > 0 {
			tokens = append(tokens, append([]byte(nil), object.pending...))
		}
	} else {
		if len(object.current) > 0 {
			tokens = append(tokens, append([]byte(nil), object.current...))
		}
		if len(object.pending) > 0 {
			tokens = append(tokens, append([]byte(nil), object.pending...))
		}
	}
	if len(tokens) > 2 {
		return nil, ErrCredentialConflict
	}
	return tokens, nil
}

func (vault *MemoryCredentialVault) OpenDeletingTokens(ctx context.Context, capability DeletionCapability) ([][]byte, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	object, _, err := vault.deletionObject(capability)
	if err != nil {
		return nil, err
	}
	return defensiveTokens(object.current, object.pending), nil
}

func (vault *MemoryCredentialVault) EraseDeletingTokens(ctx context.Context, capability DeletionCapability) (DeletionCapability, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return DeletionCapability{}, err
	}
	object, record, err := vault.deletionObject(capability)
	if err != nil {
		if stored := vault.deletions[string(capability.Opaque)]; stored != nil && stored.completed == "erased" && validOpaqueCredentialValue(stored.next) {
			next := DeletionCapability{Opaque: append([]byte(nil), stored.next...)}
			if _, _, nextErr := vault.deletionObject(next); nextErr == nil {
				return next, nil
			}
		}
		return DeletionCapability{}, err
	}
	object.current, object.pending = nil, nil
	object.pendingDigest = [32]byte{}
	object.state.HasCurrent, object.state.HasPending = false, false
	vault.bump(object)
	record.completed = "erased"
	result := vault.newDeletionCapability(object)
	record.next = append([]byte(nil), result.Opaque...)
	if err := vault.takeFailure(memoryOperationEraseDeleting); err != nil {
		return DeletionCapability{}, err
	}
	return result, nil
}

func (vault *MemoryCredentialVault) CompleteMaintenance(ctx context.Context, binding CredentialBinding, expected CredentialRotationState) (CredentialRotationState, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return CredentialRotationState{}, err
	}
	object, err := vault.expected(binding, expected)
	if err != nil {
		if replay, ok := vault.replayTransition("complete_maintenance", binding, expected); ok {
			return replay, nil
		}
		return CredentialRotationState{}, err
	}
	if object.state.Phase != credentialPhaseAborting || object.maintenanceOwner == "" || !object.maintenanceUntil.After(vault.now().UTC()) {
		return CredentialRotationState{}, ErrCredentialConflict
	}
	if object.abortPreserveCurrent {
		object.lastOperationID, object.lastCommandID = object.state.OperationID, object.state.CommandID
		vault.markReplay(binding, object.state.OperationID, object.state.CommandID)
		object.pending = nil
		object.pendingDigest = [32]byte{}
		object.state.HasPending, object.state.Phase = false, credentialPhaseReady
		object.state.OperationID, object.state.CommandID = "", ""
		object.state.ExpectedCommandHash = [32]byte{}
		object.state.ReconcileAfter = time.Time{}
		object.maintenanceOwner, object.maintenanceUntil = "", time.Time{}
		vault.bump(object)
		state := object.state
		vault.recordTransition("complete_maintenance", binding, expected, state, true)
		return state, nil
	}
	if err := vault.storeReplayMarker(object.binding, object.lastOperationID, object.lastCommandID); err != nil {
		return CredentialRotationState{}, err
	}
	delete(vault.objects, binding.ObjectID)
	state := CredentialRotationState{Phase: credentialPhaseAbsent}
	vault.recordTransition("complete_maintenance", binding, expected, state, false)
	return state, nil
}

func (vault *MemoryCredentialVault) Delete(ctx context.Context, capability DeletionCapability) error {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return err
	}
	if record := vault.deletions[string(capability.Opaque)]; record != nil && record.completed == "deleted" {
		if vault.objects[record.binding.ObjectID] == nil {
			return nil
		}
		return ErrCredentialConflict
	}
	object, record, err := vault.deletionObject(capability)
	if err != nil {
		return err
	}
	if object.state.HasCurrent || object.state.HasPending {
		return ErrCredentialConflict
	}
	if err := vault.storeReplayMarker(object.binding, object.lastOperationID, object.lastCommandID); err != nil {
		return err
	}
	delete(vault.objects, object.binding.ObjectID)
	record.completed = "deleted"
	return vault.takeFailure(memoryOperationDelete)
}

func (vault *MemoryCredentialVault) ConfirmAbsent(ctx context.Context, binding CredentialBinding, ref CredentialRef) (bool, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return false, err
	}
	if !validCredentialBinding(binding) || !validCredentialRef(binding, ref) {
		return false, ErrCredentialConflict
	}
	_, exists := vault.objects[binding.ObjectID]
	return !exists, nil
}

func (vault *MemoryCredentialVault) List(ctx context.Context, before time.Time, cursor string, limit int) ([]CredentialMetadata, string, error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return nil, "", err
	}
	if !validCredentialTime(before) || limit <= 0 || limit > 100 {
		return nil, "", ErrCredentialConflict
	}
	keys := make([]string, 0, len(vault.objects))
	for key, object := range vault.objects {
		if (cursor == "" || key > cursor) && !object.createdAt.After(before) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	next := ""
	if len(keys) > limit {
		next, keys = keys[limit-1], keys[:limit]
	}
	result := make([]CredentialMetadata, 0, len(keys))
	for _, key := range keys {
		object := vault.objects[key]
		result = append(result, CredentialMetadata{Binding: object.binding, Ref: object.state.Ref, Phase: object.state.Phase, CreatedAt: object.createdAt})
	}
	return result, next, nil
}

func (vault *MemoryCredentialVault) failAfter(operation string, err error) {
	vault.mu.Lock()
	defer vault.mu.Unlock()
	vault.failures[operation] = err
}

func (vault *MemoryCredentialVault) takeFailure(operation string) error {
	err := vault.failures[operation]
	delete(vault.failures, operation)
	return err
}

func (vault *MemoryCredentialVault) object(binding CredentialBinding) (*memoryCredentialObject, error) {
	if !validCredentialBinding(binding) {
		return nil, ErrCredentialConflict
	}
	object := vault.objects[binding.ObjectID]
	if object != nil && object.binding != binding {
		return nil, ErrCredentialConflict
	}
	return object, nil
}

func (vault *MemoryCredentialVault) expected(binding CredentialBinding, expected CredentialRotationState) (*memoryCredentialObject, error) {
	object, err := vault.object(binding)
	if err != nil || object == nil || object.state != expected || validateCredentialState(binding, expected) != nil {
		if err != nil {
			return nil, err
		}
		return nil, ErrCredentialConflict
	}
	return object, nil
}

func (vault *MemoryCredentialVault) bump(object *memoryCredentialObject) {
	object.revision++
	object.state.Version = strconv.FormatUint(object.revision, 10)
	object.state.HasCurrent, object.state.HasPending = len(object.current) > 0, len(object.pending) > 0
}

func (vault *MemoryCredentialVault) newHandle(kind string) []byte {
	vault.counter++
	mac := hmac.New(sha256.New, vault.capabilityKey[:])
	mac.Write([]byte(kind))
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], vault.counter)
	mac.Write(counter[:])
	return []byte(base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
}

func (vault *MemoryCredentialVault) newDeletionCapability(object *memoryCredentialObject) DeletionCapability {
	handle := vault.newHandle("deletion-capability")
	vault.deletions[string(handle)] = &memoryDeletionCapability{binding: object.binding, ref: object.state.Ref, version: object.state.Version,
		operationID: object.state.OperationID, commandID: object.state.CommandID, markerSequence: object.state.MarkerSequence, markerDigest: object.state.MarkerDigest}
	return DeletionCapability{Opaque: handle}
}

func (vault *MemoryCredentialVault) deletionObject(capability DeletionCapability) (*memoryCredentialObject, *memoryDeletionCapability, error) {
	if !validOpaqueCredentialValue(capability.Opaque) {
		return nil, nil, ErrCredentialConflict
	}
	record := vault.deletions[string(capability.Opaque)]
	if record == nil || record.completed != "" {
		return nil, nil, ErrCredentialConflict
	}
	object := vault.objects[record.binding.ObjectID]
	if object == nil || object.binding != record.binding || object.state.Phase != credentialPhaseDeleting || object.state.Ref != record.ref ||
		object.state.Version != record.version || object.state.OperationID != record.operationID || object.state.CommandID != record.commandID ||
		object.state.MarkerSequence != record.markerSequence || object.state.MarkerDigest != record.markerDigest {
		return nil, nil, ErrCredentialConflict
	}
	return object, record, nil
}

func (vault *MemoryCredentialVault) recordTransition(name string, binding CredentialBinding, input, output CredentialRotationState, present bool) {
	vault.replays[name+":"+binding.ObjectID] = memoryTransitionReplay{binding: binding, input: input, output: output, present: present}
}

func (vault *MemoryCredentialVault) markReplay(binding CredentialBinding, operationID, commandID string) {
	vault.replayMarkers[binding.ObjectID] = memoryCredentialReplayMarker{binding: binding, operationID: operationID, commandID: commandID}
}

func (vault *MemoryCredentialVault) storeReplayMarker(binding CredentialBinding, operationID, commandID string) error {
	if !validCredentialBinding(binding) || !validNonzeroUUID(operationID) || !validNonzeroUUID(commandID) {
		return ErrCredentialConflict
	}
	if existing, exists := vault.replayMarkers[binding.ObjectID]; exists && existing.binding != binding {
		return ErrCredentialConflict
	}
	vault.markReplay(binding, operationID, commandID)
	return vault.takeFailure(memoryOperationStoreReplayMarker)
}

func (vault *MemoryCredentialVault) replayTransition(name string, binding CredentialBinding, input CredentialRotationState) (CredentialRotationState, bool) {
	replay, ok := vault.replays[name+":"+binding.ObjectID]
	if !ok || replay.binding != binding || replay.input != input {
		return CredentialRotationState{}, false
	}
	object := vault.objects[binding.ObjectID]
	if replay.present {
		if object == nil || object.binding != binding || object.state != replay.output {
			return CredentialRotationState{}, false
		}
	} else if object != nil {
		return CredentialRotationState{}, false
	}
	return replay.output, true
}

func NewAppleCredentialBinding(accountID, instanceID string) (CredentialBinding, error) {
	if !validNonzeroUUID(accountID) || !validNonzeroUUID(instanceID) {
		return CredentialBinding{}, ErrInvalidRequest
	}
	digest := sha256.Sum256([]byte("baby-tracker-apple-refresh-object-v1\x00" + instanceID + "\x00" + accountID + "\x00" + AppleCredentialPurpose))
	binding := CredentialBinding{AccountID: accountID, ServerInstanceID: instanceID, Purpose: AppleCredentialPurpose, ObjectID: credentialUUIDv8(digest)}
	return binding, nil
}

// CanonicalAAD returns the deterministic authenticated coordinates consumed by
// credential vaults. Provider CAS versions are deliberately excluded.
func (binding CredentialBinding) CanonicalAAD() ([]byte, error) {
	if !validCredentialBinding(binding) {
		return nil, ErrCredentialConflict
	}
	result := make([]byte, 0, len(credentialBindingAADDomain)+4*4+len(binding.AccountID)+len(binding.ServerInstanceID)+len(binding.Purpose)+len(binding.ObjectID))
	result = append(result, credentialBindingAADDomain...)
	for _, value := range []string{binding.AccountID, binding.ServerInstanceID, binding.Purpose, binding.ObjectID} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(value)))
		result = append(result, length[:]...)
		result = append(result, value...)
	}
	return result, nil
}

func validCredentialBinding(binding CredentialBinding) bool {
	if !validNonzeroUUID(binding.AccountID) || !validNonzeroUUID(binding.ServerInstanceID) || binding.Purpose != AppleCredentialPurpose || !validNonzeroUUID(binding.ObjectID) {
		return false
	}
	expected, err := NewAppleCredentialBinding(binding.AccountID, binding.ServerInstanceID)
	return err == nil && expected == binding
}

func validCredentialRef(binding CredentialBinding, ref CredentialRef) bool {
	return ref.ID == binding.ObjectID && credentialReferencePattern.MatchString(ref.ID) && credentialReferencePattern.MatchString(ref.KeyID)
}

func validateCredentialState(binding CredentialBinding, state CredentialRotationState) error {
	if !validCredentialBinding(binding) || !validCredentialRef(binding, state.Ref) || state.Version == "" {
		return ErrCredentialConflict
	}
	zeroHash, zeroMarker := zeroDigest(state.ExpectedCommandHash), zeroDigest(state.MarkerDigest)
	switch state.Phase {
	case credentialPhaseReady:
		if !state.HasCurrent || state.HasPending || state.OperationID != "" || state.CommandID != "" || !state.ReconcileAfter.IsZero() || !zeroHash || state.MarkerSequence != 0 || !zeroMarker {
			return ErrCredentialConflict
		}
	case credentialPhaseReserved:
		if state.HasPending || !validNonzeroUUID(state.OperationID) || !validNonzeroUUID(state.CommandID) || !validCredentialTime(state.ReconcileAfter) || !zeroHash || state.MarkerSequence != 0 || !zeroMarker {
			return ErrCredentialConflict
		}
	case credentialPhaseCandidate:
		if !state.HasPending || !validNonzeroUUID(state.OperationID) || !validNonzeroUUID(state.CommandID) || !validCredentialTime(state.ReconcileAfter) || !zeroHash || state.MarkerSequence != 0 || !zeroMarker {
			return ErrCredentialConflict
		}
	case credentialPhaseRetiring:
		if state.HasCurrent || !state.HasPending || !validNonzeroUUID(state.OperationID) || !validNonzeroUUID(state.CommandID) || !validCredentialTime(state.ReconcileAfter) || !zeroHash || state.MarkerSequence != 0 || !zeroMarker {
			return ErrCredentialConflict
		}
	case credentialPhasePromoted:
		if !state.HasCurrent || state.HasPending || !validNonzeroUUID(state.OperationID) || !validNonzeroUUID(state.CommandID) || !validCredentialTime(state.ReconcileAfter) || !zeroHash || state.MarkerSequence != 0 || !zeroMarker {
			return ErrCredentialConflict
		}
	case credentialPhaseAdopting:
		if !state.HasCurrent || state.HasPending || !validNonzeroUUID(state.OperationID) || !validNonzeroUUID(state.CommandID) || !validCredentialTime(state.ReconcileAfter) || zeroHash || state.MarkerSequence != 0 || !zeroMarker {
			return ErrCredentialConflict
		}
	case credentialPhaseAborting:
		if !validNonzeroUUID(state.OperationID) || !validNonzeroUUID(state.CommandID) || !validCredentialTime(state.ReconcileAfter) || !zeroHash || state.MarkerSequence != 0 || !zeroMarker {
			return ErrCredentialConflict
		}
	case credentialPhaseDeleting:
		if !validNonzeroUUID(state.OperationID) || !validNonzeroUUID(state.CommandID) || !validCredentialTime(state.ReconcileAfter) || !zeroHash || state.MarkerSequence == 0 || zeroMarker {
			return ErrCredentialConflict
		}
	default:
		return ErrCredentialConflict
	}
	return nil
}

func validPromotionShape(binding CredentialBinding, promotion CredentialPromotion, prepared bool) bool {
	if promotion.Ref.ID == "" || promotion.Version == "" || !validNonzeroUUID(promotion.OperationID) || !validNonzeroUUID(promotion.CommandID) ||
		(binding.ObjectID != "" && promotion.Ref.ID != binding.ObjectID) {
		return false
	}
	if !prepared {
		return zeroDigest(promotion.ExpectedCommandHash) && len(promotion.ConfirmIntent.Opaque) == 0 && promotion.ExpiresAt.IsZero()
	}
	return !zeroDigest(promotion.ExpectedCommandHash) && validPromotionIntent(promotion.ConfirmIntent) && promotion.ExpiresAt == promotion.ConfirmIntent.ExpiresAt()
}

func validPromotionIntent(intent PromotionCASIntent) bool {
	return validOpaqueCredentialValue(intent.Opaque) && validCredentialTime(intent.ExpiresAt())
}
func validDeletionIntent(intent DeletionCASIntent) bool {
	return validOpaqueCredentialValue(intent.Opaque) && validCredentialTime(intent.ExpiresAt())
}
func validOpaqueCredentialValue(value []byte) bool {
	return len(value) > 0 && len(value) <= maxCredentialOpaque
}
func validAppleToken(value string) bool { return len(value) > 0 && len(value) <= maxAppleToken }
func validCredentialTime(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC && value.Year() >= 1 && value.Year() <= 9999
}
func validIntentExpiry(expiresAt, now time.Time) bool {
	return validCredentialTime(expiresAt) && validCredentialTime(now) && expiresAt.After(now) && !expiresAt.After(now.Add(credentialIntentTTL))
}
func zeroDigest(value [32]byte) bool { return value == [32]byte{} }
func contextError(ctx context.Context) error {
	if ctx == nil {
		return ErrUnavailable
	}
	return ctx.Err()
}

func clonePromotionIntent(intent PromotionCASIntent) PromotionCASIntent {
	return PromotionCASIntent{Opaque: append([]byte(nil), intent.Opaque...), expiresAt: intent.expiresAt}
}
func cloneAdoptionCapability(capability AdoptionCapability) AdoptionCapability {
	return AdoptionCapability{Opaque: append([]byte(nil), capability.Opaque...)}
}

func defensiveTokens(values ...[]byte) [][]byte {
	result := make([][]byte, 0, len(values))
	for _, value := range values {
		if len(value) > 0 {
			result = append(result, append([]byte(nil), value...))
		}
	}
	return result
}

func deletionMarker(binding CredentialBinding, ref CredentialRef, operation DeletionOperation, sequence uint64, version string) [32]byte {
	digest := sha256.New()
	for _, value := range []string{"baby-tracker-credential-deletion-marker-v1", binding.AccountID, binding.ServerInstanceID, binding.Purpose,
		binding.ObjectID, ref.ID, ref.KeyID, operation.OperationID, operation.CommandID, version} {
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

func credentialUUIDv8(digest [32]byte) string {
	raw := append([]byte(nil), digest[:16]...)
	raw[6] = raw[6]&0x0f | 0x80
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}
