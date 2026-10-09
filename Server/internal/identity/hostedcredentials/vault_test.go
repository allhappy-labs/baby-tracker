package hostedcredentials

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/identity"
	"babytracker/server/internal/identity/credentialtest"
	"babytracker/server/internal/security/hosted"
)

func TestCredentialReplayObjectIDKnownAnswerAndDistinct(t *testing.T) {
	binding, err := identity.NewAppleCredentialBinding("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	got, err := replayObjectID(binding)
	if err != nil {
		t.Fatal(err)
	}
	if got != "0e1049b8-6485-88ff-812c-d29f8f972c99" {
		t.Fatalf("replay object ID=%q", got)
	}
	if got == binding.ObjectID {
		t.Fatal("replay marker aliases main credential object")
	}
}

func TestHostedCredentialVaultConformance(t *testing.T) {
	credentialtest.Run(t, func(t *testing.T) credentialtest.Harness {
		now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
		store := newMemorySecretStore(func() time.Time { return now })
		cipher := &testEnvelopeCipher{keys: map[string][]byte{"vault-key-v1": bytes.Repeat([]byte{0x44}, 32)}}
		open := func(t *testing.T) CredentialAdapters {
			t.Helper()
			adapters, err := newCredentialAdapters(store, cipher, "vault-key-v1", func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			return adapters
		}
		adapters := open(t)
		return credentialtest.Harness{
			Vault: adapters.Vault, Inventory: adapters.Inventory,
			Now: func() time.Time { return now }, Advance: func(delta time.Duration) { now = now.Add(delta) },
			Reopen: func(t *testing.T) (identity.CredentialVault, identity.CredentialInventory) {
				reopened := open(t)
				return reopened.Vault, reopened.Inventory
			},
		}
	})
}

func TestHostedCredentialAdapterAuthenticatesReconcileAfter(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	store := newMemorySecretStore(func() time.Time { return now })
	cipher := &testEnvelopeCipher{keys: map[string][]byte{"vault-key-v1": bytes.Repeat([]byte{0x44}, 32)}}
	adapters, err := newCredentialAdapters(store, cipher, "vault-key-v1", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	binding, err := identity.NewAppleCredentialBinding("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := adapters.Vault.ReserveRotation(t.Context(), binding, "70000000-0000-4000-8000-000000000001", "70000000-0000-4000-8000-000000000101")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if reserved.ReconcileAfter != now.Add(10*time.Minute) {
		t.Fatalf("reconcile_after=%s", reserved.ReconcileAfter)
	}
	reopened, err := newCredentialAdapters(store, cipher, "vault-key-v1", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	got, exists, err := reopened.Vault.Resolve(t.Context(), binding)
	if err != nil || !exists || got.ReconcileAfter != reserved.ReconcileAfter {
		t.Fatalf("reopened=%+v exists=%v err=%v", got, exists, err)
	}
}

func TestHostedCredentialAdapterReadsRetainedEnvelopeKeyAndRewrapsOnTransition(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	reserved := reserveCredential(t, fixture.adapters.Vault, fixture.binding, 1)
	fixture.cipher.keys["vault-key-v2"] = bytes.Repeat([]byte{0x55}, 32)

	rotated, err := newCredentialAdapters(fixture.store, fixture.cipher, "vault-key-v2", func() time.Time { return fixture.now })
	if err != nil {
		t.Fatal(err)
	}
	got, exists, err := rotated.Vault.Resolve(t.Context(), fixture.binding)
	if err != nil || !exists || got != reserved || got.Ref.KeyID != "vault-key-v1" {
		t.Fatalf("old-key resolve=%+v exists=%v err=%v", got, exists, err)
	}
	rows, _, err := rotated.Inventory.List(t.Context(), fixture.now.Add(time.Hour), "", 10)
	if err != nil || len(rows) != 1 || rows[0].Ref.KeyID != "vault-key-v1" {
		t.Fatalf("old-key inventory=%+v err=%v", rows, err)
	}

	candidate, err := rotated.Vault.StoreCandidate(t.Context(), fixture.binding, got, []byte("rotated-refresh-token"))
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Ref.KeyID != "vault-key-v1" || candidate.Ref != got.Ref {
		t.Fatalf("rewrapped state ref=%+v", candidate.Ref)
	}
	fixture.store.mu.Lock()
	object := cloneSecretObject(fixture.store.objects[fixture.binding.ObjectID])
	fixture.store.mu.Unlock()
	if object.KeyID != "vault-key-v2" {
		t.Fatalf("transition envelope key=%q", object.KeyID)
	}
	reopened, err := newCredentialAdapters(fixture.store, fixture.cipher, "vault-key-v2", func() time.Time { return fixture.now })
	if err != nil {
		t.Fatal(err)
	}
	after, exists, err := reopened.Vault.Resolve(t.Context(), fixture.binding)
	if err != nil || !exists || after != candidate || after.Ref != reserved.Ref {
		t.Fatalf("rewrapped reopen=%+v exists=%v object_key=%q err=%v", after, exists, object.KeyID, err)
	}
}

func TestHostedCredentialAdapterReclaimsAcrossRetainedMarkerKey(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	deleteAbandonedCredential(t, fixture, 1)
	fixture.cipher.keys["vault-key-v2"] = bytes.Repeat([]byte{0x55}, 32)
	rotated, err := newCredentialAdapters(fixture.store, fixture.cipher, "vault-key-v2", func() time.Time { return fixture.now })
	if err != nil {
		t.Fatal(err)
	}

	reclaimed := reserveCredential(t, rotated.Vault, fixture.binding, 2)
	if reclaimed.Ref.KeyID != "vault-key-v2" {
		t.Fatalf("reclaimed ref=%+v", reclaimed.Ref)
	}
	rows, _, err := rotated.Inventory.List(t.Context(), fixture.now.Add(time.Hour), "", 10)
	if err != nil || len(rows) != 1 || rows[0].Ref.KeyID != "vault-key-v2" {
		t.Fatalf("mixed-key inventory=%+v err=%v", rows, err)
	}
	marker, exists, err := fixture.vault().loadMarker(t.Context(), fixture.binding)
	if err != nil || !exists || marker.body.OperationID != testCredentialUUID(1) {
		t.Fatalf("retained marker=%+v exists=%v err=%v", marker, exists, err)
	}
}

func TestHostedCredentialAdapterKeyRotationPreservesDeletionCapabilityEquality(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	ready := readyCredential(t, fixture, 1)
	fixture.cipher.keys["vault-key-v2"] = bytes.Repeat([]byte{0x55}, 32)
	rotated, err := newCredentialAdapters(fixture.store, fixture.cipher, "vault-key-v2", func() time.Time { return fixture.now })
	if err != nil {
		t.Fatal(err)
	}
	ready, exists, err := rotated.Vault.Resolve(t.Context(), fixture.binding)
	if err != nil || !exists || ready.Ref.KeyID != "vault-key-v1" {
		t.Fatalf("old ready state=%+v exists=%v err=%v", ready, exists, err)
	}
	intent, err := rotated.Vault.PrepareDeletionCAS(t.Context(), fixture.binding, ready, identity.DeletionOperation{
		OperationID: testCredentialUUID(200), CommandID: testCredentialUUID(201),
	})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := rotated.Vault.MarkDeleting(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := rotated.Vault.OpenDeletingTokens(t.Context(), capability)
	if err != nil || len(tokens) != 1 || string(tokens[0]) != "refresh-1" {
		t.Fatalf("deletion tokens=%q err=%v", tokens, err)
	}
	deleting, exists, err := rotated.Vault.Resolve(t.Context(), fixture.binding)
	if err != nil || !exists || deleting.Ref != ready.Ref {
		t.Fatalf("rewrapped deleting=%+v exists=%v err=%v", deleting, exists, err)
	}
	fixture.store.mu.Lock()
	object := cloneSecretObject(fixture.store.objects[fixture.binding.ObjectID])
	fixture.store.mu.Unlock()
	if object.KeyID != "vault-key-v2" || object.KeyID == deleting.Ref.KeyID {
		t.Fatalf("envelope key=%q stable state ref=%+v", object.KeyID, deleting.Ref)
	}
	restarted, err := newCredentialAdapters(fixture.store, fixture.cipher, "vault-key-v2", func() time.Time { return fixture.now })
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.Vault.RecoverDeletionCapability(t.Context(), identity.DeletionRecovery{
		Binding: fixture.binding, Ref: ready.Ref, OperationID: deleting.OperationID, CommandID: deleting.CommandID,
		MarkerSequence: deleting.MarkerSequence, MarkerDigest: deleting.MarkerDigest,
	})
	if err != nil {
		t.Fatalf("recover with persisted original ref: %v", err)
	}
	tokens, err = restarted.Vault.OpenDeletingTokens(t.Context(), recovered)
	if err != nil || len(tokens) != 1 || string(tokens[0]) != "refresh-1" {
		t.Fatalf("recovered deletion tokens=%q err=%v", tokens, err)
	}
}

func TestCredentialEnvelopeSequenceUsesJSONSafeIntegerBound(t *testing.T) {
	const maxSafeInteger = uint64(9007199254740991)
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	reserveCredential(t, fixture.adapters.Vault, fixture.binding, 1)
	fixture.resealMainAtSequence(t, maxSafeInteger)
	state, exists, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding)
	if err != nil || !exists {
		t.Fatalf("maximum safe sequence rejected: exists=%v err=%v", exists, err)
	}
	fixture.store.mu.Lock()
	before := cloneSecretObject(fixture.store.objects[fixture.binding.ObjectID])
	fixture.store.mu.Unlock()
	requireCredentialConflict(t, func() error {
		_, err := fixture.adapters.Vault.StoreCandidate(t.Context(), fixture.binding, state, []byte("must-not-wrap"))
		return err
	})
	fixture.store.mu.Lock()
	after := cloneSecretObject(fixture.store.objects[fixture.binding.ObjectID])
	fixture.store.mu.Unlock()
	if !reflect.DeepEqual(after, before) {
		t.Fatal("bound rejection changed provider object")
	}

	fixture.resealMainAtSequence(t, maxSafeInteger+1)
	requireCredentialConflict(t, func() error {
		_, _, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding)
		return err
	})
}

func TestCredentialReplayMarkerEnvelopeSequenceUsesJSONSafeIntegerBound(t *testing.T) {
	const maxSafeInteger = uint64(9007199254740991)
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	deleteAbandonedCredential(t, fixture, 1)
	fixture.resealMarkerAtSequence(t, maxSafeInteger)
	marker, exists, err := fixture.vault().loadMarker(t.Context(), fixture.binding)
	if err != nil || !exists || marker.sequence != maxSafeInteger {
		t.Fatalf("maximum safe marker=%+v exists=%v err=%v", marker, exists, err)
	}
	before := fixture.store.objectCount()
	requireCredentialConflict(t, func() error {
		_, err := fixture.adapters.Vault.ReserveRotation(t.Context(), fixture.binding, testCredentialUUID(2), testCredentialUUID(102))
		return err
	})
	after := fixture.store.objectCount()
	if after != before {
		t.Fatalf("bound rejection changed provider object count: before=%d after=%d", before, after)
	}

	fixture.resealMarkerAtSequence(t, maxSafeInteger+1)
	requireCredentialConflict(t, func() error {
		_, _, err := fixture.vault().loadMarker(t.Context(), fixture.binding)
		return err
	})
}

func TestCredentialDeletionMarkerSequenceUsesJSONSafeIntegerBound(t *testing.T) {
	const maxSafeInteger = uint64(9007199254740991)
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	readyCredential(t, fixture, 1)
	prepareReadyDeletion(t, fixture, 200)
	deleting, exists, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding)
	if err != nil || !exists {
		t.Fatalf("resolve deleting: exists=%v err=%v", exists, err)
	}
	fixture.resealMain(t, func(body *mainBody) {
		body.State.MarkerSequence = maxSafeInteger
		operation := identity.DeletionOperation{OperationID: body.State.OperationID, CommandID: body.State.CommandID}
		digest := deletionMarker(body.Binding, body.Ref, operation, maxSafeInteger, deleting.Version)
		body.State.MarkerDigest = fmt.Sprintf("%x", digest)
	})
	if _, exists, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding); err != nil || !exists {
		t.Fatalf("maximum safe marker sequence rejected: exists=%v err=%v", exists, err)
	}
	fixture.resealMain(t, func(body *mainBody) { body.State.MarkerSequence = maxSafeInteger + 1 })
	requireCredentialConflict(t, func() error {
		_, _, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding)
		return err
	})
}

