package database_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
)

var importTableColumns = map[string][]string{
	"import_sessions": {
		"family_id", "import_id", "initiating_account_id", "initiating_device_id", "import_mode",
		"normalization_version", "total_record_count", "stored_record_count", "next_chunk_index",
		"manifest_complete", "finalized_at", "mapping_complete", "mapping_completed_at", "resolver_revision",
		"created_at", "expires_at", "expired_at", "storage_bytes",
	},
	"import_chunks": {
		"family_id", "import_id", "chunk_index", "canonical_sha256", "record_count", "finalize",
		"response_status", "response_body", "storage_bytes", "created_at",
	},
	"import_baby_candidates": {
		"family_id", "import_id", "local_baby_id", "name", "normalized_name", "birth_date", "sort_order", "storage_bytes",
	},
	"import_record_manifests": {
		"family_id", "import_id", "record_id", "entity_kind", "local_baby_id", "chunk_index", "content_sha256", "storage_bytes",
	},
	"import_conflicts": {
		"family_id", "import_id", "conflict_id", "kind", "local_baby_id", "record_id",
		"candidate_baby_ids", "candidate_versions", "candidate_fingerprint", "decision_choice",
		"decision_canonical_baby_id", "decision_account_id", "decision_revision", "decided_at",
		"created_revision", "updated_revision", "created_at", "updated_at", "storage_bytes",
	},
	"import_conflict_receipts": {
		"family_id", "import_id", "command_id", "account_id", "device_id", "command_sha256",
		"resolver_revision", "response_status", "response_body", "storage_bytes", "created_at",
	},
	"import_baby_mappings": {
		"family_id", "import_id", "local_baby_id", "shared_baby_id", "published_revision",
		"shared_baby_entity_version", "shared_baby_cursor", "storage_bytes", "created_at",
	},
}

var importTables = []string{
	"import_sessions", "import_chunks", "import_baby_candidates", "import_record_manifests",
	"import_conflicts", "import_conflict_receipts", "import_baby_mappings",
}

const importCountSignature = "app.import_unfinished_session_count(uuid,timestamp with time zone)"

