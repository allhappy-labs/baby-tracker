ALTER TABLE public.families
    ADD COLUMN profile_required boolean NOT NULL DEFAULT false;

CREATE TABLE public.family_slots (
    account_id uuid PRIMARY KEY REFERENCES public.accounts(id),
    family_id uuid NOT NULL REFERENCES public.families(id),
    state text NOT NULL CHECK (state IN ('active', 'family_recovery')),
    CHECK (account_id <> '00000000-0000-0000-0000-000000000000'),
    CHECK (family_id <> '00000000-0000-0000-0000-000000000000')
);
CREATE INDEX family_slots_family_id_idx ON public.family_slots(family_id);

CREATE TABLE public.invitations (
    id uuid PRIMARY KEY CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    family_id uuid NOT NULL REFERENCES public.families(id),
    creator_account_id uuid NOT NULL REFERENCES public.accounts(id),
    token_digest text NOT NULL UNIQUE
        CHECK (token_digest ~ '^hmac-sha256:[a-z][a-z0-9._-]{0,63}:[0-9a-f]{64}$'),
    role text NOT NULL CHECK (role = 'caregiver'),
    status text NOT NULL CHECK (status IN ('active', 'redeemed', 'revoked')),
    expires_at timestamptz NOT NULL CHECK (isfinite(expires_at)),
    redeemed_by_account_id uuid REFERENCES public.accounts(id),
    redeemed_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL CHECK (isfinite(created_at)),
    updated_at timestamptz NOT NULL CHECK (isfinite(updated_at)),
    CHECK ((status = 'active' AND redeemed_by_account_id IS NULL AND redeemed_at IS NULL AND revoked_at IS NULL)
        OR (status = 'redeemed' AND redeemed_by_account_id IS NOT NULL AND redeemed_at IS NOT NULL AND revoked_at IS NULL)
        OR (status = 'revoked' AND redeemed_by_account_id IS NULL AND redeemed_at IS NULL AND revoked_at IS NOT NULL)),
    CHECK ((redeemed_at IS NULL OR isfinite(redeemed_at)) AND (revoked_at IS NULL OR isfinite(revoked_at)))
);
CREATE INDEX invitations_family_status_idx ON public.invitations(family_id, status);
CREATE INDEX invitations_expiry_idx ON public.invitations(expires_at) WHERE status = 'active';

CREATE TABLE public.family_entitlements (
    family_id uuid NOT NULL REFERENCES public.families(id),
    entitlement_key text NOT NULL CHECK (entitlement_key ~ '^[a-z][a-z0-9_]{0,63}$'),
    source text NOT NULL CHECK (source ~ '^[a-z][a-z0-9_]{0,63}$'),
    status text NOT NULL CHECK (status IN ('active', 'suspended')),
    expires_at timestamptz CHECK (expires_at IS NULL OR isfinite(expires_at)),
    created_at timestamptz NOT NULL CHECK (isfinite(created_at)),
    updated_at timestamptz NOT NULL CHECK (isfinite(updated_at)),
    PRIMARY KEY (family_id, entitlement_key)
);

CREATE TABLE public.family_actor_labels (
    family_id uuid NOT NULL REFERENCES public.families(id),
    account_id uuid NOT NULL CHECK (account_id <> '00000000-0000-0000-0000-000000000000'),
    display_name text NOT NULL CHECK (char_length(display_name) <= 80 AND octet_length(display_name) <= 320),
    former boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL CHECK (isfinite(created_at)),
    updated_at timestamptz NOT NULL CHECK (isfinite(updated_at)),
    PRIMARY KEY (family_id, account_id)
);
CREATE INDEX family_actor_labels_page_idx ON public.family_actor_labels(family_id, account_id);

CREATE TABLE public.account_profile_commands (
    account_id uuid NOT NULL REFERENCES public.accounts(id),
    command_id uuid NOT NULL CHECK (command_id <> '00000000-0000-0000-0000-000000000000'),
    input_hash bytea NOT NULL CHECK (octet_length(input_hash) = 32),
    created_at timestamptz NOT NULL CHECK (isfinite(created_at)),
    PRIMARY KEY (account_id, command_id)
);

CREATE TABLE public.family_actor_label_history (
    label_version bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY
        CHECK (label_version > 0 AND label_version <= 9007199254740991),
    family_id uuid NOT NULL REFERENCES public.families(id),
    account_id uuid NOT NULL CHECK (account_id <> '00000000-0000-0000-0000-000000000000'),
    display_name text NOT NULL CHECK (char_length(display_name) <= 80 AND octet_length(display_name) <= 320),
    former boolean NOT NULL,
    recorded_at timestamptz NOT NULL CHECK (isfinite(recorded_at))
);
CREATE INDEX family_actor_label_history_page_idx
    ON public.family_actor_label_history(family_id, account_id, label_version DESC);
CREATE INDEX family_actor_label_history_high_water_idx
    ON public.family_actor_label_history(family_id, label_version DESC);

CREATE TABLE public.family_command_material (
    command_id uuid PRIMARY KEY CHECK (command_id <> '00000000-0000-0000-0000-000000000000'),
    command_hash bytea NOT NULL CHECK (octet_length(command_hash) = 32),
    projection_material_digest bytea NOT NULL CHECK (octet_length(projection_material_digest) = 32),
    material_kind text NOT NULL CHECK (material_kind IN ('family_create','family_profile_repair')),
    family_id uuid NOT NULL CHECK (family_id <> '00000000-0000-0000-0000-000000000000'),
    account_id uuid NOT NULL CHECK (account_id <> '00000000-0000-0000-0000-000000000000'),
    family_display_name text,
    material_bytes bytea,
    created_at timestamptz NOT NULL CHECK (isfinite(created_at)),
    expires_at timestamptz NOT NULL CHECK (isfinite(expires_at) AND expires_at = created_at + interval '10 minutes'),
    projected_at timestamptz CHECK (projected_at IS NULL OR isfinite(projected_at)),
    CHECK ((projected_at IS NULL AND family_display_name IS NOT NULL AND material_bytes IS NOT NULL
            AND octet_length(material_bytes) BETWEEN 1 AND 1024
            AND char_length(family_display_name) BETWEEN 1 AND 80 AND octet_length(family_display_name) <= 320)
        OR (projected_at IS NOT NULL AND family_display_name IS NULL AND material_bytes IS NULL))
);
CREATE UNIQUE INDEX family_command_material_one_pending_repair_idx
    ON public.family_command_material(family_id)
    WHERE material_kind='family_profile_repair' AND projected_at IS NULL;