func TestCredentialEnvelopeAADKnownAnswer(t *testing.T) {
	header := CredentialEnvelopeHeader{Schema: "credential-envelope-v1", EnvelopeSequence: 7}
	got, err := credentialAAD("11111111-1111-4111-8111-111111111111", "vault-key-v1", header)
	if err != nil {
		t.Fatal(err)
	}
	want := literalCredentialFrame(
		[]byte("baby-tracker-credential-envelope-aad-v1"),
		[]byte("11111111-1111-4111-8111-111111111111"),
		[]byte("vault-key-v1"),
		[]byte("credential-envelope-v1"),
		[]byte{0, 0, 0, 0, 0, 0, 0, 7},
	)
	if !bytes.Equal(got, want) {
		t.Fatalf("AAD\n got %x\nwant %x", got, want)
	}
}

func TestCredentialInventoryConstructorRequiresCipher(t *testing.T) {
	store := &stubSecretStore{}
	for name, storeAndCipher := range map[string]struct {
		store  SecretStore
		cipher hosted.EnvelopeCipher
	}{
		"nil store":  {cipher: stubCipher{}},
		"nil cipher": {store: store},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewCredentialAdapters(storeAndCipher.store, storeAndCipher.cipher, "vault-key-v1"); err == nil {
				t.Fatal("invalid hosted credential configuration accepted")
			}
		})
	}
}

func TestCredentialEnvelopeRoundTripFromMetadataOnly(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	reserved := reserveCredential(t, fixture.adapters.Vault, fixture.binding, 1)
	if _, err := fixture.adapters.Vault.StoreCandidate(t.Context(), fixture.binding, reserved, []byte("private-refresh-token")); err != nil {
		t.Fatal(err)
	}
	rows, cursor, err := fixture.adapters.Inventory.List(t.Context(), fixture.now.Add(time.Hour), "", 10)
	if err != nil || cursor != "" || len(rows) != 1 || rows[0].Binding != fixture.binding || rows[0].Phase != "candidate" {
		t.Fatalf("rows=%+v cursor=%q err=%v", rows, cursor, err)
	}
	fixture.store.mu.Lock()
	object := fixture.store.objects[fixture.binding.ObjectID]
	fixture.store.mu.Unlock()
	if bytes.Contains(object.Ciphertext, []byte("private-refresh-token")) || bytes.Contains(object.Ciphertext, []byte(fixture.binding.AccountID)) {
		t.Fatal("secret metadata object exposed encrypted plaintext")
	}
}

func TestCredentialEnvelopeRejectsWrongKey(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	reserveCredential(t, fixture.adapters.Vault, fixture.binding, 1)
	fixture.mutateMain(t, func(object *SecretObject) { object.KeyID = "wrong-key" })
	requireCredentialConflict(t, func() error {
		_, _, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding)
		return err
	})
}

func TestCredentialEnvelopeRejectsWrongOuterObjectOrKeyID(t *testing.T) {
	for name, mutate := range map[string]func(*SecretObject){
		"object": func(object *SecretObject) { object.ID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc" },
		"key":    func(object *SecretObject) { object.KeyID = "wrong-key" },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
			reserveCredential(t, fixture.adapters.Vault, fixture.binding, 1)
			fixture.mutateMain(t, mutate)
			requireCredentialConflict(t, func() error {
				_, _, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding)
				return err
			})
		})
	}
}

func TestCredentialEnvelopeRejectsWrongHeaderSchemaOrSequence(t *testing.T) {
	for name, mutate := range map[string]func(*SecretObject){
		"schema":   func(object *SecretObject) { object.Header.Schema = "credential-envelope-v2" },
		"sequence": func(object *SecretObject) { object.Header.EnvelopeSequence++ },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
			reserveCredential(t, fixture.adapters.Vault, fixture.binding, 1)
			fixture.mutateMain(t, mutate)
			requireCredentialConflict(t, func() error {
				_, _, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding)
				return err
			})
		})
	}
}

func TestCredentialEnvelopeRejectsEmbeddedBindingRefOrPhaseMismatch(t *testing.T) {
	for name, mutate := range map[string]func(*mainBody){
		"binding": func(body *mainBody) { body.Binding.AccountID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc" },
		"ref":     func(body *mainBody) { body.Ref.ID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc" },
		"phase":   func(body *mainBody) { body.State.Phase = "ready" },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
			reserveCredential(t, fixture.adapters.Vault, fixture.binding, 1)
			fixture.resealMain(t, mutate)
			requireCredentialConflict(t, func() error {
				_, _, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding)
				return err
			})
		})
	}
}

func TestCredentialReplayMarkerHiddenFromInventory(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	deleteAbandonedCredential(t, fixture, 1)
	rows, cursor, err := fixture.adapters.Inventory.List(t.Context(), fixture.now.Add(time.Hour), "", 10)
	if err != nil || cursor != "" || len(rows) != 0 {
		t.Fatalf("rows=%+v cursor=%q err=%v", rows, cursor, err)
	}
	if fixture.store.objectCount() != 1 {
		t.Fatalf("provider object count=%d, want replay marker only", fixture.store.objectCount())
	}
}

func TestCredentialInventoryOneProviderPagePerCall(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	reserveCredential(t, fixture.adapters.Vault, fixture.binding, 1)
	fixture.store.resetCalls()
	if _, _, err := fixture.adapters.Inventory.List(t.Context(), fixture.now.Add(time.Hour), "provider-cursor", 7); err != nil {
		t.Fatal(err)
	}
	if calls, limits, cursors := fixture.store.listStats(); calls != 1 || !reflect.DeepEqual(limits, []int{7}) || !reflect.DeepEqual(cursors, []string{"provider-cursor"}) {
		t.Fatalf("calls=%d limits=%v cursors=%v", calls, limits, cursors)
	}
}

func TestCredentialInventoryMarkerOnlyPagesAdvanceCursor(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	deleteAbandonedCredential(t, fixture, 1)
	markerID, _ := replayObjectID(fixture.binding)
	var mainBinding identity.CredentialBinding
	for index := 1; index < 1000; index++ {
		accountID := fmt.Sprintf("cccccccc-cccc-4ccc-8ccc-%012d", index)
		candidate, err := identity.NewAppleCredentialBinding(accountID, fixture.binding.ServerInstanceID)
		if err == nil && markerID < candidate.ObjectID {
			mainBinding = candidate
			break
		}
	}
	if mainBinding.ObjectID == "" {
		t.Fatal("could not construct main object ordered after marker")
	}
	reserveCredential(t, fixture.adapters.Vault, mainBinding, 2)
	rows, cursor, err := fixture.adapters.Inventory.List(t.Context(), fixture.now.Add(time.Hour), "", 1)
	if err != nil || len(rows) != 0 || cursor != markerID {
		t.Fatalf("rows=%+v cursor=%q marker=%q err=%v", rows, cursor, markerID, err)
	}
	rows, next, err := fixture.adapters.Inventory.List(t.Context(), fixture.now.Add(time.Hour), cursor, 1)
	if err != nil || len(rows) != 1 || rows[0].Binding != mainBinding || next != "" {
		t.Fatalf("second rows=%+v cursor=%q err=%v", rows, next, err)
	}
}

