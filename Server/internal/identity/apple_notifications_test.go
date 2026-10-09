package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/security"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	notificationInstanceID = "50000000-0000-4000-8000-000000000001"
	notificationAccountID  = "10000000-0000-4000-8000-000000000001"
)

type notificationRepositorySpy struct {
	mu           sync.Mutex
	lookupCalls  []KeyedDigest
	reserveCalls [][]KeyedDigest
	lookups      map[KeyedDigest]AuthIdentityLookup
	reservations map[KeyedDigest]string
	reserveError error
	reserveHook  func()
}

func (repository *notificationRepositorySpy) LookupAuthIdentity(_ context.Context, _ string, keyID string, digest [32]byte) (AuthIdentityLookup, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()
	candidate := KeyedDigest{KeyID: keyID, Sum: digest}
	repository.lookupCalls = append(repository.lookupCalls, candidate)
	return repository.lookups[candidate], nil
}

func (repository *notificationRepositorySpy) ReserveAppleNotification(_ context.Context, candidates []KeyedDigest, commandID string, _ time.Time) (string, bool, error) {
	if repository.reserveHook != nil {
		repository.reserveHook()
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	repository.reserveCalls = append(repository.reserveCalls, append([]KeyedDigest(nil), candidates...))
	if repository.reserveError != nil {
		return "", false, repository.reserveError
	}
	var stored string
	for _, candidate := range candidates {
		if match := repository.reservations[candidate]; match != "" {
			if stored != "" && stored != match {
				return "", false, errors.New("multiple notification reservation matches")
			}
			stored = match
		}
	}
	if stored != "" {
		return stored, false, nil
	}
	if repository.reservations == nil {
		repository.reservations = make(map[KeyedDigest]string)
	}
	repository.reservations[candidates[0]] = commandID
	return commandID, true, nil
}

func TestNotificationCommandIDKnownAnswer(t *testing.T) {
	key := DigestKey{ID: "notification.v1"}
	for index := range key.Secret {
		key.Secret[index] = 1
	}
	commandID, digest := NotificationCommandID(key, appleIssuer, testAppleAudience, "notification-jti-1")
	if commandID != "e871d351-93d8-8913-b112-5e2b8eb031e9" {
		t.Fatalf("command ID=%q", commandID)
	}
	if digest.KeyID != key.ID || fmt.Sprintf("%x", digest.Sum) != "e871d35193d8d913f1125e2b8eb031e94fb64ceb94199dde01d60997b40d7670" {
		t.Fatalf("digest=%s:%x", digest.KeyID, digest.Sum)
	}
}

func TestNotificationRepositoryReservationIsDuplicateSafe(t *testing.T) {
	database := testpg.New(t)
	repository, err := NewRepository(database.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	keys := notificationKeyring()
	firstCommand, firstDigest := NotificationCommandID(keys.Current, appleIssuer, testAppleAudience, "database-jti")
	stored, created, err := repository.ReserveAppleNotification(t.Context(), []KeyedDigest{firstDigest}, firstCommand,
		time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	if err != nil || !created || stored != firstCommand {
		t.Fatalf("first stored=%q created=%v err=%v", stored, created, err)
	}
	changedCommand := "30000000-0000-4000-8000-000000000099"
	stored, created, err = repository.ReserveAppleNotification(t.Context(), []KeyedDigest{firstDigest}, changedCommand,
		time.Date(2026, 9, 20, 12, 0, 1, 0, time.UTC))
	if err != nil || created || stored != firstCommand {
		t.Fatalf("replay stored=%q created=%v err=%v", stored, created, err)
	}
}

func TestAppleNotificationEmailEventValidatesWithoutPersistence(t *testing.T) {
	fixture := newNotificationFixture(t)
	repository := &notificationRepositorySpy{}
	service := fixture.service(t, repository)
	body := fixture.body(t, `{"alg":"RS256","kid":"apple-key"}`, fixture.claims(
		`{"event_time":1790000000,"sub":"apple-subject","type":"email-disabled","email":"parent@example.test","is_private_email":"false"}`,
	))
	if err := service.Handle(t.Context(), body); err != nil {
		t.Fatal(err)
	}
	if len(repository.lookupCalls) != 0 || len(repository.reserveCalls) != 0 {
		t.Fatalf("email notification persisted: lookups=%d reservations=%d", len(repository.lookupCalls), len(repository.reserveCalls))
	}
}

func TestAppleNotificationAcceptsNoExpiryAndInclusiveAgeBounds(t *testing.T) {
	fixture := newNotificationFixture(t)
	service := fixture.service(t, newNotificationRepository())
	for _, offset := range []time.Duration{-24 * time.Hour, time.Minute} {
		t.Run(offset.String(), func(t *testing.T) {
			at := fixture.now.Add(offset).Unix()
			event := fmt.Sprintf(`{"event_time":%d,"sub":"apple-subject","type":"email-enabled"}`, at)
			claims := fmt.Sprintf(`{"iss":%q,"aud":%q,"iat":%d,"jti":"boundary-%d","events":%s}`,
				appleIssuer, testAppleAudience, at, at, event)
			if strings.Contains(claims, `"exp"`) {
				t.Fatal("fixture unexpectedly contains exp")
			}
			if err := service.Handle(t.Context(), fixture.body(t, `{"alg":"RS256","kid":"apple-key"}`, claims)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAppleNotificationStrictValidation(t *testing.T) {
	fixture := newNotificationFixture(t)
	validHeader := `{"alg":"RS256","kid":"apple-key"}`
	validEvent := `{"event_time":1790000000,"sub":"apple-subject","type":"email-enabled","email":"parent@example.test","is_private_email":"true"}`
	validClaims := fixture.claims(validEvent)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		body func() string
	}{
		{name: "outer duplicate", body: func() string {
			token := signJWT(t, fixture.apple.key, validHeader, validClaims)
			return `{"payload":` + quoted(token) + `,"\u0070ayload":` + quoted(token) + `}`
		}},
		{name: "outer unknown", body: func() string {
			token := signJWT(t, fixture.apple.key, validHeader, validClaims)
			return `{"payload":` + quoted(token) + `,"extra":true}`
		}},
		{name: "outer payload not string", body: func() string { return `{"payload":{}}` }},
		{name: "oversized body", body: func() string { return strings.Repeat("x", maxAppleNotificationBodyBytes+1) }},
		{name: "wrong algorithm", body: func() string { return fixture.body(t, `{"alg":"ES256","kid":"apple-key"}`, validClaims) }},
		{name: "unknown header", body: func() string { return fixture.body(t, `{"alg":"RS256","kid":"apple-key","typ":"JWT"}`, validClaims) }},
		{name: "duplicate header", body: func() string {
			return fixture.body(t, `{"alg":"RS256","\u0061lg":"RS256","kid":"apple-key"}`, validClaims)
		}},
		{name: "wrong signature", body: func() string {
			return notificationEnvelope(signJWT(t, otherKey, validHeader, validClaims))
		}},
		{name: "wrong issuer", body: func() string {
			return fixture.body(t, validHeader, strings.Replace(validClaims, appleIssuer, "https://attacker.test", 1))
		}},
		{name: "wrong audience", body: func() string {
			return fixture.body(t, validHeader, strings.Replace(validClaims, testAppleAudience, "other.audience", 1))
		}},
		{name: "expiry forbidden", body: func() string {
			return fixture.body(t, validHeader, strings.TrimSuffix(validClaims, "}")+`,"exp":1790000060}`)
		}},
		{name: "unknown claim", body: func() string {
			return fixture.body(t, validHeader, strings.TrimSuffix(validClaims, "}")+`,"nonce":"x"}`)
		}},
		{name: "duplicate claim", body: func() string {
			return fixture.body(t, validHeader, strings.TrimSuffix(validClaims, "}")+`,"\u006aud":"other"}`)
		}},
		{name: "events string", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(`"not-an-object"`))
		}},
		{name: "unknown event field", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(strings.TrimSuffix(validEvent, "}")+`,"extra":true}`))
		}},
		{name: "duplicate event field", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(strings.TrimSuffix(validEvent, "}")+`,"\u0073ub":"other"}`))
		}},
		{name: "fractional issued at", body: func() string {
			return fixture.body(t, validHeader, strings.Replace(validClaims, `"iat":1790000000`, `"iat":1790000000.5`, 1))
		}},
		{name: "fractional event time", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(strings.Replace(validEvent, `"event_time":1790000000`, `"event_time":1790000000.5`, 1)))
		}},
		{name: "issued too old", body: func() string {
			return fixture.body(t, validHeader, strings.Replace(validClaims, `"iat":1790000000`, `"iat":1789913599`, 1))
		}},
		{name: "event too far future", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(strings.Replace(validEvent, `"event_time":1790000000`, `"event_time":1790000061`, 1)))
		}},
		{name: "control jti", body: func() string {
			return fixture.body(t, validHeader, strings.Replace(validClaims, `"jti":"notification-jti-1"`, `"jti":"bad\u0000jti"`, 1))
		}},
		{name: "long subject", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(strings.Replace(validEvent, `"sub":"apple-subject"`, `"sub":"`+strings.Repeat("s", 256)+`"`, 1)))
		}},
		{name: "email field on destructive event", body: func() string {
			event := strings.Replace(validEvent, `"type":"email-enabled"`, `"type":"consent-revoked"`, 1)
			return fixture.body(t, validHeader, fixture.claims(event))
		}},
		{name: "boolean private email", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(strings.Replace(validEvent, `"is_private_email":"true"`, `"is_private_email":true`, 1)))
		}},
		{name: "unknown type", body: func() string {
			return fixture.body(t, validHeader, fixture.claims(strings.Replace(validEvent, `"type":"email-enabled"`, `"type":"profile-updated"`, 1)))
		}},
	}
	service := fixture.service(t, &notificationRepositorySpy{})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := service.Handle(t.Context(), test.body()); Code(err) != "invalid_apple_proof" {
				t.Fatalf("code=%q err=%v", Code(err), err)
			}
		})
	}
}

