-- 000037_family_delete_cascade: Phase 5's one schema migration. It holds the
-- family delete fix below and the tuning DDL the 10,000-family measurements
-- justified (Task 22: Phase 5 codifies its tuning in this file):
--
-- 1. A raw DELETE FROM families no longer depends on trigger-name order:
--    import_mappings_shared_baby_fk cascades like the other family-owned
--    import rows (Phase 4 Task 26 M4, Phase 5 C41).
-- 2. Task 21 C1: the three row-security context helpers app.authority_context(),
--    app.current_family_id() and app.current_account_id() are inlinable.
--    Evidence (2x run d1-20261024-1613, t21-candidates.md B): every policy is
--    built from them, and with SET search_path PostgreSQL never inlines a
--    function, so each call site in each statement ran the SQL-function
--    executor (GUC push and pop, re-plan of the body per execution on 17).
--    request_authority took 1.40 ms a call over 27 cached buffer hits and no
--    reads (788,324 calls, 1,105 s); the live-lease check spent 79 us a row
--    on about 14 hits; the registration sweep about 1 ms a registration.
-- 3. Task 21 C2: the Live Activity work sweep runs in pages, each its own short
--    transaction (app.sweep_live_activity_work_page). Evidence (same run,
--    candidate A1): app.sweep_live_activity_work() was one 25.6 s transaction
--    that kept every swept family's live_activity_family_dirty row locked to
--    its commit; the device upsert's trigger locks that row first, so 228 of
--    240 blocked lock samples were the upsert (transactionid ShareLock, waits
--    up to 9.7 s, 351 ms a call over 286 hits), in bursts at the sweep's
--    cadence.
-- 4. Task 21 R1: app.request_authority reads sessions once. Evidence (2x run
--    w2A2, d1-20261024-1613/runs/7, t21-roundtrips.md R1): it runs on every
--    request but a refresh (1,005,237 calls in the 15-minute window) and was
--    the largest consumer of CPU inside statements, 38% of it (822 s of
--    execution less read time, 0.82 ms a call in the run, 0.09 ms isolated).
--    Each call probed sessions by the access digests to count the matches,
--    then joined sessions again by the digest the probe found.
-- 5. Task 21 S1: the pull reads its high water through app.family_high_water,
--    so its cached plan no longer names families. Evidence (VM 2x-2.5x runs,
--    vm-v2-analysis.md "the stall at ~40 s past every minute"): every write
--    advances families.next_sequence (~1,200 rows a minute at 2x), so
--    autovacuum vacuums and analyzes families on every launcher pass; each
--    pass updates families' pg_class row, which invalidates every plan that
--    names the table. Every pooled backend then re-planned the pull's
--    generic plan over all 16 care_entities partitions within the same
--    second: 53-85 re-plans a minute at 28-46 ms each (maxima 0.3-0.93 s),
--    a ~0.5 s stall each minute that admission shed as 503 server_busy.
--    With families autovacuum off as an A/B the pull's re-plans fell from
--    806 to 67 in 10 minutes and the minute bursts went away.
--
-- 1. The import mapping key.
--
-- A family delete reaches an import mapping two ways: families cascades to
-- import_sessions, which cascades to import_baby_mappings, and families
-- cascades to care_entities, whose delete ran this key's NO ACTION check.
-- PostgreSQL fires families' cascade triggers in name order, and the names
-- follow the constraints' oids, so the delete worked only while the
-- import_sessions cascade happened to fire first and remove the mapping
-- before the check looked for it. With ON DELETE CASCADE the care side
-- removes the mapping itself, whichever cascade runs first.
--
-- The key is otherwise unchanged: same columns, same referenced key, NO
-- ACTION on update, validated. The purge (app.purge_family_step) still
-- deletes import staging before the babies, the order that kept it clear of
-- the check; with the cascade that order is no longer load-bearing.
ALTER TABLE public.import_baby_mappings DROP CONSTRAINT import_mappings_shared_baby_fk;
ALTER TABLE public.import_baby_mappings ADD CONSTRAINT import_mappings_shared_baby_fk
  FOREIGN KEY (family_id, shared_baby_id) REFERENCES public.care_entities(family_id, id) ON DELETE CASCADE;

