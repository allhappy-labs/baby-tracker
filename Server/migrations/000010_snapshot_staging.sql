-- Durable page-token bindings and immutable full-snapshot staging. Production
-- sync handlers land in the following feature commit; this migration is the
-- deployable overlap predecessor for schema 9 -> 10.

ALTER TABLE public.sessions
    ADD CONSTRAINT sessions_sync_binding_key UNIQUE
        (account_id, device_id, session_id, family_id, server_instance_id,
         recovery_epoch, security_revision);

CREATE TABLE public.sync_snapshots (
    family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    id uuid NOT NULL CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    account_id uuid NOT NULL CHECK (account_id <> '00000000-0000-0000-0000-000000000000'),
    device_id uuid NOT NULL CHECK (device_id <> '00000000-0000-0000-0000-000000000000'),
    session_id uuid NOT NULL CHECK (session_id <> '00000000-0000-0000-0000-000000000000'),
    server_instance_id uuid NOT NULL CHECK (server_instance_id <> '00000000-0000-0000-0000-000000000000'),
    recovery_epoch bigint NOT NULL CHECK (recovery_epoch BETWEEN 1 AND 9007199254740991),
    security_revision bigint NOT NULL CHECK (security_revision BETWEEN 1 AND 9007199254740991),
    high_water_sequence bigint NOT NULL CHECK (high_water_sequence BETWEEN 0 AND 9007199254740991),
    entity_count bigint NOT NULL,
    encoded_bytes bigint NOT NULL,
    captured_at timestamptz NOT NULL CHECK (isfinite(captured_at)),
    expires_at timestamptz NOT NULL CHECK (isfinite(expires_at) AND expires_at > captured_at),
    PRIMARY KEY (family_id, id),
    CONSTRAINT sync_snapshots_budget_bounds CHECK (
        entity_count BETWEEN 0 AND 2000000
        AND encoded_bytes BETWEEN 0 AND 536870912
        AND high_water_sequence BETWEEN 0 AND 9007199254740991),
    CONSTRAINT sync_snapshots_session_fk FOREIGN KEY
        (account_id, device_id, session_id, family_id, server_instance_id,
         recovery_epoch, security_revision)
        REFERENCES public.sessions
        (account_id, device_id, session_id, family_id, server_instance_id,
         recovery_epoch, security_revision),
    UNIQUE (family_id, id, account_id),
    UNIQUE (family_id, id, account_id, device_id, session_id,
        server_instance_id, recovery_epoch, security_revision),
    UNIQUE (family_id, id, account_id, device_id, session_id,
        server_instance_id, recovery_epoch, security_revision, high_water_sequence,
        entity_count)
);
CREATE INDEX sync_snapshots_expiry_idx
    ON public.sync_snapshots(expires_at, family_id, id);
CREATE INDEX sync_snapshots_account_active_idx
    ON public.sync_snapshots(account_id, expires_at, family_id, id);
CREATE INDEX sync_snapshots_family_active_idx
    ON public.sync_snapshots(family_id, expires_at, id);

-- Account-scoped, metadata-only admission rows let the application count the
-- current account's still-active captures even after its family context has
-- changed. The runtime has no direct privileges on this table.
CREATE TABLE public.sync_snapshot_admissions (
    account_id uuid NOT NULL CHECK (account_id <> '00000000-0000-0000-0000-000000000000'),
    family_id uuid NOT NULL,
    snapshot_id uuid NOT NULL CHECK (snapshot_id <> '00000000-0000-0000-0000-000000000000'),
    expires_at timestamptz NOT NULL CHECK (isfinite(expires_at)),
    PRIMARY KEY (account_id, snapshot_id),
    CONSTRAINT sync_snapshot_admissions_snapshot_fk FOREIGN KEY
        (family_id, snapshot_id, account_id)
        REFERENCES public.sync_snapshots(family_id, id, account_id) ON DELETE CASCADE
);
CREATE INDEX sync_snapshot_admissions_expiry_idx
    ON public.sync_snapshot_admissions(expires_at, account_id, snapshot_id);