func TestAppleNotificationDestructiveEventLooksUpEveryIdentityKeyAndCommits(t *testing.T) {
	fixture := newNotificationFixture(t)
	previous := DigestKey{ID: "identity.previous"}
	for index := range previous.Secret {
		previous.Secret[index] = 0x55
	}
	identityKeys := testKeyring()
	identityKeys.Previous = []DigestKey{previous}
	fixture.identityKeys = identityKeys
	fixture.authority = newNotificationAuthority(t, identityKeys, previous, "apple-subject", fixture.now)
	previousDigest := identityDigest(previous, "apple", testAppleAudience, "apple-subject")
	repository := newNotificationRepository()
	repository.lookups[previousDigest] = AuthIdentityLookup{AccountID: notificationAccountID, AuthorityStatus: "active", DisplayName: "Parent"}
	service := fixture.service(t, repository)

	if err := service.Handle(t.Context(), fixture.destructiveBody(t, "consent-revoked")); err != nil {
		t.Fatal(err)
	}
	if len(repository.lookupCalls) != 2 || repository.lookupCalls[0].KeyID != identityKeys.Current.ID || repository.lookupCalls[1].KeyID != previous.ID {
		t.Fatalf("identity lookups=%+v", repository.lookupCalls)
	}
	if len(repository.reserveCalls) != 1 || len(repository.reserveCalls[0]) != 1 {
		t.Fatalf("reservation calls=%+v", repository.reserveCalls)
	}
	state, err := fixture.authority.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	account, ok := state.Account(notificationAccountID)
	if !ok || account.Status != "identity_unavailable" {
		t.Fatalf("account=%+v ok=%v", account, ok)
	}
}

