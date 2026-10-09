package recovery

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"babytracker/server/internal/security"
)

const (
	actorIDKeyFile      = "actor-id-key.json"
	actorIDKeyTemporary = ".actor-id-key.tmp"
	actorIDKeyVersion   = "v1"
	actorIDKeyDomain    = "baby-tracker-actor-id-commitment-v1"
	actorIDKeyMaxBytes  = 512
)

var (
	ErrActorIDKeyMismatch     = errors.New("actor_id_key_mismatch")
	ErrAuthorityChainMismatch = errors.New("authority_chain_mismatch")
	actorCommitmentPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	actorPepperPattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type actorIDKeyDisk struct {
	Version    string `json:"version"`
	InstanceID string `json:"instance_id"`
	Commitment string `json:"commitment"`
}

// ActorIDCommitment binds the permanent actor-ID key to one canonical server
// instance without exposing the key itself.
func ActorIDCommitment(key [32]byte, instanceID string) ([32]byte, error) {
	var zero [32]byte
	if key == zero || !uuidPattern.MatchString(instanceID) || instanceID == "00000000-0000-0000-0000-000000000000" {
		return zero, ErrActorIDKeyMismatch
	}
	mac := hmac.New(sha256.New, key[:])
	for _, field := range []string{actorIDKeyDomain, instanceID} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		_, _ = mac.Write(length[:])
		_, _ = mac.Write([]byte(field))
	}
	var commitment [32]byte
	copy(commitment[:], mac.Sum(nil))
	return commitment, nil
}

