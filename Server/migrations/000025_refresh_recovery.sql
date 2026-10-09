-- Extend the v5 identity branch in place; the v6/v7/v8 public wrappers remain intact.
-- Applied migration bodies 1-24 are immutable. Canonical legacy bytes are unchanged.
ALTER TABLE public.auth_exchange_material ADD COLUMN recovery_material_bytes bytea;

CREATE TABLE public.refresh_recovery_receipts (
    server_instance_id uuid NOT NULL,
    recovery_epoch bigint NOT NULL CHECK (recovery_epoch BETWEEN 1 AND 9007199254740991),
    account_id uuid NOT NULL REFERENCES public.accounts(id) ON DELETE CASCADE,
    device_id uuid NOT NULL REFERENCES public.devices(id) ON DELETE CASCADE,
    parent_session_id uuid NOT NULL UNIQUE REFERENCES public.sessions(session_id) ON DELETE CASCADE,
    attempt_id uuid NOT NULL CHECK (attempt_id <> '00000000-0000-0000-0000-000000000000'),
    child_session_id uuid NOT NULL UNIQUE REFERENCES public.sessions(session_id) ON DELETE CASCADE,
    material_bytes bytea NOT NULL CHECK (octet_length(material_bytes) BETWEEN 1 AND 32768),
    projection_material_digest bytea NOT NULL CHECK (octet_length(projection_material_digest)=32 AND sha256(material_bytes)=projection_material_digest),
    parent_key_id text NOT NULL,
    output_key_id text NOT NULL,
    continuity_key_id text NOT NULL,
    created_at timestamptz NOT NULL CHECK (isfinite(created_at)),
    effective_deadline timestamptz NOT NULL CHECK (isfinite(effective_deadline) AND effective_deadline >= created_at),
    refresh_expires_at timestamptz NOT NULL CHECK (isfinite(refresh_expires_at)),
    PRIMARY KEY (server_instance_id,recovery_epoch,account_id,device_id,parent_session_id,attempt_id)
);
REVOKE ALL ON public.refresh_recovery_receipts FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;

-- Go [32]byte JSON is a fixed numeric array, not a base64 string.
CREATE FUNCTION app.refresh_recovery_digest(p_value jsonb) RETURNS bytea
LANGUAGE plpgsql IMMUTABLE SET search_path=pg_catalog AS $$
DECLARE result bytea; item jsonb; encoded text := '';
BEGIN
    IF jsonb_typeof(p_value) IS DISTINCT FROM 'array' OR jsonb_array_length(p_value) <> 32 THEN
        RAISE EXCEPTION 'invalid recovery digest' USING ERRCODE='22023';
    END IF;
    FOR item IN SELECT value FROM jsonb_array_elements(p_value) LOOP
        IF jsonb_typeof(item) <> 'number' OR item::text !~ '^(0|[1-9][0-9]{0,2})$' OR (item::text)::int > 255 THEN
            RAISE EXCEPTION 'invalid recovery digest byte' USING ERRCODE='22023';
        END IF;
        encoded := encoded || lpad(to_hex((item::text)::int),2,'0');
    END LOOP;
    result := decode(encoded,'hex');
    IF result=decode(repeat('00',32),'hex') THEN RAISE EXCEPTION 'empty recovery digest' USING ERRCODE='22023'; END IF;
    RETURN result;
END;
$$;
REVOKE ALL ON FUNCTION app.refresh_recovery_digest(jsonb) FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;

-- Strict ordered serializer for the frozen v3 input. It also checks that every
-- duplicated derivation field agrees with the canonical session stage.
CREATE FUNCTION app.canonical_refresh_recovery(p_material jsonb) RETURNS text
LANGUAGE plpgsql IMMUTABLE SET search_path=pg_catalog AS $$
DECLARE
    recovery jsonb := p_material->'recovery';
    frozen jsonb := recovery->'input';
    field text;
    canonical text := '{"input":{';
    separator text := '';
    value text;
    fields text[] := ARRAY['derivation_version','server_instance_id','account_id','device_id','parent_session_id','lineage_root_session_id','recovery_epoch','parent_key_id','parent_digest','output_key_id','scope','family_id','access_expires_at','refresh_expires_at','reauthenticate_at','absolute_expires_at','head_generation','head_revision','head_hash','target_revision','attempt_id','canonical_origin','continuity_key_id','continuity_digest'];
BEGIN
    IF jsonb_typeof(recovery) IS DISTINCT FROM 'object' OR (recovery-ARRAY['input','created_at','effective_deadline']) <> '{}'::jsonb
       OR NOT (recovery ?& ARRAY['input','created_at','effective_deadline'])
       OR jsonb_typeof(frozen) IS DISTINCT FROM 'object' OR (frozen-fields) <> '{}'::jsonb OR NOT (frozen ?& fields) THEN
        RAISE EXCEPTION 'invalid recovery material shape' USING ERRCODE='22023';
    END IF;
    FOREACH field IN ARRAY fields LOOP
        IF field IN ('parent_digest','head_hash','continuity_digest') THEN
            PERFORM app.refresh_recovery_digest(frozen->field);
            value := replace((frozen->field)::text,' ','');
        ELSIF field IN ('recovery_epoch','head_generation','head_revision','target_revision') THEN
            IF jsonb_typeof(frozen->field) <> 'number' OR frozen->>field !~ '^(0|[1-9][0-9]{0,15})$'
               OR (frozen->>field)::numeric > 9007199254740991 OR (field<>'head_revision' AND (frozen->>field)::numeric=0) THEN
                RAISE EXCEPTION 'invalid recovery revision' USING ERRCODE='22023'; END IF;
            value := frozen->>field;
        ELSIF field='family_id' AND jsonb_typeof(frozen->field)='null' THEN
            value := 'null';
        ELSE
            IF jsonb_typeof(frozen->field) <> 'string' THEN RAISE EXCEPTION 'invalid recovery string' USING ERRCODE='22023'; END IF;
            value := app.go_json_string(frozen->>field);
        END IF;
        canonical := canonical || separator || app.go_json_string(field) || ':' || value;
        separator := ',';
    END LOOP;
    FOREACH field IN ARRAY ARRAY['server_instance_id','account_id','device_id','parent_session_id','lineage_root_session_id','recovery_epoch',
        'scope','family_id','access_expires_at','refresh_expires_at','reauthenticate_at','absolute_expires_at','canonical_origin'] LOOP
        IF frozen->field IS DISTINCT FROM p_material->field THEN
            RAISE EXCEPTION 'recovery input differs from stage' USING ERRCODE='22023'; END IF;
    END LOOP;
    IF frozen->>'derivation_version'<>'v3' OR frozen->>'output_key_id' IS DISTINCT FROM p_material->>'token_key_id'
       OR frozen->>'parent_key_id' !~ '^[a-z][a-z0-9._-]{0,63}$' OR frozen->>'continuity_key_id' !~ '^[a-z][a-z0-9._-]{0,63}$'
       OR frozen->>'attempt_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR frozen->>'attempt_id' IN ('00000000-0000-0000-0000-000000000000','00000000-0000-4000-8000-000000000000')
       OR (frozen->>'target_revision')::bigint<>(frozen->>'head_revision')::bigint+1
       OR frozen->'target_revision' IS DISTINCT FROM p_material->'security_revision' THEN
        RAISE EXCEPTION 'invalid recovery input binding' USING ERRCODE='22023';
    END IF;
    FOREACH field IN ARRAY ARRAY['created_at','effective_deadline'] LOOP
        IF jsonb_typeof(recovery->field)<>'string' OR recovery->>field !~ '^[0-9]{4}-(0[1-9]|1[0-2])-([0-2][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{0,8}[1-9])?Z$'
           OR NOT isfinite((recovery->>field)::timestamptz) THEN
            RAISE EXCEPTION 'invalid recovery deadline' USING ERRCODE='22023'; END IF;
    END LOOP;
    FOREACH field IN ARRAY ARRAY['access_expires_at','refresh_expires_at','reauthenticate_at','absolute_expires_at'] LOOP
        IF frozen->>field !~ '^[0-9]{4}-(0[1-9]|1[0-2])-([0-2][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{0,8}[1-9])?Z$'
           OR NOT isfinite((frozen->>field)::timestamptz) THEN
            RAISE EXCEPTION 'noncanonical frozen recovery deadline' USING ERRCODE='22023'; END IF;
    END LOOP;
    IF (recovery->>'effective_deadline')::timestamptz < (recovery->>'created_at')::timestamptz
       OR (recovery->>'effective_deadline')::timestamptz > (frozen->>'absolute_expires_at')::timestamptz
       OR (frozen->>'access_expires_at')::timestamptz > (frozen->>'refresh_expires_at')::timestamptz
       OR (frozen->>'refresh_expires_at')::timestamptz > (frozen->>'absolute_expires_at')::timestamptz
       OR (frozen->>'reauthenticate_at')::timestamptz > (frozen->>'absolute_expires_at')::timestamptz THEN
        RAISE EXCEPTION 'invalid recovery deadline order' USING ERRCODE='22023'; END IF;
    RETURN canonical || '},"created_at":' || app.go_json_string(recovery->>'created_at') || ',"effective_deadline":' || app.go_json_string(recovery->>'effective_deadline') || '}';
END;
$$;
REVOKE ALL ON FUNCTION app.canonical_refresh_recovery(jsonb) FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;

CREATE OR REPLACE FUNCTION app.stage_auth_material(
    p_command_id uuid, p_command_hash bytea, p_projection_material_digest bytea,
    p_material_bytes bytea, p_now timestamptz
) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
AS $$
DECLARE
    material jsonb;
    material_text text;
    canonical text;
    common_keys text[] := ARRAY['version','kind','account_id','device_id','session_id','lineage_root_session_id',
        'server_instance_id','canonical_origin','token_key_id','access_digest','refresh_digest','recovery_epoch',
        'security_revision','scope','family_id','access_expires_at','refresh_expires_at','reauthenticate_at','absolute_expires_at'];
    exchange_keys text[] := ARRAY['identity_provider','identity_key_id','identity_digest','identity_audience','identity_subject',
        'display_name','profile_confirmed','register_account','register_device','installation_key_id','installation_digest',
        'continuity_key_id','continuity_digest','apple_credential_id','apple_credential_key_id','apple_credential_purpose',
        'apple_credential_object_id'];
    all_keys text[];
