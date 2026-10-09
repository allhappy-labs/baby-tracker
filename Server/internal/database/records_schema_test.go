package database_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/protocol"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
)

var canonicalRecordColumns = map[string][]string{
	"babies":              {"family_id", "id", "entity_version", "creator_account_id", "last_editor_account_id", "client_mutation_time", "accepted_at", "tombstoned_at", "last_sequence", "storage_bytes", "name", "normalized_name", "normalization_version", "birth_date", "sort_order", "icon"},
	"feed_entries":        {"family_id", "id", "baby_id", "entity_version", "creator_account_id", "last_editor_account_id", "client_mutation_time", "accepted_at", "tombstoned_at", "last_sequence", "storage_bytes", "kind", "started_at", "ended_at", "time_zone_id", "note", "component_version", "feeding_session_id"},
	"sleep_entries":       {"family_id", "id", "baby_id", "entity_version", "creator_account_id", "last_editor_account_id", "client_mutation_time", "accepted_at", "tombstoned_at", "last_sequence", "storage_bytes", "started_at", "ended_at", "time_zone_id", "note", "kind_override", "payload_version", "sleep_session_id"},
	"diaper_entries":      {"family_id", "id", "baby_id", "entity_version", "creator_account_id", "last_editor_account_id", "client_mutation_time", "accepted_at", "tombstoned_at", "last_sequence", "storage_bytes", "started_at", "ended_at", "time_zone_id", "note", "contents", "consistency", "color"},
	"mutation_receipts":   {"family_id", "actor_account_id", "device_id", "mutation_id", "semantic_hash", "outcome", "accepted_at", "last_sequence"},
	"family_record_usage": {"family_id", "canonical_record_count", "charged_storage_bytes", "updated_at"},
	"mutation_journal":    {"family_id", "sequence", "entity_kind", "entity_id", "entity_version", "actor_account_id", "device_id", "operation", "canonical_entity", "accepted_at"},
}

func TestFeedComponentSchemaHasExactPrivilegeAndShape(t *testing.T) {
	fixture := testpg.New(t)
	var current int64
	if err := fixture.Owner.QueryRow(t.Context(), "SELECT current_version FROM schema_compatibility WHERE singleton").Scan(&current); err != nil || current != 29 {
		t.Fatalf("schema version=%d err=%v", current, err)
	}
	for table, want := range map[string][]string{
		"feed_components":  {"family_id", "feed_id", "component_id", "position", "kind", "side", "seconds", "volume_ml", "contents", "foods", "amount", "started_at", "ended_at"},
		"timer_components": {"family_id", "timer_id", "component_id", "position", "kind", "side", "seconds", "volume_ml", "contents", "foods", "amount", "started_at", "ended_at"},
	} {
		rows, err := fixture.Owner.Query(t.Context(), `SELECT a.attname FROM pg_attribute a
			WHERE a.attrelid=($1::text)::regclass AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, "public."+table)
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
			t.Fatalf("%s columns=%v want=%v", table, got, want)
		}
		var enabled, forced, runtimeDML, foreignDML bool
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT c.relrowsecurity,c.relforcerowsecurity,
			has_table_privilege('baby_tracker_runtime',$1,'SELECT,INSERT,DELETE')
				AND NOT has_table_privilege('baby_tracker_runtime',$1,'UPDATE,TRUNCATE,REFERENCES,TRIGGER'),
			NOT has_table_privilege('baby_tracker_projector',$1,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
				AND NOT has_table_privilege('baby_tracker_apple_revoker',$1,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
				AND NOT has_table_privilege('public',$1,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
			FROM pg_class c WHERE c.oid=($1::text)::regclass`, "public."+table).Scan(&enabled, &forced, &runtimeDML, &foreignDML); err != nil {
			t.Fatal(err)
		}
		if !enabled || !forced || !runtimeDML || !foreignDML {
			t.Fatalf("%s rls=%t/%t runtime=%t foreign-denied=%t", table, enabled, forced, runtimeDML, foreignDML)
		}
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 29); err != nil {
		t.Fatalf("schema-twenty exact privilege snapshot rejected: %v", err)
	}
}

