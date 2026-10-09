package database_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/migrate"
	"babytracker/server/internal/testpg"
	"babytracker/server/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestVerifyRoleSeparation(t *testing.T) {
	fixture := testpg.New(t)
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 29); err != nil {
		t.Fatalf("valid runtime/projector roles rejected: %v", err)
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Runtime, 21); err == nil {
		t.Fatal("runtime role was accepted as projector")
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Projector, fixture.Projector, 29); err == nil {
		t.Fatal("projector role was accepted as runtime")
	}
}

func TestVerifyRoleSeparationRejectsDifferentLogicalDatabases(t *testing.T) {
	fixture := testpg.New(t)
	var ownerUser string
	if err := fixture.Owner.QueryRow(t.Context(), "SELECT current_user").Scan(&ownerUser); err != nil {
		t.Fatal(err)
	}
	databaseName := fmt.Sprintf("role_split_%d", time.Now().UnixNano())
	if _, err := fixture.Admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize()+" OWNER "+pgx.Identifier{ownerUser}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fixture.Admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)")
	})
	openDatabase := func(source *pgxpool.Pool) *pgxpool.Pool {
		t.Helper()
		parsed, err := url.Parse(source.Config().ConnString())
		if err != nil {
			t.Fatal(err)
		}
		parsed.Path = "/" + databaseName
		pool, err := pgxpool.New(t.Context(), parsed.String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return pool
	}
	owner := openDatabase(fixture.Owner)
	if err := migrate.New(migrations.Files).Apply(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	projector := openDatabase(fixture.Projector)
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, projector, 21); err == nil {
		t.Fatal("runtime and projector on different logical databases were accepted")
	}
}

func TestVerifyRoleSeparationRejectsProjectorTenantAuthority(t *testing.T) {
	for _, grant := range []string{
		"GRANT SELECT, UPDATE ON public.families TO baby_tracker_projector",
		"GRANT DELETE ON public.memberships TO baby_tracker_projector",
		"GRANT REFERENCES (id) ON public.families TO baby_tracker_projector",
		"GRANT EXECUTE ON FUNCTION app.current_family_id() TO baby_tracker_projector",
		"CREATE SEQUENCE public.projector_escape; GRANT USAGE ON SEQUENCE public.projector_escape TO baby_tracker_projector",
	} {
		t.Run(grant, func(t *testing.T) {
			fixture := testpg.New(t)
			if _, err := fixture.Owner.Exec(t.Context(), grant); err != nil {
				t.Fatal(err)
			}
			if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 29); err == nil {
				t.Fatal("projector tenant authority was accepted")
			}
		})
	}
}