CREATE INDEX sync_snapshot_admissions_account_active_idx
    ON public.sync_snapshot_admissions(account_id, expires_at, snapshot_id);
CREATE INDEX sync_snapshot_admissions_family_active_idx
    ON public.sync_snapshot_admissions(family_id, expires_at, snapshot_id);

CREATE TABLE public.sync_snapshot_entities (
    family_id uuid NOT NULL,
    snapshot_id uuid NOT NULL CHECK (snapshot_id <> '00000000-0000-0000-0000-000000000000'),
    account_id uuid NOT NULL CHECK (account_id <> '00000000-0000-0000-0000-000000000000'),
    device_id uuid NOT NULL CHECK (device_id <> '00000000-0000-0000-0000-000000000000'),
    session_id uuid NOT NULL CHECK (session_id <> '00000000-0000-0000-0000-000000000000'),
    server_instance_id uuid NOT NULL CHECK (server_instance_id <> '00000000-0000-0000-0000-000000000000'),
    recovery_epoch bigint NOT NULL CHECK (recovery_epoch BETWEEN 1 AND 9007199254740991),
    security_revision bigint NOT NULL CHECK (security_revision BETWEEN 1 AND 9007199254740991),
    position bigint NOT NULL CHECK (position BETWEEN 0 AND 9007199254740991),
    entity_kind text NOT NULL CHECK (entity_kind IN ('baby', 'feed', 'sleep', 'diaper', 'timer')),
    entity_id uuid NOT NULL CHECK (entity_id <> '00000000-0000-0000-0000-000000000000'),
    entity_version bigint NOT NULL CHECK (entity_version BETWEEN 1 AND 9007199254740991),
    canonical_entity jsonb NOT NULL,
    encoded_bytes bigint NOT NULL,
    PRIMARY KEY (family_id, snapshot_id, position),
    UNIQUE (family_id, snapshot_id, entity_kind, entity_id),
    CONSTRAINT sync_snapshot_entities_canonical_object CHECK (
        jsonb_typeof(canonical_entity) = 'object'),
    CONSTRAINT sync_snapshot_entities_encoded_bytes_bounds CHECK (
        encoded_bytes BETWEEN 1 AND 4194304),
    CONSTRAINT sync_snapshot_entities_snapshot_fk FOREIGN KEY
        (family_id, snapshot_id, account_id, device_id, session_id,
         server_instance_id, recovery_epoch, security_revision)
        REFERENCES public.sync_snapshots
        (family_id, id, account_id, device_id, session_id,
         server_instance_id, recovery_epoch, security_revision) ON DELETE CASCADE
);
CREATE INDEX sync_snapshot_entities_cleanup_idx
    ON public.sync_snapshot_entities(snapshot_id, position);

