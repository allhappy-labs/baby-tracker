CREATE TABLE security_projection_head (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    generation bigint NOT NULL CHECK (generation > 0),
    revision bigint NOT NULL CHECK (revision >= 0),
    event_hash bytea NOT NULL CHECK (octet_length(event_hash) = 32)
);
INSERT INTO security_projection_head VALUES (true, 1, 0, decode(repeat('00', 32), 'hex'));

CREATE TABLE security_command_receipts (
    actor_account_id uuid NOT NULL CHECK (actor_account_id <> '00000000-0000-0000-0000-000000000000'),
    command_id uuid NOT NULL CHECK (command_id <> '00000000-0000-0000-0000-000000000000'),
    command_hash bytea NOT NULL CHECK (octet_length(command_hash) = 32),
    generation bigint NOT NULL CHECK (generation > 0),
    revision bigint NOT NULL CHECK (revision > 0),
    result jsonb NOT NULL,
    UNIQUE (generation, revision),
    PRIMARY KEY (actor_account_id, command_id)
);

CREATE TABLE account_security_state (
    account_id uuid PRIMARY KEY CHECK (account_id <> '00000000-0000-0000-0000-000000000000'),
    identity_key text,
    status text NOT NULL CHECK (status IN ('active', 'identity_unavailable', 'deleted')),
    generation bigint NOT NULL CHECK (generation > 0),
    revision bigint NOT NULL CHECK (revision > 0),
    event_hash bytea NOT NULL CHECK (octet_length(event_hash) = 32),
    UNIQUE (identity_key),
    UNIQUE (generation, revision),
    UNIQUE (event_hash)
);

CREATE TABLE family_security_state (
    family_id uuid PRIMARY KEY CHECK (family_id <> '00000000-0000-0000-0000-000000000000'),
    owner_account_id uuid,
    lifecycle text NOT NULL CHECK (lifecycle IN ('active', 'deletion_pending', 'purge_started', 'purge_completed')),
    purge_after timestamptz,
    generation bigint NOT NULL CHECK (generation > 0),
    revision bigint NOT NULL CHECK (revision > 0),
    event_hash bytea NOT NULL CHECK (octet_length(event_hash) = 32),
    UNIQUE (generation, revision),
    UNIQUE (event_hash)
);

CREATE TABLE security_audit_events (
    generation bigint NOT NULL CHECK (generation > 0),
    revision bigint NOT NULL CHECK (revision > 0),
    event_hash bytea NOT NULL CHECK (octet_length(event_hash) = 32),
    command_id uuid NOT NULL CHECK (command_id <> '00000000-0000-0000-0000-000000000000'),
    actor_account_id uuid NOT NULL CHECK (actor_account_id <> '00000000-0000-0000-0000-000000000000'),
    kind text NOT NULL,
    event_data jsonb NOT NULL,
    event_bytes bytea NOT NULL CHECK (octet_length(event_bytes) BETWEEN 1 AND 65536),
    PRIMARY KEY (generation, revision),
    UNIQUE (event_hash)
);

REVOKE ALL ON security_projection_head, security_command_receipts,
    account_security_state, family_security_state, security_audit_events FROM PUBLIC;
REVOKE ALL ON security_projection_head, security_command_receipts,
    account_security_state, family_security_state, security_audit_events
    FROM baby_tracker_runtime, baby_tracker_projector;
GRANT USAGE ON SCHEMA app TO baby_tracker_projector;

CREATE FUNCTION app.go_json_string(p_value text) RETURNS text
LANGUAGE plpgsql IMMUTABLE STRICT
SET search_path = pg_catalog
AS $$
DECLARE
    encoded text := '"';
    character text;
    codepoint integer;
    character_index integer;