ALTER TABLE public.family_slots ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.family_slots FORCE ROW LEVEL SECURITY;
ALTER TABLE public.invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.invitations FORCE ROW LEVEL SECURITY;
ALTER TABLE public.family_entitlements ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.family_entitlements FORCE ROW LEVEL SECURITY;
ALTER TABLE public.family_actor_labels ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.family_actor_labels FORCE ROW LEVEL SECURITY;
ALTER TABLE public.account_profile_commands ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.account_profile_commands FORCE ROW LEVEL SECURITY;
ALTER TABLE public.family_actor_label_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.family_actor_label_history FORCE ROW LEVEL SECURITY;
ALTER TABLE public.family_command_material ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.family_command_material FORCE ROW LEVEL SECURITY;

CREATE POLICY own_family_slot ON public.family_slots
    USING (account_id = app.current_account_id())
    WITH CHECK (account_id = app.current_account_id());
CREATE POLICY invitation_family_context ON public.invitations
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());
CREATE POLICY entitlement_family_context ON public.family_entitlements
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());
CREATE POLICY actor_label_family_context ON public.family_actor_labels
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());
CREATE POLICY own_account_profile_command ON public.account_profile_commands
    USING (account_id = app.current_account_id())
    WITH CHECK (account_id = app.current_account_id());
CREATE POLICY actor_label_history_family_context ON public.family_actor_label_history
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_material_context ON public.family_command_material
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());
CREATE POLICY invitation_preview_owner ON public.invitations FOR SELECT
    USING (app.current_account_id() IS NOT NULL
        AND CURRENT_USER::regrole = (SELECT relowner FROM pg_class WHERE oid='public.invitations'::regclass));

REVOKE INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER ON public.accounts, public.families, public.memberships
    FROM baby_tracker_runtime;
REVOKE ALL ON TABLE public.family_slots, public.invitations, public.family_entitlements,
    public.family_actor_labels, public.family_actor_label_history, public.account_profile_commands,
    public.family_command_material
    FROM PUBLIC, baby_tracker_runtime, baby_tracker_projector;
REVOKE ALL ON SEQUENCE public.family_actor_label_history_label_version_seq
    FROM PUBLIC, baby_tracker_runtime, baby_tracker_projector;
GRANT SELECT ON public.accounts, public.families, public.memberships, public.family_slots,
    public.invitations, public.family_entitlements, public.family_actor_labels
    TO baby_tracker_runtime;

CREATE FUNCTION app.valid_display_name(p_value text)
RETURNS boolean
LANGUAGE sql IMMUTABLE
SET search_path = pg_catalog
AS $$
    SELECT p_value IS NOT NULL
       AND char_length(p_value) BETWEEN 1 AND 80
       AND octet_length(p_value) <= 320
       AND p_value !~ U&'(^[\0009-\000D\0020\0085\00A0\1680\2000-\200A\2028\2029\202F\205F\3000])|([\0009-\000D\0020\0085\00A0\1680\2000-\200A\2028\2029\202F\205F\3000]$)'
       AND p_value !~ U&'[\0001-\001F\007F-\009F]'
$$;
REVOKE ALL ON FUNCTION app.valid_display_name(text)
    FROM PUBLIC, baby_tracker_runtime, baby_tracker_projector;

CREATE FUNCTION app.current_account_profile(p_account_id uuid)
RETURNS TABLE(account_id uuid, display_name text, authority_status text, profile_required boolean,
    family_id uuid, slot_state text)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = pg_catalog
AS $$
BEGIN
    IF p_account_id IS NULL OR p_account_id IS DISTINCT FROM app.current_account_id() THEN
        RAISE EXCEPTION 'account context mismatch' USING ERRCODE = '42501';
    END IF;
    RETURN QUERY
    SELECT a.id, a.display_name, a.status, a.profile_required, s.family_id, s.state
      FROM public.accounts a
      LEFT JOIN public.family_slots s ON s.account_id = a.id
     WHERE a.id = p_account_id;
END;
$$;

CREATE FUNCTION app.family_actor_labels_high_water()
RETURNS bigint
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = pg_catalog
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

CREATE FUNCTION app.list_family_actor_labels(
    p_family_id uuid,
    p_high_water bigint,
    p_after_account_id uuid,
    p_limit integer
) RETURNS TABLE(account_id uuid, display_name text, former boolean, high_water bigint, has_more boolean)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = pg_catalog
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

CREATE FUNCTION app.lookup_family_invitation(p_token_digest text)
RETURNS TABLE(invitation_id uuid, family_id uuid, family_display_name text, status text,
    role text, expires_at timestamptz)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    item record;
    preview_name text;
    previous_family text := coalesce(current_setting('app.family_id',true),'');
BEGIN
    IF app.current_account_id() IS NULL
       OR p_token_digest IS NULL
       OR p_token_digest !~ '^hmac-sha256:[a-z][a-z0-9._-]{0,63}:[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'invalid invitation lookup' USING ERRCODE = '22023';
    END IF;
    SELECT i.id,i.family_id,i.status,i.role,i.expires_at INTO item
      FROM public.invitations i WHERE i.token_digest=p_token_digest;
    IF NOT FOUND THEN RETURN; END IF;
    PERFORM set_config('app.family_id',item.family_id::text,true);
    SELECT f.display_name INTO preview_name FROM public.families f
     WHERE f.id=item.family_id AND f.status='active';
    PERFORM set_config('app.family_id',previous_family,true);
    IF preview_name IS NULL THEN RETURN; END IF;
    invitation_id:=item.id;
    family_id:=item.family_id;
    family_display_name:=preview_name;
    status:=item.status;
    role:=item.role;
    expires_at:=item.expires_at;
    RETURN NEXT;
END;
$$;

CREATE FUNCTION app.stage_family_material(
    p_command_id uuid,
    p_command_hash bytea,
    p_projection_material_digest bytea,
    p_material_bytes bytea,
    p_now timestamptz
) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    affected_rows bigint;
    material jsonb;
    material_kind text;
    target_family_id uuid;
    actor_account_id uuid;
    family_display_name text;
    canonical_material text;
    violated_constraint text;
    previous_family text := coalesce(current_setting('app.family_id',true),'');
