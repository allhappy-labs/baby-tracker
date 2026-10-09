-- Apply `member_role_changed` in the security projection.
--
-- 000014 added the role everywhere except here: it widened the CHECK on
-- `memberships.role`, widened the one on `invitations.role`, and added
-- `app.membership_may_write` for the write path to consult. The Go side has
-- the command, the HTTP route, the event descriptor and the state machine.
-- The database projection does not, and the projection is where an event
-- becomes a fact.
--
-- The consequence was not a clear error. `app.project_security_event` forwards
-- any kind it does not recognise down the chain of earlier versions, and the
-- oldest of them raises on an unknown kind; the coordinator turns that into
-- `service_unavailable` and quarantines the projection. So an owner demoting a
-- caregiver got "the service is unavailable", the ledger kept the event it had
-- already committed, and the projection stopped. A read-only caregiver could
-- not be created at all, which is why the protocol tests for one were skipped.
--
-- Additive in the same sense 000014 was: a schema 14 binary never emits this
-- kind, and every other kind still reaches v7 unchanged.

ALTER FUNCTION app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    RENAME TO project_security_event_v7;
REVOKE ALL ON FUNCTION app.project_security_event_v7(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;

CREATE FUNCTION app.project_security_event(
    p_generation bigint,p_revision bigint,p_previous_hash bytea,p_event_hash bytea,
    p_actor_account_id uuid,p_command_id uuid,p_command_hash bytea,p_result jsonb,p_event jsonb,p_event_bytes bytea
) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
    event_kind text;
    event_at timestamptz;
    effect jsonb;
    canonical_event text;
    current_generation bigint;
    current_revision bigint;
    current_hash bytea;
    affected_rows bigint;
    family_value uuid;
    account_value uuid;
    previous_account text:=coalesce(current_setting('app.account_id',true),'');
    previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
    event_kind:=p_event->>'kind';
    IF event_kind<>'member_role_changed' THEN
        PERFORM app.project_security_event_v7(p_generation,p_revision,p_previous_hash,p_event_hash,p_actor_account_id,
            p_command_id,p_command_hash,p_result,p_event,p_event_bytes);
        RETURN;
    END IF;

    -- The envelope is re-validated here rather than trusted from the caller.
    -- This function is SECURITY DEFINER and is the only thing standing between
    -- a malformed event and the projection tables; every earlier version
    -- checks the same things for the kinds it owns.
    IF p_generation IS NULL OR p_revision IS NULL OR p_generation<=0 OR p_revision<=0
       OR p_generation>9007199254740991 OR p_revision>9007199254740991
       OR octet_length(p_previous_hash) IS DISTINCT FROM 32 OR octet_length(p_event_hash) IS DISTINCT FROM 32
       OR octet_length(p_command_hash) IS DISTINCT FROM 32 OR octet_length(p_event_bytes) NOT BETWEEN 1 AND 65536
       OR p_actor_account_id IS NULL OR p_actor_account_id='00000000-0000-0000-0000-000000000000'
       OR p_command_id IS NULL OR p_command_id='00000000-0000-0000-0000-000000000000'
       OR sha256(p_event_bytes) IS DISTINCT FROM p_event_hash
       OR convert_from(p_event_bytes,'UTF8')::jsonb IS DISTINCT FROM p_event
       OR jsonb_typeof(p_event) IS DISTINCT FROM 'object'
       OR (p_event-ARRAY['version','generation','revision','previous_hash','command_id','command_hash','actor_account_id','actor_device_id','kind','at','effects']::text[])<>'{}'::jsonb
       OR NOT (p_event ?& ARRAY['version','generation','revision','previous_hash','command_id','command_hash','actor_account_id','actor_device_id','kind','at','effects'])
       OR p_event->>'version'<>'v1' OR jsonb_typeof(p_event->'generation')<>'number' OR jsonb_typeof(p_event->'revision')<>'number'
       OR (p_event->>'generation')::bigint<>p_generation OR (p_event->>'revision')::bigint<>p_revision
       OR p_event->>'previous_hash' !~ '^[0-9a-f]{64}$' OR decode(p_event->>'previous_hash','hex')<>p_previous_hash
       OR p_event->>'command_hash' !~ '^[0-9a-f]{64}$' OR decode(p_event->>'command_hash','hex')<>p_command_hash
       OR p_event->>'actor_account_id'<>p_actor_account_id::text OR (p_event->>'actor_account_id')::uuid<>p_actor_account_id
       OR p_event->>'command_id'<>p_command_id::text OR (p_event->>'command_id')::uuid<>p_command_id
       OR jsonb_typeof(p_event->'actor_device_id')<>'string'
       OR (p_event->>'actor_device_id'<>'' AND (p_event->>'actor_device_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
           OR p_event->>'actor_device_id'='00000000-0000-0000-0000-000000000000'))
       OR jsonb_typeof(p_event->'at')<>'string'
       OR p_event->>'at' !~ '^[0-9]{4}-(0[1-9]|1[0-2])-([0-2][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{0,8}[1-9])?Z$'
       OR jsonb_typeof(p_event->'effects')<>'array' OR jsonb_array_length(p_event->'effects')<>1
       OR p_result IS DISTINCT FROM jsonb_build_object('command_id',p_command_id::text,'outcome','committed','code','ok','revision',p_revision) THEN
        RAISE EXCEPTION 'invalid member role event envelope' USING ERRCODE='22023';
    END IF;
    event_at:=(p_event->>'at')::timestamptz;
    IF event_at IS NULL OR NOT isfinite(event_at) THEN
        RAISE EXCEPTION 'invalid member role event time' USING ERRCODE='22023'; END IF;

    effect:=p_event->'effects'->0;
    -- Exactly these keys and no others. A role change carries no expiry, no
    -- digest and no operator evidence, and an effect that smuggled one in
    -- would be recorded in the audit trail as though the projection had
    -- honoured it.
    IF jsonb_typeof(effect)<>'object'
       OR (effect-ARRAY['kind','account_id','family_id','role','status']::text[])<>'{}'::jsonb
       OR NOT (effect ?& ARRAY['kind','account_id','family_id','role','status'])
       OR EXISTS (SELECT FROM jsonb_each(effect) item WHERE jsonb_typeof(item.value)<>'string')
       OR effect->>'kind'<>event_kind
       OR effect->>'account_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR effect->>'account_id'='00000000-0000-0000-0000-000000000000'
       OR effect->>'family_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR effect->>'family_id'='00000000-0000-0000-0000-000000000000'
       -- Never 'owner'. Promotion and demotion of an owner is
       -- `owner_transferred`, which names a successor in the same event so the
       -- family is never left without one. Accepting 'owner' here would be a
       -- second, quieter way to reach that state.
       OR effect->>'role' NOT IN ('caregiver','viewer')
       OR effect->>'status'<>'active' THEN
        RAISE EXCEPTION 'invalid member role effect' USING ERRCODE='22023';
    END IF;

    canonical_event:='{"version":"v1","generation":'||p_generation::text||',"revision":'||p_revision::text||
        ',"previous_hash":'||app.go_json_string(p_event->>'previous_hash')||',"command_id":'||app.go_json_string(p_event->>'command_id')||
        ',"command_hash":'||app.go_json_string(p_event->>'command_hash')||',"actor_account_id":'||app.go_json_string(p_event->>'actor_account_id')||
        ',"actor_device_id":'||app.go_json_string(p_event->>'actor_device_id')||',"kind":'||app.go_json_string(event_kind)||
        ',"at":'||app.go_json_string(p_event->>'at')||',"effects":['||
        '{"kind":'||app.go_json_string(effect->>'kind')||
        ',"account_id":'||app.go_json_string(effect->>'account_id')||
        ',"family_id":'||app.go_json_string(effect->>'family_id')||
        ',"role":'||app.go_json_string(effect->>'role')||
        ',"status":'||app.go_json_string(effect->>'status')||'}]}';
    IF convert_from(p_event_bytes,'UTF8')<>canonical_event THEN
        RAISE EXCEPTION 'event bytes are not canonical' USING ERRCODE='22023'; END IF;

    SELECT generation,revision,event_hash INTO current_generation,current_revision,current_hash
      FROM public.security_projection_head WHERE singleton FOR UPDATE;
    -- Replaying the event already at the head is a no-op, not an error: the
    -- coordinator re-applies after a restart and must not fail for having
    -- succeeded before.
    IF current_generation=p_generation AND current_revision=p_revision AND current_hash=p_event_hash THEN
        IF NOT EXISTS(SELECT FROM public.security_audit_events WHERE generation=p_generation AND revision=p_revision
            AND event_hash=p_event_hash AND event_bytes=p_event_bytes AND event_data=p_event) THEN
            RAISE EXCEPTION 'projection head lacks exact audit receipt' USING ERRCODE='23514'; END IF;
        RETURN;
    END IF;
    IF current_generation<>p_generation OR current_revision+1<>p_revision OR current_hash<>p_previous_hash THEN
        RAISE EXCEPTION 'security projection head mismatch' USING ERRCODE='40001'; END IF;

    family_value:=(effect->>'family_id')::uuid;
    account_value:=(effect->>'account_id')::uuid;
    PERFORM set_config('app.family_id',family_value::text,true);
    PERFORM set_config('app.account_id',account_value::text,true);

    -- `role<>'owner'` in the predicate, not merely absent from the new value:
    -- this is the row-level restatement of the rule above, and it is what
    -- makes demoting the owner impossible rather than merely unintended.
    UPDATE public.memberships SET role=effect->>'role',updated_at=event_at
     WHERE family_id=family_value AND account_id=account_value
       AND status='active' AND role<>'owner';
    GET DIAGNOSTICS affected_rows=ROW_COUNT;
    IF affected_rows<>1 THEN
        RAISE EXCEPTION 'member role target unavailable' USING ERRCODE='23514'; END IF;

    INSERT INTO public.security_audit_events(generation,revision,event_hash,command_id,actor_account_id,kind,event_data,event_bytes)
      VALUES(p_generation,p_revision,p_event_hash,p_command_id,p_actor_account_id,event_kind,p_event,p_event_bytes);
    INSERT INTO public.security_command_receipts(actor_account_id,command_id,command_hash,generation,revision,result)
      VALUES(p_actor_account_id,p_command_id,p_command_hash,p_generation,p_revision,p_result);
    UPDATE public.security_projection_head SET generation=p_generation,revision=p_revision,event_hash=p_event_hash WHERE singleton;
    PERFORM set_config('app.family_id',previous_family,true);
    PERFORM set_config('app.account_id',previous_account,true);
END;
$$;

REVOKE ALL ON FUNCTION app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    FROM PUBLIC,baby_tracker_runtime,baby_tracker_apple_revoker;
GRANT EXECUTE ON FUNCTION app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    TO baby_tracker_projector;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
SELECT 15,principal,object_kind,object_identity,privileges
  FROM public.schema_privilege_manifest WHERE schema_version=14;

UPDATE public.schema_compatibility
   SET current_version=15,introduced_by='000015_member_role_projection.sql',recorded_at=now()
 WHERE singleton;