func TestCredentialInventoryFilteredPagesDoNotDuplicateMainObjects(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	bindings := []identity.CredentialBinding{fixture.binding}
	for index := 1; index <= 3; index++ {
		binding, err := identity.NewAppleCredentialBinding(fmt.Sprintf("cccccccc-cccc-4ccc-8ccc-%012d", index), fixture.binding.ServerInstanceID)
		if err != nil {
			t.Fatal(err)
		}
		bindings = append(bindings, binding)
	}
	for index, binding := range bindings {
		reserveCredential(t, fixture.adapters.Vault, binding, index+1)
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		rows, next, err := fixture.adapters.Inventory.List(t.Context(), fixture.now.Add(time.Hour), cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if seen[row.Binding.ObjectID] {
				t.Fatalf("duplicate main object %s", row.Binding.ObjectID)
			}
			seen[row.Binding.ObjectID] = true
		}
		if next == "" {
			break
		}
		if next == cursor {
			t.Fatalf("cursor did not advance: %q", cursor)
		}
		cursor = next
	}
	if len(seen) != len(bindings) {
		t.Fatalf("seen=%d want=%d", len(seen), len(bindings))
	}
}

func TestCredentialReplayMarkerWrongAADFailsClosed(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	deleteAbandonedCredential(t, fixture, 1)
	markerID, _ := replayObjectID(fixture.binding)
	fixture.store.mu.Lock()
	marker := fixture.store.objects[markerID]
	marker.Header.EnvelopeSequence++
	fixture.store.objects[markerID] = marker
	fixture.store.mu.Unlock()
	requireCredentialConflict(t, func() error {
		_, err := fixture.adapters.Vault.ReserveRotation(t.Context(), fixture.binding, testCredentialUUID(2), testCredentialUUID(102))
		return err
	})
}

func TestCredentialReplayMarkerPurgeIsAuthenticatedAndIdempotent(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	deleteAbandonedCredential(t, fixture, 1)
	if err := fixture.adapters.ReplayMarkers.PurgeReplayMarker(t.Context(), fixture.binding); err != nil {
		t.Fatal(err)
	}
	if err := fixture.adapters.ReplayMarkers.PurgeReplayMarker(t.Context(), fixture.binding); err != nil {
		t.Fatalf("idempotent purge: %v", err)
	}
	if fixture.store.objectCount() != 0 {
		t.Fatalf("objects remain=%d", fixture.store.objectCount())
	}
}

func TestReadyDeleteRepairsMissingReplayMarker(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	readyCredential(t, fixture, 1)
	main, _, _ := fixture.vault().loadMain(t.Context(), fixture.binding)
	deleteReadyCredential(t, fixture, 500)
	marker, exists, err := fixture.vault().loadMarker(t.Context(), fixture.binding)
	if err != nil || !exists || marker.sequence <= main.sequence || marker.body.OperationID != testCredentialUUID(1) || marker.body.CommandID != testCredentialUUID(101) {
		t.Fatalf("marker=%+v exists=%v err=%v", marker, exists, err)
	}
}

func TestCredentialReplayMarkerCASKeepsOneObject(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	readyCredential(t, fixture, 1)
	deleteReadyCredential(t, fixture, 500)
	first, _, _ := fixture.vault().loadMarker(t.Context(), fixture.binding)
	readyCredential(t, fixture, 2)
	deleteReadyCredential(t, fixture, 600)
	marker, exists, err := fixture.vault().loadMarker(t.Context(), fixture.binding)
	if err != nil || !exists || marker.sequence <= first.sequence || marker.body.OperationID != testCredentialUUID(2) || marker.body.CommandID != testCredentialUUID(102) {
		t.Fatalf("marker=%+v exists=%v err=%v", marker, exists, err)
	}
	if fixture.store.objectCount() != 1 {
		t.Fatalf("provider object count=%d", fixture.store.objectCount())
	}
}

func TestCredentialInventoryMetadataIsOpaque(t *testing.T) {
	typeOf := reflect.TypeOf(SecretMetadata{})
	want := []string{"ObjectID", "Version", "KeyID", "CreatedAt"}
	if typeOf.NumField() != len(want) {
		t.Fatalf("metadata fields=%d want=%d", typeOf.NumField(), len(want))
	}
	for index, name := range want {
		if typeOf.Field(index).Name != name {
			t.Fatalf("field[%d]=%s want=%s", index, typeOf.Field(index).Name, name)
		}
	}
}

func TestCredentialProviderErrorsAreSafe(t *testing.T) {
	poison := errors.New("provider-secret-token-ciphertext")
	store := &errorSecretStore{err: poison}
	adapters, err := NewCredentialAdapters(store, stubCipher{}, "vault-key-v1")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := identity.NewAppleCredentialBinding("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	_, _, got := adapters.Vault.Resolve(t.Context(), binding)
	if !errors.Is(got, identity.ErrUnavailable) || strings.Contains(got.Error(), poison.Error()) {
		t.Fatalf("unsafe resolve error: %v", got)
	}
	_, _, got = adapters.Inventory.List(t.Context(), time.Now().UTC(), "", 1)
	if !errors.Is(got, identity.ErrUnavailable) || strings.Contains(got.Error(), poison.Error()) {
		t.Fatalf("unsafe inventory error: %v", got)
	}
}

func TestCredentialReplayMarkerPurgeRejectsTampering(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	deleteAbandonedCredential(t, fixture, 1)
	markerID, _ := replayObjectID(fixture.binding)
	fixture.store.mu.Lock()
	marker := fixture.store.objects[markerID]
	marker.Ciphertext[0] ^= 0xff
	marker.SHA256 = sha256.Sum256(marker.Ciphertext)
	fixture.store.objects[markerID] = marker
	fixture.store.mu.Unlock()
	requireCredentialConflict(t, func() error {
		return fixture.adapters.ReplayMarkers.PurgeReplayMarker(t.Context(), fixture.binding)
	})
	if fixture.store.objectCount() != 1 {
		t.Fatal("tampered marker was deleted")
	}
}

func TestReplayMarkerProviderTimeoutMatrix(t *testing.T) {
	for _, path := range []string{"deleting", "maintenance"} {
		for _, scenario := range []struct {
			name      string
			method    string
			after     bool
			remaining int
			wantError bool
			stale     bool
		}{
			{name: "before marker resolve", method: "resolve-marker", remaining: 1, wantError: true},
			{name: "after marker resolve", method: "resolve-marker", after: true, remaining: 1, wantError: true},
			{name: "before marker create", method: "create-marker", remaining: markerCASAttempts, wantError: true},
			{name: "after marker create", method: "create-marker", after: true, remaining: 1},
			{name: "before marker cas", method: "cas-marker", remaining: markerCASAttempts, wantError: true, stale: true},
			{name: "after marker cas", method: "cas-marker", after: true, remaining: 1, stale: true},
			{name: "before main delete", method: "delete-main", remaining: 1, wantError: true},
			{name: "after main delete", method: "delete-main", after: true, remaining: 1},
		} {
			t.Run(path+"/"+scenario.name, func(t *testing.T) {
				fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
				if scenario.stale {
					readyCredential(t, fixture, 1)
					deleteReadyCredential(t, fixture, 500)
				}
				faults := &faultSecretStore{memorySecretStore: fixture.store}
				var err error
				fixture.adapters, err = newCredentialAdapters(faults, fixture.cipher, "vault-key-v1", func() time.Time { return fixture.now })
				if err != nil {
					t.Fatal(err)
				}
				var invoke func() error
				if path == "deleting" {
					readyCredential(t, fixture, 2)
					capability := prepareReadyDeletion(t, fixture, 600)
					invoke = func() error { return fixture.adapters.Vault.Delete(t.Context(), capability) }
				} else {
					state := prepareAbandonedMaintenance(t, fixture, 2)
					invoke = func() error {
						_, err := fixture.adapters.Vault.CompleteMaintenance(t.Context(), fixture.binding, state)
						return err
					}
				}
				markerID, _ := replayObjectID(fixture.binding)
				faults.setFault(scenario.method, fixture.binding.ObjectID, markerID, scenario.after, scenario.remaining)
				err = invoke()
				if scenario.wantError && !errors.Is(err, identity.ErrUnavailable) && !errors.Is(err, identity.ErrCredentialConflict) {
					t.Fatalf("err=%v", err)
				}
				if !scenario.wantError && err != nil {
					t.Fatalf("ambiguous committed mutation did not reconcile: %v", err)
				}
				if scenario.wantError {
					if _, exists, _ := fixture.store.Resolve(t.Context(), fixture.binding.ObjectID); !exists {
						t.Fatal("main credential deleted before exact marker was proven")
					}
					faults.clearFault()
					if err := invoke(); err != nil {
						t.Fatalf("restart/retry: %v", err)
					}
				}
				if _, exists, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding); err != nil || exists {
					t.Fatalf("main exists=%v err=%v", exists, err)
				}
				marker, exists, err := fixture.vault().loadMarker(t.Context(), fixture.binding)
				if err != nil || !exists || marker.body.OperationID != testCredentialUUID(2) || marker.body.CommandID != testCredentialUUID(102) {
					t.Fatalf("marker=%+v exists=%v err=%v", marker, exists, err)
				}
			})
		}
	}
}

func TestDeletionCapabilityRejectsWrongObjectVersionOrMarkerSequence(t *testing.T) {
	for name, mutate := range map[string]func(*capabilityBody){
		"version": func(payload *capabilityBody) {
			payload.Version = "stale-version"
			payload.Expected.Version = "stale-version"
		},
		"marker sequence": func(payload *capabilityBody) {
			payload.MarkerSequence++
			payload.Expected.MarkerSequence++
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
			readyCredential(t, fixture, 1)
			capability := prepareReadyDeletion(t, fixture, 500)
			payload, err := fixture.vault().openCapability(t.Context(), capability.Opaque, "deletion-capability-v1")
			if err != nil {
				t.Fatal(err)
			}
			mutate(&payload)
			opaque, err := fixture.vault().sealCapability(t.Context(), payload)
			if err != nil {
				t.Fatal(err)
			}
			requireCredentialConflict(t, func() error {
				_, err := fixture.adapters.Vault.OpenDeletingTokens(t.Context(), identity.DeletionCapability{Opaque: opaque})
				return err
			})
		})
	}
}

