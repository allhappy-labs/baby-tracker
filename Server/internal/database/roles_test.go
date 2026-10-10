package database_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"babytracker/server/internal/database"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestVerifyRuntimeRoleRequiresExactManifest(t *testing.T) {
	fixture := testpg.New(t)
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 32); err != nil {
		t.Fatalf("valid runtime role rejected: %v", err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 30); err == nil {
		t.Fatal("runtime role accepted at a stale compatible version")
	}
	if err := database.VerifyRuntimeRole(t.Context(), nil, 32); err == nil {
		t.Fatal("nil runtime pool accepted")
	}
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Maintenance, 32); err == nil {
		t.Fatal("maintenance role accepted as runtime")
	}
	if _, err := fixture.Owner.Exec(t.Context(), "GRANT UPDATE (created_at) ON public.families TO baby_tracker_runtime"); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 32); err == nil {
		t.Fatal("unmanifested runtime column grant accepted")
	}
	if _, err := fixture.Owner.Exec(t.Context(), "REVOKE UPDATE (created_at) ON public.families FROM baby_tracker_runtime; REVOKE UPDATE (status) ON public.families FROM baby_tracker_runtime"); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 32); err == nil {
		t.Fatal("manifested runtime grant missing but accepted")
	}
}

// The maintenance login holds exactly manifest 32's baby_tracker_maintenance
// rows, read through the runtime pool (the maintenance login cannot read the
// manifest), and nothing else.
func TestVerifyMaintenanceRoleRequiresExactManifest(t *testing.T) {
	fixture := testpg.New(t)
	ctx := t.Context()
	if err := database.VerifyMaintenanceRole(ctx, fixture.Runtime, fixture.Maintenance, 32); err != nil {
		t.Fatalf("valid maintenance role rejected: %v", err)
	}
	for name, call := range map[string]func() error{
		"stale version":        func() error { return database.VerifyMaintenanceRole(ctx, fixture.Runtime, fixture.Maintenance, 30) },
		"nil manifest pool":    func() error { return database.VerifyMaintenanceRole(ctx, nil, fixture.Maintenance, 32) },
		"nil maintenance pool": func() error { return database.VerifyMaintenanceRole(ctx, fixture.Runtime, nil, 32) },
		"runtime as maintenance": func() error {
			return database.VerifyMaintenanceRole(ctx, fixture.Runtime, fixture.Runtime, 32)
		},
		// The maintenance login cannot read the manifest itself.
		"maintenance reads its own manifest": func() error {
			return database.VerifyMaintenanceRole(ctx, fixture.Maintenance, fixture.Maintenance, 32)
		},
	} {
		if err := call(); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	for _, drift := range []struct{ name, apply, undo string }{
		{"extra column grant", "GRANT UPDATE (next_sequence) ON public.families TO baby_tracker_maintenance", "REVOKE UPDATE (next_sequence) ON public.families FROM baby_tracker_maintenance"},
		{"extra table grant", "GRANT INSERT ON public.care_receipts TO baby_tracker_maintenance", "REVOKE INSERT ON public.care_receipts FROM baby_tracker_maintenance"},
		{"missing table grant", "REVOKE DELETE ON public.care_receipts FROM baby_tracker_maintenance", "GRANT DELETE ON public.care_receipts TO baby_tracker_maintenance"},
		{"missing column grant", "REVOKE UPDATE (care_wire_bytes) ON public.families FROM baby_tracker_maintenance", "GRANT UPDATE (care_wire_bytes) ON public.families TO baby_tracker_maintenance"},
		{"extra function", "GRANT EXECUTE ON FUNCTION app.authority_context() TO baby_tracker_maintenance", "REVOKE EXECUTE ON FUNCTION app.authority_context() FROM baby_tracker_maintenance"},
		{"missing function", "REVOKE EXECUTE ON FUNCTION app.ensure_audit_partitions(timestamptz) FROM baby_tracker_maintenance", "GRANT EXECUTE ON FUNCTION app.ensure_audit_partitions(timestamptz) TO baby_tracker_maintenance"},
	} {
		if _, err := fixture.Owner.Exec(ctx, drift.apply); err != nil {
			t.Fatal(err)
		}
		if err := database.VerifyMaintenanceRole(ctx, fixture.Runtime, fixture.Maintenance, 32); err == nil {
			t.Fatalf("%s accepted", drift.name)
		}
		if _, err := fixture.Owner.Exec(ctx, drift.undo); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.VerifyMaintenanceRole(ctx, fixture.Runtime, fixture.Maintenance, 32); err != nil {
		t.Fatalf("maintenance role rejected after every drift was undone: %v", err)
	}
}

// A runtime whose sessions start in the authority context would turn every
// transaction into an authority transaction; the role audit refuses it.
func TestVerifyRuntimeRoleRejectsPresetAuthority(t *testing.T) {
	fixture := testpg.New(t)
	var databaseName, runtimeUser string
	if err := fixture.Runtime.QueryRow(t.Context(), "SELECT current_database(), current_user").Scan(&databaseName, &runtimeUser); err != nil {
		t.Fatal(err)
	}
	for _, setting := range []struct{ set, reset string }{
		{"ALTER DATABASE %[1]s SET app.authority = '1'", "ALTER DATABASE %[1]s RESET app.authority"},
		{"ALTER ROLE baby_tracker_runtime IN DATABASE %[1]s SET app.authority = '1'", "ALTER ROLE baby_tracker_runtime IN DATABASE %[1]s RESET app.authority"},
		{"ALTER ROLE %[2]s IN DATABASE %[1]s SET app.authority = '1'", "ALTER ROLE %[2]s IN DATABASE %[1]s RESET app.authority"},
	} {
		statement := fmt.Sprintf(setting.set, pgx.Identifier{databaseName}.Sanitize(), pgx.Identifier{runtimeUser}.Sanitize())
		t.Run(statement, func(t *testing.T) {
			if _, err := fixture.Admin.Exec(t.Context(), statement); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = fixture.Admin.Exec(context.Background(), fmt.Sprintf(setting.reset, pgx.Identifier{databaseName}.Sanitize(), pgx.Identifier{runtimeUser}.Sanitize()))
			})
			if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 32); err == nil {
				t.Fatal("preset app.authority accepted")
			}
		})
	}
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 32); err != nil {
		t.Fatalf("runtime role rejected after reset: %v", err)
	}
	// A connection option that presets the flag is visible as the session value.
	config, err := pgxpool.ParseConfig(fixture.Runtime.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["options"] = "-c app.authority=1"
	preset, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(preset.Close)
	var presetAuthority bool
	if err := preset.QueryRow(t.Context(), "SELECT app.authority_context()").Scan(&presetAuthority); err != nil || !presetAuthority {
		t.Fatalf("connection option did not preset the flag: %t %v", presetAuthority, err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), preset, 32); err == nil {
		t.Fatal("connection option app.authority accepted")
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
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 32); err == nil {
		t.Fatal("unmanifested additive grant was accepted")
	}
	if _, err := fixture.Owner.Exec(t.Context(), `INSERT INTO schema_privilege_manifest
		SELECT 33,principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=32`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Owner.Exec(t.Context(), "INSERT INTO schema_privilege_manifest VALUES (33,'baby_tracker_runtime','function',$1,'EXECUTE')", signature); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 32); err == nil {
		t.Fatal("future snapshot was accepted before the compatible schema version")
	}
	if _, err := fixture.Owner.Exec(t.Context(), "UPDATE schema_compatibility SET current_version=33 WHERE singleton"); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 33); err != nil {
		t.Fatalf("exact additive snapshot rejected: %v", err)
	}
	if _, err := fixture.Runtime.Exec(t.Context(), `INSERT INTO schema_privilege_manifest VALUES
		(10,'baby_tracker_runtime','function','app.untrusted()','EXECUTE')`); err == nil {
		t.Fatal("runtime could modify privilege manifest")
	}
	if _, err := fixture.Owner.Exec(t.Context(), "UPDATE schema_privilege_manifest SET privileges='EXECUTE,CREATE' WHERE schema_version=33 AND principal='baby_tracker_runtime' AND object_identity=$1", signature); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 33); err == nil {
		t.Fatal("forbidden privilege vocabulary was accepted")
	}
	if _, err := fixture.Owner.Exec(t.Context(), "UPDATE schema_privilege_manifest SET privileges='EXECUTE',object_identity='app.wrong_signature(text)' WHERE schema_version=33 AND principal='baby_tracker_runtime' AND object_identity=$1", signature); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 33); err == nil {
		t.Fatal("wrong function signature was accepted")
	}
	if _, err := fixture.Owner.Exec(t.Context(), "UPDATE schema_privilege_manifest SET object_identity=$1 WHERE schema_version=33 AND principal='baby_tracker_runtime' AND object_identity='app.wrong_signature(text)'", signature); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Owner.Exec(t.Context(), "REVOKE EXECUTE ON FUNCTION app.future_identity_challenge(text) FROM baby_tracker_runtime"); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 33); err == nil {
		t.Fatal("manifest row without effective grant was accepted")
	}
}

func TestVersionedPrivilegeManifestAuthorizesExactColumnGrant(t *testing.T) {
	fixture := testpg.New(t)
	if _, err := fixture.Owner.Exec(t.Context(), `INSERT INTO schema_privilege_manifest
		SELECT 33,principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=32;
		INSERT INTO schema_privilege_manifest VALUES (33,'baby_tracker_runtime','column','public.families.created_at','UPDATE');
		GRANT UPDATE (created_at) ON public.families TO baby_tracker_runtime;
		UPDATE schema_compatibility SET current_version=33 WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 33); err != nil {
		t.Fatalf("exact manifested column grant rejected: %v", err)
	}
	if _, err := fixture.Owner.Exec(t.Context(), "GRANT REFERENCES (id) ON public.families TO baby_tracker_runtime"); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 33); err == nil {
		t.Fatal("unmanifested column grant was accepted")
	}
	if _, err := fixture.Owner.Exec(t.Context(), "REVOKE REFERENCES (id) ON public.families FROM baby_tracker_runtime; GRANT UPDATE ON public.families TO baby_tracker_runtime"); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 33); err == nil {
		t.Fatal("table-wide grant was accepted for a column-only manifest")
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
