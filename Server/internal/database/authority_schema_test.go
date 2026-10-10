package database_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAuthoritySchemaHoldsAtVersionThirty(t *testing.T) {
	f := testpg.New(t)
	var v int64
	if err := f.Runtime.QueryRow(t.Context(), `SELECT current_version FROM schema_compatibility WHERE singleton`).Scan(&v); err != nil || v != 32 {
		t.Fatalf("version=%d err=%v", v, err)
	}
	// Catalog checks run against the migrated test database (the Admin pool
	// is connected to the bootstrap database); prove that with a table that
	// must exist.
	for _, kept := range []string{"sessions", "refresh_receipts", "command_receipts", "security_audit_events", "audit_checkpoints", "apple_notifications"} {
		var exists bool
		if err := f.Owner.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, "public."+kept).Scan(&exists); err != nil || !exists {
			t.Fatalf("authority table %s missing: %v", kept, err)
		}
	}
	for _, gone := range []string{"security_projection_head", "security_command_receipts", "account_security_state", "family_security_state", "auth_exchange_material", "session_refresh_history", "refresh_recovery_receipts", "operator_audit_events", "family_command_material", "apple_revocation_jobs", "credential_cleanup_jobs"} {
		var exists bool
		if err := f.Owner.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, "public."+gone).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("legacy table %s survived", gone)
		}
	}
	var projector bool
	if err := f.Owner.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND p.proname LIKE 'project_security_event%')`).Scan(&projector); err != nil {
		t.Fatal(err)
	}
	if projector {
		t.Fatal("projector function survived")
	}
	if err := database.VerifyRuntimeRole(t.Context(), f.Runtime, 32); err != nil {
		t.Fatal(err)
	}
}

// authorityExec runs one statement as the table owner inside a transaction
// that carries the authority context, the way authority transactions write
// the forced-RLS authority tables.
func authorityExec(ctx context.Context, pool *pgxpool.Pool, statement string, args ...any) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT set_config('app.authority','1',true)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, statement, args...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func requireUniqueViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("expected unique_violation on %s, got %v", constraint, err)
	}
	if constraint != "" && pgErr.ConstraintName != constraint {
		t.Fatalf("unique_violation on %s, want %s", pgErr.ConstraintName, constraint)
	}
}

func TestAuthoritySchemaUniqueRules(t *testing.T) {
	f := testpg.New(t)
	ctx := t.Context()
	const (
		accountOne = "10000000-0000-4000-8000-000000000001"
		accountTwo = "10000000-0000-4000-8000-000000000002"
		familyOne  = "20000000-0000-4000-8000-000000000001"
		familyTwo  = "20000000-0000-4000-8000-000000000002"
		deviceOne  = "30000000-0000-4000-8000-000000000001"
	)
	digest := func(b byte) []byte {
		out := make([]byte, 32)
		for i := range out {
			out[i] = b
		}
		return out
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, seed := range []string{
		`INSERT INTO accounts(id,display_name,status) VALUES ('` + accountOne + `','One','active'),('` + accountTwo + `','Two','active')`,
		`INSERT INTO families(id,display_name,status,owner_account_id) VALUES ('` + familyOne + `','Family One','active','` + accountOne + `'),('` + familyTwo + `','Family Two','active','` + accountTwo + `')`,
	} {
		if err := authorityExec(ctx, f.Owner, seed); err != nil {
			t.Fatalf("seed %q: %v", seed, err)
		}
	}
	if err := authorityExec(ctx, f.Owner, `INSERT INTO devices(id,account_id,canonical_origin,continuity_key_id,continuity_digest,created_at,updated_at)
		VALUES ($1,$2,'https://tracker.example','continuity.v1',$3,$4,$4)`, deviceOne, accountOne, digest(7), now); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	// (a) One family per account: family_slots is keyed by account.
	if err := authorityExec(ctx, f.Owner, `INSERT INTO family_slots(account_id,family_id,state) VALUES ($1,$2,'active')`, accountOne, familyOne); err != nil {
		t.Fatal(err)
	}
	requireUniqueViolation(t, authorityExec(ctx, f.Owner, `INSERT INTO family_slots(account_id,family_id,state) VALUES ($1,$2,'active')`, accountOne, familyTwo), "family_slots_pkey")

	// (b) One active owner per family.
	if err := authorityExec(ctx, f.Owner, `INSERT INTO memberships(family_id,account_id,role,status) VALUES ($1,$2,'owner','active')`, familyOne, accountOne); err != nil {
		t.Fatal(err)
	}
	requireUniqueViolation(t, authorityExec(ctx, f.Owner, `INSERT INTO memberships(family_id,account_id,role,status) VALUES ($1,$2,'owner','active')`, familyOne, accountTwo), "memberships_one_active_owner")

	// (c) One MCP integration per verifier.
	insertIntegration := `INSERT INTO mcp_integrations(id,account_id,family_id,recovery_epoch,name,scope,verifier,created_at,expires_at)
		VALUES ($1,$2,$3,1,'Claude','family:read',$4,$5,$5::timestamptz + interval '7776000 seconds')`
	if err := authorityExec(ctx, f.Owner, insertIntegration, "40000000-0000-4000-8000-000000000001", accountOne, familyOne, digest(9), now); err != nil {
		t.Fatal(err)
	}
	requireUniqueViolation(t, authorityExec(ctx, f.Owner, insertIntegration, "40000000-0000-4000-8000-000000000002", accountTwo, familyOne, digest(9), now), "mcp_integrations_verifier_key")

	// (d) Sessions are keyed by access digest.
	insertSession := `INSERT INTO sessions(access_digest,id,account_id,device_id,lineage_root_id,lineage_started_at,scope,recovery_epoch,issued_seq,token_key_id,
		refresh_digest,access_expires_at,refresh_expires_at,deadline_at,created_at)
		VALUES ($1,$2,$3,$4,$2,$5,'account',1,1,'token.v1',$6,$5::timestamptz + interval '15 minutes',$5::timestamptz + interval '30 days',$5::timestamptz + interval '90 days',$5)`
	if err := authorityExec(ctx, f.Owner, insertSession, digest(1), "50000000-0000-4000-8000-000000000001", accountOne, deviceOne, now, digest(2)); err != nil {
		t.Fatal(err)
	}
	requireUniqueViolation(t, authorityExec(ctx, f.Owner, insertSession, digest(1), "50000000-0000-4000-8000-000000000002", accountOne, deviceOne, now, digest(3)), "sessions_pkey")

	// (e) One live account per Apple identity.
	insertIdentity := `INSERT INTO account_identities(provider,identity_key_id,identity_digest,account_id,lifecycle_generation,audience,subject,created_at)
		VALUES ('apple','identity.v1',$1,$2,$3,'com.example.tracker','subject',$4)`
	if err := authorityExec(ctx, f.Owner, insertIdentity, digest(5), accountOne, 1, now); err != nil {
		t.Fatal(err)
	}
	requireUniqueViolation(t, authorityExec(ctx, f.Owner, insertIdentity, digest(5), accountTwo, 2, now), "account_identities_live_idx")
}

func TestAuthorityRuntimeGrantsAreExact(t *testing.T) {
	f := testpg.New(t)
	deny := map[string]string{"security_audit_events": "UPDATE,DELETE", "command_receipts": "UPDATE,DELETE", "server_state": "INSERT,UPDATE,DELETE"}
	for table, privs := range deny {
		var ok bool
		if err := f.Runtime.QueryRow(t.Context(), `SELECT has_table_privilege(current_user,$1,$2)`, "public."+table, privs).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("%s grants %s", table, privs)
		}
	}
	var colOK, tblOK bool
	if err := f.Runtime.QueryRow(t.Context(), `SELECT has_column_privilege(current_user,'public.families','status','UPDATE'), has_table_privilege(current_user,'public.families','DELETE')`).Scan(&colOK, &tblOK); err != nil {
		t.Fatal(err)
	}
	if !colOK || tblOK {
		t.Fatalf("families grants col=%v tbl=%v", colOK, tblOK)
	}
}

// Only the tenant helpers, the authority context, the authority definers and
// the maintenance definers survive in the app schema: 000029 dropped the
// projection's functions, 000030 the legacy care helpers the coexistence
// ruling had kept, and 000031 the rate-limit definer; 000031 added the audit
// partition roller and the three push-token definers; 000032 the Live
// Activity definers and triggers.
func TestAppSchemaKeepsOnlyAuthorityFunctions(t *testing.T) {
	f := testpg.New(t)
	rows, err := f.Owner.Query(t.Context(), `SELECT p.oid::regprocedure::text FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname='app' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[string]bool{
		"app.authority_context()": true,
		"app.authority_actor_label(uuid,uuid,boolean,timestamp with time zone)": true,
		"app.reject_tenant_authority_write()":                                   true,
		"app.reject_audit_chain_regression()":                                   true,
		"app.request_authority(bytea[],uuid,boolean,bigint)":                    true,
		"app.integration_authority(uuid,bigint)":                                true,
		"app.current_account_id()":                                              true,
		"app.current_family_id()":                                               true,
		"app.family_actor_labels_high_water()":                                  true,
		"app.import_unfinished_session_count(uuid,timestamp with time zone)":    true,
		"app.list_family_actor_labels(uuid,bigint,uuid,integer)":                true,
		"app.membership_may_write(uuid,uuid)":                                   true,
		"app.ensure_audit_partitions(timestamp with time zone)":                 true,
		"app.push_targets(uuid[],uuid[])":                                       true,
		"app.prune_push_tokens(integer)":                                        true,
		"app.forget_push_token(uuid,bytea)":                                     true,
		"app.purge_family_step(uuid,integer)":                                   true,
		"app.update_own_profile(uuid,uuid,text,timestamp with time zone)":       true,
		"app.valid_display_name(text)":                                          true,
		// 000032: Live Activity.
		"app.abandon_live_activity_start(jsonb,text,text,boolean,boolean)":                                              true,
		"app.claim_live_activity_work(timestamp with time zone,integer)":                                                true,
		"app.dirty_live_activity_families(uuid[],bigint[])":                                                             true,
		"app.dirty_live_activity_family(uuid,bigint)":                                                                   true,
		"app.fence_live_activity_installation(uuid,bigint,uuid,uuid,uuid,uuid,bigint,timestamp with time zone,boolean)": true,
		"app.finish_live_activity_work(jsonb,timestamp with time zone,text)":                                            true,
		"app.live_activity_authorized(uuid,bigint,boolean)":                                                             true,
		"app.live_activity_claim_live(jsonb)":                                                                           true,
		"app.live_activity_lineage_live(uuid,uuid,uuid,bigint,timestamp with time zone)":                                true,
		"app.live_activity_registration_dirty()":                                                                        true,
		"app.live_activity_registration_state(uuid,jsonb)":                                                              true,
		"app.live_activity_session_binding(uuid,uuid,uuid,bigint)":                                                      true,
		"app.live_activity_target(jsonb,jsonb,boolean,timestamp with time zone,jsonb,jsonb)":                            true,
		"app.live_activity_token_fingerprint()":                                                                         true,
		"app.live_activity_token_ownership()":                                                                           true,
		"app.release_stale_live_activity_lease(jsonb)":                                                                  true,
		"app.retain_live_activity_work()":                                                                               true,
		"app.retire_stale_live_activity_registrations()":                                                                true,
		"app.sweep_live_activity_registrations(boolean,integer)":                                                        true,
		"app.sweep_live_activity_work()":                                                                                true,
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if !want[name] {
			t.Errorf("unexpected app function %s", name)
		}
		delete(want, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for name := range want {
		t.Errorf("missing app function %s", name)
	}
}

func authorityDigest(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

// authorityFixture seeds two families, each with an owner account, slot,
// device and membership, through authority transactions.
type authorityFixture struct {
	accountA, accountB, familyA, familyB, deviceA string
}

func seedAuthorityFamilies(t *testing.T, f *testpg.Fixture) authorityFixture {
	t.Helper()
	ids := authorityFixture{
		accountA: "61000000-0000-4000-8000-000000000001", accountB: "61000000-0000-4000-8000-000000000002",
		familyA: "62000000-0000-4000-8000-000000000001", familyB: "62000000-0000-4000-8000-000000000002",
		deviceA: "63000000-0000-4000-8000-000000000001",
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, step := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO accounts(id,display_name,status) VALUES ($1,'A','active'),($2,'B','active')`, []any{ids.accountA, ids.accountB}},
		{`INSERT INTO families(id,display_name,status,owner_account_id) VALUES ($1,'Family A','active',$2),($3,'Family B','active',$4)`, []any{ids.familyA, ids.accountA, ids.familyB, ids.accountB}},
		{`INSERT INTO memberships(family_id,account_id,role,status) VALUES ($1,$2,'owner','active'),($3,$4,'owner','active')`, []any{ids.familyA, ids.accountA, ids.familyB, ids.accountB}},
		{`INSERT INTO family_slots(account_id,family_id,state) VALUES ($1,$2,'active'),($3,$4,'active')`, []any{ids.accountA, ids.familyA, ids.accountB, ids.familyB}},
		{`INSERT INTO devices(id,account_id,canonical_origin,continuity_key_id,continuity_digest,created_at,updated_at)
			VALUES ($1,$2,'https://tracker.example','continuity.v1',$3,$4,$4)`, []any{ids.deviceA, ids.accountA, authorityDigest(7), now}},
		{`INSERT INTO invitations(id,family_id,creator_account_id,token_digest,role,status,expires_at,created_at,updated_at) VALUES
			('64000000-0000-4000-8000-000000000001',$1,$2,'hmac-sha256:invite.v1:' || repeat('a',64),'caregiver','active',$5,$6,$6),
			('64000000-0000-4000-8000-000000000002',$3,$4,'hmac-sha256:invite.v1:' || repeat('b',64),'caregiver','active',$5,$6,$6)`,
			[]any{ids.familyA, ids.accountA, ids.familyB, ids.accountB, now.Add(time.Hour), now}},
		{`INSERT INTO family_entitlements(family_id,entitlement_key,source,status,created_at,updated_at) VALUES
			($1,'family_sync','hosted_beta','active',$3,$3),($2,'family_sync','hosted_beta','active',$3,$3)`, []any{ids.familyA, ids.familyB, now}},
	} {
		if err := authorityExec(t.Context(), f.Owner, step.sql, step.args...); err != nil {
			t.Fatalf("seed %q: %v", step.sql, err)
		}
	}
	return ids
}

func tenantTx(t *testing.T, pool *pgxpool.Pool, account, family string) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: account, FamilyID: family}); err != nil {
		t.Fatal(err)
	}
	return tx
}

// Replaces TestRuntimeCursorColumnGrantDoesNotRestoreStructuralWrites for
// schema 29: tenant context may not touch authority state, while sync's
// next_sequence and app.update_own_profile keep working.
func TestAuthorityColumnsRejectTenantWrites(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f)
	familyColumns := map[string]string{
		"status": "'deletion_pending'", "owner_account_id": "'" + ids.accountB + "'", "display_name": "'Renamed'",
		"profile_required": "true", "purge_after": "now()", "purge_started_at": "now()",
		"purge_command_id": "'65000000-0000-4000-8000-000000000001'", "purge_receipt": "'65000000-0000-4000-8000-000000000002'",
		"purged_at": "now()", "audit_seq": "1", "audit_hash": "decode(repeat('11',32),'hex')", "chain_failed": "true",
		"entitlement_required": "true",
	}
	accountColumns := map[string]string{
		"status": "'deleted'", "lifecycle_generation": "2", "audit_seq": "1",
		"audit_hash": "decode(repeat('11',32),'hex')", "chain_failed": "true",
	}
	for _, pool := range []struct {
		name string
		pool *pgxpool.Pool
	}{{"runtime", f.Runtime}, {"owner", f.Owner}} {
		for column, value := range familyColumns {
			t.Run(pool.name+"/families."+column, func(t *testing.T) {
				tx := tenantTx(t, pool.pool, ids.accountA, ids.familyA)
				_, err := tx.Exec(t.Context(), "UPDATE families SET "+column+"="+value+" WHERE id=$1", ids.familyA)
				if sqlState(err) != "42501" {
					t.Fatalf("tenant families.%s err=%v", column, err)
				}
			})
		}
		for column, value := range accountColumns {
			t.Run(pool.name+"/accounts."+column, func(t *testing.T) {
				tx := tenantTx(t, pool.pool, ids.accountA, ids.familyA)
				_, err := tx.Exec(t.Context(), "UPDATE accounts SET "+column+"="+value+" WHERE id=$1", ids.accountA)
				if sqlState(err) != "42501" {
					t.Fatalf("tenant accounts.%s err=%v", column, err)
				}
			})
		}
	}

	// Authority transactions may write the same columns.
	if err := authorityExec(t.Context(), f.Owner, "UPDATE families SET chain_failed=true, entitlement_required=true WHERE id=$1", ids.familyA); err != nil {
		t.Fatalf("authority family write: %v", err)
	}
	if err := authorityExec(t.Context(), f.Owner, "UPDATE accounts SET chain_failed=true WHERE id=$1", ids.accountA); err != nil {
		t.Fatalf("authority account write: %v", err)
	}

	tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	var next int64
	if err := tx.QueryRow(t.Context(), "UPDATE families SET next_sequence=next_sequence+1, updated_at=now() WHERE id=$1 RETURNING next_sequence", ids.familyA).Scan(&next); err != nil || next != 1 {
		t.Fatalf("tenant next_sequence=%d err=%v", next, err)
	}
	if _, err := tx.Exec(t.Context(), "SELECT app.update_own_profile($1,'66000000-0000-4000-8000-000000000001','Renamed A',now())", ids.accountA); err != nil {
		t.Fatalf("update_own_profile: %v", err)
	}
	deleted, err := tx.Exec(t.Context(), "DELETE FROM family_slots WHERE account_id=$1", ids.accountA)
	if err != nil || deleted.RowsAffected() != 0 {
		t.Fatalf("tenant slot delete rows=%d err=%v", deleted.RowsAffected(), err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	for name, statement := range map[string]string{
		"accounts insert":     "INSERT INTO accounts(id,display_name) VALUES ('" + ids.accountA[:35] + "9','Other')",
		"families insert":     "INSERT INTO families(id,display_name,owner_account_id) VALUES ('62000000-0000-4000-8000-000000000007','Again','" + ids.accountA + "')",
		"family_slots insert": "INSERT INTO family_slots(account_id,family_id,state) VALUES ('" + ids.accountA + "','" + ids.familyB + "','active')",
		"family_slots update": "UPDATE family_slots SET state='family_recovery' WHERE account_id='" + ids.accountA + "'",
	} {
		t.Run(name, func(t *testing.T) {
			tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
			result, err := tx.Exec(t.Context(), statement)
			if sqlState(err) != "42501" && !(err == nil && result.RowsAffected() == 0) {
				t.Fatalf("tenant %s rows=%d err=%v", name, result.RowsAffected(), err)
			}
		})
	}
	// The owner pool (table owner, FORCE RLS) is also held to the policies.
	ownerTx := tenantTx(t, f.Owner, ids.accountA, ids.familyA)
	for statement, id := range map[string]string{"DELETE FROM families WHERE id=$1": ids.familyA, "DELETE FROM accounts WHERE id=$1": ids.accountA} {
		result, err := ownerTx.Exec(t.Context(), statement, id)
		if err != nil || result.RowsAffected() != 0 {
			t.Fatalf("owner tenant %q rows=%d err=%v", statement, result.RowsAffected(), err)
		}
	}
	var slots int
	if err := f.Runtime.QueryRow(t.Context(), "SELECT count(*) FROM family_slots").Scan(&slots); err != nil || slots != 0 {
		t.Fatalf("runtime without context sees %d slots err=%v", slots, err)
	}
}

// app.authority is a setting the runtime login can raise, not a privilege:
// even inside an authority transaction the runtime may only move an audit
// chain head forward (an append advances audit_seq with its new hash; the
// verifier sets chain_failed). Clearing chain_failed, lowering audit_seq or
// changing audit_hash without advancing audit_seq is refused with 42501;
// the table owner may still do it (operator recovery after investigation).
func TestAuditChainHeadsOnlyMoveForward(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f)
	for _, scope := range []struct{ table, id string }{{"families", ids.familyA}, {"accounts", ids.accountA}} {
		t.Run(scope.table, func(t *testing.T) {
			run := func(pool *pgxpool.Pool, statement string, args ...any) error {
				return authorityExec(t.Context(), pool, "UPDATE "+scope.table+" "+statement+" WHERE id=$1", append([]any{scope.id}, args...)...)
			}
			// Normal appends, one and then several rows ahead.
			if err := run(f.Runtime, "SET audit_seq=audit_seq+1, audit_hash=$2", authorityDigest(1)); err != nil {
				t.Fatalf("append: %v", err)
			}
			if err := run(f.Runtime, "SET audit_seq=audit_seq+2, audit_hash=$2", authorityDigest(2)); err != nil {
				t.Fatalf("append two: %v", err)
			}
			// Rewriting the columns to their current values is no regression.
			if err := run(f.Runtime, "SET audit_seq=audit_seq, audit_hash=audit_hash, chain_failed=chain_failed"); err != nil {
				t.Fatalf("unchanged head: %v", err)
			}
			for name, statement := range map[string]string{
				"lower seq":           "SET audit_seq=audit_seq-1",
				"lower seq with hash": "SET audit_seq=audit_seq-1, audit_hash=$2",
				"hash without seq":    "SET audit_hash=$2",
				"rewind to genesis":   "SET audit_seq=0, audit_hash=decode(repeat('00',32),'hex')",
			} {
				var args []any
				if strings.Contains(statement, "$2") {
					args = []any{authorityDigest(9)}
				}
				if err := run(f.Runtime, statement, args...); sqlState(err) != "42501" {
					t.Fatalf("%s: err=%v, want 42501", name, err)
				}
			}
			if err := run(f.Runtime, "SET chain_failed=true"); err != nil {
				t.Fatalf("verifier marks the chain failed: %v", err)
			}
			if err := run(f.Runtime, "SET chain_failed=false"); sqlState(err) != "42501" {
				t.Fatalf("runtime cleared chain_failed: err=%v, want 42501", err)
			}
			var seq int64
			var failed bool
			tx, err := f.Owner.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := tx.Exec(t.Context(), "SELECT set_config('app.authority','1',true)"); err != nil {
				t.Fatal(err)
			}
			if err := tx.QueryRow(t.Context(), "SELECT audit_seq, chain_failed FROM "+scope.table+" WHERE id=$1", scope.id).Scan(&seq, &failed); err != nil || seq != 3 || !failed {
				t.Fatalf("head after refusals seq=%d failed=%t err=%v", seq, failed, err)
			}
			_ = tx.Rollback(t.Context())
			// The table owner recovers a failed chain explicitly.
			if err := run(f.Owner, "SET chain_failed=false"); err != nil {
				t.Fatalf("owner recovery: %v", err)
			}
			if err := run(f.Owner, "SET audit_seq=0, audit_hash=decode(repeat('00',32),'hex')"); err != nil {
				t.Fatalf("owner rewind: %v", err)
			}
		})
	}
}

func TestAuthorityContextIsFalseWhenUnset(t *testing.T) {
	f := testpg.New(t)
	tx, err := f.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var unset, set, cleared *bool
	if err := tx.QueryRow(t.Context(), "SELECT app.authority_context()").Scan(&unset); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "SELECT set_config('app.authority','1',true)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(t.Context(), "SELECT app.authority_context()").Scan(&set); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), "SELECT set_config('app.authority','',true)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(t.Context(), "SELECT app.authority_context()").Scan(&cleared); err != nil {
		t.Fatal(err)
	}
	if unset == nil || *unset || set == nil || !*set || cleared == nil || *cleared {
		t.Fatalf("authority_context unset=%v set=%v cleared=%v", unset, set, cleared)
	}
}

func TestFamiliesActiveRequireOwner(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f)
	err := authorityExec(t.Context(), f.Owner, "INSERT INTO families(id,display_name,status) VALUES ('62000000-0000-4000-8000-000000000009','No owner','active')")
	if sqlState(err) != "23514" {
		t.Fatalf("active family without owner err=%v", err)
	}
	if err := authorityExec(t.Context(), f.Owner, "INSERT INTO families(id,display_name,status) VALUES ('62000000-0000-4000-8000-000000000008','Pending','deletion_pending')"); err != nil {
		t.Fatalf("inactive family without owner: %v", err)
	}
	if err := authorityExec(t.Context(), f.Owner, "UPDATE families SET status='active' WHERE id='62000000-0000-4000-8000-000000000008'"); sqlState(err) != "23514" {
		t.Fatalf("reactivation without owner err=%v", err)
	}
	if err := authorityExec(t.Context(), f.Owner, "UPDATE families SET owner_account_id=NULL WHERE id=$1", ids.familyA); sqlState(err) != "23514" {
		t.Fatalf("clearing active owner err=%v", err)
	}
}

var authorityOnlyTables = []string{"sessions", "account_identities", "refresh_receipts", "auth_challenges", "apple_notifications",
	"command_receipts", "security_audit_events", "audit_checkpoints", "mcp_integrations"}

// authorityOnlyRows returns one valid INSERT per authority-only table; n
// varies every key so the statements can be run more than once.
func authorityOnlyRows(ids authorityFixture, n byte) map[string]struct {
	sql  string
	args []any
} {
	now := time.Now().UTC().Truncate(time.Second)
	id := func(prefix string) string { return fmt.Sprintf("%s000000-0000-4000-8000-0000000000%02x", prefix, n) }
	parent, child := id("71"), id("72")
	return map[string]struct {
		sql  string
		args []any
	}{
		"sessions": {`INSERT INTO sessions(access_digest,id,account_id,device_id,lineage_root_id,lineage_started_at,scope,recovery_epoch,issued_seq,token_key_id,
			refresh_digest,access_expires_at,refresh_expires_at,deadline_at,created_at) VALUES
			($1,$2,$3,$4,$2,$5,'account',1,1,'token.v1',$6,$5::timestamptz+interval '15 minutes',$5::timestamptz+interval '30 days',$5::timestamptz+interval '90 days',$5),
			($7,$8,$3,$4,$2,$5,'account',1,1,'token.v1',$9,$5::timestamptz+interval '15 minutes',$5::timestamptz+interval '30 days',$5::timestamptz+interval '90 days',$5)`,
			[]any{authorityDigest(0x10 + n), parent, ids.accountA, ids.deviceA, now, authorityDigest(0x20 + n), authorityDigest(0x30 + n), child, authorityDigest(0x40 + n)}},
		"account_identities": {`INSERT INTO account_identities(provider,identity_key_id,identity_digest,account_id,lifecycle_generation,audience,subject,created_at)
			VALUES ('apple','identity.v1',$1,$2,$3,'com.example.tracker','subject',$4)`, []any{authorityDigest(0x50 + n), ids.accountA, int64(n), now}},
		"refresh_receipts": {`INSERT INTO refresh_receipts(parent_id,child_id,attempt_id,account_id,device_id,material,material_digest,created_at,effective_deadline,refresh_expires_at)
			VALUES ($1,$2,$3,$4,$5,'\x01'::bytea,sha256('\x01'::bytea),$6,$6,$6)`, []any{parent, child, id("73"), ids.accountA, ids.deviceA, now}},
		"auth_challenges":     {`INSERT INTO auth_challenges(token_digest,expires_at,created_at) VALUES ($1,$2,$2)`, []any{authorityDigest(0x60 + n), now}},
		"apple_notifications": {`INSERT INTO apple_notifications(notification_key_id,notification_digest,command_id,received_at) VALUES ('notification.v1',$1,$2,$3)`, []any{authorityDigest(0x70 + n), id("74"), now}},
		"command_receipts": {`INSERT INTO command_receipts(actor_account_id,command_id,command_hash,kind,result,created_at)
			VALUES ($1,$2,$3,'family_created','{}',$4)`, []any{ids.accountA, id("75"), authorityDigest(0x80 + n), now}},
		"security_audit_events": {`INSERT INTO security_audit_events(scope,scope_id,seq,prev_hash,hash,kind,command_id,actor_account_id,at,payload)
			VALUES ('family',$1,$2,$3,$4,'family_created',$5,$6,$7,'{}')`, []any{ids.familyA, int64(n), authorityDigest(0), authorityDigest(0x90 + n), id("76"), ids.accountA, now}},
		"audit_checkpoints": {`INSERT INTO audit_checkpoints(scope,scope_id,verified_seq,verified_hash,verified_at) VALUES ('account',$1,1,$2,$3)`,
			[]any{id("77"), authorityDigest(0xa0 + n), now}},
		"mcp_integrations": {`INSERT INTO mcp_integrations(id,account_id,family_id,recovery_epoch,name,scope,verifier,created_at,expires_at)
			VALUES ($1,$2,$3,1,'Claude','family:read',$4,$5,$5::timestamptz + interval '7776000 seconds')`, []any{id("78"), ids.accountA, ids.familyA, authorityDigest(0xb0 + n), now}},
	}
}

func TestAuthorityRLSShape(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f)

	// Catalog: FORCE survives the fresh-install guard; authority-only tables
	// carry exactly one authority policy.
	all := append([]string{"accounts", "families", "memberships", "devices", "family_slots", "invitations", "family_entitlements", "family_recovery_claims"}, authorityOnlyTables...)
	for _, table := range all {
		var enabled, forced bool
		if err := f.Owner.QueryRow(t.Context(), "SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid=($1::text)::regclass", "public."+table).Scan(&enabled, &forced); err != nil {
			t.Fatal(err)
		}
		if !enabled || !forced {
			t.Errorf("%s RLS enabled=%t forced=%t", table, enabled, forced)
		}
	}
	for _, table := range authorityOnlyTables {
		rows, err := f.Owner.Query(t.Context(), `SELECT polname, polcmd::text, polpermissive, pg_get_expr(polqual, polrelid), pg_get_expr(polwithcheck, polrelid)
			FROM pg_policy WHERE polrelid=($1::text)::regclass`, "public."+table)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for rows.Next() {
			var name, command, qual, check string
			var permissive bool
			if err := rows.Scan(&name, &command, &permissive, &qual, &check); err != nil {
				t.Fatal(err)
			}
			count++
			if name != "authority_only" || command != "*" || !permissive || qual != "app.authority_context()" || check != "app.authority_context()" {
				t.Errorf("%s policy %s cmd=%s permissive=%t using=%q check=%q", table, name, command, permissive, qual, check)
			}
		}
		rows.Close()
		if count != 1 {
			t.Errorf("%s has %d policies", table, count)
		}
	}

	// Behaviour: seeded rows are invisible and unwritable without the flag,
	// even with a tenant context for the owning account and family.
	for _, table := range []string{"sessions", "account_identities", "auth_challenges", "apple_notifications", "command_receipts", "security_audit_events", "audit_checkpoints", "mcp_integrations"} {
		row := authorityOnlyRows(ids, 1)[table]
		if err := authorityExec(t.Context(), f.Owner, row.sql, row.args...); err != nil {
			t.Fatalf("seed %s: %v", table, err)
		}
	}
	row := authorityOnlyRows(ids, 1)["refresh_receipts"]
	if err := authorityExec(t.Context(), f.Owner, row.sql, row.args...); err != nil {
		t.Fatalf("seed refresh_receipts: %v", err)
	}
	for _, table := range authorityOnlyTables {
		var authorityCount int
		tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
		var tenantCount int
		if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&tenantCount); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), "SELECT set_config('app.authority','1',true)"); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&authorityCount); err != nil {
			t.Fatal(err)
		}
		if tenantCount != 0 || authorityCount == 0 {
			t.Errorf("%s tenant rows=%d authority rows=%d", table, tenantCount, authorityCount)
		}
		insert := authorityOnlyRows(ids, 2)[table]
		denied := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
		if _, err := denied.Exec(t.Context(), insert.sql, insert.args...); sqlState(err) != "42501" {
			t.Errorf("%s tenant insert err=%v", table, err)
		}
	}

	// Family A's context never reads family B's rows.
	tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	for table, column := range map[string]string{"families": "id", "memberships": "family_id", "invitations": "family_id", "family_entitlements": "family_id"} {
		var foreign, own int
		if err := tx.QueryRow(t.Context(), "SELECT count(*) FILTER (WHERE "+column+"=$1), count(*) FILTER (WHERE "+column+"=$2) FROM "+table, ids.familyB, ids.familyA).Scan(&foreign, &own); err != nil {
			t.Fatal(err)
		}
		if foreign != 0 || own == 0 {
			t.Errorf("%s family A context foreign=%d own=%d", table, foreign, own)
		}
	}
	for table, column := range map[string]string{"accounts": "id", "family_slots": "account_id", "devices": "account_id"} {
		var foreign int
		if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM "+table+" WHERE "+column+"<>$1", ids.accountA).Scan(&foreign); err != nil {
			t.Fatal(err)
		}
		if foreign != 0 {
			t.Errorf("%s account A context sees %d foreign rows", table, foreign)
		}
	}
}

// Actor labels (Task 4 amendment): authority transactions write them through
// app.authority_actor_label; the runtime keeps no write grant and no direct
// history read, the definer refuses tenant context, and app.update_own_profile
// still renames the caller's own label.
func TestActorLabelsAreWrittenByTheAuthorityDefinerOnly(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f)
	tx, err := f.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	for _, call := range []struct {
		sql  string
		args []any
	}{
		{`SELECT set_config('app.authority','1',true)`, nil},
		{`SELECT app.authority_actor_label($1,$2,false,now())`, []any{ids.familyA, ids.accountA}},
		{`SELECT app.authority_actor_label($1,$2,false,now())`, []any{ids.familyB, ids.accountB}},
		// The definer follows the membership its caller just wrote.
		{`UPDATE memberships SET status='removed' WHERE family_id=$1 AND account_id=$2`, []any{ids.familyB, ids.accountB}},
		{`SELECT app.authority_actor_label($1,$2,true,now())`, []any{ids.familyB, ids.accountB}},
		{`UPDATE memberships SET status='active' WHERE family_id=$1 AND account_id=$2`, []any{ids.familyB, ids.accountB}},
		{`SELECT app.authority_actor_label($1,$2,false,now())`, []any{ids.familyB, ids.accountB}},
	} {
		if _, err := tx.Exec(t.Context(), call.sql, call.args...); err != nil {
			t.Fatalf("authority %q: %v", call.sql, err)
		}
	}
	var labelB string
	var formerB bool
	var historyB, formerHistoryB int
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.Owner.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE former AND display_name='Former caregiver') FROM family_actor_label_history
		WHERE family_id=$1 AND account_id=$2`, ids.familyB, ids.accountB).Scan(&historyB, &formerHistoryB); err != nil {
		t.Fatal(err)
	}
	// The owner pool is held to FORCE RLS and sees nothing without a context.
	if historyB != 0 {
		t.Fatalf("owner without context reads %d history rows", historyB)
	}
	ownerTx := tenantTx(t, f.Owner, ids.accountB, ids.familyB)
	if err := ownerTx.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE former AND display_name='Former caregiver') FROM family_actor_label_history
		WHERE family_id=$1 AND account_id=$2`, ids.familyB, ids.accountB).Scan(&historyB, &formerHistoryB); err != nil {
		t.Fatal(err)
	}
	if err := ownerTx.QueryRow(t.Context(), `SELECT display_name, former FROM family_actor_labels WHERE family_id=$1 AND account_id=$2`, ids.familyB, ids.accountB).
		Scan(&labelB, &formerB); err != nil || labelB != "B" || formerB {
		t.Fatalf("rejoined label %q former=%v err=%v", labelB, formerB, err)
	}
	// join, leave (rewrites the join row and adds a former row), rejoin.
	if historyB != 3 || formerHistoryB != 2 {
		t.Fatalf("history rows=%d former=%d, want 3/2", historyB, formerHistoryB)
	}

	denied := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	if _, err := denied.Exec(t.Context(), `SELECT app.authority_actor_label($1,$2,true,now())`, ids.familyA, ids.accountA); sqlState(err) != "42501" {
		t.Fatalf("tenant definer call err=%v", err)
	}
	// A label needs the matching membership: a non-member gets no active
	// label, and an active member cannot be marked former.
	for name, args := range map[string][]any{
		"non-member label":     {ids.familyA, ids.accountB, false},
		"non-member former":    {ids.familyA, ids.accountB, true},
		"active member former": {ids.familyA, ids.accountA, true},
	} {
		tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
		if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), `SELECT app.authority_actor_label($1,$2,$3,now())`, args...); sqlState(err) != "23514" {
			t.Fatalf("%s err=%v", name, err)
		}
	}
	for name, statement := range map[string]string{
		"label insert":   `INSERT INTO family_actor_labels(family_id,account_id,display_name,former,created_at,updated_at) VALUES ('` + ids.familyA + `','` + ids.accountB + `','X',false,now(),now())`,
		"label update":   `UPDATE family_actor_labels SET display_name='X' WHERE family_id='` + ids.familyA + `'`,
		"history read":   `SELECT count(*) FROM family_actor_label_history`,
		"history insert": `INSERT INTO family_actor_label_history(family_id,account_id,display_name,former,recorded_at) VALUES ('` + ids.familyA + `','` + ids.accountA + `','X',false,now())`,
	} {
		t.Run(name, func(t *testing.T) {
			for _, authority := range []bool{false, true} {
				tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
				if authority {
					if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := tx.Exec(t.Context(), statement); sqlState(err) != "42501" {
					t.Fatalf("runtime %s (authority=%v) err=%v", name, authority, err)
				}
			}
		})
	}

	profile := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
	if _, err := profile.Exec(t.Context(), "SELECT app.update_own_profile($1,'66000000-0000-4000-8000-000000000009','Renamed A',now())", ids.accountA); err != nil {
		t.Fatalf("update_own_profile: %v", err)
	}
	var label string
	if err := profile.QueryRow(t.Context(), `SELECT display_name FROM family_actor_labels WHERE family_id=$1 AND account_id=$2`, ids.familyA, ids.accountA).
		Scan(&label); err != nil || label != "Renamed A" {
		t.Fatalf("after update_own_profile label=%q err=%v", label, err)
	}
}

// Task 1 deferred minor: memberships, invitations, family_entitlements,
// family_recovery_claims and devices keep one ALL policy whose USING admits a
// tenant, so a tenant DELETE passes RLS and only the missing runtime DELETE
// grant closes it. Pin both halves: those tables grant the runtime no
// DELETE, and every authority table the runtime may DELETE from has no
// DELETE-applicable policy that admits a non-authority context. A future
// DELETE grant on any of the five therefore fails here until its policy is
// split the way family_slots' is.
func TestRuntimeHasNoDeleteWhereAPolicyAdmitsTenantDelete(t *testing.T) {
	f := testpg.New(t)
	ids := seedAuthorityFamilies(t, f)
	for _, table := range []string{"memberships", "invitations", "family_entitlements", "family_recovery_claims", "devices"} {
		var grant bool
		if err := f.Owner.QueryRow(t.Context(), `SELECT has_table_privilege('baby_tracker_runtime',$1,'DELETE')`, "public."+table).Scan(&grant); err != nil {
			t.Fatal(err)
		}
		if grant {
			t.Errorf("runtime holds DELETE on %s, whose policy admits a tenant DELETE", table)
		}
		// Behaviour, with and without the authority flag.
		for _, authority := range []bool{false, true} {
			tx := tenantTx(t, f.Runtime, ids.accountA, ids.familyA)
			if authority {
				if _, err := tx.Exec(t.Context(), `SELECT set_config('app.authority','1',true)`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.Exec(t.Context(), "DELETE FROM public."+table); sqlState(err) != "42501" {
				t.Errorf("runtime DELETE on %s (authority=%t) err=%v", table, authority, err)
			}
			// Release any row locks before the next attempt reaches the same rows.
			_ = tx.Rollback(t.Context())
		}
	}

	all := append([]string{"accounts", "families", "memberships", "devices", "family_slots", "invitations", "family_entitlements", "family_recovery_claims"}, authorityOnlyTables...)
	var deletable []string
	for _, table := range all {
		var grant bool
		if err := f.Owner.QueryRow(t.Context(), `SELECT has_table_privilege('baby_tracker_runtime',$1,'DELETE')`, "public."+table).Scan(&grant); err != nil {
			t.Fatal(err)
		}
		if !grant {
			continue
		}
		deletable = append(deletable, table)
		rows, err := f.Owner.Query(t.Context(), `SELECT polname, pg_get_expr(polqual, polrelid) FROM pg_policy
			WHERE polrelid=($1::text)::regclass AND polcmd IN ('*','d') AND polpermissive`, "public."+table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name, using string
			if err := rows.Scan(&name, &using); err != nil {
				t.Fatal(err)
			}
			if using != "app.authority_context()" {
				t.Errorf("runtime may DELETE from %s and policy %s admits USING %s", table, name, using)
			}
		}
		rows.Close()
	}
	sort.Strings(deletable)
	if want := []string{"auth_challenges", "family_slots", "refresh_receipts", "sessions"}; !equalStrings(deletable, want) {
		t.Fatalf("runtime DELETE on authority tables=%v want=%v", deletable, want)
	}
}
