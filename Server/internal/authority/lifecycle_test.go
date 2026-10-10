package authority_test

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
	"github.com/jackc/pgx/v5"
)

const purgeDelay = 30 * 24 * time.Hour

// keyedMember is a member whose identity key is known (account deletion
// names it).
func keyedMember(t *testing.T, f *authoritytest.Fixture) (member, string) {
	t.Helper()
	m := member{account: f.Account(t)}
	key := seedIdentity(t, f, m.account, 1)
	m.device = f.Device(t, m.account)
	m.session, _, m.refresh = f.Session(t, m.account, m.device, "account", "", "")
	return m, key
}

// freshProof is a new device and session of account after a refresh, as a
// fresh sign-in followed by a refresh leaves it: its scope is what the
// account's slot grants now.
func freshProof(t *testing.T, f *authoritytest.Fixture, account string) access.Principal {
	t.Helper()
	device := f.Device(t, account)
	_, _, raw := f.Session(t, account, device, "account", "", "")
	res, err := f.Service.Refresh(t.Context(), refreshOf(f, raw, "", nil))
	if err != nil {
		t.Fatalf("fresh proof refresh: %v", err)
	}
	return principalOf(f, t, res.Child.ID)
}

func scheduleOf(t *testing.T, f *authoritytest.Fixture, owner string, p access.Principal) authority.ScheduleDeletion {
	t.Helper()
	return authority.ScheduleDeletion{CommandID: authoritytest.NewID(t), OwnerAccountID: owner, At: f.Now, Principal: p}
}

func familyState(t *testing.T, f *authoritytest.Fixture, family string) (status, owner string, purgeAfter *time.Time) {
	t.Helper()
	inAuthorityTxQuery(t, f, `SELECT status::text, coalesce(owner_account_id::text,''), purge_after FROM families WHERE id=$1`, []any{family},
		&status, &owner, &purgeAfter)
	return status, owner, purgeAfter
}

func claimOf(t *testing.T, f *authoritytest.Fixture, family, account string) string {
	t.Helper()
	var status string
	inAuthorityTxQuery(t, f, `SELECT coalesce((SELECT status::text FROM family_recovery_claims WHERE family_id=$1 AND account_id=$2),'')`,
		[]any{family, account}, &status)
	return status
}

func slotOf(t *testing.T, f *authoritytest.Fixture, account string) *authority.FamilySlot {
	t.Helper()
	slot, err := f.Service.FamilySlot(t.Context(), account)
	if err != nil {
		t.Fatal(err)
	}
	return slot
}

func TestScheduleReservesOnlyOwnerRecoverySlot(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	caregiver := joined(t, f, f.Service, owner, family, "caregiver")
	viewer := joined(t, f, f.Service, owner, family, "viewer")
	pending := invite(t, f, f.Service, owner, family, "caregiver")
	// Every member also holds a family session on a second device.
	for _, m := range []member{owner, caregiver, viewer} {
		role, _ := membershipOf(t, f, family, m.account)
		f.Session(t, m.account, f.Device(t, m.account), "family", family, role)
	}

	_, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, caregiver.account, caregiver.principal(t, f)))
	wantCode(t, err, "owner_required")
	stranger := newMember(t, f)
	_, err = f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, stranger.account, stranger.principal(t, f)))
	wantCode(t, err, "family_unavailable")

	in := scheduleOf(t, f, owner.account, owner.principal(t, f))
	res, err := f.Service.ScheduleDeletion(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	purgeAfter := f.Now.Add(purgeDelay)
	familySeq, _ := chainHead(t, f, authority.ScopeFamily, family)
	if res.FamilyID != family || !res.PurgeAfter.Equal(purgeAfter) || res.CommandID != in.CommandID || res.Outcome != "committed" || res.Revision != familySeq {
		t.Fatalf("schedule result %+v (family seq %d)", res, familySeq)
	}
	status, ownerID, after := familyState(t, f, family)
	if status != "deletion_pending" || ownerID != owner.account || after == nil || !after.Equal(purgeAfter) {
		t.Fatalf("family %s owner %s purge_after %v", status, ownerID, after)
	}
	for _, m := range []member{owner, caregiver, viewer} {
		if _, st := membershipOf(t, f, family, m.account); st != "former" {
			t.Fatalf("membership of %s is %s, want former", m.account, st)
		}
		if n := count(t, f, `SELECT count(*) FROM sessions WHERE account_id=$1 AND status='active'`, m.account); n != 0 {
			t.Fatalf("%d live sessions of %s", n, m.account)
		}
		if n := count(t, f, `SELECT count(*) FROM sessions WHERE account_id=$1 AND revoke_reason<>'family_deleted'`, m.account); n != 0 {
			t.Fatalf("%d sessions of %s revoked for another reason", n, m.account)
		}
		if label, former := labelOf(t, f, family, m.account); label != "Former caregiver" || !former {
			t.Fatalf("label of %s %q former=%v", m.account, label, former)
		}
		if row := lastAudit(t, f, authority.ScopeAccount, m.account); row.Kind != "family_deletion_scheduled" || row.ActorID != owner.account ||
			row.CommandID != in.CommandID {
			t.Fatalf("account audit %+v", row)
		}
	}
	if slot := slotOf(t, f, owner.account); slot == nil || slot.FamilyID != family || slot.State != "family_recovery" {
		t.Fatalf("owner slot %+v", slot)
	}
	for _, m := range []member{caregiver, viewer} {
		if slot := slotOf(t, f, m.account); slot != nil {
			t.Fatalf("former member %s kept slot %+v", m.account, slot)
		}
	}
	if claim := claimOf(t, f, family, owner.account); claim != "active" {
		t.Fatalf("owner claim %q", claim)
	}
	if n := count(t, f, `SELECT count(*) FROM family_recovery_claims WHERE family_id=$1`, family); n != 1 {
		t.Fatalf("%d claims, want only the owner's", n)
	}
	if view, ok, err := f.Service.Invitation(t.Context(), family, pending.InvitationID); err != nil || !ok || view.Status != "revoked" {
		t.Fatalf("pending invitation %+v %v %v", view, ok, err)
	}
	row := lastAudit(t, f, authority.ScopeFamily, family)
	if row.Kind != "family_deletion_scheduled" || row.Seq != familySeq {
		t.Fatalf("family audit %+v", row)
	}
	if p := payloadOf(t, row); p["family_id"] != family || p["purge_after"] != purgeAfter.Format(time.RFC3339Nano) {
		t.Fatalf("family audit payload %v", p)
	}

	// Only the owner may recover: its fresh proof is family_recovery scoped.
	status2, err := f.Service.RecoveryStatus(t.Context(), owner.account)
	if err != nil || status2.FamilyID != family || status2.State != "deletion_pending" || !status2.PurgeAfter.Equal(purgeAfter) || !status2.CanRecover {
		t.Fatalf("owner recovery status %+v %v", status2, err)
	}
	_, err = f.Service.RecoveryStatus(t.Context(), caregiver.account)
	wantCode(t, err, "recovery_only")
	proof := freshProof(t, f, owner.account)
	if proof.Scope != "family_recovery" || proof.SessionFamilyID != family {
		t.Fatalf("owner fresh proof %+v", proof)
	}
	if caregiverProof := freshProof(t, f, caregiver.account); caregiverProof.Scope != "account" {
		t.Fatalf("caregiver fresh proof %+v", caregiverProof)
	}
	// The family is no longer active: a second schedule has no family.
	_, err = f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, proof))
	wantCode(t, err, "family_unavailable")
}

