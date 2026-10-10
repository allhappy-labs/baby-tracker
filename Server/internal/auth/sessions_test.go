package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

const sessionOrigin = "https://sync.example.test"

type sessionClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *sessionClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *sessionClock) Advance(delta time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(delta)
}

// sessionFixture is an auth Service over a real authority on a disposable
// database. Restart rebuilds the identity, authority and auth services over
// the same database (nothing is cached across a restart: the database is the
// only state).
type sessionFixture struct {
	t                *testing.T
	db               *authoritytest.Fixture
	Clock            *sessionClock
	Service          *Service
	Authority        *authority.Service
	Identity         *identity.Service
	apple            *fakeAppleExchanger
	mode             config.Mode
	origin, instance string
	ids              int
	epoch            uint64
	// requireEntitlement makes the authority require the family_sync
	// entitlement (hosted nodes).
	requireEntitlement bool

	identityKeys, tokenKeys, deviceKeys identity.DigestKeyring
	actorKey                            [32]byte
}

func newSessionFixture(t *testing.T, mode config.Mode) *sessionFixture {
	t.Helper()
	db := authoritytest.New(t)
	h := &sessionFixture{
		t: t, db: db, mode: mode, origin: sessionOrigin, instance: db.InstanceID,
		Clock:        &sessionClock{now: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)},
		epoch:        authoritytest.RecoveryEpoch,
		apple:        &fakeAppleExchanger{pool: db.Pool},
		identityKeys: testKeyring("identity.v1", 0x11), tokenKeys: testKeyring("session.v1", 0x22),
		deviceKeys: testKeyring("device.v1", 0x33),
	}
	for index := range h.actorKey {
		h.actorKey[index] = 0x55
	}
	h.restartWith(h.origin, h.instance)
	return h
}

func (h *sessionFixture) Restart() { h.t.Helper(); h.restartWith(h.origin, h.instance) }

func (h *sessionFixture) restartWith(origin, instance string) {
	h.t.Helper()
	var err error
	h.Authority, err = authority.New(authority.Options{Pool: h.db.Pool, Now: h.Clock.Now, InstanceID: instance, RecoveryEpoch: h.epoch,
		TokenKeys: h.tokenKeys, Allowed: authoritytest.Allowed, EntitlementExempt: authoritytest.EntitlementExempt,
		RequireFamilySyncEntitlement: h.requireEntitlement})
	if err != nil {
		h.t.Fatal(err)
	}
	verifier := identity.IdentityVerifierFunc(func(_ context.Context, token, _ string) (identity.VerifiedIdentity, error) {
		if strings.HasPrefix(token, "invalid") {
			return identity.VerifiedIdentity{}, errors.New("invalid test proof")
		}
		return identity.VerifiedIdentity{Provider: "apple", Audience: "com.example.babytracker", Subject: token,
			IssuedAt: h.Clock.Now().Add(-time.Minute), ExpiresAt: h.Clock.Now().Add(time.Hour)}, nil
	})
	h.Identity, err = identity.New(identity.Options{Verifier: verifier, Authority: h.Authority.IdentityAuthority(), Origin: origin,
		InstanceID: instance, IdentityKeys: h.identityKeys, Now: h.Clock.Now})
	if err != nil {
		h.t.Fatal(err)
	}
	options := Options{Identity: h.Identity, Verifier: verifier, Authority: h.Authority, Mode: h.mode, Origin: origin, InstanceID: instance,
		TokenKeys: h.tokenKeys, DeviceKeys: h.deviceKeys, ActorIDKey: h.actorKey, Now: h.Clock.Now}
	if h.mode == config.ModeHosted {
		options.Apple = h.apple
	}
	h.Service, err = New(options)
	if err != nil {
		h.t.Fatal(err)
	}
}

// SetEpoch restarts the node at another recovery epoch.
func (h *sessionFixture) SetEpoch(epoch uint64) { h.t.Helper(); h.epoch = epoch; h.Restart() }

// RotateTokenKeys makes next the current token key, keeping the old one.
func (h *sessionFixture) RotateTokenKeys(next identity.DigestKey) identity.DigestKey {
	h.t.Helper()
	old := h.tokenKeys.Current
	h.tokenKeys = identity.DigestKeyring{Current: next, Previous: []identity.DigestKey{old}}
	h.Restart()
	return old
}

