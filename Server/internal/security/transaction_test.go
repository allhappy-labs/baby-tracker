package security

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/diagnostics"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
)

type recoveryFixture struct {
	Service *Service
	Runtime interface {
		BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	}
	Principal access.Principal
	FamilyID  string
	fence     *fixtureFence
	mu        sync.Mutex
	removed   bool
	authority *authorityFixture
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	database := testpg.New(t)
	authority := newAuthority(t)
	fence := &fixtureFence{}
	principal := access.Principal{
		AccountID: "10000000-0000-4000-8000-000000000001", DeviceID: "30000000-0000-4000-8000-000000000001",
		SessionID: "40000000-0000-4000-8000-000000000001", RecoveryEpoch: 1, SecurityRevision: 1, Scope: "account",
	}
	commitFixtureEvent(t, authority, "identity_session_issued", []Effect{
		{Kind: "account_registered", AccountID: principal.AccountID, IdentityKey: "hmac-sha256:identity-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "active"},
		{Kind: "device_registered", AccountID: principal.AccountID, DeviceID: principal.DeviceID, ServerInstanceID: "50000000-0000-4000-8000-000000000001", CanonicalOrigin: "https://family.example", DeviceCredentialDigest: "hmac-sha256:device-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Status: "active"},
		{Kind: "session_granted", AccountID: principal.AccountID, IdentityKey: "hmac-sha256:identity-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DeviceID: principal.DeviceID, SessionID: principal.SessionID, LineageRootSessionID: principal.SessionID, ServerInstanceID: "50000000-0000-4000-8000-000000000001", CanonicalOrigin: "https://family.example", DeviceCredentialDigest: "hmac-sha256:device-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ProjectionMaterialDigest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "active"},
	})
	h := &recoveryFixture{Runtime: database.Runtime, Principal: principal, FamilyID: "20000000-0000-4000-8000-000000000001", fence: fence, authority: authority}
	decide := func(_ context.Context, p access.Principal, familyID, _ string) (access.Grant, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.removed || p.AccountID != h.Principal.AccountID || familyID != h.FamilyID {
			return access.Grant{}, errorCode("membership_revoked")
		}
		return access.Grant{AccountID: p.AccountID, FamilyID: familyID, Role: "caregiver", Revision: authority.Head().Revision}, nil
	}
	service, err := New(Options{Ledger: authority.ledger, Heads: authority.heads, Projection: authority.Projection, Fence: fence,
		Now: authority.clock, Runtime: database.Runtime, Decide: decide, Epoch: func(context.Context) (uint64, error) { return 1, nil }})
	if err != nil {
		t.Fatal(err)
	}
	h.Service = service
	return h
}

func commitFixtureEvent(t *testing.T, authority *authorityFixture, kind EventKind, effects []Effect) {
	t.Helper()
	prior := authority.Head()
	event := Event{Version: "v1", Generation: 1, Revision: prior.Revision + 1, PreviousHash: hex.EncodeToString(prior.EventHash[:]),
		CommandID: "30000000-0000-4000-8000-" + fmt.Sprintf("%012d", prior.Revision+1), CommandHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActorAccountID: "10000000-0000-4000-8000-000000000001", Kind: kind, At: time.Date(2026, 9, 20, 12, 0, int(prior.Revision+1), 0, time.UTC).Format(time.RFC3339Nano), Effects: effects}
	encoded, err := EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{CommandID: event.CommandID, Prior: prior, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
	if err := authority.ledger.AppendCandidate(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	next := Head{Generation: 1, Revision: event.Revision, EventHash: candidate.EventHash}
	if swapped, err := authority.heads.CompareAndSwap(t.Context(), prior, next); err != nil || !swapped {
		t.Fatalf("head append swapped=%v err=%v", swapped, err)
	}
	if err := authority.Projection.Apply(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
}

func (h *recoveryFixture) CommitEvent(t *testing.T, kind EventKind, effects []Effect) {
	t.Helper()
	release, err := h.fence.Write(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	commitFixtureEvent(t, h.authority, kind, effects)
}

func (h *recoveryFixture) CommitRemoval() {
	release, err := h.fence.Write(context.Background())
	if err != nil {
		panic(err)
	}
	defer release()
	h.mu.Lock()
	h.removed = true
	h.mu.Unlock()
}

func TestGuardRevocationWinsBeforeTransaction(t *testing.T) {
	h := newRecoveryFixture(t)
	stale := h.Principal
	h.CommitRemoval()
	called := false
	err := h.Service.WithAuthorizedTransaction(t.Context(), stale, h.FamilyID, "read", func(tx pgx.Tx, grant access.Grant) error {
		called = true
		return nil
	})
	if Code(err) != "membership_revoked" || called {
		t.Fatal("stale authorization entered tenant SQL")
	}
}

func TestDecisionContextAllowsOnlyAuthorityStateRead(t *testing.T) {
	h := newAuthority(t)
	var retained context.Context
	want := errors.New("decision complete")
	h.Service.decide = func(ctx context.Context, _ access.Principal, _, _ string) (access.Grant, error) {
		retained = ctx
		if h.Service.hasFenceMarker(ctx) {
			return access.Grant{}, errors.New("decision received internal fence marker")
		}
		if !h.Service.callbackContextDenied(ctx) {
			return access.Grant{}, errors.New("decision did not receive callback denial")
		}
		state, err := h.Service.ReadState(ctx)
		if err != nil || state.Head() != h.Head() {
			return access.Grant{}, fmt.Errorf("decision authority read head=%+v err=%v", state.Head(), err)
		}
		denied := map[string]func() error{
			"head":  func() error { _, err := h.Service.Head(ctx); return err },
			"check": func() error { return h.Service.Check(ctx) },
			"descendant": func() error {
				_, err := h.Service.IsDescendant(ctx, Head{Generation: 1})
				return err
			},
			"snapshot": func() error {
				return h.Service.WithAuthoritySnapshot(ctx, func(AuthoritySnapshot) error { return nil })
			},
			"write": func() error {
				_, err := h.Service.ExecutePrepared(ctx, func(context.Context, AuthoritySnapshot) (PreparedCommand, error) {
					return PreparedCommand{}, errorCode("invalid_command")
				})
				return err
			},
		}
		for name, operation := range denied {
			if err := operation(); Code(err) != "security_busy" {
				return access.Grant{}, fmt.Errorf("decision %s code=%q err=%v", name, Code(err), err)
			}
		}
		return access.Grant{}, want
	}
	internalCtx, lease := h.Service.withFenceLease(t.Context())
	defer lease.invalidate()
	if _, err := h.Service.decideWithAuthorityRead(t.Context(), internalCtx, access.Principal{}, "family", "read"); !errors.Is(err, want) {
		t.Fatalf("decision error=%v", err)
	}
	if _, err := h.Service.ReadState(retained); Code(err) != "security_busy" {
		t.Fatalf("retained decision context code=%q err=%v", Code(err), err)
	}
}

type decisionBlockingLedger struct {
	SecurityLedger
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (ledger *decisionBlockingLedger) WalkCommitted(ctx context.Context, head Head, visit func(Candidate) error) error {
	if ledger.armed.CompareAndSwap(true, false) {
		close(ledger.entered)
		select {
		case <-ledger.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ledger.SecurityLedger.WalkCommitted(ctx, head, visit)
}

func TestDecisionReadLeaseDrainsEnteredGoroutineBeforeTransactionAdvances(t *testing.T) {
	h := newAuthority(t)
	ledger := &decisionBlockingLedger{SecurityLedger: h.Service.ledger, entered: make(chan struct{}), release: make(chan struct{})}
	h.Service.ledger = ledger
	decisionReturned := make(chan struct{})
	readDone := make(chan error, 1)
	retained := make(chan context.Context, 1)
	h.Service.decide = func(ctx context.Context, _ access.Principal, _, _ string) (access.Grant, error) {
		retained <- ctx
		ledger.armed.Store(true)
		go func() {
			_, err := h.Service.ReadState(ctx)
			readDone <- err
		}()
		select {
		case <-ledger.entered:
			close(decisionReturned)
			return access.Grant{}, nil
		case err := <-readDone:
			return access.Grant{}, fmt.Errorf("decision read returned before entering authority: %w", err)
		case <-time.After(time.Second):
			return access.Grant{}, errors.New("decision read did not enter authority")
		}
	}
	internalCtx, fenceLease := h.Service.withFenceLease(t.Context())
	defer fenceLease.invalidate()
	outerDone := make(chan error, 1)
	go func() {
		_, err := h.Service.decideWithAuthorityRead(t.Context(), internalCtx, access.Principal{}, "family", "read")
		outerDone <- err
	}()
	var decisionCtx context.Context
	select {
	case <-decisionReturned:
		decisionCtx = <-retained
	case err := <-outerDone:
		t.Fatalf("transaction returned before decision read entered: %v", err)
	case <-time.After(time.Second):
		t.Fatal("decision did not return")
	}
	select {
	case err := <-outerDone:
		t.Fatalf("decision call returned while an admitted read was in flight: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := h.Service.ReadState(decisionCtx); Code(err) != "security_busy" {
		t.Fatalf("new read entered after decision returned: code=%q err=%v", Code(err), err)
	}
	close(ledger.release)
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("admitted decision read goroutine did not drain")
	}
	select {
	case err := <-outerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("decision call did not advance after admitted read drained")
	}
}

func TestAccountGuardRejectsSameEpochAuthorityChanges(t *testing.T) {
	for _, test := range []struct {
		name    string
		effects func(*recoveryFixture) (EventKind, []Effect)
	}{
		{name: "identity unavailable", effects: func(h *recoveryFixture) (EventKind, []Effect) {
			return "identity_unavailable", []Effect{{Kind: "identity_unavailable", AccountID: h.Principal.AccountID, IdentityKey: "hmac-sha256:identity-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Status: "identity_unavailable"}}
		}},
		{name: "device revoked", effects: func(h *recoveryFixture) (EventKind, []Effect) {
			return "device_binding_revoked", []Effect{{Kind: "device_binding_revoked", AccountID: h.Principal.AccountID, DeviceID: h.Principal.DeviceID, ServerInstanceID: "50000000-0000-4000-8000-000000000001", CanonicalOrigin: "https://family.example", DeviceCredentialDigest: "hmac-sha256:device-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Status: "revoked"}}
		}},
		{name: "session revoked", effects: func(h *recoveryFixture) (EventKind, []Effect) {
			return "session_revoked", []Effect{{Kind: "session_revoked", AccountID: h.Principal.AccountID, DeviceID: h.Principal.DeviceID, SessionID: h.Principal.SessionID, LineageRootSessionID: h.Principal.SessionID, RecoveryEpoch: 1, ExpiresAt: "2026-09-21T12:00:00Z", Status: "revoked"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoveryFixture(t)
			kind, effects := test.effects(h)
			h.CommitEvent(t, kind, effects)
			called := false
			err := h.Service.WithAccountTransaction(t.Context(), h.Principal, func(pgx.Tx) error { called = true; return nil })
			if Code(err) != "session_revoked" || called {
				t.Fatalf("code=%s called=%v err=%v", Code(err), called, err)
			}
		})
	}
}

func TestUnrelatedHeadAdvancePreservesMintedSessionAndFamilyScope(t *testing.T) {
	h := newRecoveryFixture(t)
	h.CommitEvent(t, "account_registered", []Effect{{Kind: "account_registered", AccountID: "10000000-0000-4000-8000-000000000002", IdentityKey: "second-identity", Status: "active"}})
	called := false
	if err := h.Service.WithAccountTransaction(t.Context(), h.Principal, func(pgx.Tx) error { called = true; return nil }); err != nil || !called {
		t.Fatalf("unrelated head advance invalidated session called=%v err=%v", called, err)
	}
	familyPrincipal := h.Principal
	familyPrincipal.Scope = "family"
	called = false
	if err := h.Service.WithAuthorizedTransaction(t.Context(), familyPrincipal, h.FamilyID, "read", func(pgx.Tx, access.Grant) error { called = true; return nil }); err != nil || !called {
		t.Fatalf("family scope rejected called=%v err=%v", called, err)
	}
	recoveryPrincipal := h.Principal
	recoveryPrincipal.Scope = "family_recovery"
	if err := h.Service.WithAccountTransaction(t.Context(), recoveryPrincipal, func(pgx.Tx) error { t.Fatal("recovery scope entered account transaction"); return nil }); Code(err) != "session_revoked" {
		t.Fatalf("recovery account route code=%s err=%v", Code(err), err)
	}
}

func TestGuardDistinguishesEpochChangeFromSameEpochRevocation(t *testing.T) {
	h := newRecoveryFixture(t)
	stale := h.Principal
	stale.RecoveryEpoch = 2
	for name, run := range map[string]func() error{
		"account": func() error {
			return h.Service.WithAccountTransaction(t.Context(), stale, func(pgx.Tx) error { t.Fatal("callback ran"); return nil })
		},
		"family": func() error {
			return h.Service.WithAuthorizedTransaction(t.Context(), stale, h.FamilyID, "read", func(pgx.Tx, access.Grant) error { t.Fatal("callback ran"); return nil })
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); Code(err) != "epoch_changed" {
				t.Fatalf("code=%s err=%v", Code(err), err)
			}
		})
	}
}

func TestGuardRejectsSessionAtAbsoluteDeadline(t *testing.T) {
	h := newRecoveryFixture(t)
	h.Service.now = func() time.Time { return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC) }
	for name, run := range map[string]func() error{
		"account": func() error {
			return h.Service.WithAccountTransaction(t.Context(), h.Principal, func(pgx.Tx) error { t.Fatal("callback ran"); return nil })
		},
		"family": func() error {
			return h.Service.WithAuthorizedTransaction(t.Context(), h.Principal, h.FamilyID, "read", func(pgx.Tx, access.Grant) error { t.Fatal("callback ran"); return nil })
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); Code(err) != "reauthentication_required" {
				t.Fatalf("code=%s err=%v", Code(err), err)
			}
		})
	}
}

func TestGuardTransactionFenceCompletesBeforeRemoval(t *testing.T) {
	h := newRecoveryFixture(t)
	entered := make(chan struct{})
	resume := make(chan struct{})
	transactionDone := make(chan error, 1)
	go func() {
		transactionDone <- h.Service.WithAuthorizedTransaction(t.Context(), h.Principal, h.FamilyID, "read", func(tx pgx.Tx, grant access.Grant) error {
			close(entered)
			<-resume
			return nil
		})
	}()
	<-entered
	removalDone := make(chan struct{})
	go func() { h.CommitRemoval(); close(removalDone) }()
	select {
	case <-removalDone:
		t.Fatal("removal crossed an active shared transaction fence")
	case <-time.After(50 * time.Millisecond):
	}
	close(resume)
	if err := <-transactionDone; err != nil {
		t.Fatal(err)
	}
	<-removalDone
	called := false
	err := h.Service.WithAuthorizedTransaction(t.Context(), h.Principal, h.FamilyID, "read", func(pgx.Tx, access.Grant) error {
		called = true
		return nil
	})
	if Code(err) != "membership_revoked" || called {
		t.Fatalf("post-removal transaction code=%s called=%v", Code(err), called)
	}
}

func TestDiagnosticRecordersRunAfterAuthorityFenceRelease(t *testing.T) {
	h := newRecoveryFixture(t)
	var phases []diagnostics.Phase
	var heldAtCallback []diagnostics.Phase
	var reentryErr error
	ctx := diagnostics.WithRecorder(t.Context(), func(_ context.Context, phase diagnostics.Phase, _ time.Duration) {
		if !h.fence.mu.TryLock() {
			heldAtCallback = append(heldAtCallback, phase)
		} else {
			h.fence.mu.Unlock()
		}
		if reentryErr == nil {
			reentryErr = h.Service.WithAuthoritySnapshot(context.Background(), func(AuthoritySnapshot) error { return nil })
		}
		phases = append(phases, phase)
	})

	leased, _, release, err := h.Service.beginGuardedTransaction(ctx, h.Principal)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Service.hasFenceMarker(leased) {
		t.Fatal("guarded transaction did not carry its active fence marker")
	}
	if err := h.Service.WithAuthoritySnapshot(h.Service.withCallbackDenial(ctx), func(AuthoritySnapshot) error {
		t.Fatal("denied callback entered authority snapshot")
		return nil
	}); Code(err) != "security_busy" || len(phases) != 0 {
		t.Fatalf("denied nested snapshot code=%q phases=%v", Code(err), phases)
	}
	release()
	if h.Service.hasFenceMarker(leased) {
		t.Fatal("returned fence lease survived release")
	}
	if err := h.Service.WithAuthorizedTransaction(ctx, h.Principal, h.FamilyID, "read", func(pgx.Tx, access.Grant) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := h.Service.WithAuthoritySnapshot(ctx, func(AuthoritySnapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(phases) != 4 || phases[0] != diagnostics.Authority || phases[1] != diagnostics.Authority ||
		phases[2] != diagnostics.Database || phases[3] != diagnostics.Authority {
		t.Fatalf("diagnostic phase order = %v", phases)
	}
	if len(heldAtCallback) != 0 || reentryErr != nil {
		t.Fatalf("recorder ran under authority fence=%v or failed reentry=%v", heldAtCallback, reentryErr)
	}
}

func TestTransactionLocalSettingsClearAfterEveryExit(t *testing.T) {
	h := newRecoveryFixture(t)
	for _, test := range []struct {
		name string
		run  func(pgx.Tx, access.Grant) error
	}{
		{name: "commit", run: func(pgx.Tx, access.Grant) error { return nil }},
		{name: "callback error", run: func(pgx.Tx, access.Grant) error { return errors.New("stop") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			_ = h.Service.WithAuthorizedTransaction(t.Context(), h.Principal, h.FamilyID, "read", test.run)
			tx, err := h.Runtime.BeginTx(t.Context(), pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			var account, family string
			if err := tx.QueryRow(t.Context(), "SELECT current_setting('app.account_id', true), current_setting('app.family_id', true)").Scan(&account, &family); err != nil {
				t.Fatal(err)
			}
			if account != "" || family != "" {
				t.Fatalf("transaction settings leaked: account=%q family=%q", account, family)
			}
		})
	}
}
