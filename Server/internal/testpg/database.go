package testpg

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/migrate"
	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Fixture struct {
	Admin       *pgxpool.Pool
	Owner       *pgxpool.Pool
	Runtime     *pgxpool.Pool
	Maintenance *pgxpool.Pool
}

func RequireDatabase(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("BABY_TRACKER_TEST_DATABASE_URL")
	if dsn == "" && os.Getenv("BABY_TRACKER_REQUIRE_DATABASE") == "1" {
		t.Fatal("disposable PostgreSQL is required")
	}
	if dsn == "" {
		t.Skip("BABY_TRACKER_TEST_DATABASE_URL is not set")
	}
	return dsn
}

// New returns a disposable database migrated to the newest schema.
func New(t *testing.T) *Fixture {
	t.Helper()
	return newFixture(t, migrations.Files)
}

// NewWithICUCollation returns a disposable database migrated to the newest
// schema whose default collation is the ICU locale (for example the numeric
// "und-u-kn-true"), so a test can prove a statement orders by the uuid and
// not by its text under a collation the server does not control: a
// self-hoster's database may sort text this way.
func NewWithICUCollation(t *testing.T, locale string) *Fixture {
	t.Helper()
	return newFixtureCreating(t, migrations.Files, " TEMPLATE template0 LOCALE_PROVIDER icu ICU_LOCALE "+quoteLiteral(locale))
}

func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// NewThrough returns a disposable database migrated only up to version, so a
// test can seed an older schema and then apply the next migration itself with
// MigrateThrough, as the same owner role production's migrate command uses.
// Version 0 is an empty database: nothing is applied and schema_migrations
// does not exist. Versions 1–31 were squashed into 000032_baseline.sql
// (Phase 4 Task 26): asking for one fails the test.
func NewThrough(t *testing.T, version int64) *Fixture {
	t.Helper()
	return newFixture(t, Through(migrations.Files, version))
}

// MigrateThrough applies the migrations numbered up to version as the
// table-owning, non-BYPASSRLS owner role. It returns the migration error so a
// test can assert that a migration refuses to run.
func (fixture *Fixture) MigrateThrough(ctx context.Context, version int64) error {
	return migrate.New(Through(migrations.Files, version)).Apply(ctx, fixture.Owner)
}

// Through exposes only the migration files numbered at or below version.
func Through(source fs.FS, version int64) fs.FS {
	return throughFS{source: source, version: version}
}

type throughFS struct {
	source  fs.FS
	version int64
}

func (filtered throughFS) Open(name string) (fs.File, error) {
	if !filtered.includes(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return filtered.source.Open(name)
}

// ReadDir lists the files at or below the version. A version between 0 and a
// squashed baseline in the source (NNNNNN_baseline.sql) names a schema no
// file builds any more: the listing fails, so NewThrough fails the test and
// MigrateThrough returns the error.
func (filtered throughFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(filtered.source, name)
	if err != nil {
		return nil, err
	}
	kept := make([]fs.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if version, ok := baselineVersion(entry.Name()); ok && filtered.version > 0 && filtered.version < version {
			return nil, fmt.Errorf("testpg: schema %d is gone: pre-baseline versions were squashed into %s", filtered.version, entry.Name())
		}
		if filtered.includes(path.Join(name, entry.Name())) {
			kept = append(kept, entry)
		}
	}
	return kept, nil
}

// hasMigrations reports whether source lists any .sql file. Unlike fs.Glob,
// it returns the listing's error.
func hasMigrations(source fs.FS) (bool, error) {
	entries, err := fs.ReadDir(source, ".")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			return true, nil
		}
	}
	return false, nil
}

// baselineVersion is the version of a squashed baseline file name.
func baselineVersion(base string) (int64, bool) {
	if len(base) != len("000000_baseline.sql") || base[6:] != "_baseline.sql" {
		return 0, false
	}
	version, err := strconv.ParseInt(base[:6], 10, 64)
	return version, err == nil
}

func (filtered throughFS) includes(name string) bool {
	base := path.Base(name)
	if !strings.HasSuffix(base, ".sql") {
		return true
	}
	if len(base) < 7 || base[6] != '_' {
		return false
	}
	version, err := strconv.ParseInt(base[:6], 10, 64)
	return err == nil && version <= filtered.version
}

func newFixture(t *testing.T, source fs.FS) *Fixture {
	t.Helper()
	return newFixtureCreating(t, source, "")
}

// newFixtureCreating is newFixture with createOptions appended to the
// CREATE DATABASE statement.
func newFixtureCreating(t *testing.T, source fs.FS, createOptions string) *Fixture {
	t.Helper()
	dsn := RequireDatabase(t)
	// A source without migrations (NewThrough(t, 0)) leaves the database
	// empty; the runner itself refuses an empty source. A source that cannot
	// be listed (a squashed version, Through) fails the test here.
	pending, err := hasMigrations(source)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &Fixture{Admin: admin}
	t.Cleanup(admin.Close)
	bootstrapPath := filepath.Join("..", "..", "deploy", "postgres", "init-roles.sql")
	bootstrap, err := os.ReadFile(bootstrapPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatal(err)
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	database := "security_test_" + suffix
	ownerRole, runtimeRole, maintenanceRole := "security_owner_"+suffix, "security_runtime_"+suffix, "security_maintenance_"+suffix
	for _, role := range []string{ownerRole, runtimeRole, maintenanceRole} {
		if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN PASSWORD 'disposable-test-only' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			t.Fatal(err)
		}
		role := role
		t.Cleanup(func() {
			if _, err := admin.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Error(err)
			}
		})
	}
	if _, err := admin.Exec(ctx, "GRANT baby_tracker_runtime TO "+pgx.Identifier{runtimeRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "GRANT "+pgx.Identifier{runtimeRole}.Sanitize()+" TO "+pgx.Identifier{ownerRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "GRANT baby_tracker_maintenance TO "+pgx.Identifier{maintenanceRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+" OWNER "+pgx.Identifier{ownerRole}.Sanitize()+createOptions); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})

	open := func(role string, max int32) *pgxpool.Pool {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, "disposable-test-only")
		u.Path = "/" + database
		config, err := pgxpool.ParseConfig(u.String())
		if err != nil {
			t.Fatal(err)
		}
		config.MaxConns = max
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	fixture.Owner = open(ownerRole, 2)
	if pending {
		if err := migrate.New(source).Apply(ctx, fixture.Owner); err != nil {
			t.Fatal(err)
		}
	}
	// Identity persistence tests deliberately race 100 callers. A bounded pool
	// still exercises real concurrency without exhausting PostgreSQL's default
	// connection budget when owner/admin pools are also open.
	fixture.Runtime = open(runtimeRole, 32)
	fixture.Maintenance = open(maintenanceRole, 4)
	return fixture
}
