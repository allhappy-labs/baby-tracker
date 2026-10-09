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
	LiveActivity *pgxpool.Pool
	Admin        *pgxpool.Pool
	Owner        *pgxpool.Pool
	Runtime      *pgxpool.Pool
	Projector    *pgxpool.Pool
	Revoker      *pgxpool.Pool
	Maintenance  *pgxpool.Pool
}

func RequireDatabase(t *testing.T) string {
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

// NewThrough returns a disposable database migrated only up to version, so a
// test can seed an older schema and then apply the next migration itself with
// MigrateThrough, as the same owner role production's migrate command uses.
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

func (filtered throughFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(filtered.source, name)
	if err != nil {
		return nil, err
	}
	kept := make([]fs.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if filtered.includes(path.Join(name, entry.Name())) {
			kept = append(kept, entry)
		}
	}
	return kept, nil
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
	dsn := RequireDatabase(t)
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
	ownerRole, runtimeRole, projectorRole, revokerRole, maintenanceRole := "security_owner_"+suffix, "security_runtime_"+suffix, "security_projector_"+suffix, "security_revoker_"+suffix, "security_maintenance_"+suffix
	liveActivityRole := "security_live_activity_" + suffix
	for _, role := range []string{ownerRole, runtimeRole, projectorRole, revokerRole, maintenanceRole, liveActivityRole} {
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
	if _, err := admin.Exec(ctx, "GRANT baby_tracker_projector TO "+pgx.Identifier{projectorRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "GRANT baby_tracker_apple_revoker TO "+pgx.Identifier{revokerRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "GRANT baby_tracker_maintenance TO "+pgx.Identifier{maintenanceRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+" OWNER "+pgx.Identifier{ownerRole}.Sanitize()); err != nil {
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
	if _, err := admin.Exec(ctx, "GRANT baby_tracker_live_activity TO "+pgx.Identifier{liveActivityRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	fixture.Owner = open(ownerRole, 2)
	if err := migrate.New(source).Apply(ctx, fixture.Owner); err != nil {
		t.Fatal(err)
	}
	// Identity persistence tests deliberately race 100 callers. A bounded pool
	// still exercises real concurrency without exhausting PostgreSQL's default
	// connection budget when owner/projector/admin pools are also open.
	fixture.Runtime = open(runtimeRole, 32)
	fixture.Projector = open(projectorRole, 1)
	fixture.Revoker = open(revokerRole, 2)
	fixture.Maintenance = open(maintenanceRole, 4)
	fixture.LiveActivity = open(liveActivityRole, 2)
	return fixture
}
