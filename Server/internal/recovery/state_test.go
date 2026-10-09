package recovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
)

type testFence struct{ mu sync.RWMutex }

type emptyLedger struct{}

func (emptyLedger) AppendCandidate(context.Context, security.Candidate) error { return nil }
func (emptyLedger) ReadCommitted(context.Context, security.Head) ([]security.Candidate, error) {
	return nil, nil
}
func (emptyLedger) WalkCommitted(_ context.Context, head security.Head, visit func(security.Candidate) error) error {
	if head != (security.Head{Generation: 1}) || visit == nil {
		return security.ErrLedgerCorrupt
	}
	return nil
}

type blockingAuthorityInitializer struct {
	store       localstore.Store
	initialized chan struct{}
	resume      chan struct{}
}

func (initializer blockingAuthorityInitializer) InitializeEmptyUnderFence(ctx context.Context) error {
	if err := initializer.store.InitializeEmptyUnderFence(ctx); err != nil {
		return err
	}
	close(initializer.initialized)
	<-initializer.resume
	return nil
}

func TestInitializeAuthorityHoldsOneExclusiveFenceAcrossHeadAndIdentity(t *testing.T) {
	root := t.TempDir()
	stores, err := localstore.Open(root+"/ledger", root+"/heads")
	if err != nil {
		t.Fatal(err)
	}
	durable, err := NewStore(root+"/heads", stores.Fence)
	if err != nil {
		t.Fatal(err)
	}
	state := State{InstanceID: "123e4567-e89b-42d3-a456-426614174000", Epoch: 1, TrustedHead: security.Head{Generation: 1}}
	initializer := blockingAuthorityInitializer{store: stores, initialized: make(chan struct{}), resume: make(chan struct{})}
	initResult := make(chan error, 1)
	go func() { initResult <- durable.InitializeAuthority(context.Background(), initializer, state) }()
	<-initializer.initialized

	writerAcquired := make(chan struct{})
	go func() {
		release, err := stores.Fence.Write(context.Background())
		if err == nil {
			close(writerAcquired)
			release()
		}
	}()
	select {
	case <-writerAcquired:
		t.Fatal("authority writer entered between genesis and durable identity")
	case <-time.After(50 * time.Millisecond):
	}
	close(initializer.resume)
	if err := <-initResult; err != nil {
		t.Fatal(err)
	}
	select {
	case <-writerAcquired:
	case <-time.After(time.Second):
		t.Fatal("authority writer did not resume after initialization")
	}
	if loaded, err := durable.Load(t.Context()); err != nil || loaded != state {
		t.Fatalf("state=%+v err=%v", loaded, err)
	}
}

func TestRotationSerializesWithAuthorityWriter(t *testing.T) {
	t.Run("authority commit first", func(t *testing.T) {
		stores, store, initial, candidate := newRotationRace(t, nil)
		writerAcquired, writerProceed, writerResult := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			writerResult <- commitAuthority(context.Background(), stores, candidate, writerAcquired, writerProceed)
		}()
		<-writerAcquired
		rotationResult := make(chan error, 1)
		go func() {
			_, err := store.Rotate(context.Background(), 1, initial.TrustedHead)
			rotationResult <- err
		}()
		close(writerProceed)
		if err := <-writerResult; err != nil {
			t.Fatal(err)
		}
		if err := <-rotationResult; !errors.Is(err, ErrHeadConflict) {
			t.Fatalf("rotation did not observe committed authority head: %v", err)
		}
		if persisted, err := store.Load(t.Context()); err != nil || persisted != initial {
			t.Fatalf("losing rotation changed state=%+v err=%v", persisted, err)
		}
	})

	t.Run("rotation first", func(t *testing.T) {
		rotationAcquired, rotationProceed := make(chan struct{}), make(chan struct{})
		stores, store, initial, candidate := newRotationRace(t, func(ctx context.Context, inner security.Fence) security.Fence {
			return &rotationBarrierFence{Fence: inner, acquired: rotationAcquired, proceed: rotationProceed}
		})
		rotationResult := make(chan error, 1)
		go func() {
			_, err := store.Rotate(context.WithValue(context.Background(), rotationActorKey{}, true), 1, initial.TrustedHead)
			rotationResult <- err
		}()
		<-rotationAcquired
		writerResult := make(chan error, 1)
		go func() { writerResult <- commitAuthority(context.Background(), stores, candidate, nil, nil) }()
		close(rotationProceed)
		if err := <-rotationResult; err != nil {
			t.Fatalf("first rotation failed: %v", err)
		}
		if err := <-writerResult; err != nil {
			t.Fatalf("serialized authority commit failed: %v", err)
		}
		persisted, err := store.Load(t.Context())
		if err != nil || persisted.Epoch != 2 || persisted.TrustedHead != initial.TrustedHead {
			t.Fatalf("rotation-first state=%+v err=%v", persisted, err)
		}
	})
}