func TestRecoveryAtDeadlineDeniedAndForfeitIsExclusive(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	caregiver := joined(t, f, f.Service, owner, family, "caregiver")
	start := f.Now
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
		t.Fatal(err)
	}
	proof := freshProof(t, f, owner.account)
	// An account-scope principal of the owner is not a recovery principal,
	// nor is a former caregiver's.
	accountScope := principalOf(f, t, func() string {
		id, _, _ := f.Session(t, owner.account, f.Device(t, owner.account), "account", "", "")
		return id
	}())
	_, err := f.Service.RecoverFamily(t.Context(), authority.RecoverFamily{CommandID: authoritytest.NewID(t), AccountID: owner.account, At: f.Now, Principal: accountScope})
	wantCode(t, err, "recovery_only")
	caregiverProof := freshProof(t, f, caregiver.account)
	_, err = f.Service.ForfeitRecovery(t.Context(), authority.ForfeitRecovery{CommandID: authoritytest.NewID(t), AccountID: caregiver.account, At: f.Now,
		Principal: caregiverProof})
	wantCode(t, err, "recovery_only")

	// At the deadline recovery is expired; forfeiting has no deadline.
	f.Now = start.Add(purgeDelay)
	_, err = f.Service.RecoverFamily(t.Context(), authority.RecoverFamily{CommandID: authoritytest.NewID(t), AccountID: owner.account, At: f.Now, Principal: proof})
	wantCode(t, err, "recovery_expired")
	if status, err := f.Service.RecoveryStatus(t.Context(), owner.account); err != nil || status.CanRecover {
		t.Fatalf("status at the deadline %+v %v", status, err)
	}
	forfeit := authority.ForfeitRecovery{CommandID: authoritytest.NewID(t), AccountID: owner.account, At: f.Now, Principal: proof}
	if _, err := f.Service.ForfeitRecovery(t.Context(), forfeit); err != nil {
		t.Fatal(err)
	}
	if claim := claimOf(t, f, family, owner.account); claim != "forfeited" {
		t.Fatalf("claim %q", claim)
	}
	if slot := slotOf(t, f, owner.account); slot != nil {
		t.Fatalf("forfeited owner kept slot %+v", slot)
	}
	if row := lastAudit(t, f, authority.ScopeFamily, family); row.Kind != "recovery_forfeited" || row.ActorID != owner.account {
		t.Fatalf("family audit %+v", row)
	}
	if row := lastAudit(t, f, authority.ScopeAccount, owner.account); row.Kind != "recovery_forfeited" {
		t.Fatalf("account audit %+v", row)
	}
	_, err = f.Service.RecoverFamily(t.Context(), authority.RecoverFamily{CommandID: authoritytest.NewID(t), AccountID: owner.account,
		At: start.Add(29 * 24 * time.Hour), Principal: proof})
	wantCode(t, err, "recovery_forfeited")
	_, err = f.Service.RecoveryStatus(t.Context(), owner.account)
	wantCode(t, err, "recovery_only")

	// Recover and forfeit race for one claim: exactly one wins.
	outcomes := map[string]int{}
	for round := range 8 {
		f.Now = start
		owner := newMember(t, f)
		family := createFamily(t, f, f.Service, owner)
		caregiver := joined(t, f, f.Service, owner, family, "caregiver")
		if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
			t.Fatal(err)
		}
		proof := freshProof(t, f, owner.account)
		at := start.Add(24 * time.Hour)
		var recoverErr, forfeitErr error
		var wg sync.WaitGroup
		gate := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-gate
			_, recoverErr = f.Service.RecoverFamily(context.Background(), authority.RecoverFamily{CommandID: authoritytest.NewID(t), AccountID: owner.account,
				At: at, Principal: proof})
		}()
		go func() {
			defer wg.Done()
			<-gate
			if round%2 == 1 {
				time.Sleep(time.Millisecond)
			}
			_, forfeitErr = f.Service.ForfeitRecovery(context.Background(), authority.ForfeitRecovery{CommandID: authoritytest.NewID(t), AccountID: owner.account,
				At: at, Principal: proof})
		}()
		close(gate)
		wg.Wait()
		switch {
		case recoverErr == nil && authority.CodeOf(forfeitErr) == "family_unavailable":
			outcomes["recovered"]++
			status, ownerID, after := familyState(t, f, family)
			if status != "active" || ownerID != owner.account || after != nil {
				t.Fatalf("round %d: recovered family %s %s %v", round, status, ownerID, after)
			}
			if slot := slotOf(t, f, owner.account); slot == nil || slot.State != "active" || slot.FamilyID != family {
				t.Fatalf("round %d: recovered slot %+v", round, slot)
			}
			if role, st := membershipOf(t, f, family, owner.account); role != "owner" || st != "active" {
				t.Fatalf("round %d: recovered membership %s/%s", round, role, st)
			}
			if _, st := membershipOf(t, f, family, caregiver.account); st != "former" {
				t.Fatalf("round %d: caregiver %s after recovery", round, st)
			}
			if claim := claimOf(t, f, family, owner.account); claim != "recovered" {
				t.Fatalf("round %d: claim %q", round, claim)
			}
			if label, former := labelOf(t, f, family, owner.account); label != "Fixture" || former {
				t.Fatalf("round %d: owner label %q former=%v", round, label, former)
			}
			if row := lastAudit(t, f, authority.ScopeFamily, family); row.Kind != "family_recovered" {
				t.Fatalf("round %d: audit %+v", round, row)
			}
		case forfeitErr == nil && authority.CodeOf(recoverErr) == "recovery_forfeited":
			outcomes["forfeited"]++
			if status, _, _ := familyState(t, f, family); status != "deletion_pending" {
				t.Fatalf("round %d: forfeited family %s", round, status)
			}
			if claim := claimOf(t, f, family, owner.account); claim != "forfeited" {
				t.Fatalf("round %d: claim %q", round, claim)
			}
		default:
			t.Fatalf("round %d: recover %v, forfeit %v", round, recoverErr, forfeitErr)
		}
	}
	t.Logf("outcomes %v", outcomes)
}

