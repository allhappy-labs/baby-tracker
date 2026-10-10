package database_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Schema 31 (Phase 3, 000031): the request path, retention and wakes.

func TestSchemaIsVersionThirtyOne(t *testing.T) {
	f := testpg.New(t)
	var v int64
	if err := f.Runtime.QueryRow(t.Context(), `SELECT current_version FROM schema_compatibility WHERE singleton`).Scan(&v); err != nil || v != 32 {
		t.Fatalf("version=%d err=%v", v, err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), f.Runtime, 32); err != nil {
		t.Fatal(err)
	}
	if err := database.VerifyMaintenanceRole(t.Context(), f.Runtime, f.Maintenance, 32); err != nil {
		t.Fatal(err)
	}
	var gone bool
	if err := f.Owner.QueryRow(t.Context(), `SELECT to_regclass('public.rate_limit_buckets') IS NULL
		AND to_regprocedure('app.take_rate_limits(jsonb,timestamptz)') IS NULL`).Scan(&gone); err != nil || !gone {
		t.Fatalf("rate-limit storage survived 000031 (%v)", err)
	}
}

func TestMaintenanceGrantsAreExact(t *testing.T) {
	f := testpg.New(t)
	for _, c := range []struct {
		sql  string
		want bool
	}{
		{`bool_and(has_table_privilege(current_user,t,'DELETE')) FROM unnest(ARRAY['public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t`, true},
		// Care rows and import staging only through app.purge_family_step
		// (Task 2 re-review m1): no DELETE of its own.
		{`has_table_privilege(current_user,'public.care_entities','DELETE') OR has_table_privilege(current_user,'public.import_sessions','DELETE')`, false},
		{`has_function_privilege(current_user,'app.purge_family_step(uuid,integer)','EXECUTE')`, true},
		// Whole rows only where the rows are metadata (receipts carry the
		// outcome the release recomputes); care rows and import sessions
		// through their keys only (the column lists below).
		{`bool_and(has_table_privilege(current_user,t,'SELECT')) FROM unnest(ARRAY['public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t`, true},
		{`has_table_privilege(current_user,'public.care_entities','SELECT') OR has_table_privilege(current_user,'public.import_sessions','SELECT')`, false},
		{`bool_or(has_table_privilege(current_user,t,'INSERT') OR has_table_privilege(current_user,t,'UPDATE') OR has_table_privilege(current_user,t,'TRUNCATE')) FROM unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions','public.import_sessions']) t`, false},
		{`bool_and(has_column_privilege(current_user,'public.families',c,'UPDATE')) FROM unnest(ARRAY['care_record_count','care_charged_bytes','care_entity_count','care_wire_bytes']) c`, true},
		{`has_column_privilege(current_user,'public.families','status','UPDATE') OR has_column_privilege(current_user,'public.families','next_sequence','UPDATE') OR has_table_privilege(current_user,'public.families','DELETE')`, false},
		{`has_table_privilege(current_user,'public.device_sync_cursors','SELECT') AND has_table_privilege(current_user,'public.device_sync_cursors','INSERT') AND has_table_privilege(current_user,'public.device_sync_cursors','UPDATE') AND has_table_privilege(current_user,'public.device_sync_cursors','DELETE')`, true},
		{`has_function_privilege(current_user,'app.ensure_audit_partitions(timestamp with time zone)','EXECUTE')`, true},
		{`has_function_privilege(current_user,'app.push_targets(uuid[],uuid[])','EXECUTE') AND has_function_privilege(current_user,'app.prune_push_tokens(integer)','EXECUTE') AND has_function_privilege(current_user,'app.forget_push_token(uuid,bytea)','EXECUTE')`, true},
		// Tokens only through the three definers: no direct privilege at all.
		{`has_table_privilege(current_user,'public.device_push_tokens','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR has_any_column_privilege(current_user,'public.device_push_tokens','SELECT,INSERT,UPDATE,REFERENCES')`, false},
	} {
		var got bool
		if err := f.Maintenance.QueryRow(t.Context(), `SELECT `+c.sql).Scan(&got); err != nil || got != c.want {
			t.Fatalf("maintenance: %s = %v (%v)", c.sql, got, err)
		}
	}
	for _, c := range []string{
		`has_table_privilege(current_user,'public.device_sync_cursors','SELECT')`,
		`has_function_privilege(current_user,'app.ensure_audit_partitions(timestamp with time zone)','EXECUTE')`,
		`has_function_privilege(current_user,'app.push_targets(uuid[],uuid[])','EXECUTE')`,
		`has_function_privilege(current_user,'app.prune_push_tokens(integer)','EXECUTE')`,
		`has_function_privilege(current_user,'app.forget_push_token(uuid,bytea)','EXECUTE')`,
		`has_function_privilege(current_user,'app.purge_family_step(uuid,integer)','EXECUTE')`,
		`bool_or(has_table_privilege(current_user,t,'DELETE')) FROM unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t`,
	} {
		var got bool
		if err := f.Runtime.QueryRow(t.Context(), `SELECT `+c).Scan(&got); err != nil || got {
			t.Fatalf("runtime: %s = %v (%v)", c, got, err)
		}
	}
	// Exact column grants: the runtime updates three push-token columns; the
	// maintenance group reads no push-token column, the keys of care rows and
	// import sessions, and on families seven columns, updating four.
	for _, c := range []struct {
		pool             *pgxpool.Pool
		table, privilege string
		want             string
	}{
		{f.Runtime, "device_push_tokens", "UPDATE", "environment,token,registered_at"},
		{f.Maintenance, "device_push_tokens", "SELECT", ""},
		{f.Maintenance, "care_entities", "SELECT", "family_id,id,kind"},
		{f.Maintenance, "import_sessions", "SELECT", "family_id,import_id"},
		{f.Maintenance, "families", "SELECT", "id,status,next_sequence,care_record_count,care_charged_bytes,care_entity_count,care_wire_bytes"},
		{f.Maintenance, "families", "UPDATE", "care_record_count,care_charged_bytes,care_entity_count,care_wire_bytes"},
	} {
		var columns string
		if err := c.pool.QueryRow(t.Context(), `SELECT coalesce(string_agg(attname, ',' ORDER BY attnum), '') FROM pg_attribute
			WHERE attrelid=$1::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege(current_user, attrelid, attnum, $2)`,
			"public."+c.table, c.privilege).Scan(&columns); err != nil || columns != c.want {
			t.Fatalf("%s %s columns %q (%v), want %q", c.table, c.privilege, columns, err, c.want)
		}
	}
	// The maintenance group executes exactly its five definers, and no other
	// principal (PUBLIC included) executes any of them.
	maintenanceFunctions := []string{
		"app.ensure_audit_partitions(timestamp with time zone)", "app.forget_push_token(uuid,bytea)", "app.prune_push_tokens(integer)",
		"app.purge_family_step(uuid,integer)", "app.push_targets(uuid[],uuid[])",
	}
	// Beside them it executes only the nine Live Activity worker definers
	// (000032, live_activity_schema_test.go).
	assertExactAppExecutables(t, f, "baby_tracker_maintenance", append(append([]string(nil), maintenanceFunctions...), liveActivityWorkerFunctions...))
	for _, signature := range maintenanceFunctions {
		assertPublicCannotExecute(t, f, signature)
		assertFunctionPrivilege(t, f.Runtime, signature, false)
	}
	// The five definers pin search_path; the partition roller also pins UTC
	// and bounds its own lock waits.
	rows, err := f.Owner.Query(t.Context(), `SELECT p.oid::regprocedure::text, p.prosecdef, coalesce(array_to_string(p.proconfig, ','), '')
		FROM pg_proc p WHERE p.oid = ANY($1::regprocedure[]) ORDER BY 1`, maintenanceFunctions)
	if err != nil {
		t.Fatal(err)
	}
	type definer struct {
		name     string
		definer  bool
		settings string
	}
	got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (definer, error) {
		var d definer
		return d, row.Scan(&d.name, &d.definer, &d.settings)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []definer{
		{"app.ensure_audit_partitions(timestamp with time zone)", true, "search_path=pg_catalog,TimeZone=UTC,lock_timeout=1s"},
		{"app.forget_push_token(uuid,bytea)", true, "search_path=pg_catalog"},
		{"app.prune_push_tokens(integer)", true, "search_path=pg_catalog"},
		{"app.purge_family_step(uuid,integer)", true, "search_path=pg_catalog"},
		{"app.push_targets(uuid[],uuid[])", true, "search_path=pg_catalog"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("definers %+v, want %+v", got, want)
	}
}

// The maintenance worker's own statements (Tasks 8, 10, 11 and 15) run on the
// maintenance login under forced RLS: it sees the care tables, cursors and
// family rows whole, releases charges through the four counters, and is
// refused everything else.
func TestMaintenanceRunsTheWorkerStatements(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f)
	ctx := t.Context()
	if err := authorityExec(ctx, f.Owner, `UPDATE families SET next_sequence=5 WHERE id=$1`, ids.familyA); err != nil {
		t.Fatal(err)
	}
	tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	receiptDevice, receiptMutation := randomUUID(t), randomUUID(t)
	if _, err := tx.Exec(ctx, `INSERT INTO care_receipts(device_id,mutation_id,family_id,account_id,semantic_hash,outcome,accepted_at)
		VALUES ($1,$2,$3,$4,$5,'{"status":"applied"}',now() - interval '31 days')`, receiptDevice, receiptMutation, ids.familyA, ids.accountA, authorityDigest(0x31)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE families SET care_charged_bytes=100 WHERE id=$1`, ids.familyA); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	btx := tenantTx(t, f.Runtime, ids.accountB, ids.familyB)
	if _, err := btx.Exec(ctx, `INSERT INTO care_entities(family_id,id,kind,entity_version,changed_seq,creator_account_id,actor_account_id,device_id,operation,accepted_at,sort_order,name_hash,wire)
		VALUES ($1,$2,1,1,1,$3,$3,$4,'create',now(),0,$5,convert_to('{"name":"secret","note":"' || repeat('x',64) || '"}','UTF8'))`, ids.familyB, randomUUID(t), ids.accountB, randomUUID(t), randomDigest(t)); err != nil {
		t.Fatal(err)
	}
	if err := btx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	m := f.Maintenance
	var families int
	if err := m.QueryRow(ctx, `SELECT count(*) FROM families`).Scan(&families); err != nil || families != 2 {
		t.Fatalf("maintenance sees %d families (%v), want both", families, err)
	}
	// Task 8: flush (inactive families' progress dropped), lag scan, mark.
	flushed, err := m.Exec(ctx, `INSERT INTO device_sync_cursors(family_id,device_id,acked_sequence,delivered_sequence,seen_at)
		SELECT v.f, v.d, v.a, v.dl, v.s FROM unnest($1::uuid[],$2::uuid[],$3::bigint[],$4::bigint[],$5::timestamptz[]) AS v(f,d,a,dl,s)
		JOIN families fam ON fam.id = v.f AND fam.status = 'active'
		ON CONFLICT (family_id, device_id) DO UPDATE SET acked_sequence = greatest(device_sync_cursors.acked_sequence, EXCLUDED.acked_sequence),
		  delivered_sequence = greatest(device_sync_cursors.delivered_sequence, EXCLUDED.delivered_sequence),
		  seen_at = greatest(device_sync_cursors.seen_at, EXCLUDED.seen_at)`,
		[]string{ids.familyA, ids.familyB}, []string{ids.deviceA, ids.deviceA}, []int64{2, 0}, []int64{2, 0}, []time.Time{time.Now(), time.Now()})
	if err != nil || flushed.RowsAffected() != 2 {
		t.Fatalf("flush %d rows (%v)", flushed.RowsAffected(), err)
	}
	var lagging int
	if err := m.QueryRow(ctx, `SELECT count(*) FROM (SELECT d.family_id::text, d.device_id::text, f.next_sequence, d.pushed_sequence, d.pushed_at
		FROM device_sync_cursors d JOIN families f ON f.id = d.family_id
		WHERE d.family_id = ANY($1::uuid[]) AND f.status = 'active'
		  AND f.next_sequence > greatest(d.acked_sequence, d.delivered_sequence, d.woken_sequence)
		  AND (d.woken_at IS NULL OR d.woken_at <= $2)
		ORDER BY d.family_id, d.device_id LIMIT $3) lag`, []string{ids.familyA, ids.familyB}, time.Now(), 500).Scan(&lagging); err != nil || lagging != 1 {
		t.Fatalf("lag scan found %d (%v), want family A's device", lagging, err)
	}
	for _, mark := range []string{
		`UPDATE device_sync_cursors d SET woken_sequence = v.seq, woken_at = $1
		 FROM unnest($2::uuid[],$3::uuid[],$4::bigint[]) AS v(f,dv,seq) WHERE d.family_id = v.f AND d.device_id = v.dv`,
		`UPDATE device_sync_cursors d SET pushed_sequence = v.seq, pushed_at = $1
		 FROM unnest($2::uuid[],$3::uuid[],$4::bigint[]) AS v(f,dv,seq) WHERE d.family_id = v.f AND d.device_id = v.dv`,
	} {
		if tag, err := m.Exec(ctx, mark, time.Now(), []string{ids.familyA}, []string{ids.deviceA}, []int64{5}); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("mark %d (%v)", tag.RowsAffected(), err)
		}
	}
	// B is being purged (Task 11 below).
	if err := authorityExec(ctx, f.Owner, `UPDATE families SET status='purge_started', purge_started_at=now(), purge_command_id=$2 WHERE id=$1`,
		ids.familyB, randomUUID(t)); err != nil {
		t.Fatal(err)
	}
	// Task 10: lock the families (never an account row), delete the receipts
	// past the window, release their charge.
	wtx, err := m.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer wtx.Rollback(context.Background())
	var locked int
	if err := wtx.QueryRow(ctx, `SELECT count(*) FROM (SELECT id FROM families WHERE id = ANY($1::uuid[]) ORDER BY id FOR NO KEY UPDATE SKIP LOCKED) l`,
		[]string{ids.familyA, ids.familyB}).Scan(&locked); err != nil || locked != 2 {
		t.Fatalf("family lock %d (%v)", locked, err)
	}
	deleted, err := wtx.Exec(ctx, `DELETE FROM care_receipts r WHERE (r.device_id, r.mutation_id) IN (
		SELECT device_id, mutation_id FROM care_receipts WHERE family_id = ANY($3::uuid[]) AND accepted_at <= $1 ORDER BY accepted_at LIMIT $2)
		RETURNING r.family_id::text, r.outcome::text`, time.Now().Add(-30*24*time.Hour), 2000, []string{ids.familyA})
	if err != nil || deleted.RowsAffected() != 1 {
		t.Fatalf("receipt sweep deleted %d (%v)", deleted.RowsAffected(), err)
	}
	if _, err := wtx.Exec(ctx, `UPDATE families f SET care_charged_bytes = f.care_charged_bytes - v.bytes
		FROM unnest($1::uuid[], $2::bigint[]) AS v(id, bytes) WHERE f.id = v.id`, []string{ids.familyA}, []int64{40}); err != nil {
		t.Fatal(err)
	}
	// Task 11: every purge step of a purge_started family (B), then the
	// counter reset. The care rows and import staging go only through
	// app.purge_family_step, which refuses an active family (A).
	for _, step := range []string{
		`DELETE FROM care_receipts WHERE ctid = ANY(ARRAY(SELECT ctid FROM care_receipts WHERE family_id = $1 LIMIT 7))`,
		`DELETE FROM sync_page_bindings WHERE ctid = ANY(ARRAY(SELECT ctid FROM sync_page_bindings WHERE family_id = $1 LIMIT 7))`,
		`DELETE FROM sync_snapshot_sessions WHERE ctid = ANY(ARRAY(SELECT ctid FROM sync_snapshot_sessions WHERE family_id = $1 LIMIT 7))`,
		`DELETE FROM device_sync_cursors WHERE family_id = $1 AND device_id = ANY(ARRAY(SELECT device_id FROM device_sync_cursors WHERE family_id = $1 ORDER BY device_id LIMIT 7 FOR UPDATE))`,
	} {
		if _, err := wtx.Exec(ctx, step, ids.familyB); err != nil {
			t.Fatalf("purge step %q: %v", step, err)
		}
	}
	// The caller's context is preset (review m2): the definer must restore
	// all three settings it changes, after a step and after a refusal.
	contextOf := func() string {
		var got string
		if err := wtx.QueryRow(ctx, `SELECT coalesce(current_setting('app.family_id', true), '') || '|' || coalesce(current_setting('app.account_id', true), '')
			|| '|' || coalesce(current_setting('app.authority', true), '')`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if _, err := wtx.Exec(ctx, `SELECT set_config('app.family_id', $1, true), set_config('app.account_id', $2, true), set_config('app.authority', 'preset', true)`,
		ids.familyA, ids.accountA); err != nil {
		t.Fatal(err)
	}
	preset := contextOf()
	var purgeSteps []string
	for range 10 {
		var step string
		var deleted int
		if err := wtx.QueryRow(ctx, `SELECT step, deleted FROM app.purge_family_step($1, 7)`, ids.familyB).Scan(&step, &deleted); err != nil {
			t.Fatalf("purge_family_step: %v", err)
		}
		if got := contextOf(); got != preset {
			t.Fatalf("context after the purge step %s: %q, want the caller's %q", step, got, preset)
		}
		purgeSteps = append(purgeSteps, fmt.Sprintf("%s:%d", step, deleted))
		if step == "none" {
			break
		}
	}
	// B's baby, then its member names (what the seed gave it), then nothing.
	got := strings.Join(purgeSteps, ",")
	rest, found := strings.CutPrefix(got, "babies:1,")
	order := []string{"label_history", "labels", "memberships", "recovery_claim", "none"}
	for _, step := range strings.Split(rest, ",") {
		name, _, _ := strings.Cut(step, ":")
		for len(order) > 0 && order[0] != name {
			order = order[1:]
		}
		if len(order) == 0 {
			found = false
		}
	}
	if !found || !strings.HasSuffix(got, ",none:0") || !strings.Contains(got, "memberships:") {
		t.Fatalf("purge steps %s, want B's baby, then its label history, labels, memberships and recovery claim, then nothing", got)
	}
	savepoint := func(sql string) {
		if _, err := wtx.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	savepoint(`SAVEPOINT refused`)
	_, err = wtx.Exec(ctx, `SELECT * FROM app.purge_family_step($1, 7)`, ids.familyA)
	assertSQLState(t, err, "55000")
	savepoint(`ROLLBACK TO SAVEPOINT refused`)
	if got := contextOf(); got != preset {
		t.Fatalf("context after a refused purge step: %q, want the caller's %q", got, preset)
	}
	if _, err := wtx.Exec(ctx, `SELECT set_config('app.family_id', '', true), set_config('app.account_id', '', true), set_config('app.authority', '', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := wtx.Exec(ctx, `UPDATE families SET care_record_count=0, care_charged_bytes=0, care_entity_count=0, care_wire_bytes=0 WHERE id=$1`, ids.familyB); err != nil {
		t.Fatal(err)
	}
	// Task 10: the TTL sweeps (no row lock: the maintenance group holds no UPDATE on these tables).
	for _, sweep := range []string{
		`DELETE FROM sync_page_bindings WHERE (family_id, token_digest) IN (SELECT family_id, token_digest FROM sync_page_bindings WHERE expires_at <= $1 ORDER BY expires_at LIMIT $2)`,
		`DELETE FROM sync_snapshot_sessions WHERE (family_id, id) IN (SELECT family_id, id FROM sync_snapshot_sessions WHERE expires_at <= $1 ORDER BY expires_at LIMIT $2)`,
		`DELETE FROM device_sync_cursors WHERE (family_id, device_id) IN (SELECT family_id, device_id FROM device_sync_cursors WHERE seen_at <= $1 ORDER BY seen_at LIMIT $2)`,
	} {
		if _, err := wtx.Exec(ctx, sweep, time.Now().Add(-time.Hour), 5000); err != nil {
			t.Fatalf("sweep %q: %v", sweep, err)
		}
	}
	if err := wtx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var charged int64
	var cursors, careLeft int
	if err := m.QueryRow(ctx, `SELECT (SELECT care_charged_bytes FROM families WHERE id=$1), (SELECT count(*) FROM device_sync_cursors),
		(SELECT count(*) FROM care_entities WHERE family_id=$2)`, ids.familyA, ids.familyB).Scan(&charged, &cursors, &careLeft); err != nil || charged != 60 || cursors != 1 || careLeft != 0 {
		t.Fatalf("charged=%d cursors=%d care rows left=%d (%v), want 60, family A's cursor and none", charged, cursors, careLeft, err)
	}
	// The purge step refuses an active family, and a family that is not
	// there.
	for _, family := range []string{ids.familyA, randomUUID(t)} {
		_, err := m.Exec(ctx, `SELECT * FROM app.purge_family_step($1, 7)`, family)
		assertSQLState(t, err, "55000")
	}
	// Refused: authority columns, other family columns, care inserts and
	// updates, the push-token columns it does not need, and the runtime on
	// the cursors.
	for _, c := range []struct {
		pool *pgxpool.Pool
		sql  string
	}{
		{m, `UPDATE families SET status='deletion_pending' WHERE id='` + ids.familyA + `'`},
		{m, `UPDATE families SET next_sequence=next_sequence+1 WHERE id='` + ids.familyA + `'`},
		{m, `SELECT display_name FROM families`},
		{m, `DELETE FROM families`},
		{m, `UPDATE care_receipts SET accepted_at=now()`},
		{m, `UPDATE sync_page_bindings SET expires_at=now()`},
		{m, `INSERT INTO care_receipts(device_id,mutation_id,family_id,account_id,semantic_hash,outcome,accepted_at) SELECT device_id,mutation_id,family_id,account_id,semantic_hash,outcome,accepted_at FROM care_receipts`},
		{m, `TRUNCATE care_entities`},
		{m, `DELETE FROM care_entities`},
		{m, `DELETE FROM import_sessions`},
		{f.Runtime, `SELECT * FROM app.purge_family_step('` + ids.familyB + `', 1)`},
		{m, `SELECT account_id FROM device_push_tokens`},
		{m, `SELECT device_id, token FROM device_push_tokens`},
		{m, `DELETE FROM device_push_tokens`},
		{m, `UPDATE device_push_tokens SET token=token`},
		// No care payload: the maintenance login reads care rows and import
		// sessions through their keys only.
		{m, `SELECT wire FROM care_entities`},
		{m, `SELECT label, name_hash FROM care_entities`},
		{m, `SELECT * FROM care_entities`},
		{m, `SELECT initiating_account_id FROM import_sessions`},
		{m, `SELECT * FROM import_sessions`},
		{m, `SELECT 1 FROM sessions`},
		{m, `SELECT 1 FROM security_audit_events`},
		{f.Runtime, `SELECT 1 FROM device_sync_cursors`},
		{f.Runtime, `DELETE FROM care_receipts`},
	} {
		_, err := c.pool.Exec(ctx, c.sql)
		assertSQLState(t, err, "42501")
	}
}

// Every policy 000031 creates, with its roles, and every policy that names
// the maintenance group: the maintenance policies admit the maintenance group
// only (whole-table, as export_worker does, 000019); the push-token tenant
// policy admits a tenant to its own account's rows, and the authority policy
// admits only the migration owner that owns the push-token definers, not the
// runtime's authority transactions.
func TestRequestPathPoliciesAreExact(t *testing.T) {
	f := testpg.New(t)
	type policy struct{ table, name, command, roles, using, check string }
	const m = "{baby_tracker_maintenance}"
	var owner string // the migration owner, which owns the push-token definers
	if err := f.Owner.QueryRow(t.Context(), `SELECT current_user`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	want := []policy{
		{"care_entities", "maintenance_read", "SELECT", m, "true", ""},
		{"care_receipts", "maintenance_delete", "DELETE", m, "true", ""},
		{"care_receipts", "maintenance_read", "SELECT", m, "true", ""},
		{"device_push_tokens", "push_token_authority", "ALL", "{" + owner + "}", "app.authority_context()", "app.authority_context()"},
		{"device_push_tokens", "push_token_owner", "ALL", "{public}", "(account_id = app.current_account_id())", "(account_id = app.current_account_id())"},
		{"device_sync_cursors", "wake_worker", "ALL", m, "true", "true"},
		{"export_chunks", "export_worker", "ALL", m, "true", "true"}, // 000019
		{"export_jobs", "export_worker", "ALL", m, "true", "true"},
		{"export_rate_reservations", "export_worker", "ALL", m, "true", "true"},
		{"families", "maintenance_counters", "UPDATE", m, "true", "true"},
		{"families", "maintenance_read", "SELECT", m, "true", ""},
		{"import_sessions", "maintenance_read", "SELECT", m, "true", ""},
		{"sync_page_bindings", "maintenance_delete", "DELETE", m, "true", ""},
		{"sync_page_bindings", "maintenance_read", "SELECT", m, "true", ""},
		{"sync_snapshot_sessions", "maintenance_delete", "DELETE", m, "true", ""},
		{"sync_snapshot_sessions", "maintenance_read", "SELECT", m, "true", ""},
	}
	rows, err := f.Owner.Query(t.Context(), `SELECT tablename::text, policyname::text, cmd, roles::text, coalesce(qual,''), coalesce(with_check,'')
		FROM pg_policies WHERE schemaname='public' AND (tablename IN ('device_push_tokens','device_sync_cursors') OR 'baby_tracker_maintenance' = ANY(roles))
		ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (policy, error) {
		var p policy
		return p, row.Scan(&p.table, &p.name, &p.command, &p.roles, &p.using, &p.check)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("policies:\n got %v\nwant %v", got, want)
	}
	var unforced []string
	rows, err = f.Owner.Query(t.Context(), `SELECT relname::text FROM pg_class WHERE oid IN ('public.device_push_tokens'::regclass, 'public.device_sync_cursors'::regclass)
		AND NOT (relrowsecurity AND relforcerowsecurity)`)
	if err != nil {
		t.Fatal(err)
	}
	if unforced, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(unforced) != 0 {
		t.Fatalf("tables without forced RLS: %v (%v)", unforced, err)
	}
}

// device_sync_cursors holds only possible progress: a real device, JSON-safe
// sequences and finite times, in an existing family (and goes with it).
func TestDeviceSyncCursorsRefuseImpossibleRows(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f)
	ctx := t.Context()
	insert := `INSERT INTO device_sync_cursors(family_id,device_id,acked_sequence,delivered_sequence,woken_sequence,seen_at,woken_at,pushed_sequence,pushed_at)
		VALUES ($1,$2,$3,$4,$5,$6::text::timestamptz,$7::text::timestamptz,$8,$9::text::timestamptz)`
	type row struct {
		family, device                  string
		acked, delivered, woken, pushed int64
		seen, wokenAt, pushedAt         any
	}
	valid := row{ids.familyA, ids.deviceA, 1, 2, 3, 4, "now", "now", "now"}
	const unsafe = 9007199254740992
	for name, c := range map[string]struct {
		mutate func(*row)
		state  string
	}{
		"zero device":       {func(r *row) { r.device = "00000000-0000-0000-0000-000000000000" }, "23514"},
		"negative acked":    {func(r *row) { r.acked = -1 }, "23514"},
		"unsafe delivered":  {func(r *row) { r.delivered = unsafe }, "23514"},
		"negative woken":    {func(r *row) { r.woken = -1 }, "23514"},
		"unsafe pushed":     {func(r *row) { r.pushed = unsafe }, "23514"},
		"infinite seen":     {func(r *row) { r.seen = "infinity" }, "23514"},
		"missing seen":      {func(r *row) { r.seen = nil }, "23502"},
		"infinite woken_at": {func(r *row) { r.wokenAt = "-infinity" }, "23514"},
		"infinite pushed":   {func(r *row) { r.pushedAt = "infinity" }, "23514"},
		"unknown family":    {func(r *row) { r.family = "62000000-0000-4000-8000-0000000000ff" }, "23503"},
	} {
		r := valid
		c.mutate(&r)
		_, err := f.Maintenance.Exec(ctx, insert, r.family, r.device, r.acked, r.delivered, r.woken, r.seen, r.wokenAt, r.pushed, r.pushedAt)
		if pgErr := (*pgconn.PgError)(nil); !errors.As(err, &pgErr) || pgErr.Code != c.state {
			t.Fatalf("%s: %v, want SQLSTATE %s", name, err, c.state)
		}
	}
	if _, err := f.Maintenance.Exec(ctx, insert, valid.family, valid.device, valid.acked, valid.delivered, valid.woken, valid.seen, valid.wokenAt, valid.pushed, valid.pushedAt); err != nil {
		t.Fatalf("a valid cursor: %v", err)
	}
	// A family with nothing else attached, so its row can go.
	familyC := randomUUID(t)
	if err := authorityExec(ctx, f.Owner, `INSERT INTO families(id,display_name,status,owner_account_id) VALUES ($1,'Family C','active',$2)`, familyC, ids.accountA); err != nil {
		t.Fatal(err)
	}
	var defaults string
	if err := f.Maintenance.QueryRow(ctx, `INSERT INTO device_sync_cursors(family_id,device_id,acked_sequence,delivered_sequence,seen_at)
		VALUES ($1,$2,0,0,now()) RETURNING woken_sequence::text || ',' || pushed_sequence::text || ',' || (woken_at IS NULL)::text || ',' || (pushed_at IS NULL)::text`,
		familyC, ids.deviceA).Scan(&defaults); err != nil || defaults != "0,0,true,true" {
		t.Fatalf("defaults %q (%v)", defaults, err)
	}
	// The family's row takes its cursors with it (the purge deletes them first;
	// this is the backstop).
	if err := authorityExec(ctx, f.Owner, `DELETE FROM families WHERE id=$1`, familyC); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := f.Maintenance.QueryRow(ctx, `SELECT count(*) FROM device_sync_cursors WHERE family_id=$1`, familyC).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d cursors outlived their family (%v)", left, err)
	}
}

func TestRetentionIndexesNameTheirSweeps(t *testing.T) {
	f := testpg.New(t)
	for index, want := range map[string]string{
		"refresh_receipts_expiry_idx":  "(LEAST(refresh_expires_at, effective_deadline))",
		"sessions_revoked_idx":         "(revoked_at) WHERE (status = 'revoked'",
		"sessions_expired_idx":         "(refresh_expires_at) WHERE (status = 'active'",
		"device_sync_cursors_seen_idx": "(seen_at)",
	} {
		var def string
		if err := f.Owner.QueryRow(t.Context(), `SELECT pg_get_indexdef($1::regclass)`, "public."+index).Scan(&def); err != nil || !strings.Contains(def, want) {
			t.Fatalf("%s: %q (%v), want %q", index, def, err, want)
		}
	}
	// No index leads with sync_snapshot_sessions.expires_at: it would compete
	// with the request path's snapshot admission count under generic plans
	// (sync's TestSyncStatementsUseOnePartitionAndIndexes pins that plan).
	var competing []string
	rows, err := f.Owner.Query(t.Context(), `SELECT indexrelid::regclass::text FROM pg_index
		WHERE indrelid='public.sync_snapshot_sessions'::regclass AND indkey[0] = (SELECT attnum FROM pg_attribute
		  WHERE attrelid='public.sync_snapshot_sessions'::regclass AND attname='expires_at')`)
	if err != nil {
		t.Fatal(err)
	}
	if competing, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(competing) != 0 {
		t.Fatalf("snapshot-session indexes leading with expires_at: %v (%v)", competing, err)
	}
}

// Each retention index serves its sweep's statement as the sweep's own login
// runs it, under forced RLS (a non-leakproof qual cannot use an index there).
func TestRetentionIndexesServeTheirSweepsUnderRLS(t *testing.T) {
	f := testpg.New(t)
	for _, c := range []struct {
		index     string
		pool      *pgxpool.Pool
		authority bool
		query     string
	}{
		{"refresh_receipts_expiry_idx", f.Runtime, true, `SELECT parent_id FROM refresh_receipts WHERE LEAST(refresh_expires_at, effective_deadline) <= $1
			ORDER BY LEAST(refresh_expires_at, effective_deadline) LIMIT 100`},
		{"sessions_revoked_idx", f.Runtime, true, `SELECT s.id FROM sessions s WHERE s.status = 'revoked' AND s.revoked_at < $1 ORDER BY s.revoked_at LIMIT 100`},
		{"sessions_expired_idx", f.Runtime, true, `SELECT s.id FROM sessions s WHERE s.status = 'active' AND s.refresh_expires_at < $1 ORDER BY s.refresh_expires_at LIMIT 100`},
		{"device_sync_cursors_seen_idx", f.Maintenance, false, `SELECT family_id, device_id FROM device_sync_cursors WHERE seen_at <= $1 ORDER BY seen_at LIMIT 100`},
	} {
		t.Run(c.index, func(t *testing.T) {
			tx, err := c.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if c.authority {
				if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.Exec(t.Context(), `SET LOCAL enable_seqscan = off`); err != nil {
				t.Fatal(err)
			}
			rows, err := tx.Query(t.Context(), `EXPLAIN (COSTS OFF) `+c.query, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			if plan := strings.Join(lines, "\n"); !strings.Contains(plan, c.index) {
				t.Fatalf("plan does not use %s:\n%s", c.index, plan)
			}
		})
	}
}

func TestAuditPartitionsAreMonthlyFromTwentyTwentySeven(t *testing.T) {
	f := testpg.New(t)
	rows, err := f.Owner.Query(t.Context(), `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
		WHERE i.inhparent='public.security_audit_events'::regclass ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, "security_audit_events_2026") || !slices.Contains(names, "security_audit_events_default") ||
		slices.Contains(names, "security_audit_events_2027") || slices.Contains(names, "security_audit_events_2031") {
		t.Fatalf("audit partitions %v", names)
	}
	var current bool // the month 92 days ahead (never before 2027-01) has its partition, whenever the test runs
	if err := f.Owner.QueryRow(t.Context(), `SELECT to_regclass('public.security_audit_events_' ||
		to_char(greatest(timestamptz '2027-01-01 00:00:00+00', date_trunc('month', (now() + interval '92 days') AT TIME ZONE 'UTC') AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', 'YYYY_MM')) IS NOT NULL`).Scan(&current); err != nil || !current {
		t.Fatalf("no partition 92 days ahead: %v", err)
	}
	var created int
	if err := f.Maintenance.QueryRow(t.Context(), `SELECT created FROM app.ensure_audit_partitions(now() + interval '92 days')`).Scan(&created); err != nil || created != 0 {
		t.Fatalf("a second ensure created %d (%v); the migration already covers 92 days", created, err)
	}
	var pgErr *pgconn.PgError
	if _, err := f.Maintenance.Exec(t.Context(), `SELECT app.ensure_audit_partitions(now() + interval '401 days')`); !errors.As(err, &pgErr) || pgErr.Code != "22023" {
		t.Fatalf("a horizon past 400 days: %v", err)
	}
	for _, horizon := range []string{`NULL`, `'infinity'`} {
		if _, err := f.Maintenance.Exec(t.Context(), `SELECT app.ensure_audit_partitions(`+horizon+`)`); !errors.As(err, &pgErr) || pgErr.Code != "22023" {
			t.Fatalf("horizon %s: %v", horizon, err)
		}
	}
	if _, err := f.Runtime.Exec(t.Context(), `SELECT app.ensure_audit_partitions(now())`); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("runtime executed the partition roller: %v", err)
	}
	// A run one year ahead creates every missing month up to it, each a
	// one-month range owned like its siblings and tuned for inserts, and the
	// maintenance login gains no privilege on any of them.
	// The months it creates are exactly the walk's months up to the horizon
	// that did not exist yet (auditMonths; never a count, which varies with
	// the date).
	yearAhead := rollAuditPartitions(t, f, 365)
	yearAhead.requireExactly(t, nil)
	var bad int
	if err := f.Owner.QueryRow(t.Context(), `SELECT count(*) FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
		WHERE i.inhparent='public.security_audit_events'::regclass AND c.relname ~ '^security_audit_events_[0-9]{4}_[0-9]{2}$'
		  AND (pg_get_expr(c.relpartbound, c.oid) <> format('FOR VALUES FROM (%L) TO (%L)',
		         to_date(substr(c.relname, 23) || '_01', 'YYYY_MM_DD')::timestamp AT TIME ZONE 'UTC',
		         (to_date(substr(c.relname, 23) || '_01', 'YYYY_MM_DD')::timestamp + interval '1 month') AT TIME ZONE 'UTC')
		    OR c.relowner <> (SELECT relowner FROM pg_class WHERE oid='public.security_audit_events'::regclass)
		    OR NOT coalesce(c.reloptions @> ARRAY['autovacuum_vacuum_insert_scale_factor=0.05'], false)
		    OR c.relacl IS NOT NULL)`).Scan(&bad); err != nil || bad != 0 {
		t.Fatalf("%d monthly partitions with a wrong range, owner, options or grant (%v)", bad, err)
	}
	if err := database.VerifyMaintenanceRole(t.Context(), f.Runtime, f.Maintenance, 32); err != nil {
		t.Fatalf("rolled partitions broke the maintenance audit: %v", err)
	}
	if err := database.VerifyRuntimeRole(t.Context(), f.Runtime, 32); err != nil {
		t.Fatalf("rolled partitions broke the runtime audit: %v", err)
	}
}

// A month is attached, not created as a partition: attaching takes SHARE
// UPDATE EXCLUSIVE on the audit parent, which neither an open audit insert nor
// a reader holding the whole history conflicts with, so the roller never
// queues the node's audit inserts (every refresh, sign-in and family command)
// behind itself. Its own lock waits are bounded (lock_timeout 1s, in the
// function): behind a reader of the default partition it gives up quickly.
func TestAuditRollerNeverBlocksAuditInserts(t *testing.T) {
	for _, holder := range []struct {
		name  string
		hold  string
		fails bool // the roller may give up (55P03) rather than finish
	}{
		{"an open audit insert", `INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
			VALUES ('account',gen_random_uuid(),1,sha256('p'::bytea),sha256('h'::bytea),'session_rotated',gen_random_uuid(),gen_random_uuid(),now(),'{}')`, false},
		{"an open reader of the whole history", `SELECT count(*) FROM security_audit_events`, true},
	} {
		t.Run(holder.name, func(t *testing.T) {
			f := testpg.New(t)
			ctx := t.Context()
			hold, err := f.Owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer hold.Rollback(context.Background())
			if _, err := hold.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
				t.Fatal(err)
			}
			if _, err := hold.Exec(ctx, holder.hold); err != nil {
				t.Fatal(err)
			}
			type result struct {
				created int
				err     error
				took    time.Duration
			}
			done := make(chan result, 1)
			rollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			started := time.Now()
			go func() {
				var r result
				r.err = f.Maintenance.QueryRow(rollCtx, `SELECT created FROM app.ensure_audit_partitions(now() + interval '365 days')`).Scan(&r.created)
				r.took = time.Since(started)
				done <- r
			}()
			// Let the roller reach its lock request (or finish).
			var r result
			select {
			case r = <-done:
			case <-time.After(300 * time.Millisecond):
			}
			insertCtx, insertCancel := context.WithTimeout(ctx, 3*time.Second)
			defer insertCancel()
			insertStarted := time.Now()
			err = authorityExec(insertCtx, f.Runtime, `INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
				VALUES ('account',gen_random_uuid(),1,sha256('p'::bytea),sha256('h'::bytea),'session_rotated',gen_random_uuid(),gen_random_uuid(),now(),'{}')`)
			waited := time.Since(insertStarted)
			if err != nil || waited > 100*time.Millisecond {
				t.Fatalf("an audit insert issued while the roller ran waited %v (%v), want under 100ms", waited, err)
			}
			if r.took == 0 {
				r = <-done
			}
			var pgErr *pgconn.PgError
			switch {
			case r.err == nil && r.created > 0:
			case holder.fails && errors.As(r.err, &pgErr) && pgErr.Code == "55P03":
			default:
				t.Fatalf("roller: created %d, %v after %v", r.created, r.err, r.took)
			}
			if r.took > 1500*time.Millisecond {
				t.Fatalf("the roller took %v: its lock waits are not bounded", r.took)
			}
		})
	}
}

// Two rollers at once never race on a month's name: they serialize on the
// function's advisory lock, so the second, run after the first commits,
// finds every month and creates none, and neither fails.
func TestAuditRollersSerialize(t *testing.T) {
	f := testpg.New(t)
	ctx := t.Context()
	type result struct {
		created int
		err     error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			var r result
			tx, err := f.Maintenance.Begin(ctx)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer tx.Rollback(context.Background())
			if r.err = tx.QueryRow(ctx, `SELECT created FROM app.ensure_audit_partitions(now() + interval '365 days')`).Scan(&r.created); r.err == nil {
				time.Sleep(300 * time.Millisecond) // hold the new months uncommitted
				r.err = tx.Commit(ctx)
			}
			results <- r
		}()
	}
	close(start)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || (a.created == 0) == (b.created == 0) {
		t.Fatalf("concurrent rollers: (%d, %v) and (%d, %v); want one to create every month and the other none", a.created, a.err, b.created, b.err)
	}
}

// One misdated audit row in the default partition refuses only its own
// month's attach (23514); the roller reports that month and still creates
// every other month up to its horizon.
func TestAuditRollerSkipsAMonthTheDefaultPartitionHolds(t *testing.T) {
	f := testpg.New(t)
	ctx := t.Context()
	var month time.Time
	if err := f.Owner.QueryRow(ctx, `SELECT (date_trunc('month', (now() + interval '200 days') AT TIME ZONE 'UTC')) AT TIME ZONE 'UTC'`).Scan(&month); err != nil {
		t.Fatal(err)
	}
	if err := authorityExec(ctx, f.Owner, `INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
		VALUES ('account',gen_random_uuid(),1,sha256('p'::bytea),sha256('h'::bytea),'session_rotated',gen_random_uuid(),gen_random_uuid(),$1,'{}')`, month.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var inDefault int
	if err := f.Owner.QueryRow(ctx, `SELECT count(*) FROM security_audit_events_default`).Scan(&inDefault); err != nil || inDefault != 1 {
		t.Fatalf("the misdated row is not in the default partition: %d (%v)", inDefault, err)
	}
	want := "security_audit_events_" + month.UTC().Format("2006_01")
	// Every month the walk reaches that did not exist is created, except the
	// refused one, which is reported: an exact set, whatever the date.
	rollAuditPartitions(t, f, 365).requireExactly(t, []string{want})
	var exists bool
	if err := f.Owner.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+want).Scan(&exists); err != nil || exists {
		t.Fatalf("the refused month's table survived: %v (%v)", exists, err)
	}
	var after int
	if err := f.Owner.QueryRow(ctx, `SELECT count(*) FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
		WHERE i.inhparent='public.security_audit_events'::regclass AND c.relname > $1 AND c.relname ~ '^security_audit_events_[0-9]{4}_[0-9]{2}$'`, want).Scan(&after); err != nil || after == 0 {
		t.Fatalf("no month after the refused one was created: %d (%v)", after, err)
	}
	// A clean second run creates nothing and reports the same month again.
	var created int
	var skipped []string
	if err := f.Maintenance.QueryRow(ctx, `SELECT created, skipped FROM app.ensure_audit_partitions(now() + interval '365 days')`).Scan(&created, &skipped); err != nil ||
		created != 0 || !slices.Equal(skipped, []string{want}) {
		t.Fatalf("second run: created %d, skipped %v (%v)", created, skipped, err)
	}
}

// auditMonths is app.ensure_audit_partitions' month walk (000031 section 6):
// the partition of every month from the later of 2027-01 and the month of now,
// in UTC, whose first instant is at or before through.
func auditMonths(now, through time.Time) []string {
	start := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	if floor := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC); start.Before(floor) {
		start = floor
	}
	var months []string
	for month := start; !month.After(through); month = month.AddDate(0, 1, 0) {
		months = append(months, "security_audit_events_"+month.Format("2006_01"))
	}
	return months
}

