package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordingAppleRevoker struct {
	mu     sync.Mutex
	tokens []string
	err    error
}

func (revoker *recordingAppleRevoker) Revoke(_ context.Context, request AppleRevocationRequest) error {
	revoker.mu.Lock()
	defer revoker.mu.Unlock()
	revoker.tokens = append(revoker.tokens, string(request.RefreshToken))
	return revoker.err
}

func (revoker *recordingAppleRevoker) count() int {
	revoker.mu.Lock()
	defer revoker.mu.Unlock()
	return len(revoker.tokens)
}

func TestSequentialHundredHostedSignInsExchangeHundredCodesOneObjectMaxTwoTokens(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	vault := NewMemoryCredentialVault(func() time.Time { return now })
	revoker := &recordingAppleRevoker{}
	rotator, err := NewCredentialRotator(vault, revoker, func() string { return credentialTestUUID(999) })
	if err != nil {
		t.Fatal(err)
	}
	binding := credentialTestBinding(t)
	exchanges := 0
	for index := 1; index <= 100; index++ {
		operationID, commandID := credentialTestUUID(index), credentialTestUUID(index+1000)
		promotion, err := rotator.RotateForSession(t.Context(), binding, operationID, commandID, func(context.Context) (AppleTokens, error) {
			exchanges++
			return AppleTokens{IdentityToken: fmt.Sprintf("identity-%d", index), RefreshToken: fmt.Sprintf("refresh-%d", index)}, nil
		})
		if err != nil {
			t.Fatalf("rotation %d: %v", index, err)
		}
		if promotion.ExpectedCommandHash != ([32]byte{}) || len(promotion.ConfirmIntent.Opaque) != 0 {
			t.Fatalf("unprepared promotion authorized staging: %+v", promotion)
		}
		hash := sha256.Sum256([]byte(commandID))
		promotion, err = rotator.PreparePromotionCAS(t.Context(), binding, promotion, hash)
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
	state, exists, err := vault.Resolve(t.Context(), binding)
	if err != nil || !exists || state.Phase != credentialPhaseReady || !state.HasCurrent || state.HasPending {
		t.Fatalf("state=%+v exists=%v err=%v", state, exists, err)
	}
	metadata, cursor, err := vault.List(t.Context(), now.Add(time.Hour), "", 10)
	if err != nil || cursor != "" || len(metadata) != 1 {
		t.Fatalf("metadata=%+v cursor=%q err=%v", metadata, cursor, err)
	}
	if exchanges != 100 || revoker.count() != 99 {
		t.Fatalf("exchanges=%d revocations=%d", exchanges, revoker.count())
	}
	for _, secret := range []string{"identity-100", "refresh-100"} {
		if strings.Contains(fmt.Sprintf("%+v %+v", state, metadata), secret) {
			t.Fatalf("exported credential state leaked %q", secret)
		}
	}
}

func TestConcurrentHostedSignInsOnlyOneReservesAndExchanges(t *testing.T) {
	vault := NewMemoryCredentialVault(time.Now)
	rotator, err := NewCredentialRotator(vault, &recordingAppleRevoker{}, func() string { return credentialTestUUID(999) })
	if err != nil {
		t.Fatal(err)
	}
	binding := credentialTestBinding(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var exchanges atomic.Int32
	results := make(chan error, 2)
	go func() {
		_, err := rotator.RotateForSession(t.Context(), binding, credentialTestUUID(1), credentialTestUUID(101), func(context.Context) (AppleTokens, error) {
			exchanges.Add(1)
			close(entered)
			<-release
			return AppleTokens{IdentityToken: "identity-a", RefreshToken: "refresh-a"}, nil
		})
		results <- err
	}()
	<-entered
	go func() {
		_, err := rotator.RotateForSession(t.Context(), binding, credentialTestUUID(2), credentialTestUUID(102), func(context.Context) (AppleTokens, error) {
			exchanges.Add(1)
			return AppleTokens{IdentityToken: "identity-b", RefreshToken: "refresh-b"}, nil
		})
		results <- err
	}()
	close(release)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) || exchanges.Load() != 1 {
		t.Fatalf("errors=(%v,%v) exchanges=%d", first, second, exchanges.Load())
	}
	if Code(first) != "credential_rotation_busy" && Code(second) != "credential_rotation_busy" {
		t.Fatalf("loser errors=(%v,%v)", first, second)
	}
}

func TestCredentialRotationStatePersistsReconcileAfterEveryNonterminalPhase(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	vault := NewMemoryCredentialVault(func() time.Time { return now })
	binding := credentialTestBinding(t)

	reserved, err := vault.ReserveRotation(t.Context(), binding, credentialTestUUID(1), credentialTestUUID(101))
	if err != nil {
		t.Fatal(err)
	}
	wantRotationDeadline := now.Add(10 * time.Minute)
	if !reserved.ReconcileAfter.Equal(wantRotationDeadline) {
		t.Fatalf("reserved reconcile_after=%s want=%s", reserved.ReconcileAfter, wantRotationDeadline)
	}
	candidate, err := vault.StoreCandidate(t.Context(), binding, reserved, []byte("refresh-1"))
	if err != nil {
		t.Fatal(err)
	}
	if candidate.ReconcileAfter != reserved.ReconcileAfter {
		t.Fatalf("candidate extended deadline: reserved=%s candidate=%s", reserved.ReconcileAfter, candidate.ReconcileAfter)
	}
	promoted, err := vault.PromotePending(t.Context(), binding, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.ReconcileAfter != reserved.ReconcileAfter {
		t.Fatalf("promoted extended deadline: reserved=%s promoted=%s", reserved.ReconcileAfter, promoted.ReconcileAfter)
	}
	intent, err := vault.PreparePromotionCAS(t.Context(), binding, promoted, sha256.Sum256([]byte("command")))
	if err != nil {
		t.Fatal(err)
	}
	capability, err := vault.ConfirmPromotion(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	adopting, exists, err := vault.Resolve(t.Context(), binding)
	if err != nil || !exists {
		t.Fatalf("resolve adopting: exists=%v err=%v", exists, err)
	}
	if adopting.ReconcileAfter != intent.ExpiresAt() {
		t.Fatalf("adopting reconcile_after=%s want intent expiry=%s", adopting.ReconcileAfter, intent.ExpiresAt())
	}
	if reopened, ok, err := vault.Reopen().Resolve(t.Context(), binding); err != nil || !ok || reopened.ReconcileAfter != adopting.ReconcileAfter {
		t.Fatalf("reopened adopting=%+v exists=%v err=%v", reopened, ok, err)
	}
	if err := vault.AbandonAdoption(t.Context(), binding, capability); err != nil {
		t.Fatal(err)
	}
	aborting, exists, err := vault.Resolve(t.Context(), binding)
	if err != nil || !exists || aborting.Phase != credentialPhaseAborting || aborting.ReconcileAfter != adopting.ReconcileAfter {
		t.Fatalf("aborting=%+v exists=%v err=%v", aborting, exists, err)
	}

	// A separate ready object proves deletion stores the exact deletion-intent
	// expiry rather than reusing or extending the rotation deadline.
	second := credentialTestBinding(t)
	second.AccountID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	second, err = NewAppleCredentialBinding(second.AccountID, second.ServerInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	rotator, err := NewCredentialRotator(vault, &recordingAppleRevoker{}, func() string { return credentialTestUUID(999) })
	if err != nil {
		t.Fatal(err)
	}
	credentialTestReady(t, rotator, second, 2)
	ready, exists, err := vault.Resolve(t.Context(), second)
	if err != nil || !exists || !ready.ReconcileAfter.IsZero() {
		t.Fatalf("ready=%+v exists=%v err=%v", ready, exists, err)
	}
	deletionIntent, err := vault.PrepareDeletionCAS(t.Context(), second, ready, DeletionOperation{OperationID: credentialTestUUID(3), CommandID: credentialTestUUID(103)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vault.MarkDeleting(t.Context(), deletionIntent); err != nil {
		t.Fatal(err)
	}
	deleting, exists, err := vault.Resolve(t.Context(), second)
	if err != nil || !exists || deleting.ReconcileAfter != deletionIntent.ExpiresAt() {
		t.Fatalf("deleting=%+v exists=%v err=%v want=%s", deleting, exists, err, deletionIntent.ExpiresAt())
	}
}

func TestCredentialRotationAmbiguousCASRecoversExactCapability(t *testing.T) {
	vault, rotator, binding := credentialTestRotator(t)
	promotion := credentialTestPromoted(t, rotator, binding, 1)
	hash := sha256.Sum256([]byte("prepared-command"))
	prepared, err := rotator.PreparePromotionCAS(t.Context(), binding, promotion, hash)
	if err != nil {
		t.Fatal(err)
	}
	vault.failAfter(memoryOperationConfirmPromotion, ErrUnavailable)
	if _, err := rotator.ConfirmPromotion(t.Context(), prepared); err == nil {
		t.Fatal("ambiguous promotion CAS returned success")
	}
	restarted, err := NewCredentialRotator(vault, &recordingAppleRevoker{}, func() string { return credentialTestUUID(999) })
	if err != nil {
		t.Fatal(err)
	}
	resume, err := restarted.Resume(t.Context(), binding)
	if err != nil || resume.State.Phase != credentialPhaseAdopting || resume.ExpectedCommandHash != hash || resume.Adoption == nil {
		t.Fatalf("resume=%+v err=%v", resume, err)
	}
	if err := restarted.FinishAdoption(t.Context(), binding, *resume.Adoption); err != nil {
		t.Fatal(err)
	}
	state, _, _ := vault.Resolve(t.Context(), binding)
	if state.Phase != credentialPhaseReady {
		t.Fatalf("phase=%s", state.Phase)
	}
}

func TestAdoptingReceiptOutcomesAreMonotonic(t *testing.T) {
	for _, outcome := range []string{"present", "absent", "ambiguous"} {
		t.Run(outcome, func(t *testing.T) {
			vault, rotator, binding := credentialTestRotator(t)
			promotion := credentialTestPromoted(t, rotator, binding, 1)
			hash := sha256.Sum256([]byte("authority-command"))
			promotion, err := rotator.PreparePromotionCAS(t.Context(), binding, promotion, hash)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rotator.ConfirmPromotion(t.Context(), promotion); err != nil {
				t.Fatal(err)
			}
			resume, err := rotator.Resume(t.Context(), binding)
			if err != nil || resume.Adoption == nil {
				t.Fatalf("resume=%+v err=%v", resume, err)
			}
			switch outcome {
			case "present":
				err = rotator.FinishAdoption(t.Context(), binding, *resume.Adoption)
			case "absent":
				err = rotator.AbandonAdoption(t.Context(), binding, *resume.Adoption)
			case "ambiguous":
			}
			if err != nil {
				t.Fatal(err)
			}
			state, _, _ := vault.Resolve(t.Context(), binding)
			want := map[string]string{"present": credentialPhaseReady, "absent": credentialPhaseAborting, "ambiguous": credentialPhaseAdopting}[outcome]
			if state.Phase != want {
				t.Fatalf("phase=%s want=%s", state.Phase, want)
			}
		})
	}
}

func TestDeletionFreezesAtMostTwoSlotsAndBindsCapability(t *testing.T) {
	vault, rotator, binding := credentialTestRotator(t)
	promotion := credentialTestPromoted(t, rotator, binding, 1)
	state, _, err := vault.Resolve(t.Context(), binding)
	if err != nil || state.Phase != credentialPhasePromoted {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	operation := DeletionOperation{OperationID: credentialTestUUID(500), CommandID: credentialTestUUID(501)}
	intent, err := vault.PrepareDeletionCAS(t.Context(), binding, state, operation)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := vault.MarkDeleting(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := vault.OpenDeletingTokens(t.Context(), capability)
	if err != nil || len(tokens) != 1 || string(tokens[0]) != "refresh-1" {
		t.Fatalf("tokens=%q err=%v promotion=%+v", tokens, err, promotion)
	}
	tampered := DeletionCapability{Opaque: append([]byte(nil), capability.Opaque...)}
	tampered.Opaque[0] ^= 0xff
	if _, err := vault.OpenDeletingTokens(t.Context(), tampered); err == nil {
		t.Fatal("tampered deletion capability opened tokens")
	}
	capability, err = vault.EraseDeletingTokens(t.Context(), capability)
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Delete(t.Context(), capability); err != nil {
		t.Fatal(err)
	}
	absent, err := vault.ConfirmAbsent(t.Context(), binding, state.Ref)
	if err != nil || !absent {
		t.Fatalf("absent=%v err=%v", absent, err)
	}
}

func TestCredentialBindingAndIntentMismatchFailClosed(t *testing.T) {
	vault, rotator, binding := credentialTestRotator(t)
	promotion := credentialTestPromoted(t, rotator, binding, 1)
	hash := sha256.Sum256([]byte("command"))
	promotion, err := rotator.PreparePromotionCAS(t.Context(), binding, promotion, hash)
	if err != nil {
		t.Fatal(err)
	}
	wrong := binding
	wrong.AccountID = credentialTestUUID(888)
	if _, err := rotator.PreparePromotionCAS(t.Context(), wrong, promotion, hash); err == nil {
		t.Fatal("wrong binding accepted")
	}
	tampered := promotion
	tampered.ConfirmIntent.Opaque = append([]byte(nil), promotion.ConfirmIntent.Opaque...)
	tampered.ConfirmIntent.Opaque[0] ^= 0xff
	if _, err := rotator.ConfirmPromotion(t.Context(), tampered); err == nil {
		t.Fatal("tampered promotion intent accepted")
	}
	if _, err := rotator.ConfirmPromotion(t.Context(), promotion); err != nil {
		t.Fatal(err)
	}
	if _, err := rotator.ConfirmPromotion(t.Context(), promotion); err == nil {
		t.Fatal("successful promotion intent replayed")
	}
	state, _, _ := vault.Resolve(t.Context(), binding)
	if state.Phase != credentialPhaseAdopting {
		t.Fatalf("phase=%s", state.Phase)
	}
}

func TestCredentialBindingCanonicalAAD(t *testing.T) {
	binding := credentialTestBinding(t)
	if binding.ObjectID != "f71e3c87-1a11-8bac-88ba-b811fd571347" {
		t.Fatalf("object id=%q", binding.ObjectID)
	}
	aad, err := binding.CanonicalAAD()
	if err != nil {
		t.Fatal(err)
	}
	want := "626162792d747261636b65722d63726564656e7469616c2d62696e64696e672d76310000002462626262626262622d626262622d346262622d386262622d6262626262626262626262620000002461616161616161612d616161612d346161612d386161612d6161616161616161616161610000001b6170706c652d726566726573682d7265766f636174696f6e2d76310000002466373165336338372d316131312d386261632d383862612d623831316664353731333437"
	if got := hex.EncodeToString(aad); got != want {
		t.Fatalf("aad=%s", got)
	}
	for name, mutate := range map[string]func(*CredentialBinding){
		"unknown purpose":      func(value *CredentialBinding) { value.Purpose = "other" },
		"noncanonical account": func(value *CredentialBinding) { value.AccountID = strings.ToUpper(value.AccountID) },
		"nul":                  func(value *CredentialBinding) { value.Purpose += "\x00" },
		"wrong object":         func(value *CredentialBinding) { value.ObjectID = credentialTestUUID(45) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := binding
			mutate(&changed)
			if _, err := changed.CanonicalAAD(); err == nil {
				t.Fatal("invalid binding encoded")
			}
		})
	}
}

func TestCredentialObjectIDKnownAnswer(t *testing.T) {
	if got := credentialTestBinding(t).ObjectID; got != "f71e3c87-1a11-8bac-88ba-b811fd571347" {
		t.Fatalf("object id=%q", got)
	}
}

func TestCredentialCASIntentConstructionRejectsZeroExpiredAndOverlong(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	valid, err := NewPromotionCASIntent([]byte("sealed"), now.Add(credentialIntentTTL), now)
	if err != nil || valid.ExpiresAt() != now.Add(credentialIntentTTL) {
		t.Fatalf("valid intent=%+v err=%v", valid, err)
	}
	for name, testCase := range map[string]struct {
		opaque    []byte
		expiresAt time.Time
	}{
		"zero opaque":  {opaque: nil, expiresAt: now.Add(time.Second)},
		"expired":      {opaque: []byte("sealed"), expiresAt: now},
		"overlong ttl": {opaque: []byte("sealed"), expiresAt: now.Add(credentialIntentTTL + time.Nanosecond)},
		"non utc":      {opaque: []byte("sealed"), expiresAt: now.In(time.FixedZone("offset", 3600)).Add(time.Second)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewPromotionCASIntent(testCase.opaque, testCase.expiresAt, now); err == nil {
				t.Fatal("invalid promotion intent accepted")
			}
			if _, err := NewDeletionCASIntent(testCase.opaque, testCase.expiresAt, now); err == nil {
				t.Fatal("invalid deletion intent accepted")
			}
		})
	}
}

func TestCredentialAdoptionAbandonClearsCommandHash(t *testing.T) {
	vault, rotator, binding := credentialTestRotator(t)
	promotion := credentialTestPromoted(t, rotator, binding, 1)
	promotion, err := rotator.PreparePromotionCAS(t.Context(), binding, promotion, sha256.Sum256([]byte("abandon")))
	if err != nil {
		t.Fatal(err)
	}
	capability, err := rotator.ConfirmPromotion(t.Context(), promotion)
	if err != nil {
		t.Fatal(err)
	}
	if err := rotator.AbandonAdoption(t.Context(), binding, capability); err != nil {
		t.Fatal(err)
	}
	if err := rotator.AbandonAdoption(t.Context(), binding, capability); err != nil {
		t.Fatalf("idempotent abandon: %v", err)
	}
	state, _, err := vault.Resolve(t.Context(), binding)
	if err != nil || state.Phase != credentialPhaseAborting || !zeroDigest(state.ExpectedCommandHash) || validateCredentialState(binding, state) != nil {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestCredentialMaintenanceTransitionsReplayExactLostSuccess(t *testing.T) {
	vault, rotator, binding := credentialTestRotator(t)
	credentialTestReady(t, rotator, binding, 1)
	reserved, err := vault.ReserveRotation(t.Context(), binding, credentialTestUUID(2), credentialTestUUID(102))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := vault.StoreCandidate(t.Context(), binding, reserved, []byte("refresh-2"))
	if err != nil {
		t.Fatal(err)
	}
	aborting, err := vault.MarkAbandoned(t.Context(), binding, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := vault.MarkAbandoned(t.Context(), binding, candidate); err != nil || replay != aborting {
		t.Fatalf("abandon replay=%+v err=%v", replay, err)
	}
	until := time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC)
	claimed, err := vault.ClaimMaintenance(t.Context(), binding, aborting, "worker-1", until)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := vault.ClaimMaintenance(t.Context(), binding, aborting, "worker-1", until); err != nil || replay != claimed {
		t.Fatalf("claim replay=%+v err=%v", replay, err)
	}
	ready, err := vault.CompleteMaintenance(t.Context(), binding, claimed)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := vault.CompleteMaintenance(t.Context(), binding, claimed); err != nil || replay != ready {
		t.Fatalf("maintenance replay=%+v err=%v", replay, err)
	}
}

func TestCredentialCompletedOperationReplayDoesNotExchangeAgain(t *testing.T) {
	vault, rotator, binding := credentialTestRotator(t)
	exchanges := 0
	operationID, commandID := credentialTestUUID(1), credentialTestUUID(101)
	promotion, err := rotator.RotateForSession(t.Context(), binding, operationID, commandID, func(context.Context) (AppleTokens, error) {
		exchanges++
		return AppleTokens{IdentityToken: "identity-1", RefreshToken: "refresh-1"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	promotion, err = rotator.PreparePromotionCAS(t.Context(), binding, promotion, sha256.Sum256([]byte("completed")))
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
	for name, identifiers := range map[string][2]string{
		"exact pair":     {operationID, commandID},
		"operation only": {operationID, credentialTestUUID(202)},
		"command only":   {credentialTestUUID(2), commandID},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := rotator.RotateForSession(t.Context(), binding, identifiers[0], identifiers[1], func(context.Context) (AppleTokens, error) {
				exchanges++
				return AppleTokens{IdentityToken: "unexpected", RefreshToken: "unexpected"}, nil
			})
			if err == nil {
				t.Fatal("completed rotation identifiers accepted")
			}
		})
	}
	if exchanges != 1 {
		t.Fatalf("exchanges=%d", exchanges)
	}
	state, exists, err := vault.Resolve(t.Context(), binding)
	if err != nil || !exists || state.Phase != credentialPhaseReady {
		t.Fatalf("state=%+v exists=%v err=%v", state, exists, err)
	}
}

func TestDeletionCASIntentRejectsBlankOrStaleRotationOperationIDs(t *testing.T) {
	t.Run("blank", func(t *testing.T) {
		vault, rotator, binding := credentialTestRotator(t)
		credentialTestReady(t, rotator, binding, 1)
		state, _, err := vault.Resolve(t.Context(), binding)
		if err != nil {
			t.Fatal(err)
		}
		for _, operation := range []DeletionOperation{
			{CommandID: credentialTestUUID(501)},
			{OperationID: credentialTestUUID(500)},
		} {
			if _, err := vault.PrepareDeletionCAS(t.Context(), binding, state, operation); err == nil {
				t.Fatalf("blank deletion operation accepted: %+v", operation)
			}
		}
		if _, err := vault.PrepareDeletionCAS(t.Context(), binding, state, DeletionOperation{OperationID: credentialTestUUID(500), CommandID: credentialTestUUID(501)}); err != nil {
			t.Fatalf("fresh deletion operation: %v", err)
		}
	})

	t.Run("reserved with current", func(t *testing.T) {
		vault, rotator, binding := credentialTestRotator(t)
		credentialTestReady(t, rotator, binding, 1)
		operationID, commandID := credentialTestUUID(2), credentialTestUUID(102)
		reserved, err := vault.ReserveRotation(t.Context(), binding, operationID, commandID)
		if err != nil {
			t.Fatal(err)
		}
		ready, err := vault.MarkAbandoned(t.Context(), binding, reserved)
		if err != nil || ready.Phase != credentialPhaseReady {
			t.Fatalf("ready=%+v err=%v", ready, err)
		}
		assertStaleDeletionOperation(t, vault, binding, ready, operationID, commandID)
	})

	t.Run("candidate preserving current", func(t *testing.T) {
		vault, rotator, binding := credentialTestRotator(t)
		credentialTestReady(t, rotator, binding, 1)
		operationID, commandID := credentialTestUUID(2), credentialTestUUID(102)
		reserved, err := vault.ReserveRotation(t.Context(), binding, operationID, commandID)
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := vault.StoreCandidate(t.Context(), binding, reserved, []byte("refresh-2"))
		if err != nil {
			t.Fatal(err)
		}
		aborting, err := vault.MarkAbandoned(t.Context(), binding, candidate)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := vault.ClaimMaintenance(t.Context(), binding, aborting, "worker-stale", time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		ready, err := vault.CompleteMaintenance(t.Context(), binding, claimed)
		if err != nil || ready.Phase != credentialPhaseReady {
			t.Fatalf("ready=%+v err=%v", ready, err)
		}
		assertStaleDeletionOperation(t, vault, binding, ready, operationID, commandID)
	})
}

func TestCredentialReplayMarkerIsBoundedAndOverwritten(t *testing.T) {
	vault, _, binding := credentialTestRotator(t)
	for index := 1; index <= 2; index++ {
		operationID, commandID := credentialTestUUID(index), credentialTestUUID(index+100)
		reserved, err := vault.ReserveRotation(t.Context(), binding, operationID, commandID)
		if err != nil {
			t.Fatal(err)
		}
		aborting, err := vault.MarkAbandoned(t.Context(), binding, reserved)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := vault.ClaimMaintenance(t.Context(), binding, aborting, "worker-marker", time.Date(2026, 9, 20, 12, index, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		absent, err := vault.CompleteMaintenance(t.Context(), binding, claimed)
		if err != nil || absent.Phase != credentialPhaseAbsent {
			t.Fatalf("absent=%+v err=%v", absent, err)
		}
		if resolved, exists, err := vault.Resolve(t.Context(), binding); err != nil || exists || resolved.Phase != credentialPhaseAbsent {
			t.Fatalf("resolved=%+v exists=%v err=%v", resolved, exists, err)
		}
	}
	if len(vault.replayMarkers) != 1 {
		t.Fatalf("replay marker count=%d", len(vault.replayMarkers))
	}
	marker := vault.replayMarkers[binding.ObjectID]
	if marker.binding != binding || marker.operationID != credentialTestUUID(2) || marker.commandID != credentialTestUUID(102) {
		t.Fatalf("replay marker=%+v", marker)
	}
}

func TestCredentialDeletePersistsReplayMarkerBeforeObjectRemoval(t *testing.T) {
	vault, rotator, binding := credentialTestRotator(t)
	credentialTestReady(t, rotator, binding, 1)
	vault.replayMarkers[binding.ObjectID] = memoryCredentialReplayMarker{
		binding: binding, operationID: credentialTestUUID(9), commandID: credentialTestUUID(109),
	}
	ready, exists, err := vault.Resolve(t.Context(), binding)
	if err != nil || !exists {
		t.Fatalf("ready=%+v exists=%v err=%v", ready, exists, err)
	}
	intent, err := vault.PrepareDeletionCAS(t.Context(), binding, ready, DeletionOperation{OperationID: credentialTestUUID(500), CommandID: credentialTestUUID(501)})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := vault.MarkDeleting(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	capability, err = vault.EraseDeletingTokens(t.Context(), capability)
	if err != nil {
		t.Fatal(err)
	}
	vault.failAfter(memoryOperationStoreReplayMarker, ErrUnavailable)
	if err := vault.Delete(t.Context(), capability); err == nil {
		t.Fatal("delete succeeded after replay-marker write fault")
	}
	if state, exists, err := vault.Resolve(t.Context(), binding); err != nil || !exists || state.Phase != credentialPhaseDeleting {
		t.Fatalf("after fault state=%+v exists=%v err=%v", state, exists, err)
	}
	marker := vault.replayMarkers[binding.ObjectID]
	if marker.operationID != credentialTestUUID(1) || marker.commandID != credentialTestUUID(101) {
		t.Fatalf("marker=%+v", marker)
	}
	if err := vault.Delete(t.Context(), capability); err != nil {
		t.Fatal(err)
	}
	if err := vault.Delete(t.Context(), capability); err != nil {
		t.Fatalf("delete replay: %v", err)
	}
	if _, exists, err := vault.Resolve(t.Context(), binding); err != nil || exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	marker = vault.replayMarkers[binding.ObjectID]
	if marker.operationID != credentialTestUUID(1) || marker.commandID != credentialTestUUID(101) {
		t.Fatalf("persisted marker=%+v", marker)
	}
}

func TestCompleteMaintenancePersistsReplayMarkerBeforeNoCurrentDelete(t *testing.T) {
	setup := func(t *testing.T) (*MemoryCredentialVault, CredentialBinding, CredentialRotationState) {
		t.Helper()
		vault, _, binding := credentialTestRotator(t)
		reserved, err := vault.ReserveRotation(t.Context(), binding, credentialTestUUID(1), credentialTestUUID(101))
		if err != nil {
			t.Fatal(err)
		}
		aborting, err := vault.MarkAbandoned(t.Context(), binding, reserved)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := vault.ClaimMaintenance(t.Context(), binding, aborting, "maintenance-worker", time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		return vault, binding, claimed
	}

	t.Run("wrong marker binding", func(t *testing.T) {
		vault, binding, claimed := setup(t)
		other := binding
		other.AccountID = credentialTestUUID(44)
		vault.replayMarkers[binding.ObjectID] = memoryCredentialReplayMarker{binding: other, operationID: credentialTestUUID(1), commandID: credentialTestUUID(101)}
		if _, err := vault.CompleteMaintenance(t.Context(), binding, claimed); err == nil {
			t.Fatal("wrong marker binding accepted")
		}
		if state, exists, err := vault.Resolve(t.Context(), binding); err != nil || !exists || state.Phase != credentialPhaseAborting {
			t.Fatalf("state=%+v exists=%v err=%v", state, exists, err)
		}
	})

	t.Run("invalid main history", func(t *testing.T) {
		vault, binding, claimed := setup(t)
		vault.objects[binding.ObjectID].lastOperationID = ""
		if _, err := vault.CompleteMaintenance(t.Context(), binding, claimed); err == nil {
			t.Fatal("invalid main history accepted")
		}
		if state, exists, err := vault.Resolve(t.Context(), binding); err != nil || !exists || state.Phase != credentialPhaseAborting {
			t.Fatalf("state=%+v exists=%v err=%v", state, exists, err)
		}
	})

	t.Run("ambiguous marker success", func(t *testing.T) {
		vault, binding, claimed := setup(t)
		vault.failAfter(memoryOperationStoreReplayMarker, ErrUnavailable)
		if _, err := vault.CompleteMaintenance(t.Context(), binding, claimed); err == nil {
			t.Fatal("maintenance deleted after ambiguous marker write")
		}
		if state, exists, err := vault.Resolve(t.Context(), binding); err != nil || !exists || state.Phase != credentialPhaseAborting {
			t.Fatalf("state=%+v exists=%v err=%v", state, exists, err)
		}
		marker := vault.replayMarkers[binding.ObjectID]
		if marker.binding != binding || marker.operationID != credentialTestUUID(1) || marker.commandID != credentialTestUUID(101) {
			t.Fatalf("marker=%+v", marker)
		}
		absent, err := vault.CompleteMaintenance(t.Context(), binding, claimed)
		if err != nil || absent.Phase != credentialPhaseAbsent {
			t.Fatalf("retry=%+v err=%v", absent, err)
		}
		if replay, err := vault.CompleteMaintenance(t.Context(), binding, claimed); err != nil || replay != absent {
			t.Fatalf("replay=%+v err=%v", replay, err)
		}
	})
}

func assertStaleDeletionOperation(t *testing.T, vault CredentialVault, binding CredentialBinding, state CredentialRotationState, staleOperationID, staleCommandID string) {
	t.Helper()
	for name, operation := range map[string]DeletionOperation{
		"exact pair":     {OperationID: staleOperationID, CommandID: staleCommandID},
		"operation only": {OperationID: staleOperationID, CommandID: credentialTestUUID(501)},
		"command only":   {OperationID: credentialTestUUID(500), CommandID: staleCommandID},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := vault.PrepareDeletionCAS(t.Context(), binding, state, operation); err == nil {
				t.Fatalf("stale deletion operation accepted: %+v", operation)
			}
		})
	}
	if _, err := vault.PrepareDeletionCAS(t.Context(), binding, state, DeletionOperation{OperationID: credentialTestUUID(500), CommandID: credentialTestUUID(501)}); err != nil {
		t.Fatalf("fresh deletion operation: %v", err)
	}
}

func TestCredentialRotationCrashMatrixResumesWithoutSecondExchange(t *testing.T) {
	for _, operation := range []string{memoryOperationReserveRotation, memoryOperationStoreCandidate, memoryOperationPromotePending} {
		t.Run(operation, func(t *testing.T) {
			vault, rotator, binding := credentialTestRotator(t)
			vault.failAfter(operation, ErrUnavailable)
			exchanges := 0
			rotate := func() (CredentialPromotion, error) {
				return rotator.RotateForSession(t.Context(), binding, credentialTestUUID(1), credentialTestUUID(101), func(context.Context) (AppleTokens, error) {
					exchanges++
					return AppleTokens{IdentityToken: "identity", RefreshToken: "refresh"}, nil
				})
			}
			promotion, err := rotate()
			if operation == memoryOperationReserveRotation {
				if err == nil || exchanges != 0 {
					t.Fatalf("reserve crash err=%v exchanges=%d", err, exchanges)
				}
				promotion, err = rotate()
			}
			if err != nil || promotion.Ref.ID != binding.ObjectID || exchanges != 1 {
				t.Fatalf("promotion=%+v err=%v exchanges=%d", promotion, err, exchanges)
			}
		})
	}

	t.Run(memoryOperationMarkPriorRevoked, func(t *testing.T) {
		vault, rotator, binding := credentialTestRotator(t)
		credentialTestReady(t, rotator, binding, 1)
		vault.failAfter(memoryOperationMarkPriorRevoked, ErrUnavailable)
		exchanges := 0
		promotion, err := rotator.RotateForSession(t.Context(), binding, credentialTestUUID(2), credentialTestUUID(102), func(context.Context) (AppleTokens, error) {
			exchanges++
			return AppleTokens{IdentityToken: "identity-2", RefreshToken: "refresh-2"}, nil
		})
		if err != nil || promotion.Ref.ID != binding.ObjectID || exchanges != 1 {
			t.Fatalf("promotion=%+v err=%v exchanges=%d", promotion, err, exchanges)
		}
	})
}

func TestCredentialRotationRejectsSecondPending(t *testing.T) {
	vault, _, binding := credentialTestRotator(t)
	reserved, err := vault.ReserveRotation(t.Context(), binding, credentialTestUUID(1), credentialTestUUID(101))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vault.StoreCandidate(t.Context(), binding, reserved, []byte("refresh-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.ReserveRotation(t.Context(), binding, credentialTestUUID(2), credentialTestUUID(102)); Code(err) != "credential_rotation_busy" {
		t.Fatalf("second reservation err=%v", err)
	}
	if _, err := vault.StoreCandidate(t.Context(), binding, reserved, []byte("different")); err == nil {
		t.Fatal("second pending token accepted")
	}
}

func TestCredentialDeletionRevokesBothSlotsAndRejectsWrongCapabilityState(t *testing.T) {
	vault, rotator, binding := credentialTestRotator(t)
	credentialTestReady(t, rotator, binding, 1)
	reserved, err := vault.ReserveRotation(t.Context(), binding, credentialTestUUID(2), credentialTestUUID(102))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := vault.StoreCandidate(t.Context(), binding, reserved, []byte("refresh-2"))
	if err != nil {
		t.Fatal(err)
	}
	operation := DeletionOperation{OperationID: credentialTestUUID(500), CommandID: credentialTestUUID(501)}
	intent, err := vault.PrepareDeletionCAS(t.Context(), binding, candidate, operation)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := vault.MarkDeleting(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := vault.OpenDeletingTokens(t.Context(), capability)
	if err != nil || len(tokens) != 2 {
		t.Fatalf("tokens=%q err=%v", tokens, err)
	}
	record := vault.deletions[string(capability.Opaque)]
	originalVersion := record.version
	record.version = "wrong"
	if _, err := vault.OpenDeletingTokens(t.Context(), capability); err == nil {
		t.Fatal("wrong provider version accepted")
	}
	record.version = originalVersion
	originalSequence := record.markerSequence
	record.markerSequence++
	if _, err := vault.OpenDeletingTokens(t.Context(), capability); err == nil {
		t.Fatal("wrong marker sequence accepted")
	}
	record.markerSequence = originalSequence
	wrongBinding := binding
	wrongBinding.AccountID = credentialTestUUID(999)
	if _, err := vault.RecoverDeletionCapability(t.Context(), DeletionRecovery{Binding: wrongBinding, Ref: candidate.Ref,
		OperationID: operation.OperationID, CommandID: operation.CommandID, MarkerSequence: originalSequence, MarkerDigest: record.markerDigest}); err == nil {
		t.Fatal("wrong object binding recovered capability")
	}
	newCapability, err := vault.EraseDeletingTokens(t.Context(), capability)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := vault.EraseDeletingTokens(t.Context(), capability); err != nil || string(replay.Opaque) != string(newCapability.Opaque) {
		t.Fatalf("erase replay=%q want=%q err=%v", replay.Opaque, newCapability.Opaque, err)
	}
	if err := vault.Delete(t.Context(), newCapability); err != nil {
		t.Fatal(err)
	}
	if err := vault.Delete(t.Context(), newCapability); err != nil {
		t.Fatalf("delete replay: %v", err)
	}
}

func credentialTestReady(t *testing.T, rotator CredentialRotator, binding CredentialBinding, index int) {
	t.Helper()
	promotion := credentialTestPromoted(t, rotator, binding, index)
	promotion, err := rotator.PreparePromotionCAS(t.Context(), binding, promotion, sha256.Sum256([]byte(fmt.Sprintf("command-%d", index))))
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

func credentialTestRotator(t *testing.T) (*MemoryCredentialVault, CredentialRotator, CredentialBinding) {
	t.Helper()
	vault := NewMemoryCredentialVault(func() time.Time { return time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC) })
	rotator, err := NewCredentialRotator(vault, &recordingAppleRevoker{}, func() string { return credentialTestUUID(999) })
	if err != nil {
		t.Fatal(err)
	}
	return vault, rotator, credentialTestBinding(t)
}

func credentialTestPromoted(t *testing.T, rotator CredentialRotator, binding CredentialBinding, index int) CredentialPromotion {
	t.Helper()
	promotion, err := rotator.RotateForSession(t.Context(), binding, credentialTestUUID(index), credentialTestUUID(index+100), func(context.Context) (AppleTokens, error) {
		return AppleTokens{IdentityToken: fmt.Sprintf("identity-%d", index), RefreshToken: fmt.Sprintf("refresh-%d", index)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return promotion
}

func credentialTestBinding(t *testing.T) CredentialBinding {
	t.Helper()
	binding, err := NewAppleCredentialBinding("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func credentialTestUUID(index int) string {
	return fmt.Sprintf("70000000-0000-4000-8000-%012d", index)
}
