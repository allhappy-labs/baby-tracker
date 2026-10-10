package tenant

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Bootstrap must reject pre-existing privilege escalation before migrations
// grant the application tables to either group. Changes below are rolled back.
func TestBootstrapRoles(t *testing.T) {
	databaseURL := os.Getenv("BABY_TRACKER_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("BABY_TRACKER_TEST_DATABASE_URL is not set; requires disposable PostgreSQL with role-administration privileges")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	bootstrap, err := os.ReadFile("../../deploy/postgres/init-roles.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, string(bootstrap)); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"baby_tracker_runtime", "baby_tracker_maintenance"} {
		t.Run(role, func(t *testing.T) {
			t.Run("compatible reruns preserve incoming login members", func(t *testing.T) {
				tx := begin(t, admin)
				principal := fmt.Sprintf("bootstrap_member_%d", time.Now().UnixNano())
				exec(t, tx, "CREATE ROLE "+pgx.Identifier{principal}.Sanitize()+" LOGIN")
				exec(t, tx, "GRANT "+pgx.Identifier{role}.Sanitize()+" TO "+pgx.Identifier{principal}.Sanitize())
				for range 2 {
					exec(t, tx, string(bootstrap))
				}
				var member bool
				if err := tx.QueryRow(ctx, "SELECT pg_has_role($1, $2, 'MEMBER')", principal, role).Scan(&member); err != nil {
					t.Fatal(err)
				}
				if !member {
					t.Fatal("bootstrap removed the login's group membership")
				}
			})
			for _, attribute := range []string{"LOGIN", "SUPERUSER", "CREATEDB", "CREATEROLE", "REPLICATION", "BYPASSRLS"} {
				t.Run("rejects "+attribute, func(t *testing.T) {
					tx := begin(t, admin)
					exec(t, tx, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" "+attribute)
					_, err := tx.Exec(ctx, string(bootstrap))
					assertSQLState(t, err, "42501")
				})
			}
			for name, statement := range map[string]string{
				"NOINHERIT":   "ALTER ROLE " + pgx.Identifier{role}.Sanitize() + " NOINHERIT",
				"role config": "ALTER ROLE " + pgx.Identifier{role}.Sanitize() + " SET search_path=pg_catalog",
				"expiry":      "ALTER ROLE " + pgx.Identifier{role}.Sanitize() + " VALID UNTIL '2000-01-01'",
			} {
				t.Run("rejects "+name, func(t *testing.T) {
					tx := begin(t, admin)
					exec(t, tx, statement)
					_, err := tx.Exec(ctx, string(bootstrap))
					assertSQLState(t, err, "42501")
				})
			}
			for _, options := range []string{"", " WITH INHERIT FALSE, SET FALSE"} {
				t.Run("rejects outgoing role membership"+options, func(t *testing.T) {
					tx := begin(t, admin)
					parent := fmt.Sprintf("bootstrap_parent_%d", time.Now().UnixNano())
					attributes := " NOLOGIN"
					if options == "" {
						attributes += " BYPASSRLS"
					}
					exec(t, tx, "CREATE ROLE "+pgx.Identifier{parent}.Sanitize()+attributes)
					exec(t, tx, "GRANT "+pgx.Identifier{parent}.Sanitize()+" TO "+pgx.Identifier{role}.Sanitize()+options)
					_, err := tx.Exec(ctx, string(bootstrap))
					assertSQLState(t, err, "42501")
				})
			}
		})
	}
}