// The walk the roller tests compare against, at fixed dates. With one month
// skipped, a year's roll after the migration's 92 days creates 7 or 8 months
// depending on the date, so a count bound (the former `created < 8`) failed
// on about 14 days a year, 2026-11-30 among them; the tests assert the exact
// set instead.
func TestAuditMonthWalkAtFixedDates(t *testing.T) {
	day := 24 * time.Hour
	for _, c := range []struct {
		now              time.Time
		migrated, rolled []string // by the migration (92 days), by a year's roll with month(now + 200 days) skipped
		skipped          string
	}{
		{time.Date(2026, 11, 30, 12, 0, 0, 0, time.UTC),
			[]string{"2027_01", "2027_02", "2027_03"}, []string{"2027_04", "2027_05", "2027_07", "2027_08", "2027_09", "2027_10", "2027_11"}, "2027_06"},
		{time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC),
			[]string{"2027_01"}, []string{"2027_02", "2027_03", "2027_04", "2027_06", "2027_07", "2027_08", "2027_09", "2027_10"}, "2027_05"},
		{time.Date(2027, 6, 15, 12, 0, 0, 0, time.UTC),
			[]string{"2027_06", "2027_07", "2027_08", "2027_09"}, []string{"2027_10", "2027_11", "2027_12", "2028_02", "2028_03", "2028_04", "2028_05", "2028_06"}, "2028_01"},
	} {
		prefixed := func(months []string) []string {
			out := make([]string, len(months))
			for i, m := range months {
				out[i] = "security_audit_events_" + m
			}
			return out
		}
		migrated := auditMonths(c.now, c.now.Add(92*day))
		skipped := "security_audit_events_" + c.now.Add(200*day).Format("2006_01")
		var rolled []string
		for _, m := range auditMonths(c.now, c.now.Add(365*day)) {
			if !slices.Contains(migrated, m) && m != skipped {
				rolled = append(rolled, m)
			}
		}
		if !slices.Equal(migrated, prefixed(c.migrated)) || !slices.Equal(rolled, prefixed(c.rolled)) || skipped != "security_audit_events_"+c.skipped {
			t.Fatalf("%s: migrated %v, rolled %v, skipped %s", c.now.Format(time.DateOnly), migrated, rolled, skipped)
		}
	}
	// The former bound: false at 2026-11-30 (7 months), true at 2026-10-15 (8).
	if n := len(auditMonths(time.Date(2026, 11, 30, 12, 0, 0, 0, time.UTC), time.Date(2027, 11, 30, 12, 0, 0, 0, time.UTC))) - 3 - 1; n >= 8 {
		t.Fatalf("2026-11-30 rolls %d months; the count bound this test replaced would have passed", n)
	}
}

