package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/security"
)

type hostedSessionApple struct {
	mu              sync.Mutex
	exchanges       int
	revocations     int
	returnedSubject string
	beforeExchange  func(context.Context) error
	exchangeEntered chan struct{}
	exchangeRelease chan struct{}
	exchangeOnce    sync.Once
}

func (apple *hostedSessionApple) ExchangeCode(ctx context.Context, request identity.AppleCodeExchangeRequest) (identity.AppleTokens, error) {
	if apple.beforeExchange != nil {
		if err := apple.beforeExchange(ctx); err != nil {
			return identity.AppleTokens{}, err
		}
	}
	apple.mu.Lock()
	apple.exchanges++
	exchangeNumber := apple.exchanges
	subject := apple.returnedSubject
	if subject == "" {
		subject = request.ExpectedIdentity.Subject
	}
	entered, release := apple.exchangeEntered, apple.exchangeRelease
	apple.mu.Unlock()
	if entered != nil {
		apple.exchangeOnce.Do(func() { close(entered) })
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return identity.AppleTokens{}, ctx.Err()
		case <-release:
		}
	}
	return identity.AppleTokens{IdentityToken: subject, RefreshToken: fmt.Sprintf("hosted-refresh-%d", exchangeNumber)}, nil
}

func (apple *hostedSessionApple) Revoke(context.Context, identity.AppleRevocationRequest) error {
	apple.mu.Lock()
	defer apple.mu.Unlock()
	apple.revocations++
	return nil
}

func (apple *hostedSessionApple) counts() (int, int) {
	apple.mu.Lock()
	defer apple.mu.Unlock()
	return apple.exchanges, apple.revocations
}

type hostedSessionRotator struct {
	delegate identity.CredentialRotator

	mu                  sync.Mutex
	preparedHash        [32]byte
	confirmedHash       [32]byte
	prepareCount        int
	confirmCount        int
	finishCount         int
	abandonCount        int
	resumeCount         int
	loseConfirmResponse bool
	beforeFirstPrepare  func() error
	assertNothingStaged func(string) error
	rotateEntered       chan int
	firstRotateComplete chan struct{}
	releaseFirstRotate  chan struct{}
	rotateCalls         int
	firstRotateOnce     sync.Once
}

func (rotator *hostedSessionRotator) RotateForSession(ctx context.Context, binding identity.CredentialBinding, operationID, commandID string,
	exchange func(context.Context) (identity.AppleTokens, error)) (identity.CredentialPromotion, error) {
	rotator.mu.Lock()
	rotator.rotateCalls++
	call := rotator.rotateCalls
	entered, completed, release := rotator.rotateEntered, rotator.firstRotateComplete, rotator.releaseFirstRotate
	rotator.mu.Unlock()
	if entered != nil {
		entered <- call
	}
	promotion, err := rotator.delegate.RotateForSession(ctx, binding, operationID, commandID, exchange)
	if call == 1 && completed != nil {
		rotator.firstRotateOnce.Do(func() { close(completed) })
		if release != nil {
			select {
			case <-ctx.Done():
				return identity.CredentialPromotion{}, ctx.Err()
			case <-release:
			}
		}
	}
	return promotion, err
}

func (rotator *hostedSessionRotator) PreparePromotionCAS(ctx context.Context, binding identity.CredentialBinding,
	promotion identity.CredentialPromotion, hash [32]byte) (identity.CredentialPromotion, error) {
	rotator.mu.Lock()
	rotator.prepareCount++
	first := rotator.prepareCount == 1
	rotator.preparedHash = hash
	hook := rotator.beforeFirstPrepare
	rotator.mu.Unlock()
	if first && hook != nil {
		if err := hook(); err != nil {
			return identity.CredentialPromotion{}, err
		}
	}
	return rotator.delegate.PreparePromotionCAS(ctx, binding, promotion, hash)
}

