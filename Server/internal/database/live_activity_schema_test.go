package database_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"babytracker/server/internal/access"
	"babytracker/server/internal/database"
	"babytracker/server/internal/tenant"
	"babytracker/server/internal/testkit"
	"babytracker/server/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Schema 32 (Phase 4, 000032): Live Activity push on the authority schema,
// and care entity origins.

// liveActivityRuntimeFunctions are the registration definers the runtime
// alone executes.
var liveActivityRuntimeFunctions = []string{
	"app.fence_live_activity_installation(uuid,bigint,uuid,uuid,uuid,uuid,bigint,timestamp with time zone,boolean)",
	"app.live_activity_authorized(uuid,bigint,boolean)",
	"app.live_activity_claim_live(jsonb)",
	"app.live_activity_registration_state(uuid,jsonb)",
	"app.live_activity_session_binding(uuid,uuid,uuid,bigint)",
}

// liveActivityWorkerFunctions are the worker definers the maintenance group
// alone executes.
var liveActivityWorkerFunctions = []string{
	"app.abandon_live_activity_start(jsonb,text,text,boolean,boolean)",
	"app.claim_live_activity_work(timestamp with time zone,integer)",
	"app.dirty_live_activity_families(uuid[],bigint[])",
	"app.finish_live_activity_work(jsonb,timestamp with time zone,text)",
	"app.live_activity_target(jsonb,jsonb,boolean,timestamp with time zone,jsonb,jsonb)",
	"app.release_stale_live_activity_lease(jsonb)",
	"app.retain_live_activity_work()",
	"app.sweep_live_activity_registrations(boolean,integer)",
	"app.sweep_live_activity_work()",
}