func (h *sessionFixture) nextID(kind byte) string {
	h.ids++
	return fmt.Sprintf("%08x-0000-4%03x-8%03x-%012x", h.ids, kind, kind, h.ids)
}

func (h *sessionFixture) exchange(subject string, profile *identity.ProfileInput, claim *identity.DeviceContinuityClaim) (SessionBundle, error) {
	h.t.Helper()
	challenge, err := h.Identity.Challenge(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	request := identity.ExchangeRequest{CommandID: h.nextID(3), Challenge: challenge.Challenge,
		IdentityToken: subject, InstallationID: "installation-" + subject, Profile: profile, DeviceContinuity: claim}
	if h.mode == config.ModeHosted {
		request.AuthorizationCode = "code:" + subject
	}
	return h.Service.Exchange(h.t.Context(), request)
}

func (h *sessionFixture) Issue() SessionBundle {
	h.t.Helper()
	bundle, err := h.exchange("subject-one", &identity.ProfileInput{DisplayName: "Parent One", Confirmed: true}, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return bundle
}

func (h *sessionFixture) Reauthenticate(original SessionBundle) (SessionBundle, error) {
	h.t.Helper()
	return h.exchange("subject-one", nil, &identity.DeviceContinuityClaim{DeviceID: original.DeviceID, Credential: original.DeviceCredential})
}

func (h *sessionFixture) principal(bundle SessionBundle) access.Principal {
	h.t.Helper()
	principal, err := h.Service.Authenticate(h.t.Context(), bundle.AccessToken)
	if err != nil {
		h.t.Fatal(err)
	}
	return principal
}

// query reads as the table owner inside an authority transaction (the
// authority tables admit only authority transactions).
func (h *sessionFixture) query(statement string, args []any, dest ...any) {
	h.t.Helper()
	ctx := context.Background()
	tx, err := h.db.DB.Owner.Begin(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
		h.t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, statement, args...).Scan(dest...); err != nil {
		h.t.Fatalf("query %q: %v", statement, err)
	}
}

// exec writes as the table owner inside an authority transaction and returns
// the affected row count.
func (h *sessionFixture) exec(statement string, args ...any) int64 {
	h.t.Helper()
	return h.db.Exec(h.t, statement, args...)
}

// sessionByAccessToken reads the session a raw access token was issued as.
func (h *sessionFixture) sessionByAccessToken(raw string) (id, status, tokenKeyID string, deadline time.Time) {
	h.t.Helper()
	for _, key := range append([]identity.DigestKey{h.tokenKeys.Current}, h.tokenKeys.Previous...) {
		var found bool
		digest := tokenDigest(key, "access", raw).Sum
		h.query(`SELECT EXISTS(SELECT 1 FROM sessions WHERE access_digest=$1 AND token_key_id=$2)`, []any{digest[:], key.ID}, &found)
		if found {
			h.query(`SELECT id::text, status::text, token_key_id, deadline_at FROM sessions WHERE access_digest=$1 AND token_key_id=$2`,
				[]any{digest[:], key.ID}, &id, &status, &tokenKeyID, &deadline)
			return id, status, tokenKeyID, deadline.UTC()
		}
	}
	h.t.Fatal("no session for access token")
	return "", "", "", time.Time{}
}

// sessionByRefreshToken reads the ID of the session a raw refresh token
// was issued as.
func (h *sessionFixture) sessionByRefreshToken(raw string) string {
	h.t.Helper()
	var id string
	digests := [][]byte{}
	for _, key := range append([]identity.DigestKey{h.tokenKeys.Current}, h.tokenKeys.Previous...) {
		digest := tokenDigest(key, "refresh", raw).Sum
		digests = append(digests, digest[:])
	}
	h.query(`SELECT id::text FROM sessions WHERE refresh_digest = ANY($1::bytea[])`, []any{digests}, &id)
	return id
}

// authMutationState is everything an authority command changes, recovery
// receipts included (an acknowledgement retires one without a command).
type authMutationState struct {
	Audit, Sessions, ActiveSessions, Devices, ActiveDevices, Receipts, Accounts, ActiveAccounts, RefreshReceipts int
}

func (h *sessionFixture) authMutationSnapshot() authMutationState {
	h.t.Helper()
	var state authMutationState
	h.query(`SELECT (SELECT count(*) FROM security_audit_events), (SELECT count(*) FROM sessions), (SELECT count(*) FROM sessions WHERE status='active'),
		(SELECT count(*) FROM devices), (SELECT count(*) FROM devices WHERE status='active'), (SELECT count(*) FROM command_receipts),
		(SELECT count(*) FROM accounts), (SELECT count(*) FROM accounts WHERE status='active'), (SELECT count(*) FROM refresh_receipts)`, nil,
		&state.Audit, &state.Sessions, &state.ActiveSessions, &state.Devices, &state.ActiveDevices, &state.Receipts, &state.Accounts, &state.ActiveAccounts,
		&state.RefreshReceipts)
	return state
}

// fakeAppleExchanger exchanges "code:<subject>" for an identity token the
// fixture verifier accepts as <subject>. A returned token can be swapped. It
// records how many runtime connections were checked out while it ran: the
// Apple network call must not hold a transaction open.
type fakeAppleExchanger struct {
	mu                 sync.Mutex
	requests           []identity.AppleCodeExchangeRequest
	fail               error
	swap               string
	pool               interface{ Stat() *pgxpool.Stat }
	acquiredInExchange []int32
}

func (apple *fakeAppleExchanger) ExchangeCode(_ context.Context, request identity.AppleCodeExchangeRequest) (identity.AppleTokens, error) {
	apple.mu.Lock()
	defer apple.mu.Unlock()
	apple.requests = append(apple.requests, request)
	if apple.pool != nil {
		apple.acquiredInExchange = append(apple.acquiredInExchange, apple.pool.Stat().AcquiredConns())
	}
	if apple.fail != nil {
		return identity.AppleTokens{}, apple.fail
	}
	subject, ok := strings.CutPrefix(request.AuthorizationCode, "code:")
	if !ok {
		return identity.AppleTokens{}, identity.ErrInvalidAppleAuthorization
	}
	if apple.swap != "" {
		subject = apple.swap
	}
	return identity.AppleTokens{IdentityToken: subject, RefreshToken: "apple-refresh-" + subject}, nil
}

func (apple *fakeAppleExchanger) calls() []identity.AppleCodeExchangeRequest {
	apple.mu.Lock()
	defer apple.mu.Unlock()
	return append([]identity.AppleCodeExchangeRequest(nil), apple.requests...)
}

func testKeyring(id string, fill byte) identity.DigestKeyring {
	key := identity.DigestKey{ID: id}
	for index := range key.Secret {
		key.Secret[index] = fill
	}
	return identity.DigestKeyring{Current: key}
}

func TestFreshAppleProofPreservesDevice(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	h.Clock.Advance(24 * time.Hour)
	h.Restart()
	next, err := h.Reauthenticate(original)
	if err != nil || next.DeviceID != original.DeviceID {
		t.Fatalf("device continuity lost: next=%+v err=%v", next, err)
	}
	if next.DeviceCredential != "" {
		t.Fatal("reauth unexpectedly rotated one-time continuity credential")
	}
	if next.AccessToken == original.AccessToken || next.RefreshToken == original.RefreshToken {
		t.Fatal("reauth revived an old session token")
	}
	if _, status, _, deadline := h.sessionByAccessToken(original.AccessToken); status != "active" || !deadline.Equal(original.ReauthenticateAt) {
		t.Fatalf("old session status=%s deadline=%s want %s", status, deadline, original.ReauthenticateAt)
	}
}

func TestAuthenticatePrincipalCarriesAccessDeadline(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	bundle := h.Issue()
	principal, err := h.Service.Authenticate(t.Context(), bundle.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if !principal.AccessExpiresAt.Equal(bundle.ExpiresAt) || !principal.AccessExpiresAt.After(h.Clock.Now()) {
		t.Fatalf("principal deadline=%s want=%s", principal.AccessExpiresAt, bundle.ExpiresAt)
	}
	if principal.RecoveryEpoch != bundle.RecoveryEpoch || principal.Scope != "account" || principal.DeviceID != bundle.DeviceID {
		t.Fatalf("principal=%+v bundle=%+v", principal, bundle)
	}
}

func TestDeletedIdentityCreatesNewGenerationActorWithoutOldContinuity(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	principal := h.principal(original)
	candidates, err := h.Identity.AuthorityKeyCandidates(identity.VerifiedIdentity{Provider: "apple", Audience: "com.example.babytracker", Subject: "subject-one"})
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := h.Authority.ResolveIdentity(t.Context(), candidates)
	if err != nil || resolution.AccountID != original.Account.ID {
		t.Fatalf("resolution=%+v err=%v", resolution, err)
	}
	deletion := authority.DeleteAccount{CommandID: h.nextID(8), AccountID: principal.AccountID, IdentityKey: resolution.IdentityKey, Principal: principal}
	first, err := h.Authority.DeleteAccount(t.Context(), deletion)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.Authority.DeleteAccount(t.Context(), deletion)
	if err != nil || second != first {
		t.Fatalf("delete replay=%+v want=%+v err=%v", second, first, err)
	}
	if _, err := h.Service.Authenticate(t.Context(), original.AccessToken); Code(err) != "session_revoked" {
		t.Fatalf("deleted session code=%q err=%v", Code(err), err)
	}
	next, err := h.exchange("subject-one", &identity.ProfileInput{DisplayName: "Parent Recreated", Confirmed: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.Account.ID == original.Account.ID {
		t.Fatalf("deleted actor continuity was reused: %s", next.Account.ID)
	}
	if next.DeviceID == original.DeviceID {
		t.Fatalf("deleted device continuity was reused: %s", next.DeviceID)
	}
	if _, err := h.Reauthenticate(original); Code(err) != "device_continuity_invalid" {
		t.Fatalf("old device continuity for the new actor: code=%q err=%v", Code(err), err)
	}
}

// A continuity credential only rebinds a device to a fresh Apple proof: with
// a failed proof it mints nothing (it never reaches the authority).
func TestContinuityCredentialWithoutFreshAppleProofDenied(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	before := h.authMutationSnapshot()
	_, err := h.exchange("invalid-proof", nil, &identity.DeviceContinuityClaim{DeviceID: original.DeviceID, Credential: original.DeviceCredential})
	if Code(err) != "invalid_apple_proof" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	if after := h.authMutationSnapshot(); after != before {
		t.Fatalf("continuity-only proof mutated auth state: before=%+v after=%+v", before, after)
	}
}

func TestSignInRecordsProfileAndResolvesExistingAccount(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	if original.Account.DisplayName != "Parent One" || original.Account.ProfileRequired || original.FamilySlot != nil ||
		original.RecoveryEpoch != authoritytest.RecoveryEpoch || original.DeviceCredential == "" {
		t.Fatalf("bundle=%+v", original)
	}
	again, err := h.exchange("subject-one", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Account != original.Account || again.DeviceID == original.DeviceID {
		t.Fatalf("second sign-in account=%+v device=%s", again.Account, again.DeviceID)
	}
	if _, err := h.exchange("subject-new", nil, nil); Code(err) != "display_name_required" {
		t.Fatalf("new identity without profile code=%q err=%v", Code(err), err)
	}
}

func newHostedSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	return newSessionFixture(t, config.ModeHosted)
}

// Hosted sign-in exchanges the authorization code and verifies the returned
// identity before the sign-in transaction; the Apple refresh token is not
// persisted.
func TestHostedExchangeVerifiesAppleCodeBeforeSignIn(t *testing.T) {
	h := newHostedSessionFixture(t)
	bundle, err := h.exchange("subject-one", &identity.ProfileInput{DisplayName: "Parent", Confirmed: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := h.apple.calls()
	if len(calls) != 1 || calls[0].AuthorizationCode != "code:subject-one" || calls[0].ExpectedIdentity.Subject != "subject-one" || calls[0].ExpectedNonce == "" {
		t.Fatalf("apple calls=%+v", calls)
	}
	if held := h.apple.acquiredInExchange; len(held) != 1 || held[0] != 0 {
		t.Fatalf("runtime connections held during the Apple code exchange: %v", held)
	}
	if _, err := h.Service.Authenticate(t.Context(), bundle.AccessToken); err != nil {
		t.Fatal(err)
	}
	var persisted bool
	h.query(`SELECT EXISTS(SELECT 1 FROM security_audit_events WHERE payload::text LIKE '%apple-refresh-%')
		OR EXISTS(SELECT 1 FROM command_receipts WHERE result::text LIKE '%apple-refresh-%')`, nil, &persisted)
	if persisted {
		t.Fatal("Apple refresh token persisted")
	}
	if remaining := bundle.ReauthenticateAt.Sub(h.Clock.Now()); remaining != defaultHostedAge {
		t.Fatalf("hosted deadline=%s want=%s", remaining, defaultHostedAge)
	}
}

func TestHostedExchangeRefusesMissingFailedOrSwappedAppleCode(t *testing.T) {
	for _, test := range []struct {
		name, code string
		prepare    func(*sessionFixture)
		want       string
	}{
		{name: "missing code", want: "invalid_request", prepare: func(h *sessionFixture) {}},
		{name: "exchange fails", code: "code:subject-one", want: "apple_unavailable", prepare: func(h *sessionFixture) { h.apple.fail = identity.ErrAppleUnavailable }},
		{name: "swapped identity", code: "code:subject-one", want: "invalid_apple_proof", prepare: func(h *sessionFixture) { h.apple.swap = "subject-two" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHostedSessionFixture(t)
			test.prepare(h)
			before := h.authMutationSnapshot()
			challenge, err := h.Identity.Challenge(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.Service.Exchange(t.Context(), identity.ExchangeRequest{CommandID: h.nextID(3), Challenge: challenge.Challenge, IdentityToken: "subject-one",
				AuthorizationCode: test.code, InstallationID: "installation", Profile: &identity.ProfileInput{DisplayName: "Parent", Confirmed: true}})
			if Code(err) != test.want {
				t.Fatalf("code=%q want %q err=%v", Code(err), test.want, err)
			}
			if after := h.authMutationSnapshot(); after != before {
				t.Fatalf("refused hosted sign-in mutated authority: %+v -> %+v", before, after)
			}
		})
	}
}

func TestNewRequiresTheAuthorityInstanceAndHostedAppleExchange(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	options := Options{Identity: h.Identity, Verifier: h.Service.verifier, Authority: h.Authority, Mode: config.ModeSelfHosted, Origin: h.origin,
		InstanceID: h.instance, TokenKeys: h.tokenKeys, DeviceKeys: h.deviceKeys, ActorIDKey: h.actorKey, Now: h.Clock.Now}
	if _, err := New(options); err != nil {
		t.Fatal(err)
	}
	other := options
	other.InstanceID = "50000000-0000-4000-8000-000000000092"
	if _, err := New(other); err == nil {
		t.Fatal("accepted an instance ID other than the authority's")
	}
	hosted := options
	hosted.Mode = config.ModeHosted
	if _, err := New(hosted); err == nil {
		t.Fatal("hosted auth accepted without Apple code exchange")
	}
	missing := options
	missing.Authority = nil
	if _, err := New(missing); err == nil {
		t.Fatal("accepted no authority")
	}
	mismatched := options
	mismatched.TokenKeys = testKeyring(h.tokenKeys.Current.ID, 0x23)
	if _, err := New(mismatched); err == nil {
		t.Fatal("accepted a token keyring other than the authority's")
	}
	extra := options
	extra.TokenKeys = identity.DigestKeyring{Current: h.tokenKeys.Current, Previous: []identity.DigestKey{testKeyring("session.v0", 0x24).Current}}
	if _, err := New(extra); err == nil {
		t.Fatal("accepted a token keyring with a key the authority lacks")
	}
}

// An explicit identity code wins over the authority cause it wraps; an
// authority code passed through unchanged is reported as is.
func TestCodePrecedence(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{ErrSessionRevoked, "session_revoked"},
		{fmt.Errorf("%w: %w", identity.ErrUnavailable, authority.ErrAuthorizationQuarantine), "service_unavailable"},
		{fmt.Errorf("%w: %w", identity.ErrInvalidAppleProof, authority.ErrUnavailable), "invalid_apple_proof"},
		{authority.ErrAuthorizationQuarantine, "authorization_quarantine"},
		{fmt.Errorf("wrapped: %w", authority.ErrCommandIDReused), "command_id_reused"},
		{errors.New("plain"), "service_unavailable"},
	} {
		if got := Code(test.err); got != test.want {
			t.Fatalf("Code(%v)=%q want %q", test.err, got, test.want)
		}
	}
}
