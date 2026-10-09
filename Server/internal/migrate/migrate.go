package migrate

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const advisoryLockID int64 = 410428716390123

var (
	ErrChecksumMismatch = errors.New("migration_checksum_mismatch")
	ErrDatabaseNewer    = errors.New("database_newer_than_binary")
)

type Runner struct {
	source fs.FS
}

type migration struct {
	version  int64
	name     string
	checksum string
	sql      string
}

func New(source fs.FS) *Runner {
	return &Runner{source: source}
}

func (r *Runner) Apply(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := r.loadMigrations()
	if err != nil {
		return err
	}
	conn, err := acquireLocked(ctx, pool)
	if err != nil {
		return err
	}
	defer releaseLocked(conn)
	if err := ensureTable(ctx, conn); err != nil {
		return err
	}
	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return err
	}
	if err := validateApplied(migrations, applied); err != nil {
		return err
	}
	for _, migration := range migrations {
		if _, ok := applied[migration.version]; ok {
			continue
		}
		if err := applyOne(ctx, conn, migration); err != nil {
			return err
		}
	}
	return nil
}

func acquireLocked(ctx context.Context, pool *pgxpool.Pool) (*pgxpool.Conn, error) {
	if pool == nil {
		return nil, errors.New("database pool is required")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire migration connection: %w", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", advisoryLockID); err != nil {
		conn.Release()
		return nil, fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	return conn, nil
}

func releaseLocked(conn *pgxpool.Conn) {
	unlockContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.Exec(unlockContext, "SELECT pg_advisory_unlock($1)", advisoryLockID); err != nil {
		conn.Conn().Close(unlockContext)
	}
	conn.Release()
}

func ensureTable(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version bigint PRIMARY KEY,
		name text NOT NULL,
		sha256 text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`)
	if err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	return nil
}

func applyOne(ctx context.Context, conn *pgxpool.Conn, migration migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", migration.version, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, migration.sql); err != nil {
		return fmt.Errorf("execute migration %d: %w", migration.version, err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name, sha256) VALUES ($1, $2, $3)", migration.version, migration.name, migration.checksum); err != nil {
		return fmt.Errorf("record migration %d: %w", migration.version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %d: %w", migration.version, err)
	}
	return nil
}

func (r *Runner) loadMigrations() ([]migration, error) {
	if r == nil || r.source == nil {
		return nil, errors.New("migration source is required")
	}
	entries, err := fs.ReadDir(r.source, ".")
	if err != nil {
		return nil, fmt.Errorf("read migration source: %w", err)
	}
	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(entry.Name())
		if err != nil {
			return nil, err
		}
		contents, err := fs.ReadFile(r.source, path.Clean(entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		checksum := sha256.Sum256(contents)
		migrations = append(migrations, migration{version: version, name: name, checksum: fmt.Sprintf("%x", checksum), sql: string(contents)})
	}
	if len(migrations) == 0 {
		return nil, errors.New("no migrations found")
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	for i, migration := range migrations {
		if migration.version != int64(i+1) {
			return nil, fmt.Errorf("migration_gap: expected version %d, found %d", i+1, migration.version)
		}
	}
	return migrations, nil
}

func parseMigrationName(filename string) (int64, string, error) {
	if len(filename) < len("000001_a.sql") || filename[6] != '_' || !strings.HasSuffix(filename, ".sql") {
		return 0, "", fmt.Errorf("invalid migration filename %q", filename)
	}
	version, err := strconv.ParseInt(filename[:6], 10, 64)
	if err != nil || version < 1 || filename[7:len(filename)-4] == "" {
		return 0, "", fmt.Errorf("invalid migration filename %q", filename)
	}
	return version, filename, nil
}

func appliedMigrations(ctx context.Context, conn *pgxpool.Conn) (map[int64]appliedMigration, error) {
	rows, err := conn.Query(ctx, "SELECT version, name, sha256 FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[int64]appliedMigration)
	for rows.Next() {
		var item appliedMigration
		if err := rows.Scan(&item.version, &item.name, &item.checksum); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[item.version] = item
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	return applied, nil
}

type appliedMigration struct {
	version  int64
	name     string
	checksum string
}

func validateApplied(migrations []migration, applied map[int64]appliedMigration) error {
	byVersion := make(map[int64]migration, len(migrations))
	for _, migration := range migrations {
		byVersion[migration.version] = migration
	}
	for version, actual := range applied {
		expected, ok := byVersion[version]
		if !ok {
			return fmt.Errorf("%w: version %d", ErrDatabaseNewer, version)
		}
		if actual.name != expected.name || actual.checksum != expected.checksum {
			return fmt.Errorf("%w: version %d", ErrChecksumMismatch, version)
		}
	}
	var current int64
	for version := range applied {
		if version > current {
			current = version
		}
	}
	for version := int64(1); version <= current; version++ {
		if _, ok := applied[version]; !ok {
			return fmt.Errorf("migration_gap: applied history is missing version %d", version)
		}
	}
	return nil
}
