# PostgreSQL Authority — Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make PostgreSQL the only security authority: new relational authority schema, transactional command handlers with per-scope hash-chained audit rows, and one-round-trip request authentication, with the file ledger, projector and in-memory replay removed from the runtime.
**Architecture:** A new `Server/internal/authority` package owns every security write as one transaction (lock accounts then families by ID, receipt-first exact retry, validate, write state + audit + receipt, commit) and every request-path authentication (tenant context + session primary-key lookup in one pipelined batch). Legacy `security`, `recovery`, `localstore`, `hosted`, `hostedcredentials` leave the runtime; the legacy event/replay code survives unused in `Server/internal/legacyledger` for the Phase 4 importer.
**Tech Stack:** Go 1.26.2, pgx v5.11 (pipelined batches, prepared statement cache), PostgreSQL 17 (enum types, partial unique indexes, RLS, range partitioning), existing disposable-Docker test harness.
**Spec:** docs/superpowers/specs/2026-10-04-postgres-authority-performance-design.md

**Phase size ruling:** Phase 1 is too large for one review cycle. It is split into **1a (Tasks 1–9: schema, authority package, request-path API, verifier, legacy move; the legacy `security` package stays compiled and in use so every task boundary builds and tests green)** and **1b (Tasks 10–16: caller migration, composition, deletion, test porting, e2e)**. 1a is written fully below; 1b tasks are complete in files/interfaces/tests but keep code to signatures and tables. Task 13 is the unavoidable big-bang switch and is isolated.

## Global Constraints
- "PostgreSQL only. The file security ledger, file fences, in-memory replay and object-store backend are removed"
- "Relational tables written by transactional command handlers, plus per-scope hash-chained audit rows in the same transaction"
- "Request authentication is one primary-key lookup with no joins. Membership or role changes update the affected session rows in the same transaction"
- "Node-wide rules (one active family per account, unique device binding, unique MCP verifier, ...) are unique constraints or partial unique indexes, not locks"
- "`server_state` is a single row holding instance ID, recovery epoch and the actor-ID key commitment"
- "A mismatch fails closed for that scope only (its requests return 503) and logs an alert. The table is partitioned by time"
- "Runtime role: exactly the DML each table needs, column-scoped where possible. The separate projector role and projection function are removed"
- "Lock the involved account rows, then family rows, each in ID order" / "An exact retry returns the stored result before any other check; a different payload under the same command ID is refused"
- "Refresh ... locks only the parent session row ... A lost-acknowledgement retry returns the committed child"
- "Sign-in and identity perform Apple verification and any other network calls before the transaction opens"
- "The commit timestamp is captured after the locks are acquired; MCP expiry is exactly 90 days from it; an exact retry never restamps or reactivates"
- "Old binaries fail closed: the schema compatibility version is bumped and pre-migration binaries refuse to start"
- "Revocation applies to every transaction that starts after the revoking transaction commits; an in-flight transaction completes, as today"
- "No transaction stays open across network waits"
- "Port existing server suites; keep security invariant tests one-for-one: revocation, cross-family isolation, lost-acknowledgement retry, issuance time"

## Review Focus
1. Lost refresh reply: the parent is consumed, the client retries the same refresh token → must get the same child (recoverable path) or the exact `refresh_reused` revocation (legacy path), never a second child. Pinned in Task 3 (`TestRefreshLostAckRetryReturnsCommittedChild`, `TestRefreshReuseRevokesLineageOnce`).
2. Role/membership change racing a sync write: a caregiver removed while its request is in flight completes that request, and every later transaction is refused because the session rows were updated in the removing transaction. Pinned in Task 7 (`TestRevocationAppliesToNextTransaction`).
3. Two owners in one family / two families for one account under concurrency: only the partial unique indexes decide. Pinned in Task 4 (`TestConcurrentCreateVersusJoinClaimsOneSlot`) and Task 1 (`TestAuthoritySchemaUniqueRules`).
4. Operator MCP issue retried after a lost CLI reply hours later: expiry stays 90 days from the original commit; a differing payload under the same command ID is refused. Pinned in Task 6 (`TestIntegrationExactRetryKeepsIssuanceTime`).
5. A tampered or truncated audit row: only that family (or account) returns 503; other families keep working; the admin full verify names the scope. Pinned in Task 8 (`TestVerifierFailsClosedPerScope`).

## File Structure
Create
- `Server/migrations/000029_postgres_authority.sql` — drops legacy authority objects, creates the new authority schema, grants, RLS, manifest 29, compatibility 29.
- `Server/internal/authority/{service.go, schema.go, errors.go, chain.go, receipts.go, locks.go, tx.go, authenticate.go, sessions.go, identity.go, family.go, lifecycle.go, operator.go, integration.go, invalidation.go, verifier.go, keys.go}` + `_test.go` siblings — the authority.
- `Server/internal/authority/authoritytest/fixture.go` — DB fixture (accounts, devices, sessions) for authority and caller tests.
- `Server/internal/legacyledger/` — moved copies of `security/event.go`, `event_handlers.go`, `state.go`, `types.go` (Head/Candidate/Command/CommandResult), `integration_events.go` (payload/effect parts), `localstore/ledger.go`+`head.go`+`json.go` reader parts; unused by runtime.
- `Server/cmd/baby-tracker-admin/authority_cli.go` — `authority init|verify`.
Modify (1b)
- `Server/internal/auth/{sessions.go,refresh.go,refresh_recovery.go,refresh_recovery_retention.go,repository.go,types.go,devices.go}`, `Server/internal/identity/{apple_notifications.go,repository.go,challenge.go}`, `Server/internal/family/{service.go,repository.go,lifecycle.go,entitlements.go}`, `Server/internal/account/lifecycle.go`, `Server/internal/operator/{security_authority.go,integration.go}`, `Server/internal/export/{service.go,worker.go,repository.go}`, `Server/internal/realtime/{authorizer.go,hub.go}`, `Server/internal/familyread/{service.go,history.go,summary.go,types.go}`, `Server/internal/mcpapi/handler.go`, `Server/internal/sync/{mutation.go,paging.go,snapshot.go}`, `Server/internal/importer/{resolver.go,conflict.go}`, `Server/internal/httpapi/{router.go,auth.go,sync.go,live.go,account.go}`, `Server/internal/testkit/{fixtures.go,postgres.go}`, `Server/internal/testpg/database.go`, `Server/internal/database/roles.go`, `Server/internal/migrate/compatibility.json`, `Server/internal/config/{config.go,security.go}`, `Server/cmd/baby-tracker-server/{main.go,mcp.go,identity_verifier_identitytest.go}`, `Server/cmd/baby-tracker-admin/main.go`, `Server/compose.yaml`, `Server/Dockerfile`, `Server/scripts/{lib/disposable-sync.sh,smoke.sh,test-db.sh,check-principals.sh}`, `Server/README.md`, `docs/operations/security-state.md`, `Server/internal/capacity/fixture_identitytest.go`.
Delete (1b, Task 14)
- `Server/internal/security/**`, `Server/internal/recovery/**`, `Server/internal/identity/hostedcredentials/**`, `Server/internal/identity/credentials.go` + `credentials_test.go` + `credentials_conformance_test.go` + `credentialtest/`, `Server/internal/account/apple_revocation*.go`, `Server/cmd/baby-tracker-server/{recovery_readiness.go,recovery_cleanup.go,recovery_observation*.go,lifecycle_worker.go,hosted_security_test.go,readiness_integrity_test.go,recovery_*_test.go}`, `Server/internal/config/security.go` hosted-provider parts.

## Coexistence ruling (authority tables vs. legacy care tables until Phase 2)
Care tables (`babies`, `feed_entries`, …, `mutation_journal`, `sync_*`, `import_*`, `delivery_pending`, `export_*`, `family_actor_labels*`, `account_profile_commands`) FK to `families(id)`/`accounts(id)` and their RLS policies use `app.current_family_id()`/`app.current_account_id()`. Phase 1 therefore **keeps the row identities and names** `accounts`, `families`, `memberships`, `devices`, `invitations`, `family_slots`, `family_entitlements`, `family_recovery_claims`, `family_actor_labels(+history)`, `account_profile_commands` and ALTERs them into the new shape; everything else authority-related is dropped and recreated. `app.current_*` helpers, care RLS, `app.schedule_family_wakes`, `app.update_own_profile`, `app.list_family_actor_labels`, `app.family_actor_labels_high_water` are kept verbatim. `families.next_sequence` (sync) stays. The legacy `account_context`/`family_context`/`membership_context` policies are replaced by policies that also admit `app.authority_context()` (a transaction-local flag set only by authority transactions), because command handlers legitimately touch two accounts or a family whose context is not the caller's. Migration 000029 refuses to run when `accounts` contains rows (fresh installs only; the Phase 4 importer replaces this guard).

## Hosted-mode ruling
`identity/hostedcredentials` and the Apple credential vault are removed. Hosted mode keeps `code_exchange_required`, entitlements, notifications and the delivery queue; the Apple authorization code is exchanged and verified **before** the sign-in transaction (`identity.AppleCodeExchanger`), and the returned Apple refresh token is **not persisted** in Phase 1 (account deletion reports `apple_authorization_revocation: "manual_required"` in both modes). `BT_IDENTITYTEST_HOSTED_ADAPTER` keeps meaning "use the identitytest Apple exchanger". Recorded as an open item for Phase 6.

---

### Task 1: Migration 000029 — the authority schema
**Files:** Create `Server/migrations/000029_postgres_authority.sql`; modify `Server/internal/migrate/compatibility.json` (min=max=29, append digest), `Server/internal/migrate/compatibility_test.go` (bounds 29, add digest row for 000029), `Server/internal/database/roles.go` (new `VerifyRuntimeRole(ctx, runtime, version)`; `VerifyRoleSeparation` kept until Task 13), `Server/internal/database/roles_test.go`; create `Server/internal/database/authority_schema_test.go`.
**Interfaces:** Produces the tables, enums and functions below (used by every later task) and `database.VerifyRuntimeRole(ctx context.Context, runtime *pgxpool.Pool, compatibleVersion int64) error`.

- [ ] **Step 1: Write the failing test** `Server/internal/database/authority_schema_test.go`
```go
func TestAuthoritySchemaIsVersionTwentyNine(t *testing.T) {
	f := testpg.New(t)
	var v int64
	if err := f.Runtime.QueryRow(t.Context(), `SELECT current_version FROM schema_compatibility WHERE singleton`).Scan(&v); err != nil || v != 29 { t.Fatalf("version=%d err=%v", v, err) }
	for _, gone := range []string{"security_projection_head", "security_command_receipts", "account_security_state", "family_security_state", "auth_exchange_material", "session_refresh_history", "refresh_recovery_receipts", "operator_audit_events", "family_command_material", "apple_revocation_jobs", "credential_cleanup_jobs"} {
		var exists bool
		_ = f.Admin.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, "public."+gone).Scan(&exists)
		if exists { t.Fatalf("legacy table %s survived", gone) }
	}
	var projector bool
	_ = f.Admin.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND p.proname LIKE 'project_security_event%')`).Scan(&projector)
	if projector { t.Fatal("projector function survived") }
	if err := database.VerifyRuntimeRole(t.Context(), f.Runtime, 29); err != nil { t.Fatal(err) }
}
func TestAuthoritySchemaUniqueRules(t *testing.T) {
	f := testpg.New(t) // seed two accounts, one family via f.Owner as table owner
	// (a) second active family_slots row for one account -> unique_violation (PK account_id)
	// (b) second membership with role='owner' AND status='active' in one family -> unique_violation memberships_one_active_owner
	// (c) second mcp_integrations row with same verifier -> unique_violation
	// (d) second sessions row with same access_digest -> unique_violation (PK)
	// (e) second live account_identities row with same (provider,key_id,digest) -> unique_violation account_identities_live_idx
	// each asserted with pgconn.PgError.Code == "23505"
}
func TestAuthorityRuntimeGrantsAreExact(t *testing.T) {
	f := testpg.New(t)
	deny := map[string]string{"security_audit_events": "UPDATE,DELETE", "command_receipts": "UPDATE,DELETE", "server_state": "INSERT,UPDATE,DELETE"}
	for table, privs := range deny { var ok bool; _ = f.Runtime.QueryRow(t.Context(), `SELECT has_table_privilege(current_user,$1,$2)`, "public."+table, privs).Scan(&ok); if ok { t.Fatalf("%s grants %s", table, privs) } }
	var colOK, tblOK bool
	_ = f.Runtime.QueryRow(t.Context(), `SELECT has_column_privilege(current_user,'public.families','status','UPDATE'), has_table_privilege(current_user,'public.families','DELETE')`).Scan(&colOK, &tblOK)
	if !colOK || tblOK { t.Fatalf("families grants col=%v tbl=%v", colOK, tblOK) }
}
```
- [ ] **Step 2: Run test to verify it fails**: `rtk proxy env BT_TEST_PACKAGES='./internal/database' BT_TEST_RUN='TestAuthoritySchema|TestAuthorityRuntimeGrants' bash Server/scripts/test-db.sh` → fails: `version=28`, `VerifyRuntimeRole undefined`.
- [ ] **Step 3: Write the migration** `Server/migrations/000029_postgres_authority.sql` (complete DDL; comments abbreviated):
```sql
-- Fresh installs only until the Phase 4 importer replaces this guard.
DO $$ BEGIN IF EXISTS (SELECT 1 FROM public.accounts) THEN
  RAISE EXCEPTION 'legacy authority data present; use baby-tracker-admin import-legacy' USING ERRCODE='55000'; END IF; END $$;

