//go:build identitytest

package capacity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bootstrap "babytracker/server/deploy/postgres"
	"babytracker/server/internal/migrate"
	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const postgresImage = "postgres:17.6-alpine@sha256:ef257d85f76e48da1c64832459b59fcaba1a4dac97bf5d7450c77753542eee94"
const ownedLabel = "com.babytracker.capacity.owner"

// ownedPostgres has no caller-selected connection or database path. Its data
// exists only in tmpfs, never an ambient volume or a host database directory.
type ownedPostgres struct {
	id, owner, root, dsn, password string
	Runtime, Maintenance, Owner    *pgxpool.Pool
	pools                          []*pgxpool.Pool
	mu                             sync.Mutex
	closed                         bool
	receipt                        resourceReceipt
	receiptPath                    string
}

func dockerCommand(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "docker", args...)
}
func dockerOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmd := dockerCommand(ctx, args...)
	raw, err := cmd.Output()
	if err != nil {
		return nil, errors.New("capacity_docker_operation_failed")
	}
	return raw, nil
}

func localDocker(ctx context.Context) error {
	// Honor no ambient remote daemon selector. Inspect the selected context before
	// creating anything; TCP, SSH and remote contexts are never capacity targets.
	for _, name := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		if os.Getenv(name) != "" {
			return ErrUnsafeTarget
		}
	}
	raw, err := dockerOutput(ctx, "context", "inspect")
	if err != nil {
		return err
	}
	var contexts []struct {
		Endpoints map[string]struct{ Host string }
	}
	if json.Unmarshal(raw, &contexts) != nil || len(contexts) != 1 || !strings.HasPrefix(contexts[0].Endpoints["docker"].Host, "unix:///") {
		return ErrUnsafeTarget
	}
	return nil
}

func randomHex(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", ErrFixtureInvalid
	}
	return hex.EncodeToString(raw), nil
}

// Reserve every private path in durable ownership before creating any material.
func reservePostgres(fixtureRoot, restoreRoot string) (*ownedPostgres, error) {
	for _, path := range []string{fixtureRoot, restoreRoot} {
		if path != "" {
			if err := disposableDestination(path); err != nil {
				return nil, err
			}
		}
	}
	owner, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	suffix, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	password, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	tempBase, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return nil, err
	}
	root := filepath.Join(tempBase, "bt-capacity-"+owner+"-"+suffix)
	if err := disposableDestination(root); err != nil {
		return nil, err
	}
	db := &ownedPostgres{owner: owner, root: root, password: password, id: "bt-capacity-" + owner}
	db.receipt = resourceReceipt{Version: 1, Owner: owner, Root: root, FixtureRoot: fixtureRoot, RestoreRoot: restoreRoot, Containers: []string{db.id}, Networks: []string{}}
	db.receiptPath = filepath.Join(tempBase, "resources-"+owner+".json")
	if control := os.Getenv("BT_CAPACITY_CONTROL_DIR"); control != "" {
		if err := privateDirectory(control); err != nil {
			return nil, err
		}
		db.receiptPath = filepath.Join(control, "resources-"+owner+".json")
	}
	if err := db.writeReceipt(); err != nil {
		return nil, err
	}
	return db, nil
}
func startPostgres(ctx context.Context, db *ownedPostgres) (_ *ownedPostgres, resultErr error) {
	if err := localDocker(ctx); err != nil {
		return nil, err
	}
	owner, password, root := db.owner, db.password, db.root
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, db.Close())
		}
	}()
	if err := os.Mkdir(root, 0700); err != nil {
		return nil, err
	}
	secret := filepath.Join(root, "postgres-password")
	if err := os.WriteFile(secret, []byte(password), 0600); err != nil {
		return nil, err
	}
	// Test-only child boundary; untagged production has no such seam.
	if os.Getenv("BT_CAPACITY_TEST_PAUSE_BEFORE_POSTGRES") == "1" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	raw, err := dockerOutput(ctx, "run", "--detach", "--cpus", "2.5", "--memory", "3221225472", "--memory-swap", "3221225472", "--name", "bt-capacity-"+owner, "--label", ownedLabel+"="+owner, "--publish", "127.0.0.1::5432", "--mount", "type=bind,src="+secret+",dst=/run/secrets/password,readonly", "--tmpfs", "/var/lib/postgresql/data:rw,size=2147483648", "--env", "POSTGRES_PASSWORD_FILE=/run/secrets/password", postgresImage)
	if err != nil {
		return nil, err
	}
	db.id = strings.TrimSpace(string(raw))
	if len(db.id) != 64 {
		return nil, ErrUnsafeTarget
	}
	raw, err = dockerOutput(ctx, "port", db.id, "5432/tcp")
	if err != nil {
		return nil, err
	}
	binding := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(binding, "127.0.0.1:") || strings.Contains(binding, "\n") {
		return nil, ErrUnsafeTarget
	}
	u := url.URL{Scheme: "postgres", Host: binding, Path: "/postgres", User: url.UserPassword("postgres", password)}
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	db.dsn = u.String()
	admin, err := pgxpool.New(ctx, db.dsn)
	if err != nil {
		return nil, ErrFixtureInvalid
	}
	db.pools = append(db.pools, admin)
	deadline := time.NewTimer(40 * time.Second)
	defer deadline.Stop()
	for {
		if admin.Ping(ctx) == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errors.New("capacity_postgres_start_timeout")
		case <-time.After(200 * time.Millisecond):
		}
	}
	if _, err := admin.Exec(ctx, bootstrap.BootstrapSQL); err != nil {
		return nil, errors.New("capacity_role_bootstrap_failed")
	}
	for _, role := range []string{"capacity_owner", "capacity_runtime", "capacity_maintenance"} {
		// Password is generated hex; identifiers are closed constants. Never expose SQL.
		if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN PASSWORD '"+password+"' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			return nil, errors.New("capacity_role_create_failed")
		}
	}
	for _, grant := range []string{"GRANT baby_tracker_runtime TO capacity_runtime", "GRANT baby_tracker_maintenance TO capacity_maintenance", "CREATE DATABASE capacity_fixture OWNER capacity_owner"} {
		if _, err := admin.Exec(ctx, grant); err != nil {
			return nil, errors.New("capacity_database_create_failed")
		}
	}
	db.Owner, err = db.open(ctx, "capacity_owner", 2)
	if err != nil {
		return nil, err
	}
	db.Runtime, err = db.open(ctx, "capacity_runtime", 32)
	if err != nil {
		return nil, err
	}
	db.Maintenance, err = db.open(ctx, "capacity_maintenance", 2)
	if err != nil {
		return nil, err
	}
	return db, nil
}