func TestAccountDeletionRejectsActiveOwnerAndForfeitsClaim(t *testing.T) {
	f := authoritytest.New(t)
	owner, ownerKey := keyedMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	caregiver, caregiverKey := keyedMember(t, f)
	in := invite(t, f, f.Service, owner, family, "caregiver")
	if _, err := f.Service.RedeemInvitation(t.Context(), redeemOf(t, f, caregiver, in.TokenDigest)); err != nil {
		t.Fatal(err)
	}

	remove := authority.DeleteAccount{CommandID: authoritytest.NewID(t), AccountID: owner.account, IdentityKey: ownerKey, At: f.Now, Principal: owner.principal(t, f)}
	_, err := f.Service.DeleteAccount(t.Context(), remove)
	wantCode(t, err, "ownership_transfer_required")
	wrongKey := remove
	wrongKey.CommandID, wrongKey.IdentityKey = authoritytest.NewID(t), identityKey(t, "identity.v1")
	_, err = f.Service.DeleteAccount(t.Context(), wrongKey)
	wantCode(t, err, "session_revoked")

	// A caregiver deletes their account: the family keeps going without them.
	caregiverDelete := authority.DeleteAccount{CommandID: authoritytest.NewID(t), AccountID: caregiver.account, IdentityKey: caregiverKey, At: f.Now,
		Principal: caregiver.principal(t, f)}
	res, err := f.Service.DeleteAccount(t.Context(), caregiverDelete)
	if err != nil {
		t.Fatal(err)
	}
	accountSeq, _ := chainHead(t, f, authority.ScopeAccount, caregiver.account)
	if res.Revision != accountSeq || res.CommandID != caregiverDelete.CommandID {
		t.Fatalf("delete result %+v (account seq %d)", res, accountSeq)
	}
	if _, st := membershipOf(t, f, family, caregiver.account); st != "removed" {
		t.Fatalf("caregiver membership %s", st)
	}
	if slot := slotOf(t, f, caregiver.account); slot != nil {
		t.Fatalf("deleted caregiver kept slot %+v", slot)
	}
	if label, former := labelOf(t, f, family, caregiver.account); label != "Former caregiver" || !former {
		t.Fatalf("deleted caregiver label %q %v", label, former)
	}
	if row := lastAudit(t, f, authority.ScopeFamily, family); row.Kind != "account_deleted" || row.ActorID != caregiver.account {
		t.Fatalf("family audit %+v", row)
	}
	if status, ownerID, _ := familyState(t, f, family); status != "active" || ownerID != owner.account {
		t.Fatalf("family after caregiver deletion %s %s", status, ownerID)
	}

	// The owner schedules deletion, proves again and deletes the account
	// under the command ID that was refused before (refusals keep no receipt).
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
		t.Fatal(err)
	}
	proof := freshProof(t, f, owner.account)
	remove.Principal = proof
	res, err = f.Service.DeleteAccount(t.Context(), remove)
	if err != nil {
		t.Fatal(err)
	}
	var status, name string
	var live bool
	inAuthorityTxQuery(t, f, `SELECT a.status::text, a.display_name, i.live FROM accounts a JOIN account_identities i ON i.account_id=a.id WHERE a.id=$1`,
		[]any{owner.account}, &status, &name, &live)
	if status != "deleted" || name != "" || live {
		t.Fatalf("deleted account %s %q live=%v", status, name, live)
	}
	if claim := claimOf(t, f, family, owner.account); claim != "forfeited" {
		t.Fatalf("claim %q", claim)
	}
	if _, st := membershipOf(t, f, family, owner.account); st != "removed" {
		t.Fatalf("owner membership %s", st)
	}
	if slot := slotOf(t, f, owner.account); slot != nil {
		t.Fatalf("deleted owner kept slot %+v", slot)
	}
	if n := count(t, f, `SELECT count(*) FROM devices WHERE account_id=$1 AND status='active'`, owner.account); n != 0 {
		t.Fatalf("%d active devices", n)
	}
	if n := count(t, f, `SELECT count(*) FROM sessions WHERE account_id=$1 AND status='active'`, owner.account); n != 0 {
		t.Fatalf("%d live sessions", n)
	}
	if n := count(t, f, `SELECT count(*) FROM sessions WHERE account_id=$1 AND revoke_reason='account_deleted'`, owner.account); n == 0 {
		t.Fatal("the fresh proof was not revoked as account_deleted")
	}
	if row := lastAudit(t, f, authority.ScopeAccount, owner.account); row.Kind != "account_deleted" || row.Seq != res.Revision {
		t.Fatalf("account audit %+v", row)
	}
	if row := lastAudit(t, f, authority.ScopeFamily, family); row.Kind != "account_deleted" || row.ActorID != owner.account {
		t.Fatalf("family audit %+v", row)
	}
	resolution, err := f.Service.ResolveIdentity(t.Context(), []string{ownerKey})
	if err != nil || resolution.Status != "deleted" || resolution.NextActorGeneration != 2 {
		t.Fatalf("resolution %+v %v", resolution, err)
	}
	// The exact retry returns the receipt although the account is gone.
	if again, err := f.Service.DeleteAccount(t.Context(), remove); err != nil || again != res {
		t.Fatalf("replay %+v %v, want %+v", again, err, res)
	}
}

