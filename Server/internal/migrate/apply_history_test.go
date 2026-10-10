package migrate_test

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"babytracker/server/internal/migrate"
	"babytracker/server/internal/testpg"
	"babytracker/server/migrations"
)

// overTheBaseline is the embedded baseline plus a synthetic 000033, as the
// next migration will sit over it.
func overTheBaseline(t *testing.T) fstest.MapFS {
	t.Helper()
	baseline, err := fs.ReadFile(migrations.Files, "000032_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{
		"000032_baseline.sql": &fstest.MapFile{Data: baseline},
		"000033_next.sql":     &fstest.MapFile{Data: []byte("CREATE TABLE public.apply_next (id bigint PRIMARY KEY)")},
	}
}

func TestApplyAppliesOnlyTheMissingMigrations(t *testing.T) {
	f := testpg.New(t) // the baseline
	var before string
	if err := f.Owner.QueryRow(t.Context(), `SELECT string_agg(version || ':' || applied_at::text, ',' ORDER BY version) FROM schema_migrations`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	source := overTheBaseline(t)
	if err := migrate.New(source).Apply(t.Context(), f.Owner); err != nil {
		t.Fatal(err)
	}
	var kept string
	var top int64
	var created bool
	if err := f.Owner.QueryRow(t.Context(), `SELECT string_agg(version || ':' || applied_at::text, ',' ORDER BY version) FILTER (WHERE version < 33), max(version),
		to_regclass('public.apply_next') IS NOT NULL FROM schema_migrations`).Scan(&kept, &top, &created); err != nil {
		t.Fatal(err)
	}
	if kept != before || top != 33 || !created {
		t.Fatalf("Apply rewrote the applied history or missed the top: %q -> %q, top %d, created %t", before, kept, top, created)
	}
	// A second run has nothing left to do.
	if err := migrate.New(source).Apply(t.Context(), f.Owner); err != nil {
		t.Fatalf("a second Apply: %v", err)
	}
}

func TestApplyRefusesANewerDatabaseBeforeRunningAnything(t *testing.T) {
	// The database is at the baseline but records one more, unknown, version
	// past 000033: nothing may run, not even the missing 000033.
	f := testpg.New(t)
	if _, err := f.Owner.Exec(t.Context(), `INSERT INTO schema_migrations (version, name, sha256) VALUES (34, '000034_future.sql', repeat('0',64))`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.New(overTheBaseline(t)).Apply(t.Context(), f.Owner); !errors.Is(err, migrate.ErrDatabaseNewer) {
		t.Fatalf("an unknown version: %v", err)
	}
	var ran bool
	if err := f.Owner.QueryRow(t.Context(), `SELECT EXISTS (SELECT FROM schema_migrations WHERE version = 33) OR to_regclass('public.apply_next') IS NOT NULL`).Scan(&ran); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("a refused Apply ran migration 33")
	}
}

func TestApplyRefusesAChangedHistoryBeforeRunningAnything(t *testing.T) {
	f := testpg.New(t)
	if _, err := f.Owner.Exec(t.Context(), `UPDATE schema_migrations SET sha256=repeat('0',64) WHERE version=(SELECT max(version) FROM schema_migrations)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.New(overTheBaseline(t)).Apply(t.Context(), f.Owner); !errors.Is(err, migrate.ErrChecksumMismatch) {
		t.Fatalf("a changed top row: %v", err)
	}
	var ran bool
	if err := f.Owner.QueryRow(t.Context(), `SELECT EXISTS (SELECT FROM schema_migrations WHERE version = 33) OR to_regclass('public.apply_next') IS NOT NULL`).Scan(&ran); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("a refused Apply ran migration 33")
	}
}