func (rotator *hostedSessionRotator) ConfirmPromotion(ctx context.Context, promotion identity.CredentialPromotion) (identity.AdoptionCapability, error) {
	rotator.mu.Lock()
	rotator.confirmCount++
	rotator.confirmedHash = promotion.ExpectedCommandHash
	assertNothingStaged := rotator.assertNothingStaged
	rotator.mu.Unlock()
	if assertNothingStaged != nil {
		if err := assertNothingStaged(promotion.CommandID); err != nil {
			return identity.AdoptionCapability{}, err
		}
	}
	capability, err := rotator.delegate.ConfirmPromotion(ctx, promotion)
	rotator.mu.Lock()
	loseResponse := rotator.loseConfirmResponse
	rotator.mu.Unlock()
	if err == nil && loseResponse {
		return identity.AdoptionCapability{}, identity.ErrUnavailable
	}
	return capability, err
}

func (rotator *hostedSessionRotator) RecoverAdoptionCapability(ctx context.Context, promotion identity.CredentialPromotion) (identity.AdoptionCapability, error) {
	return rotator.delegate.RecoverAdoptionCapability(ctx, promotion)
}

func (rotator *hostedSessionRotator) FinishAdoption(ctx context.Context, binding identity.CredentialBinding, capability identity.AdoptionCapability) error {
	rotator.mu.Lock()
	rotator.finishCount++
	rotator.mu.Unlock()
	return rotator.delegate.FinishAdoption(ctx, binding, capability)
}

func (rotator *hostedSessionRotator) AbandonAdoption(ctx context.Context, binding identity.CredentialBinding, capability identity.AdoptionCapability) error {
	rotator.mu.Lock()
	rotator.abandonCount++
	rotator.mu.Unlock()
	return rotator.delegate.AbandonAdoption(ctx, binding, capability)
}

func (rotator *hostedSessionRotator) Resume(ctx context.Context, binding identity.CredentialBinding) (identity.CredentialResume, error) {
	rotator.mu.Lock()
	rotator.resumeCount++
	rotator.mu.Unlock()
	return rotator.delegate.Resume(ctx, binding)
}

func (rotator *hostedSessionRotator) Commit(ctx context.Context, binding identity.CredentialBinding, operationID string) error {
	return rotator.delegate.Commit(ctx, binding, operationID)
}

func (rotator *hostedSessionRotator) Abandon(ctx context.Context, binding identity.CredentialBinding, operationID string) error {
	rotator.mu.Lock()
	rotator.abandonCount++
	rotator.mu.Unlock()
	return rotator.delegate.Abandon(ctx, binding, operationID)
}

func (rotator *hostedSessionRotator) snapshot() (prepared, confirmed [32]byte, prepares, confirms, finishes, abandons, resumes int) {
	rotator.mu.Lock()
	defer rotator.mu.Unlock()
	return rotator.preparedHash, rotator.confirmedHash, rotator.prepareCount, rotator.confirmCount, rotator.finishCount, rotator.abandonCount, rotator.resumeCount
}

type hostedSessionFixture struct {
	*sessionFixture
	SelfHosted  *Service
	Apple       *hostedSessionApple
	Vault       *identity.MemoryCredentialVault
	Credentials *hostedSessionRotator
}