CREATE TABLE public.sync_page_sessions (
    family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    token_digest bytea NOT NULL CHECK (octet_length(token_digest) = 32),
    token_key_id text NOT NULL CHECK (token_key_id ~ '^[a-z][a-z0-9._-]{0,63}$'),
    account_id uuid NOT NULL CHECK (account_id <> '00000000-0000-0000-0000-000000000000'),
    device_id uuid NOT NULL CHECK (device_id <> '00000000-0000-0000-0000-000000000000'),
    session_id uuid NOT NULL CHECK (session_id <> '00000000-0000-0000-0000-000000000000'),
    server_instance_id uuid NOT NULL CHECK (server_instance_id <> '00000000-0000-0000-0000-000000000000'),
    recovery_epoch bigint NOT NULL CHECK (recovery_epoch BETWEEN 1 AND 9007199254740991),
    security_revision bigint NOT NULL CHECK (security_revision BETWEEN 1 AND 9007199254740991),
    kind text NOT NULL CHECK (kind IN ('sync', 'snapshot')),
    high_water_sequence bigint NOT NULL CHECK (high_water_sequence BETWEEN 0 AND 9007199254740991),
    range_end_position bigint NOT NULL CHECK (range_end_position BETWEEN 0 AND 9007199254740991),
    resume_position bigint NOT NULL CHECK (resume_position BETWEEN 0 AND 9007199254740991),
    page_end_position bigint NOT NULL CHECK (page_end_position BETWEEN 0 AND 9007199254740991),
    page_item_count integer NOT NULL CHECK (page_item_count BETWEEN 1 AND 1000),
    page_encoded_bytes integer NOT NULL CHECK (page_encoded_bytes BETWEEN 1 AND 4194304),
    snapshot_id uuid,
    item_limit integer NOT NULL,
    byte_limit integer NOT NULL,
    created_at timestamptz NOT NULL CHECK (isfinite(created_at)),
    expires_at timestamptz NOT NULL CHECK (isfinite(expires_at) AND expires_at > created_at),
    PRIMARY KEY (family_id, token_digest),
    CONSTRAINT sync_page_sessions_page_range CHECK (
        resume_position <= page_end_position
        AND page_end_position <= range_end_position),
    CONSTRAINT sync_page_sessions_snapshot_kind CHECK (
        (kind = 'sync' AND snapshot_id IS NULL
            AND range_end_position = high_water_sequence)
        OR (kind = 'snapshot' AND snapshot_id IS NOT NULL)),
    CONSTRAINT sync_page_sessions_policy_bounds CHECK (
        item_limit BETWEEN 1 AND 1000
        AND byte_limit BETWEEN 65536 AND 4194304
        AND page_item_count <= item_limit
        AND page_encoded_bytes <= byte_limit
        AND page_end_position - resume_position = page_item_count),
    CONSTRAINT sync_page_sessions_session_fk FOREIGN KEY
        (account_id, device_id, session_id, family_id, server_instance_id,
         recovery_epoch, security_revision)
        REFERENCES public.sessions
        (account_id, device_id, session_id, family_id, server_instance_id,
         recovery_epoch, security_revision),
    CONSTRAINT sync_page_sessions_snapshot_fk FOREIGN KEY
        (family_id, snapshot_id, account_id, device_id, session_id,
         server_instance_id, recovery_epoch, security_revision, high_water_sequence,
         range_end_position)
        REFERENCES public.sync_snapshots
        (family_id, id, account_id, device_id, session_id,
         server_instance_id, recovery_epoch, security_revision, high_water_sequence,
         entity_count)
        ON DELETE CASCADE
);
CREATE INDEX sync_page_sessions_expiry_idx
    ON public.sync_page_sessions(expires_at, family_id, token_digest);
CREATE INDEX sync_page_sessions_account_active_idx
    ON public.sync_page_sessions(account_id, family_id, expires_at, token_digest);

ALTER TABLE public.sync_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.sync_snapshots FORCE ROW LEVEL SECURITY;
ALTER TABLE public.sync_snapshot_admissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.sync_snapshot_admissions FORCE ROW LEVEL SECURITY;
ALTER TABLE public.sync_snapshot_entities ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.sync_snapshot_entities FORCE ROW LEVEL SECURITY;
ALTER TABLE public.sync_page_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.sync_page_sessions FORCE ROW LEVEL SECURITY;

CREATE POLICY family_context ON public.sync_snapshots
    USING (family_id = app.current_family_id() AND account_id = app.current_account_id())
    WITH CHECK (family_id = app.current_family_id() AND account_id = app.current_account_id());
CREATE POLICY account_context ON public.sync_snapshot_admissions
    USING (account_id = app.current_account_id() OR family_id = app.current_family_id())
    WITH CHECK (account_id = app.current_account_id()
        AND family_id = app.current_family_id());
CREATE POLICY family_context ON public.sync_snapshot_entities
    USING (family_id = app.current_family_id() AND account_id = app.current_account_id())
    WITH CHECK (family_id = app.current_family_id() AND account_id = app.current_account_id());
