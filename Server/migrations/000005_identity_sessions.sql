ALTER TABLE public.accounts
    ADD COLUMN profile_required boolean NOT NULL DEFAULT false;

CREATE TABLE public.account_identities (
    provider text NOT NULL CHECK (provider = 'apple'),
    identity_key_id text NOT NULL CHECK (identity_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'),
    identity_digest bytea NOT NULL CHECK (octet_length(identity_digest) = 32),
    account_id uuid NOT NULL REFERENCES public.accounts(id),
    identity_audience text,
    identity_subject text,
    apple_credential_id text,
    apple_credential_key_id text,
    apple_credential_purpose text,
    apple_credential_object_id uuid,
    material_missing boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (provider, identity_key_id, identity_digest),
    UNIQUE (account_id, provider),
    CHECK (account_id <> '00000000-0000-0000-0000-000000000000'),
    CHECK ((material_missing IS TRUE AND identity_audience IS NULL AND identity_subject IS NULL
            AND apple_credential_id IS NULL AND apple_credential_key_id IS NULL
            AND apple_credential_purpose IS NULL AND apple_credential_object_id IS NULL)
        OR (material_missing IS FALSE
            AND identity_audience IS NOT NULL AND octet_length(identity_audience) BETWEEN 1 AND 255
            AND identity_subject IS NOT NULL AND octet_length(identity_subject) BETWEEN 1 AND 255
            AND ((apple_credential_id IS NULL AND apple_credential_key_id IS NULL
                  AND apple_credential_purpose IS NULL AND apple_credential_object_id IS NULL)
              OR (apple_credential_id IS NOT NULL AND apple_credential_id ~ '^[A-Za-z0-9._-]{1,128}$'
                  AND apple_credential_key_id IS NOT NULL AND apple_credential_key_id ~ '^[A-Za-z0-9._-]{1,128}$'
                  AND apple_credential_purpose IS NOT NULL AND apple_credential_purpose = 'apple-refresh-revocation-v1'
                  AND apple_credential_object_id IS NOT NULL
                  AND apple_credential_object_id <> '00000000-0000-0000-0000-000000000000')))),
    CHECK (isfinite(created_at) AND isfinite(updated_at))
);

CREATE TABLE public.devices (
    id uuid PRIMARY KEY CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    account_id uuid NOT NULL REFERENCES public.accounts(id),
    server_instance_id uuid NOT NULL CHECK (server_instance_id <> '00000000-0000-0000-0000-000000000000'),
    canonical_origin text NOT NULL CHECK (octet_length(canonical_origin) BETWEEN 1 AND 2048
        AND octet_length(canonical_origin) = char_length(canonical_origin)),
    installation_key_id text CHECK (installation_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'),
    installation_digest bytea CHECK (octet_length(installation_digest) = 32),
    continuity_key_id text NOT NULL CHECK (continuity_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'),
    continuity_digest bytea NOT NULL CHECK (octet_length(continuity_digest) = 32),
    binding_revoked_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (id, account_id),
    CHECK ((installation_key_id IS NULL) = (installation_digest IS NULL)),
    CHECK ((binding_revoked_at IS NULL OR isfinite(binding_revoked_at)) AND isfinite(created_at) AND isfinite(updated_at))
);

CREATE TABLE public.auth_challenges (
    id uuid PRIMARY KEY CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    token_digest bytea NOT NULL UNIQUE CHECK (octet_length(token_digest) = 32),
    canonical_origin text NOT NULL CHECK (octet_length(canonical_origin) BETWEEN 1 AND 2048
        AND octet_length(canonical_origin) = char_length(canonical_origin)),
    server_instance_id uuid NOT NULL CHECK (server_instance_id <> '00000000-0000-0000-0000-000000000000'),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL,
    CHECK (isfinite(created_at) AND isfinite(expires_at) AND expires_at = created_at + interval '5 minutes'),
    CHECK (consumed_at IS NULL OR isfinite(consumed_at))
);
CREATE INDEX auth_challenges_unconsumed_expiry_idx
    ON public.auth_challenges(expires_at) WHERE consumed_at IS NULL;

CREATE TABLE public.auth_exchange_material (
    command_id uuid PRIMARY KEY CHECK (command_id <> '00000000-0000-0000-0000-000000000000'),
    command_hash bytea NOT NULL CHECK (octet_length(command_hash) = 32),
    projection_material_digest bytea NOT NULL CHECK (octet_length(projection_material_digest) = 32),
    material_kind text NOT NULL CHECK (material_kind IN ('exchange', 'refresh')),
    account_id uuid NOT NULL CHECK (account_id <> '00000000-0000-0000-0000-000000000000'),
    device_id uuid NOT NULL CHECK (device_id <> '00000000-0000-0000-0000-000000000000'),
    session_id uuid NOT NULL CHECK (session_id <> '00000000-0000-0000-0000-000000000000'),
    parent_session_id uuid CHECK (parent_session_id <> '00000000-0000-0000-0000-000000000000'),
    lineage_root_session_id uuid NOT NULL CHECK (lineage_root_session_id <> '00000000-0000-0000-0000-000000000000'),
    server_instance_id uuid NOT NULL CHECK (server_instance_id <> '00000000-0000-0000-0000-000000000000'),
    canonical_origin text NOT NULL CHECK (octet_length(canonical_origin) BETWEEN 1 AND 2048
        AND octet_length(canonical_origin) = char_length(canonical_origin)),
    token_key_id text CHECK (token_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'),
    access_digest bytea CHECK (octet_length(access_digest) = 32),
    refresh_digest bytea CHECK (octet_length(refresh_digest) = 32),
    recovery_epoch bigint NOT NULL CHECK (recovery_epoch BETWEEN 1 AND 9007199254740991),
    security_revision bigint NOT NULL CHECK (security_revision BETWEEN 1 AND 9007199254740991),
    scope text NOT NULL CHECK (scope IN ('account', 'family', 'family_recovery')),
    family_id uuid,
    access_expires_at timestamptz NOT NULL,
    refresh_expires_at timestamptz NOT NULL,
    reauthenticate_at timestamptz NOT NULL,
    absolute_expires_at timestamptz NOT NULL,
    identity_provider text,
    identity_key_id text,
    identity_digest bytea,
    identity_audience text,
    identity_subject text,
    display_name text,
    profile_confirmed boolean,
    register_account boolean,
    register_device boolean,
    installation_key_id text,
    installation_digest bytea,
    continuity_key_id text,
    continuity_digest bytea,
    apple_credential_id text,
    apple_credential_key_id text,
    apple_credential_purpose text,
    apple_credential_object_id uuid,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    projected_at timestamptz,
    CHECK ((scope = 'account' AND family_id IS NULL) OR (scope <> 'account' AND family_id IS NOT NULL
        AND family_id <> '00000000-0000-0000-0000-000000000000')),
    CHECK (isfinite(access_expires_at) AND isfinite(refresh_expires_at) AND isfinite(reauthenticate_at)
        AND isfinite(absolute_expires_at) AND access_expires_at <= refresh_expires_at
        AND refresh_expires_at <= reauthenticate_at AND reauthenticate_at = absolute_expires_at),
    CHECK (isfinite(created_at) AND isfinite(expires_at) AND expires_at = created_at + interval '10 minutes'
        AND (projected_at IS NULL OR isfinite(projected_at))),
    CHECK ((projected_at IS NULL AND token_key_id IS NOT NULL AND access_digest IS NOT NULL AND refresh_digest IS NOT NULL
        AND ((material_kind = 'refresh' AND parent_session_id IS NOT NULL
            AND identity_provider IS NULL AND identity_key_id IS NULL AND identity_digest IS NULL
            AND identity_audience IS NULL AND identity_subject IS NULL AND display_name IS NULL
            AND profile_confirmed IS NULL AND register_account IS NULL AND register_device IS NULL
            AND installation_key_id IS NULL AND installation_digest IS NULL
            AND continuity_key_id IS NULL AND continuity_digest IS NULL
            AND apple_credential_id IS NULL AND apple_credential_key_id IS NULL
            AND apple_credential_purpose IS NULL AND apple_credential_object_id IS NULL)
        OR (material_kind = 'exchange' AND parent_session_id IS NULL
            AND identity_provider IS NOT NULL AND identity_provider = 'apple'
            AND identity_key_id IS NOT NULL AND identity_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'
            AND identity_digest IS NOT NULL AND octet_length(identity_digest) = 32
            AND identity_audience IS NOT NULL AND octet_length(identity_audience) BETWEEN 1 AND 255
            AND identity_subject IS NOT NULL AND octet_length(identity_subject) BETWEEN 1 AND 255
            AND profile_confirmed IS NOT NULL AND register_account IS NOT NULL AND register_device IS NOT NULL
            AND ((display_name IS NULL AND profile_confirmed IS FALSE)
                 OR (display_name IS NOT NULL AND profile_confirmed IS TRUE AND char_length(display_name) BETWEEN 1 AND 80
                     AND octet_length(display_name) <= 320))
            AND ((installation_key_id IS NULL AND installation_digest IS NULL)
                 OR (installation_key_id IS NOT NULL AND installation_digest IS NOT NULL
                     AND installation_key_id ~ '^[a-z][a-z0-9._-]{0,63}$' AND octet_length(installation_digest) = 32))
            AND ((register_device IS TRUE AND continuity_key_id IS NOT NULL AND continuity_digest IS NOT NULL
                     AND continuity_key_id ~ '^[a-z][a-z0-9._-]{0,63}$' AND octet_length(continuity_digest) = 32)
                 OR (register_device IS FALSE AND continuity_key_id IS NULL AND continuity_digest IS NULL))
            AND ((apple_credential_id IS NULL AND apple_credential_key_id IS NULL
                  AND apple_credential_purpose IS NULL AND apple_credential_object_id IS NULL)
                 OR (apple_credential_id IS NOT NULL AND apple_credential_key_id IS NOT NULL
                     AND apple_credential_purpose IS NOT NULL AND apple_credential_object_id IS NOT NULL
                     AND apple_credential_id ~ '^[A-Za-z0-9._-]{1,128}$'
                     AND apple_credential_key_id ~ '^[A-Za-z0-9._-]{1,128}$'
                     AND apple_credential_purpose = 'apple-refresh-revocation-v1'
                     AND apple_credential_object_id <> '00000000-0000-0000-0000-000000000000')))))
        OR (projected_at IS NOT NULL AND token_key_id IS NULL AND access_digest IS NULL AND refresh_digest IS NULL
            AND identity_provider IS NULL AND identity_key_id IS NULL AND identity_digest IS NULL
            AND identity_audience IS NULL AND identity_subject IS NULL AND display_name IS NULL
            AND profile_confirmed IS NULL AND register_account IS NULL AND register_device IS NULL
            AND installation_key_id IS NULL AND installation_digest IS NULL
            AND continuity_key_id IS NULL AND continuity_digest IS NULL
            AND apple_credential_id IS NULL AND apple_credential_key_id IS NULL
            AND apple_credential_purpose IS NULL AND apple_credential_object_id IS NULL))
);

CREATE TABLE public.sessions (
    session_id uuid PRIMARY KEY CHECK (session_id <> '00000000-0000-0000-0000-000000000000'),
    account_id uuid NOT NULL REFERENCES public.accounts(id),
    device_id uuid NOT NULL,
    lineage_root_session_id uuid NOT NULL,
    parent_session_id uuid,
    server_instance_id uuid NOT NULL CHECK (server_instance_id <> '00000000-0000-0000-0000-000000000000'),
    recovery_epoch bigint NOT NULL CHECK (recovery_epoch BETWEEN 1 AND 9007199254740991),
    security_revision bigint NOT NULL CHECK (security_revision BETWEEN 1 AND 9007199254740991),
    scope text NOT NULL CHECK (scope IN ('account', 'family', 'family_recovery')),
    family_id uuid,
    token_key_id text CHECK (token_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'),
    access_digest bytea CHECK (octet_length(access_digest) = 32),
    refresh_digest bytea CHECK (octet_length(refresh_digest) = 32),
    access_expires_at timestamptz,
    refresh_expires_at timestamptz,
    reauthenticate_at timestamptz NOT NULL,
    absolute_expires_at timestamptz NOT NULL,
    status text NOT NULL CHECK (status IN ('active', 'consumed', 'revoked', 'material_missing')),
    consumed_at timestamptz,
    revoked_at timestamptz,
    revoke_reason text CHECK (revoke_reason ~ '^[a-z][a-z0-9_]{0,63}$'),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (device_id, account_id) REFERENCES public.devices(id, account_id),
    FOREIGN KEY (lineage_root_session_id) REFERENCES public.sessions(session_id) DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (parent_session_id) REFERENCES public.sessions(session_id) DEFERRABLE INITIALLY DEFERRED,
    CHECK ((scope = 'account' AND family_id IS NULL) OR (scope <> 'account' AND family_id IS NOT NULL
        AND family_id <> '00000000-0000-0000-0000-000000000000')),
    CHECK (isfinite(reauthenticate_at) AND isfinite(absolute_expires_at) AND reauthenticate_at = absolute_expires_at),
    CHECK (isfinite(created_at) AND isfinite(updated_at)
        AND (consumed_at IS NULL OR isfinite(consumed_at)) AND (revoked_at IS NULL OR isfinite(revoked_at))),
    CHECK ((status = 'material_missing' AND token_key_id IS NULL AND access_digest IS NULL AND refresh_digest IS NULL
             AND access_expires_at IS NULL AND refresh_expires_at IS NULL)
            OR (status IN ('active','consumed','revoked')
                AND token_key_id IS NOT NULL AND access_digest IS NOT NULL AND refresh_digest IS NOT NULL
                AND access_expires_at IS NOT NULL AND refresh_expires_at IS NOT NULL
                AND isfinite(access_expires_at) AND isfinite(refresh_expires_at)
                AND access_expires_at <= refresh_expires_at AND refresh_expires_at <= reauthenticate_at)),
    CHECK ((revoked_at IS NULL AND revoke_reason IS NULL)
        OR (revoked_at IS NOT NULL AND revoke_reason IS NOT NULL)),
    CHECK ((status = 'active' AND consumed_at IS NULL AND revoked_at IS NULL)
        OR (status = 'consumed' AND consumed_at IS NOT NULL AND revoked_at IS NULL)
        OR (status = 'revoked' AND revoked_at IS NOT NULL)
        OR status = 'material_missing')
);
CREATE UNIQUE INDEX sessions_access_digest_idx ON public.sessions(token_key_id, access_digest)
    WHERE token_key_id IS NOT NULL AND access_digest IS NOT NULL;
CREATE UNIQUE INDEX sessions_refresh_digest_idx ON public.sessions(token_key_id, refresh_digest)
    WHERE token_key_id IS NOT NULL AND refresh_digest IS NOT NULL;
CREATE INDEX sessions_account_status_idx ON public.sessions(account_id, status);
CREATE INDEX sessions_device_status_idx ON public.sessions(device_id, status);
CREATE INDEX sessions_lineage_status_idx ON public.sessions(lineage_root_session_id, status);

CREATE TABLE public.session_refresh_history (
    token_key_id text NOT NULL CHECK (token_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'),
    refresh_digest bytea NOT NULL CHECK (octet_length(refresh_digest) = 32),
    session_id uuid NOT NULL REFERENCES public.sessions(session_id),
    lineage_root_session_id uuid NOT NULL REFERENCES public.sessions(session_id),
    consumed_at timestamptz NOT NULL CHECK (isfinite(consumed_at)),
    reuse_detected_at timestamptz CHECK (reuse_detected_at IS NULL OR isfinite(reuse_detected_at)),
    PRIMARY KEY (token_key_id, refresh_digest)
);
CREATE INDEX session_refresh_history_lineage_idx ON public.session_refresh_history(lineage_root_session_id);

CREATE TABLE public.apple_notification_dedup (
    notification_key_id text NOT NULL CHECK (notification_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'),
    notification_digest bytea NOT NULL CHECK (octet_length(notification_digest) = 32),
    command_id uuid NOT NULL UNIQUE CHECK (command_id <> '00000000-0000-0000-0000-000000000000'),
    received_at timestamptz NOT NULL CHECK (isfinite(received_at)),
    PRIMARY KEY (notification_key_id, notification_digest)
);

CREATE TABLE public.apple_revocation_jobs (
    job_id uuid PRIMARY KEY CHECK (job_id <> '00000000-0000-0000-0000-000000000000'),
    account_id uuid NOT NULL REFERENCES public.accounts(id),
    server_instance_id uuid NOT NULL CHECK (server_instance_id <> '00000000-0000-0000-0000-000000000000'),
    credential_purpose text NOT NULL CHECK (credential_purpose = 'apple-refresh-revocation-v1'),
    credential_object_id uuid NOT NULL CHECK (credential_object_id <> '00000000-0000-0000-0000-000000000000'),
    credential_reference_id text NOT NULL CHECK (credential_reference_id ~ '^[A-Za-z0-9._-]{1,128}$'),
    credential_reference_key_id text NOT NULL CHECK (credential_reference_key_id ~ '^[A-Za-z0-9._-]{1,128}$'),
    status text NOT NULL CHECK (status IN ('pending', 'retry', 'complete')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 1000000),
    next_attempt_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    last_error_code text CHECK (last_error_code ~ '^[a-z0-9_]{1,64}$'),
    UNIQUE (account_id, server_instance_id, credential_purpose, credential_object_id,
        credential_reference_id, credential_reference_key_id),
    CHECK (isfinite(next_attempt_at) AND isfinite(created_at) AND isfinite(updated_at)
        AND (completed_at IS NULL OR isfinite(completed_at)))
);

CREATE TABLE public.rate_limit_buckets (
    scope text NOT NULL CHECK (scope IN (
        'auth-challenge-ip', 'auth-exchange-ip', 'auth-refresh-ip', 'auth-refresh-account',
        'auth-refresh-device', 'family-structural-account', 'family-structural-device',
        'invitation-create-account', 'invitation-create-ip', 'invitation-redeem-account', 'invitation-redeem-ip')),
    key_digest bytea NOT NULL CHECK (octet_length(key_digest) = 32),
    window_start timestamptz NOT NULL,
    window_end timestamptz NOT NULL,
    count integer NOT NULL CHECK (count BETWEEN 1 AND 1000000),
    PRIMARY KEY (scope, key_digest, window_start),
    CHECK (isfinite(window_start) AND isfinite(window_end) AND window_end > window_start
        AND window_end - window_start <= interval '24 hours')
);
CREATE INDEX rate_limit_buckets_expiry_idx ON public.rate_limit_buckets(window_end);

REVOKE ALL ON TABLE public.account_identities, public.devices, public.auth_challenges,
    public.auth_exchange_material, public.sessions, public.session_refresh_history,
    public.apple_notification_dedup, public.apple_revocation_jobs, public.rate_limit_buckets
    FROM PUBLIC, baby_tracker_runtime, baby_tracker_projector;

CREATE FUNCTION app.create_auth_challenge(
    p_token_digest bytea, p_canonical_origin text, p_server_instance_id uuid,
    p_expires_at timestamptz, p_now timestamptz
) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE challenge_id uuid;
BEGIN
    IF octet_length(p_token_digest) IS DISTINCT FROM 32
       OR octet_length(p_canonical_origin) NOT BETWEEN 1 AND 2048
       OR octet_length(p_canonical_origin) <> char_length(p_canonical_origin)
       OR p_server_instance_id IS NULL OR p_server_instance_id = '00000000-0000-0000-0000-000000000000'
       OR NOT isfinite(p_now) OR NOT isfinite(p_expires_at)
       OR p_expires_at IS DISTINCT FROM p_now + interval '5 minutes' THEN
        RAISE EXCEPTION 'invalid auth challenge' USING ERRCODE = '22023';
    END IF;
    challenge_id := gen_random_uuid();
    INSERT INTO public.auth_challenges(id, token_digest, canonical_origin, server_instance_id, expires_at, created_at)
    VALUES (challenge_id, p_token_digest, p_canonical_origin, p_server_instance_id, p_expires_at, p_now);
    RETURN challenge_id;
END;
$$;

CREATE FUNCTION app.consume_auth_challenge(
    p_token_digest bytea, p_canonical_origin text, p_server_instance_id uuid, p_now timestamptz
) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE challenge_id uuid;
BEGIN
    IF octet_length(p_token_digest) IS DISTINCT FROM 32
       OR octet_length(p_canonical_origin) NOT BETWEEN 1 AND 2048
       OR octet_length(p_canonical_origin) <> char_length(p_canonical_origin)
       OR p_server_instance_id IS NULL OR p_server_instance_id = '00000000-0000-0000-0000-000000000000'
       OR NOT isfinite(p_now) THEN
        RAISE EXCEPTION 'invalid auth challenge' USING ERRCODE = '22023';
    END IF;
    UPDATE public.auth_challenges SET consumed_at = p_now
     WHERE token_digest = p_token_digest AND canonical_origin = p_canonical_origin
       AND server_instance_id = p_server_instance_id AND consumed_at IS NULL AND expires_at > p_now
     RETURNING id INTO challenge_id;
    IF challenge_id IS NULL THEN
        RAISE EXCEPTION 'invalid or expired challenge' USING ERRCODE = 'P0002';
    END IF;
    RETURN challenge_id;
END;
$$;

CREATE FUNCTION app.lookup_auth_identity(
    p_provider text, p_identity_key_id text, p_identity_digest bytea
) RETURNS TABLE(account_id uuid, authority_status text, display_name text, profile_required boolean)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE found_account uuid;
    previous_account_setting text := coalesce(current_setting('app.account_id',true),'');
BEGIN
    IF p_provider IS DISTINCT FROM 'apple' OR p_identity_key_id !~ '^[a-z][a-z0-9._-]{0,63}$'
       OR octet_length(p_identity_digest) IS DISTINCT FROM 32 THEN
        RAISE EXCEPTION 'invalid identity lookup' USING ERRCODE = '22023';
    END IF;
    SELECT i.account_id INTO found_account FROM public.account_identities i
     WHERE i.provider=p_provider AND i.identity_key_id=p_identity_key_id AND i.identity_digest=p_identity_digest;
    IF found_account IS NULL THEN RETURN; END IF;
    PERFORM set_config('app.account_id', found_account::text, true);
    RETURN QUERY SELECT i.account_id, s.status, a.display_name, a.profile_required
      FROM public.account_identities i
      JOIN public.account_security_state s ON s.account_id=i.account_id AND s.identity_key =
        'hmac-sha256:' || i.identity_key_id || ':' || encode(i.identity_digest,'hex')
     JOIN public.accounts a ON a.id=i.account_id
     WHERE i.provider=p_provider AND i.identity_key_id=p_identity_key_id AND i.identity_digest=p_identity_digest;
    PERFORM set_config('app.account_id',previous_account_setting,true);
END;
$$;

CREATE FUNCTION app.lookup_device_binding(
    p_account_id uuid, p_device_id uuid, p_server_instance_id uuid, p_canonical_origin text
) RETURNS TABLE(continuity_key_id text, continuity_digest bytea, binding_revoked boolean)
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog
AS $$
BEGIN
    IF p_account_id IS NULL OR p_account_id='00000000-0000-0000-0000-000000000000'
       OR p_device_id IS NULL OR p_device_id='00000000-0000-0000-0000-000000000000'
       OR p_server_instance_id IS NULL OR p_server_instance_id='00000000-0000-0000-0000-000000000000'
       OR octet_length(p_canonical_origin) NOT BETWEEN 1 AND 2048
       OR octet_length(p_canonical_origin) <> char_length(p_canonical_origin)
       THEN
        RAISE EXCEPTION 'invalid device lookup' USING ERRCODE = '22023';
    END IF;
    RETURN QUERY SELECT d.continuity_key_id,d.continuity_digest,d.binding_revoked_at IS NOT NULL
      FROM public.devices d WHERE d.account_id=p_account_id AND d.id=p_device_id
       AND d.server_instance_id=p_server_instance_id AND d.canonical_origin=p_canonical_origin;
END;
$$;

CREATE FUNCTION app.lookup_access_session(p_token_key_id text, p_access_digest bytea)
RETURNS TABLE(session_id uuid, account_id uuid, device_id uuid, lineage_root_session_id uuid,
    server_instance_id uuid, recovery_epoch bigint, security_revision bigint, scope text, family_id uuid,
    access_expires_at timestamptz, refresh_expires_at timestamptz, reauthenticate_at timestamptz,
    absolute_expires_at timestamptz, session_status text, consumed_at timestamptz,
    revoked_at timestamptz, revoke_reason text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
AS $$
BEGIN
    IF p_token_key_id !~ '^[a-z][a-z0-9._-]{0,63}$' OR octet_length(p_access_digest) IS DISTINCT FROM 32 THEN
        RAISE EXCEPTION 'invalid access lookup' USING ERRCODE = '22023';
    END IF;
    RETURN QUERY SELECT s.session_id,s.account_id,s.device_id,s.lineage_root_session_id,s.server_instance_id,
        s.recovery_epoch,s.security_revision,s.scope,s.family_id,s.access_expires_at,s.refresh_expires_at,
        s.reauthenticate_at,s.absolute_expires_at,s.status,s.consumed_at,s.revoked_at,s.revoke_reason
      FROM public.sessions s WHERE s.token_key_id=p_token_key_id AND s.access_digest=p_access_digest;
END;
$$;

CREATE FUNCTION app.lookup_refresh_session(p_token_key_id text, p_refresh_digest bytea)
RETURNS TABLE(session_id uuid, account_id uuid, device_id uuid, parent_session_id uuid,
    lineage_root_session_id uuid, server_instance_id uuid, recovery_epoch bigint, security_revision bigint,
    scope text, family_id uuid, access_expires_at timestamptz, refresh_expires_at timestamptz,
    reauthenticate_at timestamptz, absolute_expires_at timestamptz, session_status text,
    refresh_consumed_at timestamptz, reuse_detected_at timestamptz, revoked_at timestamptz, revoke_reason text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
AS $$
BEGIN
    IF p_token_key_id !~ '^[a-z][a-z0-9._-]{0,63}$' OR octet_length(p_refresh_digest) IS DISTINCT FROM 32 THEN
        RAISE EXCEPTION 'invalid refresh lookup' USING ERRCODE = '22023';
    END IF;
    RETURN QUERY SELECT s.session_id,s.account_id,s.device_id,s.parent_session_id,s.lineage_root_session_id,
        s.server_instance_id,s.recovery_epoch,s.security_revision,s.scope,s.family_id,s.access_expires_at,
        s.refresh_expires_at,s.reauthenticate_at,s.absolute_expires_at,s.status,
        coalesce(h.consumed_at,s.consumed_at),h.reuse_detected_at,s.revoked_at,s.revoke_reason
      FROM public.sessions s LEFT JOIN public.session_refresh_history h
        ON h.token_key_id=p_token_key_id AND h.refresh_digest=p_refresh_digest AND h.session_id=s.session_id
     WHERE (s.token_key_id=p_token_key_id AND s.refresh_digest=p_refresh_digest) OR h.session_id IS NOT NULL;
END;
$$;

CREATE FUNCTION app.take_rate_limits(p_charges jsonb, p_now timestamptz)
RETURNS TABLE(allowed boolean, remaining integer, retry_after_seconds integer)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
AS $$
DECLARE
    charge jsonb;
    previous_key text;
    charge_key text;
    charged_count integer;
    minimum_remaining integer := 1000000;
    blocking_end timestamptz;
BEGIN
    IF NOT isfinite(p_now) OR jsonb_typeof(p_charges) IS DISTINCT FROM 'array'
       OR jsonb_array_length(p_charges) NOT BETWEEN 1 AND 3 THEN
        RAISE EXCEPTION 'invalid rate limit charges' USING ERRCODE = '22023';
    END IF;
    BEGIN
        FOR charge IN SELECT value FROM jsonb_array_elements(p_charges)
        LOOP
            IF jsonb_typeof(charge) IS DISTINCT FROM 'object'
               OR (charge - ARRAY['scope','key_digest','cost','limit','window_start','window_end']::text[]) <> '{}'::jsonb
               OR NOT (charge ?& ARRAY['scope','key_digest','cost','limit','window_start','window_end'])
               OR jsonb_typeof(charge->'scope') <> 'string'
               OR jsonb_typeof(charge->'key_digest') <> 'string'
               OR jsonb_typeof(charge->'cost') <> 'number'
               OR jsonb_typeof(charge->'limit') <> 'number'
               OR jsonb_typeof(charge->'window_start') <> 'string'
               OR jsonb_typeof(charge->'window_end') <> 'string'
               OR charge->>'scope' NOT IN (
                  'auth-challenge-ip','auth-exchange-ip','auth-refresh-ip','auth-refresh-account',
                  'auth-refresh-device','family-structural-account','family-structural-device',
                  'invitation-create-account','invitation-create-ip','invitation-redeem-account','invitation-redeem-ip')
               OR charge->>'key_digest' !~ '^[0-9a-f]{64}$'
               OR charge->>'cost' !~ '^[1-9][0-9]{0,6}$'
               OR charge->>'limit' !~ '^[1-9][0-9]{0,6}$'
               OR (charge->>'cost')::integer > 1000000 OR (charge->>'limit')::integer > 1000000
               OR (charge->>'cost')::integer > (charge->>'limit')::integer
               OR NOT isfinite((charge->>'window_start')::timestamptz)
               OR NOT isfinite((charge->>'window_end')::timestamptz)
               OR (charge->>'window_start')::timestamptz > p_now
               OR (charge->>'window_end')::timestamptz <= p_now
               OR (charge->>'window_end')::timestamptz - (charge->>'window_start')::timestamptz > interval '24 hours' THEN
                RAISE EXCEPTION 'invalid rate limit charge' USING ERRCODE = '22023';
            END IF;
            charge_key := (charge->>'scope') || ':' || (charge->>'key_digest');
            IF previous_key IS NOT NULL AND previous_key >= charge_key THEN
                RAISE EXCEPTION 'rate limit charges are not canonical' USING ERRCODE = '22023';
            END IF;
            previous_key := charge_key;
        END LOOP;
        FOR charge IN SELECT value FROM jsonb_array_elements(p_charges)
        LOOP
            INSERT INTO public.rate_limit_buckets(scope,key_digest,window_start,window_end,count)
            VALUES(charge->>'scope',decode(charge->>'key_digest','hex'),(charge->>'window_start')::timestamptz,
                   (charge->>'window_end')::timestamptz,(charge->>'cost')::integer)
            ON CONFLICT(scope,key_digest,window_start) DO UPDATE
               SET count=public.rate_limit_buckets.count+EXCLUDED.count
             WHERE public.rate_limit_buckets.window_end=EXCLUDED.window_end
               AND public.rate_limit_buckets.count BETWEEN 1 AND 1000000
               AND public.rate_limit_buckets.count+EXCLUDED.count <= (charge->>'limit')::integer
            RETURNING count INTO charged_count;
            IF charged_count IS NULL THEN
                blocking_end := greatest(coalesce(blocking_end,'-infinity'::timestamptz),(charge->>'window_end')::timestamptz);
                RAISE EXCEPTION 'private rate denial' USING ERRCODE = 'P0001';
            END IF;
            minimum_remaining := least(minimum_remaining,(charge->>'limit')::integer-charged_count);
            charged_count := NULL;
        END LOOP;
    EXCEPTION WHEN SQLSTATE 'P0001' THEN
        SELECT max((candidate->>'window_end')::timestamptz) INTO blocking_end
          FROM jsonb_array_elements(p_charges) candidate
          JOIN public.rate_limit_buckets bucket
            ON bucket.scope=candidate->>'scope'
           AND bucket.key_digest=decode(candidate->>'key_digest','hex')
           AND bucket.window_start=(candidate->>'window_start')::timestamptz
         WHERE bucket.window_end<>(candidate->>'window_end')::timestamptz
            OR bucket.count+(candidate->>'cost')::integer>(candidate->>'limit')::integer;
        IF blocking_end IS NULL THEN
            RAISE EXCEPTION 'rate limit denial state changed' USING ERRCODE = '40001';
        END IF;
        RETURN QUERY SELECT false,0,greatest(1,ceil(extract(epoch FROM blocking_end-p_now))::integer);
        RETURN;
    END;
    RETURN QUERY SELECT true,minimum_remaining,0;
END;
$$;

CREATE FUNCTION app.stage_auth_material(
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
        canonical := canonical || ',"parent_session_id":' || app.go_json_string(material->>'parent_session_id') || '}';
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
        apple_credential_id,apple_credential_key_id,apple_credential_purpose,apple_credential_object_id,created_at,expires_at)
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
        p_now,p_now+interval '10 minutes')
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

CREATE FUNCTION app.reserve_apple_notification(
    p_candidates jsonb, p_current_key_id text, p_current_digest bytea, p_command_id uuid, p_received_at timestamptz
) RETURNS TABLE(command_id uuid, newly_reserved boolean)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog
AS $$
DECLARE candidate jsonb; matches integer; stored_command uuid;
BEGIN
    IF jsonb_typeof(p_candidates)<>'array' OR jsonb_array_length(p_candidates) NOT BETWEEN 1 AND 8
       OR p_current_key_id !~ '^[a-z][a-z0-9._-]{0,63}$' OR octet_length(p_current_digest)<>32
       OR p_command_id IS NULL OR p_command_id='00000000-0000-0000-0000-000000000000' OR NOT isfinite(p_received_at) THEN
        RAISE EXCEPTION 'invalid notification reservation' USING ERRCODE = '22023';
    END IF;
    IF EXISTS (SELECT FROM jsonb_array_elements(p_candidates) c
                WHERE jsonb_typeof(c)<>'object' OR (c-ARRAY['key_id','digest']::text[])<>'{}'::jsonb
                   OR NOT (c ?& ARRAY['key_id','digest']) OR jsonb_typeof(c->'key_id')<>'string'
                   OR jsonb_typeof(c->'digest')<>'string' OR c->>'key_id' !~ '^[a-z][a-z0-9._-]{0,63}$'
                   OR c->>'digest' !~ '^[0-9a-f]{64}$')
       OR (p_candidates->0->>'key_id') IS DISTINCT FROM p_current_key_id
       OR decode(p_candidates->0->>'digest','hex') IS DISTINCT FROM p_current_digest
       OR (SELECT count(*) FROM jsonb_array_elements(p_candidates)) <>
          (SELECT count(*) FROM (SELECT DISTINCT c->>'key_id',c->>'digest' FROM jsonb_array_elements(p_candidates) c) d)
       OR (SELECT count(*) FROM jsonb_array_elements(p_candidates)) <>
          (SELECT count(DISTINCT c->>'key_id') FROM jsonb_array_elements(p_candidates) c) THEN
        RAISE EXCEPTION 'invalid notification candidates' USING ERRCODE = '22023';
    END IF;
    -- Candidate sets can overlap across key rotation. Serialize the bounded
    -- lookup/insert so concurrent current and retained-key requests converge
    -- on one deterministic command instead of exposing a unique violation.
    LOCK TABLE public.apple_notification_dedup IN SHARE ROW EXCLUSIVE MODE;
    SELECT count(*),min(d.command_id::text)::uuid INTO matches,stored_command
      FROM public.apple_notification_dedup d JOIN jsonb_array_elements(p_candidates) c
        ON d.notification_key_id=c->>'key_id' AND d.notification_digest=decode(c->>'digest','hex');
    IF matches>1 THEN RAISE EXCEPTION 'ambiguous notification reservation' USING ERRCODE='23505'; END IF;
    IF matches=1 THEN RETURN QUERY SELECT stored_command,false; RETURN; END IF;
    INSERT INTO public.apple_notification_dedup(notification_key_id,notification_digest,command_id,received_at)
    VALUES(p_current_key_id,p_current_digest,p_command_id,p_received_at);
    RETURN QUERY SELECT p_command_id,true;
END;
$$;

CREATE OR REPLACE FUNCTION app.project_security_event(
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
            effect->>'parent_session_id',effect->>'lineage_root_session_id',effect->>'server_instance_id',effect->>'invitation_id'] LOOP
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
            ',"projection_material_digest":'||app.go_json_string(effect->>'projection_material_digest')||',"status":'||app.go_json_string(effect->>'status')||
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
            'lineage_root_session_id','projection_material_digest','status','expires_at','recovery_epoch']::text[])<>'{}'::jsonb
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
        IF has_stage THEN
            INSERT INTO public.sessions(session_id,account_id,device_id,lineage_root_session_id,parent_session_id,server_instance_id,
                recovery_epoch,security_revision,scope,family_id,token_key_id,access_digest,refresh_digest,access_expires_at,
                refresh_expires_at,reauthenticate_at,absolute_expires_at,status,created_at,updated_at)
            VALUES(staged.session_id,staged.account_id,staged.device_id,staged.lineage_root_session_id,staged.parent_session_id,
                staged.server_instance_id,staged.recovery_epoch,p_revision,staged.scope,staged.family_id,
                staged.token_key_id,staged.access_digest,staged.refresh_digest,staged.access_expires_at,staged.refresh_expires_at,
                staged.reauthenticate_at,staged.absolute_expires_at,'active',(p_event->>'at')::timestamptz,(p_event->>'at')::timestamptz);
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
    PERFORM set_config('app.account_id',previous_account_setting,true);
    INSERT INTO public.security_audit_events(generation,revision,event_hash,command_id,actor_account_id,kind,event_data,event_bytes)
    VALUES(p_generation,p_revision,p_event_hash,p_command_id,p_actor_account_id,event_kind,p_event,p_event_bytes);
    INSERT INTO public.security_command_receipts(actor_account_id,command_id,command_hash,generation,revision,result)
    VALUES(p_actor_account_id,p_command_id,p_command_hash,p_generation,p_revision,p_result);
    UPDATE public.security_projection_head SET generation=p_generation,revision=p_revision,event_hash=p_event_hash WHERE singleton;
END;
$$;

REVOKE ALL ON FUNCTION app.create_auth_challenge(bytea,text,uuid,timestamptz,timestamptz),
    app.consume_auth_challenge(bytea,text,uuid,timestamptz),
    app.lookup_auth_identity(text,text,bytea),
    app.lookup_device_binding(uuid,uuid,uuid,text),
    app.lookup_access_session(text,bytea),
    app.lookup_refresh_session(text,bytea),
    app.take_rate_limits(jsonb,timestamptz),
    app.stage_auth_material(uuid,bytea,bytea,bytea,timestamptz),
    app.reserve_apple_notification(jsonb,text,bytea,uuid,timestamptz)
    FROM PUBLIC, baby_tracker_projector;
GRANT EXECUTE ON FUNCTION app.create_auth_challenge(bytea,text,uuid,timestamptz,timestamptz),
    app.consume_auth_challenge(bytea,text,uuid,timestamptz),
    app.lookup_auth_identity(text,text,bytea),
    app.lookup_device_binding(uuid,uuid,uuid,text),
    app.lookup_access_session(text,bytea),
    app.lookup_refresh_session(text,bytea),
    app.take_rate_limits(jsonb,timestamptz),
    app.stage_auth_material(uuid,bytea,bytea,bytea,timestamptz),
    app.reserve_apple_notification(jsonb,text,bytea,uuid,timestamptz)
    TO baby_tracker_runtime;

REVOKE ALL ON FUNCTION app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    FROM PUBLIC, baby_tracker_runtime;
GRANT EXECUTE ON FUNCTION app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    TO baby_tracker_projector;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
SELECT 5,principal,object_kind,object_identity,privileges
  FROM public.schema_privilege_manifest WHERE schema_version=4;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
    (5,'baby_tracker_runtime','function','app.consume_auth_challenge(bytea,text,uuid,timestamp with time zone)','EXECUTE'),
    (5,'baby_tracker_runtime','function','app.create_auth_challenge(bytea,text,uuid,timestamp with time zone,timestamp with time zone)','EXECUTE'),
    (5,'baby_tracker_runtime','function','app.lookup_access_session(text,bytea)','EXECUTE'),
    (5,'baby_tracker_runtime','function','app.lookup_auth_identity(text,text,bytea)','EXECUTE'),
    (5,'baby_tracker_runtime','function','app.lookup_device_binding(uuid,uuid,uuid,text)','EXECUTE'),
    (5,'baby_tracker_runtime','function','app.lookup_refresh_session(text,bytea)','EXECUTE'),
    (5,'baby_tracker_runtime','function','app.reserve_apple_notification(jsonb,text,bytea,uuid,timestamp with time zone)','EXECUTE'),
    (5,'baby_tracker_runtime','function','app.stage_auth_material(uuid,bytea,bytea,bytea,timestamp with time zone)','EXECUTE'),
    (5,'baby_tracker_runtime','function','app.take_rate_limits(jsonb,timestamp with time zone)','EXECUTE');

UPDATE public.schema_compatibility
   SET current_version=5,introduced_by='000005_identity_sessions.sql',recorded_at=now()
 WHERE singleton;