BEGIN
    IF p_command_id IS NULL OR p_command_id='00000000-0000-0000-0000-000000000000'
       OR octet_length(p_command_hash) IS DISTINCT FROM 32
       OR octet_length(p_projection_material_digest) IS DISTINCT FROM 32
       OR octet_length(p_material_bytes) NOT BETWEEN 1 AND 32768
       OR sha256(p_material_bytes) IS DISTINCT FROM p_projection_material_digest
       OR NOT isfinite(p_now) THEN
        RAISE EXCEPTION 'invalid auth material envelope' USING ERRCODE = '22023';
    END IF;
    material_text := convert_from(p_material_bytes,'UTF8');
    material := material_text::jsonb;
    IF jsonb_typeof(material) IS DISTINCT FROM 'object'
       OR material->>'version' IS DISTINCT FROM 'v1'
       OR material->>'kind' NOT IN ('exchange','refresh') THEN
        RAISE EXCEPTION 'invalid auth material' USING ERRCODE = '22023';
    END IF;
    IF material->>'kind'='exchange' THEN
        all_keys := common_keys || exchange_keys;
    ELSE
        all_keys := common_keys || ARRAY['parent_session_id'];
        IF material ? 'recovery' THEN all_keys := all_keys || ARRAY['recovery']; END IF;
    END IF;
    IF (material-all_keys) <> '{}'::jsonb OR NOT (material ?& all_keys)
       OR EXISTS (SELECT FROM unnest(common_keys) AS k(name)
                   WHERE k.name <> 'family_id' AND jsonb_typeof(material->k.name) <>
                     CASE WHEN k.name IN ('recovery_epoch','security_revision') THEN 'number' ELSE 'string' END)
       OR material->>'account_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR material->>'device_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR material->>'session_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR material->>'lineage_root_session_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR material->>'server_instance_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
       OR (material->>'account_id')::uuid='00000000-0000-0000-0000-000000000000'
       OR (material->>'device_id')::uuid='00000000-0000-0000-0000-000000000000'
       OR (material->>'session_id')::uuid='00000000-0000-0000-0000-000000000000'
       OR (material->>'lineage_root_session_id')::uuid='00000000-0000-0000-0000-000000000000'
       OR (material->>'server_instance_id')::uuid='00000000-0000-0000-0000-000000000000'
       OR material->>'token_key_id' !~ '^[a-z][a-z0-9._-]{0,63}$'
       OR material->>'access_digest' !~ '^[0-9a-f]{64}$' OR material->>'refresh_digest' !~ '^[0-9a-f]{64}$'
       OR material->>'recovery_epoch' !~ '^[1-9][0-9]{0,15}$'
       OR material->>'security_revision' !~ '^[1-9][0-9]{0,15}$'
       OR (material->>'recovery_epoch')::numeric > 9007199254740991
       OR (material->>'security_revision')::numeric > 9007199254740991
       OR material->>'scope' NOT IN ('account','family','family_recovery')
       OR ((material->>'scope'='account') IS DISTINCT FROM (jsonb_typeof(material->'family_id')='null'))
       OR (material->>'scope'<>'account' AND (jsonb_typeof(material->'family_id')<>'string'
            OR material->>'family_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
            OR (material->>'family_id')::uuid='00000000-0000-0000-0000-000000000000'))
       OR material->>'canonical_origin'='' OR octet_length(material->>'canonical_origin')>2048
       OR octet_length(material->>'canonical_origin')<>char_length(material->>'canonical_origin')
       OR material->>'access_expires_at' !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$'
       OR material->>'refresh_expires_at' !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$'
       OR material->>'reauthenticate_at' !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$'
       OR material->>'absolute_expires_at' !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$' THEN
        RAISE EXCEPTION 'invalid auth material common fields' USING ERRCODE = '22023';
    END IF;
    canonical := '{"version":' || app.go_json_string(material->>'version') || ',"kind":' || app.go_json_string(material->>'kind') ||
        ',"account_id":' || app.go_json_string(material->>'account_id') || ',"device_id":' || app.go_json_string(material->>'device_id') ||
        ',"session_id":' || app.go_json_string(material->>'session_id') || ',"lineage_root_session_id":' || app.go_json_string(material->>'lineage_root_session_id') ||
        ',"server_instance_id":' || app.go_json_string(material->>'server_instance_id') || ',"canonical_origin":' || app.go_json_string(material->>'canonical_origin') ||
        ',"token_key_id":' || app.go_json_string(material->>'token_key_id') || ',"access_digest":' || app.go_json_string(material->>'access_digest') ||
        ',"refresh_digest":' || app.go_json_string(material->>'refresh_digest') || ',"recovery_epoch":' || (material->>'recovery_epoch') ||
        ',"security_revision":' || (material->>'security_revision') || ',"scope":' || app.go_json_string(material->>'scope') ||
        ',"family_id":' || CASE WHEN jsonb_typeof(material->'family_id')='null' THEN 'null' ELSE app.go_json_string(material->>'family_id') END ||
        ',"access_expires_at":' || app.go_json_string(material->>'access_expires_at') || ',"refresh_expires_at":' || app.go_json_string(material->>'refresh_expires_at') ||
        ',"reauthenticate_at":' || app.go_json_string(material->>'reauthenticate_at') || ',"absolute_expires_at":' || app.go_json_string(material->>'absolute_expires_at');
    IF material->>'kind'='exchange' THEN
        IF jsonb_typeof(material->'identity_provider')<>'string' OR material->>'identity_provider'<>'apple'
           OR jsonb_typeof(material->'identity_key_id')<>'string' OR material->>'identity_key_id' !~ '^[a-z][a-z0-9._-]{0,63}$'
           OR jsonb_typeof(material->'identity_digest')<>'string' OR material->>'identity_digest' !~ '^[0-9a-f]{64}$'
           OR jsonb_typeof(material->'identity_audience')<>'string' OR octet_length(material->>'identity_audience') NOT BETWEEN 1 AND 255
           OR jsonb_typeof(material->'identity_subject')<>'string' OR octet_length(material->>'identity_subject') NOT BETWEEN 1 AND 255
           OR jsonb_typeof(material->'profile_confirmed')<>'boolean'
           OR jsonb_typeof(material->'register_account')<>'boolean' OR jsonb_typeof(material->'register_device')<>'boolean'
           OR ((jsonb_typeof(material->'display_name')='null') IS DISTINCT FROM ((material->>'profile_confirmed')::boolean=false))
           OR (jsonb_typeof(material->'display_name')<>'null' AND (jsonb_typeof(material->'display_name')<>'string'
                OR char_length(material->>'display_name') NOT BETWEEN 1 AND 80 OR octet_length(material->>'display_name')>320))
           OR ((jsonb_typeof(material->'installation_key_id')='null') IS DISTINCT FROM (jsonb_typeof(material->'installation_digest')='null'))
           OR (jsonb_typeof(material->'installation_key_id')<>'null' AND (material->>'installation_key_id' !~ '^[a-z][a-z0-9._-]{0,63}$'
                OR material->>'installation_digest' !~ '^[0-9a-f]{64}$'))
           OR (((material->>'register_device')::boolean) IS DISTINCT FROM (jsonb_typeof(material->'continuity_key_id')<>'null'))
           OR ((material->>'register_device')::boolean AND (material->>'continuity_key_id' !~ '^[a-z][a-z0-9._-]{0,63}$'
                OR material->>'continuity_digest' !~ '^[0-9a-f]{64}$'))
           OR (NOT (material->>'register_device')::boolean AND jsonb_typeof(material->'continuity_digest')<>'null')
           OR NOT ((jsonb_typeof(material->'apple_credential_id')='null' AND jsonb_typeof(material->'apple_credential_key_id')='null'
                    AND jsonb_typeof(material->'apple_credential_purpose')='null' AND jsonb_typeof(material->'apple_credential_object_id')='null')
                OR (material->>'apple_credential_id' ~ '^[A-Za-z0-9._-]{1,128}$'
                    AND material->>'apple_credential_key_id' ~ '^[A-Za-z0-9._-]{1,128}$'
                    AND material->>'apple_credential_purpose'='apple-refresh-revocation-v1'
                    AND material->>'apple_credential_object_id' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                    AND (material->>'apple_credential_object_id')::uuid<>'00000000-0000-0000-0000-000000000000')) THEN
            RAISE EXCEPTION 'invalid auth exchange material' USING ERRCODE = '22023';
        END IF;
        canonical := canonical || ',"identity_provider":' || app.go_json_string(material->>'identity_provider') ||
            ',"identity_key_id":' || app.go_json_string(material->>'identity_key_id') || ',"identity_digest":' || app.go_json_string(material->>'identity_digest') ||
            ',"identity_audience":' || app.go_json_string(material->>'identity_audience') || ',"identity_subject":' || app.go_json_string(material->>'identity_subject') ||
            ',"display_name":' || CASE WHEN jsonb_typeof(material->'display_name')='null' THEN 'null' ELSE app.go_json_string(material->>'display_name') END ||
            ',"profile_confirmed":' || (material->>'profile_confirmed') || ',"register_account":' || (material->>'register_account') ||
            ',"register_device":' || (material->>'register_device') || ',"installation_key_id":' || CASE WHEN jsonb_typeof(material->'installation_key_id')='null' THEN 'null' ELSE app.go_json_string(material->>'installation_key_id') END ||
            ',"installation_digest":' || CASE WHEN jsonb_typeof(material->'installation_digest')='null' THEN 'null' ELSE app.go_json_string(material->>'installation_digest') END ||
            ',"continuity_key_id":' || CASE WHEN jsonb_typeof(material->'continuity_key_id')='null' THEN 'null' ELSE app.go_json_string(material->>'continuity_key_id') END ||
            ',"continuity_digest":' || CASE WHEN jsonb_typeof(material->'continuity_digest')='null' THEN 'null' ELSE app.go_json_string(material->>'continuity_digest') END ||
            ',"apple_credential_id":' || CASE WHEN jsonb_typeof(material->'apple_credential_id')='null' THEN 'null' ELSE app.go_json_string(material->>'apple_credential_id') END ||
            ',"apple_credential_key_id":' || CASE WHEN jsonb_typeof(material->'apple_credential_key_id')='null' THEN 'null' ELSE app.go_json_string(material->>'apple_credential_key_id') END ||
            ',"apple_credential_purpose":' || CASE WHEN jsonb_typeof(material->'apple_credential_purpose')='null' THEN 'null' ELSE app.go_json_string(material->>'apple_credential_purpose') END ||
            ',"apple_credential_object_id":' || CASE WHEN jsonb_typeof(material->'apple_credential_object_id')='null' THEN 'null' ELSE app.go_json_string(material->>'apple_credential_object_id') END || '}';
    ELSE
        IF jsonb_typeof(material->'parent_session_id')<>'string'
           OR material->>'parent_session_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
           OR (material->>'parent_session_id')::uuid='00000000-0000-0000-0000-000000000000' THEN
            RAISE EXCEPTION 'invalid auth refresh material' USING ERRCODE = '22023';
        END IF;
        canonical := canonical || ',"parent_session_id":' || app.go_json_string(material->>'parent_session_id');
        IF material ? 'recovery' THEN canonical := canonical || ',"recovery":' || app.canonical_refresh_recovery(material); END IF;
        canonical := canonical || '}';
    END IF;
    IF material_text IS DISTINCT FROM canonical THEN
        RAISE EXCEPTION 'auth material is not canonical' USING ERRCODE = '22023';
    END IF;
    INSERT INTO public.auth_exchange_material(
        command_id,command_hash,projection_material_digest,material_kind,account_id,device_id,session_id,parent_session_id,
        lineage_root_session_id,server_instance_id,canonical_origin,token_key_id,access_digest,refresh_digest,recovery_epoch,
        security_revision,scope,family_id,access_expires_at,refresh_expires_at,reauthenticate_at,absolute_expires_at,
        identity_provider,identity_key_id,identity_digest,identity_audience,identity_subject,display_name,profile_confirmed,
        register_account,register_device,installation_key_id,installation_digest,continuity_key_id,continuity_digest,
        apple_credential_id,apple_credential_key_id,apple_credential_purpose,apple_credential_object_id,created_at,expires_at,recovery_material_bytes)
    VALUES(p_command_id,p_command_hash,p_projection_material_digest,material->>'kind',(material->>'account_id')::uuid,
        (material->>'device_id')::uuid,(material->>'session_id')::uuid,
        CASE WHEN material->>'kind'='refresh' THEN (material->>'parent_session_id')::uuid END,
        (material->>'lineage_root_session_id')::uuid,(material->>'server_instance_id')::uuid,material->>'canonical_origin',
        material->>'token_key_id',decode(material->>'access_digest','hex'),decode(material->>'refresh_digest','hex'),
        (material->>'recovery_epoch')::bigint,(material->>'security_revision')::bigint,material->>'scope',
        CASE WHEN jsonb_typeof(material->'family_id')='null' THEN NULL ELSE (material->>'family_id')::uuid END,
        (material->>'access_expires_at')::timestamptz,(material->>'refresh_expires_at')::timestamptz,
        (material->>'reauthenticate_at')::timestamptz,(material->>'absolute_expires_at')::timestamptz,
        material->>'identity_provider',material->>'identity_key_id',
        CASE WHEN material ? 'identity_digest' THEN decode(material->>'identity_digest','hex') END,
        material->>'identity_audience',material->>'identity_subject',material->>'display_name',
        CASE WHEN material ? 'profile_confirmed' THEN (material->>'profile_confirmed')::boolean END,
        CASE WHEN material ? 'register_account' THEN (material->>'register_account')::boolean END,
        CASE WHEN material ? 'register_device' THEN (material->>'register_device')::boolean END,
        material->>'installation_key_id',CASE WHEN jsonb_typeof(material->'installation_digest')='string' THEN decode(material->>'installation_digest','hex') END,
        material->>'continuity_key_id',CASE WHEN jsonb_typeof(material->'continuity_digest')='string' THEN decode(material->>'continuity_digest','hex') END,
        material->>'apple_credential_id',material->>'apple_credential_key_id',material->>'apple_credential_purpose',
        CASE WHEN jsonb_typeof(material->'apple_credential_object_id')='string' THEN (material->>'apple_credential_object_id')::uuid END,
        p_now,p_now+interval '10 minutes',CASE WHEN material ? 'recovery' THEN p_material_bytes END)
    ON CONFLICT(command_id) DO UPDATE SET command_id=EXCLUDED.command_id
      WHERE public.auth_exchange_material.command_hash=EXCLUDED.command_hash
        AND public.auth_exchange_material.projection_material_digest=EXCLUDED.projection_material_digest
        AND public.auth_exchange_material.material_kind=EXCLUDED.material_kind
        AND public.auth_exchange_material.account_id=EXCLUDED.account_id
        AND public.auth_exchange_material.device_id=EXCLUDED.device_id
        AND public.auth_exchange_material.session_id=EXCLUDED.session_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'auth material command conflict' USING ERRCODE = '23505';
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION app.project_security_event_v5(
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
    account_effect jsonb;
    device_effect jsonb;
    session_effect jsonb;
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
    account_count integer := 0;
    device_count integer := 0;
    session_count integer := 0;
    staged public.auth_exchange_material%ROWTYPE;
    has_stage boolean := false;
    recovery_material jsonb;
    frozen_recovery jsonb;
    projected_identity_key_id text;
    projected_identity_digest bytea;
    projected_continuity_key_id text;
    projected_continuity_digest bytea;
    previous_account_setting text := coalesce(current_setting('app.account_id',true),'');
BEGIN
    IF p_generation IS NULL OR p_revision IS NULL OR p_generation <= 0 OR p_revision <= 0
       OR p_generation > 9007199254740991 OR p_revision > 9007199254740991
       OR octet_length(p_previous_hash) IS DISTINCT FROM 32 OR octet_length(p_event_hash) IS DISTINCT FROM 32
       OR octet_length(p_command_hash) IS DISTINCT FROM 32 OR octet_length(p_event_bytes) NOT BETWEEN 1 AND 65536
       OR p_actor_account_id IS NULL OR p_actor_account_id='00000000-0000-0000-0000-000000000000'
       OR p_command_id IS NULL OR p_command_id='00000000-0000-0000-0000-000000000000' THEN
        RAISE EXCEPTION 'invalid security projection input' USING ERRCODE='22023';
    END IF;
    IF sha256(p_event_bytes) IS DISTINCT FROM p_event_hash
       OR convert_from(p_event_bytes,'UTF8')::jsonb IS DISTINCT FROM p_event
       OR jsonb_typeof(p_event) IS DISTINCT FROM 'object'
       OR (p_event-ARRAY['version','generation','revision','previous_hash','command_id','command_hash','actor_account_id','actor_device_id','kind','at','effects']::text[])<>'{}'::jsonb
       OR NOT (p_event ?& ARRAY['version','generation','revision','previous_hash','command_id','command_hash','actor_account_id','actor_device_id','kind','at','effects'])
       OR p_event->>'version'<>'v1' OR jsonb_typeof(p_event->'generation')<>'number' OR jsonb_typeof(p_event->'revision')<>'number'
       OR (p_event->>'generation')::bigint<>p_generation OR (p_event->>'revision')::bigint<>p_revision
       OR jsonb_typeof(p_event->'previous_hash')<>'string' OR p_event->>'previous_hash' !~ '^[0-9a-f]{64}$'
       OR decode(p_event->>'previous_hash','hex')<>p_previous_hash
       OR jsonb_typeof(p_event->'command_hash')<>'string' OR p_event->>'command_hash' !~ '^[0-9a-f]{64}$'
       OR decode(p_event->>'command_hash','hex')<>p_command_hash
       OR jsonb_typeof(p_event->'command_id')<>'string' OR p_event->>'command_id'<>p_command_id::text
       OR (p_event->>'command_id')::uuid<>p_command_id
       OR jsonb_typeof(p_event->'actor_account_id')<>'string' OR p_event->>'actor_account_id'<>p_actor_account_id::text
       OR (p_event->>'actor_account_id')::uuid<>p_actor_account_id
       OR jsonb_typeof(p_event->'actor_device_id')<>'string'
       OR (p_event->>'actor_device_id'<>'' AND (p_event->>'actor_device_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
            OR p_event->>'actor_device_id'='00000000-0000-0000-0000-000000000000'
            OR (p_event->>'actor_device_id')::uuid::text<>p_event->>'actor_device_id'))
       OR jsonb_typeof(p_event->'kind')<>'string' OR jsonb_typeof(p_event->'at')<>'string'
       OR p_event->>'at' !~ '^[0-9]{4}-(0[1-9]|1[0-2])-([0-2][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{0,8}[1-9])?Z$'
       OR NOT isfinite((p_event->>'at')::timestamptz)
       OR jsonb_typeof(p_event->'effects')<>'array' OR jsonb_array_length(p_event->'effects')=0 THEN
        RAISE EXCEPTION 'event envelope mismatch' USING ERRCODE='22023';
    END IF;
    IF p_result IS DISTINCT FROM jsonb_build_object('command_id',p_command_id::text,'outcome','committed','code','ok','revision',p_revision) THEN
        RAISE EXCEPTION 'command result mismatch' USING ERRCODE='22023';
    END IF;
    event_kind := p_event->>'kind';
    IF event_kind NOT IN ('account_registered','identity_unavailable','identity_restored','identity_session_issued',
        'device_registered','device_binding_revoked','session_granted','session_rotated','session_revoked',
        'family_created','invitation_created','invitation_revoked','invitation_redeemed','member_left','member_removed',
        'owner_transferred','owner_recovered','entitlement_granted','entitlement_suspended','family_deletion_scheduled',
        'family_recovered','recovery_forfeited','account_deleted','family_purge_started','family_purge_completed') THEN
        RAISE EXCEPTION 'unsupported security event' USING ERRCODE='22023';
    END IF;
    IF event_kind NOT IN ('account_registered','identity_unavailable','identity_restored','identity_session_issued',
        'session_rotated','session_revoked','device_binding_revoked','family_created','family_deletion_scheduled',
        'family_recovered','family_purge_started','family_purge_completed') THEN
        RAISE EXCEPTION 'projector case not installed for security event %',event_kind USING ERRCODE='0A000';
    END IF;

    FOR effect IN SELECT value FROM jsonb_array_elements(p_event->'effects') LOOP
        IF jsonb_typeof(effect)<>'object' THEN RAISE EXCEPTION 'effect must be an object' USING ERRCODE='22023'; END IF;
        FOREACH identifier IN ARRAY ARRAY[effect->>'account_id',effect->>'family_id',effect->>'device_id',effect->>'session_id',
            effect->>'parent_session_id',effect->>'lineage_root_session_id',effect->>'server_instance_id',effect->>'invitation_id',effect->>'refresh_attempt_id'] LOOP
            IF identifier IS NOT NULL AND (identifier !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                OR identifier='00000000-0000-0000-0000-000000000000' OR identifier::uuid::text<>identifier) THEN
                RAISE EXCEPTION 'effect identifier is not canonical' USING ERRCODE='22023';
            END IF;
        END LOOP;
        FOREACH opaque_value IN ARRAY ARRAY[effect->>'canonical_origin',effect->>'identity_key',effect->>'token_digest',
            effect->>'device_credential_digest',effect->>'projection_material_digest',effect->>'role',effect->>'status',
            effect->>'entitlement_key',effect->>'source'] LOOP
            IF opaque_value IS NOT NULL AND octet_length(opaque_value) NOT BETWEEN 1 AND 4096 THEN
                RAISE EXCEPTION 'effect opaque value is outside byte bounds' USING ERRCODE='22023';
            END IF;
        END LOOP;
        IF effect ? 'recovery_epoch' AND (jsonb_typeof(effect->'recovery_epoch')<>'number'
            OR effect->>'recovery_epoch' !~ '^[1-9][0-9]{0,15}$' OR (effect->>'recovery_epoch')::numeric>9007199254740991) THEN
            RAISE EXCEPTION 'effect epoch is invalid' USING ERRCODE='22023';
        END IF;
        IF effect ? 'expires_at' AND (jsonb_typeof(effect->'expires_at')<>'string'
            OR effect->>'expires_at' !~ '^[0-9]{4}-(0[1-9]|1[0-2])-([0-2][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{0,8}[1-9])?Z$'
            OR NOT isfinite((effect->>'expires_at')::timestamptz)) THEN
            RAISE EXCEPTION 'effect timestamp is not canonical' USING ERRCODE='22023';
        END IF;
        IF effect ? 'purge_after' AND (jsonb_typeof(effect->'purge_after')<>'string'
            OR effect->>'purge_after' !~ '^[0-9]{4}-(0[1-9]|1[0-2])-([0-2][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{0,8}[1-9])?Z$'
            OR NOT isfinite((effect->>'purge_after')::timestamptz)) THEN
            RAISE EXCEPTION 'effect timestamp is not canonical' USING ERRCODE='22023';
        END IF;
        canonical_effect := CASE
          WHEN event_kind='identity_session_issued' AND effect->>'kind'='account_registered' THEN
            '{"kind":'||app.go_json_string(effect->>'kind')||',"account_id":'||app.go_json_string(effect->>'account_id')||
            ',"identity_key":'||app.go_json_string(effect->>'identity_key')||',"status":'||app.go_json_string(effect->>'status')||'}'
          WHEN event_kind='identity_session_issued' AND effect->>'kind'='device_registered' THEN
            '{"kind":'||app.go_json_string(effect->>'kind')||',"account_id":'||app.go_json_string(effect->>'account_id')||
            ',"device_id":'||app.go_json_string(effect->>'device_id')||',"server_instance_id":'||app.go_json_string(effect->>'server_instance_id')||
            ',"canonical_origin":'||app.go_json_string(effect->>'canonical_origin')||',"device_credential_digest":'||app.go_json_string(effect->>'device_credential_digest')||
            ',"status":'||app.go_json_string(effect->>'status')||'}'
          WHEN event_kind='identity_session_issued' AND effect->>'kind'='session_granted' THEN
            '{"kind":'||app.go_json_string(effect->>'kind')||',"account_id":'||app.go_json_string(effect->>'account_id')||
            ',"device_id":'||app.go_json_string(effect->>'device_id')||',"session_id":'||app.go_json_string(effect->>'session_id')||
            ',"lineage_root_session_id":'||app.go_json_string(effect->>'lineage_root_session_id')||',"server_instance_id":'||app.go_json_string(effect->>'server_instance_id')||
            ',"canonical_origin":'||app.go_json_string(effect->>'canonical_origin')||',"identity_key":'||app.go_json_string(effect->>'identity_key')||
            ',"device_credential_digest":'||app.go_json_string(effect->>'device_credential_digest')||',"projection_material_digest":'||app.go_json_string(effect->>'projection_material_digest')||
            ',"status":'||app.go_json_string(effect->>'status')||',"expires_at":'||app.go_json_string(effect->>'expires_at')||
            ',"recovery_epoch":'||(effect->>'recovery_epoch')||'}'
          WHEN event_kind='session_rotated' THEN
            '{"kind":'||app.go_json_string(effect->>'kind')||',"account_id":'||app.go_json_string(effect->>'account_id')||
            ',"device_id":'||app.go_json_string(effect->>'device_id')||',"session_id":'||app.go_json_string(effect->>'session_id')||
            ',"parent_session_id":'||app.go_json_string(effect->>'parent_session_id')||',"lineage_root_session_id":'||app.go_json_string(effect->>'lineage_root_session_id')||
            ',"projection_material_digest":'||app.go_json_string(effect->>'projection_material_digest')||CASE WHEN effect ? 'refresh_attempt_id' THEN ',"refresh_attempt_id":'||app.go_json_string(effect->>'refresh_attempt_id') ELSE '' END||',"status":'||app.go_json_string(effect->>'status')||
            ',"expires_at":'||app.go_json_string(effect->>'expires_at')||',"recovery_epoch":'||(effect->>'recovery_epoch')||'}'
          WHEN event_kind='session_revoked' THEN
            '{"kind":'||app.go_json_string(effect->>'kind')||',"account_id":'||app.go_json_string(effect->>'account_id')||
            ',"device_id":'||app.go_json_string(effect->>'device_id')||',"session_id":'||app.go_json_string(effect->>'session_id')||
            ',"lineage_root_session_id":'||app.go_json_string(effect->>'lineage_root_session_id')||',"status":'||app.go_json_string(effect->>'status')||
            ',"expires_at":'||app.go_json_string(effect->>'expires_at')||',"recovery_epoch":'||(effect->>'recovery_epoch')||'}'
          WHEN event_kind='device_binding_revoked' THEN
            '{"kind":'||app.go_json_string(effect->>'kind')||',"account_id":'||app.go_json_string(effect->>'account_id')||
            ',"device_id":'||app.go_json_string(effect->>'device_id')||',"server_instance_id":'||app.go_json_string(effect->>'server_instance_id')||
            ',"canonical_origin":'||app.go_json_string(effect->>'canonical_origin')||',"device_credential_digest":'||app.go_json_string(effect->>'device_credential_digest')||
            ',"status":'||app.go_json_string(effect->>'status')||'}'
          WHEN event_kind IN ('account_registered','identity_unavailable','identity_restored') THEN
            '{"kind":'||app.go_json_string(effect->>'kind')||',"account_id":'||app.go_json_string(effect->>'account_id')||
            ',"identity_key":'||app.go_json_string(effect->>'identity_key')||',"status":'||app.go_json_string(effect->>'status')||'}'
          WHEN event_kind IN ('family_created','family_recovered') THEN
            '{"kind":'||app.go_json_string(effect->>'kind')||',"account_id":'||app.go_json_string(effect->>'account_id')||
            ',"family_id":'||app.go_json_string(effect->>'family_id')||',"role":'||app.go_json_string(effect->>'role')||
            ',"status":'||app.go_json_string(effect->>'status')||'}'
          WHEN event_kind='family_deletion_scheduled' THEN
            '{"kind":'||app.go_json_string(effect->>'kind')||',"account_id":'||app.go_json_string(effect->>'account_id')||
            ',"family_id":'||app.go_json_string(effect->>'family_id')||',"role":'||app.go_json_string(effect->>'role')||
            ',"status":'||app.go_json_string(effect->>'status')||',"purge_after":'||app.go_json_string(effect->>'purge_after')||'}'
          WHEN event_kind='family_purge_started' THEN
            '{"kind":'||app.go_json_string(effect->>'kind')||',"family_id":'||app.go_json_string(effect->>'family_id')||
            ',"status":'||app.go_json_string(effect->>'status')||',"purge_after":'||app.go_json_string(effect->>'purge_after')||'}'
          WHEN event_kind='family_purge_completed' THEN
            '{"kind":'||app.go_json_string(effect->>'kind')||',"family_id":'||app.go_json_string(effect->>'family_id')||
            ',"canonical_origin":'||app.go_json_string(effect->>'canonical_origin')||',"status":'||app.go_json_string(effect->>'status')||'}'
        END;
        IF canonical_effect IS NULL THEN RAISE EXCEPTION 'effect kind mismatch' USING ERRCODE='22023'; END IF;
        IF event_kind='family_deletion_scheduled' THEN
            IF deletion_family IS NULL THEN deletion_family:=effect->>'family_id'; deletion_deadline:=effect->>'purge_after';
            ELSIF effect->>'family_id'<>deletion_family OR effect->>'purge_after'<>deletion_deadline THEN
                RAISE EXCEPTION 'family deletion effects disagree' USING ERRCODE='22023'; END IF;
            IF deletion_accounts ? (effect->>'account_id') THEN RAISE EXCEPTION 'family deletion account is duplicated' USING ERRCODE='22023'; END IF;
            deletion_accounts:=deletion_accounts||jsonb_build_object(effect->>'account_id',true);
            IF previous_canonical_effect IS NOT NULL AND previous_canonical_effect>=canonical_effect THEN
                RAISE EXCEPTION 'family deletion effects are not canonical' USING ERRCODE='22023'; END IF;
            previous_canonical_effect:=canonical_effect;
        END IF;
        IF NOT first_effect THEN canonical_effects:=canonical_effects||','; END IF;
        canonical_effects:=canonical_effects||canonical_effect; first_effect:=false;
    END LOOP;
    canonical_effects:=canonical_effects||']';
    canonical_event:='{"version":"v1","generation":'||p_generation::text||',"revision":'||p_revision::text||
      ',"previous_hash":'||app.go_json_string(p_event->>'previous_hash')||',"command_id":'||app.go_json_string(p_event->>'command_id')||
      ',"command_hash":'||app.go_json_string(p_event->>'command_hash')||',"actor_account_id":'||app.go_json_string(p_event->>'actor_account_id')||
      ',"actor_device_id":'||app.go_json_string(p_event->>'actor_device_id')||',"kind":'||app.go_json_string(event_kind)||
      ',"at":'||app.go_json_string(p_event->>'at')||',"effects":'||canonical_effects||'}';
    IF convert_from(p_event_bytes,'UTF8')<>canonical_event THEN RAISE EXCEPTION 'event bytes are not canonical' USING ERRCODE='22023'; END IF;

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

    IF event_kind='identity_session_issued' THEN
        FOR effect IN SELECT value FROM jsonb_array_elements(p_event->'effects') LOOP
            IF effect->>'kind'='account_registered' THEN
                account_count:=account_count+1; account_effect:=effect;
                IF (effect-ARRAY['kind','account_id','identity_key','status']::text[])<>'{}'::jsonb
                   OR NOT (effect ?& ARRAY['kind','account_id','identity_key','status']) OR effect->>'status'<>'active' THEN
                    RAISE EXCEPTION 'invalid account issue effect' USING ERRCODE='22023'; END IF;
            ELSIF effect->>'kind'='device_registered' THEN
                device_count:=device_count+1; device_effect:=effect;
                IF (effect-ARRAY['kind','account_id','device_id','server_instance_id','canonical_origin','device_credential_digest','status']::text[])<>'{}'::jsonb
                   OR NOT (effect ?& ARRAY['kind','account_id','device_id','server_instance_id','canonical_origin','device_credential_digest','status'])
                   OR effect->>'status'<>'active' THEN RAISE EXCEPTION 'invalid device issue effect' USING ERRCODE='22023'; END IF;
            ELSIF effect->>'kind'='session_granted' THEN
                session_count:=session_count+1; session_effect:=effect;
                IF (effect-ARRAY['kind','account_id','device_id','session_id','lineage_root_session_id','server_instance_id','canonical_origin',
                    'identity_key','device_credential_digest','projection_material_digest','status','expires_at','recovery_epoch']::text[])<>'{}'::jsonb
                   OR NOT (effect ?& ARRAY['kind','account_id','device_id','session_id','lineage_root_session_id','server_instance_id','canonical_origin',
                    'identity_key','device_credential_digest','projection_material_digest','status','expires_at','recovery_epoch'])
                   OR effect->>'status'<>'active' OR effect->>'lineage_root_session_id'<>effect->>'session_id'
                   OR effect->>'identity_key' !~ '^hmac-sha256:[a-z][a-z0-9._-]{0,63}:[0-9a-f]{64}$'
                   OR effect->>'device_credential_digest' !~ '^hmac-sha256:[a-z][a-z0-9._-]{0,63}:[0-9a-f]{64}$'
                   OR effect->>'projection_material_digest' !~ '^[0-9a-f]{64}$' THEN
                    RAISE EXCEPTION 'invalid session issue effect' USING ERRCODE='22023'; END IF;
            ELSE RAISE EXCEPTION 'invalid identity issue effect kind' USING ERRCODE='22023'; END IF;
        END LOOP;
        IF account_count>1 OR device_count>1 OR session_count<>1 OR session_effect->>'account_id'<>p_actor_account_id::text
           OR (account_count=1 AND (account_effect->>'account_id'<>session_effect->>'account_id' OR account_effect->>'identity_key'<>session_effect->>'identity_key'))
           OR (device_count=1 AND (device_effect->>'account_id'<>session_effect->>'account_id' OR device_effect->>'device_id'<>session_effect->>'device_id'
                OR device_effect->>'server_instance_id'<>session_effect->>'server_instance_id' OR device_effect->>'canonical_origin'<>session_effect->>'canonical_origin'
                OR device_effect->>'device_credential_digest'<>session_effect->>'device_credential_digest')) THEN
            RAISE EXCEPTION 'identity issue effects disagree' USING ERRCODE='22023'; END IF;
        SELECT * INTO staged FROM public.auth_exchange_material WHERE command_id=p_command_id FOR UPDATE;
        has_stage:=FOUND;
        IF has_stage AND (staged.command_hash<>p_command_hash OR encode(staged.projection_material_digest,'hex')<>session_effect->>'projection_material_digest'
            OR staged.material_kind<>'exchange' OR staged.account_id::text<>session_effect->>'account_id'
            OR staged.device_id::text<>session_effect->>'device_id' OR staged.session_id::text<>session_effect->>'session_id'
            OR staged.lineage_root_session_id::text<>session_effect->>'lineage_root_session_id'
            OR staged.server_instance_id::text<>session_effect->>'server_instance_id' OR staged.canonical_origin<>session_effect->>'canonical_origin'
            OR staged.recovery_epoch::text<>session_effect->>'recovery_epoch' OR staged.security_revision<>p_revision
            OR staged.absolute_expires_at<>(session_effect->>'expires_at')::timestamptz
            OR staged.register_account<>(account_count=1) OR staged.register_device<>(device_count=1)
            OR 'hmac-sha256:'||staged.identity_key_id||':'||encode(staged.identity_digest,'hex')<>session_effect->>'identity_key'
            OR (device_count=1 AND 'hmac-sha256:'||staged.continuity_key_id||':'||encode(staged.continuity_digest,'hex')<>session_effect->>'device_credential_digest')) THEN
            RAISE EXCEPTION 'staged issue material mismatch' USING ERRCODE='23514'; END IF;
        projected_identity_key_id:=split_part(session_effect->>'identity_key',':',2);
        projected_identity_digest:=decode(split_part(session_effect->>'identity_key',':',3),'hex');
        projected_continuity_key_id:=split_part(session_effect->>'device_credential_digest',':',2);
        projected_continuity_digest:=decode(split_part(session_effect->>'device_credential_digest',':',3),'hex');
        PERFORM set_config('app.account_id',session_effect->>'account_id',true);
        IF account_count=1 THEN
            INSERT INTO public.account_security_state(account_id,identity_key,status,generation,revision,event_hash)
            VALUES((session_effect->>'account_id')::uuid,session_effect->>'identity_key','active',p_generation,p_revision,p_event_hash);
            INSERT INTO public.accounts(id,display_name,profile_required,created_at,updated_at)
            VALUES((session_effect->>'account_id')::uuid,CASE WHEN has_stage THEN coalesce(staged.display_name,'') ELSE '' END,
                NOT has_stage OR staged.display_name IS NULL,(p_event->>'at')::timestamptz,(p_event->>'at')::timestamptz);
        ELSE
            IF NOT EXISTS(SELECT FROM public.account_security_state WHERE account_id=(session_effect->>'account_id')::uuid
                AND identity_key=session_effect->>'identity_key' AND status IN ('active','identity_unavailable'))
               OR NOT EXISTS(SELECT FROM public.accounts WHERE id=(session_effect->>'account_id')::uuid) THEN
                RAISE EXCEPTION 'identity issue account prerequisite missing' USING ERRCODE='23514'; END IF;
            UPDATE public.account_security_state SET status='active',generation=p_generation,revision=p_revision,event_hash=p_event_hash
             WHERE account_id=(session_effect->>'account_id')::uuid AND status='identity_unavailable';
            IF has_stage AND staged.display_name IS NOT NULL THEN
                UPDATE public.accounts SET display_name=staged.display_name,profile_required=false,
                    updated_at=(p_event->>'at')::timestamptz
                 WHERE id=(session_effect->>'account_id')::uuid AND profile_required IS TRUE;
            END IF;
        END IF;
        INSERT INTO public.account_identities(provider,identity_key_id,identity_digest,account_id,identity_audience,identity_subject,
            apple_credential_id,apple_credential_key_id,apple_credential_purpose,apple_credential_object_id,material_missing,created_at,updated_at)
        VALUES('apple',projected_identity_key_id,projected_identity_digest,(session_effect->>'account_id')::uuid,
            CASE WHEN has_stage THEN staged.identity_audience END,CASE WHEN has_stage THEN staged.identity_subject END,
            CASE WHEN has_stage THEN staged.apple_credential_id END,CASE WHEN has_stage THEN staged.apple_credential_key_id END,
            CASE WHEN has_stage THEN staged.apple_credential_purpose END,CASE WHEN has_stage THEN staged.apple_credential_object_id END,
            NOT has_stage,(p_event->>'at')::timestamptz,(p_event->>'at')::timestamptz)
        ON CONFLICT(provider,identity_key_id,identity_digest) DO UPDATE SET
            updated_at=CASE WHEN public.account_identities.material_missing THEN EXCLUDED.updated_at
                ELSE public.account_identities.updated_at END,
            identity_audience=CASE WHEN public.account_identities.material_missing THEN EXCLUDED.identity_audience ELSE public.account_identities.identity_audience END,
            identity_subject=CASE WHEN public.account_identities.material_missing THEN EXCLUDED.identity_subject ELSE public.account_identities.identity_subject END,
            apple_credential_id=CASE WHEN public.account_identities.material_missing THEN EXCLUDED.apple_credential_id ELSE public.account_identities.apple_credential_id END,
            apple_credential_key_id=CASE WHEN public.account_identities.material_missing THEN EXCLUDED.apple_credential_key_id ELSE public.account_identities.apple_credential_key_id END,
            apple_credential_purpose=CASE WHEN public.account_identities.material_missing THEN EXCLUDED.apple_credential_purpose ELSE public.account_identities.apple_credential_purpose END,
            apple_credential_object_id=CASE WHEN public.account_identities.material_missing THEN EXCLUDED.apple_credential_object_id ELSE public.account_identities.apple_credential_object_id END,
            material_missing=public.account_identities.material_missing AND EXCLUDED.material_missing
        WHERE public.account_identities.account_id=EXCLUDED.account_id;
        IF device_count=1 THEN
            INSERT INTO public.devices(id,account_id,server_instance_id,canonical_origin,installation_key_id,installation_digest,
                continuity_key_id,continuity_digest,created_at,updated_at)
            VALUES((session_effect->>'device_id')::uuid,(session_effect->>'account_id')::uuid,(session_effect->>'server_instance_id')::uuid,
                session_effect->>'canonical_origin',CASE WHEN has_stage THEN staged.installation_key_id END,
                CASE WHEN has_stage THEN staged.installation_digest END,projected_continuity_key_id,projected_continuity_digest,
                (p_event->>'at')::timestamptz,(p_event->>'at')::timestamptz);
        ELSIF NOT EXISTS(SELECT FROM public.devices d WHERE d.id=(session_effect->>'device_id')::uuid
            AND d.account_id=(session_effect->>'account_id')::uuid AND d.server_instance_id=(session_effect->>'server_instance_id')::uuid
            AND d.canonical_origin=session_effect->>'canonical_origin' AND d.continuity_key_id=projected_continuity_key_id
            AND d.continuity_digest=projected_continuity_digest AND d.binding_revoked_at IS NULL) THEN
            RAISE EXCEPTION 'identity issue device prerequisite missing' USING ERRCODE='23514';
        END IF;
        IF has_stage THEN
            INSERT INTO public.sessions(session_id,account_id,device_id,lineage_root_session_id,parent_session_id,server_instance_id,
                recovery_epoch,security_revision,scope,family_id,token_key_id,access_digest,refresh_digest,access_expires_at,
                refresh_expires_at,reauthenticate_at,absolute_expires_at,status,created_at,updated_at)
            VALUES(staged.session_id,staged.account_id,staged.device_id,staged.lineage_root_session_id,NULL,staged.server_instance_id,
                staged.recovery_epoch,p_revision,staged.scope,staged.family_id,staged.token_key_id,staged.access_digest,
                staged.refresh_digest,staged.access_expires_at,staged.refresh_expires_at,staged.reauthenticate_at,
                staged.absolute_expires_at,'active',(p_event->>'at')::timestamptz,(p_event->>'at')::timestamptz);
            -- Existing-device fresh proof replaces unresolved installs for this device only.
            IF device_count=0 THEN
                DELETE FROM public.refresh_recovery_receipts WHERE account_id=staged.account_id AND device_id=staged.device_id;
            END IF;
            UPDATE public.auth_exchange_material SET projected_at=(p_event->>'at')::timestamptz,
                token_key_id=NULL,access_digest=NULL,refresh_digest=NULL,
                identity_provider=NULL,identity_key_id=NULL,identity_digest=NULL,identity_audience=NULL,identity_subject=NULL,
                display_name=NULL,profile_confirmed=NULL,register_account=NULL,register_device=NULL,
                installation_key_id=NULL,installation_digest=NULL,continuity_key_id=NULL,continuity_digest=NULL,
                apple_credential_id=NULL,apple_credential_key_id=NULL,apple_credential_purpose=NULL,apple_credential_object_id=NULL
             WHERE command_id=p_command_id;
        ELSE
            INSERT INTO public.sessions(session_id,account_id,device_id,lineage_root_session_id,parent_session_id,server_instance_id,
                recovery_epoch,security_revision,scope,family_id,reauthenticate_at,absolute_expires_at,status,created_at,updated_at)
            VALUES((session_effect->>'session_id')::uuid,(session_effect->>'account_id')::uuid,(session_effect->>'device_id')::uuid,
                (session_effect->>'lineage_root_session_id')::uuid,NULL,(session_effect->>'server_instance_id')::uuid,
                (session_effect->>'recovery_epoch')::bigint,p_revision,'account',NULL,(session_effect->>'expires_at')::timestamptz,
                (session_effect->>'expires_at')::timestamptz,'material_missing',(p_event->>'at')::timestamptz,(p_event->>'at')::timestamptz);
        END IF;
    ELSIF event_kind='session_rotated' THEN
        effect:=p_event->'effects'->0;
        IF jsonb_array_length(p_event->'effects')<>1 OR (effect-ARRAY['kind','account_id','device_id','session_id','parent_session_id',
            'lineage_root_session_id','projection_material_digest','refresh_attempt_id','status','expires_at','recovery_epoch']::text[])<>'{}'::jsonb
           OR NOT (effect ?& ARRAY['kind','account_id','device_id','session_id','parent_session_id','lineage_root_session_id',
            'projection_material_digest','status','expires_at','recovery_epoch']) OR effect->>'kind'<>'session_rotated'
           OR effect->>'status'<>'active' OR effect->>'projection_material_digest' !~ '^[0-9a-f]{64}$' THEN
            RAISE EXCEPTION 'invalid session rotation effect' USING ERRCODE='22023'; END IF;
        SELECT * INTO staged FROM public.auth_exchange_material WHERE command_id=p_command_id FOR UPDATE;
        has_stage:=FOUND;
        IF has_stage AND (staged.command_hash<>p_command_hash OR staged.material_kind<>'refresh'
            OR encode(staged.projection_material_digest,'hex')<>effect->>'projection_material_digest'
            OR staged.account_id::text<>effect->>'account_id' OR staged.device_id::text<>effect->>'device_id'
            OR staged.session_id::text<>effect->>'session_id' OR staged.parent_session_id::text<>effect->>'parent_session_id'
            OR staged.lineage_root_session_id::text<>effect->>'lineage_root_session_id'
            OR staged.recovery_epoch::text<>effect->>'recovery_epoch' OR staged.security_revision<>p_revision
            OR staged.absolute_expires_at<>(effect->>'expires_at')::timestamptz) THEN
            RAISE EXCEPTION 'staged rotation material mismatch' USING ERRCODE='23514'; END IF;
        IF effect ? 'refresh_attempt_id' THEN
            IF NOT has_stage OR staged.recovery_material_bytes IS NULL OR sha256(staged.recovery_material_bytes)<>staged.projection_material_digest THEN
                RAISE EXCEPTION 'recovery rotation material unavailable' USING ERRCODE='23514'; END IF;
            recovery_material := convert_from(staged.recovery_material_bytes,'UTF8')::jsonb;
            frozen_recovery := recovery_material->'recovery'->'input';
            IF frozen_recovery->>'attempt_id' IS DISTINCT FROM effect->>'refresh_attempt_id'
               OR recovery_material->>'account_id' IS DISTINCT FROM effect->>'account_id'
               OR recovery_material->>'device_id' IS DISTINCT FROM effect->>'device_id'
               OR recovery_material->>'session_id' IS DISTINCT FROM effect->>'session_id'
               OR recovery_material->>'parent_session_id' IS DISTINCT FROM effect->>'parent_session_id'
               OR recovery_material->>'lineage_root_session_id' IS DISTINCT FROM effect->>'lineage_root_session_id'
               OR recovery_material->'recovery_epoch' IS DISTINCT FROM effect->'recovery_epoch'
               OR recovery_material->'security_revision' IS DISTINCT FROM p_event->'revision'
               OR (frozen_recovery->>'head_generation')::bigint<>p_generation
               OR (frozen_recovery->>'head_revision')::bigint<>p_revision-1
               OR app.refresh_recovery_digest(frozen_recovery->'head_hash')<>p_previous_hash
               OR recovery_material->>'token_key_id' IS DISTINCT FROM staged.token_key_id
               OR decode(recovery_material->>'access_digest','hex') IS DISTINCT FROM staged.access_digest
               OR decode(recovery_material->>'refresh_digest','hex') IS DISTINCT FROM staged.refresh_digest
               OR (recovery_material->>'access_expires_at')::timestamptz IS DISTINCT FROM staged.access_expires_at
               OR (recovery_material->>'refresh_expires_at')::timestamptz IS DISTINCT FROM staged.refresh_expires_at
               OR (recovery_material->>'reauthenticate_at')::timestamptz IS DISTINCT FROM staged.reauthenticate_at
               OR (recovery_material->>'absolute_expires_at')::timestamptz IS DISTINCT FROM staged.absolute_expires_at
               OR recovery_material->>'scope' IS DISTINCT FROM staged.scope
               OR (recovery_material->>'family_id')::uuid IS DISTINCT FROM staged.family_id
               OR (recovery_material->>'server_instance_id')::uuid IS DISTINCT FROM staged.server_instance_id
               OR recovery_material->>'canonical_origin' IS DISTINCT FROM staged.canonical_origin THEN
                RAISE EXCEPTION 'recovery committed material mismatch' USING ERRCODE='23514'; END IF;
            PERFORM 1 FROM public.sessions s JOIN public.devices d ON d.id=s.device_id
             WHERE s.session_id=staged.parent_session_id AND s.token_key_id=frozen_recovery->>'parent_key_id'
               AND s.refresh_digest=app.refresh_recovery_digest(frozen_recovery->'parent_digest')
               AND d.continuity_key_id=frozen_recovery->>'continuity_key_id'
               AND d.continuity_digest=app.refresh_recovery_digest(frozen_recovery->'continuity_digest')
               AND d.server_instance_id=staged.server_instance_id AND d.canonical_origin=staged.canonical_origin;
            IF NOT FOUND THEN RAISE EXCEPTION 'recovery parent binding mismatch' USING ERRCODE='23514'; END IF;
        ELSIF has_stage AND staged.recovery_material_bytes IS NOT NULL THEN
            RAISE EXCEPTION 'recovery stage requires authority attempt' USING ERRCODE='23514';
        END IF;
        IF has_stage THEN
            PERFORM 1 FROM public.sessions
             WHERE session_id=(effect->>'parent_session_id')::uuid AND account_id=(effect->>'account_id')::uuid
               AND device_id=(effect->>'device_id')::uuid AND lineage_root_session_id=(effect->>'lineage_root_session_id')::uuid
               AND recovery_epoch=(effect->>'recovery_epoch')::bigint AND status='active' AND token_key_id IS NOT NULL
             FOR UPDATE;
        ELSE
            PERFORM 1 FROM public.sessions
             WHERE session_id=(effect->>'parent_session_id')::uuid AND account_id=(effect->>'account_id')::uuid
               AND device_id=(effect->>'device_id')::uuid AND lineage_root_session_id=(effect->>'lineage_root_session_id')::uuid
               AND recovery_epoch=(effect->>'recovery_epoch')::bigint
               AND ((status='active' AND token_key_id IS NOT NULL)
                    OR (status='material_missing' AND consumed_at IS NULL AND revoked_at IS NULL))
             FOR UPDATE;
        END IF;
        IF NOT FOUND THEN RAISE EXCEPTION 'rotation parent unavailable' USING ERRCODE='23514'; END IF;
        INSERT INTO public.session_refresh_history(token_key_id,refresh_digest,session_id,lineage_root_session_id,consumed_at)
          SELECT token_key_id,refresh_digest,session_id,lineage_root_session_id,(p_event->>'at')::timestamptz
            FROM public.sessions WHERE session_id=(effect->>'parent_session_id')::uuid AND account_id=(effect->>'account_id')::uuid
             AND device_id=(effect->>'device_id')::uuid AND lineage_root_session_id=(effect->>'lineage_root_session_id')::uuid
             AND recovery_epoch=(effect->>'recovery_epoch')::bigint AND status='active' AND token_key_id IS NOT NULL;
        UPDATE public.sessions SET status=CASE WHEN status='material_missing' THEN status ELSE 'consumed' END,
            consumed_at=(p_event->>'at')::timestamptz,updated_at=(p_event->>'at')::timestamptz
         WHERE session_id=(effect->>'parent_session_id')::uuid AND account_id=(effect->>'account_id')::uuid
           AND device_id=(effect->>'device_id')::uuid AND lineage_root_session_id=(effect->>'lineage_root_session_id')::uuid
           AND recovery_epoch=(effect->>'recovery_epoch')::bigint
           AND ((has_stage AND status='active' AND token_key_id IS NOT NULL)
                OR (NOT has_stage AND ((status='active' AND token_key_id IS NOT NULL)
                    OR (status='material_missing' AND consumed_at IS NULL AND revoked_at IS NULL))));
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'rotation parent unavailable' USING ERRCODE='23514'; END IF;
        DELETE FROM public.refresh_recovery_receipts WHERE child_session_id=(effect->>'parent_session_id')::uuid;
        IF has_stage THEN
            INSERT INTO public.sessions(session_id,account_id,device_id,lineage_root_session_id,parent_session_id,server_instance_id,
                recovery_epoch,security_revision,scope,family_id,token_key_id,access_digest,refresh_digest,access_expires_at,
                refresh_expires_at,reauthenticate_at,absolute_expires_at,status,created_at,updated_at)
            VALUES(staged.session_id,staged.account_id,staged.device_id,staged.lineage_root_session_id,staged.parent_session_id,
                staged.server_instance_id,staged.recovery_epoch,p_revision,staged.scope,staged.family_id,
                staged.token_key_id,staged.access_digest,staged.refresh_digest,staged.access_expires_at,staged.refresh_expires_at,
                staged.reauthenticate_at,staged.absolute_expires_at,'active',(p_event->>'at')::timestamptz,(p_event->>'at')::timestamptz);
            IF effect ? 'refresh_attempt_id' THEN
                INSERT INTO public.refresh_recovery_receipts(server_instance_id,recovery_epoch,account_id,device_id,parent_session_id,
                    attempt_id,child_session_id,material_bytes,projection_material_digest,parent_key_id,output_key_id,continuity_key_id,
                    created_at,effective_deadline,refresh_expires_at)
                VALUES(staged.server_instance_id,staged.recovery_epoch,staged.account_id,staged.device_id,staged.parent_session_id,
                    (effect->>'refresh_attempt_id')::uuid,staged.session_id,staged.recovery_material_bytes,staged.projection_material_digest,
                    frozen_recovery->>'parent_key_id',frozen_recovery->>'output_key_id',frozen_recovery->>'continuity_key_id',
                    (recovery_material->'recovery'->>'created_at')::timestamptz,
                    (recovery_material->'recovery'->>'effective_deadline')::timestamptz,staged.refresh_expires_at);
            END IF;
            UPDATE public.auth_exchange_material SET projected_at=(p_event->>'at')::timestamptz,
                token_key_id=NULL,access_digest=NULL,refresh_digest=NULL,recovery_material_bytes=NULL,
                identity_provider=NULL,identity_key_id=NULL,identity_digest=NULL,identity_audience=NULL,identity_subject=NULL,
                display_name=NULL,profile_confirmed=NULL,register_account=NULL,register_device=NULL,
                installation_key_id=NULL,installation_digest=NULL,continuity_key_id=NULL,continuity_digest=NULL,
                apple_credential_id=NULL,apple_credential_key_id=NULL,apple_credential_purpose=NULL,apple_credential_object_id=NULL
             WHERE command_id=p_command_id;
        ELSE
            INSERT INTO public.sessions(session_id,account_id,device_id,lineage_root_session_id,parent_session_id,server_instance_id,
                recovery_epoch,security_revision,scope,family_id,reauthenticate_at,absolute_expires_at,status,created_at,updated_at)
              SELECT (effect->>'session_id')::uuid,account_id,device_id,lineage_root_session_id,(effect->>'parent_session_id')::uuid,
                server_instance_id,(effect->>'recovery_epoch')::bigint,p_revision,scope,family_id,(effect->>'expires_at')::timestamptz,
                (effect->>'expires_at')::timestamptz,'material_missing',(p_event->>'at')::timestamptz,(p_event->>'at')::timestamptz
                FROM public.sessions WHERE session_id=(effect->>'parent_session_id')::uuid;
        END IF;
    ELSIF event_kind='session_revoked' THEN
        effect:=p_event->'effects'->0;
        IF jsonb_array_length(p_event->'effects')<>1 OR (effect-ARRAY['kind','account_id','device_id','session_id','lineage_root_session_id',
            'status','expires_at','recovery_epoch']::text[])<>'{}'::jsonb
           OR NOT (effect ?& ARRAY['kind','account_id','device_id','session_id','lineage_root_session_id','status','expires_at','recovery_epoch'])
           OR effect->>'kind'<>'session_revoked' OR effect->>'status'<>'revoked' THEN
            RAISE EXCEPTION 'invalid session revocation effect' USING ERRCODE='22023'; END IF;
        IF NOT EXISTS(SELECT FROM public.sessions WHERE session_id=(effect->>'session_id')::uuid AND account_id=(effect->>'account_id')::uuid
            AND device_id=(effect->>'device_id')::uuid AND lineage_root_session_id=(effect->>'lineage_root_session_id')::uuid
            AND recovery_epoch=(effect->>'recovery_epoch')::bigint) THEN
            RAISE EXCEPTION 'session revocation prerequisite missing' USING ERRCODE='23514'; END IF;
        UPDATE public.sessions SET status=CASE WHEN status='material_missing' THEN status ELSE 'revoked' END,
            revoked_at=(p_event->>'at')::timestamptz,revoke_reason='refresh_reused',updated_at=(p_event->>'at')::timestamptz
         WHERE account_id=(effect->>'account_id')::uuid AND device_id=(effect->>'device_id')::uuid
           AND lineage_root_session_id=(effect->>'lineage_root_session_id')::uuid AND revoked_at IS NULL;
        UPDATE public.session_refresh_history SET reuse_detected_at=(p_event->>'at')::timestamptz
         WHERE lineage_root_session_id=(effect->>'lineage_root_session_id')::uuid AND reuse_detected_at IS NULL;
    ELSIF event_kind='device_binding_revoked' THEN
        effect:=p_event->'effects'->0;
        IF jsonb_array_length(p_event->'effects')<>1 OR (effect-ARRAY['kind','account_id','device_id','server_instance_id','canonical_origin',
            'device_credential_digest','status']::text[])<>'{}'::jsonb
           OR NOT (effect ?& ARRAY['kind','account_id','device_id','server_instance_id','canonical_origin','device_credential_digest','status'])
           OR effect->>'kind'<>'device_binding_revoked' OR effect->>'status'<>'revoked'
           OR effect->>'device_credential_digest' !~ '^hmac-sha256:[a-z][a-z0-9._-]{0,63}:[0-9a-f]{64}$' THEN
            RAISE EXCEPTION 'invalid device revocation effect' USING ERRCODE='22023'; END IF;
        UPDATE public.devices SET binding_revoked_at=(p_event->>'at')::timestamptz,updated_at=(p_event->>'at')::timestamptz
         WHERE id=(effect->>'device_id')::uuid AND account_id=(effect->>'account_id')::uuid
           AND server_instance_id=(effect->>'server_instance_id')::uuid AND canonical_origin=effect->>'canonical_origin'
           AND continuity_key_id=split_part(effect->>'device_credential_digest',':',2)
           AND continuity_digest=decode(split_part(effect->>'device_credential_digest',':',3),'hex') AND binding_revoked_at IS NULL;
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'device revocation prerequisite missing' USING ERRCODE='23514'; END IF;
        UPDATE public.sessions SET status=CASE WHEN status='material_missing' THEN status ELSE 'revoked' END,
            revoked_at=(p_event->>'at')::timestamptz,revoke_reason='device_revoked',updated_at=(p_event->>'at')::timestamptz
         WHERE account_id=(effect->>'account_id')::uuid AND device_id=(effect->>'device_id')::uuid AND revoked_at IS NULL;
    ELSE
        FOR effect IN SELECT value FROM jsonb_array_elements(p_event->'effects') LOOP
            IF effect->>'kind'<>event_kind THEN RAISE EXCEPTION 'effect kind mismatch' USING ERRCODE='22023'; END IF;
            IF event_kind IN ('account_registered','identity_unavailable','identity_restored') THEN
                IF jsonb_array_length(p_event->'effects')<>1 OR (effect-ARRAY['kind','account_id','identity_key','status']::text[])<>'{}'::jsonb
                   OR NOT (effect ?& ARRAY['kind','account_id','identity_key','status']) OR effect->>'identity_key'=''
                   OR effect->>'status' IS DISTINCT FROM (CASE WHEN event_kind='identity_unavailable' THEN 'identity_unavailable' ELSE 'active' END) THEN
                    RAISE EXCEPTION 'invalid account security effect' USING ERRCODE='22023'; END IF;
                IF event_kind='account_registered' THEN
                    INSERT INTO public.account_security_state(account_id,identity_key,status,generation,revision,event_hash)
                    VALUES((effect->>'account_id')::uuid,effect->>'identity_key','active',p_generation,p_revision,p_event_hash);
                ELSE
                    UPDATE public.account_security_state SET status=effect->>'status',generation=p_generation,revision=p_revision,event_hash=p_event_hash
                     WHERE account_id=(effect->>'account_id')::uuid AND identity_key=effect->>'identity_key';
                    GET DIAGNOSTICS affected_rows=ROW_COUNT;
                    IF affected_rows<>1 THEN RAISE EXCEPTION 'account security prerequisite missing' USING ERRCODE='23514'; END IF;
                    IF event_kind='identity_unavailable' THEN
                        UPDATE public.sessions SET status=CASE WHEN status='material_missing' THEN status ELSE 'revoked' END,
                            revoked_at=(p_event->>'at')::timestamptz,
                            revoke_reason='identity_unavailable',updated_at=(p_event->>'at')::timestamptz
                         WHERE account_id=(effect->>'account_id')::uuid AND revoked_at IS NULL;
                    END IF;
                END IF;
            ELSIF event_kind='family_created' THEN
                IF jsonb_array_length(p_event->'effects')<>1 OR (effect-ARRAY['kind','account_id','family_id','role','status']::text[])<>'{}'::jsonb
                   OR NOT (effect ?& ARRAY['kind','account_id','family_id','role','status']) OR effect->>'role'<>'owner' OR effect->>'status'<>'active' THEN
                    RAISE EXCEPTION 'invalid family creation effect' USING ERRCODE='22023'; END IF;
                INSERT INTO public.family_security_state(family_id,owner_account_id,lifecycle,generation,revision,event_hash)
                VALUES((effect->>'family_id')::uuid,(effect->>'account_id')::uuid,'active',p_generation,p_revision,p_event_hash);
            ELSIF event_kind='family_deletion_scheduled' THEN
                IF (effect-ARRAY['kind','account_id','family_id','role','status','purge_after']::text[])<>'{}'::jsonb
                   OR NOT (effect ?& ARRAY['kind','account_id','family_id','role','status','purge_after'])
                   OR effect->>'role' NOT IN ('owner','caregiver') OR effect->>'status' NOT IN ('deletion_pending','former')
                   OR (effect->>'status'='deletion_pending' AND effect->>'role'<>'owner') THEN
                    RAISE EXCEPTION 'invalid family deletion effect' USING ERRCODE='22023'; END IF;
                IF effect->>'status'='deletion_pending' THEN
                    anchor_count:=anchor_count+1;
                    UPDATE public.family_security_state SET lifecycle='deletion_pending',purge_after=(effect->>'purge_after')::timestamptz,
                        generation=p_generation,revision=p_revision,event_hash=p_event_hash
                     WHERE family_id=(effect->>'family_id')::uuid AND owner_account_id=(effect->>'account_id')::uuid AND lifecycle='active';
                    GET DIAGNOSTICS affected_rows=ROW_COUNT;
                    IF affected_rows<>1 THEN RAISE EXCEPTION 'family deletion prerequisite missing' USING ERRCODE='23514'; END IF;
                END IF;
            ELSIF event_kind='family_recovered' THEN
                IF jsonb_array_length(p_event->'effects')<>1 OR (effect-ARRAY['kind','account_id','family_id','role','status']::text[])<>'{}'::jsonb
                   OR NOT (effect ?& ARRAY['kind','account_id','family_id','role','status']) OR effect->>'role'<>'owner' OR effect->>'status'<>'active' THEN
                    RAISE EXCEPTION 'invalid family recovery effect' USING ERRCODE='22023'; END IF;
                UPDATE public.family_security_state SET lifecycle='active',owner_account_id=(effect->>'account_id')::uuid,purge_after=NULL,
                    generation=p_generation,revision=p_revision,event_hash=p_event_hash
                 WHERE family_id=(effect->>'family_id')::uuid AND lifecycle='deletion_pending';
                GET DIAGNOSTICS affected_rows=ROW_COUNT;
                IF affected_rows<>1 THEN RAISE EXCEPTION 'family is not recoverable' USING ERRCODE='23514'; END IF;
            ELSIF event_kind='family_purge_started' THEN
                IF jsonb_array_length(p_event->'effects')<>1 OR (effect-ARRAY['kind','family_id','status','purge_after']::text[])<>'{}'::jsonb
                   OR NOT (effect ?& ARRAY['kind','family_id','status','purge_after']) OR effect->>'status'<>'purge_started' THEN
                    RAISE EXCEPTION 'invalid purge start effect' USING ERRCODE='22023'; END IF;
                UPDATE public.family_security_state SET lifecycle='purge_started',generation=p_generation,revision=p_revision,event_hash=p_event_hash
                 WHERE family_id=(effect->>'family_id')::uuid AND lifecycle='deletion_pending'
                   AND purge_after::text=(effect->>'purge_after')::timestamptz::text AND purge_after<=(p_event->>'at')::timestamptz;
                GET DIAGNOSTICS affected_rows=ROW_COUNT;
                IF affected_rows<>1 THEN RAISE EXCEPTION 'family purge is not due' USING ERRCODE='23514'; END IF;
            ELSIF event_kind='family_purge_completed' THEN
                IF jsonb_array_length(p_event->'effects')<>1 OR (effect-ARRAY['kind','family_id','canonical_origin','status']::text[])<>'{}'::jsonb
                   OR NOT (effect ?& ARRAY['kind','family_id','canonical_origin','status']) OR effect->>'canonical_origin'='' OR effect->>'status'<>'purge_completed' THEN
                    RAISE EXCEPTION 'invalid purge completion effect' USING ERRCODE='22023'; END IF;
                UPDATE public.family_security_state SET lifecycle='purge_completed',generation=p_generation,revision=p_revision,event_hash=p_event_hash
                 WHERE family_id=(effect->>'family_id')::uuid AND lifecycle='purge_started';
                GET DIAGNOSTICS affected_rows=ROW_COUNT;
                IF affected_rows<>1 THEN RAISE EXCEPTION 'family purge was not started' USING ERRCODE='23514'; END IF;
            END IF;
        END LOOP;
    END IF;
    IF event_kind='family_deletion_scheduled' AND anchor_count<>1 THEN
        RAISE EXCEPTION 'family deletion requires one anchor' USING ERRCODE='22023'; END IF;
    IF event_kind IN ('session_revoked','device_binding_revoked','identity_unavailable') THEN
        DELETE FROM public.refresh_recovery_receipts r USING public.sessions s
        WHERE r.child_session_id=s.session_id AND r.account_id=p_actor_account_id
          AND (s.revoked_at IS NOT NULL OR s.status IN ('revoked','consumed'));
    END IF;
    PERFORM set_config('app.account_id',previous_account_setting,true);
    INSERT INTO public.security_audit_events(generation,revision,event_hash,command_id,actor_account_id,kind,event_data,event_bytes)
    VALUES(p_generation,p_revision,p_event_hash,p_command_id,p_actor_account_id,event_kind,p_event,p_event_bytes);
    INSERT INTO public.security_command_receipts(actor_account_id,command_id,command_hash,generation,revision,result)
    VALUES(p_actor_account_id,p_command_id,p_command_hash,p_generation,p_revision,p_result);
    UPDATE public.security_projection_head SET generation=p_generation,revision=p_revision,event_hash=p_event_hash WHERE singleton;
END;
$$;

CREATE FUNCTION app.lookup_refresh_recovery_receipt(p_parent_session_id uuid,p_attempt_id uuid)
RETURNS TABLE(material bytea,projection_material_digest bytea,child_session_id uuid,parent_session_id uuid,attempt_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
    SELECT r.material_bytes,r.projection_material_digest,r.child_session_id,r.parent_session_id,r.attempt_id
    FROM public.refresh_recovery_receipts r WHERE r.parent_session_id=p_parent_session_id AND r.attempt_id=p_attempt_id;
$$;
REVOKE ALL ON FUNCTION app.lookup_refresh_recovery_receipt(uuid,uuid) FROM PUBLIC,baby_tracker_projector,baby_tracker_apple_revoker;
GRANT EXECUTE ON FUNCTION app.lookup_refresh_recovery_receipt(uuid,uuid) TO baby_tracker_runtime;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 25,principal,object_kind,object_identity,privileges FROM public.schema_privilege_manifest WHERE schema_version=24;
INSERT INTO public.schema_privilege_manifest VALUES
 (25,'baby_tracker_runtime','function','app.lookup_refresh_recovery_receipt(uuid,uuid)','EXECUTE');
UPDATE public.schema_compatibility SET current_version=25,introduced_by='000025_refresh_recovery.sql',recorded_at=now() WHERE singleton;


CREATE INDEX refresh_recovery_device_attempt ON public.refresh_recovery_receipts(account_id,attempt_id);
CREATE INDEX refresh_recovery_deadline ON public.refresh_recovery_receipts(LEAST(effective_deadline,refresh_expires_at));

CREATE FUNCTION app.lookup_refresh_recovery_child(p_session_id uuid)
RETURNS TABLE(token_key_id text,refresh_digest bytea)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT s.token_key_id,s.refresh_digest FROM public.sessions s WHERE s.session_id=p_session_id AND s.token_key_id IS NOT NULL;
$$;
CREATE FUNCTION app.lookup_refresh_recovery_ack(p_account_id uuid,p_device_id uuid,p_instance_id uuid,p_epoch bigint,p_attempt_id uuid)
RETURNS TABLE(material bytea,projection_material_digest bytea,child_session_id uuid,parent_session_id uuid,attempt_id uuid)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT r.material_bytes,r.projection_material_digest,r.child_session_id,r.parent_session_id,r.attempt_id
 FROM public.refresh_recovery_receipts r WHERE r.account_id=p_account_id AND r.device_id=p_device_id
 AND r.server_instance_id=p_instance_id AND r.recovery_epoch=p_epoch AND r.attempt_id=p_attempt_id;
$$;
CREATE FUNCTION app.ack_refresh_recovery_receipt(p_account_id uuid,p_device_id uuid,p_instance_id uuid,p_attempt_id uuid,p_epoch bigint,p_lineage uuid)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
 DELETE FROM public.refresh_recovery_receipts r USING public.sessions s
 WHERE r.child_session_id=s.session_id AND r.account_id=p_account_id AND r.device_id=p_device_id
 AND r.server_instance_id=p_instance_id AND r.attempt_id=p_attempt_id AND r.recovery_epoch=p_epoch
 AND s.lineage_root_session_id=p_lineage;
$$;
CREATE FUNCTION app.cleanup_refresh_recovery_receipts(p_now timestamptz)
RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
 DELETE FROM public.refresh_recovery_receipts WHERE child_session_id IN (
 SELECT r.child_session_id FROM public.refresh_recovery_receipts r WHERE
 LEAST(r.effective_deadline,r.refresh_expires_at)<=p_now
 OR EXISTS(SELECT FROM public.sessions s WHERE s.session_id=r.child_session_id AND (s.status IN ('revoked','consumed') OR s.revoked_at IS NOT NULL))
 OR EXISTS(SELECT FROM public.devices d WHERE d.id=r.device_id AND d.binding_revoked_at IS NOT NULL)
 OR EXISTS(SELECT FROM public.account_security_state a WHERE a.account_id=r.account_id AND a.status<>'active')
 OR EXISTS(SELECT FROM public.server_state s WHERE s.singleton AND (s.instance_id<>r.server_instance_id OR s.recovery_epoch<>r.recovery_epoch))
 ORDER BY LEAST(r.effective_deadline,r.refresh_expires_at),r.child_session_id LIMIT 512);
$$;
CREATE FUNCTION app.refresh_recovery_key_candidates()
RETURNS TABLE(session_id uuid,account_id uuid,lineage_id uuid,token_key_id text,parent_key_id text,continuity_key_id text,
 refresh_expires_at timestamptz,reauthenticate_at timestamptz,absolute_expires_at timestamptz,effective_deadline timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT s.session_id,s.account_id,s.lineage_root_session_id,s.token_key_id,NULL::text,NULL::text,
 s.refresh_expires_at,s.reauthenticate_at,s.absolute_expires_at,NULL::timestamptz
 FROM public.sessions s WHERE s.status='active' AND s.revoked_at IS NULL AND s.token_key_id IS NOT NULL
 UNION ALL
 SELECT r.child_session_id,r.account_id,s.lineage_root_session_id,r.output_key_id,r.parent_key_id,r.continuity_key_id,
 r.refresh_expires_at,s.reauthenticate_at,s.absolute_expires_at,r.effective_deadline
 FROM public.refresh_recovery_receipts r JOIN public.sessions s ON s.session_id=r.child_session_id;
$$;
REVOKE ALL ON FUNCTION app.lookup_refresh_recovery_child(uuid),app.lookup_refresh_recovery_ack(uuid,uuid,uuid,bigint,uuid),
 app.ack_refresh_recovery_receipt(uuid,uuid,uuid,uuid,bigint,uuid),app.cleanup_refresh_recovery_receipts(timestamptz),
 app.refresh_recovery_key_candidates() FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker,baby_tracker_maintenance;
GRANT EXECUTE ON FUNCTION app.lookup_refresh_recovery_child(uuid),app.lookup_refresh_recovery_ack(uuid,uuid,uuid,bigint,uuid),
 app.ack_refresh_recovery_receipt(uuid,uuid,uuid,uuid,bigint,uuid),app.refresh_recovery_key_candidates() TO baby_tracker_runtime;
GRANT EXECUTE ON FUNCTION app.cleanup_refresh_recovery_receipts(timestamptz) TO baby_tracker_maintenance;
INSERT INTO public.schema_privilege_manifest VALUES
 (25,'baby_tracker_runtime','function','app.lookup_refresh_recovery_child(uuid)','EXECUTE'),
 (25,'baby_tracker_runtime','function','app.lookup_refresh_recovery_ack(uuid,uuid,uuid,bigint,uuid)','EXECUTE'),
 (25,'baby_tracker_runtime','function','app.ack_refresh_recovery_receipt(uuid,uuid,uuid,uuid,bigint,uuid)','EXECUTE'),
 (25,'baby_tracker_runtime','function','app.refresh_recovery_key_candidates()','EXECUTE'),
 (25,'baby_tracker_maintenance','function','app.cleanup_refresh_recovery_receipts(timestamptz)','EXECUTE');

GRANT USAGE ON SCHEMA app TO baby_tracker_maintenance;
INSERT INTO public.schema_privilege_manifest VALUES (25,'baby_tracker_maintenance','schema','app','USAGE');
