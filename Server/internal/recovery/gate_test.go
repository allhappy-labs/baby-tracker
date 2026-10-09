package recovery

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"babytracker/server/internal/security"
)

type fakeAuthority struct {
	head       security.Head
	checkError error
	reconciled bool
}

func (authority *fakeAuthority) Check(context.Context) error { return authority.checkError }
func (authority *fakeAuthority) Head(context.Context) (security.Head, error) {
	return authority.head, nil
}
func (authority *fakeAuthority) IsDescendant(_ context.Context, trusted security.Head) (bool, error) {
	return trusted == authority.head || (trusted == (security.Head{Generation: 1}) && authority.head.Generation == 1), nil
}
func (authority *fakeAuthority) Reconcile(context.Context) error {
	authority.reconciled = true
	authority.checkError = nil
	return nil
}
func (authority *fakeAuthority) ReconcileTrusted(_ context.Context, trusted security.Head) (security.Head, error) {
	if ok, _ := authority.IsDescendant(context.Background(), trusted); !ok {
		return security.Head{}, security.ErrTrustedHeadRequired
	}
	return authority.head, authority.Reconcile(context.Background())
}

type fakeSchema struct{ err error }

func (schema fakeSchema) Check(context.Context) error { return schema.err }

type fakeMirror struct {
	state  State
	exists bool
}

var gateTestActorIDKey = [32]byte{1, 2, 3, 4}

