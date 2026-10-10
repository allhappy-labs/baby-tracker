package family

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/account"
	"babytracker/server/internal/auth"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"babytracker/server/internal/config"
	"babytracker/server/internal/identity"
	"babytracker/server/internal/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// familyFixture is the production composition over one disposable database:
// the authority (with this package's role policy), identity, auth and the
// family service, sharing one clock.
type familyFixture struct {
	t              *testing.T
	Service        *Service
	Authority      *authority.Service
	Auth           *auth.Service
	Identity       *identity.Service
	Owner          access.Principal
	SecondOwner    access.Principal
	NewCaregiver   access.Principal
	Runtime        *pgxpool.Pool
	db             *authoritytest.Fixture
	now            time.Time
	ids            atomic.Uint64
	invitationForA string
	invitationForB string
	invitationIDA  string
	invitationIDB  string
	bundles        map[string]auth.SessionBundle
}

// readGrant is the grant family policy gives p for action in one read
// transaction of the authority, with the family codes; a grant for any
// family but familyID is membership_revoked.
func (h *familyFixture) readGrant(ctx context.Context, p access.Principal, familyID, action string) (access.Grant, error) {
	var granted access.Grant
	err := h.Authority.WithFamilyReadTx(ctx, p, action, func(_ pgx.Tx, grant access.Grant) error {
		if grant.FamilyID != familyID {
			return ErrMembershipRevoked
		}
		granted = grant
		return nil
	})
	if err != nil {
		return access.Grant{}, familyError(err)
	}
	return granted, nil
}

func newFamilyFixture(t *testing.T) *familyFixture {
	return newFamilyFixtureMode(t, false)
}

