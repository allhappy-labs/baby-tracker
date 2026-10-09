package database_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
)

var familyTables = []string{
	"account_profile_commands", "family_actor_label_history", "family_actor_labels", "family_command_material", "family_entitlements", "family_slots", "invitations",
}

func TestStageFamilyMaterialRequiresCanonicalDigestActorAndImmutableRetry(t *testing.T) {
	fixture := testpg.New(t)
	const (
		accountID = "10000000-0000-4000-8000-000000000001"
		familyID  = "20000000-0000-4000-8000-000000000001"
		commandID = "30000000-0000-4000-8000-000000000001"
	)
	material := []byte(`{"version":"v1","kind":"family_create","family_id":"` + familyID + `","actor_account_id":"` + accountID + `","display_name":"Household"}`)
	digest := sha256.Sum256(material)
	commandHash := sha256.Sum256([]byte("command"))
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	call := func(command, actorMaterial string, materialDigest []byte, at time.Time) error {
		tx, err := fixture.Runtime.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(t.Context())
		if err := tenant.SetAccountLocal(t.Context(), tx, accountID); err != nil {
			return err
		}
		payload := material
		if actorMaterial != accountID {
			payload = []byte(`{"version":"v1","kind":"family_create","family_id":"` + familyID + `","actor_account_id":"` + actorMaterial + `","display_name":"Household"}`)
		}
		if materialDigest == nil {
			sum := sha256.Sum256(payload)
			materialDigest = sum[:]
		}
		if _, err := tx.Exec(t.Context(), "SELECT app.stage_family_material($1,$2,$3,$4,$5)", command, commandHash[:], materialDigest, payload, at); err != nil {
			return err
		}
		return tx.Commit(t.Context())
	}
	if err := call(commandID, accountID, digest[:], now); err != nil {
		t.Fatalf("canonical family material rejected: %v", err)
	}
	if err := call(commandID, accountID, digest[:], now.Add(time.Minute)); err != nil {
		t.Fatalf("identical unprojected retry rejected: %v", err)
	}
	badDigest := sha256.Sum256([]byte("wrong"))
	if err := call("30000000-0000-4000-8000-000000000002", accountID, badDigest[:], now); err == nil {
		t.Fatal("material digest mismatch accepted")
	}
	if err := call("30000000-0000-4000-8000-000000000003", "10000000-0000-4000-8000-000000000002", nil, now); err == nil {
		t.Fatal("cross-account family material accepted")
	}
	for index, encodedName := range []string{"   ", `\u0001Hidden`} {
		payload := []byte(`{"version":"v1","kind":"family_create","family_id":"` + familyID + `","actor_account_id":"` + accountID + `","display_name":"` + encodedName + `"}`)
		digest := sha256.Sum256(payload)
		commandHash := sha256.Sum256([]byte(encodedName))
		tx, err := fixture.Runtime.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := tenant.SetAccountLocal(t.Context(), tx, accountID); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(t.Context(), "SELECT app.stage_family_material($1,$2,$3,$4,$5)",
			fmt.Sprintf("30000000-0000-4000-8000-%012d", index+4), commandHash[:], digest[:], payload, now)
		tx.Rollback(t.Context())
		if sqlState(err) != "22023" {
			t.Fatalf("invalid family display name %q err=%v state=%q", encodedName, err, sqlState(err))
		}
	}
	direct, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), direct, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := direct.Exec(t.Context(), "SELECT count(*) FROM family_command_material"); err == nil || sqlState(err) != "42501" {
		t.Fatalf("runtime direct material read err=%v", err)
	}
	read, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback(t.Context())
	if _, err := read.Exec(t.Context(), "SELECT set_config('app.family_id',$1,true)", familyID); err != nil {
		t.Fatal(err)
	}
	var stored []byte
	var createdAt, expiresAt time.Time
	if err := read.QueryRow(t.Context(), `SELECT material_bytes,created_at,expires_at
		FROM family_command_material WHERE command_id=$1`, commandID).Scan(&stored, &createdAt, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(material) || !createdAt.Equal(now) || !expiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("stored family material contract mismatch")
	}
}

