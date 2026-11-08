package tenant

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"testing"
	"time"

	"babytracker/server/internal/migrate"
	"babytracker/server/internal/testpg"
	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Schema 29 removes the projector's mirror function: no worker role can
// write server_state (`baby-tracker-admin authority init` writes it as the
// migrator) or change the schema. The projector group itself is gone from
// the baseline (Phase 4 Task 27).
func TestRuntimeAndMaintenanceRolesCannotWriteServerState(t *testing.T) {
	fixture := testpg.New(t)
	ctx := t.Context()
	var mirror bool
	if err := fixture.Owner.QueryRow(ctx, "SELECT to_regprocedure('app.update_server_state(uuid,bigint,bigint,bigint,bytea)') IS NOT NULL").Scan(&mirror); err != nil {
		t.Fatal(err)
	}
	if mirror {
		t.Fatal("projector recovery mirror function survived")
	}
	for name, pool := range map[string]*pgxpool.Pool{"runtime": fixture.Runtime, "maintenance": fixture.Maintenance} {
		for _, query := range []string{
			"INSERT INTO server_state (instance_id, recovery_epoch) VALUES ('123e4567-e89b-42d3-a456-426614174000', 1)",
			"UPDATE server_state SET recovery_epoch = recovery_epoch + 1",
			"CREATE TABLE worker_escape (id bigint)",
		} {
			if _, err := pool.Exec(ctx, query); err == nil {
				t.Fatalf("%s crossed the server_state/DDL boundary: %s", name, query)
			}
		}
	}
}

const (
	accountA = "10000000-0000-4000-8000-000000000001"
	accountB = "10000000-0000-4000-8000-000000000002"
	accountC = "10000000-0000-4000-8000-000000000003"
	familyA  = "20000000-0000-4000-8000-000000000001"
	familyB  = "20000000-0000-4000-8000-000000000002"
	familyC  = "20000000-0000-4000-8000-000000000003"
)

// Removing UUID validation must fail before any transaction is consulted.
func TestSetLocalRejectsInvalidIDs(t *testing.T) {
	for _, bad := range []string{
		"", "not-a-uuid", "00000000-0000-0000-0000-000000000000", "00000000000000000000000000000000",
		"10000000x0000x4000x8000x000000000001", "10000000_0000-4000-8000-000000000001",
		"10000000000040008000000000000001", "{10000000-0000-4000-8000-000000000001}",
		"12345678-9ABC-DEF0-1234-56789ABCDEF0", "12345678-9abc-def0-1234-56789abcdeg0",
	} {
		for field, tenant := range map[string]Context{"account": {AccountID: bad, FamilyID: familyA}, "family": {AccountID: accountA, FamilyID: bad}} {
			t.Run(field+"/"+bad, func(t *testing.T) {
				defer func() {
					if recover() != nil {
						t.Fatal("invalid UUID reached the transaction instead of being rejected before SQL")
					}
				}()
				if err := SetLocal(context.Background(), nil, tenant); err == nil {
					t.Fatalf("SetLocal(%+v) accepted invalid UUID", tenant)
				}
			})
		}
	}
}

