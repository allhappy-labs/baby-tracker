package database_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"babytracker/server/internal/database"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var identityRuntimeFunctions = []string{
	"app.consume_auth_challenge(bytea,text,uuid,timestamp with time zone)",
	"app.create_auth_challenge(bytea,text,uuid,timestamp with time zone,timestamp with time zone)",
	"app.lookup_access_session(text,bytea)",
	"app.lookup_auth_identity(text,text,bytea)",
	"app.lookup_device_binding(uuid,uuid,uuid,text)",
	"app.lookup_refresh_session(text,bytea)",
	"app.reserve_apple_notification(jsonb,text,bytea,uuid,timestamp with time zone)",
	"app.stage_auth_material(uuid,bytea,bytea,bytea,timestamp with time zone)",
	"app.take_rate_limits(jsonb,timestamp with time zone)",
}

var familyRuntimeFunctions = []string{
	"app.current_account_profile(uuid)",
	"app.family_actor_labels_high_water()",
	"app.list_family_actor_labels(uuid,bigint,uuid,integer)",
	"app.lookup_family_invitation(text)",
	"app.stage_family_material(uuid,bytea,bytea,bytea,timestamp with time zone)",
	"app.update_own_profile(uuid,uuid,text,timestamp with time zone)",
}

var identityTables = []string{
	"account_identities", "apple_notification_dedup", "apple_revocation_jobs",
	"auth_challenges", "auth_exchange_material", "devices", "rate_limit_buckets",
	"refresh_recovery_receipts", "session_refresh_history", "sessions",
}

var identityTableColumns = map[string][]string{
	"refresh_recovery_receipts": {"server_instance_id", "recovery_epoch", "account_id", "device_id", "parent_session_id", "attempt_id", "child_session_id", "material_bytes", "projection_material_digest", "parent_key_id", "output_key_id", "continuity_key_id", "created_at", "effective_deadline", "refresh_expires_at"},
	"account_identities":        {"provider", "identity_key_id", "identity_digest", "account_id", "identity_audience", "identity_subject", "apple_credential_id", "apple_credential_key_id", "apple_credential_purpose", "apple_credential_object_id", "material_missing", "created_at", "updated_at"},
	"devices":                   {"id", "account_id", "server_instance_id", "canonical_origin", "installation_key_id", "installation_digest", "continuity_key_id", "continuity_digest", "binding_revoked_at", "created_at", "updated_at"},
	"auth_challenges":           {"id", "token_digest", "canonical_origin", "server_instance_id", "expires_at", "consumed_at", "created_at"},
	"auth_exchange_material":    {"command_id", "command_hash", "projection_material_digest", "material_kind", "account_id", "device_id", "session_id", "parent_session_id", "lineage_root_session_id", "server_instance_id", "canonical_origin", "token_key_id", "access_digest", "refresh_digest", "recovery_epoch", "security_revision", "scope", "family_id", "access_expires_at", "refresh_expires_at", "reauthenticate_at", "absolute_expires_at", "identity_provider", "identity_key_id", "identity_digest", "identity_audience", "identity_subject", "display_name", "profile_confirmed", "register_account", "register_device", "installation_key_id", "installation_digest", "continuity_key_id", "continuity_digest", "apple_credential_id", "apple_credential_key_id", "apple_credential_purpose", "apple_credential_object_id", "created_at", "expires_at", "projected_at", "recovery_material_bytes"},
	"sessions":                  {"session_id", "account_id", "device_id", "lineage_root_session_id", "parent_session_id", "server_instance_id", "recovery_epoch", "security_revision", "scope", "family_id", "token_key_id", "access_digest", "refresh_digest", "access_expires_at", "refresh_expires_at", "reauthenticate_at", "absolute_expires_at", "status", "consumed_at", "revoked_at", "revoke_reason", "created_at", "updated_at"},
	"session_refresh_history":   {"token_key_id", "refresh_digest", "session_id", "lineage_root_session_id", "consumed_at", "reuse_detected_at"},
	"apple_notification_dedup":  {"notification_key_id", "notification_digest", "command_id", "received_at"},
	"apple_revocation_jobs":     {"job_id", "account_id", "server_instance_id", "credential_purpose", "credential_object_id", "credential_reference_id", "credential_reference_key_id", "status", "attempts", "next_attempt_at", "created_at", "updated_at", "completed_at", "last_error_code", "deletion_operation_id", "deletion_command_id", "operation_version", "provider_revoked_at", "cleanup_started_at", "lease_owner", "lease_token", "lease_expires_at"},
	"rate_limit_buckets":        {"scope", "key_digest", "window_start", "window_end", "count"},
}