// InitializeAuthorityWithActorIDKey creates the empty authority, unchanged
// recovery identity, and actor-key sidecar while holding their shared fence.
func (store *Store) InitializeAuthorityWithActorIDKey(ctx context.Context, authority authorityInitializer, state State, key [32]byte) error {
	if store == nil || store.fence == nil || store.ledger == nil || store.heads == nil || authority == nil {
		return errStateCorrupt
	}
	if err := ValidateState(state); err != nil {
		return err
	}
	if _, err := ActorIDCommitment(key, state.InstanceID); err != nil {
		return err
	}
	release, err := store.fence.Write(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := store.removeActorIDKeyTemporaryUnderFence(); err != nil {
		return err
	}

	existing, identityErr := store.LoadUnderFence(ctx)
	identityExists := identityErr == nil
	if identityErr != nil && !errors.Is(identityErr, os.ErrNotExist) {
		return identityErr
	}
	actorPath := filepath.Join(store.directory, actorIDKeyFile)
	_, actorErr := os.Lstat(actorPath)
	actorExists := actorErr == nil
	if actorErr != nil && !os.IsNotExist(actorErr) {
		return ErrActorIDKeyMismatch
	}

	if identityExists {
		if existing != state || !actorExists {
			return ErrActorIDKeyMismatch
		}
		if err := store.validateActorIDKeyUnderFence(ctx, state.InstanceID, key); err != nil {
			return err
		}
		return store.verifyEmptyAuthorityUnderFence(ctx, state)
	}
	if actorExists {
		if err := store.validateActorIDKeyUnderFence(ctx, state.InstanceID, key); err != nil {
			return err
		}
		if err := store.verifyEmptyAuthorityUnderFence(ctx, state); err != nil {
			return err
		}
		return store.initializeUnderFence(ctx, state)
	}

	if err := authority.InitializeEmptyUnderFence(ctx); err != nil {
		return err
	}
	if err := store.initializeActorIDKeyUnderFence(ctx, state.InstanceID, key); err != nil {
		return err
	}
	return store.initializeUnderFence(ctx, state)
}

func (store *Store) verifyEmptyAuthorityUnderFence(ctx context.Context, state State) error {
	if state.TrustedHead != (security.Head{Generation: 1}) {
		return ErrActorIDKeyMismatch
	}
	head, authorityState, err := security.VerifyCommittedChain(ctx, store.ledger, store.heads, state.TrustedHead)
	if err != nil || head != state.TrustedHead || authorityState.Head() != state.TrustedHead {
		return ErrActorIDKeyMismatch
	}
	return nil
}

func (store *Store) ValidateActorIDKey(ctx context.Context, instanceID string, key [32]byte) error {
	if store == nil || store.fence == nil {
		return ErrActorIDKeyMismatch
	}
	release, err := store.fence.Read(ctx)
	if err != nil {
		return err
	}
	defer release()
	return store.validateActorIDKeyUnderFence(ctx, instanceID, key)
}

// VerifyAuthorityWithActorIDKey verifies the actor-key commitment and the
// complete committed authority chain against the same durable recovery state
// while holding one shared fence.
func (store *Store) VerifyAuthorityWithActorIDKey(ctx context.Context, key [32]byte) (State, security.Head, error) {
	if store == nil || store.fence == nil || store.ledger == nil || store.heads == nil {
		return State{}, security.Head{}, ErrActorIDKeyMismatch
	}
	release, err := store.fence.Read(ctx)
	if err != nil {
		return State{}, security.Head{}, err
	}
	defer release()
	state, err := store.LoadUnderFence(ctx)
	if err != nil {
		return State{}, security.Head{}, err
	}
	if err := store.validateActorIDKeyUnderFence(ctx, state.InstanceID, key); err != nil {
		return State{}, security.Head{}, err
	}
	head, _, err := security.VerifyCommittedChain(ctx, store.ledger, store.heads, state.TrustedHead)
	if err != nil {
		if errors.Is(err, security.ErrLedgerCorrupt) || errors.Is(err, security.ErrTrustedHeadRequired) {
			return State{}, security.Head{}, fmt.Errorf("%w: %w", ErrAuthorityChainMismatch, err)
		}
		return State{}, security.Head{}, err
	}
	return state, head, nil
}

// BindActorIDKey accepts only an already initialized, verified genesis
// authority. It is the explicit upgrade path for installations created before
// the actor-key sidecar existed.
func (store *Store) BindActorIDKey(ctx context.Context, instanceID, confirmedInstanceID string, key [32]byte) error {
	if store == nil || store.fence == nil || store.ledger == nil || store.heads == nil ||
		instanceID == "" || confirmedInstanceID != instanceID {
		return ErrActorIDKeyMismatch
	}
	if _, err := ActorIDCommitment(key, instanceID); err != nil {
		return err
	}
	release, err := store.fence.Write(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := store.removeActorIDKeyTemporaryUnderFence(); err != nil {
		return err
	}

	state, err := store.LoadUnderFence(ctx)
	if err != nil || state.InstanceID != instanceID || state.TrustedHead != (security.Head{Generation: 1}) {
		return ErrActorIDKeyMismatch
	}
	head, authorityState, err := security.VerifyCommittedChain(ctx, store.ledger, store.heads, state.TrustedHead)
	if err != nil || head != state.TrustedHead || authorityState.Head() != state.TrustedHead {
		return ErrActorIDKeyMismatch
	}
	return store.initializeActorIDKeyUnderFence(ctx, instanceID, key)
}

func (store *Store) initializeActorIDKeyUnderFence(ctx context.Context, instanceID string, key [32]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	commitment, err := ActorIDCommitment(key, instanceID)
	if err != nil {
		return err
	}
	disk := actorIDKeyDisk{Version: actorIDKeyVersion, InstanceID: instanceID, Commitment: hex.EncodeToString(commitment[:])}
	encoded, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	path := filepath.Join(store.directory, actorIDKeyFile)
	if _, err := os.Lstat(path); err == nil {
		return store.validateActorIDKeyUnderFence(ctx, instanceID, key)
	} else if !os.IsNotExist(err) {
		return ErrActorIDKeyMismatch
	}
	temporary := filepath.Join(store.directory, actorIDKeyTemporary)
	if err := store.removeActorIDKeyTemporaryUnderFence(); err != nil {
		return err
	}

	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		_ = file.Close()
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(encoded); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporary, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return store.validateActorIDKeyUnderFence(ctx, instanceID, key)
		}
		return err
	}
	if err := os.Remove(temporary); err != nil {
		return err
	}
	removeTemporary = false
	directory, err := os.Open(store.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (store *Store) removeActorIDKeyTemporaryUnderFence() error {
	temporary := filepath.Join(store.directory, actorIDKeyTemporary)
	info, err := os.Lstat(temporary)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ErrActorIDKeyMismatch
	}
	if err := os.Remove(temporary); err != nil {
		return err
	}
	directory, err := os.Open(store.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (store *Store) validateActorIDKeyUnderFence(ctx context.Context, instanceID string, key [32]byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	expected, err := ActorIDCommitment(key, instanceID)
	if err != nil {
		return ErrActorIDKeyMismatch
	}
	path := filepath.Join(store.directory, actorIDKeyFile)
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ErrActorIDKeyMismatch
	}
	defer file.Close()
	openedInfo, openErr := file.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if openErr != nil || pathErr != nil || !openedInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() ||
		openedInfo.Mode().Perm() != 0o600 || openedInfo.Size() > actorIDKeyMaxBytes || !os.SameFile(openedInfo, pathInfo) {
		return ErrActorIDKeyMismatch
	}
	data, err := io.ReadAll(io.LimitReader(file, actorIDKeyMaxBytes+1))
	if err != nil || len(data) > actorIDKeyMaxBytes {
		return ErrActorIDKeyMismatch
	}
	var disk actorIDKeyDisk
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&disk); err != nil {
		return ErrActorIDKeyMismatch
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrActorIDKeyMismatch
	}
	canonical, err := json.Marshal(disk)
	if err != nil || !bytes.Equal(data, append(canonical, '\n')) || disk.Version != actorIDKeyVersion ||
		disk.InstanceID != instanceID || !actorCommitmentPattern.MatchString(disk.Commitment) {
		return ErrActorIDKeyMismatch
	}
	actual, err := hex.DecodeString(disk.Commitment)
	if err != nil || len(actual) != len(expected) || subtle.ConstantTimeCompare(actual, expected[:]) != 1 {
		return ErrActorIDKeyMismatch
	}
	return nil
}

// LoadActorIDPepperFile applies the strict secret-file rules used by the
// security administration command without permitting an environment value.
func LoadActorIDPepperFile(path string) ([32]byte, error) {
	var key [32]byte
	if path == "" || !filepath.IsAbs(path) {
		return key, ErrActorIDKeyMismatch
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return key, ErrActorIDKeyMismatch
	}
	defer file.Close()
	openedInfo, openErr := file.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if openErr != nil || pathErr != nil || !openedInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() ||
		openedInfo.Size() > 66 || !os.SameFile(openedInfo, pathInfo) {
		return key, ErrActorIDKeyMismatch
	}
	encoded, err := io.ReadAll(io.LimitReader(file, 67))
	if err != nil || len(encoded) > 66 {
		return key, ErrActorIDKeyMismatch
	}
	if len(encoded) == 65 && encoded[64] == '\n' {
		encoded = encoded[:64]
	} else if len(encoded) == 66 && encoded[64] == '\r' && encoded[65] == '\n' {
		encoded = encoded[:64]
	}
	if !actorPepperPattern.Match(encoded) {
		return key, ErrActorIDKeyMismatch
	}
	decoded, err := hex.DecodeString(string(encoded))
	if err != nil || len(decoded) != len(key) {
		return key, ErrActorIDKeyMismatch
	}
	copy(key[:], decoded)
	return key, nil
}
