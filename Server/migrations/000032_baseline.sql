-- 000032_baseline FROZEN (Phase 4 Task 28): later changes go in 000033+.
-- 000032_baseline: the schema of 000001–000032, squashed before the homelab
-- cutover (Phase 4 Task 26). Later changes go in 000033+.
--
-- Built from a schema-only pg_dump (PostgreSQL 17.6, --no-owner
-- --no-tablespaces) of a fresh chain install, made portable:
--   - no owners: every object belongs to the migrating login that runs this file;
--   - the monthly security_audit_events_YYYY_MM partitions are not dumped but
--     created at the end by app.ensure_audit_partitions(now() + 92 days);
--   - the push_token_authority policy names CURRENT_USER (the migrator);
--   - a CHECK whose first conjunct was a BETWEEN states it again as a BETWEEN
--     (the dump's "(x >= a) AND (x <= b)" would reparse into a flatter tree);
--   - function bodies are not checked at creation (SET LOCAL below);
--   - 000002's REVOKEs on the public schema and the database are restated;
--   - foreign keys are added in the chain's order, so each table's internal
--     RI triggers sort, and its cascades fire, in the chain's order;
--   - schema_migrations itself is created by the runner before this file runs;
--   - the version-32 privilege manifest and the compatibility row are data.
-- Then the legacy roles were dropped from it (Phase 4 Task 27, C2): no grant
-- to, and no manifest row for, the projector or Apple-revoker group, and the
-- manifest's principal CHECK names only the runtime and maintenance groups.
-- The cutover's R6 drops the roles themselves.
-- internal/database TestBaselineCatalogMatchesTheChainGolden pins the catalog
-- this file builds (migrate/testdata/catalog-32.golden), and
-- TestBaselineDropsExactlyTheLegacyRoles pins its one difference from the
-- catalog the chain built: the two roles.

-- The dump creates functions before the tables their SQL bodies name (the
-- chain validated every body when it created it): bodies are not checked
-- while this file runs. The value in force before it is saved here and put
-- back at its end, so a caller applying it inside a longer transaction (the
-- legacy importer) keeps its own setting for what follows.
SELECT set_config('baby_tracker.baseline_check_function_bodies', current_setting('check_function_bodies'), true);
SET LOCAL check_function_bodies = false;

-- Privileges a schema-only dump does not carry (000002): PUBLIC creates
-- nothing in public and no temporary table in this database.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
DO $$
BEGIN
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', current_database());
END
$$;

--
-- Name: app; Type: SCHEMA; Schema: -; Owner: -
--

CREATE SCHEMA app;


--
-- Name: public; Type: SCHEMA; Schema: -; Owner: -
--

-- *not* creating schema, since initdb creates it


--
-- Name: account_status; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.account_status AS ENUM (
    'active',
    'identity_unavailable',
    'deleted'
);


--
-- Name: apns_environment; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.apns_environment AS ENUM (
    'sandbox',
    'production'
);


--
-- Name: audit_kind; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.audit_kind AS ENUM (
    'account_registered',
    'identity_unavailable',
    'identity_restored',
    'device_registered',
    'device_binding_revoked',
    'session_granted',
    'session_rotated',
    'session_revoked',
    'identity_session_issued',
    'family_created',
    'family_bootstrapped',
    'family_profile_repaired',
    'invitation_created',
    'invitation_revoked',
    'invitation_redeemed',
    'member_left',
    'member_removed',
    'member_role_changed',
    'owner_transferred',
    'owner_recovered',
    'entitlement_granted',
    'entitlement_suspended',
    'operator_entitlement_granted',
    'operator_entitlement_suspended',
    'family_deletion_scheduled',
    'family_recovered',
    'recovery_forfeited',
    'account_deleted',
    'family_purge_started',
    'family_purge_completed',
    'integration_granted',
    'integration_revoked',
    'legacy_imported'
);


--
-- Name: audit_scope; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.audit_scope AS ENUM (
    'account',
    'family'
);


--
-- Name: care_operation; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.care_operation AS ENUM (
    'create',
    'update',
    'delete',
    'timer_start',
    'timer_finish_bottle',
    'timer_stop'
);


--
-- Name: claim_status; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.claim_status AS ENUM (
    'active',
    'recovered',
    'forfeited'
);


--
-- Name: device_status; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.device_status AS ENUM (
    'active',
    'revoked'
);


--
-- Name: entitlement_status; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.entitlement_status AS ENUM (
    'active',
    'suspended'
);


--
-- Name: family_status; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.family_status AS ENUM (
    'active',
    'deletion_pending',
    'purge_started',
    'purge_completed'
);


--
-- Name: integration_status; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.integration_status AS ENUM (
    'active',
    'revoked'
);


--
-- Name: invitation_status; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.invitation_status AS ENUM (
    'active',
    'redeemed',
    'revoked'
);


--
-- Name: member_role; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.member_role AS ENUM (
    'owner',
    'caregiver',
    'viewer'
);


--
-- Name: member_status; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.member_status AS ENUM (
    'active',
    'removed',
    'former'
);


--
-- Name: page_kind; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.page_kind AS ENUM (
    'sync',
    'snapshot'
);


--
-- Name: revoke_reason; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.revoke_reason AS ENUM (
    'refresh_reused',
    'device_revoked',
    'identity_unavailable',
    'member_removed',
    'family_deleted',
    'account_deleted',
    'epoch_changed'
);


--
-- Name: session_scope; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.session_scope AS ENUM (
    'account',
    'family',
    'family_recovery'
);


--
-- Name: session_status; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.session_status AS ENUM (
    'active',
    'consumed',
    'revoked'
);


--
-- Name: slot_state; Type: TYPE; Schema: app; Owner: -
--

CREATE TYPE app.slot_state AS ENUM (
    'active',
    'family_recovery'
);


--
-- Name: abandon_live_activity_start(jsonb, text, text, boolean, boolean); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.abandon_live_activity_start(p_claim jsonb, p_generation text, p_attempt text, p_absence_allowed boolean, p_absence_used boolean) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE d public.live_activity_delivery;
BEGIN
 SELECT * INTO d FROM public.live_activity_delivery WHERE job_id=(p_claim->>'job_id')::uuid FOR UPDATE;
 IF NOT FOUND OR d.lease_id IS NULL OR
 (d.lease_id,d.registration_id,d.leased_registration_version,d.account_id,d.device_id,d.family_id,d.baby_id,d.kind,d.instance_id,d.recovery_epoch,d.leased_dirty_version,d.lease_deadline)
 IS DISTINCT FROM ((p_claim->>'lease_id')::uuid,(p_claim->>'registration_id')::uuid,(p_claim->>'registration_version')::bigint,
 (p_claim->>'account_id')::uuid,(p_claim->>'device_id')::uuid,(p_claim->>'family_id')::uuid,(p_claim->>'baby_id')::uuid,p_claim->>'kind',
 (p_claim->>'instance_id')::uuid,(p_claim->>'recovery_epoch')::bigint,(p_claim->>'dirty_version')::bigint,(p_claim->>'lease_deadline')::timestamptz)
 OR (d.target_state->>'generation_id',d.target_state->>'attempt_id',d.target_state->>'start_attempt_id',d.target_state->>'status',d.target_state->>'pending_event')
 IS DISTINCT FROM (p_generation,p_attempt,p_attempt,'startUnconfirmed','start')
 THEN RETURN false;END IF;
 UPDATE public.live_activity_delivery SET target_state=(target_state-'attempt_id'-'start_attempt_id'-'pending_event'-'retry_at')||
 jsonb_build_object('status','pending','absence_retry_allowed',p_absence_allowed,'absence_retry_used',p_absence_used),
 not_before=clock_timestamp(),lease_id=NULL,lease_deadline=NULL,leased_dirty_version=NULL,leased_registration_version=NULL WHERE job_id=d.job_id;
 RETURN true;
END $$;


--
-- Name: authority_actor_label(uuid, uuid, boolean, timestamp with time zone); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.authority_actor_label(p_family_id uuid, p_account_id uuid, p_former boolean, p_at timestamp with time zone) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE
  label text;
  previous_family text := coalesce(current_setting('app.family_id',true),'');
BEGIN
  IF NOT app.authority_context() THEN
    RAISE EXCEPTION 'actor labels require the authority context' USING ERRCODE='42501';
  END IF;
  IF p_family_id IS NULL OR p_account_id IS NULL OR p_former IS NULL OR p_at IS NULL OR NOT isfinite(p_at) THEN
    RAISE EXCEPTION 'invalid actor label' USING ERRCODE='22023';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM public.memberships m WHERE m.family_id=p_family_id AND m.account_id=p_account_id
      AND CASE WHEN p_former THEN m.status IN ('removed','former') ELSE m.status='active' END) THEN
    RAISE EXCEPTION 'actor label requires a matching membership' USING ERRCODE='23514';
  END IF;
  PERFORM set_config('app.family_id',p_family_id::text,true);
  IF p_former THEN
    UPDATE public.family_actor_label_history SET display_name='Former caregiver',former=true WHERE family_id=p_family_id AND account_id=p_account_id;
    UPDATE public.family_actor_labels SET display_name='Former caregiver',former=true,updated_at=p_at WHERE family_id=p_family_id AND account_id=p_account_id;
    INSERT INTO public.family_actor_label_history(family_id,account_id,display_name,former,recorded_at)
    VALUES (p_family_id,p_account_id,'Former caregiver',true,p_at);
  ELSE
    SELECT a.display_name INTO label FROM public.accounts a WHERE a.id=p_account_id AND a.status='active';
    IF NOT FOUND THEN
      RAISE EXCEPTION 'actor label account unavailable' USING ERRCODE='23514';
    END IF;
    INSERT INTO public.family_actor_labels(family_id,account_id,display_name,former,created_at,updated_at)
    VALUES (p_family_id,p_account_id,label,false,p_at,p_at)
    ON CONFLICT (family_id,account_id) DO UPDATE SET display_name=EXCLUDED.display_name,former=false,updated_at=EXCLUDED.updated_at;
    INSERT INTO public.family_actor_label_history(family_id,account_id,display_name,former,recorded_at)
    VALUES (p_family_id,p_account_id,label,false,p_at);
  END IF;
  PERFORM set_config('app.family_id',previous_family,true);
END $$;


--
-- Name: authority_context(); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.authority_context() RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path TO 'pg_catalog'
    AS $$ SELECT coalesce(current_setting('app.authority', true) = '1', false) $$;


--
-- Name: claim_live_activity_work(timestamp with time zone, integer); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.claim_live_activity_work(p_now timestamp with time zone, p_limit integer) RETURNS SETOF jsonb
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
BEGIN
 IF p_now IS NULL OR NOT isfinite(p_now) OR p_limit IS NULL OR p_limit<1 OR p_limit>1000 THEN RAISE EXCEPTION 'invalid claim bounds' USING ERRCODE='22023'; END IF;
 PERFORM app.retire_stale_live_activity_registrations();
 RETURN QUERY WITH due AS (
 SELECT d.job_id FROM public.live_activity_delivery d JOIN public.live_activity_registration_index r USING(registration_id)
 WHERE d.dirty_version>d.completed_version AND d.not_before<=p_now AND (d.lease_deadline IS NULL OR d.lease_deadline<=p_now)
 AND NOT r.revoked AND r.authority_expires_at>p_now
 ORDER BY d.not_before,d.job_id LIMIT p_limit FOR UPDATE OF d SKIP LOCKED
 ), claimed AS (
 UPDATE public.live_activity_delivery d SET lease_id=gen_random_uuid(),lease_deadline=p_now+interval '30 seconds',leased_dirty_version=d.dirty_version,leased_registration_version=d.registration_version,attempts=d.attempts+1
 FROM due WHERE d.job_id=due.job_id RETURNING d.*)
 SELECT jsonb_build_object('job_id',job_id,'lease_id',lease_id,'registration_id',registration_id,'registration_version',registration_version,
 'account_id',account_id,'device_id',device_id,'family_id',family_id,'baby_id',baby_id,'kind',kind,'instance_id',instance_id,'recovery_epoch',recovery_epoch,
 'dirty_version',leased_dirty_version,'lease_deadline',lease_deadline) FROM claimed;
END $$;


--
-- Name: current_account_id(); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.current_account_id() RETURNS uuid
    LANGUAGE sql STABLE
    SET search_path TO 'pg_catalog'
    AS $$ SELECT nullif(current_setting('app.account_id', true), '')::uuid $$;


--
-- Name: current_family_id(); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.current_family_id() RETURNS uuid
    LANGUAGE sql STABLE
    SET search_path TO 'pg_catalog'
    AS $$ SELECT nullif(current_setting('app.family_id', true), '')::uuid $$;


--
-- Name: dirty_live_activity_families(uuid[], bigint[]); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.dirty_live_activity_families(p_families uuid[], p_sequences bigint[]) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE r record; n integer := 0;
BEGIN
  IF p_families IS NULL OR p_sequences IS NULL OR cardinality(p_families) <> cardinality(p_sequences) OR cardinality(p_families) > 1000 THEN
    RAISE EXCEPTION 'invalid live activity dirty batch' USING ERRCODE = '22023';
  END IF;
  FOR r IN SELECT DISTINCT ON (v.f) v.f, v.s FROM unnest(p_families, p_sequences) v(f, s)
           WHERE EXISTS (SELECT 1 FROM public.live_activity_registration_index i WHERE i.family_id = v.f AND NOT i.revoked)
           ORDER BY v.f, v.s DESC LOOP
    PERFORM app.dirty_live_activity_family(r.f, r.s);
    n := n + 1;
  END LOOP;
  RETURN n;
END $$;


--
-- Name: dirty_live_activity_family(uuid, bigint); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.dirty_live_activity_family(p_family uuid, p_sequence bigint) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE v bigint; realm public.server_state; previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
 IF NOT EXISTS(SELECT FROM public.live_activity_registration_index WHERE family_id=p_family AND NOT revoked) THEN RETURN; END IF;
 SELECT * INTO realm FROM public.server_state WHERE singleton;IF NOT FOUND THEN RETURN;END IF;
 INSERT INTO public.live_activity_family_dirty(family_id,instance_id,recovery_epoch,journal_sequence) VALUES(p_family,realm.instance_id,realm.recovery_epoch,p_sequence)
 ON CONFLICT(family_id) DO UPDATE SET dirty_version=public.live_activity_family_dirty.dirty_version+1,
 instance_id=EXCLUDED.instance_id,recovery_epoch=EXCLUDED.recovery_epoch,
 journal_sequence=CASE WHEN public.live_activity_family_dirty.instance_id<>EXCLUDED.instance_id OR public.live_activity_family_dirty.recovery_epoch<>EXCLUDED.recovery_epoch THEN EXCLUDED.journal_sequence ELSE greatest(public.live_activity_family_dirty.journal_sequence,EXCLUDED.journal_sequence) END
 RETURNING dirty_version INTO v;
 PERFORM set_config('app.family_id',p_family::text,true);
 INSERT INTO public.live_activity_delivery(registration_id,registration_version,account_id,device_id,family_id,baby_id,kind,instance_id,recovery_epoch,dirty_version)
 SELECT r.registration_id,r.registration_version,r.account_id,r.device_id,r.family_id,b.id,k.kind,r.instance_id,r.recovery_epoch,v
 FROM public.live_activity_registration_index r CROSS JOIN (VALUES('feed'),('sleep')) k(kind)
 JOIN public.care_entities b ON b.family_id=r.family_id AND b.kind=1 AND b.tombstoned_at IS NULL
 WHERE r.family_id=p_family AND NOT r.revoked AND r.instance_id=realm.instance_id AND r.recovery_epoch=realm.recovery_epoch
 ON CONFLICT(registration_id,baby_id,kind) DO UPDATE SET
 dirty_version=EXCLUDED.dirty_version,registration_version=EXCLUDED.registration_version,not_before=least(public.live_activity_delivery.not_before,now());
 PERFORM set_config('app.family_id',previous_family,true);
END $$;


--
-- Name: ensure_audit_partitions(timestamp with time zone); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.ensure_audit_partitions(p_through timestamp with time zone, OUT created integer, OUT skipped text[]) RETURNS record
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    SET "TimeZone" TO 'UTC'
    SET lock_timeout TO '1s'
    AS $$
DECLARE
  month_start timestamptz := greatest(timestamptz '2027-01-01 00:00:00+00', date_trunc('month', now()));
  month_end timestamptz;
  part text;
BEGIN
  created := 0;
  skipped := '{}';
  IF p_through IS NULL OR NOT isfinite(p_through) OR p_through > now() + interval '400 days' THEN
    RAISE EXCEPTION 'invalid audit partition horizon' USING ERRCODE = '22023';
  END IF;
  PERFORM pg_advisory_xact_lock(3100031); -- app.ensure_audit_partitions
  WHILE month_start <= p_through LOOP
    month_end := month_start + interval '1 month';
    part := 'security_audit_events_' || to_char(month_start, 'YYYY_MM');
    IF to_regclass('public.' || part) IS NULL THEN
      BEGIN
        EXECUTE format('CREATE TABLE public.%I (LIKE public.security_audit_events INCLUDING DEFAULTS INCLUDING CONSTRAINTS,'
          ' CONSTRAINT %I CHECK (at >= %L AND at < %L)) WITH (autovacuum_vacuum_insert_scale_factor = 0.05)',
          part, part || '_at_range', month_start, month_end);
        EXECUTE format('ALTER TABLE public.security_audit_events ATTACH PARTITION public.%I FOR VALUES FROM (%L) TO (%L)',
          part, month_start, month_end);
        created := created + 1;
      EXCEPTION WHEN check_violation THEN
        RAISE WARNING 'audit partition % not created: the default partition holds rows of its month (%)', part, SQLERRM;
        skipped := skipped || part;
      END;
    END IF;
    month_start := month_end;
  END LOOP;
END $$;


--
-- Name: family_actor_labels_high_water(); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.family_actor_labels_high_water() RETURNS bigint
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE
    context_family_id uuid := app.current_family_id();
    result bigint;
BEGIN
    IF context_family_id IS NULL THEN
        RAISE EXCEPTION 'family context is required' USING ERRCODE = '42501';
    END IF;
    SELECT max(h.label_version) INTO result
      FROM public.family_actor_label_history h
     WHERE h.family_id=context_family_id;
    IF result IS NULL THEN
        RAISE EXCEPTION 'family actor labels unavailable' USING ERRCODE = '23514';
    END IF;
    RETURN result;
END;
$$;


--
-- Name: fence_live_activity_installation(uuid, bigint, uuid, uuid, uuid, uuid, bigint, timestamp with time zone, boolean); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.fence_live_activity_installation(p_instance uuid, p_epoch bigint, p_installation uuid, p_account uuid, p_device uuid, p_family uuid, p_version bigint, p_expires timestamp with time zone, p_revoked boolean) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE owned public.live_activity_installations; old_scope record; previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
 IF NOT EXISTS(SELECT FROM public.server_state WHERE singleton AND instance_id=p_instance AND recovery_epoch=p_epoch) THEN RAISE EXCEPTION 'registration realm denied' USING ERRCODE='42501';END IF;
 PERFORM app.retire_stale_live_activity_registrations();
 IF p_account IS DISTINCT FROM app.current_account_id() OR p_family IS DISTINCT FROM app.current_family_id() OR p_version IS NULL OR p_version NOT BETWEEN 1 AND 9007199254740991 THEN RETURN false; END IF;
 INSERT INTO public.live_activity_installations VALUES(p_instance,p_epoch,p_installation,p_account,p_device,p_family,p_version,p_revoked,p_expires) ON CONFLICT DO NOTHING;
 SELECT * INTO owned FROM public.live_activity_installations WHERE instance_id=p_instance AND recovery_epoch=p_epoch AND installation_id=p_installation FOR UPDATE;
 IF owned.account_id<>p_account OR owned.device_id<>p_device THEN RAISE EXCEPTION 'installation ownership denied' USING ERRCODE='42501'; END IF;
 IF owned.version>p_version OR (owned.version=p_version AND (owned.family_id<>p_family OR owned.revoked<>p_revoked)) THEN RETURN false; END IF;
 IF p_expires IS NULL OR NOT isfinite(p_expires) OR NOT app.live_activity_lineage_live(p_account, p_device, p_family, p_epoch, p_expires) THEN
 RAISE EXCEPTION 'installation authority denied' USING ERRCODE='42501'; END IF;
 IF owned.version<p_version THEN
 UPDATE public.live_activity_installations SET version=p_version,family_id=p_family,revoked=p_revoked,authority_expires_at=greatest(authority_expires_at,p_expires)
 WHERE instance_id=p_instance AND recovery_epoch=p_epoch AND installation_id=p_installation;
 END IF;
 FOR old_scope IN SELECT r.family_id FROM public.live_activity_registration_index r
 WHERE r.instance_id=p_instance AND r.recovery_epoch=p_epoch AND r.installation_id=p_installation AND r.family_id<>p_family AND NOT r.revoked LOOP
 PERFORM set_config('app.family_id',old_scope.family_id::text,true);
 UPDATE public.live_activity_devices SET revoked=true,registration_version=p_version WHERE instance_id=p_instance AND recovery_epoch=p_epoch AND installation_id=p_installation AND family_id=old_scope.family_id AND NOT revoked;
 END LOOP;
 PERFORM set_config('app.family_id',previous_family,true);RETURN true;
END $$;


--
-- Name: finish_live_activity_work(jsonb, timestamp with time zone, text); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.finish_live_activity_work(p_claim jsonb, p_retry_at timestamp with time zone, p_reason text) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $_$
DECLARE d public.live_activity_delivery; affected integer;
BEGIN
 IF p_retry_at IS NOT NULL AND (NOT isfinite(p_retry_at) OR p_retry_at<clock_timestamp() OR p_reason IS NULL OR p_reason !~ '^[a-z][a-z0-9_]{0,63}$') THEN RAISE EXCEPTION 'invalid retry' USING ERRCODE='22023'; END IF;
 SELECT * INTO d FROM public.live_activity_delivery WHERE job_id=(p_claim->>'job_id')::uuid FOR UPDATE;
 IF NOT FOUND OR d.lease_deadline<=clock_timestamp() OR d.lease_id IS NULL OR d.dirty_version<>d.leased_dirty_version OR
 (d.lease_id,d.registration_id,d.registration_version,d.account_id,d.device_id,d.family_id,d.baby_id,d.kind,d.instance_id,d.recovery_epoch,d.leased_dirty_version,d.lease_deadline)
 IS DISTINCT FROM ((p_claim->>'lease_id')::uuid,(p_claim->>'registration_id')::uuid,(p_claim->>'registration_version')::bigint,
 (p_claim->>'account_id')::uuid,(p_claim->>'device_id')::uuid,(p_claim->>'family_id')::uuid,(p_claim->>'baby_id')::uuid,p_claim->>'kind',
 (p_claim->>'instance_id')::uuid,(p_claim->>'recovery_epoch')::bigint,(p_claim->>'dirty_version')::bigint,(p_claim->>'lease_deadline')::timestamptz)
 THEN RETURN false; END IF;
 UPDATE public.live_activity_delivery SET completed_version=CASE WHEN p_retry_at IS NULL THEN d.dirty_version ELSE completed_version END,
 not_before=coalesce(p_retry_at,not_before),last_reason=p_reason,lease_id=NULL,lease_deadline=NULL,leased_dirty_version=NULL,leased_registration_version=NULL WHERE job_id=d.job_id;
 RETURN true;
END $_$;


--
-- Name: forget_push_token(uuid, bytea); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.forget_push_token(p_device_id uuid, p_token bytea) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE
  previous text := current_setting('app.authority', true);
  deleted integer;
BEGIN
  IF p_device_id IS NULL OR p_token IS NULL THEN
    RAISE EXCEPTION 'invalid push token' USING ERRCODE = '22023';
  END IF;
  PERFORM set_config('app.authority', '1', true);
  DELETE FROM public.device_push_tokens WHERE device_id = p_device_id AND token = p_token; -- device_push_tokens_pkey
  GET DIAGNOSTICS deleted = ROW_COUNT;
  PERFORM set_config('app.authority', coalesce(previous, ''), true);
  RETURN deleted = 1;
END $$;


--
-- Name: import_unfinished_session_count(uuid, timestamp with time zone); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.import_unfinished_session_count(p_account_id uuid, p_now timestamp with time zone) RETURNS bigint
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE
    unfinished bigint;
BEGIN
    IF p_account_id IS NULL
       OR p_account_id = '00000000-0000-0000-0000-000000000000'::uuid
       OR p_account_id IS DISTINCT FROM app.current_account_id()
       OR app.current_family_id() IS NULL
       OR p_now IS NULL OR NOT isfinite(p_now) THEN
        RAISE EXCEPTION 'import unfinished count denied' USING ERRCODE = '42501';
    END IF;
    SELECT count(*) INTO unfinished
      FROM public.import_sessions
     WHERE initiating_account_id = p_account_id
       AND NOT mapping_complete
       AND expired_at IS NULL
       AND expires_at > p_now;
    RETURN unfinished;
END
$$;


--
-- Name: integration_authority(uuid, bigint); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.integration_authority(p_grant_id uuid, p_recovery_epoch bigint) RETURNS TABLE(grant_found boolean, account_id uuid, family_id uuid, recovery_epoch bigint, scope text, verifier bytea, status text, expires_at timestamp with time zone, account_status text, account_chain_failed boolean, family_status text, family_owner_id uuid, family_audit_seq bigint, family_chain_failed boolean, family_entitlement_required boolean, member_role text, member_status text, slot_family_id uuid, slot_state text, entitlement_status text, entitlement_expires_at timestamp with time zone)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
#variable_conflict use_column
DECLARE
  previous_authority text := coalesce(current_setting('app.authority',true),'');
BEGIN
  IF p_grant_id IS NULL OR p_recovery_epoch IS NULL THEN
    RAISE EXCEPTION 'invalid integration authority lookup' USING ERRCODE='22023';
  END IF;
  PERFORM set_config('app.authority','1',true);
  SELECT true, i.account_id, i.family_id, i.recovery_epoch, i.scope, i.verifier, i.status::text, i.expires_at, a.status::text, a.chain_failed,
         f.status::text, f.owner_account_id, f.audit_seq, f.chain_failed, f.entitlement_required, m.role::text, m.status::text,
         fs.family_id, fs.state::text, e.status::text, e.expires_at
    INTO grant_found, account_id, family_id, recovery_epoch, scope, verifier, status, expires_at, account_status, account_chain_failed,
         family_status, family_owner_id, family_audit_seq, family_chain_failed, family_entitlement_required, member_role, member_status,
         slot_family_id, slot_state, entitlement_status, entitlement_expires_at
    FROM public.mcp_integrations i
    LEFT JOIN public.accounts a ON a.id = i.account_id
    LEFT JOIN public.families f ON f.id = i.family_id
    LEFT JOIN public.memberships m ON m.family_id = i.family_id AND m.account_id = i.account_id
    LEFT JOIN public.family_slots fs ON fs.account_id = i.account_id
    LEFT JOIN public.family_entitlements e ON e.family_id = i.family_id AND e.entitlement_key = 'family_sync'
   WHERE i.id = p_grant_id;
  grant_found := coalesce(grant_found,false);
  PERFORM set_config('app.authority',previous_authority,true);
  IF grant_found AND status = 'active' AND recovery_epoch = p_recovery_epoch AND account_status = 'active' AND family_status = 'active'
     AND family_owner_id = account_id AND member_role = 'owner' AND member_status = 'active' AND slot_family_id = family_id
     AND slot_state = 'active' THEN
    PERFORM set_config('app.account_id',account_id::text,true);
    PERFORM set_config('app.family_id',family_id::text,true);
  ELSE
    PERFORM set_config('app.account_id','',true);
    PERFORM set_config('app.family_id','',true);
  END IF;
  RETURN NEXT;
END $$;


--
-- Name: list_family_actor_labels(uuid, bigint, uuid, integer); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.list_family_actor_labels(p_family_id uuid, p_high_water bigint, p_after_account_id uuid, p_limit integer) RETURNS TABLE(account_id uuid, display_name text, former boolean, high_water bigint, has_more boolean)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE
    context_family_id uuid := app.current_family_id();
BEGIN
    IF context_family_id IS NULL OR p_family_id IS NULL OR p_family_id IS DISTINCT FROM context_family_id THEN
        RAISE EXCEPTION 'family context mismatch' USING ERRCODE = '42501';
    END IF;
    IF p_high_water IS NULL OR p_high_water <= 0 OR p_high_water > 9007199254740991
       OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 100
       OR p_after_account_id = '00000000-0000-0000-0000-000000000000' THEN
        RAISE EXCEPTION 'invalid family actor label page' USING ERRCODE = '22023';
    END IF;
    IF NOT EXISTS (SELECT FROM public.family_actor_label_history h
        WHERE h.family_id=context_family_id AND h.label_version=p_high_water) THEN
        RAISE EXCEPTION 'invalid family actor label high water' USING ERRCODE = '22023';
    END IF;
    RETURN QUERY
    WITH latest AS MATERIALIZED (
        SELECT DISTINCT ON (h.account_id)
            h.account_id,h.display_name,h.former
          FROM public.family_actor_label_history h
         WHERE h.family_id=context_family_id AND h.label_version<=p_high_water
         ORDER BY h.account_id,h.label_version DESC
    ), candidates AS MATERIALIZED (
        SELECT l.account_id,l.display_name,l.former
          FROM latest l
         WHERE p_after_account_id IS NULL OR l.account_id>p_after_account_id
         ORDER BY l.account_id
         LIMIT p_limit+1
    )
    SELECT c.account_id,c.display_name,c.former,p_high_water,
           (SELECT count(*)>p_limit FROM candidates)
      FROM candidates c
     ORDER BY c.account_id
     LIMIT p_limit;
