package identity_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/identity"
)

const (
	testOrigin   = "https://sync.example.test"
	testInstance = "50000000-0000-4000-8000-000000000001"
)

// authoritySpy records identity resolutions and notification reservations
// made through the real authority.
type authoritySpy struct {
	identity.Authority
	mu           sync.Mutex
	resolveCalls [][]string
	reserveCalls [][]identity.KeyedDigest
	reserveHook  func()
}

func (spy *authoritySpy) ResolveIdentity(ctx context.Context, candidates []string) (identity.IdentityAuthority, error) {
	spy.mu.Lock()
	spy.resolveCalls = append(spy.resolveCalls, append([]string(nil), candidates...))
	spy.mu.Unlock()
	return spy.Authority.ResolveIdentity(ctx, candidates)
}

func (spy *authoritySpy) ReserveAppleNotification(ctx context.Context, candidates []identity.KeyedDigest, commandID string, at time.Time) (string, bool, error) {
	if spy.reserveHook != nil {
		spy.reserveHook()
	}
	spy.mu.Lock()
	spy.reserveCalls = append(spy.reserveCalls, append([]identity.KeyedDigest(nil), candidates...))
	spy.mu.Unlock()
	return spy.Authority.ReserveAppleNotification(ctx, candidates, commandID, at)
}

func (spy *authoritySpy) reservations() int {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	return len(spy.reserveCalls)
}

// seedIdentity binds a new active account to the Apple identity subject
// under key, with an active device and account session, and returns the
// account ID.
func seedIdentity(t *testing.T, db *authoritytest.Fixture, key identity.DigestKey, subject string) string {
	t.Helper()
	accountID := db.Account(t)
	digest := identity.IdentityDigestForTest(key, identity.AppleAudienceForTest, subject)
	if n := db.Exec(t, `INSERT INTO account_identities(provider,identity_key_id,identity_digest,account_id,lifecycle_generation,audience,subject,live,created_at)
		VALUES ('apple',$1,$2,$3,1,$4,$5,true,$6)`, digest.KeyID, digest.Sum[:], accountID, identity.AppleAudienceForTest, subject, db.Now); n != 1 {
		t.Fatalf("seeded %d identities", n)
	}
	device := db.Device(t, accountID)
	db.Session(t, accountID, device, "account", "", "")
	return accountID
}

// count reads one count as the table owner in an authority transaction.
func count(t *testing.T, db *authoritytest.Fixture, statement string, args ...any) int {
	t.Helper()
	ctx := context.Background()
	tx, err := db.DB.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(ctx, statement, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", statement, err)
	}
	return n
}

type exchangeFixture struct {
	db    *authoritytest.Fixture
	apple *identity.AppleFixture
	now   time.Time
	spy   *authoritySpy
}

func newExchangeFixture(t *testing.T) *exchangeFixture {
	t.Helper()
	db := authoritytest.New(t)
	apple := identity.NewAppleFixtureForTest(t)
	return &exchangeFixture{db: db, apple: apple, now: apple.Now(), spy: &authoritySpy{Authority: db.Service.IdentityAuthority()}}
}