func newHostedSessionFixture(t *testing.T) *hostedSessionFixture {
	t.Helper()
	base := newSessionFixture(t, config.ModeSelfHosted)
	selfHosted := base.Service
	apple := &hostedSessionApple{}
	vault := identity.NewMemoryCredentialVault(base.Clock.Now)
	operationIDs := 0
	delegate, err := identity.NewCredentialRotator(vault, apple, func() string {
		operationIDs++
		return fmt.Sprintf("70000000-0000-4000-8000-%012d", operationIDs)
	})
	if err != nil {
		t.Fatal(err)
	}
	recording := &hostedSessionRotator{delegate: delegate}
	hosted, err := New(Options{Identity: base.Identity, Verifier: base.Service.verifier, Security: base.Service.security,
		Repository: base.Service.repository, Apple: apple, Credentials: recording, Mode: config.ModeHosted,
		Origin: base.origin, InstanceID: base.instance, TokenKeys: base.tokenKeys, DeviceKeys: base.deviceKeys,
		NotificationKeys: base.notificationKeys, ActorIDKey: base.actorKey, Now: base.Clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	base.Service = hosted
	return &hostedSessionFixture{sessionFixture: base, SelfHosted: selfHosted, Apple: apple, Vault: vault, Credentials: recording}
}

func (fixture *hostedSessionFixture) restartHostedAndReconcile() {
	fixture.t.Helper()
	fixture.sessionFixture.RestartAndReconcile()
	fixture.SelfHosted = fixture.sessionFixture.Service
	fixture.Vault = fixture.Vault.Reopen()
	delegate, err := identity.NewCredentialRotator(fixture.Vault, fixture.Apple, func() string {
		id, idErr := newUUID()
		if idErr != nil {
			fixture.t.Fatal(idErr)
		}
		return id
	})
	if err != nil {
		fixture.t.Fatal(err)
	}
	fixture.Credentials = &hostedSessionRotator{delegate: delegate}
	fixture.Service, err = New(Options{Identity: fixture.Identity, Verifier: fixture.SelfHosted.verifier, Security: fixture.SelfHosted.security,
		Repository: fixture.SelfHosted.repository, Apple: fixture.Apple, Credentials: fixture.Credentials, Mode: config.ModeHosted,
		Origin: fixture.origin, InstanceID: fixture.instance, TokenKeys: fixture.tokenKeys, DeviceKeys: fixture.deviceKeys,
		NotificationKeys: fixture.notificationKeys, ActorIDKey: fixture.actorKey, Now: fixture.Clock.Now})
	if err != nil {
		fixture.t.Fatal(err)
	}
}

func (fixture *hostedSessionFixture) exchangeHosted(subject string, profile *identity.ProfileInput, claim *identity.DeviceContinuityClaim) (SessionBundle, string, error) {
	fixture.t.Helper()
	challenge, err := fixture.Identity.Challenge(fixture.t.Context())
	if err != nil {
		fixture.t.Fatal(err)
	}
	commandID := fixture.nextID(7)
	bundle, err := fixture.Service.Exchange(fixture.t.Context(), identity.ExchangeRequest{CommandID: commandID, Challenge: challenge.Challenge,
		IdentityToken: subject, AuthorizationCode: "authorization-" + commandID, InstallationID: "installation-" + subject,
		Profile: profile, DeviceContinuity: claim})
	return bundle, commandID, err
}

func (fixture *hostedSessionFixture) exchangeHostedCommand(subject, commandID string, profile *identity.ProfileInput,
	claim *identity.DeviceContinuityClaim) (SessionBundle, error) {
	fixture.t.Helper()
	challenge, err := fixture.Identity.Challenge(fixture.t.Context())
	if err != nil {
		fixture.t.Fatal(err)
	}
	return fixture.Service.Exchange(fixture.t.Context(), identity.ExchangeRequest{CommandID: commandID, Challenge: challenge.Challenge,
		IdentityToken: subject, AuthorizationCode: "authorization-" + commandID, InstallationID: "installation-" + subject,
		Profile: profile, DeviceContinuity: claim})
}

func TestPromotionIntentHashMatchesFrozenStageAndPreparedCommand(t *testing.T) {
	fixture := newHostedSessionFixture(t)
	fixture.Credentials.assertNothingStaged = func(commandID string) error {
		var count int
		if err := fixture.db.Owner.QueryRow(t.Context(), `SELECT count(*) FROM auth_exchange_material WHERE command_id=$1`, commandID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("stage existed before promotion confirmation")
		}
		return nil
	}

	bundle, commandID, err := fixture.exchangeHosted("subject-one", &identity.ProfileInput{DisplayName: "Parent One", Confirmed: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.AccessToken == "" || bundle.RefreshToken == "" {
		t.Fatal("hosted exchange did not mint a session")
	}
	prepared, confirmed, prepares, confirms, finishes, abandons, _ := fixture.Credentials.snapshot()
	if subtle.ConstantTimeCompare(prepared[:], confirmed[:]) != 1 || prepares != 1 || confirms != 1 || finishes != 1 || abandons != 0 {
		t.Fatalf("prepared=%x confirmed=%x counts=(%d,%d,%d,%d)", prepared, confirmed, prepares, confirms, finishes, abandons)
	}
	var stagedHash []byte
	var credentialID, credentialKey, credentialPurpose, credentialObject string
	if err := fixture.db.Owner.QueryRow(t.Context(), `SELECT command_hash FROM auth_exchange_material WHERE command_id=$1`, commandID).
		Scan(&stagedHash); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Owner.QueryRow(t.Context(), `SELECT apple_credential_id,apple_credential_key_id,
		apple_credential_purpose,apple_credential_object_id::text FROM account_identities WHERE account_id=$1`, bundle.Account.ID).
		Scan(&credentialID, &credentialKey, &credentialPurpose, &credentialObject); err != nil {
		t.Fatal(err)
	}
	if subtle.ConstantTimeCompare(stagedHash, prepared[:]) != 1 || credentialID != credentialObject || credentialPurpose != identity.AppleCredentialPurpose || credentialKey == "" {
		t.Fatalf("staged hash/ref mismatch: hash=%x ref=(%q,%q,%q,%q)", stagedHash, credentialID, credentialKey, credentialPurpose, credentialObject)
	}
	state, exists, err := fixture.Vault.Resolve(t.Context(), identity.CredentialBinding{AccountID: bundle.Account.ID,
		ServerInstanceID: fixture.instance, Purpose: credentialPurpose, ObjectID: credentialObject})
	if err != nil || !exists || state.Phase != "ready" {
		t.Fatalf("credential state=%+v exists=%t err=%v", state, exists, err)
	}
}

func TestExecutePreparedConsumesPrebuiltPromotionIntentBeforeStaging(t *testing.T) {
	fixture := newHostedSessionFixture(t)
	checked := false
	fixture.Credentials.assertNothingStaged = func(commandID string) error {
		var count int
		if err := fixture.db.Owner.QueryRow(t.Context(), `SELECT count(*) FROM auth_exchange_material WHERE command_id=$1`, commandID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("authority material was staged before the promotion intent was consumed")
		}
		checked = true
		return nil
	}
	if _, _, err := fixture.exchangeHosted("subject-one", &identity.ProfileInput{DisplayName: "Parent One", Confirmed: true}, nil); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("promotion confirmation did not run")
	}
}

func TestHostedExchangeDoesNotHoldAuthorityFenceDuringAppleOrTokenIO(t *testing.T) {
	fixture := newHostedSessionFixture(t)
	fixture.Apple.beforeExchange = func(ctx context.Context) error {
		probeContext, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		return fixture.Service.security.WithAuthoritySnapshot(probeContext, func(_ security.AuthoritySnapshot) error { return nil })
	}
	if _, _, err := fixture.exchangeHosted("subject-one", &identity.ProfileInput{DisplayName: "Parent One", Confirmed: true}, nil); err != nil {
		t.Fatal(err)
	}
	exchanges, _ := fixture.Apple.counts()
	if exchanges != 1 {
		t.Fatalf("Apple exchange calls=%d", exchanges)
	}
}

func TestPromotionIntentHeadDriftRepreparesBeforeCAS(t *testing.T) {
	fixture := newHostedSessionFixture(t)
	fixture.Credentials.beforeFirstPrepare = func() error {
		challenge, err := fixture.Identity.Challenge(t.Context())
		if err != nil {
			return err
		}
		_, err = fixture.SelfHosted.Exchange(t.Context(), identity.ExchangeRequest{CommandID: fixture.nextID(3), Challenge: challenge.Challenge,
			IdentityToken: "drift-subject", InstallationID: "drift-installation",
			Profile: &identity.ProfileInput{DisplayName: "Drift Parent", Confirmed: true}})
		return err
	}
	if _, _, err := fixture.exchangeHosted("subject-one", &identity.ProfileInput{DisplayName: "Parent One", Confirmed: true}, nil); err != nil {
		t.Fatal(err)
	}
	_, _, prepares, confirms, finishes, abandons, _ := fixture.Credentials.snapshot()
	exchanges, _ := fixture.Apple.counts()
	if prepares != 2 || confirms != 1 || finishes != 1 || abandons != 0 || exchanges != 1 {
		t.Fatalf("prepares=%d confirms=%d finishes=%d abandons=%d exchanges=%d", prepares, confirms, finishes, abandons, exchanges)
	}
}

func TestHostedHeadDriftDuringAppleExchangeRepreparesBeforeFirstIntent(t *testing.T) {
	fixture := newHostedSessionFixture(t)
	fixture.Apple.beforeExchange = func(context.Context) error {
		challenge, err := fixture.Identity.Challenge(t.Context())
		if err != nil {
			return err
		}
		_, err = fixture.SelfHosted.Exchange(t.Context(), identity.ExchangeRequest{CommandID: fixture.nextID(3), Challenge: challenge.Challenge,
			IdentityToken: "apple-drift-subject", InstallationID: "apple-drift-installation",
			Profile: &identity.ProfileInput{DisplayName: "Apple Drift Parent", Confirmed: true}})
		return err
	}
	if _, _, err := fixture.exchangeHosted("subject-one", &identity.ProfileInput{DisplayName: "Parent One", Confirmed: true}, nil); err != nil {
		t.Fatal(err)
	}
	_, _, prepares, confirms, finishes, abandons, _ := fixture.Credentials.snapshot()
	exchanges, _ := fixture.Apple.counts()
	if prepares != 1 || confirms != 1 || finishes != 1 || abandons != 0 || exchanges != 1 {
		t.Fatalf("prepares=%d confirms=%d finishes=%d abandons=%d exchanges=%d", prepares, confirms, finishes, abandons, exchanges)
	}
}

func TestHostedPromotionCASLostResponseRecoversOutsideFenceBeforeAbandon(t *testing.T) {
	fixture := newHostedSessionFixture(t)
	fixture.Credentials.loseConfirmResponse = true
	_, commandID, err := fixture.exchangeHosted("subject-one", &identity.ProfileInput{DisplayName: "Parent One", Confirmed: true}, nil)
	if err == nil {
		t.Fatal("lost promotion response unexpectedly issued a session")
	}
	_, _, _, confirms, finishes, abandons, resumes := fixture.Credentials.snapshot()
	if confirms != 1 || finishes != 0 || abandons != 1 || resumes == 0 {
		t.Fatalf("confirms=%d finishes=%d abandons=%d resumes=%d", confirms, finishes, abandons, resumes)
	}
	var stages, receipts int
	if err := fixture.db.Owner.QueryRow(t.Context(), `SELECT count(*) FROM auth_exchange_material WHERE command_id=$1`, commandID).Scan(&stages); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Owner.QueryRow(t.Context(), `SELECT count(*) FROM security_command_receipts WHERE command_id=$1`, commandID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if stages != 0 || receipts != 0 {
		t.Fatalf("ambiguous CAS staged=%d receipts=%d", stages, receipts)
	}
}

func TestHostedCommittedAdoptionRecoversAfterRestartWithoutAppleReplay(t *testing.T) {
	fixture := newHostedSessionFixture(t)
	fixture.FailProjectionOnce()
	_, commandID, err := fixture.exchangeHosted("subject-one", &identity.ProfileInput{DisplayName: "Parent One", Confirmed: true}, nil)
	if Code(err) != "service_unavailable" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	accountID, actorErr := ActorID(fixture.actorKey, fixture.instance, identity.VerifiedIdentity{Provider: "apple", Audience: "com.example.babytracker", Subject: "subject-one"}, 1)
	if actorErr != nil {
		t.Fatal(actorErr)
	}
	binding, bindingErr := identity.NewAppleCredentialBinding(accountID, fixture.instance)
	if bindingErr != nil {
		t.Fatal(bindingErr)
	}
	state, exists, resolveErr := fixture.Vault.Resolve(t.Context(), binding)
	if resolveErr != nil || !exists || state.Phase != "adopting" {
		t.Fatalf("pre-restart state=%+v exists=%t err=%v", state, exists, resolveErr)
	}
	exchangesBefore, _ := fixture.Apple.counts()
	fixture.restartHostedAndReconcile()
	if _, retryErr := fixture.exchangeHostedCommand("subject-one", commandID, nil, nil); Code(retryErr) != "service_unavailable" {
		t.Fatalf("retry code=%s err=%v", Code(retryErr), retryErr)
	}
	exchangesAfter, _ := fixture.Apple.counts()
	state, exists, resolveErr = fixture.Vault.Resolve(t.Context(), binding)
	if resolveErr != nil || !exists || state.Phase != "ready" || exchangesAfter != exchangesBefore {
		t.Fatalf("post-restart state=%+v exists=%t err=%v exchanges=%d->%d", state, exists, resolveErr, exchangesBefore, exchangesAfter)
	}
}

func TestHostedLostSessionResponseDoesNotExchangeAuthorizationCodeAgain(t *testing.T) {
	fixture := newHostedSessionFixture(t)
	commandID := fixture.nextID(7)
	if _, err := fixture.exchangeHostedCommand("subject-one", commandID, &identity.ProfileInput{DisplayName: "Parent One", Confirmed: true}, nil); err != nil {
		t.Fatal(err)
	}
	exchangesBefore, revocationsBefore := fixture.Apple.counts()
	if _, err := fixture.exchangeHostedCommand("subject-one", commandID, nil, nil); Code(err) != "command_id_reused" {
		t.Fatalf("retry code=%s err=%v", Code(err), err)
	}
	exchangesAfter, revocationsAfter := fixture.Apple.counts()
	if exchangesAfter != exchangesBefore || revocationsAfter != revocationsBefore {
		t.Fatalf("lost-response retry touched Apple: exchanges=%d->%d revocations=%d->%d", exchangesBefore, exchangesAfter, revocationsBefore, revocationsAfter)
	}
}

func TestSequentialHundredHostedSignInsExchangeHundredCodesOneMainObjectMaxTwoTokens(t *testing.T) {
	fixture := newHostedSessionFixture(t)
	first, _, err := fixture.exchangeHosted("subject-one", &identity.ProfileInput{DisplayName: "Parent One", Confirmed: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	claim := &identity.DeviceContinuityClaim{DeviceID: first.DeviceID, Credential: first.DeviceCredential}
	for attempt := 2; attempt <= 100; attempt++ {
		next, _, exchangeErr := fixture.exchangeHosted("subject-one", nil, claim)
		if exchangeErr != nil {
			t.Fatalf("sign-in %d: %v", attempt, exchangeErr)
		}
		if next.DeviceID != first.DeviceID || next.DeviceCredential != "" {
			t.Fatalf("continuity changed on sign-in %d: first=%+v next=%+v", attempt, first, next)
		}
	}
	exchanges, revocations := fixture.Apple.counts()
	metadata, cursor, listErr := fixture.Vault.List(t.Context(), fixture.Clock.Now().Add(time.Hour), "", 10)
	if listErr != nil || cursor != "" || len(metadata) != 1 || exchanges != 100 || revocations != 99 {
		t.Fatalf("metadata=%+v cursor=%q exchanges=%d revocations=%d err=%v", metadata, cursor, exchanges, revocations, listErr)
	}
	if metadata[0].Binding.AccountID != first.Account.ID || metadata[0].Binding.ObjectID != metadata[0].Ref.ID || metadata[0].Phase != "ready" {
		t.Fatalf("unstable credential metadata: %+v", metadata[0])
	}
}

func TestConcurrentHostedSignInsOnlyOneReservesAndExchanges(t *testing.T) {
	fixture := newHostedSessionFixture(t)
	fixture.Apple.exchangeEntered = make(chan struct{})
	fixture.Apple.exchangeRelease = make(chan struct{})
	fixture.Credentials.rotateEntered = make(chan int, 2)
	fixture.Credentials.firstRotateComplete = make(chan struct{})
	fixture.Credentials.releaseFirstRotate = make(chan struct{})

	request := func(commandID string) identity.ExchangeRequest {
		challenge, err := fixture.Identity.Challenge(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return identity.ExchangeRequest{CommandID: commandID, Challenge: challenge.Challenge, IdentityToken: "subject-one",
			AuthorizationCode: "authorization-" + commandID, InstallationID: "concurrent-installation",
			Profile: &identity.ProfileInput{DisplayName: "Parent One", Confirmed: true}}
	}
	firstRequest := request(fixture.nextID(7))
	secondRequest := request(fixture.nextID(7))
	firstResult := make(chan error, 1)
	secondResult := make(chan error, 1)
	go func() {
		_, exchangeErr := fixture.Service.Exchange(t.Context(), firstRequest)
		firstResult <- exchangeErr
	}()
	if call := <-fixture.Credentials.rotateEntered; call != 1 {
		t.Fatalf("first rotate call=%d", call)
	}
	<-fixture.Apple.exchangeEntered
	go func() {
		_, exchangeErr := fixture.Service.Exchange(t.Context(), secondRequest)
		secondResult <- exchangeErr
	}()
	secondErr := <-secondResult
	close(fixture.Apple.exchangeRelease)
	<-fixture.Credentials.firstRotateComplete
	close(fixture.Credentials.releaseFirstRotate)
	firstErr := <-firstResult
	if firstErr != nil || identity.Code(secondErr) != "credential_rotation_busy" {
		t.Fatalf("first=%v second=%v second_code=%s", firstErr, secondErr, identity.Code(secondErr))
	}
	exchanges, _ := fixture.Apple.counts()
	if exchanges != 1 {
		t.Fatalf("Apple exchanges=%d", exchanges)
	}
}

func TestProvisionalActorChangeAbandonsPromotionWithoutSession(t *testing.T) {
	fixture := newHostedSessionFixture(t)
	fixture.Credentials.beforeFirstPrepare = func() error {
		challenge, err := fixture.Identity.Challenge(t.Context())
		if err != nil {
			return err
		}
		_, err = fixture.SelfHosted.Exchange(t.Context(), identity.ExchangeRequest{CommandID: fixture.nextID(3), Challenge: challenge.Challenge,
			IdentityToken: "subject-one", InstallationID: "competing-installation",
			Profile: &identity.ProfileInput{DisplayName: "Parent One", Confirmed: true}})
		return err
	}
	_, hostedCommand, err := fixture.exchangeHosted("subject-one", &identity.ProfileInput{DisplayName: "Parent One", Confirmed: true}, nil)
	if Code(err) != "security_busy" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	var stageCount, receiptCount int
	if err := fixture.db.Owner.QueryRow(t.Context(), `SELECT count(*) FROM auth_exchange_material WHERE command_id=$1`, hostedCommand).Scan(&stageCount); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Owner.QueryRow(t.Context(), `SELECT count(*) FROM security_command_receipts WHERE command_id=$1`, hostedCommand).Scan(&receiptCount); err != nil {
		t.Fatal(err)
	}
	if stageCount != 0 || receiptCount != 0 {
		t.Fatalf("abandoned hosted command staged=%d receipts=%d", stageCount, receiptCount)
	}
	accountID, actorErr := ActorID(fixture.actorKey, fixture.instance, identity.VerifiedIdentity{Provider: "apple", Audience: "com.example.babytracker", Subject: "subject-one"}, 1)
	if actorErr != nil {
		t.Fatal(actorErr)
	}
	binding, bindingErr := identity.NewAppleCredentialBinding(accountID, fixture.instance)
	if bindingErr != nil {
		t.Fatal(bindingErr)
	}
	state, exists, resolveErr := fixture.Vault.Resolve(t.Context(), binding)
	if resolveErr != nil || !exists || state.Phase != "aborting" {
		t.Fatalf("state=%+v exists=%t err=%v", state, exists, resolveErr)
	}
}

func TestSwappedReturnedIdentityAbandonsReservationBeforeCandidateOrAuthorityMutation(t *testing.T) {
	fixture := newHostedSessionFixture(t)
	fixture.Apple.returnedSubject = "swapped-subject"
	_, commandID, err := fixture.exchangeHosted("subject-one", &identity.ProfileInput{DisplayName: "Parent One", Confirmed: true}, nil)
	if identity.Code(err) != "invalid_apple_proof" {
		t.Fatalf("identity code=%s err=%v", identity.Code(err), err)
	}
	accountID, actorErr := ActorID(fixture.actorKey, fixture.instance, identity.VerifiedIdentity{Provider: "apple", Audience: "com.example.babytracker", Subject: "subject-one"}, 1)
	if actorErr != nil {
		t.Fatal(actorErr)
	}
	binding, bindingErr := identity.NewAppleCredentialBinding(accountID, fixture.instance)
	if bindingErr != nil {
		t.Fatal(bindingErr)
	}
	state, exists, resolveErr := fixture.Vault.Resolve(t.Context(), binding)
	if resolveErr != nil || !exists || state.Phase != "aborting" || state.HasPending {
		t.Fatalf("state=%+v exists=%t err=%v", state, exists, resolveErr)
	}
	var stages, receipts int
	if err := fixture.db.Owner.QueryRow(t.Context(), `SELECT count(*) FROM auth_exchange_material WHERE command_id=$1`, commandID).Scan(&stages); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Owner.QueryRow(t.Context(), `SELECT count(*) FROM security_command_receipts WHERE command_id=$1`, commandID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if stages != 0 || receipts != 0 {
		t.Fatalf("swapped identity mutated authority: stages=%d receipts=%d", stages, receipts)
	}
}