var recordRuntimeFunctions = []string{
	"app.save_mutation_receipt(uuid,uuid,uuid,uuid,bytea,jsonb,timestamp with time zone,bigint)",
}

func TestMigration000009InstallsTypedCanonicalRecordSchema(t *testing.T) {
	fixture := testpg.New(t)
	var current int64
	if err := fixture.Owner.QueryRow(t.Context(), "SELECT current_version FROM schema_compatibility WHERE singleton").Scan(&current); err != nil || current != 29 {
		t.Fatalf("schema version=%d err=%v", current, err)
	}
	for table, want := range canonicalRecordColumns {
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
	for _, name := range []string{
		"babies_active_match_idx", "feed_entries_family_baby_idx", "sleep_entries_family_baby_idx",
		"diaper_entries_family_baby_idx", "mutation_receipts_family_cursor_idx", "mutation_journal_entity_idx",
	} {
		var exists bool
		if err := fixture.Owner.QueryRow(t.Context(), "SELECT to_regclass('public.'||$1) IS NOT NULL", name).Scan(&exists); err != nil || !exists {
			t.Errorf("index %s exists=%t err=%v", name, exists, err)
		}
	}
	for _, constraint := range []string{
		"feed_entries_baby_fk", "sleep_entries_baby_fk", "diaper_entries_baby_fk",
		"babies_last_sequence_fk", "feed_entries_last_sequence_fk",
		"sleep_entries_last_sequence_fk", "diaper_entries_last_sequence_fk",
	} {
		var definition string
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid)
			FROM pg_constraint WHERE conname=$1`, constraint).Scan(&definition); err != nil {
			t.Errorf("constraint %s: %v", constraint, err)
		}
		if strings.HasSuffix(constraint, "_last_sequence_fk") && (!strings.Contains(definition, "FOREIGN KEY (family_id, id, entity_version, last_sequence)") || !strings.Contains(definition, "REFERENCES mutation_journal(family_id, entity_id, entity_version, sequence)") || !strings.Contains(definition, "DEFERRABLE INITIALLY DEFERRED")) {
			t.Errorf("constraint %s definition=%q", constraint, definition)
		}
	}
	var normalizedConstraint string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname='babies_normalized_name_bytes'`).Scan(&normalizedConstraint); err != nil || !strings.Contains(normalizedConstraint, "1600") {
		t.Fatalf("normalized-name storage constraint=%q err=%v", normalizedConstraint, err)
	}
	for _, table := range []string{"babies", "feed_entries", "sleep_entries", "diaper_entries"} {
		var defaultExpression string
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT pg_get_expr(d.adbin,d.adrelid)
			FROM pg_attribute a JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
			WHERE a.attrelid=($1::text)::regclass AND a.attname='last_sequence'`, "public."+table).Scan(&defaultExpression); err != nil || defaultExpression != "0" {
			t.Errorf("%s last_sequence default=%q err=%v", table, defaultExpression, err)
		}
	}
	// A bottle's volume and contents are paired on its part since 000027
	// dropped the feed's own scalar columns.
	var pairedBottleFields bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT EXISTS(
		SELECT FROM pg_constraint WHERE conrelid='public.feed_components'::regclass
		AND pg_get_constraintdef(oid) LIKE '%bottle%volume_ml IS NOT NULL%contents IS NOT NULL%')`).Scan(&pairedBottleFields); err != nil || !pairedBottleFields {
		t.Fatalf("bottle part volume/contents invariant exists=%t err=%v", pairedBottleFields, err)
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 29); err != nil {
		t.Fatalf("schema-twenty exact privilege snapshot rejected: %v", err)
	}
	var entityDML, childDML, receiptDML, journalDML, usageDML, saveReceipt bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		(SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT,UPDATE')
			AND NOT has_table_privilege('baby_tracker_runtime',t,'DELETE,TRUNCATE'))
		 FROM unnest(ARRAY['public.babies','public.feed_entries','public.sleep_entries','public.diaper_entries']) t),
		to_regclass('public.feed_side_intervals') IS NULL AND to_regclass('public.feed_foods') IS NULL,
		has_table_privilege('baby_tracker_runtime','public.mutation_receipts','SELECT')
			AND NOT has_table_privilege('baby_tracker_runtime','public.mutation_receipts','INSERT,UPDATE,DELETE,TRUNCATE'),
		has_table_privilege('baby_tracker_runtime','public.mutation_journal','SELECT,INSERT')
			AND NOT has_table_privilege('baby_tracker_runtime','public.mutation_journal','UPDATE,DELETE,TRUNCATE'),
		has_table_privilege('baby_tracker_runtime','public.family_record_usage','SELECT,INSERT,UPDATE')
			AND NOT has_table_privilege('baby_tracker_runtime','public.family_record_usage','DELETE,TRUNCATE'),
		has_function_privilege('baby_tracker_runtime',
			'app.save_mutation_receipt(uuid,uuid,uuid,uuid,bytea,jsonb,timestamp with time zone,bigint)','EXECUTE')`).Scan(
		&entityDML, &childDML, &receiptDML, &journalDML, &usageDML, &saveReceipt); err != nil {
		t.Fatal(err)
	}
	if !entityDML || !childDML || !receiptDML || !journalDML || !usageDML || !saveReceipt {
		t.Fatalf("runtime record grants entity=%t child=%t receipt=%t journal=%t usage=%t save=%t",
			entityDML, childDML, receiptDML, journalDML, usageDML, saveReceipt)
	}
}

func TestReceiptSaveDetectsOldFamilyCollisionWithoutDisclosure(t *testing.T) {
	fixture := testpg.New(t)
	const (
		accountID  = "96000000-0000-4000-8000-000000000001"
		deviceID   = "96000000-0000-4000-8000-000000000002"
		familyA    = "97000000-0000-4000-8000-000000000001"
		familyB    = "97000000-0000-4000-8000-000000000002"
		mutationID = "98000000-0000-4000-8000-000000000001"
	)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	setup, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), setup, tenant.Context{AccountID: accountID, FamilyID: familyA}); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Receipt owner')", accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO families(id,display_name) VALUES($1,'First')", familyA); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", familyA, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(t.Context(), `INSERT INTO devices(id,account_id,server_instance_id,canonical_origin,
		continuity_key_id,continuity_digest,created_at,updated_at)
		VALUES($1,$2,'123e4567-e89b-42d3-a456-426614174000','https://records.example',
		'device.v1',decode(repeat('11',32),'hex'),$3,$3)`, deviceID, accountID, now); err != nil {
		t.Fatal(err)
	}
	if err := setup.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	first, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), first, tenant.Context{AccountID: accountID, FamilyID: familyA}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec(t.Context(), "UPDATE families SET next_sequence=1 WHERE id=$1", familyA); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec(t.Context(), `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,
		actor_account_id,device_id,operation,canonical_entity,accepted_at)
		VALUES($1,1,'baby','98000000-0000-4000-8000-000000000002',1,$2,$3,'create','{}',$4)`, familyA, accountID, deviceID, now); err != nil {
		t.Fatal(err)
	}
	var firstResult string
	if err := first.QueryRow(t.Context(), `SELECT app.save_mutation_receipt($1,$2,$3,$4,$5,$6::jsonb,$7,1)`,
		accountID, familyA, deviceID, mutationID, make([]byte, 32), `{"status":"applied"}`, now).Scan(&firstResult); err != nil || firstResult != "inserted" {
		t.Fatalf("first receipt result=%q err=%v", firstResult, err)
	}
	if err := first.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	transition, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer transition.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), transition, tenant.Context{AccountID: accountID, FamilyID: familyA}); err != nil {
		t.Fatal(err)
	}
	if _, err := transition.Exec(t.Context(), "UPDATE families SET status='deletion_pending' WHERE id=$1", familyA); err != nil {
		t.Fatal(err)
	}
	if _, err := transition.Exec(t.Context(), "UPDATE memberships SET status='removed',removed_at=$3 WHERE family_id=$1 AND account_id=$2", familyA, accountID, now); err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), transition, tenant.Context{AccountID: accountID, FamilyID: familyB}); err != nil {
		t.Fatal(err)
	}
	if _, err := transition.Exec(t.Context(), "INSERT INTO families(id,display_name) VALUES($1,'Second')", familyB); err != nil {
		t.Fatal(err)
	}
	if _, err := transition.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", familyB, accountID); err != nil {
		t.Fatal(err)
	}
	if err := transition.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	second, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), second, tenant.Context{AccountID: accountID, FamilyID: familyB}); err != nil {
		t.Fatal(err)
	}
	var collision string
	if err := second.QueryRow(t.Context(), `SELECT app.save_mutation_receipt($1,$2,$3,$4,$5,$6::jsonb,$7,NULL)`,
		accountID, familyB, deviceID, mutationID, make([]byte, 32), `{"status":"rejected"}`, now).Scan(&collision); err != nil || collision != "collision" {
		t.Fatalf("old-family collision=%q err=%v", collision, err)
	}
	var visible int
	if err := second.QueryRow(t.Context(), "SELECT count(*) FROM mutation_receipts WHERE device_id=$1 AND mutation_id=$2", deviceID, mutationID).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("old receipt visible=%d err=%v", visible, err)
	}
	proof, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer proof.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), proof, tenant.Context{AccountID: accountID, FamilyID: familyA}); err != nil {
		t.Fatal(err)
	}
	var storedHash []byte
	var storedStatus string
	if err := proof.QueryRow(t.Context(), `SELECT semantic_hash,outcome->>'status' FROM mutation_receipts
		WHERE device_id=$1 AND mutation_id=$2`, deviceID, mutationID).Scan(&storedHash, &storedStatus); err != nil || !bytes.Equal(storedHash, make([]byte, 32)) || storedStatus != "applied" {
		t.Fatalf("first receipt changed hash=%x status=%q err=%v", storedHash, storedStatus, err)
	}
	if _, err := second.Exec(t.Context(), `INSERT INTO mutation_receipts(family_id,actor_account_id,device_id,mutation_id,
		semantic_hash,outcome,accepted_at) VALUES($1,$2,$3,'98000000-0000-4000-8000-000000000099',$4,'{}',$5)`,
		familyB, accountID, deviceID, make([]byte, 32), now); postgresState(err) != "42501" {
		t.Fatalf("direct receipt insert err=%v state=%q", err, postgresState(err))
	}
}

func TestRuntimeCursorColumnGrantDoesNotRestoreStructuralWrites(t *testing.T) {
	fixture := testpg.New(t)
	const accountID = "94000000-0000-4000-8000-000000000001"
	const familyID = "95000000-0000-4000-8000-000000000001"
	seedFamilyForRecordSchema(t, fixture, accountID, familyID)

	var nextSequence int64
	tx, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "SELECT id FROM families WHERE id=$1 FOR UPDATE", familyID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(t.Context(), `UPDATE families SET next_sequence=next_sequence+1
		WHERE id=$1 RETURNING next_sequence`, familyID).Scan(&nextSequence); err != nil || nextSequence != 1 {
		t.Fatalf("cursor allocation sequence=%d err=%v", nextSequence, err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	for name, statement := range map[string]string{
		"status":       "UPDATE families SET status='deletion_pending' WHERE id='" + familyID + "'",
		"display_name": "UPDATE families SET display_name='forbidden' WHERE id='" + familyID + "'",
		"purge_after":  "UPDATE families SET purge_after=now() WHERE id='" + familyID + "'",
		"insert":       "INSERT INTO families(id,display_name) VALUES('95000000-0000-4000-8000-000000000002','forbidden')",
		"delete":       "DELETE FROM families WHERE id='" + familyID + "'",
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := fixture.Runtime.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(t.Context(), statement)
			if postgresState(err) != "42501" {
				t.Fatalf("structural write err=%v state=%q", err, postgresState(err))
			}
		})
	}
	foreign, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), foreign, tenant.Context{AccountID: accountID, FamilyID: "95000000-0000-4000-8000-000000000099"}); err != nil {
		t.Fatal(err)
	}
	result, err := foreign.Exec(t.Context(), "UPDATE families SET next_sequence=next_sequence+1 WHERE id=$1", familyID)
	if err != nil || result.RowsAffected() != 0 {
		t.Fatalf("foreign family cursor update rows=%d err=%v", result.RowsAffected(), err)
	}

	var cursor, status, displayName, purgeAfter, broadUpdate bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		has_column_privilege('baby_tracker_runtime','public.families','next_sequence','UPDATE'),
		has_column_privilege('baby_tracker_runtime','public.families','status','UPDATE'),
		has_column_privilege('baby_tracker_runtime','public.families','display_name','UPDATE'),
		has_column_privilege('baby_tracker_runtime','public.families','purge_after','UPDATE'),
		has_table_privilege('baby_tracker_runtime','public.families','UPDATE')`).Scan(&cursor, &status, &displayName, &purgeAfter, &broadUpdate); err != nil {
		t.Fatal(err)
	}
	if !cursor || status || displayName || purgeAfter || broadUpdate {
		t.Fatalf("cursor=%t status=%t display=%t purge=%t broad=%t", cursor, status, displayName, purgeAfter, broadUpdate)
	}
}