// auditRoll is one roller call and the monthly partitions around it, all in
// one maintenance transaction, so now() is the roller's own.
type auditRoll struct {
	now, through  time.Time
	created       int
	skipped       []string
	before, after []string
}

func rollAuditPartitions(t *testing.T, f *testpg.Fixture, days int) auditRoll {
	t.Helper()
	ctx := t.Context()
	tx, err := f.Maintenance.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	monthly := func() []string {
		rows, err := tx.Query(ctx, `SELECT c.relname::text FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
			WHERE i.inhparent='public.security_audit_events'::regclass AND c.relname ~ '^security_audit_events_[0-9]{4}_[0-9]{2}$' ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	var r auditRoll
	if err := tx.QueryRow(ctx, `SELECT now(), now() + make_interval(days => $1)`, days).Scan(&r.now, &r.through); err != nil {
		t.Fatal(err)
	}
	r.before = monthly()
	if err := tx.QueryRow(ctx, `SELECT created, skipped FROM app.ensure_audit_partitions($1)`, r.through).Scan(&r.created, &r.skipped); err != nil {
		t.Fatalf("roller: %v", err)
	}
	r.after = monthly()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return r
}

// requireExactly: the roll created exactly the walk's missing months except
// skipped, reported exactly skipped, and left exactly those partitions.
func (r auditRoll) requireExactly(t *testing.T, skipped []string) {
	t.Helper()
	var created []string
	for _, m := range auditMonths(r.now, r.through) {
		if !slices.Contains(r.before, m) && !slices.Contains(skipped, m) {
			created = append(created, m)
		}
	}
	after := append(slices.Clone(r.before), created...)
	slices.Sort(after)
	if r.created != len(created) || !slices.Equal(r.skipped, append([]string{}, skipped...)) || !slices.Equal(r.after, after) {
		t.Fatalf("roll at %v through %v: created %d %v, skipped %v; want created %d %v, skipped %v (partitions before %v)",
			r.now, r.through, r.created, r.after, r.skipped, len(created), after, skipped, r.before)
	}
}

// Audit rows written at the clock's time land in a named partition, never the
// default: on both sides of the next partition boundary and now.
func TestAuditRowsNeverReachTheDefaultPartition(t *testing.T) {
	f := testpg.New(t)
	ctx := t.Context()
	var boundary time.Time
	if err := f.Owner.QueryRow(ctx, `SELECT greatest(timestamptz '2027-01-01 00:00:00+00',
		(date_trunc('month', now() AT TIME ZONE 'UTC') + interval '1 month') AT TIME ZONE 'UTC')`).Scan(&boundary); err != nil {
		t.Fatal(err)
	}
	scope := randomUUID(t)
	for seq, at := range []time.Time{time.Now(), boundary.Add(-time.Second), boundary, boundary.Add(time.Hour)} {
		if err := authorityExec(ctx, f.Owner, `INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
			VALUES ('account',$1,$2,$3,$4,'session_rotated',$5,$1,$6,'{}')`, scope, seq+1, authorityDigest(byte(seq)), authorityDigest(byte(seq+1)), randomUUID(t), at); err != nil {
			t.Fatal(err)
		}
	}
	var inDefault int
	if err := f.Owner.QueryRow(ctx, `SELECT count(*) FROM security_audit_events_default`).Scan(&inDefault); err != nil || inDefault != 0 {
		t.Fatalf("%d audit rows in the default partition (%v)", inDefault, err)
	}
	var after string
	if err := f.Owner.QueryRow(ctx, `SELECT c.relname FROM pg_class c WHERE c.oid IN (SELECT inhrelid FROM pg_inherits WHERE inhparent='public.security_audit_events'::regclass)
		AND c.relname = 'security_audit_events_' || to_char($1::timestamptz AT TIME ZONE 'UTC', 'YYYY_MM')`, boundary).Scan(&after); err != nil {
		t.Fatalf("no monthly partition at the boundary %v: %v", boundary, err)
	}
}

func TestHotTablesHaveTunedAutovacuum(t *testing.T) {
	f := testpg.New(t)
	for table, want := range map[string]string{
		"care_entities_p00":          "autovacuum_vacuum_scale_factor=0.05",
		"care_entities_p15":          "autovacuum_analyze_scale_factor=0.02",
		"care_receipts":              "autovacuum_vacuum_insert_scale_factor=0.05",
		"sessions":                   "autovacuum_vacuum_scale_factor=0.02",
		"refresh_receipts":           "autovacuum_vacuum_scale_factor=0.05",
		"families":                   "autovacuum_vacuum_scale_factor=0.05",
		"sync_page_bindings":         "autovacuum_vacuum_scale_factor=0.05",
		"sync_snapshot_sessions":     "autovacuum_vacuum_scale_factor=0.05",
		"auth_challenges":            "autovacuum_vacuum_scale_factor=0.05",
		"device_sync_cursors":        "fillfactor=70",
		"device_push_tokens":         "fillfactor=80",
		"security_audit_events_2026": "autovacuum_vacuum_insert_scale_factor=0.05",
	} {
		var options string
		if err := f.Owner.QueryRow(t.Context(), `SELECT coalesce(array_to_string(reloptions, ','), '') FROM pg_class WHERE oid=$1::regclass`, "public."+table).Scan(&options); err != nil || !strings.Contains(options, want) {
			t.Fatalf("%s reloptions %q (%v), want %s", table, options, err, want)
		}
	}
	var untuned int
	if err := f.Owner.QueryRow(t.Context(), `SELECT count(*) FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid
		WHERE i.inhparent='public.care_entities'::regclass
		  AND NOT coalesce(c.reloptions @> ARRAY['fillfactor=90','autovacuum_vacuum_scale_factor=0.05','autovacuum_analyze_scale_factor=0.02'], false)`).Scan(&untuned); err != nil || untuned != 0 {
		t.Fatalf("%d care partitions untuned (%v)", untuned, err)
	}
}

func TestRefreshReceiptsFreezeTheChildScope(t *testing.T) {
	f := testpg.New(t)
	var shape string
	if err := f.Owner.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='refresh_receipts_child_scope_shape'`).Scan(&shape); err != nil ||
		!strings.Contains(shape, "child_scope = 'account'") {
		t.Fatalf("shape %q %v", shape, err)
	}
	ids := seedAuthorityFamilies(t, f)
	parent := seedSession(t, f, ids.accountA, ids.deviceA, "", "")
	child := seedSession(t, f, ids.accountA, ids.deviceA, "", "")
	insert := func(scope, family, role any) error {
		material := []byte("recovery material")
		digest := sha256.Sum256(material)
		tx, err := f.Owner.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
			return err
		}
		_, err = tx.Exec(t.Context(), `INSERT INTO refresh_receipts(parent_id,child_id,attempt_id,account_id,device_id,material,material_digest,created_at,effective_deadline,refresh_expires_at,
			child_scope,child_family_id,child_role) VALUES ($1,$2,$3,$4,$5,$6,$7,now(),now() + interval '1 day',now() + interval '1 day',$8,$9,$10)`,
			parent, child, randomUUID(t), ids.accountA, ids.deviceA, material, digest[:], scope, family, role)
		return err
	}
	for _, accepted := range []struct{ scope, family, role any }{
		{nil, nil, nil}, {"account", nil, nil}, {"family", ids.familyA, "caregiver"}, {"family_recovery", ids.familyA, "owner"},
	} {
		if err := insert(accepted.scope, accepted.family, accepted.role); err != nil {
			t.Fatalf("%v rejected: %v", accepted, err)
		}
	}
	for _, refused := range []struct{ scope, family, role any }{
		{"account", ids.familyA, nil}, {"account", nil, "owner"}, {"family", nil, "owner"}, {"family", ids.familyA, nil}, {nil, ids.familyA, "owner"},
	} {
		assertSQLState(t, insert(refused.scope, refused.family, refused.role), "23514")
	}
}