BEGIN
    BEGIN
        material := convert_from(p_material_bytes,'UTF8')::jsonb;
    EXCEPTION WHEN OTHERS THEN
        RAISE EXCEPTION 'invalid family material' USING ERRCODE = '22023';
    END;
    material_kind := material->>'kind';
    family_display_name := material->>'display_name';
    IF p_command_id IS NULL OR p_command_id = '00000000-0000-0000-0000-000000000000'
       OR octet_length(p_command_hash) IS DISTINCT FROM 32
       OR octet_length(p_projection_material_digest) IS DISTINCT FROM 32
       OR sha256(p_material_bytes) IS DISTINCT FROM p_projection_material_digest
       OR octet_length(p_material_bytes) NOT BETWEEN 1 AND 1024
       OR jsonb_typeof(material) IS DISTINCT FROM 'object'
       OR (material-ARRAY['version','kind','family_id','actor_account_id','display_name']::text[]) IS DISTINCT FROM '{}'::jsonb
       OR NOT (material ?& ARRAY['version','kind','family_id','actor_account_id','display_name'])
       OR EXISTS (SELECT FROM jsonb_each(material) item WHERE jsonb_typeof(item.value) IS DISTINCT FROM 'string')
       OR material->>'version' IS DISTINCT FROM 'v1'
       OR material_kind NOT IN ('family_create','family_profile_repair')
       OR material->>'family_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR material->>'family_id' = '00000000-0000-0000-0000-000000000000'
       OR (material->>'family_id')::uuid::text IS DISTINCT FROM material->>'family_id'
       OR material->>'actor_account_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR material->>'actor_account_id' = '00000000-0000-0000-0000-000000000000'
       OR (material->>'actor_account_id')::uuid::text IS DISTINCT FROM material->>'actor_account_id'
       OR (material->>'actor_account_id')::uuid IS DISTINCT FROM app.current_account_id()
       OR NOT app.valid_display_name(family_display_name) OR NOT isfinite(p_now) THEN
        RAISE EXCEPTION 'invalid family material' USING ERRCODE = '22023';
    END IF;
    target_family_id := (material->>'family_id')::uuid;
    actor_account_id := (material->>'actor_account_id')::uuid;
    canonical_material := '{"version":"v1","kind":' || app.go_json_string(material_kind) ||
        ',"family_id":' || app.go_json_string(target_family_id::text) ||
        ',"actor_account_id":' || app.go_json_string(actor_account_id::text) ||
        ',"display_name":' || app.go_json_string(family_display_name) || '}';
    IF convert_from(p_material_bytes,'UTF8') IS DISTINCT FROM canonical_material THEN
        RAISE EXCEPTION 'family material is not canonical' USING ERRCODE = '22023';
    END IF;
    PERFORM set_config('app.family_id',target_family_id::text,true);
    IF material_kind='family_profile_repair' THEN
        IF NOT EXISTS (SELECT FROM public.families f
            JOIN public.memberships m ON m.family_id=f.id
            WHERE f.id=target_family_id AND f.status='active' AND f.profile_required=true
              AND m.account_id=actor_account_id AND m.role='owner' AND m.status='active') THEN
            RAISE EXCEPTION 'family profile repair is not required'
                USING ERRCODE='BT001', CONSTRAINT='family_command_material_one_pending_repair_idx';
        END IF;
        DELETE FROM public.family_command_material item
         WHERE item.family_id=target_family_id AND item.material_kind='family_profile_repair'
           AND item.projected_at IS NULL AND item.expires_at<p_now;
    END IF;
    BEGIN
        INSERT INTO public.family_command_material(command_id,command_hash,projection_material_digest,
            material_kind,family_id,account_id,family_display_name,material_bytes,created_at,expires_at)
        VALUES(p_command_id,p_command_hash,p_projection_material_digest,material_kind,target_family_id,actor_account_id,
            family_display_name,p_material_bytes,p_now,p_now+interval '10 minutes')
        ON CONFLICT(command_id) DO UPDATE SET command_id=EXCLUDED.command_id
          WHERE public.family_command_material.command_hash=EXCLUDED.command_hash
            AND public.family_command_material.projection_material_digest=EXCLUDED.projection_material_digest
            AND public.family_command_material.material_kind=EXCLUDED.material_kind
            AND public.family_command_material.family_id=EXCLUDED.family_id
            AND public.family_command_material.account_id=EXCLUDED.account_id
            AND public.family_command_material.family_display_name=EXCLUDED.family_display_name
            AND public.family_command_material.material_bytes=EXCLUDED.material_bytes
            AND public.family_command_material.projected_at IS NULL
            AND public.family_command_material.expires_at >= p_now;
        GET DIAGNOSTICS affected_rows = ROW_COUNT;
    EXCEPTION WHEN unique_violation THEN
        GET STACKED DIAGNOSTICS violated_constraint = CONSTRAINT_NAME;
        IF violated_constraint='family_command_material_one_pending_repair_idx' THEN
            RAISE EXCEPTION 'family profile repair is already pending'
                USING ERRCODE='BT001', CONSTRAINT='family_command_material_one_pending_repair_idx';
        END IF;
        RAISE;
    END;
    IF affected_rows <> 1 THEN
        RAISE EXCEPTION 'family material conflict' USING ERRCODE = '23505';
    END IF;
    PERFORM set_config('app.family_id',previous_family,true);
END;
$$;

CREATE FUNCTION app.update_own_profile(p_account_id uuid, p_command_id uuid, p_display_name text, p_now timestamptz)
RETURNS void
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog
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

REVOKE ALL ON FUNCTION app.current_account_profile(uuid), app.lookup_family_invitation(text),
    app.family_actor_labels_high_water(), app.list_family_actor_labels(uuid,bigint,uuid,integer),
    app.stage_family_material(uuid,bytea,bytea,bytea,timestamptz),
    app.update_own_profile(uuid,uuid,text,timestamptz)
    FROM PUBLIC, baby_tracker_projector;