func TestAppleNotificationReceiptPrecedesReservationAfterRestoreAndKeyRotation(t *testing.T) {
	fixture := newNotificationFixture(t)
	repository := fixture.repositoryForSubject("apple-subject")
	body := fixture.destructiveBody(t, "account-deleted")
	oldKeys := notificationKeyring()
	fixture.notificationKeys = oldKeys
	if err := fixture.service(t, repository).Handle(t.Context(), body); err != nil {
		t.Fatal(err)
	}

	rotated := DigestKey{ID: "notification.rotated"}
	for index := range rotated.Secret {
		rotated.Secret[index] = 0x66
	}
	fixture.notificationKeys = DigestKeyring{Current: rotated, Previous: []DigestKey{oldKeys.Current}}
	restoredRepository := fixture.repositoryForSubject("apple-subject") // restored before the SQL dedup row existed
	fixture.authority = fixture.authorityBacking.reopen(t, fixture.now)
	if err := fixture.service(t, restoredRepository).Handle(t.Context(), body); err != nil {
		t.Fatal(err)
	}
	if len(restoredRepository.reserveCalls) != 0 {
		t.Fatalf("authority receipt was checked after SQL reservation: %+v", restoredRepository.reserveCalls)
	}
}

func TestAppleNotificationAuthoritySurvivesMissingIdentityProjection(t *testing.T) {
	t.Run("active authority still processes", func(t *testing.T) {
		fixture := newNotificationFixture(t)
		restoredRepository := newNotificationRepository()
		if err := fixture.service(t, restoredRepository).Handle(t.Context(), fixture.destructiveBody(t, "consent-revoked")); err != nil {
			t.Fatal(err)
		}
		state, err := fixture.authority.ReadState(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		account, _ := state.Account(notificationAccountID)
		if account.Status != "identity_unavailable" {
			t.Fatalf("authority notification was skipped with missing projection: %+v", account)
		}
	})

	t.Run("restored receipt still detects changed event", func(t *testing.T) {
		fixture := newNotificationFixture(t)
		if err := fixture.service(t, fixture.repositoryForSubject("apple-subject")).Handle(t.Context(), fixture.destructiveBody(t, "consent-revoked")); err != nil {
			t.Fatal(err)
		}
		fixture.authority = fixture.authorityBacking.reopen(t, fixture.now)
		restoredRepository := newNotificationRepository()
		if err := fixture.service(t, restoredRepository).Handle(t.Context(), fixture.destructiveBody(t, "account-deleted")); security.Code(err) != "command_id_reused" {
			t.Fatalf("code=%q err=%v", security.Code(err), err)
		}
		if len(restoredRepository.reserveCalls) != 0 {
			t.Fatal("changed restored event reached SQL reservation")
		}
	})
}

func TestAppleNotificationExistingSQLRowWithoutReceiptExecutesAuthorityCommand(t *testing.T) {
	fixture := newNotificationFixture(t)
	repository := fixture.repositoryForSubject("apple-subject")
	body := fixture.destructiveBody(t, "consent-revoked")
	claims := appleNotificationClaims{Issuer: appleIssuer, Audience: testAppleAudience, JTI: "notification-jti-1"}
	commandID, digest := NotificationCommandID(fixture.notificationKeys.Current, claims.Issuer, claims.Audience, claims.JTI)
	repository.reservations[digest] = commandID

	if err := fixture.service(t, repository).Handle(t.Context(), body); err != nil {
		t.Fatal(err)
	}
	if len(repository.reserveCalls) != 1 {
		t.Fatalf("reservation calls=%d", len(repository.reserveCalls))
	}
	state, err := fixture.authority.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	account, _ := state.Account(notificationAccountID)
	if account.Status != "identity_unavailable" {
		t.Fatalf("SQL row was treated as completion: %+v", account)
	}
}

func TestAppleNotificationRetainedKeySQLRowWithoutReceiptExecutesStoredCommand(t *testing.T) {
	fixture := newNotificationFixture(t)
	previous := DigestKey{ID: "notification.previous"}
	for index := range previous.Secret {
		previous.Secret[index] = 0x77
	}
	fixture.notificationKeys.Previous = []DigestKey{previous}
	repository := fixture.repositoryForSubject("apple-subject")
	body := fixture.destructiveBody(t, "consent-revoked")
	storedCommandID, oldDigest := NotificationCommandID(previous, appleIssuer, testAppleAudience, "notification-jti-1")
	repository.reservations[oldDigest] = storedCommandID

	if err := fixture.service(t, repository).Handle(t.Context(), body); err != nil {
		t.Fatal(err)
	}
	state, err := fixture.authority.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	receipt, ok := state.Receipt(notificationAccountID, storedCommandID)
	if !ok || receipt.Result.CommandID != storedCommandID {
		t.Fatalf("stored command receipt=%+v ok=%v", receipt, ok)
	}
}

func TestAppleNotificationChangedEventWithSameJTIConflictsBeforeReservation(t *testing.T) {
	fixture := newNotificationFixture(t)
	repository := fixture.repositoryForSubject("apple-subject")
	service := fixture.service(t, repository)
	if err := service.Handle(t.Context(), fixture.destructiveBody(t, "consent-revoked")); err != nil {
		t.Fatal(err)
	}
	reservations := len(repository.reserveCalls)
	if err := service.Handle(t.Context(), fixture.destructiveBody(t, "account-deleted")); security.Code(err) != "command_id_reused" {
		t.Fatalf("code=%q err=%v", security.Code(err), err)
	}
	if len(repository.reserveCalls) != reservations {
		t.Fatal("changed same-JTI event reached SQL reservation before receipt conflict")
	}
}

func TestAppleNotificationFreshJTIAfterIdentityUnavailableIsIdempotentWithoutReservation(t *testing.T) {
	fixture := newNotificationFixture(t)
	repository := fixture.repositoryForSubject("apple-subject")
	service := fixture.service(t, repository)
	if err := service.Handle(t.Context(), fixture.destructiveBodyWithJTI(t, "consent-revoked", "notification-jti-first")); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.authority.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reservations := len(repository.reserveCalls)
	if err := service.Handle(t.Context(), fixture.destructiveBodyWithJTI(t, "account-deleted", "notification-jti-fresh")); err != nil {
		t.Fatalf("fresh JTI after identity loss returned retryable error: %v", err)
	}
	after, err := fixture.authority.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after != before || after.Revision != 2 {
		t.Fatalf("authority changed for already-achieved state: before=%+v after=%+v", before, after)
	}
	if len(repository.reserveCalls) != reservations {
		t.Fatalf("fresh JTI created SQL reservation after identity loss: before=%d after=%d", reservations, len(repository.reserveCalls))
	}
	if got := fixture.authorityBacking.identityUnavailableEffects(t); got != 1 {
		t.Fatalf("identity_unavailable effects=%d", got)
	}
}

func TestAppleNotificationConcurrentDistinctJTIsConvergeToOneEffect(t *testing.T) {
	fixture := newNotificationFixture(t)
	repository := fixture.repositoryForSubject("apple-subject")
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	repository.reserveHook = func() {
		arrived <- struct{}{}
		<-release
	}
	service := fixture.service(t, repository)
	bodies := []string{
		fixture.destructiveBodyWithJTI(t, "consent-revoked", "notification-jti-concurrent-a"),
		fixture.destructiveBodyWithJTI(t, "account-deleted", "notification-jti-concurrent-b"),
	}
	errorsByRequest := make(chan error, len(bodies))
	for _, body := range bodies {
		body := body
		go func() { errorsByRequest <- service.Handle(t.Context(), body) }()
	}
	for range bodies {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			t.Fatal("both notifications did not reach reservation barrier")
		}
	}
	close(release)
	for range bodies {
		if err := <-errorsByRequest; err != nil {
			t.Fatalf("concurrent notification did not converge: %v", err)
		}
	}
	head, err := fixture.authority.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if head.Revision != 2 {
		t.Fatalf("authority revision=%d", head.Revision)
	}
	if got := fixture.authorityBacking.identityUnavailableEffects(t); got != 1 {
		t.Fatalf("identity_unavailable effects=%d", got)
	}
	if len(repository.reserveCalls) != 2 {
		t.Fatalf("reservation attempts=%d, want one per delivery", len(repository.reserveCalls))
	}
}