func TestVerifyRoleSeparationRejectsRoleDelegationDrift(t *testing.T) {
	for _, test := range []struct {
		name    string
		setup   func(*testing.T, *testpg.Fixture, string)
		restore func(*testing.T, *testpg.Fixture, string)
	}{
		{name: "admin option", setup: membershipSQL("GRANT baby_tracker_projector TO %s WITH ADMIN OPTION"), restore: membershipSQL("GRANT baby_tracker_projector TO %s WITH ADMIN FALSE")},
		{name: "inherit false", setup: membershipSQL("GRANT baby_tracker_projector TO %s WITH INHERIT FALSE"), restore: membershipSQL("GRANT baby_tracker_projector TO %s WITH INHERIT TRUE")},
		{name: "set false", setup: membershipSQL("GRANT baby_tracker_projector TO %s WITH SET FALSE"), restore: membershipSQL("GRANT baby_tracker_projector TO %s WITH SET TRUE")},
		{name: "group login", setup: groupSQL("ALTER ROLE baby_tracker_projector LOGIN"), restore: groupSQL("ALTER ROLE baby_tracker_projector NOLOGIN")},
		{name: "group config", setup: groupSQL("ALTER ROLE baby_tracker_projector SET search_path=pg_catalog"), restore: groupSQL("ALTER ROLE baby_tracker_projector RESET ALL")},
		{name: "group outgoing membership", setup: groupSQL("CREATE ROLE projector_parent NOLOGIN; GRANT projector_parent TO baby_tracker_projector"), restore: groupSQL("REVOKE projector_parent FROM baby_tracker_projector; DROP ROLE projector_parent")},
		{name: "projector rogue incoming member", setup: groupSQL("CREATE ROLE rogue_projector LOGIN; GRANT baby_tracker_projector TO rogue_projector"), restore: groupSQL("REVOKE baby_tracker_projector FROM rogue_projector; DROP ROLE rogue_projector")},
		{name: "runtime rogue incoming member", setup: groupSQL("CREATE ROLE rogue_runtime LOGIN; GRANT baby_tracker_runtime TO rogue_runtime"), restore: groupSQL("REVOKE baby_tracker_runtime FROM rogue_runtime; DROP ROLE rogue_runtime")},
		{name: "login disabled", setup: loginSQL("ALTER ROLE %s NOLOGIN"), restore: loginSQL("ALTER ROLE %s LOGIN")},
		{name: "login config", setup: loginSQL("ALTER ROLE %s SET search_path=pg_catalog"), restore: loginSQL("ALTER ROLE %s RESET ALL")},
		{name: "login expired", setup: loginSQL("ALTER ROLE %s VALID UNTIL '2000-01-01'"), restore: loginSQL("ALTER ROLE %s VALID UNTIL 'infinity'")},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := testpg.New(t)
			var projectorUser string
			if err := fixture.Projector.QueryRow(t.Context(), "SELECT current_user").Scan(&projectorUser); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { test.restore(t, fixture, projectorUser) })
			test.setup(t, fixture, projectorUser)
			if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 29); err == nil {
				t.Fatal("unsafe role delegation state was accepted")
			}
		})
	}
}

func TestVersionedPrivilegeManifestAuthorizesOnlyExactAdditiveGrant(t *testing.T) {
	fixture := testpg.New(t)
	const signature = "app.future_identity_challenge(text)"
	if _, err := fixture.Owner.Exec(t.Context(), `CREATE FUNCTION app.future_identity_challenge(text) RETURNS boolean
		LANGUAGE sql IMMUTABLE AS $$ SELECT $1 <> '' $$;
		REVOKE ALL ON FUNCTION app.future_identity_challenge(text) FROM PUBLIC;
		GRANT EXECUTE ON FUNCTION app.future_identity_challenge(text) TO baby_tracker_runtime`); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 29); err == nil {
		t.Fatal("unmanifested additive grant was accepted")
	}
	if _, err := fixture.Owner.Exec(t.Context(), `INSERT INTO schema_privilege_manifest
		SELECT 30,principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=29`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Owner.Exec(t.Context(), "INSERT INTO schema_privilege_manifest VALUES (30,'baby_tracker_runtime','function',$1,'EXECUTE')", signature); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 29); err == nil {
		t.Fatal("future snapshot was accepted before the compatible schema version")
	}
	if _, err := fixture.Owner.Exec(t.Context(), "UPDATE schema_compatibility SET current_version=30 WHERE singleton"); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 30); err != nil {
		t.Fatalf("exact additive snapshot rejected: %v", err)
	}
	if _, err := fixture.Runtime.Exec(t.Context(), `INSERT INTO schema_privilege_manifest VALUES
		(10,'baby_tracker_runtime','function','app.untrusted()','EXECUTE')`); err == nil {
		t.Fatal("runtime could modify privilege manifest")
	}
	if _, err := fixture.Owner.Exec(t.Context(), "UPDATE schema_privilege_manifest SET privileges='EXECUTE,CREATE' WHERE schema_version=30 AND principal='baby_tracker_runtime' AND object_identity=$1", signature); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 30); err == nil {
		t.Fatal("forbidden privilege vocabulary was accepted")
	}
	if _, err := fixture.Owner.Exec(t.Context(), "UPDATE schema_privilege_manifest SET privileges='EXECUTE',object_identity='app.wrong_signature(text)' WHERE schema_version=30 AND principal='baby_tracker_runtime' AND object_identity=$1", signature); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 30); err == nil {
		t.Fatal("wrong function signature was accepted")
	}
	if _, err := fixture.Owner.Exec(t.Context(), "UPDATE schema_privilege_manifest SET object_identity=$1 WHERE schema_version=30 AND principal='baby_tracker_runtime' AND object_identity='app.wrong_signature(text)'", signature); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Owner.Exec(t.Context(), "REVOKE EXECUTE ON FUNCTION app.future_identity_challenge(text) FROM baby_tracker_runtime"); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 30); err == nil {
		t.Fatal("manifest row without effective grant was accepted")
	}
}