BEGIN
    FOR character_index IN 1..char_length(p_value)
    LOOP
        character := substr(p_value, character_index, 1);
        codepoint := ascii(character);
        CASE
            WHEN character = '"' THEN encoded := encoded || chr(92) || '"';
            WHEN character = chr(92) THEN encoded := encoded || chr(92) || chr(92);
            WHEN codepoint = 8 THEN encoded := encoded || chr(92) || 'b';
            WHEN codepoint = 9 THEN encoded := encoded || chr(92) || 't';
            WHEN codepoint = 10 THEN encoded := encoded || chr(92) || 'n';
            WHEN codepoint = 12 THEN encoded := encoded || chr(92) || 'f';
            WHEN codepoint = 13 THEN encoded := encoded || chr(92) || 'r';
            WHEN codepoint < 32 OR codepoint IN (38, 60, 62, 8232, 8233) THEN
                encoded := encoded || chr(92) || 'u' || lpad(to_hex(codepoint), 4, '0');
            ELSE encoded := encoded || character;
        END CASE;
    END LOOP;
    RETURN encoded || '"';
END;
$$;
REVOKE ALL ON FUNCTION app.go_json_string(text) FROM PUBLIC, baby_tracker_runtime, baby_tracker_projector;

CREATE FUNCTION app.project_security_event(
    p_generation bigint,
    p_revision bigint,
    p_previous_hash bytea,
    p_event_hash bytea,
    p_actor_account_id uuid,
    p_command_id uuid,
    p_command_hash bytea,
    p_result jsonb,
    p_event jsonb,
    p_event_bytes bytea
) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    current_generation bigint;
    current_revision bigint;
    current_hash bytea;
    effect jsonb;
    event_kind text;
    affected_rows bigint;
    anchor_count bigint := 0;
    canonical_effect text;
    canonical_effects text := '[';
    canonical_event text;
    first_effect boolean := true;
    identifier text;
    opaque_value text;
    previous_canonical_effect text;
    deletion_family text;
    deletion_deadline text;
    deletion_accounts jsonb := '{}'::jsonb;