// Push tokens: the owner's account only, maintenance only through the definers
// and the refused-token delete, and send-time eligibility from the sessions.
func TestPushTokensAreOwnedAndEligibilityFollowsSessions(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f) // accounts A and B, families A and B, device A
	ctx := t.Context()
	// Device A's family-scope session in family A, inserted as authority_schema_test.go:137 inserts sessions.
	seedFamilySession(t, f, ids.accountA, ids.deviceA, ids.familyA)
	tokenA := bytes.Repeat([]byte{0xa1}, 32)
	tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	if _, err := tx.Exec(ctx, `INSERT INTO device_push_tokens(device_id,account_id,environment,token,registered_at) VALUES($1,$2,'sandbox',$3,now())`,
		ids.deviceA, ids.accountA, tokenA); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if _, err := tx.Exec(ctx, `INSERT INTO device_push_tokens(device_id,account_id,environment,token,registered_at) VALUES($1,$2,'sandbox',$3,now())`,
		ids.deviceA, ids.accountB, tokenA); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("a tenant wrote another account's token: %v", err)
	}
	_ = tx.Rollback(ctx)
	// The row shape: a token of 16 to 256 bytes, a known environment, a real device.
	for _, refused := range []struct {
		device, environment string
		token               []byte
		state               string
	}{
		{ids.deviceA, "sandbox", bytes.Repeat([]byte{1}, 15), "23514"},
		{ids.deviceA, "sandbox", bytes.Repeat([]byte{1}, 257), "23514"},
		{ids.deviceA, "staging", tokenA, "22P02"},
		{"00000000-0000-0000-0000-000000000000", "sandbox", tokenA, "23514"},
		{"63000000-0000-4000-8000-0000000000ff", "sandbox", tokenA, "23503"},
		{ids.deviceA, "sandbox", nil, "23502"},
		{ids.deviceA, "infinity", tokenA, "23514"}, // registered_at, below
	} {
		tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
		environment, registered := refused.environment, "now"
		if environment == "infinity" {
			environment, registered = "sandbox", "infinity"
		}
		_, err := tx.Exec(ctx, `INSERT INTO device_push_tokens(device_id,account_id,environment,token,registered_at) VALUES($1,$2,$3::text::app.apns_environment,$4,$5::text::timestamptz)`,
			refused.device, ids.accountA, environment, refused.token, registered)
		assertSQLState(t, err, refused.state)
		_ = tx.Rollback(ctx)
	}
	tx = tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	if _, err := tx.Exec(ctx, `INSERT INTO device_push_tokens(device_id,account_id,environment,token,registered_at) VALUES($1,$2,'sandbox',$3,now())`,
		ids.deviceA, ids.accountA, tokenA); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Task 14's upsert re-registers the device; another account's tenant sees nothing.
	tx = tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	if tag, err := tx.Exec(ctx, `INSERT INTO device_push_tokens(device_id, account_id, environment, token, registered_at)
		VALUES ($1, $2, $3::text::app.apns_environment, $4, $5)
		ON CONFLICT (device_id) DO UPDATE SET environment = EXCLUDED.environment, token = EXCLUDED.token, registered_at = EXCLUDED.registered_at
		WHERE device_push_tokens.account_id = EXCLUDED.account_id`, ids.deviceA, ids.accountA, "production", tokenA, time.Now()); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("re-register %d (%v)", tag.RowsAffected(), err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var visible int
	other := tenantTx(t, f.Runtime, ids.accountB, ids.familyB)
	if err := other.QueryRow(ctx, `SELECT count(*) FROM device_push_tokens`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("account B sees %d tokens (%v)", visible, err)
	}
	_ = other.Rollback(ctx)
	// Three more of account A's devices, each with a token and each
	// ineligible: B is revoked (its family session still active), C's family
	// session can no longer refresh, D holds only an account-scope session.
	deviceB, deviceC, deviceD := seedDevice(t, f, ids.accountA, true), seedDevice(t, f, ids.accountA, false), seedDevice(t, f, ids.accountA, false)
	seedFamilySession(t, f, ids.accountA, deviceB, ids.familyA)
	expired := seedFamilySession(t, f, ids.accountA, deviceC, ids.familyA)
	if err := authorityExec(ctx, f.Owner, `UPDATE sessions SET access_expires_at = now() - interval '2 hours', refresh_expires_at = now() - interval '1 hour' WHERE id=$1`, expired); err != nil {
		t.Fatal(err)
	}
	seedSession(t, f, ids.accountA, deviceD, "", "")
	tx = tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	for i, device := range []string{deviceB, deviceC, deviceD} {
		if _, err := tx.Exec(ctx, `INSERT INTO device_push_tokens(device_id,account_id,environment,token,registered_at) VALUES($1,$2,'sandbox',$3,now())`,
			device, ids.accountA, bytes.Repeat([]byte{byte(0xb0 + i)}, 32)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var targets int
	if err := f.Maintenance.QueryRow(ctx, `SELECT count(*) FROM app.push_targets($1::uuid[], $2::uuid[])`,
		[]string{ids.familyA, ids.familyB, ids.familyA, ids.familyA, ids.familyA}, []string{ids.deviceA, ids.deviceA, deviceB, deviceC, deviceD}).Scan(&targets); err != nil || targets != 1 {
		t.Fatalf("eligible targets %d (%v): want only device A in its own family", targets, err)
	}
	var environment, family string
	var token []byte
	if err := f.Maintenance.QueryRow(ctx, `SELECT family_id::text, environment::text, token FROM app.push_targets($1::uuid[], $2::uuid[])`,
		[]string{ids.familyA}, []string{ids.deviceA}).Scan(&family, &environment, &token); err != nil || family != ids.familyA || environment != "production" || !bytes.Equal(token, tokenA) {
		t.Fatalf("target %s %s %x (%v)", family, environment, token, err)
	}
	for _, refused := range []struct{ families, devices any }{
		{[]string{ids.familyA}, []string{}}, {nil, []string{ids.deviceA}}, {slices.Repeat([]string{ids.familyA}, 1001), slices.Repeat([]string{ids.deviceA}, 1001)},
	} {
		if _, err := f.Maintenance.Exec(ctx, `SELECT * FROM app.push_targets($1::uuid[], $2::uuid[])`, refused.families, refused.devices); !errors.As(err, &pgErr) || pgErr.Code != "22023" {
			t.Fatalf("push_targets(%d,%d): %v", sliceLen(refused.families), sliceLen(refused.devices), err)
		}
	}
	// The maintenance login reaches tokens only through the definers: it
	// cannot list them, and only push_targets' eligible ones come back.
	for _, read := range []string{`SELECT device_id, token FROM device_push_tokens`, `SELECT environment FROM device_push_tokens`, `SELECT count(*) FROM device_push_tokens`} {
		if _, err := f.Maintenance.Exec(ctx, read); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("maintenance read the tokens directly (%s): %v", read, err)
		}
	}
	// The runtime's authority transactions (app.authority without a tenant)
	// do not reach tokens either: the authority policy names only the
	// migration owner that owns the definers.
	var authorityVisible int
	atx, err := f.Runtime.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer atx.Rollback(context.Background()) // a held connection would hang the pool's Close on failure
	if _, err := atx.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	if err := atx.QueryRow(ctx, `SELECT count(*) FROM device_push_tokens`).Scan(&authorityVisible); err != nil || authorityVisible != 0 {
		t.Fatalf("an authority transaction of the runtime sees %d tokens (%v)", authorityVisible, err)
	}
	if tag, err := atx.Exec(ctx, `UPDATE device_push_tokens SET token=$1`, bytes.Repeat([]byte{0xee}, 32)); err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("an authority transaction of the runtime rewrote %d tokens (%v)", tag.RowsAffected(), err)
	}
	if err := atx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for _, fn := range []string{`SELECT * FROM app.push_targets(ARRAY['` + ids.familyA + `']::uuid[], ARRAY['` + ids.deviceA + `']::uuid[])`, `SELECT app.prune_push_tokens(1)`,
		`SELECT app.forget_push_token('` + ids.deviceA + `', '\x00')`} {
		if _, err := f.Runtime.Exec(ctx, fn); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("runtime executed %s: %v", fn, err)
		}
	}
	var pruned int
	if err := f.Maintenance.QueryRow(ctx, `SELECT app.prune_push_tokens(1)`).Scan(&pruned); err != nil || pruned != 1 {
		t.Fatalf("a prune bounded at 1 pruned %d (%v)", pruned, err)
	}
	if err := f.Maintenance.QueryRow(ctx, `SELECT app.prune_push_tokens(100)`).Scan(&pruned); err != nil || pruned != 1 {
		t.Fatalf("pruned %d (%v), want the second of C (expired) and D (account scope only)", pruned, err)
	}
	if left := storedTokenDevices(t, f); !slices.Equal(left, sortedCopy([]string{ids.deviceA, deviceB})) {
		t.Fatalf("tokens left %v, want A and B (B's family session is active)", left)
	}
	if err := authorityExec(ctx, f.Runtime, `UPDATE sessions SET status='revoked', revoked_at=now(), revoke_reason='member_removed' WHERE device_id=$1`, ids.deviceA); err != nil {
		t.Fatal(err)
	}
	if err := f.Maintenance.QueryRow(ctx, `SELECT count(*) FROM app.push_targets($1::uuid[], $2::uuid[])`,
		[]string{ids.familyA}, []string{ids.deviceA}).Scan(&targets); err != nil || targets != 0 {
		t.Fatalf("a revoked device is still a push target: %d %v", targets, err)
	}
	for _, limit := range []int{0, 10001} {
		if _, err := f.Maintenance.Exec(ctx, `SELECT app.prune_push_tokens($1)`, limit); !errors.As(err, &pgErr) || pgErr.Code != "22023" {
			t.Fatalf("prune limit %d: %v", limit, err)
		}
	}
	if err := f.Maintenance.QueryRow(ctx, `SELECT app.prune_push_tokens(100)`).Scan(&pruned); err != nil || pruned != 1 {
		t.Fatalf("pruned %d (%v), want the revoked session's token", pruned, err)
	}
	var authority string
	if err := f.Maintenance.QueryRow(ctx, `SELECT coalesce(current_setting('app.authority', true), '')`).Scan(&authority); err != nil || authority == "1" {
		t.Fatalf("a definer left the authority flag set: %q %v", authority, err)
	}
	// Inside one maintenance transaction, all three definers restore the flag.
	mtx, err := f.Maintenance.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer mtx.Rollback(context.Background())
	if _, err := mtx.Exec(ctx, `SELECT * FROM app.push_targets($1::uuid[], $1::uuid[])`, []string{ids.deviceA}); err != nil {
		t.Fatal(err)
	}
	if _, err := mtx.Exec(ctx, `SELECT app.prune_push_tokens(1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := mtx.Exec(ctx, `SELECT app.forget_push_token($1, $2)`, deviceB, bytes.Repeat([]byte{0xb0}, 32)); err != nil {
		t.Fatal(err)
	}
	if err := mtx.QueryRow(ctx, `SELECT coalesce(current_setting('app.authority', true), '')`).Scan(&authority); err != nil || authority == "1" {
		t.Fatalf("a definer left the authority flag set in its transaction: %q %v", authority, err)
	}
}

// Task 15's refused-token delete, app.forget_push_token: only the token that
// was sent, through the definer (the maintenance login holds no privilege on
// device_push_tokens).
func TestMaintenanceDeletesOnlyTheRefusedToken(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f)
	ctx := t.Context()
	tokenA := bytes.Repeat([]byte{0xa2}, 32)
	tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	if _, err := tx.Exec(ctx, `INSERT INTO device_push_tokens(device_id,account_id,environment,token,registered_at) VALUES($1,$2,'sandbox',$3,now())`,
		ids.deviceA, ids.accountA, tokenA); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var forgotten bool
	if err := f.Maintenance.QueryRow(ctx, `SELECT app.forget_push_token($1, $2)`, ids.deviceA, bytes.Repeat([]byte{0xb2}, 32)).Scan(&forgotten); err != nil || forgotten {
		t.Fatalf("a stale token's refusal forgot the current one: %v (%v)", forgotten, err)
	}
	if left := storedTokenDevices(t, f); !slices.Equal(left, []string{ids.deviceA}) {
		t.Fatalf("tokens after a stale refusal: %v", left)
	}
	if err := f.Maintenance.QueryRow(ctx, `SELECT app.forget_push_token($1, $2)`, ids.deviceA, tokenA).Scan(&forgotten); err != nil || !forgotten {
		t.Fatalf("the refused token was not forgotten: %v (%v)", forgotten, err)
	}
	if left := storedTokenDevices(t, f); len(left) != 0 {
		t.Fatalf("tokens after the refusal: %v", left)
	}
	var pgErr *pgconn.PgError
	for _, args := range [][]any{{nil, tokenA}, {ids.deviceA, nil}} {
		if _, err := f.Maintenance.Exec(ctx, `SELECT app.forget_push_token($1::uuid, $2::bytea)`, args...); !errors.As(err, &pgErr) || pgErr.Code != "22023" {
			t.Fatalf("forget_push_token%v: %v", args, err)
		}
	}
}

// storedTokenDevices lists the devices holding a push token, read as the
// migration owner under the authority flag (the one reader push_token_authority
// admits besides a tenant's own account).
func storedTokenDevices(t *testing.T, f *testpg.Fixture) []string {
	t.Helper()
	tx, err := f.Owner.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(t.Context(), `SELECT device_id::text FROM device_push_tokens ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	devices, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return devices
}