END;
$$;


--
-- Name: live_activity_authorized(uuid, bigint, boolean); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.live_activity_authorized(p_registration uuid, p_version bigint, p_require_entitlement boolean) RETURNS app.member_role
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE previous text := current_setting('app.authority', true); role app.member_role; scope record;
  previous_account text := coalesce(current_setting('app.account_id', true), '');
  previous_family text := coalesce(current_setting('app.family_id', true), '');
BEGIN
  SELECT i.account_id, i.family_id INTO scope FROM public.live_activity_registration_index i WHERE i.registration_id = p_registration;
  IF NOT FOUND THEN RETURN NULL; END IF;
  -- A caller in a tenant context (the request seam) learns only about its own
  -- tenant's registrations; context-free callers (the sweep) see every one.
  IF (app.current_account_id() IS NOT NULL OR app.current_family_id() IS NOT NULL)
     AND (scope.account_id IS DISTINCT FROM app.current_account_id() OR scope.family_id IS DISTINCT FROM app.current_family_id()) THEN
    RETURN NULL;
  END IF;
  PERFORM set_config('app.account_id', scope.account_id::text, true);
  PERFORM set_config('app.family_id', scope.family_id::text, true);
  PERFORM set_config('app.authority', '1', true);
  SELECT m.role INTO role
  FROM public.live_activity_devices r
  JOIN public.server_state st ON st.singleton AND st.instance_id = r.instance_id AND st.recovery_epoch = r.recovery_epoch
  JOIN public.accounts a ON a.id = r.account_id AND a.status = 'active'
  JOIN public.devices d ON d.id = r.device_id AND d.account_id = r.account_id AND d.status = 'active'
  JOIN public.family_slots sl ON sl.account_id = r.account_id AND sl.family_id = r.family_id AND sl.state = 'active'
  JOIN public.memberships m ON m.family_id = r.family_id AND m.account_id = r.account_id AND m.status = 'active'
  JOIN public.families f ON f.id = r.family_id AND f.status = 'active' AND NOT f.chain_failed
  WHERE r.registration_id = p_registration AND r.registration_version = p_version AND NOT r.revoked
    AND r.authority_expires_at > clock_timestamp()
    AND (NOT (p_require_entitlement OR f.entitlement_required) OR EXISTS (SELECT 1 FROM public.family_entitlements e
         WHERE e.family_id = f.id AND e.entitlement_key = 'family_sync' AND e.status = 'active' AND (e.expires_at IS NULL OR e.expires_at > clock_timestamp())))
    AND EXISTS (SELECT 1 FROM public.sessions s WHERE s.account_id = r.account_id AND s.device_id = r.device_id
         AND s.family_id = r.family_id AND s.scope = 'family' AND s.status = 'active' AND s.lineage_root_id = r.lineage_root_session_id
         AND s.deadline_at = r.authority_expires_at AND s.refresh_expires_at > clock_timestamp());
  PERFORM set_config('app.authority', coalesce(previous, ''), true);
  PERFORM set_config('app.account_id', previous_account, true);
  PERFORM set_config('app.family_id', previous_family, true);
  RETURN role;
END $$;


--
-- Name: live_activity_claim_live(jsonb); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.live_activity_claim_live(p_claim jsonb) RETURNS boolean
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
 SELECT EXISTS(SELECT FROM public.live_activity_delivery d
 JOIN public.live_activity_registration_index r USING(registration_id)
 JOIN public.server_state s ON s.singleton
 WHERE d.job_id=(p_claim->>'job_id')::uuid AND d.account_id=app.current_account_id() AND d.family_id=app.current_family_id()
 AND NOT r.revoked AND r.registration_version=d.registration_version
 AND s.instance_id=d.instance_id AND s.recovery_epoch=d.recovery_epoch
 AND d.lease_deadline>clock_timestamp() AND d.dirty_version=d.leased_dirty_version
 AND (d.lease_id,d.registration_id,d.registration_version,d.account_id,d.device_id,d.family_id,d.baby_id,d.kind,d.instance_id,d.recovery_epoch,d.leased_dirty_version,d.lease_deadline)
 = ((p_claim->>'lease_id')::uuid,(p_claim->>'registration_id')::uuid,(p_claim->>'registration_version')::bigint,
 (p_claim->>'account_id')::uuid,(p_claim->>'device_id')::uuid,(p_claim->>'family_id')::uuid,(p_claim->>'baby_id')::uuid,p_claim->>'kind',
 (p_claim->>'instance_id')::uuid,(p_claim->>'recovery_epoch')::bigint,(p_claim->>'dirty_version')::bigint,(p_claim->>'lease_deadline')::timestamptz))
$$;


--
-- Name: live_activity_lineage_live(uuid, uuid, uuid, bigint, timestamp with time zone); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.live_activity_lineage_live(p_account uuid, p_device uuid, p_family uuid, p_epoch bigint, p_expires timestamp with time zone) RETURNS boolean
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE previous text := current_setting('app.authority', true); live boolean;
BEGIN
  PERFORM set_config('app.authority', '1', true);
  SELECT EXISTS (SELECT 1 FROM public.sessions s JOIN public.devices d ON d.id = s.device_id AND d.account_id = s.account_id
    WHERE s.account_id = p_account AND s.device_id = p_device AND s.recovery_epoch = p_epoch AND s.family_id = p_family
      AND s.scope = 'family' AND s.deadline_at = p_expires AND s.status = 'active' AND d.status = 'active') INTO live;
  PERFORM set_config('app.authority', coalesce(previous, ''), true);
  RETURN live;
END $$;


--
-- Name: live_activity_registration_dirty(); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.live_activity_registration_dirty() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
BEGIN
 -- Journal enqueue owns dirty before delivery. Revocation and coordinate
 -- retirement must take the same lock before deleting deliveries or ending
 -- tokens (whose update trigger schedules the family again).
 PERFORM 1 FROM public.live_activity_family_dirty WHERE family_id=NEW.family_id FOR UPDATE;
 IF TG_TABLE_NAME='live_activity_devices' THEN
 INSERT INTO public.live_activity_registration_index VALUES(NEW.registration_id,NEW.account_id,NEW.device_id,NEW.family_id,NEW.installation_id,NEW.instance_id,NEW.recovery_epoch,NEW.registration_version,NEW.authority_expires_at,NEW.revoked)
 ON CONFLICT(registration_id) DO UPDATE SET family_id=EXCLUDED.family_id,registration_version=EXCLUDED.registration_version,authority_expires_at=EXCLUDED.authority_expires_at,revoked=EXCLUDED.revoked;
 IF TG_OP='UPDATE' AND (OLD.environment IS DISTINCT FROM NEW.environment OR OLD.topic IS DISTINCT FROM NEW.topic) THEN
 -- Keep token version/generation tombstones; an old identical retry cannot
 -- resurrect a token in a different APNs namespace. A newer token version
 -- explicitly binds it to the device's current environment/topic.
 UPDATE public.live_activity_tokens SET state='ended' WHERE registration_id=NEW.registration_id AND state='active'
 AND (environment IS DISTINCT FROM NEW.environment OR topic IS DISTINCT FROM NEW.topic);
 END IF;
 IF NEW.revoked THEN
 DELETE FROM public.live_activity_delivery WHERE registration_id=NEW.registration_id;
 UPDATE public.live_activity_tokens SET state='ended' WHERE registration_id=NEW.registration_id AND state='active';
 END IF;
 END IF;
 PERFORM app.dirty_live_activity_family(NEW.family_id,0);RETURN NULL;
END $$;


--
-- Name: live_activity_registration_state(uuid, jsonb); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.live_activity_registration_state(p_registration uuid, p_inventory jsonb) RETURNS jsonb
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE device public.live_activity_devices; proof jsonb; version bigint; result jsonb;
BEGIN
 SELECT * INTO device FROM public.live_activity_devices WHERE registration_id=p_registration
 AND account_id=app.current_account_id() AND family_id=app.current_family_id() AND NOT revoked FOR UPDATE;
 IF NOT FOUND THEN RETURN NULL;END IF;
 IF p_inventory IS NOT NULL THEN
 version:=(p_inventory->>'inventory_version')::bigint;
 IF version<device.inventory_version OR (version=device.inventory_version AND p_inventory IS DISTINCT FROM device.foreground_inventory) THEN
 RAISE EXCEPTION 'live_activity_version_conflict' USING ERRCODE='P0001';END IF;
 IF version>device.inventory_version THEN
 PERFORM 1 FROM public.live_activity_family_dirty WHERE family_id=device.family_id FOR UPDATE;
 UPDATE public.live_activity_devices SET inventory_version=version,foreground_inventory=p_inventory WHERE registration_id=p_registration;
 FOR proof IN SELECT value FROM jsonb_array_elements(p_inventory->'absent_generations') LOOP
 UPDATE public.live_activity_delivery SET target_state=target_state||'{"absence_retry_allowed":true}'::jsonb,not_before=clock_timestamp()
 WHERE registration_id=p_registration AND instance_id=device.instance_id AND recovery_epoch=device.recovery_epoch
 AND target_state->>'generation_id'=proof->>'generation_id' AND target_state->>'start_attempt_id'=proof->>'observed_attempt_id'
 AND target_state->>'status'='startUnconfirmed' AND NOT coalesce((target_state->>'absence_retry_used')::boolean,false);
 END LOOP;
 END IF;
 END IF;
 SELECT coalesce(jsonb_agg(jsonb_build_object('baby_id',baby_id,'kind',kind,'state',target_state) ORDER BY baby_id,kind),'[]'::jsonb) INTO result
 FROM public.live_activity_delivery WHERE registration_id=p_registration;
 RETURN result;
END $$;


--
-- Name: live_activity_session_binding(uuid, uuid, uuid, bigint); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.live_activity_session_binding(p_account uuid, p_device uuid, p_session uuid, p_epoch bigint) RETURNS TABLE(instance_id uuid, lineage_root_session_id uuid, authority_expires_at timestamp with time zone)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE previous text := current_setting('app.authority', true);
BEGIN
  PERFORM set_config('app.authority', '1', true);
  RETURN QUERY
    SELECT st.instance_id, s.lineage_root_id, s.deadline_at
    FROM public.sessions s
    JOIN public.devices d ON d.id = s.device_id AND d.account_id = s.account_id AND d.status = 'active'
    JOIN public.server_state st ON st.singleton AND st.recovery_epoch = s.recovery_epoch
    WHERE p_account = app.current_account_id() AND s.account_id = p_account AND s.device_id = p_device AND s.id = p_session
      AND s.recovery_epoch = p_epoch AND s.status = 'active' AND s.scope = 'family' AND s.family_id = app.current_family_id()
      AND s.deadline_at > clock_timestamp() AND s.refresh_expires_at > clock_timestamp();
  PERFORM set_config('app.authority', coalesce(previous, ''), true);
END $$;


--
-- Name: live_activity_target(jsonb, jsonb, boolean, timestamp with time zone, jsonb, jsonb); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.live_activity_target(p_claim jsonb, p_state jsonb, p_finish boolean, p_deadline timestamp with time zone, p_invalid jsonb, p_outcome jsonb) RETURNS jsonb
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $_$
DECLARE d public.live_activity_delivery; result jsonb; outcome_status integer;
 previous_account text:=coalesce(current_setting('app.account_id',true),'');
 previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
 -- Token invalidation fires family dirty triggers: match journal lock order.
 PERFORM set_config('app.account_id',p_claim->>'account_id',true);
 PERFORM set_config('app.family_id',p_claim->>'family_id',true);
 -- Registration owns device before its dirty trigger. Take that order before
 -- a captured-token invalidation can touch the same device/token rows.
 PERFORM 1 FROM public.live_activity_devices WHERE registration_id=(p_claim->>'registration_id')::uuid FOR UPDATE;
 PERFORM 1 FROM public.live_activity_family_dirty WHERE family_id=(p_claim->>'family_id')::uuid FOR UPDATE;
 SELECT * INTO d FROM public.live_activity_delivery WHERE job_id=(p_claim->>'job_id')::uuid FOR UPDATE;
 IF NOT app.live_activity_claim_live(p_claim) THEN result:=NULL;
 ELSE
 result:=d.target_state;
 IF p_state IS NOT NULL THEN
 IF jsonb_typeof(p_state)<>'object' OR octet_length(p_state::text)>16384 THEN RAISE EXCEPTION 'invalid lifecycle state' USING ERRCODE='22023';END IF;
 IF p_deadline IS NOT NULL AND NOT isfinite(p_deadline) THEN RAISE EXCEPTION 'invalid lifecycle deadline' USING ERRCODE='22023';END IF;
 UPDATE public.live_activity_delivery SET target_state=p_state,
 completed_version=CASE WHEN p_finish AND p_deadline IS NULL THEN dirty_version ELSE completed_version END,
 not_before=CASE WHEN p_finish THEN coalesce(p_deadline,not_before) ELSE not_before END,
 lease_id=CASE WHEN p_finish THEN NULL ELSE lease_id END,
 lease_deadline=CASE WHEN p_finish THEN NULL ELSE lease_deadline END,
 leased_dirty_version=CASE WHEN p_finish THEN NULL ELSE leased_dirty_version END,
 leased_registration_version=CASE WHEN p_finish THEN NULL ELSE leased_registration_version END
 WHERE job_id=d.job_id;
 IF NOT p_finish AND p_state->>'pending_event' IS NOT NULL THEN
 INSERT INTO public.live_activity_history(job_id,event) VALUES(d.job_id,p_state->>'pending_event');
 END IF;
 -- APNs' answer belongs to this lease's own attempt: the job's latest intent.
 IF p_outcome IS NOT NULL THEN
 IF jsonb_typeof(p_outcome)<>'object' OR coalesce(p_outcome->>'result','') NOT IN ('accepted','invalidToken','retryable','configurationFailure','ambiguous')
 OR coalesce(p_outcome->>'reason','') !~ '^([A-Za-z]{1,64})?$' THEN RAISE EXCEPTION 'invalid push outcome' USING ERRCODE='22023';END IF;
 outcome_status:=(p_outcome->>'http_status')::integer;
 IF outcome_status NOT BETWEEN 100 AND 599 THEN RAISE EXCEPTION 'invalid push outcome' USING ERRCODE='22023';END IF;
 UPDATE public.live_activity_history h SET result=p_outcome->>'result',http_status=outcome_status,reason=p_outcome->>'reason'
 WHERE h.id=(SELECT a.id FROM public.live_activity_history a WHERE a.job_id=d.job_id ORDER BY a.recorded_at DESC,a.id DESC LIMIT 1) AND h.result IS NULL;
 UPDATE public.live_activity_delivery SET last_reason=CASE WHEN p_outcome->>'result'='accepted' THEN NULL ELSE p_outcome->>'result' END WHERE job_id=d.job_id;
 END IF;
 IF p_invalid IS NOT NULL THEN
 IF p_invalid->>'activity_id'='' THEN
 UPDATE public.live_activity_devices SET push_token=NULL WHERE registration_id=d.registration_id AND registration_version=(p_invalid->>'version')::bigint;
 ELSE
 UPDATE public.live_activity_tokens SET token=NULL,state='ended',server_retired=true WHERE registration_id=d.registration_id AND activity_id=p_invalid->>'activity_id' AND token_version=(p_invalid->>'version')::bigint;
 END IF;
 END IF;
 result:=p_state;
 END IF;
 END IF;
 PERFORM set_config('app.account_id',previous_account,true);PERFORM set_config('app.family_id',previous_family,true);
 RETURN result;
END $_$;


--
-- Name: live_activity_token_fingerprint(); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.live_activity_token_fingerprint() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog'
    AS $$
BEGIN
 IF TG_TABLE_NAME='live_activity_devices' THEN
 IF NEW.revoked THEN NEW.push_token:=NULL;END IF;RETURN NEW;END IF;
 IF NEW.token IS NOT NULL THEN NEW.token_fingerprint:=sha256(convert_to(NEW.token,'UTF8'));
 ELSIF TG_OP='UPDATE' THEN NEW.token_fingerprint:=OLD.token_fingerprint;END IF;
 IF NEW.state<>'active' THEN NEW.token:=NULL;END IF;
 RETURN NEW;
END $$;


--
-- Name: live_activity_token_ownership(); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.live_activity_token_ownership() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE token_slot text; live boolean; value text;
BEGIN
 IF TG_TABLE_NAME='live_activity_devices' THEN token_slot:='device';live:=NOT NEW.revoked AND NEW.push_token IS NOT NULL;value:=NEW.push_token;
 ELSE token_slot:='activity:'||NEW.activity_id;live:=NEW.state='active' AND NEW.token IS NOT NULL;value:=NEW.token;END IF;
 DELETE FROM public.live_activity_token_owners WHERE registration_id=NEW.registration_id AND slot=token_slot;
 IF live THEN
 INSERT INTO public.live_activity_token_owners VALUES(NEW.environment,NEW.topic,value,NEW.registration_id,token_slot);
 END IF;RETURN NULL;
END $$;


--
-- Name: membership_may_write(uuid, uuid); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.membership_may_write(target_family uuid, target_account uuid) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT EXISTS (
        SELECT 1 FROM public.memberships
         WHERE family_id = target_family
           AND account_id = target_account
           AND status = 'active'
           AND role IN ('owner', 'caregiver')
    );
$$;


--
-- Name: prune_push_tokens(integer); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.prune_push_tokens(p_limit integer) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE
  previous text := current_setting('app.authority', true);
  deleted integer;
BEGIN
  IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 10000 THEN
    RAISE EXCEPTION 'invalid push token prune limit' USING ERRCODE = '22023';
  END IF;
  PERFORM set_config('app.authority', '1', true);
  DELETE FROM public.device_push_tokens WHERE device_id IN (
    SELECT t.device_id FROM public.device_push_tokens t                             -- device_push_tokens_pkey order
    WHERE NOT EXISTS (SELECT 1 FROM public.sessions s                               -- sessions_device_live_idx
                      WHERE s.device_id = t.device_id AND s.account_id = t.account_id AND s.status = 'active'
                        AND s.scope = 'family' AND s.refresh_expires_at > now())
    ORDER BY t.device_id LIMIT p_limit FOR UPDATE SKIP LOCKED);
  GET DIAGNOSTICS deleted = ROW_COUNT;
  PERFORM set_config('app.authority', coalesce(previous, ''), true);
  RETURN deleted;
END $$;


--
-- Name: purge_family_step(uuid, integer); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.purge_family_step(p_family_id uuid, p_limit integer, OUT step text, OUT deleted integer) RETURNS record
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE
  previous_family text := current_setting('app.family_id', true);
  previous_account text := current_setting('app.account_id', true);
  previous_authority text := current_setting('app.authority', true);
  v_status app.family_status;
  v_import uuid;
BEGIN
  IF p_family_id IS NULL OR p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 10000 THEN
    RAISE EXCEPTION 'invalid purge step' USING ERRCODE = '22023';
  END IF;
  PERFORM set_config('app.authority', '', true);
  PERFORM set_config('app.account_id', '', true);
  PERFORM set_config('app.family_id', p_family_id::text, true);
  SELECT f.status INTO v_status FROM public.families f WHERE f.id = p_family_id FOR NO KEY UPDATE; -- families_pkey
  IF v_status IS DISTINCT FROM 'purge_started' THEN
    RAISE EXCEPTION 'family is not being purged' USING ERRCODE = '55000';
  END IF;
  step := 'none';
  deleted := 0;
  <<one_step>>
  BEGIN
    SELECT s.import_id INTO v_import FROM public.import_sessions s WHERE s.family_id = p_family_id ORDER BY s.import_id LIMIT 1;
    IF v_import IS NOT NULL THEN
      DELETE FROM public.import_sessions s WHERE s.family_id = p_family_id AND s.import_id = v_import;
      GET DIAGNOSTICS deleted = ROW_COUNT;
      step := 'import_sessions';
      EXIT one_step;
    END IF;
    DELETE FROM public.care_entities e WHERE e.family_id = p_family_id
      AND e.id = ANY(ARRAY(SELECT c.id FROM public.care_entities c WHERE c.family_id = p_family_id AND c.kind > 1 LIMIT p_limit));
    GET DIAGNOSTICS deleted = ROW_COUNT;
    IF deleted > 0 THEN
      step := 'care_records';
      EXIT one_step;
    END IF;
    DELETE FROM public.care_entities e WHERE e.family_id = p_family_id
      AND e.id = ANY(ARRAY(SELECT c.id FROM public.care_entities c WHERE c.family_id = p_family_id AND c.kind = 1 LIMIT p_limit));
    GET DIAGNOSTICS deleted = ROW_COUNT;
    IF deleted > 0 THEN
      step := 'babies';
      EXIT one_step;
    END IF;
    DELETE FROM public.family_actor_label_history h WHERE h.family_id = p_family_id
      AND h.label_version = ANY(ARRAY(SELECT x.label_version FROM public.family_actor_label_history x WHERE x.family_id = p_family_id LIMIT p_limit));
    GET DIAGNOSTICS deleted = ROW_COUNT;
    IF deleted > 0 THEN
      step := 'label_history';
      EXIT one_step;
    END IF;
    DELETE FROM public.family_actor_labels l WHERE l.family_id = p_family_id
      AND l.account_id = ANY(ARRAY(SELECT x.account_id FROM public.family_actor_labels x WHERE x.family_id = p_family_id LIMIT p_limit));
    GET DIAGNOSTICS deleted = ROW_COUNT;
    IF deleted > 0 THEN
      step := 'labels';
      EXIT one_step;
    END IF;
    DELETE FROM public.memberships m WHERE m.family_id = p_family_id
      AND m.account_id = ANY(ARRAY(SELECT x.account_id FROM public.memberships x WHERE x.family_id = p_family_id LIMIT p_limit));
    GET DIAGNOSTICS deleted = ROW_COUNT;
    IF deleted > 0 THEN
      step := 'memberships';
      EXIT one_step;
    END IF;
    PERFORM set_config('app.authority', '1', true);
    DELETE FROM public.family_recovery_claims c WHERE c.family_id = p_family_id;
    GET DIAGNOSTICS deleted = ROW_COUNT;
    PERFORM set_config('app.authority', '', true);
    IF deleted > 0 THEN
      step := 'recovery_claim';
    END IF;
  END one_step;
  PERFORM set_config('app.family_id', coalesce(previous_family, ''), true);
  PERFORM set_config('app.account_id', coalesce(previous_account, ''), true);
  PERFORM set_config('app.authority', coalesce(previous_authority, ''), true);
END $$;


--
-- Name: push_targets(uuid[], uuid[]); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.push_targets(p_family_ids uuid[], p_device_ids uuid[]) RETURNS TABLE(family_id uuid, device_id uuid, environment app.apns_environment, token bytea)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE previous text := current_setting('app.authority', true);
BEGIN
  IF p_family_ids IS NULL OR p_device_ids IS NULL OR cardinality(p_family_ids) <> cardinality(p_device_ids) OR cardinality(p_family_ids) > 1000 THEN
    RAISE EXCEPTION 'invalid push target request' USING ERRCODE = '22023';
  END IF;
  PERFORM set_config('app.authority', '1', true);
  RETURN QUERY
    SELECT v.f, t.device_id, t.environment, t.token
    FROM unnest(p_family_ids, p_device_ids) AS v(f, d)
    JOIN public.device_push_tokens t ON t.device_id = v.d                            -- device_push_tokens_pkey
    JOIN public.devices dv ON dv.id = t.device_id AND dv.account_id = t.account_id AND dv.status = 'active' -- devices_pkey
    WHERE EXISTS (SELECT 1 FROM public.sessions s                                   -- sessions_device_live_idx
                  WHERE s.device_id = t.device_id AND s.account_id = t.account_id AND s.status = 'active'
                    AND s.scope = 'family' AND s.family_id = v.f AND s.refresh_expires_at > now());
  PERFORM set_config('app.authority', coalesce(previous, ''), true);
END $$;


--
-- Name: reject_audit_chain_regression(); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.reject_audit_chain_regression() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog'
    AS $$ BEGIN
  IF ((OLD.chain_failed AND NOT NEW.chain_failed) OR NEW.audit_seq < OLD.audit_seq
      OR (NEW.audit_hash IS DISTINCT FROM OLD.audit_hash AND NEW.audit_seq <= OLD.audit_seq))
     AND NOT pg_has_role(current_user, (SELECT c.relowner FROM pg_class c WHERE c.oid = TG_RELID), 'MEMBER') THEN
    RAISE EXCEPTION 'audit chain heads only move forward outside the table owner' USING ERRCODE='42501';
  END IF;
  RETURN NEW;
END $$;


--
-- Name: reject_tenant_authority_write(); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.reject_tenant_authority_write() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog'
    AS $$ BEGIN
  RAISE EXCEPTION 'authority columns require the authority context' USING ERRCODE='42501';
END $$;


--
-- Name: release_stale_live_activity_lease(jsonb); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.release_stale_live_activity_lease(p_claim jsonb) RETURNS boolean
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE d public.live_activity_delivery;
BEGIN
 SELECT * INTO d FROM public.live_activity_delivery WHERE job_id=(p_claim->>'job_id')::uuid FOR UPDATE;
 IF NOT FOUND OR d.lease_id IS NULL OR d.dirty_version<=d.leased_dirty_version OR
 (d.lease_id,d.registration_id,d.leased_registration_version,d.account_id,d.device_id,d.family_id,d.baby_id,d.kind,d.instance_id,d.recovery_epoch,d.leased_dirty_version,d.lease_deadline)
 IS DISTINCT FROM ((p_claim->>'lease_id')::uuid,(p_claim->>'registration_id')::uuid,(p_claim->>'registration_version')::bigint,
 (p_claim->>'account_id')::uuid,(p_claim->>'device_id')::uuid,(p_claim->>'family_id')::uuid,(p_claim->>'baby_id')::uuid,p_claim->>'kind',
 (p_claim->>'instance_id')::uuid,(p_claim->>'recovery_epoch')::bigint,(p_claim->>'dirty_version')::bigint,(p_claim->>'lease_deadline')::timestamptz)
 THEN RETURN false;END IF;
 UPDATE public.live_activity_delivery SET lease_id=NULL,lease_deadline=NULL,leased_dirty_version=NULL,leased_registration_version=NULL WHERE job_id=d.job_id;
 RETURN true;
END $$;


--
-- Name: request_authority(bytea[], uuid, boolean, bigint); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.request_authority(p_access_digests bytea[], p_session_id uuid, p_family boolean, p_recovery_epoch bigint) RETURNS TABLE(matches integer, access_digest bytea, token_key_id text, session_id uuid, account_id uuid, device_id uuid, scope text, session_family_id uuid, session_role text, session_status text, recovery_epoch bigint, issued_seq bigint, access_expires_at timestamp with time zone, deadline_at timestamp with time zone, lineage_started_at timestamp with time zone, account_status text, account_chain_failed boolean, slot_family_id uuid, slot_state text, member_role text, member_status text, family_id uuid, family_status text, family_audit_seq bigint, family_chain_failed boolean, family_entitlement_required boolean, entitlement_status text, entitlement_expires_at timestamp with time zone)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
#variable_conflict use_column
DECLARE
  previous_authority text := coalesce(current_setting('app.authority',true),'');
  v_digest bytea;
BEGIN
  IF p_family IS NULL OR p_recovery_epoch IS NULL OR (p_access_digests IS NULL) = (p_session_id IS NULL)
     OR (p_access_digests IS NOT NULL AND cardinality(p_access_digests) NOT BETWEEN 1 AND 8) THEN
    RAISE EXCEPTION 'invalid request authority lookup' USING ERRCODE='22023';
  END IF;
  PERFORM set_config('app.authority','1',true);
  IF p_session_id IS NULL THEN
    SELECT count(*) OVER (), s.access_digest INTO matches, v_digest
      FROM public.sessions s WHERE s.access_digest = ANY(p_access_digests) LIMIT 1;
  ELSE
    SELECT 1, s.access_digest INTO matches, v_digest FROM public.sessions s WHERE s.id = p_session_id;
  END IF;
  matches := least(coalesce(matches,0),2);
  IF matches > 0 THEN
    SELECT s.access_digest, s.token_key_id, s.id, s.account_id, s.device_id, s.scope::text, s.family_id, s.role::text, s.status::text,
           s.recovery_epoch, s.issued_seq, s.access_expires_at, s.deadline_at, s.lineage_started_at, a.status::text, a.chain_failed,
           fs.family_id, fs.state::text, m.role::text, m.status::text, f.id, f.status::text, f.audit_seq, f.chain_failed,
           f.entitlement_required, e.status::text, e.expires_at
      INTO access_digest, token_key_id, session_id, account_id, device_id, scope, session_family_id, session_role, session_status,
           recovery_epoch, issued_seq, access_expires_at, deadline_at, lineage_started_at, account_status, account_chain_failed,
           slot_family_id, slot_state, member_role, member_status, family_id, family_status, family_audit_seq, family_chain_failed,
           family_entitlement_required, entitlement_status, entitlement_expires_at
      FROM public.sessions s
      JOIN public.accounts a ON a.id = s.account_id
      LEFT JOIN public.family_slots fs ON fs.account_id = s.account_id
      LEFT JOIN public.memberships m ON m.family_id = fs.family_id AND m.account_id = s.account_id
      LEFT JOIN public.families f ON p_family AND f.id = coalesce(s.family_id, fs.family_id)
      LEFT JOIN public.family_entitlements e ON e.family_id = f.id AND e.entitlement_key = 'family_sync'
     WHERE s.access_digest = v_digest;
  END IF;
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