func TestSchemaIsVersionThirtyTwo(t *testing.T) {
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
	for _, c := range []struct {
		pool *pgxpool.Pool
		sql  string
		want bool
	}{
		{f.Runtime, `has_function_privilege(current_user,'app.claim_live_activity_work(timestamp with time zone,integer)','EXECUTE')`, false},
		{f.Maintenance, `has_function_privilege(current_user,'app.claim_live_activity_work(timestamp with time zone,integer)','EXECUTE')`, true},
		{f.Maintenance, `has_table_privilege(current_user,'public.live_activity_tokens','SELECT')`, false},
		{f.Runtime, `has_table_privilege(current_user,'public.live_activity_delivery','SELECT')`, false},
		{f.Runtime, `has_function_privilege(current_user,'app.live_activity_authorized(uuid,bigint,boolean)','EXECUTE')`, true},
	} {
		var got bool
		if err := c.pool.QueryRow(t.Context(), `SELECT `+c.sql).Scan(&got); err != nil || got != c.want {
			t.Fatalf("%s = %v (%v)", c.sql, got, err)
		}
	}
	for _, signature := range liveActivityRuntimeFunctions {
		assertFunctionPrivilege(t, f.Runtime, signature, true)
		assertFunctionPrivilege(t, f.Maintenance, signature, false)
		assertPublicCannotExecute(t, f, signature)
	}
	for _, signature := range liveActivityWorkerFunctions {
		assertFunctionPrivilege(t, f.Maintenance, signature, true)
		assertFunctionPrivilege(t, f.Runtime, signature, false)
		assertPublicCannotExecute(t, f, signature)
	}
	// Every Live Activity function pins search_path; none is reachable by
	// PUBLIC (the internal definers and triggers by no principal at all).
	var unpinned, public []string
	rows, err := f.Owner.Query(t.Context(), `SELECT p.oid::regprocedure::text FROM pg_proc p
		WHERE p.pronamespace='app'::regnamespace AND p.proname LIKE '%live\_activity%'
		  AND NOT coalesce(p.proconfig @> ARRAY['search_path=pg_catalog'], false) ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	if unpinned, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(unpinned) != 0 {
		t.Fatalf("functions without a pinned search_path: %v (%v)", unpinned, err)
	}
	rows, err = f.Owner.Query(t.Context(), `SELECT p.oid::regprocedure::text FROM pg_proc p
		WHERE p.pronamespace='app'::regnamespace AND p.proname LIKE '%live\_activity%'
		  AND (has_function_privilege('public', p.oid, 'EXECUTE')
		    OR (NOT p.oid = ANY($1::regprocedure[]) AND (has_function_privilege('baby_tracker_runtime', p.oid, 'EXECUTE') OR has_function_privilege('baby_tracker_maintenance', p.oid, 'EXECUTE'))))
		ORDER BY 1`, append(append([]string(nil), liveActivityRuntimeFunctions...), liveActivityWorkerFunctions...))
	if err != nil {
		t.Fatal(err)
	}
	if public, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(public) != 0 {
		t.Fatalf("functions executable beyond their grants: %v (%v)", public, err)
	}
	var trigger bool
	if err := f.Owner.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname='live_activity_journal_dirty')`).Scan(&trigger); err != nil {
		t.Fatal(err)
	}
	if trigger {
		t.Fatal("main's journal trigger was ported")
	}
	// The definer-only tables have row-level security enabled (not forced:
	// their owner, the definers, reads them); the tenant tables are forced.
	rows, err = f.Owner.Query(t.Context(), `SELECT relname::text, relrowsecurity, relforcerowsecurity FROM pg_class
		WHERE relnamespace='public'::regnamespace AND relkind='r' AND relname LIKE 'live\_activity\_%' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	type rls struct {
		table           string
		enabled, forced bool
	}
	got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (rls, error) {
		var r rls
		return r, row.Scan(&r.table, &r.enabled, &r.forced)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []rls{{"live_activity_delivery", true, false}, {"live_activity_devices", true, true}, {"live_activity_family_dirty", true, false},
		{"live_activity_history", true, false}, {"live_activity_installations", true, false}, {"live_activity_registration_index", true, false},
		{"live_activity_token_owners", true, false}, {"live_activity_tokens", true, true}}
	if len(got) != len(want) {
		t.Fatalf("live activity tables %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("live activity tables %v, want %v", got, want)
		}
	}
}

type liveActivityRegistration struct {
	ID      string
	Version uint64
	Root    string // the session lineage the registration is bound to
}

// registerDevice registers p's device for Live Activity the way a
// registration request does: a runtime transaction with p's tenant context
// reads the session binding, fences the installation and inserts the
// live_activity_devices row (main's liveactivity.Repository.RegisterDevice,
// without membership_revision). id is the registration ID ("" for a random
// one); pushToken is nil for a registration without a push-to-start token.
func registerDevice(t *testing.T, h *testkit.Harness, p access.Principal, installation, id string, pushToken any) liveActivityRegistration {
	t.Helper()
	ctx := t.Context()
	tx, err := h.Runtime.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := tenant.SetLocal(ctx, tx, tenant.Context{AccountID: p.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	var instance string
	var expires time.Time
	reg := liveActivityRegistration{}
	if err := tx.QueryRow(ctx, `SELECT instance_id::text, lineage_root_session_id::text, authority_expires_at FROM app.live_activity_session_binding($1,$2,$3,$4)`,
		p.AccountID, p.DeviceID, p.SessionID, int64(p.RecoveryEpoch)).Scan(&instance, &reg.Root, &expires); err != nil {
		t.Fatalf("session binding: %v", err)
	}
	var fenced bool
	if err := tx.QueryRow(ctx, `SELECT app.fence_live_activity_installation($1,$2,$3,$4,$5,$6,$7,$8,false)`,
		instance, int64(p.RecoveryEpoch), installation, p.AccountID, p.DeviceID, h.FamilyID, int64(1), expires).Scan(&fenced); err != nil || !fenced {
		t.Fatalf("fence=%v (%v)", fenced, err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO live_activity_devices(registration_id,installation_id,account_id,device_id,family_id,instance_id,recovery_epoch,registration_version,environment,topic,push_token,payload_version,feed_break_minutes,session_id,lineage_root_session_id,authority_expires_at,registered_sequence)
		VALUES(coalesce(nullif($16,'')::uuid, gen_random_uuid()),$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,(SELECT next_sequence FROM families WHERE id=$4))
		RETURNING registration_id::text, registration_version`,
		installation, p.AccountID, p.DeviceID, h.FamilyID, instance, int64(p.RecoveryEpoch), int64(1), "sandbox",
		"com.example.babytracker.push-type.liveactivity", pushToken, int64(1), int64(30), p.SessionID, reg.Root, expires, id).Scan(&reg.ID, &reg.Version); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return reg
}