func (fixture *exchangeFixture) service(t *testing.T, origin, instance string, keys identity.DigestKeyring) *identity.Service {
	t.Helper()
	service, err := identity.New(identity.Options{Verifier: fixture.apple.Verifier(), Authority: fixture.spy, Origin: origin, InstanceID: instance,
		IdentityKeys: keys, Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// request mints a challenge from service and signs a matching identity token.
func (fixture *exchangeFixture) request(t *testing.T, service *identity.Service, commandID string, profile *identity.ProfileInput) identity.ExchangeRequest {
	t.Helper()
	challenge, err := service.Challenge(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := identity.AppleNonce(challenge.CanonicalOrigin, challenge.ServerInstanceID, challenge.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	fixture.apple.SetNonce(nonce)
	return identity.ExchangeRequest{CommandID: commandID, Challenge: challenge.Challenge, IdentityToken: fixture.apple.IdentityToken(t),
		InstallationID: "installation", Profile: profile}
}

func TestChallengeSingleUseAcrossServiceInstances(t *testing.T) {
	fixture := newExchangeFixture(t)
	service := fixture.service(t, testOrigin, testInstance, identity.KeyringForTest())
	secondService := fixture.service(t, testOrigin, testInstance, identity.KeyringForTest())
	request := fixture.request(t, service, "30000000-0000-4000-8000-000000000001", &identity.ProfileInput{DisplayName: "Parent", Confirmed: true})
	var wins atomic.Int32
	var wait sync.WaitGroup
	for _, candidate := range []*identity.Service{service, secondService} {
		wait.Add(1)
		go func(candidate *identity.Service) {
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
	fixture := newExchangeFixture(t)
	service := fixture.service(t, testOrigin, testInstance, identity.KeyringForTest())
	request := fixture.request(t, service, "30000000-0000-4000-8000-000000000011", &identity.ProfileInput{DisplayName: "Parent", Confirmed: true})
	expectedNonce, err := identity.AppleNonce(testOrigin, testInstance, request.Challenge)
	if err != nil {
		t.Fatal(err)
	}
	fixture.apple.SetNonce(strings.Repeat("b", 64))
	badToken := fixture.apple.IdentityToken(t)
	request.IdentityToken = badToken
	if _, err := service.VerifyExchange(t.Context(), request); identity.Code(err) != "invalid_apple_proof" {
		t.Fatalf("bad proof code=%q err=%v", identity.Code(err), err)
	} else if strings.Contains(err.Error(), badToken) || strings.Contains(err.Error(), "Parent") {
		t.Fatalf("identity error exposed proof/profile material: %v", err)
	}
	fixture.apple.SetNonce(expectedNonce)
	request.IdentityToken = fixture.apple.IdentityToken(t)
	if _, err := service.VerifyExchange(t.Context(), request); identity.Code(err) != "invalid_or_expired_challenge" {
		t.Fatalf("retry code=%q err=%v", identity.Code(err), err)
	}

	for _, other := range []struct{ name, origin, instance string }{
		{"cross-origin", "https://other.example.test", testInstance},
		{"cross-instance", testOrigin, "50000000-0000-4000-8000-000000000002"},
	} {
		request = fixture.request(t, service, "30000000-0000-4000-8000-000000000012", &identity.ProfileInput{DisplayName: "Parent", Confirmed: true})
		otherService := fixture.service(t, other.origin, other.instance, identity.KeyringForTest())
		if _, err := otherService.VerifyExchange(t.Context(), request); identity.Code(err) != "invalid_or_expired_challenge" {
			t.Fatalf("%s code=%q err=%v", other.name, identity.Code(err), err)
		}
		if _, err := service.VerifyExchange(t.Context(), request); err != nil {
			t.Fatalf("%s attempt consumed challenge: %v", other.name, err)
		}
	}
}

func TestChallengeExpiresAfterFiveMinutes(t *testing.T) {
	fixture := newExchangeFixture(t)
	service := fixture.service(t, testOrigin, testInstance, identity.KeyringForTest())
	request := fixture.request(t, service, "30000000-0000-4000-8000-000000000013", &identity.ProfileInput{DisplayName: "Parent", Confirmed: true})
	fixture.now = fixture.now.Add(5 * time.Minute)
	if _, err := service.VerifyExchange(t.Context(), request); identity.Code(err) != "invalid_or_expired_challenge" {
		t.Fatalf("expired challenge code=%q err=%v", identity.Code(err), err)
	}
}

func TestProfileNameRequiredOnlyForNewOrMissingProfile(t *testing.T) {
	fixture := newExchangeFixture(t)
	service := fixture.service(t, testOrigin, testInstance, identity.KeyringForTest())
	_, err := service.VerifyExchange(t.Context(), fixture.request(t, service, "30000000-0000-4000-8000-000000000002", nil))
	if identity.Code(err) != "display_name_required" {
		t.Fatalf("code=%q err=%v", identity.Code(err), err)
	}
}

func TestExistingProfileIsRetainedAndProfileRequiredStillNeedsConfirmation(t *testing.T) {
	fixture := newExchangeFixture(t)
	keyring := identity.KeyringForTest()
	accountID := seedIdentity(t, fixture.db, keyring.Current, "apple-subject")
	service := fixture.service(t, testOrigin, testInstance, keyring)
	verified, err := service.VerifyExchangeWithProfile(t.Context(), fixture.request(t, service, "30000000-0000-4000-8000-000000000021", nil))
	if err != nil {
		t.Fatalf("existing complete profile required a new name: %v", err)
	}
	wantKey := identity.AuthorityDigestForTest(identity.IdentityDigestForTest(keyring.Current, identity.AppleAudienceForTest, "apple-subject"))
	if verified.Authority.Status != "active" || verified.Authority.AccountID != accountID || verified.Identity.IdentityKey != wantKey ||
		len(verified.Candidates) != 1 || verified.Candidates[0] != wantKey {
		t.Fatalf("verified=%+v", verified)
	}
	fixture.db.Exec(t, `UPDATE accounts SET profile_required=true WHERE id=$1`, accountID)
	_, err = service.VerifyExchange(t.Context(), fixture.request(t, service, "30000000-0000-4000-8000-000000000022", nil))
	if identity.Code(err) != "display_name_required" {
		t.Fatalf("profile-required code=%q err=%v", identity.Code(err), err)
	}
}

func TestVerifyExchangeWithProfileReturnsNormalizedDefensiveProfile(t *testing.T) {
	fixture := newExchangeFixture(t)
	service := fixture.service(t, testOrigin, testInstance, identity.KeyringForTest())
	request := fixture.request(t, service, "30000000-0000-4000-8000-000000000031", &identity.ProfileInput{DisplayName: "  Parent  ", Confirmed: true})
	got, err := service.VerifyExchangeWithProfile(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile == nil || got.Profile.DisplayName != "Parent" || !got.Profile.Confirmed || got.Authority.Status != "absent" || got.Authority.NextActorGeneration != 1 {
		t.Fatalf("verified=%+v", got)
	}
	request.Profile.DisplayName = "Changed"
	if got.Profile.DisplayName != "Parent" {
		t.Fatal("verified profile aliases request memory")
	}
}

// Every configured identity key is resolved, current first, in one
// authority call; two live accounts bound to the identity under different
// keys are quarantined rather than either being chosen.
func TestIdentityLookupRejectsSecondSameAccountMatchAfterAllCandidates(t *testing.T) {
	fixture := newExchangeFixture(t)
	keyring := identity.KeyringForTest()
	previous := keyring.Current
	previous.ID = "identity.v0"
	previous.Secret[0] ^= 0xff
	keyring.Previous = []identity.DigestKey{previous}
	seedIdentity(t, fixture.db, keyring.Current, "apple-subject")
	seedIdentity(t, fixture.db, previous, "apple-subject")
	service := fixture.service(t, testOrigin, testInstance, keyring)
	_, err := service.VerifyExchange(t.Context(), fixture.request(t, service, "30000000-0000-4000-8000-000000000041", &identity.ProfileInput{DisplayName: "Parent", Confirmed: true}))
	if authority.CodeOf(err) != "authorization_quarantine" {
		t.Fatalf("duplicate identity error=%v", err)
	}
	if len(fixture.spy.resolveCalls) != 1 || len(fixture.spy.resolveCalls[0]) != 2 || !strings.Contains(fixture.spy.resolveCalls[0][0], ":identity.v1:") ||
		!strings.Contains(fixture.spy.resolveCalls[0][1], ":identity.v0:") {
		t.Fatalf("resolutions=%v", fixture.spy.resolveCalls)
	}
}

type notificationFixture struct {
	db                             *authoritytest.Fixture
	apple                          *identity.AppleFixture
	now                            time.Time
	identityKeys, notificationKeys identity.DigestKeyring
	accountID                      string
}

func newNotificationFixture(t *testing.T) *notificationFixture {
	t.Helper()
	db := authoritytest.New(t)
	apple := identity.NewAppleFixtureForTest(t)
	apple.SetNow(time.Unix(1790000000, 0).UTC())
	fixture := &notificationFixture{db: db, apple: apple, now: apple.Now(), identityKeys: identity.KeyringForTest(), notificationKeys: notificationKeyring(0x44, "notification.current")}
	fixture.accountID = seedIdentity(t, db, fixture.identityKeys.Current, "apple-subject")
	return fixture
}

func (fixture *notificationFixture) spy() *authoritySpy {
	return &authoritySpy{Authority: fixture.db.Service.IdentityAuthority()}
}

func (fixture *notificationFixture) service(t *testing.T, spy *authoritySpy) *identity.NotificationService {
	t.Helper()
	service, err := identity.NewNotificationService(identity.NotificationOptions{Audience: identity.AppleAudienceForTest, InstanceID: testInstance,
		HTTP: fixture.apple.Client(), IdentityKeys: fixture.identityKeys, NotificationKeys: fixture.notificationKeys, Authority: spy,
		Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func (fixture *notificationFixture) body(t *testing.T, eventType, jti string) string {
	t.Helper()
	event := fmt.Sprintf(`{"event_time":%d,"sub":"apple-subject","type":%q}`, fixture.now.Unix(), eventType)
	return fixture.apple.NotificationBody(t, fmt.Sprintf(`{"iss":%q,"aud":%q,"iat":%d,"jti":%q,"events":%s}`,
		identity.AppleIssuerForTest, identity.AppleAudienceForTest, fixture.now.Unix(), jti, event))
}

func (fixture *notificationFixture) accountStatus(t *testing.T) (status string, activeSessions int) {
	t.Helper()
	for _, candidate := range []string{"active", "identity_unavailable"} {
		if count(t, fixture.db, `SELECT count(*) FROM accounts WHERE id=$1 AND status::text=$2`, fixture.accountID, candidate) == 1 {
			status = candidate
		}
	}
	return status, count(t, fixture.db, `SELECT count(*) FROM sessions WHERE account_id=$1 AND status='active'`, fixture.accountID)
}

func (fixture *notificationFixture) identityUnavailableEffects(t *testing.T) int {
	t.Helper()
	return count(t, fixture.db, `SELECT count(*) FROM security_audit_events WHERE scope='account' AND scope_id=$1 AND kind='identity_unavailable'`, fixture.accountID)
}

func notificationKeyring(fill byte, id string) identity.DigestKeyring {
	key := identity.DigestKey{ID: id}
	for index := range key.Secret {
		key.Secret[index] = fill
	}
	return identity.DigestKeyring{Current: key}
}

func TestNotificationRepositoryReservationIsDuplicateSafe(t *testing.T) {
	db := authoritytest.New(t)
	store := db.Service.IdentityAuthority()
	keys := notificationKeyring(0x44, "notification.current")
	firstCommand, firstDigest := identity.NotificationCommandID(keys.Current, identity.AppleIssuerForTest, identity.AppleAudienceForTest, "database-jti")
	stored, created, err := store.ReserveAppleNotification(t.Context(), []identity.KeyedDigest{firstDigest}, firstCommand, time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	if err != nil || !created || stored != firstCommand {
		t.Fatalf("first stored=%q created=%v err=%v", stored, created, err)
	}
	stored, created, err = store.ReserveAppleNotification(t.Context(), []identity.KeyedDigest{firstDigest}, "30000000-0000-4000-8000-000000000099",
		time.Date(2026, 9, 20, 12, 0, 1, 0, time.UTC))
	if err != nil || created || stored != firstCommand {
		t.Fatalf("replay stored=%q created=%v err=%v", stored, created, err)
	}
}

func TestAppleNotificationDestructiveEventLooksUpEveryIdentityKeyAndCommits(t *testing.T) {
	fixture := newNotificationFixture(t)
	previous := identity.DigestKey{ID: "identity.previous"}
	for index := range previous.Secret {
		previous.Secret[index] = 0x55
	}
	// The identity is bound under a retired key that is still configured.
	fixture.db.Exec(t, `DELETE FROM account_identities WHERE account_id=$1`, fixture.accountID)
	digest := identity.IdentityDigestForTest(previous, identity.AppleAudienceForTest, "apple-subject")
	fixture.db.Exec(t, `INSERT INTO account_identities(provider,identity_key_id,identity_digest,account_id,lifecycle_generation,audience,subject,live,created_at)
		VALUES ('apple',$1,$2,$3,1,$4,'apple-subject',true,$5)`, digest.KeyID, digest.Sum[:], fixture.accountID, identity.AppleAudienceForTest, fixture.db.Now)
	fixture.identityKeys.Previous = []identity.DigestKey{previous}
	spy := fixture.spy()
	if err := fixture.service(t, spy).Handle(t.Context(), fixture.body(t, "consent-revoked", "notification-jti-1")); err != nil {
		t.Fatal(err)
	}
	if len(spy.resolveCalls) != 1 || len(spy.resolveCalls[0]) != 2 || !strings.Contains(spy.resolveCalls[0][0], ":"+fixture.identityKeys.Current.ID+":") ||
		!strings.Contains(spy.resolveCalls[0][1], ":identity.previous:") {
		t.Fatalf("identity resolutions=%v", spy.resolveCalls)
	}
	if len(spy.reserveCalls) != 1 || len(spy.reserveCalls[0]) != 1 {
		t.Fatalf("reservation calls=%+v", spy.reserveCalls)
	}
	if status, sessions := fixture.accountStatus(t); status != "identity_unavailable" || sessions != 0 {
		t.Fatalf("account status=%s active sessions=%d", status, sessions)
	}
}

func TestAppleNotificationReceiptPrecedesReservationAfterRestoreAndKeyRotation(t *testing.T) {
	fixture := newNotificationFixture(t)
	body := fixture.body(t, "account-deleted", "notification-jti-1")
	oldKeys := fixture.notificationKeys
	if err := fixture.service(t, fixture.spy()).Handle(t.Context(), body); err != nil {
		t.Fatal(err)
	}
	rotated := notificationKeyring(0x66, "notification.rotated").Current
	fixture.notificationKeys = identity.DigestKeyring{Current: rotated, Previous: []identity.DigestKey{oldKeys.Current}}
	restarted := fixture.spy()
	if err := fixture.service(t, restarted).Handle(t.Context(), body); err != nil {
		t.Fatal(err)
	}
	if restarted.reservations() != 0 {
		t.Fatalf("authority receipt was checked after reservation: %+v", restarted.reserveCalls)
	}
	if fixture.identityUnavailableEffects(t) != 1 {
		t.Fatal("redelivery repeated the effect")
	}
}

func TestAppleNotificationExistingSQLRowWithoutReceiptExecutesAuthorityCommand(t *testing.T) {
	fixture := newNotificationFixture(t)
	commandID, digest := identity.NotificationCommandID(fixture.notificationKeys.Current, identity.AppleIssuerForTest, identity.AppleAudienceForTest, "notification-jti-1")
	if _, _, err := fixture.db.Service.ReserveAppleNotification(t.Context(), []identity.KeyedDigest{digest}, commandID, fixture.now); err != nil {
		t.Fatal(err)
	}
	spy := fixture.spy()
	if err := fixture.service(t, spy).Handle(t.Context(), fixture.body(t, "consent-revoked", "notification-jti-1")); err != nil {
		t.Fatal(err)
	}
	if spy.reservations() != 1 {
		t.Fatalf("reservation calls=%d", spy.reservations())
	}
	if status, _ := fixture.accountStatus(t); status != "identity_unavailable" {
		t.Fatalf("reservation row was treated as completion: %s", status)
	}
}

func TestAppleNotificationRetainedKeySQLRowWithoutReceiptExecutesStoredCommand(t *testing.T) {
	fixture := newNotificationFixture(t)
	previous := notificationKeyring(0x77, "notification.previous").Current
	fixture.notificationKeys.Previous = []identity.DigestKey{previous}
	storedCommandID, oldDigest := identity.NotificationCommandID(previous, identity.AppleIssuerForTest, identity.AppleAudienceForTest, "notification-jti-1")
	if _, _, err := fixture.db.Service.ReserveAppleNotification(t.Context(), []identity.KeyedDigest{oldDigest}, storedCommandID, fixture.now); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service(t, fixture.spy()).Handle(t.Context(), fixture.body(t, "consent-revoked", "notification-jti-1")); err != nil {
		t.Fatal(err)
	}
	if n := count(t, fixture.db, `SELECT count(*) FROM command_receipts WHERE actor_account_id=$1 AND command_id=$2 AND kind='identity_unavailable'`,
		fixture.accountID, storedCommandID); n != 1 {
		t.Fatalf("stored command receipts=%d", n)
	}
}

func TestAppleNotificationChangedEventWithSameJTIConflictsBeforeReservation(t *testing.T) {
	fixture := newNotificationFixture(t)
	spy := fixture.spy()
	service := fixture.service(t, spy)
	if err := service.Handle(t.Context(), fixture.body(t, "consent-revoked", "notification-jti-1")); err != nil {
		t.Fatal(err)
	}
	reservations := spy.reservations()
	if err := service.Handle(t.Context(), fixture.body(t, "account-deleted", "notification-jti-1")); authority.CodeOf(err) != "command_id_reused" {
		t.Fatalf("code=%q err=%v", authority.CodeOf(err), err)
	}
	if spy.reservations() != reservations {
		t.Fatal("changed same-JTI event reached reservation before receipt conflict")
	}
}

func TestAppleNotificationFreshJTIAfterIdentityUnavailableIsIdempotentWithoutReservation(t *testing.T) {
	fixture := newNotificationFixture(t)
	spy := fixture.spy()
	service := fixture.service(t, spy)
	if err := service.Handle(t.Context(), fixture.body(t, "consent-revoked", "notification-jti-first")); err != nil {
		t.Fatal(err)
	}
	before := count(t, fixture.db, `SELECT count(*) FROM security_audit_events`)
	reservations := spy.reservations()
	if err := service.Handle(t.Context(), fixture.body(t, "account-deleted", "notification-jti-fresh")); err != nil {
		t.Fatalf("fresh JTI after identity loss returned retryable error: %v", err)
	}
	if after := count(t, fixture.db, `SELECT count(*) FROM security_audit_events`); after != before {
		t.Fatalf("authority changed for already-achieved state: before=%d after=%d", before, after)
	}
	if spy.reservations() != reservations {
		t.Fatalf("fresh JTI created a reservation after identity loss: before=%d after=%d", reservations, spy.reservations())
	}
	if got := fixture.identityUnavailableEffects(t); got != 1 {
		t.Fatalf("identity_unavailable effects=%d", got)
	}
}

func TestAppleNotificationConcurrentDistinctJTIsConvergeToOneEffect(t *testing.T) {
	fixture := newNotificationFixture(t)
	spy := fixture.spy()
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	spy.reserveHook = func() {
		arrived <- struct{}{}
		<-release
	}
	service := fixture.service(t, spy)
	bodies := []string{fixture.body(t, "consent-revoked", "notification-jti-concurrent-a"), fixture.body(t, "account-deleted", "notification-jti-concurrent-b")}
	errorsByRequest := make(chan error, len(bodies))
	for _, body := range bodies {
		go func() { errorsByRequest <- service.Handle(t.Context(), body) }()
	}
	for range bodies {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("both notifications did not reach reservation barrier")
		}
	}
	close(release)
	for range bodies {
		if err := <-errorsByRequest; err != nil {
			t.Fatalf("concurrent notification did not converge: %v", err)
		}
	}
	if got := fixture.identityUnavailableEffects(t); got != 1 {
		t.Fatalf("identity_unavailable effects=%d", got)
	}
	if spy.reservations() != 2 {
		t.Fatalf("reservation attempts=%d, want one per delivery", spy.reservations())
	}
}

func TestAppleNotificationFailsClosedOnMultipleIdentityOrReservationMatches(t *testing.T) {
	t.Run("identity matches", func(t *testing.T) {
		fixture := newNotificationFixture(t)
		previous := identity.DigestKey{ID: "identity.previous"}
		for index := range previous.Secret {
			previous.Secret[index] = 0x55
		}
		fixture.identityKeys.Previous = []identity.DigestKey{previous}
		seedIdentity(t, fixture.db, previous, "apple-subject")
		spy := fixture.spy()
		// The authority quarantines the identity; the notification surface
		// (identity and auth codes alike) reports service_unavailable, as
		// legacy did.
		err := fixture.service(t, spy).Handle(t.Context(), fixture.body(t, "consent-revoked", "notification-jti-1"))
		if err == nil || identity.Code(err) != "service_unavailable" || auth.Code(err) != "service_unavailable" ||
			authority.CodeOf(err) != "authorization_quarantine" {
			t.Fatalf("identity code=%q auth code=%q cause=%q err=%v", identity.Code(err), auth.Code(err), authority.CodeOf(err), err)
		}
		if len(spy.resolveCalls) != 1 || spy.reservations() != 0 {
			t.Fatalf("resolutions=%d reservations=%d", len(spy.resolveCalls), spy.reservations())
		}
		if status, _ := fixture.accountStatus(t); status != "active" {
			t.Fatalf("ambiguous identity changed account %s", status)
		}
	})

	t.Run("reservation matches", func(t *testing.T) {
		fixture := newNotificationFixture(t)
		previous := notificationKeyring(0x77, "notification.previous").Current
		fixture.notificationKeys.Previous = []identity.DigestKey{previous}
		_, current := identity.NotificationCommandID(fixture.notificationKeys.Current, identity.AppleIssuerForTest, identity.AppleAudienceForTest, "notification-jti-1")
		_, old := identity.NotificationCommandID(previous, identity.AppleIssuerForTest, identity.AppleAudienceForTest, "notification-jti-1")
		for digest, commandID := range map[identity.KeyedDigest]string{current: "30000000-0000-4000-8000-000000000090", old: "30000000-0000-4000-8000-000000000091"} {
			if _, _, err := fixture.db.Service.ReserveAppleNotification(t.Context(), []identity.KeyedDigest{digest}, commandID, fixture.now); err != nil {
				t.Fatal(err)
			}
		}
		if err := fixture.service(t, fixture.spy()).Handle(t.Context(), fixture.body(t, "consent-revoked", "notification-jti-1")); identity.Code(err) != "service_unavailable" || err == nil {
			t.Fatalf("code=%q err=%v", identity.Code(err), err)
		}
		if status, _ := fixture.accountStatus(t); status != "active" {
			t.Fatalf("ambiguous reservation changed account %s", status)
		}
	})
}
