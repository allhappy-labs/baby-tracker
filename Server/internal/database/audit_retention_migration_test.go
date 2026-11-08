package database_test

import (
	"context"
	"testing"

	"babytracker/server/internal/database"
	"babytracker/server/internal/testpg"
)

// A schema-34 database with audit history upgrades in place (owner decision
// D1): its audit rows and checkpoints are untouched, the retention anchors
// table is an authority table the runtime may only read, and the drop
// function is a maintenance-only definer; the privilege manifest at 35 is
// 34's plus exactly those two grants, and both readiness audits pass.
func TestAuditRetentionMigrationOverASchema34Database(t *testing.T) {
	f := testpg.NewThrough(t, 34)
	ctx := t.Context()
	ids := seedAuthorityFamilies(t, f)
	owner := func(statement string, args ...any) {
		t.Helper()
		tx, err := f.Owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	owner(`INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
		VALUES ('account',$1,1,sha256('p'::bytea),sha256('h'::bytea),'session_granted',gen_random_uuid(),$1,now(),'{}')`, ids.accountA)
	owner(`INSERT INTO audit_checkpoints(scope,scope_id,verified_seq,verified_hash,verified_at) VALUES ('account',$1,1,sha256('h'::bytea),now())`, ids.accountA)
	counts := func() (rows, checkpoints int) {
		t.Helper()
		tx, err := f.Owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM security_audit_events), (SELECT count(*) FROM audit_checkpoints)`).Scan(&rows, &checkpoints); err != nil {
			t.Fatal(err)
		}
		return rows, checkpoints
	}
	rowsBefore, checkpointsBefore := counts()

	if err := f.MigrateThrough(ctx, 35); err != nil {
		t.Fatalf("000035 over a schema-34 database: %v", err)
	}
	if rows, checkpoints := counts(); rows != rowsBefore || checkpoints != checkpointsBefore || rows == 0 {
		t.Fatalf("audit rows %d -> %d, checkpoints %d -> %d", rowsBefore, rows, checkpointsBefore, checkpoints)
	}
	var version int64
	var introducedBy string
	if err := f.Owner.QueryRow(ctx, `SELECT current_version, introduced_by FROM schema_compatibility`).Scan(&version, &introducedBy); err != nil {
		t.Fatal(err)
	}
	if version != 35 || introducedBy != "000035_audit_retention.sql" {
		t.Fatalf("compatibility %d %q", version, introducedBy)
	}
	var at34, at35, added int
	if err := f.Owner.QueryRow(ctx, `SELECT count(*) FILTER (WHERE schema_version=34), count(*) FILTER (WHERE schema_version=35),
			count(*) FILTER (WHERE schema_version=35 AND ((principal,object_kind,object_identity,privileges) IN
				(('baby_tracker_runtime','table','public.audit_retention_anchors','SELECT'),
				 ('baby_tracker_maintenance','function','app.drop_expired_audit_partition(timestamp with time zone)','EXECUTE'))))
		FROM schema_privilege_manifest`).Scan(&at34, &at35, &added); err != nil {
		t.Fatal(err)
	}
	if at34 == 0 || at35 != at34+2 || added != 2 {
		t.Fatalf("privilege manifest rows: %d at 34, %d at 35 (%d new), want 34's plus the two grants", at34, at35, added)
	}

	var forced, runtimeReads, runtimeWrites, maintenanceTouches, maintenanceRuns, othersRun bool
	if err := f.Owner.QueryRow(ctx, `SELECT c.relrowsecurity AND c.relforcerowsecurity,
			has_table_privilege('baby_tracker_runtime','public.audit_retention_anchors','SELECT'),
			has_table_privilege('baby_tracker_runtime','public.audit_retention_anchors','INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER'),
			has_table_privilege('baby_tracker_maintenance','public.audit_retention_anchors','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER'),
			has_function_privilege('baby_tracker_maintenance','app.drop_expired_audit_partition(timestamp with time zone)','EXECUTE'),
			has_function_privilege('baby_tracker_runtime','app.drop_expired_audit_partition(timestamp with time zone)','EXECUTE')
				OR has_function_privilege('public','app.drop_expired_audit_partition(timestamp with time zone)','EXECUTE')
		FROM pg_class c WHERE c.oid='public.audit_retention_anchors'::regclass`).
		Scan(&forced, &runtimeReads, &runtimeWrites, &maintenanceTouches, &maintenanceRuns, &othersRun); err != nil {
		t.Fatal(err)
	}
	if !forced || !runtimeReads || runtimeWrites || maintenanceTouches || !maintenanceRuns || othersRun {
		t.Fatalf("forced RLS %t, runtime reads %t writes %t, maintenance touches %t runs %t, others run %t",
			forced, runtimeReads, runtimeWrites, maintenanceTouches, maintenanceRuns, othersRun)
	}
	// The policy admits authority transactions only.
	owner(`INSERT INTO audit_retention_anchors(scope,scope_id,anchor_seq,anchor_hash,anchored_at,dropped_before) VALUES ('account',$1,1,sha256('h'::bytea),now(),now())`, ids.accountA)
	for _, authority := range []bool{false, true} {
		tx, err := f.Runtime.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if authority {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
				t.Fatal(err)
			}
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_retention_anchors`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		_ = tx.Rollback(ctx)
		if want := map[bool]int{false: 0, true: 1}[authority]; n != want {
			t.Fatalf("runtime with authority=%t reads %d anchors, want %d", authority, n, want)
		}
	}
	// The two schema-35 checks check-principals.sh and smoke.sh run hold.
	for _, probe := range []string{
		`SELECT has_function_privilege('baby_tracker_maintenance',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_runtime',f,'EXECUTE') FROM (SELECT 'app.drop_expired_audit_partition(timestamp with time zone)' AS f) d`,
		`SELECT has_table_privilege('baby_tracker_runtime','public.audit_retention_anchors','SELECT') AND NOT has_table_privilege('baby_tracker_runtime','public.audit_retention_anchors','INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') AND NOT bool_or(has_table_privilege(r,'public.audit_retention_anchors','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public','baby_tracker_maintenance']) r`,
	} {
		var held bool
		if err := f.Owner.QueryRow(ctx, probe).Scan(&held); err != nil || !held {
			t.Fatalf("probe %s = %t (%v)", probe, held, err)
		}
	}
	if err := database.VerifyRuntimeRole(ctx, f.Runtime, 35); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyMaintenanceRole(ctx, f.Runtime, f.Maintenance, 35); err != nil {
		t.Fatal(err)
	}
}