// registerCaregiverDevice registers the caregiver's device. The session
// binding and app.live_activity_authorized judge sessions by
// clock_timestamp(); the harness issues them at the database clock.
func registerCaregiverDevice(t *testing.T, h *testkit.Harness) liveActivityRegistration {
	t.Helper()
	return registerDevice(t, h, h.Caregiver, "60000000-0000-4000-8000-000000000001", "", "a1b2c3d4e5f60718")
}

// authorizedRole is app.live_activity_authorized(reg, false) on q, as text.
func authorizedRole(t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, reg liveActivityRegistration) *string {
	t.Helper()
	var role *string
	if err := q.QueryRow(t.Context(), `SELECT app.live_activity_authorized($1,$2,false)::text`, reg.ID, int64(reg.Version)).Scan(&role); err != nil {
		t.Fatal(err)
	}
	return role
}

func sweepRegistrations(t *testing.T, h *testkit.Harness) int {
	t.Helper()
	var revoked int
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT app.sweep_live_activity_registrations(false, 100)`).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	return revoked
}

func wantRole(t *testing.T, what string, got *string, want string) {
	t.Helper()
	switch {
	case want == "" && got != nil:
		t.Fatalf("%s: authorized as %s, want NULL", what, *got)
	case want != "" && (got == nil || *got != want):
		t.Fatalf("%s: authorized as %v, want %s", what, got, want)
	}
}

// revokeLineage revokes every live session of a lineage, as refresh-token
// reuse does (sessions_lineage_live_idx), leaving the device and the
// membership active.
func revokeLineage(t *testing.T, h *testkit.Harness, root string) {
	t.Helper()
	h.AuthorityExec(t, `UPDATE sessions SET status='revoked', revoked_at=clock_timestamp(), revoke_reason='refresh_reused'
		WHERE lineage_root_id=$1 AND status='active'`, root)
}

// app.live_activity_authorized follows the authority: removal ends the
// authorization at its commit, and the sweep then revokes the registration.
// The worker definers run over the definer-only tables (row-level security
// enabled, not forced, the token owners included).
func TestLiveActivityAuthorizationFollowsTheAuthority(t *testing.T) {
	h := testkit.New(t)
	h.SeedBabies(t, 1)
	reg := registerCaregiverDevice(t, h) // inserts a live_activity_devices row for the caregiver's session (tenant context, as Task 11's repository will)
	count := func(query string) (n int) {
		t.Helper()
		if err := h.Migrator.QueryRow(t.Context(), query, reg.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	deliveries := `SELECT count(*) FROM live_activity_delivery WHERE registration_id=$1`
	owners := `SELECT count(*) FROM live_activity_token_owners WHERE registration_id=$1`
	wantRole(t, "a live caregiver registration", authorizedRole(t, h.Runtime, reg), "caregiver")
	// The registration dirtied its family: one job per live baby and kind; its
	// push-to-start token is owned.
	if n := count(deliveries); n != 2 {
		t.Fatalf("deliveries after registration = %d, want 2", n)
	}
	if n := count(owners); n != 1 {
		t.Fatalf("token owners after registration = %d, want 1", n)
	}
	var required *string
	if err := h.Runtime.QueryRow(t.Context(), `SELECT app.live_activity_authorized($1,$2,true)::text`, reg.ID, int64(reg.Version)).Scan(&required); err != nil {
		t.Fatal(err)
	}
	if required != nil {
		t.Fatal("authorized without the family_sync entitlement a hosted node requires")
	}
	wantRole(t, "another registration version", authorizedRole(t, h.Runtime, liveActivityRegistration{ID: reg.ID, Version: reg.Version + 1}), "")
	// The worker claims and finishes both jobs and finds nothing to retain.
	rows, err := h.Maintenance.Query(t.Context(), `SELECT app.claim_live_activity_work(clock_timestamp(), 100)`)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil || len(claims) != 2 {
		t.Fatalf("claimed %d jobs (%v), want 2", len(claims), err)
	}
	for _, claim := range claims {
		var finished bool
		if err := h.Maintenance.QueryRow(t.Context(), `SELECT app.finish_live_activity_work($1::jsonb, NULL, NULL)`, string(claim)).Scan(&finished); err != nil || !finished {
			t.Fatalf("finish=%v (%v)", finished, err)
		}
	}
	var retained int64
	if err := h.Maintenance.QueryRow(t.Context(), `SELECT app.retain_live_activity_work()`).Scan(&retained); err != nil || retained != 0 {
		t.Fatalf("retain removed %d (%v)", retained, err)
	}
	if n := sweepRegistrations(t, h); n != 0 {
		t.Fatalf("sweep of an authorized registration revoked %d", n)
	}
	if _, err := h.Family.Remove(t.Context(), h.Owner, h.CommandID(), h.Caregiver.AccountID); err != nil {
		t.Fatal(err)
	}
	wantRole(t, "a removed member", authorizedRole(t, h.Runtime, reg), "")
	if n := sweepRegistrations(t, h); n != 1 {
		t.Fatalf("sweep revoked %d, want 1", n)
	}
	wantRole(t, "a swept registration", authorizedRole(t, h.Runtime, reg), "")
	if n := count(deliveries); n != 0 {
		t.Fatalf("deliveries after revocation = %d, want 0", n)
	}
	if n := count(owners); n != 0 {
		t.Fatalf("token owners after revocation = %d, want 0", n)
	}
	if n := sweepRegistrations(t, h); n != 0 {
		t.Fatalf("second sweep revoked %d", n)
	}
}

// The registration is bound to its session lineage, which replaces main's
// membership revision: with the member, the device and the family all still
// active, a revoked lineage, a revoked registration and a logout each end
// the authorization.
func TestLiveActivityAuthorizationFollowsTheSessionLineage(t *testing.T) {
	h := testkit.New(t)
	h.SeedBabies(t, 1)
	first := registerCaregiverDevice(t, h)
	wantRole(t, "the first lineage's registration", authorizedRole(t, h.Runtime, first), "caregiver")

	// A new sign-in on the same device starts a second lineage; the first
	// stays live until it is revoked (as refresh-token reuse revokes it).
	second, err := h.Reauthenticate("caregiver")
	if err != nil || second.DeviceID != h.Caregiver.DeviceID {
		t.Fatalf("second lineage on device %s, want %s (%v)", second.DeviceID, h.Caregiver.DeviceID, err)
	}
	wantRole(t, "the first lineage while it is live", authorizedRole(t, h.Runtime, first), "caregiver")
	revokeLineage(t, h, first.Root)
	wantRole(t, "a registration of a revoked lineage, the device holding another", authorizedRole(t, h.Runtime, first), "")
	if n := sweepRegistrations(t, h); n != 1 {
		t.Fatalf("sweep revoked %d, want 1", n)
	}
	wantRole(t, "a swept registration", authorizedRole(t, h.Runtime, first), "")

	// Registered again in the second lineage, the device is authorized; a
	// revoked registration (UnregisterDevice) is not, though its lineage is.
	rebound := registerDevice(t, h, second, "60000000-0000-4000-8000-000000000002", "", "b1b2c3d4e5f60718")
	if rebound.Root == first.Root {
		t.Fatal("the second sign-in kept the first lineage")
	}
	wantRole(t, "the second lineage's registration", authorizedRole(t, h.Runtime, rebound), "caregiver")
	tx, err := h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: second.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE live_activity_devices SET revoked=true WHERE registration_id=$1`, rebound.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	wantRole(t, "a revoked registration of a live lineage", authorizedRole(t, h.Runtime, rebound), "")

	// A registration whose authority deadline no longer equals its lineage's
	// session deadline is not authorized, though the session is live.
	shifted := registerDevice(t, h, second, "60000000-0000-4000-8000-000000000004", "", nil)
	wantRole(t, "a registration at its session's deadline", authorizedRole(t, h.Runtime, shifted), "caregiver")
	tx, err = h.Runtime.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: second.AccountID, FamilyID: h.FamilyID}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE live_activity_devices SET authority_expires_at = authority_expires_at - interval '1 second' WHERE registration_id=$1`, shifted.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	wantRole(t, "a registration off its session's deadline", authorizedRole(t, h.Runtime, shifted), "")
	if n := sweepRegistrations(t, h); n != 1 {
		t.Fatalf("sweep of the off-deadline registration revoked %d, want 1", n)
	}

	// Logout revokes the device's sessions: the registration made in that
	// lineage is no longer authorized, and the sweep revokes it.
	last := registerDevice(t, h, second, "60000000-0000-4000-8000-000000000003", "", "c1b2c3d4e5f60718")
	wantRole(t, "a registration before logout", authorizedRole(t, h.Runtime, last), "caregiver")
	if _, err := h.Auth.Logout(t.Context(), second, h.CommandID()); err != nil {
		t.Fatal(err)
	}
	wantRole(t, "a registration after logout", authorizedRole(t, h.Runtime, last), "")
	if n := sweepRegistrations(t, h); n != 1 {
		t.Fatalf("sweep after logout revoked %d, want 1", n)
	}
	wantRole(t, "a registration swept after logout", authorizedRole(t, h.Runtime, last), "")
}

// app.live_activity_authorized confines a caller with tenant context to its
// own tenant, serves a context-free caller every registration, and leaves the
// caller's tenant context and authority flag as it found them.
func TestLiveActivityAuthorizationKeepsTheCallersContext(t *testing.T) {
	h := testkit.New(t)
	caregiverReg := registerCaregiverDevice(t, h)
	ownerReg := registerDevice(t, h, h.Owner, "60000000-0000-4000-8000-000000000009", "", "d1b2c3d4e5f60718")
	unknown := liveActivityRegistration{ID: "70000000-0000-4000-8000-000000000001", Version: 1}
	settings := func(tx pgx.Tx) (account, family, flag string, authority bool) {
		t.Helper()
		if err := tx.QueryRow(t.Context(), `SELECT coalesce(current_setting('app.account_id', true), ''), coalesce(current_setting('app.family_id', true), ''),
			coalesce(current_setting('app.authority', true), ''), app.authority_context()`).Scan(&account, &family, &flag, &authority); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, c := range []struct {
		name                  string
		account, family, flag string
		caregiver, owner      string
	}{
		{"no context", "", "", "", "caregiver", "owner"},
		{"the caregiver's tenant", h.Caregiver.AccountID, h.FamilyID, "", "caregiver", ""},
		{"the owner's tenant", h.Owner.AccountID, h.FamilyID, "", "", "owner"},
		{"the caregiver's account in another family", h.Caregiver.AccountID, "72000000-0000-4000-8000-000000000002", "", "", ""},
		{"a family-only context", "", h.FamilyID, "", "", ""},
		{"the authority flag", "", "", "1", "caregiver", "owner"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx, err := h.Runtime.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err := tx.Exec(t.Context(), `SELECT set_config('app.account_id', $1, true), set_config('app.family_id', $2, true), set_config('app.authority', $3, true)`,
				c.account, c.family, c.flag); err != nil {
				t.Fatal(err)
			}
			wantRole(t, "the caregiver's registration", authorizedRole(t, tx, caregiverReg), c.caregiver)
			wantRole(t, "the owner's registration", authorizedRole(t, tx, ownerReg), c.owner)
			wantRole(t, "an unknown registration", authorizedRole(t, tx, unknown), "")
			account, family, flag, authority := settings(tx)
			if account != c.account || family != c.family || flag != c.flag || authority != (c.flag == "1") {
				t.Fatalf("after the calls: account=%q family=%q authority=%q (%t), want %q %q %q", account, family, flag, authority, c.account, c.family, c.flag)
			}
		})
	}
}

// The sweep judges every candidate with no tenant context, whatever the caller
// holds, sets each registration's own tenant only for its revoke, and
// restores the caller's context. The caller here holds a third tenant's
// context; the owner's authorized registrations sort before and after the
// caregiver's stale ones.
func TestLiveActivitySweepEvaluatesWithoutTenantContext(t *testing.T) {
	h := testkit.New(t)
	var stale []liveActivityRegistration
	for i := 1; i <= 2; i++ {
		stale = append(stale, registerDevice(t, h, h.Caregiver, fmt.Sprintf("61000000-0000-4000-8000-%012d", i), fmt.Sprintf("00000000-0000-4000-8000-%012d", i), nil))
	}
	owners := []liveActivityRegistration{
		registerDevice(t, h, h.Owner, "62000000-0000-4000-8000-000000000000", "00000000-0000-4000-8000-000000000000", nil),
		registerDevice(t, h, h.Owner, "62000000-0000-4000-8000-000000000001", "ffffffff-ffff-4fff-bfff-ffffffffffff", nil),
	}
	revokeLineage(t, h, stale[0].Root)
	for _, owner := range owners {
		wantRole(t, "the owner's registration before the sweep", authorizedRole(t, h.Runtime, owner), "owner")
	}

	tx, err := h.Maintenance.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	const foreignAccount, foreignFamily = "71000000-0000-4000-8000-000000000001", "72000000-0000-4000-8000-000000000001"
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.account_id', $1, true), set_config('app.family_id', $2, true)`, foreignAccount, foreignFamily); err != nil {
		t.Fatal(err)
	}
	var revoked int
	if err := tx.QueryRow(t.Context(), `SELECT app.sweep_live_activity_registrations(false, 100)`).Scan(&revoked); err != nil || revoked != len(stale) {
		t.Fatalf("sweep revoked %d (%v), want %d", revoked, err, len(stale))
	}
	// (The maintenance login cannot execute app.authority_context(); the flag is
	// read directly.)
	var account, family, flag string
	if err := tx.QueryRow(t.Context(), `SELECT coalesce(current_setting('app.account_id', true), ''), coalesce(current_setting('app.family_id', true), ''),
		coalesce(current_setting('app.authority', true), '')`).Scan(&account, &family, &flag); err != nil {
		t.Fatal(err)
	}
	if account != foreignAccount || family != foreignFamily || flag != "" {
		t.Fatalf("after the sweep: account=%q family=%q authority=%q", account, family, flag)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, owner := range owners {
		wantRole(t, "the owner's registration after the sweep", authorizedRole(t, h.Runtime, owner), "owner")
	}
	for _, reg := range stale {
		wantRole(t, "a stale registration after the sweep", authorizedRole(t, h.Runtime, reg), "")
	}
}