func TestMigration000012InstallsImportResolverSchema(t *testing.T) {
	fixture := testpg.New(t)
	var current int64
	if err := fixture.Owner.QueryRow(t.Context(), "SELECT current_version FROM schema_compatibility WHERE singleton").Scan(&current); err != nil || current != 29 {
		t.Fatalf("schema version=%d err=%v", current, err)
	}
	var actualImportTables []string
	rows, err := fixture.Owner.Query(t.Context(), `SELECT tablename FROM pg_tables
		WHERE schemaname='public' AND tablename LIKE 'import\_%' ESCAPE '\' ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		actualImportTables = append(actualImportTables, table)
	}
	rows.Close()
	wantImportTables := slices.Clone(importTables)
	slices.Sort(wantImportTables)
	if !slices.Equal(actualImportTables, wantImportTables) {
		t.Fatalf("import table inventory=%v want=%v", actualImportTables, wantImportTables)
	}
	for table, want := range importTableColumns {
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
		"import_sessions_expiry_idx":             {"expires_at", "family_id", "import_id", "WHERE ((NOT mapping_complete) AND (expired_at IS NULL))"},
		"import_sessions_account_unfinished_idx": {"initiating_account_id", "expires_at", "family_id", "import_id", "WHERE ((NOT mapping_complete) AND (expired_at IS NULL))"},
		"import_sessions_retained_account_idx":   {"family_id", "initiating_account_id", "import_id"},
		"import_manifests_local_baby_idx":        {"family_id", "import_id", "local_baby_id", "record_id"},
		"import_conflicts_unresolved_idx":        {"family_id", "import_id", "conflict_id", "WHERE (decision_choice IS NULL)"},
		"import_baby_mappings_shared_key":        {"family_id", "import_id", "shared_baby_id", "UNIQUE"},
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
		"import_chunks_session_fk":              {"FOREIGN KEY (family_id, import_id)", "REFERENCES import_sessions(family_id, import_id) ON DELETE CASCADE"},
		"import_candidates_session_fk":          {"FOREIGN KEY (family_id, import_id)", "REFERENCES import_sessions(family_id, import_id) ON DELETE CASCADE"},
		"import_manifests_candidate_fk":         {"FOREIGN KEY (family_id, import_id, local_baby_id)", "REFERENCES import_baby_candidates(family_id, import_id, local_baby_id)"},
		"import_manifests_chunk_fk":             {"FOREIGN KEY (family_id, import_id, chunk_index)", "REFERENCES import_chunks(family_id, import_id, chunk_index)"},
		"import_conflicts_manifest_fk":          {"FOREIGN KEY (family_id, import_id, record_id)", "REFERENCES import_record_manifests(family_id, import_id, record_id)"},
		"import_mappings_candidate_fk":          {"FOREIGN KEY (family_id, import_id, local_baby_id)", "REFERENCES import_baby_candidates(family_id, import_id, local_baby_id)"},
		"import_mappings_shared_baby_fk":        {"FOREIGN KEY (family_id, shared_baby_id)", "REFERENCES babies(family_id, id)"},
		"import_sessions_state_shape":           {"manifest_complete", "finalized_at", "mapping_complete", "mapping_completed_at", "expired_at"},
		"import_conflicts_decision_state_shape": {"decision_choice", "decision_canonical_baby_id", "decision_account_id", "decision_revision", "decided_at"},
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
	var sessions, conflicts, immutable, denied, helper, secureHelper bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT
		has_table_privilege('baby_tracker_runtime','public.import_sessions','SELECT,INSERT,UPDATE')
			AND NOT has_table_privilege('baby_tracker_runtime','public.import_sessions','DELETE,TRUNCATE,REFERENCES,TRIGGER'),
		has_table_privilege('baby_tracker_runtime','public.import_conflicts','SELECT,INSERT,UPDATE')
			AND NOT has_table_privilege('baby_tracker_runtime','public.import_conflicts','DELETE,TRUNCATE,REFERENCES,TRIGGER'),
		(SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT')
			AND NOT has_table_privilege('baby_tracker_runtime',t,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER'))
		 FROM unnest(ARRAY['public.import_chunks','public.import_baby_candidates','public.import_record_manifests',
			'public.import_conflict_receipts','public.import_baby_mappings']) t),
		NOT (SELECT bool_or(has_table_privilege(r,t,p))
		 FROM unnest(ARRAY['public','baby_tracker_projector','baby_tracker_apple_revoker']) r
		 CROSS JOIN unnest($1::text[]) t
		 CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p),
		has_function_privilege('baby_tracker_runtime',$2,'EXECUTE')
			AND NOT has_function_privilege('public',$2,'EXECUTE')
			AND NOT has_function_privilege('baby_tracker_projector',$2,'EXECUTE')
			AND NOT has_function_privilege('baby_tracker_apple_revoker',$2,'EXECUTE'),
		(SELECT prosecdef AND proconfig=ARRAY['search_path=pg_catalog']
		 FROM pg_proc WHERE oid=$2::regprocedure)`, prefixedImportTables(), importCountSignature).Scan(
		&sessions, &conflicts, &immutable, &denied, &helper, &secureHelper); err != nil {
		t.Fatal(err)
	}
	if !sessions || !conflicts || !immutable || !denied || !helper || !secureHelper {
		t.Fatalf("import privileges sessions=%t conflicts=%t immutable=%t denied=%t helper=%t secure=%t",
			sessions, conflicts, immutable, denied, helper, secureHelper)
	}
}