func TestPurgeStartRequiresDueDeadline(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	start := f.Now
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
		t.Fatal(err)
	}
	other := newMember(t, f)
	otherFamily := createFamily(t, f, f.Service, other)
	retention, err := authority.RetentionWorkerActorID(f.InstanceID)
	if err != nil {
		t.Fatal(err)
	}

	early := authority.StartFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: family, At: start.Add(purgeDelay - time.Microsecond)}
	_, err = f.Service.StartFamilyPurge(t.Context(), early)
	wantCode(t, err, "invalid_command")
	_, err = f.Service.StartFamilyPurge(t.Context(), authority.StartFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: otherFamily, At: start.Add(purgeDelay)})
	wantCode(t, err, "family_unavailable")

	due := authority.StartFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: family, At: start.Add(purgeDelay)}
	started, err := f.Service.StartFamilyPurge(t.Context(), due)
	if err != nil {
		t.Fatal(err)
	}
	var status, purgeCommand string
	var startedAt *time.Time
	inAuthorityTxQuery(t, f, `SELECT status::text, coalesce(purge_command_id::text,''), purge_started_at FROM families WHERE id=$1`, []any{family},
		&status, &purgeCommand, &startedAt)
	if status != "purge_started" || purgeCommand != due.CommandID || startedAt == nil || !startedAt.Equal(due.At) {
		t.Fatalf("purge start %s %s %v", status, purgeCommand, startedAt)
	}
	if slot := slotOf(t, f, owner.account); slot != nil {
		t.Fatalf("owner kept slot after purge start %+v", slot)
	}
	row := lastAudit(t, f, authority.ScopeFamily, family)
	if row.Kind != "family_purge_started" || row.ActorID != retention || row.CommandID != due.CommandID || row.Seq != started.Revision {
		t.Fatalf("purge start audit %+v (retention actor %s)", row, retention)
	}
	if again, err := f.Service.StartFamilyPurge(t.Context(), due); err != nil || again != started {
		t.Fatalf("start replay %+v %v", again, err)
	}
	// Recovery is impossible once the purge started.
	_, err = f.Service.RecoveryStatus(t.Context(), owner.account)
	wantCode(t, err, "recovery_only")

	receipt := authoritytest.NewID(t)
	wrong := authority.CompleteFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: family, StartCommandID: authoritytest.NewID(t), Receipt: receipt,
		At: start.Add(purgeDelay + time.Hour)}
	_, err = f.Service.CompleteFamilyPurge(t.Context(), wrong)
	wantCode(t, err, "invalid_command")
	complete := authority.CompleteFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: family, StartCommandID: due.CommandID, Receipt: receipt,
		At: start.Add(purgeDelay + time.Hour)}
	done, err := f.Service.CompleteFamilyPurge(t.Context(), complete)
	if err != nil {
		t.Fatal(err)
	}
	var storedReceipt string
	var purgedAt *time.Time
	inAuthorityTxQuery(t, f, `SELECT status::text, coalesce(purge_receipt::text,''), purged_at FROM families WHERE id=$1`, []any{family},
		&status, &storedReceipt, &purgedAt)
	if status != "purge_completed" || storedReceipt != receipt || purgedAt == nil || !purgedAt.Equal(complete.At) {
		t.Fatalf("purge complete %s %s %v", status, storedReceipt, purgedAt)
	}
	if claim := claimOf(t, f, family, owner.account); claim != "forfeited" {
		t.Fatalf("claim after purge %q", claim)
	}
	if row := lastAudit(t, f, authority.ScopeFamily, family); row.Kind != "family_purge_completed" || row.ActorID != retention || row.Seq != done.Revision {
		t.Fatalf("purge complete audit %+v", row)
	}
	if again, err := f.Service.CompleteFamilyPurge(t.Context(), complete); err != nil || again != done {
		t.Fatalf("complete replay %+v %v", again, err)
	}
	_, err = f.Service.CompleteFamilyPurge(t.Context(), authority.CompleteFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: family,
		StartCommandID: due.CommandID, Receipt: receipt, At: complete.At})
	wantCode(t, err, "family_unavailable")
}