// newFamilyFixtureMode builds the fixture; hosted makes the authority
// require the family_sync entitlement (family creation bootstraps a
// hosted_beta grant), as a hosted node does.
func newFamilyFixtureMode(t *testing.T, hosted bool) *familyFixture {
	t.Helper()
	db := authoritytest.New(t)
	h := &familyFixture{t: t, db: db, Runtime: db.Pool, now: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC), bundles: map[string]auth.SessionBundle{}}
	clock := func() time.Time { return h.now }
	tokenKeys := familyTestKeyring("session.v1", 0x22)
	var err error
	h.Authority, err = authority.New(authority.Options{Pool: db.Pool, Now: clock, InstanceID: db.InstanceID, RecoveryEpoch: authoritytest.RecoveryEpoch,
		TokenKeys: tokenKeys, MaxCaregivers: 10, MaxInvitations: 10, RequireFamilySyncEntitlement: hosted, Allowed: Allowed,
		EntitlementExempt: authority.LegacyEntitlementExempt})
	if err != nil {
		t.Fatal(err)
	}
	verifier := identity.IdentityVerifierFunc(func(_ context.Context, token, _ string) (identity.VerifiedIdentity, error) {
		return identity.VerifiedIdentity{Provider: "apple", Audience: "com.example.babytracker", Subject: token, IssuedAt: h.now.Add(-time.Minute), ExpiresAt: h.now.Add(time.Hour)}, nil
	})
	h.Identity, err = identity.New(identity.Options{Verifier: verifier, Authority: h.Authority.IdentityAuthority(), Origin: "https://sync.example.test",
		InstanceID: db.InstanceID, IdentityKeys: familyTestKeyring("identity.v1", 0x11), Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	var actorKey [32]byte
	for index := range actorKey {
		actorKey[index] = 0x55
	}
	h.Auth, err = auth.New(auth.Options{Identity: h.Identity, Verifier: verifier, Authority: h.Authority, Mode: config.ModeSelfHosted,
		Origin: "https://sync.example.test", InstanceID: db.InstanceID, TokenKeys: tokenKeys, DeviceKeys: familyTestKeyring("device.v1", 0x33),
		ActorIDKey: actorKey, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	h.Service, err = New(Options{Authority: h.Authority, InvitationKeys: familyTestKeyring("invitation.v1", 0x66), Origin: "https://sync.example.test",
		Now: clock, InvitationTTL: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
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

// query reads as the table owner inside an authority transaction (the
// authority tables admit only authority transactions).
func (h *familyFixture) query(statement string, args []any, dest ...any) {
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

// exec writes as the table owner inside an authority transaction.
func (h *familyFixture) exec(statement string, args ...any) int64 {
	h.t.Helper()
	return h.db.Exec(h.t, statement, args...)
}

// ActiveSlots counts the account's active slots as its own tenant sees them.
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

// slot is the account's family slot as the authority holds it (nil: none).
func (h *familyFixture) slot(accountID string) *authority.FamilySlot {
	h.t.Helper()
	slot, err := h.Authority.FamilySlot(h.t.Context(), accountID)
	if err != nil {
		h.t.Fatal(err)
	}
	return slot
}

func (h *familyFixture) familyID(accountID string) string {
	h.t.Helper()
	slot := h.slot(accountID)
	if slot == nil {
		h.t.Fatalf("missing family slot for %s", accountID)
	}
	return slot.FamilyID
}

// familySeq is the family's audit seq: every committed security change of
// the family appends exactly one row.
func (h *familyFixture) familySeq(familyID string) uint64 {
	h.t.Helper()
	seq, err := h.Authority.FamilyRevision(h.t.Context(), familyID)
	if err != nil {
		h.t.Fatal(err)
	}
	return seq
}

// receipts counts the actor's stored receipts for commandID.
func (h *familyFixture) receipts(actorID, commandID string) int {
	h.t.Helper()
	var count int
	h.query(`SELECT count(*) FROM command_receipts WHERE actor_account_id=$1 AND command_id=$2`, []any{actorID, commandID}, &count)
	return count
}

func (h *familyFixture) membership(familyID, accountID string) (role, status string) {
	h.t.Helper()
	h.query(`SELECT coalesce((SELECT role::text FROM memberships WHERE family_id=$1 AND account_id=$2),''),
		coalesce((SELECT status::text FROM memberships WHERE family_id=$1 AND account_id=$2),'')`, []any{familyID, accountID}, &role, &status)
	return role, status
}

func (h *familyFixture) familyRow(familyID string) (status, owner string) {
	h.t.Helper()
	h.query(`SELECT status::text, coalesce(owner_account_id::text,'') FROM families WHERE id=$1`, []any{familyID}, &status, &owner)
	return status, owner
}

func (h *familyFixture) claim(familyID, accountID string) string {
	h.t.Helper()
	var status string
	h.query(`SELECT coalesce((SELECT status::text FROM family_recovery_claims WHERE family_id=$1 AND account_id=$2),'')`, []any{familyID, accountID}, &status)
	return status
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

func TestRecoveryClaimReservesSlotAndFirstRefreshPersistsNarrowScope(t *testing.T) {
	h := newFamilyFixture(t)
	if _, err := h.Service.Redeem(t.Context(), h.NewCaregiver, h.CommandID(20), h.InvitationForFamilyA(), true); err != nil {
		t.Fatal(err)
	}
	slot := h.slot(h.Owner.AccountID)
	if slot == nil {
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
	h.query(`SELECT scope::text,family_id::text FROM sessions WHERE account_id=$1 AND device_id=$2 AND id=$3`,
		[]any{recovery.AccountID, recovery.DeviceID, recovery.SessionID}, &initialScope, &initialFamily)
	if initialScope != "account" || initialFamily != nil {
		t.Fatalf("fresh proof persisted scope=%q family=%v", initialScope, initialFamily)
	}
	// Before the first refresh the account-scope session already meets the
	// recovery reservation: family reads are family_unavailable (no active
	// slot) and recovery is refused (the session is not recovery scoped).
	if _, err := h.Service.Current(t.Context(), recovery); Code(err) != "family_unavailable" {
		t.Fatalf("account-scope current code=%q err=%v", Code(err), err)
	}
	if _, err := h.Service.RecoveryStatus(t.Context(), recovery); Code(err) != "recovery_only" {
		t.Fatalf("account-scope recovery status code=%q err=%v", Code(err), err)
	}
	recovery = h.refresh(recovery, "owner-a")
	if recovery.Scope != "family_recovery" {
		t.Fatalf("refreshed scope=%q", recovery.Scope)
	}
	if _, err := h.readGrant(t.Context(), recovery, status.FamilyID, "read"); Code(err) != "recovery_only" {
		t.Fatalf("recovery read code=%q err=%v", Code(err), err)
	}
	if _, err := h.Service.Current(t.Context(), recovery); Code(err) != "family_unavailable" {
		t.Fatalf("recovery current code=%q err=%v", Code(err), err)
	}
	if got, err := h.Service.RecoveryStatus(t.Context(), recovery); err != nil || got != status {
		t.Fatalf("recovery status=%+v err=%v want %+v", got, err, status)
	}
	if _, err := h.Service.Create(t.Context(), recovery, h.CommandID(22), "Another family"); Code(err) != "family_slot_occupied" {
		t.Fatalf("recovery slot code=%q err=%v", Code(err), err)
	}
	var refreshedScope, refreshedFamily string
	h.query(`SELECT scope::text,family_id::text FROM sessions WHERE account_id=$1 AND device_id=$2 AND id=$3`,
		[]any{recovery.AccountID, recovery.DeviceID, recovery.SessionID}, &refreshedScope, &refreshedFamily)
	if refreshedScope != "family_recovery" || refreshedFamily != status.FamilyID {
		t.Fatalf("refresh persisted scope=%q family=%q", refreshedScope, refreshedFamily)
	}
	if _, err := h.Service.Recover(t.Context(), recovery, h.CommandID(23), true); err != nil {
		t.Fatal(err)
	}
	if role, membership := h.membership(status.FamilyID, h.Owner.AccountID); membership != "active" || role != "owner" {
		t.Fatalf("owner membership role=%q status=%q", role, membership)
	}
	if _, membership := h.membership(status.FamilyID, h.NewCaregiver.AccountID); membership == "active" {
		t.Fatalf("former caregiver was implicitly restored: %q", membership)
	}
	// The recovered session is the owner's family session again.
	if grant, err := h.readGrant(t.Context(), recovery, status.FamilyID, "manage_members"); err != nil ||
		grant.FamilyID != status.FamilyID || grant.Role != "owner" {
		t.Fatalf("recovered grant=%+v err=%v", grant, err)
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
	accounts := &account.Service{Authority: h.Authority}
	if _, err := accounts.Delete(t.Context(), h.Owner, h.CommandID(30), true); account.Code(err) != "ownership_transfer_required" {
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
	deleted, err := accounts.Delete(t.Context(), recovery, h.CommandID(33), true)
	if err != nil || deleted.AppleAuthorizationRevocation != "manual_required" {
		t.Fatalf("deleted=%+v err=%v", deleted, err)
	}
	if claim := h.claim(status.FamilyID, recovery.AccountID); claim != "forfeited" {
		t.Fatalf("claim=%q", claim)
	}
	var accountStatus string
	h.query(`SELECT status::text FROM accounts WHERE id=$1`, []any{recovery.AccountID}, &accountStatus)
	if accountStatus != "deleted" {
		t.Fatalf("account=%q", accountStatus)
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
	claim := h.claim(status.FamilyID, recovery.AccountID)
	if claim != "recovered" && claim != "forfeited" {
		t.Fatalf("claim=%q", claim)
	}
	familyStatus, _ := h.familyRow(status.FamilyID)
	if (claim == "recovered" && familyStatus != "active") || (claim == "forfeited" && familyStatus != "deletion_pending") {
		t.Fatalf("claim=%q family=%q", claim, familyStatus)
	}
	var receipts int
	h.query(`SELECT count(*) FROM command_receipts WHERE actor_account_id=$1 AND command_id IN ($2,$3)`, []any{recovery.AccountID, recoverID, forfeitID}, &receipts)
	if receipts != 1 {
		t.Fatalf("receipts=%d", receipts)
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
		// A schedule retried after the family was recovered names a family no
		// longer pending deletion.
		if _, err := h.Service.ScheduleDeletion(t.Context(), fresh, scheduleID, true); Code(err) != "family_unavailable" {
			t.Fatalf("schedule replay after recovery code=%q err=%v", Code(err), err)
		}
	})

	t.Run("forfeit", func(t *testing.T) {
		h := newFamilyFixture(t)
		scheduleID := h.CommandID(52)
		if _, err := h.Service.ScheduleDeletion(t.Context(), h.Owner, scheduleID, true); err != nil {
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
		// The schedule retried after the forfeit reports the family still
		// pending, now unrecoverable (legacy recoveryStatusFromState).
		if status, err := h.Service.ScheduleDeletion(t.Context(), fresh, scheduleID, true); err != nil || status.CanRecover || status.State != "deletion_pending" {
			t.Fatalf("schedule replay after forfeit status=%+v err=%v", status, err)
		}
		// A new recovery command from the forfeited account is recovery_only:
		// its sessions are account scope again. That holds for the
		// pre-forfeit recovery principal too: RecoveryStatus runs before the
		// command, as in the legacy service (the command alone would say
		// recovery_forfeited).
		if _, err := h.Service.Recover(t.Context(), fresh, h.CommandID(54), true); Code(err) != "recovery_only" {
			t.Fatalf("recover after forfeit code=%q err=%v", Code(err), err)
		}
		if _, err := h.Service.Recover(t.Context(), recovery, h.CommandID(55), true); Code(err) != "recovery_only" {
			t.Fatalf("stale recovery principal code=%q err=%v", Code(err), err)
		}
		if _, err := h.Service.RecoveryStatus(t.Context(), recovery); Code(err) != "recovery_only" {
			t.Fatalf("stale recovery status code=%q err=%v", Code(err), err)
		}
	})
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
	if slot := h.slot(h.NewCaregiver.AccountID); wins.Load() != 1 || slot == nil || slot.State != "active" || h.ActiveSlots(h.NewCaregiver.AccountID) != 1 {
		t.Fatalf("one-family invariant wins=%d slot=%+v tenant_slots=%d", wins.Load(), slot, h.ActiveSlots(h.NewCaregiver.AccountID))
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
		t.Fatalf("wins=%d tenant_slots=%d", wins.Load(), h.ActiveSlots(h.NewCaregiver.AccountID))
	}
	if slot := h.slot(h.NewCaregiver.AccountID); slot == nil || slot.State != "active" {
		t.Fatalf("authority slot=%+v", slot)
	}
	if receipts := h.receipts(h.NewCaregiver.AccountID, createID) + h.receipts(h.NewCaregiver.AccountID, redeemID); receipts != 1 {
		t.Fatalf("receipts=%d", receipts)
	}
}

// Ported from TestConcurrentOwnerTransfersHaveOneAuthorityAndProjectionWinner:
// the authority row and the tenant's view are one and the same table now, so
// the winner is asserted on the family row, the memberships and the receipts.
func TestConcurrentOwnerTransfersHaveOneWinner(t *testing.T) {
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
	familyID := h.familyID(h.Owner.AccountID)
	before := h.familySeq(familyID)
	var wins atomic.Int32
	var wg sync.WaitGroup
	commandIDs := []string{h.CommandID(43), h.CommandID(44)}
	for index, target := range []string{h.NewCaregiver.AccountID, second.AccountID} {
		index, target := index, target
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.Service.Transfer(t.Context(), h.Owner, commandIDs[index], target); err == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	_, owner := h.familyRow(familyID)
	receipts := h.receipts(h.Owner.AccountID, commandIDs[0]) + h.receipts(h.Owner.AccountID, commandIDs[1])
	if wins.Load() != 1 || owner == h.Owner.AccountID || h.projectedOwnerCount(h.Owner.AccountID, familyID) != 1 || receipts != 1 || h.familySeq(familyID) != before+1 {
		t.Fatalf("wins=%d owner=%s tenant_owners=%d receipts=%d seq=%d->%d", wins.Load(), owner, h.projectedOwnerCount(h.Owner.AccountID, familyID),
			receipts, before, h.familySeq(familyID))
	}
	if role, status := h.membership(familyID, owner); role != "owner" || status != "active" {
		t.Fatalf("new owner membership role=%q status=%q", role, status)
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
	var status string
	h.query(`SELECT status::text FROM invitations WHERE id=$1`, []any{h.invitationIDA}, &status)
	joined := h.slot(h.NewCaregiver.AccountID) != nil
	if wins.Load() != 1 || (status == "redeemed") != joined || (status != "redeemed" && status != "revoked") {
		t.Fatalf("wins=%d invitation=%q joined=%v", wins.Load(), status, joined)
	}
}

func TestInvitationBoundaryMetadataAndCrossFamilyPrivacy(t *testing.T) {
	h := newFamilyFixture(t)
	h.Service.invitationTTL = time.Minute
	short, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(59), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	familyID := h.familyID(h.Owner.AccountID)
	seq := h.familySeq(familyID)
	first, err := h.Service.Invitation(t.Context(), h.Owner, short.Invitation.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.Service.Invitation(t.Context(), h.Owner, short.Invitation.ID)
	if err != nil || first != second || first != short.Invitation || h.familySeq(familyID) != seq {
		t.Fatalf("metadata first=%+v second=%+v minted=%+v seq_before=%d seq_after=%d err=%v", first, second, short.Invitation, seq, h.familySeq(familyID), err)
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
		t.Fatal("expired invitation granted a slot")
	}
}

// A session signed in before its account created its family authenticates
// as family scope at once (legacy derived the scope from the active slot;
// its session row stays account scope until the refresh persists it), so
// family requests reach the family paths without a refresh first.
func TestFamilyScopedAuthenticationRefreshAndAccountOperations(t *testing.T) {
	h := newFamilyFixture(t)
	original := h.bundles["owner-a"]
	principal, err := h.Auth.Authenticate(t.Context(), original.AccessToken)
	if err != nil || principal.Scope != "family" {
		t.Fatalf("authenticate scope=%q err=%v", principal.Scope, err)
	}
	if principal.SessionScope != "account" || principal.SessionFamilyID != "" {
		t.Fatalf("session scope=%q family=%q", principal.SessionScope, principal.SessionFamilyID)
	}
	account, err := h.Service.Account(t.Context(), principal)
	if err != nil || account.FamilySlot == nil || account.FamilySlot.FamilyID != h.familyID(h.Owner.AccountID) || account.DisplayName != "Owner A" ||
		account.Status != "active" || account.ProfileRequired {
		t.Fatalf("account=%+v err=%v", account, err)
	}
	if current, err := h.Service.Current(t.Context(), principal); err != nil || current.ID != account.FamilySlot.FamilyID {
		t.Fatalf("current=%+v err=%v", current, err)
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
	h.query(`SELECT scope::text,family_id::text FROM sessions WHERE account_id=$1 AND status='active' ORDER BY created_at DESC LIMIT 1`, []any{principal.AccountID}, &scope, &familyID)
	if scope != "family" || familyID == nil || *familyID != account.FamilySlot.FamilyID {
		t.Fatalf("persisted scope=%q family=%v", scope, familyID)
	}
	family, err := h.Auth.Authenticate(t.Context(), refreshed.AccessToken)
	if err != nil || family.Scope != "family" || family.SessionScope != "family" || family.SessionFamilyID != account.FamilySlot.FamilyID {
		t.Fatalf("refreshed principal=%+v err=%v", family, err)
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
	h.query(`SELECT scope::text,family_id::text FROM sessions WHERE account_id=$1 AND status='active' ORDER BY created_at DESC LIMIT 1`, []any{removed.AccountID}, &scope, &familyID)
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
		t.Fatal("rejoined caregiver has no slot")
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

// profileRequiredFamily is a family whose profile is missing (a legacy
// family imported without its material): created normally, then marked as
// the importer leaves it.
func (h *familyFixture) profileRequiredFamily(owner access.Principal) string {
	h.t.Helper()
	if _, err := h.Service.Create(h.t.Context(), owner, h.CommandID(0), "Before import"); err != nil {
		h.t.Fatal(err)
	}
	familyID := h.familyID(owner.AccountID)
	if affected := h.exec(`UPDATE families SET display_name='', profile_required=true WHERE id=$1`, familyID); affected != 1 {
		h.t.Fatalf("profile seed affected %d", affected)
	}
	return familyID
}

// Ported from TestMissingFamilyMaterialRequiresOneOwnerRepair: there is no
// staged material any more; a family whose profile is required is repaired
// exactly once by its owner (one receipt, one audit row).
func TestMissingFamilyMaterialRequiresOneOwnerRepair(t *testing.T) {
	h := newFamilyFixture(t)
	owner := h.signIn("repair-owner", "Repair Owner")
	familyID := h.profileRequiredFamily(owner)
	placeholder, err := h.Service.Current(t.Context(), owner)
	if err != nil || !placeholder.ProfileRequired || placeholder.DisplayName != "" {
		t.Fatalf("placeholder=%+v err=%v", placeholder, err)
	}
	seq := h.familySeq(familyID)
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
	if err != nil || wins.Load() != 1 || rejected.Load() != 1 || current.ProfileRequired || current.DisplayName == "" || h.familySeq(familyID) != seq+1 {
		t.Fatalf("wins=%d rejected=%d family=%+v seq=%d->%d err=%v", wins.Load(), rejected.Load(), current, seq, h.familySeq(familyID), err)
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
	if errs[0] != nil || errs[1] != nil || views[0].ID != views[1].ID || h.receipts(creator.AccountID, createID) != 1 || h.familySeq(views[0].ID) != 1 {
		t.Fatalf("create views=%+v errors=%v receipts=%d seq=%d", views, errs, h.receipts(creator.AccountID, createID), h.familySeq(views[0].ID))
	}
	if _, err := h.Service.Create(t.Context(), creator, createID, "Changed duplicate family"); Code(err) != "command_id_reused" {
		t.Fatalf("changed create code=%q err=%v", Code(err), err)
	}
	creator = h.refresh(creator, "duplicate-creator")
	inviteID := h.CommandID(121)
	seq := h.familySeq(views[0].ID)
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
	if available != 1 || unavailable != 1 || secrets[0].Invitation != secrets[1].Invitation || h.familySeq(views[0].ID) != seq+1 || h.receipts(creator.AccountID, inviteID) != 1 {
		t.Fatalf("secrets=%+v seq=%d->%d receipts=%d", secrets, seq, h.familySeq(views[0].ID), h.receipts(creator.AccountID, inviteID))
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
	secret, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(140), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	previousDigest, err := invitationDigest(previous, secret.Token)
	if err != nil {
		t.Fatal(err)
	}
	// A second invitation whose digest under the previous key matches the
	// same token: the token now names two invitations.
	secondCommandID := h.CommandID(141)
	secondID, err := InvitationID(h.Owner.AccountID, secondCommandID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Authority.CreateInvitation(t.Context(), authority.CreateInvitation{CommandID: secondCommandID, CreatorAccountID: h.Owner.AccountID,
		InvitationID: secondID, FamilyID: secret.Invitation.FamilyID, TokenDigest: previousDigest, Role: "caregiver", ExpiresAt: secret.Invitation.ExpiresAt,
		Principal: h.Owner}); err != nil {
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

// Ported from TestRepairProfilePreauthorizesOwnerBeforeStaging: a caregiver's
// repair is owner_required and writes nothing (no receipt, no audit row).
func TestRepairProfilePreauthorizesOwnerBeforeWriting(t *testing.T) {
	h := newFamilyFixture(t)
	owner := h.signIn("repair-owner", "Repair Owner")
	familyID := h.profileRequiredFamily(owner)
	invite, err := h.Service.Invite(t.Context(), owner, h.CommandID(0), "caregiver")
	if err != nil {
		t.Fatal(err)
	}
	caregiver := h.signIn("repair-caregiver", "Repair Caregiver")
	if _, err := h.Service.Redeem(t.Context(), caregiver, h.CommandID(160), invite.Token, true); err != nil {
		t.Fatal(err)
	}
	seq := h.familySeq(familyID)
	commandID := h.CommandID(161)
	if _, err := h.Service.RepairProfile(t.Context(), caregiver, commandID, "Caregiver cannot repair", true); Code(err) != "owner_required" {
		t.Fatalf("code=%q err=%v", Code(err), err)
	}
	if h.receipts(caregiver.AccountID, commandID) != 0 || h.familySeq(familyID) != seq {
		t.Fatalf("caregiver repair wrote receipts=%d seq=%d->%d", h.receipts(caregiver.AccountID, commandID), seq, h.familySeq(familyID))
	}
	if current, err := h.Service.Current(t.Context(), owner); err != nil || !current.ProfileRequired {
		t.Fatalf("family=%+v err=%v", current, err)
	}
}

// Ported from TestHostedProfileRepairRequiresEntitlementBeforeStaging.
func TestHostedProfileRepairRequiresEntitlementBeforeWriting(t *testing.T) {
	h := newFamilyFixtureMode(t, true)
	owner := h.signIn("repair-no-entitlement", "Repair No Entitlement")
	familyID := h.profileRequiredFamily(owner)
	if affected := h.exec(`DELETE FROM family_entitlements WHERE family_id=$1`, familyID); affected != 1 {
		t.Fatalf("entitlement removal affected %d", affected)
	}
	seq := h.familySeq(familyID)
	repairID := h.CommandID(166)
	if _, err := h.Service.RepairProfile(t.Context(), owner, repairID, "Must stay missing", true); Code(err) != "entitlement_required" {
		t.Fatalf("code=%q err=%v", Code(err), err)
	}
	if h.receipts(owner.AccountID, repairID) != 0 || h.familySeq(familyID) != seq {
		t.Fatalf("unentitled repair wrote receipts=%d seq=%d->%d", h.receipts(owner.AccountID, repairID), seq, h.familySeq(familyID))
	}
}

func TestHostedProfileRepairReceiptReplaysAfterEntitlementExpiry(t *testing.T) {
	h := newFamilyFixtureMode(t, true)
	owner := h.signIn("repair-replay", "Repair Replay")
	h.profileRequiredFamily(owner)
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

// Ported from TestReadConsumersUseDetachedViews: there are no in-memory
// authority views to detach any more; what remains is that concurrent live
// authorizations and a command of the same owner do not interfere, and the
// command's exact retry still replays.
func TestConcurrentFamilyReadAuthorizationAndInviteReplay(t *testing.T) {
	h := newFamilyFixtureMode(t, true)
	family, err := h.Service.Current(t.Context(), h.Owner)
	if err != nil {
		t.Fatal(err)
	}
	commandID := h.CommandID(90)
	first, err := h.Service.Invite(t.Context(), h.Owner, commandID, "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := h.Service.Invite(t.Context(), h.Owner, commandID, "viewer"); err != nil || replay.Invitation != first.Invitation || !replay.SecretUnavailable {
		t.Fatalf("receipt replay=%+v err=%v", replay, err)
	}
	errors := make(chan error, 9)
	for i := 0; i < 8; i++ {
		go func() {
			grant, err := h.readGrant(t.Context(), h.Owner, family.ID, "read")
			if err == nil && (grant.FamilyID != family.ID || grant.Role != "owner") {
				err = fmt.Errorf("grant=%+v", grant)
			}
			errors <- err
		}()
	}
	go func() { _, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(91), "viewer"); errors <- err }()
	for i := 0; i < 9; i++ {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := h.readGrant(ctx, h.Owner, family.ID, "read"); err == nil {
		t.Fatal("canceled authorization accepted")
	}
}

// A family read grants only the caller's own family and passes family
// policy codes through: another family is membership_revoked, a viewer's
// write is read_only_caregiver.
func TestFamilyReadGrantsOnlyTheCallersFamily(t *testing.T) {
	h := newFamilyFixture(t)
	familyA, familyB := h.familyID(h.Owner.AccountID), h.familyID(h.SecondOwner.AccountID)
	grant, err := h.readGrant(t.Context(), h.Owner, familyA, "read")
	if err != nil || grant.AccountID != h.Owner.AccountID || grant.FamilyID != familyA || grant.Role != "owner" || grant.Revision != h.familySeq(familyA) {
		t.Fatalf("grant=%+v err=%v", grant, err)
	}
	if _, err := h.readGrant(t.Context(), h.Owner, familyB, "read"); Code(err) != "membership_revoked" {
		t.Fatalf("other family code=%q err=%v", Code(err), err)
	}
	viewerInvite, err := h.Service.Invite(t.Context(), h.Owner, h.CommandID(0), "viewer")
	if err != nil {
		t.Fatal(err)
	}
	viewer := h.signIn("live-viewer", "Live Viewer")
	if _, err := h.Service.Redeem(t.Context(), viewer, h.CommandID(0), viewerInvite.Token, true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.readGrant(t.Context(), viewer, familyA, "mutate_record"); Code(err) != "read_only_caregiver" {
		t.Fatalf("viewer write code=%q err=%v", Code(err), err)
	}
}
