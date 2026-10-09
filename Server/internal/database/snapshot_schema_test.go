package database_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/testpg"
)

var snapshotStagingColumns = map[string][]string{
	"sync_page_sessions": {
		"family_id", "token_digest", "token_key_id", "account_id", "device_id", "session_id",
		"server_instance_id", "recovery_epoch", "security_revision", "kind", "high_water_sequence",
		"range_end_position", "resume_position", "page_end_position", "page_item_count", "page_encoded_bytes", "snapshot_id",
		"item_limit", "byte_limit", "created_at", "expires_at",
	},
	"sync_snapshots": {
		"family_id", "id", "account_id", "device_id", "session_id", "server_instance_id", "recovery_epoch",
		"security_revision", "high_water_sequence", "entity_count", "encoded_bytes", "captured_at", "expires_at",
	},
	"sync_snapshot_entities": {
		"family_id", "snapshot_id", "account_id", "device_id", "session_id", "server_instance_id",
		"recovery_epoch", "security_revision", "position", "entity_kind", "entity_id", "entity_version",
		"canonical_entity", "encoded_bytes",
	},
	"sync_snapshot_admissions": {
		"account_id", "family_id", "snapshot_id", "expires_at",
	},
}

var snapshotRuntimeFunctions = []string{
	"app.sync_snapshot_admission_counts(uuid,uuid,timestamp with time zone)",
}

