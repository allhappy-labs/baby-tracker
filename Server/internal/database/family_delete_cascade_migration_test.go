package database_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"babytracker/server/internal/database"
	"babytracker/server/internal/testpg"
)

// familyImportTables are the family-owned import tables a family delete must
// empty through its cascades.
var familyImportTables = []string{"import_sessions", "import_chunks", "import_baby_candidates", "import_record_manifests", "import_baby_mappings"}

// importedFamily is a family's import session, a shared baby its candidate is
// mapped to, and a feed of that baby: every row a raw family delete must take
// through the cascades.
func seedImportedFamily(t *testing.T, f *testpg.Fixture, account, family, device string, tag byte) {
	t.Helper()
	ctx := t.Context()
	id := func(n int) string { return fmt.Sprintf("%02x%06x-0000-4000-8000-%012x", tag, 0xd00000, n) }
	importID, localBaby, sharedBaby, feed, record := id(1), id(2), id(3), id(4), id(5)
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	tx := tenantTx(t, f.Runtime, account, family)
	care := `INSERT INTO care_entities(family_id,id,kind,baby_id,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,
		started_at,sort_order,name_hash,feed_kind,wire)
		VALUES ($1,$2,$3,$4,1,$5,$6,$6,$7,'create',now(),$8,$9,$10,$11,convert_to(repeat('x',80),'UTF8'))`
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{care, []any{family, sharedBaby, int16(1), nil, int64(1), account, device, nil, int32(0), authorityDigest(tag), nil}},
		{care, []any{family, feed, int16(2), sharedBaby, int64(2), account, device, now, nil, nil, int16(2)}},
		{importSessionInsertSQL, []any{family, importID, account, device, now, now.Add(time.Hour)}},
		{`INSERT INTO import_chunks(family_id,import_id,chunk_index,canonical_sha256,record_count,finalize,response_status,response_body,storage_bytes,created_at)
			VALUES($1,$2,0,decode(repeat('11',32),'hex'),1,false,200,'x',1,$3)`, []any{family, importID, now}},
		{`INSERT INTO import_baby_candidates(family_id,import_id,local_baby_id,name,normalized_name,birth_date,sort_order,storage_bytes)
			VALUES($1,$2,$3,'Baby','baby','2026-01-01',0,1)`, []any{family, importID, localBaby}},
		{`INSERT INTO import_record_manifests(family_id,import_id,record_id,entity_kind,local_baby_id,chunk_index,content_sha256,storage_bytes)
			VALUES($1,$2,$3,'feed',$4,0,decode(repeat('22',32),'hex'),1)`, []any{family, importID, record, localBaby}},
		{`INSERT INTO import_baby_mappings(family_id,import_id,local_baby_id,shared_baby_id,published_revision,shared_baby_entity_version,shared_baby_cursor,storage_bytes,created_at)
			VALUES($1,$2,$3,$4,1,1,1,1,$5)`, []any{family, importID, localBaby, sharedBaby, now}},
	} {
		if _, err := tx.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatalf("seed %q: %v", step.sql, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// seedImportedFamilies gives both of seedAuthorityFamilies' families an
// import mapping, a shared baby and a feed.
func seedImportedFamilies(t *testing.T, f *testpg.Fixture) authorityFixture {
	t.Helper()
	ids := seedAuthorityFamilies(t, f)
	const deviceB = "63000000-0000-4000-8000-000000000002"
	if err := authorityExec(t.Context(), f.Owner, `INSERT INTO devices(id,account_id,canonical_origin,continuity_key_id,continuity_digest,created_at,updated_at)
		VALUES ($1,$2,'https://tracker.example','continuity.v1',$3,now(),now())`, deviceB, ids.accountB, authorityDigest(8)); err != nil {
		t.Fatal(err)
	}
	seedImportedFamily(t, f, ids.accountA, ids.familyA, ids.deviceA, 0xa1)
	seedImportedFamily(t, f, ids.accountB, ids.familyB, deviceB, 0xb1)
	return ids
}

// familyRows counts a family's rows in the care and import tables, past row
// level security.
func familyRows(t *testing.T, su *pgxpool.Pool, family string) map[string]int {
	t.Helper()
	got := map[string]int{}
	for _, table := range append([]string{"care_entities"}, familyImportTables...) {
		var n int
		if err := su.QueryRow(t.Context(), `SELECT count(*) FROM `+table+` WHERE family_id=$1`, family).Scan(&n); err != nil {
			t.Fatal(err)
		}
		got[table] = n
	}
	return got
}

// cascadeOrder recreates one of families' two cascades, so its trigger (named
// after its new, highest oid) fires after the other: careFirst recreates the
// import_sessions cascade, otherwise the care_entities cascade is recreated.
// It then checks the order the triggers fire in (by name, as PostgreSQL sorts
// them).
func cascadeOrder(t *testing.T, f *testpg.Fixture, careFirst bool) {
	t.Helper()
	ctx := t.Context()
	table, constraint := "public.care_entities", "care_entities_family_id_fkey"
	if careFirst {
		table, constraint = "public.import_sessions", "import_sessions_family_id_fkey"
	}
	var definition string
	if err := f.Owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname=$1 AND conrelid=$2::regclass`, constraint, table).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Owner.Exec(ctx, `ALTER TABLE `+table+` DROP CONSTRAINT `+constraint+`; ALTER TABLE `+table+` ADD CONSTRAINT `+constraint+` `+definition); err != nil {
		t.Fatalf("recreate %s: %v", constraint, err)
	}
	var careLast, sessionsLast string
	if err := f.Owner.QueryRow(ctx, `SELECT
			(SELECT max(tg.tgname COLLATE "C") FROM pg_trigger tg JOIN pg_constraint c ON c.oid=tg.tgconstraint
				WHERE tg.tgrelid='public.families'::regclass AND c.conname='care_entities_family_id_fkey'),
			(SELECT max(tg.tgname COLLATE "C") FROM pg_trigger tg JOIN pg_constraint c ON c.oid=tg.tgconstraint
				WHERE tg.tgrelid='public.families'::regclass AND c.conname='import_sessions_family_id_fkey')`).Scan(&careLast, &sessionsLast); err != nil {
		t.Fatal(err)
	}
	if (careLast < sessionsLast) != careFirst {
		t.Fatalf("care cascade %s, import cascade %s: want care first %t", careLast, sessionsLast, careFirst)
	}
}

// deleteFamily runs a raw DELETE FROM families as the table owner, after the
// family's rows that reference it without a cascade (slots, invitations,
// entitlements and the like, none of them care or import rows) are gone. It
// commits only when commit is set.
func deleteFamily(t *testing.T, f *testpg.Fixture, family string, commit bool) error {
	t.Helper()
	ctx := t.Context()
	tx, err := f.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT set_config('app.authority','1',true), set_config('app.family_id',$1,true)`, family); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `SELECT c.conrelid::regclass::text, a.attname FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		WHERE c.contype = 'f' AND c.confrelid = 'public.families'::regclass AND c.confdeltype <> 'c' AND c.conparentid = 0 AND cardinality(c.conkey) = 1`)
	if err != nil {
		t.Fatal(err)
	}
	references, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var reference [2]string
		return reference, row.Scan(&reference[0], &reference[1])
	})
	if err != nil || len(references) == 0 {
		t.Fatalf("references to families: %v %v", references, err)
	}
	for _, reference := range references {
		if strings.Contains(reference[0], "care_") || strings.Contains(reference[0], "import_") {
			t.Fatalf("%s references families without a cascade", reference[0])
		}
		if _, err := tx.Exec(ctx, `DELETE FROM `+reference[0]+` WHERE `+reference[1]+`=$1`, family); err != nil {
			t.Fatalf("clear %s: %v", reference[0], err)
		}
	}
	tag, err := tx.Exec(ctx, `DELETE FROM families WHERE id=$1`, family)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("deleted %d families", tag.RowsAffected())
	}
	if commit {
		return tx.Commit(ctx)
	}
	return nil
}