func TestCredentialProviderCancellationIsPreserved(t *testing.T) {
	store := &errorSecretStore{err: errors.New("provider leaked details")}
	adapters, err := NewCredentialAdapters(store, stubCipher{}, "vault-key-v1")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := identity.NewAppleCredentialBinding("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = adapters.Vault.Resolve(ctx, binding)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context canceled", err)
	}
}

func TestFencedIntentConsumptionUsesExactlyOnePresealedCAS(t *testing.T) {
	for _, kind := range []string{"promotion", "deletion"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
			var consume func() error
			var replay func() error
			if kind == "promotion" {
				state := reserveCredential(t, fixture.adapters.Vault, fixture.binding, 1)
				var err error
				state, err = fixture.adapters.Vault.StoreCandidate(t.Context(), fixture.binding, state, []byte("refresh-1"))
				if err != nil {
					t.Fatal(err)
				}
				state, err = fixture.adapters.Vault.PromotePending(t.Context(), fixture.binding, state)
				if err != nil {
					t.Fatal(err)
				}
				intent, err := fixture.adapters.Vault.PreparePromotionCAS(t.Context(), fixture.binding, state, sha256.Sum256([]byte("authority-hash")))
				if err != nil {
					t.Fatal(err)
				}
				consume = func() error { _, err := fixture.adapters.Vault.ConfirmPromotion(t.Context(), intent); return err }
				replay = consume
			} else {
				readyCredential(t, fixture, 1)
				state, _, _ := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding)
				intent, err := fixture.adapters.Vault.PrepareDeletionCAS(t.Context(), fixture.binding, state, identity.DeletionOperation{OperationID: testCredentialUUID(500), CommandID: testCredentialUUID(501)})
				if err != nil {
					t.Fatal(err)
				}
				consume = func() error { _, err := fixture.adapters.Vault.MarkDeleting(t.Context(), intent); return err }
				replay = consume
			}
			fenced := &fencedSecretStore{base: fixture.store}
			fixture.vault().store = fenced
			fixture.vault().cipher = panicEnvelopeCipher{}
			if err := consume(); err != nil {
				t.Fatal(err)
			}
			if fenced.casCalls != 1 {
				t.Fatalf("CAS calls=%d want=1", fenced.casCalls)
			}
			requireCredentialConflict(t, replay)
			if fenced.casCalls != 1 {
				t.Fatalf("consumed replay reached provider: CAS calls=%d", fenced.casCalls)
			}
		})
	}
}

func TestPreparedIntentRejectsAlteredOpaqueOrDeadline(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	state := reserveCredential(t, fixture.adapters.Vault, fixture.binding, 1)
	state, _ = fixture.adapters.Vault.StoreCandidate(t.Context(), fixture.binding, state, []byte("refresh-1"))
	state, _ = fixture.adapters.Vault.PromotePending(t.Context(), fixture.binding, state)
	intent, err := fixture.adapters.Vault.PreparePromotionCAS(t.Context(), fixture.binding, state, sha256.Sum256([]byte("authority-hash")))
	if err != nil {
		t.Fatal(err)
	}
	for name, altered := range map[string]identity.PromotionCASIntent{
		"hash or operation opaque": mustPromotionIntent(t, append(append([]byte(nil), intent.Opaque...), 0x01), intent.ExpiresAt(), fixture.now),
		"deadline":                 mustPromotionIntent(t, intent.Opaque, intent.ExpiresAt().Add(-time.Second), fixture.now),
	} {
		t.Run(name, func(t *testing.T) {
			requireCredentialConflict(t, func() error { _, err := fixture.adapters.Vault.ConfirmPromotion(t.Context(), altered); return err })
		})
	}
	if _, err := fixture.adapters.Vault.ConfirmPromotion(t.Context(), intent); err != nil {
		t.Fatalf("unaltered intent was consumed by rejection: %v", err)
	}
}

func TestAbortPreserveCurrentClearedAcrossMaintenanceAndDeletionReopen(t *testing.T) {
	for _, exit := range []string{"maintenance", "deletion"} {
		t.Run(exit, func(t *testing.T) {
			fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
			readyCredential(t, fixture, 1)
			state := reserveCredential(t, fixture.adapters.Vault, fixture.binding, 2)
			state, _ = fixture.adapters.Vault.StoreCandidate(t.Context(), fixture.binding, state, []byte("refresh-2"))
			state, err := fixture.adapters.Vault.MarkAbandoned(t.Context(), fixture.binding, state)
			if err != nil {
				t.Fatal(err)
			}
			if exit == "maintenance" {
				state, err = fixture.adapters.Vault.ClaimMaintenance(t.Context(), fixture.binding, state, "test-worker", fixture.now.Add(time.Minute))
				if err == nil {
					state, err = fixture.adapters.Vault.CompleteMaintenance(t.Context(), fixture.binding, state)
				}
			} else {
				intent, prepareErr := fixture.adapters.Vault.PrepareDeletionCAS(t.Context(), fixture.binding, state, identity.DeletionOperation{OperationID: testCredentialUUID(500), CommandID: testCredentialUUID(501)})
				if prepareErr != nil {
					t.Fatal(prepareErr)
				}
				_, err = fixture.adapters.Vault.MarkDeleting(t.Context(), intent)
			}
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := newCredentialAdapters(fixture.store, fixture.cipher, "vault-key-v1", func() time.Time { return fixture.now })
			if err != nil {
				t.Fatal(err)
			}
			got, exists, err := reopened.Vault.Resolve(t.Context(), fixture.binding)
			if err != nil || !exists || (exit == "maintenance" && got.Phase != "ready") || (exit == "deletion" && got.Phase != "deleting") {
				t.Fatalf("state=%+v exists=%v err=%v", got, exists, err)
			}
		})
	}
}

func TestReplayMarkerHistoryNeverRollsBackAcrossBarrierRace(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	readyCredential(t, fixture, 1)
	record1, _, _ := fixture.vault().loadMain(t.Context(), fixture.binding)
	capability1 := prepareReadyDeletion(t, fixture, 500)
	if err := fixture.adapters.Vault.Delete(t.Context(), capability1); err != nil {
		t.Fatal(err)
	}
	readyCredential(t, fixture, 2)
	record2, _, _ := fixture.vault().loadMain(t.Context(), fixture.binding)
	capability2 := prepareReadyDeletion(t, fixture, 600)
	if err := fixture.adapters.Vault.Delete(t.Context(), capability2); err != nil {
		t.Fatal(err)
	}
	readyCredential(t, fixture, 3)
	record3, _, _ := fixture.vault().loadMain(t.Context(), fixture.binding)
	capability3 := prepareReadyDeletion(t, fixture, 700)
	if !(record1.sequence < record2.sequence && record2.sequence < record3.sequence) {
		t.Fatalf("main envelope sequences decreased: %d %d %d", record1.sequence, record2.sequence, record3.sequence)
	}
	start := make(chan struct{})
	errorsOut := make(chan error, 2)
	go func() { <-start; errorsOut <- fixture.adapters.Vault.Delete(t.Context(), capability2) }()
	go func() { <-start; errorsOut <- fixture.adapters.Vault.Delete(t.Context(), capability3) }()
	close(start)
	first, second := <-errorsOut, <-errorsOut
	if !((first == nil && errors.Is(second, identity.ErrCredentialConflict)) || (second == nil && errors.Is(first, identity.ErrCredentialConflict))) {
		t.Fatalf("barrier results: first=%v second=%v", first, second)
	}
	marker, exists, err := fixture.vault().loadMarker(t.Context(), fixture.binding)
	if err != nil || !exists || marker.sequence <= record3.sequence || marker.body.OperationID != testCredentialUUID(3) || marker.body.CommandID != testCredentialUUID(103) {
		t.Fatalf("marker=%+v exists=%v err=%v", marker, exists, err)
	}
	if err := fixture.vault().ensureReplayMarker(t.Context(), record2); !errors.Is(err, identity.ErrCredentialConflict) {
		t.Fatalf("stale history repair err=%v", err)
	}
	markerAfter, _, err := fixture.vault().loadMarker(t.Context(), fixture.binding)
	if err != nil || markerAfter.sequence != marker.sequence || markerAfter.body != marker.body {
		t.Fatalf("marker rolled back: before=%+v after=%+v err=%v", marker, markerAfter, err)
	}
}

func TestStaleDeletionCapabilityConflictsWhenReclaimedMainExistsOrResolveFails(t *testing.T) {
	for _, mode := range []string{"reclaimed", "resolve failure"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
			readyCredential(t, fixture, 1)
			stale := prepareReadyDeletion(t, fixture, 500)
			if err := fixture.adapters.Vault.Delete(t.Context(), stale); err != nil {
				t.Fatal(err)
			}
			readyCredential(t, fixture, 2)
			if mode == "resolve failure" {
				markerID, _ := replayObjectID(fixture.binding)
				faults := &faultSecretStore{memorySecretStore: fixture.store}
				faults.setFault("resolve-main", fixture.binding.ObjectID, markerID, false, 1)
				fixture.vault().store = faults
			}
			err := fixture.adapters.Vault.Delete(t.Context(), stale)
			if mode == "reclaimed" && !errors.Is(err, identity.ErrCredentialConflict) {
				t.Fatalf("err=%v want conflict", err)
			}
			if mode == "resolve failure" && !errors.Is(err, identity.ErrUnavailable) {
				t.Fatalf("err=%v want unavailable", err)
			}
		})
	}
}