// A lifecycle command's exact retry returns its receipt even when the
// original session was revoked by the command and the retry carries a fresh
// proof; a different command under the same ID is refused.
func TestLifecycleReceiptsReplayAcrossFreshProof(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	joined(t, f, f.Service, owner, family, "caregiver")
	schedule := scheduleOf(t, f, owner.account, owner.principal(t, f))
	first, err := f.Service.ScheduleDeletion(t.Context(), schedule)
	if err != nil {
		t.Fatal(err)
	}
	familyRows := len(readAudit(t, f, authority.ScopeFamily, family))
	f.Now = f.Now.Add(time.Hour)
	proof := freshProof(t, f, owner.account)
	retry := schedule
	retry.Principal, retry.At = proof, f.Now
	again, err := f.Service.ScheduleDeletion(t.Context(), retry)
	if err != nil || again != first {
		t.Fatalf("schedule replay %+v %v, want %+v", again, err, first)
	}
	_, err = f.Service.RecoverFamily(t.Context(), authority.RecoverFamily{CommandID: schedule.CommandID, AccountID: owner.account, At: f.Now, Principal: proof})
	wantCode(t, err, "command_id_reused")

	recover := authority.RecoverFamily{CommandID: authoritytest.NewID(t), AccountID: owner.account, At: f.Now, Principal: proof}
	recovered, err := f.Service.RecoverFamily(t.Context(), recover)
	if err != nil {
		t.Fatal(err)
	}
	f.Now = f.Now.Add(time.Hour)
	recover.Principal, recover.At = freshProof(t, f, owner.account), f.Now
	if again, err := f.Service.RecoverFamily(t.Context(), recover); err != nil || again != recovered {
		t.Fatalf("recover replay %+v %v, want %+v", again, err, recovered)
	}
	_, err = f.Service.ForfeitRecovery(t.Context(), authority.ForfeitRecovery{CommandID: recover.CommandID, AccountID: owner.account, At: f.Now,
		Principal: recover.Principal})
	wantCode(t, err, "command_id_reused")
	// A schedule retried after the family was recovered still returns its
	// receipt, not a second deletion.
	if again, err := f.Service.ScheduleDeletion(t.Context(), retry); err != nil || again != first {
		t.Fatalf("schedule replay after recovery %+v %v", again, err)
	}
	if rows := len(readAudit(t, f, authority.ScopeFamily, family)); rows != familyRows+1 {
		t.Fatalf("%d family audit rows, want %d", rows, familyRows+1)
	}
}

