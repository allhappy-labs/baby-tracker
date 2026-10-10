package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"
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

// Schema 29 replaces the projector-owned recovery mirror
// (app.update_server_state and the generation/revision/event_hash chain head)
// with one server_state row that `baby-tracker-admin authority init` writes
// as the migrator: an instance ID, a wire-safe recovery epoch and the
// actor-ID key commitment. Epoch monotonicity is the admin rotate-epoch
// command's (cmd/baby-tracker-admin TestAuthorityRotateEpochIsMonotonicAndInvalidatesSessions).
func TestServerStateIsOneBoundRowWithoutRecoveryMirror(t *testing.T) {
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
	var mirror bool
	var headColumns int
	if err := pool.QueryRow(ctx, `SELECT to_regprocedure('app.update_server_state(uuid,bigint,bigint,bigint,bytea)') IS NOT NULL,
		(SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='server_state'
		   AND column_name IN ('generation','revision','event_hash'))`).Scan(&mirror, &headColumns); err != nil {
		t.Fatal(err)
	}
	if mirror || headColumns != 0 {
		t.Fatalf("legacy mirror survived function=%t head columns=%d", mirror, headColumns)
	}
	const instance = "123e4567-e89b-42d3-a456-426614174000"
	commitment := bytesWithFirst(7)
	for name, row := range map[string]struct {
		epoch      int64
		commitment []byte
	}{
		"zero epoch":          {0, commitment},
		"unsafe epoch":        {9007199254740992, commitment},
		"short commitment":    {1, commitment[:31]},
		"oversize commitment": {1, append(append([]byte(nil), commitment...), 0)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := pool.Exec(ctx, `INSERT INTO server_state(instance_id,recovery_epoch,actor_key_commitment) VALUES ($1,$2,$3)`, instance, row.epoch, row.commitment)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Fatalf("server_state accepted %s: %v", name, err)
			}
		})
	}
	if _, err := pool.Exec(ctx, `INSERT INTO server_state(instance_id,recovery_epoch,actor_key_commitment) VALUES ($1,9007199254740991,$2)`, instance, commitment); err != nil {
		t.Fatalf("bound server_state rejected: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO server_state(instance_id,recovery_epoch) VALUES ($1,1)`, "223e4567-e89b-42d3-a456-426614174000")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("second server_state row: %v", err)
	}
	var gotInstance string
	var epoch int64
	var gotCommitment []byte
	if err := pool.QueryRow(ctx, "SELECT instance_id::text,recovery_epoch,actor_key_commitment FROM server_state WHERE singleton").Scan(&gotInstance, &epoch, &gotCommitment); err != nil {
		t.Fatal(err)
	}
	if gotInstance != instance || epoch != 9007199254740991 || !bytes.Equal(gotCommitment, commitment) {
		t.Fatalf("server state=%s/%d/%x", gotInstance, epoch, gotCommitment)
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