// Dropping any reserved table/function or widening either application's
// effective privilege set must fail the current exact schema-twenty-one readiness contract.
func TestIdentitySchemaHasExactApplicationPrivileges(t *testing.T) {
	fixture := testpg.New(t)
	if err := database.VerifyRoleSeparation(t.Context(), fixture.Runtime, fixture.Projector, 29); err != nil {
		t.Fatalf("schema-twenty-one exact privilege snapshot rejected: %v", err)
	}
	var version int64
	if err := fixture.Runtime.QueryRow(t.Context(), "SELECT current_version FROM schema_compatibility WHERE singleton").Scan(&version); err != nil || version != 29 {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	rows, err := fixture.Owner.Query(t.Context(), `SELECT tablename FROM pg_tables
		WHERE schemaname='public' AND tablename = ANY($1) ORDER BY tablename`, identityTables)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	sort.Strings(identityTables)
	if !equalStrings(tables, identityTables) {
		t.Fatalf("identity tables=%v want=%v", tables, identityTables)
	}
	var sequences []string
	rows, err = fixture.Owner.Query(t.Context(), `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname='public' AND c.relkind='S' ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var sequence string
		if err := rows.Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		sequences = append(sequences, sequence)
	}
	rows.Close()
	if !equalStrings(sequences, []string{"family_actor_label_history_label_version_seq"}) {
		t.Fatalf("public sequences=%v", sequences)
	}
	for _, role := range []string{"baby_tracker_runtime", "baby_tracker_projector"} {
		var usable, selectable bool
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT has_sequence_privilege($1,
			'public.family_actor_label_history_label_version_seq','USAGE'),has_sequence_privilege($1,
			'public.family_actor_label_history_label_version_seq','SELECT')`, role).Scan(&usable, &selectable); err != nil {
			t.Fatal(err)
		}
		if usable || selectable {
			t.Fatalf("%s has sequence usage=%t select=%t", role, usable, selectable)
		}
	}
	var publicSequencePrivilege bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT EXISTS (
		SELECT FROM pg_class c CROSS JOIN LATERAL aclexplode(coalesce(c.relacl,acldefault('S',c.relowner))) acl
		WHERE c.oid='public.family_actor_label_history_label_version_seq'::regclass AND acl.grantee=0)`).Scan(&publicSequencePrivilege); err != nil {
		t.Fatal(err)
	}
	if publicSequencePrivilege {
		t.Fatal("PUBLIC has actor-label history sequence privileges")
	}
	for _, signature := range identityRuntimeFunctions {
		assertFunctionPrivilege(t, fixture.Runtime, signature, true)
		assertFunctionPrivilege(t, fixture.Projector, signature, false)
		assertPublicCannotExecute(t, fixture, signature)
	}
	projectorFunctions := []string{
		"app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)",
		"app.update_server_state(uuid,bigint,bigint,bigint,bytea)",
	}
	for _, signature := range projectorFunctions {
		assertFunctionPrivilege(t, fixture.Runtime, signature, false)
		assertFunctionPrivilege(t, fixture.Projector, signature, true)
		assertPublicCannotExecute(t, fixture, signature)
	}
	allRuntimeFunctions := append([]string{
		"app.current_account_id()",
		"app.current_family_id()",
	}, identityRuntimeFunctions...)
	allRuntimeFunctions = append(allRuntimeFunctions, familyRuntimeFunctions...)
	allRuntimeFunctions = append(allRuntimeFunctions, "app.fence_live_activity_installation(uuid,bigint,uuid,uuid,uuid,uuid,bigint,timestamp with time zone,boolean)", "app.live_activity_session_binding(uuid,uuid,uuid,bigint)", "app.live_activity_claim_live(jsonb)", "app.live_activity_registration_subjects(uuid,integer)", "app.live_activity_registration_state(uuid,jsonb)")
	allRuntimeFunctions = append(allRuntimeFunctions, "app.lookup_refresh_recovery_receipt(uuid,uuid)", "app.lookup_refresh_recovery_child(uuid)", "app.lookup_refresh_recovery_ack(uuid,uuid,uuid,bigint,uuid)", "app.ack_refresh_recovery_receipt(uuid,uuid,uuid,uuid,bigint,uuid)", "app.refresh_recovery_key_candidates()")
	allRuntimeFunctions = append(allRuntimeFunctions, "app.export_family_audit(uuid)")
	allRuntimeFunctions = append(allRuntimeFunctions, "app.session_lineage_started_at(uuid,uuid)")
	allRuntimeFunctions = append(allRuntimeFunctions, recordRuntimeFunctions...)
	allRuntimeFunctions = append(allRuntimeFunctions, snapshotRuntimeFunctions...)
	allRuntimeFunctions = append(allRuntimeFunctions, "app.import_unfinished_session_count(uuid,timestamp with time zone)")
	allRuntimeFunctions = append(allRuntimeFunctions, deliveryScheduleSignature)
	allRuntimeFunctions = append(allRuntimeFunctions, membershipMayWriteSignature)
	assertExactAppExecutables(t, fixture, "baby_tracker_runtime", allRuntimeFunctions)
	assertExactAppExecutables(t, fixture, "baby_tracker_projector", projectorFunctions)
	rows, err = fixture.Owner.Query(t.Context(), `SELECT object_identity FROM schema_privilege_manifest WHERE schema_version=5
		AND principal='baby_tracker_runtime' AND object_kind='function'
		EXCEPT SELECT object_identity FROM schema_privilege_manifest WHERE schema_version=4
		AND principal='baby_tracker_runtime' AND object_kind='function' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var added []string
	for rows.Next() {
		var signature string
		if err := rows.Scan(&signature); err != nil {
			t.Fatal(err)
		}
		added = append(added, signature)
	}
	rows.Close()
	if !equalStrings(added, identityRuntimeFunctions) {
		t.Fatalf("schema-five runtime function delta=%v want exact nine=%v", added, identityRuntimeFunctions)
	}
}