func TestCredentialInventoryRejectsResolvedObjectIDMismatch(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	other, err := identity.NewAppleCredentialBinding("cccccccc-cccc-4ccc-8ccc-cccccccccccc", fixture.binding.ServerInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	reserveCredential(t, fixture.adapters.Vault, other, 2)
	fixture.store.mu.Lock()
	object := cloneSecretObject(fixture.store.objects[other.ObjectID])
	created := fixture.store.created[other.ObjectID]
	fixture.store.mu.Unlock()
	forged := &inventoryObjectIDMismatchStore{SecretStore: fixture.store, metadataID: fixture.binding.ObjectID, object: object, createdAt: created}
	adapters, err := newCredentialAdapters(forged, fixture.cipher, "vault-key-v1", func() time.Time { return fixture.now })
	if err != nil {
		t.Fatal(err)
	}
	requireCredentialConflict(t, func() error {
		_, _, err := adapters.Inventory.List(t.Context(), fixture.now.Add(time.Hour), "", 1)
		return err
	})
}

func TestSecondRotationAdoptionCapabilitySupportsFinishAndAbandon(t *testing.T) {
	for _, secondAction := range []string{"finish", "abandon"} {
		t.Run(secondAction, func(t *testing.T) {
			fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
			runAdoptionCycle(t, fixture, 1, true)
			runAdoptionCycle(t, fixture, 2, secondAction == "finish")
			state, exists, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding)
			want := "ready"
			if secondAction == "abandon" {
				want = "aborting"
			}
			if err != nil || !exists || state.Phase != want {
				t.Fatalf("state=%+v exists=%v err=%v want=%s", state, exists, err, want)
			}
		})
	}
}

func TestRepeatedEraseAfterMarkerPersistedDeleteFailureCompletes(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	readyCredential(t, fixture, 1)
	capability := prepareReadyDeletion(t, fixture, 500)
	before, _, _ := fixture.vault().loadMain(t.Context(), fixture.binding)
	markerID, _ := replayObjectID(fixture.binding)
	faults := &faultSecretStore{memorySecretStore: fixture.store}
	faults.setFault("delete-main", fixture.binding.ObjectID, markerID, false, 1)
	fixture.vault().store = faults
	if err := fixture.adapters.Vault.Delete(t.Context(), capability); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("first delete err=%v", err)
	}
	reopened, err := newCredentialAdapters(fixture.store, fixture.cipher, "vault-key-v1", func() time.Time { return fixture.now })
	if err != nil {
		t.Fatal(err)
	}
	state, exists, err := reopened.Vault.Resolve(t.Context(), fixture.binding)
	if err != nil || !exists || state.Phase != "deleting" {
		t.Fatalf("state=%+v exists=%v err=%v", state, exists, err)
	}
	capability, err = reopened.Vault.RecoverDeletionCapability(t.Context(), identity.DeletionRecovery{
		Binding: fixture.binding, Ref: state.Ref, OperationID: state.OperationID, CommandID: state.CommandID,
		MarkerSequence: state.MarkerSequence, MarkerDigest: state.MarkerDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	capability, err = reopened.Vault.EraseDeletingTokens(t.Context(), capability)
	if err != nil {
		t.Fatal(err)
	}
	after, _, _ := reopened.Vault.(*vault).loadMain(t.Context(), fixture.binding)
	if after.sequence != before.sequence {
		t.Fatalf("empty erase advanced envelope sequence: before=%d after=%d", before.sequence, after.sequence)
	}
	if err := reopened.Vault.Delete(t.Context(), capability); err != nil {
		t.Fatal(err)
	}
	marker, markerExists, err := reopened.Vault.(*vault).loadMarker(t.Context(), fixture.binding)
	if err != nil || !markerExists || marker.sequence <= before.sequence {
		t.Fatalf("marker=%+v exists=%v err=%v", marker, markerExists, err)
	}
}

func TestConcurrentDeleteMarkerBlocksStaleExactAndPartialReservation(t *testing.T) {
	for name, coordinates := range map[string][2]string{
		"exact":             {testCredentialUUID(1), testCredentialUUID(101)},
		"operation partial": {testCredentialUUID(1), testCredentialUUID(202)},
		"command partial":   {testCredentialUUID(2), testCredentialUUID(101)},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
			readyCredential(t, fixture, 1)
			deletion := prepareReadyDeletion(t, fixture, 500)
			markerID, _ := replayObjectID(fixture.binding)
			barrier := &markerResolveBarrierStore{SecretStore: fixture.store, markerID: markerID, captured: make(chan struct{}), release: make(chan struct{})}
			reserver, err := newCredentialAdapters(barrier, fixture.cipher, "vault-key-v1", func() time.Time { return fixture.now })
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				_, err := reserver.Vault.ReserveRotation(t.Context(), fixture.binding, coordinates[0], coordinates[1])
				result <- err
			}()
			<-barrier.captured
			if err := fixture.adapters.Vault.Delete(t.Context(), deletion); err != nil {
				t.Fatal(err)
			}
			close(barrier.release)
			if err := <-result; !errors.Is(err, identity.ErrCredentialConflict) {
				t.Fatalf("reservation err=%v", err)
			}
			if _, exists, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding); err != nil || exists {
				t.Fatalf("stale reservation main exists=%v err=%v", exists, err)
			}
			marker, exists, err := fixture.vault().loadMarker(t.Context(), fixture.binding)
			if err != nil || !exists || marker.body.OperationID != testCredentialUUID(1) || marker.body.CommandID != testCredentialUUID(101) {
				t.Fatalf("marker=%+v exists=%v err=%v", marker, exists, err)
			}
		})
	}
}

func TestRenewedMaintenanceLeaseCompletesAfterMarkerPersistedDeleteFailure(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	state := prepareAbandonedMaintenance(t, fixture, 1)
	before, _, _ := fixture.vault().loadMain(t.Context(), fixture.binding)
	markerID, _ := replayObjectID(fixture.binding)
	faults := &faultSecretStore{memorySecretStore: fixture.store}
	faults.setFault("delete-main", fixture.binding.ObjectID, markerID, false, 1)
	fixture.vault().store = faults
	if _, err := fixture.adapters.Vault.CompleteMaintenance(t.Context(), fixture.binding, state); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("first complete err=%v", err)
	}
	fixture.now = fixture.now.Add(2 * time.Minute)
	reopened, err := newCredentialAdapters(fixture.store, fixture.cipher, "vault-key-v1", func() time.Time { return fixture.now })
	if err != nil {
		t.Fatal(err)
	}
	state, exists, err := reopened.Vault.Resolve(t.Context(), fixture.binding)
	if err != nil || !exists || state.Phase != "aborting" {
		t.Fatalf("state=%+v exists=%v err=%v", state, exists, err)
	}
	state, err = reopened.Vault.ClaimMaintenance(t.Context(), fixture.binding, state, "renewed-worker", fixture.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	afterClaim, _, _ := reopened.Vault.(*vault).loadMain(t.Context(), fixture.binding)
	if afterClaim.sequence <= before.sequence {
		t.Fatalf("renewed lease did not advance main: before=%d after=%d", before.sequence, afterClaim.sequence)
	}
	if _, err := reopened.Vault.CompleteMaintenance(t.Context(), fixture.binding, state); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := reopened.Vault.Resolve(t.Context(), fixture.binding); err != nil || exists {
		t.Fatalf("main exists=%v err=%v", exists, err)
	}
}

func TestReservationRetryRejectsStaleMainAfterRollbackFailure(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	readyCredential(t, fixture, 1)
	deletion := prepareReadyDeletion(t, fixture, 500)
	markerID, _ := replayObjectID(fixture.binding)
	barrier := &markerResolveBarrierStore{SecretStore: fixture.store, markerID: markerID, captured: make(chan struct{}), release: make(chan struct{})}
	failing := &rollbackFailBarrierStore{markerResolveBarrierStore: barrier, mainID: fixture.binding.ObjectID, failRollback: true}
	reserver, err := newCredentialAdapters(failing, fixture.cipher, "vault-key-v1", func() time.Time { return fixture.now })
	if err != nil {
		t.Fatal(err)
	}
	operationID, commandID := testCredentialUUID(2), testCredentialUUID(202)
	result := make(chan error, 1)
	go func() {
		_, err := reserver.Vault.ReserveRotation(t.Context(), fixture.binding, operationID, commandID)
		result <- err
	}()
	<-barrier.captured
	if err := fixture.adapters.Vault.Delete(t.Context(), deletion); err != nil {
		t.Fatal(err)
	}
	close(barrier.release)
	if err := <-result; !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("first reservation err=%v", err)
	}
	if _, exists, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding); err != nil || !exists {
		t.Fatalf("expected stale reserved main after rollback failure: exists=%v err=%v", exists, err)
	}
	reopened, err := newCredentialAdapters(fixture.store, fixture.cipher, "vault-key-v1", func() time.Time { return fixture.now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Vault.ReserveRotation(t.Context(), fixture.binding, operationID, commandID); !errors.Is(err, identity.ErrCredentialConflict) {
		t.Fatalf("retry accepted stale history: %v", err)
	}
	if _, exists, err := reopened.Vault.Resolve(t.Context(), fixture.binding); err != nil || exists {
		t.Fatalf("stale main not reconciled: exists=%v err=%v", exists, err)
	}
	marker, exists, err := reopened.Vault.(*vault).loadMarker(t.Context(), fixture.binding)
	if err != nil || !exists || marker.body.OperationID != testCredentialUUID(1) || marker.body.CommandID != testCredentialUUID(101) {
		t.Fatalf("marker=%+v exists=%v err=%v", marker, exists, err)
	}
}

func TestTwoAbsentMarkerReserversCannotReuseStaleCreatedMain(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	readyCredential(t, fixture, 1)
	deletion := prepareReadyDeletion(t, fixture, 500)
	markerID, _ := replayObjectID(fixture.binding)
	one := newDualMarkerBarrierStore(fixture.store, markerID)
	two := newDualMarkerBarrierStore(fixture.store, markerID)
	reserverOne, _ := newCredentialAdapters(one, fixture.cipher, "vault-key-v1", func() time.Time { return fixture.now })
	reserverTwo, _ := newCredentialAdapters(two, fixture.cipher, "vault-key-v1", func() time.Time { return fixture.now })
	operationID, commandID := testCredentialUUID(2), testCredentialUUID(202)
	results := make(chan error, 2)
	go func() {
		_, err := reserverOne.Vault.ReserveRotation(t.Context(), fixture.binding, operationID, commandID)
		results <- err
	}()
	go func() {
		_, err := reserverTwo.Vault.ReserveRotation(t.Context(), fixture.binding, operationID, commandID)
		results <- err
	}()
	<-one.firstCaptured
	<-two.firstCaptured
	if err := fixture.adapters.Vault.Delete(t.Context(), deletion); err != nil {
		t.Fatal(err)
	}
	close(one.firstRelease)
	<-one.secondCaptured
	close(two.firstRelease)
	<-two.secondCaptured
	close(two.secondRelease)
	if err := <-results; !errors.Is(err, identity.ErrCredentialConflict) {
		t.Fatalf("second reserver err=%v", err)
	}
	close(one.secondRelease)
	if err := <-results; !errors.Is(err, identity.ErrCredentialConflict) {
		t.Fatalf("first reserver err=%v", err)
	}
	if _, exists, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding); err != nil || exists {
		t.Fatalf("stale main exists=%v err=%v", exists, err)
	}
}