type rotationActorKey struct{}

type rotationBarrierFence struct {
	security.Fence
	acquired chan struct{}
	proceed  chan struct{}
}

func (fence *rotationBarrierFence) Write(ctx context.Context) (func(), error) {
	release, err := fence.Fence.Write(ctx)
	if err == nil && ctx.Value(rotationActorKey{}) != nil {
		close(fence.acquired)
		<-fence.proceed
	}
	return release, err
}

func newRotationRace(t *testing.T, wrap func(context.Context, security.Fence) security.Fence) (localstore.Store, *Store, State, security.Candidate) {
	t.Helper()
	root := t.TempDir()
	stores, err := localstore.Open(root+"/ledger", root+"/heads")
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.InitializeEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	fence := stores.Fence
	if wrap != nil {
		fence = wrap(t.Context(), fence)
	}
	store, err := NewVerifiedStore(root+"/heads", fence, stores.Ledger, stores.Heads)
	if err != nil {
		t.Fatal(err)
	}
	initial := State{InstanceID: "123e4567-e89b-42d3-a456-426614174000", Epoch: 1, TrustedHead: security.Head{Generation: 1}}
	if err := store.Initialize(t.Context(), initial); err != nil {
		t.Fatal(err)
	}
	event := security.Event{Version: "v1", Generation: 1, Revision: 1,
		PreviousHash: "0000000000000000000000000000000000000000000000000000000000000000",
		CommandID:    "30000000-0000-4000-8000-000000000001", CommandHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActorAccountID: "10000000-0000-4000-8000-000000000001", Kind: "account_registered", At: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano),
		Effects: []security.Effect{{Kind: "account_registered", AccountID: "10000000-0000-4000-8000-000000000001", IdentityKey: "opaque", Status: "active"}}}
	encoded, err := security.EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	return stores, store, initial, security.Candidate{CommandID: event.CommandID, Prior: initial.TrustedHead, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
}

func commitAuthority(ctx context.Context, stores localstore.Store, candidate security.Candidate, acquired chan<- struct{}, proceed <-chan struct{}) error {
	release, err := stores.Fence.Write(ctx)
	if err != nil {
		return err
	}
	defer release()
	if acquired != nil {
		close(acquired)
	}
	if proceed != nil {
		<-proceed
	}
	if err := stores.Ledger.AppendCandidate(ctx, candidate); err != nil {
		return err
	}
	next := security.Head{Generation: candidate.Prior.Generation, Revision: candidate.Prior.Revision + 1, EventHash: candidate.EventHash}
	swapped, err := stores.Heads.CompareAndSwap(ctx, candidate.Prior, next)
	if err != nil {
		return err
	}
	if !swapped {
		return errors.New("authority head conflict")
	}
	return nil
}

type genesisHeads struct{}

func (genesisHeads) Load(context.Context) (security.Head, error) {
	return security.Head{Generation: 1}, nil
}
func (genesisHeads) CompareAndSwap(context.Context, security.Head, security.Head) (bool, error) {
	return false, nil
}

func newGenesisVerifiedStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewVerifiedStore(t.TempDir(), &testFence{}, emptyLedger{}, genesisHeads{})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func (fence *testFence) Read(context.Context) (func(), error) {
	fence.mu.RLock()
	return fence.mu.RUnlock, nil
}

func (fence *testFence) Write(context.Context) (func(), error) {
	fence.mu.Lock()
	return fence.mu.Unlock, nil
}

func TestEpochRotationIsMonotonic(t *testing.T) {
	store := newGenesisVerifiedStore(t)
	initial := State{InstanceID: "123e4567-e89b-42d3-a456-426614174000", Epoch: 1, TrustedHead: security.Head{Generation: 1}}
	if err := store.Initialize(t.Context(), initial); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := store.Rotate(t.Context(), 1, initial.TrustedHead)
			results <- err
		}()
	}
	succeeded, conflicted := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrEpochConflict):
			conflicted++
		default:
			t.Fatalf("unexpected rotation error: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("rotations succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	state, err := store.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Epoch != 2 || state.InstanceID != initial.InstanceID || state.TrustedHead != initial.TrustedHead {
		t.Fatalf("rotated state=%+v", state)
	}
}

