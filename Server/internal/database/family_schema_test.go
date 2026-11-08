package database_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
)

// familyTables are the schema-29 family authority and presentation tables.
var familyTables = []string{
	"account_profile_commands", "accounts", "families", "family_actor_label_history", "family_actor_labels", "family_entitlements",
	"family_recovery_claims", "family_slots", "invitations", "memberships",
}

var familyTableColumns = map[string][]string{
	"accounts": {"id", "display_name", "status", "created_at", "updated_at", "profile_required", "lifecycle_generation", "audit_seq", "audit_hash", "chain_failed"},
	"families": {"id", "display_name", "status", "created_at", "updated_at", "purge_after", "profile_required", "next_sequence", "owner_account_id",
		"purge_started_at", "purge_command_id", "purge_receipt", "purged_at", "audit_seq", "audit_hash", "chain_failed", "entitlement_required",
		"care_record_count", "care_charged_bytes", "care_entity_count", "care_wire_bytes"},
	"memberships":                {"family_id", "account_id", "role", "status", "created_at", "updated_at"},
	"account_profile_commands":   {"account_id", "command_id", "input_hash", "created_at"},
	"family_actor_label_history": {"label_version", "family_id", "account_id", "display_name", "former", "recorded_at"},
	"family_slots":               {"account_id", "family_id", "state"},
	"invitations":                {"id", "family_id", "creator_account_id", "token_digest", "role", "status", "expires_at", "redeemed_at", "revoked_at", "created_at", "updated_at"},
	"family_entitlements":        {"family_id", "entitlement_key", "source", "status", "expires_at", "created_at", "updated_at"},
	"family_recovery_claims":     {"family_id", "account_id", "status"},
	"family_actor_labels":        {"family_id", "account_id", "display_name", "former", "created_at", "updated_at"},
}

// The schema-fourteen read-only caregiver authority. Runtime-only: no other
// role (the maintenance group included) may ask whether a membership may
// write.
const membershipMayWriteSignature = "app.membership_may_write(uuid,uuid)"