func TestRepeatedLeaseRenewalsRaiseMarkerHighWaterBeforeReclaim(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	state := prepareAbandonedMaintenance(t, fixture, 1)
	var lastMainSequence uint64
	for round := 0; round < 3; round++ {
		main, _, _ := fixture.vault().loadMain(t.Context(), fixture.binding)
		lastMainSequence = main.sequence
		markerID, _ := replayObjectID(fixture.binding)
		faults := &faultSecretStore{memorySecretStore: fixture.store}
		faults.setFault("delete-main", fixture.binding.ObjectID, markerID, false, 1)
		fixture.vault().store = faults
		if _, err := fixture.adapters.Vault.CompleteMaintenance(t.Context(), fixture.binding, state); !errors.Is(err, identity.ErrUnavailable) {
			t.Fatalf("round %d err=%v", round, err)
		}
		marker, exists, err := fixture.vault().loadMarker(t.Context(), fixture.binding)
		if err != nil || !exists || marker.sequence != main.sequence+1 {
			t.Fatalf("round %d marker=%+v exists=%v main=%d err=%v", round, marker, exists, main.sequence, err)
		}
		fixture.now = fixture.now.Add(2 * time.Minute)
		reopened, err := newCredentialAdapters(fixture.store, fixture.cipher, "vault-key-v1", func() time.Time { return fixture.now })
		if err != nil {
			t.Fatal(err)
		}
		fixture.adapters = reopened
		state, exists, err = reopened.Vault.Resolve(t.Context(), fixture.binding)
		if err != nil || !exists {
			t.Fatalf("round %d state=%+v exists=%v err=%v", round, state, exists, err)
		}
		state, err = reopened.Vault.ClaimMaintenance(t.Context(), fixture.binding, state, fmt.Sprintf("renew-worker-%d", round), fixture.now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
	}
	finalMain, _, _ := fixture.vault().loadMain(t.Context(), fixture.binding)
	if finalMain.sequence <= lastMainSequence {
		t.Fatalf("final main sequence=%d last=%d", finalMain.sequence, lastMainSequence)
	}
	if _, err := fixture.adapters.Vault.CompleteMaintenance(t.Context(), fixture.binding, state); err != nil {
		t.Fatal(err)
	}
	marker, exists, err := fixture.vault().loadMarker(t.Context(), fixture.binding)
	if err != nil || !exists || marker.sequence != finalMain.sequence+1 {
		t.Fatalf("final marker=%+v exists=%v main=%d err=%v", marker, exists, finalMain.sequence, err)
	}
	if _, err := fixture.adapters.Vault.ReserveRotation(t.Context(), fixture.binding, testCredentialUUID(2), testCredentialUUID(202)); err != nil {
		t.Fatal(err)
	}
	reclaimed, _, _ := fixture.vault().loadMain(t.Context(), fixture.binding)
	if reclaimed.sequence <= marker.sequence || reclaimed.sequence <= finalMain.sequence {
		t.Fatalf("reclaimed=%d marker=%d deleted-main=%d", reclaimed.sequence, marker.sequence, finalMain.sequence)
	}
}

func TestMaintenanceRemovesUnacceptedAbortingMainAfterRollbackFailure(t *testing.T) {
	fixture := newCredentialFixture(t, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	readyCredential(t, fixture, 1)
	deletion := prepareReadyDeletion(t, fixture, 500)
	markerID, _ := replayObjectID(fixture.binding)
	barrier := &markerResolveBarrierStore{SecretStore: fixture.store, markerID: markerID, captured: make(chan struct{}), release: make(chan struct{})}
	failing := &rollbackFailBarrierStore{markerResolveBarrierStore: barrier, mainID: fixture.binding.ObjectID, failRollback: true}
	reserver, err := newCredentialAdapters(failing, fixture.cipher, "vault-key-v1", func() time.Time { return fixture.now })
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := reserver.Vault.ReserveRotation(t.Context(), fixture.binding, testCredentialUUID(2), testCredentialUUID(202))
		result <- err
	}()
	<-barrier.captured
	if err := fixture.adapters.Vault.Delete(t.Context(), deletion); err != nil {
		t.Fatal(err)
	}
	close(barrier.release)
	if err := <-result; !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("reservation err=%v", err)
	}
	markerBefore, exists, err := fixture.vault().loadMarker(t.Context(), fixture.binding)
	if err != nil || !exists {
		t.Fatalf("marker=%+v exists=%v err=%v", markerBefore, exists, err)
	}
	fixture.now = fixture.now.Add(rotationTTL + time.Second)
	reopened, err := newCredentialAdapters(fixture.store, fixture.cipher, "vault-key-v1", func() time.Time { return fixture.now })
	if err != nil {
		t.Fatal(err)
	}
	state, exists, err := reopened.Vault.Resolve(t.Context(), fixture.binding)
	if err != nil || !exists || state.Phase != "reserved" {
		t.Fatalf("state=%+v exists=%v err=%v", state, exists, err)
	}
	state, err = reopened.Vault.MarkAbandoned(t.Context(), fixture.binding, state)
	if err != nil || state.Phase != "aborting" {
		t.Fatalf("aborting=%+v err=%v", state, err)
	}
	state, err = reopened.Vault.ClaimMaintenance(t.Context(), fixture.binding, state, "stale-cleanup-worker", fixture.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	absent, err := reopened.Vault.CompleteMaintenance(t.Context(), fixture.binding, state)
	if err != nil || absent.Phase != "absent" {
		t.Fatalf("absent=%+v err=%v", absent, err)
	}
	if replay, err := reopened.Vault.CompleteMaintenance(t.Context(), fixture.binding, state); err != nil || replay != absent {
		t.Fatalf("cleanup replay=%+v err=%v", replay, err)
	}
	markerAfter, exists, err := reopened.Vault.(*vault).loadMarker(t.Context(), fixture.binding)
	if err != nil || !exists || markerAfter.sequence != markerBefore.sequence || markerAfter.body != markerBefore.body {
		t.Fatalf("marker changed: before=%+v after=%+v exists=%v err=%v", markerBefore, markerAfter, exists, err)
	}
	if _, err := reopened.Vault.ReserveRotation(t.Context(), fixture.binding, testCredentialUUID(3), testCredentialUUID(203)); err != nil {
		t.Fatalf("fresh reservation: %v", err)
	}
}

type stubSecretStore struct{}

func (*stubSecretStore) Resolve(context.Context, string) (SecretObject, bool, error) {
	return SecretObject{}, false, nil
}
func (*stubSecretStore) Create(context.Context, SecretObject) error { return nil }
func (*stubSecretStore) CompareAndSwap(context.Context, string, string, SecretObject) (bool, error) {
	return false, nil
}
func (*stubSecretStore) Delete(context.Context, string, string) error { return nil }
func (*stubSecretStore) ListMetadata(context.Context, time.Time, string, int) ([]SecretMetadata, string, error) {
	return nil, "", nil
}

type stubCipher struct{}

func (stubCipher) Seal(_ context.Context, _ string, plaintext, _ []byte) ([]byte, error) {
	return append([]byte(nil), plaintext...), nil
}

type panicEnvelopeCipher struct{}

func (panicEnvelopeCipher) Seal(context.Context, string, []byte, []byte) ([]byte, error) {
	panic("Seal called inside authority fence")
}
func (panicEnvelopeCipher) Open(context.Context, string, []byte, []byte) ([]byte, error) {
	panic("Open called inside authority fence")
}

type fencedSecretStore struct {
	base     *memorySecretStore
	casCalls int
}

func (*fencedSecretStore) Resolve(context.Context, string) (SecretObject, bool, error) {
	panic("Resolve called inside authority fence")
}
func (*fencedSecretStore) Create(context.Context, SecretObject) error {
	panic("Create called inside authority fence")
}
func (store *fencedSecretStore) CompareAndSwap(ctx context.Context, id, expected string, object SecretObject) (bool, error) {
	store.casCalls++
	return store.base.CompareAndSwap(ctx, id, expected, object)
}
func (*fencedSecretStore) Delete(context.Context, string, string) error {
	panic("Delete called inside authority fence")
}
func (*fencedSecretStore) ListMetadata(context.Context, time.Time, string, int) ([]SecretMetadata, string, error) {
	panic("ListMetadata called inside authority fence")
}
func (stubCipher) Open(_ context.Context, _ string, ciphertext, _ []byte) ([]byte, error) {
	return append([]byte(nil), ciphertext...), nil
}

type errorSecretStore struct{ err error }

func (store *errorSecretStore) Resolve(context.Context, string) (SecretObject, bool, error) {
	return SecretObject{}, false, store.err
}

type inventoryObjectIDMismatchStore struct {
	SecretStore
	metadataID string
	object     SecretObject
	createdAt  time.Time
}

type markerResolveBarrierStore struct {
	SecretStore
	markerID string
	captured chan struct{}
	release  chan struct{}
	once     sync.Once
}

type rollbackFailBarrierStore struct {
	*markerResolveBarrierStore
	mainID       string
	failRollback bool
}

type dualMarkerBarrierStore struct {
	SecretStore
	markerID       string
	mu             sync.Mutex
	markerCalls    int
	firstCaptured  chan struct{}
	firstRelease   chan struct{}
	secondCaptured chan struct{}
	secondRelease  chan struct{}
}

func newDualMarkerBarrierStore(store SecretStore, markerID string) *dualMarkerBarrierStore {
	return &dualMarkerBarrierStore{
		SecretStore: store, markerID: markerID,
		firstCaptured: make(chan struct{}), firstRelease: make(chan struct{}),
		secondCaptured: make(chan struct{}), secondRelease: make(chan struct{}),
	}
}

func (store *dualMarkerBarrierStore) Resolve(ctx context.Context, id string) (SecretObject, bool, error) {
	object, exists, err := store.SecretStore.Resolve(ctx, id)
	if id != store.markerID {
		return object, exists, err
	}
	store.mu.Lock()
	store.markerCalls++
	call := store.markerCalls
	store.mu.Unlock()
	var captured, release chan struct{}
	switch call {
	case 1:
		captured, release = store.firstCaptured, store.firstRelease
	case 2:
		captured, release = store.secondCaptured, store.secondRelease
	default:
		return object, exists, err
	}
	close(captured)
	select {
	case <-release:
		return object, exists, err
	case <-ctx.Done():
		return SecretObject{}, false, ctx.Err()
	}
}

func (store *rollbackFailBarrierStore) Delete(ctx context.Context, id, expected string) error {
	if id == store.mainID && store.failRollback {
		store.failRollback = false
		return errInjectedProviderTimeout
	}
	return store.SecretStore.Delete(ctx, id, expected)
}

func (store *markerResolveBarrierStore) Resolve(ctx context.Context, id string) (SecretObject, bool, error) {
	object, exists, err := store.SecretStore.Resolve(ctx, id)
	if id == store.markerID {
		blocked := false
		store.once.Do(func() {
			blocked = true
			close(store.captured)
		})
		if blocked {
			select {
			case <-store.release:
			case <-ctx.Done():
				return SecretObject{}, false, ctx.Err()
			}
		}
	}
	return object, exists, err
}

func (store *inventoryObjectIDMismatchStore) Resolve(context.Context, string) (SecretObject, bool, error) {
	return cloneSecretObject(store.object), true, nil
}
func (store *inventoryObjectIDMismatchStore) ListMetadata(context.Context, time.Time, string, int) ([]SecretMetadata, string, error) {
	return []SecretMetadata{{ObjectID: store.metadataID, Version: store.object.Version, KeyID: store.object.KeyID, CreatedAt: store.createdAt}}, "", nil
}
func (store *errorSecretStore) Create(context.Context, SecretObject) error { return store.err }
func (store *errorSecretStore) CompareAndSwap(context.Context, string, string, SecretObject) (bool, error) {
	return false, store.err
}
func (store *errorSecretStore) Delete(context.Context, string, string) error { return store.err }
func (store *errorSecretStore) ListMetadata(context.Context, time.Time, string, int) ([]SecretMetadata, string, error) {
	return nil, "", store.err
}

func literalCredentialFrame(fields ...[]byte) []byte {
	var result []byte
	for _, field := range fields {
		length := len(field)
		result = append(result, byte(length>>24), byte(length>>16), byte(length>>8), byte(length))
		result = append(result, field...)
	}
	return result
}

type memorySecretStore struct {
	mu          sync.Mutex
	objects     map[string]SecretObject
	created     map[string]time.Time
	version     uint64
	now         func() time.Time
	listCalls   int
	listLimits  []int
	listCursors []string
}

type faultSecretStore struct {
	*memorySecretStore
	muFault   sync.Mutex
	method    string
	mainID    string
	markerID  string
	after     bool
	remaining int
}

var errInjectedProviderTimeout = errors.New("provider timeout with secret details")

func (store *faultSecretStore) setFault(method, mainID, markerID string, after bool, remaining int) {
	store.muFault.Lock()
	defer store.muFault.Unlock()
	store.method, store.mainID, store.markerID, store.after, store.remaining = method, mainID, markerID, after, remaining
}

func (store *faultSecretStore) clearFault() {
	store.muFault.Lock()
	defer store.muFault.Unlock()
	store.method, store.remaining = "", 0
}

func (store *faultSecretStore) trips(method, id string, after bool) bool {
	store.muFault.Lock()
	defer store.muFault.Unlock()
	if store.remaining <= 0 || store.after != after || store.method != method {
		return false
	}
	wantID := store.mainID
	if strings.Contains(method, "marker") {
		wantID = store.markerID
	}
	if id != wantID {
		return false
	}
	store.remaining--
	return true
}

func (store *faultSecretStore) Resolve(ctx context.Context, id string) (SecretObject, bool, error) {
	method := "resolve-main"
	if id == store.markerID {
		method = "resolve-marker"
	}
	if store.trips(method, id, false) {
		return SecretObject{}, false, errInjectedProviderTimeout
	}
	object, exists, err := store.memorySecretStore.Resolve(ctx, id)
	if store.trips(method, id, true) {
		return SecretObject{}, false, errInjectedProviderTimeout
	}
	return object, exists, err
}

func (store *faultSecretStore) Create(ctx context.Context, object SecretObject) error {
	method := "create-main"
	if object.ID == store.markerID {
		method = "create-marker"
	}
	if store.trips(method, object.ID, false) {
		return errInjectedProviderTimeout
	}
	err := store.memorySecretStore.Create(ctx, object)
	if store.trips(method, object.ID, true) {
		return errInjectedProviderTimeout
	}
	return err
}

func (store *faultSecretStore) CompareAndSwap(ctx context.Context, id, expected string, object SecretObject) (bool, error) {
	method := "cas-main"
	if id == store.markerID {
		method = "cas-marker"
	}
	if store.trips(method, id, false) {
		return false, errInjectedProviderTimeout
	}
	swapped, err := store.memorySecretStore.CompareAndSwap(ctx, id, expected, object)
	if store.trips(method, id, true) {
		return false, errInjectedProviderTimeout
	}
	return swapped, err
}

func (store *faultSecretStore) Delete(ctx context.Context, id, expected string) error {
	method := "delete-main"
	if id == store.markerID {
		method = "delete-marker"
	}
	if store.trips(method, id, false) {
		return errInjectedProviderTimeout
	}
	err := store.memorySecretStore.Delete(ctx, id, expected)
	if store.trips(method, id, true) {
		return errInjectedProviderTimeout
	}
	return err
}

func newMemorySecretStore(now func() time.Time) *memorySecretStore {
	return &memorySecretStore{objects: map[string]SecretObject{}, created: map[string]time.Time{}, now: now}
}

func (store *memorySecretStore) Resolve(_ context.Context, id string) (SecretObject, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	object, ok := store.objects[id]
	return cloneSecretObject(object), ok, nil
}

func (store *memorySecretStore) Create(_ context.Context, object SecretObject) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.objects[object.ID]; exists {
		return errors.New("object exists")
	}
	store.version++
	object.Version = fmt.Sprintf("v%d", store.version)
	store.objects[object.ID] = cloneSecretObject(object)
	store.created[object.ID] = store.now().UTC()
	return nil
}

