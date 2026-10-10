package testkit

import (
	"fmt"
	"testing"

	"babytracker/server/internal/care"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is a disposable database's pools. The authority runs on Runtime;
// there is no projector pool.
type Postgres struct{ Admin, Runtime, Migrator, Maintenance *pgxpool.Pool }

func NewPostgres(t *testing.T) Postgres {
	t.Helper()
	return postgresOf(testpg.New(t))
}

// NewWithICUCollation is New on a database whose default collation is the
// ICU locale (testpg.NewWithICUCollation).
func NewWithICUCollation(t *testing.T, locale string) *Harness {
	t.Helper()
	return newHarnessOn(t, postgresOf(testpg.NewWithICUCollation(t, locale)), "", false, databaseNoon)
}

func postgresOf(fixture *testpg.Fixture) Postgres {
	return Postgres{Admin: fixture.Admin, Runtime: fixture.Runtime, Migrator: fixture.Owner, Maintenance: fixture.Maintenance}
}

func count(t *testing.T, pool *pgxpool.Pool, table, familyID, accountID string) int {
	t.Helper()
	allowed := map[string]bool{"import_sessions": true, "import_chunks": true, "import_baby_candidates": true, "import_record_manifests": true, "import_conflicts": true, "import_conflict_receipts": true, "import_baby_mappings": true, "care_entities": true, "care_receipts": true, "sync_page_bindings": true, "sync_snapshot_sessions": true}
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
	if err = tx.QueryRow(t.Context(), `SELECT care_record_count,care_charged_bytes FROM families WHERE id=$1`, familyID).Scan(&count, &bytes); err != nil {
		t.Fatal(err)
	}
	return count, bytes
}

// CareCount is the care_entities rows of kind (a care kind name) in
// familyID, tombstones included, read as the owner.
func (h *Harness) CareCount(t *testing.T, kind, familyID string) int {
	t.Helper()
	code, ok := care.KindCode(kind)
	if !ok {
		t.Fatalf("care kind %q", kind)
	}
	tx, err := h.Migrator.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: familyID}); err != nil {
		t.Fatal(err)
	}
	var result int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM care_entities WHERE family_id=$1 AND kind=$2`, familyID, code).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

// SequenceOf is familyID's next_sequence: the number of changes the family
// has taken, read as accountID.
func (h *Harness) SequenceOf(t *testing.T, accountID, familyID string) uint64 {
	t.Helper()
	return familySequence(t, h.Migrator, accountID, familyID)
}
