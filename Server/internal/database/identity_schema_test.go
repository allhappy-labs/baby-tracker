package database_test

import (
	"sort"
	"strings"
	"testing"
	"time"

	"babytracker/server/internal/database"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runtimeAppFunctions is every app function the runtime may execute at
// schema 31: the tenant helpers and the authority definers. The schema-28
// identity, family-material, refresh-recovery and export-audit functions are
// gone with the projection (29), the legacy care helpers with the legacy
// care storage (30), and the rate-limit definer with the database rate
// limits (31; limits live in process memory). 32 adds the five Live
// Activity registration definers; 37 the pull's high water (an invoker).
var runtimeAppFunctions = []string{
	"app.fence_live_activity_installation(uuid,bigint,uuid,uuid,uuid,uuid,bigint,timestamp with time zone,boolean)",
	"app.live_activity_authorized(uuid,bigint,boolean)",
	"app.live_activity_claim_live(jsonb)",
	"app.live_activity_registration_state(uuid,jsonb)",
	"app.live_activity_session_binding(uuid,uuid,uuid,bigint)",
	"app.authority_actor_label(uuid,uuid,boolean,timestamp with time zone)",
	"app.authority_context()",
	"app.current_account_id()",
	"app.current_family_id()",
	"app.family_actor_labels_high_water()",
	"app.family_high_water(uuid)",
	"app.import_unfinished_session_count(uuid,timestamp with time zone)",
	"app.integration_authority(uuid,bigint)",
	"app.list_family_actor_labels(uuid,bigint,uuid,integer)",
	membershipMayWriteSignature,
	"app.request_authority(bytea[],uuid,boolean,bigint)",
	"app.update_own_profile(uuid,uuid,text,timestamp with time zone)",
}

// identityTables are the schema-29 tables that hold sign-in, device and
// session authority (rate_limit_buckets went with 000031).
var identityTables = []string{
	"account_identities", "apple_notifications", "auth_challenges", "devices", "refresh_receipts", "sessions",
}

var identityTableColumns = map[string][]string{
	"account_identities":  {"provider", "identity_key_id", "identity_digest", "account_id", "lifecycle_generation", "audience", "subject", "live", "created_at"},
	"apple_notifications": {"notification_key_id", "notification_digest", "command_id", "received_at"},
	"auth_challenges":     {"token_digest", "expires_at", "consumed_at", "created_at"},
	"devices":             {"id", "account_id", "canonical_origin", "continuity_key_id", "continuity_digest", "created_at", "updated_at", "status", "revoked_at"},
	"refresh_receipts": {"parent_id", "child_id", "attempt_id", "account_id", "device_id", "material", "material_digest", "created_at", "effective_deadline", "refresh_expires_at",
		"child_scope", "child_family_id", "child_role"},
	"sessions": {"access_digest", "id", "account_id", "device_id", "lineage_root_id", "parent_id", "lineage_started_at", "scope", "family_id", "role",
		"recovery_epoch", "issued_seq", "token_key_id", "refresh_digest", "access_expires_at", "refresh_expires_at", "deadline_at", "status",
		"consumed_at", "consumed_attempt_id", "child_id", "revoked_at", "revoke_reason", "created_at"},
}

// Dropping any reserved table/function or widening any application role's
// effective privilege set must fail the exact schema-29 readiness contract.
func TestIdentitySchemaHasExactApplicationPrivileges(t *testing.T) {
	fixture := testpg.New(t)
	if err := database.VerifyRuntimeRole(t.Context(), fixture.Runtime, 37); err != nil {
		t.Fatalf("schema-30 exact privilege snapshot rejected: %v", err)
	}
	present := tableSet(t, fixture, identityTables)
	if !equalStrings(present, sortedCopy(identityTables)) {
		t.Fatalf("identity tables=%v want=%v", present, identityTables)
	}
	var sequences []string
	rows, err := fixture.Owner.Query(t.Context(), `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
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
	for _, role := range []string{"baby_tracker_runtime", "baby_tracker_maintenance"} {
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
	for _, signature := range runtimeAppFunctions {
		assertFunctionPrivilege(t, fixture.Runtime, signature, true)
		assertFunctionPrivilege(t, fixture.Maintenance, signature, false)
		assertPublicCannotExecute(t, fixture, signature)
	}
	assertExactAppExecutables(t, fixture, "baby_tracker_runtime", runtimeAppFunctions)
	// The maintenance group's five definers (000031) and the audit retention drop (000035): request_path_schema_test.go;
	// its ten Live Activity worker definers (000032, the work sweep's page 000037): live_activity_schema_test.go.
	assertExactAppExecutables(t, fixture, "baby_tracker_maintenance", append([]string{
		"app.drop_expired_audit_partition(timestamp with time zone)", "app.ensure_audit_partitions(timestamp with time zone)", "app.forget_push_token(uuid,bytea)", "app.prune_push_tokens(integer)",
		"app.purge_family_step(uuid,integer)", "app.push_targets(uuid[],uuid[])",
	}, liveActivityWorkerFunctions...))
}

func TestIdentityTablesHaveExactColumnsAndCriticalIndexes(t *testing.T) {
	fixture := testpg.New(t)
	for table, want := range identityTableColumns {
		t.Run(table, func(t *testing.T) {
			if got := tableColumns(t, fixture, table); !equalStrings(got, want) {
				t.Fatalf("columns=%v want=%v", got, want)
			}
		})
	}
	for index, definition := range map[string]string{
		"sessions_pkey":                       "UNIQUE INDEX sessions_pkey ON public.sessions USING btree (access_digest)",
		"sessions_id_key":                     "UNIQUE INDEX sessions_id_key ON public.sessions USING btree (id)",
		"sessions_refresh_digest_key":         "UNIQUE INDEX sessions_refresh_digest_key ON public.sessions USING btree (refresh_digest)",
		"sessions_account_live_idx":           "(account_id) WHERE (status = 'active'::app.session_status)",
		"sessions_device_live_idx":            "(device_id) WHERE (status = 'active'::app.session_status)",
		"sessions_lineage_live_idx":           "(lineage_root_id) WHERE (status = 'active'::app.session_status)",
		"sessions_family_live_idx":            "(family_id) WHERE (status = 'active'::app.session_status)",
		"sessions_consumed_idx":               "(consumed_at) WHERE (status = 'consumed'::app.session_status)",
		"account_identities_live_idx":         "UNIQUE INDEX account_identities_live_idx ON public.account_identities USING btree (provider, identity_key_id, identity_digest) WHERE live",
		"account_identities_account_id_key":   "UNIQUE INDEX account_identities_account_id_key ON public.account_identities USING btree (account_id)",
		"devices_active_account_device_idx":   "(account_id, id) WHERE (status = 'active'::app.device_status)",
		"auth_challenges_pkey":                "UNIQUE INDEX auth_challenges_pkey ON public.auth_challenges USING btree (token_digest)",
		"auth_challenges_expiry_idx":          "(expires_at)",
		"refresh_receipts_device_attempt_idx": "(account_id, device_id, attempt_id)",
		"refresh_receipts_child_id_key":       "UNIQUE INDEX refresh_receipts_child_id_key ON public.refresh_receipts USING btree (child_id)",
		"apple_notifications_pkey":            "UNIQUE INDEX apple_notifications_pkey ON public.apple_notifications USING btree (notification_key_id, notification_digest)",
		"apple_notifications_command_id_key":  "UNIQUE INDEX apple_notifications_command_id_key ON public.apple_notifications USING btree (command_id)",
	} {
		var got string
		if err := fixture.Owner.QueryRow(t.Context(), `SELECT indexdef FROM pg_indexes
			WHERE schemaname='public' AND indexname=$1`, index).Scan(&got); err != nil {
			t.Fatalf("index %s: %v", index, err)
		}
		if !strings.Contains(got, definition) {
			t.Fatalf("index %s=%q missing %q", index, got, definition)
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

	// A refresh token digest names one session (Task 1 deferred minor).
	ids := seedAuthorityFamilies(t, fixture)
	now := time.Now().UTC().Truncate(time.Second)
	insertSession := `INSERT INTO sessions(access_digest,id,account_id,device_id,lineage_root_id,lineage_started_at,scope,recovery_epoch,issued_seq,token_key_id,
		refresh_digest,access_expires_at,refresh_expires_at,deadline_at,created_at)
		VALUES ($1,$2,$3,$4,$2,$5,'account',1,1,'token.v1',$6,$5::timestamptz + interval '15 minutes',$5::timestamptz + interval '30 days',$5::timestamptz + interval '90 days',$5)`
	if err := authorityExec(t.Context(), fixture.Owner, insertSession, authorityDigest(0xc1), "73000000-0000-4000-8000-000000000001", ids.accountA, ids.deviceA, now, authorityDigest(0xc2)); err != nil {
		t.Fatal(err)
	}
	requireUniqueViolation(t, authorityExec(t.Context(), fixture.Owner, insertSession, authorityDigest(0xc3), "73000000-0000-4000-8000-000000000002", ids.accountA, ids.deviceA, now, authorityDigest(0xc2)), "sessions_refresh_digest_key")
}

// The identity tables are authority-only for every application role: the
// runtime reaches them only inside an authority transaction (RLS), can never
// TRUNCATE them, and DELETEs only the rows authority commands retire
// (sessions, refresh receipts, challenges); the worker roles hold nothing.
func TestIdentityTablesDenyRuntimeAndProjectorDirectDML(t *testing.T) {
	fixture := testpg.New(t)
	ids := seedAuthorityFamilies(t, fixture)
	for _, table := range []string{"sessions", "refresh_receipts", "account_identities", "auth_challenges", "apple_notifications"} {
		row := authorityOnlyRows(ids, 1)[table]
		if err := authorityExec(t.Context(), fixture.Owner, row.sql, row.args...); err != nil {
			t.Fatalf("seed %s: %v", table, err)
		}
	}
	runtimeDeletes := map[string]bool{"sessions": true, "refresh_receipts": true, "auth_challenges": true}
	for _, table := range identityTables {
		if table == "devices" {
			continue // devices are tenant-readable (TestAuthorityRLSShape)
		}
		t.Run("runtime/"+table, func(t *testing.T) {
			var visible int
			if err := fixture.Runtime.QueryRow(t.Context(), "SELECT count(*) FROM public."+table).Scan(&visible); err != nil || visible != 0 {
				t.Fatalf("runtime outside authority sees %d rows err=%v", visible, err)
			}
			_, err := fixture.Runtime.Exec(t.Context(), "TRUNCATE public."+table)
			assertSQLState(t, err, "42501")
			deleted, err := fixture.Runtime.Exec(t.Context(), "DELETE FROM public."+table)
			if runtimeDeletes[table] {
				if err != nil || deleted.RowsAffected() != 0 {
					t.Fatalf("runtime delete outside authority rows=%d err=%v", deleted.RowsAffected(), err)
				}
			} else {
				assertSQLState(t, err, "42501")
			}
		})
	}
	for _, role := range []struct {
		name string
		pool *pgxpool.Pool
	}{{"maintenance", fixture.Maintenance}} {
		for _, table := range identityTables {
			for _, statement := range []string{"SELECT * FROM public." + table + " LIMIT 0", "DELETE FROM public." + table, "TRUNCATE public." + table} {
				t.Run(role.name+"/"+table+"/"+statement[:6], func(t *testing.T) {
					_, err := role.pool.Exec(t.Context(), statement)
					assertSQLState(t, err, "42501")
				})
			}
		}
	}
}

func tableColumns(t *testing.T, fixture *testpg.Fixture, table string) []string {
	t.Helper()
	rows, err := fixture.Owner.Query(t.Context(), `SELECT a.attname FROM pg_attribute a
		WHERE a.attrelid=($1::text)::regclass AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, "public."+table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		got = append(got, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func tableSet(t *testing.T, fixture *testpg.Fixture, tables []string) []string {
	t.Helper()
	rows, err := fixture.Owner.Query(t.Context(), `SELECT tablename FROM pg_tables
		WHERE schemaname='public' AND tablename = ANY($1) ORDER BY tablename`, tables)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	return got
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
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
	if !equalStrings(got, sortedCopy(want)) {
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
