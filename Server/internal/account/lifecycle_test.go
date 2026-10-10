package account

import (
	"crypto/rand"
	"testing"

	"babytracker/server/internal/access"
	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
)

func TestDeleteRequiresExplicitConfirmationAndCanonicalCommandID(t *testing.T) {
	service := &Service{}
	if _, err := service.Delete(t.Context(), structPrincipal(), "30000000-0000-4000-8000-000000000001", false); Code(err) != "confirmation_required" {
		t.Fatalf("confirmation code=%q err=%v", Code(err), err)
	}
	if _, err := service.Delete(t.Context(), structPrincipal(), "NOT-A-UUID", true); Code(err) != "invalid_request" {
		t.Fatalf("invalid code=%q err=%v", Code(err), err)
	}
	if Code(authority.ErrUnavailable) != "service_unavailable" {
		t.Fatal("authority error code was not preserved")
	}
}

func structPrincipal() access.Principal {
	return access.Principal{AccountID: "10000000-0000-4000-8000-000000000001"}
}

// signedIn is an account with a live Apple identity, one device and an
// account-scope session, as a sign-in leaves it.
type signedIn struct{ account, device, session string }

func signIn(t *testing.T, f *authoritytest.Fixture) signedIn {
	t.Helper()
	m := signedIn{account: f.Account(t)}
	var digest [32]byte
	if _, err := rand.Read(digest[:]); err != nil {
		t.Fatal(err)
	}
	if affected := f.Exec(t, `INSERT INTO account_identities(provider,identity_key_id,identity_digest,account_id,lifecycle_generation,audience,subject,live,created_at)
		VALUES ('apple','identity.v1',$1,$2,1,'com.example.app','subject',true,$3)`, digest[:], m.account, f.Now); affected != 1 {
		t.Fatalf("identity seed affected %d", affected)
	}
	m.device = f.Device(t, m.account)
	m.session, _, _ = f.Session(t, m.account, m.device, "account", "", "")
	return m
}

func (m signedIn) principal(t *testing.T, f *authoritytest.Fixture) access.Principal {
	t.Helper()
	var p access.Principal
	var epoch, seq int64
	query(t, f, `SELECT recovery_epoch, issued_seq, scope::text FROM sessions WHERE id=$1`, []any{m.session}, &epoch, &seq, &p.Scope)
	p.AccountID, p.DeviceID, p.SessionID, p.RecoveryEpoch, p.SecurityRevision, p.SessionScope = m.account, m.device, m.session, uint64(epoch), uint64(seq), p.Scope
	return p
}

// query reads as the table owner inside an authority transaction (the
// authority tables admit only authority transactions).
func query(t *testing.T, f *authoritytest.Fixture, statement string, args []any, dest ...any) {
	t.Helper()
	tx, err := f.DB.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(t.Context(), statement, args...).Scan(dest...); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

func accountStatus(t *testing.T, f *authoritytest.Fixture, account string) (status string, live bool) {
	t.Helper()
	query(t, f, `SELECT a.status::text, i.live FROM accounts a JOIN account_identities i ON i.account_id=a.id WHERE a.id=$1`, []any{account}, &status, &live)
	return status, live
}

// Deletion runs as one authority command over the account's identity key:
// it reports the manual Apple revocation, an exact retry from the deleted
// account returns the stored result, and the session is gone.
func TestDeleteCommitsThroughTheAuthorityAndReportsManualAppleRevocation(t *testing.T) {
	f := authoritytest.New(t)
	service := &Service{Authority: f.Service}
	m := signIn(t, f)
	principal := m.principal(t, f)
	commandID := authoritytest.NewID(t)
	first, err := service.Delete(t.Context(), principal, commandID, true)
	if err != nil || first.CommandID != commandID || first.Revision == 0 || first.AppleAuthorizationRevocation != "manual_required" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if status, live := accountStatus(t, f, m.account); status != "deleted" || live {
		t.Fatalf("account status=%q live=%v", status, live)
	}
	replay, err := service.Delete(t.Context(), principal, commandID, true)
	if err != nil || replay != first {
		t.Fatalf("replay=%+v err=%v want %+v", replay, err, first)
	}
	if _, err := service.Delete(t.Context(), principal, authoritytest.NewID(t), true); Code(err) != "session_revoked" {
		t.Fatalf("new command after deletion code=%q err=%v", Code(err), err)
	}
	var receipts int
	query(t, f, `SELECT count(*) FROM command_receipts WHERE actor_account_id=$1 AND kind='account_deleted'`, []any{m.account}, &receipts)
	if receipts != 1 {
		t.Fatalf("receipts=%d", receipts)
	}
}

// An active owner must hand the family on first; a command ID spent on
// another command is command_id_reused; an account without an identity row
// is membership_revoked.
func TestDeleteRefusalsKeepLegacyCodes(t *testing.T) {
	f := authoritytest.New(t)
	service := &Service{Authority: f.Service}
	owner := signIn(t, f)
	familyID, createID := authoritytest.NewID(t), authoritytest.NewID(t)
	if _, err := f.Service.CreateFamily(t.Context(), authority.CreateFamily{CommandID: createID, OwnerAccountID: owner.account, FamilyID: familyID,
		DisplayName: "The Smiths", Principal: owner.principal(t, f)}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Delete(t.Context(), owner.principal(t, f), authoritytest.NewID(t), true); Code(err) != "ownership_transfer_required" {
		t.Fatalf("active owner code=%q err=%v", Code(err), err)
	}
	if status, live := accountStatus(t, f, owner.account); status != "active" || !live {
		t.Fatalf("refused deletion changed account status=%q live=%v", status, live)
	}
	if _, err := service.Delete(t.Context(), owner.principal(t, f), createID, true); Code(err) != "command_id_reused" {
		t.Fatalf("reused command code=%q err=%v", Code(err), err)
	}
	bare := f.Account(t)
	device := f.Device(t, bare)
	session, _, _ := f.Session(t, bare, device, "account", "", "")
	p := signedIn{account: bare, device: device, session: session}.principal(t, f)
	if _, err := service.Delete(t.Context(), p, authoritytest.NewID(t), true); Code(err) != "membership_revoked" {
		t.Fatalf("no identity code=%q err=%v", Code(err), err)
	}
}