func (store *memorySecretStore) CompareAndSwap(_ context.Context, id, expected string, object SecretObject) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	current, exists := store.objects[id]
	if !exists || current.Version != expected {
		return false, nil
	}
	store.version++
	object.Version = fmt.Sprintf("v%d", store.version)
	store.objects[id] = cloneSecretObject(object)
	return true, nil
}

func (store *memorySecretStore) Delete(_ context.Context, id, expected string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	current, exists := store.objects[id]
	if !exists {
		return nil
	}
	if current.Version != expected {
		return errors.New("version conflict")
	}
	delete(store.objects, id)
	delete(store.created, id)
	return nil
}

func (store *memorySecretStore) ListMetadata(_ context.Context, before time.Time, cursor string, limit int) ([]SecretMetadata, string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.listCalls++
	store.listLimits = append(store.listLimits, limit)
	store.listCursors = append(store.listCursors, cursor)
	var ids []string
	for id := range store.objects {
		if (cursor == "" || id > cursor) && !store.created[id].After(before) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	next := ""
	if len(ids) > limit {
		next, ids = ids[limit-1], ids[:limit]
	}
	result := make([]SecretMetadata, 0, len(ids))
	for _, id := range ids {
		object := store.objects[id]
		result = append(result, SecretMetadata{ObjectID: id, Version: object.Version, KeyID: object.KeyID, CreatedAt: store.created[id]})
	}
	return result, next, nil
}

func (store *memorySecretStore) objectCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.objects)
}

func (store *memorySecretStore) resetCalls() {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.listCalls, store.listLimits, store.listCursors = 0, nil, nil
}

func (store *memorySecretStore) listStats() (int, []int, []string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.listCalls, append([]int(nil), store.listLimits...), append([]string(nil), store.listCursors...)
}

func cloneSecretObject(object SecretObject) SecretObject {
	object.Ciphertext = append([]byte(nil), object.Ciphertext...)
	return object
}

type testEnvelopeCipher struct{ keys map[string][]byte }

func (cipherStore *testEnvelopeCipher) Seal(_ context.Context, keyID string, plaintext, aad []byte) ([]byte, error) {
	aead, err := cipherStore.aead(keyID)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return append(nonce, aead.Seal(nil, nonce, plaintext, aad)...), nil
}

func (cipherStore *testEnvelopeCipher) Open(_ context.Context, keyID string, ciphertext, aad []byte) ([]byte, error) {
	aead, err := cipherStore.aead(keyID)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < aead.NonceSize() {
		return nil, errors.New("ciphertext truncated")
	}
	return aead.Open(nil, ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():], aad)
}