func TestMigration000010InstallsBoundSnapshotStagingSchema(t *testing.T) {
	fixture := testpg.New(t)
	var current int64
	if err := fixture.Owner.QueryRow(t.Context(), "SELECT current_version FROM schema_compatibility WHERE singleton").Scan(&current); err != nil || current != 29 {
		t.Fatalf("schema version=%d err=%v", current, err)
	}
	for table, want := range snapshotStagingColumns {
		rows, err := fixture.Owner.Query(t.Context(), `SELECT a.attname
			FROM pg_attribute a
			WHERE a.attrelid=($1::text)::regclass AND a.attnum>0 AND NOT a.attisdropped
			ORDER BY a.attnum`, "public."+table)
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
		if !slices.Equal(got, want) {
			t.Errorf("%s columns=%v want=%v", table, got, want)
		}
		var enabled, forced bool
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT relrowsecurity,relforcerowsecurity
			FROM pg_class WHERE oid=($1::text)::regclass`, "public."+table).Scan(&enabled, &forced); err != nil || !enabled || !forced {
			t.Errorf("%s RLS enabled=%t forced=%t err=%v", table, enabled, forced, err)
		}
	}
	for _, index := range []string{
		"sync_page_sessions_expiry_idx", "sync_page_sessions_account_active_idx",
		"sync_snapshots_expiry_idx", "sync_snapshots_account_active_idx", "sync_snapshots_family_active_idx",
		"sync_snapshot_admissions_expiry_idx", "sync_snapshot_admissions_account_active_idx",
		"sync_snapshot_admissions_family_active_idx", "sync_snapshot_entities_cleanup_idx",
	} {
		var exists bool
		if err := fixture.Owner.QueryRow(t.Context(), "SELECT to_regclass('public.'||$1) IS NOT NULL", index).Scan(&exists); err != nil || !exists {
			t.Errorf("index %s exists=%t err=%v", index, exists, err)
		}
	}
	for index, orderedColumns := range map[string]string{
		"sync_snapshot_admissions_family_active_idx": "(family_id, expires_at, snapshot_id)",
		"sync_snapshot_entities_cleanup_idx":         "(snapshot_id, \"position\")",
	} {
		var definition string
		if err := fixture.Owner.QueryRow(t.Context(), "SELECT pg_get_indexdef(($1::text)::regclass)", "public."+index).Scan(&definition); err != nil || !strings.Contains(definition, orderedColumns) {
			t.Errorf("index %s definition=%q err=%v", index, definition, err)
		}
	}
	for _, constraint := range []string{
		"sessions_sync_binding_key", "sync_snapshots_session_fk", "sync_page_sessions_session_fk",
		"sync_page_sessions_snapshot_fk", "sync_snapshot_entities_snapshot_fk",
	} {
		var definition string
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid)
			FROM pg_constraint WHERE conname=$1`, constraint).Scan(&definition); err != nil {
			t.Errorf("constraint %s: %v", constraint, err)
			continue
		}
		if strings.Contains(constraint, "session_fk") && !strings.Contains(definition, "account_id, device_id, session_id, family_id, server_instance_id, recovery_epoch, security_revision") {
			t.Errorf("constraint %s does not bind complete session authority: %s", constraint, definition)
		}
		if constraint == "sync_page_sessions_snapshot_fk" && (!strings.Contains(definition, "range_end_position") || !strings.Contains(definition, "entity_count")) {
			t.Errorf("constraint %s does not bind the frozen snapshot range: %s", constraint, definition)
		}
		if constraint == "sync_snapshot_entities_snapshot_fk" && !strings.Contains(definition, "account_id, device_id, session_id, server_instance_id, recovery_epoch, security_revision") {
			t.Errorf("constraint %s does not bind entity ownership and authority: %s", constraint, definition)
		}
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 29); err != nil {
		t.Fatalf("schema-twenty exact privilege snapshot rejected: %v", err)
	}
	var runtimeDML, publicDML, projectorDML, admissionDML, counterExecute, publicExecute, projectorExecute bool
	var stageRuntimeExecute, stagePublicExecute, stageProjectorExecute, stageRevokerExecute bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		(SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT')
			AND NOT has_table_privilege('baby_tracker_runtime',t,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER'))
		 FROM unnest(ARRAY['public.sync_page_sessions','public.sync_snapshots','public.sync_snapshot_entities']) t),
		(SELECT bool_or(has_table_privilege('public',t,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER'))
		 FROM unnest(ARRAY['public.sync_page_sessions','public.sync_snapshots','public.sync_snapshot_entities']) t),
		(SELECT bool_or(has_table_privilege('baby_tracker_projector',t,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER'))
		 FROM unnest(ARRAY['public.sync_page_sessions','public.sync_snapshots','public.sync_snapshot_entities']) t),
		has_table_privilege('baby_tracker_runtime','public.sync_snapshot_admissions','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER'),
		has_function_privilege('baby_tracker_runtime','app.sync_snapshot_admission_counts(uuid,uuid,timestamp with time zone)','EXECUTE'),
		has_function_privilege('public','app.sync_snapshot_admission_counts(uuid,uuid,timestamp with time zone)','EXECUTE'),
		has_function_privilege('baby_tracker_projector','app.sync_snapshot_admission_counts(uuid,uuid,timestamp with time zone)','EXECUTE'),
		has_function_privilege('baby_tracker_runtime','app.stage_sync_snapshot_admission()','EXECUTE'),
		has_function_privilege('public','app.stage_sync_snapshot_admission()','EXECUTE'),
		has_function_privilege('baby_tracker_projector','app.stage_sync_snapshot_admission()','EXECUTE'),
		has_function_privilege('baby_tracker_apple_revoker','app.stage_sync_snapshot_admission()','EXECUTE')`).Scan(
		&runtimeDML, &publicDML, &projectorDML, &admissionDML, &counterExecute, &publicExecute, &projectorExecute,
		&stageRuntimeExecute, &stagePublicExecute, &stageProjectorExecute, &stageRevokerExecute); err != nil {
		t.Fatal(err)
	}
	if !runtimeDML || publicDML || projectorDML || admissionDML || !counterExecute || publicExecute || projectorExecute ||
		stageRuntimeExecute || stagePublicExecute || stageProjectorExecute || stageRevokerExecute {
		t.Fatalf("snapshot privileges runtime=%t public=%t projector=%t admission=%t counter=%t public_counter=%t projector_counter=%t stage_runtime=%t stage_public=%t stage_projector=%t stage_revoker=%t",
			runtimeDML, publicDML, projectorDML, admissionDML, counterExecute, publicExecute, projectorExecute,
			stageRuntimeExecute, stagePublicExecute, stageProjectorExecute, stageRevokerExecute)
	}
}

func TestSnapshotStagingConstraintsBindTokensPagesAndSessions(t *testing.T) {
	fixture := testpg.New(t)
	for constraint, fragments := range map[string][]string{
		"sync_page_sessions_page_range":               {"range_end_position", "resume_position", "page_end_position"},
		"sync_page_sessions_snapshot_kind":            {"kind", "snapshot_id", "range_end_position", "high_water_sequence"},
		"sync_page_sessions_policy_bounds":            {"item_limit", "byte_limit", "65536", "page_end_position", "resume_position", "page_item_count"},
		"sync_snapshots_budget_bounds":                {"entity_count", "encoded_bytes", "high_water_sequence"},
		"sync_snapshot_entities_canonical_object":     {"jsonb_typeof", "canonical_entity", "object"},
		"sync_snapshot_entities_encoded_bytes_bounds": {"encoded_bytes", "4194304"},
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
	var snapshotBounds string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname='sync_snapshots_budget_bounds'`).Scan(&snapshotBounds); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshotBounds, "2000000") || !strings.Contains(snapshotBounds, "536870912") {
		t.Fatalf("snapshot hard bounds=%q", snapshotBounds)
	}
	var digestCheck, tokenKeyCheck bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		EXISTS (SELECT FROM pg_constraint WHERE conrelid='public.sync_page_sessions'::regclass
			AND pg_get_constraintdef(oid) LIKE '%octet_length(token_digest) = 32%'),
		EXISTS (SELECT FROM pg_constraint WHERE conrelid='public.sync_page_sessions'::regclass
			AND pg_get_constraintdef(oid) LIKE '%token_key_id%~%')`).Scan(&digestCheck, &tokenKeyCheck); err != nil {
		t.Fatal(err)
	}
	if !digestCheck || !tokenKeyCheck {
		t.Fatalf("hashed lookup/key-id constraints digest=%t key=%t", digestCheck, tokenKeyCheck)
	}
}

func TestSnapshotAdmissionCountsOwnExpiredFamilyHeadersWithoutEntityVisibility(t *testing.T) {
	fixture := testpg.New(t)
	const (
		accountID = "aa000000-0000-4000-8000-000000000001"
		otherID   = "aa000000-0000-4000-8000-000000000002"
		familyA   = "ab000000-0000-4000-8000-000000000001"
		familyB   = "ab000000-0000-4000-8000-000000000002"
		deviceID  = "ac000000-0000-4000-8000-000000000001"
		sessionID = "ad000000-0000-4000-8000-000000000001"
		snapshot  = "ae000000-0000-4000-8000-000000000001"
		entityID  = "af000000-0000-4000-8000-000000000001"
		instance  = "123e4567-e89b-42d3-a456-426614174000"
	)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	setup, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), setup, tenant.Context{AccountID: accountID, FamilyID: familyA}); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Snapshot owner')", accountID); err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetAccountLocal(t.Context(), setup, otherID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Other')", otherID); err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), setup, tenant.Context{AccountID: accountID, FamilyID: familyA}); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name) VALUES($1,'Prior')", familyA); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", familyA, accountID); err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), setup, tenant.Context{AccountID: accountID, FamilyID: familyB}); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name) VALUES($1,'Current')", familyB); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", familyB, accountID); err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), setup, tenant.Context{AccountID: accountID, FamilyID: familyA}); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO devices(id,account_id,server_instance_id,canonical_origin,
		continuity_key_id,continuity_digest,created_at,updated_at)
		VALUES($1,$2,$3,'https://snapshot.example','device.v1',decode(repeat('11',32),'hex'),$4,$4)`,
		deviceID, accountID, instance, now); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO sessions(session_id,account_id,device_id,lineage_root_session_id,
		server_instance_id,recovery_epoch,security_revision,scope,family_id,token_key_id,access_digest,refresh_digest,
		access_expires_at,refresh_expires_at,reauthenticate_at,absolute_expires_at,status,created_at,updated_at)
		VALUES($1,$2,$3,$1,$4,1,1,'family',$5,'session.v1',decode(repeat('22',32),'hex'),
		decode(repeat('33',32),'hex'),$6,$7,$7,$7,'active',$8,$8)`,
		sessionID, accountID, deviceID, instance, familyA, now.Add(time.Hour), now.Add(2*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO sync_snapshots(family_id,id,account_id,device_id,session_id,
		server_instance_id,recovery_epoch,security_revision,high_water_sequence,entity_count,encoded_bytes,captured_at,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,1,1,0,1,128,$7,$8)`, familyA, snapshot, accountID, deviceID,
		sessionID, instance, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO sync_snapshot_entities(family_id,snapshot_id,account_id,device_id,
		session_id,server_instance_id,recovery_epoch,security_revision,position,entity_kind,entity_id,entity_version,
		canonical_entity,encoded_bytes)
		VALUES($1,$2,$3,$4,$5,$6,1,1,0,'baby',$7,1,'{}',128)`, familyA, snapshot, accountID,
		deviceID, sessionID, instance, entityID); err != nil {
		t.Fatal(err)
	}
	if err := setup.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	read, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), read, tenant.Context{AccountID: accountID, FamilyID: familyB}); err != nil {
		t.Fatal(err)
	}
	var accountCount, familyCount, headers, entities int64
	if err := read.QueryRow(t.Context(), "SELECT account_active,family_active FROM app.sync_snapshot_admission_counts($1,$2,$3)",
		accountID, familyB, now).Scan(&accountCount, &familyCount); err != nil || accountCount != 1 || familyCount != 0 {
		t.Fatalf("active snapshots account=%d family=%d err=%v", accountCount, familyCount, err)
	}
	if err := read.QueryRow(t.Context(), "SELECT count(*) FROM sync_snapshots").Scan(&headers); err != nil || headers != 0 {
		t.Fatalf("prior-family snapshot headers visible=%d err=%v", headers, err)
	}
	if err := read.QueryRow(t.Context(), "SELECT count(*) FROM sync_snapshot_entities").Scan(&entities); err != nil || entities != 0 {
		t.Fatalf("prior-family entities visible=%d err=%v", entities, err)
	}
	if _, err := read.Exec(t.Context(), "SELECT count(*) FROM sync_snapshot_admissions"); postgresState(err) != "42501" {
		t.Fatalf("admission metadata direct read err=%v state=%q", err, postgresState(err))
	}
	if _, err := read.Exec(t.Context(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	denied, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer denied.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), denied, tenant.Context{AccountID: accountID, FamilyID: familyB}); err != nil {
		t.Fatal(err)
	}
	if _, err := denied.Exec(t.Context(), "SELECT * FROM app.sync_snapshot_admission_counts($1,$2,$3)", otherID, familyB, now); postgresState(err) != "42501" {
		t.Fatalf("cross-account snapshot count err=%v state=%q", err, postgresState(err))
	}
}

func TestSnapshotStagingRuntimeBindingsBoundsAndAdmissionCaps(t *testing.T) {
	h := testkit.New(t)
	const (
		snapshotOne   = "be000000-0000-4000-8000-000000000001"
		snapshotTwo   = "be000000-0000-4000-8000-000000000002"
		snapshotThree = "be000000-0000-4000-8000-000000000003"
		entityID      = "bf000000-0000-4000-8000-000000000001"
		instance      = "50000000-0000-4000-8000-000000000001"
	)
	expires := h.Now.Add(15 * time.Minute)
	insertSnapshot := func(snapshotID string, count, bytes int64) error {
		tx, err := h.Runtime.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(t.Context())
		if err = tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
			return err
		}
		_, err = tx.Exec(t.Context(), `INSERT INTO sync_snapshots(family_id,id,account_id,device_id,session_id,
			server_instance_id,recovery_epoch,security_revision,high_water_sequence,entity_count,encoded_bytes,captured_at,expires_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,0,$9,$10,$11,$12)`, h.FamilyID, snapshotID, h.Owner.AccountID,
			h.Owner.DeviceID, h.Owner.SessionID, instance, h.Owner.RecoveryEpoch, h.Owner.SecurityRevision, count, bytes, h.Now, expires)
		if err != nil {
			return err
		}
		return tx.Commit(t.Context())
	}
	if err := insertSnapshot(snapshotOne, 1, 128); err != nil {
		t.Fatalf("valid runtime snapshot insert: %v", err)
	}
	ownerWrite, err := h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer ownerWrite.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), ownerWrite, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := ownerWrite.Exec(t.Context(), `INSERT INTO sync_snapshot_entities(family_id,snapshot_id,account_id,device_id,
		session_id,server_instance_id,recovery_epoch,security_revision,position,entity_kind,entity_id,entity_version,
		canonical_entity,encoded_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,0,'baby',$9,1,'{}',128)`,
		h.FamilyID, snapshotOne, h.Owner.AccountID, h.Owner.DeviceID, h.Owner.SessionID, instance,
		h.Owner.RecoveryEpoch, h.Owner.SecurityRevision, entityID); err != nil {
		t.Fatalf("valid runtime entity insert: %v", err)
	}
	if _, err := ownerWrite.Exec(t.Context(), `INSERT INTO sync_page_sessions(family_id,token_digest,token_key_id,
		account_id,device_id,session_id,server_instance_id,recovery_epoch,security_revision,kind,high_water_sequence,
		range_end_position,resume_position,page_end_position,page_item_count,page_encoded_bytes,snapshot_id,item_limit,
		byte_limit,created_at,expires_at) VALUES($1,decode(repeat('41',32),'hex'),'page.v1',$2,$3,$4,$5,$6,$7,
		'snapshot',0,1,0,1,1,128,$8,2,65536,$9,$10)`, h.FamilyID, h.Owner.AccountID, h.Owner.DeviceID,
		h.Owner.SessionID, instance, h.Owner.RecoveryEpoch, h.Owner.SecurityRevision, snapshotOne, h.Now, expires); err != nil {
		t.Fatalf("valid runtime page insert: %v", err)
	}
	if err := ownerWrite.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	caregiverRead, err := h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer caregiverRead.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), caregiverRead, tenant.Context{AccountID: h.Caregiver.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	var visible, visibleHeaders, visiblePages int
	if err := caregiverRead.QueryRow(t.Context(), "SELECT count(*) FROM sync_snapshot_entities").Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("other caregiver entity visibility=%d err=%v", visible, err)
	}
	if err := caregiverRead.QueryRow(t.Context(), "SELECT count(*) FROM sync_snapshots").Scan(&visibleHeaders); err != nil || visibleHeaders != 0 {
		t.Fatalf("other caregiver snapshot header visibility=%d err=%v", visibleHeaders, err)
	}
	if err := caregiverRead.QueryRow(t.Context(), "SELECT count(*) FROM sync_page_sessions").Scan(&visiblePages); err != nil || visiblePages != 0 {
		t.Fatalf("other caregiver page session visibility=%d err=%v", visiblePages, err)
	}
	if err := caregiverRead.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	assertEntityDenied := func(name, familyID, accountID, deviceID, sessionID, snapshotID string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			tx, err := h.Runtime.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(t.Context(), `INSERT INTO sync_snapshot_entities(family_id,snapshot_id,account_id,device_id,
				session_id,server_instance_id,recovery_epoch,security_revision,position,entity_kind,entity_id,entity_version,
				canonical_entity,encoded_bytes) VALUES($1,$2,$3,$4,$5,$6,1,1,1,'baby',
				'bf000000-0000-4000-8000-000000000002',1,'{}',64)`, familyID, snapshotID, accountID, deviceID, sessionID, instance)
			if state := postgresState(err); state != "23503" && state != "42501" {
				t.Fatalf("mismatched entity err=%v state=%q", err, state)
			}
		})
	}
	assertEntityDenied("same family other caregiver", h.FamilyID, h.Caregiver.AccountID, h.Caregiver.DeviceID, h.Caregiver.SessionID, snapshotOne)
	assertEntityDenied("wrong session", h.FamilyID, h.Owner.AccountID, h.Owner.DeviceID, "bd000000-0000-4000-8000-000000000099", snapshotOne)
	assertEntityDenied("wrong snapshot", h.FamilyID, h.Owner.AccountID, h.Owner.DeviceID, h.Owner.SessionID, "be000000-0000-4000-8000-000000000099")
	assertEntityDenied("cross family", "bc000000-0000-4000-8000-000000000099", h.Owner.AccountID, h.Owner.DeviceID, h.Owner.SessionID, snapshotOne)

	oversizedEntity, err := h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer oversizedEntity.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), oversizedEntity, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	_, err = oversizedEntity.Exec(t.Context(), `INSERT INTO sync_snapshot_entities(family_id,snapshot_id,account_id,device_id,
		session_id,server_instance_id,recovery_epoch,security_revision,position,entity_kind,entity_id,entity_version,
		canonical_entity,encoded_bytes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,1,'baby',
		'bf000000-0000-4000-8000-000000000003',1,'{}',4194305)`, h.FamilyID, snapshotOne,
		h.Owner.AccountID, h.Owner.DeviceID, h.Owner.SessionID, instance, h.Owner.RecoveryEpoch, h.Owner.SecurityRevision)
	if postgresState(err) != "23514" {
		t.Fatalf("oversized entity err=%v state=%q", err, postgresState(err))
	}
	if err := oversizedEntity.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := insertSnapshot(snapshotTwo, 0, 0); err != nil {
		t.Fatalf("second cap snapshot insert: %v", err)
	}
	caregiverSnapshot, err := h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer caregiverSnapshot.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), caregiverSnapshot, tenant.Context{AccountID: h.Caregiver.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := caregiverSnapshot.Exec(t.Context(), `INSERT INTO sync_snapshots(family_id,id,account_id,device_id,session_id,
		server_instance_id,recovery_epoch,security_revision,high_water_sequence,entity_count,encoded_bytes,captured_at,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,0,0,0,$9,$10)`, h.FamilyID, snapshotThree, h.Caregiver.AccountID,
		h.Caregiver.DeviceID, h.Caregiver.SessionID, instance, h.Caregiver.RecoveryEpoch, h.Caregiver.SecurityRevision,
		h.Now, expires); err != nil {
		t.Fatalf("caregiver snapshot insert: %v", err)
	}
	if err := caregiverSnapshot.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	counts, err := h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer counts.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), counts, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	var accountActive, familyActive int64
	if err := counts.QueryRow(t.Context(), `SELECT account_active,family_active
		FROM app.sync_snapshot_admission_counts($1,$2,$3)`, h.Owner.AccountID, h.FamilyID, h.Now).
		Scan(&accountActive, &familyActive); err != nil || accountActive != 2 || familyActive != 3 {
		t.Fatalf("cap counts account=%d family=%d err=%v", accountActive, familyActive, err)
	}
	if err := counts.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	caregiverCounts, err := h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer caregiverCounts.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), caregiverCounts, tenant.Context{AccountID: h.Caregiver.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	if err := caregiverCounts.QueryRow(t.Context(), `SELECT account_active,family_active
		FROM app.sync_snapshot_admission_counts($1,$2,$3)`, h.Caregiver.AccountID, h.FamilyID, h.Now).
		Scan(&accountActive, &familyActive); err != nil || accountActive != 1 || familyActive != 3 {
		t.Fatalf("caregiver cap counts account=%d family=%d err=%v", accountActive, familyActive, err)
	}
	if err := caregiverCounts.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, id     string
		count, bytes int64
	}{
		{name: "entity count", id: "be000000-0000-4000-8000-000000000091", count: 2_000_001, bytes: 0},
		{name: "encoded bytes", id: "be000000-0000-4000-8000-000000000092", count: 0, bytes: 536_870_913},
	} {
		t.Run("reject oversized "+test.name, func(t *testing.T) {
			if err := insertSnapshot(test.id, test.count, test.bytes); postgresState(err) != "23514" {
				t.Fatalf("oversized snapshot err=%v state=%q", err, postgresState(err))
			}
		})
	}
	insertInvalidPage := func(digestByte string, itemCount, byteLimit int) error {
		tx, err := h.Runtime.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(t.Context())
		if err = tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
			return err
		}
		_, err = tx.Exec(t.Context(), `INSERT INTO sync_page_sessions(family_id,token_digest,token_key_id,
			account_id,device_id,session_id,server_instance_id,recovery_epoch,security_revision,kind,high_water_sequence,
			range_end_position,resume_position,page_end_position,page_item_count,page_encoded_bytes,snapshot_id,item_limit,
			byte_limit,created_at,expires_at) VALUES($1,decode(repeat($2,32),'hex'),'page.v1',$3,$4,$5,$6,$7,$8,
			'snapshot',0,1,0,1,$9,128,$10,2,$11,$12,$13)`, h.FamilyID, digestByte, h.Owner.AccountID,
			h.Owner.DeviceID, h.Owner.SessionID, instance, h.Owner.RecoveryEpoch, h.Owner.SecurityRevision,
			itemCount, snapshotOne, byteLimit, h.Now, expires)
		return err
	}
	if err := insertInvalidPage("42", 1, 65_535); postgresState(err) != "23514" {
		t.Fatalf("undersized page byte limit err=%v state=%q", err, postgresState(err))
	}
	if err := insertInvalidPage("43", 0, 65_536); postgresState(err) != "23514" {
		t.Fatalf("inconsistent page range err=%v state=%q", err, postgresState(err))
	}
	zeroPage, err := h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer zeroPage.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), zeroPage, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	_, err = zeroPage.Exec(t.Context(), `INSERT INTO sync_page_sessions(family_id,token_digest,token_key_id,
		account_id,device_id,session_id,server_instance_id,recovery_epoch,security_revision,kind,high_water_sequence,
		range_end_position,resume_position,page_end_position,page_item_count,page_encoded_bytes,snapshot_id,item_limit,
		byte_limit,created_at,expires_at) VALUES($1,decode(repeat('44',32),'hex'),'page.v1',$2,$3,$4,$5,$6,$7,
		'sync',0,0,0,0,0,0,NULL,2,65536,$8,$9)`, h.FamilyID, h.Owner.AccountID, h.Owner.DeviceID,
		h.Owner.SessionID, instance, h.Owner.RecoveryEpoch, h.Owner.SecurityRevision, h.Now, expires)
	if postgresState(err) != "23514" {
		t.Fatalf("nonadvancing zero-byte page err=%v state=%q", err, postgresState(err))
	}
}