// ScheduleDeletion and DeleteAccount lock every affected member's account
// before they revoke its sessions, so a concurrent refresh either commits
// first (and its child is revoked) or runs after (and is refused).
func TestLifecycleCommandsRaceRefreshOfAffectedMember(t *testing.T) {
	type run func(f *authoritytest.Fixture, owner, target member, targetKey string, ownerP, targetP access.Principal) error
	cases := map[string]run{
		"schedule_deletion": func(f *authoritytest.Fixture, owner, target member, targetKey string, ownerP, targetP access.Principal) error {
			_, err := f.Service.ScheduleDeletion(context.Background(), authority.ScheduleDeletion{CommandID: authoritytest.NewID(t),
				OwnerAccountID: owner.account, At: f.Now, Principal: ownerP})
			return err
		},
		"delete_account": func(f *authoritytest.Fixture, owner, target member, targetKey string, ownerP, targetP access.Principal) error {
			_, err := f.Service.DeleteAccount(context.Background(), authority.DeleteAccount{CommandID: authoritytest.NewID(t), AccountID: target.account,
				IdentityKey: targetKey, At: f.Now, Principal: targetP})
			return err
		},
	}
	for name, command := range cases {
		t.Run(name, func(t *testing.T) {
			f := authoritytest.New(t)
			for round := range 12 {
				owner := newMember(t, f)
				family := createFamily(t, f, f.Service, owner)
				target, targetKey := keyedMember(t, f)
				in := invite(t, f, f.Service, owner, family, "caregiver")
				if _, err := f.Service.RedeemInvitation(t.Context(), redeemOf(t, f, target, in.TokenDigest)); err != nil {
					t.Fatal(err)
				}
				var raws []string
				for range 6 {
					_, _, raw := f.Session(t, target.account, f.Device(t, target.account), "family", family, "caregiver")
					raws = append(raws, raw)
				}
				ownerP, targetP := owner.principal(t, f), target.principal(t, f)
				gate := make(chan struct{})
				var wg sync.WaitGroup
				errs := make(chan error, 16)
				for _, raw := range raws {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-gate
						if _, err := f.Service.Refresh(context.Background(), refreshOf(f, raw, "", nil)); err != nil {
							errs <- err
						}
					}()
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-gate
					if round%3 != 0 {
						time.Sleep(time.Duration(round%3) * time.Millisecond)
					}
					if err := command(f, owner, target, targetKey, ownerP, targetP); err != nil {
						errs <- fmt.Errorf("command: %w", err)
					}
				}()
				close(gate)
				wg.Wait()
				close(errs)
				for err := range errs {
					if strings.Contains(err.Error(), "command:") || authority.CodeOf(err) != "session_revoked" {
						t.Fatalf("round %d: %v", round, err)
					}
				}
				if n := count(t, f, `SELECT count(*) FROM sessions WHERE account_id=$1 AND status='active'`, target.account); n != 0 {
					t.Fatalf("round %d: %d sessions escaped the revocation", round, n)
				}
			}
		})
	}
}

// A join racing the owner's deletion either lands before it (and is swept
// into the deletion), finds its invitation revoked by it, or makes the
// deletion's member snapshot stale (membership_revoked). No active member,
// slot or live family session of the family survives a committed deletion.
func TestScheduleDeletionRacingJoinLeavesNoActiveMember(t *testing.T) {
	f := authoritytest.New(t)
	outcomes := map[string]int{}
	for round := range 12 {
		owner := newMember(t, f)
		family := createFamily(t, f, f.Service, owner)
		joiner := newMember(t, f)
		in := invite(t, f, f.Service, owner, family, "caregiver")
		redeem := redeemOf(t, f, joiner, in.TokenDigest)
		schedule := scheduleOf(t, f, owner.account, owner.principal(t, f))
		var redeemErr, scheduleErr error
		gate := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-gate
			_, redeemErr = f.Service.RedeemInvitation(context.Background(), redeem)
		}()
		go func() {
			defer wg.Done()
			<-gate
			// No delay, a delay inside the redemption, or one past its commit.
			time.Sleep([]time.Duration{0, time.Millisecond, 50 * time.Millisecond}[round%3])
			_, scheduleErr = f.Service.ScheduleDeletion(context.Background(), schedule)
		}()
		close(gate)
		wg.Wait()
		switch {
		case scheduleErr == nil && redeemErr == nil:
			outcomes["joined_then_deleted"]++
		case scheduleErr == nil && authority.CodeOf(redeemErr) == "invitation_revoked":
			// The deletion revoked the family's pending invitations.
			outcomes["deleted_then_refused"]++
		case authority.CodeOf(scheduleErr) == "membership_revoked" && redeemErr == nil:
			outcomes["stale_snapshot"]++
			continue
		default:
			t.Fatalf("round %d: schedule %v, redeem %v", round, scheduleErr, redeemErr)
		}
		if n := count(t, f, `SELECT count(*) FROM memberships WHERE family_id=$1 AND status='active'`, family); n != 0 {
			t.Fatalf("round %d: %d active members of a deleted family", round, n)
		}
		if n := count(t, f, `SELECT count(*) FROM family_slots WHERE family_id=$1 AND state='active'`, family); n != 0 {
			t.Fatalf("round %d: %d active slots of a deleted family", round, n)
		}
		if n := count(t, f, `SELECT count(*) FROM sessions WHERE family_id=$1 AND status='active'`, family); n != 0 {
			t.Fatalf("round %d: %d live family sessions of a deleted family", round, n)
		}
	}
	t.Logf("outcomes %v", outcomes)
}