-- 1. Remove the projection era.
DROP FUNCTION IF EXISTS app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea),
  app.project_security_event_v5(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea), app.project_security_event_v6(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea),
  app.project_security_event_v7(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea), app.project_security_event_v8(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea),
  app.project_security_event_v9(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea),
  app.update_server_state(uuid,bigint,bigint,bigint,bytea), app.go_json_string(text),
  app.stage_auth_material(uuid,bytea,bytea,bytea,timestamptz), app.lookup_access_session(text,bytea), app.lookup_refresh_session(text,bytea),
  app.lookup_auth_identity(text,text,bytea), app.lookup_device_binding(uuid,uuid,uuid,text),
  app.create_auth_challenge(bytea,text,uuid,timestamptz,timestamptz), app.consume_auth_challenge(bytea,text,uuid,timestamptz),
  app.reserve_apple_notification(jsonb,text,bytea,uuid,timestamptz), app.session_lineage_started_at(uuid,uuid),
  app.lookup_refresh_recovery_receipt(uuid,uuid), app.lookup_refresh_recovery_child(uuid), app.lookup_refresh_recovery_ack(uuid,uuid,uuid,bigint,uuid),
  app.ack_refresh_recovery_receipt(uuid,uuid,uuid,uuid,bigint,uuid), app.cleanup_refresh_recovery_receipts(timestamptz), app.refresh_recovery_key_candidates(),
  app.refresh_recovery_digest(jsonb), app.canonical_refresh_recovery(jsonb),
  app.stage_family_material(uuid,bytea,bytea,bytea,timestamptz), app.lookup_family_invitation(text), app.current_account_profile(uuid),
  app.export_family_audit(uuid), app.lifecycle_operation_uuid(text,uuid), app.assign_actor_lifecycle_generation(),
  app.assert_apple_revocation_claim(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
  app.claim_apple_revocation_job(text,timestamptz,timestamptz), app.validate_apple_revocation_claim(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
  app.apple_credential_referenced_outside_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
  app.mark_apple_provider_revoked(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
  app.begin_apple_credential_cleanup(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
  app.complete_apple_revocation_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
  app.retry_apple_revocation_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz,text),
  app.assert_credential_cleanup_claim(text,text,uuid,uuid,uuid,uuid,text,text,timestamptz), app.claim_credential_cleanup(uuid,uuid,text,uuid,text,text,timestamptz,timestamptz),
  app.validate_credential_cleanup_claim(text,text,uuid,uuid,uuid,uuid,text,text,timestamptz), app.credential_cleanup_referenced(text,text,uuid,uuid,uuid,uuid,text,text,timestamptz),
  app.finish_credential_cleanup(text,text,uuid,uuid,uuid,uuid,text,text,timestamptz), app.retry_credential_cleanup(text,text,uuid,uuid,uuid,uuid,text,text,text,timestamptz,timestamptz),
  app.claim_unfinished_credential_cleanup(text,timestamptz,timestamptz), app.lock_membership_families(), app.check_family_owner();
-- (implementers: confirm each signature with `\df app.*` on a schema-28 database before committing; a wrong signature fails the migration loudly, never silently.)
DROP TABLE IF EXISTS public.security_projection_head, public.security_command_receipts, public.account_security_state, public.family_security_state,
  public.security_audit_events, public.sessions, public.session_refresh_history, public.auth_exchange_material, public.refresh_recovery_receipts,
  public.auth_challenges, public.account_identities, public.apple_notification_dedup, public.apple_revocation_jobs, public.credential_cleanup_jobs,
  public.family_command_material, public.operator_audit_events, public.mcp_integrations CASCADE;
DELETE FROM public.schema_privilege_manifest WHERE principal='baby_tracker_projector';

-- 2. Closed sets as native types.
CREATE TYPE app.account_status AS ENUM ('active','identity_unavailable','deleted');
CREATE TYPE app.family_status AS ENUM ('active','deletion_pending','purge_started','purge_completed');
CREATE TYPE app.member_role AS ENUM ('owner','caregiver','viewer');
CREATE TYPE app.member_status AS ENUM ('active','removed','former');
CREATE TYPE app.slot_state AS ENUM ('active','family_recovery');
CREATE TYPE app.device_status AS ENUM ('active','revoked');
CREATE TYPE app.session_status AS ENUM ('active','consumed','revoked');
CREATE TYPE app.session_scope AS ENUM ('account','family','family_recovery');
CREATE TYPE app.revoke_reason AS ENUM ('refresh_reused','device_revoked','identity_unavailable','member_removed','family_deleted','account_deleted','epoch_changed');
CREATE TYPE app.invitation_status AS ENUM ('active','redeemed','revoked');
CREATE TYPE app.entitlement_status AS ENUM ('active','suspended');
CREATE TYPE app.claim_status AS ENUM ('active','recovered','forfeited');
CREATE TYPE app.integration_status AS ENUM ('active','revoked');
CREATE TYPE app.audit_scope AS ENUM ('account','family');
CREATE TYPE app.audit_kind AS ENUM ('account_registered','identity_unavailable','identity_restored','device_registered','device_binding_revoked',
  'session_granted','session_rotated','session_revoked','identity_session_issued','family_created','family_bootstrapped','family_profile_repaired',
  'invitation_created','invitation_revoked','invitation_redeemed','member_left','member_removed','member_role_changed','owner_transferred','owner_recovered',
  'entitlement_granted','entitlement_suspended','operator_entitlement_granted','operator_entitlement_suspended','family_deletion_scheduled','family_recovered',
  'recovery_forfeited','account_deleted','family_purge_started','family_purge_completed','integration_granted','integration_revoked','legacy_imported');

-- 3. Authority context for RLS: set only by authority transactions (set_config('app.authority','1',true)).
CREATE FUNCTION app.authority_context() RETURNS boolean LANGUAGE sql STABLE SET search_path=pg_catalog
AS $$ SELECT current_setting('app.authority', true) = '1' $$;
REVOKE ALL ON FUNCTION app.authority_context() FROM PUBLIC; GRANT EXECUTE ON FUNCTION app.authority_context() TO baby_tracker_runtime;

-- 4. server_state: one row, no chain head.
ALTER TABLE public.server_state DROP CONSTRAINT server_state_head_shape, DROP COLUMN generation, DROP COLUMN revision, DROP COLUMN event_hash,
  ADD COLUMN actor_key_commitment bytea CHECK (actor_key_commitment IS NULL OR octet_length(actor_key_commitment)=32);

-- 5. Accounts (kept identity; new shape).
ALTER TABLE public.accounts DROP COLUMN deleted_at, ALTER COLUMN status DROP DEFAULT, ALTER COLUMN status TYPE app.account_status USING status::app.account_status,
  ADD COLUMN lifecycle_generation bigint NOT NULL DEFAULT 1 CHECK (lifecycle_generation BETWEEN 1 AND 9007199254740991),
  ADD COLUMN audit_seq bigint NOT NULL DEFAULT 0 CHECK (audit_seq >= 0), ADD COLUMN audit_hash bytea NOT NULL DEFAULT decode(repeat('00',32),'hex') CHECK (octet_length(audit_hash)=32),
  ADD COLUMN chain_failed boolean NOT NULL DEFAULT false;
ALTER TABLE public.accounts SET (fillfactor=70);
CREATE TABLE public.account_identities (
  provider text NOT NULL CHECK (provider='apple'),
  identity_key_id text NOT NULL CHECK (identity_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'),
  identity_digest bytea NOT NULL CHECK (octet_length(identity_digest)=32),
  account_id uuid NOT NULL REFERENCES public.accounts(id),
  lifecycle_generation bigint NOT NULL CHECK (lifecycle_generation BETWEEN 1 AND 9007199254740991),
  audience text NOT NULL CHECK (octet_length(audience) BETWEEN 1 AND 255), subject text NOT NULL CHECK (octet_length(subject) BETWEEN 1 AND 255),
  live boolean NOT NULL DEFAULT true, created_at timestamptz NOT NULL,
  PRIMARY KEY (provider, identity_key_id, identity_digest, lifecycle_generation), UNIQUE (account_id));
CREATE UNIQUE INDEX account_identities_live_idx ON public.account_identities(provider, identity_key_id, identity_digest) WHERE live; -- one live actor per identity
-- 6. Devices (kept name; single-instance node: origin only).
ALTER TABLE public.devices DROP COLUMN installation_key_id, DROP COLUMN installation_digest, DROP COLUMN server_instance_id,
  ADD COLUMN status app.device_status NOT NULL DEFAULT 'active', ADD COLUMN revoked_at timestamptz;
ALTER TABLE public.devices DROP COLUMN binding_revoked_at; -- replaced by status/revoked_at
-- 7. Sessions: keyed by access digest; everything authentication needs, no joins.
CREATE TABLE public.sessions (
  access_digest bytea PRIMARY KEY CHECK (octet_length(access_digest)=32),
  id uuid NOT NULL UNIQUE CHECK (id<>'00000000-0000-0000-0000-000000000000'),
  account_id uuid NOT NULL REFERENCES public.accounts(id), device_id uuid NOT NULL,
  lineage_root_id uuid NOT NULL, parent_id uuid, lineage_started_at timestamptz NOT NULL,
  scope app.session_scope NOT NULL, family_id uuid, role app.member_role,
  recovery_epoch bigint NOT NULL CHECK (recovery_epoch BETWEEN 1 AND 9007199254740991),
  issued_seq bigint NOT NULL CHECK (issued_seq BETWEEN 1 AND 9007199254740991),       -- account audit_seq at issuance; wire SecurityRevision
  token_key_id text NOT NULL CHECK (token_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'),
  refresh_digest bytea NOT NULL UNIQUE CHECK (octet_length(refresh_digest)=32),
  access_expires_at timestamptz NOT NULL, refresh_expires_at timestamptz NOT NULL, deadline_at timestamptz NOT NULL, -- deadline = reauthenticate = absolute
  status app.session_status NOT NULL DEFAULT 'active', consumed_at timestamptz, consumed_attempt_id uuid, child_id uuid, revoked_at timestamptz, revoke_reason app.revoke_reason,
  created_at timestamptz NOT NULL,
  FOREIGN KEY (device_id, account_id) REFERENCES public.devices(id, account_id),
  CHECK ((scope='account' AND family_id IS NULL AND role IS NULL) OR (scope<>'account' AND family_id IS NOT NULL AND role IS NOT NULL)),
  CHECK (access_expires_at<=refresh_expires_at AND refresh_expires_at<=deadline_at),
  CHECK ((status='active' AND consumed_at IS NULL AND revoked_at IS NULL) OR (status='consumed' AND consumed_at IS NOT NULL AND child_id IS NOT NULL) OR (status='revoked' AND revoked_at IS NOT NULL AND revoke_reason IS NOT NULL))
) WITH (fillfactor=70);
CREATE INDEX sessions_account_live_idx ON public.sessions(account_id) WHERE status='active';   -- revoke fan-out: account_deleted, identity_unavailable, member_removed, role change
CREATE INDEX sessions_device_live_idx ON public.sessions(device_id) WHERE status='active';     -- device_binding_revoked, logout
CREATE INDEX sessions_consumed_idx ON public.sessions(consumed_at) WHERE status='consumed';     -- Phase 3 pruning after the retry window
CREATE TABLE public.refresh_receipts (
  parent_id uuid PRIMARY KEY REFERENCES public.sessions(id) ON DELETE CASCADE, child_id uuid NOT NULL UNIQUE REFERENCES public.sessions(id) ON DELETE CASCADE,
  attempt_id uuid NOT NULL CHECK (attempt_id<>'00000000-0000-0000-0000-000000000000'), account_id uuid NOT NULL, device_id uuid NOT NULL,
  material bytea NOT NULL CHECK (octet_length(material) BETWEEN 1 AND 32768), material_digest bytea NOT NULL CHECK (octet_length(material_digest)=32 AND sha256(material)=material_digest),
  created_at timestamptz NOT NULL, effective_deadline timestamptz NOT NULL, refresh_expires_at timestamptz NOT NULL);
CREATE INDEX refresh_receipts_device_attempt_idx ON public.refresh_receipts(account_id, device_id, attempt_id);
CREATE TABLE public.auth_challenges (token_digest bytea PRIMARY KEY CHECK (octet_length(token_digest)=32), expires_at timestamptz NOT NULL, consumed_at timestamptz, created_at timestamptz NOT NULL);
CREATE INDEX auth_challenges_expiry_idx ON public.auth_challenges(expires_at);
CREATE TABLE public.apple_notifications (notification_key_id text NOT NULL, notification_digest bytea NOT NULL CHECK (octet_length(notification_digest)=32),
  command_id uuid NOT NULL UNIQUE, received_at timestamptz NOT NULL, PRIMARY KEY (notification_key_id, notification_digest));
-- 8. Families and family-scoped authority (kept names).
ALTER TABLE public.families DROP COLUMN deleted_at, ALTER COLUMN status DROP DEFAULT, ALTER COLUMN status TYPE app.family_status USING status::app.family_status,
  ADD COLUMN owner_account_id uuid REFERENCES public.accounts(id), ADD COLUMN purge_started_at timestamptz, ADD COLUMN purge_command_id uuid, ADD COLUMN purge_receipt uuid, ADD COLUMN purged_at timestamptz,
  ADD COLUMN audit_seq bigint NOT NULL DEFAULT 0 CHECK (audit_seq >= 0), ADD COLUMN audit_hash bytea NOT NULL DEFAULT decode(repeat('00',32),'hex') CHECK (octet_length(audit_hash)=32),
  ADD COLUMN chain_failed boolean NOT NULL DEFAULT false, ADD COLUMN entitlement_required boolean NOT NULL DEFAULT false;
ALTER TABLE public.families SET (fillfactor=70);
ALTER TABLE public.memberships DROP COLUMN removed_at, ALTER COLUMN role TYPE app.member_role USING role::app.member_role,
  ALTER COLUMN status DROP DEFAULT, ALTER COLUMN status TYPE app.member_status USING status::app.member_status;
CREATE UNIQUE INDEX memberships_one_active_owner ON public.memberships(family_id) WHERE role='owner' AND status='active'; -- replaces check_family_owner trigger
DROP TRIGGER IF EXISTS membership_family_lock ON public.memberships; DROP TRIGGER IF EXISTS family_owner_from_family ON public.families; DROP TRIGGER IF EXISTS family_owner_from_membership ON public.memberships;
ALTER TABLE public.family_slots ALTER COLUMN state TYPE app.slot_state USING state::app.slot_state;            -- PK(account_id) = one family per account
ALTER TABLE public.invitations DROP COLUMN redeemed_by_account_id, ALTER COLUMN role TYPE app.member_role USING role::app.member_role, ADD CONSTRAINT invitations_role_check CHECK (role<>'owner'),
  ALTER COLUMN status TYPE app.invitation_status USING status::app.invitation_status;
ALTER TABLE public.family_entitlements ALTER COLUMN status TYPE app.entitlement_status USING status::app.entitlement_status;
ALTER TABLE public.family_recovery_claims DROP COLUMN revision, DROP COLUMN purge_after, ALTER COLUMN status TYPE app.claim_status USING status::app.claim_status;
CREATE TABLE public.mcp_integrations (
  id uuid PRIMARY KEY, account_id uuid NOT NULL REFERENCES public.accounts(id), family_id uuid NOT NULL REFERENCES public.families(id),
  recovery_epoch bigint NOT NULL, name text NOT NULL CHECK (octet_length(name)<=64 AND name ~ '^[A-Za-z0-9]([A-Za-z0-9 ._-]{0,62}[A-Za-z0-9._-])?$'),
  scope text NOT NULL CHECK (scope='family:read'), verifier bytea NOT NULL UNIQUE CHECK (octet_length(verifier)=32),
  status app.integration_status NOT NULL DEFAULT 'active', created_at timestamptz NOT NULL CHECK (created_at=date_trunc('second',created_at)),
  expires_at timestamptz NOT NULL CHECK (expires_at=created_at+interval '7776000 seconds'), revoked_at timestamptz,
  CHECK ((status='revoked')=(revoked_at IS NOT NULL)));
-- 9. Receipts and audit.
CREATE TABLE public.command_receipts (
  actor_account_id uuid NOT NULL, command_id uuid NOT NULL, command_hash bytea NOT NULL CHECK (octet_length(command_hash)=32),
  kind app.audit_kind NOT NULL, result jsonb NOT NULL, family_ids uuid[] NOT NULL DEFAULT '{}', created_at timestamptz NOT NULL,
  PRIMARY KEY (actor_account_id, command_id));
CREATE TABLE public.security_audit_events (
  scope app.audit_scope NOT NULL, scope_id uuid NOT NULL, seq bigint NOT NULL CHECK (seq>0),
  prev_hash bytea NOT NULL CHECK (octet_length(prev_hash)=32), hash bytea NOT NULL CHECK (octet_length(hash)=32),
  kind app.audit_kind NOT NULL, command_id uuid NOT NULL, actor_account_id uuid NOT NULL, at timestamptz NOT NULL,
  payload jsonb NOT NULL, legacy_hash bytea CHECK (legacy_hash IS NULL OR octet_length(legacy_hash)=32),
  PRIMARY KEY (scope, scope_id, seq, at)) PARTITION BY RANGE (at);
CREATE TABLE public.security_audit_events_default PARTITION OF public.security_audit_events DEFAULT;
DO $$ DECLARE y int; BEGIN FOR y IN 2026..2031 LOOP
  EXECUTE format('CREATE TABLE public.security_audit_events_%s PARTITION OF public.security_audit_events FOR VALUES FROM (%L) TO (%L)', y, make_timestamptz(y,1,1,0,0,0,'UTC'), make_timestamptz(y+1,1,1,0,0,0,'UTC'));
END LOOP; END $$;
CREATE TABLE public.audit_checkpoints (scope app.audit_scope NOT NULL, scope_id uuid NOT NULL, verified_seq bigint NOT NULL, verified_hash bytea NOT NULL, verified_at timestamptz NOT NULL, PRIMARY KEY (scope, scope_id));
-- 10. RLS: authority tables admit the authority context or the tenant.
DROP POLICY account_context ON public.accounts; DROP POLICY family_context ON public.families; DROP POLICY membership_context ON public.memberships;
DROP POLICY own_family_slot ON public.family_slots; DROP POLICY invitation_family_context ON public.invitations; DROP POLICY invitation_preview_owner ON public.invitations;
DROP POLICY entitlement_family_context ON public.family_entitlements; DROP POLICY family_recovery_context ON public.family_recovery_claims;
CREATE POLICY account_context ON public.accounts USING (app.authority_context() OR id=app.current_account_id()) WITH CHECK (app.authority_context() OR id=app.current_account_id());
CREATE POLICY family_context ON public.families USING (app.authority_context() OR id=app.current_family_id()) WITH CHECK (app.authority_context() OR id=app.current_family_id());
CREATE POLICY membership_context ON public.memberships USING (app.authority_context() OR family_id=app.current_family_id()) WITH CHECK (app.authority_context());
CREATE POLICY own_family_slot ON public.family_slots USING (app.authority_context() OR account_id=app.current_account_id()) WITH CHECK (app.authority_context());
CREATE POLICY invitation_context ON public.invitations USING (app.authority_context() OR family_id=app.current_family_id()) WITH CHECK (app.authority_context());
CREATE POLICY entitlement_context ON public.family_entitlements USING (app.authority_context() OR family_id=app.current_family_id()) WITH CHECK (app.authority_context());
CREATE POLICY claim_context ON public.family_recovery_claims USING (app.authority_context() OR account_id=app.current_account_id()) WITH CHECK (app.authority_context());
ALTER TABLE public.devices ENABLE ROW LEVEL SECURITY; ALTER TABLE public.devices FORCE ROW LEVEL SECURITY;
CREATE POLICY device_context ON public.devices USING (app.authority_context() OR account_id=app.current_account_id()) WITH CHECK (app.authority_context());
-- sessions, account_identities, refresh_receipts, auth_challenges, apple_notifications, command_receipts, security_audit_events, audit_checkpoints, mcp_integrations:
--   authority-only tables; RLS with a single policy USING (app.authority_context()) WITH CHECK (app.authority_context()), ENABLE+FORCE on each.
--   (write these nine ALTER/CREATE POLICY statements verbatim; the planner keeps PK/index use because the predicate is a constant per transaction.)
-- 11. Grants (runtime). Projector receives nothing.
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM baby_tracker_projector;
GRANT SELECT, INSERT ON public.accounts, public.families, public.memberships, public.devices, public.family_slots, public.invitations, public.family_entitlements,
  public.family_recovery_claims, public.account_identities, public.sessions, public.refresh_receipts, public.auth_challenges, public.apple_notifications,
  public.mcp_integrations, public.command_receipts, public.security_audit_events, public.audit_checkpoints TO baby_tracker_runtime;
GRANT UPDATE (status, display_name, profile_required, lifecycle_generation, audit_seq, audit_hash, chain_failed, updated_at) ON public.accounts TO baby_tracker_runtime;
GRANT UPDATE (status, owner_account_id, display_name, profile_required, purge_after, purge_started_at, purge_command_id, purge_receipt, purged_at, audit_seq, audit_hash, chain_failed, entitlement_required, next_sequence, updated_at) ON public.families TO baby_tracker_runtime;
GRANT UPDATE (role, status, updated_at) ON public.memberships TO baby_tracker_runtime;
GRANT UPDATE (status, revoked_at, updated_at) ON public.devices TO baby_tracker_runtime;
GRANT UPDATE (family_id, state) ON public.family_slots TO baby_tracker_runtime;
GRANT UPDATE (status, redeemed_at, revoked_at, updated_at) ON public.invitations TO baby_tracker_runtime;
GRANT UPDATE (status, expires_at, updated_at) ON public.family_entitlements TO baby_tracker_runtime;
GRANT UPDATE (status) ON public.family_recovery_claims TO baby_tracker_runtime;
GRANT UPDATE (live) ON public.account_identities TO baby_tracker_runtime;
GRANT UPDATE (status, family_id, role, consumed_at, consumed_attempt_id, child_id, revoked_at, revoke_reason) ON public.sessions TO baby_tracker_runtime;
GRANT UPDATE (consumed_at) ON public.auth_challenges TO baby_tracker_runtime;
GRANT UPDATE (status, revoked_at) ON public.mcp_integrations TO baby_tracker_runtime;
GRANT UPDATE (verified_seq, verified_hash, verified_at) ON public.audit_checkpoints TO baby_tracker_runtime;
GRANT DELETE ON public.sessions, public.refresh_receipts, public.auth_challenges, public.family_slots TO baby_tracker_runtime;
GRANT SELECT ON public.server_state TO baby_tracker_runtime; GRANT INSERT, UPDATE ON public.server_state TO baby_tracker_maintenance; -- `authority init` runs as migrator/owner
-- 12. Manifest 29: schema-28 runtime/maintenance/revoker rows minus dropped objects, plus the rows above (one VALUES row per grant above; functions app.authority_context() EXECUTE).
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 29,principal,object_kind,object_identity,privileges FROM public.schema_privilege_manifest WHERE schema_version=28 AND principal<>'baby_tracker_projector'
  AND NOT (object_kind='function' AND object_identity ~ '^app\.(stage_auth_material|lookup_|create_auth_challenge|consume_auth_challenge|reserve_apple_notification|session_lineage_started_at|ack_refresh|cleanup_refresh|refresh_recovery|stage_family_material|current_account_profile|export_family_audit|claim_|validate_|assert_|mark_apple|begin_apple|complete_apple|retry_|finish_|credential_cleanup)')
  AND NOT (object_kind IN ('table','column') AND object_identity ~ '^public\.(accounts|families|memberships|devices|family_slots|invitations|family_entitlements|family_recovery_claims|export_jobs\.|sessions|auth_exchange_material|refresh_recovery)');
-- then explicit VALUES rows for every table/column/function grant in section 11 (list them; TestAuthorityRuntimeGrantsAreExact + roles tests verify exactness).
UPDATE public.schema_compatibility SET current_version=29, introduced_by='000029_postgres_authority.sql', recorded_at=now() WHERE singleton;
```
Also: `migrate/compatibility.json` → `"min": 29, "max": 29`, append `{version:29, name:"000029_postgres_authority.sql", sha256:<sha256 of the file>}` (compute with `shasum -a 256`). `database.VerifyRuntimeRole` = `verifyRoleSnapshot` for `baby_tracker_runtime` only (no snapshot export, no projector); `VerifyRoleSeparation` stays until Task 13 and keeps passing because the projector manifest set is empty (adjust `loadRoleInventory` to accept an empty set).
- [ ] **Step 4: Run tests**: `rtk proxy env BT_TEST_PACKAGES='./internal/database ./internal/migrate ./internal/tenant' bash Server/scripts/test-db.sh` — expect failures ONLY in legacy schema tests that assert dropped objects (`identity_schema_test`, `family_schema_test`, `lifecycle_*`, `integration_schema_test`, `refresh_recovery_schema_test`, `family_projection_test`); mark those with `t.Skip("replaced in Task 15")` in this task so the package is green, and record them in the Task 15 table. Everything else green.
- [ ] **Step 5: Commit**: `rtk git add Server/migrations/000029_postgres_authority.sql Server/internal/migrate Server/internal/database && rtk git commit -m "feat(schema): add postgres authority schema and bump compatibility to 29"`

### Task 2: Authority core — transactions, locks, receipts, audit chain
**Files:** Create `Server/internal/authority/{service.go,errors.go,tx.go,locks.go,receipts.go,chain.go}`, `chain_test.go`, `receipts_test.go`, `locks_test.go`, `authoritytest/fixture.go`.
**Interfaces:** Produces
```go
package authority
type Options struct { Pool *pgxpool.Pool; Now func() time.Time; InstanceID string; RecoveryEpoch uint64; TokenKeys identity.DigestKeyring; MaxCaregivers, MaxInvitations int; RequireFamilySyncEntitlement bool; Allowed func(role, action string) bool }
func New(o Options) (*Service, error)
type Service struct { /* unexported */ }
type Code string                       // stable error codes, identical strings to today's security.Code values
func CodeOf(err error) string          // "" for nil, "service_unavailable" for unknown
var ErrUnavailable, ErrSessionRevoked, ErrMembershipRevoked, ErrCommandIDReused, ErrInvalidCommand, ErrEpochChanged, ErrScopeFailed error // + every code in security/types.go, family/types.go, auth/refresh.go
type Result struct { CommandID string `json:"command_id"`; Outcome string `json:"outcome"`; Code string `json:"code"`; Revision uint64 `json:"revision"` } // Revision = audit seq of the primary scope row written
type scopeRef struct { kind audit.Scope; id string }
// internal building blocks used by every handler:
func (s *Service) begin(ctx context.Context) (pgx.Tx, error)                          // BEGIN + set_config('app.authority','1',true) in one batch
func lockAccounts(ctx context.Context, tx pgx.Tx, ids ...string) (map[string]accountRow, error) // sorted, FOR UPDATE, missing ids absent from map
func lockFamilies(ctx context.Context, tx pgx.Tx, ids ...string) (map[string]familyRow, error)  // sorted, FOR UPDATE; called only after lockAccounts
func lookupReceipt(ctx, tx, actorID, commandID string, hash [32]byte) (Result, found bool, err error) // hash mismatch -> ErrCommandIDReused
func writeReceipt(ctx, tx, actorID, commandID string, hash [32]byte, kind Kind, result Result, familyIDs []string, at time.Time) error
func appendAudit(ctx, tx, scope scopeRef, kind Kind, commandID, actorID string, at time.Time, payload any) (seq uint64, hash [32]byte, err error) // reads+bumps the scope row's audit_seq/audit_hash (row already locked by caller; UPDATE ... RETURNING)
func CanonicalCommandHash(actorID string, kind Kind, payload any) ([32]byte, error) // sha256 of {"version":"v2","command_id"?,"kind","actor_account_id","payload"} with json.Marshal of a struct whose field order is fixed
```
Audit row hash: `sha256(scope||0x00||scope_id||0x00||seq(8 BE)||prev_hash||kind||0x00||command_id||0x00||actor_id||0x00||at.UTC().Format(RFC3339Nano)||0x00||payload canonical JSON bytes)`; genesis `prev_hash` = 32 zero bytes. One audit row per affected scope per command (family-scoped commands write the family row and one row per affected account).
- [ ] **Step 1: Failing tests** `chain_test.go`: `TestAuditHashIsDeterministicAndChains` (two appends on one account scope: seq 1,2; row2.prev_hash==row1.hash; recomputed hash equals stored), `TestAppendAuditRequiresLockedScope` (append without FOR UPDATE on the account row inside a concurrent tx must serialize, never produce two seq=1 rows: run 20 goroutines appending to one account; assert seqs 1..20 unique). `receipts_test.go`: `TestReceiptExactRetryReturnsStoredResultBeforeChecks`, `TestReceiptHashMismatchIsCommandIDReused`. `locks_test.go`: `TestLockOrderingNeverDeadlocks` (property: 200 random commands touching random subsets of 5 accounts and 3 families, run concurrently through `lockAccounts`+`lockFamilies`; no `40P01` deadlock error).
- [ ] **Step 2**: `rtk proxy env BT_TEST_PACKAGES='./internal/authority' bash Server/scripts/test-db.sh` → compile failure (package missing).
- [ ] **Step 3**: Implement. `begin`: `tx, _ := pool.BeginTx(ctx, pgx.TxOptions{}); batch := &pgx.Batch{}; batch.Queue("SELECT set_config('app.authority','1',true)"); tx.SendBatch(ctx, batch).Close()`. `lockAccounts`: `SELECT id,status,lifecycle_generation,audit_seq,audit_hash,chain_failed FROM accounts WHERE id = ANY($1) ORDER BY id FOR UPDATE` (ORDER BY inside FOR UPDATE on a PK scan locks in ID order; assert `len(ids)` sorted first). `appendAudit`: `UPDATE accounts SET audit_seq=audit_seq+1, audit_hash=$2 WHERE id=$1 RETURNING audit_seq` after computing hash from the locked row's (seq+1, hash). `authoritytest.Fixture`: `New(t) *Fixture{Pool *pgxpool.Pool; Service *Service; Now time.Time}` with helpers `Account(t) string`, `Device(t, account) string`, `Session(t, account, device, scope, family, role) (sessionID, rawAccess, rawRefresh string)` inserting rows directly as the DB owner.
- [ ] **Step 4**: same command → green.
- [ ] **Step 5**: `rtk git add Server/internal/authority && rtk git commit -m "feat(authority): add transactional core with locks, receipts and audit chains"`

### Task 3: Identity and session handlers (worked pattern: Refresh)
**Files:** Create `Server/internal/authority/{sessions.go,identity.go,challenges.go}` + tests `sessions_test.go`, `identity_test.go`, `challenges_test.go`.
**Interfaces:** Consumes Task 2. Produces
```go
type SignIn struct { CommandID string; At time.Time; Candidates []string /* identity keys current-first, hmac-sha256:kid:hex */; Audience, Subject string; ProposedAccountID string; ProposedGeneration uint64; DisplayName *string; ProfileConfirmed bool; Device DeviceBinding; RegisterDevice bool; SessionID string; TokenKeyID string; AccessDigest, RefreshDigest [32]byte; AccessExpiresAt, RefreshExpiresAt, DeadlineAt time.Time; CanonicalOrigin string }
type DeviceBinding struct { DeviceID, AccountID, CanonicalOrigin, CredentialKeyID string; CredentialDigest [32]byte }
type SignInResult struct { Result; AccountID string; Registered bool; Restored bool; FamilySlot *FamilySlot; Account AccountView }
func (s *Service) SignIn(ctx context.Context, in SignIn) (SignInResult, error)            // identity_session_issued (+account_registered/device_registered effects; identity_restored when account was identity_unavailable)
type Refresh struct { ParentRefreshDigestCandidates []KeyedDigest /* per token key */; At time.Time; AttemptID string /* "" = legacy */; Continuity *DeviceBinding; Derive func(parent SessionRow, key identity.DigestKey) (RefreshDerivation, error); OutputKeyID string; AccessTTL, RefreshTTL time.Duration; Mode config.Mode; Material func(stage RefreshStage) ([]byte, [32]byte, error) }
type RefreshResult struct { Result; Child SessionRow; Parent SessionRow; AccessToken, RefreshToken string; Recovered bool; FamilySlot *FamilySlot; Account AccountView }
func (s *Service) Refresh(ctx context.Context, in Refresh) (RefreshResult, error)          // session_rotated | session_revoked (reuse)
func (s *Service) ResolveRefreshSubject(ctx context.Context, candidates []KeyedDigest) (accountID, deviceID string, err error) // read-only
func (s *Service) AcknowledgeRefresh(ctx, p access.Principal, attemptID string, continuity DeviceBinding) error
func (s *Service) Logout(ctx context.Context, p access.Principal, commandID string, at time.Time) (Result, error)  // device_binding_revoked
type IdentityUnavailable struct { CommandID string; AccountID string; Candidates []string; NotificationDigest string; At time.Time }
func (s *Service) MarkIdentityUnavailable(ctx context.Context, in IdentityUnavailable) (Result, error)
func (s *Service) ResolveIdentity(ctx, candidates []string) (IdentityResolution, error)   // {Status, AccountID, IdentityKey, LifecycleGeneration, NextActorGeneration}; same rules as security/state.go:126-186
func (s *Service) CreateChallenge(ctx, digest [32]byte, expiresAt, now time.Time) error; func (s *Service) ConsumeChallenge(ctx, digest [32]byte, now time.Time) error // invalid_or_expired_challenge
func (s *Service) ReserveAppleNotification(ctx, candidates []identity.KeyedDigest, commandID string, at time.Time) (storedCommandID string, newly bool, err error)
type SessionRow struct { ID, AccountID, DeviceID, LineageRootID, ParentID, FamilyID string; Scope, Role, Status string; RecoveryEpoch, IssuedSeq uint64; TokenKeyID string; AccessDigest, RefreshDigest [32]byte; AccessExpiresAt, RefreshExpiresAt, DeadlineAt, LineageStartedAt, CreatedAt time.Time; ConsumedAt, RevokedAt *time.Time; ConsumedAttemptID, ChildID string }
```
Precondition/effect table (copy exactly; file:line = behaviour to preserve):
| Handler | Preconditions (error) | Locks | Effects | Audit rows |
|---|---|---|---|---|
| SignIn | ResolveIdentity matches `ProposedAccountID/Generation` (security/prepared.go:109-128); absent/deleted → register account (`accounts` INSERT status active, lifecycle_generation=next; `account_identities` INSERT live); active → identity key must equal resolution key; identity_unavailable → restore (prepared.go:176-195); RegisterDevice: device must not exist else `invalid_command`; existing device must be active+same account+origin+credential digest (prepared.go:205-218); session id must be new | account row (if exists) | sessions INSERT scope=account (FamilySlot read gives presentation only; scope upgrade happens at refresh, auth/refresh.go:80-87), issued_seq=account.audit_seq+1 | account: `identity_session_issued` payload {register_account, register_device, device_id, session_id, identity_key(kid only, digest hex), origin} |
| Refresh (fresh) | parent row by any candidate refresh digest, exactly one match else `session_revoked` (auth/refresh.go:329-348); epoch==server epoch else `epoch_changed`; parent status active, device active, account active (prepared.go:238-240, transaction.go:275-296); now < min(deadline_at, lineage_started_at+365d self-hosted) and now < refresh_expires_at else `reauthentication_required` (auth/refresh.go:241-251,193-195); scope recomputed from family_slots + membership role (auth/refresh.go:174-175, 402-413), family entitlement when hosted (auth/refresh_recovery.go:154-187) | `SELECT ... FROM sessions WHERE id=$1 FOR UPDATE` only; account row updated last (chain head) | parent → consumed (consumed_at, consumed_attempt_id=AttemptID, child_id); child INSERT (new digests, same lineage, deadline_at unchanged, issued_seq=new seq); recoverable: refresh_receipts INSERT and DELETE of the receipt whose child_id==parent.id (successor proves possession, session-recovery spec §"Server state") | account: `session_rotated` {session_id, parent_session_id, lineage_root_session_id, refresh_attempt_id?, material_digest} |
| Refresh (lost ack) | parent status consumed AND (`AttemptID==""` legacy → fall to reuse) / `AttemptID==parent.consumed_attempt_id` → load child; child must be active and receipt present else `refresh_receipt_unavailable` (auth/refresh_recovery.go:113-152); recompute tokens from frozen material via `Derive` and verify digests match the child row (refresh_recovery.go:213-269) | parent FOR SHARE | none | none (no audit on exact retry) |
| Refresh (reuse) | parent consumed and attempt differs (or legacy path) → revoke whole lineage (prepared.go:247-268, event_handlers.go:490-516); `refresh_attempt_conflict` when recoverable, `refresh_reused` otherwise; invalid continuity proof never revokes (refresh_recovery.go:76-101) | all active sessions of lineage FOR UPDATE (by lineage_root_id) | sessions status=revoked reason refresh_reused; receipts for lineage DELETE | account: `session_revoked` {lineage_root_session_id, session_id} |
| AcknowledgeRefresh | caller active, continuity proof valid for caller's device, receipt (if any) binds same account/device/lineage and attempt (auth/refresh_recovery_retention.go:18-84); absent receipt → 204 | receipt FOR UPDATE | refresh_receipts DELETE | none |
| Logout | principal session active; device active (auth/refresh.go:260-281) | device's sessions FOR UPDATE | devices status=revoked; all device sessions revoked reason device_revoked | account: `device_binding_revoked` |
| MarkIdentityUnavailable | resolution active and account matches (prepared.go:293-313, apple_notifications.go:401-418); exact retry via receipt | account | accounts status=identity_unavailable; all account sessions revoked reason identity_unavailable (event_handlers.go:396-412) | account: `identity_unavailable` {notification_digest} |
| ResolveIdentity | ≤8 candidates, keyed-digest grammar; multiple live actors → `authorization_quarantine` (state.go:126-186) | none | none | none |
| Challenges/Notifications | same semantics as `app.create_auth_challenge/consume_auth_challenge/reserve_apple_notification` (migration 000005:278-330, 677-) | row | INSERT/UPDATE | none |
- [ ] **Step 1: Failing tests** (`sessions_test.go`): `TestRefreshRotatesAndWritesOneAuditRow`, `TestRefreshLostAckRetryReturnsCommittedChild` (consume parent with attempt A; retry with A → same child tokens, no new rows), `TestRefreshReuseRevokesLineageOnce` (retry with attempt B → `refresh_attempt_conflict`, every lineage session revoked, one audit row; second retry → `session_revoked`, no second audit row), `TestRefreshLocksOnlyParentRow` (hold `FOR UPDATE` on the account's family row in another tx; refresh still commits within 1s), `TestRefreshAtDeadlineRequiresReauthentication`, `TestRefreshUpgradesScopeFromSlot` (account→family after slot exists; `family_recovery` only via refresh), `TestLogoutRevokesEveryDeviceSession`, `TestRefreshAcrossTokenKeyRotation` (parent under previous key). (`identity_test.go`): `TestSignInRegistersAccountDeviceAndSession`, `TestSignInExactRetryReturnsSameSession`, `TestSignInRestoresIdentityUnavailable`, `TestDeletedIdentityGetsNextGenerationActor`, `TestIdentityUnavailableRevokesSessionsAndIsIdempotentByNotification`.
- [ ] **Step 2**: `rtk proxy env BT_TEST_PACKAGES='./internal/authority' BT_TEST_RUN='TestRefresh|TestSignIn|TestLogout|TestIdentity|TestDeleted' bash Server/scripts/test-db.sh` → undefined handlers.
- [ ] **Step 3**: Worked handler (pattern for all others):
```go
func (s *Service) Refresh(ctx context.Context, in Refresh) (RefreshResult, error) {
	if len(in.ParentRefreshDigestCandidates) == 0 || in.Derive == nil || in.Material == nil { return RefreshResult{}, ErrInvalidCommand }
	tx, err := s.begin(ctx); if err != nil { return RefreshResult{}, ErrUnavailable }
	defer tx.Rollback(ctx)
	digests := make([][]byte, 0, len(in.ParentRefreshDigestCandidates)); for _, c := range in.ParentRefreshDigestCandidates { digests = append(digests, c.Sum[:]) }
	rows, err := tx.Query(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE refresh_digest = ANY($1) FOR UPDATE`, digests)
	parents, err := pgx.CollectRows(rows, pgx.RowToStructByName[SessionRow]); if err != nil { return RefreshResult{}, ErrUnavailable }
	if len(parents) != 1 { return RefreshResult{}, ErrSessionRevoked }
	parent := parents[0]; key := keyFor(in.ParentRefreshDigestCandidates, parent.RefreshDigest)
	if parent.RecoveryEpoch != s.epoch { return RefreshResult{}, ErrEpochChanged }
	switch parent.Status {
	case "consumed":
		if in.AttemptID != "" && in.AttemptID == parent.ConsumedAttemptID { return s.recoverChild(ctx, tx, parent, key, in) } // lost-ack: read-only, exact child
		return s.revokeLineageForReuse(ctx, tx, parent, in)                                                       // writes session_revoked audit row, commits, returns refresh_reused / refresh_attempt_conflict
	case "revoked": return RefreshResult{}, ErrSessionRevoked
	}
	now := in.At.UTC().Truncate(time.Microsecond)
	deadline := parent.DeadlineAt; if in.Mode == config.ModeSelfHosted { deadline = minTime(deadline, parent.LineageStartedAt.Add(365*24*time.Hour)) }
	if !now.Before(deadline) || !now.Before(parent.RefreshExpiresAt) { return RefreshResult{}, ErrReauthenticationRequired }
	acct, err := s.accountAuthority(ctx, tx, parent.AccountID, parent.DeviceID) // one batch: account status+chain_failed, device status, family_slots, membership role, entitlement (hosted)
	if err != nil { return RefreshResult{}, err }                               // ErrSessionRevoked for inactive account/device, ErrScopeFailed for chain_failed
	scope, familyID, role := acct.scope()                                       // auth/refresh.go:402-413
	derived, err := in.Derive(parent, key); if err != nil { return RefreshResult{}, ErrUnavailable }
	child := SessionRow{ID: derived.SessionID, AccountID: parent.AccountID, DeviceID: parent.DeviceID, LineageRootID: parent.LineageRootID, ParentID: parent.ID, LineageStartedAt: parent.LineageStartedAt,
		Scope: scope, FamilyID: familyID, Role: role, RecoveryEpoch: parent.RecoveryEpoch, TokenKeyID: in.OutputKeyID,
		AccessDigest: digest(in.OutputKey, "access", derived.AccessToken), RefreshDigest: digest(in.OutputKey, "refresh", derived.RefreshToken),
		AccessExpiresAt: minTime(now.Add(in.AccessTTL), minTime(now.Add(in.RefreshTTL), parent.DeadlineAt)), RefreshExpiresAt: minTime(now.Add(in.RefreshTTL), parent.DeadlineAt), DeadlineAt: parent.DeadlineAt, CreatedAt: now}
	material, materialDigest, err := in.Material(stageOf(child, parent)); if err != nil { return RefreshResult{}, ErrUnavailable }
	seq, _, err := s.appendAudit(ctx, tx, scopeRef{account, parent.AccountID}, KindSessionRotated, derived.CommandID, parent.AccountID, now, rotatedPayload(parent, child, in.AttemptID, materialDigest))
	if err != nil { return RefreshResult{}, err }
	child.IssuedSeq = seq
	b := &pgx.Batch{}
	b.Queue(`UPDATE sessions SET status='consumed', consumed_at=$2, consumed_attempt_id=NULLIF($3,'')::uuid, child_id=$4 WHERE id=$1 AND status='active'`, parent.ID, now, in.AttemptID, child.ID)
	b.Queue(insertSessionSQL, child.args()...)
	if in.AttemptID != "" {
		b.Queue(`DELETE FROM refresh_receipts WHERE child_id=$1`, parent.ID) // successor proves possession of the predecessor's child
		b.Queue(`INSERT INTO refresh_receipts(parent_id,child_id,attempt_id,account_id,device_id,material,material_digest,created_at,effective_deadline,refresh_expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			parent.ID, child.ID, in.AttemptID, parent.AccountID, parent.DeviceID, material, materialDigest[:], now, deadline, child.RefreshExpiresAt)
	}
	if err := drain(tx.SendBatch(ctx, b), 1); err != nil { return RefreshResult{}, ErrUnavailable } // drain asserts the UPDATE affected exactly 1 row
	if err := tx.Commit(ctx); err != nil { return RefreshResult{}, ErrUnavailable }
	s.publish(SecurityInvalidation{AccountID: parent.AccountID, SessionID: parent.ID, Revision: seq})
	return RefreshResult{Result: Result{CommandID: derived.CommandID, Outcome: "committed", Code: "ok", Revision: seq}, Child: child, Parent: parent, AccessToken: derived.AccessToken, RefreshToken: derived.RefreshToken, FamilySlot: acct.slot, Account: acct.view}, nil
}
```
No `command_receipts` row for rotations (the consumed parent row is the receipt; spec: consumed sessions pruned after the retry window). Every other handler writes a receipt. Implement SignIn/Logout/MarkIdentityUnavailable/ResolveIdentity/challenges per the table.
- [ ] **Step 4**: `rtk proxy env BT_TEST_PACKAGES='./internal/authority' bash Server/scripts/test-db.sh` green.
- [ ] **Step 5**: `rtk git commit -am "feat(authority): add identity and session command handlers"`

### Task 4: Family handlers
**Files:** Create `Server/internal/authority/family.go`, `family_test.go`.
**Interfaces:** Produces `CreateFamily{CommandID, OwnerAccountID, FamilyID, DisplayName string; At time.Time}`, `RepairFamilyProfile{...}`, `CreateInvitation{CommandID, CreatorAccountID, InvitationID, FamilyID, TokenDigest, Role string; ExpiresAt, At time.Time}`, `RedeemInvitation{CommandID, RedeemerAccountID, TokenDigestCandidates []string; At}`, `RevokeInvitation{CommandID, ActorAccountID, InvitationID string; At}`, `LeaveFamily{CommandID, AccountID; At}`, `RemoveMember{CommandID, OwnerAccountID, AccountID; At}`, `ChangeRole{CommandID, OwnerAccountID, AccountID, Role; At}`, `TransferOwner{CommandID, OldOwnerID, NewOwnerID; At}`; each `func (s *Service) X(ctx, in X) (Result, error)`; `CreateFamily` returns `(FamilyResult{Result; FamilyID string}, error)`; read helpers `func (s *Service) FamilySlot(ctx, accountID) (*FamilySlot, error)`, `func (s *Service) Invitation(ctx, familyID, invitationID) (InvitationView, bool, error)`, `func (s *Service) LookupInvitation(ctx, digests []string) (InvitationView, bool, error)` (preview; cross-family privacy: returns family display name + status only).
| Handler | Preconditions (error) | Locks | Effects | Audit |
|---|---|---|---|---|
| CreateFamily / hosted bootstrap | account active + device/session principal valid (state.go:319-323, 367-382); slot free else `family_slot_occupied` (state.go:439-441; enforced by `family_slots` PK on concurrent insert → map 23505 to the same code); family id unused | account | families INSERT (owner, display_name, status active, entitlement_required = hosted), memberships owner, family_slots active; hosted: family_entitlements family_sync/hosted_beta active expires = at.Truncate(second)+366d (state.go:446-462) | family: `family_created`/`family_bootstrapped`; account: same kind |
| RepairFamilyProfile | owner (state.go:384-388), entitled when required (state.go:411-427) | account, family | families display_name, profile_required=false | family: `family_profile_repaired` |
| CreateInvitation | owner; entitled; active unexpired invitations < MaxInvitations else `invitation_limit`; role caregiver/viewer; expires in future (state.go:477-505) | account, family | invitations INSERT | family: `invitation_created` {invitation_id, role, expires_at} |
| RedeemInvitation | slot free (`family_slot_occupied`); invitation by digest exists else `invitation_invalid`; redeemed→`invitation_consumed`, revoked→`invitation_revoked`, expired→`invitation_expired`; family active else `family_unavailable`; active members < MaxCaregivers else `caregiver_limit`; entitled (state.go:506-543) | redeemer account, family | invitations redeemed; memberships INSERT role=invitation.role; family_slots INSERT; redeemer's active sessions: scope stays until refresh (spec: refresh upgrades) — no session update | family + redeemer account: `invitation_redeemed` |
| RevokeInvitation | owner; entitled; invitation belongs to family else `invitation_invalid`; consumed/revoked codes as above (state.go:544-571) | account, family | invitations revoked | family: `invitation_revoked` |
| LeaveFamily / RemoveMember | member_left: self; member_removed: owner + entitled; target membership active else `membership_revoked`; target not owner else `owner_required` (state.go:572-593) | both accounts, family | memberships status removed; family_slots DELETE; target's active sessions revoked reason member_removed (event_handlers.go:627-638; spec: closes live connections after commit) | family + target account: kind |
| ChangeRole | owner; not self (`owner_required`); entitled; target active non-owner (state.go:594-619) | owner+target accounts, family | memberships role; target's active family sessions `UPDATE sessions SET role=$ WHERE account_id=$ AND family_id=$ AND status='active'` | family + target account: `member_role_changed` |
| TransferOwner | old owner is owner; entitled; new owner active caregiver with active account (state.go:620-640) | both accounts, family | memberships swap roles (use deferred update order: demote first then promote to satisfy `memberships_one_active_owner`), families.owner_account_id; both accounts' active family sessions role updated | family + both accounts: `owner_transferred` |
Hosted entitlement check = `family_entitlements` row family_sync active and unexpired (state.go:411-427). Exact retry on every handler returns the receipt before any check.
- [ ] **Step 1: Failing tests**: `TestCreateInviteRedeemReplay` (receipt replay returns same Result; invitation replay carries `SecretUnavailable` semantics at service layer), `TestConcurrentCreateVersusJoinClaimsOneSlot` (20 goroutines: 10 CreateFamily, 10 RedeemInvitation for one account → exactly one slot, all others `family_slot_occupied`), `TestConcurrentOwnerTransfersHaveOneWinner`, `TestRemoveMemberRevokesSessionsAndFreesSlot`, `TestChangeRoleUpdatesLiveSessionRows`, `TestFamilyCapsAreCheckedInsideTransaction`, `TestHostedBootstrapEntitlementExpiryIsEventTimePlus366Days`, `TestEntitlementExpiryDeniesNewCommandButReplayWins`.
- [ ] **Step 2**: run with `BT_TEST_RUN='TestCreate|TestConcurrent|TestRemove|TestChangeRole|TestFamilyCaps|TestHosted|TestEntitlement'` → undefined.
- [ ] **Step 3**: implement per table; map `pgconn.PgError` 23505 on `family_slots_pkey` → `family_slot_occupied`, on `memberships_one_active_owner` → `ErrInvalidCommand`.
- [ ] **Step 4**: green. **Step 5**: `rtk git commit -am "feat(authority): add family command handlers"`

### Task 5: Lifecycle handlers
**Files:** Create `Server/internal/authority/lifecycle.go`, `lifecycle_test.go`.
**Interfaces:** `ScheduleDeletion{CommandID, OwnerAccountID; At}`→`(DeletionResult{Result; FamilyID; PurgeAfter time.Time}, error)`, `RecoverFamily{CommandID, AccountID; At}`, `ForfeitRecovery{...}`, `DeleteAccount{CommandID, AccountID, IdentityKey; At}`, `StartFamilyPurge{CommandID, FamilyID; At}`, `CompleteFamilyPurge{CommandID, FamilyID, Receipt string; At}`, read `RecoveryStatus(ctx, accountID) (RecoveryStatus, error)`.
| Handler | Preconditions | Locks | Effects | Audit |
|---|---|---|---|---|
| ScheduleDeletion | owner (`owner_required`); member list snapshot must equal active members (state.go:648-684) | all member accounts (sorted), family | families status deletion_pending, purge_after=at+30d; every membership status former; non-owner slots DELETE; owner slot state family_recovery; family_recovery_claims INSERT active; all members' active sessions revoked reason family_deleted (event_handlers.go:741-780) | family + each member account: `family_deletion_scheduled` |
| RecoverFamily | principal scope `family_recovery` else `recovery_only`; claim active, slot family_recovery, family deletion_pending else `family_unavailable`/`recovery_forfeited`; now < purge_after else `recovery_expired` (state.go:685-713) | account, family | families active owner=account purge_after NULL; slot active; membership owner active; claim recovered (event_handlers.go:782-797) | family + account: `family_recovered` |
| ForfeitRecovery | as above minus deadline (state.go:714) | account, family | claim forfeited; slot DELETE (event_handlers.go:799-811) | family + account: `recovery_forfeited` |
| DeleteAccount | account active and identity key matches else `session_revoked`; owns no active family else `ownership_transfer_required` (state.go:715-730) | account, every family with a membership | accounts deleted; account_identities live=false; slot DELETE; all sessions revoked reason account_deleted; devices revoked; memberships removed; claims forfeited (event_handlers.go:813-847) | account + each family: `account_deleted` |
| StartFamilyPurge | family deletion_pending and at ≥ purge_after (event_handlers.go:849-866); actor = `RetentionWorkerActorID(instance)` (event.go:411-419) | family | status purge_started, purge_started_at, purge_command_id; owner slot DELETE | family: `family_purge_started` |
| CompleteFamilyPurge | status purge_started, same purge command (event_handlers.go:868-888) | family | status purge_completed, purge_receipt, purged_at; claims forfeited | family: `family_purge_completed` |
- [ ] Tests: `TestScheduleReservesOnlyOwnerRecoverySlot`, `TestRecoveryAtDeadlineDeniedAndForfeitIsExclusive` (recover vs forfeit race: one winner), `TestAccountDeletionRejectsActiveOwnerAndForfeitsClaim`, `TestPurgeStartRequiresDueDeadline`, `TestLifecycleReceiptsReplayAcrossFreshProof`.
- [ ] Run → red → implement → green → `rtk git commit -am "feat(authority): add lifecycle command handlers"`

### Task 6: Operator and integration handlers, invalidations
**Files:** Create `Server/internal/authority/{operator.go,integration.go,invalidation.go}` + tests.
**Interfaces:** Produces `OperatorRecoverOwner`, `OperatorEntitlement`, `OperatorIntegration` inputs identical field-for-field to `security.OperatorRecoveryInput`/`OperatorEntitlementInput`/`OperatorIntegrationInput` (security/operator_prepared.go:11-26, integration_prepared.go:13-27) with `ExpectedRevision` meaning **the family's `audit_seq`**; `func (s *Service) PreflightOperator*(ctx, in) error`, `ReplayOperator*(ctx, in) (Result, bool, error)`, `ExecuteOperator*(ctx, in) (Result, error)`; `ListFamilyIntegrations(ctx, familyID) ([]IntegrationView, error)`; `type SecurityInvalidation struct{AccountID, FamilyID, SessionID string; Revision uint64}`, `type InvalidationSink interface{ Invalidate(context.Context, SecurityInvalidation) }`, `func (s *Service) Subscribe(InvalidationSink) func()` (same semantics as security/invalidation.go:24-57; published after commit).
| Handler | Preconditions | Time rule | Effects | Audit |
|---|---|---|---|---|
| RecoverOwner | family active owned by old owner; old owner `identity_unavailable`; new owner active caregiver (operator_prepared.go:238-256); confirmation unexpired at commit (:230-232); `ExpectedRevision==family.audit_seq` else `stale_security_revision` | commit time captured after locks | roles swap, owner change, session roles updated | family + both accounts: `owner_recovered` with policy/evidence/reason/expected_revision |
| Entitlement grant/suspend | family active; grant: next expiry after now and after current (if exists, source hosted_beta active); suspend: exact current expiry (operator_prepared.go:258-285) | as above | family_entitlements upsert | family: `operator_entitlement_*` |
| Integration grant | family active (`family_unavailable`); account active owner (`owner_required`); grant id unused; verifier unused (`integration_verifier_reused`, also enforced by UNIQUE); epoch==server epoch else `epoch_changed` (integration_prepared.go:94-112, integration_events.go:156-182) | `at = now().UTC().Truncate(time.Second)` sampled **after** `lockFamilies`; expires = at+7776000s; exact retry returns receipt before any check (never restamps) | mcp_integrations INSERT | family + owner account: `integration_granted` |
| Integration revoke | grant exists with same account/family/epoch else `integration_invalid`; active else `integration_revoked` (integration_events.go:187-204) | — | status revoked | family: `integration_revoked` |
Preflight = same checks in a read-only tx without writes; Replay = receipt lookup only.
- [ ] Tests: `TestIntegrationExactRetryKeepsIssuanceTime` (commit at T, retry at T+2h with identical input → same Result, `expires_at` unchanged; changed name → `command_id_reused`), `TestIntegrationCommitTimeFollowsLockAcquisition` (hold family lock 1.5s in another tx; issued `created_at` ≥ lock release), `TestIntegrationRejectsReusedVerifierAndStaleRevision`, `TestOperatorRecoveryRequiresUnavailableOwnerAndCaregiverSuccessor`, `TestOperatorEntitlementSuspendRequiresExactExpiry`, `TestInvalidationsPublishedOnlyAfterCommit`.
- [ ] Run → red → implement → green → `rtk git commit -am "feat(authority): add operator, integration and invalidation handlers"`

### Task 7: Request-path authentication and guarded transactions
**Files:** Create `Server/internal/authority/{authenticate.go,guard.go,integration_read.go}` + `authenticate_test.go`, `guard_test.go`, `integration_read_test.go`.
**Interfaces:** Produces (this is the API Phase 2 builds on)
```go
type KeyedDigest = identity.KeyedDigest
func (s *Service) Authenticate(ctx context.Context, accessDigests []KeyedDigest) (access.Principal, error)
//   one pool query (no tx): SELECT <session cols>, a.status, a.chain_failed FROM sessions s JOIN accounts a ON a.id=s.account_id WHERE s.access_digest = ANY($1)
//   (the only join on the hot path; by PK on both sides) → access_expired | session_revoked | reauthentication_required | epoch_changed | scope_failed(503)
//   Principal: SecurityRevision=issued_seq, Scope = session scope (family_recovery only if the row's scope is), SessionScope, SessionFamilyID, AccessExpiresAt
func (s *Service) WithAccountTx(ctx context.Context, p access.Principal, run func(pgx.Tx) error) error
//   BEGIN; one batch: set_config(app.account_id=p.AccountID, app.family_id=''), SELECT status FROM sessions WHERE id=$1 → must be active (revocation after commit wins)
func (s *Service) WithFamilyTx(ctx context.Context, p access.Principal, action string, run func(pgx.Tx, access.Grant) error) error
//   BEGIN; one batch: set_config(account, family=p.SessionFamilyID), SELECT status,role,family_id FROM sessions WHERE id=$1,
//   SELECT status,audit_seq,chain_failed,entitlement_required FROM families WHERE id=$2, SELECT status,expires_at FROM family_entitlements WHERE family_id=$2 AND entitlement_key='family_sync'
//   → membership_revoked / read_only_caregiver / owner_required / entitlement_required / recovery_only / family_unavailable exactly as family.Authorize (family/service.go:134-189) using Options.Allowed;
//   Grant{AccountID, FamilyID, Role, Revision: families.audit_seq}; mutations lock nothing here (sync locks the family row itself, as today)
func (s *Service) WithFamilyReadTx(ctx, p, action, run) error         // same, pgx.TxOptions{IsoLevel: RepeatableRead, AccessMode: ReadOnly}
func (s *Service) CheckMany(ctx context.Context, leases []LiveLease) ([]bool, error) // one read-only tx; batch of per-lease session+family reads; replaces realtime.AuthorityAuthorizer.CheckMany
type LiveLease struct { Principal access.Principal; Grant access.Grant }
func (s *Service) CheckExportSubject(ctx context.Context, familyID, requesterID string, epoch uint64) error // owner+active family+active slot; replaces WithExportSnapshot's authority half (export_snapshot.go:40-49)
// MCP
type IntegrationIdentity struct { GrantID, AccountID, FamilyID string; ExpiresAt time.Time }
type IntegrationReader struct{ /* authority + dedicated read-only pool */ }
func NewIntegrationReader(a *Service, pool *pgxpool.Pool) (*IntegrationReader, error)
func (r *IntegrationReader) CheckIntegration(ctx, credential string) (IntegrationIdentity, error)
func (r *IntegrationReader) WithIntegrationRead(ctx, credential string, run func(context.Context, pgx.Tx, access.Grant) error) error
//   parse btmcp1 (security/integration_read.go:67-100 moved verbatim), one query by grant id, constant-time verifier compare, status/epoch/expiry, owner+family+slot live checks (:255-277), then read-only tx on the dedicated pool with tenant context
var ErrIntegrationUnauthorized = Code("integration_unauthorized")
```
- [ ] **Step 1: Failing tests**: `TestAuthenticateIsOneQueryAndMapsEveryCode` (table: active→ok; expired access→`access_expired`; revoked→`session_revoked`; consumed→`session_revoked`; past deadline→`reauthentication_required`; wrong epoch→`epoch_changed`; account chain_failed→`scope_failed`), `TestRevocationAppliesToNextTransaction` (start WithFamilyTx and block inside run; RemoveMember from another goroutine commits; in-flight run completes and commits; a new WithFamilyTx → `membership_revoked`), `TestCrossFamilyIsolationUnderTenantContext` (family A session inside WithFamilyTx cannot read family B's `babies` row; `SELECT count(*)`=0), `TestFamilyTxMapsPolicyCodes` (viewer+mutate_record→`read_only_caregiver`; caregiver+manage_members→`owner_required`; family_recovery slot→`recovery_only`; hosted expired entitlement→`entitlement_required`), `TestFamilyTxFailsClosedWhenFamilyChainFailed` (503 `scope_failed` for that family; another family OK), `TestCheckManyDeniesRemovedMemberInOneBatch`, `TestIntegrationReadDeniesAfterRevocation` (CheckIntegration ok; revoke; WithIntegrationRead → unauthorized, zero callbacks), `TestIntegrationReadRejectsSQLMutation`, `TestIntegrationReadExpiryAtExactDeadline`, `TestPhoneTokenAndIntegrationCredentialAreNotInterchangeable`.
- [ ] **Step 2**: run → red. **Step 3**: implement; `pgx.Batch` for the pipelined statements; statement cache is pgx default (`QueryExecModeCacheStatement`). **Step 4**: green.
- [ ] **Step 5**: `rtk git commit -am "feat(authority): add one-round-trip authentication and guarded transactions"`

### Task 8: Audit chain verifier, fail-closed, key inventory
**Files:** Create `Server/internal/authority/{verifier.go,keys.go}`, `verifier_test.go`, `keys_test.go`.
**Interfaces:** `type Verifier struct{}`; `func NewVerifier(s *Service, logger *slog.Logger) *Verifier`; `func (v *Verifier) VerifyIncremental(ctx context.Context, maxScopes int) (VerifyReport, error)` (scopes whose `audit_seq` > checkpoint, oldest checkpoint first; walks rows after the checkpoint recomputing hashes; writes checkpoint; mismatch → `UPDATE accounts|families SET chain_failed=true` + `logger.Error("audit chain mismatch", scope, id, seq)`); `func (v *Verifier) VerifyAll(ctx) (VerifyReport, error)` (every scope from genesis, ignores checkpoints; used by `baby-tracker-admin authority verify`); `type VerifyReport struct{ Scopes, Rows int; Failed []ScopeRef }`; `func (v *Verifier) Run(ctx, interval time.Duration)` loop. `keys.go`: `func (s *Service) ReferencedKeyIDs(ctx) (KeyInventory, error)` — `SELECT DISTINCT token_key_id FROM sessions WHERE status='active'` ∪ `continuity_key_id FROM devices WHERE status='active'` ∪ identity key ids ∪ invitation key ids; `type KeyInventory struct{ SessionToken, DeviceContinuity, Identity, Invitation []string }`.
- [ ] Tests: `TestVerifierAcceptsCleanChainsAndAdvancesCheckpoints`, `TestVerifierFailsClosedPerScope` (as DB owner tamper `payload` of one family's row 3 → VerifyIncremental marks that family failed; Authenticate for a member still ok at account level, WithFamilyTx → `scope_failed`; other family unaffected), `TestVerifyAllIgnoresCheckpoints`, `TestReferencedKeyIDsAreSortedAndIgnoreRevoked`.
- [ ] Run → red → implement → green → `rtk git commit -am "feat(authority): add incremental audit chain verifier and fail-closed scopes"`

### Task 9: Move legacy verification/replay code to `legacyledger`
**Files:** Create `Server/internal/legacyledger/{event.go,event_handlers.go,state.go,types.go,integration.go,ledger.go,head.go,json.go,doc.go}` by copying from `Server/internal/security/{event.go,event_handlers.go,state.go,types.go(Head,Candidate,Command,CommandResult,Err*),integration_events.go}` and `security/localstore/{ledger.go(read/walk only),head.go(Load only),json.go}`; copy tests `event_test.go`, `event_registry_test.go`, `reserved_events_test.go`, `lifecycle_commands_test.go` (Replay-based parts), `member_role_test.go`, `integration_events_test.go` (replay parts), `localstore/ledger_test.go` (read parts) with package renamed. `doc.go`: "Unused by the runtime; consumed only by the Phase 4 importer. Do not add dependencies on it."
**Interfaces:** Exports `legacyledger.VerifyCommittedChain(ctx, ledgerDir, headDir string) (Head, State, error)` (wraps localstore reader + `Replay`), `State` accessors as today.
- [ ] Step 1: write `legacyledger/replay_test.go::TestReplayFixtureChain` using an existing fixture ledger from `security/localstore` tests (committed envelope files) → fails (package missing). Step 2: run `rtk go test ./internal/legacyledger/...` (no DB). Step 3: copy + rename; `go vet` clean; add `//go:build !release` nothing — plain package. Step 4: green. Step 5: `rtk git commit -am "refactor(legacyledger): preserve ledger verification and replay for the importer"`

---
### Task 10 (1b): Switch `auth` and `identity` to the authority
**Files:** Modify `Server/internal/auth/{sessions.go,refresh.go,refresh_recovery.go,refresh_recovery_retention.go,repository.go,types.go,devices.go}`, `Server/internal/identity/{apple_notifications.go,repository.go,challenge.go}`; delete `auth/repository.go` SQL function calls (repository becomes a thin wrapper over `authority.Service` for challenges/notifications); tests ported per Task 15.
**Interfaces:** `auth.Options.Security *security.Service` → `Authority *authority.Service`; `auth.Service.Authenticate(ctx, raw)` computes `[]KeyedDigest` over the keyring (auth/refresh.go:308-327) and calls `Authority.Authenticate`; `Refresh` builds `authority.Refresh{Derive: DeriveRefreshRotation/DeriveRecoverableRefreshRotation, Material: EncodeRefreshStage/EncodeRecoveryRefreshStage}`; `Exchange` = verify Apple (+code exchange when hosted) **before** `Authority.SignIn`; `exchangeHosted` vault logic deleted; `NewRecoveryKeyReferences` deleted (replaced by `Authority.ReferencedKeyIDs`). `RefreshDerivationInput.HeadGeneration/HeadRevision/HeadHash/TargetRevision` keep their names but are fed `1`, `parent.IssuedSeq`, `sha256(parent.ID)`, `parent.IssuedSeq+1` so the v2/v3 derivation domains and frozen vectors are unchanged (`tokens_test.go` known answers stay valid).
- [ ] Tests red (compile) → implement → `rtk proxy env BT_TEST_PACKAGES='./internal/auth ./internal/identity' bash Server/scripts/test-db.sh` green with the Task 15 porting applied for these two packages → `rtk git commit -am "refactor(auth): authenticate and rotate sessions through the postgres authority"`

### Task 11 (1b): Switch `family`, `account`, `operator`
**Files:** Modify `Server/internal/family/{service.go,repository.go,lifecycle.go,entitlements.go,types.go}`, `Server/internal/account/lifecycle.go`, `Server/internal/operator/{security_authority.go,integration.go}`.
**Interfaces:** `family.Options.Security` → `Authority *authority.Service`; `family.Service.Authorize` is deleted (policy lives in `authority.WithFamilyTx` via `Options.Allowed = family.Allowed` exported from membership.go); repository reads use `Authority.WithFamilyTx/WithAccountTx`; `StageFamilyMaterial` deleted (display name is a command field); `operator.SecurityAuthority` wraps `authority.Service` directly, `securityAuthorityGate` becomes `interface{ Check(context.Context) error }` (readiness). `httpapi.LiveFamilyAuthorizer` interface implemented by a small adapter calling `WithFamilyReadTx`.
- [ ] Red → implement → `BT_TEST_PACKAGES='./internal/family ./internal/account ./internal/operator'` green → `rtk git commit -am "refactor(family,operator): execute commands through the postgres authority"`

### Task 12 (1b): Switch data readers and HTTP: sync, importer, export, realtime, familyread, mcpapi, httpapi, testkit
**Files:** Modify `Server/internal/sync/{mutation.go,paging.go,snapshot.go}` (guard type `authority.FamilyGuard` interface `{WithFamilyTx(...)}`), `Server/internal/importer/{resolver.go,conflict.go}`, `Server/internal/export/{service.go,worker.go,repository.go}` (`WithAuthorizedSnapshot`→`WithFamilyReadTx`; worker `invalidateChanged` → `CheckExportSubject`; archive audit reads `security_audit_events WHERE scope='family' AND scope_id=$1` under authority context via a new `authority.FamilyAuditPage` helper), `Server/internal/realtime/{authorizer.go,hub.go}` (`AuthorityAuthorizer` → `authority.Service.CheckMany`; `Invalidations invalidationSource` → `authority.Service`), `Server/internal/familyread/{service.go,history.go,summary.go,types.go}` (Guard = `*authority.IntegrationReader`), `Server/internal/mcpapi/handler.go` (`security.IntegrationIdentity`→`authority.IntegrationIdentity`), `Server/internal/httpapi/{router.go,auth.go,sync.go,live.go,account.go}` (`security.CommandResult`→`authority.Result`; `authReadinessCode` drops `recovery.ErrQuarantine`), `Server/internal/testkit/{fixtures.go,postgres.go}` (harness builds `authority.New` on `db.Runtime`; no ledger dirs; `Projector` pool removed), `Server/internal/testpg/database.go` (projector role still created; pool kept only for roles tests).
- [ ] Red → implement → `BT_TEST_PACKAGES='./internal/sync ./internal/importer ./internal/export ./internal/realtime ./internal/familyread ./internal/mcpapi ./internal/httpapi ./internal/records ./internal/testkit'` green → `rtk git commit -am "refactor(http,sync): use the postgres authority on the request path"`

### Task 13 (1b, big-bang switch): composition, admin CLI, config, deploy scripts, docs
**Files:** Modify `Server/cmd/baby-tracker-server/{main.go,mcp.go,identity_verifier_identitytest.go}`, `Server/cmd/baby-tracker-admin/main.go`; create `Server/cmd/baby-tracker-admin/authority_cli.go`; modify `Server/internal/config/{config.go,security.go,database_pools.go}`, `Server/compose.yaml`, `Server/Dockerfile`, `Server/scripts/{lib/disposable-sync.sh,smoke.sh,smoke_test.go,check-principals.sh}`, `Server/README.md`, `docs/operations/security-state.md`, `Server/internal/capacity/fixture_identitytest.go`.
**Interfaces:** `runtimeServices(cfg, runtimePool) (*authority.Service, *family.Service, *authority.Verifier, error)`; interim readiness `databaseReadiness.Check` = ping + `CompatibleVersion` + `VerifyRuntimeRole` + `server_state` row equals configured instance/epoch and `actor_key_commitment == recovery-style HMAC commitment of cfg.Auth.ActorIDPepper` (function `authority.ActorKeyCommitment(key [32]byte, instanceID string) [32]byte`, formula from recovery/actor_key.go:46-61) + cached key-inventory flag refreshed by the verifier loop every 5 min (startup computes it before serving); still per request (Phase 3 moves it off the request path). Admin: `authority init --instance-id --epoch 1 --confirm-instance` (inserts/updates `server_state` as the migrator; idempotent on identical values; refuses different values), `authority verify` (VerifyAll; exit 1 and print failed scopes), remove `security init|bind-actor-key|verify|reconcile`, `recovery rotate-epoch` → `authority rotate-epoch --expected-epoch --reason-code database_restore --confirm-instance` (bumps `server_state.recovery_epoch`; all sessions fail `epoch_changed`). Config: drop `BT_SECURITY_LEDGER_DIR`, `BT_LEDGER_HEAD_DIR`, `BT_PROJECTOR_DATABASE_URL_FILE`, hosted provider settings (reject if set). Compose: remove ledger volumes and projector env/secret from `app`/`migrate`; Dockerfile drops `/state`. Scripts: `security init` → `authority init`; delete `security reconcile`; `[[ $schema == 29 ...`; smoke replaces the actor-pepper replacement proof with `authority verify` after replacing the pepper → readiness 503 (`actor_id_key_mismatch` from readiness). README install steps updated; `docs/operations/security-state.md` rewritten: PostgreSQL authority, backups = database backup, `authority verify`, epoch rotation after restore.
- [ ] `rtk go build ./... && rtk go vet ./...` green; `rtk proxy env BT_TEST_PACKAGES='./cmd/... ./internal/config ./scripts' bash Server/scripts/test-db.sh` green (with Task 15 porting) → `rtk git commit -am "feat(server): compose the postgres authority and retire the file ledger"`

### Task 14 (1b): Delete the legacy runtime
**Files:** Delete `Server/internal/security/**`, `Server/internal/recovery/**`, `Server/internal/identity/hostedcredentials/**`, `identity/credentials.go`, `credentials_test.go`, `credentials_conformance_test.go`, `identity/credentialtest/`, `account/apple_revocation*.go`, `cmd/baby-tracker-server/{recovery_*.go,lifecycle_worker*.go,hosted_security_test.go,readiness_integrity_test.go}`, `cmd/baby-tracker-server/recovery_observation*.go`, `config/security.go` hosted parts; `database/roles.go` projector inventory and `VerifyRoleSeparation`; `testpg` Projector pool; `check-principals.sh` projector assertions become "projector has no privileges".
- [ ] `rtk grep -rn "internal/security\b\|internal/recovery\|hostedcredentials\|localstore" Server --include='*.go'` returns nothing; `rtk go build ./... && rtk go vet ./...` green; commit `rtk git commit -am "chore: remove file ledger, projector, recovery stores and hosted vault"`

### Task 15 (1b): Port, rewrite or delete tests
| Suite | Disposition |
|---|---|
| `security/transaction_test.go` (TestGuardRevocationWinsBeforeTransaction, TestAccountGuardRejectsSameEpochAuthorityChanges, TestGuardDistinguishesEpochChangeFromSameEpochRevocation, TestGuardRejectsSessionAtAbsoluteDeadline, TestTransactionLocalSettingsClearAfterEveryExit) | **Ported one-for-one** into `authority/guard_test.go` (Task 7 names) |
| `security/integration_read_test.go` (15 tests) | **Ported** into `authority/integration_read_test.go`; `TestIntegrationReadHoldsSharedFenceAgainstRevocation` rewritten as revocation-after-commit (no fence) |
| `security/integration_prepared_test.go` issuance-time, exact-retry, verifier-reuse, projection-repair | **Ported** (Task 6); projection-repair/orphan-candidate/warm-cache cases **deleted** (no projection) |
| `security/operator_prepared_test.go` | **Ported** except fence/projection-repair cases (deleted) |
| `security/family_commands_test.go`, `lifecycle_commands_test.go`, `member_role_test.go` | **Ported** into `authority/family_test.go`, `lifecycle_test.go` (Tasks 4–5) |
| `security/refresh_recovery_*_test.go`, `session_material_test.go` | **Ported** (Task 3) |
| `security/invalidation_test.go` | **Ported** (Task 6) |
| `security/{authority_cache,authority_successor,authority_view,constructor,coordinator,credential_cleanup,credential_inventory,identity_projector,projector}_test.go`, `security/localstore/**`, `security/hosted/**` | **Deleted** — test the cache, fence, CAS, projection and hosted adapters that no longer exist |
| `security/{event,event_registry,reserved_events,identity_resolution}_test.go` | **Moved** to `legacyledger` (Task 9); `TestIdentityAuthorityResolution*` also **rewritten** against `authority.ResolveIdentity` |
| `auth/{refresh,sessions,devices,refresh_recovery,refresh_recovery_retention,tokens,repository}_test.go` | **Ported**; `TestRefreshCASLossRepreparesAtNewRevision`, `TestRefreshAuthorityDriftRetryIsBounded`, `TestRecoveryRepairsCommittedProjectionAfterLostReply`, `TestRecoveryKeyEligibility*`, `TestAuthRepository*` **deleted** (CAS/drift/projection gone); `TestRecoveryExactRetryOneChild`, `TestRefreshLostResponseRevokesLineage`, `TestLogoutExactRetryReturnsReceiptAndNewCommandIsDenied`, `TestEpochInvalidatesAccessAndRefresh` kept verbatim |
| `auth/hosted_sessions_test.go`, `identity/credentials*_test.go`, `hostedcredentials/vault_test.go`, `account/apple_revocation*_test.go` | **Deleted** (vault removed; hosted ruling) |
| `identity/{apple_notifications,challenge,apple,apple_exchange,nonce}_test.go` | **Ported** (notifications/challenge via authority); apple/exchange/nonce unchanged |
| `family/*_test.go` | **Ported**; projection-winner assertions become receipt/row assertions |
| `operator/*_test.go` | **Ported**; `TestGatedSecurityAuthorityAllowsReceiptProbeButGatesPreflightAndCommit` keeps shape with readiness gate |
| `recovery/*_test.go` | **Deleted**; `TestEpochRotationIsMonotonic` **rewritten** as `admin authority rotate-epoch` test |
| `database/{identity_schema,identity_runtime,family_schema,family_projection,lifecycle_schema,lifecycle_projection,lifecycle_worker,integration_schema,refresh_recovery_schema}_test.go` | **Rewritten** as `authority_schema_test.go` (Task 1) + `authority_grants_test.go`; projection tests deleted |
| `database/roles_test.go` | **Rewritten** for `VerifyRuntimeRole`; projector-drift cases deleted |
| `httpapi/*_test.go`, `realtime/*_test.go`, `export/*_test.go`, `mcpapi/*_test.go`, `familyread/*_test.go`, `sync/*_test.go`, `records/*_test.go`, `importer/*_test.go` | **Ported** (fixture type changes only) |
| `cmd/baby-tracker-server/main_test.go` | **Ported**; `TestRuntimeGateRejectsSharedPool`, `TestHTTPAuthCompositionHasNoProjectorCapability`, `TestRecoveryKeyReadiness`, `readiness_integrity_test.go`, `recovery_*_test.go`, `hosted_security_test.go`, `lifecycle_worker_test.go` **deleted**; add `TestOldBinaryFailsClosedOnSchemaTwentyNine` (embedded compatibility max=28 copy rejects a 29 database → readiness 503) |
| `cmd/baby-tracker-admin/main_test.go` | security init/verify/bind/epoch tests **rewritten** as `authority init|verify|rotate-epoch` tests; `mcp_cli_test.go`, `cli_examples_test.go` **ported** |
| `tests/protocol/*`, `tests/identity/*`, `scripts/*_test.go` | **Ported**; `TestServerRestartReconcilesWithoutMemoryHint` keeps name, asserts restart with no reconcile step |
- [ ] `rtk proxy bash Server/scripts/test-db.sh` (full, `-race`) green; `rtk go vet ./...` clean; commit `rtk git commit -am "test: port security invariants to the postgres authority"`

### Task 16 (1b): Fresh-install end-to-end gates
- [ ] `rtk make server-build` ; `rtk make server-test` ; `(cd Server && rtk go vet ./...)`
- [ ] `rtk make server-protocol-e2e` (hosted default via identitytest exchanger) and `rtk env BT_RECOVERY_E2E_MODE=self_hosted make server-protocol-e2e`
- [ ] `rtk bash Server/scripts/mcp-e2e.sh`
- [ ] `rtk make server-smoke`
- [ ] Commit any script fixes: `rtk git commit -am "test(e2e): run protocol, mcp and smoke gates against the postgres authority"`

### Critical Files for Implementation
- <repo>/.worktrees/postgres-authority/Server/migrations/000029_postgres_authority.sql (new)
- <repo>/.worktrees/postgres-authority/Server/internal/authority/service.go, sessions.go, guard.go (new)
- <repo>/.worktrees/postgres-authority/Server/internal/security/state.go (behaviour source: preconditions at :291-734)
- <repo>/.worktrees/postgres-authority/Server/internal/auth/refresh.go and refresh_recovery.go (behaviour source for rotation/recovery)
- <repo>/.worktrees/postgres-authority/Server/cmd/baby-tracker-server/main.go (composition switch)
