package database_test

import (
	"errors"
	"testing"
	"time"

	"babytracker/server/internal/security"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRecoveryClaimForfeitAllowsLaterFamilyDeletionForSameOwner(t *testing.T) {
	fixture := testpg.New(t)
	const (
		owner     = "10000000-0000-4000-8000-000000000001"
		caregiver = "10000000-0000-4000-8000-000000000002"
		familyOne = "20000000-0000-4000-8000-000000000001"
		familyTwo = "20000000-0000-4000-8000-000000000002"
	)
	prior := seedLifecycleFamily(t, fixture, owner, caregiver, familyOne)
	projector := security.NewProjector(fixture.Projector)
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	scheduledOne := familyProjectionCandidate(t, prior, owner, "30000000-0000-4000-8000-000000000002", "family_deletion_scheduled", []security.Effect{
		{Kind: "family_deletion_scheduled", AccountID: owner, FamilyID: familyOne, Role: "owner", Status: "deletion_pending", PurgeAfter: at.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)},
		{Kind: "family_deletion_scheduled", AccountID: caregiver, FamilyID: familyOne, Role: "caregiver", Status: "former", PurgeAfter: at.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)},
	}, at)
	if err := projector.Apply(t.Context(), scheduledOne); err != nil {
		t.Fatalf("schedule first family: %v", err)
	}
	forfeited := familyProjectionCandidate(t, security.Head{Generation: 1, Revision: 2, EventHash: scheduledOne.EventHash}, owner,
		"30000000-0000-4000-8000-000000000003", "recovery_forfeited", []security.Effect{{
			Kind: "recovery_forfeited", AccountID: owner, FamilyID: familyOne, Status: "forfeited",
		}}, at.Add(time.Minute))
	if err := projector.Apply(t.Context(), forfeited); err != nil {
		t.Fatalf("forfeit first family: %v", err)
	}

	seedSecondLifecycleFamily(t, fixture, owner, familyTwo, security.Head{Generation: 1, Revision: 3, EventHash: forfeited.EventHash})
	secondAt := at.Add(2 * time.Minute)
	scheduledTwo := familyProjectionCandidate(t, security.Head{Generation: 1, Revision: 3, EventHash: forfeited.EventHash}, owner,
		"30000000-0000-4000-8000-000000000004", "family_deletion_scheduled", []security.Effect{{
			Kind: "family_deletion_scheduled", AccountID: owner, FamilyID: familyTwo, Role: "owner", Status: "deletion_pending",
			PurgeAfter: secondAt.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano),
		}}, secondAt)
	if err := projector.Apply(t.Context(), scheduledTwo); err != nil {
		t.Fatalf("schedule second family: %v", err)
	}

	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetAccountLocal(t.Context(), tx, owner); err != nil {
		t.Fatal(err)
	}
	var firstStatus, secondStatus string
	if err := tx.QueryRow(t.Context(), `SELECT
		(SELECT status FROM family_recovery_claims WHERE family_id=$1),
		(SELECT status FROM family_recovery_claims WHERE family_id=$2)`, familyOne, familyTwo).Scan(&firstStatus, &secondStatus); err != nil {
		t.Fatal(err)
	}
	if firstStatus != "forfeited" || secondStatus != "active" {
		t.Fatalf("claims first=%s second=%s", firstStatus, secondStatus)
	}
}

