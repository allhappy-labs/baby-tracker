package database_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"babytracker/server/internal/testpg"
)

// A schema-33 database with care rows of every kind upgrades in place. The
// old rows read back unchanged, and kind 10 (pump) is then accepted with no
// baby, with an end at most six hours after its start, and at most one still
// running per family, by the parent and every partition.
func TestPumpingMigrationOverASchema33DatabaseWithRows(t *testing.T) {
	f := testpg.NewThrough(t, 33)
	ids := seedAuthorityFamilies(t, f)
	ctx := t.Context()
	const babyA, babyB = "71000000-0000-4000-8000-00000000000a", "71000000-0000-4000-8000-00000000000b"
	insert := `INSERT INTO care_entities(family_id,id,kind,baby_id,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,
		started_at,ended_at,sort_order,label,name_hash,feed_kind,timer_kind,wire)
		VALUES ($1,$2,$3,$4,1,$5,$6,$6,$7,'create',now(),$8,$9,$10,$11,$12,$13,$14,convert_to(repeat('x',80),'UTF8'))`
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
		// One row of every kind schema 33 has.
		{babyA, 1, nil, nil, int32(0), nil, hash, nil, nil},
		{"72000000-0000-4000-8000-000000000002", 2, babyA, started, nil, nil, nil, int16(2), nil},
		{"72000000-0000-4000-8000-000000000003", 3, babyA, started, nil, nil, nil, nil, nil},
		{"72000000-0000-4000-8000-000000000004", 4, babyA, started, nil, nil, nil, nil, nil},
		{"72000000-0000-4000-8000-000000000005", 5, babyA, started, nil, nil, nil, nil, int16(2)},
		{"72000000-0000-4000-8000-000000000006", 6, babyA, started, nil, "Vitamin D", nil, nil, nil},
		{"72000000-0000-4000-8000-000000000007", 7, babyA, nil, nil, nil, nil, nil, nil},
		{"72000000-0000-4000-8000-000000000008", 8, babyA, started, nil, nil, nil, nil, nil},
		{"72000000-0000-4000-8000-000000000009", 9, babyA, started, nil, nil, nil, nil, nil},
	} {
		if _, err := seed.Exec(ctx, insert, ids.familyA, row.id, row.kind, row.baby, int64(seq+1), ids.accountA, ids.deviceA, row.started, nil, row.sort, row.label, row.hash, row.feed, row.timer); err != nil {
			t.Fatalf("seed kind %d: %v", row.kind, err)
		}
	}
	// A pump row of family A: kind, baby, start, end.
	pump := func(id string, kind int16, baby, started, ended any, seq int64) []any {
		return []any{ids.familyA, id, kind, baby, seq, ids.accountA, ids.deviceA, started, ended, nil, nil, nil, nil, nil}
	}
	refused := func(tx pgx.Tx, what, state string, args []any) {
		t.Helper()
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sp.Exec(ctx, insert, args...)
		if got := sqlState(err); got != state {
			t.Fatalf("%s: want SQLSTATE %s, got %v", what, state, err)
		}
		if err := sp.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}
	refused(seed, "kind 10 at schema 33", "23514", pump("74000000-0000-4000-8000-000000000001", 10, nil, started, nil, 50))
	if err := seed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.MigrateThrough(ctx, 34); err != nil {
		t.Fatalf("000034 over a schema-33 database with rows: %v", err)
	}
	var version int64
	var introducedBy string
	if err := f.Owner.QueryRow(ctx, `SELECT current_version, introduced_by FROM schema_compatibility`).Scan(&version, &introducedBy); err != nil {
		t.Fatal(err)
	}
	if version != 34 || introducedBy != "000034_pumping.sql" {
		t.Fatalf("compatibility %d %q", version, introducedBy)
	}
	var at33, at34 int
	if err := f.Owner.QueryRow(ctx, `SELECT count(*) FILTER (WHERE schema_version=33), count(*) FILTER (WHERE schema_version=34) FROM schema_privilege_manifest`).Scan(&at33, &at34); err != nil {
		t.Fatal(err)
	}
	if at33 == 0 || at33 != at34 {
		t.Fatalf("privilege manifest rows: %d at 33, %d at 34", at33, at34)
	}

	// The parent and all 16 partitions carry the widened checks and the new one.
	for name, fragment := range map[string]string{
		"care_entities_kind_check": "kind <= 10", "care_entities_kind_shape": "ARRAY[1, 10]", "care_entities_pump_six_hours": "06:00:00",
	} {
		var all, matching int
		if err := f.Owner.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE position($2 in pg_get_constraintdef(oid)) > 0)
			FROM pg_constraint WHERE conname=$1`, name, fragment).Scan(&all, &matching); err != nil {
			t.Fatal(err)
		}
		if all != 17 || matching != 17 {
			t.Fatalf("%s: %d constraints, %d with %q; want 17 and 17", name, all, matching, fragment)
		}
	}
	var partitionIndexes int
	if err := f.Owner.QueryRow(ctx, `SELECT count(*) FROM pg_inherits WHERE inhparent='public.care_entities_one_open_pump'::regclass`).Scan(&partitionIndexes); err != nil || partitionIndexes != 16 {
		t.Fatalf("care_entities_one_open_pump on %d partitions (%v)", partitionIndexes, err)
	}
	var resume, manifest string
	if err := f.Owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='sync_page_bindings_resume_kind_check'`).Scan(&resume); err != nil || !strings.Contains(resume, "resume_kind <= 10") {
		t.Fatalf("resume kind check %q (%v)", resume, err)
	}
	if err := f.Owner.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='import_record_manifests_entity_kind_check'`).Scan(&manifest); err != nil || strings.Contains(manifest, "'pump'") {
		t.Fatalf("a pump is never an import record: %q (%v)", manifest, err)
	}

	after := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	var kinds string
	if err := after.QueryRow(ctx, `SELECT string_agg(kind::text, ',' ORDER BY kind) FROM care_entities`).Scan(&kinds); err != nil {
		t.Fatal(err)
	}
	if kinds != "1,2,3,4,5,6,7,8,9" {
		t.Fatalf("the seeded rows after the migration: %s", kinds)
	}
	sixHours := started.Add(6 * time.Hour)
	refused(after, "kind 11", "23514", pump("74000000-0000-4000-8000-000000000002", 11, nil, started, nil, 51))
	refused(after, "a pump with a baby", "23514", pump("74000000-0000-4000-8000-000000000003", 10, babyA, started, nil, 52))
	refused(after, "a pump without a start", "23514", pump("74000000-0000-4000-8000-000000000004", 10, nil, nil, nil, 53))
	refused(after, "a pump of six hours and a second", "23514", pump("74000000-0000-4000-8000-000000000005", 10, nil, started, sixHours.Add(time.Second), 54))
	refused(after, "a pump that ends before it starts", "23514", pump("74000000-0000-4000-8000-000000000006", 10, nil, started, started.Add(-time.Second), 55))
	if _, err := after.Exec(ctx, insert, pump("74000000-0000-4000-8000-000000000007", 10, nil, started, sixHours, 56)...); err != nil {
		t.Fatalf("a pump of exactly six hours: %v", err)
	}
	const running = "74000000-0000-4000-8000-000000000008"
	if _, err := after.Exec(ctx, insert, pump(running, 10, nil, started, nil, 57)...); err != nil {
		t.Fatalf("a running pump: %v", err)
	}
	refused(after, "a second running pump", "23505", pump("74000000-0000-4000-8000-000000000009", 10, nil, started, nil, 58))
	// Once the first has ended, another may run; a tombstone frees the slot too.
	if _, err := after.Exec(ctx, `UPDATE care_entities SET ended_at=$3, entity_version=2, changed_seq=59 WHERE family_id=$1 AND id=$2`, ids.familyA, running, started.Add(time.Hour)); err != nil {
		t.Fatalf("ending the running pump: %v", err)
	}
	const second = "74000000-0000-4000-8000-00000000000a"
	if _, err := after.Exec(ctx, insert, pump(second, 10, nil, started, nil, 60)...); err != nil {
		t.Fatalf("a running pump after the first ended: %v", err)
	}
	if _, err := after.Exec(ctx, `UPDATE care_entities SET tombstoned_at=now(), entity_version=2, changed_seq=61 WHERE family_id=$1 AND id=$2`, ids.familyA, second); err != nil {
		t.Fatalf("tombstoning the running pump: %v", err)
	}
	if _, err := after.Exec(ctx, insert, pump("74000000-0000-4000-8000-00000000000b", 10, nil, started, nil, 62)...); err != nil {
		t.Fatalf("a running pump after the last was deleted: %v", err)
	}
	if err := after.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Another family's running pump is its own: the rule is per family.
	other := tenantTx(t, f.Runtime, ids.accountB, ids.familyB)
	if _, err := other.Exec(ctx, insert, ids.familyB, babyB, int16(1), nil, int64(1), ids.accountB, ids.deviceA, nil, nil, int32(0), nil, hash, nil, nil); err != nil {
		t.Fatalf("family B baby: %v", err)
	}
	if _, err := other.Exec(ctx, insert, ids.familyB, "74000000-0000-4000-8000-00000000000c", int16(10), nil, int64(2), ids.accountB, ids.deviceA, started, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("family B running pump: %v", err)
	}
}