GRANT EXECUTE ON FUNCTION app.current_account_profile(uuid), app.lookup_family_invitation(text),
    app.family_actor_labels_high_water(), app.list_family_actor_labels(uuid,bigint,uuid,integer),
    app.stage_family_material(uuid,bytea,bytea,bytea,timestamptz),
    app.update_own_profile(uuid,uuid,text,timestamptz)
    TO baby_tracker_runtime;

ALTER FUNCTION app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    RENAME TO project_security_event_v5;
REVOKE ALL ON FUNCTION app.project_security_event_v5(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    FROM PUBLIC, baby_tracker_runtime, baby_tracker_projector;

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
    event_kind text;
    effect jsonb;
    canonical_effect text;
    canonical_effects text := '[';
    canonical_event text;
    first_effect boolean := true;
    identifier text;
    affected_rows bigint;
    family_effect jsonb;
    entitlement_effect jsonb;
    old_owner_effect jsonb;
    new_owner_effect jsonb;
    family_count integer := 0;
    entitlement_count integer := 0;
    old_owner_count integer := 0;
    new_owner_count integer := 0;
    stage_record record;
    stage_exists boolean := false;
    presentation_missing boolean := false;
    family_name text;
    account_name text;
    event_at timestamptz;
    previous_account_setting text := coalesce(current_setting('app.account_id',true),'');
    previous_family_setting text := coalesce(current_setting('app.family_id',true),'');
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
       OR jsonb_typeof(p_event->'generation') IS DISTINCT FROM 'number'
       OR jsonb_typeof(p_event->'revision') IS DISTINCT FROM 'number'
       OR (p_event->>'generation')::bigint IS DISTINCT FROM p_generation
       OR (p_event->>'revision')::bigint IS DISTINCT FROM p_revision
       OR p_event->>'previous_hash' !~ '^[0-9a-f]{64}$'
       OR p_event->>'command_hash' !~ '^[0-9a-f]{64}$'
       OR decode(p_event->>'previous_hash','hex') IS DISTINCT FROM p_previous_hash
       OR decode(p_event->>'command_hash','hex') IS DISTINCT FROM p_command_hash
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
       OR jsonb_typeof(p_event->'effects') IS DISTINCT FROM 'array'
       OR jsonb_array_length(p_event->'effects') = 0 THEN
        RAISE EXCEPTION 'event envelope mismatch' USING ERRCODE = '22023';
    END IF;
    event_at := (p_event->>'at')::timestamptz;
    IF event_at IS NULL OR NOT isfinite(event_at) THEN
        RAISE EXCEPTION 'event time mismatch' USING ERRCODE = '22023';
    END IF;
    IF p_result IS DISTINCT FROM jsonb_build_object(
        'command_id',p_command_id::text,'outcome','committed','code','ok','revision',p_revision) THEN
        RAISE EXCEPTION 'command result mismatch' USING ERRCODE = '22023';
    END IF;

    event_kind := p_event->>'kind';
    IF event_kind NOT IN ('family_created','family_bootstrapped','family_profile_repaired','invitation_created','invitation_revoked',
                          'invitation_redeemed','member_left','member_removed','owner_transferred',
                          'entitlement_granted','entitlement_suspended') THEN
        PERFORM app.project_security_event_v5(p_generation,p_revision,p_previous_hash,p_event_hash,
            p_actor_account_id,p_command_id,p_command_hash,p_result,p_event,p_event_bytes);
        RETURN;
    END IF;

    FOR effect IN SELECT value FROM jsonb_array_elements(p_event->'effects')
    LOOP
        IF jsonb_typeof(effect) IS DISTINCT FROM 'object'
           OR EXISTS (SELECT FROM jsonb_each(effect) item WHERE jsonb_typeof(item.value) IS DISTINCT FROM 'string') THEN
            RAISE EXCEPTION 'effect must contain only strings' USING ERRCODE = '22023';
        END IF;
        FOREACH identifier IN ARRAY ARRAY[effect->>'account_id',effect->>'family_id',effect->>'invitation_id']
        LOOP
            IF identifier IS NOT NULL AND (identifier !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                OR identifier = '00000000-0000-0000-0000-000000000000' OR identifier::uuid::text IS DISTINCT FROM identifier) THEN
                RAISE EXCEPTION 'effect identifier is not canonical' USING ERRCODE = '22023';
            END IF;
        END LOOP;

        CASE effect->>'kind'
        WHEN 'family_created' THEN
            IF (effect - ARRAY['kind','account_id','family_id','projection_material_digest','role','status']::text[]) IS DISTINCT FROM '{}'::jsonb
               OR NOT (effect ?& ARRAY['kind','account_id','family_id','projection_material_digest','role','status'])
               OR effect->>'projection_material_digest' !~ '^[0-9a-f]{64}$'
               OR effect->>'role' IS DISTINCT FROM 'owner' OR effect->>'status' IS DISTINCT FROM 'active' THEN
                RAISE EXCEPTION 'invalid family creation effect' USING ERRCODE = '22023';
            END IF;
            family_count := family_count + 1;
            family_effect := effect;
            canonical_effect := '{"kind":' || app.go_json_string(effect->>'kind') ||
                ',"account_id":' || app.go_json_string(effect->>'account_id') ||
                ',"family_id":' || app.go_json_string(effect->>'family_id') ||
                ',"projection_material_digest":' || app.go_json_string(effect->>'projection_material_digest') ||
                ',"role":' || app.go_json_string(effect->>'role') ||
                ',"status":' || app.go_json_string(effect->>'status') || '}';
        WHEN 'family_profile_repaired' THEN
            IF (effect - ARRAY['kind','account_id','family_id','projection_material_digest']::text[]) IS DISTINCT FROM '{}'::jsonb
               OR NOT (effect ?& ARRAY['kind','account_id','family_id','projection_material_digest'])
               OR effect->>'projection_material_digest' !~ '^[0-9a-f]{64}$' THEN
                RAISE EXCEPTION 'invalid family profile repair effect' USING ERRCODE = '22023';
            END IF;
            family_count := family_count + 1;
            family_effect := effect;
            canonical_effect := '{"kind":' || app.go_json_string(effect->>'kind') ||
                ',"account_id":' || app.go_json_string(effect->>'account_id') ||
                ',"family_id":' || app.go_json_string(effect->>'family_id') ||
                ',"projection_material_digest":' || app.go_json_string(effect->>'projection_material_digest') || '}';
        WHEN 'invitation_created','invitation_revoked','invitation_redeemed' THEN
            IF (effect - ARRAY['kind','account_id','family_id','invitation_id','token_digest','role','status','expires_at']::text[]) IS DISTINCT FROM '{}'::jsonb
               OR NOT (effect ?& ARRAY['kind','account_id','family_id','invitation_id','token_digest','role','status','expires_at'])
               OR effect->>'token_digest' !~ '^hmac-sha256:[a-z][a-z0-9._-]{0,63}:[0-9a-f]{64}$'
               OR effect->>'role' IS DISTINCT FROM 'caregiver'
               OR effect->>'expires_at' !~ '^[0-9]{4}-(0[1-9]|1[0-2])-([0-2][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{0,8}[1-9])?Z$'
               OR effect->>'status' IS DISTINCT FROM (CASE effect->>'kind'
                    WHEN 'invitation_created' THEN 'active' WHEN 'invitation_revoked' THEN 'revoked' ELSE 'redeemed' END) THEN
                RAISE EXCEPTION 'invalid invitation effect' USING ERRCODE = '22023';
            END IF;
            canonical_effect := '{"kind":' || app.go_json_string(effect->>'kind') ||
                ',"account_id":' || app.go_json_string(effect->>'account_id') ||
                ',"family_id":' || app.go_json_string(effect->>'family_id') ||
                ',"invitation_id":' || app.go_json_string(effect->>'invitation_id') ||
                ',"token_digest":' || app.go_json_string(effect->>'token_digest') ||
                ',"role":' || app.go_json_string(effect->>'role') ||
                ',"status":' || app.go_json_string(effect->>'status') ||
                ',"expires_at":' || app.go_json_string(effect->>'expires_at') || '}';
        WHEN 'member_left','member_removed' THEN
            IF (effect - ARRAY['kind','account_id','family_id','status']::text[]) IS DISTINCT FROM '{}'::jsonb
               OR NOT (effect ?& ARRAY['kind','account_id','family_id','status'])
               OR effect->>'status' IS DISTINCT FROM 'removed' THEN
                RAISE EXCEPTION 'invalid membership effect' USING ERRCODE = '22023';
            END IF;
            canonical_effect := '{"kind":' || app.go_json_string(effect->>'kind') ||
                ',"account_id":' || app.go_json_string(effect->>'account_id') ||
                ',"family_id":' || app.go_json_string(effect->>'family_id') ||
                ',"status":' || app.go_json_string(effect->>'status') || '}';
        WHEN 'owner_transferred' THEN
            IF (effect - ARRAY['kind','account_id','family_id','role','status']::text[]) IS DISTINCT FROM '{}'::jsonb
               OR NOT (effect ?& ARRAY['kind','account_id','family_id','role','status'])
               OR effect->>'role' NOT IN ('owner','caregiver') OR effect->>'status' IS DISTINCT FROM 'active' THEN
                RAISE EXCEPTION 'invalid owner transfer effect' USING ERRCODE = '22023';
            END IF;
            IF effect->>'role' = 'caregiver' THEN
                old_owner_count := old_owner_count + 1; old_owner_effect := effect;
            ELSE
                new_owner_count := new_owner_count + 1; new_owner_effect := effect;
            END IF;
            canonical_effect := '{"kind":' || app.go_json_string(effect->>'kind') ||
                ',"account_id":' || app.go_json_string(effect->>'account_id') ||
                ',"family_id":' || app.go_json_string(effect->>'family_id') ||
                ',"role":' || app.go_json_string(effect->>'role') ||
                ',"status":' || app.go_json_string(effect->>'status') || '}';
        WHEN 'entitlement_granted','entitlement_suspended' THEN
            IF (effect - ARRAY['kind','family_id','projection_material_digest','status','entitlement_key','source','expires_at']::text[]) IS DISTINCT FROM '{}'::jsonb
               OR NOT (effect ?& ARRAY['kind','family_id','status','entitlement_key','source'])
               OR effect->>'entitlement_key' !~ '^[a-z][a-z0-9_]{0,63}$'
               OR effect->>'source' !~ '^[a-z][a-z0-9_]{0,63}$'
               OR (effect ? 'projection_material_digest' AND effect->>'projection_material_digest' !~ '^[0-9a-f]{64}$')
               OR (effect ? 'expires_at' AND effect->>'expires_at' !~ '^[0-9]{4}-(0[1-9]|1[0-2])-([0-2][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{0,8}[1-9])?Z$')
               OR effect->>'status' IS DISTINCT FROM (CASE effect->>'kind' WHEN 'entitlement_granted' THEN 'active' ELSE 'suspended' END) THEN
                RAISE EXCEPTION 'invalid entitlement effect' USING ERRCODE = '22023';
            END IF;
            entitlement_count := entitlement_count + 1;
            entitlement_effect := effect;
            canonical_effect := '{"kind":' || app.go_json_string(effect->>'kind') ||
                ',"family_id":' || app.go_json_string(effect->>'family_id');
            IF effect ? 'projection_material_digest' THEN
                canonical_effect := canonical_effect || ',"projection_material_digest":' || app.go_json_string(effect->>'projection_material_digest');
            END IF;
            canonical_effect := canonical_effect || ',"status":' || app.go_json_string(effect->>'status') ||
                ',"entitlement_key":' || app.go_json_string(effect->>'entitlement_key') ||
                ',"source":' || app.go_json_string(effect->>'source');
            IF effect ? 'expires_at' THEN
                canonical_effect := canonical_effect || ',"expires_at":' || app.go_json_string(effect->>'expires_at');
            END IF;
            canonical_effect := canonical_effect || '}';
        ELSE
            RAISE EXCEPTION 'effect kind mismatch' USING ERRCODE = '22023';
        END CASE;
        IF NOT first_effect THEN canonical_effects := canonical_effects || ','; END IF;
        canonical_effects := canonical_effects || canonical_effect;
        first_effect := false;
    END LOOP;
    canonical_effects := canonical_effects || ']';
    canonical_event := '{"version":"v1","generation":' || p_generation::text || ',"revision":' || p_revision::text ||
        ',"previous_hash":' || app.go_json_string(p_event->>'previous_hash') ||
        ',"command_id":' || app.go_json_string(p_event->>'command_id') ||
        ',"command_hash":' || app.go_json_string(p_event->>'command_hash') ||
        ',"actor_account_id":' || app.go_json_string(p_event->>'actor_account_id') ||
        ',"actor_device_id":' || app.go_json_string(p_event->>'actor_device_id') ||
        ',"kind":' || app.go_json_string(event_kind) || ',"at":' || app.go_json_string(p_event->>'at') ||
        ',"effects":' || canonical_effects || '}';
    IF convert_from(p_event_bytes,'UTF8') IS DISTINCT FROM canonical_event THEN
        RAISE EXCEPTION 'event bytes are not canonical' USING ERRCODE = '22023';
    END IF;

    IF event_kind = 'family_bootstrapped' THEN
        IF jsonb_array_length(p_event->'effects') <> 2 OR family_count <> 1 OR entitlement_count <> 1
           OR family_effect->>'family_id' IS DISTINCT FROM entitlement_effect->>'family_id'
           OR family_effect->>'account_id' IS DISTINCT FROM p_actor_account_id::text
           OR family_effect->>'projection_material_digest' IS DISTINCT FROM entitlement_effect->>'projection_material_digest'
           OR entitlement_effect->>'entitlement_key' IS DISTINCT FROM 'family_sync'
           OR entitlement_effect->>'source' IS DISTINCT FROM 'hosted_beta'
           OR (entitlement_effect->>'expires_at')::timestamptz IS DISTINCT FROM event_at + interval '366 days' THEN
            RAISE EXCEPTION 'invalid family bootstrap' USING ERRCODE = '22023';
        END IF;
    ELSIF event_kind = 'family_created' THEN
        IF jsonb_array_length(p_event->'effects') <> 1 OR family_count <> 1
           OR family_effect->>'account_id' IS DISTINCT FROM p_actor_account_id::text THEN
            RAISE EXCEPTION 'invalid family creation' USING ERRCODE = '22023';
        END IF;
    ELSIF event_kind = 'family_profile_repaired' THEN
        IF jsonb_array_length(p_event->'effects') <> 1 OR family_count <> 1
           OR family_effect->>'account_id' IS DISTINCT FROM p_actor_account_id::text THEN
            RAISE EXCEPTION 'invalid family profile repair' USING ERRCODE = '22023';
        END IF;
    ELSIF event_kind = 'owner_transferred' THEN
        IF jsonb_array_length(p_event->'effects') <> 2 OR old_owner_count <> 1 OR new_owner_count <> 1
           OR old_owner_effect->>'family_id' IS DISTINCT FROM new_owner_effect->>'family_id'
           OR old_owner_effect->>'account_id' = new_owner_effect->>'account_id' THEN
            RAISE EXCEPTION 'invalid owner transfer' USING ERRCODE = '22023';
        END IF;
    ELSIF jsonb_array_length(p_event->'effects') <> 1 OR effect->>'kind' IS DISTINCT FROM event_kind THEN
        RAISE EXCEPTION 'event effect mismatch' USING ERRCODE = '22023';
    END IF;

    SELECT generation,revision,event_hash INTO current_generation,current_revision,current_hash
      FROM public.security_projection_head WHERE singleton FOR UPDATE;
    IF current_generation=p_generation AND current_revision=p_revision AND current_hash=p_event_hash THEN
        IF NOT EXISTS (SELECT FROM public.security_audit_events WHERE generation=p_generation AND revision=p_revision
            AND event_hash=p_event_hash AND event_bytes=p_event_bytes AND event_data=p_event) THEN
            RAISE EXCEPTION 'projection head lacks exact audit receipt' USING ERRCODE = '23514';
        END IF;
        RETURN;
    END IF;
    IF current_generation IS DISTINCT FROM p_generation OR current_revision+1 IS DISTINCT FROM p_revision
       OR current_hash IS DISTINCT FROM p_previous_hash THEN
        RAISE EXCEPTION 'security projection head mismatch' USING ERRCODE = '40001';
    END IF;

    PERFORM set_config('app.account_id',p_actor_account_id::text,true);
    PERFORM set_config('app.family_id',coalesce(family_effect->>'family_id',effect->>'family_id',
        old_owner_effect->>'family_id',new_owner_effect->>'family_id',entitlement_effect->>'family_id'),true);

    IF event_kind IN ('family_created','family_bootstrapped') THEN
        SELECT * INTO stage_record FROM public.family_command_material WHERE command_id=p_command_id;
        stage_exists := FOUND;
        IF stage_exists AND (stage_record.command_hash IS DISTINCT FROM p_command_hash
            OR stage_record.projection_material_digest IS DISTINCT FROM decode(family_effect->>'projection_material_digest','hex')
            OR stage_record.family_id IS DISTINCT FROM (family_effect->>'family_id')::uuid
            OR stage_record.account_id IS DISTINCT FROM (family_effect->>'account_id')::uuid
            OR stage_record.material_kind IS DISTINCT FROM 'family_create'
            OR stage_record.projected_at IS NOT NULL OR stage_record.expires_at < event_at) THEN
            RAISE EXCEPTION 'family projection material mismatch' USING ERRCODE = '23514';
        END IF;
        presentation_missing := NOT stage_exists;
        family_name := CASE WHEN presentation_missing THEN '' ELSE stage_record.family_display_name END;
        SELECT display_name INTO account_name FROM public.accounts
         WHERE id=(family_effect->>'account_id')::uuid AND status='active';
        IF NOT FOUND THEN RAISE EXCEPTION 'family owner unavailable' USING ERRCODE = '23514'; END IF;
        INSERT INTO public.families(id,display_name,status,created_at,updated_at,profile_required)
        VALUES((family_effect->>'family_id')::uuid,family_name,'active',event_at,event_at,presentation_missing);
        INSERT INTO public.memberships(family_id,account_id,role,status,created_at,updated_at)
        VALUES((family_effect->>'family_id')::uuid,(family_effect->>'account_id')::uuid,'owner','active',event_at,event_at);
        INSERT INTO public.family_slots(account_id,family_id,state)
        VALUES((family_effect->>'account_id')::uuid,(family_effect->>'family_id')::uuid,'active');
        INSERT INTO public.family_actor_labels(family_id,account_id,display_name,former,created_at,updated_at)
        VALUES((family_effect->>'family_id')::uuid,(family_effect->>'account_id')::uuid,account_name,false,event_at,event_at);
        INSERT INTO public.family_actor_label_history(family_id,account_id,display_name,former,recorded_at)
        VALUES((family_effect->>'family_id')::uuid,(family_effect->>'account_id')::uuid,account_name,false,event_at);
        INSERT INTO public.family_security_state(family_id,owner_account_id,lifecycle,generation,revision,event_hash)
        VALUES((family_effect->>'family_id')::uuid,(family_effect->>'account_id')::uuid,'active',p_generation,p_revision,p_event_hash);
        IF event_kind = 'family_bootstrapped' THEN
            INSERT INTO public.family_entitlements(family_id,entitlement_key,source,status,expires_at,created_at,updated_at)
            VALUES((entitlement_effect->>'family_id')::uuid,entitlement_effect->>'entitlement_key',
                entitlement_effect->>'source','active',(entitlement_effect->>'expires_at')::timestamptz,event_at,event_at);
        END IF;
        IF stage_exists THEN
            UPDATE public.family_command_material SET family_display_name=NULL,material_bytes=NULL,projected_at=event_at WHERE command_id=p_command_id;
        END IF;
    ELSIF event_kind = 'family_profile_repaired' THEN
        SELECT * INTO stage_record FROM public.family_command_material WHERE command_id=p_command_id;
        stage_exists := FOUND;
        IF stage_exists AND (stage_record.command_hash IS DISTINCT FROM p_command_hash
            OR stage_record.projection_material_digest IS DISTINCT FROM decode(family_effect->>'projection_material_digest','hex')
            OR stage_record.material_kind IS DISTINCT FROM 'family_profile_repair'
            OR stage_record.family_id IS DISTINCT FROM (family_effect->>'family_id')::uuid
            OR stage_record.account_id IS DISTINCT FROM (family_effect->>'account_id')::uuid
            OR stage_record.projected_at IS NOT NULL OR stage_record.expires_at < event_at) THEN
            RAISE EXCEPTION 'family profile repair material mismatch' USING ERRCODE = '23514';
        END IF;
        UPDATE public.families
           SET display_name=CASE WHEN stage_exists THEN stage_record.family_display_name ELSE '' END,
               profile_required=NOT stage_exists,
               updated_at=event_at
         WHERE id=(family_effect->>'family_id')::uuid AND status='active'
           AND (NOT stage_exists OR profile_required=true)
           AND EXISTS (SELECT FROM public.memberships WHERE family_id=(family_effect->>'family_id')::uuid
               AND account_id=(family_effect->>'account_id')::uuid AND role='owner' AND status='active');
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'family profile is not repairable' USING ERRCODE='23514'; END IF;
        IF stage_exists THEN
            UPDATE public.family_command_material SET family_display_name=NULL,material_bytes=NULL,projected_at=event_at
             WHERE command_id=p_command_id;
        END IF;
    ELSIF event_kind = 'invitation_created' THEN
        IF NOT EXISTS (SELECT FROM public.memberships WHERE family_id=(effect->>'family_id')::uuid
            AND account_id=(effect->>'account_id')::uuid AND role='owner' AND status='active') THEN
            RAISE EXCEPTION 'invitation creator is not owner' USING ERRCODE = '23514';
        END IF;
        INSERT INTO public.invitations(id,family_id,creator_account_id,token_digest,role,status,
            expires_at,created_at,updated_at)
        VALUES((effect->>'invitation_id')::uuid,(effect->>'family_id')::uuid,(effect->>'account_id')::uuid,
            effect->>'token_digest','caregiver','active',(effect->>'expires_at')::timestamptz,event_at,event_at);
    ELSIF event_kind = 'invitation_revoked' THEN
        UPDATE public.invitations SET status='revoked',revoked_at=event_at,updated_at=event_at
         WHERE id=(effect->>'invitation_id')::uuid AND family_id=(effect->>'family_id')::uuid
           AND creator_account_id=(effect->>'account_id')::uuid AND token_digest=effect->>'token_digest'
           AND role=effect->>'role' AND expires_at=(effect->>'expires_at')::timestamptz AND status='active';
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'invitation is not active' USING ERRCODE='23514'; END IF;
    ELSIF event_kind = 'invitation_redeemed' THEN
        SELECT display_name INTO account_name FROM public.accounts
         WHERE id=(effect->>'account_id')::uuid AND status='active';
        IF NOT FOUND THEN RAISE EXCEPTION 'invitation redeemer unavailable' USING ERRCODE='23514'; END IF;
        UPDATE public.invitations SET status='redeemed',redeemed_by_account_id=(effect->>'account_id')::uuid,
            redeemed_at=event_at,updated_at=event_at
         WHERE id=(effect->>'invitation_id')::uuid AND family_id=(effect->>'family_id')::uuid
           AND token_digest=effect->>'token_digest' AND role=effect->>'role'
           AND expires_at=(effect->>'expires_at')::timestamptz AND event_at < expires_at AND status='active';
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'invitation is not redeemable' USING ERRCODE='23514'; END IF;
        INSERT INTO public.memberships(family_id,account_id,role,status,created_at,updated_at)
        VALUES((effect->>'family_id')::uuid,(effect->>'account_id')::uuid,'caregiver','active',event_at,event_at)
        ON CONFLICT(family_id,account_id) DO UPDATE
           SET role='caregiver',status='active',removed_at=NULL,updated_at=event_at
         WHERE memberships.role='caregiver' AND memberships.status='removed';
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'caregiver membership is not joinable' USING ERRCODE='23514'; END IF;
        INSERT INTO public.family_slots(account_id,family_id,state)
        VALUES((effect->>'account_id')::uuid,(effect->>'family_id')::uuid,'active');
        INSERT INTO public.family_actor_labels(family_id,account_id,display_name,former,created_at,updated_at)
        VALUES((effect->>'family_id')::uuid,(effect->>'account_id')::uuid,account_name,false,event_at,event_at)
        ON CONFLICT(family_id,account_id) DO UPDATE SET display_name=EXCLUDED.display_name,former=false,updated_at=EXCLUDED.updated_at;
        INSERT INTO public.family_actor_label_history(family_id,account_id,display_name,former,recorded_at)
        VALUES((effect->>'family_id')::uuid,(effect->>'account_id')::uuid,account_name,false,event_at);
    ELSIF event_kind IN ('member_left','member_removed') THEN
        IF event_kind='member_left' AND effect->>'account_id' IS DISTINCT FROM p_actor_account_id::text THEN
            RAISE EXCEPTION 'member leave actor mismatch' USING ERRCODE='23514';
        END IF;
        UPDATE public.memberships SET status='removed',removed_at=event_at,updated_at=event_at
         WHERE family_id=(effect->>'family_id')::uuid AND account_id=(effect->>'account_id')::uuid
           AND role='caregiver' AND status='active';
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'caregiver membership unavailable' USING ERRCODE='23514'; END IF;
        PERFORM set_config('app.account_id',effect->>'account_id',true);
        DELETE FROM public.family_slots WHERE account_id=(effect->>'account_id')::uuid
          AND family_id=(effect->>'family_id')::uuid AND state='active';
        UPDATE public.family_actor_label_history SET display_name='Former caregiver',former=true
         WHERE family_id=(effect->>'family_id')::uuid AND account_id=(effect->>'account_id')::uuid;
        UPDATE public.family_actor_labels SET display_name='Former caregiver',former=true,updated_at=event_at
         WHERE family_id=(effect->>'family_id')::uuid AND account_id=(effect->>'account_id')::uuid;
        INSERT INTO public.family_actor_label_history(family_id,account_id,display_name,former,recorded_at)
        VALUES((effect->>'family_id')::uuid,(effect->>'account_id')::uuid,'Former caregiver',true,event_at);
        UPDATE public.sessions SET status='revoked',revoked_at=event_at,revoke_reason='membership_revoked',updated_at=event_at
         WHERE account_id=(effect->>'account_id')::uuid AND status IN ('active','consumed');
    ELSIF event_kind = 'owner_transferred' THEN
        UPDATE public.memberships SET role='caregiver',updated_at=event_at
         WHERE family_id=(old_owner_effect->>'family_id')::uuid AND account_id=(old_owner_effect->>'account_id')::uuid
           AND role='owner' AND status='active';
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'current owner unavailable' USING ERRCODE='23514'; END IF;
        UPDATE public.memberships SET role='owner',updated_at=event_at
         WHERE family_id=(new_owner_effect->>'family_id')::uuid AND account_id=(new_owner_effect->>'account_id')::uuid
           AND role='caregiver' AND status='active';
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'new owner unavailable' USING ERRCODE='23514'; END IF;
        UPDATE public.family_security_state SET owner_account_id=(new_owner_effect->>'account_id')::uuid,
            generation=p_generation,revision=p_revision,event_hash=p_event_hash
         WHERE family_id=(new_owner_effect->>'family_id')::uuid
           AND owner_account_id=(old_owner_effect->>'account_id')::uuid AND lifecycle='active';
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'family authority mismatch' USING ERRCODE='23514'; END IF;
    ELSIF event_kind = 'entitlement_granted' THEN
        INSERT INTO public.family_entitlements(family_id,entitlement_key,source,status,expires_at,created_at,updated_at)
        VALUES((effect->>'family_id')::uuid,effect->>'entitlement_key',effect->>'source','active',
            CASE WHEN effect ? 'expires_at' THEN (effect->>'expires_at')::timestamptz END,event_at,event_at);
    ELSIF event_kind = 'entitlement_suspended' THEN
        UPDATE public.family_entitlements SET status='suspended',updated_at=event_at
         WHERE family_id=(effect->>'family_id')::uuid AND entitlement_key=effect->>'entitlement_key'
           AND source=effect->>'source' AND status='active'
           AND expires_at IS NOT DISTINCT FROM CASE WHEN effect ? 'expires_at' THEN (effect->>'expires_at')::timestamptz END;
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'entitlement unavailable' USING ERRCODE='23514'; END IF;
    END IF;

    INSERT INTO public.security_audit_events(generation,revision,event_hash,command_id,actor_account_id,kind,event_data,event_bytes)
    VALUES(p_generation,p_revision,p_event_hash,p_command_id,p_actor_account_id,event_kind,p_event,p_event_bytes);
    INSERT INTO public.security_command_receipts(actor_account_id,command_id,command_hash,generation,revision,result)
    VALUES(p_actor_account_id,p_command_id,p_command_hash,p_generation,p_revision,p_result);
    UPDATE public.security_projection_head SET generation=p_generation,revision=p_revision,event_hash=p_event_hash WHERE singleton;
    PERFORM set_config('app.family_id',previous_family_setting,true);
    PERFORM set_config('app.account_id',previous_account_setting,true);
END;
$$;

REVOKE ALL ON FUNCTION app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    FROM PUBLIC, baby_tracker_runtime;
GRANT EXECUTE ON FUNCTION app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    TO baby_tracker_projector;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
SELECT 6,principal,object_kind,object_identity,privileges
  FROM public.schema_privilege_manifest WHERE schema_version=5;

UPDATE public.schema_privilege_manifest SET privileges='SELECT'
 WHERE schema_version=6 AND principal='baby_tracker_runtime' AND object_kind='table'
   AND object_identity IN ('public.accounts','public.families','public.memberships');

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
    (6,'baby_tracker_runtime','table','public.family_actor_labels','SELECT'),
    (6,'baby_tracker_runtime','table','public.family_entitlements','SELECT'),
    (6,'baby_tracker_runtime','table','public.family_slots','SELECT'),
    (6,'baby_tracker_runtime','table','public.invitations','SELECT'),
    (6,'baby_tracker_runtime','function','app.current_account_profile(uuid)','EXECUTE'),
    (6,'baby_tracker_runtime','function','app.family_actor_labels_high_water()','EXECUTE'),
    (6,'baby_tracker_runtime','function','app.list_family_actor_labels(uuid,bigint,uuid,integer)','EXECUTE'),
    (6,'baby_tracker_runtime','function','app.lookup_family_invitation(text)','EXECUTE'),
    (6,'baby_tracker_runtime','function','app.stage_family_material(uuid,bytea,bytea,bytea,timestamp with time zone)','EXECUTE'),
    (6,'baby_tracker_runtime','function','app.update_own_profile(uuid,uuid,text,timestamp with time zone)','EXECUTE');

UPDATE public.schema_compatibility
   SET current_version=6,introduced_by='000006_family_commands.sql',recorded_at=now()
 WHERE singleton;