func TestRecoveredFamilyCanBeDeletedByTransferredOwner(t *testing.T) {
	fixture := testpg.New(t)
	const (
		owner    = "10000000-0000-4000-8000-000000000001"
		newOwner = "10000000-0000-4000-8000-000000000002"
		family   = "20000000-0000-4000-8000-000000000001"
	)
	prior := seedLifecycleFamily(t, fixture, owner, newOwner, family)
	projector := security.NewProjector(fixture.Projector)
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	scheduled := familyProjectionCandidate(t, prior, owner, "30000000-0000-4000-8000-000000000002", "family_deletion_scheduled", []security.Effect{
		{Kind: "family_deletion_scheduled", AccountID: owner, FamilyID: family, Role: "owner", Status: "deletion_pending", PurgeAfter: at.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)},
		{Kind: "family_deletion_scheduled", AccountID: newOwner, FamilyID: family, Role: "caregiver", Status: "former", PurgeAfter: at.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)},
	}, at)
	if err := projector.Apply(t.Context(), scheduled); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	recovered := familyProjectionCandidate(t, security.Head{Generation: 1, Revision: 2, EventHash: scheduled.EventHash}, owner,
		"30000000-0000-4000-8000-000000000003", "family_recovered", []security.Effect{{
			Kind: "family_recovered", AccountID: owner, FamilyID: family, Role: "owner", Status: "active",
		}}, at.Add(time.Minute))
	if err := projector.Apply(t.Context(), recovered); err != nil {
		t.Fatalf("recover: %v", err)
	}
	reactivateCaregiver(t, fixture, owner, newOwner, family)
	transferred := familyProjectionCandidate(t, security.Head{Generation: 1, Revision: 3, EventHash: recovered.EventHash}, owner,
		"30000000-0000-4000-8000-000000000004", "owner_transferred", []security.Effect{
			{Kind: "owner_transferred", AccountID: owner, FamilyID: family, Role: "caregiver", Status: "active"},
			{Kind: "owner_transferred", AccountID: newOwner, FamilyID: family, Role: "owner", Status: "active"},
		}, at.Add(2*time.Minute))
	if err := projector.Apply(t.Context(), transferred); err != nil {
		t.Fatalf("transfer owner: %v", err)
	}
	secondAt := at.Add(3 * time.Minute)
	rescheduled := familyProjectionCandidate(t, security.Head{Generation: 1, Revision: 4, EventHash: transferred.EventHash}, newOwner,
		"30000000-0000-4000-8000-000000000005", "family_deletion_scheduled", []security.Effect{
			{Kind: "family_deletion_scheduled", AccountID: owner, FamilyID: family, Role: "caregiver", Status: "former", PurgeAfter: secondAt.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)},
			{Kind: "family_deletion_scheduled", AccountID: newOwner, FamilyID: family, Role: "owner", Status: "deletion_pending", PurgeAfter: secondAt.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)},
		}, secondAt)
	if err := projector.Apply(t.Context(), rescheduled); err != nil {
		t.Fatalf("new owner schedule: %v", err)
	}

	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: newOwner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	var claimOwner, status string
	if err := tx.QueryRow(t.Context(), `SELECT account_id::text,status FROM family_recovery_claims WHERE family_id=$1`, family).Scan(&claimOwner, &status); err != nil {
		t.Fatal(err)
	}
	if claimOwner != newOwner || status != "active" {
		t.Fatalf("claim owner=%s status=%s", claimOwner, status)
	}
}

func TestOperatorEntitlementSuspensionBindsExactGrantExpiry(t *testing.T) {
	tests := []struct {
		name, storedExpiry, effectExpiry string
	}{
		{name: "different expiry", storedExpiry: "2027-09-21T12:00:00Z", effectExpiry: "2027-09-21T12:00:01Z"},
		{name: "effect omits non-null expiry", storedExpiry: "2027-09-21T12:00:00Z"},
		{name: "effect adds expiry to null grant", effectExpiry: "2027-09-21T12:00:00Z"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := testpg.New(t)
			const (
				owner     = "10000000-0000-4000-8000-000000000001"
				caregiver = "10000000-0000-4000-8000-000000000002"
				family    = "20000000-0000-4000-8000-000000000001"
			)
			prior := seedLifecycleFamily(t, fixture, owner, caregiver, family)
			tx, err := fixture.Owner.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
				t.Fatal(err)
			}
			var expiry any
			if test.storedExpiry != "" {
				expiry = test.storedExpiry
			}
			if _, err := tx.Exec(t.Context(), `INSERT INTO family_entitlements(family_id,entitlement_key,source,status,expires_at,created_at,updated_at)
				VALUES($1,'family_sync','hosted_beta','active',$2,now(),now())`, family, expiry); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			expected := uint64(1)
			effect := security.Effect{Kind: "operator_entitlement_suspended", FamilyID: family, EntitlementKey: "family_sync",
				Source: "hosted_beta", Status: "suspended", ExpiresAt: test.effectExpiry, OperatorPolicyID: "Entitlement.Policy-v1",
				OperatorEvidenceDigest: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
				OperatorReasonCode:     "support_suspended", ExpectedRevision: &expected}
			candidate := familyProjectionCandidate(t, prior, owner, "30000000-0000-4000-8000-000000000002",
				"operator_entitlement_suspended", []security.Effect{effect}, time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC))
			err = security.NewProjector(fixture.Projector).Apply(t.Context(), candidate)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Fatalf("mismatched suspension err=%v", err)
			}
			read, err := fixture.Owner.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer read.Rollback(t.Context())
			if err := tenant.SetLocal(t.Context(), read, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
				t.Fatal(err)
			}
			var status string
			if err := read.QueryRow(t.Context(), `SELECT status FROM family_entitlements WHERE family_id=$1`, family).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "active" {
				t.Fatalf("entitlement status=%s", status)
			}
		})
	}
}

