package database_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const deliveryScheduleSignature = "app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)"

var deliveryPendingColumns = []string{
	"family_id", "device_id", "instance_id", "recovery_epoch", "sequence", "generation",
	"not_before", "attempts", "created_at", "updated_at",
}

func TestMigration000013InstallsDeliveryPendingSchema(t *testing.T) {
	fixture := testpg.New(t)
	var current int64
	if err := fixture.Owner.QueryRow(t.Context(), "SELECT current_version FROM schema_compatibility WHERE singleton").Scan(&current); err != nil || current != 29 {
		t.Fatalf("schema version=%d err=%v", current, err)
	}

	rows, err := fixture.Owner.Query(t.Context(), `SELECT a.attname,format_type(a.atttypid,a.atttypmod),a.attnotnull,
		coalesce(pg_get_expr(d.adbin,d.adrelid),'')
		FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
		WHERE a.attrelid='public.delivery_pending'::regclass AND a.attnum>0 AND NOT a.attisdropped
		ORDER BY a.attnum`)
	if err != nil {
		t.Fatal(err)
	}
	var columns []string
	types := map[string]string{}
	defaults := map[string]string{}
	for rows.Next() {
		var column, dataType, defaultValue string
		var notNull bool
		if err := rows.Scan(&column, &dataType, &notNull, &defaultValue); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, column)
		types[column] = dataType
		defaults[column] = defaultValue
		if !notNull {
			t.Errorf("delivery_pending.%s is nullable", column)
		}
	}
	rows.Close()
	if !slices.Equal(columns, deliveryPendingColumns) {
		t.Fatalf("delivery_pending columns=%v want=%v", columns, deliveryPendingColumns)
	}
	for _, column := range []string{"family_id", "device_id", "instance_id"} {
		if types[column] != "uuid" {
			t.Errorf("delivery_pending.%s type=%q want uuid", column, types[column])
		}
	}
	for _, column := range []string{"recovery_epoch", "sequence", "generation", "attempts"} {
		if types[column] != "bigint" {
			t.Errorf("delivery_pending.%s type=%q want bigint", column, types[column])
		}
	}
	for _, column := range []string{"not_before", "created_at", "updated_at"} {
		if types[column] != "timestamp with time zone" {
			t.Errorf("delivery_pending.%s type=%q want timestamptz", column, types[column])
		}
	}
	if defaults["attempts"] != "0" {
		t.Errorf("delivery_pending.attempts default=%q want 0", defaults["attempts"])
	}

	var enabled, forced bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT relrowsecurity,relforcerowsecurity
		FROM pg_class WHERE oid='public.delivery_pending'::regclass`).Scan(&enabled, &forced); err != nil || !enabled || !forced {
		t.Fatalf("delivery_pending RLS enabled=%t forced=%t err=%v", enabled, forced, err)
	}
	var using, check string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT coalesce(qual,''),coalesce(with_check,'')
		FROM pg_policies WHERE schemaname='public' AND tablename='delivery_pending' AND policyname='family_context'`).Scan(&using, &check); err != nil {
		t.Fatal(err)
	}
	if using != "(family_id = app.current_family_id())" || check != "(family_id = app.current_family_id())" {
		t.Fatalf("delivery policy using=%q check=%q", using, check)
	}

	for index, fragments := range map[string][]string{
		"delivery_pending_pkey":             {"family_id", "device_id", "UNIQUE"},
		"delivery_pending_due_idx":          {"not_before", "family_id", "device_id"},
		"delivery_pending_created_idx":      {"created_at", "family_id", "device_id"},
		"devices_active_account_device_idx": {"account_id", "id", "WHERE (binding_revoked_at IS NULL)"},
	} {
		var definition string
		if err := fixture.Owner.QueryRow(t.Context(), "SELECT pg_get_indexdef(($1::text)::regclass)", "public."+index).Scan(&definition); err != nil {
			t.Errorf("index %s: %v", index, err)
			continue
		}
		for _, fragment := range fragments {
			if !strings.Contains(definition, fragment) {
				t.Errorf("index %s=%q omits %q", index, definition, fragment)
			}
		}
	}
	for constraint, fragments := range map[string][]string{
		"delivery_pending_family_fk":            {"FOREIGN KEY (family_id)", "REFERENCES families(id) ON DELETE CASCADE"},
		"delivery_pending_device_fk":            {"FOREIGN KEY (device_id)", "REFERENCES devices(id) ON DELETE CASCADE"},
		"delivery_pending_uuid_shape":           {"family_id", "device_id", "instance_id", "00000000-0000-0000-0000-000000000000"},
		"delivery_pending_cursor_bounds":        {"recovery_epoch", "sequence", "generation", "9007199254740991"},
		"delivery_pending_attempts_nonnegative": {"attempts >= 0"},
		"delivery_pending_timestamp_shape":      {"isfinite(not_before)", "isfinite(created_at)", "isfinite(updated_at)", "updated_at >= created_at"},
	} {
		var definition string
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid)
			FROM pg_constraint WHERE conname=$1`, constraint).Scan(&definition); err != nil {
			t.Errorf("constraint %s: %v", constraint, err)
			continue
		}
		for _, fragment := range fragments {
			if !strings.Contains(definition, fragment) {
				t.Errorf("constraint %s=%q omits %q", constraint, definition, fragment)
			}
		}
	}

	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 29); err != nil {
		t.Fatalf("schema-twenty exact privilege snapshot rejected: %v", err)
	}
	var runtimeTable, projectorTable, revokerTable, publicTable bool
	var runtimeFunction, projectorFunction, revokerFunction, publicFunction, secureFunction bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		has_table_privilege('baby_tracker_runtime','public.delivery_pending','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER'),
		has_table_privilege('baby_tracker_projector','public.delivery_pending','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER'),
		has_table_privilege('baby_tracker_apple_revoker','public.delivery_pending','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER'),
		has_table_privilege('public','public.delivery_pending','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER'),
		has_function_privilege('baby_tracker_runtime',$1,'EXECUTE'),
		has_function_privilege('baby_tracker_projector',$1,'EXECUTE'),
		has_function_privilege('baby_tracker_apple_revoker',$1,'EXECUTE'),
		has_function_privilege('public',$1,'EXECUTE'),
		(SELECT prosecdef AND proconfig=ARRAY['search_path=pg_catalog'] FROM pg_proc WHERE oid=$1::regprocedure)`,
		deliveryScheduleSignature).Scan(&runtimeTable, &projectorTable, &revokerTable, &publicTable,
		&runtimeFunction, &projectorFunction, &revokerFunction, &publicFunction, &secureFunction); err != nil {
		t.Fatal(err)
	}
	if runtimeTable || projectorTable || revokerTable || publicTable || !runtimeFunction || projectorFunction || revokerFunction || publicFunction || !secureFunction {
		t.Fatalf("delivery privileges runtime_table=%t projector_table=%t revoker_table=%t public_table=%t runtime_fn=%t projector_fn=%t revoker_fn=%t public_fn=%t secure_fn=%t",
			runtimeTable, projectorTable, revokerTable, publicTable, runtimeFunction, projectorFunction, revokerFunction, publicFunction, secureFunction)
	}
	if err := deliveryMustDeny(t, fixture.Runtime, "SELECT count(*) FROM public.delivery_pending"); err != nil {
		t.Fatalf("runtime direct delivery read boundary: %v", err)
	}
	if err := deliveryMustDeny(t, fixture.Projector, "SELECT count(*) FROM public.delivery_pending"); err != nil {
		t.Fatalf("projector direct delivery read boundary: %v", err)
	}
}

