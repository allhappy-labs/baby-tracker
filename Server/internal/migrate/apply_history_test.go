package migrate_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"babytracker/server/internal/migrate"
	"babytracker/server/internal/testpg"
	"babytracker/server/migrations"
)

// overTheEmbedded is every embedded migration plus a synthetic 000038, as the
// next migration will sit over them.
func overTheEmbedded(t *testing.T) fstest.MapFS {
	t.Helper()
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	source := fstest.MapFS{"000038_next.sql": &fstest.MapFile{Data: []byte("CREATE TABLE public.apply_next (id bigint PRIMARY KEY)")}}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		data, err := fs.ReadFile(migrations.Files, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		source[entry.Name()] = &fstest.MapFile{Data: data}
	}
	return source
}

func TestApplyAppliesOnlyTheMissingMigrations(t *testing.T) {
	f := testpg.New(t) // every embedded migration
	var before string
	if err := f.Owner.QueryRow(t.Context(), `SELECT string_agg(version || ':' || applied_at::text, ',' ORDER BY version) FROM schema_migrations`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	source := overTheEmbedded(t)
	if err := migrate.New(source).Apply(t.Context(), f.Owner); err != nil {
		t.Fatal(err)
	}
	var kept string
	var top int64
	var created bool
	if err := f.Owner.QueryRow(t.Context(), `SELECT string_agg(version || ':' || applied_at::text, ',' ORDER BY version) FILTER (WHERE version < 38), max(version),
		to_regclass('public.apply_next') IS NOT NULL FROM schema_migrations`).Scan(&kept, &top, &created); err != nil {
		t.Fatal(err)
	}
	if kept != before || top != 38 || !created {
		t.Fatalf("Apply rewrote the applied history or missed the top: %q -> %q, top %d, created %t", before, kept, top, created)
	}
	// A second run has nothing left to do.
	if err := migrate.New(source).Apply(t.Context(), f.Owner); err != nil {
		t.Fatalf("a second Apply: %v", err)
	}
}

func TestApplyRefusesANewerDatabaseBeforeRunningAnything(t *testing.T) {
	// The database is current but records one more, unknown, version
	// past 000038: nothing may run, not even the missing 000038.
	f := testpg.New(t)
	if _, err := f.Owner.Exec(t.Context(), `INSERT INTO schema_migrations (version, name, sha256) VALUES (39, '000039_future.sql', repeat('0',64))`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.New(overTheEmbedded(t)).Apply(t.Context(), f.Owner); !errors.Is(err, migrate.ErrDatabaseNewer) {
		t.Fatalf("an unknown version: %v", err)
	}
	var ran bool
	if err := f.Owner.QueryRow(t.Context(), `SELECT EXISTS (SELECT FROM schema_migrations WHERE version = 38) OR to_regclass('public.apply_next') IS NOT NULL`).Scan(&ran); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("a refused Apply ran migration 38")
	}
}

func TestApplyRefusesAChangedHistoryBeforeRunningAnything(t *testing.T) {
	f := testpg.New(t)
	if _, err := f.Owner.Exec(t.Context(), `UPDATE schema_migrations SET sha256=repeat('0',64) WHERE version=(SELECT max(version) FROM schema_migrations)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.New(overTheEmbedded(t)).Apply(t.Context(), f.Owner); !errors.Is(err, migrate.ErrChecksumMismatch) {
		t.Fatalf("a changed top row: %v", err)
	}
	var ran bool
	if err := f.Owner.QueryRow(t.Context(), `SELECT EXISTS (SELECT FROM schema_migrations WHERE version = 38) OR to_regclass('public.apply_next') IS NOT NULL`).Scan(&ran); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("a refused Apply ran migration 38")
	}
}