// care_entities' origin is both columns or neither, never after the entity's
// own latest change.
func TestCareOriginShape(t *testing.T) {
	h := testkit.New(t)
	baby := h.SeedBabies(t, 1)[0]
	device := h.Owner.DeviceID
	for name, c := range map[string]struct {
		device any
		seq    string
		code   string
	}{
		"device without sequence":  {device, "NULL", "23514"},
		"sequence without device":  {nil, "changed_seq", "23514"},
		"after the latest change":  {device, "changed_seq + 1", "23514"},
		"zero device":              {"00000000-0000-0000-0000-000000000000", "changed_seq", "23514"},
		"zero sequence":            {device, "0", "23514"},
		"neither (written pre-32)": {nil, "NULL", ""},
		"the latest change itself": {device, "changed_seq", ""},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := h.Migrator.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if err := tenant.SetLocal(t.Context(), tx, tenant.Context{AccountID: h.Owner.AccountID, FamilyID: h.FamilyID}); err != nil {
				t.Fatal(err)
			}
			tag, err := tx.Exec(t.Context(), `UPDATE care_entities SET created_device_id=$3::uuid, created_seq=`+c.seq+` WHERE family_id=$1 AND id=$2`, h.FamilyID, baby.CanonicalEntity.Meta.ID, c.device)
			var pgErr *pgconn.PgError
			switch {
			case c.code == "" && (err != nil || tag.RowsAffected() != 1):
				t.Fatalf("rows=%d err=%v", tag.RowsAffected(), err)
			case c.code != "" && (!errors.As(err, &pgErr) || pgErr.Code != c.code):
				t.Fatalf("err=%v, want %s", err, c.code)
			}
		})
	}
}