func TestScheduleFamilyWakesValidatesAuthorityAndTenantContext(t *testing.T) {
	fixture := testpg.New(t)
	ids := seedDeliveryFamily(t, fixture)

	expectDenied := func(parent *testing.T, name string, accountID, familyID, actorDeviceID, instanceID string, epoch, sequence int64) {
		parent.Helper()
		parent.Run(name, func(t *testing.T) {
			tx, err := fixture.Runtime.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if accountID != "" || familyID != "" {
				if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.Exec(t.Context(), "SELECT app.schedule_family_wakes($1,$2,$3,$4,$5)", ids.family, actorDeviceID, instanceID, epoch, sequence); err == nil {
				t.Fatal("unauthorized wake scheduling succeeded")
			} else {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "42501" || pgErr.Message != "family wake scheduling denied" {
					t.Fatalf("wake denial=%v want SQLSTATE 42501 family wake scheduling denied", err)
				}
			}
		})
	}

	expectDenied(t, "missing context", "", "", ids.actorDevice, ids.instance, 1, 1)
	expectDenied(t, "wrong account context", ids.caregiver, ids.family, ids.actorDevice, ids.instance, 1, 1)
	expectDenied(t, "wrong family context", ids.owner, ids.otherFamily, ids.actorDevice, ids.instance, 1, 1)
	expectDenied(t, "spoofed actor", ids.owner, ids.family, ids.caregiverDevice, ids.instance, 1, 1)
	expectDenied(t, "foreign device", ids.owner, ids.family, ids.foreignDevice, ids.instance, 1, 1)
	expectDenied(t, "wrong instance", ids.owner, ids.family, ids.actorDevice, ids.otherInstance, 1, 1)
	expectDenied(t, "wrong epoch", ids.owner, ids.family, ids.actorDevice, ids.instance, 2, 1)
	deliveryOwnerExecArgs(t, fixture, ids.owner, ids.family, "UPDATE families SET next_sequence=3 WHERE id=$1", ids.family)
	expectDenied(t, "positive stale sequence", ids.owner, ids.family, ids.actorDevice, ids.instance, 1, 2)
	deliveryOwnerExecArgs(t, fixture, ids.owner, ids.family, "UPDATE families SET next_sequence=1 WHERE id=$1", ids.family)
	expectDenied(t, "removed member", ids.removed, ids.family, ids.removedDevice, ids.instance, 1, 1)

	for _, test := range []struct {
		name, mutate, restore string
	}{
		{"inactive account", "UPDATE accounts SET status='deleted' WHERE id='" + ids.owner + "'", "UPDATE accounts SET status='active' WHERE id='" + ids.owner + "'"},
		{"inactive family", "UPDATE families SET status='deletion_pending' WHERE id='" + ids.family + "'", "UPDATE families SET status='active' WHERE id='" + ids.family + "'"},
		{"inactive slot", "UPDATE family_slots SET state='family_recovery' WHERE account_id='" + ids.owner + "'", "UPDATE family_slots SET state='active' WHERE account_id='" + ids.owner + "'"},
		{"suspended entitlement", "UPDATE family_entitlements SET status='suspended' WHERE family_id='" + ids.family + "'", "UPDATE family_entitlements SET status='active' WHERE family_id='" + ids.family + "'"},
		{"revoked actor", "UPDATE devices SET binding_revoked_at=now() WHERE id='" + ids.actorDevice + "'", "UPDATE devices SET binding_revoked_at=NULL WHERE id='" + ids.actorDevice + "'"},
	} {
		t.Run(test.name, func(t *testing.T) {
			deliveryOwnerExec(t, fixture, ids.owner, ids.family, test.mutate)
			expectDenied(t, "denied", ids.owner, ids.family, ids.actorDevice, ids.instance, 1, 1)
			deliveryOwnerExec(t, fixture, ids.owner, ids.family, test.restore)
		})
	}

	// The function may temporarily enumerate other account-scoped rows, but the
	// caller's account context must be restored before it returns.
	tx, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: ids.owner, FamilyID: ids.family}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "SELECT app.schedule_family_wakes($1,$2,$3,$4,$5)", ids.family, ids.actorDevice, ids.instance, 1, 1); err != nil {
		t.Fatal(err)
	}
	var restoredAccount, restoredFamily string
	if err := tx.QueryRow(t.Context(), "SELECT app.current_account_id()::text,app.current_family_id()::text").Scan(&restoredAccount, &restoredFamily); err != nil {
		t.Fatal(err)
	}
	if restoredAccount != ids.owner || restoredFamily != ids.family {
		t.Fatalf("caller context not restored account=%s family=%s", restoredAccount, restoredFamily)
	}
}