func TestAppleNotificationFailsClosedOnMultipleIdentityOrReservationMatches(t *testing.T) {
	t.Run("identity matches", func(t *testing.T) {
		fixture := newNotificationFixture(t)
		previous := DigestKey{ID: "identity.previous"}
		for index := range previous.Secret {
			previous.Secret[index] = 0x55
		}
		fixture.identityKeys.Previous = []DigestKey{previous}
		repository := fixture.repositoryForSubject("apple-subject")
		repository.lookups[identityDigest(previous, "apple", testAppleAudience, "apple-subject")] = AuthIdentityLookup{
			AccountID: "10000000-0000-4000-8000-000000000002", AuthorityStatus: "active",
		}
		if err := fixture.service(t, repository).Handle(t.Context(), fixture.destructiveBody(t, "consent-revoked")); Code(err) != "service_unavailable" {
			t.Fatalf("code=%q err=%v", Code(err), err)
		}
		if len(repository.lookupCalls) != 2 || len(repository.reserveCalls) != 0 {
			t.Fatalf("lookups=%d reservations=%d", len(repository.lookupCalls), len(repository.reserveCalls))
		}
	})

	t.Run("reservation matches", func(t *testing.T) {
		fixture := newNotificationFixture(t)
		previous := DigestKey{ID: "notification.previous"}
		for index := range previous.Secret {
			previous.Secret[index] = 0x77
		}
		fixture.notificationKeys.Previous = []DigestKey{previous}
		repository := fixture.repositoryForSubject("apple-subject")
		_, current := NotificationCommandID(fixture.notificationKeys.Current, appleIssuer, testAppleAudience, "notification-jti-1")
		_, old := NotificationCommandID(previous, appleIssuer, testAppleAudience, "notification-jti-1")
		repository.reservations[current] = "30000000-0000-4000-8000-000000000090"
		repository.reservations[old] = "30000000-0000-4000-8000-000000000091"
		if err := fixture.service(t, repository).Handle(t.Context(), fixture.destructiveBody(t, "consent-revoked")); Code(err) != "service_unavailable" {
			t.Fatalf("code=%q err=%v", Code(err), err)
		}
	})
}

