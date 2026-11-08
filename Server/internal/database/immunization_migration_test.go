package database_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"babytracker/server/internal/testpg"
)

// Review Focus 4: a schema-32 database with care rows upgrades in place.
// The old rows read back unchanged, and kind 9 (immunization) is then
// accepted with a started_at and no label, by the parent and every partition.
func TestImmunizationMigrationOverABaselineWithRows(t *testing.T) {
	f := testpg.NewThrough(t, 32)
	ids := seedAuthorityFamilies(t, f)
	ctx := t.Context()
	const babyA, babyB = "71000000-0000-4000-8000-00000000000a", "71000000-0000-4000-8000-00000000000b"
	insert := `INSERT INTO care_entities(family_id,id,kind,baby_id,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,
		started_at,sort_order,label,name_hash,feed_kind,timer_kind,wire)
		VALUES ($1,$2,$3,$4,1,$5,$6,$6,$7,'create',now(),$8,$9,$10,$11,$12,$13,convert_to(repeat('x',80),'UTF8'))`
	hash := make([]byte, 32)
	started := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

	seed := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	for seq, row := range []struct {
		id      string
		kind    int16
		baby    any
		started any
		sort    any
		label   any
		hash    any
		feed    any
		timer   any
	}{
		// One row of every kind the baseline has.
		{babyA, 1, nil, nil, int32(0), nil, hash, nil, nil},
		{"72000000-0000-4000-8000-000000000002", 2, babyA, started, nil, nil, nil, int16(2), nil},
		{"72000000-0000-4000-8000-000000000003", 3, babyA, started, nil, nil, nil, nil, nil},
		{"72000000-0000-4000-8000-000000000004", 4, babyA, started, nil, nil, nil, nil, nil},
		{"72000000-0000-4000-8000-000000000005", 5, babyA, started, nil, nil, nil, nil, int16(2)},
		{"72000000-0000-4000-8000-000000000006", 6, babyA, started, nil, "Vitamin D", nil, nil, nil},
		{"72000000-0000-4000-8000-000000000007", 7, babyA, nil, nil, nil, nil, nil, nil},
		{"72000000-0000-4000-8000-000000000008", 8, babyA, started, nil, nil, nil, nil, nil},
	} {
		if _, err := seed.Exec(ctx, insert, ids.familyA, row.id, row.kind, row.baby, int64(seq+1), ids.accountA, ids.deviceA, row.started, row.sort, row.label, row.hash, row.feed, row.timer); err != nil {
			t.Fatalf("seed kind %d: %v", row.kind, err)
		}
	}
	shot := func(id string, kind int16, started, label any, seq int64) []any {
		return []any{ids.familyA, id, kind, babyA, seq, ids.accountA, ids.deviceA, started, nil, label, nil, nil, nil}
	}
	refused := func(tx pgx.Tx, what string, args []any) {
		t.Helper()
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sp.Exec(ctx, insert, args...)
		if state := sqlState(err); state != "23514" {
			t.Fatalf("%s: want a check violation, got %v", what, err)
		}
		if err := sp.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}
	refused(seed, "kind 9 at schema 32", shot("73000000-0000-4000-8000-000000000001", 9, started, nil, 50))
	if err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.MigrateThrough(ctx, 33); err != nil {
		t.Fatalf("000033 over a baseline with rows: %v", err)
	}
	var version int64
	var introducedBy string
	if err := f.Owner.QueryRow(ctx, `SELECT current_version, introduced_by FROM schema_compatibility`).Scan(&version, &introducedBy); err != nil {
		t.Fatal(err)
	}
	if version != 33 || introducedBy != "000033_immunizations.sql" {
		t.Fatalf("compatibility %d %q", version, introducedBy)
	}
	var at32, at33 int
	if err := f.Owner.QueryRow(ctx, `SELECT count(*) FILTER (WHERE schema_version=32), count(*) FILTER (WHERE schema_version=33) FROM schema_privilege_manifest`).Scan(&at32, &at33); err != nil {
		t.Fatal(err)
	}
	if at32 == 0 || at32 != at33 {
		t.Fatalf("privilege manifest rows: %d at 32, %d at 33", at32, at33)
	}

	// The parent and all 16 partitions carry the widened checks.
	for name, fragment := range map[string]string{
		"care_entities_kind_check": "kind <= 9", "care_entities_kind_shape": "ARRAY[2, 3, 4, 5, 6, 8, 9]",
	} {
		var all, widened int
		if err := f.Owner.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE position($2 in pg_get_constraintdef(oid)) > 0)
			FROM pg_constraint WHERE conname=$1`, name, fragment).Scan(&all, &widened); err != nil {
			t.Fatal(err)
		}
		if all != 17 || widened != 17 {
			t.Fatalf("%s: %d constraints, %d widened; want 17 and 17", name, all, widened)
		}
	}
	for name, fragment := range map[string]string{
		"sync_page_bindings_resume_kind_check": "resume_kind <= 9", "import_record_manifests_entity_kind_check": "'immunization'::text",
	} {
		var def string
		if err := f.Owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname=$1`, name).Scan(&def); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(def, fragment) || strings.Contains(def, "'pump'") {
			t.Fatalf("%s: %s", name, def)
		}
	}

	after := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	var kinds string
	if err := after.QueryRow(ctx, `SELECT string_agg(kind::text, ',' ORDER BY kind) FROM care_entities`).Scan(&kinds); err != nil {
		t.Fatal(err)
	}
	if kinds != "1,2,3,4,5,6,7,8" {
		t.Fatalf("the seeded rows after the migration: %s", kinds)
	}
	refused(after, "kind 10", shot("73000000-0000-4000-8000-000000000002", 10, started, nil, 51))
	refused(after, "kind 9 without started_at", shot("73000000-0000-4000-8000-000000000003", 9, nil, nil, 52))
	refused(after, "kind 9 with a label", shot("73000000-0000-4000-8000-000000000004", 9, started, "Prevenar 13", 53))
	if _, err := after.Exec(ctx, insert, shot("73000000-0000-4000-8000-000000000005", 9, started, nil, 54)...); err != nil {
		t.Fatalf("kind 9 after the migration: %v", err)
	}

	// The other family's rows live in another partition's table or the same; either accepts it.
	other := tenantTx(t, f.Runtime, ids.accountB, ids.familyB)
	if _, err := other.Exec(ctx, insert, ids.familyB, babyB, int16(1), nil, int64(1), ids.accountB, ids.deviceA, nil, int32(0), nil, hash, nil, nil); err != nil {
		t.Fatalf("family B baby: %v", err)
	}
	if _, err := other.Exec(ctx, insert, ids.familyB, "73000000-0000-4000-8000-000000000006", int16(9), babyB, int64(2), ids.accountB, ids.deviceA, started, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("family B kind 9: %v", err)
	}
}