func TestInvitationPreviewAllowsAuthenticatedOutsiderWithoutTenantLeak(t *testing.T) {
	fixture := testpg.New(t)
	const (
		ownerID         = "10000000-0000-4000-8000-000000000001"
		outsiderID      = "10000000-0000-4000-8000-000000000002"
		familyID        = "20000000-0000-4000-8000-000000000001"
		otherFamilyID   = "20000000-0000-4000-8000-000000000002"
		invitationID    = "60000000-0000-4000-8000-000000000001"
		invitationToken = "hmac-sha256:invite.v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	setup, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Rollback(t.Context())
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true),set_config('app.family_id',$2,true)", ownerID, familyID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Owner')", ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true)", outsiderID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Outsider')", outsiderID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true)", ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name) VALUES($1,'Safe household')", familyID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", familyID, ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO invitations(id,family_id,creator_account_id,token_digest,role,status,expires_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,'caregiver','active','2026-09-27T12:00:00Z','2026-09-20T12:00:00Z','2026-09-20T12:00:00Z')`, invitationID, familyID, ownerID, invitationToken); err != nil {
		t.Fatal(err)
	}
	if err := setup.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, familyContext string
	}{
		{name: "no family"},
		{name: "different family", familyContext: otherFamilyID},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := fixture.Runtime.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err := tx.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true),set_config('app.family_id',$2,true)", outsiderID, test.familyContext); err != nil {
				t.Fatal(err)
			}
			var gotInvitation, gotFamily, gotName string
			if err := tx.QueryRow(t.Context(), `SELECT invitation_id::text,family_id::text,family_display_name
				FROM app.lookup_family_invitation($1)`, invitationToken).Scan(&gotInvitation, &gotFamily, &gotName); err != nil {
				t.Fatalf("authenticated outsider preview: %v", err)
			}
			if gotInvitation != invitationID || gotFamily != familyID || gotName != "Safe household" {
				t.Fatalf("preview invitation=%q family=%q name=%q", gotInvitation, gotFamily, gotName)
			}
			var invites, families int
			if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM invitations").Scan(&invites); err != nil {
				t.Fatal(err)
			}
			if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM families").Scan(&families); err != nil {
				t.Fatal(err)
			}
			if invites != 0 || families != 0 {
				t.Fatalf("outsider direct rows invitations=%d families=%d", invites, families)
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
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Before')", accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name) VALUES($1,'Active')", familyID); err != nil {
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

func TestFamilyProfileRepairStagingIsSingleFlightAndFailClosed(t *testing.T) {
	fixture := testpg.New(t)
	const (
		accountID      = "10000000-0000-4000-8000-000000000001"
		familyID       = "20000000-0000-4000-8000-000000000001"
		otherFamilyID  = "20000000-0000-4000-8000-000000000002"
		firstCommand   = "30000000-0000-4000-8000-000000000001"
		secondCommand  = "30000000-0000-4000-8000-000000000002"
		expiredCommand = "30000000-0000-4000-8000-000000000003"
	)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	setup, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), setup, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Owner')", accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name,profile_required) VALUES($1,'',true)", familyID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", familyID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.family_id',$1,true)", otherFamilyID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name,profile_required) VALUES($1,'Ready',false)", otherFamilyID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", otherFamilyID, accountID); err != nil {
		t.Fatal(err)
	}
	if err := setup.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	stage := func(commandID, targetFamily, name string, at time.Time) error {
		material := []byte(`{"version":"v1","kind":"family_profile_repair","family_id":"` + targetFamily + `","actor_account_id":"` + accountID + `","display_name":"` + name + `"}`)
		digest := sha256.Sum256(material)
		commandHash := sha256.Sum256([]byte(commandID))
		tx, err := fixture.Runtime.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(t.Context())
		if err := tenant.SetAccountLocal(t.Context(), tx, accountID); err != nil {
			return err
		}
		if _, err := tx.Exec(t.Context(), "SELECT app.stage_family_material($1,$2,$3,$4,$5)", commandID, commandHash[:], digest[:], material, at); err != nil {
			return err
		}
		return tx.Commit(t.Context())
	}
	if err := stage(firstCommand, familyID, "Recovered", now); err != nil {
		t.Fatalf("first repair stage: %v", err)
	}
	if err := stage(firstCommand, familyID, "Recovered", now.Add(time.Minute)); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	if err := stage(secondCommand, familyID, "Racing", now.Add(time.Minute)); sqlState(err) != "BT001" {
		t.Fatalf("second pending repair err=%v state=%q", err, sqlState(err))
	}
	if err := stage(secondCommand, otherFamilyID, "Not needed", now.Add(time.Minute)); sqlState(err) != "BT001" {
		t.Fatalf("unneeded repair err=%v state=%q", err, sqlState(err))
	}
	if err := stage(expiredCommand, familyID, "After expiry", now.Add(11*time.Minute)); err != nil {
		t.Fatalf("expired reservation replacement: %v", err)
	}

	read, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback(t.Context())
	if _, err := read.Exec(t.Context(), "SELECT set_config('app.family_id',$1,true)", familyID); err != nil {
		t.Fatal(err)
	}
	var count int
	var commandID string
	if err := read.QueryRow(t.Context(), `SELECT count(*),min(command_id::text) FROM family_command_material
		WHERE family_id=$1 AND material_kind='family_profile_repair' AND projected_at IS NULL`, familyID).Scan(&count, &commandID); err != nil {
		t.Fatal(err)
	}
	if count != 1 || commandID != expiredCommand {
		t.Fatalf("pending repairs=%d command=%q", count, commandID)
	}
	var indexDefinition string
	if err := read.QueryRow(t.Context(), `SELECT indexdef FROM pg_indexes WHERE schemaname='public'
		AND indexname='family_command_material_one_pending_repair_idx'`).Scan(&indexDefinition); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(indexDefinition, "UNIQUE INDEX") ||
		!strings.Contains(indexDefinition, "WHERE ((material_kind = 'family_profile_repair'::text) AND (projected_at IS NULL))") {
		t.Fatalf("repair reservation index=%q", indexDefinition)
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
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name) VALUES($1,'Family')", familyID); err != nil {
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