func TestEntityZeroSequenceCannotCommitWithoutMatchingJournal(t *testing.T) {
	fixture := testpg.New(t)
	const accountID = "99000000-0000-4000-8000-000000000001"
	const familyID = "99000000-0000-4000-8000-000000000002"
	seedFamilyForRecordSchema(t, fixture, accountID, familyID)
	tx, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO babies(family_id,id,entity_version,creator_account_id,last_editor_account_id,
		client_mutation_time,accepted_at,storage_bytes,name,normalized_name,normalization_version,birth_date,sort_order)
		VALUES($1,'99000000-0000-4000-8000-000000000003',1,$2,$2,now(),now(),1,'Baby','baby','nfc-trim-fold-nfc-v1','2026-01-01',0)`, familyID, accountID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); postgresState(err) != "23503" {
		t.Fatalf("zero last_sequence commit err=%v state=%q", err, postgresState(err))
	}
}

func TestNormalizedBabyNameExpansionPersistsWithinSchemaBound(t *testing.T) {
	fixture := testpg.New(t)
	const accountID = "9a000000-0000-4000-8000-000000000001"
	const familyID = "9a000000-0000-4000-8000-000000000002"
	const babyID = "9a000000-0000-4000-8000-000000000003"
	const deviceID = "9a000000-0000-4000-8000-000000000004"
	seedFamilyForRecordSchema(t, fixture, accountID, familyID)
	// U+0130 occupies two input bytes and expands during case folding. Repeating
	// it 100 times exercises the maximum valid Unicode-scalar name length.
	name := strings.Repeat("İ", 100)
	normalized, err := protocol.NormalizeBabyName(name)
	if err != nil || len(name) != 200 || len(normalized) != 300 || len(normalized) > 1600 {
		t.Fatalf("normalization input_bytes=%d normalized_bytes=%d err=%v", len(name), len(normalized), err)
	}
	tx, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "SELECT id FROM families WHERE id=$1 FOR UPDATE", familyID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "UPDATE families SET next_sequence=1 WHERE id=$1", familyID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO babies(family_id,id,entity_version,creator_account_id,last_editor_account_id,
		client_mutation_time,accepted_at,last_sequence,storage_bytes,name,normalized_name,normalization_version,birth_date,sort_order)
		VALUES($1,$2,1,$3,$3,now(),now(),1,512,$4,$5,'nfc-trim-fold-nfc-v1','2026-01-01',0)`, familyID, babyID, accountID, name, normalized); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,
		actor_account_id,device_id,operation,canonical_entity,accepted_at)
		VALUES($1,1,'baby',$2,1,$3,$4,'create','{}',now())`, familyID, babyID, accountID, deviceID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	read, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), read, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := read.QueryRow(t.Context(), "SELECT normalized_name FROM babies WHERE id=$1", babyID).Scan(&stored); err != nil || stored != normalized {
		t.Fatalf("stored normalized bytes=%d err=%v", len(stored), err)
	}
}

func seedFamilyForRecordSchema(t *testing.T, fixture *testpg.Fixture, accountID, familyID string) {
	t.Helper()
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO accounts(id,display_name) VALUES($1,'Schema owner')", accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO families(id,display_name) VALUES($1,'Schema family')", familyID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "INSERT INTO memberships(family_id,account_id,role) VALUES($1,$2,'owner')", familyID, accountID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func postgresState(err error) string {
	if typed, ok := err.(*pgconn.PgError); ok {
		return typed.Code
	}
	return ""
}