func (db *ownedPostgres) open(ctx context.Context, role string, max int32) (*pgxpool.Pool, error) {
	return db.openWith(ctx, role, max, false)
}

// openReadOnly opens a pool whose sessions default to read-only
// transactions (database.ReadOnlyByDefault), as the server's MCP pool.
func (db *ownedPostgres) openReadOnly(ctx context.Context, role string, max int32) (*pgxpool.Pool, error) {
	return db.openWith(ctx, role, max, true)
}

func (db *ownedPostgres) openWith(ctx context.Context, role string, max int32, readOnly bool) (*pgxpool.Pool, error) {
	u, _ := url.Parse(db.dsn)
	u.User = url.UserPassword(role, db.password)
	u.Path = "/capacity_fixture"
	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, ErrFixtureInvalid
	}
	cfg.MaxConns = max
	if readOnly {
		cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, ErrFixtureInvalid
	}
	db.pools = append(db.pools, pool)
	return pool, nil
}

func (db *ownedPostgres) migrate(ctx context.Context) error {
	return migrate.New(migrations.Files).Apply(ctx, db.Owner)
}

func (db *ownedPostgres) dump(ctx context.Context, path string) error {
	// No open application writers remain when the snapshot is taken.
	for _, pool := range db.pools {
		pool.Close()
	}
	db.pools = nil
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	cmd := dockerCommand(ctx, "exec", db.id, "pg_dump", "-U", "postgres", "-d", "capacity_fixture", "--format=custom", "--create")
	cmd.Stdout = file
	cmd.Stderr = io.Discard
	runErr := cmd.Run()
	syncErr := file.Sync()
	closeErr := file.Close()
	if runErr != nil {
		return errors.New("capacity_dump_failed")
	}
	return errors.Join(syncErr, closeErr)
}

func (db *ownedPostgres) restore(ctx context.Context, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	cmd := dockerCommand(ctx, "exec", "-i", db.id, "pg_restore", "-U", "postgres", "-d", "postgres", "--exit-on-error", "--clean", "--if-exists", "--create")
	cmd.Stdin = file
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		return errors.New("capacity_restore_failed")
	}
	return nil
}

func (db *ownedPostgres) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return nil
	}
	for _, pool := range db.pools {
		pool.Close()
	}
	db.pools = nil
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := len(db.receipt.Containers) - 1; i >= 1; i-- {
		if err := removeOwned(ctx, "container", db.receipt.Containers[i], db.owner); err != nil {
			return err
		}
	}

	if db.id != "" {
		existing, err := dockerOutput(ctx, "ps", "--all", "--quiet", "--filter", "label="+ownedLabel+"="+db.owner)
		if err != nil {
			return errors.New("capacity_cleanup_verification_failed")
		}
		if strings.TrimSpace(string(existing)) == "" {
			db.id = ""
		}
	}
	if db.id != "" {
		raw, err := dockerOutput(ctx, "inspect", "--format", `{{index .Config.Labels "`+ownedLabel+`"}}`, db.id)
		if err != nil || strings.TrimSpace(string(raw)) != db.owner {
			return errors.New("capacity_cleanup_ownership_failed")
		}
		if _, err := dockerOutput(ctx, "rm", "--force", "--volumes", db.id); err != nil {
			return errors.New("capacity_cleanup_failed")
		}
		raw, err = dockerOutput(ctx, "ps", "--all", "--quiet", "--filter", "label="+ownedLabel+"="+db.owner)
		if err != nil || len(strings.TrimSpace(string(raw))) != 0 {
			return errors.New("capacity_cleanup_container_remains")
		}
		raw, err = dockerOutput(ctx, "volume", "ls", "--quiet", "--filter", "label="+ownedLabel+"="+db.owner)
		if err != nil || len(strings.TrimSpace(string(raw))) != 0 {
			return errors.New("capacity_cleanup_volume_remains")
		}
		db.id = ""
	}
	for _, name := range db.receipt.Networks {
		if err := removeOwned(ctx, "network", name, db.owner); err != nil {
			return err
		}
	}
	if db.root != "" {
		// This path came exclusively from MkdirTemp; no caller database paths exist.
		if !strings.HasPrefix(filepath.Base(db.root), "bt-capacity-"+db.owner+"-") {
			return ErrUnsafeTarget
		}
		if err := os.RemoveAll(db.root); err != nil {
			return errors.New("capacity_cleanup_private_material_remains")
		}
		if _, err := os.Lstat(db.root); !os.IsNotExist(err) {
			return errors.New("capacity_cleanup_private_material_remains")
		}
	}
	if os.Getenv("BT_CAPACITY_CONTROL_DIR") == "" {
		if err := os.Remove(db.receiptPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	db.closed = true
	return nil
}