func TestPersistentIdentityRejectsOverwriteAndCorruption(t *testing.T) {
	store, err := NewStore(t.TempDir(), &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	state := State{InstanceID: "123e4567-e89b-42d3-a456-426614174000", Epoch: 1, TrustedHead: security.Head{Generation: 1}}
	if err := store.Initialize(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	if err := store.Initialize(t.Context(), state); err != nil {
		t.Fatalf("exact initialization is not idempotent: %v", err)
	}
	different := state
	different.InstanceID = "123e4567-e89b-42d3-a456-426614174001"
	if err := store.Initialize(t.Context(), different); err == nil {
		t.Fatal("durable identity was overwritten")
	}
	fabricated := security.Head{Generation: 1, Revision: 1, EventHash: [32]byte{1}}
	if _, err := store.Rotate(t.Context(), 1, fabricated); !errors.Is(err, ErrHeadConflict) {
		t.Fatalf("fabricated head error=%v", err)
	}
	persisted, err := store.Load(t.Context())
	if err != nil || persisted != state {
		t.Fatalf("fabricated rotation changed state=%+v error=%v", persisted, err)
	}
}

func TestInitializeRecoversInterruptedExactIdentityTemp(t *testing.T) {
	directory := t.TempDir()
	store, err := NewStore(directory, &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	state := State{InstanceID: "123e4567-e89b-42d3-a456-426614174000", Epoch: 1, TrustedHead: security.Head{Generation: 1}}
	temporary := filepath.Join(directory, ".identity-1.tmp")
	if err := os.WriteFile(temporary, []byte("interrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Initialize(t.Context(), state); err != nil {
		t.Fatalf("exact interrupted identity temp was not recoverable: %v", err)
	}
	if got, err := store.Load(t.Context()); err != nil || got != state {
		t.Fatalf("state=%+v err=%v", got, err)
	}
}

func TestRecoveryEpochSafeIntegerBounds(t *testing.T) {
	state := State{InstanceID: "123e4567-e89b-42d3-a456-426614174000", Epoch: 9007199254740990, TrustedHead: security.Head{Generation: 1}}
	store := newGenesisVerifiedStore(t)
	if err := store.Initialize(t.Context(), state); err != nil {
		t.Fatalf("maximum-minus-one safe epoch rejected: %v", err)
	}
	rotated, err := store.Rotate(t.Context(), state.Epoch, state.TrustedHead)
	if err != nil || rotated.Epoch != 9007199254740991 {
		t.Fatalf("rotation to maximum state=%+v err=%v", rotated, err)
	}
	if _, err := store.Rotate(t.Context(), rotated.Epoch, rotated.TrustedHead); err == nil {
		t.Fatal("maximum safe epoch rotated out of range")
	}
	if persisted, err := store.Load(t.Context()); err != nil || persisted != rotated {
		t.Fatalf("overflow attempt changed state=%+v err=%v", persisted, err)
	}
	overflow := rotated
	overflow.Epoch++
	overflowStore, err := NewStore(t.TempDir(), &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	if err := overflowStore.Initialize(t.Context(), overflow); err == nil {
		t.Fatal("unsafe integer epoch initialized")
	}
	writeDiskState(t, overflowStore, overflow)
	if _, err := overflowStore.Load(t.Context()); err == nil {
		t.Fatal("unsafe integer epoch loaded")
	}
	maxRevision := State{InstanceID: state.InstanceID, Epoch: 1, TrustedHead: security.Head{Generation: 1, Revision: 9007199254740991, EventHash: [32]byte{1}}}
	maxStore, _ := NewStore(t.TempDir(), &testFence{})
	if err := maxStore.Initialize(t.Context(), maxRevision); err != nil {
		t.Fatalf("maximum safe revision rejected: %v", err)
	}
	if loaded, err := maxStore.Load(t.Context()); err != nil || loaded != maxRevision {
		t.Fatalf("maximum safe revision load=%+v err=%v", loaded, err)
	}
	maxRevision.TrustedHead.Revision++
	overflowRevisionStore, _ := NewStore(t.TempDir(), &testFence{})
	if err := overflowRevisionStore.Initialize(t.Context(), maxRevision); err == nil {
		t.Fatal("unsafe integer trusted revision initialized")
	}
	writeDiskState(t, overflowRevisionStore, maxRevision)
	if _, err := overflowRevisionStore.Load(t.Context()); err == nil {
		t.Fatal("unsafe integer trusted revision loaded")
	}
}

func writeDiskState(t *testing.T, store *Store, state State) {
	t.Helper()
	disk := diskState{InstanceID: state.InstanceID, Epoch: state.Epoch, Generation: state.TrustedHead.Generation,
		Revision: state.TrustedHead.Revision, EventHash: hex.EncodeToString(state.TrustedHead.EventHash[:])}
	encoded, err := json.Marshal(disk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}
