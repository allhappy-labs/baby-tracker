package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRunnerApplyTracksVersionChecksumAndConcurrentCalls(t *testing.T) {
	databaseURL := os.Getenv("BABY_TRACKER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("BABY_TRACKER_TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS app CASCADE; DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatalf("reset test database: %v", err)
	}

	runner := New(singleMigrationFS("CREATE TABLE server_state (instance_id text NOT NULL)"))
	status, err := runner.Status(ctx, pool)
	if err != nil {
		t.Fatalf("status before apply: %v", err)
	}
	if status.Required != 1 || status.Current != 0 {
		t.Fatalf("status before apply = %+v, want required 1 and current 0", status)
	}

	var group sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			errs <- runner.Apply(ctx, pool)
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent apply: %v", err)
		}
	}

	var version int64
	var checksum string
	if err := pool.QueryRow(ctx, "SELECT version, sha256 FROM schema_migrations").Scan(&version, &checksum); err != nil {
		t.Fatalf("read applied migration: %v", err)
	}
	wantChecksum := fmt.Sprintf("%x", sha256.Sum256([]byte("CREATE TABLE server_state (instance_id text NOT NULL)")))
	if version != 1 || checksum != wantChecksum {
		t.Fatalf("applied migration = version %d checksum %q, want version 1 checksum %q", version, checksum, wantChecksum)
	}
	if err := runner.Apply(ctx, pool); err != nil {
		t.Fatalf("idempotent apply: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count applied migrations: %v", err)
	}
	if count != 1 {
		t.Fatalf("applied migration rows after idempotent apply = %d, want 1", count)
	}
	status, err = runner.Status(ctx, pool)
	if err != nil {
		t.Fatalf("status after apply: %v", err)
	}
	if status.Required != 1 || status.Current != 1 {
		t.Fatalf("status after apply = %+v, want required 1 and current 1", status)
	}

	changed := New(singleMigrationFS("CREATE TABLE server_state (instance_id bigint NOT NULL)"))
	if err := changed.Apply(ctx, pool); err == nil || !strings.Contains(err.Error(), "migration_checksum_mismatch") {
		t.Fatalf("changed migration Apply() error = %v, want migration_checksum_mismatch", err)
	}
}

func TestRunnerRejectsGapsInAppliedHistory(t *testing.T) {
	databaseURL := os.Getenv("BABY_TRACKER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("BABY_TRACKER_TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer pool.Close()
	runner := New(threeMigrationFS())

	for _, version := range []int64{1, 2} {
		t.Run(fmt.Sprintf("missing version %d", version), func(t *testing.T) {
			if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS app CASCADE; DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
				t.Fatalf("reset test database: %v", err)
			}
			if err := runner.Apply(ctx, pool); err != nil {
				t.Fatalf("apply complete history: %v", err)
			}
			if _, err := pool.Exec(ctx, "DELETE FROM schema_migrations WHERE version = $1", version); err != nil {
				t.Fatalf("delete migration %d: %v", version, err)
			}
			if _, err := runner.Status(ctx, pool); err == nil || !strings.Contains(err.Error(), "migration_gap") {
				t.Fatalf("status after deleting version %d error = %v, want migration_gap", version, err)
			}
			if err := runner.Apply(ctx, pool); err == nil || !strings.Contains(err.Error(), "migration_gap") {
				t.Fatalf("apply after deleting version %d error = %v, want migration_gap", version, err)
			}
		})
	}
}

func TestRecoveryCompatibilityMigrationInstallsMirrorFunction(t *testing.T) {
	databaseURL := os.Getenv("BABY_TRACKER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("BABY_TRACKER_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS app CASCADE; DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
		t.Fatal(err)
	}
	if err := New(os.DirFS("../../migrations")).Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	aheadHash := make([]byte, 32)
	aheadHash[0] = 2
	if _, err := pool.Exec(ctx, "SELECT app.update_server_state($1,$2,$3,$4,$5)",
		"123e4567-e89b-42d3-a456-426614174000", int64(1), int64(1), int64(2), aheadHash); err != nil {
		t.Fatalf("initial mirror update failed: %v", err)
	}
	for name, update := range map[string]struct {
		revision int64
		hash     []byte
	}{
		"lower revision": {1, bytesWithFirst(1)},
		"changed hash":   {2, bytesWithFirst(3)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, "SELECT app.update_server_state($1,$2,$3,$4,$5)",
				"123e4567-e89b-42d3-a456-426614174000", int64(1), int64(1), update.revision, update.hash); err == nil {
				t.Fatalf("same-epoch mirror accepted %s", name)
			}
			var gotRevision int64
			var gotHash []byte
			if err := pool.QueryRow(ctx, "SELECT revision,event_hash FROM server_state WHERE singleton").Scan(&gotRevision, &gotHash); err != nil || gotRevision != 2 || !bytes.Equal(gotHash, aheadHash) {
				t.Fatalf("same-epoch rejection changed mirror revision=%d hash=%x err=%v", gotRevision, gotHash, err)
			}
		})
	}
	if _, err := pool.Exec(ctx, "SELECT app.update_server_state($1,$2,$3,$4,$5)",
		"123e4567-e89b-42d3-a456-426614174000", int64(2), int64(1), int64(0), make([]byte, 32)); err != nil {
		t.Fatalf("higher-epoch recovery transition rejected: %v", err)
	}
	if _, err := pool.Exec(ctx, "SELECT app.update_server_state($1,$2,$3,$4,$5)",
		"123e4567-e89b-42d3-a456-426614174000", int64(9007199254740991), int64(1), int64(0), make([]byte, 32)); err != nil {
		t.Fatalf("projection-owned mirror update failed: %v", err)
	}
	if _, err := pool.Exec(ctx, "SELECT app.update_server_state($1,$2,$3,$4,$5)",
		"123e4567-e89b-42d3-a456-426614174000", int64(9007199254740992), int64(1), int64(0), make([]byte, 32)); err == nil {
		t.Fatal("mirror function accepted an unsafe integer epoch")
	}
	var instance string
	var epoch, generation, revision int64
	if err := pool.QueryRow(ctx, "SELECT instance_id::text,recovery_epoch,generation,revision FROM server_state WHERE singleton").Scan(&instance, &epoch, &generation, &revision); err != nil {
		t.Fatal(err)
	}
	if instance != "123e4567-e89b-42d3-a456-426614174000" || epoch != 9007199254740991 || generation != 1 || revision != 0 {
		t.Fatalf("server state mirror=%s/%d/%d/%d", instance, epoch, generation, revision)
	}
	var compatibilityRows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_compatibility WHERE singleton").Scan(&compatibilityRows); err != nil || compatibilityRows != 1 {
		t.Fatalf("schema compatibility metadata rows=%d err=%v", compatibilityRows, err)
	}
}

func bytesWithFirst(value byte) []byte {
	result := make([]byte, 32)
	result[0] = value
	return result
}

func singleMigrationFS(sql string) fs.FS {
	return fstest.MapFS{
		"000001_server_state.sql": &fstest.MapFile{Data: []byte(sql)},
	}
}

func threeMigrationFS() fs.FS {
	return fstest.MapFS{
		"000001_first.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE migration_one (id bigint PRIMARY KEY)")},
		"000002_second.sql": &fstest.MapFile{Data: []byte("CREATE TABLE migration_two (id bigint PRIMARY KEY)")},
		"000003_third.sql":  &fstest.MapFile{Data: []byte("CREATE TABLE migration_three (id bigint PRIMARY KEY)")},
	}
}
