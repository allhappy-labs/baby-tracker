package authority_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"github.com/jackc/pgx/v5/pgconn"
)

// member is an account with a live identity, one device and an
// account-scope session, as a sign-in leaves it.
type member struct {
	account, device, session, refresh string
}

func newMember(t *testing.T, f *authoritytest.Fixture) member {
	t.Helper()
	m := member{account: liveAccount(t, f)}
	m.device = f.Device(t, m.account)
	m.session, _, m.refresh = f.Session(t, m.account, m.device, "account", "", "")
	return m
}

func (m member) principal(t *testing.T, f *authoritytest.Fixture) access.Principal {
	t.Helper()
	return principalOf(f, t, m.session)
}

// serviceWith is a second Service over the fixture's pool with changed
// options (hosted mode, smaller caps).
func serviceWith(t *testing.T, f *authoritytest.Fixture, change func(*authority.Options)) *authority.Service {
	t.Helper()
	o := authority.Options{Pool: f.Pool, Now: func() time.Time { return f.Now }, InstanceID: f.InstanceID, RecoveryEpoch: authoritytest.RecoveryEpoch,
		TokenKeys: f.TokenKeys, MaxCaregivers: 10, MaxInvitations: 10, Allowed: authoritytest.Allowed}
	change(&o)
	s, err := authority.New(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func createFamily(t *testing.T, f *authoritytest.Fixture, s *authority.Service, owner member) string {
	t.Helper()
	family := authoritytest.NewID(t)
	if _, err := s.CreateFamily(t.Context(), authority.CreateFamily{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account, FamilyID: family,
		DisplayName: "The Smiths", At: f.Now, Principal: owner.principal(t, f)}); err != nil {
		t.Fatal(err)
	}
	return family
}

func invitationOf(t *testing.T, f *authoritytest.Fixture, owner member, family, role string) authority.CreateInvitation {
	t.Helper()
	return authority.CreateInvitation{CommandID: authoritytest.NewID(t), CreatorAccountID: owner.account, InvitationID: authoritytest.NewID(t), FamilyID: family,
		TokenDigest: identityKey(t, "invite.v1"), Role: role, ExpiresAt: f.Now.Add(72 * time.Hour), At: f.Now, Principal: owner.principal(t, f)}
}

func invite(t *testing.T, f *authoritytest.Fixture, s *authority.Service, owner member, family, role string) authority.CreateInvitation {
	t.Helper()
	in := invitationOf(t, f, owner, family, role)
	if _, err := s.CreateInvitation(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	return in
}

func redeemOf(t *testing.T, f *authoritytest.Fixture, m member, digest string) authority.RedeemInvitation {
	t.Helper()
	return authority.RedeemInvitation{CommandID: authoritytest.NewID(t), RedeemerAccountID: m.account, TokenDigestCandidates: []string{digest}, At: f.Now,
		Principal: m.principal(t, f)}
}

// joined is a member that redeemed an invitation for role into family.
func joined(t *testing.T, f *authoritytest.Fixture, s *authority.Service, owner member, family, role string) member {
	t.Helper()
	m := newMember(t, f)
	in := invite(t, f, s, owner, family, role)
	if _, err := s.RedeemInvitation(t.Context(), redeemOf(t, f, m, in.TokenDigest)); err != nil {
		t.Fatal(err)
	}
	return m
}

func auditKinds(rows []auditRow) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.Kind
	}
	return out
}

func lastAudit(t *testing.T, f *authoritytest.Fixture, scope authority.Scope, id string) auditRow {
	t.Helper()
	rows := readAudit(t, f, scope, id)
	if len(rows) == 0 {
		t.Fatalf("no %s audit rows for %s", scope, id)
	}
	assertContiguous(t, rows, uint64(len(rows)))
	last := rows[len(rows)-1]
	if recompute(t, last) != last.Hash {
		t.Fatalf("audit row %+v does not recompute", last)
	}
	return last
}

func payloadOf(t *testing.T, row auditRow) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(row.Payload, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func membershipOf(t *testing.T, f *authoritytest.Fixture, family, account string) (role, status string) {
	t.Helper()
	inAuthorityTxQuery(t, f, `SELECT role::text, status::text FROM memberships WHERE family_id=$1 AND account_id=$2`, []any{family, account}, &role, &status)
	return role, status
}

func inAuthorityTxQuery(t *testing.T, f *authoritytest.Fixture, query string, args []any, dest ...any) {
	t.Helper()
	tx, err := f.Service.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := tx.QueryRow(t.Context(), query, args...).Scan(dest...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// labelQuery reads actor labels as the table owner in the family's tenant
// context: the runtime cannot read label history at all.
func labelQuery(t *testing.T, f *authoritytest.Fixture, family, query string, args []any, dest ...any) {
	t.Helper()
	tx, err := f.DB.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.family_id',$1,true)`, family); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(t.Context(), query, args...).Scan(dest...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func labelOf(t *testing.T, f *authoritytest.Fixture, family, account string) (name string, former bool) {
	t.Helper()
	labelQuery(t, f, family, `SELECT display_name, former FROM family_actor_labels WHERE family_id=$1 AND account_id=$2`, []any{family, account}, &name, &former)
	return name, former
}

func labelHistory(t *testing.T, f *authoritytest.Fixture, family, account string) (rows, active int) {
	t.Helper()
	labelQuery(t, f, family, `SELECT count(*), count(*) FILTER (WHERE NOT former) FROM family_actor_label_history WHERE family_id=$1 AND account_id=$2`,
		[]any{family, account}, &rows, &active)
	return rows, active
}

func TestCreateInviteRedeemReplay(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := authoritytest.NewID(t)
	create := authority.CreateFamily{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account, FamilyID: family, DisplayName: "The Smiths",
		At: f.Now, Principal: owner.principal(t, f)}
	created, err := f.Service.CreateFamily(t.Context(), create)
	if err != nil {
		t.Fatal(err)
	}
	if created.FamilyID != family || created.CommandID != create.CommandID || created.Outcome != "committed" || created.Code != "ok" || created.Revision != 1 {
		t.Fatalf("create result %+v", created)
	}
	var name, status, ownerID string
	var profileRequired, entitlementRequired bool
	inAuthorityTxQuery(t, f, `SELECT display_name, status::text, owner_account_id::text, profile_required, entitlement_required FROM families WHERE id=$1`,
		[]any{family}, &name, &status, &ownerID, &profileRequired, &entitlementRequired)
	if name != "The Smiths" || status != "active" || ownerID != owner.account || profileRequired || entitlementRequired {
		t.Fatalf("family row %q %s %s %v %v", name, status, ownerID, profileRequired, entitlementRequired)
	}
	if role, st := membershipOf(t, f, family, owner.account); role != "owner" || st != "active" {
		t.Fatalf("owner membership %s/%s", role, st)
	}
	slot, err := f.Service.FamilySlot(t.Context(), owner.account)
	if err != nil || slot == nil || slot.FamilyID != family || slot.State != "active" {
		t.Fatalf("owner slot %+v %v", slot, err)
	}
	if label, former := labelOf(t, f, family, owner.account); label != "Fixture" || former {
		t.Fatalf("owner label %q former=%v", label, former)
	}
	if rows, _ := labelHistory(t, f, family, owner.account); rows != 1 {
		t.Fatalf("%d label history rows, want 1", rows)
	}
	if row := lastAudit(t, f, authority.ScopeFamily, family); row.Kind != "family_created" || row.CommandID != create.CommandID || row.ActorID != owner.account {
		t.Fatalf("family audit %+v", row)
	}
	if row := lastAudit(t, f, authority.ScopeAccount, owner.account); row.Kind != "family_created" || row.Seq != 1 {
		t.Fatalf("account audit %+v", row)
	}

	// Exact retry: the stored result, nothing written; a different name under
	// the same command ID is refused.
	again, err := f.Service.CreateFamily(t.Context(), create)
	if err != nil || again != created {
		t.Fatalf("replay %+v %v, want %+v", again, err, created)
	}
	changed := create
	changed.DisplayName = "The Joneses"
	_, err = f.Service.CreateFamily(t.Context(), changed)
	wantCode(t, err, "command_id_reused")
	if rows := readAudit(t, f, authority.ScopeFamily, family); len(rows) != 1 {
		t.Fatalf("%d family audit rows after replays", len(rows))
	}
	// A second family for the same account is refused.
	second := authority.CreateFamily{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account, FamilyID: authoritytest.NewID(t), DisplayName: "Other",
		At: f.Now, Principal: owner.principal(t, f)}
	_, err = f.Service.CreateFamily(t.Context(), second)
	wantCode(t, err, "family_slot_occupied")

	// Invitation: the first answer carries the secret; the exact retry is
	// the stored result marked SecretUnavailable, never a second invitation.
	inv := invitationOf(t, f, owner, family, "viewer")
	first, err := f.Service.CreateInvitation(t.Context(), inv)
	if err != nil || first.SecretUnavailable || first.Invitation.ID != inv.InvitationID || first.Invitation.Role != "viewer" ||
		first.Invitation.Status != "active" || !first.Invitation.ExpiresAt.Equal(inv.ExpiresAt) || first.Revision != 2 {
		t.Fatalf("invite %+v %v", first, err)
	}
	retry := inv
	retry.TokenDigest = identityKey(t, "invite.v1") // the service mints a fresh token per attempt
	retry.ExpiresAt = inv.ExpiresAt.Add(time.Minute)
	replayed, err := f.Service.CreateInvitation(t.Context(), retry)
	if err != nil || !replayed.SecretUnavailable || replayed.Result != first.Result || replayed.Invitation.ID != inv.InvitationID ||
		replayed.Invitation.TokenDigest != inv.TokenDigest || !replayed.Invitation.ExpiresAt.Equal(inv.ExpiresAt) {
		t.Fatalf("invitation replay %+v %v", replayed, err)
	}
	if n := count(t, f, `SELECT count(*) FROM invitations WHERE family_id=$1`, family); n != 1 {
		t.Fatalf("%d invitations after replay", n)
	}
	other := inv
	other.Role = "caregiver"
	_, err = f.Service.CreateInvitation(t.Context(), other)
	wantCode(t, err, "command_id_reused")
	if row := lastAudit(t, f, authority.ScopeFamily, family); row.Kind != "invitation_created" {
		t.Fatalf("invitation audit %+v", row)
	} else if p := payloadOf(t, row); p["invitation_id"] != inv.InvitationID || p["role"] != "viewer" || p["expires_at"] == nil {
		t.Fatalf("invitation audit payload %s", row.Payload)
	}

	// Preview: display name and status only.
	preview, found, err := f.Service.LookupInvitation(t.Context(), []string{identityKey(t, "invite.v0"), inv.TokenDigest})
	if err != nil || !found || preview.FamilyDisplayName != "The Smiths" || preview.Status != "active" || preview.FamilyID != "" || preview.ID != "" ||
		!preview.ExpiresAt.Equal(inv.ExpiresAt) {
		t.Fatalf("preview %+v %v %v", preview, found, err)
	}

	// Redeem: membership with the invitation's role, a slot, the label; the
	// redeemer's session keeps its scope until it refreshes.
	joiner := newMember(t, f)
	redeem := redeemOf(t, f, joiner, inv.TokenDigest)
	joinedResult, err := f.Service.RedeemInvitation(t.Context(), redeem)
	if err != nil || joinedResult.FamilyID != family || joinedResult.Revision != 3 {
		t.Fatalf("redeem %+v %v", joinedResult, err)
	}
	if role, st := membershipOf(t, f, family, joiner.account); role != "viewer" || st != "active" {
		t.Fatalf("joiner membership %s/%s", role, st)
	}
	if slot, err := f.Service.FamilySlot(t.Context(), joiner.account); err != nil || slot == nil || slot.FamilyID != family {
		t.Fatalf("joiner slot %+v %v", slot, err)
	}
	view, found, err := f.Service.Invitation(t.Context(), family, inv.InvitationID)
	if err != nil || !found || view.Status != "redeemed" {
		t.Fatalf("invitation after redeem %+v %v %v", view, found, err)
	}
	if session, _ := sessionRow(t, f, joiner.session); session.Scope != "account" || session.Status != "active" {
		t.Fatalf("redeemer session changed: %+v", session)
	}
	if label, former := labelOf(t, f, family, joiner.account); label != "Fixture" || former {
		t.Fatalf("joiner label %q %v", label, former)
	}
	if row := lastAudit(t, f, authority.ScopeAccount, joiner.account); row.Kind != "invitation_redeemed" {
		t.Fatalf("redeemer audit %+v", row)
	}
	if row := lastAudit(t, f, authority.ScopeFamily, family); row.Kind != "invitation_redeemed" {
		t.Fatalf("family audit %+v", row)
	}
	replayJoin, err := f.Service.RedeemInvitation(t.Context(), redeem)
	if err != nil || replayJoin != joinedResult {
		t.Fatalf("redeem replay %+v %v", replayJoin, err)
	}
	late := newMember(t, f)
	_, err = f.Service.RedeemInvitation(t.Context(), redeemOf(t, f, late, inv.TokenDigest))
	wantCode(t, err, "invitation_consumed")
	_, err = f.Service.RedeemInvitation(t.Context(), redeemOf(t, f, late, identityKey(t, "invite.v1")))
	wantCode(t, err, "invitation_invalid")
}

func TestRedeemInvitationRefusals(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)

	expired := invite(t, f, f.Service, owner, family, "caregiver")
	f.Now = f.Now.Add(72 * time.Hour)
	_, err := f.Service.RedeemInvitation(t.Context(), redeemOf(t, f, newMember(t, f), expired.TokenDigest))
	wantCode(t, err, "invitation_expired")

	revoked := invite(t, f, f.Service, owner, family, "caregiver")
	if _, err := f.Service.RevokeInvitation(t.Context(), authority.RevokeInvitation{CommandID: authoritytest.NewID(t), ActorAccountID: owner.account,
		InvitationID: revoked.InvitationID, At: f.Now, Principal: owner.principal(t, f)}); err != nil {
		t.Fatal(err)
	}
	_, err = f.Service.RedeemInvitation(t.Context(), redeemOf(t, f, newMember(t, f), revoked.TokenDigest))
	wantCode(t, err, "invitation_revoked")

	// A member already in a family cannot join another.
	elsewhere := newMember(t, f)
	createFamily(t, f, f.Service, elsewhere)
	valid := invite(t, f, f.Service, owner, family, "caregiver")
	_, err = f.Service.RedeemInvitation(t.Context(), redeemOf(t, f, elsewhere, valid.TokenDigest))
	wantCode(t, err, "family_slot_occupied")

	// A family that is no longer active refuses joins.
	f.Exec(t, `UPDATE families SET status='deletion_pending', purge_after=$2 WHERE id=$1`, family, f.Now.Add(time.Hour))
	_, err = f.Service.RedeemInvitation(t.Context(), redeemOf(t, f, newMember(t, f), valid.TokenDigest))
	wantCode(t, err, "family_unavailable")
	if _, found, err := f.Service.LookupInvitation(t.Context(), []string{valid.TokenDigest}); err != nil || found {
		t.Fatalf("preview of an inactive family found=%v err=%v", found, err)
	}
}

func TestConcurrentCreateVersusJoinClaimsOneSlot(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	var invitations []authority.CreateInvitation
	for range 10 {
		invitations = append(invitations, invite(t, f, f.Service, owner, family, "caregiver"))
	}
	joiner := newMember(t, f)
	p := joiner.principal(t, f)
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var err error
			if i%2 == 0 {
				_, err = f.Service.CreateFamily(context.Background(), authority.CreateFamily{CommandID: authoritytest.NewID(t), OwnerAccountID: joiner.account,
					FamilyID: authoritytest.NewID(t), DisplayName: fmt.Sprintf("Family %d", i), At: f.Now, Principal: p})
			} else {
				_, err = f.Service.RedeemInvitation(context.Background(), authority.RedeemInvitation{CommandID: authoritytest.NewID(t), RedeemerAccountID: joiner.account,
					TokenDigestCandidates: []string{invitations[i/2].TokenDigest}, At: f.Now, Principal: p})
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	wins := 0
	for err := range errs {
		switch authority.CodeOf(err) {
		case "":
			wins++
		case "family_slot_occupied":
		default:
			t.Errorf("unexpected %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d winners, want exactly 1", wins)
	}
	if n := count(t, f, `SELECT count(*) FROM family_slots WHERE account_id=$1`, joiner.account); n != 1 {
		t.Fatalf("%d slots", n)
	}
	if n := count(t, f, `SELECT count(*) FROM memberships WHERE account_id=$1 AND status='active'`, joiner.account); n != 1 {
		t.Fatalf("%d active memberships", n)
	}
}

// The family_slots primary key is the backstop for one family per account:
// a unique violation on it is family_slot_occupied, on the one-owner index
// invalid_command.
func TestFamilyUniqueViolationsMapToLegacyCodes(t *testing.T) {
	for constraint, want := range map[string]string{"family_slots_pkey": "family_slot_occupied", "memberships_one_active_owner": "invalid_command",
		"some_other_key": "security_busy"} {
		err := authority.MapFamilyWriteError(errors.Join(&pgconn.PgError{Code: "23505", ConstraintName: constraint}, authority.ErrSecurityBusy))
		if got := authority.CodeOf(err); got != want {
			t.Errorf("%s: %q, want %q", constraint, got, want)
		}
	}
}

func TestConcurrentOwnerTransfersHaveOneWinner(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	a := joined(t, f, f.Service, owner, family, "caregiver")
	b := joined(t, f, f.Service, owner, family, "caregiver")
	ownerFamilySession, _, _ := f.Session(t, owner.account, owner.device, "family", family, "owner")
	aSession, _, _ := f.Session(t, a.account, a.device, "family", family, "caregiver")
	bSession, _, _ := f.Session(t, b.account, b.device, "family", family, "caregiver")
	p := owner.principal(t, f)
	results := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, target := range []member{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = f.Service.TransferOwner(context.Background(), authority.TransferOwner{CommandID: authoritytest.NewID(t), OldOwnerID: owner.account,
				NewOwnerID: target.account, At: f.Now, Principal: p})
		}()
	}
	close(start)
	wg.Wait()
	winner, winnerSession, loserSession := -1, "", ""
	for i, err := range results {
		switch authority.CodeOf(err) {
		case "":
			if winner != -1 {
				t.Fatal("two transfers won")
			}
			winner = i
		case "owner_required":
		default:
			t.Fatalf("transfer %d: %v", i, err)
		}
	}
	if winner == -1 {
		t.Fatal("no transfer won")
	}
	newOwner := []member{a, b}[winner]
	winnerSession, loserSession = []string{aSession, bSession}[winner], []string{aSession, bSession}[1-winner]
	if n := count(t, f, `SELECT count(*) FROM memberships WHERE family_id=$1 AND role='owner' AND status='active'`, family); n != 1 {
		t.Fatalf("%d active owners", n)
	}
	if role, _ := membershipOf(t, f, family, newOwner.account); role != "owner" {
		t.Fatalf("new owner role %s", role)
	}
	if role, _ := membershipOf(t, f, family, owner.account); role != "caregiver" {
		t.Fatalf("old owner role %s", role)
	}
	var ownerID string
	inAuthorityTxQuery(t, f, `SELECT owner_account_id::text FROM families WHERE id=$1`, []any{family}, &ownerID)
	if ownerID != newOwner.account {
		t.Fatalf("families.owner_account_id %s", ownerID)
	}
	for id, role := range map[string]string{ownerFamilySession: "caregiver", winnerSession: "owner", loserSession: "caregiver"} {
		if row, _ := sessionRow(t, f, id); row.Role != role || row.Status != "active" {
			t.Fatalf("session %s role %s status %s, want %s", id, row.Role, row.Status, role)
		}
	}
	if row, _ := sessionRow(t, f, owner.session); row.Scope != "account" || row.Role != "" {
		t.Fatalf("account-scope session touched: %+v", row)
	}
	row := lastAudit(t, f, authority.ScopeFamily, family)
	if row.Kind != "owner_transferred" {
		t.Fatalf("family audit %+v", row)
	}
	for _, account := range []string{owner.account, newOwner.account} {
		if row := lastAudit(t, f, authority.ScopeAccount, account); row.Kind != "owner_transferred" {
			t.Fatalf("account %s audit %+v", account, row)
		}
	}
	// A viewer cannot be made owner.
	viewer := joined(t, f, f.Service, newOwner, family, "viewer")
	_, err := f.Service.TransferOwner(t.Context(), authority.TransferOwner{CommandID: authoritytest.NewID(t), OldOwnerID: newOwner.account,
		NewOwnerID: viewer.account, At: f.Now, Principal: newOwner.principal(t, f)})
	wantCode(t, err, "membership_revoked")
}

func TestRemoveMemberRevokesSessionsAndFreesSlot(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	caregiver := joined(t, f, f.Service, owner, family, "caregiver")
	familySession, _, _ := f.Session(t, caregiver.account, caregiver.device, "family", family, "caregiver")
	viewer := joined(t, f, f.Service, owner, family, "viewer")

	// Only the owner removes, never the owner, never a non-member.
	_, err := f.Service.RemoveMember(t.Context(), authority.RemoveMember{CommandID: authoritytest.NewID(t), OwnerAccountID: caregiver.account,
		AccountID: viewer.account, At: f.Now, Principal: caregiver.principal(t, f)})
	wantCode(t, err, "owner_required")
	_, err = f.Service.RemoveMember(t.Context(), authority.RemoveMember{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account,
		AccountID: owner.account, At: f.Now, Principal: owner.principal(t, f)})
	wantCode(t, err, "owner_required")
	_, err = f.Service.RemoveMember(t.Context(), authority.RemoveMember{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account,
		AccountID: newMember(t, f).account, At: f.Now, Principal: owner.principal(t, f)})
	wantCode(t, err, "membership_revoked")

	remove := authority.RemoveMember{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account, AccountID: caregiver.account, At: f.Now,
		Principal: owner.principal(t, f)}
	removed, err := f.Service.RemoveMember(t.Context(), remove)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{caregiver.session, familySession} {
		if row, reason := sessionRow(t, f, id); row.Status != "revoked" || reason != "member_removed" || row.RevokedAt == nil || !row.RevokedAt.Equal(f.Now) {
			t.Fatalf("session %s after removal: %s %q", id, row.Status, reason)
		}
	}
	if slot, err := f.Service.FamilySlot(t.Context(), caregiver.account); err != nil || slot != nil {
		t.Fatalf("slot after removal %+v %v", slot, err)
	}
	if _, st := membershipOf(t, f, family, caregiver.account); st != "removed" {
		t.Fatalf("membership status %s", st)
	}
	if label, former := labelOf(t, f, family, caregiver.account); label != "Former caregiver" || !former {
		t.Fatalf("label after removal %q %v", label, former)
	}
	if rows, active := labelHistory(t, f, family, caregiver.account); rows != 2 || active != 0 {
		t.Fatalf("history rows=%d active=%d after removal, want 2/0", rows, active)
	}
	if row := lastAudit(t, f, authority.ScopeFamily, family); row.Kind != "member_removed" || row.Seq != removed.Revision {
		t.Fatalf("family audit %+v result %+v", row, removed)
	}
	if row := lastAudit(t, f, authority.ScopeAccount, caregiver.account); row.Kind != "member_removed" {
		t.Fatalf("target audit %+v", row)
	}
	if again, err := f.Service.RemoveMember(t.Context(), remove); err != nil || again != removed {
		t.Fatalf("replay %+v %v", again, err)
	}
	// Other members keep their sessions.
	if row, _ := sessionRow(t, f, viewer.session); row.Status != "active" {
		t.Fatalf("viewer session %s", row.Status)
	}

	// The removed caregiver can rejoin with a new invitation.
	back := invite(t, f, f.Service, owner, family, "viewer")
	rejoiner := caregiver
	rejoiner.session, _, rejoiner.refresh = f.Session(t, caregiver.account, caregiver.device, "account", "", "")
	if _, err := f.Service.RedeemInvitation(t.Context(), redeemOf(t, f, rejoiner, back.TokenDigest)); err != nil {
		t.Fatal(err)
	}
	if role, st := membershipOf(t, f, family, caregiver.account); role != "viewer" || st != "active" {
		t.Fatalf("rejoined membership %s/%s", role, st)
	}
	if label, former := labelOf(t, f, family, caregiver.account); label != "Fixture" || former {
		t.Fatalf("rejoined label %q %v", label, former)
	}

	// Leaving: the member's own command, same effects; the owner cannot leave.
	leave := authority.LeaveFamily{CommandID: authoritytest.NewID(t), AccountID: viewer.account, At: f.Now, Principal: viewer.principal(t, f)}
	if _, err := f.Service.LeaveFamily(t.Context(), leave); err != nil {
		t.Fatal(err)
	}
	if row, reason := sessionRow(t, f, viewer.session); row.Status != "revoked" || reason != "member_removed" {
		t.Fatalf("leaver session %s %q", row.Status, reason)
	}
	if row := lastAudit(t, f, authority.ScopeAccount, viewer.account); row.Kind != "member_left" {
		t.Fatalf("leaver audit %+v", row)
	}
	_, err = f.Service.LeaveFamily(t.Context(), authority.LeaveFamily{CommandID: authoritytest.NewID(t), AccountID: owner.account, At: f.Now, Principal: owner.principal(t, f)})
	wantCode(t, err, "owner_required")
}

func TestChangeRoleUpdatesLiveSessionRows(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	caregiver := joined(t, f, f.Service, owner, family, "caregiver")
	live1, _, _ := f.Session(t, caregiver.account, caregiver.device, "family", family, "caregiver")
	live2, _, _ := f.Session(t, caregiver.account, f.Device(t, caregiver.account), "family", family, "caregiver")
	dead, _, _ := f.Session(t, caregiver.account, caregiver.device, "family", family, "caregiver")
	f.Exec(t, `UPDATE sessions SET status='revoked', revoked_at=$2, revoke_reason='device_revoked' WHERE id=$1`, dead, f.Now)

	change := authority.ChangeRole{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account, AccountID: caregiver.account, Role: "viewer", At: f.Now,
		Principal: owner.principal(t, f)}
	result, err := f.Service.ChangeRole(t.Context(), change)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{live1, live2} {
		if row, _ := sessionRow(t, f, id); row.Role != "viewer" || row.Status != "active" {
			t.Fatalf("live session %s role %s", id, row.Role)
		}
	}
	if row, _ := sessionRow(t, f, dead); row.Role != "caregiver" {
		t.Fatalf("revoked session rewritten: %s", row.Role)
	}
	if row, _ := sessionRow(t, f, caregiver.session); row.Scope != "account" || row.Role != "" {
		t.Fatalf("account session %+v", row)
	}
	if role, _ := membershipOf(t, f, family, caregiver.account); role != "viewer" {
		t.Fatalf("membership role %s", role)
	}
	row := lastAudit(t, f, authority.ScopeFamily, family)
	if row.Kind != "member_role_changed" || row.Seq != result.Revision {
		t.Fatalf("family audit %+v", row)
	}
	if p := payloadOf(t, row); p["account_id"] != caregiver.account || p["role"] != "viewer" {
		t.Fatalf("payload %s", row.Payload)
	}
	if row := lastAudit(t, f, authority.ScopeAccount, caregiver.account); row.Kind != "member_role_changed" {
		t.Fatalf("target audit %+v", row)
	}
	if again, err := f.Service.ChangeRole(t.Context(), change); err != nil || again != result {
		t.Fatalf("replay %+v %v", again, err)
	}

	// The owner cannot change their own role or an owner's; non-owners cannot change roles.
	_, err = f.Service.ChangeRole(t.Context(), authority.ChangeRole{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account, AccountID: owner.account,
		Role: "viewer", At: f.Now, Principal: owner.principal(t, f)})
	wantCode(t, err, "owner_required")
	_, err = f.Service.ChangeRole(t.Context(), authority.ChangeRole{CommandID: authoritytest.NewID(t), OwnerAccountID: caregiver.account, AccountID: owner.account,
		Role: "viewer", At: f.Now, Principal: caregiver.principal(t, f)})
	wantCode(t, err, "owner_required")
	_, err = f.Service.ChangeRole(t.Context(), authority.ChangeRole{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account, AccountID: caregiver.account,
		Role: "owner", At: f.Now, Principal: owner.principal(t, f)})
	wantCode(t, err, "invalid_command")
}

func TestFamilyCapsAreCheckedInsideTransaction(t *testing.T) {
	f := authoritytest.New(t)
	s := serviceWith(t, f, func(o *authority.Options) { o.MaxCaregivers, o.MaxInvitations = 3, 6 })
	owner := newMember(t, f)
	family := createFamily(t, f, s, owner)
	var invitations []authority.CreateInvitation
	for range 6 {
		invitations = append(invitations, invite(t, f, s, owner, family, "caregiver"))
	}
	_, err := s.CreateInvitation(t.Context(), invitationOf(t, f, owner, family, "caregiver"))
	wantCode(t, err, "invitation_limit")

	// Six redeemers race for two free places (owner + 2 = 3).
	joiners := make([]member, 6)
	for i := range joiners {
		joiners[i] = newMember(t, f)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, len(joiners))
	for i := range joiners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := redeemOf(t, f, joiners[i], invitations[i].TokenDigest)
			<-start
			_, errs[i] = s.RedeemInvitation(context.Background(), in)
		}()
	}
	close(start)
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch authority.CodeOf(err) {
		case "":
			wins++
		case "caregiver_limit":
		default:
			t.Errorf("unexpected %v", err)
		}
	}
	if wins != 2 {
		t.Fatalf("%d joins, want 2", wins)
	}
	if n := count(t, f, `SELECT count(*) FROM memberships WHERE family_id=$1 AND status='active'`, family); n != 3 {
		t.Fatalf("%d active members", n)
	}
	// Expired invitations do not count against the invitation limit.
	f.Now = f.Now.Add(73 * time.Hour)
	if _, err := s.CreateInvitation(t.Context(), invitationOf(t, f, owner, family, "caregiver")); err != nil {
		t.Fatal(err)
	}
}

func TestHostedBootstrapEntitlementExpiryIsEventTimePlus366Days(t *testing.T) {
	f := authoritytest.New(t)
	f.Now = f.Now.Add(1234567 * time.Microsecond)
	hosted := serviceWith(t, f, func(o *authority.Options) { o.RequireFamilySyncEntitlement = true })
	owner := newMember(t, f)
	family := createFamily(t, f, hosted, owner)
	var source, status string
	var expires time.Time
	inAuthorityTxQuery(t, f, `SELECT source, status::text, expires_at FROM family_entitlements WHERE family_id=$1 AND entitlement_key='family_sync'`,
		[]any{family}, &source, &status, &expires)
	want := f.Now.Truncate(time.Second).Add(366 * 24 * time.Hour)
	if source != "hosted_beta" || status != "active" || !expires.Equal(want) {
		t.Fatalf("entitlement %s %s %v, want expiry %v", source, status, expires, want)
	}
	var required bool
	inAuthorityTxQuery(t, f, `SELECT entitlement_required FROM families WHERE id=$1`, []any{family}, &required)
	if !required {
		t.Fatal("hosted family does not require its entitlement")
	}
	row := lastAudit(t, f, authority.ScopeFamily, family)
	if row.Kind != "family_bootstrapped" || !row.At.Equal(f.Now.Truncate(time.Second)) {
		t.Fatalf("bootstrap audit %+v", row)
	}
	if row := lastAudit(t, f, authority.ScopeAccount, owner.account); row.Kind != "family_bootstrapped" {
		t.Fatalf("account audit %+v", row)
	}
}

func TestEntitlementExpiryDeniesNewCommandButReplayWins(t *testing.T) {
	f := authoritytest.New(t)
	hosted := serviceWith(t, f, func(o *authority.Options) { o.RequireFamilySyncEntitlement = true })
	owner := newMember(t, f)
	family := createFamily(t, f, hosted, owner)
	inv := invitationOf(t, f, owner, family, "caregiver")
	first, err := hosted.CreateInvitation(t.Context(), inv)
	if err != nil {
		t.Fatal(err)
	}
	// Expire the grant without passing the fixture sessions' deadline.
	f.Exec(t, `UPDATE family_entitlements SET expires_at=$2 WHERE family_id=$1`, family, f.Now.Add(time.Hour))
	f.Now = f.Now.Add(time.Hour)
	again, err := hosted.CreateInvitation(t.Context(), inv)
	if err != nil || again.Result != first.Result || !again.SecretUnavailable {
		t.Fatalf("replay after expiry %+v %v", again, err)
	}
	fresh := invitationOf(t, f, owner, family, "caregiver")
	_, err = hosted.CreateInvitation(t.Context(), fresh)
	wantCode(t, err, "entitlement_required")
	_, err = hosted.RedeemInvitation(t.Context(), redeemOf(t, f, newMember(t, f), inv.TokenDigest))
	wantCode(t, err, "entitlement_required")
	// The same family on a self-hosted node: the family row still requires it.
	_, err = f.Service.CreateInvitation(t.Context(), invitationOf(t, f, owner, family, "caregiver"))
	wantCode(t, err, "entitlement_required")
}

func TestRepairFamilyProfile(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	caregiver := joined(t, f, f.Service, owner, family, "caregiver")
	repair := authority.RepairFamilyProfile{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account, DisplayName: "Renamed", At: f.Now,
		Principal: owner.principal(t, f)}
	_, err := f.Service.RepairFamilyProfile(t.Context(), repair)
	wantCode(t, err, "profile_not_required")
	f.Exec(t, `UPDATE families SET display_name='', profile_required=true WHERE id=$1`, family)
	_, err = f.Service.RepairFamilyProfile(t.Context(), authority.RepairFamilyProfile{CommandID: authoritytest.NewID(t), OwnerAccountID: caregiver.account,
		DisplayName: "Mine", At: f.Now, Principal: caregiver.principal(t, f)})
	wantCode(t, err, "owner_required")
	result, err := f.Service.RepairFamilyProfile(t.Context(), repair)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	var required bool
	inAuthorityTxQuery(t, f, `SELECT display_name, profile_required FROM families WHERE id=$1`, []any{family}, &name, &required)
	if name != "Renamed" || required {
		t.Fatalf("family after repair %q %v", name, required)
	}
	if row := lastAudit(t, f, authority.ScopeFamily, family); row.Kind != "family_profile_repaired" || row.Seq != result.Revision {
		t.Fatalf("audit %+v", row)
	}
	if again, err := f.Service.RepairFamilyProfile(t.Context(), repair); err != nil || again != result {
		t.Fatalf("replay %+v %v", again, err)
	}
	_, err = f.Service.RepairFamilyProfile(t.Context(), authority.RepairFamilyProfile{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account,
		DisplayName: " padded ", At: f.Now, Principal: owner.principal(t, f)})
	wantCode(t, err, "invalid_command")
}

func TestRevokeInvitation(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	inv := invite(t, f, f.Service, owner, family, "caregiver")
	revoke := authority.RevokeInvitation{CommandID: authoritytest.NewID(t), ActorAccountID: owner.account, InvitationID: inv.InvitationID, At: f.Now,
		Principal: owner.principal(t, f)}
	result, err := f.Service.RevokeInvitation(t.Context(), revoke)
	if err != nil {
		t.Fatal(err)
	}
	if view, _, _ := f.Service.Invitation(t.Context(), family, inv.InvitationID); view.Status != "revoked" {
		t.Fatalf("invitation %+v", view)
	}
	if row := lastAudit(t, f, authority.ScopeFamily, family); row.Kind != "invitation_revoked" || row.Seq != result.Revision {
		t.Fatalf("audit %+v", row)
	}
	if again, err := f.Service.RevokeInvitation(t.Context(), revoke); err != nil || again != result {
		t.Fatalf("replay %+v %v", again, err)
	}
	_, err = f.Service.RevokeInvitation(t.Context(), authority.RevokeInvitation{CommandID: authoritytest.NewID(t), ActorAccountID: owner.account,
		InvitationID: inv.InvitationID, At: f.Now, Principal: owner.principal(t, f)})
	wantCode(t, err, "invitation_revoked")

	redeemed := invite(t, f, f.Service, owner, family, "caregiver")
	caregiver := newMember(t, f)
	if _, err := f.Service.RedeemInvitation(t.Context(), redeemOf(t, f, caregiver, redeemed.TokenDigest)); err != nil {
		t.Fatal(err)
	}
	_, err = f.Service.RevokeInvitation(t.Context(), authority.RevokeInvitation{CommandID: authoritytest.NewID(t), ActorAccountID: owner.account,
		InvitationID: redeemed.InvitationID, At: f.Now, Principal: owner.principal(t, f)})
	wantCode(t, err, "invitation_consumed")
	pending := invite(t, f, f.Service, owner, family, "caregiver")
	_, err = f.Service.RevokeInvitation(t.Context(), authority.RevokeInvitation{CommandID: authoritytest.NewID(t), ActorAccountID: caregiver.account,
		InvitationID: pending.InvitationID, At: f.Now, Principal: caregiver.principal(t, f)})
	wantCode(t, err, "owner_required")

	// Another family's invitation is invisible.
	stranger := newMember(t, f)
	strangerFamily := createFamily(t, f, f.Service, stranger)
	_, err = f.Service.RevokeInvitation(t.Context(), authority.RevokeInvitation{CommandID: authoritytest.NewID(t), ActorAccountID: stranger.account,
		InvitationID: pending.InvitationID, At: f.Now, Principal: stranger.principal(t, f)})
	wantCode(t, err, "invitation_invalid")
	if _, found, err := f.Service.Invitation(t.Context(), strangerFamily, pending.InvitationID); err != nil || found {
		t.Fatalf("cross-family invitation read found=%v err=%v", found, err)
	}
}

// Family commands re-check the caller's session inside their transaction: a
// revoked session is refused, but an exact retry still returns its receipt.
func TestFamilyCommandsRequireALiveSessionButReplayWins(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	inv := invitationOf(t, f, owner, family, "caregiver")
	first, err := f.Service.CreateInvitation(t.Context(), inv)
	if err != nil {
		t.Fatal(err)
	}
	f.Exec(t, `UPDATE sessions SET status='revoked', revoked_at=$2, revoke_reason='device_revoked' WHERE id=$1`, owner.session, f.Now)
	if again, err := f.Service.CreateInvitation(t.Context(), inv); err != nil || again.Result != first.Result {
		t.Fatalf("replay with a revoked session %+v %v", again, err)
	}
	_, err = f.Service.CreateInvitation(t.Context(), invitationOf(t, f, owner, family, "caregiver"))
	wantCode(t, err, "session_revoked")
	// A principal that is not the actor is malformed.
	stranger := newMember(t, f)
	_, err = f.Service.LeaveFamily(t.Context(), authority.LeaveFamily{CommandID: authoritytest.NewID(t), AccountID: owner.account, At: f.Now,
		Principal: stranger.principal(t, f)})
	wantCode(t, err, "invalid_command")
}

// Every command that revokes or re-roles a member's sessions locks that
// member's account first, so a concurrent refresh of the member either
// commits before the command (and its child is updated by it) or runs after
// it (and sees the new membership). No child escapes with a stale role or
// scope, and no session survives a removal.
func TestFamilyCommandsRaceRefreshOfAffectedMember(t *testing.T) {
	type outcome struct {
		revoked bool
		role    string
	}
	type run func(f *authoritytest.Fixture, owner, target member, ownerP, targetP access.Principal) (outcome, error)
	cases := map[string]run{
		"remove": func(f *authoritytest.Fixture, owner, target member, ownerP, targetP access.Principal) (outcome, error) {
			_, err := f.Service.RemoveMember(context.Background(), authority.RemoveMember{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account,
				AccountID: target.account, At: f.Now, Principal: ownerP})
			return outcome{revoked: true}, err
		},
		"leave": func(f *authoritytest.Fixture, owner, target member, ownerP, targetP access.Principal) (outcome, error) {
			_, err := f.Service.LeaveFamily(context.Background(), authority.LeaveFamily{CommandID: authoritytest.NewID(t), AccountID: target.account, At: f.Now,
				Principal: targetP})
			return outcome{revoked: true}, err
		},
		"change_role": func(f *authoritytest.Fixture, owner, target member, ownerP, targetP access.Principal) (outcome, error) {
			_, err := f.Service.ChangeRole(context.Background(), authority.ChangeRole{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account,
				AccountID: target.account, Role: "viewer", At: f.Now, Principal: ownerP})
			return outcome{role: "viewer"}, err
		},
		"transfer_owner": func(f *authoritytest.Fixture, owner, target member, ownerP, targetP access.Principal) (outcome, error) {
			_, err := f.Service.TransferOwner(context.Background(), authority.TransferOwner{CommandID: authoritytest.NewID(t), OldOwnerID: owner.account,
				NewOwnerID: target.account, At: f.Now, Principal: ownerP})
			return outcome{role: "owner"}, err
		},
		// Operator owner recovery (Task 6) changes the successor's session
		// roles: it must hold the successor's account lock too.
		"operator_owner_recovery": func(f *authoritytest.Fixture, owner, target member, ownerP, targetP access.Principal) (outcome, error) {
			ctx := context.Background()
			slot, err := f.Service.FamilySlot(ctx, target.account)
			if err != nil || slot == nil {
				return outcome{}, fmt.Errorf("target slot %v: %w", slot, err)
			}
			// The owner's identity is gone, as MarkIdentityUnavailable leaves it.
			tx, err := f.Service.Begin(ctx)
			if err != nil {
				return outcome{}, err
			}
			defer tx.Rollback(ctx)
			var revision uint64
			if _, err := tx.Exec(ctx, `UPDATE accounts SET status='identity_unavailable' WHERE id=$1`, owner.account); err != nil {
				return outcome{}, err
			}
			if err := tx.QueryRow(ctx, `SELECT audit_seq FROM families WHERE id=$1`, slot.FamilyID).Scan(&revision); err != nil {
				return outcome{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return outcome{}, err
			}
			_, err = f.Service.ExecuteOperatorRecoverOwner(ctx, authority.OperatorRecoverOwner{CommandID: authoritytest.NewID(t),
				OperatorID: authoritytest.NewID(t), FamilyID: slot.FamilyID, OldOwnerAccountID: owner.account, NewOwnerAccountID: target.account,
				PolicyID: "ownership-policy-v1", EvidenceDigest: operatorEvidence, ReasonCode: "owner_identity_unavailable",
				ExpectedRevision: revision, ConfirmationExpiresAt: f.Now.Add(time.Minute).Truncate(time.Second)})
			return outcome{role: "owner"}, err
		},
	}
	for name, command := range cases {
		t.Run(name, func(t *testing.T) {
			f := authoritytest.New(t)
			for round := range 12 {
				owner := newMember(t, f)
				family := createFamily(t, f, f.Service, owner)
				target := joined(t, f, f.Service, owner, family, "caregiver")
				// The target's devices hold family sessions that refresh once
				// while the command runs (a second refresh would read the new
				// membership and hide an escaped child).
				var raws []string
				for range 6 {
					device := f.Device(t, target.account)
					_, _, raw := f.Session(t, target.account, device, "family", family, "caregiver")
					raws = append(raws, raw)
				}
				ownerP, targetP := owner.principal(t, f), target.principal(t, f)
				start := make(chan struct{})
				var wg sync.WaitGroup
				errs := make(chan error, 32)
				for _, raw := range raws {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						if _, err := f.Service.Refresh(context.Background(), refreshOf(f, raw, "", nil)); err != nil {
							errs <- err
						}
					}()
				}
				var want outcome
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if round%3 != 0 {
						time.Sleep(time.Duration(round%3) * time.Millisecond)
					}
					var err error
					want, err = command(f, owner, target, ownerP, targetP)
					if err != nil {
						errs <- fmt.Errorf("command: %w", err)
					}
				}()
				close(start)
				wg.Wait()
				close(errs)
				for err := range errs {
					if strings.Contains(err.Error(), "command:") || authority.CodeOf(err) != "session_revoked" {
						t.Fatalf("round %d: %v", round, err)
					}
				}
				if want.revoked {
					if n := count(t, f, `SELECT count(*) FROM sessions WHERE account_id=$1 AND status='active'`, target.account); n != 0 {
						t.Fatalf("round %d: %d sessions escaped the revocation", round, n)
					}
					continue
				}
				if n := count(t, f, `SELECT count(*) FROM sessions WHERE account_id=$1 AND status='active' AND scope='family' AND (family_id<>$2 OR role<>$3::text::app.member_role)`,
					target.account, family, want.role); n != 0 {
					t.Fatalf("round %d: %d live sessions with a stale role", round, n)
				}
				if n := count(t, f, `SELECT count(*) FROM sessions WHERE account_id=$1 AND status='active' AND scope='family'`, target.account); n != 6 {
					t.Fatalf("round %d: %d live family sessions, want 6", round, n)
				}
			}
		})
	}
}