func TestImportSchemaEnforcesTenantContextAndExactRuntimeDML(t *testing.T) {
	fixture := testpg.New(t)
	const (
		accountA = "a1000000-0000-4000-8000-000000000001"
		familyA  = "a1000000-0000-4000-8000-000000000002"
		deviceA  = "a1000000-0000-4000-8000-000000000003"
		importA  = "a1000000-0000-4000-8000-000000000004"
		babyA    = "a1000000-0000-4000-8000-000000000005"
		conflict = "a1000000-0000-4000-8000-000000000006"
		command  = "a1000000-0000-4000-8000-000000000007"
		accountB = "a2000000-0000-4000-8000-000000000001"
		familyB  = "a2000000-0000-4000-8000-000000000002"
	)
	seedFamilyForRecordSchema(t, fixture, accountA, familyA)
	seedFamilyForRecordSchema(t, fixture, accountB, familyB)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	tx, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountA, FamilyID: familyA}); err != nil {
		t.Fatal(err)
	}
	insertImportSession(t, tx, familyA, importA, accountA, deviceA, now)
	if _, err := tx.Exec(t.Context(), `INSERT INTO import_chunks(family_id,import_id,chunk_index,canonical_sha256,
		record_count,finalize,response_status,response_body,storage_bytes,created_at)
		VALUES($1,$2,0,decode(repeat('11',32),'hex'),1,false,200,convert_to('{"ok":true}','UTF8'),64,$3)`, familyA, importA, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO import_baby_candidates(family_id,import_id,local_baby_id,name,
		normalized_name,birth_date,sort_order,storage_bytes) VALUES($1,$2,$3,'Baby','baby','2026-01-01',0,64)`, familyA, importA, babyA); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO import_record_manifests(family_id,import_id,record_id,entity_kind,
		local_baby_id,chunk_index,content_sha256,storage_bytes)
		VALUES($1,$2,'a1000000-0000-4000-8000-000000000008','feed',$3,0,decode(repeat('22',32),'hex'),64)`, familyA, importA, babyA); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO import_conflicts(family_id,import_id,conflict_id,kind,local_baby_id,
		candidate_baby_ids,candidate_versions,candidate_fingerprint,created_revision,updated_revision,storage_bytes)
		VALUES($1,$2,$3,'ambiguous_baby',$4,'{}','{}',decode(repeat('33',32),'hex'),1,1,64)`, familyA, importA, conflict, babyA); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO import_conflict_receipts(family_id,import_id,command_id,account_id,
		device_id,command_sha256,resolver_revision,response_status,response_body,storage_bytes,created_at)
		VALUES($1,$2,$3,$4,$5,decode(repeat('44',32),'hex'),1,200,convert_to('{"ok":true}','UTF8'),64,$6)`,
		familyA, importA, command, accountA, deviceA, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "UPDATE import_sessions SET stored_record_count=1,next_chunk_index=1 WHERE family_id=$1 AND import_id=$2", familyA, importA); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "UPDATE import_conflicts SET updated_at=updated_at WHERE family_id=$1 AND import_id=$2", familyA, importA); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"import_chunks", "import_baby_candidates", "import_record_manifests", "import_conflict_receipts", "import_baby_mappings"} {
		assertImportStatementState(t, tx, "runtime update "+table, "42501",
			"UPDATE "+pgx.Identifier{table}.Sanitize()+" SET storage_bytes=storage_bytes")
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	for name, context := range map[string]*tenant.Context{
		"without context": nil,
		"wrong family":    {AccountID: accountB, FamilyID: familyB},
	} {
		t.Run(name, func(t *testing.T) {
			read, err := fixture.Runtime.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer read.Rollback(t.Context())
			if context != nil {
				if err := tenant.SetLocal(t.Context(), read, *context); err != nil {
					t.Fatal(err)
				}
			}
			for _, table := range importTables {
				var count int
				if err := read.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil || count != 0 {
					t.Fatalf("%s visible rows=%d err=%v", table, count, err)
				}
			}
			if result, err := read.Exec(t.Context(), "UPDATE import_sessions SET storage_bytes=storage_bytes WHERE family_id=$1", familyA); err != nil || result.RowsAffected() != 0 {
				t.Fatalf("session cross-context update rows=%d err=%v", result.RowsAffected(), err)
			}
			if result, err := read.Exec(t.Context(), "UPDATE import_conflicts SET updated_at=updated_at WHERE family_id=$1", familyA); err != nil || result.RowsAffected() != 0 {
				t.Fatalf("conflict cross-context update rows=%d err=%v", result.RowsAffected(), err)
			}
		})
	}

	for _, context := range []*tenant.Context{nil, {AccountID: accountB, FamilyID: familyB}} {
		attempt, err := fixture.Runtime.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if context != nil {
			if err := tenant.SetLocal(t.Context(), attempt, *context); err != nil {
				t.Fatal(err)
			}
		}
		_, insertErr := attempt.Exec(t.Context(), importSessionInsertSQL, familyA, "a1000000-0000-4000-8000-000000000009", accountA, deviceA, now, now.Add(time.Hour))
		_ = attempt.Rollback(t.Context())
		if postgresState(insertErr) != "42501" {
			t.Fatalf("cross-context session insert err=%v state=%q", insertErr, postgresState(insertErr))
		}
	}
}