// seedFamilySession inserts an active family-scope owner session for device
// in family, as authority_schema_test.go:137 inserts sessions, refreshable for
// 30 days.
func seedFamilySession(t *testing.T, f *testpg.Fixture, account, device, family string) string {
	t.Helper()
	return seedSession(t, f, account, device, family, "owner")
}

// seedSession inserts an active session (account scope when family is empty).
func seedSession(t *testing.T, f *testpg.Fixture, account, device, family, role string) string {
	t.Helper()
	id := randomUUID(t)
	scope := "account"
	var familyID, roleValue any
	if family != "" {
		scope, familyID, roleValue = "family", family, role
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := authorityExec(t.Context(), f.Owner, `INSERT INTO sessions(access_digest,id,account_id,device_id,lineage_root_id,lineage_started_at,scope,family_id,role,
		recovery_epoch,issued_seq,token_key_id,refresh_digest,access_expires_at,refresh_expires_at,deadline_at,status,created_at)
		VALUES ($1,$2,$3,$4,$2,$5,$6::text::app.session_scope,$7,$8::text::app.member_role,1,1,'token.v1',$9,$5::timestamptz + interval '15 minutes',
		  now() + interval '30 days',$5::timestamptz + interval '90 days','active',$5)`,
		randomDigest(t), id, account, device, now, scope, familyID, roleValue, randomDigest(t)); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedDevice inserts a device of account, revoked or active.
func seedDevice(t *testing.T, f *testpg.Fixture, account string, revoked bool) string {
	t.Helper()
	id := randomUUID(t)
	status, revokedAt := "active", any(nil)
	if revoked {
		status, revokedAt = "revoked", time.Now()
	}
	if err := authorityExec(t.Context(), f.Owner, `INSERT INTO devices(id,account_id,canonical_origin,continuity_key_id,continuity_digest,created_at,updated_at,status,revoked_at)
		VALUES ($1,$2,'https://tracker.example','continuity.v1',$3,now(),now(),$4::text::app.device_status,$5)`, id, account, randomDigest(t), status, revokedAt); err != nil {
		t.Fatal(err)
	}
	return id
}

func randomUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randomDigest(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func sliceLen(v any) int {
	if s, ok := v.([]string); ok {
		return len(s)
	}
	return -1
}