// Schema 29 drops app.lookup_family_invitation and the invitation_preview_owner
// policy: an invitee previews through authority.LookupInvitation (an
// authority read returning only the family's display name, the invitation's
// status and expiry; authority TestCreateInviteRedeemReplay). An
// authenticated outsider's own tenant context still reads none of the
// inviting family's rows.
func TestInvitationPreviewAllowsAuthenticatedOutsiderWithoutTenantLeak(t *testing.T) {
	fixture := testpg.New(t)
	ids := seedAuthorityFamilies(t, fixture)
	const outsider = "61000000-0000-4000-8000-000000000003"
	if err := authorityExec(t.Context(), fixture.Owner, "INSERT INTO accounts(id,display_name) VALUES ($1,'Outsider')", outsider); err != nil {
		t.Fatal(err)
	}
	var previewFunction bool
	var previewPolicies int
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT to_regprocedure('app.lookup_family_invitation(text)') IS NOT NULL,
		(SELECT count(*) FROM pg_policies WHERE schemaname='public' AND tablename='invitations')`).Scan(&previewFunction, &previewPolicies); err != nil {
		t.Fatal(err)
	}
	if previewFunction || previewPolicies != 1 {
		t.Fatalf("legacy preview function=%t invitation policies=%d", previewFunction, previewPolicies)
	}
	for _, test := range []struct{ name, account, family string }{
		{"no family", outsider, ""},
		{"different family", outsider, ids.familyB},
		{"other family's owner", ids.accountB, ids.familyB},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := fixture.Runtime.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err := tx.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true),set_config('app.family_id',$2,true)", test.account, test.family); err != nil {
				t.Fatal(err)
			}
			for table, column := range map[string]string{"invitations": "family_id", "families": "id", "memberships": "family_id", "family_entitlements": "family_id"} {
				var leaked int
				if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM "+table+" WHERE "+column+"=$1", ids.familyA).Scan(&leaked); err != nil || leaked != 0 {
					t.Fatalf("outsider reads %d %s rows of the inviting family err=%v", leaked, table, err)
				}
			}
		})
	}
}

func TestUpdateOwnProfileRefreshesOnlyActiveFamilyLabelFromAccountContext(t *testing.T) {
	fixture := testpg.New(t)
	const (
		accountID     = "10000000-0000-4000-8000-000000000001"
		familyID      = "20000000-0000-4000-8000-000000000001"
		otherFamilyID = "20000000-0000-4000-8000-000000000002"
		commandID     = "30000000-0000-4000-8000-000000000001"
	)
	setup, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), setup, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	// Authority tables admit writes only from authority transactions.
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.authority','1',true)"); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Before')", accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name,owner_account_id) VALUES($1,'Active',$2)", familyID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", familyID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO family_slots(account_id,family_id,state) VALUES($1,$2,'active')", accountID, familyID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO family_actor_labels(family_id,account_id,display_name,created_at,updated_at)
		VALUES($1,$2,'Before','2026-09-20T11:00:00Z','2026-09-20T11:00:00Z')`, familyID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.family_id',$1,true)", otherFamilyID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name,status) VALUES($1,'Other','deletion_pending')", otherFamilyID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO family_actor_labels(family_id,account_id,display_name,created_at,updated_at)
		VALUES($1,$2,'Unrelated','2026-09-20T11:00:00Z','2026-09-20T11:00:00Z')`, otherFamilyID, accountID); err != nil {
		t.Fatal(err)
	}
	if err := setup.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	runtime, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Rollback(t.Context())
	if err := tenant.SetAccountLocal(t.Context(), runtime, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Exec(t.Context(), "SELECT app.update_own_profile($1,$2,'After','2026-09-20T12:00:00Z')", accountID, commandID); err != nil {
		t.Fatalf("account-only profile update: %v", err)
	}
	if _, err := runtime.Exec(t.Context(), "SELECT app.update_own_profile($1,$2,'After','2026-09-20T12:01:00Z')", accountID, commandID); err != nil {
		t.Fatalf("exact profile retry: %v", err)
	}
	if err := runtime.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	read, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback(t.Context())
	if _, err := read.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true),set_config('app.family_id',$2,true)", accountID, familyID); err != nil {
		t.Fatal(err)
	}
	var accountName, activeLabel string
	if err := read.QueryRow(t.Context(), `SELECT a.display_name,l.display_name FROM accounts a
		JOIN family_actor_labels l ON l.account_id=a.id WHERE a.id=$1 AND l.family_id=$2`, accountID, familyID).Scan(&accountName, &activeLabel); err != nil {
		t.Fatal(err)
	}
	if accountName != "After" || activeLabel != "After" {
		t.Fatalf("account=%q active label=%q", accountName, activeLabel)
	}
	var historyName string
	var historyFormer bool
	if err := read.QueryRow(t.Context(), `SELECT display_name,former FROM family_actor_label_history
		WHERE family_id=$1 AND account_id=$2 ORDER BY label_version DESC LIMIT 1`, familyID, accountID).
		Scan(&historyName, &historyFormer); err != nil {
		t.Fatal(err)
	}
	if historyName != "After" || historyFormer {
		t.Fatalf("profile history name=%q former=%t", historyName, historyFormer)
	}
	var historyCount, hashLength int
	if err := read.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM family_actor_label_history
		WHERE family_id=$1 AND account_id=$2),(SELECT octet_length(input_hash) FROM account_profile_commands
		WHERE account_id=$2 AND command_id=$3)`, familyID, accountID, commandID).Scan(&historyCount, &hashLength); err != nil {
		t.Fatal(err)
	}
	if historyCount != 1 || hashLength != 32 {
		t.Fatalf("profile retry history=%d receipt hash bytes=%d", historyCount, hashLength)
	}
	if _, err := read.Exec(t.Context(), "SELECT set_config('app.family_id',$1,true)", otherFamilyID); err != nil {
		t.Fatal(err)
	}
	var unrelated string
	if err := read.QueryRow(t.Context(), "SELECT display_name FROM family_actor_labels WHERE family_id=$1 AND account_id=$2", otherFamilyID, accountID).Scan(&unrelated); err != nil {
		t.Fatal(err)
	}
	if unrelated != "Unrelated" {
		t.Fatalf("unrelated family label=%q", unrelated)
	}

	reused, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer reused.Rollback(t.Context())
	if err := tenant.SetAccountLocal(t.Context(), reused, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := reused.Exec(t.Context(), "SELECT app.update_own_profile($1,$2,'Changed','2026-09-20T12:02:00Z')", accountID, commandID); sqlState(err) != "BT002" {
		t.Fatalf("changed profile retry err=%v state=%q", err, sqlState(err))
	}
	for index, invalidName := range []string{"   ", "\u00a0Padded", "Hidden\u0001"} {
		tx, err := fixture.Runtime.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := tenant.SetAccountLocal(t.Context(), tx, accountID); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(t.Context(), "SELECT app.update_own_profile($1,$2,$3,'2026-09-20T12:03:00Z')", accountID,
			fmt.Sprintf("30000000-0000-4000-8000-%012d", index+10), invalidName)
		tx.Rollback(t.Context())
		if sqlState(err) != "22023" {
			t.Fatalf("invalid profile display name %q err=%v state=%q", invalidName, err, sqlState(err))
		}
	}
}