func TestFamilyPoliciesAreExactAndCommandMaterialIsIsolated(t *testing.T) {
	fixture := testpg.New(t)
	type policy struct{ table, name, using, check string }
	want := []policy{
		{"account_profile_commands", "own_account_profile_command", "(account_id = app.current_account_id())", "(account_id = app.current_account_id())"},
		{"family_actor_label_history", "actor_label_history_family_context", "(family_id = app.current_family_id())", "(family_id = app.current_family_id())"},
		{"family_actor_labels", "actor_label_family_context", "(family_id = app.current_family_id())", "(family_id = app.current_family_id())"},
		{"family_command_material", "family_material_context", "(family_id = app.current_family_id())", "(family_id = app.current_family_id())"},
		{"family_entitlements", "entitlement_family_context", "(family_id = app.current_family_id())", "(family_id = app.current_family_id())"},
		{"family_slots", "own_family_slot", "(account_id = app.current_account_id())", "(account_id = app.current_account_id())"},
		{"invitations", "invitation_family_context", "(family_id = app.current_family_id())", "(family_id = app.current_family_id())"},
	}
	rows, err := fixture.Owner.Query(t.Context(), `SELECT tablename,policyname,coalesce(qual,''),coalesce(with_check,'')
		FROM pg_policies WHERE schemaname='public' AND tablename = ANY($1)
		AND policyname <> 'invitation_preview_owner' ORDER BY tablename,policyname`, familyTables)
	if err != nil {
		t.Fatal(err)
	}
	var got []policy
	for rows.Next() {
		var item policy
		if err := rows.Scan(&item.table, &item.name, &item.using, &item.check); err != nil {
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
	var previewUsing, previewCheck string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT coalesce(qual,''),coalesce(with_check,'') FROM pg_policies
		WHERE schemaname='public' AND tablename='invitations' AND policyname='invitation_preview_owner'`).Scan(&previewUsing, &previewCheck); err != nil {
		t.Fatal(err)
	}
	const wantPreviewUsing = "((app.current_account_id() IS NOT NULL) AND (((CURRENT_USER)::regrole)::oid = ( SELECT pg_class.relowner\n   FROM pg_class\n  WHERE (pg_class.oid = ('invitations'::regclass)::oid))))"
	if previewUsing != wantPreviewUsing || previewCheck != "" {
		t.Fatalf("preview policy using=%q check=%q", previewUsing, previewCheck)
	}
	for _, table := range familyTables {
		if table == "family_command_material" {
			continue
		}
		var enabled, forced bool
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid=($1::text)::regclass`, "public."+table).Scan(&enabled, &forced); err != nil {
			t.Fatal(err)
		}
		if !enabled || !forced {
			t.Fatalf("%s RLS enabled=%t forced=%t", table, enabled, forced)
		}
	}
	var enabled, forced bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT relrowsecurity,relforcerowsecurity FROM pg_class WHERE oid='public.family_command_material'::regclass`).Scan(&enabled, &forced); err != nil {
		t.Fatal(err)
	}
	if !enabled || !forced {
		t.Fatalf("family_command_material RLS enabled=%t forced=%t", enabled, forced)
	}
}

var familyTableColumns = map[string][]string{
	"account_profile_commands":   {"account_id", "command_id", "input_hash", "created_at"},
	"family_actor_label_history": {"label_version", "family_id", "account_id", "display_name", "former", "recorded_at"},
	"family_slots":               {"account_id", "family_id", "state"},
	"invitations":                {"id", "family_id", "creator_account_id", "token_digest", "role", "status", "expires_at", "redeemed_by_account_id", "redeemed_at", "revoked_at", "created_at", "updated_at"},
	"family_entitlements":        {"family_id", "entitlement_key", "source", "status", "expires_at", "created_at", "updated_at"},
	"family_actor_labels":        {"family_id", "account_id", "display_name", "former", "created_at", "updated_at"},
	"family_command_material":    {"command_id", "command_hash", "projection_material_digest", "material_kind", "family_id", "account_id", "family_display_name", "material_bytes", "created_at", "expires_at", "projected_at"},
}

// The schema-fourteen read-only caregiver authority. Runtime-only: the
// projector and apple-revoker roles must never be able to ask whether a
// membership may write.
const membershipMayWriteSignature = "app.membership_may_write(uuid,uuid)"

func TestFamilySchemaHasExactTablesPoliciesAndPrivileges(t *testing.T) {
	fixture := testpg.New(t)
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 29); err != nil {
		t.Fatalf("schema-twenty exact privilege snapshot rejected: %v", err)
	}
	rows, err := fixture.Owner.Query(t.Context(), `SELECT tablename FROM pg_tables
		WHERE schemaname='public' AND tablename = ANY($1) ORDER BY tablename`, familyTables)
	if err != nil {
		t.Fatal(err)
	}
	var gotTables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		gotTables = append(gotTables, table)
	}
	rows.Close()
	sort.Strings(familyTables)
	if !equalStrings(gotTables, familyTables) {
		t.Fatalf("family tables=%v want=%v", gotTables, familyTables)
	}
	for table, want := range familyTableColumns {
		t.Run(table+" columns", func(t *testing.T) {
			rows, err := fixture.Owner.Query(t.Context(), `SELECT column_name FROM information_schema.columns
				WHERE table_schema='public' AND table_name=$1 ORDER BY ordinal_position`, table)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for rows.Next() {
				var column string
				if err := rows.Scan(&column); err != nil {
					t.Fatal(err)
				}
				got = append(got, column)
			}
			rows.Close()
			if !equalStrings(got, want) {
				t.Fatalf("columns=%v want=%v", got, want)
			}
		})
	}
	for _, table := range []string{"account_profile_commands", "family_slots", "invitations", "family_entitlements", "family_actor_labels", "family_actor_label_history"} {
		var enabled, forced bool
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT relrowsecurity,relforcerowsecurity
			FROM pg_class WHERE oid=($1::text)::regclass`, "public."+table).Scan(&enabled, &forced); err != nil {
			t.Fatal(err)
		}
		if !enabled || !forced {
			t.Fatalf("%s RLS enabled=%t forced=%t", table, enabled, forced)
		}
	}
	var profileRequired string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT is_nullable||':'||column_default FROM information_schema.columns
		WHERE table_schema='public' AND table_name='families' AND column_name='profile_required'`).Scan(&profileRequired); err != nil {
		t.Fatal(err)
	}
	if profileRequired != "NO:false" {
		t.Fatalf("families.profile_required contract=%q", profileRequired)
	}
	for _, signature := range familyRuntimeFunctions {
		assertFunctionPrivilege(t, fixture.Runtime, signature, true)
		assertFunctionPrivilege(t, fixture.Projector, signature, false)
		assertPublicCannotExecute(t, fixture, signature)
	}
	var projectorDefinition string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT pg_get_functiondef(
		'app.project_security_event_v6(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)'::regprocedure)`).Scan(&projectorDefinition); err != nil {
		t.Fatal(err)
	}
	for _, familyCase := range []string{"family_bootstrapped", "family_profile_repaired", "invitation_created", "invitation_redeemed", "owner_transferred"} {
		if !strings.Contains(projectorDefinition, familyCase) {
			t.Fatalf("schema-six projector omits %s", familyCase)
		}
	}
	rows, err = fixture.Owner.Query(t.Context(), `SELECT object_identity FROM schema_privilege_manifest WHERE schema_version=6
		AND principal='baby_tracker_runtime' AND object_kind='function'
		EXCEPT SELECT object_identity FROM schema_privilege_manifest WHERE schema_version=5
		AND principal='baby_tracker_runtime' AND object_kind='function' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var added []string
	for rows.Next() {
		var signature string
		if err := rows.Scan(&signature); err != nil {
			t.Fatal(err)
		}
		added = append(added, signature)
	}
	rows.Close()
	if !equalStrings(added, familyRuntimeFunctions) {
		t.Fatalf("schema-six runtime function delta=%v want=%v", added, familyRuntimeFunctions)
	}
}

func TestFamilyStructuralTablesDenyDirectWritesAndCrossFamilyReads(t *testing.T) {
	fixture := testpg.New(t)
	for _, table := range []string{"families", "memberships", "account_profile_commands", "family_slots", "invitations", "family_entitlements", "family_actor_labels", "family_actor_label_history", "family_command_material"} {
		for _, pool := range []struct {
			name string
			exec func(context.Context, string, ...any) (pgconn.CommandTag, error)
		}{{"runtime", fixture.Runtime.Exec}, {"projector", fixture.Projector.Exec}} {
			t.Run(pool.name+"/"+table, func(t *testing.T) {
				_, err := pool.exec(t.Context(), "DELETE FROM public."+table)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
					t.Fatalf("error=%v, want SQLSTATE 42501", err)
				}
			})
		}
	}
	const accountID = "10000000-0000-4000-8000-000000000001"
	const otherFamilyID = "20000000-0000-4000-8000-000000000002"
	ownerTx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer ownerTx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), ownerTx, tenant.Context{AccountID: accountID, FamilyID: otherFamilyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerTx.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'owner')", accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerTx.Exec(t.Context(), "INSERT INTO families(id,display_name) VALUES($1,'other')", otherFamilyID); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerTx.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", otherFamilyID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerTx.Exec(t.Context(), "INSERT INTO family_slots(account_id,family_id,state) VALUES($1,$2,'active')", accountID, otherFamilyID); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerTx.Exec(t.Context(), `INSERT INTO invitations(id,family_id,creator_account_id,token_digest,role,status,expires_at,created_at,updated_at)
		VALUES('60000000-0000-4000-8000-000000000001',$1,$2,'hmac-sha256:invite.v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
		'caregiver','active','2026-09-27T12:00:00Z','2026-09-20T12:00:00Z','2026-09-20T12:00:00Z')`, otherFamilyID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerTx.Exec(t.Context(), `INSERT INTO family_entitlements(family_id,entitlement_key,source,status,created_at,updated_at)
		VALUES($1,'family_sync','hosted_beta','active','2026-09-20T12:00:00Z','2026-09-20T12:00:00Z')`, otherFamilyID); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerTx.Exec(t.Context(), `INSERT INTO family_actor_labels(family_id,account_id,display_name,created_at,updated_at)
		VALUES($1,$2,'Owner','2026-09-20T12:00:00Z','2026-09-20T12:00:00Z')`, otherFamilyID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerTx.Exec(t.Context(), `INSERT INTO family_command_material(command_id,command_hash,projection_material_digest,material_kind,family_id,account_id,
		family_display_name,material_bytes,created_at,expires_at)
		VALUES('30000000-0000-4000-8000-000000000001',decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),'family_create',$1,$2,
		'Other',decode('7b','hex'),'2026-09-20T12:00:00Z','2026-09-20T12:10:00Z')`, otherFamilyID, accountID); err != nil {
		t.Fatal(err)
	}
	if err := ownerTx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: "20000000-0000-4000-8000-000000000001"}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"families", "invitations", "family_entitlements", "family_actor_labels"} {
		var count int
		if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("cross-family %s rows=%d err=%v", table, count, err)
		}
	}
	var count int
	if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM family_slots").Scan(&count); err != nil || count != 1 {
		t.Fatalf("own slot rows=%d err=%v", count, err)
	}
	if _, err := tx.Exec(t.Context(), "SELECT set_config('app.account_id','10000000-0000-4000-8000-000000000002',true)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM family_slots").Scan(&count); err != nil || count != 0 {
		t.Fatalf("cross-account slot rows=%d err=%v", count, err)
	}
	if _, err := tx.Exec(t.Context(), "SELECT count(*) FROM family_command_material"); err == nil || sqlState(err) != "42501" {
		t.Fatalf("runtime material read err=%v", err)
	}

	ownerRead, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer ownerRead.Rollback(t.Context())
	if _, err := ownerRead.Exec(t.Context(), "SELECT set_config('app.family_id','20000000-0000-4000-8000-000000000001',true)"); err != nil {
		t.Fatal(err)
	}
	if err := ownerRead.QueryRow(t.Context(), "SELECT count(*) FROM family_command_material").Scan(&count); err != nil || count != 0 {
		t.Fatalf("cross-family material rows=%d err=%v", count, err)
	}
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
