package family

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/account"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/security"
	"babytracker/server/internal/security/localstore"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

type familyFixture struct {
	t              *testing.T
	Service        *Service
	Security       *security.Service
	Auth           *auth.Service
	Identity       *identity.Service
	Owner          access.Principal
	SecondOwner    access.Principal
	NewCaregiver   access.Principal
	Runtime        *pgxpool.Pool
	database       *testpg.Fixture
	now            time.Time
	ids            atomic.Uint64
	invitationForA string
	invitationForB string
	invitationIDA  string
	invitationIDB  string
	bundles        map[string]auth.SessionBundle
}

func newFamilyFixture(t *testing.T) *familyFixture {
	return newFamilyFixtureMode(t, false)
}

func newFamilyFixtureMode(t *testing.T, hosted bool) *familyFixture {
	t.Helper()
	database := testpg.New(t)
	store, err := localstore.Open(filepath.Join(t.TempDir(), "ledger"), filepath.Join(t.TempDir(), "head"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeEmpty(t.Context()); err != nil {
		t.Fatal(err)
	}
	h := &familyFixture{t: t, database: database, Runtime: database.Runtime, now: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC), bundles: map[string]auth.SessionBundle{}}
	identityKeys := familyTestKeyring("identity.v1", 0x11)
	verifier := identity.IdentityVerifierFunc(func(_ context.Context, token, _ string) (identity.VerifiedIdentity, error) {
		return identity.VerifiedIdentity{Provider: "apple", Audience: "com.example.babytracker", Subject: token, IssuedAt: h.now.Add(-time.Minute), ExpiresAt: h.now.Add(time.Hour)}, nil
	})
	identityRepository, err := identity.NewRepository(database.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	h.Identity, err = identity.New(identity.Options{Verifier: verifier, Repository: identityRepository, Origin: "https://sync.example.test",
		InstanceID: "50000000-0000-4000-8000-000000000001", IdentityKeys: identityKeys, Now: func() time.Time { return h.now }})
	if err != nil {
		t.Fatal(err)
	}
	var service *Service
	h.Security, err = security.New(security.Options{Ledger: store.Ledger, Heads: store.Heads, Projection: security.NewProjector(database.Projector), Fence: store.Fence,
		Now: func() time.Time { return h.now }, Runtime: database.Runtime, Epoch: func(context.Context) (uint64, error) { return 1, nil },
		Decide: func(ctx context.Context, principal access.Principal, familyID, action string) (access.Grant, error) {
			if service == nil {
				return access.Grant{}, security.ErrUnavailable
			}
			return service.Authorize(ctx, principal, familyID, action)
		}, MaxCaregivers: 10, MaxInvitations: 10, RequireFamilySyncEntitlement: hosted})
	if err != nil {
		t.Fatal(err)
	}
	authRepository, err := auth.NewRepository(database.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	var actorKey [32]byte
	for index := range actorKey {
		actorKey[index] = 0x55
	}
	h.Auth, err = auth.New(auth.Options{Identity: h.Identity, Verifier: verifier, Security: h.Security, Repository: authRepository,
		Mode: config.ModeSelfHosted, Origin: "https://sync.example.test", InstanceID: "50000000-0000-4000-8000-000000000001",
		TokenKeys: familyTestKeyring("session.v1", 0x22), DeviceKeys: familyTestKeyring("device.v1", 0x33),
		NotificationKeys: familyTestKeyring("notification.v1", 0x44), ActorIDKey: actorKey, Now: func() time.Time { return h.now }})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(database.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	var entitlementPolicy EntitlementPolicy = SelfHostedEntitlementPolicy{}
	if hosted {
		entitlementPolicy = HostedEntitlementPolicy{Security: h.Security}
	}
	service, err = New(Options{Security: h.Security, Repository: repository, Entitlements: entitlementPolicy,
		InvitationKeys: familyTestKeyring("invitation.v1", 0x66), Origin: "https://sync.example.test", Now: func() time.Time { return h.now },
		MaxCaregivers: 10, MaxInvitations: 10, InvitationTTL: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h.Service = service
	h.Owner = h.signIn("owner-a", "Owner A")
	h.SecondOwner = h.signIn("owner-b", "Owner B")
	h.NewCaregiver = h.signIn("caregiver", "Caregiver")
	if _, err := h.Service.Create(t.Context(), h.Owner, h.CommandID(1), "Family A"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Create(t.Context(), h.SecondOwner, h.CommandID(2), "Family B"); err != nil {
		t.Fatal(err)
	}
	inviteA, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(3), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	inviteB, err := h.Service.Invite(t.Context(), h.SecondOwner, h.CommandID(4), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	h.invitationForA, h.invitationForB = inviteA.Token, inviteB.Token
	h.invitationIDA, h.invitationIDB = inviteA.Invitation.ID, inviteB.Invitation.ID
	return h
}

func familyTestKeyring(id string, fill byte) identity.DigestKeyring {
	key := identity.DigestKey{ID: id}
	for index := range key.Secret {
		key.Secret[index] = fill
	}
	return identity.DigestKeyring{Current: key}
}

func TestRecoveryClaimReservesSlotAndFirstRefreshPersistsNarrowScope(t *testing.T) {
	h := newFamilyFixture(t)
	if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(20), h.InvitationForFamilyA(), true); err != nil {
		t.Fatal(err)
	}
	state, err := h.Security.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	slot, ok := state.FamilySlot(h.Owner.AccountID)
	if !ok {
		t.Fatal("owner family slot missing")
	}
	status, err := h.Service.ScheduleDeletion(t.Context(), h.Owner, h.CommandID(21), true)
	if err != nil || status.FamilyID != slot.FamilyID || !status.PurgeAfter.Equal(h.now.Add(30*24*time.Hour)) || !status.CanRecover {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if _, err := h.Auth.Authenticate(t.Context(), h.bundles["owner-a"].AccessToken); auth.Code(err) != "session_revoked" {
		t.Fatalf("scheduled deletion retained old session: code=%q err=%v", auth.Code(err), err)
	}
	recovery := h.signIn("owner-a", "Owner A")
	if recovery.Scope != "account" {
		t.Fatalf("fresh proof scope=%q", recovery.Scope)
	}
	if slot := h.bundles["owner-a"].FamilySlot; slot == nil || slot.State != "family_recovery" || slot.FamilyID != status.FamilyID {
		t.Fatalf("fresh proof slot=%+v", slot)
	}
	var initialScope string
	var initialFamily *string
	if err := h.database.Owner.QueryRow(t.Context(), `SELECT scope,family_id::text FROM sessions
		WHERE account_id=$1::uuid AND device_id=$2::uuid AND session_id=$3::uuid`,
		recovery.AccountID, recovery.DeviceID, recovery.SessionID).Scan(&initialScope, &initialFamily); err != nil {
		t.Fatal(err)
	}
	if initialScope != "account" || initialFamily != nil {
		t.Fatalf("fresh proof persisted scope=%q family=%v", initialScope, initialFamily)
	}
	recovery = h.refresh(recovery, "owner-a")
	if recovery.Scope != "family_recovery" {
		t.Fatalf("refreshed scope=%q", recovery.Scope)
	}
	if _, err := h.Service.Authorize(t.Context(), recovery, status.FamilyID, "read"); Code(err) != "recovery_only" {
		t.Fatalf("recovery read code=%q err=%v", Code(err), err)
	}
	if _, err := h.Service.Create(t.Context(), recovery, h.CommandID(22), "Another family"); Code(err) != "family_slot_occupied" {
		t.Fatalf("recovery slot code=%q err=%v", Code(err), err)
	}
	var refreshedScope, refreshedFamily string
	if err := h.database.Owner.QueryRow(t.Context(), `SELECT scope,family_id::text FROM sessions
		WHERE account_id=$1::uuid AND device_id=$2::uuid AND session_id=$3::uuid`,
		recovery.AccountID, recovery.DeviceID, recovery.SessionID).Scan(&refreshedScope, &refreshedFamily); err != nil {
		t.Fatal(err)
	}
	if refreshedScope != "family_recovery" || refreshedFamily != status.FamilyID {
		t.Fatalf("refresh persisted scope=%q family=%q", refreshedScope, refreshedFamily)
	}
	if _, err := h.Service.Recover(t.Context(), recovery, h.CommandID(23), true); err != nil {
		t.Fatal(err)
	}
	state, err = h.Security.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if owner, _ := state.Membership(status.FamilyID, h.Owner.AccountID); owner.Status != "active" || owner.Role != "owner" {
		t.Fatalf("owner membership=%+v", owner)
	}
	if former, _ := state.Membership(status.FamilyID, h.NewCaregiver.AccountID); former.Status == "active" {
		t.Fatalf("former caregiver was implicitly restored: %+v", former)
	}
}

func TestInvitationRedeemCanonicalizesSubMicrosecondClockForPostgresRoundTrip(t *testing.T) {
	h := newFamilyFixture(t)
	h.now = h.now.Add(789 * time.Nanosecond)
	invitation, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(190), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(191), invitation.Token, true); err != nil {
		t.Fatal(err)
	}
}

func TestHostedFamilyCreateCanonicalizesSubMicrosecondClockForPostgresRoundTrip(t *testing.T) {
	h := newFamilyFixtureMode(t, true)
	h.now = h.now.Add(789 * time.Nanosecond)
	creator := h.signIn("hosted-precision-owner", "Hosted Precision Owner")
	if _, err := h.Service.Create(t.Context(), creator, h.CommandID(192), "Hosted Precision Family"); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryAtDeadlineDeniedAndOwnerAccountDeletionForfeitsClaim(t *testing.T) {
	h := newFamilyFixture(t)
	if _, err := (&account.Service{Security: h.Security}).Delete(t.Context(), h.Owner, h.CommandID(30), true); account.Code(err) != "ownership_transfer_required" {
		t.Fatalf("active owner deletion code=%q err=%v", account.Code(err), err)
	}
	status, err := h.Service.ScheduleDeletion(t.Context(), h.Owner, h.CommandID(31), true)
	if err != nil {
		t.Fatal(err)
	}
	h.now = status.PurgeAfter
	recovery := h.refresh(h.signIn("owner-a", "Owner A"), "owner-a")
	if _, err := h.Service.Recover(t.Context(), recovery, h.CommandID(32), true); Code(err) != "recovery_expired" {
		t.Fatalf("deadline recovery code=%q err=%v", Code(err), err)
	}
	if _, err := (&account.Service{Security: h.Security}).Delete(t.Context(), recovery, h.CommandID(33), true); err != nil {
		t.Fatal(err)
	}
	state, err := h.Security.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if claim, ok := state.RecoveryClaim(status.FamilyID, recovery.AccountID); !ok || claim.Status != "forfeited" {
		t.Fatalf("claim=%+v present=%v", claim, ok)
	}
	if accountView, _ := state.Account(recovery.AccountID); accountView.Status != "deleted" {
		t.Fatalf("account=%+v", accountView)
	}
}

func TestRecoveryVersusForfeitRaceHasOneAuthoritativeWinner(t *testing.T) {
	h := newFamilyFixture(t)
	status, err := h.Service.ScheduleDeletion(t.Context(), h.Owner, h.CommandID(40), true)
	if err != nil {
		t.Fatal(err)
	}
	recovery := h.refresh(h.signIn("owner-a", "Owner A"), "owner-a")
	recoverID, forfeitID := h.CommandID(41), h.CommandID(42)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for _, run := range []func() error{
		func() error { _, err := h.Service.Recover(t.Context(), recovery, recoverID, true); return err },
		func() error { _, err := h.Service.Forfeit(t.Context(), recovery, forfeitID, true); return err },
	} {
		wg.Add(1)
		go func(run func() error) {
			defer wg.Done()
			if run() == nil {
				wins.Add(1)
			}
		}(run)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("lifecycle winners=%d", wins.Load())
	}
	state, err := h.Security.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := state.RecoveryClaim(status.FamilyID, recovery.AccountID)
	if !ok || (claim.Status != "recovered" && claim.Status != "forfeited") {
		t.Fatalf("claim=%+v present=%v", claim, ok)
	}
	familyState, ok := state.Family(status.FamilyID)
	if !ok || (claim.Status == "recovered" && familyState.Status != "active") ||
		(claim.Status == "forfeited" && familyState.Status != "deletion_pending") {
		t.Fatalf("claim=%+v family=%+v present=%v", claim, familyState, ok)
	}
}

func TestLifecycleReceiptsReplayAcrossFreshProofAndTerminalState(t *testing.T) {
	t.Run("schedule and recover", func(t *testing.T) {
		h := newFamilyFixture(t)
		scheduleID := h.CommandID(50)
		firstStatus, err := h.Service.ScheduleDeletion(t.Context(), h.Owner, scheduleID, true)
		if err != nil {
			t.Fatal(err)
		}
		recovery := h.refresh(h.signIn("owner-a", "Owner A"), "owner-a")
		replayedStatus, err := h.Service.ScheduleDeletion(t.Context(), recovery, scheduleID, true)
		if err != nil || replayedStatus != firstStatus {
			t.Fatalf("schedule replay=%+v want=%+v err=%v", replayedStatus, firstStatus, err)
		}
		recoverID := h.CommandID(51)
		first, err := h.Service.Recover(t.Context(), recovery, recoverID, true)
		if err != nil {
			t.Fatal(err)
		}
		fresh := h.signIn("owner-a", "Owner A")
		second, err := h.Service.Recover(t.Context(), fresh, recoverID, true)
		if err != nil || second != first {
			t.Fatalf("recover replay=%+v want=%+v err=%v", second, first, err)
		}
		if _, err := h.Service.Forfeit(t.Context(), fresh, recoverID, true); Code(err) != "command_id_reused" {
			t.Fatalf("cross-kind code=%q err=%v", Code(err), err)
		}
	})

	t.Run("forfeit", func(t *testing.T) {
		h := newFamilyFixture(t)
		if _, err := h.Service.ScheduleDeletion(t.Context(), h.Owner, h.CommandID(52), true); err != nil {
			t.Fatal(err)
		}
		recovery := h.refresh(h.signIn("owner-a", "Owner A"), "owner-a")
		forfeitID := h.CommandID(53)
		first, err := h.Service.Forfeit(t.Context(), recovery, forfeitID, true)
		if err != nil {
			t.Fatal(err)
		}
		fresh := h.signIn("owner-a", "Owner A")
		second, err := h.Service.Forfeit(t.Context(), fresh, forfeitID, true)
		if err != nil || second != first {
			t.Fatalf("forfeit replay=%+v want=%+v err=%v", second, first, err)
		}
	})
}

func (h *familyFixture) signIn(subject, name string) access.Principal {
	h.t.Helper()
	challenge, err := h.Identity.Challenge(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	bundle, err := h.Auth.Exchange(h.t.Context(), identity.ExchangeRequest{CommandID: h.CommandID(int(h.ids.Load()) + 100), Challenge: challenge.Challenge,
		IdentityToken: subject, InstallationID: "installation-" + subject, Profile: &identity.ProfileInput{DisplayName: name, Confirmed: true}})
	if err != nil {
		h.t.Fatal(err)
	}
	h.bundles[subject] = bundle
	principal, err := h.Auth.Authenticate(h.t.Context(), bundle.AccessToken)
	if err != nil {
		h.t.Fatal(err)
	}
	return principal
}

func (h *familyFixture) refresh(principal access.Principal, subject string) access.Principal {
	h.t.Helper()
	bundle, err := h.Auth.Refresh(h.t.Context(), h.bundles[subject].RefreshToken)
	if err != nil {
		h.t.Fatal(err)
	}
	h.bundles[subject] = bundle
	refreshed, err := h.Auth.Authenticate(h.t.Context(), bundle.AccessToken)
	if err != nil {
		h.t.Fatal(err)
	}
	if refreshed.AccountID != principal.AccountID {
		h.t.Fatalf("refresh account=%q want=%q", refreshed.AccountID, principal.AccountID)
	}
	return refreshed
}

func (h *familyFixture) InvitationForFamilyA() string { return h.invitationForA }
func (h *familyFixture) InvitationForFamilyB() string { return h.invitationForB }
func (h *familyFixture) CommandID(index int) string {
	_ = index
	return fmt.Sprintf("30000000-0000-4000-8000-%012x", h.ids.Add(1))
}
func (h *familyFixture) ActiveSlots(accountID string) int {
	h.t.Helper()
	tx, err := h.Runtime.Begin(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	defer tx.Rollback(h.t.Context())
	if err := tenant.SetAccountLocal(h.t.Context(), tx, accountID); err != nil {
		h.t.Fatal(err)
	}
	var count int
	if err := tx.QueryRow(h.t.Context(), `SELECT count(*) FROM family_slots WHERE account_id=$1::uuid AND state='active'`, accountID).Scan(&count); err != nil {
		h.t.Fatal(err)
	}
	return count
}
func (h *familyFixture) CurrentHead() security.Head {
	head, err := h.Security.Head(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	return head
}

func (h *familyFixture) familyID(accountID string) string {
	h.t.Helper()
	state, err := h.Security.ReadState(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	slot, ok := state.FamilySlot(accountID)
	if !ok {
		h.t.Fatalf("missing family slot for %s", accountID)
	}
	return slot.FamilyID
}

func (h *familyFixture) projectedOwnerCount(accountID, familyID string) int {
	h.t.Helper()
	tx, err := h.Runtime.Begin(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	defer tx.Rollback(h.t.Context())
	if err := tenant.SetLocal(h.t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		h.t.Fatal(err)
	}
	var count int
	if err := tx.QueryRow(h.t.Context(), `SELECT count(*) FROM memberships WHERE family_id=$1::uuid AND role='owner' AND status='active'`, familyID).Scan(&count); err != nil {
		h.t.Fatal(err)
	}
	return count
}

func TestConcurrentFamilySlotClaims(t *testing.T) {
	h := newFamilyFixture(t)
	a, b := h.InvitationForFamilyA(), h.InvitationForFamilyB()
	var wins atomic.Int32
	var wg sync.WaitGroup
	for index, token := range []string{a, b} {
		index, token := index, token
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(index+20), token, true); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	state, err := h.Security.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if slot, ok := state.FamilySlot(h.NewCaregiver.AccountID); wins.Load() != 1 || !ok || slot.State != "active" || h.ActiveSlots(h.NewCaregiver.AccountID) != 1 {
		t.Fatalf("one-family invariant wins=%d slot=%+v present=%v projected=%d", wins.Load(), slot, ok, h.ActiveSlots(h.NewCaregiver.AccountID))
	}
}

func TestConcurrentCreateVersusJoinClaimsOneSlot(t *testing.T) {
	h := newFamilyFixture(t)
	createID, redeemID := h.CommandID(30), h.CommandID(31)
	var wins atomic.Int32
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := h.Service.Create(t.Context(), h.NewCaregiver, createID, "Caregiver family"); err == nil {
			wins.Add(1)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, redeemID, h.InvitationForFamilyA(), true); err == nil {
			wins.Add(1)
		}
	}()
	wg.Wait()
	if wins.Load() != 1 || h.ActiveSlots(h.NewCaregiver.AccountID) != 1 {
		t.Fatalf("wins=%d projected_slots=%d", wins.Load(), h.ActiveSlots(h.NewCaregiver.AccountID))
	}
	state, err := h.Security.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if slot, ok := state.FamilySlot(h.NewCaregiver.AccountID); !ok || slot.State != "active" {
		t.Fatalf("authority slot=%+v present=%v", slot, ok)
	}
}

func TestConcurrentOwnerTransfersHaveOneAuthorityAndProjectionWinner(t *testing.T) {
	h := newFamilyFixture(t)
	if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(40), h.InvitationForFamilyA(), true); err != nil {
		t.Fatal(err)
	}
	second := h.signIn("caregiver-two", "Caregiver Two")
	secret, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(41), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Redeem(t.Context(), second, h.CommandID(42), secret.Token, true); err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for index, target := range []string{h.NewCaregiver.AccountID, second.AccountID} {
		index, target := index, target
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.Service.Transfer(t.Context(), h.Owner, h.CommandID(43+index), target); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	familyID := h.familyID(h.Owner.AccountID)
	state, err := h.Security.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	family, ok := state.Family(familyID)
	if wins.Load() != 1 || !ok || family.OwnerAccountID == h.Owner.AccountID || h.projectedOwnerCount(h.Owner.AccountID, familyID) != 1 {
		t.Fatalf("wins=%d authority=%+v present=%v projected_owners=%d", wins.Load(), family, ok, h.projectedOwnerCount(h.Owner.AccountID, familyID))
	}
}

func TestInvitationRedeemVersusRevokeHasOneWinner(t *testing.T) {
	h := newFamilyFixture(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(50), h.InvitationForFamilyA(), true); err == nil {
			wins.Add(1)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := h.Service.RevokeInvitation(t.Context(), h.Owner, h.CommandID(51), h.invitationIDA); err == nil {
			wins.Add(1)
		}
	}()
	wg.Wait()
	state, err := h.Security.ReadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	invitation, ok := state.Invitation(h.invitationIDA)
	_, joined := state.FamilySlot(h.NewCaregiver.AccountID)
	if wins.Load() != 1 || !ok || (invitation.Status == "redeemed") != joined || (invitation.Status != "redeemed" && invitation.Status != "revoked") {
		t.Fatalf("wins=%d invitation=%+v present=%v joined=%v", wins.Load(), invitation, ok, joined)
	}
}

func TestInvitationBoundaryMetadataAndCrossFamilyPrivacy(t *testing.T) {
	h := newFamilyFixture(t)
	h.Service.invitationTTL = time.Minute
	short, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(59), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	head := h.CurrentHead()
	first, err := h.Service.Invitation(t.Context(), h.Owner, short.Invitation.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.Service.Invitation(t.Context(), h.Owner, short.Invitation.ID)
	if err != nil || first != second || h.CurrentHead() != head {
		t.Fatalf("metadata first=%+v second=%+v head_before=%+v head_after=%+v err=%v", first, second, head, h.CurrentHead(), err)
	}
	if _, err := h.Service.Invitation(t.Context(), h.Owner, h.invitationIDB); Code(err) != "invitation_invalid" {
		t.Fatalf("cross-family code=%q err=%v", Code(err), err)
	}
	h.now = first.ExpiresAt
	if _, err := h.Service.Preview(t.Context(), h.NewCaregiver, short.Token); Code(err) != "invitation_expired" {
		t.Fatalf("preview boundary code=%q err=%v", Code(err), err)
	}
	if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(60), short.Token, true); Code(err) != "invitation_expired" {
		t.Fatalf("redeem boundary code=%q err=%v", Code(err), err)
	}
	if h.ActiveSlots(h.NewCaregiver.AccountID) != 0 {
		t.Fatal("expired invitation projected a slot")
	}
}

func TestFamilyScopedAuthenticationRefreshAndAccountOperations(t *testing.T) {
	h := newFamilyFixture(t)
	original := h.bundles["owner-a"]
	principal, err := h.Auth.Authenticate(t.Context(), original.AccessToken)
	if err != nil || principal.Scope != "family" {
		t.Fatalf("authenticate scope=%q err=%v", principal.Scope, err)
	}
	account, err := h.Service.Account(t.Context(), principal)
	if err != nil || account.FamilySlot == nil || account.FamilySlot.FamilyID != h.familyID(h.Owner.AccountID) {
		t.Fatalf("account=%+v err=%v", account, err)
	}
	if _, err := h.Service.UpdateProfile(t.Context(), principal, h.CommandID(70), "Updated Owner", true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Preview(t.Context(), principal, h.InvitationForFamilyB()); err != nil {
		t.Fatal(err)
	}
	refreshed, err := h.Auth.Refresh(t.Context(), original.RefreshToken)
	if err != nil || refreshed.FamilySlot == nil || refreshed.FamilySlot.FamilyID != account.FamilySlot.FamilyID {
		t.Fatalf("refresh slot=%+v err=%v", refreshed.FamilySlot, err)
	}
	var scope string
	var familyID *string
	if err := h.database.Owner.QueryRow(t.Context(), `SELECT scope,family_id::text FROM sessions WHERE account_id=$1::uuid AND status='active' ORDER BY created_at DESC LIMIT 1`, principal.AccountID).Scan(&scope, &familyID); err != nil {
		t.Fatal(err)
	}
	if scope != "family" || familyID == nil || *familyID != account.FamilySlot.FamilyID {
		t.Fatalf("persisted scope=%q family=%v", scope, familyID)
	}
}

func TestUpdateProfileCommandReceiptIsImmutableAndAuthorityAware(t *testing.T) {
	h := newFamilyFixture(t)
	commandID := h.CommandID(71)
	first, err := h.Service.UpdateProfile(t.Context(), h.Owner, commandID, "First profile", true)
	if err != nil || first.DisplayName != "First profile" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err := h.Service.UpdateProfile(t.Context(), h.Owner, commandID, "First profile", true); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	latest, err := h.Service.UpdateProfile(t.Context(), h.Owner, h.CommandID(72), "Latest profile", true)
	if err != nil || latest.DisplayName != "Latest profile" {
		t.Fatalf("latest=%+v err=%v", latest, err)
	}
	replayed, err := h.Service.UpdateProfile(t.Context(), h.Owner, commandID, "First profile", true)
	if err != nil || replayed.DisplayName != "Latest profile" {
		t.Fatalf("old exact retry remutated profile=%+v err=%v", replayed, err)
	}
	if _, err := h.Service.UpdateProfile(t.Context(), h.Owner, commandID, "Changed reuse", true); Code(err) != "command_id_reused" {
		t.Fatalf("changed retry code=%q err=%v", Code(err), err)
	}
	inviteCommandID := h.CommandID(73)
	if _, err := h.Service.Invite(t.Context(), h.Owner, inviteCommandID, "caregiver"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.UpdateProfile(t.Context(), h.Owner, inviteCommandID, "Cross kind", true); Code(err) != "command_id_reused" {
		t.Fatalf("authority reuse code=%q err=%v", Code(err), err)
	}
}

func TestRefreshPersistsJoinedAndRemovedAuthoritySlot(t *testing.T) {
	h := newFamilyFixture(t)
	if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(75), h.InvitationForFamilyA(), true); err != nil {
		t.Fatal(err)
	}
	joined := h.refresh(h.NewCaregiver, "caregiver")
	joinedBundle := h.bundles["caregiver"]
	if joined.Scope != "family" || joinedBundle.FamilySlot == nil || joinedBundle.FamilySlot.State != "active" {
		t.Fatalf("joined principal=%+v slot=%+v", joined, joinedBundle.FamilySlot)
	}
	if _, err := h.Service.Remove(t.Context(), h.Owner, h.CommandID(76), joined.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Auth.Refresh(t.Context(), joinedBundle.RefreshToken); auth.Code(err) != "session_revoked" {
		t.Fatalf("removed refresh code=%q err=%v", auth.Code(err), err)
	}
	removed := h.signIn("caregiver", "Caregiver")
	removedBundle := h.bundles["caregiver"]
	if removed.Scope != "account" || removedBundle.FamilySlot != nil {
		t.Fatalf("removed principal=%+v slot=%+v", removed, removedBundle.FamilySlot)
	}
	var scope string
	var familyID *string
	if err := h.database.Owner.QueryRow(t.Context(), `SELECT scope,family_id::text FROM sessions WHERE account_id=$1::uuid AND status='active' ORDER BY created_at DESC LIMIT 1`, removed.AccountID).Scan(&scope, &familyID); err != nil {
		t.Fatal(err)
	}
	if scope != "account" || familyID != nil {
		t.Fatalf("persisted removed scope=%q family=%v", scope, familyID)
	}
}

func TestStructuralReceiptReplayAfterRefreshAndRoleChanges(t *testing.T) {
	h := newFamilyFixture(t)
	if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(80), h.InvitationForFamilyA(), true); err != nil {
		t.Fatal(err)
	}
	createdByOldOwner, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(81), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	transferID := h.CommandID(82)
	firstTransfer, err := h.Service.Transfer(t.Context(), h.Owner, transferID, h.NewCaregiver.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	oldOwnerFresh := h.refresh(h.Owner, "owner-a")
	secondTransfer, err := h.Service.Transfer(t.Context(), oldOwnerFresh, transferID, h.NewCaregiver.AccountID)
	if err != nil || secondTransfer != firstTransfer {
		t.Fatalf("transfer replay first=%+v second=%+v err=%v", firstTransfer, secondTransfer, err)
	}
	if _, err := h.Service.Transfer(t.Context(), oldOwnerFresh, transferID, h.SecondOwner.AccountID); Code(err) != "command_id_reused" {
		t.Fatalf("changed transfer code=%q err=%v", Code(err), err)
	}
	newOwnerFresh := h.refresh(h.NewCaregiver, "caregiver")
	revokeID := h.CommandID(83)
	firstRevoke, err := h.Service.RevokeInvitation(t.Context(), newOwnerFresh, revokeID, createdByOldOwner.Invitation.ID)
	if err != nil {
		t.Fatal(err)
	}
	newOwnerFresh = h.refresh(newOwnerFresh, "caregiver")
	secondRevoke, err := h.Service.RevokeInvitation(t.Context(), newOwnerFresh, revokeID, createdByOldOwner.Invitation.ID)
	if err != nil || secondRevoke != firstRevoke {
		t.Fatalf("revoke replay first=%+v second=%+v err=%v", firstRevoke, secondRevoke, err)
	}
	outsider := h.signIn("revoked-outsider", "Revoked Outsider")
	if _, err := h.Service.Redeem(t.Context(), outsider, h.CommandID(833), createdByOldOwner.Token, true); Code(err) != "invitation_revoked" {
		t.Fatalf("revoked invitation code=%q err=%v", Code(err), err)
	}
	removeID := h.CommandID(84)
	firstRemove, err := h.Service.Remove(t.Context(), newOwnerFresh, removeID, h.Owner.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	secondRemove, err := h.Service.Remove(t.Context(), newOwnerFresh, removeID, h.Owner.AccountID)
	if err != nil || secondRemove != firstRemove {
		t.Fatalf("remove replay first=%+v second=%+v err=%v", firstRemove, secondRemove, err)
	}
	leaver := h.signIn("leaver", "Leaver")
	invite, err := h.Service.Invite(t.Context(), newOwnerFresh, h.CommandID(85), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Redeem(t.Context(), leaver, h.CommandID(86), invite.Token, true); err != nil {
		t.Fatal(err)
	}
	leaveID := h.CommandID(87)
	firstLeave, err := h.Service.Leave(t.Context(), leaver, leaveID)
	if err != nil {
		t.Fatal(err)
	}
	leaverFresh := h.signIn("leaver", "Leaver")
	secondLeave, err := h.Service.Leave(t.Context(), leaverFresh, leaveID)
	if err != nil || secondLeave != firstLeave {
		t.Fatalf("leave replay first=%+v second=%+v err=%v", firstLeave, secondLeave, err)
	}
	if _, err := h.Service.Invite(t.Context(), leaverFresh, leaveID, "caregiver"); Code(err) != "command_id_reused" {
		t.Fatalf("cross-kind reuse code=%q err=%v", Code(err), err)
	}
}

func TestRemovedMemberIsPseudonymizedDeniedAndMayRejoin(t *testing.T) {
	h := newFamilyFixture(t)
	if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(90), h.InvitationForFamilyA(), true); err != nil {
		t.Fatal(err)
	}
	oldToken := h.bundles["caregiver"].AccessToken
	if _, err := h.Service.Remove(t.Context(), h.Owner, h.CommandID(91), h.NewCaregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Auth.Authenticate(t.Context(), oldToken); auth.Code(err) != "session_revoked" {
		t.Fatalf("removed authentication code=%q err=%v", auth.Code(err), err)
	}
	if _, err := h.Service.Preview(t.Context(), h.NewCaregiver, h.InvitationForFamilyB()); Code(err) != "session_revoked" {
		t.Fatalf("invitation operational code=%q err=%v", Code(err), err)
	}
	page, err := h.Service.ListActors(t.Context(), h.Owner, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	foundFormer := false
	for _, actor := range page.Items {
		if actor.AccountID == h.NewCaregiver.AccountID {
			foundFormer = actor.Former && actor.DisplayName == "Former caregiver"
		}
	}
	if !foundFormer {
		t.Fatalf("former actor not pseudonymized: %+v", page.Items)
	}
	rejoined := h.signIn("caregiver", "Caregiver")
	invite, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(92), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Redeem(t.Context(), rejoined, h.CommandID(93), invite.Token, true); err != nil {
		t.Fatal(err)
	}
	if h.ActiveSlots(rejoined.AccountID) != 1 {
		t.Fatal("rejoined caregiver has no projected slot")
	}
	page, err = h.Service.ListActors(t.Context(), h.Owner, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range page.Items {
		if actor.AccountID == rejoined.AccountID && (actor.Former || actor.DisplayName != "Caregiver") {
			t.Fatalf("rejoined actor=%+v", actor)
		}
	}
}

func TestMissingFamilyMaterialRequiresOneOwnerRepair(t *testing.T) {
	h := newFamilyFixture(t)
	owner := h.signIn("repair-owner", "Repair Owner")
	createID := h.CommandID(100)
	familyID, err := FamilyID(owner.AccountID, createID)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(security.FamilyCreatedPayload{Version: "v1", FamilyID: familyID, OwnerAccountID: owner.AccountID,
		ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	if _, err := h.Security.Execute(t.Context(), owner, security.Command{ID: createID, Kind: "family_created", AffectedAccountIDs: []string{owner.AccountID},
		AffectedFamilyIDs: []string{familyID}, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	placeholder, err := h.Service.Current(t.Context(), owner)
	if err != nil || !placeholder.ProfileRequired || placeholder.DisplayName != "" {
		t.Fatalf("placeholder=%+v err=%v", placeholder, err)
	}
	head := h.CurrentHead()
	var wins atomic.Int32
	var rejected atomic.Int32
	var wg sync.WaitGroup
	for index, name := range []string{"Recovered family A", "Recovered family B"} {
		index, name := index, name
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.Service.RepairProfile(t.Context(), owner, h.CommandID(101+index), name, true); err == nil {
				wins.Add(1)
			} else if Code(err) == "profile_not_required" {
				rejected.Add(1)
			} else {
				t.Errorf("repair code=%q err=%v", Code(err), err)
			}
		}()
	}
	wg.Wait()
	current, err := h.Service.Current(t.Context(), owner)
	if err != nil || wins.Load() != 1 || rejected.Load() != 1 || current.ProfileRequired || current.DisplayName == "" || h.CurrentHead().Revision != head.Revision+1 {
		t.Fatalf("wins=%d rejected=%d family=%+v before=%+v after=%+v err=%v", wins.Load(), rejected.Load(), current, head, h.CurrentHead(), err)
	}
}

func TestActorLabelPaginationUsesFixedHistoryHighWater(t *testing.T) {
	h := newFamilyFixture(t)
	if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(110), h.InvitationForFamilyA(), true); err != nil {
		t.Fatal(err)
	}
	second := h.signIn("label-two", "Label Two")
	invite, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(111), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Redeem(t.Context(), second, h.CommandID(112), invite.Token, true); err != nil {
		t.Fatal(err)
	}
	page, err := h.Service.ListActors(t.Context(), h.Owner, "", "", 1)
	if err != nil || page.HighWater == "" || page.NextCursor == "" || len(page.Items) != 1 {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	third := h.signIn("label-three", "Label Three")
	invite, err = h.Service.Invite(t.Context(), h.Owner, h.CommandID(113), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Redeem(t.Context(), third, h.CommandID(114), invite.Token, true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.UpdateProfile(t.Context(), h.NewCaregiver, h.CommandID(115), "Changed after high water", true); err != nil {
		t.Fatal(err)
	}
	items := append([]ActorLabelView{}, page.Items...)
	for page.NextCursor != "" {
		page, err = h.Service.ListActors(t.Context(), h.Owner, page.NextCursor, page.HighWater, 1)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, page.Items...)
	}
	if len(items) != 3 {
		t.Fatalf("fixed page items=%+v", items)
	}
	for _, actor := range items {
		if actor.AccountID == third.AccountID || (actor.AccountID == h.NewCaregiver.AccountID && actor.DisplayName != "Caregiver") {
			t.Fatalf("post-high-water label leaked: %+v", actor)
		}
	}
}

func TestConcurrentDuplicateCreateAndInviteReturnStableResults(t *testing.T) {
	h := newFamilyFixture(t)
	creator := h.signIn("duplicate-creator", "Duplicate Creator")
	createID := h.CommandID(120)
	head := h.CurrentHead()
	views := make([]FamilyView, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for index := range views {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			views[index], errs[index] = h.Service.Create(t.Context(), creator, createID, "Duplicate Family")
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || views[0].ID != views[1].ID || h.CurrentHead().Revision != head.Revision+1 {
		t.Fatalf("create views=%+v errors=%v head_before=%d head_after=%d", views, errs, head.Revision, h.CurrentHead().Revision)
	}
	if _, err := h.Service.Create(t.Context(), creator, createID, "Changed duplicate family"); Code(err) != "command_id_reused" {
		t.Fatalf("changed create code=%q err=%v", Code(err), err)
	}
	creator = h.refresh(creator, "duplicate-creator")
	inviteID := h.CommandID(121)
	head = h.CurrentHead()
	secrets := make([]InvitationSecret, 2)
	errs = make([]error, 2)
	for index := range secrets {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			secrets[index], errs[index] = h.Service.Invite(t.Context(), creator, inviteID, "caregiver")
		}()
	}
	wg.Wait()
	available, unavailable := 0, 0
	for index, secret := range secrets {
		if errs[index] != nil {
			t.Fatalf("invite %d: %v", index, errs[index])
		}
		if secret.Token != "" {
			available++
		}
		if secret.SecretUnavailable {
			unavailable++
		}
	}
	if available != 1 || unavailable != 1 || secrets[0].Invitation.ID != secrets[1].Invitation.ID || h.CurrentHead().Revision != head.Revision+1 {
		t.Fatalf("secrets=%+v head_before=%d head_after=%d", secrets, head.Revision, h.CurrentHead().Revision)
	}
}

func TestCreateInviteAndRedeemReplayAfterRefresh(t *testing.T) {
	h := newFamilyFixture(t)
	creator := h.signIn("fresh-create", "Fresh Creator")
	createID := h.CommandID(130)
	firstFamily, err := h.Service.Create(t.Context(), creator, createID, "Fresh Family")
	if err != nil {
		t.Fatal(err)
	}
	creator = h.refresh(creator, "fresh-create")
	secondFamily, err := h.Service.Create(t.Context(), creator, createID, "Fresh Family")
	if err != nil || secondFamily.ID != firstFamily.ID {
		t.Fatalf("create replay first=%+v second=%+v err=%v", firstFamily, secondFamily, err)
	}
	inviteID := h.CommandID(131)
	firstInvite, err := h.Service.Invite(t.Context(), creator, inviteID, "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	creator = h.refresh(creator, "fresh-create")
	secondInvite, err := h.Service.Invite(t.Context(), creator, inviteID, "caregiver")
	if err != nil || secondInvite.Invitation != firstInvite.Invitation || !secondInvite.SecretUnavailable || secondInvite.Token != "" {
		t.Fatalf("invite replay first=%+v second=%+v err=%v", firstInvite, secondInvite, err)
	}
	redeemer := h.signIn("fresh-redeem", "Fresh Redeemer")
	redeemID := h.CommandID(132)
	firstRedeem, err := h.Service.Redeem(t.Context(), redeemer, redeemID, firstInvite.Token, true)
	if err != nil {
		t.Fatal(err)
	}
	redeemer = h.refresh(redeemer, "fresh-redeem")
	secondRedeem, err := h.Service.Redeem(t.Context(), redeemer, redeemID, firstInvite.Token, true)
	if err != nil || secondRedeem.ID != firstRedeem.ID {
		t.Fatalf("redeem replay first=%+v second=%+v err=%v", firstRedeem, secondRedeem, err)
	}
}

func TestInvitationDigestAmbiguityQuarantinesWithoutRedeem(t *testing.T) {
	h := newFamilyFixture(t)
	previous := familyTestKeyring("invitation.v0", 0x67).Current
	h.Service.invitationKeys.Previous = []identity.DigestKey{previous}
	commandID := h.CommandID(140)
	secret, err := h.Service.Invite(t.Context(), h.Owner, commandID, "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	previousDigest, err := invitationDigest(previous, secret.Token)
	if err != nil {
		t.Fatal(err)
	}
	secondCommandID := h.CommandID(141)
	secondID, err := InvitationID(h.Owner.AccountID, secondCommandID)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(security.InvitationPayload{Version: "v1", InvitationID: secondID, FamilyID: secret.Invitation.FamilyID,
		CreatorAccountID: h.Owner.AccountID, TokenDigest: previousDigest, Role: "caregiver", Status: "active", ExpiresAt: secret.Invitation.ExpiresAt.Format(time.RFC3339Nano)})
	if _, err := h.Security.Execute(t.Context(), h.Owner, security.Command{ID: secondCommandID, Kind: "invitation_created", AffectedAccountIDs: []string{h.Owner.AccountID},
		AffectedFamilyIDs: []string{secret.Invitation.FamilyID}, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Preview(t.Context(), h.NewCaregiver, secret.Token); Code(err) != "authorization_quarantine" {
		t.Fatalf("preview code=%q err=%v", Code(err), err)
	}
	if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(142), secret.Token, true); Code(err) != "authorization_quarantine" {
		t.Fatalf("redeem code=%q err=%v", Code(err), err)
	}
	if h.ActiveSlots(h.NewCaregiver.AccountID) != 0 {
		t.Fatal("ambiguous invitation redeemed")
	}
}

func TestHostedCommittedFamilyReceiptsReplayAfterEntitlementExpiry(t *testing.T) {
	h := newFamilyFixtureMode(t, true)
	creator := h.signIn("hosted-replay-owner", "Hosted Replay Owner")
	createID := h.CommandID(150)
	firstFamily, err := h.Service.Create(t.Context(), creator, createID, "Hosted Replay Family")
	if err != nil {
		t.Fatal(err)
	}
	inviteID := h.CommandID(151)
	firstInvite, err := h.Service.Invite(t.Context(), creator, inviteID, "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	redeemer := h.signIn("hosted-replay-caregiver", "Hosted Replay Caregiver")
	redeemID := h.CommandID(152)
	firstRedeem, err := h.Service.Redeem(t.Context(), redeemer, redeemID, firstInvite.Token, true)
	if err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(367 * 24 * time.Hour)
	creator = h.signIn("hosted-replay-owner", "Hosted Replay Owner")
	redeemer = h.signIn("hosted-replay-caregiver", "Hosted Replay Caregiver")
	if _, err := h.Service.Current(t.Context(), creator); Code(err) != "entitlement_required" {
		t.Fatalf("ordinary read after expiry code=%q err=%v", Code(err), err)
	}
	secondFamily, err := h.Service.Create(t.Context(), creator, createID, "Hosted Replay Family")
	if err != nil || secondFamily.ID != firstFamily.ID {
		t.Fatalf("create replay first=%+v second=%+v err=%v", firstFamily, secondFamily, err)
	}
	secondInvite, err := h.Service.Invite(t.Context(), creator, inviteID, "caregiver")
	if err != nil || secondInvite.Invitation.ID != firstInvite.Invitation.ID || !secondInvite.SecretUnavailable {
		t.Fatalf("invite replay first=%+v second=%+v err=%v", firstInvite, secondInvite, err)
	}
	secondRedeem, err := h.Service.Redeem(t.Context(), redeemer, redeemID, firstInvite.Token, true)
	if err != nil || secondRedeem.ID != firstRedeem.ID {
		t.Fatalf("redeem replay first=%+v second=%+v err=%v", firstRedeem, secondRedeem, err)
	}
}

func TestRepairProfilePreauthorizesOwnerBeforeStaging(t *testing.T) {
	h := newFamilyFixture(t)
	if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(160), h.InvitationForFamilyA(), true); err != nil {
		t.Fatal(err)
	}
	commandID := h.CommandID(161)
	if _, err := h.Service.RepairProfile(t.Context(), h.NewCaregiver, commandID, "Caregiver cannot repair", true); Code(err) != "owner_required" {
		t.Fatalf("code=%q err=%v", Code(err), err)
	}
	var staged int
	if err := h.database.Owner.QueryRow(t.Context(), `SELECT count(*) FROM family_command_material WHERE command_id=$1::uuid`, commandID).Scan(&staged); err != nil {
		t.Fatal(err)
	}
	if staged != 0 {
		t.Fatalf("caregiver repair staged %d rows", staged)
	}
}

func TestHostedProfileRepairRequiresEntitlementBeforeStaging(t *testing.T) {
	h := newFamilyFixtureMode(t, true)
	owner := h.signIn("repair-no-entitlement", "Repair No Entitlement")
	createID := h.CommandID(165)
	familyID, err := FamilyID(owner.AccountID, createID)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(security.FamilyCreatedPayload{Version: "v1", FamilyID: familyID, OwnerAccountID: owner.AccountID,
		ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	if _, err := h.Security.Execute(t.Context(), owner, security.Command{ID: createID, Kind: "family_created", AffectedAccountIDs: []string{owner.AccountID},
		AffectedFamilyIDs: []string{familyID}, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	repairID := h.CommandID(166)
	if _, err := h.Service.RepairProfile(t.Context(), owner, repairID, "Must stay missing", true); Code(err) != "entitlement_required" {
		t.Fatalf("code=%q err=%v", Code(err), err)
	}
	var staged int
	if err := h.database.Owner.QueryRow(t.Context(), `SELECT count(*) FROM family_command_material WHERE command_id=$1::uuid`, repairID).Scan(&staged); err != nil {
		t.Fatal(err)
	}
	if staged != 0 {
		t.Fatalf("unentitled repair staged %d rows", staged)
	}
}

func TestHostedProfileRepairReceiptReplaysAfterEntitlementExpiry(t *testing.T) {
	h := newFamilyFixtureMode(t, true)
	owner := h.signIn("repair-replay", "Repair Replay")
	createID := h.CommandID(167)
	familyID, err := FamilyID(owner.AccountID, createID)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(security.FamilyBootstrappedPayload{Version: "v1", FamilyID: familyID, OwnerAccountID: owner.AccountID,
		EntitlementKey: "family_sync", EntitlementSource: "hosted_beta", ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	if _, err := h.Security.Execute(t.Context(), owner, security.Command{ID: createID, Kind: "family_bootstrapped", AffectedAccountIDs: []string{owner.AccountID},
		AffectedFamilyIDs: []string{familyID}, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	repairID := h.CommandID(168)
	first, err := h.Service.RepairProfile(t.Context(), owner, repairID, "Repaired before expiry", true)
	if err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(367 * 24 * time.Hour)
	owner = h.signIn("repair-replay", "Repair Replay")
	if _, err := h.Service.Current(t.Context(), owner); Code(err) != "entitlement_required" {
		t.Fatalf("ordinary read code=%q err=%v", Code(err), err)
	}
	second, err := h.Service.RepairProfile(t.Context(), owner, repairID, "Repaired before expiry", true)
	if err != nil || second.ID != first.ID || second.DisplayName != first.DisplayName {
		t.Fatalf("repair replay first=%+v second=%+v err=%v", first, second, err)
	}
}

func TestRevokeInvitationHidesMissingAndCrossFamilyBeforeAuthority(t *testing.T) {
	h := newFamilyFixture(t)
	missing := "60000000-0000-4000-8000-000000000099"
	for name, invitationID := range map[string]string{"missing": missing, "cross-family": h.invitationIDB} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.Service.RevokeInvitation(t.Context(), h.Owner, h.CommandID(170), invitationID); Code(err) != "invitation_invalid" {
				t.Fatalf("code=%q err=%v", Code(err), err)
			}
		})
	}
	if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(171), h.InvitationForFamilyA(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.RevokeInvitation(t.Context(), h.NewCaregiver, h.CommandID(172), h.invitationIDA); Code(err) != "owner_required" {
		t.Fatalf("same-family caregiver code=%q err=%v", Code(err), err)
	}
	if _, err := h.Service.RevokeInvitation(t.Context(), h.NewCaregiver, h.CommandID(173), h.invitationIDB); Code(err) != "invitation_invalid" {
		t.Fatalf("caregiver cross-family code=%q err=%v", Code(err), err)
	}
}

func TestRedeemReceiptNeverReturnsActorsLaterFamily(t *testing.T) {
	h := newFamilyFixture(t)
	redeemAID := h.CommandID(180)
	familyA, err := h.Service.Redeem(t.Context(), h.NewCaregiver, redeemAID, h.InvitationForFamilyA(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Leave(t.Context(), h.NewCaregiver, h.CommandID(181)); err != nil {
		t.Fatal(err)
	}
	rejoined := h.signIn("caregiver", "Caregiver")
	familyB, err := h.Service.Redeem(t.Context(), rejoined, h.CommandID(182), h.InvitationForFamilyB(), true)
	if err != nil {
		t.Fatal(err)
	}
	if familyA.ID == familyB.ID {
		t.Fatal("fixture families unexpectedly match")
	}
	if replay, err := h.Service.Redeem(t.Context(), rejoined, redeemAID, h.InvitationForFamilyA(), true); Code(err) != "family_unavailable" || replay.ID != "" {
		t.Fatalf("replay leaked later family=%+v code=%q err=%v", replay, Code(err), err)
	}
	current, err := h.Service.Current(t.Context(), rejoined)
	if err != nil || current.ID != familyB.ID {
		t.Fatalf("current=%+v err=%v", current, err)
	}
}

func TestReadConsumersUseDetachedViews(t *testing.T) {
	h := newFamilyFixtureMode(t, true)
	family, err := h.Service.Current(t.Context(), h.Owner)
	if err != nil {
		t.Fatal(err)
	}
	familyID := family.ID
	commandID := h.CommandID(90)
	if _, err := h.Service.Invite(t.Context(), h.Owner, commandID, "viewer"); err != nil {
		t.Fatal(err)
	}
	var receipt security.ReceiptView
	if err := h.Security.WithAuthorityView(t.Context(), func(view security.AuthorityView) error {
		receipt, _ = view.Receipt(h.Owner.AccountID, commandID)
		receipt.FamilyIDs[0] = "tampered"
		members := view.Memberships(familyID)
		members[0].Status = "revoked"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Invite(t.Context(), h.Owner, commandID, "viewer"); err != nil {
		t.Fatalf("receipt replay: %v", err)
	}
	if _, err := h.Service.Authorize(t.Context(), h.Owner, familyID, "read"); err != nil {
		t.Fatalf("membership/entitlement: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.Security.WithAuthorityView(ctx, func(security.AuthorityView) error { t.Error("canceled callback ran"); return nil }); err == nil {
		t.Fatal("canceled read accepted")
	}
	errors := make(chan error, 9)
	for i := 0; i < 8; i++ {
		go func() { _, err := h.Service.Authorize(t.Context(), h.Owner, familyID, "read"); errors <- err }()
	}
	go func() { _, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(91), "viewer"); errors <- err }()
	for i := 0; i < 9; i++ {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
}