// A raw DELETE FROM families, as the table owner, takes the family's import
// staging, mappings and care rows whichever of its cascades fires first. At
// schema 36 it depended on the import_sessions cascade removing the mapping
// before the care_entities cascade's NO ACTION check from
// import_mappings_shared_baby_fk ran (Phase 4 Task 26 M4); 000037 makes that
// key cascade like the family's other import rows.
func TestRawFamilyDeleteCascadesThroughImportMappings(t *testing.T) {
	f := testpg.NewThrough(t, 36)
	ids := seedImportedFamilies(t, f)
	su := testpg.Superuser(t, f)
	ctx := t.Context()
	familyB := familyRows(t, su, ids.familyB)
	for table, n := range familyRows(t, su, ids.familyA) {
		if n == 0 {
			t.Fatalf("family A seeded no %s row", table)
		}
	}

	// Schema 36: the delete works only when the import cascade fires first.
	cascadeOrder(t, f, true)
	var pgErr *pgconn.PgError
	if err := deleteFamily(t, f, ids.familyA, false); !errors.As(err, &pgErr) || pgErr.Code != "23503" || pgErr.TableName != "import_baby_mappings" {
		t.Fatalf("schema 36, care cascade first: %v, want 23503 from import_baby_mappings", err)
	}
	cascadeOrder(t, f, false)
	if err := deleteFamily(t, f, ids.familyA, false); err != nil {
		t.Fatalf("schema 36, import cascade first: %v", err)
	}

	if err := f.MigrateThrough(ctx, 37); err != nil {
		t.Fatalf("000037: %v", err)
	}
	for _, careFirst := range []bool{false, true, false} {
		cascadeOrder(t, f, careFirst)
		if err := deleteFamily(t, f, ids.familyA, false); err != nil {
			t.Fatalf("schema 37, care cascade first %t: %v", careFirst, err)
		}
	}
	cascadeOrder(t, f, true)
	if err := deleteFamily(t, f, ids.familyA, true); err != nil {
		t.Fatalf("schema 37, committed delete: %v", err)
	}
	for table, n := range familyRows(t, su, ids.familyA) {
		if n != 0 {
			t.Errorf("%s keeps %d rows of the deleted family", table, n)
		}
	}
	for table, n := range familyRows(t, su, ids.familyB) {
		if n != familyB[table] {
			t.Errorf("family B's %s rows %d -> %d", table, familyB[table], n)
		}
	}
}