func TestScheduleFamilyWakesCoalescesOnlyGenuinelyNewerCursors(t *testing.T) {
	fixture := testpg.New(t)
	ids := seedDeliveryFamily(t, fixture)
	scheduleDelivery(t, fixture, ids.owner, ids.family, ids.actorDevice, ids.instance, 1, 1)

	rows := readDeliveryRows(t, fixture, ids.owner, ids.family)
	if len(rows) != 2 {
		t.Fatalf("scheduled devices=%d want=2 rows=%+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.deviceID == ids.actorDevice || row.deviceID == ids.revokedDevice || row.deviceID == ids.removedDevice || row.deviceID == ids.foreignDevice {
			t.Fatalf("ineligible device scheduled: %+v", row)
		}
		if row.instance != ids.instance || row.epoch != 1 || row.sequence != 1 || row.generation != 1 || row.attempts != 0 || !row.notBefore.Equal(row.createdAt) || !row.updatedAt.Equal(row.createdAt) {
			t.Fatalf("initial wake=%+v", row)
		}
	}

	first := rows[0]
	scheduleDelivery(t, fixture, ids.owner, ids.family, ids.actorDevice, ids.instance, 1, 1)
	afterDuplicate := deliveryRowFor(t, fixture, ids.owner, ids.family, first.deviceID)
	if afterDuplicate != first {
		t.Fatalf("duplicate cursor changed row before=%+v after=%+v", first, afterDuplicate)
	}

	oldCreated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	backoff := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	deliveryOwnerExecArgs(t, fixture, ids.owner, ids.family, `UPDATE delivery_pending
		SET not_before=$1,attempts=3,created_at=$2,updated_at=$2 WHERE family_id=$3`, backoff, oldCreated, ids.family)
	deliveryOwnerExecArgs(t, fixture, ids.owner, ids.family, "UPDATE families SET next_sequence=2 WHERE id=$1", ids.family)
	scheduleDelivery(t, fixture, ids.owner, ids.family, ids.actorDevice, ids.instance, 1, 2)
	advanced := deliveryRowFor(t, fixture, ids.owner, ids.family, first.deviceID)
	if advanced.sequence != 2 || advanced.generation != 2 || advanced.attempts != 3 || !advanced.notBefore.Equal(backoff) || !advanced.createdAt.After(oldCreated) || !advanced.updatedAt.Equal(advanced.createdAt) {
		t.Fatalf("same-epoch coalescing=%+v", advanced)
	}

	// A stale per-device row comparison is a no-op even when the supplied cursor
	// is still the family's exact current high water.
	deliveryOwnerExecArgs(t, fixture, ids.owner, ids.family, "UPDATE delivery_pending SET sequence=3 WHERE family_id=$1 AND device_id=$2", ids.family, first.deviceID)
	scheduleDelivery(t, fixture, ids.owner, ids.family, ids.actorDevice, ids.instance, 1, 2)
	stale := deliveryRowFor(t, fixture, ids.owner, ids.family, first.deviceID)
	if stale.sequence != 3 || stale.generation != advanced.generation {
		t.Fatalf("stale cursor changed row=%+v", stale)
	}

	deliveryOwnerExecArgs(t, fixture, ids.owner, ids.family, "UPDATE server_state SET instance_id=$1,recovery_epoch=2", ids.otherInstance)
	deliveryOwnerExecArgs(t, fixture, ids.owner, ids.family, "UPDATE devices SET server_instance_id=$1 WHERE server_instance_id=$2", ids.otherInstance, ids.instance)
	deliveryOwnerExecArgs(t, fixture, ids.owner, ids.family, "UPDATE families SET next_sequence=1 WHERE id=$1", ids.family)
	scheduleDelivery(t, fixture, ids.owner, ids.family, ids.actorDevice, ids.otherInstance, 2, 1)
	replaced := deliveryRowFor(t, fixture, ids.owner, ids.family, first.deviceID)
	if replaced.instance != ids.otherInstance || replaced.epoch != 2 || replaced.sequence != 1 || replaced.generation != advanced.generation+1 || replaced.attempts != 0 || !replaced.notBefore.Equal(replaced.createdAt) || !replaced.updatedAt.Equal(replaced.createdAt) || !replaced.createdAt.After(advanced.createdAt) {
		t.Fatalf("epoch replacement=%+v advanced=%+v", replaced, advanced)
	}

	deliveryOwnerExecArgs(t, fixture, ids.owner, ids.family, `UPDATE delivery_pending SET generation=9007199254740991 WHERE family_id=$1 AND device_id=$2`, ids.family, first.deviceID)
	deliveryOwnerExecArgs(t, fixture, ids.owner, ids.family, "UPDATE families SET next_sequence=2 WHERE id=$1", ids.family)
	tx, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: ids.owner, FamilyID: ids.family}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "SELECT app.schedule_family_wakes($1,$2,$3,$4,$5)", ids.family, ids.actorDevice, ids.otherInstance, 2, 2); err == nil {
		t.Fatal("generation exhaustion accepted")
	}
}