-- 2. Inlinable context helpers.
--
-- The planner inlines a SQL function only without a SET clause, so these drop
-- `SET search_path TO 'pg_catalog'`. That clause kept the bodies from resolving
-- a name through the caller's search_path; an inlined body is parsed in the
-- caller's search_path instead, so every name in it is schema-qualified:
-- pg_catalog.current_setting, OPERATOR(pg_catalog.=), pg_catalog.uuid.
-- NULLIF resolves its `=` through the search_path, so the two id helpers
-- spell it as a CASE: '' and an unset setting (NULL) are NULL, any other
-- text is cast to uuid and fails with 22P02 when it is not one, as before.
-- COALESCE and CASE are syntax, not names. Signatures, volatility (STABLE),
-- owner and grants are unchanged (CREATE OR REPLACE keeps the owner and the
-- ACL and drops the SET clause), so the privilege manifest rows for them
-- are 36's. Inlining needs EXECUTE for the role planning the statement: the
-- runtime and the definers' owner have it, as before.
CREATE OR REPLACE FUNCTION app.authority_context() RETURNS boolean
    LANGUAGE sql STABLE
    AS $$ SELECT COALESCE(pg_catalog.current_setting('app.authority', true) OPERATOR(pg_catalog.=) '1', false) $$;

CREATE OR REPLACE FUNCTION app.current_account_id() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$ SELECT CASE WHEN pg_catalog.current_setting('app.account_id', true) OPERATOR(pg_catalog.=) '' THEN NULL
         ELSE pg_catalog.current_setting('app.account_id', true)::pg_catalog.uuid END $$;

CREATE OR REPLACE FUNCTION app.current_family_id() RETURNS uuid
    LANGUAGE sql STABLE
    AS $$ SELECT CASE WHEN pg_catalog.current_setting('app.family_id', true) OPERATOR(pg_catalog.=) '' THEN NULL
         ELSE pg_catalog.current_setting('app.family_id', true)::pg_catalog.uuid END $$;

-- 3. The work sweep in pages.
--
-- One call is app.sweep_live_activity_work()'s loop over at most p_limit
-- families, in family order after p_after (NULL: from the first), and the
-- worker commits each call on its own. It returns the last family of a full
-- page, the next call's p_after, or NULL once a page comes back short: the
-- pass is over. Each family's dirty row is locked from its page's call to
-- that call's commit, never for the whole pass. A pass still visits every
-- family registered throughout it exactly once, at a strictly rising cursor;
-- a family whose sequence moves after its page is caught by the change feed
-- or the next pass (seq > journal_sequence), as a change during the old
-- single transaction was. The body is the old function's, statement for
-- statement. app.sweep_live_activity_work() stays (its signature is in the
-- manifest) and runs the same pass in one transaction.
CREATE FUNCTION app.sweep_live_activity_work_page(p_after uuid, p_limit integer) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE r record; seq bigint; realm public.server_state; last_family uuid; n integer:=0; previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
 IF p_limit IS NULL OR p_limit<1 OR p_limit>1000 THEN RAISE EXCEPTION 'invalid sweep page' USING ERRCODE='22023'; END IF;
 SELECT * INTO realm FROM public.server_state WHERE singleton;IF NOT FOUND THEN RETURN NULL;END IF;
 FOR r IN SELECT DISTINCT family_id FROM public.live_activity_registration_index WHERE NOT revoked AND instance_id=realm.instance_id AND recovery_epoch=realm.recovery_epoch
   AND (p_after IS NULL OR family_id>p_after) ORDER BY family_id LIMIT p_limit LOOP
 PERFORM set_config('app.family_id',r.family_id::text,true);
 SELECT next_sequence INTO seq FROM public.families WHERE id=r.family_id;
 IF EXISTS(SELECT FROM public.live_activity_family_dirty WHERE family_id=r.family_id AND authority_checked_at<=clock_timestamp()-interval '60 seconds') OR seq>coalesce((SELECT journal_sequence FROM public.live_activity_family_dirty WHERE family_id=r.family_id AND instance_id=realm.instance_id AND recovery_epoch=realm.recovery_epoch),-1) THEN
 PERFORM app.dirty_live_activity_family(r.family_id,seq);
 UPDATE public.live_activity_family_dirty SET authority_checked_at=clock_timestamp() WHERE family_id=r.family_id;
 END IF;
 last_family:=r.family_id; n:=n+1;
 END LOOP;
 PERFORM set_config('app.family_id',previous_family,true);
 RETURN CASE WHEN n<p_limit THEN NULL ELSE last_family END;
END $$;
REVOKE ALL ON FUNCTION app.sweep_live_activity_work_page(uuid, integer) FROM PUBLIC;
GRANT ALL ON FUNCTION app.sweep_live_activity_work_page(uuid, integer) TO baby_tracker_maintenance;