// A schema-36 database with rows in every family-owned table it seeds
// upgrades in place: 000037 changes one foreign key's delete action and
// moves no row (its other items replace function bodies and add one
// function). Every table keeps its row count, the key and each partition's
// copy of it are the same key plus ON DELETE CASCADE, the privilege manifest
// at 37 is 36's plus the work sweep's page function, and both readiness
// audits pass at 37.
func TestMigration37KeepsEverySeededRow(t *testing.T) {
	f := testpg.NewThrough(t, 36)
	seedImportedFamilies(t, f)
	su := testpg.Superuser(t, f)
	ctx := t.Context()
	counts := func() map[string]int64 {
		t.Helper()
		rows, err := su.Query(ctx, `SELECT format('%I.%I', n.nspname, c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
			WHERE n.nspname IN ('public','app') AND c.relkind IN ('r','p') AND NOT c.relispartition ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int64{}
		for _, table := range tables {
			var n int64
			if err := su.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			got[table] = n
		}
		return got
	}
	definitions := func() []string {
		t.Helper()
		// The key and its copies, one per care_entities partition (named
		// by PostgreSQL, so found through conparentid).
		rows, err := su.Query(ctx, `WITH RECURSIVE k AS (
				SELECT oid FROM pg_constraint WHERE conname='import_mappings_shared_baby_fk' AND conparentid=0
				UNION ALL SELECT c.oid FROM pg_constraint c JOIN k ON c.conparentid=k.oid)
			SELECT c.conrelid::regclass::text || ' ' || c.confrelid::regclass::text || ' ' || c.convalidated::text || ' ' || pg_get_constraintdef(c.oid)
			FROM k JOIN pg_constraint c ON c.oid=k.oid ORDER BY c.confrelid::regclass::text`)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	before, keysBefore := counts(), definitions()
	for _, table := range append([]string{"public.care_entities", "public.families"}, familyImportTables...) {
		if !strings.HasPrefix(table, "public.") {
			table = "public." + table
		}
		if before[table] == 0 {
			t.Fatalf("%s seeded no row", table)
		}
	}
	if len(keysBefore) != 17 {
		t.Fatalf("import_mappings_shared_baby_fk at 36: %d constraints, want the parent's and 16 partitions'", len(keysBefore))
	}

	if err := f.MigrateThrough(ctx, 37); err != nil {
		t.Fatalf("000037 over a schema-36 database with rows: %v", err)
	}
	after := counts()
	if len(after) != len(before) {
		t.Fatalf("tables %d -> %d", len(before), len(after))
	}
	for table, n := range before {
		if table == "public.schema_migrations" || table == "public.schema_privilege_manifest" {
			continue
		}
		if after[table] != n {
			t.Errorf("%s rows %d -> %d", table, n, after[table])
		}
	}
	if after["public.schema_migrations"] != before["public.schema_migrations"]+1 {
		t.Errorf("schema_migrations rows %d -> %d", before["public.schema_migrations"], after["public.schema_migrations"])
	}
	keysAfter := definitions()
	if len(keysAfter) != len(keysBefore) {
		t.Fatalf("import_mappings_shared_baby_fk constraints %d -> %d", len(keysBefore), len(keysAfter))
	}
	for i := range keysBefore {
		if keysAfter[i] != keysBefore[i]+" ON DELETE CASCADE" || !strings.Contains(keysAfter[i], " true FOREIGN KEY ") {
			t.Errorf("key %q -> %q, want the same validated key plus ON DELETE CASCADE", keysBefore[i], keysAfter[i])
		}
	}

	var version int64
	var introducedBy string
	if err := f.Owner.QueryRow(ctx, `SELECT current_version, introduced_by FROM schema_compatibility`).Scan(&version, &introducedBy); err != nil {
		t.Fatal(err)
	}
	if version != 37 || introducedBy != "000037_family_delete_cascade.sql" {
		t.Fatalf("compatibility %d %q", version, introducedBy)
	}
	var at36, missing int
	if err := f.Owner.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM schema_privilege_manifest WHERE schema_version=36),
			(SELECT count(*) FROM (SELECT principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=36
			  EXCEPT SELECT principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=37) m)`).
		Scan(&at36, &missing); err != nil || at36 == 0 || missing != 0 {
		t.Fatalf("privilege manifest at 37 lacks %d of 36's %d rows (%v)", missing, at36, err)
	}
	// Beside 36's rows, 37's has the work sweep's page function, for the
	// maintenance group alone, and the pull's high water, for the runtime
	// alone.
	rows, err := f.Owner.Query(ctx, `SELECT concat_ws('|',principal,object_kind,object_identity,privileges) FROM (
			SELECT principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=37
			EXCEPT SELECT principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=36) e ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	extra, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(extra) != 2 || extra[0] != "baby_tracker_maintenance|function|app.sweep_live_activity_work_page(uuid,integer)|EXECUTE" ||
		extra[1] != "baby_tracker_runtime|function|app.family_high_water(uuid)|EXECUTE" {
		t.Fatalf("privilege manifest at 37 adds %v to 36's (%v)", extra, err)
	}
	if err := database.VerifyRuntimeRole(ctx, f.Runtime, 37); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyMaintenanceRole(ctx, f.Runtime, f.Maintenance, 37); err != nil {
		t.Fatal(err)
	}
}