func TestLifecycleProjectionSchedulesAndRecoversFamily(t *testing.T) {
	fixture := testpg.New(t)
	const (
		owner     = "10000000-0000-4000-8000-000000000001"
		caregiver = "10000000-0000-4000-8000-000000000002"
		family    = "20000000-0000-4000-8000-000000000001"
	)
	prior := seedLifecycleFamily(t, fixture, owner, caregiver, family)
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	purge := at.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)
	scheduled := familyProjectionCandidate(t, prior, owner, "30000000-0000-4000-8000-000000000002", "family_deletion_scheduled", []security.Effect{
		{Kind: "family_deletion_scheduled", AccountID: owner, FamilyID: family, Role: "owner", Status: "deletion_pending", PurgeAfter: purge},
		{Kind: "family_deletion_scheduled", AccountID: caregiver, FamilyID: family, Role: "caregiver", Status: "former", PurgeAfter: purge},
	}, at)
	projector := security.NewProjector(fixture.Projector)
	if err := projector.Apply(t.Context(), scheduled); err != nil {
		t.Fatalf("project deletion schedule: %v", err)
	}
	read, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), read, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	var familyStatus, claimStatus, slotState string
	var activeMembers int
	if err := read.QueryRow(t.Context(), `SELECT f.status,c.status,s.state,
		(SELECT count(*) FROM memberships WHERE family_id=$1 AND status='active')
		FROM families f JOIN family_recovery_claims c ON c.family_id=f.id
		JOIN family_slots s ON s.family_id=f.id AND s.account_id=c.account_id WHERE f.id=$1`, family).
		Scan(&familyStatus, &claimStatus, &slotState, &activeMembers); err != nil {
		t.Fatal(err)
	}
	if familyStatus != "deletion_pending" || claimStatus != "active" || slotState != "family_recovery" || activeMembers != 0 {
		t.Fatalf("scheduled family=%s claim=%s slot=%s active=%d", familyStatus, claimStatus, slotState, activeMembers)
	}
	if err := read.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	caregiverRead, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), caregiverRead, tenant.Context{AccountID: caregiver, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	var caregiverSlots int
	if err := caregiverRead.QueryRow(t.Context(), `SELECT count(*) FROM family_slots WHERE account_id=$1`, caregiver).Scan(&caregiverSlots); err != nil {
		t.Fatal(err)
	}
	_ = caregiverRead.Rollback(t.Context())
	if caregiverSlots != 0 {
		t.Fatalf("caregiver recovery slots=%d want zero", caregiverSlots)
	}

	recovered := familyProjectionCandidate(t, security.Head{Generation: 1, Revision: 2, EventHash: scheduled.EventHash}, owner,
		"30000000-0000-4000-8000-000000000003", "family_recovered", []security.Effect{{
			Kind: "family_recovered", AccountID: owner, FamilyID: family, Role: "owner", Status: "active",
		}}, at.Add(time.Minute))
	if err := projector.Apply(t.Context(), recovered); err != nil {
		t.Fatalf("project recovery: %v", err)
	}
	verify, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), verify, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	var ownerStatus, caregiverStatus, recoveryStatus string
	if err := verify.QueryRow(t.Context(), `SELECT
		(SELECT status FROM memberships WHERE family_id=$1 AND account_id=$2),
		(SELECT status FROM memberships WHERE family_id=$1 AND account_id=$3),
		(SELECT status FROM family_recovery_claims WHERE family_id=$1)`, family, owner, caregiver).
		Scan(&ownerStatus, &caregiverStatus, &recoveryStatus); err != nil {
		t.Fatal(err)
	}
	if ownerStatus != "active" || caregiverStatus != "removed" || recoveryStatus != "recovered" {
		t.Fatalf("owner=%s caregiver=%s recovery=%s", ownerStatus, caregiverStatus, recoveryStatus)
	}
	_ = verify.Rollback(t.Context())

	secondAt := at.Add(2 * time.Minute)
	rescheduled := familyProjectionCandidate(t, security.Head{Generation: 1, Revision: 3, EventHash: recovered.EventHash}, owner,
		"30000000-0000-4000-8000-000000000004", "family_deletion_scheduled", []security.Effect{{
			Kind: "family_deletion_scheduled", AccountID: owner, FamilyID: family, Role: "owner", Status: "deletion_pending",
			PurgeAfter: secondAt.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano),
		}}, secondAt)
	if err := projector.Apply(t.Context(), rescheduled); err != nil {
		t.Fatalf("project repeated deletion schedule: %v", err)
	}
	repeatRead, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer repeatRead.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), repeatRead, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	var repeatedStatus string
	var repeatedRevision int64
	if err := repeatRead.QueryRow(t.Context(), `SELECT status,revision FROM family_recovery_claims WHERE family_id=$1`, family).
		Scan(&repeatedStatus, &repeatedRevision); err != nil {
		t.Fatal(err)
	}
	if repeatedStatus != "active" || repeatedRevision != 4 {
		t.Fatalf("repeated recovery claim status=%s revision=%d", repeatedStatus, repeatedRevision)
	}
}