func (cipherStore *testEnvelopeCipher) aead(keyID string) (cipher.AEAD, error) {
	key := cipherStore.keys[keyID]
	if len(key) != 32 {
		return nil, errors.New("key unavailable")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

type credentialFixture struct {
	now      time.Time
	store    *memorySecretStore
	cipher   *testEnvelopeCipher
	binding  identity.CredentialBinding
	adapters CredentialAdapters
}

func newCredentialFixture(t *testing.T, accountID string) *credentialFixture {
	t.Helper()
	fixture := &credentialFixture{now: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)}
	fixture.store = newMemorySecretStore(func() time.Time { return fixture.now })
	fixture.cipher = &testEnvelopeCipher{keys: map[string][]byte{"vault-key-v1": bytes.Repeat([]byte{0x44}, 32)}}
	var err error
	fixture.binding, err = identity.NewAppleCredentialBinding(accountID, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	fixture.adapters, err = newCredentialAdapters(fixture.store, fixture.cipher, "vault-key-v1", func() time.Time { return fixture.now })
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *credentialFixture) vault() *vault { return fixture.adapters.Vault.(*vault) }

func (fixture *credentialFixture) mutateMain(t *testing.T, mutate func(*SecretObject)) {
	t.Helper()
	fixture.store.mu.Lock()
	defer fixture.store.mu.Unlock()
	object, exists := fixture.store.objects[fixture.binding.ObjectID]
	if !exists {
		t.Fatal("main credential missing")
	}
	mutate(&object)
	fixture.store.objects[fixture.binding.ObjectID] = object
}

func (fixture *credentialFixture) resealMain(t *testing.T, mutate func(*mainBody)) {
	t.Helper()
	fixture.store.mu.Lock()
	object := cloneSecretObject(fixture.store.objects[fixture.binding.ObjectID])
	fixture.store.mu.Unlock()
	aad, err := credentialAAD(object.ID, object.KeyID, object.Header)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := fixture.cipher.Open(t.Context(), object.KeyID, object.Ciphertext, aad)
	if err != nil {
		t.Fatal(err)
	}
	defer zero(plaintext)
	var body mainBody
	if err := json.Unmarshal(plaintext, &body); err != nil {
		t.Fatal(err)
	}
	mutate(&body)
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	defer zero(encoded)
	ciphertext, err := fixture.cipher.Seal(t.Context(), object.KeyID, encoded, aad)
	if err != nil {
		t.Fatal(err)
	}
	object.Ciphertext, object.SHA256 = ciphertext, sha256.Sum256(ciphertext)
	fixture.store.mu.Lock()
	fixture.store.objects[fixture.binding.ObjectID] = object
	fixture.store.mu.Unlock()
}

func (fixture *credentialFixture) resealMainAtSequence(t *testing.T, sequence uint64) {
	t.Helper()
	fixture.store.mu.Lock()
	object := cloneSecretObject(fixture.store.objects[fixture.binding.ObjectID])
	fixture.store.mu.Unlock()
	oldAAD, err := credentialAAD(object.ID, object.KeyID, object.Header)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := fixture.cipher.Open(t.Context(), object.KeyID, object.Ciphertext, oldAAD)
	if err != nil {
		t.Fatal(err)
	}
	defer zero(plaintext)
	object.Header.EnvelopeSequence = sequence
	var encodedSequence [8]byte
	binary.BigEndian.PutUint64(encodedSequence[:], sequence)
	newAAD := literalCredentialFrame(
		[]byte(credentialAADDomain), []byte(object.ID), []byte(object.KeyID),
		[]byte(object.Header.Schema), encodedSequence[:],
	)
	ciphertext, err := fixture.cipher.Seal(t.Context(), object.KeyID, plaintext, newAAD)
	if err != nil {
		t.Fatal(err)
	}
	object.Ciphertext, object.SHA256 = ciphertext, sha256.Sum256(ciphertext)
	fixture.store.mu.Lock()
	fixture.store.objects[fixture.binding.ObjectID] = object
	fixture.store.mu.Unlock()
}

func (fixture *credentialFixture) resealMarkerAtSequence(t *testing.T, sequence uint64) {
	t.Helper()
	markerID, err := replayObjectID(fixture.binding)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.mu.Lock()
	object := cloneSecretObject(fixture.store.objects[markerID])
	fixture.store.mu.Unlock()
	oldAAD, err := credentialAAD(object.ID, object.KeyID, object.Header)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := fixture.cipher.Open(t.Context(), object.KeyID, object.Ciphertext, oldAAD)
	if err != nil {
		t.Fatal(err)
	}
	defer zero(plaintext)
	var body markerBody
	if err := json.Unmarshal(plaintext, &body); err != nil {
		t.Fatal(err)
	}
	body.EnvelopeSequence = sequence
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	defer zero(encoded)
	object.Header.EnvelopeSequence = sequence
	var encodedSequence [8]byte
	binary.BigEndian.PutUint64(encodedSequence[:], sequence)
	newAAD := literalCredentialFrame(
		[]byte(credentialAADDomain), []byte(object.ID), []byte(object.KeyID),
		[]byte(object.Header.Schema), encodedSequence[:],
	)
	ciphertext, err := fixture.cipher.Seal(t.Context(), object.KeyID, encoded, newAAD)
	if err != nil {
		t.Fatal(err)
	}
	object.Ciphertext, object.SHA256 = ciphertext, sha256.Sum256(ciphertext)
	fixture.store.mu.Lock()
	fixture.store.objects[markerID] = object
	fixture.store.mu.Unlock()
}

func reserveCredential(t *testing.T, vault identity.CredentialVault, binding identity.CredentialBinding, index int) identity.CredentialRotationState {
	t.Helper()
	state, err := vault.ReserveRotation(t.Context(), binding, testCredentialUUID(index), testCredentialUUID(100+index))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func readyCredential(t *testing.T, fixture *credentialFixture, index int) identity.CredentialRotationState {
	t.Helper()
	state := reserveCredential(t, fixture.adapters.Vault, fixture.binding, index)
	var err error
	state, err = fixture.adapters.Vault.StoreCandidate(t.Context(), fixture.binding, state, []byte(fmt.Sprintf("refresh-%d", index)))
	if err != nil {
		t.Fatal(err)
	}
	state, err = fixture.adapters.Vault.PromotePending(t.Context(), fixture.binding, state)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := fixture.adapters.Vault.PreparePromotionCAS(t.Context(), fixture.binding, state, sha256.Sum256([]byte(fmt.Sprintf("commit-%d", index))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.adapters.Vault.ConfirmPromotion(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	state, exists, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding)
	if err != nil || !exists {
		t.Fatalf("resolve adopting: exists=%v err=%v", exists, err)
	}
	state, err = fixture.adapters.Vault.MarkCommitted(t.Context(), fixture.binding, state)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func runAdoptionCycle(t *testing.T, fixture *credentialFixture, index int, finish bool) {
	t.Helper()
	state := reserveCredential(t, fixture.adapters.Vault, fixture.binding, index)
	var err error
	state, err = fixture.adapters.Vault.StoreCandidate(t.Context(), fixture.binding, state, []byte(fmt.Sprintf("refresh-%d", index)))
	if err != nil {
		t.Fatal(err)
	}
	if state.HasCurrent {
		state, err = fixture.adapters.Vault.MarkPriorRevoked(t.Context(), fixture.binding, state)
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err = fixture.adapters.Vault.PromotePending(t.Context(), fixture.binding, state)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := fixture.adapters.Vault.PreparePromotionCAS(t.Context(), fixture.binding, state, sha256.Sum256([]byte(fmt.Sprintf("commit-%d", index))))
	if err != nil {
		t.Fatal(err)
	}
	capability, err := fixture.adapters.Vault.ConfirmPromotion(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if finish {
		err = fixture.adapters.Vault.FinishAdoption(t.Context(), fixture.binding, capability)
	} else {
		err = fixture.adapters.Vault.AbandonAdoption(t.Context(), fixture.binding, capability)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func deleteReadyCredential(t *testing.T, fixture *credentialFixture, coordinate int) {
	t.Helper()
	capability := prepareReadyDeletion(t, fixture, coordinate)
	if err := fixture.adapters.Vault.Delete(t.Context(), capability); err != nil {
		t.Fatal(err)
	}
}

func prepareReadyDeletion(t *testing.T, fixture *credentialFixture, coordinate int) identity.DeletionCapability {
	t.Helper()
	state, exists, err := fixture.adapters.Vault.Resolve(t.Context(), fixture.binding)
	if err != nil || !exists {
		t.Fatalf("resolve ready: exists=%v err=%v", exists, err)
	}
	intent, err := fixture.adapters.Vault.PrepareDeletionCAS(t.Context(), fixture.binding, state, identity.DeletionOperation{
		OperationID: testCredentialUUID(coordinate), CommandID: testCredentialUUID(coordinate + 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := fixture.adapters.Vault.MarkDeleting(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	capability, err = fixture.adapters.Vault.EraseDeletingTokens(t.Context(), capability)
	if err != nil {
		t.Fatal(err)
	}
	return capability
}

func deleteAbandonedCredential(t *testing.T, fixture *credentialFixture, index int) {
	t.Helper()
	state := prepareAbandonedMaintenance(t, fixture, index)
	if _, err := fixture.adapters.Vault.CompleteMaintenance(t.Context(), fixture.binding, state); err != nil {
		t.Fatal(err)
	}
}

func prepareAbandonedMaintenance(t *testing.T, fixture *credentialFixture, index int) identity.CredentialRotationState {
	t.Helper()
	state := reserveCredential(t, fixture.adapters.Vault, fixture.binding, index)
	state, err := fixture.adapters.Vault.MarkAbandoned(t.Context(), fixture.binding, state)
	if err != nil {
		t.Fatal(err)
	}
	state, err = fixture.adapters.Vault.ClaimMaintenance(t.Context(), fixture.binding, state, "test-worker", fixture.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func requireCredentialConflict(t *testing.T, call func() error) {
	t.Helper()
	if err := call(); !errors.Is(err, identity.ErrCredentialConflict) {
		t.Fatalf("err=%v, want credential_conflict", err)
	}
}

func testCredentialUUID(index int) string {
	return fmt.Sprintf("70000000-0000-4000-8000-%012d", index)
}

func mustPromotionIntent(t *testing.T, opaque []byte, expiresAt, now time.Time) identity.PromotionCASIntent {
	t.Helper()
	intent, err := identity.NewPromotionCASIntent(opaque, expiresAt, now)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}