// The purge keeps invitation rows, so a lost-acknowledgement retry of a
// redemption into a purged family still returns its stored result.
func TestPurgedFamilyKeepsRedeemReplay(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	joiner := newMember(t, f)
	in := invite(t, f, f.Service, owner, family, "caregiver")
	redeem := redeemOf(t, f, joiner, in.TokenDigest)
	joinedResult, err := f.Service.RedeemInvitation(t.Context(), redeem)
	if err != nil {
		t.Fatal(err)
	}
	start := f.Now
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
		t.Fatal(err)
	}
	purge := authority.StartFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: family, At: start.Add(purgeDelay)}
	if _, err := f.Service.StartFamilyPurge(t.Context(), purge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Service.CompleteFamilyPurge(t.Context(), authority.CompleteFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: family,
		StartCommandID: purge.CommandID, Receipt: authoritytest.NewID(t), At: start.Add(purgeDelay)}); err != nil {
		t.Fatal(err)
	}
	f.Now = start.Add(purgeDelay)
	again, err := f.Service.RedeemInvitation(t.Context(), redeem)
	if err != nil || again != joinedResult {
		t.Fatalf("redeem replay after purge %+v %v, want %+v", again, err, joinedResult)
	}
}

// The retention worker's actor is legacy security.RetentionWorkerActorID:
// sha256("baby-tracker/retention-worker/v1/" + instance) shaped as a UUID
// with version nibble 8 (golden value from the legacy function).
func TestRetentionWorkerActorIDMatchesLegacyDerivation(t *testing.T) {
	got, err := authority.RetentionWorkerActorID("6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b")
	if err != nil || got != "181ba054-cb92-828e-8aad-1d15ff901250" {
		t.Fatalf("retention actor %q %v", got, err)
	}
	_, err = authority.RetentionWorkerActorID("not-a-uuid")
	wantCode(t, err, "invalid_command")
}

// Starting a purge releases the family's recovery claims (recovery is no
// longer possible), so the owner may create another family and schedule its
// deletion while the first purge is still running; completing the first
// purge afterwards leaves the second claim alone.
func TestPurgeStartReleasesOwnerClaimForANewFamily(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	first := createFamily(t, f, f.Service, owner)
	start := f.Now
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
		t.Fatal(err)
	}
	f.Now = start.Add(purgeDelay)
	purge := authority.StartFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: first, At: f.Now}
	if _, err := f.Service.StartFamilyPurge(t.Context(), purge); err != nil {
		t.Fatal(err)
	}
	if claim := claimOf(t, f, first, owner.account); claim != "forfeited" {
		t.Fatalf("claim after purge start %q", claim)
	}
	proof := freshProof(t, f, owner.account)
	if proof.Scope != "account" {
		t.Fatalf("proof after purge start %+v", proof)
	}
	second := authoritytest.NewID(t)
	if _, err := f.Service.CreateFamily(t.Context(), authority.CreateFamily{CommandID: authoritytest.NewID(t), OwnerAccountID: owner.account, FamilyID: second,
		DisplayName: "Second", At: f.Now, Principal: proof}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, proof)); err != nil {
		t.Fatalf("second deletion while the first purge runs: %v", err)
	}
	if claim := claimOf(t, f, second, owner.account); claim != "active" {
		t.Fatalf("second claim %q", claim)
	}
	if _, err := f.Service.CompleteFamilyPurge(t.Context(), authority.CompleteFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: first,
		StartCommandID: purge.CommandID, Receipt: authoritytest.NewID(t), At: f.Now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if a, b := claimOf(t, f, first, owner.account), claimOf(t, f, second, owner.account); a != "forfeited" || b != "active" {
		t.Fatalf("claims after completion first=%q second=%q", a, b)
	}
	status, err := f.Service.RecoveryStatus(t.Context(), owner.account)
	if err != nil || status.FamilyID != second || !status.CanRecover {
		t.Fatalf("recovery status %+v %v", status, err)
	}
	if n := count(t, f, `SELECT count(*) FROM family_recovery_claims WHERE account_id=$1 AND status='active'`, owner.account); n != 1 {
		t.Fatalf("%d active claims", n)
	}
}

// A forfeit's exact retry returns its receipt across a fresh proof (whose
// scope is no longer family_recovery once the slot is gone).
func TestForfeitExactRetryAcrossFreshProof(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
		t.Fatal(err)
	}
	forfeit := authority.ForfeitRecovery{CommandID: authoritytest.NewID(t), AccountID: owner.account, At: f.Now, Principal: freshProof(t, f, owner.account)}
	first, err := f.Service.ForfeitRecovery(t.Context(), forfeit)
	if err != nil {
		t.Fatal(err)
	}
	rows := len(readAudit(t, f, authority.ScopeFamily, family))
	f.Now = f.Now.Add(time.Hour)
	forfeit.Principal, forfeit.At = freshProof(t, f, owner.account), f.Now
	if forfeit.Principal.Scope != "account" {
		t.Fatalf("proof after forfeit %+v", forfeit.Principal)
	}
	if again, err := f.Service.ForfeitRecovery(t.Context(), forfeit); err != nil || again != first {
		t.Fatalf("forfeit replay %+v %v, want %+v", again, err, first)
	}
	if n := len(readAudit(t, f, authority.ScopeFamily, family)); n != rows {
		t.Fatalf("%d family audit rows after replay, want %d", n, rows)
	}
}

