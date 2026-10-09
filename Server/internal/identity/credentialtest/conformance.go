// Package credentialtest provides a black-box conformance suite for
// CredentialVault implementations.
package credentialtest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"babytracker/server/internal/identity"
)

type Harness struct {
	Vault     identity.CredentialVault
	Inventory identity.CredentialInventory
	Now       func() time.Time
	Advance   func(time.Duration)
	// Reopen returns a distinct adapter instance over the same durable backing
	// store. It must not copy state from the existing Vault in memory.
	Reopen func(*testing.T) (identity.CredentialVault, identity.CredentialInventory)
}

type Factory func(*testing.T) Harness

// Run exercises only the public vault/rotator contracts. Provider-specific
// fault injection belongs in the implementation's own package tests.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("monotonic rotation", func(t *testing.T) {
		harness := requireHarness(t, factory)
		binding := testBinding(t)
		rotator := testRotator(t, harness.Vault)
		finishRotation(t, rotator, binding, 1)

		reserved, err := harness.Vault.ReserveRotation(t.Context(), binding, testUUID(2), testUUID(102))
		if err != nil {
			t.Fatal(err)
		}
		rotationDeadline := requireReconcileAfter(t, reserved)
		assertPublicMarkerFieldsZero(t, reserved)
		reserved = reopenNonterminal(t, &harness, binding, reserved.Phase, rotationDeadline)
		candidate, err := harness.Vault.StoreCandidate(t.Context(), binding, reserved, []byte("refresh-2"))
		if err != nil {
			t.Fatal(err)
		}
		assertSameReconcileAfter(t, candidate, rotationDeadline)
		assertPublicMarkerFieldsZero(t, candidate)
		candidate = reopenNonterminal(t, &harness, binding, candidate.Phase, rotationDeadline)
		retiring, err := harness.Vault.MarkPriorRevoked(t.Context(), binding, candidate)
		if err != nil {
			t.Fatal(err)
		}
		assertSameReconcileAfter(t, retiring, rotationDeadline)
		assertPublicMarkerFieldsZero(t, retiring)
		retiring = reopenNonterminal(t, &harness, binding, retiring.Phase, rotationDeadline)
		if replay, err := harness.Vault.MarkPriorRevoked(t.Context(), binding, candidate); err != nil || replay != retiring {
			t.Fatalf("prior replay=%+v err=%v", replay, err)
		}
		promoted, err := harness.Vault.PromotePending(t.Context(), binding, retiring)
		if err != nil {
			t.Fatal(err)
		}
		assertSameReconcileAfter(t, promoted, rotationDeadline)
		assertPublicMarkerFieldsZero(t, promoted)
		promoted = reopenNonterminal(t, &harness, binding, promoted.Phase, rotationDeadline)
		if replay, err := harness.Vault.PromotePending(t.Context(), binding, retiring); err != nil || replay != promoted {
			t.Fatalf("promote replay=%+v err=%v", replay, err)
		}
		intent, err := harness.Vault.PreparePromotionCAS(t.Context(), binding, promoted, sha256.Sum256([]byte("commit")))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := harness.Vault.ConfirmPromotion(t.Context(), intent); err != nil {
			t.Fatal(err)
		}
		adopting, exists, err := harness.Vault.Resolve(t.Context(), binding)
		if err != nil || !exists || adopting.Phase != "adopting" {
			t.Fatalf("adopting=%+v exists=%v err=%v", adopting, exists, err)
		}
		assertSameReconcileAfter(t, adopting, intent.ExpiresAt())
		reopenHarness(t, &harness)
		adoptingAfterRestart, exists, err := harness.Vault.Resolve(t.Context(), binding)
		if err != nil || !exists || adoptingAfterRestart.ReconcileAfter != adopting.ReconcileAfter {
			t.Fatalf("reopened adopting=%+v exists=%v err=%v", adoptingAfterRestart, exists, err)
		}
		adopting = adoptingAfterRestart
		assertPublicMarkerFieldsZero(t, adopting)
		ready, err := harness.Vault.MarkCommitted(t.Context(), binding, adopting)
		if err != nil {
			t.Fatal(err)
		}
		if !ready.ReconcileAfter.IsZero() {
			t.Fatalf("ready retained reconcile_after: %+v", ready)
		}
		assertPublicMarkerFieldsZero(t, ready)
		if replay, err := harness.Vault.MarkCommitted(t.Context(), binding, adopting); err != nil || replay != ready {
			t.Fatalf("commit replay=%+v err=%v", replay, err)
		}
		wrong := adopting
		wrong.Version = "stale"
		if _, err := harness.Vault.MarkCommitted(t.Context(), binding, wrong); err == nil {
			t.Fatal("mismatched prior state replayed")
		}
		reopenHarness(t, &harness)
		reopened, exists, err := harness.Vault.Resolve(t.Context(), binding)
		if err != nil || !exists || reopened.Phase != "ready" {
			t.Fatalf("reopened ready=%+v exists=%v err=%v", reopened, exists, err)
		}
		assertPublicMarkerFieldsZero(t, reopened)
	})

	t.Run("separate abandonment replay marker", func(t *testing.T) {
		harness := requireHarness(t, factory)
		binding := testBinding(t)
		rotator := testRotator(t, harness.Vault)
		operationID, commandID := testUUID(1), testUUID(101)
		exchanges := 0
		_, err := rotator.RotateForSession(t.Context(), binding, operationID, commandID, func(context.Context) (identity.AppleTokens, error) {
			exchanges++
			return identity.AppleTokens{}, errors.New("exchange failed")
		})
		if err == nil || exchanges != 1 {
			t.Fatalf("initial err=%v exchanges=%d", err, exchanges)
		}
		aborting, exists, err := harness.Vault.Resolve(t.Context(), binding)
		if err != nil || !exists || aborting.Phase != "aborting" || aborting.HasCurrent || aborting.HasPending {
			t.Fatalf("aborting=%+v exists=%v err=%v", aborting, exists, err)
		}
		abortingDeadline := requireReconcileAfter(t, aborting)
		assertPublicMarkerFieldsZero(t, aborting)
		aborting = reopenNonterminal(t, &harness, binding, aborting.Phase, abortingDeadline)
		claimed, err := harness.Vault.ClaimMaintenance(t.Context(), binding, aborting, "conformance-worker", harness.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		assertPublicMarkerFieldsZero(t, claimed)
		tokens, err := harness.Vault.OpenMaintenanceTokens(t.Context(), binding, claimed)
		if err != nil || len(tokens) != 0 {
			t.Fatalf("tokens=%q err=%v", tokens, err)
		}
		absent, err := harness.Vault.CompleteMaintenance(t.Context(), binding, claimed)
		if err != nil || absent.Phase != "absent" || absent.HasCurrent || absent.HasPending {
			t.Fatalf("absent=%+v err=%v", absent, err)
		}
		assertPublicMarkerFieldsZero(t, absent)
		if replay, err := harness.Vault.CompleteMaintenance(t.Context(), binding, claimed); err != nil || replay != absent {
			t.Fatalf("maintenance replay=%+v err=%v", replay, err)
		}
		resolved, exists, err := harness.Vault.Resolve(t.Context(), binding)
		if err != nil || exists || resolved.Phase != "absent" {
			t.Fatalf("resolved=%+v exists=%v err=%v", resolved, exists, err)
		}
		if harness.Inventory != nil {
			metadata, cursor, err := harness.Inventory.List(t.Context(), harness.Now().Add(time.Hour), "", 10)
			if err != nil || cursor != "" || len(metadata) != 0 {
				t.Fatalf("metadata=%+v cursor=%q err=%v", metadata, cursor, err)
			}
		}
		if _, err := harness.Vault.ClaimMaintenance(t.Context(), binding, claimed, "conformance-worker", harness.Now().Add(2*time.Minute)); err == nil {
			t.Fatal("maintenance claim recreated absent credential")
		}
		resolvedAfterClaim, existsAfterClaim, err := harness.Vault.Resolve(t.Context(), binding)
		if err != nil || existsAfterClaim || resolvedAfterClaim != resolved {
			t.Fatalf("after claim=%+v exists=%v err=%v", resolvedAfterClaim, existsAfterClaim, err)
		}
		reopenAbsentHarness(t, &harness, binding)
		rotator = testRotator(t, harness.Vault)
		for name, identifiers := range map[string][2]string{
			"exact":          {operationID, commandID},
			"operation only": {operationID, testUUID(202)},
			"command only":   {testUUID(2), commandID},
		} {
			t.Run(name, func(t *testing.T) {
				_, err := rotator.RotateForSession(t.Context(), binding, identifiers[0], identifiers[1], func(context.Context) (identity.AppleTokens, error) {
					exchanges++
					return identity.AppleTokens{IdentityToken: "unexpected", RefreshToken: "unexpected"}, nil
				})
				if err == nil {
					t.Fatal("abandoned identifiers accepted")
				}
			})
		}
		if exchanges != 1 {
			t.Fatalf("replay exchanges=%d", exchanges)
		}
		freshOperationID, freshCommandID := testUUID(2), testUUID(202)
		reserved, err := harness.Vault.ReserveRotation(t.Context(), binding, freshOperationID, freshCommandID)
		if err != nil {
			t.Fatal(err)
		}
		assertPublicMarkerFieldsZero(t, reserved)
		for name, operation := range map[string]identity.DeletionOperation{
			"stale pair":      {OperationID: operationID, CommandID: commandID},
			"stale operation": {OperationID: operationID, CommandID: testUUID(501)},
			"stale command":   {OperationID: testUUID(500), CommandID: commandID},
		} {
			t.Run("deletion "+name, func(t *testing.T) {
				if _, err := harness.Vault.PrepareDeletionCAS(t.Context(), binding, reserved, operation); err == nil {
					t.Fatalf("stale deletion coordinates accepted: %+v", operation)
				}
			})
		}
		if _, err := harness.Vault.PrepareDeletionCAS(t.Context(), binding, reserved, identity.DeletionOperation{OperationID: testUUID(500), CommandID: testUUID(501)}); err != nil {
			t.Fatalf("fresh deletion coordinates: %v", err)
		}
		promotion, err := rotator.RotateForSession(t.Context(), binding, testUUID(2), testUUID(202), func(context.Context) (identity.AppleTokens, error) {
			exchanges++
			return identity.AppleTokens{IdentityToken: "identity-2", RefreshToken: "refresh-2"}, nil
		})
		if err != nil || promotion.Ref.ID != binding.ObjectID || exchanges != 2 {
			t.Fatalf("fresh promotion=%+v err=%v exchanges=%d", promotion, err, exchanges)
		}
		if harness.Inventory != nil {
			metadata, cursor, err := harness.Inventory.List(t.Context(), harness.Now().Add(time.Hour), "", 10)
			if err != nil || cursor != "" || len(metadata) != 1 || metadata[0].Binding != binding {
				t.Fatalf("metadata=%+v cursor=%q err=%v", metadata, cursor, err)
			}
		}
	})

	t.Run("completed deletion preserves replay marker", func(t *testing.T) {
		harness := requireHarness(t, factory)
		binding := testBinding(t)
		rotator := testRotator(t, harness.Vault)
		operationID, commandID := testUUID(3), testUUID(103)
		finishRotation(t, rotator, binding, 3)
		ready, exists, err := harness.Vault.Resolve(t.Context(), binding)
		if err != nil || !exists || ready.Phase != "ready" {
			t.Fatalf("ready=%+v exists=%v err=%v", ready, exists, err)
		}
		assertPublicMarkerFieldsZero(t, ready)
		intent, err := harness.Vault.PrepareDeletionCAS(t.Context(), binding, ready, identity.DeletionOperation{OperationID: testUUID(500), CommandID: testUUID(501)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := harness.Vault.MarkDeleting(t.Context(), intent); err != nil {
			t.Fatal(err)
		}
		capability := reopenDeletingAndRecover(t, &harness, binding)
		capability, err = harness.Vault.EraseDeletingTokens(t.Context(), capability)
		if err != nil {
			t.Fatal(err)
		}
		if err := harness.Vault.Delete(t.Context(), capability); err != nil {
			t.Fatal(err)
		}
		if state, exists, err := harness.Vault.Resolve(t.Context(), binding); err != nil || exists || state.Phase != "absent" {
			t.Fatalf("after delete state=%+v exists=%v err=%v", state, exists, err)
		}
		reopenAbsentHarness(t, &harness, binding)
		rotator = testRotator(t, harness.Vault)
		exchanges := 0
		for name, identifiers := range map[string][2]string{
			"exact":          {operationID, commandID},
			"operation only": {operationID, testUUID(204)},
			"command only":   {testUUID(4), commandID},
		} {
			t.Run(name, func(t *testing.T) {
				_, err := rotator.RotateForSession(t.Context(), binding, identifiers[0], identifiers[1], func(context.Context) (identity.AppleTokens, error) {
					exchanges++
					return identity.AppleTokens{IdentityToken: "unexpected", RefreshToken: "unexpected"}, nil
				})
				if err == nil {
					t.Fatal("deleted credential replay accepted")
				}
			})
		}
		if exchanges != 0 {
			t.Fatalf("replay exchanges=%d", exchanges)
		}
		freshOperationID, freshCommandID := testUUID(4), testUUID(204)
		reserved, err := harness.Vault.ReserveRotation(t.Context(), binding, freshOperationID, freshCommandID)
		if err != nil {
			t.Fatal(err)
		}
		assertPublicMarkerFieldsZero(t, reserved)
		for name, operation := range map[string]identity.DeletionOperation{
			"stale pair":      {OperationID: operationID, CommandID: commandID},
			"stale operation": {OperationID: operationID, CommandID: testUUID(601)},
			"stale command":   {OperationID: testUUID(600), CommandID: commandID},
		} {
			t.Run("deletion "+name, func(t *testing.T) {
				if _, err := harness.Vault.PrepareDeletionCAS(t.Context(), binding, reserved, operation); err == nil {
					t.Fatalf("stale deletion coordinates accepted: %+v", operation)
				}
			})
		}
		if _, err := harness.Vault.PrepareDeletionCAS(t.Context(), binding, reserved, identity.DeletionOperation{OperationID: testUUID(600), CommandID: testUUID(601)}); err != nil {
			t.Fatalf("fresh deletion coordinates: %v", err)
		}
		promotion, err := rotator.RotateForSession(t.Context(), binding, testUUID(4), testUUID(204), func(context.Context) (identity.AppleTokens, error) {
			exchanges++
			return identity.AppleTokens{IdentityToken: "identity-4", RefreshToken: "refresh-4"}, nil
		})
		if err != nil || promotion.Ref.ID != binding.ObjectID || exchanges != 1 {
			t.Fatalf("fresh promotion=%+v err=%v exchanges=%d", promotion, err, exchanges)
		}
	})

	for _, phase := range []string{"reserved", "candidate"} {
		t.Run("initial "+phase+" deletion preserves superseded rotation", func(t *testing.T) {
			harness := requireHarness(t, factory)
			binding := testBinding(t)
			operationID, commandID := testUUID(5), testUUID(105)
			state, err := harness.Vault.ReserveRotation(t.Context(), binding, operationID, commandID)
			if err != nil {
				t.Fatal(err)
			}
			assertPublicMarkerFieldsZero(t, state)
			if phase == "candidate" {
				state, err = harness.Vault.StoreCandidate(t.Context(), binding, state, []byte("refresh-5"))
				if err != nil {
					t.Fatal(err)
				}
				assertPublicMarkerFieldsZero(t, state)
			}
			intent, err := harness.Vault.PrepareDeletionCAS(t.Context(), binding, state, identity.DeletionOperation{OperationID: testUUID(700), CommandID: testUUID(701)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := harness.Vault.MarkDeleting(t.Context(), intent); err != nil {
				t.Fatal(err)
			}
			capability := reopenDeletingAndRecover(t, &harness, binding)
			capability, err = harness.Vault.EraseDeletingTokens(t.Context(), capability)
			if err != nil {
				t.Fatal(err)
			}
			if err := harness.Vault.Delete(t.Context(), capability); err != nil {
				t.Fatal(err)
			}
			reopenAbsentHarness(t, &harness, binding)
			rotator := testRotator(t, harness.Vault)
			exchanges := 0
			for name, identifiers := range map[string][2]string{
				"exact":          {operationID, commandID},
				"operation only": {operationID, testUUID(206)},
				"command only":   {testUUID(6), commandID},
			} {
				t.Run(name, func(t *testing.T) {
					_, err := rotator.RotateForSession(t.Context(), binding, identifiers[0], identifiers[1], func(context.Context) (identity.AppleTokens, error) {
						exchanges++
						return identity.AppleTokens{IdentityToken: "unexpected", RefreshToken: "unexpected"}, nil
					})
					if err == nil {
						t.Fatal("superseded rotation replay accepted")
					}
				})
			}
			if exchanges != 0 {
				t.Fatalf("replay exchanges=%d", exchanges)
			}
			// The deletion operation is not replay history. Only the superseded
			// rotation coordinates are retained when the credential is reclaimed.
			fresh, err := harness.Vault.ReserveRotation(t.Context(), binding, testUUID(700), testUUID(701))
			if err != nil {
				t.Fatalf("deletion coordinates incorrectly retained as replay history: %v", err)
			}
			assertPublicMarkerFieldsZero(t, fresh)
			for name, operation := range map[string]identity.DeletionOperation{
				"stale pair":      {OperationID: operationID, CommandID: commandID},
				"stale operation": {OperationID: operationID, CommandID: testUUID(801)},
				"stale command":   {OperationID: testUUID(800), CommandID: commandID},
			} {
				t.Run("deletion "+name, func(t *testing.T) {
					if _, err := harness.Vault.PrepareDeletionCAS(t.Context(), binding, fresh, operation); err == nil {
						t.Fatalf("superseded deletion coordinates accepted: %+v", operation)
					}
				})
			}
			if _, err := harness.Vault.PrepareDeletionCAS(t.Context(), binding, fresh, identity.DeletionOperation{OperationID: testUUID(800), CommandID: testUUID(801)}); err != nil {
				t.Fatalf("fresh deletion coordinates: %v", err)
			}
		})
	}

	t.Run("promotion intent expiry", func(t *testing.T) {
		harness := requireHarness(t, factory)
		binding := testBinding(t)
		rotator := testRotator(t, harness.Vault)
		promotion, err := rotator.RotateForSession(t.Context(), binding, testUUID(9), testUUID(109), func(context.Context) (identity.AppleTokens, error) {
			return identity.AppleTokens{IdentityToken: "identity-9", RefreshToken: "refresh-9"}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		promotion, err = rotator.PreparePromotionCAS(t.Context(), binding, promotion, sha256.Sum256([]byte("expiring")))
		if err != nil {
			t.Fatal(err)
		}
		harness.Advance(6 * time.Second)
		if _, err := rotator.ConfirmPromotion(t.Context(), promotion); err == nil {
			t.Fatal("expired promotion intent confirmed")
		}
	})
}

func reopenAbsentHarness(t *testing.T, harness *Harness, binding identity.CredentialBinding) {
	t.Helper()
	reopenHarness(t, harness)
	state, exists, err := harness.Vault.Resolve(t.Context(), binding)
	if err != nil || exists || state.Phase != "absent" {
		t.Fatalf("reopened state=%+v exists=%v err=%v", state, exists, err)
	}
	assertPublicMarkerFieldsZero(t, state)
	metadata, cursor, err := harness.Inventory.List(t.Context(), harness.Now().Add(time.Hour), "", 10)
	if err != nil || cursor != "" || len(metadata) != 0 {
		t.Fatalf("reopened inventory=%+v cursor=%q err=%v", metadata, cursor, err)
	}
}

func reopenDeletingAndRecover(t *testing.T, harness *Harness, binding identity.CredentialBinding) identity.DeletionCapability {
	t.Helper()
	reopenHarness(t, harness)
	state, exists, err := harness.Vault.Resolve(t.Context(), binding)
	if err != nil || !exists || state.Phase != "deleting" {
		t.Fatalf("reopened deleting state=%+v exists=%v err=%v", state, exists, err)
	}
	if state.MarkerSequence == 0 || state.MarkerDigest == [32]byte{} {
		t.Fatalf("deleting marker fields missing: %+v", state)
	}
	requireReconcileAfter(t, state)
	capability, err := harness.Vault.RecoverDeletionCapability(t.Context(), identity.DeletionRecovery{
		Binding:        binding,
		Ref:            state.Ref,
		OperationID:    state.OperationID,
		CommandID:      state.CommandID,
		MarkerSequence: state.MarkerSequence,
		MarkerDigest:   state.MarkerDigest,
	})
	if err != nil {
		t.Fatalf("recover deletion capability: %v", err)
	}
	return capability
}

func reopenNonterminal(t *testing.T, harness *Harness, binding identity.CredentialBinding, phase string, deadline time.Time) identity.CredentialRotationState {
	t.Helper()
	reopenHarness(t, harness)
	state, exists, err := harness.Vault.Resolve(t.Context(), binding)
	if err != nil || !exists || state.Phase != phase || state.ReconcileAfter != deadline {
		t.Fatalf("reopened %s state=%+v exists=%v err=%v deadline=%s", phase, state, exists, err, deadline)
	}
	return state
}

func reopenHarness(t *testing.T, harness *Harness) {
	t.Helper()
	previousVault, previousInventory := harness.Vault, harness.Inventory
	vault, inventory := harness.Reopen(t)
	if vault == nil || inventory == nil {
		t.Fatal("reopen must return nonnil vault and inventory")
	}
	assertDistinctAdapter(t, "vault", previousVault, vault)
	assertDistinctAdapter(t, "inventory", previousInventory, inventory)
	harness.Vault, harness.Inventory = vault, inventory
}

func assertDistinctAdapter(t *testing.T, name string, previous, reopened any) {
	t.Helper()
	previousValue := requirePointerAdapter(t, "previous "+name, previous)
	reopenedValue := requirePointerAdapter(t, "reopened "+name, reopened)
	if previousValue.Type() != reopenedValue.Type() {
		return
	}
	if previousValue.Pointer() == reopenedValue.Pointer() {
		t.Fatalf("reopen returned the same %s adapter instance", name)
	}
}

func requirePointerAdapter(t *testing.T, name string, adapter any) reflect.Value {
	t.Helper()
	value := reflect.ValueOf(adapter)
	if !value.IsValid() {
		t.Fatalf("%s adapter identity is invalid", name)
	}
	if value.Kind() != reflect.Pointer {
		t.Fatalf("cannot prove %s adapter identity for non-pointer type %T", name, adapter)
	}
	if value.IsNil() {
		t.Fatalf("%s adapter is a typed nil", name)
	}
	return value
}

func assertPublicMarkerFieldsZero(t *testing.T, state identity.CredentialRotationState) {
	t.Helper()
	if state.Phase != "deleting" && (state.MarkerSequence != 0 || state.MarkerDigest != [32]byte{}) {
		t.Fatalf("non-deleting marker fields exposed: %+v", state)
	}
}

func requireReconcileAfter(t *testing.T, state identity.CredentialRotationState) time.Time {
	t.Helper()
	if state.ReconcileAfter.IsZero() || state.ReconcileAfter.Location() != time.UTC {
		t.Fatalf("nonterminal state missing canonical reconcile_after: %+v", state)
	}
	return state.ReconcileAfter
}

func assertSameReconcileAfter(t *testing.T, state identity.CredentialRotationState, want time.Time) {
	t.Helper()
	if state.ReconcileAfter != want {
		t.Fatalf("phase %q reconcile_after=%s want=%s", state.Phase, state.ReconcileAfter, want)
	}
}

func requireHarness(t *testing.T, factory Factory) Harness {
	t.Helper()
	if factory == nil {
		t.Fatal("nil credential vault factory")
	}
	harness := factory(t)
	if harness.Now == nil || harness.Advance == nil || harness.Reopen == nil {
		t.Fatal("credential vault harness requires vault, inventory, clock helpers, and reopen")
	}
	requirePointerAdapter(t, "initial vault", harness.Vault)
	requirePointerAdapter(t, "initial inventory", harness.Inventory)
	return harness
}

func testRotator(t *testing.T, vault identity.CredentialVault) identity.CredentialRotator {
	t.Helper()
	rotator, err := identity.NewCredentialRotator(vault, revoker{}, func() string { return testUUID(999) })
	if err != nil {
		t.Fatal(err)
	}
	return rotator
}

func finishRotation(t *testing.T, rotator identity.CredentialRotator, binding identity.CredentialBinding, index int) {
	t.Helper()
	promotion, err := rotator.RotateForSession(t.Context(), binding, testUUID(index), testUUID(index+100), func(context.Context) (identity.AppleTokens, error) {
		return identity.AppleTokens{IdentityToken: fmt.Sprintf("identity-%d", index), RefreshToken: fmt.Sprintf("refresh-%d", index)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	promotion, err = rotator.PreparePromotionCAS(t.Context(), binding, promotion, sha256.Sum256([]byte(fmt.Sprintf("command-%d", index))))
	if err != nil {
		t.Fatal(err)
	}
	capability, err := rotator.ConfirmPromotion(t.Context(), promotion)
	if err != nil {
		t.Fatal(err)
	}
	if err := rotator.FinishAdoption(t.Context(), binding, capability); err != nil {
		t.Fatal(err)
	}
}

func testBinding(t *testing.T) identity.CredentialBinding {
	t.Helper()
	binding, err := identity.NewAppleCredentialBinding("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func testUUID(index int) string { return fmt.Sprintf("70000000-0000-4000-8000-%012d", index) }

type revoker struct{}

func (revoker) Revoke(context.Context, identity.AppleRevocationRequest) error { return nil }
