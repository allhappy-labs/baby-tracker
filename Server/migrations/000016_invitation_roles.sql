-- Invitations that confer view-only.
--
-- 000014 widened `invitations.role` to accept 'viewer' and stopped there. The
-- three invitation events still refuse anything but 'caregiver' in the
-- projection, and both places that write a role out of one are literals:
-- `invitations.role` on creation and `memberships.role` on redemption. So a
-- view-only invitation could be described, stored and redeemed — as a
-- caregiver, silently.
--
-- All THREE invitation kinds move, not just creation. Revocation matches the
-- stored row on `role=effect->>'role'` and validates the effect's role against
-- the same literal, so leaving it behind would have made a viewer invitation
-- impossible to withdraw.
--
-- The membership upsert's conflict guard widens with it: a removed member
-- rejoining may come back as either role, and pinning it to 'caregiver' would
-- have refused a returning viewer their own invitation. It still refuses an
-- owner, which is the part that matters.

ALTER FUNCTION app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    RENAME TO project_security_event_v8;
REVOKE ALL ON FUNCTION app.project_security_event_v8(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
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
    account_name text;
    family_value uuid;
    account_value uuid;
    previous_account text:=coalesce(current_setting('app.account_id',true),'');
    previous_family text:=coalesce(current_setting('app.family_id',true),'');
BEGIN
    event_kind:=p_event->>'kind';
    IF event_kind NOT IN ('invitation_created','invitation_revoked','invitation_redeemed') THEN
        PERFORM app.project_security_event_v8(p_generation,p_revision,p_previous_hash,p_event_hash,p_actor_account_id,
            p_command_id,p_command_hash,p_result,p_event,p_event_bytes);
        RETURN;
    END IF;

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
        RAISE EXCEPTION 'invalid invitation event envelope' USING ERRCODE='22023';
    END IF;
    event_at:=(p_event->>'at')::timestamptz;
    IF event_at IS NULL OR NOT isfinite(event_at) THEN
        RAISE EXCEPTION 'invalid invitation event time' USING ERRCODE='22023'; END IF;

    effect:=p_event->'effects'->0;
    IF jsonb_typeof(effect)<>'object'
       OR (effect-ARRAY['kind','account_id','family_id','invitation_id','token_digest','role','status','expires_at']::text[])<>'{}'::jsonb
       OR NOT (effect ?& ARRAY['kind','account_id','family_id','invitation_id','token_digest','role','status','expires_at'])
       OR EXISTS (SELECT FROM jsonb_each(effect) item WHERE jsonb_typeof(item.value)<>'string')
       OR effect->>'kind'<>event_kind
       OR effect->>'token_digest' !~ '^hmac-sha256:[a-z][a-z0-9._-]{0,63}:[0-9a-f]{64}$'
       -- The widening. Never 'owner': an owner arrives by creating the family
       -- or by transfer, and a link that conferred ownership would be a second
       -- way to end up with two of them.
       OR effect->>'role' NOT IN ('caregiver','viewer')
       OR effect->>'expires_at' !~ '^[0-9]{4}-(0[1-9]|1[0-2])-([0-2][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{0,8}[1-9])?Z$'
       OR effect->>'status'<>(CASE event_kind
            WHEN 'invitation_created' THEN 'active' WHEN 'invitation_revoked' THEN 'revoked' ELSE 'redeemed' END) THEN
        RAISE EXCEPTION 'invalid invitation effect' USING ERRCODE='22023';
    END IF;
    FOR account_name IN SELECT unnest(ARRAY[effect->>'account_id',effect->>'family_id',effect->>'invitation_id']) LOOP
        IF account_name !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
           OR account_name='00000000-0000-0000-0000-000000000000' OR account_name::uuid::text<>account_name THEN
            RAISE EXCEPTION 'invitation identifier is not canonical' USING ERRCODE='22023';
        END IF;
    END LOOP;
    account_name:=NULL;

    canonical_event:='{"version":"v1","generation":'||p_generation::text||',"revision":'||p_revision::text||
        ',"previous_hash":'||app.go_json_string(p_event->>'previous_hash')||',"command_id":'||app.go_json_string(p_event->>'command_id')||
        ',"command_hash":'||app.go_json_string(p_event->>'command_hash')||',"actor_account_id":'||app.go_json_string(p_event->>'actor_account_id')||
        ',"actor_device_id":'||app.go_json_string(p_event->>'actor_device_id')||',"kind":'||app.go_json_string(event_kind)||
        ',"at":'||app.go_json_string(p_event->>'at')||',"effects":['||
        '{"kind":'||app.go_json_string(effect->>'kind')||
        ',"account_id":'||app.go_json_string(effect->>'account_id')||
        ',"family_id":'||app.go_json_string(effect->>'family_id')||
        ',"invitation_id":'||app.go_json_string(effect->>'invitation_id')||
        ',"token_digest":'||app.go_json_string(effect->>'token_digest')||
        ',"role":'||app.go_json_string(effect->>'role')||
        ',"status":'||app.go_json_string(effect->>'status')||
        ',"expires_at":'||app.go_json_string(effect->>'expires_at')||'}]}';
    IF convert_from(p_event_bytes,'UTF8')<>canonical_event THEN
        RAISE EXCEPTION 'event bytes are not canonical' USING ERRCODE='22023'; END IF;

    SELECT generation,revision,event_hash INTO current_generation,current_revision,current_hash
      FROM public.security_projection_head WHERE singleton FOR UPDATE;
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
    PERFORM set_config('app.account_id',p_actor_account_id::text,true);
    PERFORM set_config('app.family_id',family_value::text,true);

    IF event_kind='invitation_created' THEN
        IF NOT EXISTS (SELECT FROM public.memberships WHERE family_id=family_value
            AND account_id=account_value AND role='owner' AND status='active') THEN
            RAISE EXCEPTION 'invitation creator is not owner' USING ERRCODE='23514';
        END IF;
        INSERT INTO public.invitations(id,family_id,creator_account_id,token_digest,role,status,
            expires_at,created_at,updated_at)
        VALUES((effect->>'invitation_id')::uuid,family_value,account_value,
            effect->>'token_digest',effect->>'role','active',(effect->>'expires_at')::timestamptz,event_at,event_at);
    ELSIF event_kind='invitation_revoked' THEN
        UPDATE public.invitations SET status='revoked',revoked_at=event_at,updated_at=event_at
         WHERE id=(effect->>'invitation_id')::uuid AND family_id=family_value
           AND creator_account_id=account_value AND token_digest=effect->>'token_digest'
           AND role=effect->>'role' AND expires_at=(effect->>'expires_at')::timestamptz AND status='active';
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'invitation is not active' USING ERRCODE='23514'; END IF;
    ELSE
        SELECT display_name INTO account_name FROM public.accounts
         WHERE id=account_value AND status='active';
        IF NOT FOUND THEN RAISE EXCEPTION 'invitation redeemer unavailable' USING ERRCODE='23514'; END IF;
        UPDATE public.invitations SET status='redeemed',redeemed_by_account_id=account_value,
            redeemed_at=event_at,updated_at=event_at
         WHERE id=(effect->>'invitation_id')::uuid AND family_id=family_value
           AND token_digest=effect->>'token_digest' AND role=effect->>'role'
           AND expires_at=(effect->>'expires_at')::timestamptz AND event_at < expires_at AND status='active';
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'invitation is not redeemable' USING ERRCODE='23514'; END IF;
        INSERT INTO public.memberships(family_id,account_id,role,status,created_at,updated_at)
        VALUES(family_value,account_value,effect->>'role','active',event_at,event_at)
        ON CONFLICT(family_id,account_id) DO UPDATE
           SET role=excluded.role,status='active',removed_at=NULL,updated_at=event_at
         WHERE memberships.role<>'owner' AND memberships.status='removed';
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'membership is not joinable' USING ERRCODE='23514'; END IF;
        INSERT INTO public.family_slots(account_id,family_id,state)
        VALUES(account_value,family_value,'active');
        INSERT INTO public.family_actor_labels(family_id,account_id,display_name,former,created_at,updated_at)
        VALUES(family_value,account_value,account_name,false,event_at,event_at)
        ON CONFLICT(family_id,account_id) DO UPDATE SET display_name=EXCLUDED.display_name,former=false,updated_at=EXCLUDED.updated_at;
        INSERT INTO public.family_actor_label_history(family_id,account_id,display_name,former,recorded_at)
        VALUES(family_value,account_value,account_name,false,event_at);
    END IF;

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
SELECT 16,principal,object_kind,object_identity,privileges
  FROM public.schema_privilege_manifest WHERE schema_version=15;

UPDATE public.schema_compatibility
   SET current_version=16,introduced_by='000016_invitation_roles.sql',recorded_at=now()
 WHERE singleton;