type notificationFixture struct {
	apple            *appleVerifierFixture
	now              time.Time
	identityKeys     DigestKeyring
	notificationKeys DigestKeyring
	authority        *security.Service
	authorityBacking *notificationAuthorityBacking
}

func newNotificationFixture(t *testing.T) *notificationFixture {
	t.Helper()
	apple := newAppleVerifierFixture(t)
	apple.now = time.Unix(1790000000, 0).UTC()
	identityKeys := testKeyring()
	authority, backing := newNotificationAuthorityWithBacking(t, identityKeys, identityKeys.Current, "apple-subject", apple.now)
	return &notificationFixture{apple: apple, now: apple.now, identityKeys: identityKeys, notificationKeys: notificationKeyring(),
		authority: authority, authorityBacking: backing}
}

func (fixture *notificationFixture) service(t *testing.T, repository NotificationRepository) *NotificationService {
	t.Helper()
	service, err := NewNotificationService(NotificationOptions{
		Audience: testAppleAudience, InstanceID: notificationInstanceID, HTTP: fixture.apple.client,
		IdentityKeys: fixture.identityKeys, NotificationKeys: fixture.notificationKeys, Repository: repository,
		Security: fixture.authority, Now: func() time.Time { return fixture.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func (fixture *notificationFixture) destructiveBody(t *testing.T, eventType string) string {
	return fixture.destructiveBodyWithJTI(t, eventType, "notification-jti-1")
}

func (fixture *notificationFixture) destructiveBodyWithJTI(t *testing.T, eventType, jti string) string {
	t.Helper()
	event := fmt.Sprintf(`{"event_time":%d,"sub":"apple-subject","type":%q}`, fixture.now.Unix(), eventType)
	claims := fmt.Sprintf(`{"iss":%q,"aud":%q,"iat":%d,"jti":%q,"events":%s}`,
		appleIssuer, testAppleAudience, fixture.now.Unix(), jti, event)
	return fixture.body(t, `{"alg":"RS256","kid":"apple-key"}`, claims)
}

func (fixture *notificationFixture) repositoryForSubject(subject string) *notificationRepositorySpy {
	repository := newNotificationRepository()
	digest := identityDigest(fixture.identityKeys.Current, "apple", testAppleAudience, subject)
	repository.lookups[digest] = AuthIdentityLookup{AccountID: notificationAccountID, AuthorityStatus: "active", DisplayName: "Parent"}
	return repository
}

func newNotificationRepository() *notificationRepositorySpy {
	return &notificationRepositorySpy{lookups: make(map[KeyedDigest]AuthIdentityLookup), reservations: make(map[KeyedDigest]string)}
}

func (fixture *notificationFixture) claims(event string) string {
	return fmt.Sprintf(`{"iss":%q,"aud":%q,"iat":%d,"jti":"notification-jti-1","events":%s}`,
		appleIssuer, testAppleAudience, fixture.now.Unix(), event)
}

func (fixture *notificationFixture) body(t *testing.T, header, claims string) string {
	t.Helper()
	return notificationEnvelope(signJWT(t, fixture.apple.key, header, claims))
}

func notificationEnvelope(token string) string { return `{"payload":` + quoted(token) + `}` }

func quoted(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func notificationKeyring() DigestKeyring {
	var current [32]byte
	for index := range current {
		current[index] = 0x44
	}
	return DigestKeyring{Current: DigestKey{ID: "notification.current", Secret: current}}
}

var _ NotificationRepository = (*notificationRepositorySpy)(nil)
var _ NotificationRepository = (*Repository)(nil)

type notificationLedger struct {
	mu         sync.Mutex
	candidates []security.Candidate
}

func (ledger *notificationLedger) AppendCandidate(_ context.Context, candidate security.Candidate) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	ledger.candidates = append(ledger.candidates, candidate)
	return nil
}

func (ledger *notificationLedger) ReadCommitted(_ context.Context, head security.Head) ([]security.Candidate, error) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return append([]security.Candidate(nil), ledger.candidates[:head.Revision]...), nil
}

func (ledger *notificationLedger) WalkCommitted(_ context.Context, head security.Head, visit func(security.Candidate) error) error {
	ledger.mu.Lock()
	candidates := append([]security.Candidate(nil), ledger.candidates[:head.Revision]...)
	ledger.mu.Unlock()
	for _, candidate := range candidates {
		if err := visit(candidate); err != nil {
			return err
		}
	}
	return nil
}

type notificationHeads struct {
	mu   sync.Mutex
	head security.Head
}

func (heads *notificationHeads) Load(context.Context) (security.Head, error) {
	heads.mu.Lock()
	defer heads.mu.Unlock()
	return heads.head, nil
}

func (heads *notificationHeads) CompareAndSwap(_ context.Context, prior, next security.Head) (bool, error) {
	heads.mu.Lock()
	defer heads.mu.Unlock()
	if heads.head != prior {
		return false, nil
	}
	heads.head = next
	return true, nil
}

type notificationProjection struct {
	mu   sync.Mutex
	head security.Head
}

func (projection *notificationProjection) Head(context.Context) (security.Head, error) {
	projection.mu.Lock()
	defer projection.mu.Unlock()
	return projection.head, nil
}

func (projection *notificationProjection) Apply(_ context.Context, candidate security.Candidate) error {
	projection.mu.Lock()
	projection.head = security.Head{Generation: candidate.Prior.Generation, Revision: candidate.Prior.Revision + 1, EventHash: candidate.EventHash}
	projection.mu.Unlock()
	return nil
}

type notificationFence struct{ mu sync.RWMutex }

func (fence *notificationFence) Read(context.Context) (func(), error) {
	fence.mu.RLock()
	return fence.mu.RUnlock, nil
}

func (fence *notificationFence) Write(context.Context) (func(), error) {
	fence.mu.Lock()
	return fence.mu.Unlock, nil
}

type notificationAuthorityBacking struct {
	ledger     *notificationLedger
	heads      *notificationHeads
	projection *notificationProjection
	fence      *notificationFence
}

func (backing *notificationAuthorityBacking) identityUnavailableEffects(t *testing.T) int {
	t.Helper()
	backing.ledger.mu.Lock()
	candidates := append([]security.Candidate(nil), backing.ledger.candidates...)
	backing.ledger.mu.Unlock()
	count := 0
	for _, candidate := range candidates {
		var event security.Event
		if err := json.Unmarshal(candidate.EventJSON, &event); err != nil {
			t.Fatal(err)
		}
		for _, effect := range event.Effects {
			if effect.Kind == "identity_unavailable" {
				count++
			}
		}
	}
	return count
}

func (backing *notificationAuthorityBacking) reopen(t *testing.T, now time.Time) *security.Service {
	t.Helper()
	service, err := security.New(security.Options{
		Ledger: backing.ledger, Heads: backing.heads, Projection: backing.projection, Fence: backing.fence,
		Now: func() time.Time { return now }, Runtime: &pgxpool.Pool{},
		Decide: func(context.Context, access.Principal, string, string) (access.Grant, error) {
			return access.Grant{}, nil
		},
		Epoch: func(context.Context) (uint64, error) { return 1, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func newNotificationAuthority(t *testing.T, identityKeys DigestKeyring, authorityKey DigestKey, subject string, now time.Time) *security.Service {
	t.Helper()
	service, _ := newNotificationAuthorityWithBacking(t, identityKeys, authorityKey, subject, now)
	return service
}

func newNotificationAuthorityWithBacking(t *testing.T, identityKeys DigestKeyring, authorityKey DigestKey, subject string, now time.Time) (*security.Service, *notificationAuthorityBacking) {
	t.Helper()
	initial := security.Head{Generation: 1}
	backing := &notificationAuthorityBacking{
		ledger: &notificationLedger{}, heads: &notificationHeads{head: initial},
		projection: &notificationProjection{head: initial}, fence: &notificationFence{},
	}
	service := backing.reopen(t, now)
	identityKey := authorityDigest(identityDigest(authorityKey, "apple", testAppleAudience, subject))
	_, err := service.ExecutePrepared(t.Context(), func(_ context.Context, snapshot security.AuthoritySnapshot) (security.PreparedCommand, error) {
		actor, resolveErr := snapshot.ResolveIdentityActor([]string{identityKey}, notificationAccountID, 1)
		if resolveErr != nil {
			return security.PreparedCommand{}, resolveErr
		}
		return snapshot.PrepareIdentitySessionIssued(actor, security.IdentitySessionCommandInput{
			Meta: security.CommandMeta{CommandID: "30000000-0000-4000-8000-000000000080", At: now},
			Payload: security.IdentitySessionIssuedPayload{
				Version: "v1", AccountID: notificationAccountID, IdentityKey: identityKey, RegisterAccount: true,
				DeviceID: "40000000-0000-4000-8000-000000000001", ServerInstanceID: notificationInstanceID,
				CanonicalOrigin: "https://sync.example.test", DeviceCredentialDigest: "hmac-sha256:device.v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", RegisterDevice: true,
				SessionID: "60000000-0000-4000-8000-000000000001", LineageRootSessionID: "60000000-0000-4000-8000-000000000001",
				RecoveryEpoch: 1, ExpiresAt: now.Add(24 * time.Hour).Format(time.RFC3339Nano), ProjectionMaterialDigest: strings.Repeat("c", 64),
			},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, backing
}
