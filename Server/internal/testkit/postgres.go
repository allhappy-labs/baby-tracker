package testkit

import (
	"fmt"
	"testing"

	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Postgres struct{ Admin, Runtime, Migrator, Projector, Maintenance, LiveActivity *pgxpool.Pool }

func NewPostgres(t *testing.T) Postgres {
	t.Helper()
	return postgresOf(testpg.New(t))
}

// NewPostgresThrough is NewPostgres on a database migrated only up to version.
func NewPostgresThrough(t *testing.T, version int64) Postgres {
	t.Helper()
	return postgresOf(testpg.NewThrough(t, version))
}

func postgresOf(fixture *testpg.Fixture) Postgres {
	return Postgres{Admin: fixture.Admin, Runtime: fixture.Runtime, Migrator: fixture.Owner, Projector: fixture.Projector, Maintenance: fixture.Maintenance, LiveActivity: fixture.LiveActivity}
}

func count(t *testing.T, pool *pgxpool.Pool, table, familyID, accountID string) int {
	t.Helper()
	allowed := map[string]bool{"babies": true, "feed_entries": true, "feed_components": true, "sleep_entries": true, "diaper_entries": true, "supplement_entries": true, "growth_profiles": true, "measurement_entries": true, "active_timers": true, "timer_components": true, "mutation_journal": true, "mutation_receipts": true, "family_record_usage": true, "sync_snapshots": true, "sync_snapshot_admissions": true, "sync_snapshot_entities": true, "sync_page_sessions": true, "import_sessions": true, "import_chunks": true, "import_baby_candidates": true, "import_record_manifests": true, "import_conflicts": true, "import_conflict_receipts": true, "import_baby_mappings": true}
	if !allowed[table] {
		t.Fatalf("unsupported assertion table %q", table)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	var result int
	if err := tx.QueryRow(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s WHERE family_id=$1", table), familyID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func execFamily(t *testing.T, pool *pgxpool.Pool, accountID, familyID, query string, args ...any) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func familySequence(t *testing.T, pool *pgxpool.Pool, accountID, familyID string) uint64 {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	var value uint64
	if err = tx.QueryRow(t.Context(), `SELECT next_sequence FROM families WHERE id=$1`, familyID).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func familyUsage(t *testing.T, pool *pgxpool.Pool, accountID, familyID string) (int64, int64) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: accountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	var count, bytes int64
	if err = tx.QueryRow(t.Context(), `SELECT canonical_record_count,charged_storage_bytes FROM family_record_usage WHERE family_id=$1`, familyID).Scan(&count, &bytes); err != nil {
		t.Fatal(err)
	}
	return count, bytes
}
