package recovery

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"babytracker/server/internal/protocol"
	"babytracker/server/internal/security"
)

var (
	ErrEpochConflict = errors.New("epoch_conflict")
	ErrHeadConflict  = errors.New("trusted_head_conflict")
	errStateCorrupt  = errors.New("recovery_state_corrupt")
	uuidPattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type State struct {
	InstanceID  string
	Epoch       uint64
	TrustedHead security.Head
}

type Store struct {
	directory string
	path      string
	fence     security.Fence
	ledger    security.SecurityLedger
	heads     security.LedgerHeadStore
	mu        sync.Mutex
}

type authorityInitializer interface {
	InitializeEmptyUnderFence(context.Context) error
}

func NewVerifiedStore(directory string, fence security.Fence, ledger security.SecurityLedger, heads security.LedgerHeadStore) (*Store, error) {
	if ledger == nil || heads == nil {
		return nil, errors.New("verified recovery store requires ledger and heads")
	}
	store, err := NewStore(directory, fence)
	if err != nil {
		return nil, err
	}
	store.ledger, store.heads = ledger, heads
	return store, nil
}

type diskState struct {
	InstanceID string `json:"instance_id"`
	Epoch      uint64 `json:"epoch"`
	Generation uint64 `json:"generation"`
	Revision   uint64 `json:"revision"`
	EventHash  string `json:"event_hash"`
}

func NewStore(directory string, fence security.Fence) (*Store, error) {
	if directory == "" || fence == nil {
		return nil, errors.New("recovery store requires directory and fence")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("recovery store path must be a directory")
	}
	return &Store{directory: absolute, path: filepath.Join(absolute, "identity.json"), fence: fence}, nil
}

func (store *Store) Initialize(ctx context.Context, state State) error {
	if err := ValidateState(state); err != nil {
		return err
	}
	release, err := store.fence.Write(ctx)
	if err != nil {
		return err
	}
	defer release()
	return store.initializeUnderFence(ctx, state)
}

// InitializeAuthority validates first, then initializes the empty authority
// head and durable recovery identity while holding their shared write fence.
func (store *Store) InitializeAuthority(ctx context.Context, authority authorityInitializer, state State) error {
	if store == nil || store.fence == nil || authority == nil {
		return errStateCorrupt
	}
	if err := ValidateState(state); err != nil {
		return err
	}
	release, err := store.fence.Write(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := authority.InitializeEmptyUnderFence(ctx); err != nil {
		return err
	}
	return store.initializeUnderFence(ctx, state)
}

func (store *Store) initializeUnderFence(ctx context.Context, state State) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	existing, err := store.loadLocked(ctx)
	if err == nil {
		if existing == state {
			return nil
		}
		return errors.New("recovery identity already initialized with different state")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return store.writeLocked(state)
}

func (store *Store) Load(ctx context.Context) (State, error) {
	if store == nil || store.fence == nil {
		return State{}, errStateCorrupt
	}
	release, err := store.fence.Read(ctx)
	if err != nil {
		return State{}, err
	}
	defer release()
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.loadLocked(ctx)
}

// LoadUnderFence is for collaborators already holding store's shared security
// fence. It avoids recursive flock/RWMutex acquisition.
func (store *Store) LoadUnderFence(ctx context.Context) (State, error) {
	if store == nil || store.fence == nil {
		return State{}, errStateCorrupt
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.loadLocked(ctx)
}

func (store *Store) Rotate(ctx context.Context, expectedEpoch uint64, head security.Head) (State, error) {
	if store == nil || store.fence == nil {
		return State{}, errStateCorrupt
	}
	release, err := store.fence.Write(ctx)
	if err != nil {
		return State{}, err
	}
	defer release()
	store.mu.Lock()
	defer store.mu.Unlock()
	state, err := store.loadLocked(ctx)
	if err != nil {
		return State{}, err
	}
	if state.Epoch != expectedEpoch {
		return State{}, ErrEpochConflict
	}
	if store.ledger == nil || store.heads == nil {
		return State{}, ErrHeadConflict
	}
	verified, _, err := security.VerifyCommittedChain(ctx, store.ledger, store.heads, state.TrustedHead)
	if err != nil || verified != head {
		return State{}, ErrHeadConflict
	}
	if state.Epoch >= protocol.MaxSafeInteger {
		return State{}, errors.New("recovery epoch overflow")
	}
	state.Epoch++
	state.TrustedHead = head
	if err := store.writeLocked(state); err != nil {
		return State{}, err
	}
	return state, nil
}

func (store *Store) loadLocked(ctx context.Context) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	file, err := os.Open(store.path)
	if err != nil {
		return State{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 {
		return State{}, errStateCorrupt
	}
	var disk diskState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&disk); err != nil {
		return State{}, errStateCorrupt
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return State{}, errStateCorrupt
	}
	canonical, err := json.Marshal(disk)
	if err != nil || !bytes.Equal(data, append(canonical, '\n')) {
		return State{}, errStateCorrupt
	}
	hash, err := hex.DecodeString(disk.EventHash)
	if err != nil || len(hash) != 32 || hex.EncodeToString(hash) != disk.EventHash {
		return State{}, errStateCorrupt
	}
	var eventHash [32]byte
	copy(eventHash[:], hash)
	state := State{InstanceID: disk.InstanceID, Epoch: disk.Epoch, TrustedHead: security.Head{Generation: disk.Generation, Revision: disk.Revision, EventHash: eventHash}}
	if err := ValidateState(state); err != nil {
		return State{}, errStateCorrupt
	}
	return state, nil
}

func (store *Store) writeLocked(state State) error {
	disk := diskState{InstanceID: state.InstanceID, Epoch: state.Epoch, Generation: state.TrustedHead.Generation,
		Revision: state.TrustedHead.Revision, EventHash: hex.EncodeToString(state.TrustedHead.EventHash[:])}
	encoded, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	temporary := filepath.Join(store.directory, fmt.Sprintf(".identity-%d.tmp", state.Epoch))
	if info, statErr := os.Lstat(temporary); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("recovery identity temporary artifact is invalid")
		}
		if err := os.Remove(temporary); err != nil {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(encoded); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, store.path); err != nil {
		return err
	}
	remove = false
	directory, err := os.Open(store.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// ValidateState checks the complete durable recovery identity without I/O.
func ValidateState(state State) error {
	if !uuidPattern.MatchString(state.InstanceID) || state.InstanceID == "00000000-0000-0000-0000-000000000000" || !protocol.ValidRecoveryEpoch(state.Epoch) {
		return errors.New("recovery identity requires nonzero UUID and epoch")
	}
	if state.TrustedHead.Generation != 1 || state.TrustedHead.Revision > protocol.MaxSafeInteger ||
		(state.TrustedHead.Revision == 0) != (state.TrustedHead.EventHash == [32]byte{}) {
		return errors.New("recovery trusted head is invalid")
	}
	return nil
}