func TestIdentityTablesHaveExactColumnsAndCriticalIndexes(t *testing.T) {
	fixture := testpg.New(t)
	for table, want := range identityTableColumns {
		t.Run(table, func(t *testing.T) {
			rows, err := fixture.Owner.Query(t.Context(), `SELECT column_name FROM information_schema.columns
				WHERE table_schema='public' AND table_name=$1 ORDER BY ordinal_position`, table)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for rows.Next() {
				var column string
				if err := rows.Scan(&column); err != nil {
					t.Fatal(err)
				}
				got = append(got, column)
			}
			rows.Close()
			if !equalStrings(got, want) {
				t.Fatalf("columns=%v want=%v", got, want)
			}
		})
	}
	for index, fragment := range map[string]string{
		"auth_challenges_unconsumed_expiry_idx": "WHERE (consumed_at IS NULL)",
		"sessions_access_digest_idx":            "UNIQUE INDEX sessions_access_digest_idx",
		"sessions_refresh_digest_idx":           "UNIQUE INDEX sessions_refresh_digest_idx",
		"sessions_account_status_idx":           "(account_id, status)",
		"sessions_device_status_idx":            "(device_id, status)",
		"sessions_lineage_status_idx":           "(lineage_root_session_id, status)",
		"session_refresh_history_lineage_idx":   "(lineage_root_session_id)",
		"rate_limit_buckets_expiry_idx":         "(window_end)",
	} {
		var definition string
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT indexdef FROM pg_indexes
			WHERE schemaname='public' AND indexname=$1`, index).Scan(&definition); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(definition, fragment) {
			t.Fatalf("index %s=%q missing %q", index, definition, fragment)
		}
	}
	var profileRequired string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT is_nullable||':'||column_default FROM information_schema.columns
		WHERE table_schema='public' AND table_name='accounts' AND column_name='profile_required'`).Scan(&profileRequired); err != nil {
		t.Fatal(err)
	}
	if profileRequired != "NO:false" {
		t.Fatalf("accounts.profile_required contract=%q", profileRequired)
	}
	var projectorDefinition string
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT pg_get_functiondef(
		'app.project_security_event_v5(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)'::regprocedure)`).Scan(&projectorDefinition); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(projectorDefinition, "family_bootstrapped") {
		t.Fatal("historical schema-five projector contains the schema-six family_bootstrapped SQL case")
	}
}

// Granting direct DML would bypass validation and authority projection.
func TestIdentityTablesDenyRuntimeAndProjectorDirectDML(t *testing.T) {
	fixture := testpg.New(t)
	for _, pool := range []struct {
		name string
		exec func(context.Context, string, ...any) (pgconn.CommandTag, error)
	}{{"runtime", fixture.Runtime.Exec}, {"projector", fixture.Projector.Exec}} {
		for _, table := range identityTables {
			for _, statement := range []string{
				"SELECT * FROM public." + table + " LIMIT 0",
				"DELETE FROM public." + table,
				"TRUNCATE public." + table,
			} {
				t.Run(pool.name+"/"+table+"/"+statement[:6], func(t *testing.T) {
					_, err := pool.exec(t.Context(), statement)
					var pgErr *pgconn.PgError
					if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
						t.Fatalf("error=%v, want SQLSTATE 42501", err)
					}
				})
			}
		}
	}
}

func assertFunctionPrivilege(t *testing.T, query *pgxpool.Pool, signature string, want bool) {
	t.Helper()
	var got bool
	if err := query.QueryRow(t.Context(), "SELECT has_function_privilege(current_user,$1,'EXECUTE')", signature).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("execute %s=%t want=%t", signature, got, want)
	}
}

func assertPublicCannotExecute(t *testing.T, fixture *testpg.Fixture, signature string) {
	t.Helper()
	var publicExecute bool
	if err := fixture.Owner.QueryRow(t.Context(), `SELECT EXISTS (
		SELECT FROM pg_proc p
		CROSS JOIN LATERAL aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) acl
		WHERE p.oid=$1::regprocedure AND acl.grantee=0 AND acl.privilege_type='EXECUTE')`, signature).Scan(&publicExecute); err != nil {
		t.Fatal(err)
	}
	if publicExecute {
		t.Fatalf("PUBLIC can execute %s", signature)
	}
}

func assertExactAppExecutables(t *testing.T, fixture *testpg.Fixture, role string, want []string) {
	t.Helper()
	rows, err := fixture.Owner.Query(t.Context(), `SELECT p.oid::regprocedure::text
		FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		WHERE n.nspname='app' AND has_function_privilege($1,p.oid,'EXECUTE')
		ORDER BY 1`, role)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var signature string
		if err := rows.Scan(&signature); err != nil {
			t.Fatal(err)
		}
		got = append(got, signature)
	}
	want = append([]string(nil), want...)
	sort.Strings(want)
	if !equalStrings(got, want) {
		t.Fatalf("%s app executables=%v want=%v", role, got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