func bindGateTestActorIDKey(t *testing.T, store *Store, instanceID string) {
	t.Helper()
	release, err := store.fence.Write(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := store.initializeActorIDKeyUnderFence(t.Context(), instanceID, gateTestActorIDKey); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorAuthorityUnderFenceDistinguishesProjectionLagFromRecoveryDrift(t *testing.T) {
	newFixture := func(t *testing.T) (*Store, *Gate, *fakeAuthority, *fakeMirror, State) {
		t.Helper()
		store, err := NewStore(t.TempDir(), &testFence{})
		if err != nil {
			t.Fatal(err)
		}
		state := State{InstanceID: "123e4567-e89b-42d3-a456-426614174000", Epoch: 1, TrustedHead: security.Head{Generation: 1}}
		if err := store.Initialize(t.Context(), state); err != nil {
			t.Fatal(err)
		}
		bindGateTestActorIDKey(t, store, state.InstanceID)
		authority := &fakeAuthority{head: state.TrustedHead}
		mirror := &fakeMirror{state: state, exists: true}
		gate, err := NewGate(Options{Store: store, Security: authority, Schema: fakeSchema{}, Mirror: mirror,
			ExpectedInstanceID: state.InstanceID, ExpectedEpoch: state.Epoch, ActorIDKey: gateTestActorIDKey})
		if err != nil {
			t.Fatal(err)
		}
		if err := gate.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
		authority.checkError = errors.New("projection behind")
		return store, gate, authority, mirror, state
	}
	validate := func(t *testing.T, store *Store, gate *Gate, head security.Head) error {
		t.Helper()
		release, err := store.fence.Write(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		return gate.ValidateOperatorAuthorityUnderFence(t.Context(), head)
	}

	t.Run("projection lag", func(t *testing.T) {
		store, gate, _, _, state := newFixture(t)
		if err := validate(t, store, gate, state.TrustedHead); err != nil {
			t.Fatalf("projection-only lag rejected: %v", err)
		}
	})
	t.Run("recovery gate quarantine", func(t *testing.T) {
		store, gate, _, _, state := newFixture(t)
		gate.setQuarantine(true)
		if err := validate(t, store, gate, state.TrustedHead); !errors.Is(err, ErrQuarantine) {
			t.Fatalf("quarantined recovery gate accepted: %v", err)
		}
	})
	t.Run("epoch rotation", func(t *testing.T) {
		store, gate, _, _, state := newFixture(t)
		release, _ := store.fence.Write(t.Context())
		store.mu.Lock()
		state.Epoch++
		err := store.writeLocked(state)
		store.mu.Unlock()
		release()
		if err != nil {
			t.Fatal(err)
		}
		if err := validate(t, store, gate, state.TrustedHead); !errors.Is(err, ErrQuarantine) {
			t.Fatalf("rotated epoch accepted: %v", err)
		}
	})
	t.Run("manifest mismatch", func(t *testing.T) {
		store, gate, _, mirror, state := newFixture(t)
		state.TrustedHead = security.Head{Generation: 1, Revision: 1, EventHash: sha256.Sum256([]byte("untrusted replacement"))}
		release, _ := store.fence.Write(t.Context())
		store.mu.Lock()
		err := store.writeLocked(state)
		store.mu.Unlock()
		release()
		if err != nil {
			t.Fatal(err)
		}
		mirror.state = state
		if err := validate(t, store, gate, security.Head{Generation: 1}); !errors.Is(err, ErrQuarantine) {
			t.Fatalf("untrusted manifest accepted: %v", err)
		}
	})
	t.Run("database mirror ahead", func(t *testing.T) {
		store, gate, _, mirror, state := newFixture(t)
		mirror.state.TrustedHead = security.Head{Generation: 1, Revision: 1, EventHash: sha256.Sum256([]byte("database ahead"))}
		if err := validate(t, store, gate, state.TrustedHead); !errors.Is(err, ErrQuarantine) {
			t.Fatalf("database-ahead mirror accepted: %v", err)
		}
	})
}

func TestGateRequiresActorKeyAndRecoversAfterSidecarRestore(t *testing.T) {
	directory := t.TempDir()
	store, err := NewStore(directory, &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	state := State{InstanceID: "123e4567-e89b-42d3-a456-426614174000", Epoch: 1, TrustedHead: security.Head{Generation: 1}}
	if err := store.Initialize(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	bindGateTestActorIDKey(t, store, state.InstanceID)
	authority := &fakeAuthority{head: state.TrustedHead}
	gate, err := NewGate(Options{Store: store, Security: authority, Schema: fakeSchema{}, Mirror: &fakeMirror{},
		ExpectedInstanceID: state.InstanceID, ExpectedEpoch: state.Epoch, ActorIDKey: gateTestActorIDKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := gate.Check(t.Context()); err != nil {
		t.Fatalf("valid actor key was not ready: %v", err)
	}

	path := filepath.Join(directory, actorIDKeyFile)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backup := path + ".original"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":"v1","instance_id":"123e4567-e89b-42d3-a456-426614174000","commitment":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gate.Check(t.Context()); !errors.Is(err, ErrActorIDKeyMismatch) || !errors.Is(err, ErrQuarantine) {
		t.Fatalf("replaced actor sidecar readiness error=%v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if err := gate.Check(t.Context()); err != nil {
		t.Fatalf("restored actor sidecar was not ready: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := gate.Check(t.Context()); !errors.Is(err, ErrActorIDKeyMismatch) {
		t.Fatalf("missing actor sidecar readiness error=%v", err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gate.Check(t.Context()); !errors.Is(err, ErrActorIDKeyMismatch) {
		t.Fatalf("wrong-mode actor sidecar readiness error=%v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	replacementKey := gateTestActorIDKey
	replacementKey[0] ^= 0xff
	replacementGate, err := NewGate(Options{Store: store, Security: authority, Schema: fakeSchema{}, Mirror: &fakeMirror{state: state, exists: true},
		ExpectedInstanceID: state.InstanceID, ExpectedEpoch: state.Epoch, ActorIDKey: replacementKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := replacementGate.Check(t.Context()); !errors.Is(err, ErrActorIDKeyMismatch) {
		t.Fatalf("replacement actor pepper readiness error=%v", err)
	}
	if _, err := NewGate(Options{Store: store, Security: authority, Schema: fakeSchema{}, Mirror: &fakeMirror{},
		ExpectedInstanceID: state.InstanceID, ExpectedEpoch: state.Epoch}); err == nil {
		t.Fatal("recovery gate accepted a missing actor ID key")
	}
}

func (mirror *fakeMirror) Load(context.Context) (State, bool, error) {
	return mirror.state, mirror.exists, nil
}

func (mirror *fakeMirror) Update(_ context.Context, state State) error {
	mirror.state = state
	mirror.exists = true
	return nil
}

func TestRecoveryManifestMismatchKeepsQuarantine(t *testing.T) {
	store, err := NewStore(t.TempDir(), &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	trusted := security.Head{Generation: 1, Revision: 99, EventHash: sha256.Sum256([]byte("different head"))}
	state := State{InstanceID: "123e4567-e89b-42d3-a456-426614174000", Epoch: 1, TrustedHead: trusted}
	if err := store.Initialize(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	bindGateTestActorIDKey(t, store, state.InstanceID)
	authority := &fakeAuthority{head: security.Head{Generation: 1}}
	gate, err := NewGate(Options{Store: store, Security: authority, Schema: fakeSchema{}, Mirror: &fakeMirror{}, ExpectedInstanceID: state.InstanceID, ExpectedEpoch: 1, ActorIDKey: gateTestActorIDKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Check(t.Context()); !errors.Is(err, ErrQuarantine) {
		t.Fatalf("untrusted restore error=%v", err)
	}
}

func TestDatabaseAheadMirrorKeepsQuarantineAndIsNotOverwritten(t *testing.T) {
	store, err := NewStore(t.TempDir(), &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	state := State{InstanceID: "123e4567-e89b-42d3-a456-426614174000", Epoch: 1, TrustedHead: security.Head{Generation: 1}}
	if err := store.Initialize(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	bindGateTestActorIDKey(t, store, state.InstanceID)
	ahead := state
	ahead.TrustedHead = security.Head{Generation: 1, Revision: 1, EventHash: sha256.Sum256([]byte("unexplained database head"))}
	mirror := &fakeMirror{state: ahead, exists: true}
	gate, err := NewGate(Options{Store: store, Security: &fakeAuthority{head: state.TrustedHead}, Schema: fakeSchema{}, Mirror: mirror,
		ExpectedInstanceID: state.InstanceID, ExpectedEpoch: state.Epoch, ActorIDKey: gateTestActorIDKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Reconcile(t.Context()); !errors.Is(err, ErrQuarantine) {
		t.Fatalf("database-ahead mirror reconciled: %v", err)
	}
	if mirror.state != ahead {
		t.Fatalf("database-ahead mirror overwritten: %+v", mirror.state)
	}
	if err := gate.Check(t.Context()); !errors.Is(err, ErrQuarantine) {
		t.Fatalf("database-ahead mirror cleared quarantine: %v", err)
	}
}

func TestOlderDatabaseStaysQuarantinedUntilReplay(t *testing.T) {
	store, err := NewStore(t.TempDir(), &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	head := security.Head{Generation: 1, Revision: 1, EventHash: sha256.Sum256([]byte("latest"))}
	state := State{InstanceID: "123e4567-e89b-42d3-a456-426614174000", Epoch: 2, TrustedHead: head}
	if err := store.Initialize(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	bindGateTestActorIDKey(t, store, state.InstanceID)
	authority := &fakeAuthority{head: head, checkError: errors.New("projection behind")}
	mirror := &fakeMirror{}
	gate, err := NewGate(Options{Store: store, Security: authority, Schema: fakeSchema{}, Mirror: mirror, ExpectedInstanceID: state.InstanceID, ExpectedEpoch: 2, ActorIDKey: gateTestActorIDKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Check(t.Context()); !errors.Is(err, ErrQuarantine) {
		t.Fatalf("older projection error=%v", err)
	}
	if err := gate.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !authority.reconciled || mirror.state != state {
		t.Fatalf("reconciled=%v mirror=%+v", authority.reconciled, mirror.state)
	}
	if err := gate.Check(t.Context()); err != nil {
		t.Fatalf("replayed authority remained quarantined: %v", err)
	}
}