BEGIN
    IF p_generation IS NULL OR p_revision IS NULL OR p_generation <= 0 OR p_revision <= 0
       OR p_generation > 9007199254740991 OR p_revision > 9007199254740991
       OR octet_length(p_previous_hash) IS DISTINCT FROM 32
       OR octet_length(p_event_hash) IS DISTINCT FROM 32
       OR octet_length(p_command_hash) IS DISTINCT FROM 32
       OR octet_length(p_event_bytes) NOT BETWEEN 1 AND 65536
       OR p_actor_account_id IS NULL OR p_actor_account_id = '00000000-0000-0000-0000-000000000000'
       OR p_command_id IS NULL OR p_command_id = '00000000-0000-0000-0000-000000000000' THEN
        RAISE EXCEPTION 'invalid security projection input' USING ERRCODE = '22023';
    END IF;
    IF sha256(p_event_bytes) IS DISTINCT FROM p_event_hash
       OR convert_from(p_event_bytes, 'UTF8')::jsonb IS DISTINCT FROM p_event
       OR jsonb_typeof(p_event) IS DISTINCT FROM 'object'
       OR (p_event - ARRAY['version','generation','revision','previous_hash','command_id','command_hash','actor_account_id','actor_device_id','kind','at','effects']::text[]) IS DISTINCT FROM '{}'::jsonb
       OR NOT (p_event ?& ARRAY['version','generation','revision','previous_hash','command_id','command_hash','actor_account_id','actor_device_id','kind','at','effects'])
       OR p_event->>'version' IS DISTINCT FROM 'v1'
       OR jsonb_typeof(p_event->'previous_hash') IS DISTINCT FROM 'string'
       OR jsonb_typeof(p_event->'command_id') IS DISTINCT FROM 'string'
       OR jsonb_typeof(p_event->'command_hash') IS DISTINCT FROM 'string'
       OR jsonb_typeof(p_event->'actor_account_id') IS DISTINCT FROM 'string'
       OR jsonb_typeof(p_event->'generation') IS DISTINCT FROM 'number'
       OR jsonb_typeof(p_event->'revision') IS DISTINCT FROM 'number'
       OR (p_event->>'generation')::bigint IS DISTINCT FROM p_generation
       OR (p_event->>'revision')::bigint IS DISTINCT FROM p_revision
       OR p_event->>'previous_hash' !~ '^[0-9a-f]{64}$'
       OR p_event->>'command_hash' !~ '^[0-9a-f]{64}$'
       OR decode(p_event->>'previous_hash', 'hex') IS DISTINCT FROM p_previous_hash
       OR decode(p_event->>'command_hash', 'hex') IS DISTINCT FROM p_command_hash
       OR p_event->>'actor_account_id' IS DISTINCT FROM p_actor_account_id::text
       OR p_event->>'command_id' IS DISTINCT FROM p_command_id::text
       OR (p_event->>'actor_account_id')::uuid IS DISTINCT FROM p_actor_account_id
       OR (p_event->>'command_id')::uuid IS DISTINCT FROM p_command_id
       OR jsonb_typeof(p_event->'actor_device_id') IS DISTINCT FROM 'string'
       OR (p_event->>'actor_device_id' <> '' AND (p_event->>'actor_device_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
           OR p_event->>'actor_device_id' = '00000000-0000-0000-0000-000000000000'
           OR (p_event->>'actor_device_id')::uuid::text IS DISTINCT FROM p_event->>'actor_device_id'))
       OR jsonb_typeof(p_event->'kind') IS DISTINCT FROM 'string'
       OR jsonb_typeof(p_event->'at') IS DISTINCT FROM 'string'
       OR p_event->>'at' !~ '^[0-9]{4}-(0[1-9]|1[0-2])-([0-2][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{0,8}[1-9])?Z$'
       OR (p_event->>'at')::timestamptz IS NULL
       OR jsonb_typeof(p_event->'effects') IS DISTINCT FROM 'array'
       OR jsonb_array_length(p_event->'effects') = 0 THEN
        RAISE EXCEPTION 'event envelope mismatch' USING ERRCODE = '22023';
    END IF;
    IF p_result IS DISTINCT FROM jsonb_build_object(
        'command_id', p_command_id::text, 'outcome', 'committed', 'code', 'ok', 'revision', p_revision
    ) THEN
        RAISE EXCEPTION 'command result mismatch' USING ERRCODE = '22023';
    END IF;
    event_kind := p_event->>'kind';
    IF event_kind NOT IN (
        'account_registered', 'identity_unavailable', 'identity_restored',
        'device_registered', 'device_binding_revoked', 'session_granted', 'session_rotated', 'session_revoked',
        'family_created', 'invitation_created', 'invitation_revoked', 'invitation_redeemed',
        'member_left', 'member_removed', 'owner_transferred', 'owner_recovered',
        'entitlement_granted', 'entitlement_suspended', 'family_deletion_scheduled', 'family_recovered',
        'recovery_forfeited', 'account_deleted', 'family_purge_started', 'family_purge_completed'
    ) THEN
        RAISE EXCEPTION 'unsupported security event' USING ERRCODE = '22023';
    END IF;
    IF event_kind NOT IN ('account_registered', 'identity_unavailable', 'identity_restored', 'family_created',
                          'family_deletion_scheduled', 'family_recovered', 'family_purge_started', 'family_purge_completed') THEN
        RAISE EXCEPTION 'projector case not installed for security event %', event_kind USING ERRCODE = '0A000';
    END IF;

    FOR effect IN SELECT value FROM jsonb_array_elements(p_event->'effects')
    LOOP
        IF jsonb_typeof(effect) IS DISTINCT FROM 'object' THEN
            RAISE EXCEPTION 'effect must be an object' USING ERRCODE = '22023';
        END IF;
        FOREACH identifier IN ARRAY ARRAY[effect->>'account_id', effect->>'family_id', effect->>'device_id',
            effect->>'session_id', effect->>'parent_session_id', effect->>'server_instance_id', effect->>'invitation_id']
        LOOP
            IF identifier IS NOT NULL AND (identifier !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                OR identifier = '00000000-0000-0000-0000-000000000000' OR identifier::uuid::text IS DISTINCT FROM identifier) THEN
                RAISE EXCEPTION 'effect identifier is not canonical' USING ERRCODE = '22023';
            END IF;
        END LOOP;
        FOREACH opaque_value IN ARRAY ARRAY[effect->>'canonical_origin', effect->>'identity_key', effect->>'token_digest',
            effect->>'device_credential_digest', effect->>'role', effect->>'status', effect->>'entitlement_key', effect->>'source']
        LOOP
            IF opaque_value IS NOT NULL AND octet_length(opaque_value) NOT BETWEEN 1 AND 4096 THEN
                RAISE EXCEPTION 'effect opaque value is outside byte bounds' USING ERRCODE = '22023';
            END IF;
        END LOOP;
        IF effect ? 'purge_after' AND (effect->>'purge_after' !~ '^[0-9]{4}-(0[1-9]|1[0-2])-([0-2][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{0,8}[1-9])?Z$'
            OR (effect->>'purge_after')::timestamptz IS NULL) THEN
            RAISE EXCEPTION 'effect timestamp is not canonical' USING ERRCODE = '22023';
        END IF;
        canonical_effect := CASE
            WHEN event_kind IN ('account_registered', 'identity_unavailable', 'identity_restored') THEN
                '{"kind":' || app.go_json_string(effect->>'kind') || ',"account_id":' || app.go_json_string(effect->>'account_id') ||
                ',"identity_key":' || app.go_json_string(effect->>'identity_key') || ',"status":' || app.go_json_string(effect->>'status') || '}'
            WHEN event_kind IN ('family_created', 'family_recovered') THEN
                '{"kind":' || app.go_json_string(effect->>'kind') || ',"account_id":' || app.go_json_string(effect->>'account_id') ||
                ',"family_id":' || app.go_json_string(effect->>'family_id') || ',"role":' || app.go_json_string(effect->>'role') ||
                ',"status":' || app.go_json_string(effect->>'status') || '}'
            WHEN event_kind = 'family_deletion_scheduled' THEN
                '{"kind":' || app.go_json_string(effect->>'kind') || ',"account_id":' || app.go_json_string(effect->>'account_id') ||
                ',"family_id":' || app.go_json_string(effect->>'family_id') || ',"role":' || app.go_json_string(effect->>'role') ||
                ',"status":' || app.go_json_string(effect->>'status') || ',"purge_after":' || app.go_json_string(effect->>'purge_after') || '}'
            WHEN event_kind = 'family_purge_started' THEN
                '{"kind":' || app.go_json_string(effect->>'kind') || ',"family_id":' || app.go_json_string(effect->>'family_id') ||
                ',"status":' || app.go_json_string(effect->>'status') || ',"purge_after":' || app.go_json_string(effect->>'purge_after') || '}'
            WHEN event_kind = 'family_purge_completed' THEN
                '{"kind":' || app.go_json_string(effect->>'kind') || ',"family_id":' || app.go_json_string(effect->>'family_id') ||
                ',"canonical_origin":' || app.go_json_string(effect->>'canonical_origin') || ',"status":' || app.go_json_string(effect->>'status') || '}'
        END;
        IF event_kind = 'family_deletion_scheduled' THEN
            IF deletion_family IS NULL THEN
                deletion_family := effect->>'family_id';
                deletion_deadline := effect->>'purge_after';
            ELSIF effect->>'family_id' IS DISTINCT FROM deletion_family OR effect->>'purge_after' IS DISTINCT FROM deletion_deadline THEN
                RAISE EXCEPTION 'family deletion effects disagree' USING ERRCODE = '22023';
            END IF;
            IF deletion_accounts ? (effect->>'account_id') THEN
                RAISE EXCEPTION 'family deletion account is duplicated' USING ERRCODE = '22023';
            END IF;
            deletion_accounts := deletion_accounts || jsonb_build_object(effect->>'account_id', true);
            IF previous_canonical_effect IS NOT NULL AND previous_canonical_effect >= canonical_effect THEN
                RAISE EXCEPTION 'family deletion effects are not canonical' USING ERRCODE = '22023';
            END IF;
            previous_canonical_effect := canonical_effect;
        END IF;
        IF NOT first_effect THEN canonical_effects := canonical_effects || ','; END IF;
        canonical_effects := canonical_effects || canonical_effect;
        first_effect := false;
    END LOOP;
    canonical_effects := canonical_effects || ']';
    canonical_event := '{"version":"v1","generation":' || p_generation::text || ',"revision":' || p_revision::text ||
        ',"previous_hash":' || app.go_json_string(p_event->>'previous_hash') || ',"command_id":' || app.go_json_string(p_event->>'command_id') ||
        ',"command_hash":' || app.go_json_string(p_event->>'command_hash') || ',"actor_account_id":' || app.go_json_string(p_event->>'actor_account_id') ||
        ',"actor_device_id":' || app.go_json_string(p_event->>'actor_device_id') || ',"kind":' || app.go_json_string(event_kind) ||
        ',"at":' || app.go_json_string(p_event->>'at') || ',"effects":' || canonical_effects || '}';
    IF convert_from(p_event_bytes, 'UTF8') IS DISTINCT FROM canonical_event THEN
        RAISE EXCEPTION 'event bytes are not canonical' USING ERRCODE = '22023';
    END IF;

    SELECT generation, revision, event_hash INTO current_generation, current_revision, current_hash
      FROM public.security_projection_head WHERE singleton FOR UPDATE;
    IF current_generation = p_generation AND current_revision = p_revision AND current_hash = p_event_hash THEN
        IF NOT EXISTS (
            SELECT FROM public.security_audit_events
             WHERE generation = p_generation AND revision = p_revision AND event_hash = p_event_hash
               AND event_bytes = p_event_bytes AND event_data = p_event
        ) THEN
            RAISE EXCEPTION 'projection head lacks exact audit receipt' USING ERRCODE = '23514';
        END IF;
        RETURN;
    END IF;
    IF current_generation IS DISTINCT FROM p_generation OR current_revision + 1 IS DISTINCT FROM p_revision
       OR current_hash IS DISTINCT FROM p_previous_hash THEN
        RAISE EXCEPTION 'security projection head mismatch' USING ERRCODE = '40001';
    END IF;

    FOR effect IN SELECT value FROM jsonb_array_elements(p_event->'effects')
    LOOP
        IF jsonb_typeof(effect) IS DISTINCT FROM 'object' OR effect->>'kind' IS DISTINCT FROM event_kind
           OR EXISTS (SELECT FROM jsonb_each(effect) AS item WHERE jsonb_typeof(item.value) IS DISTINCT FROM 'string') THEN
            RAISE EXCEPTION 'effect kind mismatch' USING ERRCODE = '22023';
        END IF;
        IF event_kind IN ('account_registered', 'identity_unavailable', 'identity_restored') THEN
            IF jsonb_array_length(p_event->'effects') <> 1
               OR (effect - ARRAY['kind','account_id','identity_key','status']::text[]) IS DISTINCT FROM '{}'::jsonb
               OR NOT (effect ?& ARRAY['kind','account_id','identity_key','status'])
               OR effect->>'identity_key' = ''
               OR effect->>'status' IS DISTINCT FROM (CASE WHEN event_kind = 'identity_unavailable' THEN 'identity_unavailable' ELSE 'active' END) THEN
                RAISE EXCEPTION 'invalid account security effect' USING ERRCODE = '22023';
            END IF;
            IF event_kind = 'account_registered' THEN
                INSERT INTO public.account_security_state (account_id, identity_key, status, generation, revision, event_hash)
                VALUES ((effect->>'account_id')::uuid, effect->>'identity_key', 'active', p_generation, p_revision, p_event_hash);
            ELSE
                UPDATE public.account_security_state SET status = effect->>'status', generation = p_generation,
                    revision = p_revision, event_hash = p_event_hash
                    WHERE account_id = (effect->>'account_id')::uuid AND identity_key = effect->>'identity_key';
                GET DIAGNOSTICS affected_rows = ROW_COUNT;
                IF affected_rows <> 1 THEN RAISE EXCEPTION 'account security prerequisite missing' USING ERRCODE = '23514'; END IF;
            END IF;
        ELSIF event_kind = 'family_created' THEN
            IF jsonb_array_length(p_event->'effects') <> 1
               OR (effect - ARRAY['kind','account_id','family_id','role','status']::text[]) IS DISTINCT FROM '{}'::jsonb
               OR NOT (effect ?& ARRAY['kind','account_id','family_id','role','status'])
               OR effect->>'role' IS DISTINCT FROM 'owner' OR effect->>'status' IS DISTINCT FROM 'active' THEN
                RAISE EXCEPTION 'invalid family creation effect' USING ERRCODE = '22023';
            END IF;
            INSERT INTO public.family_security_state (family_id, owner_account_id, lifecycle, generation, revision, event_hash)
            VALUES ((effect->>'family_id')::uuid, (effect->>'account_id')::uuid, 'active', p_generation, p_revision, p_event_hash);
        ELSIF event_kind = 'family_deletion_scheduled' THEN
            IF (effect - ARRAY['kind','account_id','family_id','role','status','purge_after']::text[]) IS DISTINCT FROM '{}'::jsonb
               OR NOT (effect ?& ARRAY['kind','account_id','family_id','role','status','purge_after'])
               OR effect->>'role' NOT IN ('owner','caregiver') OR effect->>'status' NOT IN ('deletion_pending','former')
               OR (effect->>'status' = 'deletion_pending' AND effect->>'role' <> 'owner') THEN
                RAISE EXCEPTION 'invalid family deletion effect' USING ERRCODE = '22023';
            END IF;
            IF effect->>'status' = 'deletion_pending' THEN
                anchor_count := anchor_count + 1;
                UPDATE public.family_security_state SET lifecycle = 'deletion_pending', purge_after = (effect->>'purge_after')::timestamptz,
                    generation = p_generation, revision = p_revision, event_hash = p_event_hash
                    WHERE family_id = (effect->>'family_id')::uuid AND owner_account_id = (effect->>'account_id')::uuid AND lifecycle = 'active';
                GET DIAGNOSTICS affected_rows = ROW_COUNT;
                IF affected_rows <> 1 THEN RAISE EXCEPTION 'family deletion prerequisite missing' USING ERRCODE = '23514'; END IF;
            END IF;
        ELSIF event_kind = 'family_recovered' THEN
            IF jsonb_array_length(p_event->'effects') <> 1
               OR (effect - ARRAY['kind','account_id','family_id','role','status']::text[]) IS DISTINCT FROM '{}'::jsonb
               OR NOT (effect ?& ARRAY['kind','account_id','family_id','role','status'])
               OR effect->>'role' IS DISTINCT FROM 'owner' OR effect->>'status' IS DISTINCT FROM 'active' THEN
                RAISE EXCEPTION 'invalid family recovery effect' USING ERRCODE = '22023';
            END IF;
            UPDATE public.family_security_state SET lifecycle = 'active', owner_account_id = (effect->>'account_id')::uuid,
                purge_after = NULL, generation = p_generation, revision = p_revision, event_hash = p_event_hash
                WHERE family_id = (effect->>'family_id')::uuid AND lifecycle = 'deletion_pending';
            GET DIAGNOSTICS affected_rows = ROW_COUNT;
            IF affected_rows <> 1 THEN RAISE EXCEPTION 'family is not recoverable' USING ERRCODE = '23514'; END IF;
        ELSIF event_kind = 'family_purge_started' THEN
            IF jsonb_array_length(p_event->'effects') <> 1
               OR (effect - ARRAY['kind','family_id','status','purge_after']::text[]) IS DISTINCT FROM '{}'::jsonb
               OR NOT (effect ?& ARRAY['kind','family_id','status','purge_after'])
               OR effect->>'status' IS DISTINCT FROM 'purge_started' THEN
                RAISE EXCEPTION 'invalid purge start effect' USING ERRCODE = '22023';
            END IF;
            UPDATE public.family_security_state SET lifecycle = 'purge_started', generation = p_generation,
                revision = p_revision, event_hash = p_event_hash
                WHERE family_id = (effect->>'family_id')::uuid AND lifecycle = 'deletion_pending'
                  AND purge_after::text = (effect->>'purge_after')::timestamptz::text
                  AND purge_after <= (p_event->>'at')::timestamptz;
            GET DIAGNOSTICS affected_rows = ROW_COUNT;
            IF affected_rows <> 1 THEN RAISE EXCEPTION 'family purge is not due' USING ERRCODE = '23514'; END IF;
        ELSIF event_kind = 'family_purge_completed' THEN
            IF jsonb_array_length(p_event->'effects') <> 1
               OR (effect - ARRAY['kind','family_id','canonical_origin','status']::text[]) IS DISTINCT FROM '{}'::jsonb
               OR NOT (effect ?& ARRAY['kind','family_id','canonical_origin','status'])
               OR effect->>'canonical_origin' = '' OR effect->>'status' IS DISTINCT FROM 'purge_completed' THEN
                RAISE EXCEPTION 'invalid purge completion effect' USING ERRCODE = '22023';
            END IF;
            UPDATE public.family_security_state SET lifecycle = 'purge_completed', generation = p_generation,
                revision = p_revision, event_hash = p_event_hash
                WHERE family_id = (effect->>'family_id')::uuid AND lifecycle = 'purge_started';
            GET DIAGNOSTICS affected_rows = ROW_COUNT;
            IF affected_rows <> 1 THEN RAISE EXCEPTION 'family purge was not started' USING ERRCODE = '23514'; END IF;
        END IF;
    END LOOP;
    IF event_kind = 'family_deletion_scheduled' AND anchor_count <> 1 THEN
        RAISE EXCEPTION 'family deletion requires one anchor' USING ERRCODE = '22023';
    END IF;

    INSERT INTO public.security_audit_events
        (generation, revision, event_hash, command_id, actor_account_id, kind, event_data, event_bytes)
    VALUES (p_generation, p_revision, p_event_hash, p_command_id, p_actor_account_id, event_kind, p_event, p_event_bytes);
    INSERT INTO public.security_command_receipts
        (actor_account_id, command_id, command_hash, generation, revision, result)
    VALUES (p_actor_account_id, p_command_id, p_command_hash, p_generation, p_revision, p_result);
    UPDATE public.security_projection_head SET generation = p_generation, revision = p_revision, event_hash = p_event_hash
      WHERE singleton;
END;
$$;

REVOKE ALL ON FUNCTION app.project_security_event(bigint, bigint, bytea, bytea, uuid, uuid, bytea, jsonb, jsonb, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.project_security_event(bigint, bigint, bytea, bytea, uuid, uuid, bytea, jsonb, jsonb, bytea)
    TO baby_tracker_projector;
GRANT SELECT ON security_projection_head TO baby_tracker_projector;