-- 4. One sessions read per request.
--
-- 36's body counted the sessions matching the digests (count(*) OVER ()
-- ... LIMIT 1), or found the session by id, then ran the six-table join by
-- the digest that probe returned: two statements, two sessions lookups. The
-- join now runs once, by the digests (or the id) themselves, and counts its
-- own rows. Every join is to a unique key (accounts_pkey, family_slots_pkey
-- on the account, memberships_pkey, families_pkey, family_entitlements_pkey
-- with the key fixed) and accounts is the session's foreign key, so the
-- join has exactly one row per matching session and its window count is
-- the probe's count. With two matches the row returned is the lowest
-- digest's, the order the probe's sessions_pkey scan returned them in (the
-- caller refuses any count but 1 before it reads a column). By id there is
-- at most one row (sessions_id_key).
--
-- The function plans its statements once per connection
-- (plan_cache_mode = force_generic_plan). With the digests in the join, a
-- generic plan assumes ten of them (an array parameter's default length),
-- so its estimate is ten times a custom plan's and the plan cache would
-- plan the join again on every call; the generic plan has the custom
-- plans' shape, every table by its unique key from the sessions_pkey
-- lookup. 36 planned its probe on every call for the same reason; its
-- join, by one digest, was generic.
--
-- Nothing else changes: the signature, the definer and its search_path,
-- app.authority raised before the first read and restored before the
-- context is set, the context set only for a live session (the same
-- predicate), the matches cap of 2 and every returned column. CREATE OR
-- REPLACE keeps the owner and grants, so the request_authority manifest
-- row is 36's. One answer differs, for a context no caller sets: when the
-- transaction already holds text that is no uuid in app.family_id or
-- app.account_id, a lookup that matches nothing now fails with 22P02 (the
-- joined tables' policies cast the setting to uuid), as a lookup that matches
-- already did; 36 skipped the join then. Only request_authority and the
-- definers set those settings, from uuids or ''.
CREATE OR REPLACE FUNCTION app.request_authority(p_access_digests bytea[], p_session_id uuid, p_family boolean, p_recovery_epoch bigint) RETURNS TABLE(matches integer, access_digest bytea, token_key_id text, session_id uuid, account_id uuid, device_id uuid, scope text, session_family_id uuid, session_role text, session_status text, recovery_epoch bigint, issued_seq bigint, access_expires_at timestamp with time zone, deadline_at timestamp with time zone, lineage_started_at timestamp with time zone, account_status text, account_chain_failed boolean, slot_family_id uuid, slot_state text, member_role text, member_status text, family_id uuid, family_status text, family_audit_seq bigint, family_chain_failed boolean, family_entitlement_required boolean, entitlement_status text, entitlement_expires_at timestamp with time zone)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    SET plan_cache_mode TO 'force_generic_plan'
    AS $$
#variable_conflict use_column
DECLARE
  previous_authority text := coalesce(current_setting('app.authority',true),'');
BEGIN
  IF p_family IS NULL OR p_recovery_epoch IS NULL OR (p_access_digests IS NULL) = (p_session_id IS NULL)
     OR (p_access_digests IS NOT NULL AND cardinality(p_access_digests) NOT BETWEEN 1 AND 8) THEN
    RAISE EXCEPTION 'invalid request authority lookup' USING ERRCODE='22023';
  END IF;
  PERFORM set_config('app.authority','1',true);
  -- The family key is coalesce(s.family_id, fs.family_id) written as a CASE:
  -- a COALESCE here is not an index key under the families policy (000036).
  IF p_session_id IS NULL THEN
    SELECT count(*) OVER (), s.access_digest, s.token_key_id, s.id, s.account_id, s.device_id, s.scope::text, s.family_id, s.role::text,
           s.status::text, s.recovery_epoch, s.issued_seq, s.access_expires_at, s.deadline_at, s.lineage_started_at, a.status::text,
           a.chain_failed, fs.family_id, fs.state::text, m.role::text, m.status::text, f.id, f.status::text, f.audit_seq, f.chain_failed,
           f.entitlement_required, e.status::text, e.expires_at
      INTO matches, access_digest, token_key_id, session_id, account_id, device_id, scope, session_family_id, session_role,
           session_status, recovery_epoch, issued_seq, access_expires_at, deadline_at, lineage_started_at, account_status,
           account_chain_failed, slot_family_id, slot_state, member_role, member_status, family_id, family_status, family_audit_seq,
           family_chain_failed, family_entitlement_required, entitlement_status, entitlement_expires_at
      FROM public.sessions s
      JOIN public.accounts a ON a.id = s.account_id
      LEFT JOIN public.family_slots fs ON fs.account_id = s.account_id
      LEFT JOIN public.memberships m ON m.family_id = fs.family_id AND m.account_id = s.account_id
      LEFT JOIN public.families f ON p_family
        AND f.id = CASE WHEN s.family_id IS NOT NULL THEN s.family_id ELSE fs.family_id END
      LEFT JOIN public.family_entitlements e ON e.family_id = f.id AND e.entitlement_key = 'family_sync'
     WHERE s.access_digest = ANY(p_access_digests)
     ORDER BY s.access_digest LIMIT 1;
  ELSE
    SELECT 1, s.access_digest, s.token_key_id, s.id, s.account_id, s.device_id, s.scope::text, s.family_id, s.role::text,
           s.status::text, s.recovery_epoch, s.issued_seq, s.access_expires_at, s.deadline_at, s.lineage_started_at, a.status::text,
           a.chain_failed, fs.family_id, fs.state::text, m.role::text, m.status::text, f.id, f.status::text, f.audit_seq, f.chain_failed,
           f.entitlement_required, e.status::text, e.expires_at
      INTO matches, access_digest, token_key_id, session_id, account_id, device_id, scope, session_family_id, session_role,
           session_status, recovery_epoch, issued_seq, access_expires_at, deadline_at, lineage_started_at, account_status,
           account_chain_failed, slot_family_id, slot_state, member_role, member_status, family_id, family_status, family_audit_seq,
           family_chain_failed, family_entitlement_required, entitlement_status, entitlement_expires_at
      FROM public.sessions s
      JOIN public.accounts a ON a.id = s.account_id
      LEFT JOIN public.family_slots fs ON fs.account_id = s.account_id
      LEFT JOIN public.memberships m ON m.family_id = fs.family_id AND m.account_id = s.account_id
      LEFT JOIN public.families f ON p_family
        AND f.id = CASE WHEN s.family_id IS NOT NULL THEN s.family_id ELSE fs.family_id END
      LEFT JOIN public.family_entitlements e ON e.family_id = f.id AND e.entitlement_key = 'family_sync'
     WHERE s.id = p_session_id;
  END IF;
  matches := least(coalesce(matches,0),2);
  PERFORM set_config('app.authority',previous_authority,true);
  IF matches = 1 AND session_status = 'active' AND account_status = 'active' AND recovery_epoch = p_recovery_epoch
     AND NOT account_chain_failed THEN
    PERFORM set_config('app.account_id',account_id::text,true);
    PERFORM set_config('app.family_id',
      CASE WHEN p_family AND family_status = 'active' AND NOT family_chain_failed AND scope <> 'family_recovery' THEN family_id::text ELSE '' END,
      true);
  ELSE
    PERFORM set_config('app.account_id','',true);
    PERFORM set_config('app.family_id','',true);
  END IF;
  RETURN NEXT;
END $$;

-- 5. The pull's high water off the pull's plan.
--
-- The pull (care.ReadPull) read families.next_sequence in its own statement
-- (WITH hw AS (SELECT next_sequence FROM families WHERE id = $1) ...), so its
-- cached plan named families, and every VACUUM or ANALYZE of families
-- invalidated it. It now reads it through this function: the planner never
-- inlines a PL/pgSQL function, so the pull's plan names care_entities alone
-- and only its own partitions' statistics re-plan it. The function's own
-- statement, a families_pkey lookup, is what families' vacuum re-plans now.
--
-- The answer is the old CTE's: SECURITY INVOKER, so the families policy is
-- the caller's (a family the tenant cannot see is no row, the pull's
-- not_found), and STABLE, so its statement runs in the calling statement's
-- snapshot (a STABLE function takes no snapshot of its own): the high water
-- and the changes it bounds are still read from one snapshot, whatever the
-- isolation level. ROWS 1 is the estimate the CTE had (one key lookup).
CREATE FUNCTION app.family_high_water(p_family uuid) RETURNS SETOF bigint
    LANGUAGE plpgsql STABLE ROWS 1
    SET search_path TO 'pg_catalog'
    AS $$
BEGIN
  RETURN QUERY SELECT f.next_sequence FROM public.families f WHERE f.id = p_family;
END $$;
REVOKE ALL ON FUNCTION app.family_high_water(uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION app.family_high_water(uuid) TO baby_tracker_runtime;

-- The manifest at 37 is 36's plus the page function, maintenance only, and
-- the pull's high water, runtime only.
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 37,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=36;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 VALUES (37,'baby_tracker_maintenance','function','app.sweep_live_activity_work_page(uuid,integer)','EXECUTE'),
        (37,'baby_tracker_runtime','function','app.family_high_water(uuid)','EXECUTE');
UPDATE public.schema_compatibility SET current_version=37,
 introduced_by='000037_family_delete_cascade.sql',recorded_at=now() WHERE singleton;