func TestImportSchemaRejectsMalformedStateDigestsAndNonInjectiveMappings(t *testing.T) {
	fixture := testpg.New(t)
	const (
		accountID = "b1000000-0000-4000-8000-000000000001"
		familyID  = "b1000000-0000-4000-8000-000000000002"
		deviceID  = "b1000000-0000-4000-8000-000000000003"
		importID  = "b1000000-0000-4000-8000-000000000004"
		babyA     = "b1000000-0000-4000-8000-000000000005"
		babyB     = "b1000000-0000-4000-8000-000000000006"
		sharedID  = "b1000000-0000-4000-8000-000000000007"
	)
	seedFamilyForRecordSchema(t, fixture, accountID, familyID)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	tx, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	insertImportSession(t, tx, familyID, importID, accountID, deviceID, now)
	assertImportStatementState(t, tx, "invalid session mode", "23514", `INSERT INTO import_sessions(
		family_id,import_id,initiating_account_id,initiating_device_id,import_mode,normalization_version,
		total_record_count,stored_record_count,next_chunk_index,manifest_complete,mapping_complete,resolver_revision,
		created_at,expires_at,storage_bytes)
		VALUES($1,$2,$3,$4,'bad-mode','nfc-trim-fold-nfc-v1',1,0,0,false,false,1,$5,$6,64)`,
		familyID, "b1000000-0000-4000-8000-000000000008", accountID, deviceID, now, now.Add(time.Hour))
	assertImportStatementState(t, tx, "malformed session state", "23514", `UPDATE import_sessions
		SET mapping_complete=true WHERE family_id=$1 AND import_id=$2`, familyID, importID)
	assertImportStatementState(t, tx, "finalized incomplete session", "23514", `UPDATE import_sessions
		SET manifest_complete=true,finalized_at=$3 WHERE family_id=$1 AND import_id=$2`, familyID, importID, now)
	assertImportStatementState(t, tx, "mapping completed after expiry", "23514", `UPDATE import_sessions
		SET stored_record_count=total_record_count,manifest_complete=true,finalized_at=expires_at,
			mapping_complete=true,mapping_completed_at=expires_at + interval '1 microsecond'
		WHERE family_id=$1 AND import_id=$2`, familyID, importID)
	if _, err := tx.Exec(t.Context(), `INSERT INTO import_sessions(family_id,import_id,initiating_account_id,initiating_device_id,
		import_mode,normalization_version,total_record_count,stored_record_count,next_chunk_index,manifest_complete,finalized_at,
		mapping_complete,mapping_completed_at,resolver_revision,created_at,expires_at,storage_bytes)
		VALUES($1,'b1000000-0000-4000-8000-000000000013',$2,$3,'match_shared','nfc-trim-fold-nfc-v1',
		1,1,1,true,$4,true,$4,1,$5,$4,64)`, familyID, accountID, deviceID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("valid deadline-bound completion rejected: %v", err)
	}
	assertImportStatementState(t, tx, "short chunk digest", "23514", `INSERT INTO import_chunks(family_id,import_id,
		chunk_index,canonical_sha256,record_count,finalize,response_status,response_body,storage_bytes,created_at)
		VALUES($1,$2,0,decode(repeat('11',31),'hex'),1,false,200,'x',1,$3)`, familyID, importID, now)
	assertImportStatementState(t, tx, "empty nonfinal chunk", "23514", `INSERT INTO import_chunks(family_id,import_id,
		chunk_index,canonical_sha256,record_count,finalize,response_status,response_body,storage_bytes,created_at)
		VALUES($1,$2,0,decode(repeat('11',32),'hex'),0,false,200,'x',1,$3)`, familyID, importID, now)
	if _, err := tx.Exec(t.Context(), `INSERT INTO import_chunks(family_id,import_id,chunk_index,canonical_sha256,
		record_count,finalize,response_status,response_body,storage_bytes,created_at)
		VALUES($1,$2,0,decode(repeat('11',32),'hex'),1,false,200,'x',1,$3)`, familyID, importID, now); err != nil {
		t.Fatal(err)
	}
	for _, baby := range []string{babyA, babyB} {
		if _, err := tx.Exec(t.Context(), `INSERT INTO import_baby_candidates(family_id,import_id,local_baby_id,name,
			normalized_name,birth_date,sort_order,storage_bytes) VALUES($1,$2,$3,'Baby','baby','2026-01-01',0,1)`, familyID, importID, baby); err != nil {
			t.Fatal(err)
		}
	}
	assertImportStatementState(t, tx, "short manifest digest", "23514", `INSERT INTO import_record_manifests(
		family_id,import_id,record_id,entity_kind,local_baby_id,chunk_index,content_sha256,storage_bytes)
		VALUES($1,$2,'b1000000-0000-4000-8000-000000000009','feed',$3,0,decode(repeat('22',31),'hex'),1)`, familyID, importID, babyA)
	assertImportStatementState(t, tx, "unknown manifest kind", "23514", `INSERT INTO import_record_manifests(
		family_id,import_id,record_id,entity_kind,local_baby_id,chunk_index,content_sha256,storage_bytes)
		VALUES($1,$2,'b1000000-0000-4000-8000-000000000009','baby',$3,0,decode(repeat('22',32),'hex'),1)`, familyID, importID, babyA)
	assertImportStatementState(t, tx, "missing manifest candidate", "23503", `INSERT INTO import_record_manifests(
		family_id,import_id,record_id,entity_kind,local_baby_id,chunk_index,content_sha256,storage_bytes)
		VALUES($1,$2,'b1000000-0000-4000-8000-000000000009','timer','b1000000-0000-4000-8000-000000000010',0,decode(repeat('22',32),'hex'),1)`, familyID, importID)
	assertImportStatementState(t, tx, "mismatched conflict candidates", "23514", `INSERT INTO import_conflicts(
		family_id,import_id,conflict_id,kind,local_baby_id,candidate_baby_ids,candidate_versions,candidate_fingerprint,
		created_revision,updated_revision,storage_bytes) VALUES($1,$2,'b1000000-0000-4000-8000-000000000011',
		'ambiguous_baby',$3,ARRAY[$4::uuid],ARRAY[]::bigint[],decode(repeat('33',32),'hex'),1,1,1)`, familyID, importID, babyA, sharedID)
	assertImportStatementState(t, tx, "malformed keep-local decision", "23514", `INSERT INTO import_conflicts(
		family_id,import_id,conflict_id,kind,local_baby_id,candidate_baby_ids,candidate_versions,candidate_fingerprint,
		decision_choice,decision_canonical_baby_id,decision_account_id,decision_revision,decided_at,
		created_revision,updated_revision,storage_bytes) VALUES($1,$2,'b1000000-0000-4000-8000-000000000011',
		'ambiguous_baby',$3,ARRAY[$4::uuid],ARRAY[1::bigint],decode(repeat('33',32),'hex'),
		'keep_local',$4,$5,1,$6,1,1,1)`, familyID, importID, babyA, sharedID, accountID, now)
	assertImportStatementState(t, tx, "missing decision choice with payload", "23514", `INSERT INTO import_conflicts(
		family_id,import_id,conflict_id,kind,local_baby_id,candidate_baby_ids,candidate_versions,candidate_fingerprint,
		decision_account_id,decision_revision,decided_at,created_revision,updated_revision,storage_bytes)
		VALUES($1,$2,'b1000000-0000-4000-8000-000000000014','ambiguous_baby',$3,ARRAY[$4::uuid],ARRAY[1::bigint],
		decode(repeat('33',32),'hex'),$5,1,$6,1,1,1)`, familyID, importID, babyA, sharedID, accountID, now)
	assertImportStatementState(t, tx, "partial use-existing decision", "23514", `INSERT INTO import_conflicts(
		family_id,import_id,conflict_id,kind,local_baby_id,candidate_baby_ids,candidate_versions,candidate_fingerprint,
		decision_choice,decision_canonical_baby_id,decision_revision,decided_at,created_revision,updated_revision,storage_bytes)
		VALUES($1,$2,'b1000000-0000-4000-8000-000000000015','ambiguous_baby',$3,ARRAY[$4::uuid],ARRAY[1::bigint],
		decode(repeat('33',32),'hex'),'use_existing',$4,1,$5,1,1,1)`, familyID, importID, babyA, sharedID, now)
	assertImportStatementState(t, tx, "short receipt digest", "23514", `INSERT INTO import_conflict_receipts(
		family_id,import_id,command_id,account_id,device_id,command_sha256,resolver_revision,response_status,response_body,storage_bytes,created_at)
		VALUES($1,$2,'b1000000-0000-4000-8000-000000000012',$3,$4,decode(repeat('44',31),'hex'),1,200,'x',1,$5)`, familyID, importID, accountID, deviceID, now)

	if _, err := tx.Exec(t.Context(), "UPDATE families SET next_sequence=1 WHERE id=$1", familyID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO mutation_journal(family_id,sequence,entity_kind,entity_id,entity_version,
		actor_account_id,device_id,operation,canonical_entity,accepted_at)
		VALUES($1,1,'baby',$2,1,$3,$4,'create','{}',$5)`, familyID, sharedID, accountID, deviceID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO babies(family_id,id,entity_version,creator_account_id,last_editor_account_id,
		client_mutation_time,accepted_at,last_sequence,storage_bytes,name,normalized_name,normalization_version,birth_date,sort_order)
		VALUES($1,$2,1,$3,$3,$4,$4,1,1,'Baby','baby','nfc-trim-fold-nfc-v1','2026-01-01',0)`, familyID, sharedID, accountID, now); err != nil {
		t.Fatal(err)
	}
	for _, baby := range []string{babyA, babyB} {
		_, err := tx.Exec(t.Context(), `INSERT INTO import_baby_mappings(family_id,import_id,local_baby_id,shared_baby_id,
			published_revision,shared_baby_entity_version,shared_baby_cursor,storage_bytes,created_at)
			VALUES($1,$2,$3,$4,1,1,1,1,$5)`, familyID, importID, baby, sharedID, now)
		if baby == babyB {
			if postgresState(err) != "23505" {
				t.Fatalf("non-injective mapping err=%v state=%q", err, postgresState(err))
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestImportUnfinishedCountHelperIsGlobalAndTenantValidated(t *testing.T) {
	fixture := testpg.New(t)
	const (
		accountA = "c1000000-0000-4000-8000-000000000001"
		familyA  = "c1000000-0000-4000-8000-000000000002"
		familyB  = "c1000000-0000-4000-8000-000000000003"
		accountB = "c1000000-0000-4000-8000-000000000004"
		deviceA  = "c1000000-0000-4000-8000-000000000005"
	)
	seedFamilyForRecordSchema(t, fixture, accountA, familyA)
	seedFamilyForRecordSchema(t, fixture, accountB, familyB)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	for index, family := range []string{familyA, familyB} {
		tx, err := fixture.Owner.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountA, FamilyID: family}); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), importSessionInsertSQL, family,
			[]string{"c1000000-0000-4000-8000-000000000006", "c1000000-0000-4000-8000-000000000007"}[index],
			accountA, deviceA, now, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	read, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback(t.Context())
	if err := tenant.SetLocal(t.Context(), read, tenant.Context{AccountID: accountA, FamilyID: familyA}); err != nil {
		t.Fatal(err)
	}
	var direct, directAfterSetRole, global int64
	if err := read.QueryRow(t.Context(), "SELECT count(*) FROM import_sessions").Scan(&direct); err != nil || direct != 1 {
		t.Fatalf("direct tenant count=%d err=%v", direct, err)
	}
	if err := read.QueryRow(t.Context(), "SELECT app.import_unfinished_session_count($1,$2)", accountA, now).Scan(&global); err != nil || global != 2 {
		t.Fatalf("global helper count=%d err=%v", global, err)
	}
	if _, err := read.Exec(t.Context(), "SET LOCAL ROLE baby_tracker_runtime"); err != nil {
		t.Fatal(err)
	}
	if err := read.QueryRow(t.Context(), "SELECT count(*) FROM import_sessions").Scan(&directAfterSetRole); err != nil || directAfterSetRole != 1 {
		t.Fatalf("SET ROLE widened direct tenant count=%d err=%v", directAfterSetRole, err)
	}
	if err := read.QueryRow(t.Context(), "SELECT app.import_unfinished_session_count($1,$2)", accountA, now).Scan(&global); err != nil || global != 2 {
		t.Fatalf("global helper after SET ROLE count=%d err=%v", global, err)
	}
	if err := read.QueryRow(t.Context(), "SELECT app.import_unfinished_session_count($1,$2)", accountB, now).Scan(&global); postgresState(err) != "42501" {
		t.Fatalf("cross-account helper err=%v state=%q", err, postgresState(err))
	}
	without, err := fixture.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer without.Rollback(t.Context())
	if err := without.QueryRow(t.Context(), "SELECT app.import_unfinished_session_count($1,$2)", accountA, now).Scan(&global); postgresState(err) != "42501" {
		t.Fatalf("context-free helper err=%v state=%q", err, postgresState(err))
	}
}

const importSessionInsertSQL = `INSERT INTO import_sessions(family_id,import_id,initiating_account_id,initiating_device_id,
	import_mode,normalization_version,total_record_count,stored_record_count,next_chunk_index,manifest_complete,mapping_complete,
	resolver_revision,created_at,expires_at,storage_bytes)
	VALUES($1,$2,$3,$4,'match_shared','nfc-trim-fold-nfc-v1',1,0,0,false,false,1,$5,$6,64)`

func insertImportSession(t *testing.T, tx pgx.Tx, familyID, importID, accountID, deviceID string, now time.Time) {
	t.Helper()
	if _, err := tx.Exec(t.Context(), importSessionInsertSQL, familyID, importID, accountID, deviceID, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func assertImportStatementState(t *testing.T, tx pgx.Tx, name, wantState, statement string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(t.Context(), "SAVEPOINT import_contract"); err != nil {
		t.Fatal(err)
	}
	_, err := tx.Exec(t.Context(), statement, args...)
	if postgresState(err) != wantState {
		t.Fatalf("%s err=%v state=%q want=%q", name, err, postgresState(err), wantState)
	}
	if _, rollbackErr := tx.Exec(t.Context(), "ROLLBACK TO SAVEPOINT import_contract"); rollbackErr != nil {
		t.Fatal(rollbackErr)
	}
}

func prefixedImportTables() []string {
	tables := make([]string, len(importTables))
	for index, table := range importTables {
		tables[index] = "public." + table
	}
	return tables
}
