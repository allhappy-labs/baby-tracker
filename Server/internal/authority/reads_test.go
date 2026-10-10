package authority_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/authority/authoritytest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// FamilyRevision is the family's audit seq, the ExpectedRevision every
// operator command compares against; an unknown family is family_unavailable.
func TestFamilyRevisionIsTheFamilyAuditSeq(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	got, err := f.Service.FamilyRevision(t.Context(), family)
	if err != nil || got != 1 || got != familyRevision(t, f, family) {
		t.Fatalf("revision=%d err=%v want %d", got, err, familyRevision(t, f, family))
	}
	invite(t, f, f.Service, owner, family, "caregiver")
	if got, err := f.Service.FamilyRevision(t.Context(), family); err != nil || got != 2 {
		t.Fatalf("after invite revision=%d err=%v", got, err)
	}
	if _, err := f.Service.FamilyRevision(t.Context(), authoritytest.NewID(t)); authority.CodeOf(err) != "family_unavailable" {
		t.Fatalf("unknown family err=%v", err)
	}
	if _, err := f.Service.FamilyRevision(t.Context(), "not-a-uuid"); authority.CodeOf(err) != "invalid_command" {
		t.Fatalf("malformed family err=%v", err)
	}
	// The revision read is what an operator command expects: a grant at that
	// revision commits, a stale one is stale_security_revision.
	grant := grantOf(t, f, family, owner.account)
	revision, err := f.Service.FamilyRevision(t.Context(), family)
	if err != nil {
		t.Fatal(err)
	}
	stale := grant
	stale.ExpectedRevision = revision - 1
	if _, err := f.Service.ExecuteOperatorIntegration(t.Context(), stale); authority.CodeOf(err) != "stale_security_revision" {
		t.Fatalf("stale revision err=%v", err)
	}
	grant.ExpectedRevision = revision
	if _, err := f.Service.ExecuteOperatorIntegration(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
}

// CommandReceipt reports the actor's stored receipt for a command ID (any
// kind) with the families it touched; another actor's receipt is invisible.
func TestCommandReceiptReportsKindAndFamilies(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	commandID := authoritytest.NewID(t)
	family := authoritytest.NewID(t)
	created, err := f.Service.CreateFamily(t.Context(), authority.CreateFamily{CommandID: commandID, OwnerAccountID: owner.account, FamilyID: family,
		DisplayName: "The Smiths", At: f.Now, Principal: owner.principal(t, f)})
	if err != nil {
		t.Fatal(err)
	}
	receipt, found, err := f.Service.CommandReceipt(t.Context(), owner.account, commandID)
	if err != nil || !found || receipt.Kind != authority.KindFamilyCreated || len(receipt.FamilyIDs) != 1 || receipt.FamilyIDs[0] != family ||
		receipt.Result != created.Result {
		t.Fatalf("receipt=%+v found=%v err=%v", receipt, found, err)
	}
	if _, found, err := f.Service.CommandReceipt(t.Context(), owner.account, authoritytest.NewID(t)); err != nil || found {
		t.Fatalf("unknown command found=%v err=%v", found, err)
	}
	other := newMember(t, f)
	if _, found, err := f.Service.CommandReceipt(t.Context(), other.account, commandID); err != nil || found {
		t.Fatalf("other actor found=%v err=%v", found, err)
	}
	if _, _, err := f.Service.CommandReceipt(t.Context(), "bad", commandID); authority.CodeOf(err) != "invalid_command" {
		t.Fatalf("malformed actor err=%v", err)
	}
}

// FamilyRecoveryStatus is legacy recoveryStatusFromState: it names the family
// directly (no slot), so a deletion-pending family reports its window, a
// forfeited claim is recovery_forfeited and a recovered family is
// family_unavailable. RecoveryStatus (by slot) is unchanged.
func TestFamilyRecoveryStatusNamesTheFamily(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	if _, err := f.Service.FamilyRecoveryStatus(t.Context(), owner.account, family); authority.CodeOf(err) != "family_unavailable" {
		t.Fatalf("active family err=%v", err)
	}
	deleted, err := f.Service.ScheduleDeletion(t.Context(), scheduleOf(t, f, owner.account, owner.principal(t, f)))
	if err != nil {
		t.Fatal(err)
	}
	want := authority.RecoveryStatus{FamilyID: family, State: "deletion_pending", PurgeAfter: deleted.PurgeAfter, CanRecover: true}
	got, err := f.Service.FamilyRecoveryStatus(t.Context(), owner.account, family)
	if err != nil || got != want {
		t.Fatalf("status=%+v err=%v want %+v", got, err, want)
	}
	if bySlot, err := f.Service.RecoveryStatus(t.Context(), owner.account); err != nil || bySlot != want {
		t.Fatalf("by slot=%+v err=%v", bySlot, err)
	}
	f.Now = deleted.PurgeAfter
	if got, err := f.Service.FamilyRecoveryStatus(t.Context(), owner.account, family); err != nil || got.CanRecover {
		t.Fatalf("at deadline status=%+v err=%v", got, err)
	}
	f.Now = deleted.PurgeAfter.Add(-time.Hour)
	recovery := freshProof(t, f, owner.account)
	if _, err := f.Service.ForfeitRecovery(t.Context(), authority.ForfeitRecovery{CommandID: authoritytest.NewID(t), AccountID: owner.account,
		At: f.Now, Principal: recovery}); err != nil {
		t.Fatal(err)
	}
	// A forfeited claim on a family still pending deletion reports the window
	// with CanRecover false, as legacy recoveryStatusFromState did.
	if got, err := f.Service.FamilyRecoveryStatus(t.Context(), owner.account, family); err != nil || got.CanRecover || got.FamilyID != family {
		t.Fatalf("forfeited status=%+v err=%v", got, err)
	}
	// Once the family leaves deletion_pending, the forfeited claim is what the
	// owner is told.
	f.Exec(t, `UPDATE families SET status='purge_started', purge_started_at=$2, purge_command_id=$3 WHERE id=$1`, family, f.Now, authoritytest.NewID(t))
	if _, err := f.Service.FamilyRecoveryStatus(t.Context(), owner.account, family); authority.CodeOf(err) != "recovery_forfeited" {
		t.Fatalf("forfeited err=%v", err)
	}
	if _, err := f.Service.RecoveryStatus(t.Context(), owner.account); authority.CodeOf(err) != "recovery_only" {
		t.Fatalf("forfeited by slot err=%v", err)
	}
	stranger := newMember(t, f)
	if _, err := f.Service.FamilyRecoveryStatus(t.Context(), stranger.account, family); authority.CodeOf(err) != "family_unavailable" {
		t.Fatalf("stranger err=%v", err)
	}
	if _, err := f.Service.FamilyRecoveryStatus(t.Context(), owner.account, "bad"); authority.CodeOf(err) != "invalid_command" {
		t.Fatalf("malformed err=%v", err)
	}
}

// AccountIdentityKey is the key DeleteAccount expects; it stays readable
// after the deletion (live=false) so an exact retry hashes identically.
func TestAccountIdentityKeyFeedsDeleteAccount(t *testing.T) {
	f := authoritytest.New(t)
	m, key := keyedMember(t, f)
	got, found, err := f.Service.AccountIdentityKey(t.Context(), m.account)
	if err != nil || !found || got != key {
		t.Fatalf("key=%q found=%v err=%v want %q", got, found, err, key)
	}
	in := authority.DeleteAccount{CommandID: authoritytest.NewID(t), AccountID: m.account, IdentityKey: got, At: f.Now, Principal: m.principal(t, f)}
	first, err := f.Service.DeleteAccount(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	after, found, err := f.Service.AccountIdentityKey(t.Context(), m.account)
	if err != nil || !found || after != key {
		t.Fatalf("after deletion key=%q found=%v err=%v", after, found, err)
	}
	in.IdentityKey = after
	if replay, err := f.Service.DeleteAccount(t.Context(), in); err != nil || replay != first {
		t.Fatalf("replay=%+v err=%v want %+v", replay, err, first)
	}
	bare := f.Account(t)
	if key, found, err := f.Service.AccountIdentityKey(t.Context(), bare); err != nil || found || key != "" {
		t.Fatalf("no identity key=%q found=%v err=%v", key, found, err)
	}
	if _, _, err := f.Service.AccountIdentityKey(t.Context(), "bad"); authority.CodeOf(err) != "invalid_command" {
		t.Fatalf("malformed err=%v", err)
	}
}

func TestValidIntegrationNameMatchesTheGrantRule(t *testing.T) {
	for name, want := range map[string]bool{
		"Claude Desktop": true, "a": true, "x-1.2_3": true, strings.Repeat("a", 64): true,
		"": false, " padded": false, "padded ": false, strings.Repeat("a", 65): false, "naïve": false, "tab\tname": false,
	} {
		if got := authority.ValidIntegrationName(name); got != want {
			t.Errorf("ValidIntegrationName(%q)=%v want %v", name, got, want)
		}
	}
}

// The read helpers run in read-only transactions: a write attempted through
// the same path is refused by PostgreSQL (25006), so a helper can never
// mutate authority rows.
func TestReadHelpersRunReadOnly(t *testing.T) {
	f := authoritytest.New(t)
	err := authority.ReadOnlyForTest(f.Service, t.Context(), func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE server_state SET recovery_epoch=recovery_epoch WHERE singleton`)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("write in read helper transaction err=%v", err)
	}
}

// FamilyAuditPage is the family's own chain in seq order, bounded by
// (after, through] and the page size, with no payload: the export archive's
// audit section. Another scope's rows (the owner's account chain) are not in
// it.
func TestFamilyAuditPageReadsTheFamilyChainInPages(t *testing.T) {
	f := authoritytest.New(t)
	owner := newMember(t, f)
	family := createFamily(t, f, f.Service, owner)
	for range 3 {
		invite(t, f, f.Service, owner, family, "caregiver")
	}
	revision := familyRevision(t, f, family)
	want := readAudit(t, f, authority.ScopeFamily, family)
	if uint64(len(want)) != revision || revision != 4 {
		t.Fatalf("family chain rows=%d revision=%d", len(want), revision)
	}
	all, err := f.Service.FamilyAuditPage(t.Context(), family, 0, revision, authority.MaxAuditPageRows)
	if err != nil || len(all) != len(want) {
		t.Fatalf("page=%d err=%v", len(all), err)
	}
	for i, entry := range all {
		row := want[i]
		if entry.Seq != row.Seq || entry.Kind != row.Kind || entry.ActorAccountID != row.ActorID || !entry.At.Equal(row.At) {
			t.Fatalf("entry %d=%+v want %+v", i, entry, row)
		}
	}
	if all[0].Kind != "family_created" || all[0].ActorAccountID != owner.account {
		t.Fatalf("first entry %+v", all[0])
	}
	// Pages: (1, 3] holds seq 2 and 3; a limit of one returns seq 2 only;
	// an empty range is an empty, non-nil page.
	page, err := f.Service.FamilyAuditPage(t.Context(), family, 1, 3, authority.MaxAuditPageRows)
	if err != nil || len(page) != 2 || page[0].Seq != 2 || page[1].Seq != 3 {
		t.Fatalf("(1,3] page=%+v err=%v", page, err)
	}
	if page, err = f.Service.FamilyAuditPage(t.Context(), family, 1, revision, 1); err != nil || len(page) != 1 || page[0].Seq != 2 {
		t.Fatalf("limit 1 page=%+v err=%v", page, err)
	}
	if page, err = f.Service.FamilyAuditPage(t.Context(), family, revision, revision, 10); err != nil || page == nil || len(page) != 0 {
		t.Fatalf("empty page=%+v err=%v", page, err)
	}
	// The owner's account chain shares no rows with the family's.
	if page, err = f.Service.FamilyAuditPage(t.Context(), owner.account, 0, 100, 10); err != nil || len(page) != 0 {
		t.Fatalf("account scope as family page=%+v err=%v", page, err)
	}
	for _, bad := range []struct {
		family       string
		after, until uint64
		limit        int
	}{{"not-a-uuid", 0, 1, 1}, {family, 0, 1, 0}, {family, 0, 1, authority.MaxAuditPageRows + 1}, {family, 0, 1 << 60, 1}} {
		if _, err := f.Service.FamilyAuditPage(t.Context(), bad.family, bad.after, bad.until, bad.limit); authority.CodeOf(err) != "invalid_command" {
			t.Fatalf("%+v err=%v", bad, err)
		}
	}
}