// A family recovered, rejoined, transferred and deleted again re-arms its one
// claim row for the new owner: the old owner is no longer a recovery
// principal and the new owner can recover.
func TestDeletionAfterTransferRearmsClaimForNewOwner(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	caregiver := joined(t, f, f.Service, owner, family, "caregiver")
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Service.RecoverFamily(t.Context(), authority.RecoverFamily{CommandID: authoritytest.NewID(t), AccountID: owner.account, At: f.Now,
		Principal: freshProof(t, f, owner.account)}); err != nil {
		t.Fatal(err)
	}
	ownerP := freshProof(t, f, owner.account)
	inv := invitationOf(t, f, owner, family, "caregiver")
	inv.Principal = ownerP
	if _, err := f.Service.CreateInvitation(t.Context(), inv); err != nil {
		t.Fatal(err)
	}
	caregiverP := freshProof(t, f, caregiver.account)
	if _, err := f.Service.RedeemInvitation(t.Context(), authority.RedeemInvitation{CommandID: authoritytest.NewID(t), RedeemerAccountID: caregiver.account,
		TokenDigestCandidates: []string{inv.TokenDigest}, At: f.Now, Principal: caregiverP}); err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if _, err := f.Service.TransferOwner(t.Context(), authority.TransferOwner{CommandID: authoritytest.NewID(t), OldOwnerID: owner.account,
		NewOwnerID: caregiver.account, At: f.Now, Principal: ownerP}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, caregiver.account, caregiverP)); err != nil {
		t.Fatal(err)
	}
	var claimAccount, claimStatus string
	inAuthorityTxQuery(t, f, `SELECT account_id::text, status::text FROM family_recovery_claims WHERE family_id=$1`, []any{family}, &claimAccount, &claimStatus)
	if claimAccount != caregiver.account || claimStatus != "active" {
		t.Fatalf("claim %s/%s, want %s/active", claimAccount, claimStatus, caregiver.account)
	}
	_, err := f.Service.RecoveryStatus(t.Context(), owner.account)
	wantCode(t, err, "recovery_only")
	oldOwnerProof := freshProof(t, f, owner.account)
	_, err = f.Service.RecoverFamily(t.Context(), authority.RecoverFamily{CommandID: authoritytest.NewID(t), AccountID: owner.account, At: f.Now,
		Principal: oldOwnerProof})
	wantCode(t, err, "recovery_only")
	newOwnerProof := freshProof(t, f, caregiver.account)
	if newOwnerProof.Scope != "family_recovery" || newOwnerProof.SessionFamilyID != family {
		t.Fatalf("new owner proof %+v", newOwnerProof)
	}
	if _, err := f.Service.RecoverFamily(t.Context(), authority.RecoverFamily{CommandID: authoritytest.NewID(t), AccountID: caregiver.account, At: f.Now,
		Principal: newOwnerProof}); err != nil {
		t.Fatal(err)
	}
	if status, ownerID, _ := familyState(t, f, family); status != "active" || ownerID != caregiver.account {
		t.Fatalf("family after second recovery %s %s", status, ownerID)
	}
	if _, st := membershipOf(t, f, family, owner.account); st != "former" {
		t.Fatalf("old owner membership %s", st)
	}
}

// Purge start deletes the owner's recovery slot; the owner's recovery
// sessions become account scope in the same transaction (as a forfeit leaves
// them), so the account surface (GET /v1/account, invitation preview) keeps
// working on the same session instead of refusing it as session_revoked and
// forcing a sign-in. Nothing is revoked.
func TestPurgeStartMovesOwnerRecoverySessionsToAccountScope(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	start := f.Now
	if _, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f))); err != nil {
		t.Fatal(err)
	}
	proof := freshProof(t, f, owner.account)
	if proof.SessionScope != "family_recovery" || proof.SessionFamilyID != family {
		t.Fatalf("recovery proof %+v", proof)
	}
	if err := f.Service.WithAccountTx(t.Context(), proof, func(pgx.Tx) error { return nil }); !errors.Is(err, authority.ErrSessionRevoked) {
		t.Fatalf("recovery session on the account surface before purge: %v", err)
	}
	f.Now = start.Add(purgeDelay)
	if _, err := f.Service.StartFamilyPurge(t.Context(), authority.StartFamilyPurge{CommandID: authoritytest.NewID(t), FamilyID: family, At: f.Now}); err != nil {
		t.Fatal(err)
	}
	if err := f.Service.WithAccountTx(t.Context(), proof, func(pgx.Tx) error { return nil }); err != nil {
		t.Fatalf("account surface after purge start: %v", err)
	}
	if n := count(t, f, `SELECT count(*) FROM sessions WHERE account_id=$1 AND status='active' AND (scope<>'account' OR family_id IS NOT NULL OR role IS NOT NULL)`,
		owner.account); n != 0 {
		t.Fatalf("%d owner sessions kept a family scope after purge start", n)
	}
	if n := count(t, f, `SELECT count(*) FROM sessions WHERE id=$1 AND status='active'`, proof.SessionID); n != 1 {
		t.Fatal("purge start revoked the owner's recovery session")
	}
}
