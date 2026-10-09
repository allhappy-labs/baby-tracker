package database_test

import (
	"slices"
	"strings"
	"testing"

	"babytracker/server/internal/database"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
)

var timerTableColumns = map[string][]string{
	"active_timers": {
		"family_id", "id", "baby_id", "entity_version", "creator_account_id", "last_editor_account_id",
		"client_mutation_time", "accepted_at", "tombstoned_at", "last_sequence", "storage_bytes",
		"timer_kind", "started_at", "time_zone_id", "current_side", "current_side_started_at",
		"completed_record_id", "component_version", "current_bottle_started_at", "session_id",
	},
	"timer_components": {
		"family_id", "timer_id", "component_id", "position", "kind", "side", "seconds", "volume_ml", "contents",
		"foods", "amount", "started_at", "ended_at",
	},
}

func TestMigration000011InstallsTypedTimerSchema(t *testing.T) {
	fixture := testpg.New(t)
	var current int64
	if err := fixture.Owner.QueryRow(t.Context(), "SELECT current_version FROM schema_compatibility WHERE singleton").Scan(&current); err != nil || current != 29 {
		t.Fatalf("schema version=%d err=%v", current, err)
	}
	for table, want := range timerTableColumns {
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

	for index, fragments := range map[string][]string{
		"one_live_timer_per_kind":     {"family_id", "baby_id", "timer_kind", "WHERE (tombstoned_at IS NULL)"},
		"active_timers_live_baby_idx": {"family_id", "baby_id", "id", "WHERE (tombstoned_at IS NULL)"},
		"timer_components_pkey":       {"family_id", "timer_id", "position"},
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

	var dropped bool
	for constraint, fragments := range map[string][]string{
		"active_timers_baby_fk":                 {"FOREIGN KEY (family_id, baby_id)", "REFERENCES babies(family_id, id)"},
		"active_timers_last_sequence_fk":        {"FOREIGN KEY (family_id, id, entity_version, last_sequence)", "REFERENCES mutation_journal(family_id, entity_id, entity_version, sequence)", "DEFERRABLE INITIALLY DEFERRED"},
		"timer_components_timer_fk":             {"FOREIGN KEY (family_id, timer_id)", "REFERENCES active_timers(family_id, id) ON DELETE CASCADE"},
		"active_timers_state_shape":             {"timer_kind", "current_side", "current_side_started_at", "current_bottle_started_at", "completed_record_id"},
		"active_timers_component_version_check": {"component_version = 5"},
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

	if err := fixture.Owner.QueryRow(t.Context(), "SELECT to_regclass('public.timer_side_intervals') IS NULL").Scan(&dropped); err != nil || !dropped {
		t.Errorf("timer_side_intervals dropped=%t err=%v", dropped, err)
	}
	for _, table := range []string{"active_timers", "timer_components"} {
		var using, check string
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT coalesce(qual,''),coalesce(with_check,'')
			FROM pg_policies WHERE schemaname='public' AND tablename=$1 AND policyname='family_context'`, table).Scan(&using, &check); err != nil {
			t.Errorf("%s family policy: %v", table, err)
			continue
		}
		if using != "(family_id = app.current_family_id())" || check != "(family_id = app.current_family_id())" {
			t.Errorf("%s family policy using=%q check=%q", table, using, check)
		}
	}

	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 29); err != nil {
		t.Fatalf("schema-twenty exact privilege snapshot rejected: %v", err)
	}
	var timers, intervals, publicAccess, projectorAccess, revokerAccess bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		has_table_privilege('baby_tracker_runtime','public.active_timers','SELECT,INSERT,UPDATE')
			AND NOT has_table_privilege('baby_tracker_runtime','public.active_timers','DELETE,TRUNCATE,REFERENCES,TRIGGER'),
		has_table_privilege('baby_tracker_runtime','public.timer_components','SELECT,INSERT,DELETE')
			AND NOT has_table_privilege('baby_tracker_runtime','public.timer_components','UPDATE,TRUNCATE,REFERENCES,TRIGGER'),
		(SELECT bool_or(has_table_privilege('public',t,p)) FROM unnest(ARRAY['public.active_timers','public.timer_components']) t
			CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p),
		(SELECT bool_or(has_table_privilege('baby_tracker_projector',t,p)) FROM unnest(ARRAY['public.active_timers','public.timer_components']) t
			CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p),
		(SELECT bool_or(has_table_privilege('baby_tracker_apple_revoker',t,p)) FROM unnest(ARRAY['public.active_timers','public.timer_components']) t
			CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p)`).Scan(
		&timers, &intervals, &publicAccess, &projectorAccess, &revokerAccess); err != nil {
		t.Fatal(err)
	}
	if !timers || !intervals || publicAccess || projectorAccess || revokerAccess {
		t.Fatalf("timer grants timers=%t intervals=%t public=%t projector=%t revoker=%t",
			timers, intervals, publicAccess, projectorAccess, revokerAccess)
	}
}

func TestTimerSchemaEnforcesTenantRLSBehavior(t *testing.T) {
	fixture := testpg.New(t)
	const (
		accountID      = "bb000000-0000-4000-8000-000000000001"
		familyID       = "bb000000-0000-4000-8000-000000000002"
		babyID         = "bb000000-0000-4000-8000-000000000003"
		deviceID       = "bb000000-0000-4000-8000-000000000004"
		timerID        = "bb000000-0000-4000-8000-000000000005"
		otherAccountID = "bb000000-0000-4000-8000-000000000006"
		otherFamilyID  = "bb000000-0000-4000-8000-000000000007"
	)
	seedFamilyForRecordSchema(t, fixture, accountID, familyID)
	seedFamilyForRecordSchema(t, fixture, otherAccountID, otherFamilyID)

	seed, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), seed, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(t.Context(), "UPDATE families SET next_sequence=2 WHERE id=$1", familyID); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(t.Context(), `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,
		actor_account_id,device_id,operation,canonical_entity,accepted_at) VALUES
		($1,1,'baby',$2,1,$3,$4,'create','{}',now()),
		($1,2,'timer',$5,1,$3,$4,'timer_start','{}',now())`, familyID, babyID, accountID, deviceID, timerID); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(t.Context(), `INSERT INTO babies(family_id,id,entity_version,creator_account_id,last_editor_account_id,
		client_mutation_time,accepted_at,last_sequence,storage_bytes,name,normalized_name,normalization_version,birth_date,sort_order)
		VALUES($1,$2,1,$3,$3,now(),now(),1,1,'Baby','baby','nfc-trim-fold-nfc-v1','2026-01-01',0)`,
		familyID, babyID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(t.Context(), `INSERT INTO active_timers(family_id,id,baby_id,entity_version,creator_account_id,
		last_editor_account_id,client_mutation_time,accepted_at,last_sequence,storage_bytes,timer_kind,started_at,
		time_zone_id,current_side,current_side_started_at)
		VALUES($1,$2,$3,1,$4,$4,now(),now(),2,1,'feed',now(),'UTC','left',now())`, familyID, timerID, babyID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(t.Context(), `INSERT INTO timer_components(family_id,timer_id,component_id,position,kind,side,seconds)
		VALUES($1,$2,'bb000000-0000-4000-8000-00000000000a',0,'breast','left',30)`, familyID, timerID); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	withoutContext, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer withoutContext.Rollback(t.Context())
	var timers, intervals int
	if err := withoutContext.QueryRow(t.Context(), "SELECT count(*) FROM active_timers").Scan(&timers); err != nil || timers != 0 {
		t.Fatalf("context-free timers=%d err=%v", timers, err)
	}
	if err := withoutContext.QueryRow(t.Context(), "SELECT count(*) FROM timer_components").Scan(&intervals); err != nil || intervals != 0 {
		t.Fatalf("context-free intervals=%d err=%v", intervals, err)
	}

	assertInsertDenied := func(name string, context *tenant.Context, statement string, args ...any) {
		t.Helper()
		tx, err := fixture.Runtime.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		if context != nil {
			if err := tenant.SetLocal(t.Context(), tx, *context); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := tx.Exec(t.Context(), statement, args...); postgresState(err) != "42501" {
			t.Fatalf("%s insert err=%v state=%q", name, err, postgresState(err))
		}
	}
	insertTimer := `INSERT INTO active_timers(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,
		client_mutation_time,accepted_at,last_sequence,storage_bytes,timer_kind,started_at,time_zone_id,current_side,current_side_started_at)
		VALUES($1,$2,$3,1,$4,$4,now(),now(),2,1,'feed',now(),'UTC','left',now())`
	insertInterval := `INSERT INTO timer_components(family_id,timer_id,component_id,position,kind,side,seconds)
		VALUES($1,$2,'bb000000-0000-4000-8000-00000000000b',1,'breast','right',10)`
	assertInsertDenied("context-free timer", nil, insertTimer, familyID, "bb000000-0000-4000-8000-000000000008", babyID, accountID)
	assertInsertDenied("context-free interval", nil, insertInterval, familyID, timerID)

	owner, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), owner, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(t.Context(), `SELECT count(*),count(i.*) FROM active_timers t
		LEFT JOIN timer_components i ON i.family_id=t.family_id AND i.timer_id=t.id`).Scan(&timers, &intervals); err != nil || timers != 1 || intervals != 1 {
		t.Fatalf("owner timers=%d intervals=%d err=%v", timers, intervals, err)
	}
	if result, err := owner.Exec(t.Context(), "UPDATE active_timers SET storage_bytes=storage_bytes WHERE family_id=$1 AND id=$2", familyID, timerID); err != nil || result.RowsAffected() != 1 {
		t.Fatalf("owner timer update rows=%d err=%v", result.RowsAffected(), err)
	}
	if _, err := owner.Exec(t.Context(), insertInterval, familyID, timerID); err != nil {
		t.Fatalf("owner interval insert: %v", err)
	}
	if result, err := owner.Exec(t.Context(), "DELETE FROM timer_components WHERE family_id=$1 AND timer_id=$2 AND position=1", familyID, timerID); err != nil || result.RowsAffected() != 1 {
		t.Fatalf("owner interval delete rows=%d err=%v", result.RowsAffected(), err)
	}
	if err := owner.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	wrongContext := tenant.Context{AccountID: otherAccountID, FamilyID: otherFamilyID}
	wrong, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), wrong, wrongContext); err != nil {
		t.Fatal(err)
	}
	if err := wrong.QueryRow(t.Context(), "SELECT count(*) FROM active_timers").Scan(&timers); err != nil || timers != 0 {
		t.Fatalf("wrong-family timers=%d err=%v", timers, err)
	}
	if err := wrong.QueryRow(t.Context(), "SELECT count(*) FROM timer_components").Scan(&intervals); err != nil || intervals != 0 {
		t.Fatalf("wrong-family intervals=%d err=%v", intervals, err)
	}
	if result, err := wrong.Exec(t.Context(), "UPDATE active_timers SET storage_bytes=storage_bytes WHERE family_id=$1 AND id=$2", familyID, timerID); err != nil || result.RowsAffected() != 0 {
		t.Fatalf("wrong-family timer update rows=%d err=%v", result.RowsAffected(), err)
	}
	if result, err := wrong.Exec(t.Context(), "DELETE FROM timer_components WHERE family_id=$1 AND timer_id=$2", familyID, timerID); err != nil || result.RowsAffected() != 0 {
		t.Fatalf("wrong-family interval delete rows=%d err=%v", result.RowsAffected(), err)
	}
	if err := wrong.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertInsertDenied("wrong-family timer", &wrongContext, insertTimer, familyID, "bb000000-0000-4000-8000-000000000009", babyID, accountID)
	assertInsertDenied("wrong-family interval", &wrongContext, insertInterval, familyID, timerID)
}

func TestTimerSchemaEnforcesLiveUniquenessJournalAndTenantBoundaries(t *testing.T) {
	fixture := testpg.New(t)
	const (
		accountID = "ba000000-0000-4000-8000-000000000001"
		familyID  = "ba000000-0000-4000-8000-000000000002"
		babyID    = "ba000000-0000-4000-8000-000000000003"
		deviceID  = "ba000000-0000-4000-8000-000000000004"
		timerA    = "ba000000-0000-4000-8000-000000000005"
		timerB    = "ba000000-0000-4000-8000-000000000006"
	)
	seedFamilyForRecordSchema(t, fixture, accountID, familyID)
	tx, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,
		actor_account_id,device_id,operation,canonical_entity,accepted_at)
		VALUES($1,1,'baby',$2,1,$3,$4,'create','{}',now())`, familyID, babyID, accountID, deviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO babies(family_id,id,entity_version,creator_account_id,last_editor_account_id,
		client_mutation_time,accepted_at,last_sequence,storage_bytes,name,normalized_name,normalization_version,birth_date,sort_order)
		VALUES($1,$2,1,$3,$3,now(),now(),1,1,'Baby','baby','nfc-trim-fold-nfc-v1','2026-01-01',0)`, familyID, babyID, accountID); err != nil {
		t.Fatal(err)
	}
	insertTimer := `INSERT INTO active_timers(family_id,id,baby_id,entity_version,creator_account_id,last_editor_account_id,
		client_mutation_time,accepted_at,storage_bytes,timer_kind,started_at,time_zone_id,current_side,current_side_started_at)
		VALUES($1,$2,$3,1,$4,$4,now(),now(),1,'feed',now(),'UTC','left',now())`
	if _, err := tx.Exec(t.Context(), insertTimer, familyID, timerA, babyID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "SAVEPOINT duplicate_live_timer"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), insertTimer, familyID, timerB, babyID, accountID); postgresState(err) != "23505" {
		t.Fatalf("second live timer err=%v state=%q", err, postgresState(err))
	}
	if _, err := tx.Exec(t.Context(), "ROLLBACK TO SAVEPOINT duplicate_live_timer"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "UPDATE active_timers SET tombstoned_at=now() WHERE family_id=$1 AND id=$2", familyID, timerA); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), insertTimer, familyID, timerB, babyID, accountID); err != nil {
		t.Fatalf("replacement after tombstone: %v", err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO timer_components(family_id,timer_id,component_id,position,kind,side,seconds)
		VALUES($1,$2,'ba000000-0000-4000-8000-00000000000a',0,'breast','left',30)`, familyID, timerB); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); postgresState(err) != "23503" {
		t.Fatalf("timer without matching journal committed err=%v state=%q", err, postgresState(err))
	}
}