func TestAccountDeletionErasesIdentityAndQueuesOneCredentialJob(t *testing.T) {
	fixture := testpg.New(t)
	const (
		account     = "10000000-0000-4000-8000-000000000001"
		familyOwner = "10000000-0000-4000-8000-000000000009"
		family      = "20000000-0000-4000-8000-000000000009"
		identity    = "hmac-sha256:identity.v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	prior := seedLifecycleAccount(t, fixture, account, identity)
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	deleted := familyProjectionCandidate(t, prior, account, "30000000-0000-4000-8000-000000000002", "account_deleted", []security.Effect{{
		Kind: "account_deleted", AccountID: account, IdentityKey: identity, Status: "deleted",
	}}, at)
	projector := security.NewProjector(fixture.Projector)
	if err := projector.Apply(t.Context(), deleted); err != nil {
		t.Fatalf("project account deletion: %v", err)
	}
	read, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback(t.Context())
	if err := tenant.SetAccountLocal(t.Context(), read, account); err != nil {
		t.Fatal(err)
	}
	var status, display string
	var identities, jobs int
	var commandID, operationID string
	if err := read.QueryRow(t.Context(), `SELECT status,display_name,
		(SELECT count(*) FROM account_identities WHERE account_id=$1),
		(SELECT count(*) FROM apple_revocation_jobs WHERE account_id=$1),
		(SELECT deletion_command_id::text FROM apple_revocation_jobs WHERE account_id=$1),
		(SELECT deletion_operation_id::text FROM apple_revocation_jobs WHERE account_id=$1)
		FROM accounts WHERE id=$1`, account).Scan(&status, &display, &identities, &jobs, &commandID, &operationID); err != nil {
		t.Fatal(err)
	}
	if status != "deleted" || display != "" || identities != 0 || jobs != 1 || commandID != "30000000-0000-4000-8000-000000000002" || operationID == "" {
		t.Fatalf("status=%q display=%q identities=%d jobs=%d command=%q operation=%q", status, display, identities, jobs, commandID, operationID)
	}
	if err := tenant.SetLocal(t.Context(), read, tenant.Context{AccountID: familyOwner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	var membershipStatus, label, historicalLabel string
	if err := read.QueryRow(t.Context(), `SELECT m.status,l.display_name,h.display_name FROM memberships m
		JOIN family_actor_labels l USING(family_id,account_id)
		JOIN LATERAL (SELECT display_name FROM family_actor_label_history WHERE family_id=m.family_id AND account_id=m.account_id ORDER BY label_version LIMIT 1) h ON true
		WHERE m.family_id=$1 AND m.account_id=$2`, family, account).Scan(&membershipStatus, &label, &historicalLabel); err != nil {
		t.Fatal(err)
	}
	if membershipStatus != "removed" || label != "Former caregiver" || historicalLabel != "Former caregiver" {
		t.Fatalf("membership=%s label=%q history=%q", membershipStatus, label, historicalLabel)
	}
}

func seedLifecycleFamily(t *testing.T, fixture *testpg.Fixture, owner, caregiver, family string) security.Head {
	t.Helper()
	head := security.Head{Generation: 1, Revision: 1}
	for i := range head.EventHash {
		head.EventHash[i] = 0x11
	}
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO accounts(id,display_name) VALUES($1,'Owner')`, owner); err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetAccountLocal(t.Context(), tx, caregiver); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO accounts(id,display_name) VALUES($1,'Caregiver')`, caregiver); err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO families(id,display_name) VALUES($1,'Family')`, []any{family}},
		{`INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner'),($1,$3,'caregiver')`, []any{family, owner, caregiver}},
		{`INSERT INTO family_actor_labels(family_id,account_id,display_name,created_at,updated_at)
		 VALUES($1,$2,'Owner',now(),now()),($1,$3,'Caregiver',now(),now())`, []any{family, owner, caregiver}},
		{`INSERT INTO family_actor_label_history(family_id,account_id,display_name,former,recorded_at)
		 VALUES($1,$2,'Owner',false,now()),($1,$3,'Caregiver',false,now())`, []any{family, owner, caregiver}},
		{`INSERT INTO family_security_state(family_id,owner_account_id,lifecycle,generation,revision,event_hash)
		 VALUES($1,$2,'active',1,1,$3)`, []any{family, owner, head.EventHash[:]}},
		{`UPDATE security_projection_head SET generation=1,revision=1,event_hash=$1 WHERE singleton`, []any{head.EventHash[:]}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO family_slots(account_id,family_id,state) VALUES($1,$2,'active')`, owner, family); err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: caregiver, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO family_slots(account_id,family_id,state) VALUES($1,$2,'active')`, caregiver, family); err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return head
}

func seedSecondLifecycleFamily(t *testing.T, fixture *testpg.Fixture, owner, family string, head security.Head) {
	t.Helper()
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO families(id,display_name) VALUES($1,'Second family')`, []any{family}},
		{`INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')`, []any{family, owner}},
		{`INSERT INTO family_actor_labels(family_id,account_id,display_name,created_at,updated_at) VALUES($1,$2,'Owner',now(),now())`, []any{family, owner}},
		{`INSERT INTO family_actor_label_history(family_id,account_id,display_name,former,recorded_at) VALUES($1,$2,'Owner',false,now())`, []any{family, owner}},
		{`INSERT INTO family_security_state(family_id,owner_account_id,lifecycle,generation,revision,event_hash) VALUES($1,$2,'active',$3,$4,$5)`, []any{family, owner, head.Generation, head.Revision, head.EventHash[:]}},
		{`INSERT INTO family_slots(account_id,family_id,state) VALUES($1,$2,'active')`, []any{owner, family}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func reactivateCaregiver(t *testing.T, fixture *testpg.Fixture, owner, caregiver, family string) {
	t.Helper()
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: caregiver, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE memberships SET role='caregiver',status='active',removed_at=NULL,updated_at=now()
		WHERE family_id=$1 AND account_id=$2`, family, caregiver); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO family_slots(account_id,family_id,state) VALUES($1,$2,'active')`, caregiver, family); err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: owner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func seedLifecycleAccount(t *testing.T, fixture *testpg.Fixture, account, identity string) security.Head {
	t.Helper()
	head := security.Head{Generation: 1, Revision: 1}
	for i := range head.EventHash {
		head.EventHash[i] = 0x22
	}
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetAccountLocal(t.Context(), tx, account); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO accounts(id,display_name) VALUES($1,'Sensitive name')`, account); err != nil {
		t.Fatal(err)
	}
	const familyOwner = "10000000-0000-4000-8000-000000000009"
	const family = "20000000-0000-4000-8000-000000000009"
	if err := tenant.SetAccountLocal(t.Context(), tx, familyOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO accounts(id,display_name) VALUES($1,'Owner')`, familyOwner); err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: familyOwner, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO families(id,display_name) VALUES($1,'Family')`, family); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')`, family, familyOwner); err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: account, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'caregiver')`,
		`INSERT INTO family_slots(account_id,family_id,state) VALUES($2,$1,'active')`,
		`INSERT INTO family_actor_labels(family_id,account_id,display_name,created_at,updated_at) VALUES($1,$2,'Sensitive name',now(),now())`,
		`INSERT INTO family_actor_label_history(family_id,account_id,display_name,former,recorded_at) VALUES($1,$2,'Sensitive old name',false,now())`,
	} {
		if _, err := tx.Exec(t.Context(), statement, family, account); err != nil {
			t.Fatal(err)
		}
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO account_security_state(account_id,identity_key,status,generation,revision,event_hash,lifecycle_generation)
		 VALUES($1,$2,'active',1,1,$3,1)`, []any{account, identity, head.EventHash[:]}},
		{`INSERT INTO account_identities(provider,identity_key_id,identity_digest,account_id,identity_audience,identity_subject,
		 apple_credential_id,apple_credential_key_id,apple_credential_purpose,apple_credential_object_id,created_at,updated_at)
		 VALUES('apple','identity.v1',decode(repeat('aa',32),'hex'),$1,'com.example','subject','credential.ref','credential.key',
		 'apple-refresh-revocation-v1','50000000-0000-4000-8000-000000000001',now(),now())`, []any{account}},
		{`INSERT INTO devices(id,account_id,server_instance_id,canonical_origin,continuity_key_id,continuity_digest,created_at,updated_at)
		 VALUES('60000000-0000-4000-8000-000000000001',$1,'40000000-0000-4000-8000-000000000001','https://example.test',
		 'device.v1',decode(repeat('bb',32),'hex'),now(),now())`, []any{account}},
		{`UPDATE security_projection_head SET generation=1,revision=1,event_hash=$1 WHERE singleton`, []any{head.EventHash[:]}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return head
}