--
-- Name: retain_live_activity_work(); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.retain_live_activity_work() RETURNS bigint
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE r record; removed bigint:=0; previous_account text:=coalesce(current_setting('app.account_id',true),'');previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
 FOR r IN SELECT registration_id,account_id,family_id FROM public.live_activity_registration_index WHERE authority_expires_at<=clock_timestamp() LOOP
 PERFORM set_config('app.account_id',r.account_id::text,true);PERFORM set_config('app.family_id',r.family_id::text,true);
 DELETE FROM public.live_activity_devices WHERE registration_id=r.registration_id AND authority_expires_at<=clock_timestamp();
 IF FOUND THEN removed:=removed+1;END IF;
 END LOOP;
 PERFORM set_config('app.account_id',previous_account,true);PERFORM set_config('app.family_id',previous_family,true);
 DELETE FROM public.live_activity_installations WHERE authority_expires_at<=clock_timestamp();
 DELETE FROM public.live_activity_history WHERE recorded_at<clock_timestamp()-interval '7 days';
 RETURN removed;
END $$;


--
-- Name: retire_stale_live_activity_registrations(); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.retire_stale_live_activity_registrations() RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE r record; realm public.server_state; previous_account text:=coalesce(current_setting('app.account_id',true),'');previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
 SELECT * INTO realm FROM public.server_state WHERE singleton;
 IF NOT FOUND THEN RETURN;END IF;
 FOR r IN SELECT * FROM public.live_activity_registration_index
 WHERE NOT revoked AND (instance_id<>realm.instance_id OR recovery_epoch<>realm.recovery_epoch)
 ORDER BY registration_id LOOP
 PERFORM set_config('app.account_id',r.account_id::text,true);PERFORM set_config('app.family_id',r.family_id::text,true);
 UPDATE public.live_activity_devices SET revoked=true WHERE registration_id=r.registration_id AND NOT revoked;
 END LOOP;
 PERFORM set_config('app.account_id',previous_account,true);PERFORM set_config('app.family_id',previous_family,true);
END $$;


--
-- Name: sweep_live_activity_registrations(boolean, integer); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.sweep_live_activity_registrations(p_require_entitlement boolean, p_limit integer) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE ids uuid[]; versions bigint[]; accounts uuid[]; families uuid[]; k integer; n integer := 0;
  previous_account text := coalesce(current_setting('app.account_id', true), '');
  previous_family text := coalesce(current_setting('app.family_id', true), '');
BEGIN
  IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 1000 THEN RAISE EXCEPTION 'invalid sweep limit' USING ERRCODE = '22023'; END IF;
  PERFORM set_config('app.account_id', '', true);
  PERFORM set_config('app.family_id', '', true);
  SELECT array_agg(c.registration_id ORDER BY c.registration_id), array_agg(c.registration_version ORDER BY c.registration_id),
         array_agg(c.account_id ORDER BY c.registration_id), array_agg(c.family_id ORDER BY c.registration_id)
    INTO ids, versions, accounts, families
    FROM (SELECT x.registration_id, x.registration_version, x.account_id, x.family_id FROM public.live_activity_registration_index x
          WHERE NOT x.revoked AND app.live_activity_authorized(x.registration_id, x.registration_version, p_require_entitlement) IS NULL
          ORDER BY x.registration_id LIMIT p_limit) c;
  FOR k IN 1 .. coalesce(cardinality(ids), 0) LOOP
    PERFORM set_config('app.account_id', accounts[k]::text, true);
    PERFORM set_config('app.family_id', families[k]::text, true);
    UPDATE public.live_activity_devices SET revoked = true WHERE registration_id = ids[k] AND registration_version = versions[k] AND NOT revoked;
    n := n + 1;
  END LOOP;
  PERFORM set_config('app.account_id', previous_account, true);
  PERFORM set_config('app.family_id', previous_family, true);
  RETURN n;
END $$;


--
-- Name: sweep_live_activity_work(); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.sweep_live_activity_work() RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE r record; seq bigint; realm public.server_state; previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
 SELECT * INTO realm FROM public.server_state WHERE singleton;IF NOT FOUND THEN RETURN;END IF;
 FOR r IN SELECT DISTINCT family_id FROM public.live_activity_registration_index WHERE NOT revoked AND instance_id=realm.instance_id AND recovery_epoch=realm.recovery_epoch ORDER BY family_id LOOP
 PERFORM set_config('app.family_id',r.family_id::text,true);
 SELECT next_sequence INTO seq FROM public.families WHERE id=r.family_id;
 IF EXISTS(SELECT FROM public.live_activity_family_dirty WHERE family_id=r.family_id AND authority_checked_at<=clock_timestamp()-interval '60 seconds') OR seq>coalesce((SELECT journal_sequence FROM public.live_activity_family_dirty WHERE family_id=r.family_id AND instance_id=realm.instance_id AND recovery_epoch=realm.recovery_epoch),-1) THEN
 PERFORM app.dirty_live_activity_family(r.family_id,seq);
 UPDATE public.live_activity_family_dirty SET authority_checked_at=clock_timestamp() WHERE family_id=r.family_id;
 END IF;
 END LOOP;
 PERFORM set_config('app.family_id',previous_family,true);
END $$;


--
-- Name: update_own_profile(uuid, uuid, text, timestamp with time zone); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.update_own_profile(p_account_id uuid, p_command_id uuid, p_display_name text, p_now timestamp with time zone) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
DECLARE
    affected_rows bigint;
    active_family_id uuid;
    requested_hash bytea;
    prior_hash bytea;
    previous_family text := coalesce(current_setting('app.family_id',true),'');
BEGIN
    IF p_account_id IS NULL OR p_account_id IS DISTINCT FROM app.current_account_id()
       OR p_command_id IS NULL OR p_command_id='00000000-0000-0000-0000-000000000000'
       OR NOT app.valid_display_name(p_display_name) OR NOT isfinite(p_now) THEN
        RAISE EXCEPTION 'invalid own profile update' USING ERRCODE = '22023';
    END IF;
    requested_hash := sha256(convert_to('profile-v1:' || p_display_name,'UTF8'));
    INSERT INTO public.account_profile_commands(account_id,command_id,input_hash,created_at)
    VALUES(p_account_id,p_command_id,requested_hash,p_now)
    ON CONFLICT(account_id,command_id) DO NOTHING;
    GET DIAGNOSTICS affected_rows = ROW_COUNT;
    IF affected_rows=0 THEN
        SELECT item.input_hash INTO prior_hash FROM public.account_profile_commands item
         WHERE item.account_id=p_account_id AND item.command_id=p_command_id;
        IF prior_hash IS NULL THEN
            RAISE EXCEPTION 'profile command receipt unavailable' USING ERRCODE='23514';
        END IF;
        IF prior_hash IS DISTINCT FROM requested_hash THEN
            RAISE EXCEPTION 'profile command identifier reused'
                USING ERRCODE='BT002', CONSTRAINT='account_profile_commands_pkey';
        END IF;
        RETURN;
    END IF;
    UPDATE public.accounts SET display_name=p_display_name,profile_required=false,updated_at=p_now
     WHERE id=p_account_id AND status='active';
    GET DIAGNOSTICS affected_rows = ROW_COUNT;
    IF affected_rows <> 1 THEN
        RAISE EXCEPTION 'account unavailable' USING ERRCODE = '23514';
    END IF;
    SELECT family_id INTO active_family_id FROM public.family_slots
     WHERE account_id=p_account_id AND state='active';
    IF active_family_id IS NULL THEN RETURN; END IF;
    PERFORM set_config('app.family_id',active_family_id::text,true);
    UPDATE public.family_actor_labels SET display_name=p_display_name,updated_at=p_now
     WHERE family_id=active_family_id AND account_id=p_account_id AND former=false;
    GET DIAGNOSTICS affected_rows = ROW_COUNT;
    IF affected_rows=1 THEN
        INSERT INTO public.family_actor_label_history(family_id,account_id,display_name,former,recorded_at)
        VALUES(active_family_id,p_account_id,p_display_name,false,p_now);
    END IF;
    PERFORM set_config('app.family_id',previous_family,true);
END;
$$;


--
-- Name: valid_display_name(text); Type: FUNCTION; Schema: app; Owner: -
--

CREATE FUNCTION app.valid_display_name(p_value text) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path TO 'pg_catalog'
    AS $_$
    SELECT p_value IS NOT NULL
       AND char_length(p_value) BETWEEN 1 AND 80
       AND octet_length(p_value) <= 320
       AND p_value !~ U&'(^[\0009-\000D\0020\0085\00A0\1680\2000-\200A\2028\2029\202F\205F\3000])|([\0009-\000D\0020\0085\00A0\1680\2000-\200A\2028\2029\202F\205F\3000]$)'
       AND p_value !~ U&'[\0001-\001F\007F-\009F]'
$_$;


SET default_table_access_method = heap;

