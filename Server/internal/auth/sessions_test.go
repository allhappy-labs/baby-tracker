package auth

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/account"
	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
	"babytracker/server/internal/testpg"
)

const (
	sessionOrigin   = "https://sync.example.test"
	sessionInstance = "50000000-0000-4000-8000-000000000091"
)

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

type sessionFixture struct {
	t                *testing.T
	db               *testpg.Fixture
	store            localstore.Store
	root             string
	Clock            *sessionClock
	Service          *Service
	Identity         *identity.Service
	mode             config.Mode
	origin, instance string
	ids              int
	epoch            uint64
	projection       *sessionProjection
	heads            *sessionHeads

	identityKeys, tokenKeys, deviceKeys, notificationKeys identity.DigestKeyring
	actorKey                                              [32]byte
}

func newSessionFixture(t *testing.T, mode config.Mode) *sessionFixture {
	t.Helper()
	database := testpg.New(t)
	root := t.TempDir()
	store, err := localstore.Open(filepath.Join(root, "ledger"), filepath.Join(root, "head"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	h := &sessionFixture{
		t: t, db: database, store: store, root: root, mode: mode, origin: sessionOrigin, instance: sessionInstance,
		Clock:        &sessionClock{now: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)},
		epoch:        1,
		identityKeys: testKeyring("identity.v1", 0x11), tokenKeys: testKeyring("session.v1", 0x22),
		deviceKeys: testKeyring("device.v1", 0x33), notificationKeys: testKeyring("notify.v1", 0x44),
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
	store, err := localstore.Open(filepath.Join(h.root, "ledger"), filepath.Join(h.root, "head"))
	if err != nil {
		h.t.Fatal(err)
	}
	h.store = store
	identityRepository, err := identity.NewRepository(h.db.Runtime)
	if err != nil {
		h.t.Fatal(err)
	}
	verifier := identity.IdentityVerifierFunc(func(_ context.Context, token, _ string) (identity.VerifiedIdentity, error) {
		return identity.VerifiedIdentity{Provider: "apple", Audience: "com.example.babytracker", Subject: token,
			IssuedAt: h.Clock.Now().Add(-time.Minute), ExpiresAt: h.Clock.Now().Add(time.Hour)}, nil
	})
	h.Identity, err = identity.New(identity.Options{Verifier: verifier, Repository: identityRepository, Origin: origin,
		InstanceID: instance, IdentityKeys: h.identityKeys, Now: h.Clock.Now})
	if err != nil {
		h.t.Fatal(err)
	}
	h.projection = &sessionProjection{delegate: security.NewProjector(h.db.Projector)}
	h.heads = &sessionHeads{delegate: store.Heads}
	authority, err := security.New(security.Options{
		Ledger: store.Ledger, Heads: h.heads, Projection: h.projection, Fence: store.Fence,
		Now: h.Clock.Now, Runtime: h.db.Runtime, Epoch: func(context.Context) (uint64, error) { return h.epoch, nil },
		Decide: func(context.Context, access.Principal, string, string) (access.Grant, error) {
			return access.Grant{}, nil
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	repository, err := NewRepository(h.db.Runtime)
	if err != nil {
		h.t.Fatal(err)
	}
	h.Service, err = New(Options{Identity: h.Identity, Verifier: verifier, Security: authority, Repository: repository,
		Mode: h.mode, Origin: origin, InstanceID: instance, TokenKeys: h.tokenKeys, DeviceKeys: h.deviceKeys,
		NotificationKeys: h.notificationKeys, ActorIDKey: h.actorKey, Now: h.Clock.Now})
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *sessionFixture) SetEpoch(epoch uint64) { h.epoch = epoch }

func (h *sessionFixture) FailProjectionOnce() { h.projection.failOnce = true }

func (h *sessionFixture) FailHeadCASOnce(run func() error) {
	h.heads.mu.Lock()
	h.heads.rejectOnce = run
	h.heads.mu.Unlock()
}

func (h *sessionFixture) newCompetingService() *Service {
	h.t.Helper()
	authority, err := security.New(security.Options{Ledger: h.store.Ledger, Heads: h.store.Heads,
		Projection: security.NewProjector(h.db.Projector), Fence: sessionIndependentFence{}, Now: h.Clock.Now,
		Runtime: h.db.Runtime, Epoch: func(context.Context) (uint64, error) { return h.epoch, nil },
		Decide: func(context.Context, access.Principal, string, string) (access.Grant, error) {
			return access.Grant{}, nil
		}})
	if err != nil {
		h.t.Fatal(err)
	}
	repository, err := NewRepository(h.db.Runtime)
	if err != nil {
		h.t.Fatal(err)
	}
	service, err := New(Options{Identity: h.Identity, Verifier: h.Service.verifier, Security: authority, Repository: repository,
		Mode: h.mode, Origin: h.origin, InstanceID: h.instance, TokenKeys: h.tokenKeys, DeviceKeys: h.deviceKeys,
		NotificationKeys: h.notificationKeys, ActorIDKey: h.actorKey, Now: h.Clock.Now})
	if err != nil {
		h.t.Fatal(err)
	}
	return service
}

func (h *sessionFixture) RestartAndReconcile() {
	h.t.Helper()
	h.Restart()
	if err := h.Service.security.Reconcile(h.t.Context()); err != nil {
		h.t.Fatal(err)
	}
}

type sessionProjection struct {
	delegate security.Projection
	failOnce bool
}

type sessionHeads struct {
	delegate   security.LedgerHeadStore
	mu         sync.Mutex
	rejectOnce func() error
}

func (heads *sessionHeads) Load(ctx context.Context) (security.Head, error) {
	return heads.delegate.Load(ctx)
}

func (heads *sessionHeads) CompareAndSwap(ctx context.Context, prior, next security.Head) (bool, error) {
	heads.mu.Lock()
	run := heads.rejectOnce
	heads.rejectOnce = nil
	heads.mu.Unlock()
	if run != nil {
		if err := run(); err != nil {
			return false, err
		}
		return false, nil
	}
	return heads.delegate.CompareAndSwap(ctx, prior, next)
}

type sessionIndependentFence struct{}

func (sessionIndependentFence) Read(context.Context) (func(), error)  { return func() {}, nil }
func (sessionIndependentFence) Write(context.Context) (func(), error) { return func() {}, nil }

func (projection *sessionProjection) Head(ctx context.Context) (security.Head, error) {
	return projection.delegate.Head(ctx)
}

func (projection *sessionProjection) Apply(ctx context.Context, candidate security.Candidate) error {
	if projection.failOnce {
		projection.failOnce = false
		return context.DeadlineExceeded
	}
	return projection.delegate.Apply(ctx, candidate)
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
	return h.Service.Exchange(h.t.Context(), identity.ExchangeRequest{CommandID: h.nextID(3), Challenge: challenge.Challenge,
		IdentityToken: subject, InstallationID: "installation-" + subject, Profile: profile, DeviceContinuity: claim})
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
	lookup, found, err := h.Service.repository.LookupAccessSession(t.Context(), h.tokenKeys.Current.ID,
		tokenDigest(h.tokenKeys.Current, "access", original.AccessToken).Sum)
	if err != nil || !found || !lookup.AbsoluteExpiresAt.Equal(original.ReauthenticateAt) {
		t.Fatalf("old session lookup=%+v found=%t err=%v", lookup, found, err)
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
}

func TestDeletedIdentityCreatesNewGenerationActorWithoutOldContinuity(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	principal, err := h.Service.Authenticate(t.Context(), original.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	accounts := &account.Service{Security: h.Service.security}
	deleteID := h.nextID(8)
	first, err := accounts.Delete(t.Context(), principal, deleteID, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := accounts.Delete(t.Context(), principal, deleteID, true)
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
}

func TestContinuityCredentialWithoutFreshAppleProofDenied(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	original := h.Issue()
	before := h.authMutationSnapshot()
	_, _, err := h.Service.ResolveDevice(t.Context(), identity.VerifiedIdentity{Provider: "apple", Audience: "com.example.babytracker", Subject: "subject-one"},
		&identity.DeviceContinuityClaim{DeviceID: original.DeviceID, Credential: original.DeviceCredential})
	if Code(err) != "device_continuity_invalid" {
		t.Fatalf("code=%s err=%v", Code(err), err)
	}
	if after := h.authMutationSnapshot(); after != before {
		t.Fatalf("continuity-only proof mutated auth state: before=%+v after=%+v", before, after)
	}
}

func TestReadConsumersUseDetachedViews(t *testing.T) {
	h := newSessionFixture(t, config.ModeSelfHosted)
	bundle := h.Issue()
	principal, err := h.Service.Authenticate(t.Context(), bundle.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	var retained security.AuthorityView
	if err := h.Service.security.WithAuthorityView(t.Context(), func(view security.AuthorityView) error {
		retained = view
		scope, family, slot := authorityFamilySlot(view, principal.AccountID)
		if scope != "account" || family != nil || slot != nil {
			t.Fatal("account scope changed")
		}
		session, ok := view.Session(principal.SessionID)
		if !ok {
			t.Fatal("session missing")
		}
		session.Status = "revoked"
		keys := view.ReferencedCredentialKeyIDs()
		if len(keys.Identity) > 0 {
			keys.Identity[0] = "changed"
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := retained.Session(principal.SessionID); ok {
		t.Fatal("expired view retained authority")
	}
	again, err := h.Service.Authenticate(t.Context(), bundle.AccessToken)
	if err != nil || again.AccountID != principal.AccountID || again.Scope != principal.Scope {
		t.Fatalf("detached selection changed authentication: %v", err)
	}
}
