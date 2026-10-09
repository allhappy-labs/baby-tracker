package database_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"babytracker/server/internal/security"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
)

func TestFamilyProjectionErasesFormerNameAndAllowsCaregiverRejoin(t *testing.T) {
	fixture := testpg.New(t)
	const (
		owner       = "10000000-0000-4000-8000-000000000001"
		caregiver   = "10000000-0000-4000-8000-000000000002"
		family      = "20000000-0000-4000-8000-000000000001"
		invitation  = "60000000-0000-4000-8000-000000000001"
		tokenDigest = "hmac-sha256:invite.v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	setup, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), setup, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Owner')", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true)", caregiver); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Current caregiver')", caregiver); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true)", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name) VALUES($1,'Family')", family); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO memberships(family_id,account_id,role) VALUES
		($1,$2,'owner'),($1,$3,'caregiver')`, family, owner, caregiver); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true)", caregiver); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO family_slots(account_id,family_id,state) VALUES($1,$2,'active')", caregiver, family); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO family_actor_labels(family_id,account_id,display_name,created_at,updated_at)
		VALUES($1,$2,'Sensitive latest','2026-09-20T10:00:00Z','2026-09-20T11:00:00Z')`, family, caregiver); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO family_actor_label_history(family_id,account_id,display_name,former,recorded_at) VALUES
		($1,$2,'Sensitive old',false,'2026-09-20T10:00:00Z'),
		($1,$2,'Sensitive latest',false,'2026-09-20T11:00:00Z')`, family, caregiver); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "SELECT set_config('app.account_id',$1,true)", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO invitations(id,family_id,creator_account_id,token_digest,role,status,expires_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,'caregiver','active','2026-09-22T12:00:00Z','2026-09-20T12:00:00Z','2026-09-20T12:00:00Z')`, invitation, family, owner, tokenDigest); err != nil {
		t.Fatal(err)
	}
	if err := setup.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	removed := familyProjectionCandidate(t, security.Head{Generation: 1}, owner,
		"30000000-0000-4000-8000-000000000001", "member_removed", []security.Effect{{
			Kind: "member_removed", AccountID: caregiver, FamilyID: family, Status: "removed",
		}}, time.Date(2026, 9, 20, 12, 1, 0, 0, time.UTC))
	projector := security.NewProjector(fixture.Projector)
	if err := projector.Apply(t.Context(), removed); err != nil {
		t.Fatalf("project removal: %v", err)
	}
	read, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), read, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	var named, active, historyRows int
	if err := read.QueryRow(t.Context(), `SELECT
		count(*) FILTER (WHERE display_name<>'Former caregiver' OR former=false),
		count(*) FILTER (WHERE former=true),count(*)
		FROM family_actor_label_history WHERE family_id=$1 AND account_id=$2`, family, caregiver).
		Scan(&named, &active, &historyRows); err != nil {
		t.Fatal(err)
	}
	if named != 0 || active != 3 || historyRows != 3 {
		t.Fatalf("removed history named=%d former=%d rows=%d", named, active, historyRows)
	}
	if err := read.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	redeemed := familyProjectionCandidate(t, security.Head{Generation: 1, Revision: 1, EventHash: removed.EventHash}, caregiver,
		"30000000-0000-4000-8000-000000000002", "invitation_redeemed", []security.Effect{{
			Kind: "invitation_redeemed", AccountID: caregiver, FamilyID: family, InvitationID: invitation,
			TokenDigest: tokenDigest, Role: "caregiver", Status: "redeemed", ExpiresAt: "2026-09-22T12:00:00Z",
		}}, time.Date(2026, 9, 20, 12, 2, 0, 0, time.UTC))
	if err := projector.Apply(t.Context(), redeemed); err != nil {
		t.Fatalf("project rejoin: %v", err)
	}
	verify, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), verify, tenant.Context{AccountID: caregiver, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	var membershipStatus, labelName, slotState string
	if err := verify.QueryRow(t.Context(), `SELECT m.status,l.display_name,s.state FROM memberships m
		JOIN family_actor_labels l USING(family_id,account_id) JOIN family_slots s USING(account_id,family_id)
		WHERE m.family_id=$1 AND m.account_id=$2`, family, caregiver).Scan(&membershipStatus, &labelName, &slotState); err != nil {
		t.Fatal(err)
	}
	if membershipStatus != "active" || labelName != "Current caregiver" || slotState != "active" {
		t.Fatalf("rejoin membership=%q label=%q slot=%q", membershipStatus, labelName, slotState)
	}
	var leaked, formerRows, currentRows int
	if err := verify.QueryRow(t.Context(), `SELECT
		count(*) FILTER (WHERE display_name LIKE 'Sensitive%'),
		count(*) FILTER (WHERE display_name='Former caregiver' AND former=true),
		count(*) FILTER (WHERE display_name='Current caregiver' AND former=false)
		FROM family_actor_label_history WHERE family_id=$1 AND account_id=$2`, family, caregiver).
		Scan(&leaked, &formerRows, &currentRows); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 || formerRows != 3 || currentRows != 1 {
		t.Fatalf("rejoined history leaked=%d former=%d current=%d", leaked, formerRows, currentRows)
	}
}

func TestFamilyProfileRepairReplayWithoutStagingPreservesRepairRequirement(t *testing.T) {
	fixture := testpg.New(t)
	const (
		owner  = "10000000-0000-4000-8000-000000000001"
		family = "20000000-0000-4000-8000-000000000001"
	)
	setup, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), setup, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Owner')", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name,profile_required) VALUES($1,'',true)", family); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", family, owner); err != nil {
		t.Fatal(err)
	}
	if err := setup.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	event := familyProjectionCandidate(t, security.Head{Generation: 1}, owner,
		"30000000-0000-4000-8000-000000000001", "family_profile_repaired", []security.Effect{{
			Kind: "family_profile_repaired", AccountID: owner, FamilyID: family,
			ProjectionMaterialDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}}, time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC))
	projector := security.NewProjector(fixture.Projector)
	if err := projector.Apply(t.Context(), event); err != nil {
		t.Fatalf("replay missing repair stage: %v", err)
	}
	read, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), read, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	var name string
	var required bool
	if err := read.QueryRow(t.Context(), "SELECT display_name,profile_required FROM families WHERE id=$1", family).Scan(&name, &required); err != nil {
		t.Fatal(err)
	}
	if name != "" || !required {
		t.Fatalf("replayed family name=%q profile_required=%t", name, required)
	}
	if head, err := projector.Head(t.Context()); err != nil || head.Revision != 1 || head.EventHash != event.EventHash {
		t.Fatalf("replayed head=%+v err=%v", head, err)
	}
}

func familyProjectionCandidate(t *testing.T, prior security.Head, actor, commandID string, kind security.EventKind, effects []security.Effect, at time.Time) security.Candidate {
	t.Helper()
	event := security.Event{
		Version: "v1", Generation: prior.Generation, Revision: prior.Revision + 1,
		PreviousHash: hex.EncodeToString(prior.EventHash[:]), CommandID: commandID,
		CommandHash:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ActorAccountID: actor, ActorDeviceID: "", Kind: kind, At: at.Format(time.RFC3339Nano), Effects: effects,
	}
	encoded, err := security.EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	return security.Candidate{CommandID: commandID, Prior: prior, EventJSON: encoded, EventHash: sha256.Sum256(encoded)}
}