func TestFamilyActorLabelPagesUseStableHighWaterAndDenyDirectHistoryReads(t *testing.T) {
	fixture := testpg.New(t)
	const (
		accountOne    = "10000000-0000-4000-8000-000000000001"
		accountTwo    = "10000000-0000-4000-8000-000000000002"
		accountThree  = "10000000-0000-4000-8000-000000000003"
		familyID      = "20000000-0000-4000-8000-000000000001"
		otherFamilyID = "20000000-0000-4000-8000-000000000002"
	)
	setup, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), setup, tenant.Context{AccountID: accountOne, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	// Authority tables admit writes only from authority transactions.
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.authority','1',true)"); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'One')", accountOne); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true)", accountThree); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Three before')", accountThree); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true)", accountOne); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name,owner_account_id) VALUES($1,'Family',$2)", familyID, accountOne); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO memberships(family_id,account_id,role) VALUES
		($1,$2,'owner'),($1,$3,'caregiver')`, familyID, accountOne, accountThree); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO family_slots(account_id,family_id,state) VALUES($1,$2,'active')", accountOne, familyID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true)", accountThree); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO family_slots(account_id,family_id,state) VALUES($1,$2,'active')", accountThree, familyID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO family_actor_labels(family_id,account_id,display_name,created_at,updated_at) VALUES
		($1,$2,'One','2026-09-20T11:00:00Z','2026-09-20T11:00:00Z'),
		($1,$3,'Three before','2026-09-20T11:00:00Z','2026-09-20T11:00:00Z')`, familyID, accountOne, accountThree); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO family_actor_label_history(family_id,account_id,display_name,former,recorded_at) VALUES
		($1,$2,'One',false,'2026-09-20T11:00:00Z'),
		($1,$3,'Three before',false,'2026-09-20T11:00:00Z')`, familyID, accountOne, accountThree); err != nil {
		t.Fatal(err)
	}
	if err := setup.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	type actorLabel struct {
		accountID, name string
		former, hasMore bool
		highWater       int64
	}
	page := func(highWater int64, after any, limit int) ([]actorLabel, error) {
		tx, err := fixture.Runtime.Begin(t.Context())
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(t.Context())
		if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountOne, FamilyID: familyID}); err != nil {
			return nil, err
		}
		rows, err := tx.Query(t.Context(), `SELECT account_id::text,display_name,former,high_water,has_more
			FROM app.list_family_actor_labels($1,$2,$3::uuid,$4)`, familyID, highWater, after, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var result []actorLabel
		for rows.Next() {
			var item actorLabel
			if err := rows.Scan(&item.accountID, &item.name, &item.former, &item.highWater, &item.hasMore); err != nil {
				return nil, err
			}
			result = append(result, item)
		}
		return result, rows.Err()
	}

	waterTx, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), waterTx, tenant.Context{AccountID: accountOne, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	var oldHighWater int64
	if err := waterTx.QueryRow(t.Context(), "SELECT app.family_actor_labels_high_water()").Scan(&oldHighWater); err != nil {
		t.Fatal(err)
	}
	if err := waterTx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	first, err := page(oldHighWater, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].accountID != accountOne || !first[0].hasMore || first[0].highWater != oldHighWater {
		t.Fatalf("first stable page=%+v high_water=%d", first, oldHighWater)
	}

	update, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetAccountLocal(t.Context(), update, accountThree); err != nil {
		t.Fatal(err)
	}
	if _, err := update.Exec(t.Context(), `SELECT app.update_own_profile($1,
		'30000000-0000-4000-8000-000000000003','Three after','2026-09-20T12:00:00Z')`, accountThree); err != nil {
		t.Fatal(err)
	}
	if err := update.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	add, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer add.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), add, tenant.Context{AccountID: accountTwo, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	// Authority tables admit writes only from authority transactions.
	if _, err := add.Exec(t.Context(), "SELECT set_config('app.authority','1',true)"); err != nil {
		t.Fatal(err)
	}
	if _, err := add.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Two')", accountTwo); err != nil {
		t.Fatal(err)
	}
	if _, err := add.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'caregiver')", familyID, accountTwo); err != nil {
		t.Fatal(err)
	}
	if _, err := add.Exec(t.Context(), `INSERT INTO family_actor_labels(family_id,account_id,display_name,created_at,updated_at)
		VALUES($1,$2,'Two','2026-09-20T12:01:00Z','2026-09-20T12:01:00Z')`, familyID, accountTwo); err != nil {
		t.Fatal(err)
	}
	if _, err := add.Exec(t.Context(), `INSERT INTO family_actor_label_history(family_id,account_id,display_name,former,recorded_at)
		VALUES($1,$2,'Two',false,'2026-09-20T12:01:00Z')`, familyID, accountTwo); err != nil {
		t.Fatal(err)
	}
	if err := add.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	second, err := page(oldHighWater, accountOne, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].accountID != accountThree || second[0].name != "Three before" || second[0].hasMore {
		t.Fatalf("second stable page=%+v", second)
	}
	freshTx, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), freshTx, tenant.Context{AccountID: accountOne, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	var freshHighWater int64
	if err := freshTx.QueryRow(t.Context(), "SELECT app.family_actor_labels_high_water()").Scan(&freshHighWater); err != nil {
		t.Fatal(err)
	}
	if err := freshTx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	fresh, err := page(freshHighWater, accountOne, 10)
	if err != nil {
		t.Fatal(err)
	}
	if freshHighWater <= oldHighWater || len(fresh) != 2 || fresh[0].accountID != accountTwo || fresh[1].accountID != accountThree || fresh[1].name != "Three after" {
		t.Fatalf("fresh page=%+v old=%d fresh=%d", fresh, oldHighWater, freshHighWater)
	}

	for name, test := range map[string]struct {
		highWater int64
		family    string
		state     string
	}{
		"future high water": {freshHighWater + 100, familyID, "22023"},
		"cross family":      {freshHighWater, otherFamilyID, "42501"},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := fixture.Runtime.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountOne, FamilyID: familyID}); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(t.Context(), "SELECT * FROM app.list_family_actor_labels($1,$2,NULL,10)", test.family, test.highWater)
			if sqlState(err) != test.state {
				t.Fatalf("err=%v state=%q want=%q", err, sqlState(err), test.state)
			}
		})
	}
	direct, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), direct, tenant.Context{AccountID: accountOne, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := direct.Exec(t.Context(), "SELECT count(*) FROM family_actor_label_history"); sqlState(err) != "42501" {
		t.Fatalf("runtime direct history read err=%v state=%q", err, sqlState(err))
	}
}

// The exact schema-29 policies on the family and account tables (plus
// 000031's two maintenance-group policies on families). The
// authority tables admit app.authority_context() or the tenant; accounts,
// families and family_slots split their policies so that a tenant may only
// read (and, for accounts and families, UPDATE the columns the authority
// column triggers leave it); memberships, invitations, family_entitlements,
// family_recovery_claims and devices keep one ALL policy whose USING admits a
// tenant DELETE, so their DELETE is closed only by the missing runtime grant
// (TestRuntimeHasNoDeleteWhereAPolicyAdmitsTenantDelete).
func TestFamilyPoliciesAreExactAndCommandMaterialIsIsolated(t *testing.T) {
	fixture := testpg.New(t)
	type policy struct{ table, name, command, using, check string }
	const authority, tenantAccount, tenantFamily = "app.authority_context()", "(app.authority_context() OR (account_id = app.current_account_id()))", "(app.authority_context() OR (family_id = app.current_family_id()))"
	want := []policy{
		{"account_profile_commands", "own_account_profile_command", "ALL", "(account_id = app.current_account_id())", "(account_id = app.current_account_id())"},
		{"accounts", "account_authority", "ALL", authority, authority},
		{"accounts", "account_context", "SELECT", "(app.authority_context() OR (id = app.current_account_id()))", ""},
		{"accounts", "account_tenant_update", "UPDATE", "(app.authority_context() OR (id = app.current_account_id()))", "(app.authority_context() OR (id = app.current_account_id()))"},
		{"devices", "device_context", "ALL", tenantAccount, authority},
		{"families", "family_authority", "ALL", authority, authority},
		{"families", "family_context", "SELECT", "(app.authority_context() OR (id = app.current_family_id()))", ""},
		{"families", "family_tenant_update", "UPDATE", "(app.authority_context() OR (id = app.current_family_id()))", "(app.authority_context() OR (id = app.current_family_id()))"},
		{"families", "maintenance_counters", "UPDATE", "true", "true"}, // 000031: the maintenance group only (request_path_schema_test.go)
		{"families", "maintenance_read", "SELECT", "true", ""},
		{"family_actor_label_history", "actor_label_history_family_context", "ALL", "(family_id = app.current_family_id())", "(family_id = app.current_family_id())"},
		{"family_actor_labels", "actor_label_family_context", "ALL", "(family_id = app.current_family_id())", "(family_id = app.current_family_id())"},
		{"family_entitlements", "entitlement_context", "ALL", tenantFamily, authority},
		{"family_recovery_claims", "claim_context", "ALL", tenantAccount, authority},
		{"family_slots", "family_slot_authority", "ALL", authority, authority},
		{"family_slots", "own_family_slot", "SELECT", tenantAccount, ""},
		{"invitations", "invitation_context", "ALL", tenantFamily, authority},
		{"memberships", "membership_context", "ALL", tenantFamily, authority},
	}
	rows, err := fixture.Owner.Query(t.Context(), `SELECT tablename,policyname,cmd,coalesce(qual,''),coalesce(with_check,'')
		FROM pg_policies WHERE schemaname='public' AND tablename = ANY($1) ORDER BY tablename,policyname`, append(familyTables, "devices"))
	if err != nil {
		t.Fatal(err)
	}
	var got []policy
	for rows.Next() {
		var item policy
		if err := rows.Scan(&item.table, &item.name, &item.command, &item.using, &item.check); err != nil {
			t.Fatal(err)
		}
		got = append(got, item)
	}
	rows.Close()
	if len(got) != len(want) {
		t.Fatalf("policies=%v want=%v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("policy[%d]=%v want=%v", index, got[index], want[index])
		}
	}
	for _, table := range append(familyTables, "devices") {
		var enabled, forced bool
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid=($1::text)::regclass`, "public."+table).Scan(&enabled, &forced); err != nil {
			t.Fatal(err)
		}
		if !enabled || !forced {
			t.Fatalf("%s RLS enabled=%t forced=%t", table, enabled, forced)
		}
	}
	var material bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT to_regclass('public.family_command_material') IS NOT NULL`).Scan(&material); err != nil || material {
		t.Fatalf("family_command_material survived=%t err=%v", material, err)
	}
}

func TestFamilySchemaHasExactTablesPoliciesAndPrivileges(t *testing.T) {
	fixture := testpg.New(t)
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 37); err != nil {
		t.Fatalf("schema-30 exact privilege snapshot rejected: %v", err)
	}
	if got := tableSet(t, fixture, familyTables); !equalStrings(got, sortedCopy(familyTables)) {
		t.Fatalf("family tables=%v want=%v", got, familyTables)
	}
	for table, want := range familyTableColumns {
		t.Run(table+" columns", func(t *testing.T) {
			if got := tableColumns(t, fixture, table); !equalStrings(got, want) {
				t.Fatalf("columns=%v want=%v", got, want)
			}
		})
	}
	var profileRequired string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT is_nullable||':'||column_default FROM information_schema.columns
		WHERE table_schema='public' AND table_name='families' AND column_name='profile_required'`).Scan(&profileRequired); err != nil {
		t.Fatal(err)
	}
	if profileRequired != "NO:false" {
		t.Fatalf("families.profile_required contract=%q", profileRequired)
	}
	for _, signature := range []string{membershipMayWriteSignature, "app.update_own_profile(uuid,uuid,text,timestamp with time zone)",
		"app.list_family_actor_labels(uuid,bigint,uuid,integer)", "app.family_actor_labels_high_water()"} {
		assertFunctionPrivilege(t, fixture.Runtime, signature, true)
		assertFunctionPrivilege(t, fixture.Maintenance, signature, false)
		assertPublicCannotExecute(t, fixture, signature)
	}
	// Presentation tables: the runtime reads active labels only; label and
	// profile-receipt writes go through the definers.
	for table, want := range map[string]string{"family_actor_labels": "SELECT", "family_actor_label_history": "", "account_profile_commands": ""} {
		var privileges string
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT coalesce(string_agg(p,',' ORDER BY p),'') FROM unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE']) p
			WHERE has_table_privilege('baby_tracker_runtime',$1,p)`, "public."+table).Scan(&privileges); err != nil {
			t.Fatal(err)
		}
		if privileges != want {
			t.Fatalf("runtime %s privileges=%q want=%q", table, privileges, want)
		}
	}
}

// Structural family rows are written only by authority transactions: a tenant
// runtime transaction cannot DELETE them (no grant, or an authority-only
// DELETE policy for family_slots), and a family or account context reads none
// of another family's or account's rows, devices and recovery claims included
// (Task 1 deferred minor: TestAuthorityRLSShape seeded neither for account B).
func TestFamilyStructuralTablesDenyDirectWritesAndCrossFamilyReads(t *testing.T) {
	fixture := testpg.New(t)
	ids := seedAuthorityFamilies(t, fixture)
	now := time.Now().UTC().Truncate(time.Second)
	for _, seed := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO devices(id,account_id,canonical_origin,continuity_key_id,continuity_digest,created_at,updated_at)
			VALUES ('63000000-0000-4000-8000-000000000002',$1,'https://tracker.example','continuity.v1',$2,$3,$3)`, []any{ids.accountB, authorityDigest(8), now}},
		{`INSERT INTO family_recovery_claims(family_id,account_id,status) VALUES ($1,$2,'active')`, []any{ids.familyB, ids.accountB}},
		{`SELECT app.authority_actor_label($1,$2,false,$3)`, []any{ids.familyB, ids.accountB, now}},
	} {
		if err := authorityExec(t.Context(), fixture.Owner, seed.sql, seed.args...); err != nil {
			t.Fatalf("seed %q: %v", seed.sql, err)
		}
	}
	for _, table := range []string{"accounts", "families", "memberships", "invitations", "family_entitlements", "family_recovery_claims", "devices",
		"account_profile_commands", "family_actor_labels", "family_actor_label_history", "family_slots"} {
		t.Run("delete/"+table, func(t *testing.T) {
			for _, account := range []string{ids.accountA, ids.accountB} {
				family := ids.familyA
				if account == ids.accountB {
					family = ids.familyB
				}
				tx := tenantTx(t, fixture.Runtime, account, family)
				result, err := tx.Exec(t.Context(), "DELETE FROM public."+table)
				_ = tx.Rollback(t.Context())
				if table == "family_slots" {
					if err != nil || result.RowsAffected() != 0 {
						t.Fatalf("tenant slot delete rows=%d err=%v", result.RowsAffected(), err)
					}
					continue
				}
				if sqlState(err) != "42501" {
					t.Fatalf("tenant delete err=%v", err)
				}
			}
		})
	}
	tx := tenantTx(t, fixture.Runtime, ids.accountA, ids.familyA)
	for table, query := range map[string]string{
		"families":               "SELECT count(*) FROM families WHERE id=$1",
		"memberships":            "SELECT count(*) FROM memberships WHERE family_id=$1",
		"invitations":            "SELECT count(*) FROM invitations WHERE family_id=$1",
		"family_entitlements":    "SELECT count(*) FROM family_entitlements WHERE family_id=$1",
		"family_actor_labels":    "SELECT count(*) FROM family_actor_labels WHERE family_id=$1",
		"family_recovery_claims": "SELECT count(*) FROM family_recovery_claims WHERE family_id=$1",
	} {
		var foreign int
		if err := tx.QueryRow(t.Context(), query, ids.familyB).Scan(&foreign); err != nil || foreign != 0 {
			t.Fatalf("family A context reads %d %s rows of family B err=%v", foreign, table, err)
		}
	}
	for table, column := range map[string]string{"accounts": "id", "family_slots": "account_id", "devices": "account_id", "family_recovery_claims": "account_id"} {
		var foreign int
		if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM "+table+" WHERE "+column+"=$1", ids.accountB).Scan(&foreign); err != nil || foreign != 0 {
			t.Fatalf("account A context reads %d %s rows of account B err=%v", foreign, table, err)
		}
	}
	// Positive control: account B's own context sees its device and claim.
	own := tenantTx(t, fixture.Runtime, ids.accountB, ids.familyB)
	var devices, claims int
	if err := own.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM devices), (SELECT count(*) FROM family_recovery_claims)").Scan(&devices, &claims); err != nil || devices != 1 || claims != 1 {
		t.Fatalf("account B own devices=%d claims=%d err=%v", devices, claims, err)
	}
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