--
-- Name: account_identities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.account_identities (
    provider text NOT NULL,
    identity_key_id text NOT NULL,
    identity_digest bytea NOT NULL,
    account_id uuid NOT NULL,
    lifecycle_generation bigint NOT NULL,
    audience text NOT NULL,
    subject text NOT NULL,
    live boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT account_identities_audience_check CHECK (((octet_length(audience) >= 1) AND (octet_length(audience) <= 255))),
    CONSTRAINT account_identities_identity_digest_check CHECK ((octet_length(identity_digest) = 32)),
    CONSTRAINT account_identities_identity_key_id_check CHECK ((identity_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'::text)),
    CONSTRAINT account_identities_lifecycle_generation_check CHECK (((lifecycle_generation >= 1) AND (lifecycle_generation <= '9007199254740991'::bigint))),
    CONSTRAINT account_identities_provider_check CHECK ((provider = 'apple'::text)),
    CONSTRAINT account_identities_subject_check CHECK (((octet_length(subject) >= 1) AND (octet_length(subject) <= 255)))
);

ALTER TABLE ONLY public.account_identities FORCE ROW LEVEL SECURITY;


--
-- Name: account_profile_commands; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.account_profile_commands (
    account_id uuid NOT NULL,
    command_id uuid NOT NULL,
    input_hash bytea NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT account_profile_commands_command_id_check CHECK ((command_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT account_profile_commands_created_at_check CHECK (isfinite(created_at)),
    CONSTRAINT account_profile_commands_input_hash_check CHECK ((octet_length(input_hash) = 32))
);

ALTER TABLE ONLY public.account_profile_commands FORCE ROW LEVEL SECURITY;


--
-- Name: accounts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.accounts (
    id uuid NOT NULL,
    display_name text NOT NULL,
    status app.account_status DEFAULT 'active'::app.account_status NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    profile_required boolean DEFAULT false NOT NULL,
    lifecycle_generation bigint DEFAULT 1 NOT NULL,
    audit_seq bigint DEFAULT 0 NOT NULL,
    audit_hash bytea DEFAULT decode(repeat('00'::text, 32), 'hex'::text) NOT NULL,
    chain_failed boolean DEFAULT false NOT NULL,
    CONSTRAINT accounts_audit_hash_check CHECK ((octet_length(audit_hash) = 32)),
    CONSTRAINT accounts_audit_seq_check CHECK ((audit_seq >= 0)),
    CONSTRAINT accounts_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT accounts_lifecycle_generation_check CHECK (((lifecycle_generation >= 1) AND (lifecycle_generation <= '9007199254740991'::bigint)))
)
WITH (fillfactor='70');

ALTER TABLE ONLY public.accounts FORCE ROW LEVEL SECURITY;


--
-- Name: apple_notifications; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.apple_notifications (
    notification_key_id text NOT NULL,
    notification_digest bytea NOT NULL,
    command_id uuid NOT NULL,
    received_at timestamp with time zone NOT NULL,
    CONSTRAINT apple_notifications_notification_digest_check CHECK ((octet_length(notification_digest) = 32))
);

ALTER TABLE ONLY public.apple_notifications FORCE ROW LEVEL SECURITY;


--
-- Name: audit_checkpoints; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.audit_checkpoints (
    scope app.audit_scope NOT NULL,
    scope_id uuid NOT NULL,
    verified_seq bigint NOT NULL,
    verified_hash bytea NOT NULL,
    verified_at timestamp with time zone NOT NULL
);

ALTER TABLE ONLY public.audit_checkpoints FORCE ROW LEVEL SECURITY;


--
-- Name: auth_challenges; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.auth_challenges (
    token_digest bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT auth_challenges_token_digest_check CHECK ((octet_length(token_digest) = 32))
)
WITH (autovacuum_vacuum_scale_factor='0.05');

ALTER TABLE ONLY public.auth_challenges FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
PARTITION BY HASH (family_id);

ALTER TABLE ONLY public.care_entities FORCE ROW LEVEL SECURITY;


--
-- Name: COLUMN care_entities.kind; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.care_entities.kind IS 'care kind: 1 baby, 2 feed, 3 sleep, 4 diaper, 5 timer, 6 supplement, 7 growth_profile, 8 measurement';


--
-- Name: COLUMN care_entities.timer_kind; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.care_entities.timer_kind IS 'timer kind: 1 feed, 2 sleep';


--
-- Name: COLUMN care_entities.feed_kind; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.care_entities.feed_kind IS 'feed kind: 1 breast, 2 bottle, 3 solids';


--
-- Name: care_entities_p00; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p00 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p00 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p01; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p01 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p01 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p02; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p02 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p02 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p03; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p03 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p03 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p04; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p04 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p04 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p05; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p05 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p05 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p06; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p06 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p06 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p07; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p07 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p07 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p08; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p08 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p08 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p09; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p09 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p09 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p10; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p10 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p10 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p11; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p11 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p11 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p12; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p12 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p12 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p13; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p13 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p13 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p14; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p14 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p14 FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p15; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_entities_p15 (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    kind smallint NOT NULL,
    baby_id uuid,
    entity_version bigint NOT NULL,
    changed_seq bigint NOT NULL,
    creator_account_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    operation app.care_operation NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    tombstoned_at timestamp with time zone,
    started_at timestamp with time zone,
    ended_at timestamp with time zone,
    session_id uuid,
    timer_kind smallint,
    completed_record_id uuid,
    sort_order integer,
    label text,
    name_hash bytea,
    breast_seconds integer DEFAULT 0 NOT NULL,
    bottle_volume_ml numeric(13,6),
    bottle_observations smallint DEFAULT 0 NOT NULL,
    bottle_missing smallint DEFAULT 0 NOT NULL,
    shortest_part_seconds integer,
    wire bytea NOT NULL,
    created_device_id uuid,
    created_seq bigint,
    feed_kind smallint,
    CONSTRAINT care_entities_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_entities_actor_account_id_check CHECK ((actor_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_baby_id_check CHECK ((baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_bottle_missing_check CHECK (((bottle_missing >= 0) AND (bottle_missing <= 512))),
    CONSTRAINT care_entities_bottle_observations_check CHECK (((bottle_observations >= 0) AND (bottle_observations <= 512))),
    CONSTRAINT care_entities_bottle_volume_ml_check CHECK (((bottle_volume_ml >= (0)::numeric) AND (bottle_volume_ml <= (1024000)::numeric))),
    CONSTRAINT care_entities_breast_seconds_check CHECK (((breast_seconds >= 0) AND (breast_seconds <= 604800))),
    CONSTRAINT care_entities_changed_seq_check CHECK (((changed_seq >= 1) AND (changed_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_check CHECK (((tombstoned_at IS NULL) OR (isfinite(tombstoned_at) AND (tombstoned_at >= accepted_at)))),
    CONSTRAINT care_entities_completed_record_id_check CHECK ((completed_record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_device_id_check CHECK ((created_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_created_seq_check CHECK (((created_seq >= 1) AND (created_seq <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_creator_account_id_check CHECK ((creator_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_ended_at_check CHECK (((ended_at IS NULL) OR isfinite(ended_at))),
    CONSTRAINT care_entities_entity_version_check CHECK (((entity_version >= 1) AND (entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT care_entities_feed_kind_check CHECK (((feed_kind >= 1) AND (feed_kind <= 3))),
    CONSTRAINT care_entities_feed_kind_shape CHECK ((((feed_kind IS NULL) OR (kind = 2)) AND ((kind <> 2) OR (tombstoned_at IS NOT NULL) OR (feed_kind IS NOT NULL)))),
    CONSTRAINT care_entities_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_entities_kind_check CHECK (((kind >= 1) AND (kind <= 8))),
    CONSTRAINT care_entities_kind_shape CHECK ((((kind = 1) = (baby_id IS NULL)) AND ((kind = 1) = (sort_order IS NOT NULL)) AND ((kind = 1) = (name_hash IS NOT NULL)) AND ((kind = 5) = (timer_kind IS NOT NULL)) AND ((kind = 6) = (label IS NOT NULL)) AND ((kind = ANY (ARRAY[2, 3, 4, 5, 6, 8])) = (started_at IS NOT NULL)) AND ((ended_at IS NULL) OR (kind = ANY (ARRAY[2, 3, 4]))) AND ((session_id IS NULL) OR (kind = ANY (ARRAY[2, 3, 5]))) AND ((completed_record_id IS NULL) OR ((kind = 5) AND (tombstoned_at IS NOT NULL))) AND ((kind = 2) OR ((breast_seconds = 0) AND (bottle_volume_ml IS NULL) AND (bottle_observations = 0) AND (bottle_missing = 0))) AND ((shortest_part_seconds IS NULL) OR (kind = ANY (ARRAY[2, 5]))) AND ((breast_seconds = 0) OR (shortest_part_seconds IS NOT NULL)) AND ((shortest_part_seconds IS NULL) OR (timer_kind IS DISTINCT FROM 2)) AND ((bottle_volume_ml IS NULL) = (bottle_observations = 0)) AND ((tombstoned_at IS NULL) OR ((breast_seconds = 0) AND (bottle_observations = 0) AND (bottle_missing = 0) AND (shortest_part_seconds IS NULL))))),
    CONSTRAINT care_entities_label_check CHECK (((char_length(btrim(label)) BETWEEN 1 AND 64) AND (octet_length(label) <= 256))),
    CONSTRAINT care_entities_name_hash_check CHECK ((octet_length(name_hash) = 32)),
    CONSTRAINT care_entities_origin_shape CHECK ((((created_device_id IS NULL) = (created_seq IS NULL)) AND ((created_seq IS NULL) OR (created_seq <= changed_seq)))),
    CONSTRAINT care_entities_seven_days CHECK (((ended_at IS NULL) OR ((started_at IS NOT NULL) AND (ended_at >= started_at) AND (ended_at <= (started_at + '7 days'::interval))))),
    CONSTRAINT care_entities_shortest_part_seconds_check CHECK (((shortest_part_seconds >= 1) AND (shortest_part_seconds <= 604800))),
    CONSTRAINT care_entities_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT care_entities_started_at_check CHECK (((started_at IS NULL) OR (isfinite(started_at) AND (started_at >= '1900-01-01 00:00:00+00'::timestamp with time zone) AND (started_at < '2101-01-01 00:00:00+00'::timestamp with time zone)))),
    CONSTRAINT care_entities_timer_kind_check CHECK (((timer_kind >= 1) AND (timer_kind <= 2))),
    CONSTRAINT care_entities_wire_check CHECK (((octet_length(wire) >= 64) AND (octet_length(wire) <= 1048576)))
)
WITH (fillfactor='90', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_entities_p15 FORCE ROW LEVEL SECURITY;


--
-- Name: care_receipts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.care_receipts (
    device_id uuid NOT NULL,
    mutation_id uuid NOT NULL,
    family_id uuid NOT NULL,
    account_id uuid NOT NULL,
    semantic_hash bytea NOT NULL,
    outcome jsonb NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    CONSTRAINT care_receipts_accepted_at_check CHECK (isfinite(accepted_at)),
    CONSTRAINT care_receipts_account_id_check CHECK ((account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_receipts_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_receipts_mutation_id_check CHECK ((mutation_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT care_receipts_outcome_check CHECK ((jsonb_typeof(outcome) = 'object'::text)),
    CONSTRAINT care_receipts_semantic_hash_check CHECK ((octet_length(semantic_hash) = 32))
)
WITH (autovacuum_vacuum_scale_factor='0.02', autovacuum_vacuum_insert_scale_factor='0.05', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.care_receipts FORCE ROW LEVEL SECURITY;


--
-- Name: command_receipts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.command_receipts (
    actor_account_id uuid NOT NULL,
    command_id uuid NOT NULL,
    command_hash bytea NOT NULL,
    kind app.audit_kind NOT NULL,
    result jsonb NOT NULL,
    family_ids uuid[] DEFAULT '{}'::uuid[] NOT NULL,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT command_receipts_command_hash_check CHECK ((octet_length(command_hash) = 32))
);

ALTER TABLE ONLY public.command_receipts FORCE ROW LEVEL SECURITY;


--
-- Name: device_push_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.device_push_tokens (
    device_id uuid NOT NULL,
    account_id uuid NOT NULL,
    environment app.apns_environment NOT NULL,
    token bytea NOT NULL,
    registered_at timestamp with time zone NOT NULL,
    CONSTRAINT device_push_tokens_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT device_push_tokens_registered_at_check CHECK (isfinite(registered_at)),
    CONSTRAINT device_push_tokens_token_check CHECK (((octet_length(token) >= 16) AND (octet_length(token) <= 256)))
)
WITH (fillfactor='80');

ALTER TABLE ONLY public.device_push_tokens FORCE ROW LEVEL SECURITY;


--
-- Name: device_sync_cursors; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.device_sync_cursors (
    family_id uuid NOT NULL,
    device_id uuid NOT NULL,
    acked_sequence bigint NOT NULL,
    delivered_sequence bigint NOT NULL,
    woken_sequence bigint DEFAULT 0 NOT NULL,
    seen_at timestamp with time zone NOT NULL,
    woken_at timestamp with time zone,
    pushed_sequence bigint DEFAULT 0 NOT NULL,
    pushed_at timestamp with time zone,
    CONSTRAINT device_sync_cursors_acked_sequence_check CHECK (((acked_sequence >= 0) AND (acked_sequence <= '9007199254740991'::bigint))),
    CONSTRAINT device_sync_cursors_delivered_sequence_check CHECK (((delivered_sequence >= 0) AND (delivered_sequence <= '9007199254740991'::bigint))),
    CONSTRAINT device_sync_cursors_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT device_sync_cursors_pushed_at_check CHECK (((pushed_at IS NULL) OR isfinite(pushed_at))),
    CONSTRAINT device_sync_cursors_pushed_sequence_check CHECK (((pushed_sequence >= 0) AND (pushed_sequence <= '9007199254740991'::bigint))),
    CONSTRAINT device_sync_cursors_seen_at_check CHECK (isfinite(seen_at)),
    CONSTRAINT device_sync_cursors_woken_at_check CHECK (((woken_at IS NULL) OR isfinite(woken_at))),
    CONSTRAINT device_sync_cursors_woken_sequence_check CHECK (((woken_sequence >= 0) AND (woken_sequence <= '9007199254740991'::bigint)))
)
WITH (fillfactor='70', autovacuum_vacuum_scale_factor='0.05');

ALTER TABLE ONLY public.device_sync_cursors FORCE ROW LEVEL SECURITY;


--
-- Name: devices; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.devices (
    id uuid NOT NULL,
    account_id uuid NOT NULL,
    canonical_origin text NOT NULL,
    continuity_key_id text NOT NULL,
    continuity_digest bytea NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    status app.device_status DEFAULT 'active'::app.device_status NOT NULL,
    revoked_at timestamp with time zone,
    CONSTRAINT devices_canonical_origin_check CHECK (((octet_length(canonical_origin) BETWEEN 1 AND 2048) AND (octet_length(canonical_origin) = char_length(canonical_origin)))),
    CONSTRAINT devices_continuity_digest_check CHECK ((octet_length(continuity_digest) = 32)),
    CONSTRAINT devices_continuity_key_id_check CHECK ((continuity_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'::text)),
    CONSTRAINT devices_finite_times CHECK ((((revoked_at IS NULL) OR isfinite(revoked_at)) AND isfinite(created_at) AND isfinite(updated_at))),
    CONSTRAINT devices_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT devices_status_shape CHECK (((status = 'revoked'::app.device_status) = (revoked_at IS NOT NULL)))
);

ALTER TABLE ONLY public.devices FORCE ROW LEVEL SECURITY;


--
-- Name: export_chunks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.export_chunks (
    family_id uuid NOT NULL,
    export_id uuid NOT NULL,
    attempt integer NOT NULL,
    chunk_index bigint NOT NULL,
    plaintext_length integer NOT NULL,
    is_final boolean NOT NULL,
    ciphertext bytea NOT NULL,
    CONSTRAINT export_chunks_check CHECK ((octet_length(ciphertext) = (plaintext_length + 16))),
    CONSTRAINT export_chunks_chunk_index_check CHECK ((chunk_index >= 0)),
    CONSTRAINT export_chunks_plaintext_length_check CHECK (((plaintext_length >= 0) AND (plaintext_length <= 65536)))
);

ALTER TABLE ONLY public.export_chunks FORCE ROW LEVEL SECURITY;


--
-- Name: export_jobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.export_jobs (
    id uuid NOT NULL,
    family_id uuid NOT NULL,
    requester_account_id uuid NOT NULL,
    request_id uuid NOT NULL,
    server_instance_id uuid NOT NULL,
    recovery_epoch bigint NOT NULL,
    security_revision bigint NOT NULL,
    state text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    attempt integer DEFAULT 0 NOT NULL,
    lease_id uuid,
    lease_until timestamp with time zone,
    last_code text,
    header jsonb,
    plaintext_bytes bigint,
    plaintext_sha256 text,
    CONSTRAINT export_jobs_attempt_check CHECK (((attempt >= 0) AND (attempt <= 5))),
    CONSTRAINT export_jobs_check CHECK ((expires_at = (created_at + '24:00:00'::interval))),
    CONSTRAINT export_jobs_check1 CHECK (((lease_id IS NULL) = (lease_until IS NULL))),
    CONSTRAINT export_jobs_check2 CHECK (((state = 'ready'::text) = (header IS NOT NULL))),
    CONSTRAINT export_jobs_check3 CHECK (((state <> 'ready'::text) OR ((plaintext_bytes IS NOT NULL) AND (plaintext_sha256 IS NOT NULL)))),
    CONSTRAINT export_jobs_last_code_check CHECK ((last_code ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT export_jobs_plaintext_bytes_check CHECK (((plaintext_bytes >= 0) AND (plaintext_bytes <= 52428800))),
    CONSTRAINT export_jobs_plaintext_sha256_check CHECK ((plaintext_sha256 ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT export_jobs_recovery_epoch_check CHECK (((recovery_epoch >= 1) AND (recovery_epoch <= '9007199254740991'::bigint))),
    CONSTRAINT export_jobs_security_revision_check CHECK (((security_revision >= 1) AND (security_revision <= '9007199254740991'::bigint))),
    CONSTRAINT export_jobs_state_check CHECK ((state = ANY (ARRAY['queued'::text, 'running'::text, 'ready'::text, 'failed'::text, 'expired'::text])))
);

ALTER TABLE ONLY public.export_jobs FORCE ROW LEVEL SECURITY;


--
-- Name: export_rate_reservations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.export_rate_reservations (
    scope text NOT NULL,
    key_id uuid NOT NULL,
    request_id uuid NOT NULL,
    accepted_at timestamp with time zone NOT NULL,
    CONSTRAINT export_rate_reservations_scope_check CHECK ((scope = ANY (ARRAY['account'::text, 'family'::text])))
);

ALTER TABLE ONLY public.export_rate_reservations FORCE ROW LEVEL SECURITY;


--
-- Name: families; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.families (
    id uuid NOT NULL,
    display_name text NOT NULL,
    status app.family_status DEFAULT 'active'::app.family_status NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    purge_after timestamp with time zone,
    profile_required boolean DEFAULT false NOT NULL,
    next_sequence bigint DEFAULT 0 NOT NULL,
    owner_account_id uuid,
    purge_started_at timestamp with time zone,
    purge_command_id uuid,
    purge_receipt uuid,
    purged_at timestamp with time zone,
    audit_seq bigint DEFAULT 0 NOT NULL,
    audit_hash bytea DEFAULT decode(repeat('00'::text, 32), 'hex'::text) NOT NULL,
    chain_failed boolean DEFAULT false NOT NULL,
    entitlement_required boolean DEFAULT false NOT NULL,
    care_record_count bigint DEFAULT 0 NOT NULL,
    care_charged_bytes bigint DEFAULT 0 NOT NULL,
    care_entity_count bigint DEFAULT 0 NOT NULL,
    care_wire_bytes bigint DEFAULT 0 NOT NULL,
    CONSTRAINT families_active_has_owner CHECK (((status <> 'active'::app.family_status) OR (owner_account_id IS NOT NULL))),
    CONSTRAINT families_audit_hash_check CHECK ((octet_length(audit_hash) = 32)),
    CONSTRAINT families_audit_seq_check CHECK ((audit_seq >= 0)),
    CONSTRAINT families_care_charged_bytes_check CHECK (((care_charged_bytes >= 0) AND (care_charged_bytes <= '9007199254740991'::bigint))),
    CONSTRAINT families_care_entity_count_check CHECK (((care_entity_count >= 0) AND (care_entity_count <= '9007199254740991'::bigint))),
    CONSTRAINT families_care_record_count_check CHECK (((care_record_count >= 0) AND (care_record_count <= '9007199254740991'::bigint))),
    CONSTRAINT families_care_wire_bytes_check CHECK (((care_wire_bytes >= 0) AND (care_wire_bytes <= '9007199254740991'::bigint))),
    CONSTRAINT families_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT families_next_sequence_safe CHECK (((next_sequence >= 0) AND (next_sequence <= '9007199254740991'::bigint)))
)
WITH (fillfactor='70', autovacuum_vacuum_scale_factor='0.05', autovacuum_analyze_scale_factor='0.05');

ALTER TABLE ONLY public.families FORCE ROW LEVEL SECURITY;


--
-- Name: family_actor_label_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.family_actor_label_history (
    label_version bigint NOT NULL,
    family_id uuid NOT NULL,
    account_id uuid NOT NULL,
    display_name text NOT NULL,
    former boolean NOT NULL,
    recorded_at timestamp with time zone NOT NULL,
    CONSTRAINT family_actor_label_history_account_id_check CHECK ((account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT family_actor_label_history_display_name_check CHECK (((char_length(display_name) <= 80) AND (octet_length(display_name) <= 320))),
    CONSTRAINT family_actor_label_history_label_version_check CHECK (((label_version > 0) AND (label_version <= '9007199254740991'::bigint))),
    CONSTRAINT family_actor_label_history_recorded_at_check CHECK (isfinite(recorded_at))
);

ALTER TABLE ONLY public.family_actor_label_history FORCE ROW LEVEL SECURITY;


--
-- Name: family_actor_label_history_label_version_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.family_actor_label_history ALTER COLUMN label_version ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.family_actor_label_history_label_version_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: family_actor_labels; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.family_actor_labels (
    family_id uuid NOT NULL,
    account_id uuid NOT NULL,
    display_name text NOT NULL,
    former boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT family_actor_labels_account_id_check CHECK ((account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT family_actor_labels_created_at_check CHECK (isfinite(created_at)),
    CONSTRAINT family_actor_labels_display_name_check CHECK (((char_length(display_name) <= 80) AND (octet_length(display_name) <= 320))),
    CONSTRAINT family_actor_labels_updated_at_check CHECK (isfinite(updated_at))
);

ALTER TABLE ONLY public.family_actor_labels FORCE ROW LEVEL SECURITY;


--
-- Name: family_entitlements; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.family_entitlements (
    family_id uuid NOT NULL,
    entitlement_key text NOT NULL,
    source text NOT NULL,
    status app.entitlement_status NOT NULL,
    expires_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT family_entitlements_created_at_check CHECK (isfinite(created_at)),
    CONSTRAINT family_entitlements_entitlement_key_check CHECK ((entitlement_key ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT family_entitlements_expires_at_check CHECK (((expires_at IS NULL) OR isfinite(expires_at))),
    CONSTRAINT family_entitlements_source_check CHECK ((source ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT family_entitlements_updated_at_check CHECK (isfinite(updated_at))
);

ALTER TABLE ONLY public.family_entitlements FORCE ROW LEVEL SECURITY;


--
-- Name: family_recovery_claims; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.family_recovery_claims (
    family_id uuid NOT NULL,
    account_id uuid NOT NULL,
    status app.claim_status NOT NULL
);

ALTER TABLE ONLY public.family_recovery_claims FORCE ROW LEVEL SECURITY;


--
-- Name: family_slots; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.family_slots (
    account_id uuid NOT NULL,
    family_id uuid NOT NULL,
    state app.slot_state NOT NULL,
    CONSTRAINT family_slots_account_id_check CHECK ((account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT family_slots_family_id_check CHECK ((family_id <> '00000000-0000-0000-0000-000000000000'::uuid))
);

ALTER TABLE ONLY public.family_slots FORCE ROW LEVEL SECURITY;


--
-- Name: import_baby_candidates; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.import_baby_candidates (
    family_id uuid NOT NULL,
    import_id uuid NOT NULL,
    local_baby_id uuid NOT NULL,
    name text NOT NULL,
    normalized_name text NOT NULL,
    birth_date date NOT NULL,
    sort_order integer NOT NULL,
    storage_bytes bigint NOT NULL,
    CONSTRAINT import_baby_candidates_birth_date_check CHECK ((isfinite(birth_date) AND ((birth_date >= '1900-01-01'::date) AND (birth_date <= '2100-12-31'::date)))),
    CONSTRAINT import_baby_candidates_local_baby_id_check CHECK ((local_baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT import_baby_candidates_name_check CHECK (((octet_length(name) >= 1) AND (octet_length(name) <= 400))),
    CONSTRAINT import_baby_candidates_normalized_name_check CHECK (((octet_length(normalized_name) >= 1) AND (octet_length(normalized_name) <= 1600))),
    CONSTRAINT import_baby_candidates_sort_order_check CHECK (((sort_order >= 0) AND (sort_order <= 1000000))),
    CONSTRAINT import_baby_candidates_storage_bytes_check CHECK (((storage_bytes >= 1) AND (storage_bytes <= '9007199254740991'::bigint)))
);

ALTER TABLE ONLY public.import_baby_candidates FORCE ROW LEVEL SECURITY;


--
-- Name: import_baby_mappings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.import_baby_mappings (
    family_id uuid NOT NULL,
    import_id uuid NOT NULL,
    local_baby_id uuid NOT NULL,
    shared_baby_id uuid NOT NULL,
    published_revision bigint NOT NULL,
    shared_baby_entity_version bigint NOT NULL,
    shared_baby_cursor bigint NOT NULL,
    storage_bytes bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT import_baby_mappings_created_at_check CHECK (isfinite(created_at)),
    CONSTRAINT import_baby_mappings_local_baby_id_check CHECK ((local_baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT import_baby_mappings_published_revision_check CHECK (((published_revision >= 1) AND (published_revision <= '9007199254740991'::bigint))),
    CONSTRAINT import_baby_mappings_shared_baby_cursor_check CHECK (((shared_baby_cursor >= 1) AND (shared_baby_cursor <= '9007199254740991'::bigint))),
    CONSTRAINT import_baby_mappings_shared_baby_entity_version_check CHECK (((shared_baby_entity_version >= 1) AND (shared_baby_entity_version <= '9007199254740991'::bigint))),
    CONSTRAINT import_baby_mappings_shared_baby_id_check CHECK ((shared_baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT import_baby_mappings_storage_bytes_check CHECK (((storage_bytes >= 1) AND (storage_bytes <= '9007199254740991'::bigint)))
);

ALTER TABLE ONLY public.import_baby_mappings FORCE ROW LEVEL SECURITY;


--
-- Name: import_chunks; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.import_chunks (
    family_id uuid NOT NULL,
    import_id uuid NOT NULL,
    chunk_index bigint NOT NULL,
    canonical_sha256 bytea NOT NULL,
    record_count integer NOT NULL,
    finalize boolean NOT NULL,
    response_status integer NOT NULL,
    response_body bytea NOT NULL,
    storage_bytes bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT import_chunks_canonical_sha256_check CHECK ((octet_length(canonical_sha256) = 32)),
    CONSTRAINT import_chunks_chunk_index_check CHECK (((chunk_index >= 0) AND (chunk_index <= '9007199254740991'::bigint))),
    CONSTRAINT import_chunks_created_at_check CHECK (isfinite(created_at)),
    CONSTRAINT import_chunks_nonempty_or_final CHECK (((record_count > 0) OR finalize)),
    CONSTRAINT import_chunks_record_count_check CHECK (((record_count >= 0) AND (record_count <= 1000))),
    CONSTRAINT import_chunks_response_body_check CHECK (((octet_length(response_body) >= 1) AND (octet_length(response_body) <= 4194304))),
    CONSTRAINT import_chunks_response_status_check CHECK (((response_status >= 200) AND (response_status <= 599))),
    CONSTRAINT import_chunks_storage_bytes_check CHECK (((storage_bytes >= 1) AND (storage_bytes <= '9007199254740991'::bigint)))
);

ALTER TABLE ONLY public.import_chunks FORCE ROW LEVEL SECURITY;


--
-- Name: import_conflict_receipts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.import_conflict_receipts (
    family_id uuid NOT NULL,
    import_id uuid NOT NULL,
    command_id uuid NOT NULL,
    account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    command_sha256 bytea NOT NULL,
    resolver_revision bigint NOT NULL,
    response_status integer NOT NULL,
    response_body bytea NOT NULL,
    storage_bytes bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT import_conflict_receipts_account_id_check CHECK ((account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT import_conflict_receipts_command_id_check CHECK ((command_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT import_conflict_receipts_command_sha256_check CHECK ((octet_length(command_sha256) = 32)),
    CONSTRAINT import_conflict_receipts_created_at_check CHECK (isfinite(created_at)),
    CONSTRAINT import_conflict_receipts_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT import_conflict_receipts_resolver_revision_check CHECK (((resolver_revision >= 1) AND (resolver_revision <= '9007199254740991'::bigint))),
    CONSTRAINT import_conflict_receipts_response_body_check CHECK (((octet_length(response_body) >= 1) AND (octet_length(response_body) <= 4194304))),
    CONSTRAINT import_conflict_receipts_response_status_check CHECK (((response_status >= 200) AND (response_status <= 599))),
    CONSTRAINT import_conflict_receipts_storage_bytes_check CHECK (((storage_bytes >= 1) AND (storage_bytes <= '9007199254740991'::bigint)))
);

ALTER TABLE ONLY public.import_conflict_receipts FORCE ROW LEVEL SECURITY;


--
-- Name: import_conflicts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.import_conflicts (
    family_id uuid NOT NULL,
    import_id uuid NOT NULL,
    conflict_id uuid NOT NULL,
    kind text NOT NULL,
    local_baby_id uuid NOT NULL,
    record_id uuid,
    candidate_baby_ids uuid[] DEFAULT '{}'::uuid[] NOT NULL,
    candidate_versions bigint[] DEFAULT '{}'::bigint[] NOT NULL,
    candidate_fingerprint bytea NOT NULL,
    decision_choice text,
    decision_canonical_baby_id uuid,
    decision_account_id uuid,
    decision_revision bigint,
    decided_at timestamp with time zone,
    created_revision bigint NOT NULL,
    updated_revision bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    storage_bytes bigint NOT NULL,
    CONSTRAINT import_conflicts_candidate_fingerprint_check CHECK ((octet_length(candidate_fingerprint) = 32)),
    CONSTRAINT import_conflicts_candidate_shape CHECK (((cardinality(candidate_baby_ids) = cardinality(candidate_versions)) AND (cardinality(candidate_baby_ids) <= 500) AND (array_position(candidate_baby_ids, NULL::uuid) IS NULL) AND (array_position(candidate_versions, NULL::bigint) IS NULL) AND ('00000000-0000-0000-0000-000000000000'::uuid <> ALL (candidate_baby_ids)) AND (1 <= ALL (candidate_versions)) AND ('9007199254740991'::bigint >= ALL (candidate_versions)))),
    CONSTRAINT import_conflicts_conflict_id_check CHECK ((conflict_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT import_conflicts_created_at_check CHECK (isfinite(created_at)),
    CONSTRAINT import_conflicts_created_revision_check CHECK (((created_revision >= 1) AND (created_revision <= '9007199254740991'::bigint))),
    CONSTRAINT import_conflicts_decided_at_check CHECK (((decided_at IS NULL) OR isfinite(decided_at))),
    CONSTRAINT import_conflicts_decision_choice_check CHECK ((decision_choice = ANY (ARRAY['use_existing'::text, 'keep_local'::text]))),
    CONSTRAINT import_conflicts_decision_revision_check CHECK (((decision_revision >= 1) AND (decision_revision <= '9007199254740991'::bigint))),
    CONSTRAINT import_conflicts_decision_state_shape CHECK ((((decision_choice IS NULL) AND (decision_canonical_baby_id IS NULL) AND (decision_account_id IS NULL) AND (decision_revision IS NULL) AND (decided_at IS NULL)) OR (((decision_choice = 'use_existing'::text) IS TRUE) AND (decision_canonical_baby_id IS NOT NULL) AND (decision_account_id IS NOT NULL) AND (decision_revision IS NOT NULL) AND (decided_at IS NOT NULL)) OR (((decision_choice = 'keep_local'::text) IS TRUE) AND (decision_canonical_baby_id IS NULL) AND (decision_account_id IS NOT NULL) AND (decision_revision IS NOT NULL) AND (decided_at IS NOT NULL)))),
    CONSTRAINT import_conflicts_kind_check CHECK ((kind = ANY (ARRAY['ambiguous_baby'::text, 'ambiguous_local_babies'::text, 'baby_uuid_collision'::text, 'record_content_conflict'::text, 'record_deleted_conflict'::text]))),
    CONSTRAINT import_conflicts_kind_shape CHECK (((kind = ANY (ARRAY['record_content_conflict'::text, 'record_deleted_conflict'::text])) = (record_id IS NOT NULL))),
    CONSTRAINT import_conflicts_local_baby_id_check CHECK ((local_baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT import_conflicts_revision_order CHECK ((updated_revision >= created_revision)),
    CONSTRAINT import_conflicts_storage_bytes_check CHECK (((storage_bytes >= 1) AND (storage_bytes <= '9007199254740991'::bigint))),
    CONSTRAINT import_conflicts_updated_at_check CHECK (isfinite(updated_at)),
    CONSTRAINT import_conflicts_updated_revision_check CHECK (((updated_revision >= 1) AND (updated_revision <= '9007199254740991'::bigint)))
);

ALTER TABLE ONLY public.import_conflicts FORCE ROW LEVEL SECURITY;


--
-- Name: import_record_manifests; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.import_record_manifests (
    family_id uuid NOT NULL,
    import_id uuid NOT NULL,
    record_id uuid NOT NULL,
    entity_kind text NOT NULL,
    local_baby_id uuid NOT NULL,
    chunk_index bigint NOT NULL,
    content_sha256 bytea NOT NULL,
    storage_bytes bigint NOT NULL,
    CONSTRAINT import_record_manifests_chunk_index_check CHECK (((chunk_index >= 0) AND (chunk_index <= '9007199254740991'::bigint))),
    CONSTRAINT import_record_manifests_content_sha256_check CHECK ((octet_length(content_sha256) = 32)),
    CONSTRAINT import_record_manifests_entity_kind_check CHECK ((entity_kind = ANY (ARRAY['feed'::text, 'sleep'::text, 'diaper'::text, 'timer'::text, 'supplement'::text, 'growth_profile'::text, 'measurement'::text]))),
    CONSTRAINT import_record_manifests_local_baby_id_check CHECK ((local_baby_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT import_record_manifests_record_id_check CHECK ((record_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT import_record_manifests_storage_bytes_check CHECK (((storage_bytes >= 1) AND (storage_bytes <= '9007199254740991'::bigint)))
);

ALTER TABLE ONLY public.import_record_manifests FORCE ROW LEVEL SECURITY;


--
-- Name: import_sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.import_sessions (
    family_id uuid NOT NULL,
    import_id uuid NOT NULL,
    initiating_account_id uuid NOT NULL,
    initiating_device_id uuid NOT NULL,
    import_mode text NOT NULL,
    normalization_version text NOT NULL,
    total_record_count bigint NOT NULL,
    stored_record_count bigint DEFAULT 0 NOT NULL,
    next_chunk_index bigint DEFAULT 0 NOT NULL,
    manifest_complete boolean DEFAULT false NOT NULL,
    finalized_at timestamp with time zone,
    mapping_complete boolean DEFAULT false NOT NULL,
    mapping_completed_at timestamp with time zone,
    resolver_revision bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    expired_at timestamp with time zone,
    storage_bytes bigint NOT NULL,
    CONSTRAINT import_sessions_check CHECK ((isfinite(expires_at) AND (expires_at > created_at))),
    CONSTRAINT import_sessions_created_at_check CHECK (isfinite(created_at)),
    CONSTRAINT import_sessions_expired_at_check CHECK (((expired_at IS NULL) OR isfinite(expired_at))),
    CONSTRAINT import_sessions_finalized_at_check CHECK (((finalized_at IS NULL) OR isfinite(finalized_at))),
    CONSTRAINT import_sessions_import_id_check CHECK ((import_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT import_sessions_import_mode_check CHECK ((import_mode = ANY (ARRAY['preserve_local_ids'::text, 'match_shared'::text]))),
    CONSTRAINT import_sessions_initiating_account_id_check CHECK ((initiating_account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT import_sessions_initiating_device_id_check CHECK ((initiating_device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT import_sessions_mapping_completed_at_check CHECK (((mapping_completed_at IS NULL) OR isfinite(mapping_completed_at))),
    CONSTRAINT import_sessions_next_chunk_index_check CHECK (((next_chunk_index >= 0) AND (next_chunk_index <= '9007199254740991'::bigint))),
    CONSTRAINT import_sessions_normalization_version_check CHECK ((normalization_version = 'nfc-trim-fold-nfc-v1'::text)),
    CONSTRAINT import_sessions_resolver_revision_check CHECK (((resolver_revision >= 1) AND (resolver_revision <= '9007199254740991'::bigint))),
    CONSTRAINT import_sessions_state_shape CHECK (((stored_record_count <= total_record_count) AND (manifest_complete = (finalized_at IS NOT NULL)) AND ((NOT manifest_complete) OR (stored_record_count = total_record_count)) AND (mapping_complete = (mapping_completed_at IS NOT NULL)) AND ((NOT mapping_complete) OR manifest_complete) AND ((finalized_at IS NULL) OR ((finalized_at >= created_at) AND (finalized_at <= expires_at))) AND ((mapping_completed_at IS NULL) OR ((mapping_completed_at >= finalized_at) AND (mapping_completed_at <= expires_at))) AND ((expired_at IS NULL) OR ((NOT mapping_complete) AND (expired_at >= created_at))) AND (NOT (mapping_complete AND (expired_at IS NOT NULL))))),
    CONSTRAINT import_sessions_storage_bytes_check CHECK (((storage_bytes >= 1) AND (storage_bytes <= '9007199254740991'::bigint))),
    CONSTRAINT import_sessions_stored_record_count_check CHECK (((stored_record_count >= 0) AND (stored_record_count <= 1000000))),
    CONSTRAINT import_sessions_total_record_count_check CHECK (((total_record_count >= 0) AND (total_record_count <= 1000000)))
);

ALTER TABLE ONLY public.import_sessions FORCE ROW LEVEL SECURITY;


--
-- Name: invitations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.invitations (
    id uuid NOT NULL,
    family_id uuid NOT NULL,
    creator_account_id uuid NOT NULL,
    token_digest text NOT NULL,
    role app.member_role NOT NULL,
    status app.invitation_status NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    redeemed_at timestamp with time zone,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT invitations_check1 CHECK ((((redeemed_at IS NULL) OR isfinite(redeemed_at)) AND ((revoked_at IS NULL) OR isfinite(revoked_at)))),
    CONSTRAINT invitations_created_at_check CHECK (isfinite(created_at)),
    CONSTRAINT invitations_expires_at_check CHECK (isfinite(expires_at)),
    CONSTRAINT invitations_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT invitations_role_check CHECK ((role <> 'owner'::app.member_role)),
    CONSTRAINT invitations_status_shape CHECK ((((status = 'active'::app.invitation_status) AND (redeemed_at IS NULL) AND (revoked_at IS NULL)) OR ((status = 'redeemed'::app.invitation_status) AND (redeemed_at IS NOT NULL) AND (revoked_at IS NULL)) OR ((status = 'revoked'::app.invitation_status) AND (redeemed_at IS NULL) AND (revoked_at IS NOT NULL)))),
    CONSTRAINT invitations_token_digest_check CHECK ((token_digest ~ '^hmac-sha256:[a-z][a-z0-9._-]{0,63}:[0-9a-f]{64}$'::text)),
    CONSTRAINT invitations_updated_at_check CHECK (isfinite(updated_at))
);

ALTER TABLE ONLY public.invitations FORCE ROW LEVEL SECURITY;


--
-- Name: live_activity_delivery; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.live_activity_delivery (
    job_id uuid DEFAULT gen_random_uuid() NOT NULL,
    registration_id uuid NOT NULL,
    registration_version bigint NOT NULL,
    account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    family_id uuid NOT NULL,
    baby_id uuid NOT NULL,
    kind text NOT NULL,
    instance_id uuid NOT NULL,
    recovery_epoch bigint NOT NULL,
    dirty_version bigint NOT NULL,
    completed_version bigint DEFAULT 0 NOT NULL,
    not_before timestamp with time zone DEFAULT now() NOT NULL,
    attempts bigint DEFAULT 0 NOT NULL,
    lease_id uuid,
    lease_deadline timestamp with time zone,
    leased_dirty_version bigint,
    leased_registration_version bigint,
    last_reason text,
    target_state jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT live_activity_delivery_check CHECK ((((lease_id IS NULL) AND (lease_deadline IS NULL) AND (leased_dirty_version IS NULL)) OR ((lease_id IS NOT NULL) AND (lease_deadline IS NOT NULL) AND (leased_dirty_version IS NOT NULL)))),
    CONSTRAINT live_activity_delivery_kind_check CHECK ((kind = ANY (ARRAY['feed'::text, 'sleep'::text])))
);


--
-- Name: live_activity_devices; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.live_activity_devices (
    registration_id uuid DEFAULT gen_random_uuid() NOT NULL,
    installation_id uuid NOT NULL,
    account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    family_id uuid NOT NULL,
    instance_id uuid NOT NULL,
    recovery_epoch bigint NOT NULL,
    registration_version bigint NOT NULL,
    environment app.apns_environment NOT NULL,
    topic text NOT NULL,
    push_token text,
    payload_version bigint NOT NULL,
    feed_break_minutes bigint NOT NULL,
    session_id uuid NOT NULL,
    lineage_root_session_id uuid NOT NULL,
    authority_expires_at timestamp with time zone NOT NULL,
    revoked boolean DEFAULT false NOT NULL,
    registered_sequence bigint DEFAULT 0 NOT NULL,
    inventory_version bigint DEFAULT 0 NOT NULL,
    foreground_inventory jsonb,
    CONSTRAINT live_activity_devices_authority_expires_at_check CHECK (isfinite(authority_expires_at)),
    CONSTRAINT live_activity_devices_feed_break_minutes_check CHECK (((feed_break_minutes BETWEEN 5 AND 120) AND ((feed_break_minutes % (5)::bigint) = 0))),
    CONSTRAINT live_activity_devices_payload_version_check CHECK ((payload_version = 1)),
    CONSTRAINT live_activity_devices_push_token_check CHECK (((length(push_token) BETWEEN 2 AND 2048) AND (push_token ~ '^[0-9a-f]+$'::text) AND ((length(push_token) % 2) = 0))),
    CONSTRAINT live_activity_devices_recovery_epoch_check CHECK (((recovery_epoch >= 1) AND (recovery_epoch <= '9007199254740991'::bigint))),
    CONSTRAINT live_activity_devices_registration_version_check CHECK (((registration_version >= 1) AND (registration_version <= '9007199254740991'::bigint))),
    CONSTRAINT live_activity_devices_topic_check CHECK (((length(topic) >= 1) AND (length(topic) <= 255)))
);

ALTER TABLE ONLY public.live_activity_devices FORCE ROW LEVEL SECURITY;


--
-- Name: live_activity_family_dirty; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.live_activity_family_dirty (
    family_id uuid NOT NULL,
    instance_id uuid NOT NULL,
    recovery_epoch bigint NOT NULL,
    dirty_version bigint DEFAULT 1 NOT NULL,
    journal_sequence bigint DEFAULT 0 NOT NULL,
    authority_checked_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT live_activity_family_dirty_dirty_version_check CHECK ((dirty_version > 0)),
    CONSTRAINT live_activity_family_dirty_journal_sequence_check CHECK ((journal_sequence >= 0))
);


--
-- Name: live_activity_history; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.live_activity_history (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    job_id uuid NOT NULL,
    event text NOT NULL,
    result text,
    http_status integer,
    reason text,
    recorded_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT live_activity_history_http_status_check CHECK (((http_status >= 100) AND (http_status <= 599))),
    CONSTRAINT live_activity_history_reason_check CHECK ((reason ~ '^[A-Za-z]{1,64}$'::text)),
    CONSTRAINT live_activity_history_result_check CHECK ((result = ANY (ARRAY['accepted'::text, 'invalidToken'::text, 'retryable'::text, 'configurationFailure'::text, 'ambiguous'::text])))
);


--
-- Name: live_activity_installations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.live_activity_installations (
    instance_id uuid NOT NULL,
    recovery_epoch bigint NOT NULL,
    installation_id uuid NOT NULL,
    account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    family_id uuid NOT NULL,
    version bigint NOT NULL,
    revoked boolean DEFAULT false NOT NULL,
    authority_expires_at timestamp with time zone NOT NULL
);


--
-- Name: live_activity_registration_index; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.live_activity_registration_index (
    registration_id uuid NOT NULL,
    account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    family_id uuid NOT NULL,
    installation_id uuid NOT NULL,
    instance_id uuid NOT NULL,
    recovery_epoch bigint NOT NULL,
    registration_version bigint NOT NULL,
    authority_expires_at timestamp with time zone NOT NULL,
    revoked boolean NOT NULL
);


--
-- Name: live_activity_token_owners; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.live_activity_token_owners (
    environment app.apns_environment NOT NULL,
    topic text NOT NULL,
    token text NOT NULL,
    registration_id uuid NOT NULL,
    slot text NOT NULL
);


--
-- Name: live_activity_tokens; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.live_activity_tokens (
    registration_id uuid NOT NULL,
    account_id uuid NOT NULL,
    family_id uuid NOT NULL,
    activity_id text NOT NULL,
    baby_id uuid NOT NULL,
    kind text NOT NULL,
    environment app.apns_environment NOT NULL,
    topic text NOT NULL,
    token text,
    token_version bigint NOT NULL,
    generation_id uuid DEFAULT gen_random_uuid() NOT NULL,
    known_generation_id uuid,
    timer_id uuid,
    session_id uuid,
    state text NOT NULL,
    token_fingerprint bytea,
    server_retired boolean DEFAULT false NOT NULL,
    CONSTRAINT live_activity_tokens_activity_id_check CHECK (((length(activity_id) >= 1) AND (length(activity_id) <= 256))),
    CONSTRAINT live_activity_tokens_kind_check CHECK ((kind = ANY (ARRAY['feed'::text, 'sleep'::text]))),
    CONSTRAINT live_activity_tokens_state_check CHECK ((state = ANY (ARRAY['active'::text, 'ended'::text, 'dismissed'::text]))),
    CONSTRAINT live_activity_tokens_token_check CHECK (((length(token) BETWEEN 2 AND 2048) AND (token ~ '^[0-9a-f]+$'::text) AND ((length(token) % 2) = 0))),
    CONSTRAINT live_activity_tokens_token_version_check CHECK (((token_version >= 1) AND (token_version <= '9007199254740991'::bigint)))
);

ALTER TABLE ONLY public.live_activity_tokens FORCE ROW LEVEL SECURITY;


--
-- Name: mcp_integrations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.mcp_integrations (
    id uuid NOT NULL,
    account_id uuid NOT NULL,
    family_id uuid NOT NULL,
    recovery_epoch bigint NOT NULL,
    name text NOT NULL,
    scope text NOT NULL,
    verifier bytea NOT NULL,
    status app.integration_status DEFAULT 'active'::app.integration_status NOT NULL,
    created_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    CONSTRAINT mcp_integrations_check CHECK ((expires_at = (created_at + '2160:00:00'::interval))),
    CONSTRAINT mcp_integrations_check1 CHECK (((status = 'revoked'::app.integration_status) = (revoked_at IS NOT NULL))),
    CONSTRAINT mcp_integrations_created_at_check CHECK ((created_at = date_trunc('second'::text, created_at))),
    CONSTRAINT mcp_integrations_name_check CHECK (((octet_length(name) <= 64) AND (name ~ '^[A-Za-z0-9]([A-Za-z0-9 ._-]{0,62}[A-Za-z0-9._-])?$'::text))),
    CONSTRAINT mcp_integrations_scope_check CHECK ((scope = 'family:read'::text)),
    CONSTRAINT mcp_integrations_verifier_check CHECK ((octet_length(verifier) = 32))
);

ALTER TABLE ONLY public.mcp_integrations FORCE ROW LEVEL SECURITY;


--
-- Name: memberships; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.memberships (
    family_id uuid NOT NULL,
    account_id uuid NOT NULL,
    role app.member_role NOT NULL,
    status app.member_status DEFAULT 'active'::app.member_status NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.memberships FORCE ROW LEVEL SECURITY;


--
-- Name: refresh_receipts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.refresh_receipts (
    parent_id uuid NOT NULL,
    child_id uuid NOT NULL,
    attempt_id uuid NOT NULL,
    account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    material bytea NOT NULL,
    material_digest bytea NOT NULL,
    created_at timestamp with time zone NOT NULL,
    effective_deadline timestamp with time zone NOT NULL,
    refresh_expires_at timestamp with time zone NOT NULL,
    child_scope app.session_scope,
    child_family_id uuid,
    child_role app.member_role,
    CONSTRAINT refresh_receipts_attempt_id_check CHECK ((attempt_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT refresh_receipts_check CHECK (((octet_length(material_digest) = 32) AND (sha256(material) = material_digest))),
    CONSTRAINT refresh_receipts_child_scope_shape CHECK (
CASE
    WHEN ((child_scope IS NULL) OR (child_scope = 'account'::app.session_scope)) THEN ((child_family_id IS NULL) AND (child_role IS NULL))
    ELSE ((child_family_id IS NOT NULL) AND (child_role IS NOT NULL))
END),
    CONSTRAINT refresh_receipts_material_check CHECK (((octet_length(material) >= 1) AND (octet_length(material) <= 32768)))
)
WITH (autovacuum_vacuum_scale_factor='0.05');

ALTER TABLE ONLY public.refresh_receipts FORCE ROW LEVEL SECURITY;


--
-- Name: schema_compatibility; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.schema_compatibility (
    singleton boolean DEFAULT true NOT NULL,
    current_version bigint NOT NULL,
    introduced_by text NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT schema_compatibility_current_version_check CHECK ((current_version > 0)),
    CONSTRAINT schema_compatibility_singleton_check CHECK (singleton)
);


--
-- Name: schema_privilege_manifest; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.schema_privilege_manifest (
    schema_version bigint NOT NULL,
    principal text NOT NULL,
    object_kind text NOT NULL,
    object_identity text NOT NULL,
    privileges text NOT NULL,
    CONSTRAINT schema_privilege_manifest_object_identity_check CHECK ((object_identity <> ''::text)),
    CONSTRAINT schema_privilege_manifest_object_kind_check CHECK ((object_kind = ANY (ARRAY['table'::text, 'schema'::text, 'sequence'::text, 'column'::text, 'function'::text]))),
    CONSTRAINT schema_privilege_manifest_principal_check CHECK ((principal = ANY (ARRAY['baby_tracker_runtime'::text, 'baby_tracker_maintenance'::text]))),
    CONSTRAINT schema_privilege_manifest_privileges_check CHECK ((privileges <> ''::text)),
    CONSTRAINT schema_privilege_manifest_schema_version_check CHECK ((schema_version >= 4))
);


--
-- Name: security_audit_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.security_audit_events (
    scope app.audit_scope NOT NULL,
    scope_id uuid NOT NULL,
    seq bigint NOT NULL,
    prev_hash bytea NOT NULL,
    hash bytea NOT NULL,
    kind app.audit_kind NOT NULL,
    command_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    at timestamp with time zone NOT NULL,
    payload jsonb NOT NULL,
    legacy_hash bytea,
    CONSTRAINT security_audit_events_hash_check CHECK ((octet_length(hash) = 32)),
    CONSTRAINT security_audit_events_legacy_hash_check CHECK (((legacy_hash IS NULL) OR (octet_length(legacy_hash) = 32))),
    CONSTRAINT security_audit_events_prev_hash_check CHECK ((octet_length(prev_hash) = 32)),
    CONSTRAINT security_audit_events_seq_check CHECK ((seq > 0))
)
PARTITION BY RANGE (at);

ALTER TABLE ONLY public.security_audit_events FORCE ROW LEVEL SECURITY;


--
-- Name: security_audit_events_2026; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.security_audit_events_2026 (
    scope app.audit_scope NOT NULL,
    scope_id uuid NOT NULL,
    seq bigint NOT NULL,
    prev_hash bytea NOT NULL,
    hash bytea NOT NULL,
    kind app.audit_kind NOT NULL,
    command_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    at timestamp with time zone NOT NULL,
    payload jsonb NOT NULL,
    legacy_hash bytea,
    CONSTRAINT security_audit_events_hash_check CHECK ((octet_length(hash) = 32)),
    CONSTRAINT security_audit_events_legacy_hash_check CHECK (((legacy_hash IS NULL) OR (octet_length(legacy_hash) = 32))),
    CONSTRAINT security_audit_events_prev_hash_check CHECK ((octet_length(prev_hash) = 32)),
    CONSTRAINT security_audit_events_seq_check CHECK ((seq > 0))
)
WITH (autovacuum_vacuum_insert_scale_factor='0.05');


--
-- Name: security_audit_events_default; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.security_audit_events_default (
    scope app.audit_scope NOT NULL,
    scope_id uuid NOT NULL,
    seq bigint NOT NULL,
    prev_hash bytea NOT NULL,
    hash bytea NOT NULL,
    kind app.audit_kind NOT NULL,
    command_id uuid NOT NULL,
    actor_account_id uuid NOT NULL,
    at timestamp with time zone NOT NULL,
    payload jsonb NOT NULL,
    legacy_hash bytea,
    CONSTRAINT security_audit_events_hash_check CHECK ((octet_length(hash) = 32)),
    CONSTRAINT security_audit_events_legacy_hash_check CHECK (((legacy_hash IS NULL) OR (octet_length(legacy_hash) = 32))),
    CONSTRAINT security_audit_events_prev_hash_check CHECK ((octet_length(prev_hash) = 32)),
    CONSTRAINT security_audit_events_seq_check CHECK ((seq > 0))
);


--
-- Name: server_state; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.server_state (
    singleton boolean DEFAULT true NOT NULL,
    instance_id uuid NOT NULL,
    recovery_epoch bigint NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    actor_key_commitment bytea,
    CONSTRAINT server_state_actor_key_commitment_check CHECK (((actor_key_commitment IS NULL) OR (octet_length(actor_key_commitment) = 32))),
    CONSTRAINT server_state_recovery_epoch_check CHECK ((recovery_epoch > 0)),
    CONSTRAINT server_state_recovery_epoch_wire CHECK ((recovery_epoch <= '9007199254740991'::bigint)),
    CONSTRAINT server_state_singleton_check CHECK (singleton)
);


--
-- Name: sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sessions (
    access_digest bytea NOT NULL,
    id uuid NOT NULL,
    account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    lineage_root_id uuid NOT NULL,
    parent_id uuid,
    lineage_started_at timestamp with time zone NOT NULL,
    scope app.session_scope NOT NULL,
    family_id uuid,
    role app.member_role,
    recovery_epoch bigint NOT NULL,
    issued_seq bigint NOT NULL,
    token_key_id text NOT NULL,
    refresh_digest bytea NOT NULL,
    access_expires_at timestamp with time zone NOT NULL,
    refresh_expires_at timestamp with time zone NOT NULL,
    deadline_at timestamp with time zone NOT NULL,
    status app.session_status DEFAULT 'active'::app.session_status NOT NULL,
    consumed_at timestamp with time zone,
    consumed_attempt_id uuid,
    child_id uuid,
    revoked_at timestamp with time zone,
    revoke_reason app.revoke_reason,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT sessions_access_digest_check CHECK ((octet_length(access_digest) = 32)),
    CONSTRAINT sessions_check CHECK ((((scope = 'account'::app.session_scope) AND (family_id IS NULL) AND (role IS NULL)) OR ((scope <> 'account'::app.session_scope) AND (family_id IS NOT NULL) AND (role IS NOT NULL)))),
    CONSTRAINT sessions_check1 CHECK (((access_expires_at <= refresh_expires_at) AND (refresh_expires_at <= deadline_at))),
    CONSTRAINT sessions_check2 CHECK ((((status = 'active'::app.session_status) AND (consumed_at IS NULL) AND (revoked_at IS NULL)) OR ((status = 'consumed'::app.session_status) AND (consumed_at IS NOT NULL) AND (child_id IS NOT NULL)) OR ((status = 'revoked'::app.session_status) AND (revoked_at IS NOT NULL) AND (revoke_reason IS NOT NULL)))),
    CONSTRAINT sessions_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT sessions_issued_seq_check CHECK (((issued_seq >= 1) AND (issued_seq <= '9007199254740991'::bigint))),
    CONSTRAINT sessions_recovery_epoch_check CHECK (((recovery_epoch >= 1) AND (recovery_epoch <= '9007199254740991'::bigint))),
    CONSTRAINT sessions_refresh_digest_check CHECK ((octet_length(refresh_digest) = 32)),
    CONSTRAINT sessions_token_key_id_check CHECK ((token_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'::text))
)
WITH (fillfactor='70', autovacuum_vacuum_scale_factor='0.02', autovacuum_analyze_scale_factor='0.02');

ALTER TABLE ONLY public.sessions FORCE ROW LEVEL SECURITY;


--
-- Name: sync_page_bindings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sync_page_bindings (
    family_id uuid NOT NULL,
    token_digest bytea NOT NULL,
    token_key_id text NOT NULL,
    account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    session_id uuid NOT NULL,
    server_instance_id uuid NOT NULL,
    recovery_epoch bigint NOT NULL,
    security_revision bigint NOT NULL,
    kind app.page_kind NOT NULL,
    high_water_sequence bigint NOT NULL,
    resume_sequence bigint,
    snapshot_id uuid,
    resume_kind smallint,
    resume_id uuid,
    item_limit integer NOT NULL,
    byte_limit integer NOT NULL,
    created_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT sync_page_bindings_byte_limit_check CHECK (((byte_limit >= 65536) AND (byte_limit <= 4194304))),
    CONSTRAINT sync_page_bindings_check CHECK ((isfinite(expires_at) AND (expires_at > created_at))),
    CONSTRAINT sync_page_bindings_check1 CHECK ((((kind = 'sync'::app.page_kind) AND (resume_sequence IS NOT NULL) AND (resume_sequence <= high_water_sequence) AND (snapshot_id IS NULL) AND (resume_kind IS NULL) AND (resume_id IS NULL)) OR ((kind = 'snapshot'::app.page_kind) AND (resume_sequence IS NULL) AND (snapshot_id IS NOT NULL) AND (resume_kind IS NOT NULL) AND (resume_id IS NOT NULL)))),
    CONSTRAINT sync_page_bindings_created_at_check CHECK (isfinite(created_at)),
    CONSTRAINT sync_page_bindings_high_water_sequence_check CHECK (((high_water_sequence >= 0) AND (high_water_sequence <= '9007199254740991'::bigint))),
    CONSTRAINT sync_page_bindings_item_limit_check CHECK (((item_limit >= 1) AND (item_limit <= 1000))),
    CONSTRAINT sync_page_bindings_recovery_epoch_check CHECK (((recovery_epoch >= 1) AND (recovery_epoch <= '9007199254740991'::bigint))),
    CONSTRAINT sync_page_bindings_resume_kind_check CHECK (((resume_kind >= 1) AND (resume_kind <= 8))),
    CONSTRAINT sync_page_bindings_resume_sequence_check CHECK (((resume_sequence >= 0) AND (resume_sequence <= '9007199254740991'::bigint))),
    CONSTRAINT sync_page_bindings_security_revision_check CHECK (((security_revision >= 1) AND (security_revision <= '9007199254740991'::bigint))),
    CONSTRAINT sync_page_bindings_server_instance_id_check CHECK ((server_instance_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT sync_page_bindings_token_digest_check CHECK ((octet_length(token_digest) = 32)),
    CONSTRAINT sync_page_bindings_token_key_id_check CHECK ((token_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'::text))
)
WITH (autovacuum_vacuum_scale_factor='0.05');

ALTER TABLE ONLY public.sync_page_bindings FORCE ROW LEVEL SECURITY;


--
-- Name: COLUMN sync_page_bindings.resume_kind; Type: COMMENT; Schema: public; Owner: -
--

COMMENT ON COLUMN public.sync_page_bindings.resume_kind IS 'care kind: 1 baby, 2 feed, 3 sleep, 4 diaper, 5 timer, 6 supplement, 7 growth_profile, 8 measurement';


--
-- Name: sync_snapshot_sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sync_snapshot_sessions (
    family_id uuid NOT NULL,
    id uuid NOT NULL,
    account_id uuid NOT NULL,
    device_id uuid NOT NULL,
    session_id uuid NOT NULL,
    server_instance_id uuid NOT NULL,
    recovery_epoch bigint NOT NULL,
    security_revision bigint NOT NULL,
    high_water_sequence bigint NOT NULL,
    captured_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT sync_snapshot_sessions_account_id_check CHECK ((account_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT sync_snapshot_sessions_captured_at_check CHECK (isfinite(captured_at)),
    CONSTRAINT sync_snapshot_sessions_check CHECK ((isfinite(expires_at) AND (expires_at > captured_at))),
    CONSTRAINT sync_snapshot_sessions_device_id_check CHECK ((device_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT sync_snapshot_sessions_high_water_sequence_check CHECK (((high_water_sequence >= 0) AND (high_water_sequence <= '9007199254740991'::bigint))),
    CONSTRAINT sync_snapshot_sessions_id_check CHECK ((id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT sync_snapshot_sessions_recovery_epoch_check CHECK (((recovery_epoch >= 1) AND (recovery_epoch <= '9007199254740991'::bigint))),
    CONSTRAINT sync_snapshot_sessions_security_revision_check CHECK (((security_revision >= 1) AND (security_revision <= '9007199254740991'::bigint))),
    CONSTRAINT sync_snapshot_sessions_server_instance_id_check CHECK ((server_instance_id <> '00000000-0000-0000-0000-000000000000'::uuid)),
    CONSTRAINT sync_snapshot_sessions_session_id_check CHECK ((session_id <> '00000000-0000-0000-0000-000000000000'::uuid))
)
WITH (autovacuum_vacuum_scale_factor='0.05');

ALTER TABLE ONLY public.sync_snapshot_sessions FORCE ROW LEVEL SECURITY;


--
-- Name: care_entities_p00; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p00 FOR VALUES WITH (modulus 16, remainder 0);


--
-- Name: care_entities_p01; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p01 FOR VALUES WITH (modulus 16, remainder 1);


--
-- Name: care_entities_p02; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p02 FOR VALUES WITH (modulus 16, remainder 2);


--
-- Name: care_entities_p03; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p03 FOR VALUES WITH (modulus 16, remainder 3);


--
-- Name: care_entities_p04; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p04 FOR VALUES WITH (modulus 16, remainder 4);


--
-- Name: care_entities_p05; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p05 FOR VALUES WITH (modulus 16, remainder 5);


--
-- Name: care_entities_p06; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p06 FOR VALUES WITH (modulus 16, remainder 6);


--
-- Name: care_entities_p07; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p07 FOR VALUES WITH (modulus 16, remainder 7);


--
-- Name: care_entities_p08; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p08 FOR VALUES WITH (modulus 16, remainder 8);


--
-- Name: care_entities_p09; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p09 FOR VALUES WITH (modulus 16, remainder 9);


--
-- Name: care_entities_p10; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p10 FOR VALUES WITH (modulus 16, remainder 10);


--
-- Name: care_entities_p11; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p11 FOR VALUES WITH (modulus 16, remainder 11);


--
-- Name: care_entities_p12; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p12 FOR VALUES WITH (modulus 16, remainder 12);


--
-- Name: care_entities_p13; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p13 FOR VALUES WITH (modulus 16, remainder 13);


--
-- Name: care_entities_p14; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p14 FOR VALUES WITH (modulus 16, remainder 14);


--
-- Name: care_entities_p15; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities ATTACH PARTITION public.care_entities_p15 FOR VALUES WITH (modulus 16, remainder 15);


--
-- Name: security_audit_events_2026; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.security_audit_events ATTACH PARTITION public.security_audit_events_2026 FOR VALUES FROM ('2026-01-01 00:00:00+00') TO ('2027-01-01 00:00:00+00');


--
-- Name: security_audit_events_default; Type: TABLE ATTACH; Schema: public; Owner: -
--

ALTER TABLE ONLY public.security_audit_events ATTACH PARTITION public.security_audit_events_default DEFAULT;


--
-- Name: account_identities account_identities_account_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_identities
    ADD CONSTRAINT account_identities_account_id_key UNIQUE (account_id);


--
-- Name: account_identities account_identities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_identities
    ADD CONSTRAINT account_identities_pkey PRIMARY KEY (provider, identity_key_id, identity_digest, lifecycle_generation);


--
-- Name: account_profile_commands account_profile_commands_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_profile_commands
    ADD CONSTRAINT account_profile_commands_pkey PRIMARY KEY (account_id, command_id);


--
-- Name: accounts accounts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.accounts
    ADD CONSTRAINT accounts_pkey PRIMARY KEY (id);


--
-- Name: apple_notifications apple_notifications_command_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.apple_notifications
    ADD CONSTRAINT apple_notifications_command_id_key UNIQUE (command_id);


--
-- Name: apple_notifications apple_notifications_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.apple_notifications
    ADD CONSTRAINT apple_notifications_pkey PRIMARY KEY (notification_key_id, notification_digest);


--
-- Name: audit_checkpoints audit_checkpoints_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_checkpoints
    ADD CONSTRAINT audit_checkpoints_pkey PRIMARY KEY (scope_id, scope);


--
-- Name: auth_challenges auth_challenges_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.auth_challenges
    ADD CONSTRAINT auth_challenges_pkey PRIMARY KEY (token_digest);


--
-- Name: care_entities care_entities_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities
    ADD CONSTRAINT care_entities_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p00 care_entities_p00_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p00
    ADD CONSTRAINT care_entities_p00_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities care_entities_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities
    ADD CONSTRAINT care_entities_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p00 care_entities_p00_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p00
    ADD CONSTRAINT care_entities_p00_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p01 care_entities_p01_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p01
    ADD CONSTRAINT care_entities_p01_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p01 care_entities_p01_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p01
    ADD CONSTRAINT care_entities_p01_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p02 care_entities_p02_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p02
    ADD CONSTRAINT care_entities_p02_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p02 care_entities_p02_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p02
    ADD CONSTRAINT care_entities_p02_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p03 care_entities_p03_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p03
    ADD CONSTRAINT care_entities_p03_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p03 care_entities_p03_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p03
    ADD CONSTRAINT care_entities_p03_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p04 care_entities_p04_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p04
    ADD CONSTRAINT care_entities_p04_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p04 care_entities_p04_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p04
    ADD CONSTRAINT care_entities_p04_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p05 care_entities_p05_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p05
    ADD CONSTRAINT care_entities_p05_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p05 care_entities_p05_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p05
    ADD CONSTRAINT care_entities_p05_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p06 care_entities_p06_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p06
    ADD CONSTRAINT care_entities_p06_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p06 care_entities_p06_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p06
    ADD CONSTRAINT care_entities_p06_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p07 care_entities_p07_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p07
    ADD CONSTRAINT care_entities_p07_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p07 care_entities_p07_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p07
    ADD CONSTRAINT care_entities_p07_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p08 care_entities_p08_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p08
    ADD CONSTRAINT care_entities_p08_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p08 care_entities_p08_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p08
    ADD CONSTRAINT care_entities_p08_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p09 care_entities_p09_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p09
    ADD CONSTRAINT care_entities_p09_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p09 care_entities_p09_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p09
    ADD CONSTRAINT care_entities_p09_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p10 care_entities_p10_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p10
    ADD CONSTRAINT care_entities_p10_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p10 care_entities_p10_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p10
    ADD CONSTRAINT care_entities_p10_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p11 care_entities_p11_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p11
    ADD CONSTRAINT care_entities_p11_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p11 care_entities_p11_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p11
    ADD CONSTRAINT care_entities_p11_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p12 care_entities_p12_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p12
    ADD CONSTRAINT care_entities_p12_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p12 care_entities_p12_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p12
    ADD CONSTRAINT care_entities_p12_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p13 care_entities_p13_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p13
    ADD CONSTRAINT care_entities_p13_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p13 care_entities_p13_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p13
    ADD CONSTRAINT care_entities_p13_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p14 care_entities_p14_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p14
    ADD CONSTRAINT care_entities_p14_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p14 care_entities_p14_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p14
    ADD CONSTRAINT care_entities_p14_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_entities_p15 care_entities_p15_family_id_changed_seq_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p15
    ADD CONSTRAINT care_entities_p15_family_id_changed_seq_key UNIQUE (family_id, changed_seq);


--
-- Name: care_entities_p15 care_entities_p15_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_entities_p15
    ADD CONSTRAINT care_entities_p15_pkey PRIMARY KEY (family_id, id);


--
-- Name: care_receipts care_receipts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_receipts
    ADD CONSTRAINT care_receipts_pkey PRIMARY KEY (device_id, mutation_id);


--
-- Name: command_receipts command_receipts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.command_receipts
    ADD CONSTRAINT command_receipts_pkey PRIMARY KEY (actor_account_id, command_id);


--
-- Name: device_push_tokens device_push_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.device_push_tokens
    ADD CONSTRAINT device_push_tokens_pkey PRIMARY KEY (device_id);


--
-- Name: device_sync_cursors device_sync_cursors_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.device_sync_cursors
    ADD CONSTRAINT device_sync_cursors_pkey PRIMARY KEY (family_id, device_id);


--
-- Name: devices devices_id_account_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.devices
    ADD CONSTRAINT devices_id_account_id_key UNIQUE (id, account_id);


--
-- Name: devices devices_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.devices
    ADD CONSTRAINT devices_pkey PRIMARY KEY (id);


--
-- Name: export_chunks export_chunks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.export_chunks
    ADD CONSTRAINT export_chunks_pkey PRIMARY KEY (export_id, attempt, chunk_index);


--
-- Name: export_jobs export_jobs_id_attempt_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.export_jobs
    ADD CONSTRAINT export_jobs_id_attempt_key UNIQUE (id, attempt);


--
-- Name: export_jobs export_jobs_id_family_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.export_jobs
    ADD CONSTRAINT export_jobs_id_family_id_key UNIQUE (id, family_id);


--
-- Name: export_jobs export_jobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.export_jobs
    ADD CONSTRAINT export_jobs_pkey PRIMARY KEY (id);


--
-- Name: export_jobs export_jobs_requester_account_id_request_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.export_jobs
    ADD CONSTRAINT export_jobs_requester_account_id_request_id_key UNIQUE (requester_account_id, request_id);


--
-- Name: export_rate_reservations export_rate_reservations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.export_rate_reservations
    ADD CONSTRAINT export_rate_reservations_pkey PRIMARY KEY (scope, key_id, request_id);


--
-- Name: families families_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.families
    ADD CONSTRAINT families_pkey PRIMARY KEY (id);


--
-- Name: family_actor_label_history family_actor_label_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.family_actor_label_history
    ADD CONSTRAINT family_actor_label_history_pkey PRIMARY KEY (label_version);


--
-- Name: family_actor_labels family_actor_labels_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.family_actor_labels
    ADD CONSTRAINT family_actor_labels_pkey PRIMARY KEY (family_id, account_id);


--
-- Name: family_entitlements family_entitlements_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.family_entitlements
    ADD CONSTRAINT family_entitlements_pkey PRIMARY KEY (family_id, entitlement_key);


--
-- Name: family_recovery_claims family_recovery_claims_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.family_recovery_claims
    ADD CONSTRAINT family_recovery_claims_pkey PRIMARY KEY (family_id);


--
-- Name: family_slots family_slots_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.family_slots
    ADD CONSTRAINT family_slots_pkey PRIMARY KEY (account_id);


--
-- Name: import_baby_candidates import_baby_candidates_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_baby_candidates
    ADD CONSTRAINT import_baby_candidates_pkey PRIMARY KEY (family_id, import_id, local_baby_id);


--
-- Name: import_baby_mappings import_baby_mappings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_baby_mappings
    ADD CONSTRAINT import_baby_mappings_pkey PRIMARY KEY (family_id, import_id, local_baby_id);


--
-- Name: import_chunks import_chunks_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_chunks
    ADD CONSTRAINT import_chunks_pkey PRIMARY KEY (family_id, import_id, chunk_index);


--
-- Name: import_conflict_receipts import_conflict_receipts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_conflict_receipts
    ADD CONSTRAINT import_conflict_receipts_pkey PRIMARY KEY (family_id, import_id, command_id);


--
-- Name: import_conflicts import_conflicts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_conflicts
    ADD CONSTRAINT import_conflicts_pkey PRIMARY KEY (family_id, import_id, conflict_id);


--
-- Name: import_record_manifests import_record_manifests_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_record_manifests
    ADD CONSTRAINT import_record_manifests_pkey PRIMARY KEY (family_id, import_id, record_id);


--
-- Name: import_sessions import_sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_sessions
    ADD CONSTRAINT import_sessions_pkey PRIMARY KEY (family_id, import_id);


--
-- Name: invitations invitations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.invitations
    ADD CONSTRAINT invitations_pkey PRIMARY KEY (id);


--
-- Name: invitations invitations_token_digest_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.invitations
    ADD CONSTRAINT invitations_token_digest_key UNIQUE (token_digest);


--
-- Name: live_activity_delivery live_activity_delivery_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_delivery
    ADD CONSTRAINT live_activity_delivery_pkey PRIMARY KEY (job_id);


--
-- Name: live_activity_delivery live_activity_delivery_registration_id_baby_id_kind_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_delivery
    ADD CONSTRAINT live_activity_delivery_registration_id_baby_id_kind_key UNIQUE (registration_id, baby_id, kind);


--
-- Name: live_activity_devices live_activity_devices_instance_id_recovery_epoch_installati_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_devices
    ADD CONSTRAINT live_activity_devices_instance_id_recovery_epoch_installati_key UNIQUE (instance_id, recovery_epoch, installation_id, family_id);


--
-- Name: live_activity_devices live_activity_devices_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_devices
    ADD CONSTRAINT live_activity_devices_pkey PRIMARY KEY (registration_id);


--
-- Name: live_activity_devices live_activity_devices_registration_id_account_id_family_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_devices
    ADD CONSTRAINT live_activity_devices_registration_id_account_id_family_id_key UNIQUE (registration_id, account_id, family_id);


--
-- Name: live_activity_family_dirty live_activity_family_dirty_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_family_dirty
    ADD CONSTRAINT live_activity_family_dirty_pkey PRIMARY KEY (family_id);


--
-- Name: live_activity_history live_activity_history_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_history
    ADD CONSTRAINT live_activity_history_pkey PRIMARY KEY (id);


--
-- Name: live_activity_installations live_activity_installations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_installations
    ADD CONSTRAINT live_activity_installations_pkey PRIMARY KEY (instance_id, recovery_epoch, installation_id);


--
-- Name: live_activity_registration_index live_activity_registration_index_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_registration_index
    ADD CONSTRAINT live_activity_registration_index_pkey PRIMARY KEY (registration_id);


--
-- Name: live_activity_token_owners live_activity_token_owners_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_token_owners
    ADD CONSTRAINT live_activity_token_owners_pkey PRIMARY KEY (environment, topic, token);


--
-- Name: live_activity_token_owners live_activity_token_owners_registration_id_slot_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_token_owners
    ADD CONSTRAINT live_activity_token_owners_registration_id_slot_key UNIQUE (registration_id, slot);


--
-- Name: live_activity_tokens live_activity_tokens_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_tokens
    ADD CONSTRAINT live_activity_tokens_pkey PRIMARY KEY (registration_id, activity_id);


--
-- Name: mcp_integrations mcp_integrations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mcp_integrations
    ADD CONSTRAINT mcp_integrations_pkey PRIMARY KEY (id);


--
-- Name: mcp_integrations mcp_integrations_verifier_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mcp_integrations
    ADD CONSTRAINT mcp_integrations_verifier_key UNIQUE (verifier);


--
-- Name: memberships memberships_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memberships
    ADD CONSTRAINT memberships_pkey PRIMARY KEY (family_id, account_id);


--
-- Name: refresh_receipts refresh_receipts_child_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.refresh_receipts
    ADD CONSTRAINT refresh_receipts_child_id_key UNIQUE (child_id);


--
-- Name: refresh_receipts refresh_receipts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.refresh_receipts
    ADD CONSTRAINT refresh_receipts_pkey PRIMARY KEY (parent_id);


--
-- Name: schema_compatibility schema_compatibility_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.schema_compatibility
    ADD CONSTRAINT schema_compatibility_pkey PRIMARY KEY (singleton);


--
-- Name: schema_privilege_manifest schema_privilege_manifest_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.schema_privilege_manifest
    ADD CONSTRAINT schema_privilege_manifest_pkey PRIMARY KEY (schema_version, principal, object_kind, object_identity);


--
-- Name: security_audit_events security_audit_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.security_audit_events
    ADD CONSTRAINT security_audit_events_pkey PRIMARY KEY (scope_id, seq, at, scope);


--
-- Name: security_audit_events_2026 security_audit_events_2026_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.security_audit_events_2026
    ADD CONSTRAINT security_audit_events_2026_pkey PRIMARY KEY (scope_id, seq, at, scope);


--
-- Name: security_audit_events_default security_audit_events_default_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.security_audit_events_default
    ADD CONSTRAINT security_audit_events_default_pkey PRIMARY KEY (scope_id, seq, at, scope);


--
-- Name: server_state server_state_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.server_state
    ADD CONSTRAINT server_state_pkey PRIMARY KEY (singleton);


--
-- Name: sessions sessions_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_id_key UNIQUE (id);


--
-- Name: sessions sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (access_digest);


--
-- Name: sessions sessions_refresh_digest_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_refresh_digest_key UNIQUE (refresh_digest);


--
-- Name: sync_page_bindings sync_page_bindings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sync_page_bindings
    ADD CONSTRAINT sync_page_bindings_pkey PRIMARY KEY (family_id, token_digest);


--
-- Name: sync_snapshot_sessions sync_snapshot_sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sync_snapshot_sessions
    ADD CONSTRAINT sync_snapshot_sessions_pkey PRIMARY KEY (family_id, id);


--
-- Name: account_identities_live_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX account_identities_live_idx ON public.account_identities USING btree (provider, identity_key_id, identity_digest) WHERE live;


--
-- Name: auth_challenges_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX auth_challenges_expiry_idx ON public.auth_challenges USING btree (expires_at);


--
-- Name: care_entities_baby_match_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_baby_match_idx ON ONLY public.care_entities USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_history_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_history_idx ON ONLY public.care_entities USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_latest_breast_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_latest_breast_idx ON ONLY public.care_entities USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_latest_milk_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_latest_milk_idx ON ONLY public.care_entities USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_one_growth_profile; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_one_growth_profile ON ONLY public.care_entities USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_one_live_timer; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_one_live_timer ON ONLY public.care_entities USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p00_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p00_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p00 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p00_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p00_family_id_baby_id_idx ON public.care_entities_p00 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p00_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p00_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p00 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p00_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p00_family_id_baby_id_started_at_id_idx ON public.care_entities_p00 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p00_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p00_family_id_baby_id_timer_kind_idx ON public.care_entities_p00 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_snapshot_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_snapshot_idx ON ONLY public.care_entities USING btree (family_id, kind, id);


--
-- Name: care_entities_p00_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p00_family_id_kind_id_idx ON public.care_entities_p00 USING btree (family_id, kind, id);


--
-- Name: care_entities_p00_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p00_family_id_name_hash_idx ON public.care_entities_p00 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_session_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_session_idx ON ONLY public.care_entities USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p00_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p00_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p00 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p01_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p01_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p01 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p01_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p01_family_id_baby_id_idx ON public.care_entities_p01 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p01_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p01_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p01 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p01_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p01_family_id_baby_id_started_at_id_idx ON public.care_entities_p01 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p01_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p01_family_id_baby_id_timer_kind_idx ON public.care_entities_p01 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p01_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p01_family_id_kind_id_idx ON public.care_entities_p01 USING btree (family_id, kind, id);


--
-- Name: care_entities_p01_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p01_family_id_name_hash_idx ON public.care_entities_p01 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p01_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p01_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p01 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p02_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p02_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p02 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p02_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p02_family_id_baby_id_idx ON public.care_entities_p02 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p02_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p02_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p02 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p02_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p02_family_id_baby_id_started_at_id_idx ON public.care_entities_p02 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p02_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p02_family_id_baby_id_timer_kind_idx ON public.care_entities_p02 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p02_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p02_family_id_kind_id_idx ON public.care_entities_p02 USING btree (family_id, kind, id);


--
-- Name: care_entities_p02_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p02_family_id_name_hash_idx ON public.care_entities_p02 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p02_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p02_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p02 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p03_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p03_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p03 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p03_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p03_family_id_baby_id_idx ON public.care_entities_p03 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p03_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p03_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p03 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p03_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p03_family_id_baby_id_started_at_id_idx ON public.care_entities_p03 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p03_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p03_family_id_baby_id_timer_kind_idx ON public.care_entities_p03 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p03_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p03_family_id_kind_id_idx ON public.care_entities_p03 USING btree (family_id, kind, id);


--
-- Name: care_entities_p03_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p03_family_id_name_hash_idx ON public.care_entities_p03 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p03_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p03_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p03 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p04_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p04_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p04 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p04_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p04_family_id_baby_id_idx ON public.care_entities_p04 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p04_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p04_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p04 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p04_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p04_family_id_baby_id_started_at_id_idx ON public.care_entities_p04 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p04_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p04_family_id_baby_id_timer_kind_idx ON public.care_entities_p04 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p04_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p04_family_id_kind_id_idx ON public.care_entities_p04 USING btree (family_id, kind, id);


--
-- Name: care_entities_p04_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p04_family_id_name_hash_idx ON public.care_entities_p04 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p04_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p04_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p04 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p05_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p05_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p05 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p05_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p05_family_id_baby_id_idx ON public.care_entities_p05 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p05_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p05_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p05 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p05_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p05_family_id_baby_id_started_at_id_idx ON public.care_entities_p05 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p05_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p05_family_id_baby_id_timer_kind_idx ON public.care_entities_p05 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p05_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p05_family_id_kind_id_idx ON public.care_entities_p05 USING btree (family_id, kind, id);


--
-- Name: care_entities_p05_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p05_family_id_name_hash_idx ON public.care_entities_p05 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p05_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p05_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p05 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p06_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p06_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p06 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p06_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p06_family_id_baby_id_idx ON public.care_entities_p06 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p06_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p06_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p06 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p06_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p06_family_id_baby_id_started_at_id_idx ON public.care_entities_p06 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p06_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p06_family_id_baby_id_timer_kind_idx ON public.care_entities_p06 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p06_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p06_family_id_kind_id_idx ON public.care_entities_p06 USING btree (family_id, kind, id);


--
-- Name: care_entities_p06_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p06_family_id_name_hash_idx ON public.care_entities_p06 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p06_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p06_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p06 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p07_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p07_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p07 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p07_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p07_family_id_baby_id_idx ON public.care_entities_p07 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p07_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p07_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p07 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p07_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p07_family_id_baby_id_started_at_id_idx ON public.care_entities_p07 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p07_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p07_family_id_baby_id_timer_kind_idx ON public.care_entities_p07 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p07_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p07_family_id_kind_id_idx ON public.care_entities_p07 USING btree (family_id, kind, id);


--
-- Name: care_entities_p07_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p07_family_id_name_hash_idx ON public.care_entities_p07 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p07_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p07_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p07 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p08_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p08_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p08 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p08_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p08_family_id_baby_id_idx ON public.care_entities_p08 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p08_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p08_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p08 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p08_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p08_family_id_baby_id_started_at_id_idx ON public.care_entities_p08 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p08_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p08_family_id_baby_id_timer_kind_idx ON public.care_entities_p08 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p08_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p08_family_id_kind_id_idx ON public.care_entities_p08 USING btree (family_id, kind, id);


--
-- Name: care_entities_p08_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p08_family_id_name_hash_idx ON public.care_entities_p08 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p08_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p08_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p08 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p09_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p09_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p09 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p09_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p09_family_id_baby_id_idx ON public.care_entities_p09 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p09_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p09_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p09 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p09_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p09_family_id_baby_id_started_at_id_idx ON public.care_entities_p09 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p09_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p09_family_id_baby_id_timer_kind_idx ON public.care_entities_p09 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p09_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p09_family_id_kind_id_idx ON public.care_entities_p09 USING btree (family_id, kind, id);


--
-- Name: care_entities_p09_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p09_family_id_name_hash_idx ON public.care_entities_p09 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p09_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p09_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p09 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p10_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p10_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p10 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p10_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p10_family_id_baby_id_idx ON public.care_entities_p10 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p10_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p10_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p10 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p10_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p10_family_id_baby_id_started_at_id_idx ON public.care_entities_p10 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p10_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p10_family_id_baby_id_timer_kind_idx ON public.care_entities_p10 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p10_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p10_family_id_kind_id_idx ON public.care_entities_p10 USING btree (family_id, kind, id);


--
-- Name: care_entities_p10_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p10_family_id_name_hash_idx ON public.care_entities_p10 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p10_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p10_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p10 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p11_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p11_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p11 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p11_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p11_family_id_baby_id_idx ON public.care_entities_p11 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p11_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p11_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p11 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p11_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p11_family_id_baby_id_started_at_id_idx ON public.care_entities_p11 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p11_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p11_family_id_baby_id_timer_kind_idx ON public.care_entities_p11 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p11_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p11_family_id_kind_id_idx ON public.care_entities_p11 USING btree (family_id, kind, id);


--
-- Name: care_entities_p11_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p11_family_id_name_hash_idx ON public.care_entities_p11 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p11_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p11_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p11 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p12_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p12_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p12 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p12_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p12_family_id_baby_id_idx ON public.care_entities_p12 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p12_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p12_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p12 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p12_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p12_family_id_baby_id_started_at_id_idx ON public.care_entities_p12 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p12_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p12_family_id_baby_id_timer_kind_idx ON public.care_entities_p12 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p12_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p12_family_id_kind_id_idx ON public.care_entities_p12 USING btree (family_id, kind, id);


--
-- Name: care_entities_p12_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p12_family_id_name_hash_idx ON public.care_entities_p12 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p12_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p12_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p12 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p13_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p13_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p13 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p13_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p13_family_id_baby_id_idx ON public.care_entities_p13 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p13_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p13_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p13 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p13_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p13_family_id_baby_id_started_at_id_idx ON public.care_entities_p13 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p13_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p13_family_id_baby_id_timer_kind_idx ON public.care_entities_p13 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p13_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p13_family_id_kind_id_idx ON public.care_entities_p13 USING btree (family_id, kind, id);


--
-- Name: care_entities_p13_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p13_family_id_name_hash_idx ON public.care_entities_p13 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p13_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p13_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p13 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p14_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p14_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p14 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p14_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p14_family_id_baby_id_idx ON public.care_entities_p14 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p14_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p14_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p14 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p14_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p14_family_id_baby_id_started_at_id_idx ON public.care_entities_p14 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p14_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p14_family_id_baby_id_timer_kind_idx ON public.care_entities_p14 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p14_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p14_family_id_kind_id_idx ON public.care_entities_p14 USING btree (family_id, kind, id);


--
-- Name: care_entities_p14_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p14_family_id_name_hash_idx ON public.care_entities_p14 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p14_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p14_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p14 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_entities_p15_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p15_family_id_baby_id_coalesce_coalesce1_id_idx ON public.care_entities_p15 USING btree (family_id, baby_id, COALESCE(ended_at, started_at) DESC, COALESCE(session_id, id) DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind <> 3));


--
-- Name: care_entities_p15_family_id_baby_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p15_family_id_baby_id_idx ON public.care_entities_p15 USING btree (family_id, baby_id) WHERE (kind = 7);


--
-- Name: care_entities_p15_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p15_family_id_baby_id_kind_started_at_id_ende_idx ON public.care_entities_p15 USING btree (family_id, baby_id, kind, started_at, id) INCLUDE (ended_at) WHERE (tombstoned_at IS NULL);


--
-- Name: care_entities_p15_family_id_baby_id_started_at_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p15_family_id_baby_id_started_at_id_idx ON public.care_entities_p15 USING btree (family_id, baby_id, started_at DESC, id DESC) WHERE ((kind = 2) AND (tombstoned_at IS NULL) AND (feed_kind = 1));


--
-- Name: care_entities_p15_family_id_baby_id_timer_kind_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX care_entities_p15_family_id_baby_id_timer_kind_idx ON public.care_entities_p15 USING btree (family_id, baby_id, timer_kind) WHERE ((kind = 5) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p15_family_id_kind_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p15_family_id_kind_id_idx ON public.care_entities_p15 USING btree (family_id, kind, id);


--
-- Name: care_entities_p15_family_id_name_hash_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p15_family_id_name_hash_idx ON public.care_entities_p15 USING btree (family_id, name_hash) WHERE ((kind = 1) AND (tombstoned_at IS NULL));


--
-- Name: care_entities_p15_family_id_session_id_baby_id_kind_started_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_entities_p15_family_id_session_id_baby_id_kind_started_idx ON public.care_entities_p15 USING btree (family_id, session_id, baby_id, kind, started_at) WHERE ((tombstoned_at IS NULL) AND (session_id IS NOT NULL));


--
-- Name: care_receipts_family_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_receipts_family_idx ON public.care_receipts USING btree (family_id);


--
-- Name: care_receipts_retention_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX care_receipts_retention_idx ON public.care_receipts USING btree (accepted_at);


--
-- Name: device_sync_cursors_seen_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX device_sync_cursors_seen_idx ON public.device_sync_cursors USING btree (seen_at);


--
-- Name: devices_active_account_device_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX devices_active_account_device_idx ON public.devices USING btree (account_id, id) WHERE (status = 'active'::app.device_status);


--
-- Name: export_jobs_claim_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX export_jobs_claim_idx ON public.export_jobs USING btree (next_attempt_at, lease_until, id) WHERE (state = ANY (ARRAY['queued'::text, 'running'::text]));


--
-- Name: export_jobs_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX export_jobs_expiry_idx ON public.export_jobs USING btree (expires_at, id);


--
-- Name: export_jobs_one_active_per_family; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX export_jobs_one_active_per_family ON public.export_jobs USING btree (family_id) WHERE (state = ANY (ARRAY['queued'::text, 'running'::text, 'ready'::text]));


--
-- Name: export_rate_recent_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX export_rate_recent_idx ON public.export_rate_reservations USING btree (scope, key_id, accepted_at DESC);


--
-- Name: family_actor_label_history_high_water_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX family_actor_label_history_high_water_idx ON public.family_actor_label_history USING btree (family_id, label_version DESC);


--
-- Name: family_actor_label_history_page_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX family_actor_label_history_page_idx ON public.family_actor_label_history USING btree (family_id, account_id, label_version DESC);


--
-- Name: family_actor_labels_page_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX family_actor_labels_page_idx ON public.family_actor_labels USING btree (family_id, account_id);


--
-- Name: family_recovery_claims_one_active_per_account_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX family_recovery_claims_one_active_per_account_idx ON public.family_recovery_claims USING btree (account_id) WHERE (status = 'active'::app.claim_status);


--
-- Name: family_slots_family_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX family_slots_family_id_idx ON public.family_slots USING btree (family_id);


--
-- Name: import_baby_mappings_shared_key; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX import_baby_mappings_shared_key ON public.import_baby_mappings USING btree (family_id, import_id, shared_baby_id);


--
-- Name: import_conflicts_unresolved_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX import_conflicts_unresolved_idx ON public.import_conflicts USING btree (family_id, import_id, conflict_id) WHERE (decision_choice IS NULL);


--
-- Name: import_manifests_local_baby_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX import_manifests_local_baby_idx ON public.import_record_manifests USING btree (family_id, import_id, local_baby_id, record_id);


--
-- Name: import_sessions_account_unfinished_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX import_sessions_account_unfinished_idx ON public.import_sessions USING btree (initiating_account_id, expires_at, family_id, import_id) WHERE ((NOT mapping_complete) AND (expired_at IS NULL));


--
-- Name: import_sessions_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX import_sessions_expiry_idx ON public.import_sessions USING btree (expires_at, family_id, import_id) WHERE ((NOT mapping_complete) AND (expired_at IS NULL));


--
-- Name: import_sessions_retained_account_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX import_sessions_retained_account_idx ON public.import_sessions USING btree (family_id, initiating_account_id, import_id);


--
-- Name: invitations_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX invitations_expiry_idx ON public.invitations USING btree (expires_at) WHERE (status = 'active'::app.invitation_status);


--
-- Name: invitations_family_status_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX invitations_family_status_idx ON public.invitations USING btree (family_id, status);


--
-- Name: live_activity_delivery_due; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX live_activity_delivery_due ON public.live_activity_delivery USING btree (not_before) WHERE (dirty_version > completed_version);


--
-- Name: live_activity_device_token_owner; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX live_activity_device_token_owner ON public.live_activity_devices USING btree (environment, topic, push_token) WHERE (NOT revoked);


--
-- Name: live_activity_history_age; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX live_activity_history_age ON public.live_activity_history USING btree (recorded_at);


--
-- Name: live_activity_history_job; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX live_activity_history_job ON public.live_activity_history USING btree (job_id, recorded_at);


--
-- Name: live_activity_index_family; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX live_activity_index_family ON public.live_activity_registration_index USING btree (family_id);


--
-- Name: live_activity_update_token_owner; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX live_activity_update_token_owner ON public.live_activity_tokens USING btree (environment, topic, token) WHERE (state = 'active'::text);


--
-- Name: mcp_integrations_family_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX mcp_integrations_family_idx ON public.mcp_integrations USING btree (family_id);


--
-- Name: memberships_account_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX memberships_account_id_idx ON public.memberships USING btree (account_id);


--
-- Name: memberships_one_active_owner; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX memberships_one_active_owner ON public.memberships USING btree (family_id) WHERE ((role = 'owner'::app.member_role) AND (status = 'active'::app.member_status));


--
-- Name: refresh_receipts_device_attempt_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX refresh_receipts_device_attempt_idx ON public.refresh_receipts USING btree (account_id, device_id, attempt_id);


--
-- Name: refresh_receipts_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX refresh_receipts_expiry_idx ON public.refresh_receipts USING btree (LEAST(refresh_expires_at, effective_deadline));


--
-- Name: sessions_account_live_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions_account_live_idx ON public.sessions USING btree (account_id) WHERE (status = 'active'::app.session_status);


--
-- Name: sessions_consumed_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions_consumed_idx ON public.sessions USING btree (consumed_at) WHERE (status = 'consumed'::app.session_status);


--
-- Name: sessions_device_live_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions_device_live_idx ON public.sessions USING btree (device_id) WHERE (status = 'active'::app.session_status);


--
-- Name: sessions_expired_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions_expired_idx ON public.sessions USING btree (refresh_expires_at) WHERE (status = 'active'::app.session_status);


--
-- Name: sessions_family_live_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions_family_live_idx ON public.sessions USING btree (family_id) WHERE (status = 'active'::app.session_status);


--
-- Name: sessions_lineage_live_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions_lineage_live_idx ON public.sessions USING btree (lineage_root_id) WHERE (status = 'active'::app.session_status);


--
-- Name: sessions_revoked_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions_revoked_idx ON public.sessions USING btree (revoked_at) WHERE (status = 'revoked'::app.session_status);


--
-- Name: sync_page_bindings_expiry_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sync_page_bindings_expiry_idx ON public.sync_page_bindings USING btree (expires_at);


--
-- Name: sync_snapshot_sessions_account_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sync_snapshot_sessions_account_idx ON public.sync_snapshot_sessions USING btree (account_id, expires_at);


--
-- Name: sync_snapshot_sessions_family_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sync_snapshot_sessions_family_idx ON public.sync_snapshot_sessions USING btree (family_id, expires_at);


--
-- Name: care_entities_p00_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p00_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p00_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p00_family_id_baby_id_idx;


--
-- Name: care_entities_p00_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p00_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p00_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p00_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p00_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p00_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p00_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p00_family_id_changed_seq_key;


--
-- Name: care_entities_p00_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p00_family_id_kind_id_idx;


--
-- Name: care_entities_p00_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p00_family_id_name_hash_idx;


--
-- Name: care_entities_p00_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p00_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p00_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p00_pkey;


--
-- Name: care_entities_p01_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p01_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p01_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p01_family_id_baby_id_idx;


--
-- Name: care_entities_p01_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p01_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p01_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p01_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p01_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p01_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p01_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p01_family_id_changed_seq_key;


--
-- Name: care_entities_p01_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p01_family_id_kind_id_idx;


--
-- Name: care_entities_p01_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p01_family_id_name_hash_idx;


--
-- Name: care_entities_p01_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p01_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p01_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p01_pkey;


--
-- Name: care_entities_p02_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p02_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p02_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p02_family_id_baby_id_idx;


--
-- Name: care_entities_p02_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p02_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p02_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p02_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p02_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p02_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p02_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p02_family_id_changed_seq_key;


--
-- Name: care_entities_p02_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p02_family_id_kind_id_idx;


--
-- Name: care_entities_p02_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p02_family_id_name_hash_idx;


--
-- Name: care_entities_p02_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p02_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p02_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p02_pkey;


--
-- Name: care_entities_p03_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p03_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p03_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p03_family_id_baby_id_idx;


--
-- Name: care_entities_p03_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p03_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p03_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p03_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p03_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p03_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p03_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p03_family_id_changed_seq_key;


--
-- Name: care_entities_p03_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p03_family_id_kind_id_idx;


--
-- Name: care_entities_p03_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p03_family_id_name_hash_idx;


--
-- Name: care_entities_p03_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p03_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p03_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p03_pkey;


--
-- Name: care_entities_p04_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p04_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p04_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p04_family_id_baby_id_idx;


--
-- Name: care_entities_p04_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p04_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p04_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p04_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p04_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p04_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p04_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p04_family_id_changed_seq_key;


--
-- Name: care_entities_p04_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p04_family_id_kind_id_idx;


--
-- Name: care_entities_p04_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p04_family_id_name_hash_idx;


--
-- Name: care_entities_p04_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p04_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p04_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p04_pkey;


--
-- Name: care_entities_p05_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p05_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p05_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p05_family_id_baby_id_idx;


--
-- Name: care_entities_p05_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p05_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p05_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p05_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p05_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p05_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p05_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p05_family_id_changed_seq_key;


--
-- Name: care_entities_p05_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p05_family_id_kind_id_idx;


--
-- Name: care_entities_p05_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p05_family_id_name_hash_idx;


--
-- Name: care_entities_p05_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p05_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p05_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p05_pkey;


--
-- Name: care_entities_p06_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p06_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p06_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p06_family_id_baby_id_idx;


--
-- Name: care_entities_p06_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p06_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p06_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p06_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p06_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p06_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p06_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p06_family_id_changed_seq_key;


--
-- Name: care_entities_p06_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p06_family_id_kind_id_idx;


--
-- Name: care_entities_p06_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p06_family_id_name_hash_idx;


--
-- Name: care_entities_p06_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p06_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p06_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p06_pkey;


--
-- Name: care_entities_p07_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p07_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p07_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p07_family_id_baby_id_idx;


--
-- Name: care_entities_p07_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p07_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p07_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p07_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p07_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p07_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p07_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p07_family_id_changed_seq_key;


--
-- Name: care_entities_p07_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p07_family_id_kind_id_idx;


--
-- Name: care_entities_p07_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p07_family_id_name_hash_idx;


--
-- Name: care_entities_p07_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p07_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p07_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p07_pkey;


--
-- Name: care_entities_p08_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p08_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p08_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p08_family_id_baby_id_idx;


--
-- Name: care_entities_p08_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p08_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p08_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p08_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p08_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p08_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p08_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p08_family_id_changed_seq_key;


--
-- Name: care_entities_p08_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p08_family_id_kind_id_idx;


--
-- Name: care_entities_p08_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p08_family_id_name_hash_idx;


--
-- Name: care_entities_p08_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p08_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p08_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p08_pkey;


--
-- Name: care_entities_p09_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p09_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p09_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p09_family_id_baby_id_idx;


--
-- Name: care_entities_p09_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p09_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p09_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p09_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p09_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p09_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p09_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p09_family_id_changed_seq_key;


--
-- Name: care_entities_p09_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p09_family_id_kind_id_idx;


--
-- Name: care_entities_p09_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p09_family_id_name_hash_idx;


--
-- Name: care_entities_p09_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p09_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p09_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p09_pkey;


--
-- Name: care_entities_p10_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p10_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p10_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p10_family_id_baby_id_idx;


--
-- Name: care_entities_p10_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p10_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p10_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p10_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p10_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p10_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p10_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p10_family_id_changed_seq_key;


--
-- Name: care_entities_p10_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p10_family_id_kind_id_idx;


--
-- Name: care_entities_p10_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p10_family_id_name_hash_idx;


--
-- Name: care_entities_p10_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p10_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p10_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p10_pkey;


--
-- Name: care_entities_p11_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p11_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p11_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p11_family_id_baby_id_idx;


--
-- Name: care_entities_p11_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p11_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p11_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p11_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p11_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p11_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p11_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p11_family_id_changed_seq_key;


--
-- Name: care_entities_p11_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p11_family_id_kind_id_idx;


--
-- Name: care_entities_p11_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p11_family_id_name_hash_idx;


--
-- Name: care_entities_p11_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p11_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p11_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p11_pkey;


--
-- Name: care_entities_p12_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p12_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p12_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p12_family_id_baby_id_idx;


--
-- Name: care_entities_p12_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p12_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p12_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p12_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p12_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p12_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p12_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p12_family_id_changed_seq_key;


--
-- Name: care_entities_p12_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p12_family_id_kind_id_idx;


--
-- Name: care_entities_p12_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p12_family_id_name_hash_idx;


--
-- Name: care_entities_p12_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p12_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p12_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p12_pkey;


--
-- Name: care_entities_p13_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p13_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p13_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p13_family_id_baby_id_idx;


--
-- Name: care_entities_p13_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p13_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p13_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p13_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p13_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p13_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p13_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p13_family_id_changed_seq_key;


--
-- Name: care_entities_p13_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p13_family_id_kind_id_idx;


--
-- Name: care_entities_p13_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p13_family_id_name_hash_idx;


--
-- Name: care_entities_p13_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p13_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p13_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p13_pkey;


--
-- Name: care_entities_p14_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p14_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p14_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p14_family_id_baby_id_idx;


--
-- Name: care_entities_p14_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p14_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p14_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p14_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p14_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p14_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p14_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p14_family_id_changed_seq_key;


--
-- Name: care_entities_p14_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p14_family_id_kind_id_idx;


--
-- Name: care_entities_p14_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p14_family_id_name_hash_idx;


--
-- Name: care_entities_p14_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p14_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p14_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p14_pkey;


--
-- Name: care_entities_p15_family_id_baby_id_coalesce_coalesce1_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_milk_idx ATTACH PARTITION public.care_entities_p15_family_id_baby_id_coalesce_coalesce1_id_idx;


--
-- Name: care_entities_p15_family_id_baby_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_growth_profile ATTACH PARTITION public.care_entities_p15_family_id_baby_id_idx;


--
-- Name: care_entities_p15_family_id_baby_id_kind_started_at_id_ende_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_history_idx ATTACH PARTITION public.care_entities_p15_family_id_baby_id_kind_started_at_id_ende_idx;


--
-- Name: care_entities_p15_family_id_baby_id_started_at_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_latest_breast_idx ATTACH PARTITION public.care_entities_p15_family_id_baby_id_started_at_id_idx;


--
-- Name: care_entities_p15_family_id_baby_id_timer_kind_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_one_live_timer ATTACH PARTITION public.care_entities_p15_family_id_baby_id_timer_kind_idx;


--
-- Name: care_entities_p15_family_id_changed_seq_key; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_seq_key ATTACH PARTITION public.care_entities_p15_family_id_changed_seq_key;


--
-- Name: care_entities_p15_family_id_kind_id_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_snapshot_idx ATTACH PARTITION public.care_entities_p15_family_id_kind_id_idx;


--
-- Name: care_entities_p15_family_id_name_hash_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_baby_match_idx ATTACH PARTITION public.care_entities_p15_family_id_name_hash_idx;


--
-- Name: care_entities_p15_family_id_session_id_baby_id_kind_started_idx; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_session_idx ATTACH PARTITION public.care_entities_p15_family_id_session_id_baby_id_kind_started_idx;


--
-- Name: care_entities_p15_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.care_entities_pkey ATTACH PARTITION public.care_entities_p15_pkey;


--
-- Name: security_audit_events_2026_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.security_audit_events_pkey ATTACH PARTITION public.security_audit_events_2026_pkey;


--
-- Name: security_audit_events_default_pkey; Type: INDEX ATTACH; Schema: public; Owner: -
--

ALTER INDEX public.security_audit_events_pkey ATTACH PARTITION public.security_audit_events_default_pkey;


--
-- Name: accounts accounts_audit_chain_forward; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER accounts_audit_chain_forward BEFORE UPDATE OF audit_seq, audit_hash, chain_failed ON public.accounts FOR EACH ROW EXECUTE FUNCTION app.reject_audit_chain_regression();


--
-- Name: accounts accounts_authority_columns; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER accounts_authority_columns BEFORE UPDATE OF status, lifecycle_generation, audit_seq, audit_hash, chain_failed ON public.accounts FOR EACH ROW WHEN ((NOT app.authority_context())) EXECUTE FUNCTION app.reject_tenant_authority_write();


--
-- Name: families families_audit_chain_forward; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER families_audit_chain_forward BEFORE UPDATE OF audit_seq, audit_hash, chain_failed ON public.families FOR EACH ROW EXECUTE FUNCTION app.reject_audit_chain_regression();


--
-- Name: families families_authority_columns; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER families_authority_columns BEFORE UPDATE OF status, owner_account_id, display_name, profile_required, purge_after, purge_started_at, purge_command_id, purge_receipt, purged_at, audit_seq, audit_hash, chain_failed, entitlement_required ON public.families FOR EACH ROW WHEN ((NOT app.authority_context())) EXECUTE FUNCTION app.reject_tenant_authority_write();


--
-- Name: live_activity_devices live_activity_device_dirty; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER live_activity_device_dirty AFTER INSERT OR UPDATE ON public.live_activity_devices FOR EACH ROW EXECUTE FUNCTION app.live_activity_registration_dirty();


--
-- Name: live_activity_devices live_activity_device_retire_token; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER live_activity_device_retire_token BEFORE UPDATE ON public.live_activity_devices FOR EACH ROW EXECUTE FUNCTION app.live_activity_token_fingerprint();


--
-- Name: live_activity_devices live_activity_device_token_owner; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER live_activity_device_token_owner AFTER INSERT OR UPDATE ON public.live_activity_devices FOR EACH ROW EXECUTE FUNCTION app.live_activity_token_ownership();


--
-- Name: live_activity_tokens live_activity_token_dirty; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER live_activity_token_dirty AFTER INSERT OR UPDATE ON public.live_activity_tokens FOR EACH ROW EXECUTE FUNCTION app.live_activity_registration_dirty();


--
-- Name: live_activity_tokens live_activity_token_fingerprint; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER live_activity_token_fingerprint BEFORE INSERT OR UPDATE ON public.live_activity_tokens FOR EACH ROW EXECUTE FUNCTION app.live_activity_token_fingerprint();


--
-- Name: live_activity_tokens live_activity_update_token_owner; Type: TRIGGER; Schema: public; Owner: -
--

CREATE TRIGGER live_activity_update_token_owner AFTER INSERT OR UPDATE ON public.live_activity_tokens FOR EACH ROW EXECUTE FUNCTION app.live_activity_token_ownership();


--
-- Name: memberships memberships_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memberships
    ADD CONSTRAINT memberships_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id) ON DELETE CASCADE;


--
-- Name: memberships memberships_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.memberships
    ADD CONSTRAINT memberships_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id);


--
-- Name: devices devices_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.devices
    ADD CONSTRAINT devices_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id);


--
-- Name: family_slots family_slots_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.family_slots
    ADD CONSTRAINT family_slots_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id);


--
-- Name: family_slots family_slots_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.family_slots
    ADD CONSTRAINT family_slots_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id);


--
-- Name: invitations invitations_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.invitations
    ADD CONSTRAINT invitations_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id);


--
-- Name: invitations invitations_creator_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.invitations
    ADD CONSTRAINT invitations_creator_account_id_fkey FOREIGN KEY (creator_account_id) REFERENCES public.accounts(id);


--
-- Name: family_entitlements family_entitlements_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.family_entitlements
    ADD CONSTRAINT family_entitlements_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id);


--
-- Name: family_actor_labels family_actor_labels_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.family_actor_labels
    ADD CONSTRAINT family_actor_labels_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id);


--
-- Name: account_profile_commands account_profile_commands_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_profile_commands
    ADD CONSTRAINT account_profile_commands_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id);


--
-- Name: family_actor_label_history family_actor_label_history_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.family_actor_label_history
    ADD CONSTRAINT family_actor_label_history_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id);


--
-- Name: family_recovery_claims family_recovery_claims_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.family_recovery_claims
    ADD CONSTRAINT family_recovery_claims_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id);


--
-- Name: family_recovery_claims family_recovery_claims_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.family_recovery_claims
    ADD CONSTRAINT family_recovery_claims_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id);


--
-- Name: import_sessions import_sessions_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_sessions
    ADD CONSTRAINT import_sessions_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id) ON DELETE CASCADE;


--
-- Name: import_chunks import_chunks_session_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_chunks
    ADD CONSTRAINT import_chunks_session_fk FOREIGN KEY (family_id, import_id) REFERENCES public.import_sessions(family_id, import_id) ON DELETE CASCADE;


--
-- Name: import_baby_candidates import_candidates_session_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_baby_candidates
    ADD CONSTRAINT import_candidates_session_fk FOREIGN KEY (family_id, import_id) REFERENCES public.import_sessions(family_id, import_id) ON DELETE CASCADE;


--
-- Name: import_record_manifests import_manifests_session_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_record_manifests
    ADD CONSTRAINT import_manifests_session_fk FOREIGN KEY (family_id, import_id) REFERENCES public.import_sessions(family_id, import_id) ON DELETE CASCADE;


--
-- Name: import_record_manifests import_manifests_candidate_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_record_manifests
    ADD CONSTRAINT import_manifests_candidate_fk FOREIGN KEY (family_id, import_id, local_baby_id) REFERENCES public.import_baby_candidates(family_id, import_id, local_baby_id);


--
-- Name: import_record_manifests import_manifests_chunk_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_record_manifests
    ADD CONSTRAINT import_manifests_chunk_fk FOREIGN KEY (family_id, import_id, chunk_index) REFERENCES public.import_chunks(family_id, import_id, chunk_index);


--
-- Name: import_conflicts import_conflicts_session_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_conflicts
    ADD CONSTRAINT import_conflicts_session_fk FOREIGN KEY (family_id, import_id) REFERENCES public.import_sessions(family_id, import_id) ON DELETE CASCADE;


--
-- Name: import_conflicts import_conflicts_candidate_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_conflicts
    ADD CONSTRAINT import_conflicts_candidate_fk FOREIGN KEY (family_id, import_id, local_baby_id) REFERENCES public.import_baby_candidates(family_id, import_id, local_baby_id);


--
-- Name: import_conflicts import_conflicts_manifest_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_conflicts
    ADD CONSTRAINT import_conflicts_manifest_fk FOREIGN KEY (family_id, import_id, record_id) REFERENCES public.import_record_manifests(family_id, import_id, record_id);


--
-- Name: import_conflict_receipts import_conflict_receipts_session_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_conflict_receipts
    ADD CONSTRAINT import_conflict_receipts_session_fk FOREIGN KEY (family_id, import_id) REFERENCES public.import_sessions(family_id, import_id) ON DELETE CASCADE;


--
-- Name: import_baby_mappings import_mappings_candidate_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_baby_mappings
    ADD CONSTRAINT import_mappings_candidate_fk FOREIGN KEY (family_id, import_id, local_baby_id) REFERENCES public.import_baby_candidates(family_id, import_id, local_baby_id);


--
-- Name: import_baby_mappings import_mappings_session_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_baby_mappings
    ADD CONSTRAINT import_mappings_session_fk FOREIGN KEY (family_id, import_id) REFERENCES public.import_sessions(family_id, import_id) ON DELETE CASCADE;


--
-- Name: export_jobs export_jobs_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.export_jobs
    ADD CONSTRAINT export_jobs_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id) ON DELETE CASCADE;


--
-- Name: export_jobs export_jobs_requester_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.export_jobs
    ADD CONSTRAINT export_jobs_requester_account_id_fkey FOREIGN KEY (requester_account_id) REFERENCES public.accounts(id);


--
-- Name: export_chunks export_chunks_export_id_attempt_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.export_chunks
    ADD CONSTRAINT export_chunks_export_id_attempt_fkey FOREIGN KEY (export_id, attempt) REFERENCES public.export_jobs(id, attempt) ON DELETE CASCADE;


--
-- Name: export_chunks export_chunks_export_id_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.export_chunks
    ADD CONSTRAINT export_chunks_export_id_family_id_fkey FOREIGN KEY (export_id, family_id) REFERENCES public.export_jobs(id, family_id) ON DELETE CASCADE;


--
-- Name: account_identities account_identities_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.account_identities
    ADD CONSTRAINT account_identities_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id);


--
-- Name: sessions sessions_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id);


--
-- Name: sessions sessions_device_id_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_device_id_account_id_fkey FOREIGN KEY (device_id, account_id) REFERENCES public.devices(id, account_id);


--
-- Name: refresh_receipts refresh_receipts_parent_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.refresh_receipts
    ADD CONSTRAINT refresh_receipts_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES public.sessions(id) ON DELETE CASCADE;


--
-- Name: refresh_receipts refresh_receipts_child_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.refresh_receipts
    ADD CONSTRAINT refresh_receipts_child_id_fkey FOREIGN KEY (child_id) REFERENCES public.sessions(id) ON DELETE CASCADE;


--
-- Name: families families_owner_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.families
    ADD CONSTRAINT families_owner_account_id_fkey FOREIGN KEY (owner_account_id) REFERENCES public.accounts(id);


--
-- Name: mcp_integrations mcp_integrations_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mcp_integrations
    ADD CONSTRAINT mcp_integrations_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id);


--
-- Name: mcp_integrations mcp_integrations_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.mcp_integrations
    ADD CONSTRAINT mcp_integrations_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id);


--
-- Name: care_entities care_entities_baby_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.care_entities
    ADD CONSTRAINT care_entities_baby_fk FOREIGN KEY (family_id, baby_id) REFERENCES public.care_entities(family_id, id);


--
-- Name: care_entities care_entities_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE public.care_entities
    ADD CONSTRAINT care_entities_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id) ON DELETE CASCADE;


--
-- Name: import_baby_mappings import_mappings_shared_baby_fk; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.import_baby_mappings
    ADD CONSTRAINT import_mappings_shared_baby_fk FOREIGN KEY (family_id, shared_baby_id) REFERENCES public.care_entities(family_id, id);


--
-- Name: care_receipts care_receipts_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.care_receipts
    ADD CONSTRAINT care_receipts_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id) ON DELETE CASCADE;


--
-- Name: sync_page_bindings sync_page_bindings_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sync_page_bindings
    ADD CONSTRAINT sync_page_bindings_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id) ON DELETE CASCADE;


--
-- Name: sync_snapshot_sessions sync_snapshot_sessions_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sync_snapshot_sessions
    ADD CONSTRAINT sync_snapshot_sessions_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id) ON DELETE CASCADE;


--
-- Name: device_sync_cursors device_sync_cursors_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.device_sync_cursors
    ADD CONSTRAINT device_sync_cursors_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id) ON DELETE CASCADE;


--
-- Name: device_push_tokens device_push_tokens_device_id_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.device_push_tokens
    ADD CONSTRAINT device_push_tokens_device_id_account_id_fkey FOREIGN KEY (device_id, account_id) REFERENCES public.devices(id, account_id) ON DELETE CASCADE;


--
-- Name: live_activity_devices live_activity_devices_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_devices
    ADD CONSTRAINT live_activity_devices_account_id_fkey FOREIGN KEY (account_id) REFERENCES public.accounts(id);


--
-- Name: live_activity_devices live_activity_devices_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_devices
    ADD CONSTRAINT live_activity_devices_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id) ON DELETE CASCADE;


--
-- Name: live_activity_devices live_activity_devices_device_id_account_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_devices
    ADD CONSTRAINT live_activity_devices_device_id_account_id_fkey FOREIGN KEY (device_id, account_id) REFERENCES public.devices(id, account_id);


--
-- Name: live_activity_tokens live_activity_tokens_registration_id_account_id_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_tokens
    ADD CONSTRAINT live_activity_tokens_registration_id_account_id_family_id_fkey FOREIGN KEY (registration_id, account_id, family_id) REFERENCES public.live_activity_devices(registration_id, account_id, family_id) ON DELETE CASCADE;


--
-- Name: live_activity_token_owners live_activity_token_owners_registration_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_token_owners
    ADD CONSTRAINT live_activity_token_owners_registration_id_fkey FOREIGN KEY (registration_id) REFERENCES public.live_activity_devices(registration_id) ON DELETE CASCADE;


--
-- Name: live_activity_registration_index live_activity_registration_index_registration_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_registration_index
    ADD CONSTRAINT live_activity_registration_index_registration_id_fkey FOREIGN KEY (registration_id) REFERENCES public.live_activity_devices(registration_id) ON DELETE CASCADE;


--
-- Name: live_activity_family_dirty live_activity_family_dirty_family_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_family_dirty
    ADD CONSTRAINT live_activity_family_dirty_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.families(id) ON DELETE CASCADE;


--
-- Name: live_activity_delivery live_activity_delivery_registration_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.live_activity_delivery
    ADD CONSTRAINT live_activity_delivery_registration_id_fkey FOREIGN KEY (registration_id) REFERENCES public.live_activity_registration_index(registration_id) ON DELETE CASCADE;


--
-- Name: accounts account_authority; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY account_authority ON public.accounts USING (app.authority_context()) WITH CHECK (app.authority_context());


--
-- Name: accounts account_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY account_context ON public.accounts FOR SELECT USING ((app.authority_context() OR (id = app.current_account_id())));


--
-- Name: import_sessions account_count_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY account_count_context ON public.import_sessions FOR SELECT USING (((CURRENT_USER = pg_get_userbyid(( SELECT p.proowner
   FROM pg_proc p
  WHERE (p.oid = ('app.import_unfinished_session_count(uuid,timestamp with time zone)'::regprocedure)::oid)))) AND (initiating_account_id = app.current_account_id())));


--
-- Name: account_identities; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.account_identities ENABLE ROW LEVEL SECURITY;

--
-- Name: account_profile_commands; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.account_profile_commands ENABLE ROW LEVEL SECURITY;

--
-- Name: accounts account_tenant_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY account_tenant_update ON public.accounts FOR UPDATE USING ((app.authority_context() OR (id = app.current_account_id()))) WITH CHECK ((app.authority_context() OR (id = app.current_account_id())));


--
-- Name: accounts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.accounts ENABLE ROW LEVEL SECURITY;

--
-- Name: family_actor_labels actor_label_family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY actor_label_family_context ON public.family_actor_labels USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: family_actor_label_history actor_label_history_family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY actor_label_history_family_context ON public.family_actor_label_history USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: apple_notifications; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.apple_notifications ENABLE ROW LEVEL SECURITY;

--
-- Name: audit_checkpoints; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.audit_checkpoints ENABLE ROW LEVEL SECURITY;

--
-- Name: auth_challenges; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.auth_challenges ENABLE ROW LEVEL SECURITY;

--
-- Name: account_identities authority_only; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY authority_only ON public.account_identities USING (app.authority_context()) WITH CHECK (app.authority_context());


--
-- Name: apple_notifications authority_only; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY authority_only ON public.apple_notifications USING (app.authority_context()) WITH CHECK (app.authority_context());


--
-- Name: audit_checkpoints authority_only; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY authority_only ON public.audit_checkpoints USING (app.authority_context()) WITH CHECK (app.authority_context());


--
-- Name: auth_challenges authority_only; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY authority_only ON public.auth_challenges USING (app.authority_context()) WITH CHECK (app.authority_context());


--
-- Name: command_receipts authority_only; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY authority_only ON public.command_receipts USING (app.authority_context()) WITH CHECK (app.authority_context());


--
-- Name: mcp_integrations authority_only; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY authority_only ON public.mcp_integrations USING (app.authority_context()) WITH CHECK (app.authority_context());


--
-- Name: refresh_receipts authority_only; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY authority_only ON public.refresh_receipts USING (app.authority_context()) WITH CHECK (app.authority_context());


--
-- Name: security_audit_events authority_only; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY authority_only ON public.security_audit_events USING (app.authority_context()) WITH CHECK (app.authority_context());


--
-- Name: sessions authority_only; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY authority_only ON public.sessions USING (app.authority_context()) WITH CHECK (app.authority_context());


--
-- Name: care_entities; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p00; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p00 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p01; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p01 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p02; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p02 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p03; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p03 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p04; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p04 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p05; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p05 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p06; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p06 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p07; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p07 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p08; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p08 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p09; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p09 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p10; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p10 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p11; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p11 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p12; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p12 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p13; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p13 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p14; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p14 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_entities_p15; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_entities_p15 ENABLE ROW LEVEL SECURITY;

--
-- Name: care_receipts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.care_receipts ENABLE ROW LEVEL SECURITY;

--
-- Name: care_receipts care_receipts_read; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY care_receipts_read ON public.care_receipts FOR SELECT USING (((family_id = app.current_family_id()) OR (account_id = app.current_account_id())));


--
-- Name: care_receipts care_receipts_write; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY care_receipts_write ON public.care_receipts FOR INSERT WITH CHECK (((family_id = app.current_family_id()) AND (account_id = app.current_account_id())));


--
-- Name: family_recovery_claims claim_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY claim_context ON public.family_recovery_claims USING ((app.authority_context() OR (account_id = app.current_account_id()))) WITH CHECK (app.authority_context());


--
-- Name: command_receipts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.command_receipts ENABLE ROW LEVEL SECURITY;

--
-- Name: devices device_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY device_context ON public.devices USING ((app.authority_context() OR (account_id = app.current_account_id()))) WITH CHECK (app.authority_context());


--
-- Name: device_push_tokens; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.device_push_tokens ENABLE ROW LEVEL SECURITY;

--
-- Name: device_sync_cursors; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.device_sync_cursors ENABLE ROW LEVEL SECURITY;

--
-- Name: devices; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.devices ENABLE ROW LEVEL SECURITY;

--
-- Name: family_entitlements entitlement_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY entitlement_context ON public.family_entitlements USING ((app.authority_context() OR (family_id = app.current_family_id()))) WITH CHECK (app.authority_context());


--
-- Name: export_chunks; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.export_chunks ENABLE ROW LEVEL SECURITY;

--
-- Name: export_jobs; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.export_jobs ENABLE ROW LEVEL SECURITY;

--
-- Name: export_rate_reservations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.export_rate_reservations ENABLE ROW LEVEL SECURITY;

--
-- Name: export_chunks export_requester; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY export_requester ON public.export_chunks TO baby_tracker_runtime USING (((family_id = app.current_family_id()) AND (EXISTS ( SELECT 1
   FROM public.export_jobs j
  WHERE ((j.id = export_chunks.export_id) AND (j.requester_account_id = app.current_account_id())))))) WITH CHECK (false);


--
-- Name: export_jobs export_requester; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY export_requester ON public.export_jobs TO baby_tracker_runtime USING (((family_id = app.current_family_id()) AND (requester_account_id = app.current_account_id()))) WITH CHECK (((family_id = app.current_family_id()) AND (requester_account_id = app.current_account_id())));


--
-- Name: export_rate_reservations export_requester; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY export_requester ON public.export_rate_reservations TO baby_tracker_runtime USING ((((scope = 'account'::text) AND (key_id = app.current_account_id())) OR ((scope = 'family'::text) AND (key_id = app.current_family_id())))) WITH CHECK ((((scope = 'account'::text) AND (key_id = app.current_account_id())) OR ((scope = 'family'::text) AND (key_id = app.current_family_id()))));


--
-- Name: export_chunks export_worker; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY export_worker ON public.export_chunks TO baby_tracker_maintenance USING (true) WITH CHECK (true);


--
-- Name: export_jobs export_worker; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY export_worker ON public.export_jobs TO baby_tracker_maintenance USING (true) WITH CHECK (true);


--
-- Name: export_rate_reservations export_worker; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY export_worker ON public.export_rate_reservations TO baby_tracker_maintenance USING (true) WITH CHECK (true);


--
-- Name: families; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.families ENABLE ROW LEVEL SECURITY;

--
-- Name: family_actor_label_history; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.family_actor_label_history ENABLE ROW LEVEL SECURITY;

--
-- Name: family_actor_labels; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.family_actor_labels ENABLE ROW LEVEL SECURITY;

--
-- Name: families family_authority; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_authority ON public.families USING (app.authority_context()) WITH CHECK (app.authority_context());


--
-- Name: care_entities family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p00 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p00 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p01 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p01 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p02 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p02 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p03 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p03 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p04 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p04 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p05 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p05 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p06 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p06 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p07 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p07 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p08 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p08 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p09 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p09 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p10 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p10 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p11 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p11 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p12 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p12 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p13 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p13 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p14 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p14 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: care_entities_p15 family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.care_entities_p15 USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: families family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.families FOR SELECT USING ((app.authority_context() OR (id = app.current_family_id())));


--
-- Name: import_baby_candidates family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.import_baby_candidates USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: import_baby_mappings family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.import_baby_mappings USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: import_chunks family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.import_chunks USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: import_conflict_receipts family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.import_conflict_receipts USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: import_conflicts family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.import_conflicts USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: import_record_manifests family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.import_record_manifests USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: import_sessions family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.import_sessions USING ((family_id = app.current_family_id())) WITH CHECK ((family_id = app.current_family_id()));


--
-- Name: sync_page_bindings family_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_context ON public.sync_page_bindings USING (((family_id = app.current_family_id()) AND (account_id = app.current_account_id()))) WITH CHECK (((family_id = app.current_family_id()) AND (account_id = app.current_account_id())));


--
-- Name: family_entitlements; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.family_entitlements ENABLE ROW LEVEL SECURITY;

--
-- Name: family_recovery_claims; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.family_recovery_claims ENABLE ROW LEVEL SECURITY;

--
-- Name: family_slots family_slot_authority; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_slot_authority ON public.family_slots USING (app.authority_context()) WITH CHECK (app.authority_context());


--
-- Name: family_slots; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.family_slots ENABLE ROW LEVEL SECURITY;

--
-- Name: families family_tenant_update; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY family_tenant_update ON public.families FOR UPDATE USING ((app.authority_context() OR (id = app.current_family_id()))) WITH CHECK ((app.authority_context() OR (id = app.current_family_id())));


--
-- Name: import_baby_candidates; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.import_baby_candidates ENABLE ROW LEVEL SECURITY;

--
-- Name: import_baby_mappings; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.import_baby_mappings ENABLE ROW LEVEL SECURITY;

--
-- Name: import_chunks; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.import_chunks ENABLE ROW LEVEL SECURITY;

--
-- Name: import_conflict_receipts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.import_conflict_receipts ENABLE ROW LEVEL SECURITY;

--
-- Name: import_conflicts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.import_conflicts ENABLE ROW LEVEL SECURITY;

--
-- Name: import_record_manifests; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.import_record_manifests ENABLE ROW LEVEL SECURITY;

--
-- Name: import_sessions; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.import_sessions ENABLE ROW LEVEL SECURITY;

--
-- Name: invitations invitation_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY invitation_context ON public.invitations USING ((app.authority_context() OR (family_id = app.current_family_id()))) WITH CHECK (app.authority_context());


--
-- Name: invitations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.invitations ENABLE ROW LEVEL SECURITY;

--
-- Name: live_activity_delivery; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.live_activity_delivery ENABLE ROW LEVEL SECURITY;

--
-- Name: live_activity_devices; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.live_activity_devices ENABLE ROW LEVEL SECURITY;

--
-- Name: live_activity_family_dirty; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.live_activity_family_dirty ENABLE ROW LEVEL SECURITY;

--
-- Name: live_activity_history; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.live_activity_history ENABLE ROW LEVEL SECURITY;

--
-- Name: live_activity_installations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.live_activity_installations ENABLE ROW LEVEL SECURITY;

--
-- Name: live_activity_registration_index; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.live_activity_registration_index ENABLE ROW LEVEL SECURITY;

--
-- Name: live_activity_token_owners; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.live_activity_token_owners ENABLE ROW LEVEL SECURITY;

--
-- Name: live_activity_tokens; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.live_activity_tokens ENABLE ROW LEVEL SECURITY;

--
-- Name: families maintenance_counters; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY maintenance_counters ON public.families FOR UPDATE TO baby_tracker_maintenance USING (true) WITH CHECK (true);


--
-- Name: care_receipts maintenance_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY maintenance_delete ON public.care_receipts FOR DELETE TO baby_tracker_maintenance USING (true);


--
-- Name: sync_page_bindings maintenance_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY maintenance_delete ON public.sync_page_bindings FOR DELETE TO baby_tracker_maintenance USING (true);


--
-- Name: sync_snapshot_sessions maintenance_delete; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY maintenance_delete ON public.sync_snapshot_sessions FOR DELETE TO baby_tracker_maintenance USING (true);


--
-- Name: care_entities maintenance_read; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY maintenance_read ON public.care_entities FOR SELECT TO baby_tracker_maintenance USING (true);


--
-- Name: care_receipts maintenance_read; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY maintenance_read ON public.care_receipts FOR SELECT TO baby_tracker_maintenance USING (true);


--
-- Name: families maintenance_read; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY maintenance_read ON public.families FOR SELECT TO baby_tracker_maintenance USING (true);


--
-- Name: import_sessions maintenance_read; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY maintenance_read ON public.import_sessions FOR SELECT TO baby_tracker_maintenance USING (true);


--
-- Name: sync_page_bindings maintenance_read; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY maintenance_read ON public.sync_page_bindings FOR SELECT TO baby_tracker_maintenance USING (true);


--
-- Name: sync_snapshot_sessions maintenance_read; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY maintenance_read ON public.sync_snapshot_sessions FOR SELECT TO baby_tracker_maintenance USING (true);


--
-- Name: mcp_integrations; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.mcp_integrations ENABLE ROW LEVEL SECURITY;

--
-- Name: memberships membership_context; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY membership_context ON public.memberships USING ((app.authority_context() OR (family_id = app.current_family_id()))) WITH CHECK (app.authority_context());


--
-- Name: memberships; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.memberships ENABLE ROW LEVEL SECURITY;

--
-- Name: account_profile_commands own_account_profile_command; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY own_account_profile_command ON public.account_profile_commands USING ((account_id = app.current_account_id())) WITH CHECK ((account_id = app.current_account_id()));


--
-- Name: family_slots own_family_slot; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY own_family_slot ON public.family_slots FOR SELECT USING ((app.authority_context() OR (account_id = app.current_account_id())));


--
-- Name: device_push_tokens push_token_authority; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY push_token_authority ON public.device_push_tokens TO CURRENT_USER USING (app.authority_context()) WITH CHECK (app.authority_context());


--
-- Name: device_push_tokens push_token_owner; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY push_token_owner ON public.device_push_tokens USING ((account_id = app.current_account_id())) WITH CHECK ((account_id = app.current_account_id()));


--
-- Name: refresh_receipts; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.refresh_receipts ENABLE ROW LEVEL SECURITY;

--
-- Name: security_audit_events; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.security_audit_events ENABLE ROW LEVEL SECURITY;

--
-- Name: sessions; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.sessions ENABLE ROW LEVEL SECURITY;

--
-- Name: sync_snapshot_sessions snapshot_admission_read; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY snapshot_admission_read ON public.sync_snapshot_sessions FOR SELECT USING (((family_id = app.current_family_id()) OR (account_id = app.current_account_id())));


--
-- Name: sync_snapshot_sessions snapshot_write; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY snapshot_write ON public.sync_snapshot_sessions FOR INSERT WITH CHECK (((family_id = app.current_family_id()) AND (account_id = app.current_account_id())));


--
-- Name: sync_page_bindings; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.sync_page_bindings ENABLE ROW LEVEL SECURITY;

--
-- Name: sync_snapshot_sessions; Type: ROW SECURITY; Schema: public; Owner: -
--

ALTER TABLE public.sync_snapshot_sessions ENABLE ROW LEVEL SECURITY;

--
-- Name: live_activity_devices tenant_registration; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY tenant_registration ON public.live_activity_devices USING (((account_id = app.current_account_id()) AND (family_id = app.current_family_id()))) WITH CHECK (((account_id = app.current_account_id()) AND (family_id = app.current_family_id())));


--
-- Name: live_activity_tokens tenant_registration; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY tenant_registration ON public.live_activity_tokens USING (((account_id = app.current_account_id()) AND (family_id = app.current_family_id()))) WITH CHECK (((account_id = app.current_account_id()) AND (family_id = app.current_family_id())));


--
-- Name: device_sync_cursors wake_worker; Type: POLICY; Schema: public; Owner: -
--

CREATE POLICY wake_worker ON public.device_sync_cursors TO baby_tracker_maintenance USING (true) WITH CHECK (true);


--
-- Name: SCHEMA app; Type: ACL; Schema: -; Owner: -
--

GRANT USAGE ON SCHEMA app TO baby_tracker_runtime;
GRANT USAGE ON SCHEMA app TO baby_tracker_maintenance;


--
-- Name: SCHEMA public; Type: ACL; Schema: -; Owner: -
--

GRANT USAGE ON SCHEMA public TO baby_tracker_runtime;
GRANT USAGE ON SCHEMA public TO baby_tracker_maintenance;


--
-- Name: FUNCTION abandon_live_activity_start(p_claim jsonb, p_generation text, p_attempt text, p_absence_allowed boolean, p_absence_used boolean); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.abandon_live_activity_start(p_claim jsonb, p_generation text, p_attempt text, p_absence_allowed boolean, p_absence_used boolean) FROM PUBLIC;
GRANT ALL ON FUNCTION app.abandon_live_activity_start(p_claim jsonb, p_generation text, p_attempt text, p_absence_allowed boolean, p_absence_used boolean) TO baby_tracker_maintenance;


--
-- Name: FUNCTION authority_actor_label(p_family_id uuid, p_account_id uuid, p_former boolean, p_at timestamp with time zone); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.authority_actor_label(p_family_id uuid, p_account_id uuid, p_former boolean, p_at timestamp with time zone) FROM PUBLIC;
GRANT ALL ON FUNCTION app.authority_actor_label(p_family_id uuid, p_account_id uuid, p_former boolean, p_at timestamp with time zone) TO baby_tracker_runtime;


--
-- Name: FUNCTION authority_context(); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.authority_context() FROM PUBLIC;
GRANT ALL ON FUNCTION app.authority_context() TO baby_tracker_runtime;


--
-- Name: FUNCTION claim_live_activity_work(p_now timestamp with time zone, p_limit integer); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.claim_live_activity_work(p_now timestamp with time zone, p_limit integer) FROM PUBLIC;
GRANT ALL ON FUNCTION app.claim_live_activity_work(p_now timestamp with time zone, p_limit integer) TO baby_tracker_maintenance;


--
-- Name: FUNCTION current_account_id(); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.current_account_id() FROM PUBLIC;
GRANT ALL ON FUNCTION app.current_account_id() TO baby_tracker_runtime;


--
-- Name: FUNCTION current_family_id(); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.current_family_id() FROM PUBLIC;
GRANT ALL ON FUNCTION app.current_family_id() TO baby_tracker_runtime;


--
-- Name: FUNCTION dirty_live_activity_families(p_families uuid[], p_sequences bigint[]); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.dirty_live_activity_families(p_families uuid[], p_sequences bigint[]) FROM PUBLIC;
GRANT ALL ON FUNCTION app.dirty_live_activity_families(p_families uuid[], p_sequences bigint[]) TO baby_tracker_maintenance;


--
-- Name: FUNCTION dirty_live_activity_family(p_family uuid, p_sequence bigint); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.dirty_live_activity_family(p_family uuid, p_sequence bigint) FROM PUBLIC;


--
-- Name: FUNCTION ensure_audit_partitions(p_through timestamp with time zone, OUT created integer, OUT skipped text[]); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.ensure_audit_partitions(p_through timestamp with time zone, OUT created integer, OUT skipped text[]) FROM PUBLIC;
GRANT ALL ON FUNCTION app.ensure_audit_partitions(p_through timestamp with time zone, OUT created integer, OUT skipped text[]) TO baby_tracker_maintenance;


--
-- Name: FUNCTION family_actor_labels_high_water(); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.family_actor_labels_high_water() FROM PUBLIC;
GRANT ALL ON FUNCTION app.family_actor_labels_high_water() TO baby_tracker_runtime;


--
-- Name: FUNCTION fence_live_activity_installation(p_instance uuid, p_epoch bigint, p_installation uuid, p_account uuid, p_device uuid, p_family uuid, p_version bigint, p_expires timestamp with time zone, p_revoked boolean); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.fence_live_activity_installation(p_instance uuid, p_epoch bigint, p_installation uuid, p_account uuid, p_device uuid, p_family uuid, p_version bigint, p_expires timestamp with time zone, p_revoked boolean) FROM PUBLIC;
GRANT ALL ON FUNCTION app.fence_live_activity_installation(p_instance uuid, p_epoch bigint, p_installation uuid, p_account uuid, p_device uuid, p_family uuid, p_version bigint, p_expires timestamp with time zone, p_revoked boolean) TO baby_tracker_runtime;


--
-- Name: FUNCTION finish_live_activity_work(p_claim jsonb, p_retry_at timestamp with time zone, p_reason text); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.finish_live_activity_work(p_claim jsonb, p_retry_at timestamp with time zone, p_reason text) FROM PUBLIC;
GRANT ALL ON FUNCTION app.finish_live_activity_work(p_claim jsonb, p_retry_at timestamp with time zone, p_reason text) TO baby_tracker_maintenance;


--
-- Name: FUNCTION forget_push_token(p_device_id uuid, p_token bytea); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.forget_push_token(p_device_id uuid, p_token bytea) FROM PUBLIC;
GRANT ALL ON FUNCTION app.forget_push_token(p_device_id uuid, p_token bytea) TO baby_tracker_maintenance;


--
-- Name: FUNCTION import_unfinished_session_count(p_account_id uuid, p_now timestamp with time zone); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.import_unfinished_session_count(p_account_id uuid, p_now timestamp with time zone) FROM PUBLIC;
GRANT ALL ON FUNCTION app.import_unfinished_session_count(p_account_id uuid, p_now timestamp with time zone) TO baby_tracker_runtime;


--
-- Name: FUNCTION integration_authority(p_grant_id uuid, p_recovery_epoch bigint); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.integration_authority(p_grant_id uuid, p_recovery_epoch bigint) FROM PUBLIC;
GRANT ALL ON FUNCTION app.integration_authority(p_grant_id uuid, p_recovery_epoch bigint) TO baby_tracker_runtime;


--
-- Name: FUNCTION list_family_actor_labels(p_family_id uuid, p_high_water bigint, p_after_account_id uuid, p_limit integer); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.list_family_actor_labels(p_family_id uuid, p_high_water bigint, p_after_account_id uuid, p_limit integer) FROM PUBLIC;
GRANT ALL ON FUNCTION app.list_family_actor_labels(p_family_id uuid, p_high_water bigint, p_after_account_id uuid, p_limit integer) TO baby_tracker_runtime;


--
-- Name: FUNCTION live_activity_authorized(p_registration uuid, p_version bigint, p_require_entitlement boolean); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.live_activity_authorized(p_registration uuid, p_version bigint, p_require_entitlement boolean) FROM PUBLIC;
GRANT ALL ON FUNCTION app.live_activity_authorized(p_registration uuid, p_version bigint, p_require_entitlement boolean) TO baby_tracker_runtime;


--
-- Name: FUNCTION live_activity_claim_live(p_claim jsonb); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.live_activity_claim_live(p_claim jsonb) FROM PUBLIC;
GRANT ALL ON FUNCTION app.live_activity_claim_live(p_claim jsonb) TO baby_tracker_runtime;


--
-- Name: FUNCTION live_activity_lineage_live(p_account uuid, p_device uuid, p_family uuid, p_epoch bigint, p_expires timestamp with time zone); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.live_activity_lineage_live(p_account uuid, p_device uuid, p_family uuid, p_epoch bigint, p_expires timestamp with time zone) FROM PUBLIC;


--
-- Name: FUNCTION live_activity_registration_dirty(); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.live_activity_registration_dirty() FROM PUBLIC;


--
-- Name: FUNCTION live_activity_registration_state(p_registration uuid, p_inventory jsonb); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.live_activity_registration_state(p_registration uuid, p_inventory jsonb) FROM PUBLIC;
GRANT ALL ON FUNCTION app.live_activity_registration_state(p_registration uuid, p_inventory jsonb) TO baby_tracker_runtime;


--
-- Name: FUNCTION live_activity_session_binding(p_account uuid, p_device uuid, p_session uuid, p_epoch bigint); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.live_activity_session_binding(p_account uuid, p_device uuid, p_session uuid, p_epoch bigint) FROM PUBLIC;
GRANT ALL ON FUNCTION app.live_activity_session_binding(p_account uuid, p_device uuid, p_session uuid, p_epoch bigint) TO baby_tracker_runtime;


--
-- Name: FUNCTION live_activity_target(p_claim jsonb, p_state jsonb, p_finish boolean, p_deadline timestamp with time zone, p_invalid jsonb, p_outcome jsonb); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.live_activity_target(p_claim jsonb, p_state jsonb, p_finish boolean, p_deadline timestamp with time zone, p_invalid jsonb, p_outcome jsonb) FROM PUBLIC;
GRANT ALL ON FUNCTION app.live_activity_target(p_claim jsonb, p_state jsonb, p_finish boolean, p_deadline timestamp with time zone, p_invalid jsonb, p_outcome jsonb) TO baby_tracker_maintenance;


--
-- Name: FUNCTION live_activity_token_fingerprint(); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.live_activity_token_fingerprint() FROM PUBLIC;


--
-- Name: FUNCTION live_activity_token_ownership(); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.live_activity_token_ownership() FROM PUBLIC;


--
-- Name: FUNCTION membership_may_write(target_family uuid, target_account uuid); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.membership_may_write(target_family uuid, target_account uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION app.membership_may_write(target_family uuid, target_account uuid) TO baby_tracker_runtime;


--
-- Name: FUNCTION prune_push_tokens(p_limit integer); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.prune_push_tokens(p_limit integer) FROM PUBLIC;
GRANT ALL ON FUNCTION app.prune_push_tokens(p_limit integer) TO baby_tracker_maintenance;


--
-- Name: FUNCTION purge_family_step(p_family_id uuid, p_limit integer, OUT step text, OUT deleted integer); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.purge_family_step(p_family_id uuid, p_limit integer, OUT step text, OUT deleted integer) FROM PUBLIC;
GRANT ALL ON FUNCTION app.purge_family_step(p_family_id uuid, p_limit integer, OUT step text, OUT deleted integer) TO baby_tracker_maintenance;


--
-- Name: FUNCTION push_targets(p_family_ids uuid[], p_device_ids uuid[]); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.push_targets(p_family_ids uuid[], p_device_ids uuid[]) FROM PUBLIC;
GRANT ALL ON FUNCTION app.push_targets(p_family_ids uuid[], p_device_ids uuid[]) TO baby_tracker_maintenance;


--
-- Name: FUNCTION reject_audit_chain_regression(); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.reject_audit_chain_regression() FROM PUBLIC;


--
-- Name: FUNCTION reject_tenant_authority_write(); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.reject_tenant_authority_write() FROM PUBLIC;


--
-- Name: FUNCTION release_stale_live_activity_lease(p_claim jsonb); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.release_stale_live_activity_lease(p_claim jsonb) FROM PUBLIC;
GRANT ALL ON FUNCTION app.release_stale_live_activity_lease(p_claim jsonb) TO baby_tracker_maintenance;


--
-- Name: FUNCTION request_authority(p_access_digests bytea[], p_session_id uuid, p_family boolean, p_recovery_epoch bigint); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.request_authority(p_access_digests bytea[], p_session_id uuid, p_family boolean, p_recovery_epoch bigint) FROM PUBLIC;
GRANT ALL ON FUNCTION app.request_authority(p_access_digests bytea[], p_session_id uuid, p_family boolean, p_recovery_epoch bigint) TO baby_tracker_runtime;


--
-- Name: FUNCTION retain_live_activity_work(); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.retain_live_activity_work() FROM PUBLIC;
GRANT ALL ON FUNCTION app.retain_live_activity_work() TO baby_tracker_maintenance;


--
-- Name: FUNCTION retire_stale_live_activity_registrations(); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.retire_stale_live_activity_registrations() FROM PUBLIC;


--
-- Name: FUNCTION sweep_live_activity_registrations(p_require_entitlement boolean, p_limit integer); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.sweep_live_activity_registrations(p_require_entitlement boolean, p_limit integer) FROM PUBLIC;
GRANT ALL ON FUNCTION app.sweep_live_activity_registrations(p_require_entitlement boolean, p_limit integer) TO baby_tracker_maintenance;


--
-- Name: FUNCTION sweep_live_activity_work(); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.sweep_live_activity_work() FROM PUBLIC;
GRANT ALL ON FUNCTION app.sweep_live_activity_work() TO baby_tracker_maintenance;


--
-- Name: FUNCTION update_own_profile(p_account_id uuid, p_command_id uuid, p_display_name text, p_now timestamp with time zone); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.update_own_profile(p_account_id uuid, p_command_id uuid, p_display_name text, p_now timestamp with time zone) FROM PUBLIC;
GRANT ALL ON FUNCTION app.update_own_profile(p_account_id uuid, p_command_id uuid, p_display_name text, p_now timestamp with time zone) TO baby_tracker_runtime;


--
-- Name: FUNCTION valid_display_name(p_value text); Type: ACL; Schema: app; Owner: -
--

REVOKE ALL ON FUNCTION app.valid_display_name(p_value text) FROM PUBLIC;


--
-- Name: TABLE account_identities; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.account_identities TO baby_tracker_runtime;


--
-- Name: COLUMN account_identities.live; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(live) ON TABLE public.account_identities TO baby_tracker_runtime;


--
-- Name: TABLE accounts; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.accounts TO baby_tracker_runtime;


--
-- Name: COLUMN accounts.display_name; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(display_name) ON TABLE public.accounts TO baby_tracker_runtime;


--
-- Name: COLUMN accounts.status; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(status) ON TABLE public.accounts TO baby_tracker_runtime;


--
-- Name: COLUMN accounts.updated_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(updated_at) ON TABLE public.accounts TO baby_tracker_runtime;


--
-- Name: COLUMN accounts.profile_required; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(profile_required) ON TABLE public.accounts TO baby_tracker_runtime;


--
-- Name: COLUMN accounts.lifecycle_generation; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(lifecycle_generation) ON TABLE public.accounts TO baby_tracker_runtime;


--
-- Name: COLUMN accounts.audit_seq; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(audit_seq) ON TABLE public.accounts TO baby_tracker_runtime;


--
-- Name: COLUMN accounts.audit_hash; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(audit_hash) ON TABLE public.accounts TO baby_tracker_runtime;


--
-- Name: COLUMN accounts.chain_failed; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(chain_failed) ON TABLE public.accounts TO baby_tracker_runtime;


--
-- Name: TABLE apple_notifications; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.apple_notifications TO baby_tracker_runtime;


--
-- Name: TABLE audit_checkpoints; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.audit_checkpoints TO baby_tracker_runtime;


--
-- Name: COLUMN audit_checkpoints.verified_seq; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(verified_seq) ON TABLE public.audit_checkpoints TO baby_tracker_runtime;


--
-- Name: COLUMN audit_checkpoints.verified_hash; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(verified_hash) ON TABLE public.audit_checkpoints TO baby_tracker_runtime;


--
-- Name: COLUMN audit_checkpoints.verified_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(verified_at) ON TABLE public.audit_checkpoints TO baby_tracker_runtime;


--
-- Name: TABLE auth_challenges; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE ON TABLE public.auth_challenges TO baby_tracker_runtime;


--
-- Name: COLUMN auth_challenges.consumed_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(consumed_at) ON TABLE public.auth_challenges TO baby_tracker_runtime;


--
-- Name: TABLE care_entities; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.family_id; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT(family_id) ON TABLE public.care_entities TO baby_tracker_maintenance;


--
-- Name: COLUMN care_entities.id; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT(id) ON TABLE public.care_entities TO baby_tracker_maintenance;


--
-- Name: COLUMN care_entities.kind; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT(kind) ON TABLE public.care_entities TO baby_tracker_maintenance;


--
-- Name: COLUMN care_entities.baby_id; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(baby_id) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.entity_version; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(entity_version) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.changed_seq; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(changed_seq) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.actor_account_id; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(actor_account_id) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.device_id; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(device_id) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.operation; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(operation) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.accepted_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(accepted_at) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.tombstoned_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(tombstoned_at) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.started_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(started_at) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.ended_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(ended_at) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.session_id; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(session_id) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.completed_record_id; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(completed_record_id) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.sort_order; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(sort_order) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.label; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(label) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.name_hash; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(name_hash) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.breast_seconds; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(breast_seconds) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.bottle_volume_ml; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(bottle_volume_ml) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.bottle_observations; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(bottle_observations) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.bottle_missing; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(bottle_missing) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.shortest_part_seconds; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(shortest_part_seconds) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.wire; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(wire) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: COLUMN care_entities.feed_kind; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(feed_kind) ON TABLE public.care_entities TO baby_tracker_runtime;


--
-- Name: TABLE care_receipts; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.care_receipts TO baby_tracker_runtime;
GRANT SELECT,DELETE ON TABLE public.care_receipts TO baby_tracker_maintenance;


--
-- Name: TABLE command_receipts; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.command_receipts TO baby_tracker_runtime;


--
-- Name: TABLE device_push_tokens; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE ON TABLE public.device_push_tokens TO baby_tracker_runtime;


--
-- Name: COLUMN device_push_tokens.environment; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(environment) ON TABLE public.device_push_tokens TO baby_tracker_runtime;


--
-- Name: COLUMN device_push_tokens.token; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(token) ON TABLE public.device_push_tokens TO baby_tracker_runtime;


--
-- Name: COLUMN device_push_tokens.registered_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(registered_at) ON TABLE public.device_push_tokens TO baby_tracker_runtime;


--
-- Name: TABLE device_sync_cursors; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE public.device_sync_cursors TO baby_tracker_maintenance;


--
-- Name: TABLE devices; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.devices TO baby_tracker_runtime;


--
-- Name: COLUMN devices.updated_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(updated_at) ON TABLE public.devices TO baby_tracker_runtime;


--
-- Name: COLUMN devices.status; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(status) ON TABLE public.devices TO baby_tracker_runtime;


--
-- Name: COLUMN devices.revoked_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(revoked_at) ON TABLE public.devices TO baby_tracker_runtime;


--
-- Name: TABLE export_chunks; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT ON TABLE public.export_chunks TO baby_tracker_runtime;
GRANT SELECT,INSERT,DELETE ON TABLE public.export_chunks TO baby_tracker_maintenance;


--
-- Name: TABLE export_jobs; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.export_jobs TO baby_tracker_runtime;
GRANT SELECT,DELETE,UPDATE ON TABLE public.export_jobs TO baby_tracker_maintenance;


--
-- Name: TABLE export_rate_reservations; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.export_rate_reservations TO baby_tracker_runtime;
GRANT SELECT,DELETE ON TABLE public.export_rate_reservations TO baby_tracker_maintenance;


--
-- Name: TABLE families; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.families TO baby_tracker_runtime;


--
-- Name: COLUMN families.id; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT(id) ON TABLE public.families TO baby_tracker_maintenance;


--
-- Name: COLUMN families.display_name; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(display_name) ON TABLE public.families TO baby_tracker_runtime;


--
-- Name: COLUMN families.status; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(status) ON TABLE public.families TO baby_tracker_runtime;
GRANT SELECT(status) ON TABLE public.families TO baby_tracker_maintenance;


--
-- Name: COLUMN families.updated_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(updated_at) ON TABLE public.families TO baby_tracker_runtime;


--
-- Name: COLUMN families.purge_after; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(purge_after) ON TABLE public.families TO baby_tracker_runtime;


--
-- Name: COLUMN families.profile_required; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(profile_required) ON TABLE public.families TO baby_tracker_runtime;


--
-- Name: COLUMN families.next_sequence; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(next_sequence) ON TABLE public.families TO baby_tracker_runtime;
GRANT SELECT(next_sequence) ON TABLE public.families TO baby_tracker_maintenance;


--
-- Name: COLUMN families.owner_account_id; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(owner_account_id) ON TABLE public.families TO baby_tracker_runtime;


--
-- Name: COLUMN families.purge_started_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(purge_started_at) ON TABLE public.families TO baby_tracker_runtime;


--
-- Name: COLUMN families.purge_command_id; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(purge_command_id) ON TABLE public.families TO baby_tracker_runtime;


--
-- Name: COLUMN families.purge_receipt; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(purge_receipt) ON TABLE public.families TO baby_tracker_runtime;


--
-- Name: COLUMN families.purged_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(purged_at) ON TABLE public.families TO baby_tracker_runtime;


--
-- Name: COLUMN families.audit_seq; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(audit_seq) ON TABLE public.families TO baby_tracker_runtime;


--
-- Name: COLUMN families.audit_hash; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(audit_hash) ON TABLE public.families TO baby_tracker_runtime;


--
-- Name: COLUMN families.chain_failed; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(chain_failed) ON TABLE public.families TO baby_tracker_runtime;


--
-- Name: COLUMN families.entitlement_required; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(entitlement_required) ON TABLE public.families TO baby_tracker_runtime;


--
-- Name: COLUMN families.care_record_count; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(care_record_count) ON TABLE public.families TO baby_tracker_runtime;
GRANT SELECT(care_record_count),UPDATE(care_record_count) ON TABLE public.families TO baby_tracker_maintenance;


--
-- Name: COLUMN families.care_charged_bytes; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(care_charged_bytes) ON TABLE public.families TO baby_tracker_runtime;
GRANT SELECT(care_charged_bytes),UPDATE(care_charged_bytes) ON TABLE public.families TO baby_tracker_maintenance;


--
-- Name: COLUMN families.care_entity_count; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(care_entity_count) ON TABLE public.families TO baby_tracker_runtime;
GRANT SELECT(care_entity_count),UPDATE(care_entity_count) ON TABLE public.families TO baby_tracker_maintenance;


--
-- Name: COLUMN families.care_wire_bytes; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(care_wire_bytes) ON TABLE public.families TO baby_tracker_runtime;
GRANT SELECT(care_wire_bytes),UPDATE(care_wire_bytes) ON TABLE public.families TO baby_tracker_maintenance;


--
-- Name: TABLE family_actor_labels; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT ON TABLE public.family_actor_labels TO baby_tracker_runtime;


--
-- Name: TABLE family_entitlements; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.family_entitlements TO baby_tracker_runtime;


--
-- Name: COLUMN family_entitlements.status; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(status) ON TABLE public.family_entitlements TO baby_tracker_runtime;


--
-- Name: COLUMN family_entitlements.expires_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(expires_at) ON TABLE public.family_entitlements TO baby_tracker_runtime;


--
-- Name: COLUMN family_entitlements.updated_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(updated_at) ON TABLE public.family_entitlements TO baby_tracker_runtime;


--
-- Name: TABLE family_recovery_claims; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.family_recovery_claims TO baby_tracker_runtime;


--
-- Name: COLUMN family_recovery_claims.account_id; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(account_id) ON TABLE public.family_recovery_claims TO baby_tracker_runtime;


--
-- Name: COLUMN family_recovery_claims.status; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(status) ON TABLE public.family_recovery_claims TO baby_tracker_runtime;


--
-- Name: TABLE family_slots; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE ON TABLE public.family_slots TO baby_tracker_runtime;


--
-- Name: COLUMN family_slots.family_id; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(family_id) ON TABLE public.family_slots TO baby_tracker_runtime;


--
-- Name: COLUMN family_slots.state; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(state) ON TABLE public.family_slots TO baby_tracker_runtime;


--
-- Name: TABLE import_baby_candidates; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.import_baby_candidates TO baby_tracker_runtime;


--
-- Name: TABLE import_baby_mappings; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.import_baby_mappings TO baby_tracker_runtime;


--
-- Name: TABLE import_chunks; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.import_chunks TO baby_tracker_runtime;


--
-- Name: TABLE import_conflict_receipts; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.import_conflict_receipts TO baby_tracker_runtime;


--
-- Name: TABLE import_conflicts; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,UPDATE ON TABLE public.import_conflicts TO baby_tracker_runtime;


--
-- Name: TABLE import_record_manifests; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.import_record_manifests TO baby_tracker_runtime;


--
-- Name: TABLE import_sessions; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,UPDATE ON TABLE public.import_sessions TO baby_tracker_runtime;


--
-- Name: COLUMN import_sessions.family_id; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT(family_id) ON TABLE public.import_sessions TO baby_tracker_maintenance;


--
-- Name: COLUMN import_sessions.import_id; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT(import_id) ON TABLE public.import_sessions TO baby_tracker_maintenance;


--
-- Name: TABLE invitations; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.invitations TO baby_tracker_runtime;


--
-- Name: COLUMN invitations.status; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(status) ON TABLE public.invitations TO baby_tracker_runtime;


--
-- Name: COLUMN invitations.redeemed_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(redeemed_at) ON TABLE public.invitations TO baby_tracker_runtime;


--
-- Name: COLUMN invitations.revoked_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(revoked_at) ON TABLE public.invitations TO baby_tracker_runtime;


--
-- Name: COLUMN invitations.updated_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(updated_at) ON TABLE public.invitations TO baby_tracker_runtime;


--
-- Name: TABLE live_activity_devices; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,UPDATE ON TABLE public.live_activity_devices TO baby_tracker_runtime;


--
-- Name: TABLE live_activity_tokens; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,UPDATE ON TABLE public.live_activity_tokens TO baby_tracker_runtime;


--
-- Name: TABLE mcp_integrations; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.mcp_integrations TO baby_tracker_runtime;


--
-- Name: COLUMN mcp_integrations.status; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(status) ON TABLE public.mcp_integrations TO baby_tracker_runtime;


--
-- Name: COLUMN mcp_integrations.revoked_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(revoked_at) ON TABLE public.mcp_integrations TO baby_tracker_runtime;


--
-- Name: TABLE memberships; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.memberships TO baby_tracker_runtime;


--
-- Name: COLUMN memberships.role; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(role) ON TABLE public.memberships TO baby_tracker_runtime;


--
-- Name: COLUMN memberships.status; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(status) ON TABLE public.memberships TO baby_tracker_runtime;


--
-- Name: COLUMN memberships.updated_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(updated_at) ON TABLE public.memberships TO baby_tracker_runtime;


--
-- Name: TABLE refresh_receipts; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE ON TABLE public.refresh_receipts TO baby_tracker_runtime;


--
-- Name: TABLE schema_compatibility; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT ON TABLE public.schema_compatibility TO baby_tracker_runtime;


--
-- Name: TABLE schema_migrations; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT ON TABLE public.schema_migrations TO baby_tracker_runtime;


--
-- Name: TABLE schema_privilege_manifest; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT ON TABLE public.schema_privilege_manifest TO baby_tracker_runtime;


--
-- Name: TABLE security_audit_events; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.security_audit_events TO baby_tracker_runtime;


--
-- Name: TABLE server_state; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT ON TABLE public.server_state TO baby_tracker_runtime;


--
-- Name: TABLE sessions; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT,DELETE ON TABLE public.sessions TO baby_tracker_runtime;


--
-- Name: COLUMN sessions.scope; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(scope) ON TABLE public.sessions TO baby_tracker_runtime;


--
-- Name: COLUMN sessions.family_id; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(family_id) ON TABLE public.sessions TO baby_tracker_runtime;


--
-- Name: COLUMN sessions.role; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(role) ON TABLE public.sessions TO baby_tracker_runtime;


--
-- Name: COLUMN sessions.status; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(status) ON TABLE public.sessions TO baby_tracker_runtime;


--
-- Name: COLUMN sessions.consumed_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(consumed_at) ON TABLE public.sessions TO baby_tracker_runtime;


--
-- Name: COLUMN sessions.consumed_attempt_id; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(consumed_attempt_id) ON TABLE public.sessions TO baby_tracker_runtime;


--
-- Name: COLUMN sessions.child_id; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(child_id) ON TABLE public.sessions TO baby_tracker_runtime;


--
-- Name: COLUMN sessions.revoked_at; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(revoked_at) ON TABLE public.sessions TO baby_tracker_runtime;


--
-- Name: COLUMN sessions.revoke_reason; Type: ACL; Schema: public; Owner: -
--

GRANT UPDATE(revoke_reason) ON TABLE public.sessions TO baby_tracker_runtime;


--
-- Name: TABLE sync_page_bindings; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.sync_page_bindings TO baby_tracker_runtime;
GRANT SELECT,DELETE ON TABLE public.sync_page_bindings TO baby_tracker_maintenance;


--
-- Name: TABLE sync_snapshot_sessions; Type: ACL; Schema: public; Owner: -
--

GRANT SELECT,INSERT ON TABLE public.sync_snapshot_sessions TO baby_tracker_runtime;
GRANT SELECT,DELETE ON TABLE public.sync_snapshot_sessions TO baby_tracker_maintenance;

--
-- The version-32 privilege manifest.
--

INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','column','public.care_entities.family_id','SELECT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','column','public.care_entities.id','SELECT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','column','public.care_entities.kind','SELECT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','column','public.families.care_charged_bytes','SELECT,UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','column','public.families.care_entity_count','SELECT,UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','column','public.families.care_record_count','SELECT,UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','column','public.families.care_wire_bytes','SELECT,UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','column','public.families.id','SELECT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','column','public.families.next_sequence','SELECT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','column','public.families.status','SELECT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','column','public.import_sessions.family_id','SELECT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','column','public.import_sessions.import_id','SELECT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','function','app.abandon_live_activity_start(jsonb,text,text,boolean,boolean)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','function','app.claim_live_activity_work(timestamp with time zone,integer)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','function','app.dirty_live_activity_families(uuid[],bigint[])','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','function','app.ensure_audit_partitions(timestamp with time zone)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','function','app.finish_live_activity_work(jsonb,timestamp with time zone,text)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','function','app.forget_push_token(uuid,bytea)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','function','app.live_activity_target(jsonb,jsonb,boolean,timestamp with time zone,jsonb,jsonb)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','function','app.prune_push_tokens(integer)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','function','app.purge_family_step(uuid,integer)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','function','app.push_targets(uuid[],uuid[])','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','function','app.release_stale_live_activity_lease(jsonb)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','function','app.retain_live_activity_work()','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','function','app.sweep_live_activity_registrations(boolean,integer)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','function','app.sweep_live_activity_work()','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','schema','app','USAGE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','schema','public','USAGE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','table','public.care_receipts','SELECT,DELETE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','table','public.device_sync_cursors','SELECT,INSERT,UPDATE,DELETE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','table','public.export_chunks','SELECT,INSERT,DELETE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','table','public.export_jobs','SELECT,UPDATE,DELETE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','table','public.export_rate_reservations','SELECT,DELETE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','table','public.sync_page_bindings','SELECT,DELETE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_maintenance','table','public.sync_snapshot_sessions','SELECT,DELETE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.account_identities.live','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.accounts.audit_hash','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.accounts.audit_seq','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.accounts.chain_failed','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.accounts.display_name','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.accounts.lifecycle_generation','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.accounts.profile_required','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.accounts.status','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.accounts.updated_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.audit_checkpoints.verified_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.audit_checkpoints.verified_hash','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.audit_checkpoints.verified_seq','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.auth_challenges.consumed_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.accepted_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.actor_account_id','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.baby_id','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.bottle_missing','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.bottle_observations','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.bottle_volume_ml','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.breast_seconds','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.changed_seq','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.completed_record_id','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.device_id','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.ended_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.entity_version','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.feed_kind','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.label','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.name_hash','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.operation','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.session_id','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.shortest_part_seconds','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.sort_order','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.started_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.tombstoned_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.care_entities.wire','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.device_push_tokens.environment','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.device_push_tokens.registered_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.device_push_tokens.token','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.devices.revoked_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.devices.status','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.devices.updated_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.audit_hash','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.audit_seq','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.care_charged_bytes','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.care_entity_count','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.care_record_count','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.care_wire_bytes','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.chain_failed','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.display_name','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.entitlement_required','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.next_sequence','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.owner_account_id','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.profile_required','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.purge_after','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.purge_command_id','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.purge_receipt','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.purge_started_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.purged_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.status','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.families.updated_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.family_entitlements.expires_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.family_entitlements.status','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.family_entitlements.updated_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.family_recovery_claims.account_id','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.family_recovery_claims.status','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.family_slots.family_id','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.family_slots.state','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.invitations.redeemed_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.invitations.revoked_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.invitations.status','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.invitations.updated_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.mcp_integrations.revoked_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.mcp_integrations.status','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.memberships.role','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.memberships.status','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.memberships.updated_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.sessions.child_id','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.sessions.consumed_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.sessions.consumed_attempt_id','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.sessions.family_id','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.sessions.revoke_reason','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.sessions.revoked_at','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.sessions.role','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.sessions.scope','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','column','public.sessions.status','UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.authority_actor_label(uuid,uuid,boolean,timestamp with time zone)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.authority_context()','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.current_account_id()','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.current_family_id()','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.family_actor_labels_high_water()','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.fence_live_activity_installation(uuid,bigint,uuid,uuid,uuid,uuid,bigint,timestamp with time zone,boolean)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.integration_authority(uuid,bigint)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.list_family_actor_labels(uuid,bigint,uuid,integer)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.live_activity_authorized(uuid,bigint,boolean)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.live_activity_claim_live(jsonb)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.live_activity_registration_state(uuid,jsonb)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.live_activity_session_binding(uuid,uuid,uuid,bigint)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.membership_may_write(uuid,uuid)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.request_authority(bytea[],uuid,boolean,bigint)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','function','app.update_own_profile(uuid,uuid,text,timestamp with time zone)','EXECUTE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','schema','app','USAGE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','schema','public','USAGE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.account_identities','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.accounts','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.apple_notifications','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.audit_checkpoints','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.auth_challenges','SELECT,INSERT,DELETE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.care_entities','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.care_receipts','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.command_receipts','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.device_push_tokens','SELECT,INSERT,DELETE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.devices','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.export_chunks','SELECT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.export_jobs','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.export_rate_reservations','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.families','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.family_actor_labels','SELECT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.family_entitlements','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.family_recovery_claims','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.family_slots','SELECT,INSERT,DELETE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.import_baby_candidates','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.import_baby_mappings','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.import_chunks','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.import_conflict_receipts','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.import_conflicts','SELECT,INSERT,UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.import_record_manifests','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.import_sessions','SELECT,INSERT,UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.invitations','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.live_activity_devices','SELECT,INSERT,UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.live_activity_tokens','SELECT,INSERT,UPDATE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.mcp_integrations','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.memberships','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.refresh_receipts','SELECT,INSERT,DELETE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.schema_compatibility','SELECT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.schema_migrations','SELECT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.schema_privilege_manifest','SELECT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.security_audit_events','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.server_state','SELECT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.sessions','SELECT,INSERT,DELETE');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.sync_page_bindings','SELECT,INSERT');
INSERT INTO public.schema_privilege_manifest VALUES (32,'baby_tracker_runtime','table','public.sync_snapshot_sessions','SELECT,INSERT');

INSERT INTO public.schema_compatibility(singleton,current_version,introduced_by) VALUES (true,32,'000032_baseline.sql');

DO $$ BEGIN PERFORM app.ensure_audit_partitions(now() + interval '92 days'); END $$;

-- check_function_bodies as it was before this file ran (see the top).
SELECT set_config('check_function_bodies', current_setting('baby_tracker.baseline_check_function_bodies'), true);