CREATE POLICY family_context ON public.sync_page_sessions
    USING (family_id = app.current_family_id() AND account_id = app.current_account_id())
    WITH CHECK (family_id = app.current_family_id() AND account_id = app.current_account_id());

CREATE FUNCTION app.stage_sync_snapshot_admission() RETURNS trigger
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = pg_catalog
AS $$
BEGIN
    IF NEW.account_id IS DISTINCT FROM app.current_account_id()
       OR NEW.family_id IS DISTINCT FROM app.current_family_id() THEN
        RAISE EXCEPTION 'snapshot admission staging denied' USING ERRCODE = '42501';
    END IF;
    INSERT INTO public.sync_snapshot_admissions(account_id,family_id,snapshot_id,expires_at)
    VALUES(NEW.account_id,NEW.family_id,NEW.id,NEW.expires_at);
    RETURN NEW;
END
$$;
REVOKE ALL ON FUNCTION app.stage_sync_snapshot_admission()
    FROM PUBLIC, baby_tracker_runtime, baby_tracker_projector, baby_tracker_apple_revoker;
CREATE TRIGGER sync_snapshots_stage_admission
AFTER INSERT ON public.sync_snapshots
FOR EACH ROW EXECUTE FUNCTION app.stage_sync_snapshot_admission();

CREATE FUNCTION app.sync_snapshot_admission_counts(
    p_account_id uuid, p_family_id uuid, p_now timestamptz
) RETURNS TABLE(account_active bigint, family_active bigint)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = pg_catalog
AS $$
BEGIN
    IF p_account_id IS NULL OR p_account_id IS DISTINCT FROM app.current_account_id()
       OR p_family_id IS NULL OR p_family_id IS DISTINCT FROM app.current_family_id()
       OR p_now IS NULL OR NOT isfinite(p_now) THEN
        RAISE EXCEPTION 'snapshot admission count denied' USING ERRCODE = '42501';
    END IF;
    SELECT count(*) INTO account_active
      FROM public.sync_snapshot_admissions
     WHERE account_id = p_account_id AND expires_at > p_now;
    SELECT count(*) INTO family_active
      FROM public.sync_snapshot_admissions
     WHERE family_id = p_family_id AND expires_at > p_now;
    RETURN NEXT;
END
$$;
REVOKE ALL ON FUNCTION app.sync_snapshot_admission_counts(uuid,uuid,timestamptz)
    FROM PUBLIC, baby_tracker_projector, baby_tracker_apple_revoker;

REVOKE ALL ON public.sync_page_sessions, public.sync_snapshots,
    public.sync_snapshot_entities, public.sync_snapshot_admissions
    FROM PUBLIC, baby_tracker_runtime, baby_tracker_projector,
    baby_tracker_apple_revoker;
GRANT SELECT, INSERT ON public.sync_page_sessions, public.sync_snapshots,
    public.sync_snapshot_entities TO baby_tracker_runtime;
GRANT EXECUTE ON FUNCTION app.sync_snapshot_admission_counts(uuid,uuid,timestamptz)
    TO baby_tracker_runtime;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
SELECT 10,principal,object_kind,object_identity,privileges
  FROM public.schema_privilege_manifest WHERE schema_version=9;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
    (10,'baby_tracker_runtime','table','public.sync_page_sessions','SELECT,INSERT'),
    (10,'baby_tracker_runtime','table','public.sync_snapshots','SELECT,INSERT'),
    (10,'baby_tracker_runtime','table','public.sync_snapshot_entities','SELECT,INSERT'),
    (10,'baby_tracker_runtime','function','app.sync_snapshot_admission_counts(uuid,uuid,timestamp with time zone)','EXECUTE');

UPDATE public.schema_compatibility
   SET current_version=10,introduced_by='000010_snapshot_staging.sql',recorded_at=now()
 WHERE singleton;