func TestVersionedPrivilegeManifestAuthorizesExactColumnGrant(t *testing.T) {
	fixture := testpg.New(t)
	if _, err := fixture.Owner.Exec(t.Context(), `INSERT INTO schema_privilege_manifest
		SELECT 30,principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=29;
		INSERT INTO schema_privilege_manifest VALUES (30,'baby_tracker_runtime','column','public.families.display_name','UPDATE');
		GRANT UPDATE (display_name) ON public.families TO baby_tracker_runtime;
		UPDATE schema_compatibility SET current_version=30 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 30); err != nil {
		t.Fatalf("exact manifested column grant rejected: %v", err)
	}
	if _, err := fixture.Owner.Exec(t.Context(), "GRANT REFERENCES (id) ON public.families TO baby_tracker_runtime"); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 30); err == nil {
		t.Fatal("unmanifested column grant was accepted")
	}
	if _, err := fixture.Owner.Exec(t.Context(), "REVOKE REFERENCES (id) ON public.families FROM baby_tracker_runtime; GRANT UPDATE ON public.families TO baby_tracker_runtime"); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 30); err == nil {
		t.Fatal("table-wide grant was accepted for a column-only manifest")
	}
}

func membershipSQL(format string) func(*testing.T, *testpg.Fixture, string) {
	return func(t *testing.T, fixture *testpg.Fixture, login string) {
		t.Helper()
		if _, err := fixture.Admin.Exec(context.Background(), fmt.Sprintf(format, pgx.Identifier{login}.Sanitize())); err != nil {
			t.Fatal(err)
		}
	}
}

func loginSQL(format string) func(*testing.T, *testpg.Fixture, string) {
	return membershipSQL(format)
}

func groupSQL(query string) func(*testing.T, *testpg.Fixture, string) {
	return func(t *testing.T, fixture *testpg.Fixture, _ string) {
		t.Helper()
		if _, err := fixture.Admin.Exec(context.Background(), query); err != nil {
			t.Fatal(err)
		}
	}
}

// The database bootstrap (init-users.sh) refuses to run when the effective
// privileges of every group role differ from the current version's privilege
// manifest. It compares function identities as PostgreSQL prints them
// (regprocedure::text), so a manifest row spelled with a type alias such as
// timestamptz can never match and the bootstrap fails on any rerun after
// migrating. Run the bootstrap's own check against a freshly migrated schema.
func TestBootstrapPrivilegeManifestCheckAcceptsTheMigratedSchema(t *testing.T) {
	fixture := testpg.New(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "postgres", "init-users.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	gate := strings.Index(script, "\\if :has_privilege_manifest\n")
	if gate < 0 {
		t.Fatal("init-users.sh no longer gates the privilege manifest check")
	}
	block := script[gate+len("\\if :has_privilege_manifest\n"):]
	end := strings.Index(block, "\n$$;\n")
	if !strings.HasPrefix(block, "DO $$") || end < 0 || !strings.Contains(block[:end], "effective privileges do not match schema privilege manifest") {
		t.Fatal("init-users.sh privilege manifest check has an unexpected shape")
	}
	tx, err := fixture.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), block[:end+len("\n$$;")]); err != nil {
		t.Fatalf("bootstrap privilege check rejected the migrated schema: %v", err)
	}
}
