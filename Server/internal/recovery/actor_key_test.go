package recovery

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
)

const actorKeyTestInstance = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

type actorKeyErrorFence struct{ err error }

func (fence actorKeyErrorFence) Read(context.Context) (func(), error)  { return nil, fence.err }
func (fence actorKeyErrorFence) Write(context.Context) (func(), error) { return nil, fence.err }

func actorKeyTestPepper(t *testing.T, encoded string) [32]byte {
	t.Helper()
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("decode actor key: %v", err)
	}
	var key [32]byte
	copy(key[:], decoded)
	return key
}

func TestActorIDCommitmentKnownAnswer(t *testing.T) {
	key := actorKeyTestPepper(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	commitment, err := ActorIDCommitment(key, actorKeyTestInstance)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(commitment[:]); got != "1e288d423b480e3c0006a5488a0bd9861c076921a22ba1290771a54076381c4d" {
		t.Fatalf("commitment=%s", got)
	}
}

func TestActorIDKeySidecarIsCanonicalDurableAndNoReplace(t *testing.T) {
	directory := t.TempDir()
	store, err := NewStore(directory, &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	key := actorKeyTestPepper(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err := initializeActorIDKeyForTest(t, store, actorKeyTestInstance, key); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "actor-id-key.json")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"version\":\"v1\",\"instance_id\":\"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa\",\"commitment\":\"1e288d423b480e3c0006a5488a0bd9861c076921a22ba1290771a54076381c4d\"}\n"
	if string(contents) != want {
		t.Fatalf("sidecar=%q", contents)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
	if err := store.ValidateActorIDKey(t.Context(), actorKeyTestInstance, key); err != nil {
		t.Fatalf("valid sidecar rejected: %v", err)
	}
	if err := initializeActorIDKeyForTest(t, store, actorKeyTestInstance, key); err != nil {
		t.Fatalf("exact initialization is not idempotent: %v", err)
	}
	different := key
	different[0] ^= 0xff
	if err := initializeActorIDKeyForTest(t, store, actorKeyTestInstance, different); !errors.Is(err, ErrActorIDKeyMismatch) {
		t.Fatalf("replacement error=%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != want {
		t.Fatalf("replacement changed sidecar=%q err=%v", after, err)
	}
}

func TestActorIDKeyValidationFailsClosed(t *testing.T) {
	key := actorKeyTestPepper(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	other := key
	other[31] ^= 1
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
		id     string
		key    [32]byte
	}{
		{name: "wrong pepper", id: actorKeyTestInstance, key: other},
		{name: "wrong instance", id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", key: key},
		{name: "noncanonical bytes", id: actorKeyTestInstance, key: key, mutate: func(t *testing.T, path string) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append([]byte(" "), data...), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong mode", id: actorKeyTestInstance, key: key, mutate: func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir(), &testFence{})
			if err != nil {
				t.Fatal(err)
			}
			if err := initializeActorIDKeyForTest(t, store, actorKeyTestInstance, key); err != nil {
				t.Fatal(err)
			}
			if test.mutate != nil {
				test.mutate(t, filepath.Join(store.directory, "actor-id-key.json"))
			}
			if err := store.ValidateActorIDKey(t.Context(), test.id, test.key); !errors.Is(err, ErrActorIDKeyMismatch) {
				t.Fatalf("validation error=%v", err)
			}
		})
	}
}

func TestActorPepperReplacementFailsReadiness(t *testing.T) {
	store, err := NewStore(t.TempDir(), &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	key := actorKeyTestPepper(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err := initializeActorIDKeyForTest(t, store, actorKeyTestInstance, key); err != nil {
		t.Fatal(err)
	}
	replacement := key
	replacement[0] ^= 0xff
	if err := store.ValidateActorIDKey(t.Context(), actorKeyTestInstance, replacement); !errors.Is(err, ErrActorIDKeyMismatch) {
		t.Fatalf("replacement readiness error=%v", err)
	}
}

func TestVerifyAuthorityWithActorIDKeyPreservesFailureClasses(t *testing.T) {
	newAuthority := func(t *testing.T) (localstore.Store, *Store, [32]byte) {
		t.Helper()
		root := t.TempDir()
		stores, err := localstore.Open(filepath.Join(root, "ledger"), filepath.Join(root, "head"))
		if err != nil {
			t.Fatal(err)
		}
		durable, err := NewVerifiedStore(filepath.Join(root, "head"), stores.Fence, stores.Ledger, stores.Heads)
		if err != nil {
			t.Fatal(err)
		}
		key := actorKeyTestPepper(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
		state := State{InstanceID: actorKeyTestInstance, Epoch: 1, TrustedHead: security.Head{Generation: 1}}
		if err := durable.InitializeAuthorityWithActorIDKey(t.Context(), stores, state, key); err != nil {
			t.Fatal(err)
		}
		return stores, durable, key
	}

	t.Run("canceled context", func(t *testing.T) {
		_, durable, key := newAuthority(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, _, err := durable.VerifyAuthorityWithActorIDKey(ctx, key)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("fence acquisition", func(t *testing.T) {
		want := errors.New("fence acquisition failed")
		durable, err := NewVerifiedStore(t.TempDir(), actorKeyErrorFence{err: want}, emptyLedger{}, genesisHeads{})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = durable.VerifyAuthorityWithActorIDKey(t.Context(), [32]byte{1})
		if !errors.Is(err, want) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("missing recovery state", func(t *testing.T) {
		_, durable, key := newAuthority(t)
		if err := os.Remove(durable.path); err != nil {
			t.Fatal(err)
		}
		_, _, err := durable.VerifyAuthorityWithActorIDKey(t.Context(), key)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("corrupt recovery state", func(t *testing.T) {
		_, durable, key := newAuthority(t)
		if err := os.WriteFile(durable.path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := durable.VerifyAuthorityWithActorIDKey(t.Context(), key)
		if !errors.Is(err, errStateCorrupt) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("actor mismatch", func(t *testing.T) {
		_, durable, key := newAuthority(t)
		key[0] ^= 0xff
		_, _, err := durable.VerifyAuthorityWithActorIDKey(t.Context(), key)
		if !errors.Is(err, ErrActorIDKeyMismatch) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("committed chain mismatch", func(t *testing.T) {
		stores, durable, key := newAuthority(t)
		candidate := recoveryCandidate(t, security.Head{Generation: 1}, 1, "account_registered", []security.Effect{{
			Kind: "account_registered", AccountID: "10000000-0000-4000-8000-000000000001", IdentityKey: "opaque", Status: "active",
		}})
		if err := stores.Ledger.AppendCandidate(t.Context(), candidate); err != nil {
			t.Fatal(err)
		}
		next := security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash}
		if swapped, err := stores.Heads.CompareAndSwap(t.Context(), candidate.Prior, next); err != nil || !swapped {
			t.Fatalf("commit swapped=%v err=%v", swapped, err)
		}
		files, err := filepath.Glob(filepath.Join(filepath.Dir(durable.directory), "ledger", "generation-*", "*.json"))
		if err != nil || len(files) != 1 {
			t.Fatalf("ledger files=%v err=%v", files, err)
		}
		data, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(files[0], append(data, ' '), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err = durable.VerifyAuthorityWithActorIDKey(t.Context(), key)
		if !errors.Is(err, ErrAuthorityChainMismatch) || !errors.Is(err, security.ErrLedgerCorrupt) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("trusted manifest mismatch", func(t *testing.T) {
		_, durable, key := newAuthority(t)
		trusted := security.Head{Generation: 1, Revision: 1, EventHash: [32]byte{1}}
		durable.mu.Lock()
		err := durable.writeLocked(State{InstanceID: actorKeyTestInstance, Epoch: 1, TrustedHead: trusted})
		durable.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = durable.VerifyAuthorityWithActorIDKey(t.Context(), key)
		if !errors.Is(err, ErrAuthorityChainMismatch) || !errors.Is(err, security.ErrTrustedHeadRequired) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestActorCommitmentCrossInstanceFails(t *testing.T) {
	store, err := NewStore(t.TempDir(), &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	key := actorKeyTestPepper(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err := initializeActorIDKeyForTest(t, store, actorKeyTestInstance, key); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateActorIDKey(t.Context(), "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", key); !errors.Is(err, ErrActorIDKeyMismatch) {
		t.Fatalf("cross-instance readiness error=%v", err)
	}
}

func TestBindActorKeyOnlyOnEmptyAuthority(t *testing.T) {
	t.Run("empty authority", func(t *testing.T) {
		stores, durable := actorKeyVerifiedFixture(t)
		key := actorKeyTestPepper(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
		if err := durable.BindActorIDKey(t.Context(), actorKeyTestInstance, actorKeyTestInstance, key); err != nil {
			t.Fatal(err)
		}
		if err := durable.ValidateActorIDKey(t.Context(), actorKeyTestInstance, key); err != nil {
			t.Fatal(err)
		}
		_ = stores
	})

	t.Run("nonempty authority", func(t *testing.T) {
		stores, durable := actorKeyVerifiedFixture(t)
		candidate := recoveryCandidate(t, security.Head{Generation: 1}, 1, "account_registered", []security.Effect{{
			Kind: "account_registered", AccountID: "10000000-0000-4000-8000-000000000001", IdentityKey: "opaque", Status: "active",
		}})
		if err := stores.Ledger.AppendCandidate(t.Context(), candidate); err != nil {
			t.Fatal(err)
		}
		next := security.Head{Generation: 1, Revision: 1, EventHash: candidate.EventHash}
		if swapped, err := stores.Heads.CompareAndSwap(t.Context(), candidate.Prior, next); err != nil || !swapped {
			t.Fatalf("commit swapped=%v err=%v", swapped, err)
		}
		key := actorKeyTestPepper(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
		if err := durable.BindActorIDKey(t.Context(), actorKeyTestInstance, actorKeyTestInstance, key); err == nil {
			t.Fatal("nonempty authority accepted actor key binding")
		}
		if _, err := os.Stat(filepath.Join(durable.directory, "actor-id-key.json")); !os.IsNotExist(err) {
			t.Fatalf("rejected binding created sidecar: %v", err)
		}
	})
}

func TestSecurityInitRecoversActorSidecarBeforeIdentityCrash(t *testing.T) {
	root := t.TempDir()
	stores, err := localstore.Open(filepath.Join(root, "ledger"), filepath.Join(root, "head"))
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.InitializeEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	durable, err := NewVerifiedStore(filepath.Join(root, "head"), stores.Fence, stores.Ledger, stores.Heads)
	if err != nil {
		t.Fatal(err)
	}
	key := actorKeyTestPepper(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err := initializeActorIDKeyForTest(t, durable, actorKeyTestInstance, key); err != nil {
		t.Fatal(err)
	}
	state := State{InstanceID: actorKeyTestInstance, Epoch: 1, TrustedHead: security.Head{Generation: 1}}
	if err := durable.InitializeAuthorityWithActorIDKey(t.Context(), stores, state, key); err != nil {
		t.Fatalf("partial actor-key initialization was not recoverable: %v", err)
	}
	if got, err := durable.Load(t.Context()); err != nil || got != state {
		t.Fatalf("state=%+v err=%v", got, err)
	}
}

func TestSecurityInitRecoversActorTemporaryBeforePublicationCrash(t *testing.T) {
	root := t.TempDir()
	stores, err := localstore.Open(filepath.Join(root, "ledger"), filepath.Join(root, "head"))
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.InitializeEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	durable, err := NewVerifiedStore(filepath.Join(root, "head"), stores.Fence, stores.Ledger, stores.Heads)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "head", actorIDKeyTemporary), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := actorKeyTestPepper(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	state := State{InstanceID: actorKeyTestInstance, Epoch: 1, TrustedHead: security.Head{Generation: 1}}
	if err := durable.InitializeAuthorityWithActorIDKey(t.Context(), stores, state, key); err != nil {
		t.Fatalf("partial actor-key publication was not recoverable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "head", actorIDKeyTemporary)); !os.IsNotExist(err) {
		t.Fatalf("actor-key temporary artifact remains: %v", err)
	}
}

func TestActorCommitmentRestoreRoundTrip(t *testing.T) {
	key := actorKeyTestPepper(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	source, err := NewStore(t.TempDir(), &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	if err := initializeActorIDKeyForTest(t, source, actorKeyTestInstance, key); err != nil {
		t.Fatal(err)
	}
	destination, err := NewStore(t.TempDir(), &testFence{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(source.directory, "actor-id-key.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination.directory, "actor-id-key.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := destination.ValidateActorIDKey(t.Context(), actorKeyTestInstance, key); err != nil {
		t.Fatalf("restored sidecar rejected: %v", err)
	}
}

func TestLoadActorIDPepperFileIsStrict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "actor-pepper")
	encoded := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\n"
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := LoadActorIDPepperFile(path)
	if err != nil || key != actorKeyTestPepper(t, encoded[:64]) {
		t.Fatalf("key=%x err=%v", key, err)
	}
	if err := os.WriteFile(path, []byte("ABCDEF"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadActorIDPepperFile(path); err == nil {
		t.Fatal("invalid pepper accepted")
	}
	link := filepath.Join(filepath.Dir(path), "actor-pepper-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadActorIDPepperFile(link); err == nil {
		t.Fatal("symlink pepper accepted")
	}
}

func actorKeyVerifiedFixture(t *testing.T) (localstore.Store, *Store) {
	t.Helper()
	root := t.TempDir()
	stores, err := localstore.Open(filepath.Join(root, "ledger"), filepath.Join(root, "head"))
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.InitializeEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	durable, err := NewVerifiedStore(filepath.Join(root, "head"), stores.Fence, stores.Ledger, stores.Heads)
	if err != nil {
		t.Fatal(err)
	}
	state := State{InstanceID: actorKeyTestInstance, Epoch: 1, TrustedHead: security.Head{Generation: 1}}
	if err := durable.Initialize(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	return stores, durable
}

func initializeActorIDKeyForTest(t *testing.T, store *Store, instanceID string, key [32]byte) error {
	t.Helper()
	release, err := store.fence.Write(t.Context())
	if err != nil {
		return err
	}
	defer release()
	return store.initializeActorIDKeyUnderFence(t.Context(), instanceID, key)
}