func TestTenantFoundation(t *testing.T) {
	ctx := context.Background()
	owner, runtime := tenantDatabase(t)
	t.Run("canonical lowercase UUIDs reach transaction context", func(t *testing.T) {
		tx := begin(t, runtime)
		const account = "12345678-9abc-def0-1234-56789abcdef0"
		const family = "abcdef12-3456-789a-bcde-f0123456789a"
		setLocal(t, tx, account, family)
		assertIDs(t, tx, "SELECT app.current_account_id()::text", []string{account})
		assertIDs(t, tx, "SELECT app.current_family_id()::text", []string{family})
	})
	seed := func(account, family string) {
		tx := begin(t, owner)
		setLocal(t, tx, account, family)
		exec(t, tx, "SELECT set_config('app.authority', '1', true)")
		exec(t, tx, "INSERT INTO accounts (id, display_name) VALUES ($1, 'Parent')", account)
		exec(t, tx, "INSERT INTO families (id, display_name, owner_account_id) VALUES ($1, 'Family', $2)", family, account)
		exec(t, tx, "INSERT INTO memberships (family_id, account_id, role) VALUES ($1, $2, 'owner')", family, account)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	seed(accountA, familyA)
	seed(accountB, familyB)
	tx := begin(t, owner)
	setLocal(t, tx, accountC, familyA)
	exec(t, tx, "SELECT set_config('app.authority', '1', true)")
	exec(t, tx, "INSERT INTO accounts (id, display_name) VALUES ($1, 'Caregiver')", accountC)
	exec(t, tx, "INSERT INTO memberships (family_id, account_id, role) VALUES ($1, $2, 'caregiver')", familyA, accountC)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	t.Run("default deny and same physical connection across commit and rollback", func(t *testing.T) {
		var firstPID int
		for i, item := range []struct {
			account, family string
			rollback        bool
		}{
			{accountA, familyA, false}, {accountB, familyB, true}, {accountA, familyA, false},
		} {
			tx := begin(t, runtime)
			var pid int
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				firstPID = pid
			} else if pid != firstPID {
				t.Fatalf("pool did not reuse backend: %d != %d", pid, firstPID)
			}
			for _, table := range []string{"accounts", "families", "memberships"} {
				assertIDs(t, tx, "SELECT "+map[string]string{"accounts": "id", "families": "id", "memberships": "family_id"}[table]+"::text FROM "+table, nil)
			}
			setLocal(t, tx, item.account, item.family)
			assertIDs(t, tx, "SELECT id::text FROM accounts", []string{item.account})
			assertIDs(t, tx, "SELECT id::text FROM families", []string{item.family})
			assertIDs(t, tx, "SELECT DISTINCT family_id::text FROM memberships", []string{item.family})
			if item.rollback {
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			} else if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("all three transactions reused PostgreSQL backend %d; context absent after both commit and rollback", firstPID)
	})

	t.Run("forced RLS applies to table owner too", func(t *testing.T) {
		tx := begin(t, owner)
		assertIDs(t, tx, "SELECT id::text FROM families", nil)
		assertIDs(t, tx, "SELECT id::text FROM accounts", nil)
		assertIDs(t, tx, "SELECT family_id::text FROM memberships", nil)
	})

	t.Run("cross tenant inserts and updates rejected", func(t *testing.T) {
		for _, query := range []string{
			"INSERT INTO memberships (family_id, account_id, role) VALUES ('" + familyB + "', '" + accountC + "', 'caregiver')",
			"UPDATE memberships SET family_id = '" + familyB + "' WHERE account_id = '" + accountC + "'",
			"INSERT INTO accounts (id, display_name) VALUES ('10000000-0000-4000-8000-000000000099', 'Other')",
			"UPDATE accounts SET id = '10000000-0000-4000-8000-000000000099'",
			"INSERT INTO families (id, display_name) VALUES ('" + familyC + "', 'Other')",
			"UPDATE families SET id = '" + familyC + "'",
		} {
			tx := begin(t, runtime)
			setLocal(t, tx, accountA, familyA)
			_, err := tx.Exec(ctx, query)
			assertSQLState(t, err, "42501")
			_ = tx.Rollback(ctx)
		}
	})

	t.Run("default deny writes and missing half context", func(t *testing.T) {
		tx := begin(t, runtime)
		_, err := tx.Exec(ctx, "INSERT INTO accounts (id, display_name) VALUES ($1, 'Other')", familyC)
		assertSQLState(t, err, "42501")
		_ = tx.Rollback(ctx)
		for _, setting := range []string{"app.account_id", "app.family_id"} {
			tx := begin(t, runtime)
			value := accountA
			if setting == "app.family_id" {
				value = familyA
			}
			exec(t, tx, "SELECT set_config($1, $2, true)", setting, value)
			if setting == "app.account_id" {
				assertIDs(t, tx, "SELECT id::text FROM families", nil)
			} else {
				assertIDs(t, tx, "SELECT id::text FROM accounts", nil)
			}
			_ = tx.Rollback(ctx)
		}
	})

	// Schema 29 replaces the deferred check_family_owner trigger: an active
	// family names its owner on the row (families_active_has_owner) and a
	// family has at most one active owner membership at every statement
	// (memberships_one_active_owner, not deferrable). Zero owner memberships
	// are refused by the authority commands, not by the schema.
	authority := func(t *testing.T, tx pgx.Tx) {
		t.Helper()
		exec(t, tx, "SELECT set_config('app.authority', '1', true)")
	}

	t.Run("active family creation without owner fails", func(t *testing.T) {
		tx := begin(t, owner)
		setLocal(t, tx, accountA, familyC)
		authority(t, tx)
		_, err := tx.Exec(ctx, "INSERT INTO families (id, display_name) VALUES ($1, 'No owner')", familyC)
		assertSQLState(t, err, "23514")
	})

	t.Run("tenant context cannot remove demote or duplicate the owner; authority cannot duplicate it", func(t *testing.T) {
		for _, query := range []string{
			"DELETE FROM memberships WHERE role = 'owner'",
			"UPDATE memberships SET role = 'caregiver' WHERE role = 'owner'",
			"UPDATE memberships SET status = 'removed' WHERE role = 'owner'",
			"UPDATE memberships SET role = 'owner' WHERE role = 'caregiver'",
		} {
			tx := begin(t, runtime)
			setLocal(t, tx, accountA, familyA)
			_, err := tx.Exec(ctx, query)
			assertSQLState(t, err, "42501")
			_ = tx.Rollback(ctx)
		}
		tx := begin(t, owner)
		setLocal(t, tx, accountA, familyA)
		authority(t, tx)
		_, err := tx.Exec(ctx, "UPDATE memberships SET role = 'owner' WHERE role = 'caregiver'")
		assertSQLState(t, err, "23505")
	})

	t.Run("concurrent promotion cannot admit a second owner", func(t *testing.T) {
		const family = "20000000-0000-4000-8000-000000000004"
		tx := begin(t, owner)
		setLocal(t, tx, accountA, family)
		authority(t, tx)
		exec(t, tx, "INSERT INTO families (id, display_name, status) VALUES ($1, 'Recovering', 'deletion_pending')", family)
		exec(t, tx, "INSERT INTO memberships (family_id, account_id, role) VALUES ($1, $2, 'caregiver'), ($1, $3, 'caregiver')", family, accountA, accountC)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		first := begin(t, owner)
		setLocal(t, first, accountA, family)
		authority(t, first)
		exec(t, first, "UPDATE families SET status = 'active', owner_account_id = $2 WHERE id = $1", family, accountA)
		exec(t, first, "UPDATE memberships SET role = 'owner' WHERE family_id = $1 AND account_id = $2", family, accountA)

		second := begin(t, owner)
		setLocal(t, second, accountC, family)
		authority(t, second)
		// The second promotion blocks on the first's uncommitted index entry
		// and fails once the first commits.
		done := make(chan error, 1)
		go func() {
			_, err := second.Exec(context.Background(), "UPDATE memberships SET role = 'owner' WHERE family_id = $1 AND account_id = $2", family, accountC)
			done <- err
		}()
		if err := first.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		assertSQLState(t, <-done, "23505")
	})

	t.Run("ownership transfer demotes before it promotes", func(t *testing.T) {
		tx := begin(t, owner)
		setLocal(t, tx, accountA, familyA)
		authority(t, tx)
		_, err := tx.Exec(ctx, "UPDATE memberships SET role = 'owner' WHERE family_id = $1 AND account_id = $2", familyA, accountC)
		assertSQLState(t, err, "23505")
		_ = tx.Rollback(ctx)
		for _, step := range [][2]string{{accountA, accountC}, {accountC, accountA}} {
			tx := begin(t, owner)
			setLocal(t, tx, accountA, familyA)
			authority(t, tx)
			exec(t, tx, "UPDATE memberships SET role = 'caregiver' WHERE family_id = $1 AND account_id = $2", familyA, step[0])
			exec(t, tx, "UPDATE memberships SET role = 'owner' WHERE family_id = $1 AND account_id = $2", familyA, step[1])
			exec(t, tx, "UPDATE families SET owner_account_id = $2 WHERE id = $1", familyA, step[1])
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("inactive families may lack owner but reactivation requires one", func(t *testing.T) {
		tx := begin(t, owner)
		setLocal(t, tx, accountA, familyC)
		authority(t, tx)
		exec(t, tx, "INSERT INTO families (id, display_name, status) VALUES ($1, 'Deleted', 'deletion_pending')", familyC)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		tx = begin(t, owner)
		setLocal(t, tx, accountA, familyC)
		authority(t, tx)
		_, err := tx.Exec(ctx, "UPDATE families SET status = 'active' WHERE id = $1", familyC)
		assertSQLState(t, err, "23514")
	})

	t.Run("tenant context cannot insert or delete families", func(t *testing.T) {
		// Schema 29: family rows are created and deleted only by authority
		// transactions, even by the table owner (FORCE RLS).
		tx := begin(t, owner)
		setLocal(t, tx, accountB, familyB)
		result, err := tx.Exec(ctx, "DELETE FROM families WHERE id = $1", familyB)
		if err != nil || result.RowsAffected() != 0 {
			t.Fatalf("tenant family delete rows=%d err=%v", result.RowsAffected(), err)
		}
		_ = tx.Rollback(ctx)
		tx = begin(t, owner)
		setLocal(t, tx, accountB, familyC)
		_, err = tx.Exec(ctx, "INSERT INTO families (id, display_name, owner_account_id) VALUES ($1, 'Ephemeral', $2)", familyC, accountB)
		assertSQLState(t, err, "42501")
	})

	t.Run("runtime cannot create truncate bypass RLS or assume privileged roles", func(t *testing.T) {
		for _, query := range []string{"CREATE TABLE public.forbidden (id integer)", "CREATE TABLE app.forbidden (id integer)", "CREATE TEMP TABLE forbidden (id integer)", "TRUNCATE accounts", "ALTER TABLE families DISABLE ROW LEVEL SECURITY", "SET ROLE baby_tracker_maintenance", "CREATE ROLE forbidden"} {
			tx := begin(t, runtime)
			_, err := tx.Exec(ctx, query)
			assertSQLState(t, err, "42501")
			_ = tx.Rollback(ctx)
		}
		rows, err := owner.Query(ctx, "SELECT rolname, rolcanlogin, rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls FROM pg_roles WHERE rolname IN ('baby_tracker_runtime', 'baby_tracker_maintenance') ORDER BY rolname")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var name string
			var login, super, db, role, replication, bypass bool
			if err := rows.Scan(&name, &login, &super, &db, &role, &replication, &bypass); err != nil {
				t.Fatal(err)
			}
			if login || super || db || role || replication || bypass {
				t.Fatalf("privileged group role %s", name)
			}
			t.Logf("%s: NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS", name)
			count++
		}
		if rows.Err() != nil || count != 2 {
			t.Fatalf("role count %d: %v", count, rows.Err())
		}
		rows.Close()
		var runtimeSuper, runtimeBypass, runtimeOwner, ownerMembership bool
		if err := runtime.QueryRow(ctx, `SELECT r.rolsuper, r.rolbypassrls,
		    c.relowner = r.oid, pg_has_role(current_user, c.relowner, 'MEMBER')
		    FROM pg_roles r CROSS JOIN pg_class c
		    WHERE r.rolname = current_user AND c.oid = 'public.families'::regclass`).Scan(
			&runtimeSuper, &runtimeBypass, &runtimeOwner, &ownerMembership); err != nil {
			t.Fatal(err)
		}
		if runtimeSuper || runtimeBypass || runtimeOwner || ownerMembership {
			t.Fatal("runtime principal has privileged ownership or membership")
		}
		t.Log("runtime LOGIN: not superuser, not BYPASSRLS, not table owner, not member of owner")
		for _, table := range []string{"accounts", "families", "memberships"} {
			var selectOnly, insert, update, deletePrivilege, truncate, references, trigger bool
			if err := runtime.QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, 'SELECT'),
			    has_table_privilege(current_user, $1, 'INSERT'),
			    has_table_privilege(current_user, $1, 'UPDATE'),
			    has_table_privilege(current_user, $1, 'DELETE'),
			    has_table_privilege(current_user, $1, 'TRUNCATE'),
			    has_table_privilege(current_user, $1, 'REFERENCES'),
			    has_table_privilege(current_user, $1, 'TRIGGER')`, table).Scan(&selectOnly, &insert, &update, &deletePrivilege, &truncate, &references, &trigger); err != nil {
				t.Fatal(err)
			}
			// Schema 29: authority transactions insert rows and update only
			// column-scoped fields; no table-wide UPDATE, DELETE or DDL-like grants.
			if !selectOnly || !insert || update || deletePrivilege || truncate || references || trigger {
				t.Fatalf("unexpected runtime grants on %s", table)
			}
			t.Logf("runtime %s grants: SELECT, INSERT and column-scoped UPDATE only", table)
		}
	})

	t.Run("runtime readiness can read operational tables but cannot mutate them", func(t *testing.T) {
		status, err := migrate.New(migrations.Files).Status(ctx, runtime)
		if err != nil {
			t.Fatalf("runtime migration readiness: %v", err)
		}
		if status.Current != 37 || status.Required != 37 {
			t.Fatalf("runtime schema status = %+v", status)
		}
		var count int
		if err := runtime.QueryRow(ctx, "SELECT count(*) FROM server_state").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if err := runtime.QueryRow(ctx, "SELECT count(*) FROM schema_compatibility").Scan(&count); err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{
			"INSERT INTO schema_migrations (version, name, sha256) VALUES (99, 'forbidden', 'forbidden')",
			"UPDATE schema_migrations SET name = 'forbidden'",
			"DELETE FROM schema_migrations",
			"UPDATE schema_compatibility SET current_version = 99",
			"INSERT INTO server_state (instance_id, recovery_epoch) VALUES ('10000000-0000-4000-8000-000000000099', 1)",
			"UPDATE server_state SET recovery_epoch = 99",
			"DELETE FROM server_state",
		} {
			tx := begin(t, runtime)
			_, err := tx.Exec(ctx, query)
			assertSQLState(t, err, "42501")
			_ = tx.Rollback(ctx)
		}
		t.Log("runtime schema_migrations/server_state grants: SELECT only; actual readiness succeeds and writes are denied")
	})
}

func tenantDatabase(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("BABY_TRACKER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("BABY_TRACKER_TEST_DATABASE_URL is not set; requires disposable PostgreSQL with role/database creation privileges")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	bootstrap, err := os.ReadFile("../../deploy/postgres/init-roles.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	database, ownerRole, runtimeRole := "tenant_test_"+suffix, "tenant_owner_"+suffix, "tenant_runtime_"+suffix
	for _, role := range []string{ownerRole, runtimeRole} {
		if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN PASSWORD 'disposable-test-only' NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Error(err)
			}
		})
	}
	if _, err := admin.Exec(ctx, "GRANT baby_tracker_runtime TO "+pgx.Identifier{runtimeRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+" OWNER "+pgx.Identifier{ownerRole}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	open := func(role string, max int32) *pgxpool.Pool {
		u, err := url.Parse(databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		u.User, u.Path = url.UserPassword(role, "disposable-test-only"), "/"+database
		cfg, err := pgxpool.ParseConfig(u.String())
		if err != nil {
			t.Fatal(err)
		}
		cfg.MaxConns = max
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	owner := open(ownerRole, 2)
	if err := migrate.New(migrations.Files).Apply(ctx, owner); err != nil {
		t.Fatal(err)
	}
	return owner, open(runtimeRole, 1)
}

func begin(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func setLocal(t *testing.T, tx pgx.Tx, account, family string) {
	t.Helper()
	if err := SetLocal(context.Background(), tx, Context{AccountID: account, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
}

func exec(t *testing.T, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func assertSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("error = %v, want SQLSTATE %s", err, code)
	}
}

func assertIDs(t *testing.T, tx pgx.Tx, query string, want []string) {
	t.Helper()
	rows, err := tx.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, want %v", query, got, want)
	}
}
