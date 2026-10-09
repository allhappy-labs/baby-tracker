package identity

import (
	"context"
	"crypto/sha256"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/testpg"
)

func TestChallengeSingleUseAcrossServiceInstances(t *testing.T) {
	fixture := testpg.New(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	repository, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	apple := newAppleVerifierFixture(t)
	verifier := apple.verifier()
	options := Options{
		Verifier:   verifier,
		Repository: repository,
		Origin:     "https://sync.example.test", InstanceID: "50000000-0000-4000-8000-000000000001",
		IdentityKeys: testKeyring(), Now: func() time.Time { return now },
	}
	service, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	secondService, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := service.Challenge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	apple.nonce, err = AppleNonce(challenge.CanonicalOrigin, challenge.ServerInstanceID, challenge.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	token := signJWT(t, apple.key, `{"alg":"RS256","kid":"apple-key"}`, apple.claims(""))
	request := ExchangeRequest{CommandID: "30000000-0000-4000-8000-000000000001", Challenge: challenge.Challenge,
		IdentityToken: token, InstallationID: "installation", Profile: &ProfileInput{DisplayName: "Parent", Confirmed: true}}
	var wins atomic.Int32
	var wait sync.WaitGroup
	for _, candidate := range []*Service{service, secondService} {
		wait.Add(1)
		go func(candidate *Service) {
			defer wait.Done()
			if _, err := candidate.VerifyExchange(t.Context(), request); err == nil {
				wins.Add(1)
			}
		}(candidate)
	}
	wait.Wait()
	if wins.Load() != 1 {
		t.Fatalf("successful exchanges=%d", wins.Load())
	}
}

func TestFailedAppleProofBurnsChallengeAndCrossBindingCannotConsume(t *testing.T) {
	fixture := testpg.New(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	repository, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	apple := newAppleVerifierFixture(t)
	options := Options{Verifier: apple.verifier(), Repository: repository, Origin: "https://sync.example.test",
		InstanceID: "50000000-0000-4000-8000-000000000001", IdentityKeys: testKeyring(), Now: func() time.Time { return now }}
	service, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := service.Challenge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	expectedNonce, err := AppleNonce(challenge.CanonicalOrigin, challenge.ServerInstanceID, challenge.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	apple.nonce = strings.Repeat("b", 64)
	badToken := signJWT(t, apple.key, `{"alg":"RS256","kid":"apple-key"}`, apple.claims(""))
	request := ExchangeRequest{CommandID: "30000000-0000-4000-8000-000000000011", Challenge: challenge.Challenge,
		IdentityToken: badToken, InstallationID: "installation", Profile: &ProfileInput{DisplayName: "Parent", Confirmed: true}}
	if _, err := service.VerifyExchange(t.Context(), request); Code(err) != "invalid_apple_proof" {
		t.Fatalf("bad proof code=%q err=%v", Code(err), err)
	} else if strings.Contains(err.Error(), badToken) || strings.Contains(err.Error(), "Parent") {
		t.Fatalf("identity error exposed proof/profile material: %v", err)
	}
	apple.nonce = expectedNonce
	request.IdentityToken = signJWT(t, apple.key, `{"alg":"RS256","kid":"apple-key"}`, apple.claims(""))
	if _, err := service.VerifyExchange(t.Context(), request); Code(err) != "invalid_or_expired_challenge" {
		t.Fatalf("retry code=%q err=%v", Code(err), err)
	}

	challenge, err = service.Challenge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	apple.nonce, _ = AppleNonce(challenge.CanonicalOrigin, challenge.ServerInstanceID, challenge.Challenge)
	request.Challenge = challenge.Challenge
	request.IdentityToken = signJWT(t, apple.key, `{"alg":"RS256","kid":"apple-key"}`, apple.claims(""))
	otherOptions := options
	otherOptions.Origin = "https://other.example.test"
	other, err := New(otherOptions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.VerifyExchange(t.Context(), request); Code(err) != "invalid_or_expired_challenge" {
		t.Fatalf("cross-origin code=%q err=%v", Code(err), err)
	}
	if _, err := service.VerifyExchange(t.Context(), request); err != nil {
		t.Fatalf("cross-origin attempt consumed challenge: %v", err)
	}

	challenge, err = service.Challenge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	apple.nonce, _ = AppleNonce(challenge.CanonicalOrigin, challenge.ServerInstanceID, challenge.Challenge)
	request.Challenge = challenge.Challenge
	request.IdentityToken = signJWT(t, apple.key, `{"alg":"RS256","kid":"apple-key"}`, apple.claims(""))
	otherOptions = options
	otherOptions.InstanceID = "50000000-0000-4000-8000-000000000002"
	other, err = New(otherOptions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.VerifyExchange(t.Context(), request); Code(err) != "invalid_or_expired_challenge" {
		t.Fatalf("cross-instance code=%q err=%v", Code(err), err)
	}
	if _, err := service.VerifyExchange(t.Context(), request); err != nil {
		t.Fatalf("cross-instance attempt consumed challenge: %v", err)
	}
}

func TestProfileNameRequiredOnlyForNewOrMissingProfile(t *testing.T) {
	fixture := testpg.New(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	repository, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	apple := newAppleVerifierFixture(t)
	service, err := New(Options{Verifier: apple.verifier(), Repository: repository, Origin: "https://sync.example.test",
		InstanceID: "50000000-0000-4000-8000-000000000001", IdentityKeys: testKeyring(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := service.Challenge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	apple.nonce, err = AppleNonce(challenge.CanonicalOrigin, challenge.ServerInstanceID, challenge.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.VerifyExchange(t.Context(), ExchangeRequest{CommandID: "30000000-0000-4000-8000-000000000002",
		Challenge: challenge.Challenge, IdentityToken: signJWT(t, apple.key, `{"alg":"RS256","kid":"apple-key"}`, apple.claims("")), InstallationID: "installation"})
	if Code(err) != "display_name_required" {
		t.Fatalf("code=%q err=%v", Code(err), err)
	}
}

func TestExistingProfileIsRetainedAndProfileRequiredStillNeedsConfirmation(t *testing.T) {
	fixture := testpg.New(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	keyring := testKeyring()
	digest := identityDigest(keyring.Current, "apple", testAppleAudience, "apple-subject")
	accountID := "10000000-0000-4000-8000-000000000088"
	owner, err := fixture.Owner.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	if _, err := owner.Exec(t.Context(), `SELECT set_config('app.account_id',$1,false)`, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `INSERT INTO accounts(id,display_name,profile_required,created_at,updated_at) VALUES($1,'Existing Parent',false,$2,$2)`, accountID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `INSERT INTO account_security_state(account_id,identity_key,status,generation,revision,event_hash)
		VALUES($1,$2,'active',1,1,decode(repeat('88',32),'hex'))`, accountID, authorityDigest(digest)); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `INSERT INTO account_identities(provider,identity_key_id,identity_digest,account_id,identity_audience,identity_subject,material_missing,created_at,updated_at)
		VALUES('apple',$2,$3,$1,$4,'apple-subject',false,$5,$5)`, accountID, digest.KeyID, digest.Sum[:], testAppleAudience, now); err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	apple := newAppleVerifierFixture(t)
	service, err := New(Options{Verifier: apple.verifier(), Repository: repository, Origin: "https://sync.example.test", InstanceID: "50000000-0000-4000-8000-000000000001",
		IdentityKeys: keyring, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := service.Challenge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	apple.nonce, err = AppleNonce(challenge.CanonicalOrigin, challenge.ServerInstanceID, challenge.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	request := ExchangeRequest{CommandID: "30000000-0000-4000-8000-000000000021", Challenge: challenge.Challenge,
		IdentityToken: signJWT(t, apple.key, `{"alg":"RS256","kid":"apple-key"}`, apple.claims("")), InstallationID: "installation"}
	if _, err := service.VerifyExchange(t.Context(), request); err != nil {
		t.Fatalf("existing complete profile required a new name: %v", err)
	}
	if _, err := owner.Exec(t.Context(), `UPDATE accounts SET profile_required=true WHERE id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	challenge, err = service.Challenge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request.Challenge = challenge.Challenge
	request.CommandID = "30000000-0000-4000-8000-000000000022"
	apple.nonce, err = AppleNonce(challenge.CanonicalOrigin, challenge.ServerInstanceID, challenge.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	request.IdentityToken = signJWT(t, apple.key, `{"alg":"RS256","kid":"apple-key"}`, apple.claims(""))
	if _, err := service.VerifyExchange(t.Context(), request); Code(err) != "display_name_required" {
		t.Fatalf("profile-required code=%q err=%v", Code(err), err)
	}
}

func TestIdentityDigestBindsProviderAudienceAndSubject(t *testing.T) {
	keyring := testKeyring()
	a := identityDigest(keyring.Current, "apple", "audience-a", "subject")
	b := identityDigest(keyring.Current, "apple", "audience-b", "subject")
	c := identityDigest(keyring.Current, "apple", "audience-a", "subject-b")
	if a == b || a == c || b == c {
		t.Fatal("identity digest omitted a binding")
	}
	if sha256.Sum256([]byte("apple\x00audience-a\x00subject")) == a.Sum {
		t.Fatal("identity digest was not keyed")
	}
}

func TestVerifyExchangeWithProfileReturnsNormalizedDefensiveProfile(t *testing.T) {
	fixture := testpg.New(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	repository, err := NewRepository(fixture.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	apple := newAppleVerifierFixture(t)
	service, err := New(Options{Verifier: apple.verifier(), Repository: repository, Origin: "https://sync.example.test",
		InstanceID: "50000000-0000-4000-8000-000000000001", IdentityKeys: testKeyring(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := service.Challenge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	apple.nonce, err = AppleNonce(challenge.CanonicalOrigin, challenge.ServerInstanceID, challenge.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	request := ExchangeRequest{CommandID: "30000000-0000-4000-8000-000000000031", Challenge: challenge.Challenge,
		IdentityToken: signJWT(t, apple.key, `{"alg":"RS256","kid":"apple-key"}`, apple.claims("")), InstallationID: "installation",
		Profile: &ProfileInput{DisplayName: "  Parent  ", Confirmed: true}}
	got, err := service.VerifyExchangeWithProfile(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile == nil || got.Profile.DisplayName != "Parent" || !got.Profile.Confirmed {
		t.Fatalf("profile=%+v", got.Profile)
	}
	request.Profile.DisplayName = "Changed"
	if got.Profile.DisplayName != "Parent" {
		t.Fatal("verified profile aliases request memory")
	}
}

func TestAuthorityKeyCandidatesAreCurrentFirstAndDefensive(t *testing.T) {
	keyring := testKeyring()
	previous := keyring.Current
	previous.ID = "identity.v0"
	previous.Secret[0] ^= 0xff
	keyring.Previous = []DigestKey{previous}
	service := &Service{identityKeys: keyring}
	verified := VerifiedIdentity{Provider: "apple", Audience: "com.example", Subject: "000123"}

	got, err := service.AuthorityKeyCandidates(verified)
	if err != nil {
		t.Fatal(err)
	}
	wantCurrent := authorityDigest(identityDigest(keyring.Current, verified.Provider, verified.Audience, verified.Subject))
	wantPrevious := authorityDigest(identityDigest(previous, verified.Provider, verified.Audience, verified.Subject))
	if len(got) != 2 || got[0] != wantCurrent || got[1] != wantPrevious {
		t.Fatalf("candidates=%v", got)
	}
	got[0] = "mutated"
	again, err := service.AuthorityKeyCandidates(verified)
	if err != nil || again[0] != wantCurrent {
		t.Fatalf("defensive candidates=%v err=%v", again, err)
	}
	if _, err := service.AuthorityKeyCandidates(VerifiedIdentity{Provider: "google", Audience: "com.example", Subject: "000123"}); err == nil {
		t.Fatal("unverified provider accepted")
	}
}

func TestIdentityLookupRejectsSecondSameAccountMatchAfterAllCandidates(t *testing.T) {
	keyring := testKeyring()
	for _, id := range []string{"identity.v0", "identity.v-1"} {
		previous := keyring.Current
		previous.ID = id
		previous.Secret[0] ^= byte(len(keyring.Previous) + 1)
		keyring.Previous = append(keyring.Previous, previous)
	}
	verified := VerifiedIdentity{Provider: "apple", Audience: "com.example", Subject: "000123"}
	var calls []string
	service := &Service{identityKeys: keyring, lookupIdentity: func(_ context.Context, _ string, digest KeyedDigest) (identityLookup, bool, error) {
		calls = append(calls, digest.KeyID)
		return identityLookup{AccountID: "10000000-0000-4000-8000-000000000001"}, len(calls) <= 2, nil
	}}

	if _, _, _, err := service.lookupIdentityCandidates(t.Context(), verified); err != ErrUnavailable {
		t.Fatalf("duplicate same-account identity error=%v", err)
	}
	want := []string{"identity.v1", "identity.v0", "identity.v-1"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("lookup calls=%v want=%v", calls, want)
	}
}

func testKeyring() DigestKeyring {
	var secret [32]byte
	for index := range secret {
		secret[index] = byte(index + 1)
	}
	return DigestKeyring{Current: DigestKey{ID: "identity.v1", Secret: secret}}
}