type deliveryIDs struct {
	owner, caregiver, removed, foreignAccount                                                   string
	family, otherFamily                                                                         string
	actorDevice, ownerOtherDevice, caregiverDevice, revokedDevice, removedDevice, foreignDevice string
	instance, otherInstance                                                                     string
}

func seedDeliveryFamily(t *testing.T, fixture *testpg.Fixture) deliveryIDs {
	t.Helper()
	ids := deliveryIDs{
		owner: "d1000000-0000-4000-8000-000000000001", caregiver: "d1000000-0000-4000-8000-000000000002",
		removed: "d1000000-0000-4000-8000-000000000003", foreignAccount: "d2000000-0000-4000-8000-000000000001",
		family: "d1000000-0000-4000-8000-000000000010", otherFamily: "d2000000-0000-4000-8000-000000000010",
		actorDevice: "d1000000-0000-4000-8000-000000000101", ownerOtherDevice: "d1000000-0000-4000-8000-000000000102",
		caregiverDevice: "d1000000-0000-4000-8000-000000000103", revokedDevice: "d1000000-0000-4000-8000-000000000104",
		removedDevice: "d1000000-0000-4000-8000-000000000105", foreignDevice: "d2000000-0000-4000-8000-000000000101",
		instance: "d0000000-0000-4000-8000-000000000001", otherInstance: "d0000000-0000-4000-8000-000000000002",
	}
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	for _, account := range []string{ids.owner, ids.caregiver, ids.removed, ids.foreignAccount} {
		if err := tenant.SetAccountLocal(t.Context(), tx, account); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Delivery caregiver')", account); err != nil {
			t.Fatal(err)
		}
	}
	for _, family := range []string{ids.family, ids.otherFamily} {
		if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: ids.owner, FamilyID: family}); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), "INSERT INTO families(id,display_name,next_sequence) VALUES($1,'Delivery family',1)", family); err != nil {
			t.Fatal(err)
		}
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: ids.owner, FamilyID: ids.family}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO memberships(family_id,account_id,role,status,removed_at) VALUES
		($1,$2,'owner','active',NULL),($1,$3,'caregiver','active',NULL),($1,$4,'caregiver','removed',$5)`, ids.family, ids.owner, ids.caregiver, ids.removed, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO family_entitlements(family_id,entitlement_key,source,status,created_at,updated_at)
		VALUES($1,'family_sync','hosted_beta','active',$2,$2)`, ids.family, now); err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: ids.foreignAccount, FamilyID: ids.otherFamily}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", ids.otherFamily, ids.foreignAccount); err != nil {
		t.Fatal(err)
	}
	for _, slot := range []struct{ account, family string }{{ids.owner, ids.family}, {ids.caregiver, ids.family}, {ids.removed, ids.family}, {ids.foreignAccount, ids.otherFamily}} {
		if err := tenant.SetAccountLocal(t.Context(), tx, slot.account); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), "INSERT INTO family_slots(account_id,family_id,state) VALUES($1,$2,'active')", slot.account, slot.family); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO devices(id,account_id,server_instance_id,canonical_origin,continuity_key_id,continuity_digest,binding_revoked_at,created_at,updated_at) VALUES
		($1,$2,$7,'https://delivery.example','device.v1',decode(repeat('11',32),'hex'),NULL,$9,$9),
		($3,$2,$7,'https://delivery.example','device.v1',decode(repeat('12',32),'hex'),NULL,$9,$9),
		($4,$5,$7,'https://delivery.example','device.v1',decode(repeat('13',32),'hex'),NULL,$9,$9),
		($6,$5,$7,'https://delivery.example','device.v1',decode(repeat('14',32),'hex'),$9,$9,$9),
		($8,$10,$7,'https://delivery.example','device.v1',decode(repeat('15',32),'hex'),NULL,$9,$9),
		($11,$12,$7,'https://delivery.example','device.v1',decode(repeat('16',32),'hex'),NULL,$9,$9)`,
		ids.actorDevice, ids.owner, ids.ownerOtherDevice, ids.caregiverDevice, ids.caregiver, ids.revokedDevice,
		ids.instance, ids.removedDevice, now, ids.removed, ids.foreignDevice, ids.foreignAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO server_state(singleton,instance_id,recovery_epoch,updated_at) VALUES(true,$1,1,$2)", ids.instance, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return ids
}

func scheduleDelivery(t *testing.T, fixture *testpg.Fixture, accountID, familyID, actorDeviceID, instanceID string, epoch, sequence int64) {
	t.Helper()
	tx, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "SELECT app.schedule_family_wakes($1,$2,$3,$4,$5)", familyID, actorDeviceID, instanceID, epoch, sequence); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

type deliveryRow struct {
	deviceID, instance          string
	epoch, sequence, generation int64
	notBefore                   time.Time
	attempts                    int64
	createdAt, updatedAt        time.Time
}

func readDeliveryRows(t *testing.T, fixture *testpg.Fixture, accountID, familyID string) []deliveryRow {
	t.Helper()
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(t.Context(), `SELECT device_id::text,instance_id::text,recovery_epoch,sequence,generation,
		not_before,attempts,created_at,updated_at FROM delivery_pending ORDER BY device_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []deliveryRow
	for rows.Next() {
		var row deliveryRow
		if err := rows.Scan(&row.deviceID, &row.instance, &row.epoch, &row.sequence, &row.generation,
			&row.notBefore, &row.attempts, &row.createdAt, &row.updatedAt); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	return result
}

func deliveryRowFor(t *testing.T, fixture *testpg.Fixture, accountID, familyID, deviceID string) deliveryRow {
	t.Helper()
	for _, row := range readDeliveryRows(t, fixture, accountID, familyID) {
		if row.deviceID == deviceID {
			return row
		}
	}
	t.Fatalf("missing delivery row for device %s", deviceID)
	return deliveryRow{}
}

func deliveryOwnerExec(t *testing.T, fixture *testpg.Fixture, accountID, familyID, statement string) {
	t.Helper()
	deliveryOwnerExecArgs(t, fixture, accountID, familyID, statement)
}

func deliveryOwnerExecArgs(t *testing.T, fixture *testpg.Fixture, accountID, familyID, statement string, args ...any) {
	t.Helper()
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), statement, args...); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func deliveryMustDeny(t *testing.T, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, statement string) error {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), statement); err == nil {
		return errors.New("operation unexpectedly succeeded")
	}
	return nil
}
